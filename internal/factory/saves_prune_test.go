package factory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/slots"
)

// The factory daemon prunes the saves of its tasks' copies and of its fix
// checkouts past 14 days (slots.SaveKeep), at most once per pruneEvery, so
// a PR whose fix work ended never keeps them: nothing pruned them, and the
// code's words said the factory did.
func TestTheFactoryPrunesItsSavesPastTwoWeeks(t *testing.T) {
	h := newHarness(t)
	mk := func(folder string, at time.Time, slot string) string {
		dir := filepath.Join(folder, at.UTC().Format("20060102T150405Z")+"-"+slot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	now := h.clock.Now()
	oldTask := mk(h.layout.FactoryTaskSaves(3), now.Add(-slots.SaveKeep-time.Hour), "factory1")
	oldFix := mk(h.layout.FactoryPRWorkSaves("example/shop", 5), now.Add(-slots.SaveKeep-time.Hour), "fix-example-shop#5")
	young := mk(h.layout.FactoryPRWorkSaves("example/shop", 5), now.Add(-time.Hour), "fix-example-shop#5")

	h.tick()
	for _, d := range []string{oldTask, oldFix} {
		if _, err := os.Lstat(d); !os.IsNotExist(err) {
			t.Fatalf("%s, past 14 days: %v, want it pruned", d, err)
		}
	}
	if _, err := os.Lstat(young); err != nil {
		t.Fatalf("a save of an hour ago: %v, want it kept", err)
	}

	again := mk(h.layout.FactoryTaskSaves(3), now.Add(-slots.SaveKeep-time.Hour), "factory1")
	h.clock.Add(time.Minute)
	h.tick()
	if _, err := os.Lstat(again); err != nil {
		t.Fatalf("pruned again a minute later: %v, want it at most once per %s", err, pruneEvery)
	}
	h.clock.Add(pruneEvery)
	h.tick()
	if _, err := os.Lstat(again); !os.IsNotExist(err) {
		t.Fatalf("not pruned after %s: %v", pruneEvery, err)
	}
}
