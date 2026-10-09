package cli

// `magnum eval`: replay a corpus of PRs with known defects at pinned heads,
// as blind dry-run rounds, and report how many of the defects the review
// found (recall) and how many other findings it posted (noise). Each case
// runs in its own scratch layout (registry, reports, notes copy) and a
// detached worktree of the repository's clone, with agents tagged for the
// run (evalAgentTag), so the live registry, the PR's own agents, another
// run's agents and the PR's reports are never touched. The corpus is a
// local, gitignored file: it describes defects in real code.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	evalUsage    = "run|score|list|show"
	evalRunUsage = "[--corpus FILE] [--case NAME]... [--label TEXT] [--candidate DIR] [--judge-only] [--no-notes] [--keep] [--force]"
	// evalCorpusFile is the default corpus, in the user config's directory
	// (~/.config/magnum); evalLegacyCorpusFile its old place in a checkout.
	evalCorpusFile       = "eval.toml"
	evalLegacyCorpusFile = "eval.local.toml"
	// evalFlagged is the outcome of a case that is not replayed
	// (evalrun.Refused).
	evalFlagged = evalrun.OutcomeFlagged
	// evalCorpusFlagUsage describes --corpus (evalCorpusPath).
	evalCorpusFlagUsage = "this corpus file alone (default: " + evalCorpusFile + " next to config.toml, then the learned corpus)"
)

// evalRunsRoot is where runs live: state/eval/<run id>/ (evalrun.RunsRoot).
func evalRunsRoot(l paths.Layout) string { return evalrun.RunsRoot(l) }

// evalSystem holds the process-level seams of `magnum eval run`.
type evalSystem struct {
	// Runner runs git in magnum's own checkout (evalMagnumCommit); nil
	// means execx.Real.
	Runner execx.Runner
	// Now is the run's clock: its id, its start and end, its cases' times.
	Now func() time.Time
}

// evalSys is the real system; tests replace it.
var evalSys = evalSystem{Now: time.Now}

// evalNewRunDir creates the directory of a run started at now under root
// (eval.NewRunDir: a second run in the same second takes "<id>-2").
func evalNewRunDir(root string, now time.Time) (id, dir string, err error) {
	return eval.NewRunDir(root, now)
}

// evalAgentTag tags the agents of run id (eval.AgentTag): named apart from
// the PR's own agents and from every other run's.
func evalAgentTag(id string) string { return eval.AgentTag(id) }

func newEvalCmd(c *Context) *cobra.Command {
	cmd := newCommand(groupAct, "eval "+evalUsage, "replay PRs with known defects and score the reviews (recall, noise)",
		"Measure the review pipeline on a corpus of PRs with known defects, after a prompt, skill or model change: "+
			"`magnum eval run` replays each case at its pinned head as a blind dry run (nothing is posted, the roles "+
			"are told not to read the PR's reviews, comments or later commits) and reports, per case, how many of the "+
			"seeded defects the planned review found, at what severity, and how many other findings it made.\n\n"+
			"The corpus is ~/.config/magnum/"+evalCorpusFile+" (a checkout's eval.local.toml still works; see eval.toml.example), "+
			"then the learned corpus the daemon writes after each retro (learn/corpus.toml in the data directory: a case per general "+
			"miss and per finding an author declined as wrong or ranked too high; a case name in both is an error); --corpus FILE "+
			"reads that file alone. `run` says how many of its cases are learned ones. Each case runs "+
			"in a scratch registry and report tree under state/eval/<run>/ and a detached worktree of the repository's "+
			"clone, with agents named apart from the PR's own and from every other run's, in a herdr workspace "+
			"\"eval <repo>#<N>\" that is closed when the case ends (a judge still at work is interrupted first) and "+
			"before a later case of the PR starts; the live registry, reports and notes are never "+
			"written. The prompts and the skill are read from disk, so an edit is measured before the daemon restarts.\n\n"+
			"A case costs a full review round (every reviewer role and the judge). `run` refuses to start a case while "+
			"the Codex budget is at or above [usage] codex_soft unless --force, and stops at a usage limit. It never "+
			"replays a case whose PR Codex flagged: one the registry holds a Codex flag for (`magnum codex-flag`), or "+
			"one whose replay, under any case name, was refused before (state/eval/"+eval.FlaggedFile+").",
		func(pos []string) int {
			if len(pos) > 0 && pos[0] != "help" {
				return inspUsage(c, "eval", fmt.Sprintf("unknown eval subcommand %q", pos[0]), evalUsage)
			}
			fmt.Fprintf(c.Stdout, "usage: magnum eval %s\n", evalUsage)
			return 0
		})
	cmd.AddCommand(newEvalRunCmd(c), newEvalScoreCmd(c), newEvalListCmd(c), newEvalShowCmd(c), newEvalBaselineCmd(c))
	return cmd
}

