package staging

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// countingRunner is the real runner, counting the pushes and commit-tree
// runs it starts; beforePush, when set, runs before each push (a
// coworker's push in between).
type countingRunner struct {
	inner execx.Runner
	mu    sync.Mutex
	// pushes and commits count the push and commit-tree commands.
	pushes, commits int
	beforePush      func()
	// fail, when set, fails each command whose git subcommand (the first
	// argument after the mirror's options) it names, as git exiting 128.
	fail map[string]bool
	// failArg, when set, fails each command with an argument that holds it,
	// as git exiting 128 with failStderr.
	failArg, failStderr string
	// version, when set, is what `git version` prints, without running it.
	version string
	// cmds are the commands started, in order.
	cmds []execx.Cmd
}

// subcommand is a mirror command's git subcommand: the first argument past
// --git-dir and the -c pairs.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-c":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return ""
}

func (r *countingRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	var before func()
	r.mu.Lock()
	sub := subcommand(c.Args)
	switch {
	case sub == "push" && slices.Contains(c.Args, "origin"):
		r.pushes++
		before = r.beforePush
	case sub == "commit-tree":
		r.commits++
	}
	failed := r.fail[sub]
	stderr := "fatal: injected " + sub + " failure\n"
	if r.failArg != "" && slices.ContainsFunc(c.Args, func(a string) bool { return strings.Contains(a, r.failArg) }) {
		failed, stderr = true, r.failStderr
	}
	version := r.version
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()
	if failed {
		return execx.Result{Code: 128}, &execx.ExitError{Cmd: c, Code: 128, Stderr: stderr}
	}
	if sub == "version" && version != "" {
		return execx.Result{Stdout: []byte(version + "\n")}, nil
	}
	if before != nil {
		before()
	}
	return r.inner.Run(ctx, c)
}

// failing makes the git subcommands fail from now on.
func (r *countingRunner) failing(subs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail == nil {
		r.fail = map[string]bool{}
	}
	for _, s := range subs {
		r.fail[s] = true
	}
}

// started is the commands started so far.
func (r *countingRunner) started() []execx.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.cmds)
}

func (r *countingRunner) pushed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pushes
}

func (r *countingRunner) committed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commits
}

// fakeResolver is a Resolver whose jobs run until the test answers them.
type fakeResolver struct {
	mu        sync.Mutex
	started   []ResolveRequest
	results   map[string]ResolveResult // by job
	cancelled []string
	// resultErr, when set, fails every Result.
	resultErr error
}

func newFakeResolver() *fakeResolver { return &fakeResolver{results: map[string]ResolveResult{}} }

func (f *fakeResolver) Start(_ context.Context, r ResolveRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, r)
	job := fmt.Sprintf("job-%d", len(f.started))
	f.results[job] = ResolveResult{State: ResolveRunning}
	return job, nil
}

func (f *fakeResolver) Result(_ context.Context, job string) (ResolveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resultErr != nil {
		return ResolveResult{}, f.resultErr
	}
	res, ok := f.results[job]
	if !ok {
		return ResolveResult{State: ResolveFailed, Why: "no such job"}, nil
	}
	return res, nil
}

func (f *fakeResolver) Cancel(_ context.Context, job string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, job)
	return nil
}

// answer sets the newest job's result.
func (f *fakeResolver) answer(res ResolveResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[fmt.Sprintf("job-%d", len(f.started))] = res
}

func (f *fakeResolver) requests() []ResolveRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.started)
}

func (f *fakeResolver) cancels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cancelled)
}

// deployWorld is a real bare origin in a temp dir (master at c1,
// staging-void at s1 on it, and PR branches whose heads are also their pull
// refs), a clone that plays the coworkers, and clients on a treeless mirror
// of the origin, all over file://.
type deployWorld struct {
	t      *testing.T
	r      *execx.Real
	run    *countingRunner
	origin string // the bare origin
	work   string // the coworkers' clone
	mirror string
	gh     *fakeGitHub
	res    *fakeResolver // nil until withResolver
	c1     string        // master
	tip    string        // staging-void's tip at the start
	heads  map[int]string
	// deps is changed by the tests before clients are made.
	deps Deps

	mu     sync.Mutex
	now    time.Time
	events []string // step phase by text
	n      int      // coworker commits
}

const botName, botEmail = "magnum-bot", "bot@example.com"

