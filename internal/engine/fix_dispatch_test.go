package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// fdSetApp serves the open PRs of the per-PR repository zhuravel/app.
func fdSetApp(h *harness, prs ...prSpec) {
	for i := range prs {
		if prs[i].updated.IsZero() {
			prs[i].updated = h.clock.Now()
		}
	}
	h.gh.set("zhuravel/app", prs...)
}

// fdAppPR is PR #n of zhuravel/app.
func fdAppPR(h *harness, n int) store.PR {
	h.t.Helper()
	repo, err := h.st.RepoByFullName(h.ctx, "zhuravel/app")
	if err != nil {
		h.t.Fatal(err)
	}
	pr, err := h.st.PRByRepoNumber(h.ctx, repo.ID, n)
	if err != nil {
		h.t.Fatalf("zhuravel/app#%d: %v", n, err)
	}
	return pr
}

// fdPush pushes head to talkable PR #n (next to the #1 baseline) and ticks
// once: the poller sees the new head and the PR waits as rereview_pending.
func fdPush(h *harness, n int, head string) {
	h.t.Helper()
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
	h.tick()
	h.wantState(n, store.PRRereviewPending)
}

// fdGate is the PR's recorded gate reason (store.KVPRGate).
func fdGate(h *harness, prID int64) string {
	v, _ := h.e.getKV(h.ctx, store.KVPRGate(prID))
	return v
}

