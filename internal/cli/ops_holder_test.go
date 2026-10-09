package cli

import (
	"testing"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// opsHolder's zero value was opsMine, "run the work here": a holder nobody
// set read as both locks held. The zero value is opsUnknown, and a failed
// acquireOps returns it.
func TestAnUnsetOpsHolderIsUnknown(t *testing.T) {
	var who opsHolder
	if who != opsUnknown || who == opsMine {
		t.Fatalf("zero opsHolder = %d", who)
	}
	l := paths.Layout{Home: t.TempDir()} // no state directory: the locks cannot open
	unlock, who, err := acquireOps(l)
	if err == nil || unlock != nil || who != opsUnknown {
		t.Fatalf("failed acquireOps: who %d err %v", who, err)
	}
}

// A release whose lock answers neither "mine", "the daemon's" nor "busy"
// runs nothing here and hands nothing to the daemon: only opsMine runs the
// work in-process.
func TestReleaseWithAnUnknownLockHolderDoesNothing(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.cleaner.plan = cleanup.Plan{Actions: []cleanup.Action{{Kind: "release", Slot: "review3"}}}
	h.d.Lock = func() (func(), opsHolder, error) { return nil, opsUnknown, nil }
	if code := h.cmd("release", "5"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "magnum release:")
	if h.cleaner.applied != 0 || len(h.cleaner.opts) != 0 || len(h.requests()) != 0 {
		t.Fatalf("applied %d planned %d requests %d", h.cleaner.applied, len(h.cleaner.opts), len(h.requests()))
	}
}