func newDeployWorld(t *testing.T) *deployWorld {
	t.Helper()
	r := hermetic(t)
	w := &deployWorld{t: t, r: r, run: &countingRunner{inner: r}, gh: newFakeGitHub(), heads: map[int]string{},
		now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	w.origin = filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, r, filepath.Dir(w.origin), "init", "-q", "--bare", "-b", "master", w.origin)
	gitIn(t, r, w.origin, "config", "uploadpack.allowFilter", "true")
	gitIn(t, r, w.origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	w.work = t.TempDir()
	git := func(args ...string) string { t.Helper(); return gitIn(t, r, w.work, args...) }
	git("init", "-q", "-b", "master")
	git("remote", "add", "origin", w.origin)
	writeIn(t, w.work, "app/a.rb", "a\n")
	git("add", ".")
	git("commit", "-qm", "c1")
	w.c1 = git("rev-parse", "HEAD")
	git("push", "-q", "origin", "master")
	git("checkout", "-qb", "staging-void")
	writeIn(t, w.work, "app/s.rb", "staging\n")
	git("add", ".")
	git("commit", "-qm", "s1")
	w.tip = git("rev-parse", "HEAD")
	git("push", "-q", "origin", "staging-void")
	git("checkout", "-q", "master")
	// PR 1: a clean change with a migration. PR 2: conflicts with s1.
	w.pr(1, "pr1", map[string]string{"app/b.rb": "b\n", "db/migrate/20261108_create_things.rb": "create\n"})
	w.pr(2, "pr2", map[string]string{"app/s.rb": "theirs\n"})
	// PR 9: a clean change with a sharded migration only (talkable#12203's shape).
	w.pr(9, "pr9", map[string]string{"app/c.rb": "c\n", "db/sharded_migrate/20261105011921_add_metric_indexes.rb": "index\n"})

	w.mirror = filepath.Join(t.TempDir(), "staging", "acme", "app.git")
	w.deps = Deps{
		Git:        newGit(w.run, func(string) string { return "file://" + w.origin }),
		GitHub:     w.gh,
		Mirror:     func(string) (string, bool) { return w.mirror, true },
		MirrorRoot: func() (string, bool) { return filepath.Dir(filepath.Dir(w.mirror)), true },
		GitEnv:     func(string) map[string]string { return map[string]string{} },
		Now: func() time.Time {
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.now
		},
		Stagings: func(string) []Env { return []Env{void} },
		PushEnv: func(context.Context, string, string, string) (map[string]string, error) {
			return map[string]string{}, nil
		},
		CommitIdentity: func(context.Context, string) (string, string, error) {
			return botName, botEmail, nil
		},
		Event: func(e Effect, step, phase, text string) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if e.By == "" {
				t.Errorf("an event of %s without by: %s %s %s", e.ID, step, phase, text)
			}
			w.events = append(w.events, step+" "+phase+" "+text)
		},
	}
	c := w.client()
	refreshCloned(t, c, OpenPR{Number: 1, Base: "master"})
	c.Close()
	return w
}

// pr makes PR n: a branch off master with files, pushed as the branch and
// as refs/pull/<n>/head.
func (w *deployWorld) pr(n int, branch string, files map[string]string) string {
	w.t.Helper()
	return w.prOn(n, branch, w.c1, files)
}

// prOn is pr with the branch made on the commit base.
func (w *deployWorld) prOn(n int, branch, base string, files map[string]string) string {
	w.t.Helper()
	git := func(args ...string) string { w.t.Helper(); return gitIn(w.t, w.r, w.work, args...) }
	git("checkout", "-q", "-b", branch, base)
	for name, text := range files {
		writeIn(w.t, w.work, name, text)
	}
	git("add", "-A")
	git("commit", "-qm", branch)
	head := git("rev-parse", "HEAD")
	git("push", "-q", "origin", branch, head+":refs/pull/"+fmt.Sprint(n)+"/head")
	git("checkout", "-q", "master")
	w.heads[n] = head
	return head
}

