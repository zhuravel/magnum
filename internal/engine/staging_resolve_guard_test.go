package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
)

// typed sets PR #2's human_active_at to at, as the agents package does when
// someone types into one of its magnum panes.
func (f *resolveFixture) typed(pr store.PR, at time.Time) {
	f.h.t.Helper()
	if err := f.h.st.UpdatePR(f.h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", at) }); err != nil {
		f.h.t.Fatal(err)
	}
}

// resyncs counts the fake slots' Resync calls.
func (f *resolveFixture) resyncs() int {
	n := 0
	for _, c := range f.h.sl.all() {
		if strings.HasPrefix(c, "resync:") {
			n++
		}
	}
	return n
}

// slotBack checks that slot review1 is back at the PR's head: HEAD and the
// recorded checkout, no tracked change, no merge in progress, no hold.
func (f *resolveFixture) slotBack(t *testing.T) {
	t.Helper()
	if got := f.gitIn(f.slot, "rev-parse", "HEAD"); got != f.head {
		t.Errorf("slot review1's HEAD is %s, want the PR's head %s", got, f.head)
	}
	if got := f.gitIn(f.slot, "status", "--porcelain", "--untracked-files=no"); got != "" {
		t.Errorf("slot review1 is not clean: %q", got)
	}
	if _, err := f.run.Run(f.h.ctx, execx.Cmd{Name: "git", Args: []string{"-C", f.slot, "rev-parse", "-q", "--verify", "MERGE_HEAD"}}); err == nil {
		t.Error("a merge is still in progress in slot review1")
	}
	if sl := f.h.slot("review1"); deref(sl.CheckedOutSHA) != f.head || sl.HoldReason != nil {
		t.Errorf("slot review1 records %s, hold %v; want the PR's head and no hold", deref(sl.CheckedOutSHA), deref(sl.HoldReason))
	}
}

// letGo checks that nothing holds PR pr for the resolver any more.
func (f *resolveFixture) letGo(t *testing.T, pr store.PR) {
	t.Helper()
	if !f.h.e.reserveSlotWork(pr.ID, "test") {
		t.Errorf("PR #%d is still held: %s", pr.Number, f.h.e.heldReason(pr.ID))
		return
	}
	f.h.e.endEviction(pr.ID)
}

// Someone typed into PR #2's magnum panes after its slot's checkout (a
// question to its reviewer after the round): the job waits out the human
// cooldown, as the fixer does, then takes the slot as magnum's checkout
// (last_used_at), so putting it back reads the job's own merge as magnum's,
// not a person's: the slot goes back at the PR's head, clean, with no
// dirty_worktree hold.
func TestTheStagingResolverPutsBackASlotWhosePRsPanesSomeoneTypedInto(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	cooldown := f.h.cfg.Daemon.HumanCooldown.Duration
	if cooldown <= 0 {
		t.Fatal("the harness has no human_cooldown")
	}
	f.typed(pr, f.h.clock.Now())
	id := f.start(1)
	f.awaitIdle()
	if res, _ := f.r.Result(f.h.ctx, id); res.State != staging.ResolveRunning || !strings.Contains(res.Why, "waits for a slot") ||
		!strings.Contains(res.Why, "human_cooldown") {
		t.Fatalf("Result while the PR's panes are a person's = %+v; want it waiting out the human cooldown", res)
	}
	if n := f.resyncs(); n != 0 {
		t.Fatalf("the slot was moved %d times within the human cooldown", n)
	}
	f.h.advance(cooldown)
	f.h.tick()
	if res := f.await(id); res.State != staging.ResolveDone {
		t.Fatalf("Result after the cooldown = %+v, want done", res)
	}
	f.slotBack(t)
	f.letGo(t, pr)
}

