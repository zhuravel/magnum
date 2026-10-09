package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// handle runs the pending requests without a tick, so a forced review stays
// in line instead of being dispatched in the same tick.
func (h *harness) handle() {
	h.t.Helper()
	h.e.handleRequests(h.ctx)
	h.settle()
}

// An abort takes back a forced review that waits in line, which before only
// an ignore could (muting the PR for good): the forced mark and what the
// request asked for this round go, the PR returns to reviewed, and nothing
// else is touched (no agent interrupted, no session parked, the slot kept).
// The next tick reviews nothing: the head was reviewed already.
func TestAbortTakesBackAQueuedForcedReview(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	rounds := len(h.rd.all())
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Fresh: true, DryRun: true})
	h.handle()
	if r := h.request(review); r.State != store.RequestDone {
		t.Fatalf("review: %s %q", r.State, deref(r.Result))
	}
	if cur := h.wantState(2, store.PRRereviewPending); !cur.Forced {
		t.Fatal("the review was not forced")
	}

	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	r := h.request(id)
	if r.State != store.RequestDone {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	for _, want := range []string{"took back the forced review of talkable/talkable#2", "PR reviewed"} {
		if !strings.Contains(deref(r.Result), want) {
			t.Errorf("result %q lacks %q", deref(r.Result), want)
		}
	}
	cur := h.wantState(2, store.PRReviewed)
	if cur.Forced || cur.Muted {
		t.Errorf("after the abort: forced=%v muted=%v", cur.Forced, cur.Muted)
	}
	for _, key := range []string{store.KVPRFresh(pr.ID), store.KVPRDryRun(pr.ID)} {
		if v, ok := h.e.getKV(h.ctx, key); ok {
			t.Errorf("%s still set: %q", key, v)
		}
	}
	if h.ag.count("park:") != 0 {
		t.Error("parked the sessions of a review that never started")
	}
	if sl := h.slot("review1"); sl.PRID == nil || *sl.PRID != pr.ID {
		t.Errorf("the PR's slot was handed back: %+v", sl)
	}
	if !h.hasEvent("pr:talkable/talkable#2", "pr.aborted") {
		t.Error("no pr.aborted event")
	}
	h.tick()
	if len(h.rd.all()) != rounds {
		t.Fatal("the review taken back still ran")
	}
}

// A round paused mid-way (its judge waits for a tool pause to end) is
// stopped like a running one: its sessions parked, the PR back to reviewed.
func TestAbortStopsAPausedRound(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	if err := h.st.TransitionPR(h.ctx, pr.ID, nil, store.PRPaused, nil); err != nil {
		t.Fatal(err)
	}
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	r := h.request(id)
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "aborted talkable/talkable#2") || !strings.Contains(deref(r.Result), "PR reviewed") {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	h.wantState(2, store.PRReviewed)
	if h.ag.count("park:") == 0 {
		t.Error("the paused round's sessions were not parked")
	}
}

// A second abort of a round's state without its round (a paused round, or
// a state a crash left) while the first one still stops it was queued for
// a round goroutine that does not exist, so it never got an answer. It is
// answered at once, and the first stop finishes.
func TestASecondAbortOfAStoppingPRIsAnswered(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	if err := h.st.TransitionPR(h.ctx, pr.ID, nil, store.PRPaused, nil); err != nil {
		t.Fatal(err)
	}
	h.ag.mu.Lock()
	h.ag.parkGate = make(chan struct{})
	h.ag.parkStarted = make(chan struct{}, 1)
	gate, started := h.ag.parkGate, h.ag.parkStarted
	h.ag.mu.Unlock()
	first := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.e.handleRequests(h.ctx)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first abort never parked the sessions")
	}
	second := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.e.handleRequests(h.ctx)
	r := h.request(second)
	if r.State != store.RequestFailed || deref(r.Result) != "talkable/talkable#2: an earlier abort or ignore is stopping it; run `magnum abort talkable/talkable#2` again shortly" {
		t.Fatalf("second abort: %s %q", r.State, deref(r.Result))
	}
	close(gate)
	h.settle()
	if r := h.request(first); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "PR reviewed") {
		t.Fatalf("first abort: %s %q", r.State, deref(r.Result))
	}
	h.wantState(2, store.PRReviewed)
}

// A never-reviewed PR whose forced review waits goes back to baseline.
func TestAbortTakesBackAQueuedReviewOfANewPR(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // #1 baseline
	h.wantState(1, store.PRBaseline)
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "1"}})
	h.handle()
	h.wantState(1, store.PRQueued)
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "1"}})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "PR baseline") {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	if cur := h.wantState(1, store.PRBaseline); cur.Forced {
		t.Error("still forced")
	}
}
