package staging

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

// A clean deploy of PR 1 to void over a real origin: the plan shows the
// tip, a clean merge and its migration; the effect claims the env, merges,
// verifies and pushes, and watch and confirm read the fake GitHub. The
// merge is [tip, head] by the bot with git's message, origin's staging-void
// moves to it by a fast-forward, and every step's begin and ok is an event
// by the CLI.
func TestACleanDeployReachesConfirmWithTheBotsTwoParentMerge(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	p := w.plan(c, 1)
	want := EnvPlan{Env: void, Tip: w.tip, Clean: true, Migrations: []string{"db/migrate/20261108_create_things.rb"}}
	if len(p.Envs) != 1 || !planEqual(p.Envs[0], want) {
		t.Fatalf("plan: %+v, want %+v", p.Envs, want)
	}
	ids, err := c.Deploy(context.Background(), p, ByCLI)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	e := w.effect(c, ids[0])
	if e.State != EffectRunning || e.Step != StepPlan || e.By != ByCLI {
		t.Fatalf("after Deploy: %+v, want running at plan, claimed, by cli", e)
	}
	e = w.finish(c, ids[0])
	if e.State != EffectDone || !strings.HasPrefix(e.Why, "live") || e.RunURL != runURL || e.Try != 1 {
		t.Fatalf("finished: %+v, want done, live, with ci.yml's run", e)
	}
	parents, author, committer, subject := w.commitOf(e.Merge)
	if !slices.Equal(parents, []string{w.tip, w.heads[1]}) {
		t.Errorf("the merge's parents: %q, want [tip, head]", parents)
	}
	if bot := botName + " <" + botEmail + ">"; author != bot || committer != bot {
		t.Errorf("author %q, committer %q, want the bot as both", author, committer)
	}
	if subject != "Merge branch 'pr1' into staging-void" {
		t.Errorf("message %q", subject)
	}
	if got := w.originTip(); got != e.Merge {
		t.Errorf("origin's staging-void is %s, want the merge %s", short(got), short(e.Merge))
	}
	if w.run.pushed() != 1 {
		t.Errorf("pushes: %d, want 1", w.run.pushed())
	}
	var steps []string
	for _, ev := range w.events {
		f := strings.Fields(ev)
		steps = append(steps, f[0]+" "+f[1])
	}
	wantSteps := []string{"claim begin", "claim ok", "plan begin", "plan ok", "merge begin", "merge ok", "verify begin", "verify ok",
		"push begin", "push ok", "watch begin", "watch ok", "confirm begin", "confirm ok"}
	if !slices.Equal(steps, wantSteps) {
		t.Errorf("events:\n%s\nwant\n%s", strings.Join(steps, "\n"), strings.Join(wantSteps, "\n"))
	}
}

// planEqual compares two env plans.
func planEqual(a, b EnvPlan) bool {
	return a.Env == b.Env && a.Tip == b.Tip && a.Clean == b.Clean && slices.Equal(a.Conflicts, b.Conflicts) &&
		slices.Equal(a.Migrations, b.Migrations) && a.Workflows == b.Workflows && a.Why == b.Why
}

// A conflict with no resolver configured ends the effect failed at merge,
// naming the conflicted paths; nothing is pushed.
func TestAConflictWithoutAResolverFailsNamingThePaths(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	p := w.plan(c, 2)
	if got := p.Envs[0]; got.Clean || !slices.Equal(got.Conflicts, []string{"app/s.rb"}) {
		t.Fatalf("plan: %+v, want a conflict in app/s.rb", got)
	}
	ids, err := c.Deploy(context.Background(), p, ByCLI)
	if err != nil {
		t.Fatal(err)
	}
	e := w.drive(c, ids[0], ended)
	if e.State != EffectFailed || e.Step != StepMerge || !strings.Contains(e.Why, "conflict: resolver not configured") || !strings.Contains(e.Why, "app/s.rb") {
		t.Errorf("ended: %+v, want failed at merge, the resolver not configured, naming app/s.rb", e)
	}
	w.noPushSince(0, w.tip)
}

// A resolver's job that runs, then is done with a merge in its own
// repository: the merge is rebuilt in the mirror, verified (its tree
// differs from merge-tree's only in the conflicted path) and pushed; the
// request named the effect, the target and its tip, the head and try 1,
// and the resolved ref is gone once the effect ended.
func TestAResolversMergeIsVerifiedThenPushed(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	res := w.withResolver()
	c := w.client()
	defer c.Close()
	id := w.deploy(c, 2)
	e := w.drive(c, id, func(e Effect) bool { return e.Step == StepMerge && len(res.requests()) == 1 || ended(e) })
	if e.State != EffectRunning || !strings.Contains(e.Why, "resolver works on it") {
		t.Fatalf("with the job: %+v, want running at merge", e)
	}
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := w.effect(c, id); e.Step != StepMerge || e.State != EffectRunning {
		t.Fatalf("while the job runs: %+v, want still at merge", e)
	}
	want := ResolveRequest{Repo: "acme/app", Ref: "acme/app#2", Target: "staging-void", TargetTip: w.tip, Head: w.heads[2], Effect: string(id), Try: 1}
	if got := res.requests(); len(got) != 1 || got[0] != want {
		t.Fatalf("resolver requests: %+v, want %+v", got, want)
	}
	dir, merge := w.resolverRepo(2, []string{w.tip, w.heads[2]}, map[string]string{"app/s.rb": "staging and theirs\n"})
	res.answer(ResolveResult{State: ResolveDone, MergeSHA: merge, Dir: dir})
	e = w.finish(c, id)
	if e.State != EffectDone || e.Merge != merge {
		t.Fatalf("finished: %+v, want done with the resolver's merge %s", e, short(merge))
	}
	if got := w.originTip(); got != merge {
		t.Errorf("origin's staging-void is %s, want the resolver's merge", short(got))
	}
	if got := gitIn(t, w.r, w.mirror, "for-each-ref", resolvedPrefix); got != "" {
		t.Errorf("resolved refs left in the mirror: %s", got)
	}
}