// A slot the guard holds (its HEAD drifted, or it carries commits on no
// remote: a person's work) or that a pin keeps is never touched: the job
// waits for a slot, and its PR is let go meanwhile.
func TestTheStagingResolverWaitsForASlotTheGuardHoldsOrAPinKeeps(t *testing.T) {
	for _, tc := range []struct {
		name string
		keep func(f *resolveFixture)
		want string
	}{
		{"head drift", func(f *resolveFixture) {
			f.h.sl.mu.Lock()
			f.h.sl.guardErr = slots.ErrHold{Reason: slots.HoldHeadDrift, Detail: "review1's HEAD moved"}
			f.h.sl.mu.Unlock()
		}, slots.HoldHeadDrift},
		{"unpushed commits", func(f *resolveFixture) {
			f.h.sl.mu.Lock()
			f.h.sl.guardErr = slots.ErrHold{Reason: slots.HoldUnpushed, Detail: "review1 has 2 commits on no remote"}
			f.h.sl.mu.Unlock()
		}, slots.HoldUnpushed},
		{"pinned", func(f *resolveFixture) {
			if err := f.h.st.UpdateSlotFields(f.h.ctx, f.h.slot("review1").ID, func(u *store.SlotUpdate) { u.Set("pinned", true) }); err != nil {
				f.h.t.Fatal(err)
			}
		}, "pinned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResolveFixture(t)
			pr := f.reviewed()
			f.resolves("a = 2\nb = 1\n")
			tc.keep(f)
			id := f.start(1)
			f.awaitIdle()
			res, err := f.r.Result(f.h.ctx, id)
			if err != nil || res.State != staging.ResolveRunning || !strings.Contains(res.Why, "waits for a slot") || !strings.Contains(res.Why, tc.want) {
				t.Fatalf("Result = %+v, %v; want it waiting for a slot: %s", res, err, tc.want)
			}
			if n := f.resyncs(); n != 0 {
				t.Errorf("the slot was moved %d times", n)
			}
			if prompts, _, _, _ := f.agent.counts(); prompts != 0 {
				t.Errorf("the agent was prompted %d times", prompts)
			}
			if got := f.gitIn(f.slot, "rev-parse", "HEAD"); got != f.head {
				t.Errorf("slot review1's HEAD is %s, want it left at the PR's head", got)
			}
			f.letGo(t, pr)
		})
	}
}

// lyingGit is the resolver's git whose merge in the work tree names a
// conflict merge-tree does not.
type lyingGit struct{ *gitx.Client }

func (g lyingGit) MergeNoCommit(ctx context.Context, dir, rev string, who gitx.Signature) ([]string, error) {
	paths, err := g.Client.MergeNoCommit(ctx, dir, rev, who)
	return append(paths, "README.md"), err
}

// A checkout whose merge conflicts elsewhere than merge-tree's (what the
// deploy's verify compares with) fails the job before its agent: what the
// agent would resolve is not what the deploy checks. The slot goes back.
func TestTheStagingResolverFailsAMergeWhoseConflictsAreNotMergeTrees(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	f.r.git = lyingGit{f.git}
	res := f.await(f.start(1))
	if res.State != staging.ResolveFailed || !strings.Contains(res.Why, "the checkout's merge conflicts in") {
		t.Fatalf("Result = %+v, want failed: the conflicts differ from merge-tree's", res)
	}
	if prompts, _, _, _ := f.agent.counts(); prompts != 0 {
		t.Errorf("the agent was prompted %d times", prompts)
	}
	f.slotBack(t)
	f.letGo(t, pr)
}

// moreConflicts gives the tip and the head one more commit each (edit makes
// it, for side "tip" or "head"), fetched into the clone, with slot review1
// at the new head.
func (f *resolveFixture) moreConflicts(edit func(dir, side string)) {
	f.h.t.Helper()
	o := f.origin
	for _, side := range []struct{ name, branch string }{{"tip", "staging-void"}, {"head", "feature"}} {
		f.gitIn(o, "checkout", "--quiet", side.branch)
		edit(o, side.name)
		f.gitIn(o, "add", "-A")
		f.gitIn(o, "commit", "--quiet", "-m", side.name+" moves again")
	}
	f.tip, f.head = f.gitIn(o, "rev-parse", "staging-void"), f.gitIn(o, "rev-parse", "feature")
	f.gitIn(o, "checkout", "--quiet", "master")
	f.gitIn(f.clone, "fetch", "--quiet", "origin", "staging-void", "feature")
	f.gitIn(f.slot, "switch", "--quiet", "--detach", f.head)
}

