package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/slots"
)

// The reconcile's prune removes the review slots' saves past 14 days
// (slots.SaveKeep), those of PRs that never save again too: nothing pruned
// them.
func TestThePrunePrunesTheReviewSlotsSavesPastTwoWeeks(t *testing.T) {
	h := newHarness(t)
	mk := func(folder string, at time.Time, slot string) string {
		dir := filepath.Join(folder, at.UTC().Format("20060102T150405Z")+"-"+slot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	now := h.clock.Now()
	old := mk(h.d.Layout.PRSaves("talkable", "talkable", 5), now.Add(-slots.SaveKeep-time.Hour), "review1")
	stray := mk(h.d.Layout.SlotSaves("review2"), now.Add(-slots.SaveKeep-time.Hour), "review2")
	young := mk(h.d.Layout.PRSaves("talkable", "talkable", 5), now.Add(-time.Hour), "review1")

	if err := h.e.prune(h.ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, d := range []string{old, stray} {
		if _, err := os.Lstat(d); !os.IsNotExist(err) {
			t.Fatalf("%s, past 14 days: %v, want it pruned", d, err)
		}
	}
	if _, err := os.Lstat(young); err != nil {
		t.Fatalf("a save of an hour ago: %v, want it kept", err)
	}
}
