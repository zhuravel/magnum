package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// resolveFixture is a harness whose staging resolver works with real git:
// talkable/talkable's main clone (the pool's, HOME/talkable) and its slot
// review1 (a worktree of it, HOME/talkable.review1), where staging-void's
// tip and PR #2's head both change app/a.rb's line (a conflict), the tip
// also README.md and the head also db/schema.rb (no conflict). PR #2 is
// the operator's, reviewed at its head, which slot review1 holds; the
// fixer App's bot commits. The agent is fake.
type resolveFixture struct {
	h          *harness
	r          *stagingResolver
	run        *execx.Real
	git        *gitx.Client
	origin     string
	clone      string
	slot       string
	base       string
	tip, head  string
	agent      *fakeStagingAgent
	staging    *fakeStaging
	agentJobs  []stagingAgentJob
	agentsMu   sync.Mutex
	agentsDown error // stagingAgents fails with it
}

// gitSlots is the harness's fake slots whose Resync really switches the
// slot's work tree (after aborting a merge in progress, as slots.Resync
// does) before it records the commit, and whose Release resets it. Its
// Resync holds a person's changes as slots.Resync's guard does: when
// someone typed into the slot's PR's magnum panes after the slot's last
// checkout (last_used_at) and the tree has tracked changes, it persists
// dirty_worktree and refuses.
type gitSlots struct {
	*fakeSlots
	f *resolveFixture
}

func (g *gitSlots) Resync(ctx context.Context, slot store.Slot, sha string) error {
	if err := g.humanHold(ctx, slot); err != nil {
		return err
	}
	if _, err := g.f.run.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", slot.Path, "rev-parse", "-q", "--verify", "MERGE_HEAD"}}); err == nil {
		g.f.gitIn(slot.Path, "merge", "--abort")
	}
	g.f.gitIn(slot.Path, "switch", "--quiet", "--detach", "--discard-changes", sha)
	return g.fakeSlots.Resync(ctx, slot, sha)
}

// humanHold is slots' guardDiscard rule for a person's changes (humanActivity
// and holdChanges).
func (g *gitSlots) humanHold(ctx context.Context, slot store.Slot) error {
	sl, err := g.st.SlotByID(ctx, slot.ID)
	if err != nil || sl.PRID == nil {
		return err
	}
	pr, err := g.st.PRByID(ctx, *sl.PRID)
	if err != nil {
		return err
	}
	if at := pr.HumanActiveAt; at == nil || (sl.LastUsedAt != nil && !at.After(*sl.LastUsedAt)) {
		return nil
	}
	res, err := g.f.run.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", sl.Path, "status", "--porcelain", "--untracked-files=no"}})
	if err != nil || res.Out() == "" {
		return err
	}
	if err := g.st.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("hold_reason", slots.HoldDirtyWorktree) }); err != nil {
		return err
	}
	return slots.ErrHold{Reason: slots.HoldDirtyWorktree, Detail: sl.Path + " has tracked changes; someone typed into PR #" + fmt.Sprint(pr.Number) +
		"'s magnum panes after magnum's checkout"}
}

func newResolveFixture(t *testing.T, mods ...func(*harness)) *resolveFixture {
	t.Helper()
	f := &resolveFixture{run: editGitRunner(t), agent: &fakeStagingAgent{}, staging: &fakeStaging{}}
	if _, err := f.run.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"--version"}}); err != nil {
		t.Skipf("git not available: %v", err)
	}
	f.git = gitx.New(f.run)
	f.r = &stagingResolver{git: f.git}
	all := append([]func(*harness){withStagings(f.staging), withFixerApp, func(h *harness) {
		h.d.Slots = &gitSlots{fakeSlots: h.sl, f: f}
		h.d.stagingResolver = f.r
		h.d.stagingAgents = func(j stagingAgentJob) (stagingAgent, error) {
			f.agentsMu.Lock()
			defer f.agentsMu.Unlock()
			if f.agentsDown != nil {
				return nil, f.agentsDown
			}
			f.agentJobs = append(f.agentJobs, j)
			f.agent.job = j
			return f.agent, nil
		}
	}}, mods...)
	f.h = newHarness(t, all...)
	t.Cleanup(f.h.e.stagingStop) // before the harness's own stop waits for the worker
	home := f.h.cfg.Pools[0].MainClone
	f.clone, f.slot = home, f.h.cfg.Pools[0].Path(1)
	f.origin = filepath.Join(filepath.Dir(home), "origin")
	if err := os.MkdirAll(f.origin, 0o755); err != nil {
		t.Fatal(err)
	}
	f.gitIn(f.origin, "init", "--quiet", "-b", "master")
	writeFile(t, f.origin, "app/a.rb", "a = 1\n")
	writeFile(t, f.origin, "README.md", "hello\n")
	writeFile(t, f.origin, "lib/r.rb", "r = 1\n")
	f.gitIn(f.origin, "add", ".")
	f.gitIn(f.origin, "commit", "--quiet", "-m", "base")
	f.base = f.gitIn(f.origin, "rev-parse", "HEAD")
	f.gitIn(f.origin, "checkout", "--quiet", "-b", "staging-void")
	writeFile(t, f.origin, "app/a.rb", "a = 2\n")
	writeFile(t, f.origin, "README.md", "hello staging\n")
	f.gitIn(f.origin, "commit", "--quiet", "-am", "staging moves")
	f.tip = f.gitIn(f.origin, "rev-parse", "HEAD")
	f.gitIn(f.origin, "checkout", "--quiet", "-b", "feature", f.base)
	writeFile(t, f.origin, "app/a.rb", "a = 1\nb = 1\n")
	writeFile(t, f.origin, "db/schema.rb", "version 2\n")
	f.gitIn(f.origin, "add", ".")
	f.gitIn(f.origin, "commit", "--quiet", "-m", "the PR")
	f.head = f.gitIn(f.origin, "rev-parse", "HEAD")
	f.gitIn(f.origin, "checkout", "--quiet", "master")
	f.gitIn(filepath.Dir(home), "clone", "--quiet", "--no-checkout", f.origin, home)
	f.gitIn(home, "fetch", "--quiet", "origin", "staging-void", "feature")
	f.gitIn(home, "worktree", "add", "--quiet", "--detach", f.slot, f.head)
	return f
}

