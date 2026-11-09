package evalrun

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/dblock"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The files a case worktree's preparation reads in the fixtures' main clones.
const (
	copiedFile   = "config/amazons3.yml"
	copiedText   = "bucket: example-local\n"
	mainMiseText = "[env]\nGH_TOKEN = \"main-clone-token\"\nA = \"1\"\n"
)

// Kinds of the commands hooksFake runs.
const (
	kindSetup        = "setup"
	kindPostCheckout = "post_checkout"
	kindTeardown     = "teardown"
)

// hooksFake is the commands a case worktree's preparation runs, as a fake runner's rules: git adds
// the worktree as a directory (then onAdd, when set) and removes it, the hook commands (through
// /bin/sh, or mise exec for a pool's) are recorded with what they ran and where, each of the kind
// kinds names (kindTeardown when it names none); a setup runs onSetup when it is set and answers
// setupErr. steps is the order of the worktree's life ("worktree add", "<kind> <command>", "round",
// "worktree remove").
type hooksFake struct {
	mu       sync.Mutex
	steps    []string
	hooks    []execx.Cmd
	setupErr error
	kinds    map[string]string
	onSetup  func(c execx.Cmd)
	onAdd    func(wt string)
}

func (h *hooksFake) step(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.steps = append(h.steps, s)
}

func (h *hooksFake) said() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.steps)
}

func (h *hooksFake) ran() []execx.Cmd {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.hooks)
}

// rules are the fake's rules for the worktrees of clone.
func (h *hooksFake) rules(clone string) []execx.Rule {
	hook := func(script string, c execx.Cmd) (execx.Result, error) {
		h.mu.Lock()
		h.hooks = append(h.hooks, c)
		kind := cmpOr(h.kinds[script], kindTeardown)
		onSetup, setupErr := h.onSetup, h.setupErr
		h.mu.Unlock()
		h.step(kind + " " + script)
		if kind != kindSetup {
			return execx.Result{}, nil
		}
		if onSetup != nil {
			onSetup(c)
		}
		if setupErr != nil {
			return execx.Result{Code: 1}, setupErr
		}
		return execx.Result{}, nil
	}
	return []execx.Rule{
		{Prefix: []string{"git", "-C", clone, "worktree", "add"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			h.step("worktree add")
			wt := c.Args[len(c.Args)-2]
			if err := os.MkdirAll(wt, 0o700); err != nil {
				return execx.Result{}, err
			}
			if h.onAdd != nil {
				h.onAdd(wt)
			}
			return execx.Result{}, nil
		}},
		{Prefix: []string{"git", "-C", clone, "worktree", "remove"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			h.step("worktree remove")
			return execx.Result{}, os.RemoveAll(c.Args[len(c.Args)-1])
		}},
		{Prefix: []string{"/bin/sh", "-c"}, Fn: func(c execx.Cmd) (execx.Result, error) { return hook(c.Args[1], c) }},
		{Prefix: []string{"mise"}, Fn: func(c execx.Cmd) (execx.Result, error) { return hook(c.Args[len(c.Args)-1], c) }},
	}
}

// noMise is a LookPath that finds no mise: a per-PR worktree's hooks run through /bin/sh.
func noMise(string) (string, error) { return "", exec.ErrNotFound }

// prepFixture is newReplayFixture's machine whose example/example has a [[repo]] block with a
// setup, a teardown, an env and a copy_files entry the main clone holds, beside its local mise
// file: git adds and removes the case worktree as a directory, the hooks are hooksFake's, and the
// round's fake engine records what the worktree held when it started (atRound: the copied file and
// the rendered local mise file), answering roundErr when it is set, after cancel when that is set.
type prepFixture struct {
	*replayFixture
	h        *hooksFake
	atRound  map[string]string
	roundErr error
	cancel   context.CancelFunc
}

