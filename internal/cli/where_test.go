package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/store"
)

func TestWhereFind(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	refs := app.RefParser{DefaultRepo: "talkable/talkable"}
	slotDir := f.Home + "/talkable.review1"
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, held := inspSeedPR(t, st, "talkable/talkable", 11920, store.PRReviewed, nil)
	_, parked := inspSeedPR(t, st, "talkable/talkable", 11921, store.PRReviewed, nil)
	sl, err := st.CreateSlot(ctx, store.Slot{Name: "review1", Kind: store.SlotKindPool, Path: slotDir, MainClone: f.Home + "/talkable",
		RepoFullName: "talkable/talkable", State: store.SlotHeld, PRID: &held.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSlot(ctx, store.Slot{Name: "review2", Kind: store.SlotKindPool, Path: f.Home + "/gone",
		MainClone: f.Home + "/talkable", RepoFullName: "talkable/talkable", State: store.SlotLost}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenAssignment(ctx, store.Assignment{PRID: parked.ID, SlotID: sl.ID, Path: slotDir}); err != nil {
		t.Fatal(err)
	}
	ext := &fakeScanner{inv: inventory.Inventory{External: []inventory.ExternalView{
		{Path: f.Home + "/talkable.repo3", Repo: "talkable/talkable", Exists: true, PRNumber: 11999, PRConfirmed: true},
		{Path: f.Home + "/talkable.repo4", Repo: "talkable/talkable", Exists: true, PRNumber: 27087, PRSource: inventory.PRSourceBranchName},
	}}}

	cases := []struct {
		ref, path, note, err string
	}{
		{ref: "review1", path: slotDir},
		{ref: "11920", path: slotDir},
		{ref: "https://github.com/talkable/talkable/pull/11920", path: slotDir},
		{ref: "review2", err: "does not exist"},
		{ref: "11921", err: "has no folder"},
		{ref: "11999", path: f.Home + "/talkable.repo3", note: "manual worktree"},
		{ref: "27087", err: "not in the registry"},
		{ref: "what?", err: "neither a slot name nor a PR reference"},
	}
	for _, tc := range cases {
		path, note, err := whereFind(ctx, st, refs, ext, tc.ref)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want %q", tc.ref, err, tc.err)
			}
			continue
		}
		if err != nil || path != tc.path || !strings.Contains(note, tc.note) {
			t.Errorf("%s: path=%q note=%q err=%v, want %q", tc.ref, path, note, err, tc.path)
		}
	}
}

func TestWhereCommand(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	slotDir := f.Home + "/talkable.review1"
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSlot(context.Background(), store.Slot{Name: "review1", Kind: store.SlotKindPool, Path: slotDir,
		MainClone: f.Home + "/talkable", RepoFullName: "talkable/talkable", State: store.SlotFree}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if code := f.run("where", "review1"); code != 0 || f.Out.String() != slotDir+"\n" {
		t.Fatalf("code %d out %q err %q", code, f.Out.String(), f.Err.String())
	}
	if code := f.run("where"); code != 2 {
		t.Fatalf("no args: code %d", code)
	}
	if code := f.run("where", "review9"); code != 1 || f.Out.Len() != 0 || !strings.Contains(f.Err.String(), "magnum where:") {
		t.Fatalf("unknown slot: code %d out %q err %q", code, f.Out.String(), f.Err.String())
	}
}