// gitIn runs git in dir and returns its output.
func (f *resolveFixture) gitIn(dir string, args ...string) string {
	f.h.t.Helper()
	res, err := f.run.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)})
	if err != nil {
		f.h.t.Fatalf("git %v: %v", args, err)
	}
	return res.Out()
}

// reviewed has PR #2 reviewed at the head, slot review1 holding it.
func (f *resolveFixture) reviewed() store.PR {
	f.h.t.Helper()
	f.h.open(prSpec{n: 1, head: "base1"})
	f.h.startup()
	f.h.tick()
	f.h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: f.head, author: "zhuravel"})
	f.h.tick()
	f.h.advance(5 * time.Minute)
	f.h.tick()
	pr := f.h.wantState(2, store.PRReviewed)
	if sl := f.h.slot("review1"); deref(sl.PRID) != pr.ID || deref(sl.CheckedOutSHA) != f.head {
		f.h.t.Fatalf("slot review1 is PR %d's at %s, want PR #2's at its head", deref(sl.PRID), deref(sl.CheckedOutSHA))
	}
	return pr
}

// request is the deploy's request of the conflict, try try.
func (f *resolveFixture) request(try int) staging.ResolveRequest {
	return staging.ResolveRequest{Repo: "talkable/talkable", Ref: "talkable/talkable#2", Target: "staging-void", TargetTip: f.tip,
		Head: f.head, Effect: "void-2-20261005t100000-abcd", Try: try}
}

// start starts a job for f.request(try).
func (f *resolveFixture) start(try int) string {
	f.h.t.Helper()
	id, err := f.r.Start(f.h.ctx, f.request(try))
	if err != nil {
		f.h.t.Fatalf("Start: %v", err)
	}
	return id
}

// await waits until job id is no longer running, and returns its result.
func (f *resolveFixture) await(id string) staging.ResolveResult {
	f.h.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		res, err := f.r.Result(f.h.ctx, id)
		if err != nil {
			f.h.t.Fatal(err)
		}
		f.r.mu.Lock()
		idle := !f.r.working
		f.r.mu.Unlock()
		if res.State != staging.ResolveRunning && idle {
			return res
		}
		if time.Now().After(deadline) {
			f.h.t.Fatalf("job %s still %s: %s", id, res.State, res.Why)
		}
	}
}

// awaitIdle waits until the worker stopped.
func (f *resolveFixture) awaitIdle() {
	f.h.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		f.r.mu.Lock()
		idle := !f.r.working
		f.r.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			f.h.t.Fatal("the resolver's worker still runs")
		}
	}
}

// job is job id's row.
func (f *resolveFixture) job(id string) resolveJob {
	f.h.t.Helper()
	j, ok, err := f.h.e.resolveJob(f.h.ctx, id)
	if err != nil || !ok {
		f.h.t.Fatalf("job %s: %v, %v", id, ok, err)
	}
	return j
}

// resolves makes the fake agent resolve app/a.rb with text and write a
// result that ran spec/a_spec.rb, passed.
func (f *resolveFixture) resolves(text string) {
	f.agent.onPrompt = func(a *fakeStagingAgent, _ int, _ string) {
		writeFile(f.h.t, a.job.Cwd, "app/a.rb", text)
		a.result(f.h.t, resolveResultFile{Outcome: resolveOutcomeResolved, Specs: []string{"spec/a_spec.rb"}, SpecsResult: resolveSpecsPassed})
	}
}

// fakeStagingAgent is a job's agent: each prompt is recorded and handed to
// onPrompt (the n-th, from 1), Wait answers at once (after wait, when set),
// and Close and Detach are counted.
type fakeStagingAgent struct {
	mu       sync.Mutex
	job      stagingAgentJob
	prompts  []string
	onPrompt func(a *fakeStagingAgent, n int, prompt string)
	wait     func(ctx context.Context, until time.Time) (bool, error)
	alive    bool // Resume finds the agent at work
	resumed  int
	closed   int
	detached int
	closeErr error // what Close fails with (the agent stays)
}

func (a *fakeStagingAgent) Prompt(_ context.Context, text string) (stagingAgentAt, error) {
	a.mu.Lock()
	a.prompts = append(a.prompts, text)
	n, fn := len(a.prompts), a.onPrompt
	a.mu.Unlock()
	if fn != nil {
		fn(a, n, text)
	}
	return stagingAgentAt{Name: "magnum-staging-agent", Pane: "p-resolve", Run: fmt.Sprintf("run-%d", n)}, nil
}

func (a *fakeStagingAgent) Wait(ctx context.Context, _ string, until time.Time) (bool, error) {
	a.mu.Lock()
	fn := a.wait
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, until)
	}
	return false, nil
}

func (a *fakeStagingAgent) Resume(context.Context) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resumed++
	return a.alive, nil
}

func (a *fakeStagingAgent) Close(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed++
	return a.closeErr
}

func (a *fakeStagingAgent) Detach() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.detached++
}

// result writes r as the agent's result file.
func (a *fakeStagingAgent) result(t *testing.T, r resolveResultFile) {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Error(err)
		return
	}
	if err := os.WriteFile(filepath.Join(a.job.Dir, resolveResultName), b, 0o600); err != nil {
		t.Error(err)
	}
}

func (a *fakeStagingAgent) counts() (prompts, closed, detached, resumed int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.prompts), a.closed, a.detached, a.resumed
}