func newPrepFixture(t *testing.T) *prepFixture {
	t.Helper()
	f := &prepFixture{replayFixture: newReplayFixture(t, questionResult),
		h: &hooksFake{kinds: map[string]string{"bin/setup": kindSetup, "bin/teardown": kindTeardown}}}
	f.d.Config.Repos = []config.Repo{{Repo: "example/example", Setup: []string{"bin/setup"}, Teardown: []string{"bin/teardown"},
		CopyFiles: []string{copiedFile}, Env: map[string]string{"DB_SUFFIX": "_{slug}"}}}
	writeFile(t, filepath.Join(f.clone, copiedFile), copiedText)
	writeFile(t, filepath.Join(f.clone, "mise.local.toml"), mainMiseText)
	f.git.Rules = append(f.h.rules(f.clone), f.git.Rules...)
	f.d.lookPath = noMise
	inner := f.d.localRound
	f.d.localRound = func(ctx context.Context, cfg *config.Config, l paths.Layout, tag string, lr engine.LocalRound) (pipeline.RoundResult, error) {
		f.h.step("round")
		f.atRound = map[string]string{}
		for _, name := range []string{copiedFile, "mise.local.toml"} {
			if b, err := os.ReadFile(filepath.Join(lr.Checkout, name)); err == nil {
				f.atRound[name] = string(b)
			}
		}
		if f.cancel != nil {
			f.cancel()
			return pipeline.RoundResult{}, ctx.Err()
		}
		if f.roundErr != nil {
			return pipeline.RoundResult{}, f.roundErr
		}
		return inner(ctx, cfg, l, tag, lr)
	}
	return f
}

func (f *prepFixture) options() Options {
	return Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"}
}

// localSlugPrefix starts the workspace name of every worktree of the local case's synthetic number.
const localSlugPrefix = "magnum-eval-900011-"

// A local case's worktree is prepared as a per-PR review checkout of its repository is, before its
// round: the [[repo]] copy_files copied from the main clone, the main clone's local mise file
// rendered with the worktree's env and without the GitHub tokens, then the setup run in the
// worktree with the per-PR env under the case worktree's own workspace name (WT_BRANCH and the
// [[repo]] env's {slug}: magnum-eval-<the synthetic number>-<6 hex digits>), the daemon's secrets
// unset, under the slots' setup timeout; the round's panes get that env; its teardown runs after
// the round, with the same env under the slots' teardown timeout, before the worktree is removed.
func TestALocalCasesWorktreeIsPreparedAsAPerPRCheckoutIs(t *testing.T) {
	f := newPrepFixture(t)
	cr := RunCase(context.Background(), f.d, localEvalCase(t), f.options())
	if cr.Outcome != pipeline.OutcomeDryRun || cr.Score == nil {
		t.Fatalf("case run %+v", cr)
	}
	if got := f.h.said(); !slices.Equal(got, []string{"worktree add", "setup bin/setup", "round", "teardown bin/teardown", "worktree remove"}) {
		t.Fatalf("steps %q", got)
	}
	if f.atRound[copiedFile] != copiedText {
		t.Errorf("the round's worktree holds %s %q, want the main clone's", copiedFile, f.atRound[copiedFile])
	}
	wt := caseWorktree(f.d, f.options(), "task-1-round-1")
	hooks := f.h.ran()
	if len(hooks) != 2 || len(f.rounds) != 1 {
		t.Fatalf("hooks %v, rounds %d", hooks, len(f.rounds))
	}
	slug := hooks[0].Env["WT_BRANCH"]
	if !strings.HasPrefix(slug, localSlugPrefix) || len(slug) != len(localSlugPrefix)+6 {
		t.Fatalf("WT_BRANCH %q, want %s<6 hex digits>", slug, localSlugPrefix)
	}
	if mise := f.atRound["mise.local.toml"]; !strings.Contains(mise, `WT_BRANCH = "`+slug+`"`) || strings.Contains(mise, "GH_TOKEN") {
		t.Errorf("the round's worktree's mise.local.toml %q, want the worktree's env and no GitHub token", mise)
	}
	for i, want := range []struct {
		script  string
		timeout any
	}{{"bin/setup", slots.SetupTimeout}, {"bin/teardown", slots.TeardownTimeout}} {
		c := hooks[i]
		if c.Name != "/bin/sh" || c.Args[1] != want.script || c.Dir != wt || c.Timeout != want.timeout {
			t.Errorf("hook %d: %s in %s (timeout %s), want %s in %s (timeout %s)", i, c.String(), c.Dir, c.Timeout, want.script, wt, want.timeout)
		}
		if c.Env["WT_BRANCH"] != slug || c.Env["DB_SUFFIX"] != "_"+slug || c.Env["CLAUDE_CODE_WORKTREE_NAME"] != "" {
			t.Errorf("hook %d env %v, want the per-PR env under %s", i, c.Env, slug)
		}
		if !slices.Contains(c.Unset, "GH_TOKEN") {
			t.Errorf("hook %d unsets %v, want the daemon's GitHub tokens among them", i, c.Unset)
		}
	}
	if env := f.rounds[0].round.Env; env["WT_BRANCH"] != slug || env["DB_SUFFIX"] != "_"+slug {
		t.Errorf("the round's env %v, want the one its setup ran with", env)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("the worktree is still there: %v", err)
	}
	if log, err := os.ReadFile(filepath.Join(f.d.Layout.Logs(), caseLogName("task-1-round-1"))); err != nil || !strings.Contains(string(log), "bin/setup") {
		t.Errorf("the case's transcript %q, %v: want the setup in it", log, err)
	}
}

