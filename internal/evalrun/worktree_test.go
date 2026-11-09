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

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
)

// The files a case worktree's preparation reads in the fixtures' main clones.
const (
	copiedFile   = "config/amazons3.yml"
	copiedText   = "bucket: example-local\n"
	mainMiseText = "[env]\nGH_TOKEN = \"main-clone-token\"\nA = \"1\"\n"
)

// hooksFake is the commands a case worktree's preparation runs, as a fake runner's rules: git adds
// the worktree as a directory and removes it, the setup and teardown commands (through /bin/sh, or
// mise exec for a pool's) are recorded with what they ran and where, the setup answering setupErr;
// steps is the order of the worktree's life ("worktree add", "setup <command>", "round",
// "teardown <command>", "worktree remove").
type hooksFake struct {
	mu       sync.Mutex
	steps    []string
	hooks    []execx.Cmd
	setupErr error
	setups   map[string]bool // the setup commands (the rest are teardowns)
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
		setup := h.setups[script]
		h.mu.Unlock()
		if setup {
			h.step("setup " + script)
			if h.setupErr != nil {
				return execx.Result{Code: 1}, h.setupErr
			}
			return execx.Result{}, nil
		}
		h.step("teardown " + script)
		return execx.Result{}, nil
	}
	return []execx.Rule{
		{Prefix: []string{"git", "-C", clone, "worktree", "add"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			h.step("worktree add")
			return execx.Result{}, os.MkdirAll(c.Args[len(c.Args)-2], 0o700)
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
	f := &prepFixture{replayFixture: newReplayFixture(t, questionResult), h: &hooksFake{setups: map[string]bool{"bin/setup": true}}}
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

// A local case's worktree is prepared as a per-PR review checkout of its repository is, before its
// round: the [[repo]] copy_files copied from the main clone, the main clone's local mise file
// rendered with the per-PR env and without the GitHub tokens, then the setup run in the worktree
// with the per-PR env of the case's synthetic number (WT_BRANCH=magnum-pr-900011, the [[repo]]
// env), the daemon's secrets unset, under the slots' setup timeout; its teardown runs after the
// round, with the same env under the slots' teardown timeout, before the worktree is removed.
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
	if mise := f.atRound["mise.local.toml"]; !strings.Contains(mise, `WT_BRANCH = "magnum-pr-900011"`) || strings.Contains(mise, "GH_TOKEN") {
		t.Errorf("the round's worktree's mise.local.toml %q, want the per-PR env and no GitHub token", mise)
	}
	wt := caseWorktree(f.d, f.options(), "task-1-round-1")
	hooks := f.h.ran()
	if len(hooks) != 2 {
		t.Fatalf("hooks %v", hooks)
	}
	for i, want := range []struct {
		script  string
		timeout any
	}{{"bin/setup", slots.SetupTimeout}, {"bin/teardown", slots.TeardownTimeout}} {
		c := hooks[i]
		if c.Name != "/bin/sh" || c.Args[1] != want.script || c.Dir != wt || c.Timeout != want.timeout {
			t.Errorf("hook %d: %s in %s (timeout %s), want %s in %s (timeout %s)", i, c.String(), c.Dir, c.Timeout, want.script, wt, want.timeout)
		}
		if c.Env["WT_BRANCH"] != "magnum-pr-900011" || c.Env["DB_SUFFIX"] != "_magnum-pr-900011" || c.Env["CLAUDE_CODE_WORKTREE_NAME"] != "" {
			t.Errorf("hook %d env %v, want the per-PR env of the synthetic number", i, c.Env)
		}
		if !slices.Contains(c.Unset, "GH_TOKEN") {
			t.Errorf("hook %d unsets %v, want the daemon's GitHub tokens among them", i, c.Unset)
		}
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("the worktree is still there: %v", err)
	}
	if log, err := os.ReadFile(filepath.Join(f.d.Layout.Logs(), caseLogName("task-1-round-1"))); err != nil || !strings.Contains(string(log), "bin/setup") {
		t.Errorf("the case's transcript %q, %v: want the setup in it", log, err)
	}
}

// A case worktree's teardown runs before the worktree is removed however the replay ends: after a
// round that failed, after one cut short (its context cancelled: the teardown still runs), and after
// a setup that failed.
func TestACaseWorktreeIsTornDownHoweverTheReplayEnds(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(*prepFixture) context.Context
		steps   []string
	}{
		"the round failed": {func(f *prepFixture) context.Context {
			f.roundErr = errors.New("herdr: the workspace is gone")
			return context.Background()
		}, []string{"worktree add", "setup bin/setup", "round", "teardown bin/teardown", "worktree remove"}},
		"the replay was cut short": {func(f *prepFixture) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			f.cancel = cancel
			return ctx
		}, []string{"worktree add", "setup bin/setup", "round", "teardown bin/teardown", "worktree remove"}},
		"the setup failed": {func(f *prepFixture) context.Context {
			f.h.setupErr = &execx.ExitError{Code: 1, Stderr: "config/amazons3.yml is missing\n"}
			return context.Background()
		}, []string{"worktree add", "setup bin/setup", "teardown bin/teardown", "worktree remove"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPrepFixture(t)
			ctx := tc.arrange(f)
			cr := RunCase(ctx, f.d, localEvalCase(t), f.options())
			if cr.Score != nil {
				t.Fatalf("case run %+v, want no score", cr)
			}
			if got := f.h.said(); !slices.Equal(got, tc.steps) {
				t.Fatalf("steps %q, want %q", got, tc.steps)
			}
			if _, err := os.Stat(caseWorktree(f.d, f.options(), "task-1-round-1")); !os.IsNotExist(err) {
				t.Errorf("the worktree is still there: %v", err)
			}
		})
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

// A pooled repository's case worktree is prepared with the [[pool]]'s copy_files, from the pool's
// main clone, and the pool's setup and teardown, run through mise exec as the pool's slots run them,
// but with the per-PR env of the case's number, not a slot's: the case's databases are its own.
func TestAPooledRepositorysCaseWorktreeGetsThePoolsSetup(t *testing.T) {
	f := newPrepFixture(t)
	f.h.setups = map[string]bool{"bin/worktree-setup": true}
	poolClone := t.TempDir()
	writeFile(t, filepath.Join(poolClone, copiedFile), "bucket: example-pool\n")
	f.d.Config.Repos = nil
	f.d.Config.Pools = []config.Pool{{Repo: "example/example", MainClone: poolClone, SlotName: "review{n}", Base: "main",
		CopyFiles: []string{copiedFile}, Setup: []string{"bin/worktree-setup"}, Teardown: []string{"bin/worktree-archive"},
		Env: map[string]string{"WT_BRANCH": "{slot}"}}}
	cr := RunCase(context.Background(), f.d, localEvalCase(t), f.options())
	if cr.Outcome != pipeline.OutcomeDryRun || cr.Score == nil {
		t.Fatalf("case run %+v", cr)
	}
	if got := f.h.said(); !slices.Equal(got, []string{"worktree add", "setup bin/worktree-setup", "round", "teardown bin/worktree-archive", "worktree remove"}) {
		t.Fatalf("steps %q", got)
	}
	if f.atRound[copiedFile] != "bucket: example-pool\n" {
		t.Errorf("the round's worktree holds %s %q, want the pool's main clone's", copiedFile, f.atRound[copiedFile])
	}
	wt := caseWorktree(f.d, f.options(), "task-1-round-1")
	for _, c := range f.h.ran() {
		if c.Name != "mise" || !slices.Equal(c.Args[:2], []string{"-C", wt}) || !slices.Contains(c.Args, "WT_BRANCH=magnum-pr-900011") {
			t.Errorf("hook %s, want it through mise exec in %s with the per-PR WT_BRANCH", c.String(), wt)
		}
	}
}
