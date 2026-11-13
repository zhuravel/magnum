package slots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// failSaves points the manager's saves under a regular file, so every save
// fails at its folder; undo puts them back in the slot code's own folder.
func (h *harness) failSaves(t *testing.T) (undo func()) {
	t.Helper()
	blocker := filepath.Join(h.root, "not-a-folder")
	writeFile(t, blocker, "a file\n")
	h.m.SetSaveDir(func(store.Slot, store.PR) string { return filepath.Join(blocker, "saves") })
	return func() { h.m.SetSaveDir(func(store.Slot, store.PR) string { return "" }) }
}

// failStatus makes git status fail in every tree, as a listing of the
// changes that cannot be read; undo lets it through again.
func (h *harness) failStatus(t *testing.T) (undo func()) {
	t.Helper()
	h.fake.Rules = append([]execx.Rule{{Prefix: []string{"git"}, Result: execx.Result{Code: 128, Stderr: []byte("fatal: index file corrupt\n")}}},
		h.fake.Rules...)
	h.run.fakeGit = func(c execx.Cmd) bool { return slices.Contains(c.Args, "status") }
	return func() { h.run.fakeGit = nil }
}

// wantKept checks that the failed step discarded nothing in the tree at
// dir: HEAD at head, the tracked change and the untracked file there, and
// no slot.discarded event.
func (h *harness) wantKept(t *testing.T, name, dir, head string) {
	t.Helper()
	if got := gitT(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %s, want it left at %s", got, head)
	}
	if got := readFile(t, filepath.Join(dir, "Gemfile.lock")); got != "GEM stray\n" {
		t.Fatalf("the tracked change = %q, want it kept", got)
	}
	if got := readFile(t, filepath.Join(dir, "notes.txt")); got != "mine\n" {
		t.Fatalf("the untracked file = %q, want it kept", got)
	}
	if evs := h.discarded(t, name); len(evs) != 0 {
		t.Fatalf("slot.discarded events = %+v, want none", evs)
	}
}

// dirty leaves a tracked change and an untracked file in the tree at dir.
func dirty(t *testing.T, dir string) {
	t.Helper()
	strayEdit(t, dir)
	writeFile(t, filepath.Join(dir, "notes.txt"), "mine\n")
}

// A save that fails fails the step, which discards nothing: a resync's
// switch, a release's reset, a removal of a per-PR worktree; only a forced
// removal goes on, as you asked. No test failed the save before.
func TestAFailedSaveDiscardsNothing(t *testing.T) {
	t.Run("resync", func(t *testing.T) {
		h := newHarness(t)
		sl, _ := h.claimedCheckout(8, h.shaPR8)
		dirty(t, sl.Path)
		h.failSaves(t)
		if err := h.m.Resync(h.ctx, sl, h.shaPR7); err == nil {
			t.Fatal("Resync with a save that fails: no error")
		}
		h.wantKept(t, sl.Name, sl.Path, h.shaPR8)
	})
	t.Run("release", func(t *testing.T) {
		h := newHarness(t)
		sl, _ := h.claimedCheckout(8, h.shaPR8)
		dirty(t, sl.Path)
		h.failSaves(t)
		if err := h.m.Release(h.ctx, sl, h.pool, "pr_closed"); err == nil {
			t.Fatal("Release with a save that fails: no error")
		}
		h.wantKept(t, sl.Name, sl.Path, h.shaPR8)
		if got := h.slot(sl.Name); got.State != store.SlotReleasing {
			t.Fatalf("state = %s, want releasing, to resume", got.State)
		}
	})
	t.Run("removal", func(t *testing.T) {
		f := newPerPR(t, true)
		h := f.h
		sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
		if err != nil {
			t.Fatal(err)
		}
		dirty(t, sl.Path)
		h.failSaves(t)
		if err := h.m.RemovePRWorktree(h.ctx, sl, false); err == nil {
			t.Fatal("RemovePRWorktree with a save that fails: no error")
		}
		h.wantKept(t, sl.Name, sl.Path, f.sha7)
		if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), true); err != nil {
			t.Fatalf("a forced removal: %v", err)
		}
		if got := h.slot(sl.Name); got.State != store.SlotRemoved || fsx.Exists(sl.Path) {
			t.Fatalf("state %s, directory left %v; want a forced removal to go on", got.State, fsx.Exists(sl.Path))
		}
		if _, ok := h.saveFailure(t, sl); ok {
			t.Fatal("a forced removal left its slot's save failure recorded")
		}
	})
}