// A setup that writes the local mise file over the rendered one (talkable's bin/worktree-setup
// copies the main clone's) leaves the round the rendered file all the same: the file is rendered
// again after the setup, as a slot's checkout renders it before each round, so the strip_env keys
// (the GitHub tokens) never reach the round's checkout.
func TestASetupThatCopiesTheMainClonesMiseFileLeavesTheRoundTheRenderedOne(t *testing.T) {
	f := newPrepFixture(t)
	f.h.onSetup = func(c execx.Cmd) {
		if err := os.WriteFile(filepath.Join(c.Dir, "mise.local.toml"), []byte(mainMiseText), 0o600); err != nil {
			t.Errorf("the fake setup: %v", err)
		}
	}
	cr := RunCase(context.Background(), f.d, localEvalCase(t), f.options())
	if cr.Outcome != pipeline.OutcomeDryRun || cr.Score == nil {
		t.Fatalf("case run %+v", cr)
	}
	if mise := f.atRound["mise.local.toml"]; strings.Contains(mise, "GH_TOKEN") || !strings.Contains(mise, `WT_BRANCH = "`+localSlugPrefix) {
		t.Fatalf("the round's worktree's mise.local.toml %q, want the rendered one, without the GitHub token", mise)
	}
}

// A case worktree's teardown runs before the worktree is removed however the replay ends, a posted
// case's as a local one's: after a round that failed, after one cut short (its context cancelled:
// the teardown still runs), and after a setup that failed.
func TestACaseWorktreeIsTornDownHoweverTheReplayEnds(t *testing.T) {
	type replay struct {
		h        *hooksFake
		roundErr *error
		cancel   *context.CancelFunc
		run      func(context.Context) eval.CaseRun
		wt       string
	}
	fixtures := map[string]func(t *testing.T) replay{
		"local": func(t *testing.T) replay {
			t.Helper()
			f := newPrepFixture(t)
			return replay{h: f.h, roundErr: &f.roundErr, cancel: &f.cancel, wt: caseWorktree(f.d, f.options(), "task-1-round-1"),
				run: func(ctx context.Context) eval.CaseRun { return RunCase(ctx, f.d, localEvalCase(t), f.options()) }}
		},
		"posted": func(t *testing.T) replay {
			t.Helper()
			f := newPostedFixture(t)
			return replay{h: f.h, roundErr: &f.roundErr, cancel: &f.cancel, wt: caseWorktree(f.d, f.o, f.ec.Name),
				run: func(ctx context.Context) eval.CaseRun { return RunCase(ctx, f.d, f.ec, f.o) }}
		},
	}
	for kind, fixture := range fixtures {
		for name, tc := range map[string]struct {
			arrange func(replay) context.Context
			steps   []string
		}{
			"the round failed": {func(r replay) context.Context {
				*r.roundErr = errors.New("herdr: the workspace is gone")
				return context.Background()
			}, []string{"worktree add", "setup bin/setup", "round", "teardown bin/teardown", "worktree remove"}},
			"the replay was cut short": {func(r replay) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				*r.cancel = cancel
				return ctx
			}, []string{"worktree add", "setup bin/setup", "round", "teardown bin/teardown", "worktree remove"}},
			"the setup failed": {func(r replay) context.Context {
				r.h.setupErr = &execx.ExitError{Code: 1, Stderr: "config/amazons3.yml is missing\n"}
				return context.Background()
			}, []string{"worktree add", "setup bin/setup", "teardown bin/teardown", "worktree remove"}},
		} {
			t.Run(kind+": "+name, func(t *testing.T) {
				r := fixture(t)
				cr := r.run(tc.arrange(r))
				if cr.Score != nil {
					t.Fatalf("case run %+v, want no score", cr)
				}
				if got := r.h.said(); !slices.Equal(got, tc.steps) {
					t.Fatalf("steps %q, want %q", got, tc.steps)
				}
				if _, err := os.Stat(r.wt); !os.IsNotExist(err) {
					t.Errorf("the worktree is still there: %v", err)
				}
			})
		}
	}
}

