package cli

// `magnum eval`: replay a corpus of PRs with known defects at pinned heads,
// as blind dry-run rounds, and report how many of the defects the review
// found (recall) and how many other findings it posted (noise). Each case
// runs in its own scratch layout (registry, reports, notes copy) and a
// detached worktree of the repository's clone, with agents tagged "eval",
// so the live registry, the PR's own agents and its reports are never
// touched. The corpus is a local, gitignored file: it describes defects in
// real code.

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
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
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/usage"
)

const (
	evalUsage    = "run|score|list|show"
	evalRunUsage = "[--corpus FILE] [--case NAME]... [--label TEXT] [--no-notes] [--keep] [--force]"
	// evalCorpusFile is the default corpus, in the user config's directory
	// (~/.config/magnum); evalLegacyCorpusFile its old place in a checkout.
	evalCorpusFile       = "eval.toml"
	evalLegacyCorpusFile = "eval.local.toml"
	// evalAgentTag tags eval agents (agents.Deps.Tag).
	evalAgentTag = "eval"
	// evalReleaseTimeout bounds quitting a case's agents after its round.
	evalReleaseTimeout = 2 * time.Minute
	// evalFlagged is the outcome of a case that is not replayed: an earlier
	// replay of it or of its PR was refused (eval.FlaggedCase), or the live
	// registry holds a Codex flag for its PR (evalLiveFlag).
	evalFlagged = "flagged"
)

// evalRunsRoot is where runs live: state/eval/<run id>/.
func evalRunsRoot(l paths.Layout) string { return filepath.Join(l.State(), "eval") }

