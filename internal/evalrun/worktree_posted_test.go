package evalrun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// widgetsDetails is GitHub's answer for example/widgets#3.
const widgetsDetails = `{"data":{"repository":{"p3":{"id":"PR_3","number":3,"title":"Widgets count their parts",
"url":"https://github.com/example/widgets/pull/3","baseRefName":"main","headRefName":"alice-widgets",
"author":{"login":"alice","__typename":"User"},"labels":{"nodes":[]},"commits":{"totalCount":1}}}}}`

// postedFixture is a posted case of example/widgets#3 at frozenHead, on a machine whose config
// watches example/* as the gh identity zhuravel with the repository's clone under the watch's
// clone_root and a [[repo]] block with a setup, a teardown and a copy_files entry the clone holds:
// gh answers the PR's details, git knows the head and adds and removes the worktree as a directory,
// the hooks are hooksFake's, and the round's engine is a fake (Deps.evalRound) that records the case
// it got and what its checkout held, then writes the judge's result.
type postedFixture struct {
	d       Deps
	h       *hooksFake
	clone   string
	ec      eval.Case
	o       Options
	mu      sync.Mutex
	cases   []engine.EvalCase
	atRound map[string]string
}

func newPostedFixture(t *testing.T) *postedFixture {
	t.Helper()
	root := t.TempDir()
	f := &postedFixture{clone: filepath.Join(root, "widgets"), h: &hooksFake{setups: map[string]bool{"bin/setup": true}}}
	if err := os.MkdirAll(filepath.Join(f.clone, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.clone, copiedFile), copiedText)
	cfg := testConfig(t)
	cfg.Identities = []config.Identity{{Name: "zhuravel", Kind: "gh", Login: "zhuravel"}}
	cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"*"}, Identity: "zhuravel", PollIdentity: "zhuravel", CloneRoot: root}}
	cfg.Repos = []config.Repo{{Repo: "example/widgets", Setup: []string{"bin/setup"}, Teardown: []string{"bin/teardown"}, CopyFiles: []string{copiedFile}}}
	cfg.Usage.CodexHome = t.TempDir()
	cfg.Herdr.Socket = filepath.Join(t.TempDir(), "no-herdr.sock")
	origin := execx.Result{Stdout: []byte("https://github.com/example/widgets.git\n")}
	run := &execx.Fake{Rules: append(f.h.rules(f.clone),
		execx.Rule{Prefix: []string{"git", "-C", f.clone, "rev-parse"}, Result: execx.Result{Stdout: []byte(frozenHead + "\n")}},
		execx.Rule{Prefix: []string{"git", "-C", f.clone, "remote"}, Result: origin},
		execx.Rule{Prefix: []string{"git", "-C", f.clone, "config"}, Result: origin},
		execx.Rule{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: []byte(widgetsDetails)}},
	)}
	l := paths.Layout{Home: t.TempDir()}
	f.d = Deps{Layout: l, Config: cfg, Runner: run, lookPath: noMise}
	f.ec = eval.Case{Name: "widgets-3", PR: "example/widgets#3", Owner: "example", Repo: "widgets", Number: 3, Head: frozenHead,
		Defects: []eval.Defect{{ID: "parts", Title: "The count", Severity: "P2", Paths: []string{"app/models/credit.rb"}, Match: []string{"rounding"}}}}
	f.o = Options{RunDir: filepath.Join(RunsRoot(l), "20261109-130000"), AgentTag: "eval-test"}
	scratch := l
	scratch.Scratch = filepath.Join(f.o.RunDir, "cases", f.ec.Name)
	storetest.Seed(t, scratch.DB()) // app.New opens the case's scratch registry by its path
	f.d.evalRound = func(_ context.Context, a *app.App, _ string, c engine.EvalCase) (pipeline.RoundResult, error) {
		f.h.step("round")
		f.mu.Lock()
		f.cases = append(f.cases, c)
		f.atRound = map[string]string{}
		if b, err := os.ReadFile(filepath.Join(c.Checkout, copiedFile)); err == nil {
			f.atRound[copiedFile] = string(b)
		}
		f.mu.Unlock()
		dir := a.Layout.ReviewDir(c.Owner, c.Repo, c.Number, c.Head)
		writeFile(t, filepath.Join(dir, judgeReport(cfg, &c.Watch)), questionResult)
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun, ReportDir: dir}, nil
	}
	return f
}

// A posted case's worktree is prepared as a local case's is: its round reviews in the worktree with
// the [[repo]] copy_files copied from the clone, after the setup ran there with the per-PR env of
// the PR's own number (WT_BRANCH=magnum-pr-3), and its teardown runs before the worktree goes. The
// round's roles get that worktree as their checkout, the one the db-lock line of each names, as a
// slot's roles do.
func TestAPostedCasesWorktreeIsPreparedToo(t *testing.T) {
	f := newPostedFixture(t)
	cr := RunCase(context.Background(), f.d, f.ec, f.o)
	if cr.Outcome != pipeline.OutcomeDryRun || cr.Score == nil {
		t.Fatalf("case run %+v", cr)
	}
	if got := f.h.said(); !slices.Equal(got, []string{"worktree add", "setup bin/setup", "round", "teardown bin/teardown", "worktree remove"}) {
		t.Fatalf("steps %q", got)
	}
	if f.atRound[copiedFile] != copiedText {
		t.Errorf("the round's worktree holds %s %q, want the clone's", copiedFile, f.atRound[copiedFile])
	}
	wt := caseWorktree(f.d, f.o, f.ec.Name)
	for _, c := range f.h.ran() {
		if c.Dir != wt || c.Env["WT_BRANCH"] != "magnum-pr-3" {
			t.Errorf("hook %s in %s with env %v, want it in %s as magnum-pr-3", c.String(), c.Dir, c.Env, wt)
		}
	}
	if len(f.cases) != 1 || f.cases[0].Checkout != wt || f.cases[0].MainClone != f.clone {
		t.Fatalf("the round's cases %+v, want one in %s of %s", f.cases, wt, f.clone)
	}
	if line := agents.DBLockLine("magnum", f.cases[0].Checkout, "claude-review"); !strings.Contains(line, "--checkout "+wt+" ") {
		t.Errorf("the db-lock line %q does not name the prepared worktree %s", line, wt)
	}
}

// A posted case whose PR has a per-PR worktree of its own next to the clone is not prepared: the two
// would share the databases of magnum-pr-<N>, which the replay's teardown drops. It ends environment
// naming that worktree, before any worktree is added or command runs.
func TestAPostedCaseIsNeverPreparedBesideThePRsOwnWorktree(t *testing.T) {
	f := newPostedFixture(t)
	own := slots.PRWorktreePath(f.clone, 3)
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	cr := RunCase(context.Background(), f.d, f.ec, f.o)
	if cr.Outcome != eval.OutcomeEnvironment || !strings.Contains(cr.Error, own) || !strings.Contains(cr.Error, "magnum-pr-3") {
		t.Fatalf("case run %+v, want the environment outcome naming %s", cr, own)
	}
	if got := f.h.said(); len(got) != 0 || len(f.cases) != 0 {
		t.Fatalf("steps %q, rounds %d: want none", got, len(f.cases))
	}
}