// evalRunFlags are the parsed `magnum eval run` flags.
type evalRunFlags struct {
	corpus, label, candidate        string
	cases                           []string
	judgeOnly, noNotes, keep, force bool
}

func newEvalRunCmd(c *Context) *cobra.Command {
	var f evalRunFlags
	cmd := newCommand("", "run "+evalRunUsage, "replay the corpus (or --case ones) and score it",
		"Replay every case of the corpus (or each --case) one after the other and print the report, compared "+
			"with the previous run. --label names the run (for example the change it measures). --candidate replays "+
			"a directory shaped like magnum's checkout (a worktree with a prompt or skill change): its prompts/ and "+
			"skills/magnum-review/SKILL.md are read instead of the configured ones, and the run's inputs are theirs. --judge-only "+
			"runs the judge alone on the reviewers' reports of each case's newest full replay at the same head whose reviewers read "+
			"what this run's would, the same reviewer prompts and roles and the same --no-notes (a case without one fails before "+
			"anything starts): a judge prompt or skill change costs one agent instead of a round. --no-notes "+
			"withholds the repository notes from every role (by default each reads a scratch copy of them). --keep leaves "+
			"each case's worktree in place. --force runs past the Codex soft cap.",
		func(pos []string) int {
			if len(pos) > 0 {
				return inspUsage(c, "eval run", "no arguments expected", evalRunUsage)
			}
			return runEvalRun(c, f)
		})
	fs := cmd.Flags()
	fs.StringVar(&f.corpus, "corpus", "", evalCorpusFlagUsage)
	fs.StringArrayVar(&f.cases, "case", nil, "only this case (repeatable)")
	fs.StringVar(&f.label, "label", "", "a name for this run")
	fs.StringVar(&f.candidate, "candidate", "", "replay this directory's prompts/ and judge skill instead of the configured ones")
	fs.BoolVar(&f.judgeOnly, "judge-only", false, "run the judge alone on the reviewers' reports of each case's newest full replay")
	fs.BoolVar(&f.noNotes, "no-notes", false, "withhold the repository notes from every role")
	fs.BoolVar(&f.keep, "keep", false, "keep each case's worktree")
	fs.BoolVar(&f.force, "force", false, "run even when the Codex budget is past [usage] codex_soft")
	return cmd
}

func newEvalScoreCmd(c *Context) *cobra.Command {
	var corpus string
	cmd := newCommand("", "score [RUN] [--corpus FILE]", "re-score a run against the corpus as it is now",
		"Re-read each case's result file of RUN (default: the latest run) and score it again with the corpus's "+
			"current defects, so a match rule can be corrected without running the agents again. The corpus is "+
			"the one the run read (a run given --corpus FILE: that file alone) unless --corpus names another. The "+
			"run's report is rewritten.",
		func(pos []string) int { return runEvalScore(c, corpus, pos) })
	cmd.Flags().StringVar(&corpus, "corpus", "", evalCorpusFlagUsage)
	return cmd
}

func newEvalListCmd(c *Context) *cobra.Command {
	return newCommand("", "list", "the runs, oldest first, with recall and noise",
		"List the runs under state/eval with their label, magnum commit, recall and noise.",
		func(pos []string) int {
			if len(pos) > 0 {
				return inspUsage(c, "eval list", "no arguments expected", "")
			}
			return runEvalList(c)
		})
}

func newEvalShowCmd(c *Context) *cobra.Command {
	return newCommand("", "show [RUN]", "a run's report, compared with the run before it",
		"Print RUN's report (default: the latest run) with the run before it for comparison.",
		func(pos []string) int { return runEvalShow(c, pos) })
}