// coworker pushes a commit of its own onto origin's staging-void (a plain
// push, as a coworker's merge) and returns the new tip.
func (w *deployWorld) coworker(files map[string]string, deleted ...string) string {
	w.t.Helper()
	git := func(args ...string) string { w.t.Helper(); return gitIn(w.t, w.r, w.work, args...) }
	git("fetch", "-q", "origin", "staging-void")
	git("checkout", "-q", "--detach", "FETCH_HEAD")
	w.mu.Lock()
	w.n++
	n := w.n
	w.mu.Unlock()
	if files == nil && deleted == nil {
		files = map[string]string{fmt.Sprintf("app/coworker%d.rb", n): "c\n"}
	}
	for name, text := range files {
		writeIn(w.t, w.work, name, text)
	}
	for _, name := range deleted {
		git("rm", "-q", name)
	}
	git("add", "-A")
	git("commit", "-qm", fmt.Sprintf("coworker %d", n))
	tip := git("rev-parse", "HEAD")
	git("push", "-q", "origin", "HEAD:refs/heads/staging-void")
	git("checkout", "-q", "master")
	return tip
}

// client is a Client on the world's deps.
func (w *deployWorld) client() *Client { return New(w.deps) }

// withResolver gives the clients made after it a fake resolver.
func (w *deployWorld) withResolver() *fakeResolver {
	w.res = newFakeResolver()
	w.deps.Resolver = w.res
	return w.res
}

// advance moves the clock.
func (w *deployWorld) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
}

// originTip is origin's staging-void.
func (w *deployWorld) originTip() string {
	w.t.Helper()
	return gitIn(w.t, w.r, w.origin, "rev-parse", "refs/heads/staging-void")
}

// prOf is PR n of acme/app at its head.
func (w *deployWorld) prOf(n int) PR {
	return PR{Repo: "acme/app", Number: n, Head: w.heads[n], Base: "master", HeadRef: fmt.Sprintf("pr%d", n)}
}

// plan plans PR n on void, failing the test on an error.
func (w *deployWorld) plan(c *Client, n int) DeployPlan {
	w.t.Helper()
	p, err := c.Plan(context.Background(), w.prOf(n), []Env{void})
	if err != nil {
		w.t.Fatalf("Plan: %v", err)
	}
	return p
}

// deploy plans and deploys PR n on void by the CLI, and returns its effect.
func (w *deployWorld) deploy(c *Client, n int) EffectID {
	w.t.Helper()
	ids, err := c.Deploy(context.Background(), w.plan(c, n), ByCLI)
	if err != nil || len(ids) != 1 {
		w.t.Fatalf("Deploy: %v, %v", ids, err)
	}
	return ids[0]
}

// effect is effect id as c's Effects list it.
func (w *deployWorld) effect(c *Client, id EffectID) Effect {
	w.t.Helper()
	for _, e := range c.Effects("acme/app") {
		if e.ID == id {
			return e
		}
	}
	w.t.Fatalf("no effect %s", id)
	return Effect{}
}

// drive steps c until effect id meets until, at most 40 Steps; the fake
// GitHub answers each new merge's ci.yml with run (none = no run) and, once
// it passed, a deployment of it when deploy.
func (w *deployWorld) drive(c *Client, id EffectID, until func(Effect) bool) Effect {
	w.t.Helper()
	for range 40 {
		e := w.effect(c, id)
		if until(e) {
			return e
		}
		if j := w.journalAt(id); j != nil && j.RetryAt.After(w.deps.Now()) {
			w.advance(j.RetryAt.Sub(w.deps.Now())) // ride out the backoff
		}
		if err := c.Step(context.Background()); err != nil {
			w.t.Fatalf("Step: %v (at %s %s: %s)", err, e.Step, e.State, e.Why)
		}
	}
	e := w.effect(c, id)
	w.t.Fatalf("effect %s never got there: %s %s %q", id, e.Step, e.State, e.Why)
	return e
}

// journalAt is effect id's journal, nil when it cannot be read.
func (w *deployWorld) journalAt(id EffectID) *journal {
	root, err := openDeploys(w.mirror, false)
	if err != nil {
		return nil
	}
	defer root.Close()
	j, _ := readJournal(root, id)
	return j
}

// ended is a drive's until: the effect ended.
func ended(e Effect) bool { return e.State != EffectQueued && e.State != EffectRunning }

// at is a drive's until: the effect is at step, live.
func at(step string) func(Effect) bool {
	return func(e Effect) bool { return e.Step == step || ended(e) }
}