// The review daemon's staging client resolves a conflict: Start answers at
// once with a job, which works in the PR's own slot (no round holds it):
// detached at the tip with the head merged in, the agent (the fix role's,
// with no GitHub env) resolves the conflicted path, and the daemon makes
// the merge as the bot, [tip, head], on merge-tree's tree with the
// resolution, keeps it at refs/magnum/staging-resolved/<effect> in the
// slot's main clone (Dir) and answers done. The slot goes back to the
// PR's head, clean, with no merge in progress and no hold, and the PR's
// rounds are no longer held.
func TestTheStagingResolverResolvesAConflictInThePRsSlotAndCommitsAsTheBot(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	id := f.start(1)
	res := f.await(id)
	if res.State != staging.ResolveDone || !fullSHA.MatchString(res.MergeSHA) {
		t.Fatalf("Result = %+v, want done with a merge", res)
	}
	if res.Dir != f.clone {
		t.Errorf("Dir = %q, want the slot's main clone %q", res.Dir, f.clone)
	}
	for _, want := range []string{"app/a.rb", "review1", "spec/a_spec.rb", "parents [tip, head]", "only in the conflicted paths", fixerAppLogin} {
		if !strings.Contains(res.Why, want) {
			t.Errorf("Why %q does not say %q", res.Why, want)
		}
	}
	m := res.MergeSHA
	if got := f.gitIn(f.clone, "rev-parse", resolveRefPrefix+f.request(1).Effect); got != m {
		t.Errorf("the ref holds %s, want the merge", got)
	}
	if got := f.gitIn(f.clone, "log", "-1", "--format=%P|%an|%cn|%s", m); got != f.tip+" "+f.head+"|"+fixerAppLogin+"|"+fixerAppLogin+"|Merge pull request #2 into staging-void" &&
		got != f.tip+" "+f.head+"|"+fixerAppLogin+"|"+fixerAppLogin+"|Merge branch 'feature' into staging-void" {
		t.Errorf("the merge is %q, want [tip, head] by the bot with the clean merge's message", got)
	}
	if got := f.gitIn(f.clone, "show", m+":app/a.rb"); got != "a = 2\nb = 1" {
		t.Errorf("the merge's app/a.rb = %q, want the resolution", got)
	}
	mt, _, err := f.git.MergeTreeWrite(f.h.ctx, f.clone, f.tip, f.head)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := f.git.ChangedBetween(f.h.ctx, f.clone, mt, m); err != nil || !slices.Equal(changed, []string{"app/a.rb"}) {
		t.Errorf("the merge differs from merge-tree's tree in %v, %v; want app/a.rb alone", changed, err)
	}
	if got := f.gitIn(f.slot, "rev-parse", "HEAD"); got != f.head {
		t.Errorf("slot review1's HEAD is %s, want the PR's head back", got)
	}
	if got := f.gitIn(f.slot, "status", "--porcelain", "--untracked-files=no"); got != "" {
		t.Errorf("slot review1 is not clean: %q", got)
	}
	if _, err := f.run.Run(f.h.ctx, execx.Cmd{Name: "git", Args: []string{"-C", f.slot, "rev-parse", "-q", "--verify", "MERGE_HEAD"}}); err == nil {
		t.Error("a merge is still in progress in slot review1")
	}
	sl := f.h.slot("review1")
	if deref(sl.CheckedOutSHA) != f.head || sl.HoldReason != nil || deref(sl.PRID) != pr.ID {
		t.Errorf("slot review1 = %s for PR %d, hold %v; want the PR's at its head, no hold", deref(sl.CheckedOutSHA), deref(sl.PRID), sl.HoldReason)
	}
	if why := f.h.e.heldReason(pr.ID); why == resolveHeld {
		t.Error("the PR is still held for the resolver")
	}
	if !f.h.e.reserveSlotWork(pr.ID, "test") {
		t.Error("the PR's reservation was not let go")
	}
	f.h.e.endEviction(pr.ID)
	if _, closed, _, _ := f.agent.counts(); closed != 1 {
		t.Errorf("the agent was closed %d times, want once", closed)
	}
	j := f.agentJobs[0]
	switch {
	case j.Cwd != f.slot:
		t.Errorf("the agent works in %q, want the slot", j.Cwd)
	case j.Role.Name != "claude-fix" || j.Role.Model != f.h.fixRole().Model || j.Role.Effort != f.h.fixRole().Effort:
		t.Errorf("the agent's role is %s %s %s, want the fix role's name, model and effort", j.Role.Name, j.Role.Model, j.Role.Effort)
	case j.Role.Identity != "" || j.Role.Fix:
		t.Errorf("the agent's role acts as %q (fix %v): its pane would get a GitHub env", j.Role.Identity, j.Role.Fix)
	case j.Role.Timeout.Duration > resolveRunFor:
		t.Errorf("the agent's role times out after %s, over the job's %s", j.Role.Timeout.Duration, resolveRunFor)
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK"} {
		if v, ok := j.Env[k]; !ok || v != "" {
			t.Errorf("the agent's pane env %s = %q, %v; want it set empty", k, v, ok)
		}
	}
	if j.Env["GIT_CONFIG_GLOBAL"] != "/dev/null" || !strings.HasPrefix(j.Env["GH_CONFIG_DIR"], j.Dir) || j.Env["WT_BRANCH"] != "review1" {
		t.Errorf("the agent's pane env = %v, want no git or gh config of yours and the slot's env", j.Env)
	}
	if prompts, _, _, _ := f.agent.counts(); prompts != 1 {
		t.Errorf("%d prompts, want 1", prompts)
	}
	p := f.agent.prompts[0]
	for _, want := range []string{"talkable/talkable#2", "staging-void", f.tip, f.head, "app/a.rb", "db-lock", filepath.Join(j.Dir, resolveResultName)} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt does not name %q:\n%s", want, p)
		}
	}
	if ev := f.h.events("staging.resolve"); len(ev) != 1 {
		t.Errorf("%d staging.resolve events, want the job's end", len(ev))
	}
}