// A resolver's merge whose parents are not [tip, head] is refused at its
// hand-off (TestAResolvedMergeWithOtherParentsIsRefusedAtTheHandOff) and
// never pushed, as one that changes a path outside the conflicts is
// (TestAResolvedMergeThatChangesAnUnconflictedPathIsRefused).
func TestAResolversMergeOutsideTheRulesIsNeverPushed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		parents func(w *deployWorld) []string
		files   map[string]string
		why     string
	}{
		{"the parents reversed", func(w *deployWorld) []string { return []string{w.heads[2], w.tip} },
			map[string]string{"app/s.rb": "resolved\n"}, "parents are"},
		{"one parent", func(w *deployWorld) []string { return []string{w.tip} },
			map[string]string{"app/s.rb": "resolved\n"}, "parents are"},
		{"master as the first parent", func(w *deployWorld) []string { return []string{w.c1, w.heads[2]} },
			map[string]string{"app/s.rb": "resolved\n"}, "parents are"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			res := w.withResolver()
			c := w.client()
			defer c.Close()
			id := w.deploy(c, 2)
			w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 || ended(e) })
			dir, merge := w.resolverRepo(2, tc.parents(w), tc.files)
			res.answer(ResolveResult{State: ResolveDone, MergeSHA: merge, Dir: dir})
			e := w.drive(c, id, ended)
			if e.State != EffectFailed || e.Step != StepMerge || !strings.Contains(e.Why, tc.why) || !strings.Contains(e.Why, "nothing was pushed") {
				t.Errorf("ended: %+v, want failed at merge, %q", e, tc.why)
			}
			w.noPushSince(0, w.tip)
			if got := gitIn(t, w.r, w.mirror, "for-each-ref", resolvedPrefix); got != "" {
				t.Errorf("resolved refs left in the mirror: %s", got)
			}
		})
	}
}

// A resolver that fails, or names a repository that is a URL, relative,
// missing or the mirror itself: the effect fails, nothing is pushed.
func TestAResolversFailureOrForeignRepositoryFailsTheEffect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		res  func(w *deployWorld) ResolveResult
		why  string
	}{
		{"failed", func(*deployWorld) ResolveResult {
			return ResolveResult{State: ResolveFailed, Why: "specs red\nsecond line"}
		},
			"conflict: the resolver failed: specs red"},
		{"a URL", func(w *deployWorld) ResolveResult {
			return ResolveResult{State: ResolveDone, MergeSHA: w.heads[2], Dir: "https://github.com/acme/app.git"}
		}, "refused"},
		{"a relative path", func(w *deployWorld) ResolveResult {
			return ResolveResult{State: ResolveDone, MergeSHA: w.heads[2], Dir: "resolver/repo"}
		}, "refused"},
		{"a missing dir", func(w *deployWorld) ResolveResult {
			return ResolveResult{State: ResolveDone, MergeSHA: w.heads[2], Dir: filepath.Join(w.t.TempDir(), "gone")}
		}, "refused"},
		{"the mirror", func(w *deployWorld) ResolveResult {
			return ResolveResult{State: ResolveDone, MergeSHA: w.heads[2], Dir: w.mirror}
		}, "the mirror itself"},
		{"a merge it does not hold", func(w *deployWorld) ResolveResult {
			dir, _ := w.resolverRepo(2, []string{w.tip, w.heads[2]}, map[string]string{"app/s.rb": "x\n"})
			return ResolveResult{State: ResolveDone, MergeSHA: strings.Repeat("ab", 20), Dir: dir}
		}, "the resolver's repository does not hold its merge abababababab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			res := w.withResolver()
			c := w.client()
			defer c.Close()
			id := w.deploy(c, 2)
			w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 || ended(e) })
			res.answer(tc.res(w))
			e := w.drive(c, id, ended)
			if e.State != EffectFailed || !strings.Contains(e.Why, tc.why) || strings.Contains(e.Why, "second line") {
				t.Errorf("ended: %+v, want failed, %q", e, tc.why)
			}
			w.noPushSince(0, w.tip)
		})
	}
}