// A listing of the changes that fails fails the step too, which discards
// nothing: it was logged, and the switch dropped the changes unsaved.
func TestAFailedListingDiscardsNothing(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	dirty(t, sl.Path)
	h.failStatus(t)
	if err := h.m.Resync(h.ctx, sl, h.shaPR7); err == nil {
		t.Fatal("Resync with a listing that fails: no error")
	}
	h.wantKept(t, sl.Name, sl.Path, h.shaPR8)
}

// A forced removal whose save fails goes on, as you asked, and says what
// it did: a slot.discarded event at level warn names the changes it
// deleted and says "forced, not saved", with the save's error. It raised
// the failure as one the step retries (a slot.save_failed event and the
// toast saying the step discards nothing and retries), then wrote
// slot.save_ok "with nothing left to save", after it deleted the changes
// unsaved.
func TestAForcedRemovalWhoseSaveFailsSaysItsChangesWentUnsaved(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	dirty(t, sl.Path)
	var mu sync.Mutex
	alerts := 0
	h.m.SetSaveAlert(func(context.Context, SaveFailure) {
		mu.Lock()
		defer mu.Unlock()
		alerts++
	})
	h.failSaves(t)

	if err := h.m.RemovePRWorktree(h.ctx, sl, true); err != nil {
		t.Fatalf("a forced removal: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoved || fsx.Exists(sl.Path) {
		t.Fatalf("state %s, directory left %v; want a forced removal to go on", got.State, fsx.Exists(sl.Path))
	}
	subject := "slot:" + sl.Name
	if evs := eventKinds(t, h.st, subject, "slot.save_failed"); len(evs) != 0 || alerts != 0 {
		t.Fatalf("slot.save_failed events %+v, %d alerts; want none: the forced step retries nothing", evs, alerts)
	}
	if evs := eventKinds(t, h.st, subject, "slot.save_ok"); len(evs) != 0 {
		t.Fatalf("slot.save_ok events = %+v, want none: nothing was saved", evs)
	}
	evs := h.discarded(t, sl.Name)
	if len(evs) != 1 || evs[0].Level != "warn" {
		t.Fatalf("slot.discarded events = %+v, want one warning", evs)
	}
	for _, want := range []string{"Gemfile.lock", "notes.txt", "forced, not saved (slots: save the changes in"} {
		if !strings.Contains(evs[0].Message, want) {
			t.Fatalf("slot.discarded = %q, want it to say %q", evs[0].Message, want)
		}
	}
}

// A removal clears its slot's recorded save failure when it marks the slot
// removed, however it ends: here through a worktree whose folder was
// deleted by hand after the save failed, which git lists as prunable, so
// the removal prunes it and saves nothing. And SaveFailures, which doctor
// reads, names no removed slot's failure, such as one a removal that is
// not this package's left. The record stood for good, and doctor warned of
// a slot that no longer exists.
func TestARemovedSlotKeepsNoSaveFailure(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	dirty(t, sl.Path)
	undo := h.failSaves(t)
	if err := h.m.RemovePRWorktree(h.ctx, sl, false); err == nil {
		t.Fatal("RemovePRWorktree with a save that fails: no error")
	}
	if _, ok := h.saveFailure(t, sl); !ok {
		t.Fatal("the failed save is not recorded")
	}
	undo()
	if err := os.RemoveAll(sl.Path); err != nil {
		t.Fatal(err)
	}

	if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false); err != nil {
		t.Fatalf("RemovePRWorktree of a worktree gone: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoved {
		t.Fatalf("state = %s, want removed", got.State)
	}
	if _, ok, err := h.st.GetKV(h.ctx, saveFailedKey(sl.ID)); err != nil || ok {
		t.Fatalf("the removed slot's save failure: recorded %v, %v; want it cleared", ok, err)
	}

	_ = h.m.saveFailed(h.ctx, h.slot(sl.Name), SaveFailedSave, "the removal of the worktree", errors.New("left by another removal"))
	if _, ok, err := h.st.GetKV(h.ctx, saveFailedKey(sl.ID)); err != nil || !ok {
		t.Fatalf("a failure left for the removed slot: recorded %v, %v", ok, err)
	}
	if f, ok := h.saveFailure(t, sl); ok {
		t.Fatalf("SaveFailures names the removed slot's failure: %+v", f)
	}
}

// saveFailure is slot sl's recorded save failure, as SaveFailures reads it.
func (h *harness) saveFailure(t *testing.T, sl store.Slot) (SaveFailure, bool) {
	t.Helper()
	all, err := SaveFailures(h.ctx, h.st)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(f SaveFailure) bool { return f.Slot == sl.Name })
	if i < 0 {
		return SaveFailure{}, false
	}
	return all[i], true
}