// Result of a job the journal does not hold (or of no id at all) answers
// failed, "unknown job"; a second Start of a live job's effect and try (a
// deploy cut between its Start and its journal) answers that job; a request
// the resolver cannot take is refused.
func TestTheStagingResolverAnswersUnknownJobsAndRefusesBadRequests(t *testing.T) {
	f := newResolveFixture(t)
	for _, id := range []string{"void-2-20261005t100000-abcd.r1.0badf00d", "--all", ""} {
		res, err := f.r.Result(f.h.ctx, id)
		if err != nil || res.State != staging.ResolveFailed || res.Why != whyUnknownJob {
			t.Errorf("Result(%q) = %+v, %v; want failed, unknown job", id, res, err)
		}
		if err := f.r.Cancel(f.h.ctx, id); err != nil {
			t.Errorf("Cancel(%q) = %v, want nothing to stop", id, err)
		}
	}
	f.agentsMu.Lock()
	f.agentsDown = errors.New("no herdr")
	f.agentsMu.Unlock()
	a, b := f.start(1), f.start(1)
	if a != b {
		t.Errorf("a second Start of the same effect and try made job %s beside %s", b, a)
	}
	f.awaitIdle()
	for name, req := range map[string]staging.ResolveRequest{
		"no staging repository": {Repo: "zhuravel/other", Ref: "zhuravel/other#2", Target: "staging-void", TargetTip: f.tip, Head: f.head, Effect: "void-2-x", Try: 1},
		"another PR's repo":     {Repo: "talkable/talkable", Ref: "zhuravel/other#2", Target: "staging-void", TargetTip: f.tip, Head: f.head, Effect: "void-2-x", Try: 1},
		"no staging branch":     {Repo: "talkable/talkable", Ref: "talkable/talkable#2", Target: "master", TargetTip: f.tip, Head: f.head, Effect: "void-2-x", Try: 1},
		"a short tip":           {Repo: "talkable/talkable", Ref: "talkable/talkable#2", Target: "staging-void", TargetTip: "abc", Head: f.head, Effect: "void-2-x", Try: 1},
		"no effect":             {Repo: "talkable/talkable", Ref: "talkable/talkable#2", Target: "staging-void", TargetTip: f.tip, Head: f.head, Effect: "../x", Try: 1},
		"a third try":           {Repo: "talkable/talkable", Ref: "talkable/talkable#2", Target: "staging-void", TargetTip: f.tip, Head: f.head, Effect: "void-2-x", Try: 3},
	} {
		if id, err := f.r.Start(f.h.ctx, req); err == nil {
			t.Errorf("%s: Start answered job %s", name, id)
		}
	}
}

// Before FromApp binds it to the engine, the resolver refuses everything;
// the staging client of a daemon with staging envs has it as its Resolver.
func TestTheStagingClientsResolverRefusesUntilTheEngineIsBound(t *testing.T) {
	r := &stagingResolver{}
	if _, err := r.Start(context.Background(), staging.ResolveRequest{}); !errors.Is(err, errStagingUnbound) {
		t.Errorf("Start before the bind: %v", err)
	}
	if _, err := r.Result(context.Background(), "x"); !errors.Is(err, errStagingUnbound) {
		t.Errorf("Result before the bind: %v", err)
	}
	if err := r.Cancel(context.Background(), "x"); !errors.Is(err, errStagingUnbound) {
		t.Errorf("Cancel before the bind: %v", err)
	}
}

// The daemon's checks fail a job, nothing committed and no ref kept, and
// the slot goes back as it was all the same: the agent gave up (its why
// carried), conflict markers left in a conflicted path, the work tree
// changing a path outside the conflicts (the specs ran on what the merge
// would not hold), or a result file that stays invalid after a nudge.
func TestTheStagingResolversChecksFailAJobAndPutTheSlotBack(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name   string
		agent  func(f *resolveFixture) func(a *fakeStagingAgent, n int, prompt string)
		want   string
		nudged bool
	}{
		{"gave up", func(f *resolveFixture) func(*fakeStagingAgent, int, string) {
			return func(a *fakeStagingAgent, _ int, _ string) {
				a.result(f.h.t, resolveResultFile{Outcome: resolveOutcomeGaveUp, Why: "spec/a_spec.rb fails after the merge", Specs: []string{"spec/a_spec.rb"},
					SpecsResult: resolveSpecsFailed})
			}
		}, "the agent gave up: spec/a_spec.rb fails after the merge", false},
		{"markers left", func(f *resolveFixture) func(*fakeStagingAgent, int, string) {
			return func(a *fakeStagingAgent, _ int, _ string) {
				a.result(f.h.t, resolveResultFile{Outcome: resolveOutcomeResolved, Specs: []string{"spec/a_spec.rb"}, SpecsResult: resolveSpecsPassed})
			}
		}, "conflict markers are left in app/a.rb", false},
		{"a path outside the conflicts", func(f *resolveFixture) func(*fakeStagingAgent, int, string) {
			return func(a *fakeStagingAgent, _ int, _ string) {
				writeFile(f.h.t, a.job.Cwd, "app/a.rb", "a = 2\nb = 1\n")
				writeFile(f.h.t, a.job.Cwd, "README.md", "the agent's own change\n")
				a.result(f.h.t, resolveResultFile{Outcome: resolveOutcomeResolved, Specs: []string{"spec/a_spec.rb"}, SpecsResult: resolveSpecsPassed})
			}
		}, "also changes README.md, outside the conflicts", false},
		{"no valid result", func(f *resolveFixture) func(*fakeStagingAgent, int, string) {
			return func(a *fakeStagingAgent, _ int, _ string) {
				writeFile(f.h.t, a.job.Dir, resolveResultName, `{"outcome": "resolved", "specs": [], "specs_result": "passed"}`)
			}
		}, "no valid result after a nudge: a resolution needs the specs it ran, passed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResolveFixture(t)
			f.reviewed()
			f.agent.onPrompt = tc.agent(f)
			res := f.await(f.start(1))
			if res.State != staging.ResolveFailed || !strings.Contains(res.Why, tc.want) {
				t.Fatalf("Result = %+v, want failed: %s", res, tc.want)
			}
			if res.MergeSHA != "" || res.Dir != "" {
				t.Errorf("a failed job answers merge %q in %q", res.MergeSHA, res.Dir)
			}
			if _, err := f.run.Run(f.h.ctx, execx.Cmd{Name: "git", Args: []string{"-C", f.clone, "rev-parse", "-q", "--verify",
				resolveRefPrefix + f.request(1).Effect}}); err == nil {
				t.Error("a failed job kept a ref")
			}
			if got := f.gitIn(f.slot, "rev-parse", "HEAD"); got != f.head {
				t.Errorf("slot review1's HEAD is %s, want the PR's head", got)
			}
			if got := f.gitIn(f.slot, "status", "--porcelain", "--untracked-files=no"); got != "" {
				t.Errorf("slot review1 is not clean: %q", got)
			}
			if prompts, closed, _, _ := f.agent.counts(); closed != 1 || (prompts == 2) != tc.nudged {
				t.Errorf("%d prompts and %d closes; want the agent closed once, nudged %v", prompts, closed, tc.nudged)
			}
		})
	}
}

