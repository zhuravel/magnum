// Package evalrun replays one case of an eval corpus (internal/eval) as a
// blind dry-run round and scores it: what `magnum eval run` does for each
// case, in a package the daemon can call too. Each case runs in its own
// scratch layout (registry, reports, a copy of the notes) under the run's
// directory and in a detached worktree of the repository's clone, with
// agents tagged for the run, so the live registry, the PR's own agents,
// another run's agents and the PR's reports are never touched. A candidate
// directory (WithCandidate) replays another checkout's prompts and skill.
package evalrun

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/usage"
)

// Case outcomes of RunCase besides the round's (pipeline.Outcome*).
const (
	// OutcomeFlagged: the case is not replayed: an earlier replay of it or
	// of its PR was refused (eval.FlaggedCase), or the live registry holds
	// a Codex flag for its PR (Refused).
	OutcomeFlagged = "flagged"
	// OutcomeBudget: the Codex budget is at or past [usage] codex_soft
	// (Options.Force runs anyway); no later case should start either.
	OutcomeBudget = "budget"
	// OutcomeError: the case could not run (the round's error outcome).
	OutcomeError = pipeline.OutcomeError
)

// ReleaseTimeout bounds closing a case's agents after its round.
const ReleaseTimeout = 2 * time.Minute

// ErrNoFullReplay is a judge-only replay's error when it finds no scored
// full replay of its case at the same head whose reviewers read what its own
// would to take the reviewers' reports from, or the one it finds left none.
var ErrNoFullReplay = errors.New("no full replay to take the reviewers' reports from")

// RunsRoot is where a layout's eval runs live: <state>/eval/<run id>/, with
// the flagged cases (eval.FlaggedFile) and the cases' worktrees (wt/).
func RunsRoot(l paths.Layout) string { return filepath.Join(l.State(), "eval") }

// Deps is what a replay works with.
type Deps struct {
	// Layout is the install's: its logs, locks and gh config directories,
	// the live notes a case copies and the runs root (RunsRoot). A case's
	// scratch layout is a copy of it with Scratch set.
	Layout paths.Layout
	// Config is the config the case reviews with (WithCandidate's for
	// Options.Candidate is applied by RunCase).
	Config *config.Config
	// Store is the live registry, read for the PR's Codex flag only; nil =
	// the registry file at Layout.DB(), opened read-only for each check
	// (never created).
	Store *store.Store
	// Logger is the case app's logger (nil = warnings and errors as text to
	// Stderr).
	Logger *slog.Logger
	// Stderr receives the case app's human-readable logs (app.Options).
	Stderr io.Writer
	// Now is the case's clock (nil = time.Now).
	Now func() time.Time
	// Runner replaces the case app's subprocess runner (app.Options; nil =
	// the real one).
	Runner execx.Runner
	// Progress reports a step of the case ("fetching ...", "reviewing in
	// ..."), Warn a problem that does not end it (a worktree left behind,
	// agents that stay open); nil = none.
	Progress func(format string, args ...any)
	Warn     func(format string, args ...any)
}

