package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// A resolve that a live staging deploy needs goes on under the drain: the
// drain waits for that deploy, so holding the resolve would only fail the
// deploy past the resolve's bound (a job the start's recovery queued again
// included). A resolve with no live deploy still waits.
func TestTheRebootDrainLetsTheResolveOfALiveDeployGoOn(t *testing.T) {
	f := &fakeStaging{effects: map[string][]staging.Effect{"talkable/talkable": {{ID: "dep-8", Repo: "talkable/talkable", Env: envVoid,
		PR: staging.PR{Repo: "talkable/talkable", Number: 2}, State: staging.EffectRunning}}}}
	h := deployHarness(t, f, func(h *harness) { h.d.stagingResolver = &stagingResolver{} })
	h.rebootDrain(33, DrainUntilBoot)
	job := resolveJob{ID: "dep-8.r1.0a0b0c0f", Effect: "dep-8", Repo: "talkable/talkable", Number: 2, Target: "staging-void",
		State: resolveQueued, Started: h.clock.Now()}
	if err := h.e.saveResolveJob(h.ctx, job); err != nil {
		t.Fatal(err)
	}
	w := &resolveWork{e: h.e, r: h.d.stagingResolver, job: &job}
	if err := w.take(h.ctx); errors.Is(err, errResolveWaits) && strings.Contains(job.Why, "drains for a reboot") {
		t.Fatalf("take = %v (%q): the resolve of a deploy the drain waits for waits for the drain", err, job.Why)
	}
}

// Under the drain a live deploy's resolve goes on, and nothing else of
// staging starts: a new deploy you ask for is refused before it reaches
// staging, and a resolve whose deploy is no longer live waits.
func TestTheRebootDrainStartsNoNewDeployOnlyTheResolveOfALiveOne(t *testing.T) {
	f := &fakeStaging{effects: map[string][]staging.Effect{"talkable/talkable": {
		{ID: "dep-9", Repo: "talkable/talkable", Env: envVoid, PR: staging.PR{Repo: "talkable/talkable", Number: 2}, State: staging.EffectRunning},
		{ID: "dep-10", Repo: "talkable/talkable", Env: envVoid, PR: staging.PR{Repo: "talkable/talkable", Number: 2}, State: staging.EffectDone},
	}}}
	h := deployHarness(t, f, func(h *harness) { h.d.stagingResolver = &stagingResolver{} })
	h.rebootDrain(34, DrainUntilBoot)

	r := h.stagingRequest(ReqStagingDeploy, StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"all"}})
	if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "drains for a reboot") {
		t.Fatalf("a new deploy under the drain = %s %q, want refused for the drain", r.State, deref(r.Result))
	}
	f.mu.Lock()
	plans, deploys := len(f.plans), len(f.deploys)
	f.mu.Unlock()
	if plans != 0 || deploys != 0 {
		t.Fatalf("staging got %d plans and %d deploys under the drain, want none", plans, deploys)
	}

	for _, tc := range []struct {
		effect string
		waits  bool
	}{{"dep-9", false}, {"dep-10", true}} {
		job := resolveJob{ID: tc.effect + ".r1.0a0b0c10", Effect: tc.effect, Repo: "talkable/talkable", Number: 2, Target: "staging-void",
			State: resolveQueued, Started: h.clock.Now()}
		if err := h.e.saveResolveJob(h.ctx, job); err != nil {
			t.Fatal(err)
		}
		w := &resolveWork{e: h.e, r: h.d.stagingResolver, job: &job}
		err := w.take(h.ctx)
		if waits := errors.Is(err, errResolveWaits) && strings.Contains(job.Why, "drains for a reboot"); waits != tc.waits {
			t.Errorf("the resolve of %s: take = %v (%q), want it to wait for the drain: %v", tc.effect, err, job.Why, tc.waits)
		}
	}
}

// A stop or a yield that reached a whiteboard job between its reservation
// and its drain's re-check is answered when the drain turns the job away:
// the abort hears that nothing started, the yielded request goes back to the
// dispatch, and nothing stays in flight, which would keep the drain from
// ever being quiet.
func TestAWhiteboardTurnedAwayByTheDrainAnswersTheStopsThatReachedIt(t *testing.T) {
	w := wbHarness(t, false)
	wbWriter(w.harness, wbDone(wbSession))
	pr := w.wbReviewed()
	w.run.Rules = append([]execx.Rule{{Prefix: []string{"whiteboard", "api", "whiteboard_status"}, Fn: func(execx.Cmd) (execx.Result, error) {
		return execx.Result{Stdout: []byte(`{"ready":true}`)}, nil
	}}}, w.run.Rules...)
	repo, err := w.st.RepoByID(w.ctx, pr.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	var abort, review int64
	w.e.d.whiteboardReserved = func(prID int64) { // the tick's handlers on the new reservation, then the drain
		abort = w.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
		review = w.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
		if !w.e.stopHeld(w.ctx, abort, repo, pr, false, nil) || !w.e.yieldWhiteboard(prID, review, "magnum review") {
			t.Error("the reservation took no stop or yield")
		}
		if err := w.st.SetKV(w.ctx, KVDaemonRebootDrain, RebootDrain{Generation: 61, Since: w.clock.Now(), Until: DrainUntilBoot, Boot: bootOne}.Value()); err != nil {
			t.Error(err)
		}
	}

	id := w.wbRequest()
	w.wbAnswered(id)
	w.settle()

	w.wbFailed(id, "drains for a reboot")
	if r := w.request(abort); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "nothing to abort") {
		t.Errorf("the abort = %s %q, want answered: nothing started", r.State, deref(r.Result))
	}
	w.e.mu.Lock()
	inflight := len(w.e.inflight)
	w.e.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("%d request(s) stay in flight after the job was turned away", inflight)
	}
	if r := w.request(review); r.State != store.RequestPending {
		t.Errorf("the yielded review = %s, want it back at the dispatch, pending", r.State)
	}
}