func newEvalCmd(c *Context) *cobra.Command {
	cmd := newCommand(groupAct, "eval "+evalUsage, "replay PRs with known defects and score the reviews (recall, noise)",
		"Measure the review pipeline on a corpus of PRs with known defects, after a prompt, skill or model change: "+
			"`magnum eval run` replays each case at its pinned head as a blind dry run (nothing is posted, the roles "+
			"are told not to read the PR's reviews, comments or later commits) and reports, per case, how many of the "+
			"seeded defects the planned review found, at what severity, and how many other findings it made.\n\n"+
			"The corpus is ~/.config/magnum/"+evalCorpusFile+" (a checkout's eval.local.toml still works; see eval.toml.example). Each case runs "+
			"in a scratch registry and report tree under state/eval/<run>/ and a detached worktree of the repository's "+
			"clone, with agents named apart from the PR's own; the live registry, reports and notes are never "+
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
	corpus, label        string
	cases                []string
	noNotes, keep, force bool
}

func newEvalRunCmd(c *Context) *cobra.Command {
	var f evalRunFlags
	cmd := newCommand("", "run "+evalRunUsage, "replay the corpus (or --case ones) and score it",
		"Replay every case of the corpus (or each --case) one after the other and print the report, compared "+
			"with the previous run. --label names the run (for example the change it measures). --no-notes "+
			"withholds the repository notes (by default the judge reads a scratch copy of them). --keep leaves "+
			"each case's worktree in place. --force runs past the Codex soft cap.",
		func(pos []string) int {
			if len(pos) > 0 {
				return inspUsage(c, "eval run", "no arguments expected", evalRunUsage)
			}
			return runEvalRun(c, f)
		})
	fs := cmd.Flags()
	fs.StringVar(&f.corpus, "corpus", "", "corpus file (default: "+evalCorpusFile+" next to config.toml)")
	fs.StringArrayVar(&f.cases, "case", nil, "only this case (repeatable)")
	fs.StringVar(&f.label, "label", "", "a name for this run")
	fs.BoolVar(&f.noNotes, "no-notes", false, "do not give the judge the repository notes")
	fs.BoolVar(&f.keep, "keep", false, "keep each case's worktree")
	fs.BoolVar(&f.force, "force", false, "run even when the Codex budget is past [usage] codex_soft")
	return cmd
}

func newEvalScoreCmd(c *Context) *cobra.Command {
	var corpus string
	cmd := newCommand("", "score [RUN] [--corpus FILE]", "re-score a run against the corpus as it is now",
		"Re-read each case's result file of RUN (default: the latest run) and score it again with the corpus's "+
			"current defects, so a match rule can be corrected without running the agents again. The run's "+
			"report is rewritten.",
		func(pos []string) int { return runEvalScore(c, corpus, pos) })
	cmd.Flags().StringVar(&corpus, "corpus", "", "corpus file (default: "+evalCorpusFile+" next to config.toml)")
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

// evalCorpusPath resolves --corpus.
func evalCorpusPath(c *Context, flag string) string {
	if flag != "" {
		return flag
	}
	dir := c.Layout.ConfigDir()
	if dir == "" {
		dir = filepath.Join(paths.Expand("~"), ".config", "magnum")
	}
	user := filepath.Join(dir, evalCorpusFile)
	if legacy := filepath.Join(c.Layout.Home, evalLegacyCorpusFile); c.Layout.Home != "" && !fsx.Exists(user) && fsx.Exists(legacy) {
		return legacy
	}
	return user
}

func runEvalRun(c *Context, f evalRunFlags) int {
	if err := c.LoadConfig(); err != nil {
		return cmdFail(c, "eval run", fmt.Errorf("%w (fix config.toml; `magnum config` validates it)", err))
	}
	path := evalCorpusPath(c, f.corpus)
	corpus, err := eval.LoadCorpus(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cmdFail(c, "eval run", fmt.Errorf("no corpus at %s\nfix: copy eval.toml.example to %s and describe PRs with known defects", inspTilde(path), inspTilde(path)))
		}
		return cmdFail(c, "eval run", err)
	}
	cases, err := corpus.Select(f.cases)
	if err != nil {
		return cmdFail(c, "eval run", err)
	}
	ctx, stop := signalContext()
	defer stop()

	r := &evalRunner{c: c, cfg: c.Config, flags: f, out: c.Stdout}
	run := eval.Run{
		ID: time.Now().Format("20060102-150405"), Label: f.label, Corpus: path, Started: time.Now(),
		Models: evalModels(c.Config), Inputs: evalInputs(c.Config), NoNotes: f.noNotes, // eval_inputs.go
	}
	run.Commit, run.Dirty = evalMagnumCommit(ctx, c)
	r.dir = filepath.Join(evalRunsRoot(c.Layout), run.ID)
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return cmdFail(c, "eval run", err)
	}
	fmt.Fprintf(c.Stdout, "eval run %s: %d case(s), corpus %s, results in %s\n", run.ID, len(cases), inspTilde(path), inspTilde(r.dir))
	flagged, err := eval.LoadFlagged(evalRunsRoot(c.Layout)) // eval/flagged.go: refused replays are never run again
	if err != nil {
		return cmdFail(c, "eval run", err)
	}
	stopped := ""
	for i, ec := range cases {
		cr := eval.CaseRun{Case: ec.Name, PR: ec.PR, Head: ec.Head, Started: time.Now()}
		refused := ""
		if f, ok := eval.FlaggedCase(flagged, ec); ok {
			refused = f.Refusal()
		} else if stopped == "" && ctx.Err() == nil {
			refused = evalLiveFlag(ctx, c, ec)
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
			cr = r.runCase(ctx, ec, cr)
			if cr.Outcome == pipeline.OutcomeRefused {
				if err := eval.MarkFlagged(evalRunsRoot(c.Layout), eval.Flagged{Case: ec.Name, PR: ec.PR, Head: ec.Head, Run: run.ID,
					Reason: cr.Error, At: time.Now()}); err != nil {
					fmt.Fprintf(c.Stderr, "magnum eval run: %v\n", err)
				}
			}
			if cr.Outcome == pipeline.OutcomeUsageLimit || cr.Outcome == pipeline.OutcomeLoginRequired {
				stopped = "an earlier case hit " + cr.Outcome
			}
			if cr.Outcome == "budget" {
				stopped = cr.Error
			}
		}
		cr.Finished = time.Now()
		run.Cases = append(run.Cases, cr)
		if err := eval.SaveRun(r.dir, run); err != nil {
			fmt.Fprintf(c.Stderr, "magnum eval run: saving the run: %v\n", err)
		}
		if cr.Outcome != "skipped" && cr.Outcome != evalFlagged {
			fmt.Fprintf(c.Stdout, "      %s\n", evalCaseLine(cr))
		}
	}
	run.Finished = time.Now()
	if err := evalWriteReports(r.dir, run); err != nil {
		return cmdFail(c, "eval run", err)
	}
	fmt.Fprintln(c.Stdout)
	eval.WriteText(c.Stdout, run, evalPrevious(c.Layout, run.ID))
	return 0
}