// The resolver's repository is a local absolute dir that git calls a
// repository, never a URL, a relative or dash-led path, a missing dir, a
// plain dir, or the mirror.
func TestTheResolversRepositoryMustBeALocalRepositoryOtherThanTheMirror(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	g := w.deps.Git.(deployGit)
	plain := t.TempDir()
	work, _ := w.resolverRepo(2, []string{w.tip, w.heads[2]}, map[string]string{"app/s.rb": "x\n"})
	for _, tc := range []struct {
		dir, want string // want: the git dir, "" = refused
	}{
		{"https://github.com/acme/app.git", ""},
		{"file:///tmp/x", ""},
		{"relative/repo", ""},
		{"-upload-pack=evil", ""},
		{"/" + strings.Repeat("x", 8) + "/missing", ""},
		{plain, ""},
		{w.mirror, ""},
		{work + "/../" + filepath.Base(work), ""},
		{work, filepath.Join(work, ".git")},
		{w.origin, w.origin},
	} {
		got, err := g.checkResolverDir(context.Background(), tc.dir, w.mirror)
		if tc.want == "" && err == nil || tc.want != "" && (err != nil || got != tc.want) {
			t.Errorf("checkResolverDir(%q) = %q, %v; want %q", tc.dir, got, err, tc.want)
		}
	}
}

// The tip moves between the plan step and each merge (a coworker's push):
// verify sees it and merges again on the new tip, at most twice; the third
// move fails the effect with nothing pushed. A single move merges once
// more and pushes [the new tip, head], the coworker's commit kept.
func TestAMovedTipMergesAgainAtMostTwice(t *testing.T) {
	t.Parallel()
	t.Run("every time", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		c := w.client()
		defer c.Close()
		tips := []string{}
		c.hook = func(_ EffectID, step, point string) error {
			if step == StepMerge && point == "begin" {
				tips = append(tips, w.coworker(nil))
			}
			return nil
		}
		id := w.deploy(c, 1)
		e := w.drive(c, id, ended)
		if e.State != EffectFailed || e.Try != 3 || !strings.Contains(e.Why, "moved 3 times") || !strings.Contains(e.Why, "nothing was pushed") {
			t.Errorf("ended: %+v, want failed on try 3, moved 3 times", e)
		}
		w.noPushSince(0, tips[len(tips)-1])
		if len(tips) != 3 {
			t.Errorf("merges: %d, want 3", len(tips))
		}
	})
	t.Run("once", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		c := w.client()
		defer c.Close()
		var moved string
		c.hook = func(_ EffectID, step, point string) error {
			if step == StepMerge && point == "begin" && moved == "" {
				moved = w.coworker(nil)
			}
			return nil
		}
		id := w.deploy(c, 1)
		e := w.finish(c, id)
		if e.State != EffectDone || e.Try != 2 {
			t.Fatalf("ended: %+v, want done on try 2", e)
		}
		if parents, _, _, _ := w.commitOf(e.Merge); !slices.Equal(parents, []string{moved, w.heads[1]}) {
			t.Errorf("the merge's parents: %q, want [the moved tip, head]", parents)
		}
		if w.run.pushed() != 1 {
			t.Errorf("pushes: %d, want 1", w.run.pushed())
		}
	})
}

// A coworker's push lands right before ours: origin rejects ours (not a
// fast-forward), so the effect merges again on the new tip and pushes
// that; the coworker's commit stays on the branch (no force).
func TestANonFastForwardRejectionMergesAgain(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	var theirs string
	w.run.beforePush = func() {
		if theirs == "" {
			theirs = w.coworker(nil)
		}
	}
	id := w.deploy(c, 1)
	e := w.finish(c, id)
	if e.State != EffectDone || e.Try != 2 {
		t.Fatalf("ended: %+v, want done on try 2", e)
	}
	if parents, _, _, _ := w.commitOf(e.Merge); !slices.Equal(parents, []string{theirs, w.heads[1]}) {
		t.Errorf("the merge's parents: %q, want [the coworker's tip, head]", parents)
	}
	if got := w.run.pushed(); got != 2 {
		t.Errorf("pushes: %d, want the rejected one and the next", got)
	}
	gitIn(t, w.r, w.origin, "merge-base", "--is-ancestor", theirs, "refs/heads/staging-void")
}

// The tip moves after the plan, and the merge on the new tip now adds a
// migration (or changes a workflow) the operator's plan did not show: a
// coworker removed the copy the plan's tip had. The effect stops, needs
// you, naming it, before anything is pushed; a new plan and Deploy of the
// same PR and head resumes the same effect.
func TestAMergeThatNowAddsWhatThePlanDidNotShowNeedsYou(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, path, why string }{
		{"a migration", "db/migrate/20261108_add_flag.rb", "adds migrations you did not see: db/migrate/20261108_add_flag.rb"},
		{"a workflow", ".github/workflows/ci.yml", "changes .github/workflows paths you did not see: .github/workflows/ci.yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			w.pr(3, "pr3", map[string]string{tc.path: "same\n", "app/c.rb": "c\n"})
			w.coworker(map[string]string{tc.path: "same\n"}) // staging already has it
			c := w.client()
			defer c.Close()
			p := w.plan(c, 3)
			if got := p.Envs[0]; len(got.Migrations) != 0 || got.Workflows {
				t.Fatalf("plan: %+v, want no migration or workflow change (the tip has them)", got)
			}
			moved := w.coworker(nil, tc.path)
			ids, err := c.Deploy(context.Background(), p, ByLiaison)
			if err != nil {
				t.Fatal(err)
			}
			e := w.drive(c, ids[0], ended)
			if e.State != EffectNeedsYou || !strings.Contains(e.Why, tc.why) {
				t.Errorf("ended: %+v, want needs you, %q", e, tc.why)
			}
			w.noPushSince(0, moved)
			again, err := c.Deploy(context.Background(), w.plan(c, 3), ByCLI)
			if err != nil || len(again) != 1 || again[0] != ids[0] {
				t.Fatalf("Deploy again: %v %v, want the same effect %s resumed", again, err, ids[0])
			}
			if e := w.drive(c, ids[0], at(StepWatch)); e.State != EffectRunning || e.By != ByCLI {
				t.Errorf("resumed: %+v, want running at watch, by cli", e)
			}
		})
	}
}

