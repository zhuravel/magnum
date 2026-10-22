package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
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

// A pin that is the hold `magnum open` set shows as held with the hold's
// end, not as pinned: on the pins line, on the PR's card and in --json (the
// pin's held_until, the card's held_until); the hold lasts human_cooldown
// after the last input in the PR's panes when that is later.
func TestStatusShowsAnOpensHoldWithItsEnd(t *testing.T) {
	d, now := pinsFixture(t)
	ctx := context.Background()
	repo, err := d.Store.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := d.Store.PRByRepoNumber(ctx, repo.ID, 12010)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(engine.OpenHold{Until: now.Add(20 * time.Minute), At: now.Add(-10 * time.Minute)})
	if err := d.Store.SetKV(ctx, engine.KVPROpenHold(pr.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	typed := now.Add(10 * time.Minute) // a later input keeps it human_cooldown (15m) longer
	if err := d.Store.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", typed) }); err != nil {
		t.Fatal(err)
	}
	if pr, err = d.Store.PRByID(ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	scan := d.Inventory.(*fakeScanner) // the inventory reads the PR as it is now
	for i := range scan.inv.Slots {
		if scan.inv.Slots[i].Slot.Name == "review3" {
			scan.inv.Slots[i].PR = &pr
		}
	}
	end := typed.Add(15 * time.Minute)
	clock := inspClock(now, end)

	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	statusRender(&out, r)
	if want := "          review3 (talkable#12010, open, held by open until " + clock + ")\n"; !strings.Contains(out.String(), want) {
		t.Errorf("status lacks %q:\n%s", want, out.String())
	}
	if len(r.Pins) != 3 || r.Pins[1].HeldUntil == nil || !r.Pins[1].HeldUntil.Equal(end) || r.Pins[0].HeldUntil != nil {
		t.Errorf("pins = %+v", r.Pins)
	}
	var js bytes.Buffer
	if err := writeJSON(&js, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"held_until"`) {
		t.Errorf("JSON lacks the hold's end:\n%s", js.String())
	}

	r, err = statusGather(ctx, d, statusOptions{Ref: "12010"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	statusRender(&out, r)
	if want := "state:     reviewed (GitHub OPEN), held by open until " + clock + "\n"; !strings.Contains(out.String(), want) {
		t.Errorf("card lacks %q:\n%s", want, out.String())
	}
	if r.Detail == nil || r.Detail.HeldUntil == nil || !r.Detail.HeldUntil.Equal(end) {
		t.Errorf("detail = %+v", r.Detail)
	}
}