// evalLiveFlag is why case ec is not replayed when the live registry holds a
// Codex flag for its PR (store.KVPRCodexFlag; "" = none): the flag's
// sentence, or why the registry could not say, which refuses the case too
// (a flag that cannot be read counts). The registry is opened read-only,
// never created; a registry that is not there, or does not know the PR,
// holds no flag.
func evalLiveFlag(ctx context.Context, c *Context, ec eval.Case) string {
	path := c.Layout.DB()
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	cannot := func(err error) string {
		return "the registry cannot say whether Codex flagged " + ec.PR + " (" + err.Error() + "), so the case is not replayed"
	}
	db, err := sql.Open("sqlite", registryReadOnlyDSN(path))
	if err != nil {
		return cannot(err)
	}
	defer db.Close()
	var id int64
	err = db.QueryRowContext(ctx, `SELECT p.id FROM prs p JOIN repos r ON r.id = p.repo_id
WHERE lower(r.owner) = lower(?) AND lower(r.name) = lower(?) AND p.number = ?`, ec.Owner, ec.Repo, ec.Number).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		return cannot(err)
	}
	var v string
	err = db.QueryRowContext(ctx, "SELECT value FROM kv WHERE key = ?", store.KVPRCodexFlag(id)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		return cannot(err)
	}
	return engine.ParseCodexFlag(v).Sentence(ec.PR)
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

// evalRunner runs the cases of one run.
type evalRunner struct {
	c     *Context
	cfg   *config.Config
	flags evalRunFlags
	out   io.Writer
	dir   string
}

func (r *evalRunner) progress(format string, args ...any) {
	fmt.Fprintf(r.out, "      "+format+"\n", args...)
}