// A merge that changes .github/workflows, shown in the plan, goes on to
// PushEnv, whose refusal fails the effect with its words; a nil PushEnv
// fails it at push, "push not configured". Nothing is pushed either way.
func TestPushEnvDecidesThePush(t *testing.T) {
	t.Parallel()
	t.Run("not configured", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		w.deps.PushEnv = nil
		c := w.client()
		defer c.Close()
		e := w.drive(c, w.deploy(c, 1), ended)
		if e.State != EffectFailed || e.Step != StepPush || !strings.Contains(e.Why, "push not configured") {
			t.Errorf("ended: %+v, want failed at push, push not configured", e)
		}
		w.noPushSince(0, w.tip)
	})
	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		w.pr(3, "pr3", map[string]string{".github/workflows/ci.yml": "on: push\n"})
		var asked []string
		w.deps.PushEnv = func(_ context.Context, repo, sha, target string) (map[string]string, error) {
			asked = append(asked, repo+" "+short(sha)+" "+target)
			return nil, errors.New("needs Workflows on the App")
		}
		c := w.client()
		defer c.Close()
		p := w.plan(c, 3)
		if !p.Envs[0].Workflows {
			t.Fatalf("plan: %+v, want Workflows", p.Envs[0])
		}
		ids, err := c.Deploy(context.Background(), p, ByFactory)
		if err != nil {
			t.Fatal(err)
		}
		e := w.drive(c, ids[0], ended)
		if e.State != EffectFailed || e.Why != "push refused: needs Workflows on the App" || len(asked) != 1 || !strings.HasSuffix(asked[0], " staging-void") {
			t.Errorf("ended: %+v, PushEnv asked %q; want failed with its refusal", e, asked)
		}
		w.noPushSince(0, w.tip)
	})
}

// The commit identity: none, one that errors, or one that is no single
// line ends the effect at merge, and no commit is made.
func TestAMergeWithoutACommitIdentityMakesNoCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		id   func(context.Context, string) (string, string, error)
	}{
		{"nil", nil},
		{"an error", func(context.Context, string) (string, string, error) { return "", "", errors.New("no bot") }},
		{"a newline", func(context.Context, string) (string, string, error) { return "bot\nx", botEmail, nil }},
		{"a NUL", func(context.Context, string) (string, string, error) { return botName, "bot@example.com\x00", nil }},
		{"too long", func(context.Context, string) (string, string, error) {
			return strings.Repeat("b", maxIdent+1), botEmail, nil
		}},
		{"an ident delimiter", func(context.Context, string) (string, string, error) { return "bot <evil@example.com>", botEmail, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			w.deps.CommitIdentity = tc.id
			c := w.client()
			defer c.Close()
			e := w.drive(c, w.deploy(c, 1), ended)
			if e.State != EffectFailed || e.Step != StepMerge || !strings.HasPrefix(e.Why, "commit identity not configured") || e.Merge != "" {
				t.Errorf("ended: %+v, want failed at merge, commit identity not configured", e)
			}
			if w.run.committed() != 0 {
				t.Errorf("commit-tree ran %d times, want none", w.run.committed())
			}
			w.noPushSince(0, w.tip)
		})
	}
}

// The merge's message names the PR's head branch: HeadRef, else the one
// GitHub reads, else "Merge pull request #N".
func TestTheMergeMessageNamesTheHeadBranch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, headRef string
		github        map[int]string
		want          string
	}{
		{"HeadRef", "pr1", nil, "Merge branch 'pr1' into staging-void"},
		{"from GitHub", "", map[int]string{1: "feature/things"}, "Merge branch 'feature/things' into staging-void"},
		{"a bad HeadRef, GitHub's", "pr 1\n", map[int]string{1: "feature/things"}, "Merge branch 'feature/things' into staging-void"},
		{"GitHub's no branch name", "", map[int]string{1: "-x\nevil"}, "Merge pull request #1 into staging-void"},
		{"none", "", nil, "Merge pull request #1 into staging-void"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			w.gh.heads = tc.github
			c := w.client()
			defer c.Close()
			pr := w.prOf(1)
			pr.HeadRef = tc.headRef
			p, err := c.Plan(context.Background(), pr, []Env{void})
			if err != nil {
				t.Fatal(err)
			}
			ids, err := c.Deploy(context.Background(), p, ByCLI)
			if err != nil {
				t.Fatal(err)
			}
			e := w.drive(c, ids[0], at(StepWatch))
			if _, _, _, subject := w.commitOf(e.Merge); subject != tc.want {
				t.Errorf("message %q, want %q", subject, tc.want)
			}
		})
	}
}