// passCI makes ci.yml pass on merge, and a deployment of it succeed.
func (w *deployWorld) passCI(merge string) {
	w.gh.mu.Lock()
	defer w.gh.mu.Unlock()
	w.gh.runs[merge] = []Run{{ID: 7, URL: runURL, Status: "completed", Conclusion: "success", CreatedAt: w.deps.Now()}}
}

// runURL is the fake ci.yml run's URL.
const runURL = "https://github.com/acme/app/actions/runs/7"

// deployed records a successful deployment of void at sha.
func (w *deployWorld) deployed(sha string) {
	now := w.deps.Now()
	w.gh.mu.Lock()
	defer w.gh.mu.Unlock()
	w.gh.deploys["void"] = append([]Deployment{{ID: int64(len(w.gh.deploys["void"]) + 1), SHA: sha, Ref: sha, CreatedAt: now, Success: true}}, w.gh.deploys["void"]...)
}

// finish drives a pushed effect through watch and confirm: CI passes and a
// deployment holds the merge.
func (w *deployWorld) finish(c *Client, id EffectID) Effect {
	w.t.Helper()
	e := w.drive(c, id, at(StepWatch))
	if ended(e) {
		w.t.Fatalf("ended before watch: %s %q", e.State, e.Why)
	}
	w.passCI(e.Merge)
	e = w.drive(c, id, at(StepConfirm))
	w.deployed(e.Merge)
	return w.drive(c, id, ended)
}

// commitOf reads a commit in the origin: its parents, author, committer and
// subject.
func (w *deployWorld) commitOf(sha string) (parents []string, author, committer, subject string) {
	w.t.Helper()
	out := gitIn(w.t, w.r, w.origin, "show", "-s", "--format=%P%n%an <%ae>%n%cn <%ce>%n%s", sha)
	lines := strings.Split(out, "\n")
	return strings.Fields(lines[0]), lines[1], lines[2], lines[3]
}

// noPushSince fails the test when the origin's staging-void is not tip, or
// pushes were started beyond n.
func (w *deployWorld) noPushSince(n int, tip string) {
	w.t.Helper()
	if got := w.run.pushed(); got != n {
		w.t.Errorf("pushes: %d, want %d", got, n)
	}
	if got := w.originTip(); got != tip {
		w.t.Errorf("origin's staging-void is %s, want %s: something was pushed", short(got), short(tip))
	}
}

// resolverRepo is a clone of the origin where a resolver resolved PR n's
// conflict with staging-void at tip: the merge [parents...] of the tree
// the index holds once files are written, not on any branch. It returns
// the repository and the merge.
func (w *deployWorld) resolverRepo(n int, parents []string, files map[string]string) (string, string) {
	w.t.Helper()
	return w.resolverRepoWith(n, parents, func(dir string) {
		for name, text := range files {
			writeIn(w.t, dir, name, text)
		}
	})
}

// resolverRepoWith is resolverRepo with the work tree's resolution made by
// edit (a file removed, a mode changed, a symlink), all of it staged.
func (w *deployWorld) resolverRepoWith(n int, parents []string, edit func(dir string)) (string, string) {
	w.t.Helper()
	dir := filepath.Join(w.t.TempDir(), "repo")
	git := func(args ...string) string { w.t.Helper(); return gitIn(w.t, w.r, dir, args...) }
	gitIn(w.t, w.r, filepath.Dir(dir), "clone", "-q", w.origin, dir)
	git("fetch", "-q", "origin", "refs/pull/"+fmt.Sprint(n)+"/head")
	git("checkout", "-q", "--detach", "origin/staging-void")
	// The merge stops on the conflict (exit 1): the files resolve it.
	_, _ = w.r.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"-C", dir, "merge", "--no-commit", "--no-ff", w.heads[n]}})
	edit(dir)
	git("add", "-A")
	tree := git("write-tree")
	args := []string{"commit-tree", tree, "-m", "Merge branch 'pr" + fmt.Sprint(n) + "' into staging-void"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	merge := git(args...)
	git("merge", "--abort")
	return dir, merge
}

// kill stops c as a killed process would: its flocks go, nothing is
// written.
func kill(c *Client) { c.releaseAll() }

// stopAt makes c's Step stop at step's point once, as a killed process.
func stopAt(c *Client, step, point string) *bool {
	hit := new(bool)
	c.hook = func(_ EffectID, s, p string) error {
		if s == step && p == point && !*hit {
			*hit = true
			return errHook
		}
		return nil
	}
	return hit
}