// A save or a listing that keeps failing is raised once per slot, not at
// every retry: one slot.save_failed event and one alert (the review
// daemon's toast) whose words say the step retries, and a record that
// `magnum doctor` reads; a listing that fails after a save failed is raised
// once more. The step's next save clears it, with a slot.save_ok event.
// Each retry was a warning in the log alone.
func TestASaveThatKeepsFailingIsRaisedOncePerSlot(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	dirty(t, sl.Path)
	var mu sync.Mutex
	var alerts []SaveFailure
	h.m.SetSaveAlert(func(_ context.Context, f SaveFailure) {
		mu.Lock()
		defer mu.Unlock()
		alerts = append(alerts, f)
	})
	failed := func() []store.Event { return eventKinds(t, h.st, "slot:"+sl.Name, "slot.save_failed") }

	undo := h.failSaves(t)
	for range 3 {
		if err := h.m.Resync(h.ctx, h.slot(sl.Name), h.shaPR7); err == nil {
			t.Fatal("Resync with a save that fails: no error")
		}
	}
	evs := failed()
	if len(evs) != 1 || len(alerts) != 1 {
		t.Fatalf("after three failed saves: %d slot.save_failed events, %d alerts; want one of each", len(evs), len(alerts))
	}
	if evs[0].Level != "error" || !strings.Contains(evs[0].Message, "save") || !strings.Contains(evs[0].Message, "retries") {
		t.Fatalf("slot.save_failed = %+v, want an error that names the save and says the step retries", evs[0])
	}
	if a := alerts[0]; a.Slot != sl.Name || a.Kind != SaveFailedSave || !strings.Contains(a.What, "resync") || a.Error == "" {
		t.Fatalf("alert = %+v", a)
	}
	if f, ok := h.saveFailure(t, sl); !ok || f.Kind != SaveFailedSave || !f.Since.Equal(h.now) {
		t.Fatalf("recorded failure = %+v, %v", f, ok)
	}
	undo()

	undo = h.failStatus(t)
	for range 2 {
		if err := h.m.Resync(h.ctx, h.slot(sl.Name), h.shaPR7); err == nil {
			t.Fatal("Resync with a listing that fails: no error")
		}
	}
	if evs := failed(); len(evs) != 2 || len(alerts) != 2 || alerts[1].Kind != SaveFailedListing {
		t.Fatalf("after two failed listings: %d slot.save_failed events, alerts %+v; want one more of each, for the listing", len(evs), alerts)
	}
	undo()

	if err := h.m.Resync(h.ctx, h.slot(sl.Name), h.shaPR7); err != nil {
		t.Fatalf("Resync once the save works: %v", err)
	}
	if f, ok := h.saveFailure(t, sl); ok {
		t.Fatalf("a save that worked left the failure recorded: %+v", f)
	}
	if evs := eventKinds(t, h.st, "slot:"+sl.Name, "slot.save_ok"); len(evs) != 1 {
		t.Fatalf("slot.save_ok events = %+v, want one", evs)
	}
}
