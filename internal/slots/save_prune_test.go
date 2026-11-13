package slots

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A save whose changes.diff is the same, byte for byte, as the newest save
// of the same slot in its folder is not kept: the slot.discarded event
// names that save. post_checkout's residue (a Gemfile.lock rewritten, a
// db/schema.rb) is saved once, not at every round's switch.
func TestASaveTheSameAsTheSlotsLastIsNotKept(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	folder := h.layout.PRSaves("talkable", "talkable", pr.Number)
	resync := func() {
		t.Helper()
		h.now = h.now.Add(time.Minute)
		strayEdit(t, sl.Path)
		if err := h.m.Resync(h.ctx, h.slot(sl.Name), h.shaPR7); err != nil {
			t.Fatalf("Resync: %v", err)
		}
	}
	resync() // against PR #8's head
	resync() // against PR #7's: another patch
	if got := saves(t, folder); len(got) != 2 {
		t.Fatalf("saves = %q, want two of different patches", got)
	}
	resync() // the same residue against the same HEAD
	got := saves(t, folder)
	if len(got) != 2 {
		t.Fatalf("saves = %q, want the same patch kept once", got)
	}
	evs := h.discarded(t, sl.Name)
	if len(evs) != 3 || !strings.Contains(evs[2].Message, got[1]) || !strings.Contains(evs[2].Message, "unchanged") {
		t.Fatalf("the third slot.discarded event = %+v, want it to name the save it repeats, %s", evs[len(evs)-1], got[1])
	}
}

// fakeSave makes a save named name (its time and slot) in folder, as
// saveChanges leaves one.
func fakeSave(t *testing.T, folder, name string) string {
	t.Helper()
	dir := filepath.Join(folder, name)
	writeFile(t, filepath.Join(dir, "saved.txt"), "magnum saved\n")
	return dir
}

// Saves older than 14 days are pruned: a review slot's at its next save,
// in its folder, and every folder of saves by PruneSaves, the review
// daemon's (its PRs', a slot's of no PR) and the factory's (its tasks', its
// PR work's), whose PRs may never save again. A folder whose name is no
// save's is left alone. Nothing pruned them, and a save of the same residue
// was written at every round.
func TestSavesOlderThanTwoWeeksArePruned(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	folder := h.layout.PRSaves("talkable", "talkable", pr.Number)
	old := fakeSave(t, folder, "20260901T120000Z-review1")
	young := fakeSave(t, folder, "20260925T120000Z-review1")
	strayEdit(t, sl.Path)
	if err := h.m.Resync(h.ctx, sl, h.shaPR7); err != nil { // at t0, 2026-10-03
		t.Fatalf("Resync: %v", err)
	}
	if fsx := saves(t, folder); slices.Contains(fsx, old) || !slices.Contains(fsx, young) || len(fsx) != 2 {
		t.Fatalf("saves after a save on 2026-10-03 = %q, want the one of 09-01 pruned, 09-25's and the new one kept", fsx)
	}

	l := h.layout
	gone := []string{
		fakeSave(t, l.PRSaves("talkable", "talkable", 99), "20260901T000000Z-review2"),
		fakeSave(t, l.SlotSaves("review3"), "20260901T000000Z-review3"),
		fakeSave(t, l.FactoryTaskSaves(4), "20260901T000000Z-factory1"),
		fakeSave(t, l.FactoryPRWorkSaves("example/shop", 5), "20260901T000000Z-fix-example-shop#5"),
	}
	kept := []string{
		fakeSave(t, l.PRSaves("talkable", "talkable", 99), "20261001T000000Z-review2"),
		fakeSave(t, l.FactoryPRWorkSaves("example/shop", 5), "20261002T000000Z-fix-example-shop#5"),
		fakeSave(t, l.FactoryTaskSaves(4), "notes"),
	}
	if n, err := PruneSaves(l.ReviewSaveFolders(), h.now); err != nil || n != 2 {
		t.Fatalf("PruneSaves of the review daemon's folders = %d, %v; want 2", n, err)
	}
	if n, err := PruneSaves(l.FactorySaveFolders(), h.now); err != nil || n != 2 {
		t.Fatalf("PruneSaves of the factory's folders = %d, %v; want 2", n, err)
	}
	for _, d := range gone {
		if _, err := os.Lstat(d); !os.IsNotExist(err) {
			t.Fatalf("%s older than 14 days: %v, want it pruned", d, err)
		}
	}
	for _, d := range kept {
		if _, err := os.Lstat(d); err != nil {
			t.Fatalf("%s: %v, want it kept", d, err)
		}
	}
}