// A setup that fails is the case's environment, never a miss: the case ends environment with no
// score and no round, its error naming the command and the first line of its error output, and the
// run's report counts it among the cases without a score.
func TestAFailedSetupIsTheCasesEnvironmentNotAMiss(t *testing.T) {
	f := newPrepFixture(t)
	f.h.setupErr = &execx.ExitError{Code: 1, Stderr: "\nRails boot failed: config/amazons3.yml is missing\n\tfrom bin/setup:4\n"}
	cr := RunCase(context.Background(), f.d, localEvalCase(t), f.options())
	if cr.Outcome != eval.OutcomeEnvironment || cr.Score != nil || len(f.rounds) != 0 {
		t.Fatalf("case run %+v (%d rounds), want the environment outcome and no round", cr, len(f.rounds))
	}
	first, _, _ := strings.Cut(cr.Error, "\n")
	if !strings.Contains(first, "bin/setup") || !strings.Contains(first, "Rails boot failed: config/amazons3.yml is missing") ||
		strings.Contains(first, "from bin/setup:4") {
		t.Fatalf("error %q, want its first line to name the command and its first error line", cr.Error)
	}
	if tot := (eval.Run{Cases: []eval.CaseRun{cr}}).Totals(); tot.Scored != 0 || tot.Total != 0 {
		t.Fatalf("totals %+v, want the case unscored", tot)
	}
}

// copy_files that cannot be written into the worktree (a directory of it is a symlink out of it) end
// the case environment before any setup ran, and no teardown runs for a worktree nothing set up.
func TestCopyFilesThatCannotBeWrittenEndTheCaseWithoutATeardown(t *testing.T) {
	f := newPrepFixture(t)
	outside := t.TempDir()
	f.h.onAdd = func(wt string) {
		if err := os.Symlink(outside, filepath.Join(wt, "config")); err != nil {
			t.Errorf("symlink: %v", err)
		}
	}
	cr := RunCase(context.Background(), f.d, localEvalCase(t), f.options())
	if cr.Outcome != eval.OutcomeEnvironment || !strings.Contains(cr.Error, "copy_files") || len(f.rounds) != 0 {
		t.Fatalf("case run %+v (%d rounds), want the environment outcome naming copy_files", cr, len(f.rounds))
	}
	if got := f.h.said(); !slices.Equal(got, []string{"worktree add", "worktree remove"}) {
		t.Fatalf("steps %q, want neither a setup nor a teardown", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "amazons3.yml")); !os.IsNotExist(err) {
		t.Errorf("a file was written out of the worktree: %v", err)
	}
}