// runCase replays one case and scores it. Every failure becomes the case's
// outcome; the run goes on with the next case.
func (r *evalRunner) runCase(ctx context.Context, ec eval.Case, cr eval.CaseRun) (out eval.CaseRun) {
	fail := func(outcome string, err error) eval.CaseRun {
		cr.Outcome, cr.Error = outcome, err.Error()
		return cr
	}
	full := ec.Owner + "/" + ec.Repo
	w := r.cfg.WatchFor(full)
	if w == nil {
		return fail("error", fmt.Errorf("%s is not watched: add it to a [[watch]] so magnum knows its roles and identity", full))
	}
	if why := r.budget(ctx); why != "" {
		return fail("budget", errors.New(why))
	}
	cr.CodexBefore = evalCodexUsed(ctx, r.cfg) // the points the case uses (eval_inputs.go)
	defer func() { out.CodexAfter = evalCodexUsed(context.WithoutCancel(ctx), r.cfg) }()

	scratch := filepath.Join(r.dir, "cases", ec.Name)
	layout := r.c.Layout // logs, locks and the gh config dirs stay the install's
	layout.Scratch = scratch
	if err := layout.EnsureDirs(); err != nil {
		return fail("error", err)
	}
	notes := !r.flags.noNotes
	if notes {
		if err := evalCopyNotes(r.c.Layout, layout, ec.Owner, ec.Repo); err != nil {
			return fail("error", fmt.Errorf("copy the repository notes: %w", err))
		}
		cr.Notes = evalNotesHashes(layout, ec.Owner, ec.Repo) // eval_inputs.go
	}
	a, err := app.New(r.cfg, layout, app.Options{
		AgentTag: evalAgentTag, Stderr: r.c.Stderr,
		Logger: app.NewLogger(nil, r.c.Stderr, slog.LevelWarn),
	})
	if err != nil {
		return fail("error", err)
	}
	defer a.Close()

	mainClone, err := evalClone(ctx, a, r.cfg, w, ec)
	if err != nil {
		return fail("error", err)
	}
	gh := a.GitHub(w.PollIdentity)
	if gh == nil {
		return fail("error", fmt.Errorf("no GitHub client for poll identity %q", w.PollIdentity))
	}
	details, _, err := gh.Details(ctx, ec.Owner, ec.Repo, []int{ec.Number})
	if err != nil {
		return fail("error", fmt.Errorf("read %s from GitHub: %w", ec.PR, err))
	}
	d, ok := details[ec.Number]
	if !ok {
		return fail("error", fmt.Errorf("%s not found on GitHub", ec.PR))
	}
	r.progress("fetching %s into %s", ec.Head[:12], inspTilde(mainClone))
	if err := a.Git.FetchCommit(ctx, mainClone, ec.Head, ec.Number); err != nil {
		return fail("error", err)
	}
	// One path per case across runs: the agents trust the checkout they
	// start in (Codex's and Claude's own config files), and a new path per
	// run would add an entry there every time. A worktree left by --keep or
	// an interrupted run is replaced.
	wt := filepath.Join(evalRunsRoot(r.c.Layout), "wt", ec.Name)
	if _, err := os.Stat(wt); err == nil {
		if err := a.Git.WorktreeRemove(ctx, mainClone, wt, true); err != nil {
			return fail("error", fmt.Errorf("replace the previous worktree %s: %w", inspTilde(wt), err))
		}
	}
	if err := a.Git.WorktreeAdd(ctx, mainClone, wt, ec.Head, true, ""); err != nil {
		return fail("error", err)
	}
	if !r.flags.keep {
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if err := a.Git.WorktreeRemove(cctx, mainClone, wt, true); err != nil {
				fmt.Fprintf(r.c.Stderr, "magnum eval run: removing worktree %s: %v\n", inspTilde(wt), err)
			}
		}()
	}

	e := engine.FromApp(a)
	r.progress("reviewing in herdr workspace \"%s %s#%d\" (blind, nothing is posted)", evalAgentTag, ec.Repo, ec.Number)
	res, pr, runErr := e.RunEval(ctx, engine.EvalCase{
		Owner: ec.Owner, Repo: ec.Repo, Number: ec.Number, URL: cmp.Or(d.URL, fmt.Sprintf("https://github.com/%s/pull/%d", full, ec.Number)),
		Head: ec.Head, BaseRef: cmp.Or(ec.Base, d.BaseRefName), DefaultBranch: cmp.Or(ec.Base, d.BaseRefName),
		Title: d.Title, Author: d.AuthorLogin, AuthorType: d.AuthorType,
		Checkout: wt, MainClone: mainClone, Watch: *w, Identity: w.Identity, Notes: notes,
	})
	if pr.ID != 0 {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), evalReleaseTimeout)
		if err := e.ReleaseEval(rctx, pr); err != nil {
			fmt.Fprintf(r.c.Stderr, "magnum eval run: %s: closing the eval agents: %v (close the herdr workspace by hand)\n", ec.Name, err)
		}
		cancel()
	}
	cr.ReportDir = res.ReportDir
	cr.Outcome = cmp.Or(res.Outcome, pipeline.OutcomeError)
	if runErr != nil {
		cr.Error = runErr.Error()
	} else if res.Error != "" {
		cr.Error = res.Error
	}
	if res.ReportDir == "" {
		return cr
	}
	cr.ResultFile = filepath.Join(res.ReportDir, evalJudgeReport(r.cfg, w))
	data, err := os.ReadFile(cr.ResultFile)
	if err != nil {
		cr.Error = cmp.Or(cr.Error, "no judge result: "+err.Error())
		return cr
	}
	result, err := eval.ParseResult(data)
	if err != nil {
		cr.Error = cmp.Or(cr.Error, err.Error())
		return cr
	}
	score := eval.ScoreCase(ec, result)
	cr.Score = &score
	return cr
}