// The rules the agent must give up on are the daemon's too, whatever the
// agent would write: a binary file that conflicts (git keeps the tip's
// version, so a "resolved" one would drop the PR's change without a word)
// and a file both sides renamed to two paths fail the job before its agent
// starts, naming the paths, and the slot goes back.
func TestTheStagingResolverGivesUpOnABinaryOrARenameRenameConflictItself(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, dir, side string)
		want string
	}{
		{"binary", func(t *testing.T, dir, side string) {
			t.Helper()
			writeFile(t, dir, "app/assets/logo.png", "\x00\x01"+side+"\x02")
		}, "a binary file conflicts: app/assets/logo.png"},
		{"rename/rename", func(t *testing.T, dir, side string) {
			t.Helper()
			if err := os.Rename(filepath.Join(dir, "lib/r.rb"), filepath.Join(dir, "lib/r_"+side+".rb")); err != nil {
				t.Fatal(err)
			}
		}, "both sides renamed one file to different paths: lib/r.rb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResolveFixture(t)
			f.moreConflicts(func(dir, side string) { tc.edit(t, dir, side) })
			pr := f.reviewed()
			f.agent.onPrompt = func(a *fakeStagingAgent, _ int, _ string) {
				writeFile(t, a.job.Cwd, "app/a.rb", "a = 2\nb = 1\n")
				a.result(t, resolveResultFile{Outcome: resolveOutcomeResolved, Specs: []string{"spec/a_spec.rb"}, SpecsResult: resolveSpecsPassed})
			}
			res := f.await(f.start(1))
			if res.State != staging.ResolveFailed || !strings.Contains(res.Why, tc.want) {
				t.Fatalf("Result = %+v, want failed: %s", res, tc.want)
			}
			if prompts, _, _, _ := f.agent.counts(); prompts != 0 {
				t.Errorf("the agent was prompted %d times", prompts)
			}
			f.slotBack(t)
			f.letGo(t, pr)
		})
	}
}

// A file the agent adds is in the work tree the specs ran on but in no
// merge (the commit takes the conflicted paths alone), so a resolution
// that adds one fails the job, naming it; the slot goes back without the
// files the job added, and an untracked file it held before stays.
func TestTheStagingResolverFailsAResolutionThatAddsAFile(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	writeFile(t, f.slot, "notes.txt", "a person's notes\n")
	f.agent.onPrompt = func(a *fakeStagingAgent, _ int, _ string) {
		writeFile(t, a.job.Cwd, "app/a.rb", "a = 2\nrequire_relative 'b_helper'\n")
		writeFile(t, a.job.Cwd, "app/b_helper.rb", "b = 1\n")
		a.result(t, resolveResultFile{Outcome: resolveOutcomeResolved, Specs: []string{"spec/a_spec.rb"}, SpecsResult: resolveSpecsPassed})
	}
	res := f.await(f.start(1))
	if res.State != staging.ResolveFailed || !strings.Contains(res.Why, "the checkout also adds app/b_helper.rb, outside the conflicts") {
		t.Fatalf("Result = %+v, want failed: the checkout adds app/b_helper.rb", res)
	}
	if f.hasRef(resolveRefPrefix + f.request(1).Effect) {
		t.Error("a failed job kept a ref")
	}
	f.slotBack(t)
	if _, err := os.Stat(filepath.Join(f.slot, "app/b_helper.rb")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file the job added stays in the slot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.slot, "notes.txt")); err != nil {
		t.Errorf("the untracked file the slot held before the job went: %v", err)
	}
	f.letGo(t, pr)
}

// A coworker's PR is refused (the resolver takes your own PRs only, as the
// deploy does): the job fails, and its slot is never touched.
func TestTheStagingResolverRefusesACoworkersPR(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	if err := f.h.st.UpdatePR(f.h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("author_login", "alice") }); err != nil {
		t.Fatal(err)
	}
	before := f.resyncs()
	res := f.await(f.start(1))
	if res.State != staging.ResolveFailed || !strings.Contains(res.Why, "is not yours") {
		t.Fatalf("Result = %+v, want failed: the PR is not yours", res)
	}
	if n := f.resyncs(); n != before {
		t.Errorf("the coworker's slot was moved %d times", n-before)
	}
	if prompts, _, _, _ := f.agent.counts(); prompts != 0 {
		t.Errorf("the agent was prompted %d times", prompts)
	}
	f.letGo(t, pr)
}

