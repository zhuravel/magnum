package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// pinnedToasts lists the "title | body" toasts about a closed PR whose slot
// stays pinned.
func (h *harness) pinnedToasts() []string {
	h.t.Helper()
	var out []string
	for _, t := range h.nh.all() {
		if strings.Contains(t, "stays pinned") {
			out = append(out, t)
		}
	}
	return out
}

// A pin never lapses, but the close event used to say "slot released after
// 10m0s" while cleanup skipped the pinned slot in silence (talkable#11999
// closed on 10-12 and review16 stayed pinned since 10-07). The event now
// says the slot is kept and why, and one toast tells the operator once.
func TestAClosedPinnedPRSaysItsSlotIsKept(t *testing.T) {
	h := newHarness(t)
	h.openedPR() // #2 and review1 pinned by `magnum open`
	h.advance(time.Minute)
	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})

	var closed []string
	for _, ev := range h.events("pr.closed") {
		closed = append(closed, ev.Message)
	}
	if len(closed) != 1 || !strings.HasSuffix(closed[0], "; slot kept: pinned (magnum unpin)") || strings.Contains(closed[0], "released") {
		t.Fatalf("pr.closed events = %q, want one saying the slot is kept: pinned (magnum unpin)", closed)
	}
	want := "magnum: talkable#2 closed | its slot review1 stays pinned (magnum unpin)"
	if got := h.pinnedToasts(); len(got) != 1 || got[0] != want {
		t.Fatalf("toasts = %q, want %q", got, want)
	}

	// past the close grace the slot is still the operator's, and nothing says it again
	for range 3 {
		h.advance(10 * time.Minute)
		h.tick()
	}
	if sl := h.slot("review1"); !sl.Pinned || deref(sl.PRID) != h.pr(2).ID {
		t.Fatalf("the pinned slot was released: %+v", sl)
	}
	if got := h.pinnedToasts(); len(got) != 1 {
		t.Fatalf("toasts after the grace = %q, want the one", got)
	}
	if n := len(h.events("pr.closed")); n != 1 {
		t.Fatalf("pr.closed events after the grace = %d, want 1", n)
	}
}

// The wording is for pins: a PR that closes unpinned still has its slot
// released after the grace, and says so; nothing toasts.
func TestAClosedUnpinnedPRStillSaysItsSlotIsReleased(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})

	var closed []string
	for _, ev := range h.events("pr.closed") {
		closed = append(closed, ev.Message)
	}
	if len(closed) != 1 || !strings.Contains(closed[0], "; slot released after ") {
		t.Fatalf("pr.closed events = %q, want one saying the slot is released after the grace", closed)
	}
	if got := h.pinnedToasts(); len(got) != 0 {
		t.Fatalf("toasts for an unpinned PR: %q", got)
	}
	h.advance(10 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReleased)
}

// PinnedSince is what `magnum status` reads for a pin's age: the newest pin
// event of a subject still counts, an unpin forgets it, and a slot without a
// PR has its own events.
func TestPinnedSinceFollowsThePinEvents(t *testing.T) {
	h := newHarness(t)
	at := func(h int) time.Time { return time.Date(2026, 10, 7, h, 0, 0, 0, time.UTC) }
	add := func(kind, subject string, when time.Time) {
		t.Helper()
		if _, err := h.st.AppendEvent(h.ctx, store.Event{At: when, Level: "info", Kind: kind, Subject: &subject, Message: kind}); err != nil {
			t.Fatal(err)
		}
	}
	add("pr.opened", "pr:talkable/talkable#2", at(9))
	add("pr.pinned", "pr:talkable/talkable#3", at(10))
	add("pr.unpinned", "pr:talkable/talkable#3", at(11))
	add("pr.pinned", "pr:talkable/talkable#4", at(12))
	add("pr.pinned", "pr:talkable/talkable#4", at(13)) // pinned again: the newer
	add("slot.pinned", "slot:review7", at(14))
	add("slot.unpinned", "slot:review8", at(15))
	add("pr.muted", "pr:talkable/talkable#5", at(16))

	got, err := PinnedSince(h.ctx, h.st)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{"pr:talkable/talkable#2": at(9), "pr:talkable/talkable#4": at(13), "slot:review7": at(14)}
	if len(got) != len(want) {
		t.Fatalf("PinnedSince = %v, want %v", got, want)
	}
	for k, w := range want {
		if !got[k].Equal(w) {
			t.Errorf("%s pinned at %v, want %v", k, got[k], w)
		}
	}
}