// evalCorpusPath resolves --corpus to the operator's corpus and returns it
// with the learned corpus the daemon writes (paths.Layout.LearnCorpus),
// which eval reads after it (eval.LoadCorpora). A flag names the corpus
// alone: learned is "" then, so the learned cases, which grow with every
// retro, are replayed only with the default corpus. user is "" when no flag
// names one, the default file is not there and the learned one is: the
// learned cases are the corpus then.
func evalCorpusPath(c *Context, flag string) (user, learned string) {
	if flag != "" {
		return flag, ""
	}
	learned = c.Layout.LearnCorpus()
	dir := c.Layout.ConfigDir()
	if dir == "" {
		dir = filepath.Join(paths.Expand("~"), ".config", "magnum")
	}
	user = filepath.Join(dir, evalCorpusFile)
	if legacy := filepath.Join(c.Layout.Home, evalLegacyCorpusFile); c.Layout.Home != "" && !fsx.Exists(user) && fsx.Exists(legacy) {
		return legacy, learned
	}
	if !fsx.Exists(user) && fsx.Exists(learned) {
		return "", learned
	}
	return user, learned
}

// evalCorpusNames is how a run's start line names the corpus files it read.
func evalCorpusNames(user, learned string) string {
	switch {
	case learned == "" || !fsx.Exists(learned):
		return inspTilde(user)
	case user == "":
		return inspTilde(learned)
	}
	return inspTilde(user) + " and " + inspTilde(learned)
}