func fdCount(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

func fdHasPrefix(calls []string, prefix string) bool {
	return slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// A per-PR worktree whose folder went missing (reconcile marks the slot
// lost) is recreated by the PR's next round, in the same slot row.
func TestLostPerPRWorktreeIsRecreated(t *testing.T) {
	h := newHarness(t)
	fdSetApp(h, prSpec{n: 1, head: "x1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	fdSetApp(h, prSpec{n: 1, head: "x1"}, prSpec{n: 2, head: "y1"})
	h.tick() // #2 queued
	h.advance(5 * time.Minute)
	h.tick()
	pr := fdAppPR(h, 2)
	if pr.State != store.PRReviewed {
		t.Fatalf("zhuravel/app#2 state = %s (last_error %q)", pr.State, deref(pr.LastError))
	}
	if n := fdCount(h.sl.all(), "worktree:zhuravel/app#2"); n != 1 {
		t.Fatalf("worktree calls after the first review: %d", n)
	}

	sl := h.slot("zhuravel/app#2")
	if sl.Kind != store.SlotKindPerPR || deref(sl.PRID) != pr.ID {
		t.Fatalf("per-PR slot: %+v", sl)
	}
	err := h.st.TransitionSlot(h.ctx, sl.ID, []string{sl.State}, store.SlotLost, func(u *store.SlotUpdate) {
		u.Set("last_error", "worktree folder missing")
	})
	if err != nil {
		t.Fatal(err)
	}

	h.advance(time.Minute)
	fdSetApp(h, prSpec{n: 1, head: "x1"}, prSpec{n: 2, head: "y2"})
	h.tick()
	if p := fdAppPR(h, 2); p.State != store.PRRereviewPending {
		t.Fatalf("after the push: %s", p.State)
	}
	h.advance(40 * time.Minute)
	h.tick()

	pr = fdAppPR(h, 2)
	if pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != "y2" {
		t.Fatalf("zhuravel/app#2 = %s at %q (last_error %q), want reviewed at y2", pr.State, deref(pr.ReviewedSHA), deref(pr.LastError))
	}
	if n := fdCount(h.sl.all(), "worktree:zhuravel/app#2"); n != 2 {
		t.Fatalf("worktree calls = %d, want 2 (the lost worktree is recreated): %v", n, h.sl.all())
	}
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].TargetSHA != "y2" {
		t.Fatalf("rounds: %+v", ins)
	}
	sl = h.slot("zhuravel/app#2")
	if sl.State != store.SlotHeld || deref(sl.PRID) != pr.ID || sl.LastError != nil {
		t.Fatalf("slot after the round: %+v", sl)
	}
}

// A broken pool slot is not the dispatcher's to fix: the PR waits and the
// reason (the slot's last error and the repair command) shows on the gate and
// on the PR.
func TestBrokenPoolSlotHoldsThePRWithAReason(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	sl := h.slot("review1")
	if sl.State != store.SlotHeld || deref(sl.PRID) != pr.ID {
		t.Fatalf("slot before: %+v", sl)
	}
	err := h.st.TransitionSlot(h.ctx, sl.ID, []string{store.SlotHeld}, store.SlotBroken, func(u *store.SlotUpdate) {
		u.Set("last_error", "schema reset failed")
	})
	if err != nil {
		t.Fatal(err)
	}

	fdPush(h, 2, "b2")
	h.advance(40 * time.Minute)
	h.tick()

	pr = h.wantState(2, store.PRRereviewPending)
	want := "slot review1 is broken: schema reset failed (magnum slots repair review1)"
	if got := fdGate(h, pr.ID); got != want {
		t.Fatalf("gate = %q, want %q", got, want)
	}
	if got := deref(pr.LastError); got != want {
		t.Fatalf("last_error = %q, want %q", got, want)
	}
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want 1", n)
	}
	if got := h.slot("review1").State; got != store.SlotBroken {
		t.Fatalf("slot state = %s, want it left broken", got)
	}
	if fdHasPrefix(h.sl.all(), "repair:") {
		t.Fatalf("the dispatcher repaired a broken slot: %v", h.sl.all())
	}
}

// A slot left busy without a round (the round's busy → held write failed) is
// put back to held by the PR's next dispatch, which then runs the round.
func TestStuckBusySlotIsRepaired(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	sl := h.slot("review1")
	if err := h.st.TransitionSlot(h.ctx, sl.ID, []string{store.SlotHeld}, store.SlotBusy, nil); err != nil {
		t.Fatal(err)
	}

	fdPush(h, 2, "b2")
	h.advance(40 * time.Minute)
	h.tick()

	pr = h.wantState(2, store.PRReviewed)
	if deref(pr.ReviewedSHA) != "b2" {
		t.Fatalf("reviewed sha %q, want b2", deref(pr.ReviewedSHA))
	}
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want 2", n)
	}
	evs, err := h.st.EventsBySubject(h.ctx, "slot:review1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(evs, func(ev store.Event) bool { return ev.Kind == "slot.repaired" }) {
		t.Fatalf("no slot.repaired event for slot:review1: %+v", evs)
	}
	sl = h.slot("review1")
	if sl.State != store.SlotHeld || deref(sl.PRID) != pr.ID {
		t.Fatalf("slot after the round: %+v", sl)
	}
}

// A pool slot whose release stalled half way (an eviction that failed after
// the slot moved to releasing) is released for good by the heavy worker the
// next time its PR wants it; the PR then claims it again.
func TestStalledReleaseIsResumed(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	sl := h.slot("review1")
	if err := h.st.TransitionSlot(h.ctx, sl.ID, []string{store.SlotHeld}, store.SlotReleasing, nil); err != nil {
		t.Fatal(err)
	}

	fdPush(h, 2, "b2")
	h.advance(40 * time.Minute)
	h.tick()

	pr = h.wantState(2, store.PRRereviewPending)
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want 1 (no round while the slot is being released)", n)
	}
	if got, want := fdGate(h, pr.ID), "slot review1 is being released"; got != want {
		t.Fatalf("gate = %q, want %q", got, want)
	}
	if !slices.Contains(h.sl.all(), "release:review1:evicted") {
		t.Fatalf("the release was not resumed: %v", h.sl.all())
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("slot after the resumed release: %+v", sl)
	}

	h.tick() // the PR claims the freed slot again
	pr = h.wantState(2, store.PRReviewed)
	if deref(pr.ReviewedSHA) != "b2" {
		t.Fatalf("reviewed sha %q, want b2", deref(pr.ReviewedSHA))
	}
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want 2", n)
	}
	if sl := h.slot("review1"); sl.State != store.SlotHeld || deref(sl.PRID) != pr.ID {
		t.Fatalf("slot after the second round: %+v", sl)
	}
}