// The push's argv guard: the builder's own argv passes, and every other
// form is refused, whatever builds it: a +, a force in any spelling, a
// mirror, a delete, a refspec that is no full sha onto
// refs/heads/staging-<name> of the allowlist (a delete's empty source, a
// ref name, a glob, two refspecs, master, production), a remote other than
// origin, another git dir or extra options.
func TestThePushArgvGuardRefusesEveryOtherForm(t *testing.T) {
	t.Parallel()
	dir := "/state/staging/acme/app.git"
	sha := strings.Repeat("a", 40)
	allowed := func(branch string) bool { return branch == "staging-void" }
	good, err := pushArgv(dir, sha, "staging-void", allowed)
	if err != nil {
		t.Fatalf("pushArgv: %v", err)
	}
	if tail := good[len(good)-3:]; !slices.Equal(tail, []string{"push", "origin", sha + ":refs/heads/staging-void"}) {
		t.Fatalf("the push's tail: %q", tail)
	}
	if err := checkPushArgv(good, dir, allowed); err != nil {
		t.Fatalf("the builder's argv refused: %v", err)
	}
	with := func(tail ...string) []string { return append(slices.Clone(good[:len(good)-3]), tail...) }
	for name, args := range map[string][]string{
		"a forced refspec":             with("push", "origin", "+"+sha+":refs/heads/staging-void"),
		"--force":                      with("push", "--force", "origin", sha+":refs/heads/staging-void"),
		"-f":                           with("push", "-f", "origin", sha+":refs/heads/staging-void"),
		"--force-with-lease":           with("push", "--force-with-lease", "origin", sha+":refs/heads/staging-void"),
		"--force-with-lease=x":         with("push", "--force-with-lease=staging-void", "origin", sha+":refs/heads/staging-void"),
		"--force-if-includes":          with("push", "--force-if-includes", "origin", sha+":refs/heads/staging-void"),
		"--mirror":                     with("push", "--mirror", "origin"),
		"--delete":                     with("push", "--delete", "origin", "staging-void"),
		"a delete's empty source":      with("push", "origin", ":refs/heads/staging-void"),
		"a ref name for a source":      with("push", "origin", "refs/heads/master:refs/heads/staging-void"),
		"a short sha":                  with("push", "origin", sha[:12]+":refs/heads/staging-void"),
		"an upper-case sha":            with("push", "origin", strings.ToUpper(sha)+":refs/heads/staging-void"),
		"a glob":                       with("push", "origin", "refs/heads/*:refs/heads/*"),
		"master":                       with("push", "origin", sha+":refs/heads/master"),
		"main":                         with("push", "origin", sha+":refs/heads/main"),
		"production":                   with("push", "origin", sha+":refs/heads/production"),
		"a staging branch not allowed": with("push", "origin", sha+":refs/heads/staging-bart"),
		"a branch without refs/heads":  with("push", "origin", sha+":staging-void"),
		"a tag":                        with("push", "origin", sha+":refs/tags/staging-void"),
		"two refspecs":                 with("push", "origin", sha+":refs/heads/staging-void", sha+":refs/heads/staging-void"),
		"another remote":               with("push", "upstream", sha+":refs/heads/staging-void"),
		"a URL remote":                 with("push", "https://github.com/acme/app.git", sha+":refs/heads/staging-void"),
		"--all":                        with("push", "--all", "origin"),
		"--tags":                       with("push", "--tags", "origin", sha+":refs/heads/staging-void"),
		"--prune":                      with("push", "--prune", "origin", sha+":refs/heads/staging-void"),
		"--receive-pack":               with("push", "--receive-pack=evil", "origin", sha+":refs/heads/staging-void"),
		"a fetch":                      with("fetch", "origin", sha+":refs/heads/staging-void"),
		"another git dir":              append([]string{"--git-dir=/elsewhere.git"}, good[1:]...),
		"an extra -c":                  append([]string{good[0], "-c", "push.default=matching"}, good[1:]...),
	} {
		if err := checkPushArgv(args, dir, allowed); err == nil {
			t.Errorf("%s: %q passed the guard", name, args)
		}
	}
	for _, branch := range []string{"staging-bart", "master", "production", "staging-void "} {
		if _, err := pushArgv(dir, sha, branch, allowed); err == nil {
			t.Errorf("pushArgv built a push to %q", branch)
		}
	}
	if _, err := pushArgv(dir, "", "staging-void", allowed); err == nil {
		t.Error("pushArgv built a push of no sha (a delete)")
	}
	if _, err := pushArgv(dir, sha, "staging-void", nil); err == nil {
		t.Error("pushArgv built a push without an allowlist")
	}
}