// A repository with no setup and no copy_files keeps the bare worktree a replay always had: no
// command runs in it, and nothing is copied or rendered into it, even with a local mise file and the
// file in the main clone.
func TestARepositoryWithoutSetupOrCopyFilesKeepsTheBareWorktree(t *testing.T) {
	f := newPrepFixture(t)
	f.d.Config.Repos = []config.Repo{{Repo: "example/example", Env: map[string]string{"DB_SUFFIX": "_{slug}"}}}
	cr := RunCase(context.Background(), f.d, localEvalCase(t), f.options())
	if cr.Outcome != pipeline.OutcomeDryRun || cr.Score == nil {
		t.Fatalf("case run %+v", cr)
	}
	if got := f.h.said(); !slices.Equal(got, []string{"worktree add", "round", "worktree remove"}) {
		t.Fatalf("steps %q", got)
	}
	if len(f.atRound) != 0 || len(f.h.ran()) != 0 {
		t.Fatalf("the round's worktree held %v and the hooks %v, want neither", f.atRound, f.h.ran())
	}
	for _, c := range f.git.Calls {
		if c.Name != "git" {
			t.Errorf("ran %s, want git alone", c.String())
		}
	}
}

// A local case whose review checkouts have nothing to prepare while its factory copies have
// ([[factory.repo]] copy_files or setup) keeps the bare worktree, and the replay says so before its
// round: the round it replays ran in a prepared copy, so a spec run may fail where that one passed.
// A repository the factory prepares nothing for says nothing.
func TestALocalCaseOnlyTheFactoryPreparesSaysItsWorktreeIsBare(t *testing.T) {
	for name, tc := range map[string]struct {
		setup []string
		said  bool
	}{
		"the factory's copies have a setup": {[]string{"bin/factory-setup"}, true},
		"nothing prepares them":             {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPrepFixture(t)
			f.d.Config.Repos = nil
			f.d.Config.Factory.Repos[0].Setup = tc.setup
			var w warnings
			f.d.Warn = w.warn
			cr := RunCase(context.Background(), f.d, localEvalCase(t), f.options())
			if cr.Outcome != pipeline.OutcomeDryRun || cr.Score == nil {
				t.Fatalf("case run %+v", cr)
			}
			if got := f.h.said(); !slices.Equal(got, []string{"worktree add", "round", "worktree remove"}) {
				t.Fatalf("steps %q, want the bare worktree", got)
			}
			said := slices.ContainsFunc(w.said(), func(s string) bool {
				return strings.Contains(s, "task-1-round-1") && strings.Contains(s, "[[factory.repo]]") && strings.Contains(s, "bare")
			})
			if said != tc.said {
				t.Fatalf("said %q, want the bare worktree said: %v", w.said(), tc.said)
			}
		})
	}
}