// budget says why no case may start now ("" = it may): the Codex budget at
// or past [usage] codex_soft, which real reviews need more, unless --force.
func (r *evalRunner) budget(ctx context.Context) string {
	soft := r.cfg.Usage.CodexSoft
	if r.flags.force || soft <= 0 {
		return ""
	}
	snap, err := usage.Codex(ctx, r.cfg.Usage.CodexHome, time.Now())
	if err != nil {
		return "" // no gauge: the round's own health checks stop at a limit
	}
	if used := snap.Used(); used >= soft {
		return fmt.Sprintf("Codex budget at %.0f%%, past [usage] codex_soft %.0f%%: real reviews come first (--force to run anyway)", used, soft)
	}
	return ""
}

// evalClone is the clone a case's worktree is added to: the pool's main
// clone, else the existing clone under the watch's clone_root. eval never
// clones.
func evalClone(ctx context.Context, a *app.App, cfg *config.Config, w *config.Watch, ec eval.Case) (string, error) {
	full := ec.Owner + "/" + ec.Repo
	if p := cfg.PoolFor(full); p != nil {
		return p.MainClone, nil
	}
	dir, err := a.Git.FindClone(ctx, w.CloneRoot, ec.Owner, ec.Repo)
	if err != nil {
		return "", fmt.Errorf("no clone of %s under %s: %w\nfix: gh repo clone %s %s", full, inspTilde(w.CloneRoot), err,
			full, filepath.Join(inspTilde(w.CloneRoot), ec.Repo))
	}
	return dir, nil
}

// evalJudgeReport is the file name of the watch's judge result.
func evalJudgeReport(cfg *config.Config, w *config.Watch) string {
	for _, role := range cfg.RolesFor(w) {
		if role.Judge {
			return role.ReportFile()
		}
	}
	return "codex-judge.json"
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
	g := gitx.New(&execx.Real{})
	sha, err := g.RevParse(ctx, c.Layout.Home, "HEAD")
	if err != nil {
		return "", false
	}
	st, err := g.StatusPaths(ctx, c.Layout.Home)
	return sha[:min(7, len(sha))], err == nil && len(st) > 0
}

// evalCopyNotes copies the live notes of owner/repo (the .md file and its
// harness directory) into the scratch layout, so the judge reads what a real
// round would and its rewrite stays in the scratch tree. No notes: nothing.
func evalCopyNotes(live, scratch paths.Layout, owner, repo string) error {
	src, dst := engine.NotesPath(live, owner, repo), engine.NotesPath(scratch, owner, repo)
	if src == "" || dst == "" {
		return nil
	}
	if err := evalCopyFile(src, dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	srcDir, dstDir := engine.NotesDir(live, owner, repo), engine.NotesDir(scratch, owner, repo)
	return filepath.WalkDir(srcDir, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(srcDir, p)
		if de.IsDir() {
			return os.MkdirAll(filepath.Join(dstDir, rel), 0o700)
		}
		if !de.Type().IsRegular() {
			return nil
		}
		return evalCopyFile(p, filepath.Join(dstDir, rel))
	})
}

func evalCopyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
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
	corpus, err := eval.LoadCorpus(cmp.Or(corpusFlag, run.Corpus, evalCorpusPath(c, "")))
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