// A job never takes or touches a slot a round holds: while PR #2's round
// runs, the job stays queued, "waits for a slot", and the worker stops; the
// tick after the round looks again, and the job runs.
func TestTheStagingResolverWaitsForASlotARoundHolds(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	h := f.h
	cancel := func() {}
	h.e.mu.Lock()
	h.e.rounds[pr.ID] = &roundHandle{cancel: cancel}
	h.e.mu.Unlock()
	id := f.start(1)
	f.awaitIdle()
	res, err := f.r.Result(h.ctx, id)
	if err != nil || res.State != staging.ResolveRunning || !strings.Contains(res.Why, "waits for a slot") || !strings.Contains(res.Why, "round in progress") {
		t.Fatalf("Result while a round holds the PR = %+v, %v; want running, waits for a slot", res, err)
	}
	if prompts, _, _, _ := f.agent.counts(); prompts != 0 {
		t.Fatalf("the agent was prompted while a round held the slot")
	}
	if c := h.sl.all(); slices.ContainsFunc(c, func(s string) bool { return strings.HasPrefix(s, "resync:") }) {
		t.Errorf("the slot was touched while a round held it: %v", c)
	}
	h.e.mu.Lock()
	delete(h.e.rounds, pr.ID)
	h.e.mu.Unlock()
	h.tick()
	if res := f.await(id); res.State != staging.ResolveDone {
		t.Fatalf("Result after the round = %+v, want done", res)
	}
}

// A PR whose slot the pool took back gets a free slot of the pool for the
// job, reserved for it, and handed back to the pool at the end.
func TestTheStagingResolverClaimsAFreeSlotAndHandsItBack(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	h := f.h
	sl := h.slot("review1")
	if err := h.st.ReleaseSlot(h.ctx, sl.ID, []string{store.SlotClaimed, store.SlotHeld}, store.SlotFree, "test"); err != nil {
		t.Fatal(err)
	}
	f.resolves("a = 2\nb = 1\n")
	res := f.await(f.start(1))
	if res.State != staging.ResolveDone {
		t.Fatalf("Result = %+v, want done", res)
	}
	calls := h.sl.all()
	if !slices.Contains(calls, fmt.Sprintf("reserve:%d", pr.ID)) || !slices.ContainsFunc(calls, func(s string) bool { return strings.HasPrefix(s, "release:review1:") }) {
		t.Errorf("slot calls %v; want the free slot reserved for the PR and released at the end", calls)
	}
	if got := h.slot("review1"); got.State != store.SlotFree {
		t.Errorf("slot review1 is %s, want free again", got.State)
	}
}