// A pooled repository's case worktree is prepared with the [[pool]]'s copy_files, from the pool's
// main clone, its setup, then its post_checkout (the installs a slot's checkout runs at a head whose
// lock files it has not installed, which a fresh worktree never has, under the slots' deps timeout),
// each through mise exec as the pool's slots run them, with the pool's env (its values beside the
// names, FAKE_AWS here) under the case worktree's own workspace name, {slot} rendered as it, not a
// slot's: the case's databases are its own. The local mise file is rendered with that env, and the
// round's panes get it too.
func TestAPooledRepositorysCaseWorktreeGetsThePoolsSetup(t *testing.T) {
	f := newPrepFixture(t)
	f.h.kinds = map[string]string{"bin/worktree-setup": kindSetup, "yarn install": kindPostCheckout, "bin/worktree-archive": kindTeardown}
	poolClone := t.TempDir()
	writeFile(t, filepath.Join(poolClone, copiedFile), "bucket: example-pool\n")
	writeFile(t, filepath.Join(poolClone, "mise.local.toml"), "[env]\nGH_TOKEN = \"main-clone-token\"\nFAKE_AWS = \"0\"\n")
	f.d.Config.Repos = nil
	f.d.Config.Pools = []config.Pool{{Repo: "example/example", MainClone: poolClone, SlotName: "review{n}", Base: "main",
		CopyFiles: []string{copiedFile}, Setup: []string{"bin/worktree-setup"}, PostCheckout: []string{"yarn install"},
		Teardown: []string{"bin/worktree-archive"}, Env: map[string]string{"WT_BRANCH": "{slot}", "DB_SUFFIX": "_{slot}", "FAKE_AWS": "1"}}}
	cr := RunCase(context.Background(), f.d, localEvalCase(t), f.options())
	if cr.Outcome != pipeline.OutcomeDryRun || cr.Score == nil {
		t.Fatalf("case run %+v", cr)
	}
	if got := f.h.said(); !slices.Equal(got, []string{"worktree add", "setup bin/worktree-setup", "post_checkout yarn install", "round",
		"teardown bin/worktree-archive", "worktree remove"}) {
		t.Fatalf("steps %q", got)
	}
	if f.atRound[copiedFile] != "bucket: example-pool\n" {
		t.Errorf("the round's worktree holds %s %q, want the pool's main clone's", copiedFile, f.atRound[copiedFile])
	}
	wt := caseWorktree(f.d, f.options(), "task-1-round-1")
	env := f.rounds[0].round.Env
	slug := env["WT_BRANCH"]
	if !strings.HasPrefix(slug, localSlugPrefix) || env["DB_SUFFIX"] != "_"+slug || env["FAKE_AWS"] != "1" || env["CLAUDE_CODE_WORKTREE_NAME"] != "" {
		t.Fatalf("the round's env %v, want the pool's env under the worktree's own name", env)
	}
	if mise := f.atRound["mise.local.toml"]; !strings.Contains(mise, `FAKE_AWS = "1"`) || strings.Contains(mise, "GH_TOKEN") {
		t.Errorf("the round's mise.local.toml %q, want the pool's FAKE_AWS and no GitHub token", mise)
	}
	for _, c := range f.h.ran() {
		if c.Name != "mise" || !slices.Equal(c.Args[:2], []string{"-C", wt}) {
			t.Errorf("hook %s, want it through mise exec in %s", c.String(), wt)
		}
		for _, kv := range []string{"WT_BRANCH=" + slug, "DB_SUFFIX=_" + slug, "FAKE_AWS=1"} {
			if !slices.Contains(c.Args, kv) {
				t.Errorf("hook %s, want %s in its env", c.String(), kv)
			}
		}
		if script := c.Args[len(c.Args)-1]; script == "yarn install" && c.Timeout != slots.DepsTimeout {
			t.Errorf("post_checkout's timeout %s, want the slots' deps timeout %s", c.Timeout, slots.DepsTimeout)
		}
	}
}

// A kept worktree (Options.Keep) keeps its databases: its replay runs no teardown and leaves the
// worktree, and the next replay of the case tears it down before it replaces it, then prepares its
// own, under the same workspace name (so that teardown reaches the kept databases).
func TestAKeptCaseWorktreeKeepsItsDatabasesUntilTheNextReplayReplacesIt(t *testing.T) {
	f := newPrepFixture(t)
	kept := f.options()
	kept.Keep = true
	if cr := RunCase(context.Background(), f.d, localEvalCase(t), kept); cr.Score == nil {
		t.Fatalf("kept: case run %+v", cr)
	}
	if got := f.h.said(); !slices.Equal(got, []string{"worktree add", "setup bin/setup", "round"}) {
		t.Fatalf("kept: steps %q, want no teardown", got)
	}
	wt := caseWorktree(f.d, kept, "task-1-round-1")
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("the kept worktree: %v", err)
	}
	f.h.steps = nil
	if cr := RunCase(context.Background(), f.d, localEvalCase(t), f.options()); cr.Score == nil {
		t.Fatalf("the next replay: case run %+v", cr)
	}
	if got := f.h.said(); !slices.Equal(got, []string{"teardown bin/teardown", "worktree remove", "worktree add", "setup bin/setup", "round",
		"teardown bin/teardown", "worktree remove"}) {
		t.Fatalf("the next replay: steps %q, want the kept worktree torn down before it is replaced", got)
	}
	hooks := f.h.ran()
	for _, c := range hooks {
		if c.Env["WT_BRANCH"] != hooks[0].Env["WT_BRANCH"] {
			t.Fatalf("hook %s as %s, want every hook of the case's worktree as %s", c.String(), c.Env["WT_BRANCH"], hooks[0].Env["WT_BRANCH"])
		}
	}
}