// A pool slot whose provisioning just failed is not retried for
// provisionRetry: a waiting PR gets a slot by eviction meanwhile. Once the
// backoff is over the next waiting PR retries the provisioning.
func TestFailedProvisioningBacksOffAndEvicts(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Pools[0].Max = 2 })
	first := h.reviewedPR(2, "b1")
	h.advance(31 * time.Minute) // past min_warm
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick() // #3 queued
	h.wantState(3, store.PRQueued)

	// review2 failed to provision just now.
	pool := h.cfg.Pools[0]
	review2, err := h.st.CreateSlot(h.ctx, store.Slot{Name: pool.Slot(2), RepoFullName: pool.Repo, Kind: store.SlotKindPool,
		Path: pool.Path(2), MainClone: pool.MainClone, PlaceholderBranch: new("review2"), DBSlug: new("review2"),
		State: store.SlotProvisioning, LastError: new("setup failed")})
	if err != nil {
		t.Fatal(err)
	}

	h.advance(5 * time.Minute)
	h.tick() // #3 finds no free slot: the failed one is in backoff, so review1 is evicted
	calls := h.sl.all()
	if fdHasPrefix(calls, "provision:") {
		t.Fatalf("provisioning retried inside the backoff: %v", calls)
	}
	if !slices.Contains(calls, "release:review1:evicted") || !slices.Contains(h.ag.all(), "park:"+itoa(first.ID)) {
		t.Fatalf("review1 not evicted: slots %v agents %v", calls, h.ag.all())
	}
	if got := h.slot("review2"); got.State != store.SlotProvisioning || deref(got.LastError) != "setup failed" || got.ID != review2.ID {
		t.Fatalf("review2 touched: %+v", got)
	}

	h.tick() // the freed review1 goes to #3
	h.wantState(3, store.PRReviewed)
	if sl := h.slot("review1"); deref(sl.PRID) != h.pr(3).ID {
		t.Fatalf("review1 holder: %+v", sl)
	}
	if fdHasPrefix(h.sl.all(), "provision:") {
		t.Fatalf("provisioning retried inside the backoff: %v", h.sl.all())
	}

	// Past the backoff a waiting PR retries the failed slot. (The reconcile
	// also provisions a slot being provisioned, so it is kept out of the way.)
	h.advance(11 * time.Minute)
	h.e.lastReconcile = h.clock.Now()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"}, prSpec{n: 4, head: "d1"})
	h.tick() // #4 queued
	h.wantState(4, store.PRQueued)
	h.advance(5 * time.Minute)
	h.tick()
	if n := fdCount(h.sl.all(), "provision:2"); n != 1 {
		t.Fatalf("provision:2 calls = %d, want 1 (the failed slot is retried): %v", n, h.sl.all())
	}
	h.wantState(4, store.PRQueued)
}

// An agent kind's pause holds only the rounds whose roles run that kind.
func TestKindPauseHoldsOnlyRoundsUsingThatKind(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		addRole(h, config.Role{Name: "omp-review", Kind: config.KindOMP})
		h.cfg.Watches[1].Roles = []string{"judge", "claude-review"} // no omp-review
	})
	h.open(prSpec{n: 1, head: "base1"})
	fdSetApp(h, prSpec{n: 1, head: "x1"})
	h.startup()
	h.tick() // first sync: both #1 are baselines

	h.e.pauseTool(h.ctx, pipeline.Pause{Kind: string(agents.HealthUsageLimit), Tool: config.KindOMP, Until: h.clock.Now().Add(2 * time.Hour)})

	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	fdSetApp(h, prSpec{n: 1, head: "x1"}, prSpec{n: 2, head: "y1"})
	h.tick() // both #2 queued
	h.advance(5 * time.Minute)
	h.tick()

	if p := fdAppPR(h, 2); p.State != store.PRReviewed {
		t.Fatalf("zhuravel/app#2 (no omp role) = %s (last_error %q), want reviewed", p.State, deref(p.LastError))
	}
	pr := h.wantState(2, store.PRQueued)
	if got := fdGate(h, pr.ID); !strings.HasPrefix(got, "omp paused (usage_limit) until") {
		t.Fatalf("talkable#2 gate = %q", got)
	}
	if fdHasPrefix(h.sl.all(), "claim:") {
		t.Fatalf("a slot was claimed for the paused round: %v", h.sl.all())
	}
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want 1", n)
	}

	if res, err := h.e.requestPause(h.ctx, PausePayload{Tool: config.KindOMP}, false); err != nil || res != "resumed omp" {
		t.Fatalf("resume omp: %q %v", res, err)
	}
	h.tick()
	h.wantState(2, store.PRReviewed)
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want 2", n)
	}
	if v, ok := h.e.getKV(h.ctx, store.KVPRGate(pr.ID)); ok {
		t.Fatalf("gate kept after the round started: %q", v)
	}
}

