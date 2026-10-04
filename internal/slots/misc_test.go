package slots

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

func TestCopyFilesBesidesMise(t *testing.T) {
	h := newHarness(t)
	h.pool.CopyFiles = append(h.pool.CopyFiles, "config/local.yml", "absent.txt")
	writeFile(t, filepath.Join(h.main, "config", "local.yml"), "x: 1\n")
	sl := h.provisioned(1)
	if got := readFile(t, filepath.Join(sl.Path, "config", "local.yml")); got != "x: 1\n" {
		t.Fatalf("copied file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(sl.Path, "absent.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source produced a file: %v", err)
	}
}

func TestFreeDiskBytes(t *testing.T) {
	free, err := freeDiskBytes(filepath.Join(t.TempDir(), "does", "not", "exist"))
	if err != nil || free == 0 {
		t.Fatalf("freeDiskBytes = %d, %v", free, err)
	}
}

func TestSlotNumberForPath(t *testing.T) {
	home := testHome // TestMain points HOME there, so ~ expands to it
	if got, err := os.UserHomeDir(); err != nil || got != home {
		t.Fatalf("os.UserHomeDir() = %q, %v; want the test home %q", got, err, home)
	}
	pool := config.Pool{SlotName: "review{n}", SlotPath: "~/Projects/talkable.review{n}"}
	cases := map[string]int{
		filepath.Join(home, "Projects", "talkable.review3"):  3,
		filepath.Join(home, "Projects", "talkable.review12"): 12,
		filepath.Join(home, "Projects", "talkable.repo3"):    0,
		filepath.Join(home, "Projects", "talkable.review"):   0,
		filepath.Join(home, "Projects", "talkable.review0"):  0,
	}
	for p, want := range cases {
		n, ok := slotNumberForPath(pool, p)
		if (want == 0 && ok) || (want != 0 && n != want) {
			t.Errorf("slotNumberForPath(%s) = %d, %v; want %d", p, n, ok, want)
		}
	}
}

func TestAsHold(t *testing.T) {
	err := errors.Join(errors.New("x"), ErrHold{Reason: HoldPinned})
	if h, ok := AsHold(err); !ok || h.Reason != HoldPinned {
		t.Fatalf("AsHold = %+v, %v", h, ok)
	}
	if _, ok := AsHold(errors.New("plain")); ok {
		t.Fatal("plain error reported as hold")
	}
}

func TestDryRunMutatorsTouchNothing(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	pr := h.pr(h.repo().ID, 7, h.shaPR7, store.PRQueued)
	h.run.reset()
	h.clearScripts()
	before, _ := h.st.EventsBySubject(h.ctx, "slot:review1", 0)

	m := h.newManager(Deps{DryRun: true})
	got, err := m.Claim(h.ctx, pr, h.pool)
	if err != nil || got.ID != sl.ID {
		t.Fatalf("dry-run Claim = %+v, %v", got, err)
	}
	if err := m.Checkout(h.ctx, sl, pr, h.pool, h.shaPR7); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(h.ctx, sl, h.pool, "x"); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(h.ctx, sl, h.pool, true); err != nil {
		t.Fatal(err)
	}
	if err := m.Repair(h.ctx, sl, h.pool); err != nil {
		t.Fatal(err)
	}
	if err := m.Pin(h.ctx, sl); err != nil {
		t.Fatal(err)
	}
	if err := m.Unpin(h.ctx, sl); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreatePRWorktree(h.ctx, config.Watch{CloneRoot: h.root}, "zhuravel/widget", pr, h.shaPR7); err != nil {
		t.Fatal(err)
	}
	if err := m.RemovePRWorktree(h.ctx, sl, false); err != nil {
		t.Fatal(err)
	}
	if len(h.run.calls) != 0 || len(h.scriptCalls("")) != 0 {
		t.Fatalf("dry-run ran commands: %v", h.run.calls)
	}
	after := h.slot("review1")
	if after.State != store.SlotFree || after.Pinned || after.PRID != nil {
		t.Fatalf("dry-run changed the slot: %+v", after)
	}
	if p, _ := h.st.PRByID(h.ctx, pr.ID); p.State != store.PRQueued {
		t.Fatalf("dry-run changed the PR: %s", p.State)
	}
	if evs, _ := h.st.EventsBySubject(h.ctx, "slot:review1", 0); len(evs) != len(before) {
		t.Fatalf("dry-run wrote events")
	}
}