// evalLearnedCases is how a run's start line counts the cases of the learned
// corpus at learned among cases: " (N learned)", "" when there are none (a
// name is in one of the two files only, eval.LoadCorpora).
func evalLearnedCases(learned string, cases []eval.Case) string {
	if learned == "" {
		return ""
	}
	lc, err := eval.LoadCorpus(learned)
	if err != nil {
		return ""
	}
	n := 0
	for _, c := range cases {
		if slices.ContainsFunc(lc.Cases, func(l eval.Case) bool { return l.Name == c.Name }) {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d learned)", n)
}

func runEvalRun(c *Context, f evalRunFlags) int {
	if err := c.LoadConfig(); err != nil {
		return cmdFail(c, "eval run", fmt.Errorf("%w (fix config.toml; `magnum config` validates it)", err))
	}
	path, learned := evalCorpusPath(c, f.corpus)
	corpus, err := eval.LoadCorpora(path, learned)
	if err != nil {
		if missing := cmp.Or(path, learned); errors.Is(err, fs.ErrNotExist) {
			return cmdFail(c, "eval run", fmt.Errorf("no corpus at %s\nfix: copy eval.toml.example to %s and describe PRs with known defects", inspTilde(missing), inspTilde(missing)))
		}
		return cmdFail(c, "eval run", err)
	}
	cases, err := corpus.Select(f.cases)
	if err != nil {
		return cmdFail(c, "eval run", err)
	}
	cfg := c.Config
	if f.candidate != "" {
		if cfg, err = evalrun.WithCandidate(c.Config, f.candidate); err != nil {
			return cmdFail(c, "eval run", err)
		}
		if f.candidate, err = filepath.Abs(f.candidate); err != nil {
			return cmdFail(c, "eval run", err)
		}
	}
	ctx, stop := signalContext()
	defer stop()
	// The daemon's skill gate replays eval cases in its own process (in
	// worktrees of its own, on the same clones, budget and herdr): a run
	// waits for it to end.
	switch gate, err := c.gateRunning(ctx); {
	case err != nil:
		return cmdFail(c, "eval run", fmt.Errorf("cannot tell whether the daemon's skill gate runs (reading %s: %w)\nfix: retry in a moment",
			inspTilde(c.Layout.DB()), err))
	case gate != "":
		return cmdFail(c, "eval run", fmt.Errorf("the %s replays eval cases now; a run would replay beside it\n"+
			"fix: run it again once the gate ended (`magnum learn` lists the proposal it decides)", gate))
	}

	now := evalSys.Now
	started := now()
	id, dir, err := evalNewRunDir(evalRunsRoot(c.Layout), started)
	if err != nil {
		return cmdFail(c, "eval run", err)
	}
	run := eval.Run{
		ID: id, Label: f.label, Corpus: path, CorpusAlone: learned == "", Started: started, Candidate: f.candidate,
		Models: evalModels(cfg), Inputs: evalrun.Inputs(cfg), ReviewerInputs: evalrun.ReviewerInputs(cfg), NoNotes: f.noNotes,
	}
	deps := evalrun.Deps{Layout: c.Layout, Config: c.Config, Now: now, Stderr: c.Stderr,
		Logger:   app.NewLogger(nil, c.Stderr, slog.LevelWarn),
		Progress: func(format string, args ...any) { fmt.Fprintf(c.Stdout, "      "+format+"\n", args...) },
		Warn:     func(format string, args ...any) { fmt.Fprintf(c.Stderr, "magnum eval run: "+format+"\n", args...) }}
	opts := evalrun.Options{Candidate: f.candidate, JudgeOnly: f.judgeOnly, NoNotes: f.noNotes, Keep: f.keep, Force: f.force, AgentTag: evalAgentTag(run.ID), RunDir: dir}
	run.Commit, run.Dirty = evalMagnumCommit(ctx, c)
	fmt.Fprintf(c.Stdout, "eval run %s: %d case(s)%s, corpus %s, results in %s\n", run.ID, len(cases), evalLearnedCases(learned, cases),
		evalCorpusNames(path, learned), inspTilde(dir))
	if _, err := eval.LoadFlagged(evalRunsRoot(c.Layout)); err != nil { // eval/flagged.go: refused replays are never run again
		return cmdFail(c, "eval run", err)
	}
	stopped := ""
	for i, ec := range cases {
		cr := eval.CaseRun{Case: ec.Name, PR: ec.PR, Head: ec.Head, Started: now()}
		refused := ""
		if stopped == "" && ctx.Err() == nil {
			refused = evalrun.Refused(ctx, deps, ec)
		}
		switch {
		case stopped != "":
			cr.Outcome, cr.Error = "skipped", stopped
		case ctx.Err() != nil:
			cr.Outcome, cr.Error = "skipped", "interrupted"
		case refused != "":
			cr.Outcome, cr.Error = evalFlagged, refused
			fmt.Fprintf(c.Stdout, "[%d/%d] %s %s refused: %s\n", i+1, len(cases), ec.Name, ec.PR, cr.Error)
		default:
			fmt.Fprintf(c.Stdout, "[%d/%d] %s %s at %s\n", i+1, len(cases), ec.Name, ec.PR, textx.ShortSHA(ec.Head))
			cr = evalrun.RunCase(ctx, deps, ec, opts) // it flags a refused replay itself
			if cr.Outcome == pipeline.OutcomeUsageLimit || cr.Outcome == pipeline.OutcomeLoginRequired {
				stopped = "an earlier case hit " + cr.Outcome
			}
			if cr.Outcome == evalrun.OutcomeBudget {
				stopped = cr.Error
			}
		}
		cr.Finished = now()
		run.Cases = append(run.Cases, cr)
		if err := eval.SaveRun(dir, run); err != nil {
			fmt.Fprintf(c.Stderr, "magnum eval run: saving the run: %v\n", err)
		}
		if cr.Outcome != "skipped" && cr.Outcome != evalFlagged {
			fmt.Fprintf(c.Stdout, "      %s\n", evalCaseLine(cr))
		}
	}
	run.Finished = now()
	if err := evalWriteReports(dir, run); err != nil {
		return cmdFail(c, "eval run", err)
	}
	fmt.Fprintln(c.Stdout)
	eval.WriteText(c.Stdout, run, evalPrevious(c.Layout, run.ID))
	return 0
}

// evalCaseLine is the one-line outcome of a case.
func evalCaseLine(cr eval.CaseRun) string {
	took := cr.Finished.Sub(cr.Started).Round(time.Minute)
	if cr.Score == nil {
		return fmt.Sprintf("%s after %s: %s", cr.Outcome, took, cmp.Or(cr.Error, "no result to score"))
	}
	s := cr.Score
	return fmt.Sprintf("%s after %s: found %d/%d, noise %d, simplifications %d", cr.Outcome, took, s.Found, s.Total, s.Noise, s.Simplifications)
}

// evalModels records each configured role's model ("" = its kind's default).
func evalModels(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for _, role := range cfg.RolesFor(nil) {
		out[role.Name] = cfg.RoleModel(role)
	}
	return out
}

// evalMagnumCommit is the checkout's short HEAD and whether it has
// uncommitted changes (prompts or skill edits being measured).
func evalMagnumCommit(ctx context.Context, c *Context) (string, bool) {
	if c.Layout.Home == "" {
		return c.Version, false // an installed binary: its version names the prompts and skill it embeds
	}
	run := evalSys.Runner
	if run == nil {
		run = &execx.Real{}
	}
	g := gitx.New(run)
	sha, err := g.RevParse(ctx, c.Layout.Home, "HEAD")
	if err != nil {
		return "", false
	}
	st, err := g.StatusPaths(ctx, c.Layout.Home)
	return sha[:min(7, len(sha))], err == nil && len(st) > 0
}

// evalWriteReports saves run.json and report.md.
func evalWriteReports(dir string, run eval.Run) error {
	if err := eval.SaveRun(dir, run); err != nil {
		return err
	}
	var b strings.Builder
	eval.WriteMarkdown(&b, run)
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(b.String()), 0o600)
}