// max_total_working_codex counts the Codex agents the round's own roles add:
// a round whose roles run no Codex agent is not held by the limit.
func TestWorkingCodexLimitCountsTheRoundsCodexRoles(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		addRole(h, config.Role{Name: "claude-judge", Kind: config.KindClaude, Judge: true})
		// talkable: codex-judge and codex-review (both Codex); zhuravel: no Codex role.
		h.cfg.Watches[0].Roles = []string{"codex-judge", "claude-review", "codex-review"}
		h.cfg.Watches[1].Roles = []string{"claude-judge", "claude-review"}
		if err := h.cfg.Validate(); err != nil {
			t.Fatalf("test config: %v", err)
		}
	})
	if got := codexRoles(h.cfg.RolesFor(&h.cfg.Watches[0])); got != 2 {
		t.Fatalf("talkable codex roles = %d, want 2", got)
	}
	if got := codexRoles(h.cfg.RolesFor(&h.cfg.Watches[1])); got != 0 {
		t.Fatalf("zhuravel codex roles = %d, want 0", got)
	}

	h.hd.mu.Lock()
	for i := range 5 {
		h.hd.agents = append(h.hd.agents, agentInfo(i))
	}
	h.hd.mu.Unlock()
	h.open(prSpec{n: 1, head: "base1"})
	fdSetApp(h, prSpec{n: 1, head: "x1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	fdSetApp(h, prSpec{n: 1, head: "x1"}, prSpec{n: 2, head: "y1"})
	h.tick() // both #2 queued
	h.advance(5 * time.Minute)
	h.tick()

	pr := h.wantState(2, store.PRQueued)
	if got := fdGate(h, pr.ID); !strings.HasPrefix(got, "working Codex agents at the limit") {
		t.Fatalf("talkable#2 gate = %q", got)
	}
	app := fdAppPR(h, 2)
	if app.State != store.PRReviewed {
		t.Fatalf("zhuravel/app#2 (no Codex role) = %s (last_error %q), want reviewed", app.State, deref(app.LastError))
	}
	ins := h.rd.all()
	if len(ins) != 1 || ins[0].PR.ID != app.ID {
		t.Fatalf("rounds: %d, want only zhuravel/app#2's", len(ins))
	}
	var names []string
	for _, r := range ins[0].Roles {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"claude-review", "claude-judge"}) && !slices.Equal(names, []string{"claude-judge", "claude-review"}) {
		t.Fatalf("zhuravel round roles = %v", names)
	}

	h.hd.mu.Lock()
	h.hd.agents = nil
	h.hd.mu.Unlock()
	h.tick()
	h.wantState(2, store.PRReviewed)
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want 2", n)
	}
	if v, ok := h.e.getKV(h.ctx, store.KVPRGate(pr.ID)); ok {
		t.Fatalf("gate kept after the round started: %q", v)
	}
}