// The allowlist, in Plan, Deploy and at the push alike: an env must be one
// of Stagings(repo), its branch staging-<name>, never master, main, the
// remote's default branch or production*; with no Stagings every deploy is
// refused.
func TestTheAllowlistRefusesProductionTheDefaultBranchAndAnUnconfiguredEnv(t *testing.T) {
	t.Parallel()
	c := New(Deps{Stagings: func(string) []Env {
		return []Env{void, {Name: "production", Branch: "staging-production"}, {Name: "main", Branch: "main"}, {Name: "x", Branch: "master"}}
	}})
	for _, tc := range []struct {
		env  Env
		base string
		ok   bool
	}{
		{void, "master", true},
		{void, "staging-void", false}, // the remote's default branch
		{void, "", false},
		{Env{Name: "production", Branch: "staging-production"}, "master", false},
		{Env{Name: "Production-eu", Branch: "staging-Production-eu"}, "master", false},
		{Env{Name: "main", Branch: "main"}, "master", false},
		{Env{Name: "x", Branch: "master"}, "trunk", false},
		{Env{Name: "bart", Branch: "staging-bart"}, "master", false}, // not configured
		{Env{Name: "void", Branch: "staging-bart"}, "master", false},
		{Env{Name: "void", Branch: "staging-void/x"}, "master", false},
	} {
		if err := c.allowed("acme/app", tc.env, tc.base); (err == nil) != tc.ok {
			t.Errorf("allowed(%+v, base %q) = %v, want ok %v", tc.env, tc.base, err, tc.ok)
		}
	}
	if err := New(Deps{}).allowed("acme/app", void, "master"); err == nil {
		t.Error("no Stagings allowed void")
	}

	w := newDeployWorld(t)
	prod := Env{Name: "production", Branch: "staging-production"}
	w.deps.Stagings = func(string) []Env { return []Env{void, prod} }
	c = w.client()
	defer c.Close()
	if _, err := c.Plan(context.Background(), w.prOf(1), []Env{void, prod}); err == nil || !strings.Contains(err.Error(), "production is never staging") {
		t.Errorf("Plan with production: %v, want refused", err)
	}
	p := w.plan(c, 1)
	forged := p
	forged.Envs = append(slices.Clone(p.Envs), EnvPlan{Env: prod, Tip: w.tip, Clean: true})
	if ids, err := c.Deploy(context.Background(), forged, ByCLI); err == nil || len(ids) != 0 {
		t.Errorf("Deploy with production: %v %v, want refused, nothing started", ids, err)
	}
	if es := c.Effects("acme/app"); len(es) != 0 {
		t.Errorf("effects after a refused Deploy: %+v", es)
	}

	// The default branch, read from the mirror's HEAD: staging-void is it.
	gitIn(t, w.r, w.mirror, "symbolic-ref", "HEAD", "refs/heads/staging-void")
	if _, err := c.Plan(context.Background(), w.prOf(1), []Env{void}); err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Errorf("Plan onto the default branch: %v, want refused", err)
	}
	if _, err := c.Deploy(context.Background(), p, ByCLI); err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Errorf("Deploy onto the default branch: %v, want refused", err)
	}
	gitIn(t, w.r, w.mirror, "symbolic-ref", "HEAD", "refs/heads/master")

	// The allowlist again at the push: the env left the config after Deploy.
	id := w.deploy(c, 1)
	w.drive(c, id, at(StepPush))
	w.deps.Stagings = nil
	c2 := w.client()
	defer c2.Close()
	c.Close()
	c2.Effects("acme/app")
	e := w.drive(c2, id, ended)
	if e.State != EffectFailed || e.Step != StepPush || !strings.Contains(e.Why, "refused") {
		t.Errorf("an env no longer configured at the push: %+v, want failed at push, refused", e)
	}
	w.noPushSince(0, w.tip)
}

// Deploy refuses, starting nothing: a by that is none of cli, liaison and
// factory; an env without a plan; the same PR at the same head already
// deploying there. Plan writes no journal.
func TestDeployRefusesWithoutStartingAnything(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	p := w.plan(c, 1)
	if _, err := os.Stat(deploysDir(w.mirror)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Plan made the deploys' directory: %v", err)
	}
	for _, by := range []string{"", "operator", "CLI"} {
		if _, err := c.Deploy(context.Background(), p, by); err == nil {
			t.Errorf("Deploy by %q started", by)
		}
	}
	noPlan := p
	noPlan.Envs = []EnvPlan{{Env: void, Why: "the remote has no branch staging-void"}}
	if _, err := c.Deploy(context.Background(), noPlan, ByCLI); err == nil {
		t.Error("Deploy of an env without a plan started")
	}
	id := w.deploy(c, 1)
	if ids, err := c.Deploy(context.Background(), p, ByFactory); err == nil || !strings.Contains(err.Error(), "already deploying") {
		t.Errorf("a second Deploy of the same PR and head: %v %v, want refused", ids, err)
	}
	if es := c.Effects("acme/app"); len(es) != 1 || es[0].ID != id {
		t.Errorf("effects: %+v, want only %s", es, id)
	}
}