// evalPrevious is the run before id, nil when there is none.
func evalPrevious(l paths.Layout, id string) *eval.Run {
	runs, err := eval.ListRuns(evalRunsRoot(l))
	if err != nil {
		return nil
	}
	var prev *eval.Run
	for i := range runs {
		if runs[i].ID >= id {
			break
		}
		prev = &runs[i]
	}
	return prev
}

// evalFindRun loads RUN (an id or a unique id prefix), or the latest run.
func evalFindRun(l paths.Layout, pos []string) (eval.Run, error) {
	runs, err := eval.ListRuns(evalRunsRoot(l))
	if err != nil {
		return eval.Run{}, err
	}
	if len(runs) == 0 {
		return eval.Run{}, errors.New("no eval runs yet (magnum eval run)")
	}
	if len(pos) == 0 {
		return runs[len(runs)-1], nil
	}
	if i := slices.IndexFunc(runs, func(r eval.Run) bool { return r.ID == pos[0] }); i >= 0 {
		return runs[i], nil // a whole id, which may also prefix a later run's of the same second
	}
	matches := slices.DeleteFunc(slices.Clone(runs), func(r eval.Run) bool { return !strings.HasPrefix(r.ID, pos[0]) })
	switch len(matches) {
	case 0:
		return eval.Run{}, fmt.Errorf("no run %q (magnum eval list)", pos[0])
	case 1:
		return matches[0], nil
	}
	return eval.Run{}, fmt.Errorf("%q matches %d runs; give more of the id", pos[0], len(matches))
}

func runEvalScore(c *Context, corpusFlag string, pos []string) int {
	if len(pos) > 1 {
		return inspUsage(c, "eval score", "at most one run", "[RUN] [--corpus FILE]")
	}
	run, err := evalFindRun(c.Layout, pos)
	if err != nil {
		return cmdFail(c, "eval score", err)
	}
	user, learned := evalCorpusPath(c, corpusFlag)
	if corpusFlag == "" {
		user = cmp.Or(run.Corpus, user)
		if run.CorpusAlone { // the run read its --corpus file alone, and so does its score
			learned = ""
		}
	}
	corpus, err := eval.LoadCorpora(user, learned)
	if err != nil {
		return cmdFail(c, "eval score", err)
	}
	run = eval.Rescore(run, corpus, os.ReadFile)
	if err := evalWriteReports(filepath.Join(evalRunsRoot(c.Layout), run.ID), run); err != nil {
		return cmdFail(c, "eval score", err)
	}
	eval.WriteText(c.Stdout, run, evalPrevious(c.Layout, run.ID))
	return 0
}

func runEvalList(c *Context) int {
	runs, err := eval.ListRuns(evalRunsRoot(c.Layout))
	if err != nil {
		return cmdFail(c, "eval list", err)
	}
	if len(runs) == 0 {
		fmt.Fprintln(c.Stdout, "no eval runs yet (magnum eval run)")
		return 0
	}
	tw := inspTable(c.Stdout)
	fmt.Fprintln(tw, "RUN\tLABEL\tMAGNUM\tCASES\tRECALL\tNOISE")
	for _, r := range runs {
		t := r.Totals()
		commit := r.Commit
		if r.Dirty {
			commit += "+"
		}
		recall := "-"
		if t.Total > 0 {
			recall = fmt.Sprintf("%d/%d (%.0f%%)", t.Found, t.Total, 100*t.Recall)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%d\n", r.ID, cmp.Or(r.Label, "-"), cmp.Or(commit, "-"), t.Scored, t.Cases, recall, t.Noise)
	}
	_ = tw.Flush()
	return 0
}

func runEvalShow(c *Context, pos []string) int {
	if len(pos) > 1 {
		return inspUsage(c, "eval show", "at most one run", "[RUN]")
	}
	run, err := evalFindRun(c.Layout, pos)
	if err != nil {
		return cmdFail(c, "eval show", err)
	}
	eval.WriteText(c.Stdout, run, evalPrevious(c.Layout, run.ID))
	return 0
}
