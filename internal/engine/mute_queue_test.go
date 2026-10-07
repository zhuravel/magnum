package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// muteEvents are the pr.muted events of PR #n, oldest first.
func muteEvents(h *harness, n int) []store.Event {
	h.t.Helper()
	evs, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, evPRMuted)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range evs {
		if strings.HasSuffix(deref(ev.Subject), "#"+itoa(int64(n))) {
			out = append(out, ev)
		}
	}
	return out
}

// A mute keeps the reason it was given, in the request's answer and in its
// pr.muted event, and takes a PR waiting for a round out of the queue at
// once (ineligible, "muted"), instead of leaving it queued until a restart;
// an unmute puts it back in line.
func TestMuteWithAReasonTakesAWaitingPROutOfTheQueue(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, "b1")
	id := h.enqueue(ReqMute, TargetPayload{PRTarget: PRTarget{Ref: "2"}, Reason: "  waits for\nthe author's rework "})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestDone || deref(r.Result) != "muted talkable/talkable#2 (waits for the author's rework)" {
		t.Fatalf("mute: %s %q", r.State, deref(r.Result))
	}
	pr := h.wantState(2, store.PRIneligible)
	if !pr.Muted || deref(pr.SkipReason) != "muted" {
		t.Fatalf("muted PR: muted=%v skip_reason=%q", pr.Muted, deref(pr.SkipReason))
	}
	evs := muteEvents(h, 2)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "waits for the author's rework") {
		t.Fatalf("pr.muted events = %+v", evs)
	}
	var data struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || data.Reason != "waits for the author's rework" {
		t.Fatalf("pr.muted data = %s (%v)", evs[0].Data, err)
	}
	h.advance(10 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 0 {
		t.Fatalf("a muted PR got %d rounds", n)
	}
	h.muteRequest(ReqUnmute, 2) // past its quiet period: the unmute's tick reviews it
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds after the unmute = %d, want 1", n)
	}
	h.wantState(2, store.PRReviewed)
}

// A mute takes a PR waiting for its re-review out of the queue too; one
// without a reason says none.
func TestMuteTakesARereviewPendingPROutOfTheQueue(t *testing.T) {
	h := newHarness(t)
	pushedAfter(t, h, time.Minute) // waits for the re-review interval
	if got := h.muteRequest(ReqMute, 2); got != "muted talkable/talkable#2" {
		t.Fatalf("mute answer %q", got)
	}
	h.wantState(2, store.PRIneligible)
	if evs := muteEvents(h, 2); len(evs) != 1 || evs[0].Message != "muted" {
		t.Fatalf("pr.muted events = %+v", evs)
	}
}

// A round the operator asked for passes a mute: a forced PR stays in line.
func TestMuteLeavesAForcedPRInLine(t *testing.T) {
	h := newHarness(t)
	pr := h.queuedPR(2, "b1")
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("forced", true) }); err != nil {
		t.Fatal(err)
	}
	h.muteRequest(ReqMute, 2)
	if got := h.pr(2); got.State == store.PRIneligible || !got.Muted {
		t.Fatalf("forced PR after the mute: state %s muted %v", got.State, got.Muted)
	}
}
