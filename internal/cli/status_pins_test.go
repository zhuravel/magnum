package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/store"
)

// pinsFixture is statusFixture with three pins: review16 on talkable#11999,
// closed 5 hours ago and pinned 5 days ago, review3 on the open
// talkable#12010 pinned 2 days ago, review9 pinned 3 hours ago with no PR;
// review1 (the fixture's) is not pinned.
func pinsFixture(t *testing.T) (statusDeps, time.Time) {
	t.Helper()
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	closedAt := now.Add(-5 * time.Hour)
	_, closed := inspSeedPR(t, st, "talkable/talkable", 11999, store.PRReviewed, func(u *store.PRUpdate) { u.Set("pinned", true) })
	if err := st.TransitionPR(ctx, closed.ID, []string{store.PRReviewed}, store.PRClosed, func(u *store.PRUpdate) {
		u.Copy("prev_state", "state")
		u.Set("gh_state", store.GHClosed)
		u.Set("closed_at", closedAt)
	}); err != nil {
		t.Fatal(err)
	}
	_, open := inspSeedPR(t, st, "talkable/talkable", 12010, store.PRReviewed, func(u *store.PRUpdate) { u.Set("pinned", true) })
	closed, err := st.PRByID(ctx, closed.ID)
	if err != nil {
		t.Fatal(err)
	}
	event := func(kind, subject string, at time.Time) {
		t.Helper()
		if _, err := st.AppendEvent(ctx, store.Event{At: at, Level: "info", Kind: kind, Subject: &subject, Message: kind}); err != nil {
			t.Fatal(err)
		}
	}
	event("pr.pinned", "pr:talkable/talkable#11999", now.Add(-5*24*time.Hour))
	event("pr.opened", "pr:talkable/talkable#12010", now.Add(-2*24*time.Hour))
	event("slot.pinned", "slot:review9", now.Add(-3*time.Hour))
	size := int64(1 << 20)
	slot := func(name string, pinned bool, pr *store.PR) inventory.SlotView {
		sl := store.Slot{Name: name, Kind: store.SlotKindPool, Path: "/home/x/talkable." + name, State: store.SlotHeld, Pinned: pinned}
		return inventory.SlotView{Slot: sl, Exists: true, Worktree: true, PR: pr, SizeKB: &size}
	}
	scan := d.Inventory.(*fakeScanner)
	scan.inv.Slots = append(scan.inv.Slots, slot("review16", true, &closed), slot("review3", true, &open), slot("review9", true, nil))
	return d, now
}

// A pin never lapses, so the operator needs to see what each one holds: the
// slot, the PR, whether GitHub closed it and when, and how long the pin has
// been there ("pinned: review16 (talkable#11999, closed 5h ago, pinned 5d)").
func TestStatusListsPinsWithTheirAge(t *testing.T) {
	d, now := pinsFixture(t)
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	out := b.String()
	for _, want := range []string{
		"pinned:   review16 (talkable#11999, closed 5h ago, pinned 5d)\n",
		"          review3 (talkable#12010, open, pinned 2d)\n",
		"          review9 (no PR, pinned 3h)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "review1 (") {
		t.Errorf("the unpinned review1 is listed:\n%s", out)
	}
	if len(r.Pins) != 3 || r.Pins[0].Slot != "review16" || r.Pins[0].Number != 11999 || r.Pins[0].ClosedAt == nil ||
		!r.Pins[0].ClosedAt.Equal(now.Add(-5*time.Hour)) || r.Pins[0].PinnedAt == nil || !r.Pins[0].PinnedAt.Equal(now.Add(-5*24*time.Hour)) {
		t.Errorf("pins = %+v", r.Pins)
	}
}

// Without a pin there is no line, and a pin the events no longer remember
// shows no age instead of a wrong one.
func TestStatusPinsLineIsLeftOutWithoutPinsAndWithoutAgeWhenUnknown(t *testing.T) {
	_, _, d, _ := statusFixture(t)
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if strings.Contains(b.String(), "pinned:") || len(r.Pins) != 0 {
		t.Fatalf("a status without pins lists some: %+v\n%s", r.Pins, b.String())
	}

	d, _ = pinsFixture(t)
	if _, err := d.Store.DB().ExecContext(context.Background(), "DELETE FROM events WHERE kind LIKE '%pinned' OR kind = 'pr.opened'"); err != nil {
		t.Fatal(err)
	}
	r, err = statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b.Reset()
	statusRender(&b, r)
	if !strings.Contains(b.String(), "pinned:   review16 (talkable#11999, closed 5h ago)\n") {
		t.Fatalf("a pin without events:\n%s", b.String())
	}
}