// A read of the drain's record that fails never opens the gates nor lifts
// the drain: the daemon counts it as draining, and its tick keeps the
// acknowledgement, its state and the marks of its park for the next tick.
func TestAFailedReadOfTheRebootDrainKeepsItHeld(t *testing.T) {
	h := newHarness(t)
	d := h.rebootDrain(30, DrainUntilBoot)
	h.tick()
	before, ok := h.drainAck()
	if !ok || before.Generation != d.Generation {
		t.Fatalf("acknowledgement %+v, %v before the failed read", before, ok)
	}
	h.renamedAway("kv", func() {
		if !h.e.rebootDraining(h.ctx) {
			t.Error("a read of the drain that failed counts as no drain: the gates open")
		}
		h.e.tendRebootDrain(h.ctx)
	})
	if after, ok := h.drainAck(); !ok || after.Generation != before.Generation {
		t.Fatalf("acknowledgement %+v, %v after a failed read, want it kept", after, ok)
	}
	if evs := h.events("daemon.reboot_drain_lifted"); len(evs) != 0 {
		t.Fatalf("lifted events %+v after a failed read, want none", evs)
	}
	if h.e.reboot.gen != d.Generation {
		t.Fatalf("the drain's state = generation %d after a failed read, want %d kept", h.e.reboot.gen, d.Generation)
	}
}

// `magnum restart-sessions` asked during the drain for a session its park
// had marked already keeps that restart past `magnum resume`: the lift
// drops only the marks the drain's park alone made.
func TestARestartAskedDuringTheDrainOutlivesItsLift(t *testing.T) {
	h, _ := newLoginHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	live, err := h.st.LiveSessions(h.ctx)
	if err != nil || len(live) == 0 {
		t.Fatalf("live sessions %v, %v: want the reviewed PR's", live, err)
	}
	h.e.markRestart(h.ctx, live, drainParkWhy)
	h.e.markRestart(h.ctx, live[:1], restartRequested)
	h.e.unmarkDrainParks(h.ctx)
	marks := h.e.restartMarks(h.ctx)
	if m, ok := marks[live[0].ID]; !ok || m.Why != restartRequested {
		t.Fatalf("the mark of the session you asked to restart = %+v, %v after the lift, want %q", m, ok, restartRequested)
	}
	for _, s := range live[1:] {
		if m, ok := marks[s.ID]; ok {
			t.Errorf("the drain's own mark of session %d stays after the lift: %+v", s.ID, m)
		}
	}
}

// A drain for a reboot holds the tick's own slot work as it holds the
// reconcile's: a closed PR past its grace is not released, nor a
// Codex-flagged PR's slot, until the drain is lifted.
func TestARebootDrainHoldsTheCloseGraceAndTheFlaggedRelease(t *testing.T) {
	storetest.Parallel(t)
	t.Run("close grace", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		h.advance(time.Minute)
		h.open(prSpec{n: 1, head: "base1"})
		h.gh.closed[2] = "MERGED"
		for range 3 {
			h.tick()
			h.advance(30 * time.Second)
		}
		h.wantState(2, store.PRClosed)
		h.rebootDrain(31, DrainUntilBoot)
		h.advance(11 * time.Minute)
		h.tick()
		h.wantState(2, store.PRClosed)
		if h.slot("review1").State != store.SlotHeld || slices.Contains(h.sl.all(), "release:review1:merged") {
			t.Fatalf("slot %s, calls %v under the drain: want it held", h.slot("review1").State, h.sl.all())
		}
		if err := h.st.DeleteKV(h.ctx, KVDaemonRebootDrain); err != nil { // magnum resume
			t.Fatal(err)
		}
		h.tick()
		h.wantState(2, store.PRReleased)
	})
	t.Run("flagged release", func(t *testing.T) {
		h := newHarness(t, noFallback)
		h.refusedPR()
		h.rebootDrain(32, DrainUntilBoot)
		h.advance(time.Minute)
		h.tick()
		if h.slot("review1").State != store.SlotHeld || slices.Contains(h.sl.all(), "release:review1:codex-flagged") {
			t.Fatalf("slot %s, calls %v under the drain: want the flagged PR's slot held", h.slot("review1").State, h.sl.all())
		}
		if err := h.st.DeleteKV(h.ctx, KVDaemonRebootDrain); err != nil {
			t.Fatal(err)
		}
		h.advance(time.Minute)
		h.tick()
		if h.slot("review1").State != store.SlotFree {
			t.Fatalf("slot %s after the lift, want the flagged PR's slot released", h.slot("review1").State)
		}
	})
}
