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
// it got and what its checkout held, then writes the judge's result; it answers roundErr when that
// is set, and after cancel when that is set.
type postedFixture struct {
	d        Deps
	h        *hooksFake
	clone    string
	ec       eval.Case
	o        Options
	mu       sync.Mutex
	cases    []engine.EvalCase
	atRound  map[string]string
	roundErr error
	cancel   context.CancelFunc
}

func newPostedFixture(t *testing.T) *postedFixture {
	t.Helper()
	root := t.TempDir()
	f := &postedFixture{clone: filepath.Join(root, "widgets"), h: &hooksFake{kinds: map[string]string{"bin/setup": kindSetup, "bin/teardown": kindTeardown}}}
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
	f.seed(t, f.o, f.ec.Name)
	f.d.evalRound = func(ctx context.Context, a *app.App, _ string, c engine.EvalCase) (pipeline.RoundResult, error) {
		f.h.step("round")
		f.mu.Lock()
		f.cases = append(f.cases, c)
		f.atRound = map[string]string{}
		if b, err := os.ReadFile(filepath.Join(c.Checkout, copiedFile)); err == nil {
			f.atRound[copiedFile] = string(b)
		}
		cancel, roundErr := f.cancel, f.roundErr
		f.mu.Unlock()
		if cancel != nil {
			cancel()
			return pipeline.RoundResult{}, ctx.Err()
		}
		if roundErr != nil {
			return pipeline.RoundResult{}, roundErr
		}
		dir := a.Layout.ReviewDir(c.Owner, c.Repo, c.Number, c.Head)
		writeFile(t, filepath.Join(dir, judgeReport(cfg, &c.Watch)), questionResult)
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun, ReportDir: dir}, nil
	}
	return f
}

// seed writes the scratch registry of case name's replay in run o (app.New opens it by its path).
func (f *postedFixture) seed(t *testing.T, o Options, name string) {
	t.Helper()
	scratch := f.d.Layout
	scratch.Scratch = filepath.Join(o.RunDir, "cases", name)
	storetest.Seed(t, scratch.DB())
}

// A posted case's worktree is prepared as a local case's is: its round reviews in the worktree with
// the [[repo]] copy_files copied from the clone, after the setup ran there with the per-PR env under
// the worktree's own workspace name (magnum-eval-3-<6 hex digits>, never the PR's magnum-pr-3), and
// its teardown runs before the worktree goes. The round's roles get that worktree as their checkout,
// the one the db-lock line of each names, and the env its setup ran with, as a slot's roles do.
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
	hooks := f.h.ran()
	slug := hooks[0].Env["WT_BRANCH"]
	if !strings.HasPrefix(slug, "magnum-eval-3-") {
		t.Fatalf("WT_BRANCH %q, want the worktree's own magnum-eval-3-<6 hex digits>", slug)
	}
	for _, c := range hooks {
		if c.Dir != wt || c.Env["WT_BRANCH"] != slug {
			t.Errorf("hook %s in %s with env %v, want it in %s as %s", c.String(), c.Dir, c.Env, wt, slug)
		}
	}
	if len(f.cases) != 1 || f.cases[0].Checkout != wt || f.cases[0].MainClone != f.clone {
		t.Fatalf("the round's cases %+v, want one in %s of %s", f.cases, wt, f.clone)
	}
	if env := f.cases[0].CheckoutEnv; env["WT_BRANCH"] != slug || env["CLAUDE_CODE_WORKTREE_NAME"] != "" {
		t.Errorf("the round's checkout env %v, want the one its setup ran with", env)
	}
	if line := agents.DBLockLine("magnum", f.cases[0].Checkout, "claude-review"); !strings.Contains(line, "--checkout "+wt+" ") {
		t.Errorf("the db-lock line %q does not name the prepared worktree %s", line, wt)
	}
}

// Every case worktree's databases are its own: its workspace name is the PR's number and its path's
// (magnum-eval-<N>-<6 hex digits>), so two cases of one PR, and one case replayed by `eval run` and
// by the gate (each in its own worktree directory), set up and tear down databases apart, while a
// case replayed again in its worktree gets the same name, the one its kept worktree's teardown
// needs. None is the PR's own magnum-pr-<N>, so a case is prepared even beside the PR's per-PR
// worktree, whose databases it never touches.
func TestEveryCaseWorktreesDatabasesAreItsOwn(t *testing.T) {
	f := newPostedFixture(t)
	if err := os.MkdirAll(slots.PRWorktreePath(f.clone, 3), 0o700); err != nil {
		t.Fatal(err)
	}
	sibling := f.ec
	sibling.Name = "widgets-3-parts"
	again := Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-140000"), AgentTag: "eval-test"}
	gate := Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-150000"), AgentTag: "eval-test", Worktrees: f.d.Layout.LearnEvalWorktrees()}
	f.seed(t, f.o, sibling.Name)
	f.seed(t, again, f.ec.Name)
	f.seed(t, gate, f.ec.Name)
	names := map[string]string{}
	for _, r := range []struct {
		name string
		ec   eval.Case
		o    Options
	}{{"first", f.ec, f.o}, {"sibling", sibling, f.o}, {"again", f.ec, again}, {"gate", f.ec, gate}} {
		before := len(f.h.ran())
		cr := RunCase(context.Background(), f.d, r.ec, r.o)
		if cr.Outcome != pipeline.OutcomeDryRun || cr.Score == nil {
			t.Fatalf("%s: case run %+v", r.name, cr)
		}
		hooks := f.h.ran()[before:]
		slug := hooks[0].Env["WT_BRANCH"]
		for _, c := range hooks {
			if c.Env["WT_BRANCH"] != slug {
				t.Errorf("%s: hook %s as %s, want every hook as %s", r.name, c.String(), c.Env["WT_BRANCH"], slug)
			}
		}
		if got := f.cases[len(f.cases)-1].CheckoutEnv["WT_BRANCH"]; got != slug {
			t.Errorf("%s: the round's WT_BRANCH %q, want its setup's %q", r.name, got, slug)
		}
		if !strings.HasPrefix(slug, "magnum-eval-3-") || slug == slots.PRSlug(3) {
			t.Errorf("%s: WT_BRANCH %q, want a name of the worktree's own", r.name, slug)
		}
		names[r.name] = slug
	}
	if names["sibling"] == names["first"] || names["gate"] == names["first"] || names["again"] != names["first"] {
		t.Fatalf("names %v, want the sibling's and the gate's apart and the same case's in its worktree the same", names)
	}
}
