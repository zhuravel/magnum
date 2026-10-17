package engine

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// heldReviewedPR reviews PR #2 and leaves its slot review1 held by the slot
// guard for hold: the registry has the checkout at oldHead, while the folder's
// HEAD is newHead (what Unpin takes as the new checkout).
func (h *harness) heldReviewedPR(hold, oldHead, newHead string) store.Slot {
	h.t.Helper()
	pr := h.reviewedPR(2, "b1")
	sl := h.slot("review1")
	if err := h.st.UpdateSlotFields(h.ctx, sl.ID, func(u *store.SlotUpdate) {
		u.Set("pr_id", pr.ID)
		u.Set("hold_reason", hold)
		u.Set("checked_out_sha", oldHead)
	}); err != nil {
		h.t.Fatal(err)
	}
	h.sl.mu.Lock()
	h.sl.heads = map[string]string{"review1": newHead}
	h.sl.mu.Unlock()
	return h.slot("review1")
}

// `magnum unpin <PR>` on a PR that is not pinned, whose slot a guard holds
// for a moved HEAD or for commits on no remote, acknowledges the hold on the
// slot, as `magnum unpin <slot>` does, and says so: which hold, which slot,
// and the HEAD magnum now expects. Before, the answer said "unpinned" as if a
// pin had gone, and the operator could not tell that the hold was what ended.
// An unpinned PR whose slot holds nothing acknowledgeable, and a pinned one,
// keep the answer they had.
func TestUnpinByPRReferenceAcknowledgesTheSlotsHold(t *testing.T) {
	for _, hold := range []string{slots.HoldHeadDrift, slots.HoldUnpushed} {
		t.Run(hold, func(t *testing.T) {
			h := newHarness(t)
			h.heldReviewedPR(hold, "0b8fc1b9d2e4", "3e8c5b6a71f0c4d9e2b8a53f1c7d6e0b4a9f2c81")
			id := h.enqueue(ReqUnpin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
			h.tick()

			want := "acknowledged the " + hold + " hold of slot review1: HEAD 3e8c5b6 is now the checkout magnum expects; the next release resets it"
			if r := h.request(id); r.State != store.RequestDone || deref(r.Result) != want {
				t.Fatalf("answer = %s %q, want %q", r.State, deref(r.Result), want)
			}
			sl := h.slot("review1")
			if sl.HoldReason != nil || deref(sl.CheckedOutSHA) != "3e8c5b6a71f0c4d9e2b8a53f1c7d6e0b4a9f2c81" {
				t.Fatalf("slot: hold %q, checked out %q, want the hold gone and HEAD taken", deref(sl.HoldReason), deref(sl.CheckedOutSHA))
			}
			if !hasEvent(h, "pr.unpinned", "acknowledged the "+hold+" hold of slot review1") {
				t.Error("no pr.unpinned event saying the hold was acknowledged")
			}
		})
	}

	t.Run("no hold", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		id := h.enqueue(ReqUnpin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
		h.tick()
		if res := deref(h.request(id).Result); strings.Contains(res, "acknowledged") || !strings.HasPrefix(res, "unpinned talkable/talkable#2") {
			t.Fatalf("answer = %q, want today's unpinned answer with nothing acknowledged", res)
		}
	})
	t.Run("a hold that is not acknowledged by taking HEAD", func(t *testing.T) {
		h := newHarness(t)
		h.heldReviewedPR(slots.HoldDirtyWorktree, "0b8fc1b9d2e4", "3e8c5b6a71f0")
		id := h.enqueue(ReqUnpin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
		h.tick()
		if res := deref(h.request(id).Result); strings.Contains(res, "acknowledged") {
			t.Fatalf("answer = %q, a dirty worktree is not acknowledged by a HEAD", res)
		}
	})
	t.Run("pinned", func(t *testing.T) {
		h := newHarness(t)
		h.heldReviewedPR(slots.HoldHeadDrift, "0b8fc1b9d2e4", "3e8c5b6a71f0")
		pin := h.enqueue(ReqPin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
		h.tick()
		if r := h.request(pin); r.State != store.RequestDone {
			t.Fatalf("pin: %s %q", r.State, deref(r.Result))
		}
		id := h.enqueue(ReqUnpin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
		h.tick()
		res := deref(h.request(id).Result)
		if strings.Contains(res, "acknowledged") || !strings.HasPrefix(res, "unpinned talkable/talkable#2 (slot review1)") {
			t.Fatalf("answer = %q, want the pinned path's unpinned answer", res)
		}
		if sl := h.slot("review1"); sl.HoldReason != nil || sl.Pinned {
			t.Fatalf("slot: hold %q pinned %v, want both gone as before", deref(sl.HoldReason), sl.Pinned)
		}
	})
}

// Every sentence that says a guard holds a slot names the command that hands
// it back, with the slot in quotes: `magnum unpin` alone sent the operator of
// talkable-integration#867 to the board's u, which refuses a PR that is not
// pinned, though `magnum unpin "<slot>"` was what acknowledged the moved HEAD.
// The review request's waiting line and the PR's wait (the board's detail and
// the status card's sentence) say it, and so does the refusal of a fix.
func TestAHoldHintNamesTheUnpinCommandWithTheSlot(t *testing.T) {
	const hint = ` (magnum unpin "review1")`
	t.Run("a review request", func(t *testing.T) {
		h := newHarness(t)
		h.heldReviewedPR(slots.HoldHeadDrift, "0b8fc1b9d2e4", "3e8c5b6a71f0")
		id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
		h.tick()
		want := "waiting: slot review1 is held: head_drift" + hint
		if res := deref(h.request(id).Result); !strings.Contains(res, want) {
			t.Fatalf("answer = %q, want it to contain %q", res, want)
		}
		h.tick()
		w := waitOf(t, h, 2)
		if w.Reason != WaitSlot || w.Detail != "slot review1 is held: head_drift"+hint {
			t.Fatalf("wait = %+v, want the slot's hold with the command", w)
		}
		if s := w.Sentence("talkable/talkable#2", h.clock.Now()); !strings.HasSuffix(s, "slot review1 is held: head_drift"+hint) {
			t.Fatalf("sentence = %q, want it to end with the hold and the command", s)
		}
	})
	t.Run("a fix", func(t *testing.T) {
		h, _ := fixHarness(t)
		pinnedFixablePR(h)
		if err := h.st.UpdateSlotFields(h.ctx, h.slot("review1").ID, func(u *store.SlotUpdate) {
			u.Set("hold_reason", slots.HoldUnpushed)
		}); err != nil {
			t.Fatal(err)
		}
		id := h.fix(FixPayload{})
		h.handle()
		want := "talkable/talkable#2: slot review1 is held: unpushed_commits" + hint
		if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
			t.Fatalf("%s %q, want failed %q", r.State, deref(r.Result), want)
		}
	})
}