// ci.yml failing on the merge fails the effect with the run's URL; no
// deployment 50 minutes after it passed ends it done, "pushed, not
// confirmed"; deployments that are not readable end it so at once.
func TestWatchAndConfirm(t *testing.T) {
	t.Parallel()
	t.Run("CI failed", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		c := w.client()
		defer c.Close()
		id := w.deploy(c, 1)
		e := w.drive(c, id, at(StepWatch))
		w.gh.mu.Lock()
		w.gh.runs[e.Merge] = []Run{{ID: 8, URL: runURL, Status: "in_progress"}}
		w.gh.mu.Unlock()
		if e := w.drive(c, id, func(e Effect) bool { return e.RunURL != "" }); e.Step != StepWatch || e.State != EffectRunning {
			t.Fatalf("CI running: %+v, want still watching", e)
		}
		w.gh.mu.Lock()
		w.gh.runs[e.Merge] = []Run{{ID: 8, URL: runURL, Status: "completed", Conclusion: "failure"}}
		w.gh.mu.Unlock()
		e = w.drive(c, id, ended)
		if e.State != EffectFailed || e.Step != StepWatch || e.RunURL != runURL || !strings.Contains(e.Why, "ci.yml failed") {
			t.Errorf("CI failed: %+v, want failed at watch with the run", e)
		}
	})
	t.Run("not confirmed", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		c := w.client()
		defer c.Close()
		id := w.deploy(c, 1)
		e := w.drive(c, id, at(StepWatch))
		w.passCI(e.Merge)
		w.drive(c, id, at(StepConfirm))
		w.deployed(w.tip) // an older tip: it does not hold the merge
		w.advance(ConfirmAfterCI - time.Minute)
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if e := w.effect(c, id); e.State != EffectRunning || !strings.Contains(e.Why, "Jenkins") {
			t.Errorf("before 50 minutes: %+v, want waiting for Jenkins", e)
		}
		w.advance(time.Minute)
		e = w.drive(c, id, ended)
		if e.State != EffectDone || !strings.HasPrefix(e.Why, NotConfirmed) {
			t.Errorf("after 50 minutes: %+v, want done, pushed, not confirmed", e)
		}
	})
	t.Run("not readable", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		w.gh.deployErr["void"] = ErrNotReadable
		c := w.client()
		defer c.Close()
		id := w.deploy(c, 1)
		e := w.drive(c, id, at(StepWatch))
		w.passCI(e.Merge)
		e = w.drive(c, id, ended)
		if e.State != EffectDone || e.Why != NotConfirmed+": "+WhyNotReadable {
			t.Errorf("deployments not readable: %+v, want done, not confirmed", e)
		}
	})
}

// A process killed at each step, right after its begin (and once right
// after the push, before its ok): a new client resumes the effect from
// the journal, the step's guard runs again, and the effect ends done with
// exactly one push; after the push, the branch holding the merge already
// makes the second push a no-op that never runs.
func TestAKilledDeployResumesFromTheJournalAndNeverPushesTwice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ step, point string }{
		{StepClaim, "begin"}, {StepPlan, "begin"}, {StepMerge, "begin"}, {StepVerify, "begin"},
		{StepPush, "begin"}, {StepPush, "pushed"}, {StepWatch, "begin"}, {StepConfirm, "begin"},
	} {
		t.Run(tc.step+" "+tc.point, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			first := w.client()
			hit := stopAt(first, tc.step, tc.point)
			p := w.plan(first, 1)
			ids, err := first.Deploy(context.Background(), p, ByCLI)
			if err != nil {
				t.Fatal(err)
			}
			id := ids[0]
			// The fake GitHub answers each merge: ci.yml passes, then a
			// deployment holds it.
			answer := func(e Effect) {
				if e.Step == StepWatch && e.Merge != "" {
					w.passCI(e.Merge)
				}
				if e.Step == StepConfirm {
					w.deployed(e.Merge)
				}
			}
			for i := 0; !*hit && i < 30; i++ {
				answer(w.effect(first, id))
				if err := first.Step(context.Background()); err != nil && !errors.Is(err, errHook) {
					t.Fatalf("Step: %v", err)
				}
			}
			if !*hit {
				t.Fatalf("the hook never stopped at %s %s", tc.step, tc.point)
			}
			kill(first)
			second := w.client()
			defer second.Close()
			second.Effects("acme/app") // the daemon names the repository
			e := w.drive(second, id, func(e Effect) bool {
				answer(e)
				return ended(e)
			})
			if e.State != EffectDone || !strings.HasPrefix(e.Why, "live") {
				t.Fatalf("resumed: %+v, want done, live", e)
			}
			if got := w.run.pushed(); got != 1 {
				t.Errorf("pushes: %d, want exactly 1", got)
			}
			if got := w.originTip(); got != e.Merge {
				t.Errorf("origin's staging-void %s, want the merge %s", short(got), short(e.Merge))
			}
			if merges := gitIn(t, w.r, w.origin, "rev-list", "--merges", "--count", w.tip+"..refs/heads/staging-void"); merges != "1" {
				t.Errorf("merges on staging-void since the tip: %s, want 1", merges)
			}
		})
	}
}