// A closed PR whose round is still in its checkout keeps its slot: the
// cleanup waits for the round to end, then releases it.
func TestCloseGraceWaitsForARoundInItsCheckout(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick() // #2 queued
	h.wantState(2, store.PRQueued)

	h.sl.mu.Lock()
	h.sl.checkoutGate = make(chan struct{})
	h.sl.checkoutStarted = make(chan struct{}, 1)
	started := h.sl.checkoutStarted
	gate := h.sl.checkoutGate
	h.sl.mu.Unlock()
	h.advance(5 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil { // not h.tick: it would wait for the round
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the round never reached its checkout")
	}
	pr := h.pr(2)

	// #2 leaves the radar and GitHub confirms it merged, twice 60s apart.
	h.open(prSpec{n: 1, head: "base1"})
	h.gh.mu.Lock()
	h.gh.closed[2] = "MERGED"
	h.gh.mu.Unlock()
	for range 3 {
		h.advance(30 * time.Second)
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.e.drainHeavy(h.ctx)
	}
	h.wantState(2, store.PRClosed)

	h.advance(11 * time.Minute) // past close_grace
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.e.drainHeavy(h.ctx)
	if fdHasPrefix(h.sl.all(), "release:") {
		t.Fatalf("slot released under a round in its checkout: %v", h.sl.all())
	}
	if sl := h.slot("review1"); sl.State == store.SlotFree || deref(sl.PRID) != pr.ID {
		t.Fatalf("slot while the round is in its checkout: %+v", sl)
	}
	h.wantState(2, store.PRClosed)

	close(gate)
	h.settle() // the round's setup fails: the PR is closed
	h.wantState(2, store.PRClosed)
	if n := len(h.rd.all()); n != 0 {
		t.Fatalf("a round ran for a closed PR: %d", n)
	}

	h.tick()
	if !slices.Contains(h.sl.all(), "release:review1:merged") {
		t.Fatalf("slot not released after the round ended: %v", h.sl.all())
	}
	h.wantState(2, store.PRReleased)
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("slot after the release: %+v", sl)
	}
}

// The heavy worker evicts a slot concurrently with the tick: while the
// eviction parks the holder's sessions, the holder's own re-review must not
// start in that slot, and the slot goes to the next candidate afterwards.
func TestEvictionAndDispatchNeverShareASlot(t *testing.T) {
	h := newHarness(t)
	second := h.reviewedPR(2, "b1")
	h.advance(31 * time.Minute) // past min_warm
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick() // #3 queued
	h.wantState(3, store.PRQueued)
	h.advance(5 * time.Minute)

	h.ag.mu.Lock()
	h.ag.parkGate = make(chan struct{})
	h.ag.parkStarted = make(chan struct{}, 1)
	gate, started := h.ag.parkGate, h.ag.parkStarted
	h.ag.mu.Unlock()

	wctx, wcancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.e.heavyWorker(wctx)
	}()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			wcancel()
			<-done
		}
	}
	defer stop()

	// #3 finds no free slot: the eviction of review1 is queued, the worker
	// runs it and blocks in Park.
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the eviction never parked the holder's sessions")
	}

	// #2 gets a re-review candidate whose own held slot is review1, the one
	// being evicted.
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"}, prSpec{n: 3, head: "c1"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(2, store.PRRereviewPending)
	h.advance(40 * time.Minute)
	h.e.lastReconcile = h.clock.Now() // keep the reconcile off the busy worker
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.e.roundWG.Wait()

	h.wantState(2, store.PRRereviewPending)
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want 1: a round started in a slot being evicted", n)
	}
	if want := "checkout:review1:" + itoa(second.ID) + ":b2"; slices.Contains(h.sl.all(), want) {
		t.Fatalf("%s: the holder's round started in its slot during the eviction: %v", want, h.sl.all())
	}
	if got, want := fdGate(h, second.ID), "its slot is being evicted"; got != want {
		t.Fatalf("gate = %q, want %q", got, want)
	}

	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for h.slot("review1").State != store.SlotFree {
		if time.Now().After(deadline) {
			t.Fatalf("review1 not released: %+v", h.slot("review1"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.Contains(h.sl.all(), "release:review1:evicted") || !slices.Contains(h.ag.all(), "park:"+itoa(second.ID)) {
		t.Fatalf("eviction: agents %v slots %v", h.ag.all(), h.sl.all())
	}
	stop()

	h.tick() // the next candidate claims the freed slot
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want 2", n)
	}
	sl := h.slot("review1")
	if sl.State != store.SlotHeld || sl.PRID == nil {
		t.Fatalf("review1 after the round: %+v", sl)
	}
	holder, err := h.st.PRByID(h.ctx, *sl.PRID)
	if err != nil {
		t.Fatal(err)
	}
	if holder.State != store.PRReviewed || (holder.Number != 2 && holder.Number != 3) {
		t.Fatalf("holder of review1: #%d %s (last_error %q)", holder.Number, holder.State, deref(holder.LastError))
	}
}