// A job that waits for its slot does not hold back the next: while PR #1's
// job waits for its round, PR #2's (started after it, its slot free) runs to
// its end, one job at a time still.
func TestAResolverJobThatWaitsForItsSlotDoesNotHoldBackTheNext(t *testing.T) {
	f := newResolveFixture(t)
	f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	one := f.h.pr(1)
	if _, err := f.h.st.DB().ExecContext(f.h.ctx, `UPDATE prs SET state = ?, author_login = 'zhuravel' WHERE id = ?`, store.PRPaused, one.ID); err != nil {
		t.Fatal(err)
	}
	waiting, err := f.r.Start(f.h.ctx, staging.ResolveRequest{Repo: "talkable/talkable", Ref: "talkable/talkable#1", Target: "staging-void",
		TargetTip: f.tip, Head: f.head, Effect: "void-1-20261005t100000-abcd", Try: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.awaitIdle()
	f.h.advance(time.Minute)
	if res := f.await(f.start(1)); res.State != staging.ResolveDone {
		t.Fatalf("PR #2's job = %+v, want done while PR #1's waits", res)
	}
	if res, _ := f.r.Result(f.h.ctx, waiting); res.State != staging.ResolveRunning || !strings.Contains(res.Why, "waits for a slot") {
		t.Errorf("PR #1's job = %+v, want it still waiting for its slot", res)
	}
}

// panicGuard is gitSlots whose Guard panics once while armed: a bug in the
// job's take, after it reserved its PR.
type panicGuard struct {
	*gitSlots
	armed atomic.Bool
}

func (p *panicGuard) Guard(ctx context.Context, slot store.Slot) error {
	if p.armed.CompareAndSwap(true, false) {
		panic("a bug in the guard")
	}
	return p.gitSlots.Guard(ctx, slot)
}

// A panic in a job ends it failed, saying so, and lets go of what it held
// (the PR's reservation), so the PR's rounds and the next job go on.
func TestAPanicInAStagingResolverJobEndsItAndLetsGoOfWhatItHeld(t *testing.T) {
	var pg *panicGuard
	f := newResolveFixture(t, func(h *harness) {
		pg = &panicGuard{gitSlots: h.d.Slots.(*gitSlots)}
		h.d.Slots = pg
	})
	pr := f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	pg.armed.Store(true)
	res := f.await(f.start(1))
	if res.State != staging.ResolveFailed || !strings.Contains(res.Why, "a bug in the guard") {
		t.Fatalf("Result after a panic = %+v, want failed, naming it", res)
	}
	f.letGo(t, pr)
	if res := f.await(f.start(2)); res.State != staging.ResolveDone {
		t.Fatalf("the next job after a panic = %+v, want done", res)
	}
}

// A Cancel that ends a queued job between the worker's read of the jobs and
// its start of that job is not undone: the worker does not take the job,
// and a run of its stale copy neither brings the row back nor keeps what it
// took.
func TestAWorkersStaleCopyNeverBringsBackAJobACancelEnded(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	h := f.h
	h.e.mu.Lock()
	h.e.rounds[pr.ID] = &roundHandle{cancel: func() {}}
	h.e.mu.Unlock()
	id := f.start(1)
	f.awaitIdle()
	stale, ok := f.r.next(h.ctx, h.e, nil)
	if !ok || stale.ID != id {
		t.Fatalf("next = %+v, %v; want the queued job", stale, ok)
	}
	if err := f.r.Cancel(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	h.e.mu.Lock()
	delete(h.e.rounds, pr.ID)
	h.e.mu.Unlock()
	if _, ok := f.r.begin(h.ctx, h.e, stale.ID, func() {}); ok {
		t.Error("the worker began a job a Cancel ended")
	}
	f.r.mu.Lock()
	f.r.running = ""
	f.r.mu.Unlock()
	h.e.runResolveJob(h.ctx, h.ctx, f.r, &stale)
	if res, _ := f.r.Result(h.ctx, id); res.State != staging.ResolveFailed || res.Why != whyCancelled {
		t.Errorf("the cancelled job after its stale run = %+v, want failed, cancelled", res)
	}
	if prompts, _, _, _ := f.agent.counts(); prompts != 0 {
		t.Errorf("the agent was prompted %d times for a cancelled job", prompts)
	}
	f.slotBack(t)
	f.letGo(t, pr)
}

// A restart holds the PR of each job it goes on with before any tick: the
// worker held back, the PR is the resolver's and a round due on it (a new
// head) does not start; then the job goes on to its end.
func TestARestartHoldsAResumedResolverJobsPRBeforeAnyRound(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	f.resolves("a = 2\nb = 1\n")
	started, release := f.working()
	id := f.start(1)
	awaitSignal(t, started, "the agent's turn")
	f.agent.mu.Lock()
	f.agent.alive = true
	f.agent.mu.Unlock()
	h := f.h
	h.e.stagingStop()
	f.awaitIdle()
	h.e.stagingWG.Wait()
	f.r = &stagingResolver{git: f.git, working: true} // its worker held back: the recovery alone runs
	h.d.stagingResolver = f.r
	h.e = New(h.d)
	t.Cleanup(h.e.stagingStop)
	h.startup()
	if why := h.e.heldReason(pr.ID); why != resolveHeld {
		t.Fatalf("after the restart PR #2 is held for %q, want %q", why, resolveHeld)
	}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: strings.Repeat("e", 40), author: "zhuravel"})
	for _, d := range []time.Duration{16 * time.Minute, 16 * time.Minute, time.Minute} { // past min_rereview_interval, within the job's 45 minutes
		h.advance(d)
		h.tick()
	}
	if got := h.pr(2); slices.Contains(store.InFlightStates, got.State) {
		t.Fatalf("a round started on PR #2 (%s) while the resumed job holds it", got.State)
	}
	if c := h.sl.all(); slices.ContainsFunc(c, func(s string) bool {
		return strings.HasPrefix(s, "checkout:review1:") && strings.HasSuffix(s, strings.Repeat("e", 40))
	}) {
		t.Fatalf("the slot was checked out for the new head under the resumed job: %v", c)
	}
	f.r.mu.Lock()
	f.r.working = false
	f.r.mu.Unlock()
	f.r.wake(h.e)
	awaitSignal(t, started, "the resumed agent's turn")
	close(release)
	if res := f.await(id); res.State != staging.ResolveDone {
		t.Fatalf("the resumed job = %+v, want done", res)
	}
}

// A job cancelled while the daemon was down (its Cancel journaled, no
// worker to act on it) ends "cancelled" at the start: its slot goes back
// from the merge it held, and its PR is let go.
func TestARestartEndsAResolverJobCancelledWhileTheDaemonWasDown(t *testing.T) {
	f := newResolveFixture(t)
	pr := f.reviewed()
	f.agentsMu.Lock()
	f.agentsDown = errors.New("not yet")
	f.agentsMu.Unlock()
	id := f.start(1)
	f.awaitIdle()
	sl := f.h.slot("review1")
	if _, err := f.r.update(f.h.ctx, f.h.e, id, func(j *resolveJob) {
		j.State, j.Phase, j.Why, j.Ended, j.PRID, j.SlotID, j.Slot, j.SlotWas, j.Began, j.Cancel = resolveRunning, resolvePhaseAgent, "",
			time.Time{}, pr.ID, sl.ID, "review1", f.head, f.h.clock.Now(), true
	}); err != nil {
		t.Fatal(err)
	}
	f.gitIn(f.slot, "switch", "--quiet", "--detach", f.tip)
	_, _ = f.run.Run(f.h.ctx, execx.Cmd{Name: "git", Args: []string{"-C", f.slot, "-c", "user.name=t", "-c", "user.email=t@example.com",
		"merge", "--no-commit", "--no-ff", f.head}})
	if err := f.h.st.UpdateSlotFields(f.h.ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", f.tip) }); err != nil {
		t.Fatal(err)
	}
	f.agentsMu.Lock()
	f.agentsDown = nil
	f.agentsMu.Unlock()
	f.restart()
	f.awaitIdle()
	if res, _ := f.r.Result(f.h.ctx, id); res.State != staging.ResolveFailed || res.Why != whyCancelled {
		t.Fatalf("the job cancelled while the daemon was down = %+v, want failed, cancelled", res)
	}
	if prompts, _, _, _ := f.agent.counts(); prompts != 0 {
		t.Errorf("the agent was prompted %d times", prompts)
	}
	f.slotBack(t)
	f.letGo(t, pr)
}

// A done job whose ref cannot be dropped (its repository moved or is gone,
// or git refuses) does not stay for good: a repository that is gone lets
// the row go on to its end, and past two days the row goes whatever git
// says, the ref left as it is.
func TestADoneResolverJobWhoseRefCannotBeDroppedStillGoes(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  func(t *testing.T) string
	}{
		{"the repository is gone", func(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "moved") }},
		{"git refuses", func(t *testing.T) string { t.Helper(); return t.TempDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResolveFixture(t)
			f.reviewed()
			f.resolves("a = 2\nb = 1\n")
			id := f.start(1)
			if res := f.await(id); res.State != staging.ResolveDone {
				t.Fatalf("Result = %+v, want done", res)
			}
			dir := tc.dir(t)
			if _, err := f.r.update(f.h.ctx, f.h.e, id, func(j *resolveJob) { j.Dir = dir }); err != nil {
				t.Fatal(err)
			}
			f.effectIs(f.request(1).Effect, staging.EffectDone)
			f.h.tick()
			f.h.advance(resolveKeep)
			f.h.tick()
			f.h.tick()
			if res, err := f.r.Result(f.h.ctx, id); err != nil || res.State != staging.ResolveFailed || res.Why != whyUnknownJob {
				t.Errorf("Result two days after the end = %+v, %v; want the row gone", res, err)
			}
		})
	}
}