// Two clients (two daemons) deploy two PRs to one env: the env's flock
// serializes them. The second Deploy waits queued, and no Step of its
// client advances it while the first runs; once the first is done, the
// second claims the env and runs.
func TestTwoClientsOnOneEnvAreSerializedByTheFlock(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	w.pr(4, "pr4", map[string]string{"app/d.rb": "d\n"})
	a, b := w.client(), w.client()
	defer a.Close()
	defer b.Close()
	first := w.deploy(a, 1)
	second := w.deploy(b, 4)
	if e := w.effect(b, second); e.State != EffectQueued || e.Step != StepClaim {
		t.Fatalf("the second effect: %+v, want queued at claim", e)
	}
	running := w.effect(a, first)
	events := len(w.events)
	for range 3 {
		if err := b.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if e := w.effect(b, second); e.State != EffectQueued {
		t.Fatalf("while the first runs: %+v, want queued", e)
	}
	// Nor did b's Steps run a's effect: a holds the env.
	if e := w.effect(a, first); e.Step != running.Step || !e.Updated.Equal(running.Updated) || len(w.events) != events {
		t.Fatalf("b's Steps moved the first effect: %+v, was %+v (%d events, was %d)", e, running, len(w.events), events)
	}
	if e := w.finish(a, first); e.State != EffectDone {
		t.Fatalf("the first: %+v", e)
	}
	e := w.finish(b, second)
	if e.State != EffectDone {
		t.Fatalf("the second: %+v", e)
	}
	if parents, _, _, _ := w.commitOf(e.Merge); parents[0] != w.effect(a, first).Merge {
		t.Errorf("the second merge's first parent %s, want the first merge", short(parents[0]))
	}
}

// A cancel at each step: the effect ends cancelled, a running resolver
// job is cancelled, and nothing is pushed after it; a cancel after the
// push stops only the watching and says the push stands.
func TestACancelAtEachStepNeverPushesAfter(t *testing.T) {
	t.Parallel()
	for _, step := range []string{StepClaim, StepPlan, StepMerge, StepVerify, StepPush, StepWatch, StepConfirm, "resolver"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			res := w.withResolver()
			c := w.client()
			defer c.Close()
			var id EffectID
			switch step {
			case StepClaim:
				other := w.client()
				defer other.Close()
				w.deploy(other, 1)
				w.pr(4, "pr4", map[string]string{"app/d.rb": "d\n"})
				id = w.deploy(c, 4) // queued behind other's
			case "resolver":
				id = w.deploy(c, 2)
				w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 })
			default:
				id = w.deploy(c, 1)
				w.drive(c, id, func(e Effect) bool {
					if e.Step == StepWatch && step == StepConfirm {
						w.passCI(e.Merge)
					}
					return e.Step == step || ended(e)
				})
			}
			before := w.effect(c, id)
			pushes, tip := w.run.pushed(), w.originTip()
			if err := c.Cancel(context.Background(), id); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			for range 5 {
				if err := c.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			e := w.effect(c, id)
			if e.State != EffectCancelled {
				t.Fatalf("after the cancel at %s: %+v, want cancelled", before.Step, e)
			}
			w.noPushSince(pushes, tip)
			switch step {
			case StepWatch, StepConfirm:
				if !strings.Contains(e.Why, "the push stands") {
					t.Errorf("a cancel after the push: %q, want the push stands", e.Why)
				}
			case "resolver":
				if got := res.cancels(); !slices.Equal(got, []string{"job-1"}) {
					t.Errorf("resolver cancels: %q, want job-1", got)
				}
			default:
				if !strings.Contains(e.Why, "nothing was pushed") || pushes != 0 {
					t.Errorf("a cancel before the push: %q, %d pushes", e.Why, pushes)
				}
			}
			if err := c.Cancel(context.Background(), id); err != nil {
				t.Errorf("a second Cancel: %v", err)
			}
		})
	}
}

// The PR's head moves after the plan (a new commit on the PR): the plan
// step, or verify when it moves during the merge, fails the effect, and
// nothing is pushed.
func TestAMovedHeadFailsTheDeployBeforeThePush(t *testing.T) {
	t.Parallel()
	for _, step := range []string{StepPlan, StepVerify} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			c := w.client()
			defer c.Close()
			p := w.plan(c, 1)
			move := func() {
				git := func(args ...string) string { t.Helper(); return gitIn(t, w.r, w.work, args...) }
				git("checkout", "-q", "pr1")
				writeIn(t, w.work, "app/b.rb", "b2\n")
				git("commit", "-qam", "p1 again")
				git("push", "-q", "origin", "HEAD:refs/pull/1/head")
				git("checkout", "-q", "master")
			}
			if step == StepPlan {
				move()
			} else {
				c.hook = func(_ EffectID, s, point string) error {
					if s == StepMerge && point == "begin" {
						move()
					}
					return nil
				}
			}
			ids, err := c.Deploy(context.Background(), p, ByCLI)
			if err != nil {
				t.Fatal(err)
			}
			e := w.drive(c, ids[0], ended)
			if e.State != EffectFailed || e.Step != step || !strings.Contains(e.Why, "head moved since the plan") {
				t.Errorf("ended: %+v, want failed at %s, the head moved", e, step)
			}
			w.noPushSince(0, w.tip)
		})
	}
}

// The deploy's staging-branch rule is config's: the package keeps its own
// copy of the pattern (it does not import config), pinned to it here.
func TestTheDeploysBranchRuleIsConfigs(t *testing.T) {
	t.Parallel()
	if got, want := stagingBranchRe.String(), config.StagingBranchPattern.String(); got != want {
		t.Errorf("the deploy's branch rule %q, config's %q", got, want)
	}
}