// Options are a case's settings, the run's.
type Options struct {
	// Candidate replays a candidate directory's prompts and skill
	// (WithCandidate); "" = Config's.
	Candidate string
	// JudgeOnly runs the judge alone on the reviewers' reports of the
	// case's newest full replay at the same head whose reviewers read what
	// this replay's would (eval.FullReplayFor: ReviewerInputs of the config
	// the case reviews with, and NoNotes), copied into the case's report
	// directory (engine.EvalCase.JudgeOnly); a case without one, or whose
	// full replay left no reviewer report, fails with ErrNoFullReplay before
	// it reads GitHub or checks anything out.
	JudgeOnly bool
	// NoNotes withholds the repository notes from the judge; Keep leaves
	// the case's worktree in place; Force runs past [usage] codex_soft.
	NoNotes, Keep, Force bool
	// AgentTag tags the case's agents (agents.Deps.Tag), the run's own;
	// required, so the agents are never named as the PR's own.
	AgentTag string
	// RunDir is the run's directory (RunsRoot/<run id>), an absolute path:
	// the case's scratch tree is RunDir/cases/<case>, and a refused replay
	// is flagged under the run's id, its base name.
	RunDir string
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

func (d Deps) progress(format string, args ...any) {
	if d.Progress != nil {
		d.Progress(format, args...)
	}
}

func (d Deps) warn(format string, args ...any) {
	if d.Warn != nil {
		d.Warn(format, args...)
	}
}

// Refused says why case ec is not replayed ("" = it may be): an earlier
// replay of it or of its PR, under any case name, was refused (the runs
// root's eval.FlaggedFile, which cannot be read refuses too), or the live
// registry holds a Codex flag for its PR (liveFlag).
func Refused(ctx context.Context, d Deps, ec eval.Case) string {
	flagged, err := eval.LoadFlagged(RunsRoot(d.Layout))
	if err != nil {
		return "the flagged replays cannot be read (" + err.Error() + "), so the case is not replayed"
	}
	if f, ok := eval.FlaggedCase(flagged, ec); ok {
		return f.Refusal()
	}
	return liveFlag(ctx, d, ec)
}

// RunCase replays case ec and scores it. Every failure becomes the case's
// outcome (OutcomeError, OutcomeBudget, OutcomeFlagged or the round's); a
// refused replay is flagged (eval.MarkFlagged) so that it never runs again.
// It starts nothing for options without an absolute RunDir or an AgentTag
// (checked first), for a case Refused names, of a repository no watch
// covers, or whose candidate is no candidate.
func RunCase(ctx context.Context, d Deps, ec eval.Case, o Options) (out eval.CaseRun) {
	cr := eval.CaseRun{Case: ec.Name, PR: ec.PR, Head: ec.Head, Started: d.now()}
	fail := func(outcome string, err error) eval.CaseRun {
		cr.Outcome, cr.Error = outcome, err.Error()
		return cr
	}
	switch {
	case !filepath.IsAbs(o.RunDir):
		return fail(OutcomeError, fmt.Errorf("evalrun: case %q needs its run's directory as an absolute path (Options.RunDir), not %q", ec.Name, o.RunDir))
	case o.AgentTag == "":
		return fail(OutcomeError, fmt.Errorf("evalrun: case %q needs its run's agent tag (Options.AgentTag), so its agents are never the PR's own", ec.Name))
	}
	if why := Refused(ctx, d, ec); why != "" {
		return fail(OutcomeFlagged, errors.New(why))
	}
	cfg := d.Config
	if o.Candidate != "" {
		var err error
		if cfg, err = WithCandidate(cfg, o.Candidate); err != nil {
			return fail(OutcomeError, err)
		}
	}
	full := ec.Owner + "/" + ec.Repo
	w := cfg.WatchFor(full)
	if w == nil {
		return fail(OutcomeError, fmt.Errorf("%s is not watched: add it to a [[watch]] so magnum knows its roles and identity", full))
	}
	var stored eval.CaseRun // the full replay whose reviewer reports a judge-only replay takes
	if o.JudgeOnly {
		runs, err := eval.ListRuns(RunsRoot(d.Layout))
		if err != nil {
			return fail(OutcomeError, err)
		}
		like := eval.Run{ReviewerInputs: ReviewerInputs(cfg), NoNotes: o.NoNotes}
		r, fc, ok := eval.FullReplayFor(runs, ec.Name, ec.Head, &like)
		if !ok || fc.ReportDir == "" {
			why := "has no scored full replay"
			if other, _, ok := eval.FullReplayFor(runs, ec.Name, ec.Head, nil); ok && other.ReviewedOtherwise(like) != "" {
				why += fmt.Sprintf(" whose reviewers read what this replay's would (the newest, run %s, does not count: %s)", other.ID, other.ReviewedOtherwise(like))
			}
			return fail(OutcomeError, fmt.Errorf("%w: case %q at %s %s; run `%s` first", ErrNoFullReplay, ec.Name, textx.ShortSHA(ec.Head), why, fullReplayCommand(ec, o)))
		}
		stored, cr.JudgeOnly, cr.ReportsRun = fc, true, r.ID
	}
	if why := budget(ctx, cfg, o.Force); why != "" {
		return fail(OutcomeBudget, errors.New(why))
	}
	cr.CodexBefore = codexUsed(ctx, cfg) // the points the case uses
	defer func() { out.CodexAfter = codexUsed(context.WithoutCancel(ctx), cfg) }()

	scratch := filepath.Join(o.RunDir, "cases", ec.Name)
	layout := d.Layout // logs, locks and the gh config dirs stay the install's
	layout.Scratch = scratch
	if err := layout.EnsureDirs(); err != nil {
		return fail(OutcomeError, err)
	}
	notes := !o.NoNotes
	if notes {
		if err := copyNotes(d.Layout, layout, ec.Owner, ec.Repo); err != nil {
			return fail(OutcomeError, fmt.Errorf("copy the repository notes: %w", err))
		}
		cr.Notes = notesHashes(layout, ec.Owner, ec.Repo)
	}
	var reports []agents.Report
	if o.JudgeOnly {
		var err error
		if reports, err = copyReports(cfg, w, stored.ReportDir, layout.ReviewDir(ec.Owner, ec.Repo, ec.Number, ec.Head)); err != nil {
			return fail(OutcomeError, fmt.Errorf("copy the reviewers' reports of run %s: %w", cr.ReportsRun, err))
		}
		if !slices.ContainsFunc(reports, func(r agents.Report) bool { return r.Status == pipeline.ReportOK }) {
			return fail(OutcomeError, fmt.Errorf("%w: run %s left no reviewer report in %s (removed since?); run `%s` first",
				ErrNoFullReplay, cr.ReportsRun, tilde(stored.ReportDir), fullReplayCommand(ec, o)))
		}
	}
	logger := d.Logger
	if logger == nil {
		logger = app.NewLogger(nil, d.Stderr, slog.LevelWarn)
	}
	a, err := app.New(cfg, layout, app.Options{AgentTag: o.AgentTag, Stderr: d.Stderr, Logger: logger, Runner: d.Runner})
	if err != nil {
		return fail(OutcomeError, err)
	}
	defer a.Close()

	mainClone, err := clone(ctx, a, cfg, w, ec)
	if err != nil {
		return fail(OutcomeError, err)
	}
	gh := a.GitHub(w.PollIdentity)
	if gh == nil {
		return fail(OutcomeError, fmt.Errorf("no GitHub client for poll identity %q", w.PollIdentity))
	}
	details, _, err := gh.Details(ctx, ec.Owner, ec.Repo, []int{ec.Number})
	if err != nil {
		return fail(OutcomeError, fmt.Errorf("read %s from GitHub: %w", ec.PR, err))
	}
	pd, ok := details[ec.Number]
	if !ok {
		return fail(OutcomeError, fmt.Errorf("%s not found on GitHub", ec.PR))
	}
	d.progress("fetching %s into %s", ec.Head[:12], tilde(mainClone))
	if err := a.Git.FetchCommit(ctx, mainClone, ec.Head, ec.Number); err != nil {
		return fail(OutcomeError, err)
	}
	// One path per case across runs: the agents trust the checkout they
	// start in (Codex's and Claude's own config files), and a new path per
	// run would add an entry there every time. A worktree left by Keep or
	// an interrupted run is replaced.
	worktrees := filepath.Join(RunsRoot(d.Layout), "wt")
	wt := filepath.Join(worktrees, ec.Name)
	if _, err := os.Stat(wt); err == nil {
		if err := a.Git.WorktreeRemove(ctx, mainClone, wt, true); err != nil {
			return fail(OutcomeError, fmt.Errorf("replace the previous worktree %s: %w", tilde(wt), err))
		}
	}
	if err := a.Git.WorktreeAdd(ctx, mainClone, wt, ec.Head, true, ""); err != nil {
		return fail(OutcomeError, err)
	}
	if !o.Keep {
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if err := a.Git.WorktreeRemove(cctx, mainClone, wt, true); err != nil {
				d.warn("removing worktree %s: %v", tilde(wt), err)
			}
		}()
	}

	e := engine.FromApp(a)
	d.progress("reviewing in herdr workspace \"eval %s#%d\", agents tagged %s (blind, nothing is posted)", ec.Repo, ec.Number, a.AgentTag)
	res, pr, runErr := e.RunEval(ctx, engine.EvalCase{
		Owner: ec.Owner, Repo: ec.Repo, Number: ec.Number, URL: cmp.Or(pd.URL, fmt.Sprintf("https://github.com/%s/pull/%d", full, ec.Number)),
		Head: ec.Head, BaseRef: cmp.Or(ec.Base, pd.BaseRefName), DefaultBranch: cmp.Or(ec.Base, pd.BaseRefName),
		Title: pd.Title, Author: pd.AuthorLogin, AuthorType: pd.AuthorType,
		Checkout: wt, Worktrees: worktrees, MainClone: mainClone, Watch: *w, Identity: w.Identity, Notes: notes,
		JudgeOnly: o.JudgeOnly, Reports: reports,
	})
	if pr.ID != 0 {
		// A judge still at work is interrupted, then the workspace closed;
		// the error names each workspace that stays open.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReleaseTimeout)
		if err := e.ReleaseEval(rctx, pr); err != nil {
			d.warn("%s: closing the eval agents: %v (close the herdr workspace by hand)", ec.Name, err)
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
	if cr.Outcome == pipeline.OutcomeRefused {
		if err := eval.MarkFlagged(RunsRoot(d.Layout), eval.Flagged{Case: ec.Name, PR: ec.PR, Head: ec.Head, Run: filepath.Base(o.RunDir),
			Reason: cr.Error, At: d.now()}); err != nil {
			d.warn("%v", err)
		}
	}
	if res.ReportDir == "" {
		return cr
	}
	cr.ResultFile = filepath.Join(res.ReportDir, judgeReport(cfg, w))
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

// fullReplayCommand is the `magnum eval run` that makes the full replay a
// judge-only replay of ec with o's settings can take its reports from: the
// same candidate and notes setting, without --judge-only.
func fullReplayCommand(ec eval.Case, o Options) string {
	cmd := "magnum eval run --case " + ec.Name
	if o.Candidate != "" {
		cmd += " --candidate " + tilde(o.Candidate)
	}
	if o.NoNotes {
		cmd += " --no-notes"
	}
	return cmd
}

// budget says why no case may start now ("" = it may): the Codex budget at
// or past [usage] codex_soft, which real reviews need more, unless force.
func budget(ctx context.Context, cfg *config.Config, force bool) string {
	soft := cfg.Usage.CodexSoft
	if force || soft <= 0 {
		return ""
	}
	snap, err := usage.Codex(ctx, cfg.Usage.CodexHome, time.Now())
	if err != nil {
		return "" // no gauge: the round's own health checks stop at a limit
	}
	if used := snap.Used(); used >= soft {
		return fmt.Sprintf("Codex budget at %.0f%%, past [usage] codex_soft %.0f%%: real reviews come first (--force to run anyway)", used, soft)
	}
	return ""
}

// codexUsed is the Codex budget used now (usage.Codex: the newest snapshot
// of the account's sessions), nil when there is none to read.
func codexUsed(ctx context.Context, cfg *config.Config) *float64 {
	snap, err := usage.Codex(ctx, cfg.Usage.CodexHome, time.Now())
	if err != nil {
		return nil
	}
	used := snap.Used()
	return &used
}

// clone is the clone a case's worktree is added to: the pool's main clone,
// else the existing clone under the watch's clone_root. A replay never
// clones.
func clone(ctx context.Context, a *app.App, cfg *config.Config, w *config.Watch, ec eval.Case) (string, error) {
	full := ec.Owner + "/" + ec.Repo
	if p := cfg.PoolFor(full); p != nil {
		return p.MainClone, nil
	}
	dir, err := a.Git.FindClone(ctx, w.CloneRoot, ec.Owner, ec.Repo)
	if err != nil {
		return "", fmt.Errorf("no clone of %s under %s: %w\nfix: gh repo clone %s %s", full, tilde(w.CloneRoot), err,
			full, filepath.Join(tilde(w.CloneRoot), ec.Repo))
	}
	return dir, nil
}

// judgeReport is the file name of the watch's judge result.
func judgeReport(cfg *config.Config, w *config.Watch) string {
	for _, role := range cfg.RolesFor(w) {
		if role.Judge {
			return role.ReportFile()
		}
	}
	return "codex-judge.json"
}

// copyNotes copies the live notes of owner/repo (the .md file and its
// harness directory) into the scratch layout, so the judge reads what a real
// round would and its rewrite stays in the scratch tree. No notes: nothing.
func copyNotes(live, scratch paths.Layout, owner, repo string) error {
	src, dst := engine.NotesPath(live, owner, repo), engine.NotesPath(scratch, owner, repo)
	if src == "" || dst == "" {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
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
		return copyFile(p, filepath.Join(dstDir, rel))
	})
}

// copyReports copies the reviewer reports of a full replay (its report
// directory from) into a judge-only replay's (to) and lists them for the
// round (engine.EvalCase.Reports): one per reviewer of the watch that a
// replay's round runs (config.Role.ShouldRun of a first round), with the
// copy when the full replay left its report, else missing, as the judge of
// the full replay was told.
func copyReports(cfg *config.Config, w *config.Watch, from, to string) ([]agents.Report, error) {
	var out []agents.Report
	for _, role := range cfg.RolesFor(w) {
		if role.Judge || !role.ShouldRun(false, false) {
			continue
		}
		src, dst := filepath.Join(from, role.ReportFile()), filepath.Join(to, role.ReportFile())
		st, err := os.Stat(src)
		switch {
		case errors.Is(err, fs.ErrNotExist) || err == nil && st.Size() == 0:
			out = append(out, agents.Report{Role: role.Name, Status: pipeline.ReportMissing, Missing: true})
			continue
		case err != nil:
			return nil, err
		}
		if err := copyFile(src, dst); err != nil {
			return nil, err
		}
		out = append(out, agents.Report{Role: role.Name, Path: dst, Status: pipeline.ReportOK})
	}
	return out, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// notesHashes is what notes a case's judge reads: the hashes of the scratch
// copy of owner/repo's notes (eval.HashNotes), taken before the round can
// rewrite them; nil when they cannot be read.
func notesHashes(scratch paths.Layout, owner, repo string) map[string]string {
	file := engine.NotesPath(scratch, owner, repo)
	if file == "" {
		return nil
	}
	m, err := eval.HashNotes(file, engine.NotesDir(scratch, owner, repo))
	if err != nil {
		return nil
	}
	return m
}

// liveFlag is why case ec is not replayed when the live registry holds a
// Codex flag for its PR (store.KVPRCodexFlag; "" = none): the flag's
// sentence, or why the registry could not say, which refuses the case too
// (a flag that cannot be read counts). Without Deps.Store the registry
// file is opened read-only, never created; a registry that is not there,
// or does not know the PR, holds no flag.
func liveFlag(ctx context.Context, d Deps, ec eval.Case) string {
	cannot := func(err error) string {
		return "the registry cannot say whether Codex flagged " + ec.PR + " (" + err.Error() + "), so the case is not replayed"
	}
	if d.Store != nil {
		repo, err := d.Store.RepoByFullName(ctx, ec.Owner+"/"+ec.Repo)
		if errors.Is(err, store.ErrNotFound) {
			return ""
		}
		if err != nil {
			return cannot(err)
		}
		pr, err := d.Store.PRByRepoNumber(ctx, repo.ID, ec.Number)
		if errors.Is(err, store.ErrNotFound) {
			return ""
		}
		if err != nil {
			return cannot(err)
		}
		v, ok, err := d.Store.GetKV(ctx, store.KVPRCodexFlag(pr.ID))
		switch {
		case err != nil:
			return cannot(err)
		case !ok:
			return ""
		}
		return engine.ParseCodexFlag(v).Sentence(ec.PR)
	}
	path := d.Layout.DB()
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	db, err := sql.Open("sqlite", readOnlyDSN(path))
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

// readOnlyDSN opens the registry at path read-only, never creating it.
func readOnlyDSN(path string) string {
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}
	return u.String()
}

// tilde shortens a path under $HOME to ~/...
func tilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || p == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rel
	}
	return p
}
