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

// A save is the slot's last one again only when it also names what that
// save names: the files it left out and the untracked files the tree
// keeps. A removal whose patch repeats the last switch's but that leaves
// out a file past the cap is kept, and its saved.txt and event name the
// file; a switch that keeps one more untracked file in the tree is kept;
// one that keeps the same files again is not. The check compared
// changes.diff alone, so the removal's save was dropped and its event said
// the earlier save held a file it never held, with no word of why the file
// was left out.
func TestASaveRepeatsTheLastOnlyWithTheSameFilesLeftOutAndKept(t *testing.T) {
	t.Run("a removal that leaves out a file", func(t *testing.T) {
		f := newPerPR(t, true)
		h := f.h
		h.m.saveCap = 1500
		sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
		if err != nil {
			t.Fatal(err)
		}
		folder := h.layout.PRSaves("zhuravel", "widget", f.pr.Number)
		writeFile(t, filepath.Join(sl.Path, "README"), "residue\n")
		if err := h.m.Resync(h.ctx, sl, f.sha7); err != nil {
			t.Fatalf("Resync: %v", err)
		}
		h.now = h.now.Add(time.Minute)
		writeFile(t, filepath.Join(sl.Path, "README"), "residue\n")
		writeFile(t, filepath.Join(sl.Path, "large.txt"), strings.Repeat("x", 2000))
		if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false); err != nil {
			t.Fatalf("RemovePRWorktree: %v", err)
		}
		got := saves(t, folder)
		if len(got) != 2 {
			t.Fatalf("saves = %q, want the removal's kept beside the switch's", got)
		}
		if note := readFile(t, filepath.Join(got[1], "saved.txt")); !strings.Contains(note, "large.txt (") {
			t.Fatalf("the removal's saved.txt does not name large.txt as left out:\n%s", note)
		}
		evs := h.discarded(t, sl.Name)
		if ev := evs[len(evs)-1].Message; !strings.Contains(ev, got[1]) || !strings.Contains(ev, "large.txt (") || strings.Contains(ev, "unchanged") {
			t.Fatalf("the removal's slot.discarded = %q, want it to name its own save and why large.txt was left out", ev)
		}
	})
	t.Run("a switch that keeps one more file", func(t *testing.T) {
		h := newHarness(t)
		sl, pr := h.claimedCheckout(8, h.shaPR8)
		folder := h.layout.PRSaves("talkable", "talkable", pr.Number)
		resync := func(want int) {
			t.Helper()
			h.now = h.now.Add(time.Minute)
			strayEdit(t, sl.Path)
			if err := h.m.Resync(h.ctx, h.slot(sl.Name), h.shaPR7); err != nil {
				t.Fatalf("Resync: %v", err)
			}
			if got := saves(t, folder); len(got) != want {
				t.Fatalf("saves = %q, want %d", got, want)
			}
		}
		writeFile(t, filepath.Join(sl.Path, "a.txt"), "a\n")
		resync(1) // against PR #8's head
		resync(2) // against PR #7's: another patch
		writeFile(t, filepath.Join(sl.Path, "b.txt"), "b\n")
		resync(3) // the same patch, one more file kept
		resync(3) // the same patch and the same files kept: a repeat
		if ev := h.discarded(t, sl.Name)[3].Message; !strings.Contains(ev, "unchanged") {
			t.Fatalf("the fourth slot.discarded = %q, want it to name the save it repeats", ev)
		}
	})
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