// working makes the fake agent's turns wait until release is closed (or the
// job's ctx ends): an agent at work. started gets a value once a turn waits.
func (f *resolveFixture) working() (started chan struct{}, release chan struct{}) {
	started, release = make(chan struct{}, 4), make(chan struct{})
	f.agent.mu.Lock()
	f.agent.wait = func(ctx context.Context, _ time.Time) (bool, error) {
		started <- struct{}{}
		select {
		case <-release:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	f.agent.mu.Unlock()
	return started, release
}

// restart starts the daemon again on the same registry and slots, with a
// resolver of its own (the old one's worker ended with its engine's stop).
func (f *resolveFixture) restart() {
	f.h.t.Helper()
	f.h.e.stagingStop()
	f.awaitIdle()
	f.h.e.stagingWG.Wait()
	f.r = &stagingResolver{git: f.git}
	f.h.d.stagingResolver = f.r
	f.h.e = New(f.h.d)
	f.h.t.Cleanup(f.h.e.stagingStop)
	f.h.startup()
}

// Cancel of a queued job fails it "cancelled" at once; of a running one,
// stops its agent, puts its slot back, lets go of its PR and fails it
// "cancelled".
func TestTheStagingResolverCancelsAJob(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	started, _ := f.working()
	id := f.start(1)
	awaitSignal(t, started, "the agent's turn")
	if why := f.h.e.heldReason(pr.ID); why != resolveHeld {
		t.Errorf("the PR is held for %q while the agent works, want %q", why, resolveHeld)
	}
	if res, _ := f.r.Result(f.h.ctx, id); res.State != staging.ResolveRunning || !strings.Contains(res.Why, "p-resolve") || !strings.Contains(res.Why, stagingWorkspace) {
		t.Errorf("Result while the agent works = %+v; want running, naming its pane", res)
	}
	queued, err := f.r.Start(f.h.ctx, staging.ResolveRequest{Repo: "talkable/talkable", Ref: "talkable/talkable#2", Target: "staging-bart",
		TargetTip: f.tip, Head: f.head, Effect: "bart-2-20261005t100000-abcd", Try: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := f.r.Result(f.h.ctx, queued); res.State != staging.ResolveRunning || !strings.Contains(res.Why, "waits for its turn") {
		t.Errorf("the second job's Result = %+v; want it waiting for its turn: one job at a time", res)
	}
	if err := f.r.Cancel(f.h.ctx, queued); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.r.Result(f.h.ctx, queued); res.State != staging.ResolveFailed || res.Why != whyCancelled {
		t.Errorf("the cancelled queued job = %+v, want failed, cancelled", res)
	}
	if err := f.r.Cancel(f.h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if res := f.await(id); res.State != staging.ResolveFailed || res.Why != whyCancelled {
		t.Fatalf("the cancelled running job = %+v, want failed, cancelled", res)
	}
	if _, closed, _, _ := f.agent.counts(); closed != 1 {
		t.Errorf("the agent was closed %d times, want once", closed)
	}
	if got := f.gitIn(f.slot, "rev-parse", "HEAD"); got != f.head || deref(f.h.slot("review1").CheckedOutSHA) != f.head {
		t.Errorf("slot review1 is at %s, want back at the PR's head", got)
	}
	if !f.h.e.reserveSlotWork(pr.ID, "test") {
		t.Error("the PR's reservation was not let go")
	}
	f.h.e.endEviction(pr.ID)
}

// A job's agent is named after the job's deploy, which the operator looks
// for in herdr: the PR's repository (its name) and number and the env (the
// target staging-<env>), "talkable#2 void".
func TestAStagingResolverJobsAgentIsNamedAfterItsDeploy(t *testing.T) {
	f := newResolveFixture(t)
	f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	if res := f.await(f.start(1)); res.State != staging.ResolveDone {
		t.Fatalf("the job = %+v, want done", res)
	}
	f.agentsMu.Lock()
	jobs := slices.Clone(f.agentJobs)
	f.agentsMu.Unlock()
	if len(jobs) != 1 || jobs[0].Title != "talkable#2 void" {
		titles := make([]string, len(jobs))
		for i, j := range jobs {
			titles[i] = j.Title
		}
		t.Fatalf("the agents' titles %q; want one agent, named \"talkable#2 void\"", titles)
	}
}

// A Cancel whose agent stays (its Close fails: herdr shows it at work, and
// a pane of yours keeps its workspace open) fails the job saying so, with
// the agent's pane and why it stays, ahead of the long words of where its
// scratch registry stays, which the Why's clip would otherwise keep alone.
func TestACancelledResolverJobWhoseAgentStaysNamesItsPaneAndWhy(t *testing.T) {
	f := newResolveFixture(t)
	f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	started, _ := f.working()
	id := f.start(1)
	awaitSignal(t, started, "the agent's turn")
	f.agent.mu.Lock()
	f.agent.closeErr = fmt.Errorf("agents: park pr 1: claude-fix is working: agent busy\n"+
		"agents: quit session 1: claude-fix is working before its ctrl+c, no key sent: agent busy\n"+
		"its workspace w26 holds a pane beside the agent's, so it stays open; the %q workspace may still run the agent, "+
		"and its registry stays at %s", stagingWorkspace, filepath.Join(f.h.e.resolveDir(id), learnScratch))
	f.agent.mu.Unlock()
	if err := f.r.Cancel(f.h.ctx, id); err != nil {
		t.Fatal(err)
	}
	res := f.await(id)
	if res.State != staging.ResolveFailed || !strings.HasPrefix(res.Why, whyCancelled+";") ||
		!strings.Contains(res.Why, "claude-fix is working") || !strings.Contains(res.Why, "pane p-resolve") {
		t.Fatalf("the cancelled job = %+v; want failed, cancelled, naming the agent's pane p-resolve and why it stays", res)
	}
}

// A restart goes on with a job whose agent still runs: it holds its PR
// again before any round, the agent's turn is waited for again and the job
// ends done; one whose agent is gone fails, saying so, and its slot goes
// back. The table stays out of the parallel phase: it waits for the agent's
// turn in real time (awaitSignal).
func TestARestartGoesOnWithAResolverJobWhoseAgentRuns(t *testing.T) {
	storetest.Serial(t)
	for _, alive := range []bool{true, false} {
		t.Run(fmt.Sprintf("alive=%v", alive), func(t *testing.T) {
			f := newResolveFixture(t)
			pr := f.reviewed()
			f.resolves("a = 2\nb = 1\n")
			started, _ := f.working()
			id := f.start(1)
			awaitSignal(t, started, "the agent's turn")
			f.agent.mu.Lock()
			f.agent.alive, f.agent.wait = alive, nil // what the restart finds; the turn waited for now goes on
			f.agent.mu.Unlock()
			f.restart()
			if _, _, detached, _ := f.agent.counts(); detached != 1 {
				t.Errorf("the stop detached the agent %d times, want once (it goes on)", detached)
			}
			res := f.await(id)
			if _, _, _, resumed := f.agent.counts(); resumed != 1 {
				t.Errorf("the restart resumed the agent %d times, want once", resumed)
			}
			switch {
			case alive && res.State != staging.ResolveDone:
				t.Fatalf("Result after a restart whose agent runs = %+v, want done", res)
			case !alive && (res.State != staging.ResolveFailed || !strings.Contains(res.Why, "its agent is gone")):
				t.Fatalf("Result after a restart whose agent is gone = %+v, want failed, the agent gone", res)
			}
			if got := f.gitIn(f.slot, "rev-parse", "HEAD"); got != f.head {
				t.Errorf("slot review1's HEAD is %s, want the PR's head", got)
			}
			if !f.h.e.reserveSlotWork(pr.ID, "test") {
				t.Error("the PR's reservation was not let go")
			}
			f.h.e.endEviction(pr.ID)
		})
	}
}

// A restart that finds a job whose agent never got its prompt puts its
// slot back and queues it again: it runs to its end after the restart.
func TestARestartStartsAgainAResolverJobBeforeItsAgent(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	f.agentsMu.Lock()
	f.agentsDown = errors.New("not yet")
	f.agentsMu.Unlock()
	id := f.start(1)
	f.awaitIdle()
	// As a crash before the prompt leaves it: running in its slot, merged.
	if _, err := f.r.update(f.h.ctx, f.h.e, id, func(j *resolveJob) {
		j.State, j.Phase, j.Why, j.Ended, j.PRID, j.SlotID, j.Slot, j.SlotWas, j.Began = resolveRunning, resolvePhaseAgent, "", time.Time{},
			pr.ID, f.h.slot("review1").ID, "review1", f.head, f.h.clock.Now()
	}); err != nil {
		t.Fatal(err)
	}
	f.agentsMu.Lock()
	f.agentsDown = nil
	f.agentsMu.Unlock()
	f.resolves("a = 2\nb = 1\n")
	f.restart()
	if res := f.await(id); res.State != staging.ResolveDone {
		t.Fatalf("Result after the restart = %+v, want done", res)
	}
}

// A job runs at most 45 minutes from its slot on: past them its agent is
// stopped and it fails, before the deploy's own wait of 2 hours ends it;
// one that waited an hour for its turn and its slot fails too.
func TestAStagingResolverJobRunsAtMost45Minutes(t *testing.T) {
	f := newResolveFixture(t)
	f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	f.agent.mu.Lock()
	f.agent.wait = func(_ context.Context, until time.Time) (bool, error) {
		if !until.Equal(f.h.clock.Now().Add(resolveRunFor)) && until.After(f.h.clock.Now().Add(resolveRunFor)) {
			t.Errorf("the agent may run until %s, past the job's 45 minutes", until)
		}
		f.h.advance(resolveRunFor + time.Minute)
		return false, nil
	}
	f.agent.mu.Unlock()
	res := f.await(f.start(1))
	if res.State != staging.ResolveFailed || !strings.Contains(res.Why, "ran over 45 minutes") {
		t.Fatalf("Result = %+v, want failed: ran over 45 minutes", res)
	}
	if _, closed, _, _ := f.agent.counts(); closed != 1 {
		t.Errorf("the agent was closed %d times, want once", closed)
	}
	if resolveRunFor+resolveWaitFor >= 2*time.Hour {
		t.Errorf("a job can take %s, not under the deploy's 2 hours", resolveRunFor+resolveWaitFor)
	}

	f.agent.mu.Lock()
	f.agent.wait = nil
	f.agent.mu.Unlock()
	h := f.h
	pr := h.pr(2)
	h.e.mu.Lock()
	h.e.rounds[pr.ID] = &roundHandle{cancel: func() {}}
	h.e.mu.Unlock()
	req := f.request(2)
	id, err := f.r.Start(h.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	f.awaitIdle()
	h.advance(resolveWaitFor)
	h.tick()
	f.awaitIdle()
	if res, _ := f.r.Result(h.ctx, id); res.State != staging.ResolveFailed || !strings.Contains(res.Why, "waited over 1 hour") {
		t.Errorf("Result after an hour without a slot = %+v, want failed", res)
	}
	h.e.mu.Lock()
	delete(h.e.rounds, pr.ID)
	h.e.mu.Unlock()
}

// hasRef reports whether the clone has ref.
func (f *resolveFixture) hasRef(ref string) bool {
	_, err := f.run.Run(f.h.ctx, execx.Cmd{Name: "git", Args: []string{"-C", f.clone, "rev-parse", "-q", "--verify", ref}})
	return err == nil
}

// effectIs makes the fake staging client's journal hold effect in state.
func (f *resolveFixture) effectIs(effect, state string) {
	f.staging.mu.Lock()
	defer f.staging.mu.Unlock()
	f.staging.effects = map[string][]staging.Effect{"talkable/talkable": {{ID: staging.EffectID(effect), Repo: "talkable/talkable", State: state}}}
}

// The ref that keeps a done job's merge in its repository stays while the
// job's effect is live and goes at the first tick after it ended, or a day
// after the job's end whatever the effect says; an ended job's row goes two
// days after its end, with its directory, and its Result is then an
// unknown job's.
func TestTheTickDropsAResolvedMergesRefOnceItsEffectEnded(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name  string
		after func(f *resolveFixture)
	}{
		{"the effect ended", func(f *resolveFixture) { f.effectIs(f.request(1).Effect, staging.EffectDone) }},
		{"a day passed", func(f *resolveFixture) { f.h.advance(resolveRefKeep) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResolveFixture(t)
			f.reviewed()
			f.resolves("a = 2\nb = 1\n")
			effect := f.request(1).Effect
			f.effectIs(effect, staging.EffectRunning)
			id := f.start(1)
			if res := f.await(id); res.State != staging.ResolveDone {
				t.Fatalf("Result = %+v, want done", res)
			}
			ref := resolveRefPrefix + effect
			f.h.tick()
			if !f.hasRef(ref) || !f.job(id).Kept {
				t.Fatalf("the tick dropped %s while the effect is live", ref)
			}
			tc.after(f)
			f.h.tick()
			if f.hasRef(ref) || f.job(id).Kept {
				t.Fatalf("%s stays after %s", ref, tc.name)
			}
			if _, err := os.Stat(f.h.e.resolveDir(id)); err != nil {
				t.Errorf("the job's directory went before its row: %v", err)
			}
			f.h.advance(resolveKeep)
			f.h.tick()
			if res, err := f.r.Result(f.h.ctx, id); err != nil || res.State != staging.ResolveFailed || res.Why != whyUnknownJob {
				t.Errorf("Result two days after the end = %+v, %v; want an unknown job", res, err)
			}
			if _, err := os.Stat(f.h.e.resolveDir(id)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the job's directory stays: %v", err)
			}
		})
	}
}

// With no [[factory.repo.staging]] entries there is no staging client, so
// no resolver and no resolver's agent maker in the daemon's wiring: a job's
// row an earlier config left stays as it is through the start and the
// ticks, and nothing holds its PR. With entries, FromApp makes the client
// with the engine's resolver, bound to it, and the pane agent's maker.
func TestWithoutStagingEntriesNothingOfTheResolverRuns(t *testing.T) {
	e, _, _ := appEngine(t, nil, app.Options{}, nil)
	if e.d.stagingResolver != nil || e.d.Staging != nil {
		t.Fatalf("a config without staging entries wired a resolver (%v) or a staging client (%v)", e.d.stagingResolver, e.d.Staging)
	}
	with, _, _ := appEngine(t, nil, app.Options{}, func(cfg *config.Config) {
		cfg.Factory.Repos = append(cfg.Factory.Repos, config.FactoryRepo{Repo: "talkable/talkable", Staging: slices.Clone(testStagings)})
	})
	t.Cleanup(with.stagingStop)
	if with.d.stagingResolver == nil || with.d.stagingResolver.e.Load() != with {
		t.Fatal("a config with staging entries wired no resolver bound to the engine")
	}

	h := newHarness(t)
	h.reviewedPR(2, fixHead)
	pr := h.pr(2)
	row := resolveJob{ID: "void-2-x.r1.0a0b0c0d", Effect: "void-2-x", Repo: "talkable/talkable", Number: 2, Target: "staging-void",
		TargetTip: strings.Repeat("a", 40), Head: fixHead, Try: 1, State: resolveRunning, Phase: resolvePhaseSlot, PRID: pr.ID,
		SlotID: h.slot("review1").ID, Slot: "review1", Started: h.clock.Now(), Updated: h.clock.Now()}
	if err := h.e.saveResolveJob(h.ctx, row); err != nil {
		t.Fatal(err)
	}
	before, _ := h.kvOf(kvStagingResolve(row.ID))
	h.startup()
	h.advance(resolveKeep + time.Hour)
	h.tick()
	if got, _ := h.kvOf(kvStagingResolve(row.ID)); got != before {
		t.Errorf("the job's row changed without a resolver: %s", got)
	}
	if why := h.e.heldReason(pr.ID); why == resolveHeld {
		t.Error("the PR is held for a resolver the daemon does not have")
	}
	if c := h.sl.all(); slices.ContainsFunc(c, func(s string) bool { return strings.HasPrefix(s, "resync:") }) {
		t.Errorf("the slot was touched: %v", c)
	}
}

// A resolution that deletes the conflicted path (one side removed what the
// other changed, and the context says it goes) is a merge without it.
func TestTheStagingResolverCommitsAResolutionThatDeletesTheConflictedPath(t *testing.T) {
	f := newResolveFixture(t)
	f.reviewed()
	f.agent.onPrompt = func(a *fakeStagingAgent, _ int, _ string) {
		if err := os.Remove(filepath.Join(a.job.Cwd, "app/a.rb")); err != nil {
			t.Error(err)
		}
		a.result(t, resolveResultFile{Outcome: resolveOutcomeResolved, Specs: []string{"spec/a_spec.rb"}, SpecsResult: resolveSpecsPassed})
	}
	res := f.await(f.start(1))
	if res.State != staging.ResolveDone {
		t.Fatalf("Result = %+v, want done", res)
	}
	if got := f.gitIn(f.clone, "ls-tree", "-r", "--name-only", res.MergeSHA); strings.Contains(got, "app/a.rb") || !strings.Contains(got, "db/schema.rb") {
		t.Errorf("the merge lists %q; want db/schema.rb and no app/a.rb", got)
	}
}

// A PR whose round is paused (its sessions wait to go on in its slot) or in
// flight is never touched either: the job waits for a slot.
func TestTheStagingResolverWaitsWhileThePRsRoundIsPaused(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	if _, err := f.h.st.DB().ExecContext(f.h.ctx, `UPDATE prs SET state = ? WHERE id = ?`, store.PRPaused, pr.ID); err != nil {
		t.Fatal(err)
	}
	id := f.start(1)
	f.awaitIdle()
	res, err := f.r.Result(f.h.ctx, id)
	if err != nil || res.State != staging.ResolveRunning || !strings.Contains(res.Why, "waits for a slot") || !strings.Contains(res.Why, store.PRPaused) {
		t.Fatalf("Result while the PR's round is paused = %+v, %v; want running, waits for a slot", res, err)
	}
	if c := f.h.sl.all(); slices.ContainsFunc(c, func(s string) bool { return strings.HasPrefix(s, "resync:") }) {
		t.Errorf("the slot was touched under a paused round: %v", c)
	}
}

// A second try (the branch's tip moved after the first try's merge, and the
// merge conflicts again) carries the first try's why in its prompt.
func TestASecondTrysPromptCarriesTheFirstTrysWhy(t *testing.T) {
	f := newResolveFixture(t)
	f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	first := f.await(f.start(1))
	if first.State != staging.ResolveDone {
		t.Fatalf("the first try = %+v, want done", first)
	}
	if res := f.await(f.start(2)); res.State != staging.ResolveDone {
		t.Fatalf("the second try = %+v, want done", res)
	}
	if prompts, _, _, _ := f.agent.counts(); prompts != 2 {
		t.Fatalf("%d prompts, want one per try", prompts)
	}
	p := f.agent.prompts[1]
	if !strings.Contains(p, "second try") || !strings.Contains(p, first.Why) {
		t.Errorf("the second try's prompt does not carry the first's why %q:\n%s", first.Why, p)
	}
	if strings.Contains(f.agent.prompts[0], "second try") {
		t.Error("the first try's prompt says it is the second")
	}
}

// While the fix role's agent kind is paused (a usage limit, a logout) the
// job does not start its agent: it fails at once, saying why, its slot put
// back, so the deploy does not wait 45 minutes for an agent that cannot run.
func TestAStagingResolverJobFailsWhileItsAgentKindIsPaused(t *testing.T) {
	f := newResolveFixture(t)
	f.reviewed()
	f.h.e.PauseTool(f.h.ctx, pipeline.Pause{Kind: string(agents.HealthUsageLimit), Tool: "claude", Until: f.h.clock.Now().Add(time.Hour)})
	res := f.await(f.start(1))
	if res.State != staging.ResolveFailed || !strings.Contains(res.Why, "claude paused") {
		t.Fatalf("Result while claude is paused = %+v, want failed, saying claude is paused", res)
	}
	if prompts, _, _, _ := f.agent.counts(); prompts != 0 {
		t.Error("the agent was prompted while its kind is paused")
	}
	if got := f.gitIn(f.slot, "rev-parse", "HEAD"); got != f.head {
		t.Errorf("slot review1's HEAD is %s, want the PR's head back", got)
	}
}

// StagingResolverSays is what the newest live job of an effect does now,
// its try and its Why made safe for a terminal, for the card and `magnum
// staging`; an ended job, a job of another effect and a row under another
// key say nothing.
func TestStagingResolverSaysWhatTheLiveJobOfAnEffectDoes(t *testing.T) {
	row := func(j resolveJob) string {
		b, err := json.Marshal(j)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	const effect = "bart-5-20261005t100000-abcd"
	values := map[string]string{}
	put := func(j resolveJob) { values[kvStagingResolve(j.ID)] = row(j) }
	put(resolveJob{ID: effect + ".r1.00000001", Effect: effect, Try: 1, State: resolveFailed, Why: "the agent gave up", Started: at})
	if got := StagingResolverSays(values, effect); got != "" {
		t.Fatalf("an ended job says %q", got)
	}
	put(resolveJob{ID: effect + ".r2.00000002", Effect: effect, Try: 2, State: resolveRunning, Why: "the agent resolves \x1b[31m1 path", Started: at.Add(time.Minute)})
	put(resolveJob{ID: "void-5-20261005t090000-abcd.r1.00000003", Effect: "void-5-20261005t090000-abcd", Try: 1, State: resolveQueued,
		Why: "waits for its turn", Started: at})
	values["staging.resolve.elsewhere"] = row(resolveJob{ID: effect + ".r1.00000004", Effect: effect, Try: 1, State: resolveQueued, Why: "a stray row",
		Started: at.Add(time.Hour)})
	if got := StagingResolverSays(values, effect); got != "try 2: the agent resolves  [31m1 path" {
		t.Fatalf("the live job says %q", got)
	}
	if got := StagingResolverSays(values, "void-5-20261005t090000-abcd"); got != "try 1: waits for its turn" {
		t.Fatalf("a queued job says %q", got)
	}
}
