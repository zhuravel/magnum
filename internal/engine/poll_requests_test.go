package engine

import (
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// A request pressed while the daemon polls GitHub (pin, mute, review, ...)
// is answered before the poll's next GitHub call, not after the whole poll
// (10 to 12 s with several watches): the poll handles pending requests
// between its radar, CI and Details reads and before each repository and PR
// it applies. The daemon stays the only writer of the flags, and each
// request is handled once.
func TestRequestDuringThePollIsAnsweredBeforeTheNextGitHubCall(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync

	var id int64
	stateAtNextCall := ""
	h.gh.mu.Lock()
	h.gh.onRadar = func() { id = h.enqueue(ReqPin, TargetPayload{Slot: "review1"}) } // pressed during the radar call
	h.gh.onCIStates = func() { stateAtNextCall = h.request(id).State }
	h.gh.mu.Unlock()
	h.tick()

	if stateAtNextCall != store.RequestDone {
		t.Fatalf("the pin pressed during the radar call was %q at the poll's next GitHub call, want done", stateAtNextCall)
	}
	if !h.slot("review1").Pinned {
		t.Fatal("review1 not pinned")
	}
	evs, err := h.st.EventsBySubject(h.ctx, "request:"+itoa(id), 0)
	if err != nil || len(evs) != 1 || evs[0].Kind != "request.done" {
		t.Fatalf("events of request %d = %+v, %v; want one request.done (handled once)", id, evs, err)
	}
}

// A handler that reads GitHub while it answers a request mid-poll does not
// start another round of request handling, which would answer the request
// it is in again.
func TestRequestsMidPollDoNotNest(t *testing.T) {
	h := newHarness(t)
	h.startup()
	id := h.enqueue(ReqKick, nil)
	h.e.midPoll = true // inside requestsMidPoll
	h.e.requestsMidPoll(h.ctx)
	if r := h.request(id); r.State != store.RequestPending {
		t.Fatalf("a nested call answered request %d (%s)", id, r.State)
	}
	h.e.midPoll = false
	h.e.requestsMidPoll(h.ctx)
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("request %d = %s, want done", id, r.State)
	}
}
