package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// openedPR reviews PR #2, releases it and restores it with `magnum open`,
// which pins the PR and its slot review1; it returns when the open ran.
func (h *harness) openedPR() store.PR {
	h.t.Helper()
	h.parkedPR()
	h.advance(time.Minute)
	id := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone {
		h.t.Fatalf("open: %+v %q", r, deref(r.Result))
	}
	pr := h.pr(2)
	if sl := h.slot("review1"); !pr.Pinned || !sl.Pinned {
		h.t.Fatalf("open did not pin: PR %v slot %v", pr.Pinned, sl.Pinned)
	}
	return pr
}

// An explicit review of a PR `magnum open` pinned unpins it, says so, and
// runs: the pin alone no longer holds a review the operator asked for.
func TestReviewRequestUnpinsThePRMagnumOpenPinned(t *testing.T) {
	h := newHarness(t)
	h.openedPR()
	openedAt := h.clock.Now()
	h.advance(90 * time.Minute)
	before := len(h.rd.all())

	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()

	res := deref(h.request(id).Result)
	want := "unpinned review1 (pinned by magnum open at " + openedAt.Local().Format("15:04") + ") to review it"
	if !strings.Contains(res, want) {
		t.Fatalf("reply %q lacks %q", res, want)
	}
	if strings.Contains(res, "waiting") {
		t.Fatalf("reply %q says the review waits, but nothing holds it", res)
	}
	if n := len(h.rd.all()); n != before+1 {
		t.Fatalf("rounds = %d, want the requested one to run (%d)", n, before+1)
	}
	pr := h.wantState(2, store.PRReviewed)
	if sl := h.slot("review1"); pr.Pinned || sl.Pinned {
		t.Fatalf("still pinned after the review: PR %v slot %v", pr.Pinned, sl.Pinned)
	}
	if !hasEvent(h, "pr.unpinned", "magnum review") {
		t.Fatal("no pr.unpinned event for the unpin")
	}
}

// The guards still protect a person's work in the slot: the review unpins
// the PR, and the reply and the PR's wait name the guard that holds it.
func TestReviewRequestOfAPinnedPRNamesTheGuardThatKeepsItsSlot(t *testing.T) {
	t.Run("persisted hold", func(t *testing.T) {
		h := newHarness(t)
		h.openedPR()
		// The person changed files in the slot: the guard persisted the hold.
		if err := h.st.UpdateSlotFields(h.ctx, h.slot("review1").ID, func(u *store.SlotUpdate) {
			u.Set("hold_reason", slots.HoldDirtyWorktree)
		}); err != nil {
			t.Fatal(err)
		}
		before := len(h.rd.all())
		id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
		h.tick()

		res := deref(h.request(id).Result)
		for _, want := range []string{"unpinned review1 (pinned by magnum open at", "waiting: slot review1 is held: dirty_worktree"} {
			if !strings.Contains(res, want) {
				t.Fatalf("reply %q lacks %q", res, want)
			}
		}
		if n := len(h.rd.all()); n != before {
			t.Fatal("a round ran in a slot the guard holds")
		}
		sl := h.slot("review1")
		if sl.Pinned || deref(sl.HoldReason) != slots.HoldDirtyWorktree {
			t.Fatalf("slot: pinned %v hold %q, want unpinned and still held", sl.Pinned, deref(sl.HoldReason))
		}
		w := waitOf(t, h, 2)
		if w.Reason != WaitSlot || !strings.Contains(w.Detail, "dirty_worktree") {
			t.Fatalf("wait = %+v, want the slot's guard", w)
		}
	})
	t.Run("person's agent in the slot", func(t *testing.T) {
		h := newHarness(t)
		h.openedPR()
		hold := slots.ErrHold{Reason: slots.HoldForeignAgent, Detail: `pane p9: claude agent "alice" is idle in /tmp/review1`}
		h.sl.guardErr, h.sl.holdErr = hold, hold // the checkout's guard sees the same agent
		id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
		h.tick()

		res := deref(h.request(id).Result)
		for _, want := range []string{"unpinned review1", `waiting: slot review1 is held: foreign_agent: pane p9: claude agent "alice"`} {
			if !strings.Contains(res, want) {
				t.Fatalf("reply %q lacks %q", res, want)
			}
		}
		h.tick()
		if w := waitOf(t, h, 2); !strings.Contains(w.Detail, "foreign_agent") {
			t.Fatalf("wait = %+v, want the guard named", w)
		}
	})
}

// A PR pinned with `magnum pin` is unpinned the same way, and the reply says
// who pinned it.
func TestReviewRequestNamesMagnumPinAsThePinsOrigin(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	pin := h.enqueue(ReqPin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(pin); r.State != store.RequestDone {
		t.Fatalf("pin: %+v", r)
	}
	pinnedAt := h.clock.Now()
	h.advance(time.Hour)
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	want := "unpinned review1 (pinned by magnum pin at " + pinnedAt.Local().Format("15:04") + ") to review it"
	if res := deref(h.request(id).Result); !strings.Contains(res, want) {
		t.Fatalf("reply %q lacks %q", res, want)
	}
}

func hasEvent(h *harness, kind, sub string) bool {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind == kind && strings.Contains(ev.Message, sub) {
			return true
		}
	}
	return false
}