// One case's worktree is prepared at a time, whoever replays it: a case whose preparation finds
// another's under way waits for it, saying once whom it waits for, and runs its setup once that one
// ended; one that waits out the bound (the slots' setup timeout), or whose replay is cut short while
// it waits, runs no setup, and no teardown either: nothing was set up to tear down.
func TestOneCaseWorktreeIsPreparedAtATime(t *testing.T) {
	storetest.Serial(t) // prepareWait
	ctx := context.Background()
	hold := func(t *testing.T, f *prepFixture) *dblock.Lock {
		t.Helper()
		held, err := dblock.Acquire(ctx, prepareLock(f.d.Layout), dblock.Holder{Command: "prepare other-case (run 20261109-110000)", PID: os.Getpid()}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		return held
	}
	t.Run("waits", func(t *testing.T) {
		f := newPrepFixture(t)
		held := hold(t, f)
		var w warnings
		f.d.Warn = w.warn
		done := make(chan eval.CaseRun, 1)
		go func() { done <- RunCase(ctx, f.d, localEvalCase(t), f.options()) }()
		waitFor(t, "the replay to say it waits", func() bool { return len(w.said()) > 0 })
		if got := f.h.said(); slices.Contains(got, "setup bin/setup") {
			t.Fatalf("steps %q: the setup ran while another case's preparation held the lock", got)
		}
		held.Release()
		select {
		case cr := <-done:
			if cr.Score == nil || !slices.Contains(f.h.said(), "setup bin/setup") {
				t.Fatalf("case run %+v, steps %q", cr, f.h.said())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the replay never prepared its worktree after the other's preparation ended")
		}
		if said := w.said(); len(said) != 1 || !strings.Contains(said[0], "other-case") {
			t.Fatalf("said %q, want once whom it waits for", said)
		}
	})
	t.Run("waits out the bound", func(t *testing.T) {
		defer func(d time.Duration) { prepareWait = d }(prepareWait)
		prepareWait = 300 * time.Millisecond
		f := newPrepFixture(t)
		defer hold(t, f).Release()
		cr := RunCase(ctx, f.d, localEvalCase(t), f.options())
		if cr.Outcome != eval.OutcomeEnvironment || !strings.Contains(cr.Error, "other-case") || len(f.rounds) != 0 {
			t.Fatalf("case run %+v (%d rounds), want the environment outcome naming the other case", cr, len(f.rounds))
		}
		if got := f.h.said(); !slices.Equal(got, []string{"worktree add", "worktree remove"}) {
			t.Fatalf("steps %q, want neither a setup nor a teardown", got)
		}
	})
	t.Run("is cut short while it waits", func(t *testing.T) {
		f := newPrepFixture(t)
		defer hold(t, f).Release()
		var w warnings
		f.d.Warn = w.warn
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan eval.CaseRun, 1)
		go func() { done <- RunCase(cctx, f.d, localEvalCase(t), f.options()) }()
		waitFor(t, "the replay to say it waits", func() bool { return len(w.said()) > 0 })
		cancel()
		select {
		case cr := <-done:
			if cr.Outcome != OutcomeError || cr.Score != nil {
				t.Fatalf("case run %+v, want the replay's error", cr)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the replay kept waiting after it was cut short")
		}
		if got := f.h.said(); !slices.Equal(got, []string{"worktree add", "worktree remove"}) {
			t.Fatalf("steps %q, want neither a setup nor a teardown", got)
		}
	})
}
