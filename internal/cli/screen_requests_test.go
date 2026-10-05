package cli

import (
	"context"
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// TestScreenActionsReportTheRequestsTheyQueue: a screen action returns the
// request it queued with its id and state, so the screen can follow a request
// the daemon has not answered when the action returns; Requests re-reads them
// (leaving out one the registry no longer has) without waiting for an action
// in flight.
func TestScreenActionsReportTheRequestsTheyQueue(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.held = true // a daemon holds the lock but answers nothing in the test
	acts := newScreenActions(h.c)
	defer acts.Close()
	ctx := context.Background()

	// Before any action opened the registry, a re-read fails rather than call
	// the requests gone (the screen asks again at the next refresh).
	if got, err := acts.Requests(ctx, []int64{1}); err == nil || got != nil {
		t.Fatalf("before any action: %v %v", got, err)
	}
	res, err := acts.Pin(ctx, "talkable#5")
	if err != nil {
		t.Fatal(err)
	}
	want := []tui.Request{{ID: 1, Kind: "pin", State: tui.RequestPending}}
	if !slices.Equal(res.Requests, want) {
		t.Fatalf("pin reported %+v, want %+v", res.Requests, want)
	}
	if res, err := acts.Review(ctx, "talkable#5", tui.ReviewOpts{}); err == nil || len(res.Requests) != 0 {
		t.Fatalf("a review no daemon answers queues nothing: %+v %v", res.Requests, err)
	}

	if err := h.st.CompleteRequest(ctx, 1, store.RequestDone, "pinned review1\n"); err != nil {
		t.Fatal(err)
	}
	got, err := acts.Requests(ctx, []int64{1, 99})
	want = []tui.Request{{ID: 1, Kind: "pin", State: tui.RequestDone, Result: "pinned review1"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("re-read %+v %v, want %+v", got, err, want)
	}
}

// TestScreenActionsReportARefusedRequest: a request the daemon refuses
// within the wait comes back failed, with the daemon's words, next to the
// action's error.
func TestScreenActionsReportARefusedRequest(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		if q, err := nextPendingRequest(h.ctx, h.st); err == nil {
			_ = h.st.CompleteRequest(h.ctx, q.ID, store.RequestFailed, "talkable#5 is not open (released, GitHub MERGED)")
		}
	}
	acts := newScreenActions(h.c)
	defer acts.Close()

	res, err := acts.Mute(context.Background(), "talkable#5")
	if err == nil {
		t.Fatalf("a refused mute succeeded: %q", res.Text)
	}
	if len(res.Requests) != 1 || res.Requests[0].State != tui.RequestFailed || res.Requests[0].Result != "talkable#5 is not open (released, GitHub MERGED)" {
		t.Fatalf("requests %+v", res.Requests)
	}
}

// TestScreenReleaseIsFollowed: a release from a screen does not wait (the
// daemon runs it on its heavy worker), so it comes back pending for the
// screen to follow, instead of a "queued" that passed for a success.
func TestScreenReleaseIsFollowed(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.held = true
	acts := newScreenActions(h.c)
	defer acts.Close()

	res, err := acts.Release(context.Background(), "talkable#5")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 1 || res.Requests[0].Kind != "release" || res.Requests[0].State != tui.RequestPending {
		t.Fatalf("requests %+v", res.Requests)
	}
}
