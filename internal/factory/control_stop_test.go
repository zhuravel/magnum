package factory

import (
	"slices"
	"testing"
	"time"
)

// A stop whose wait for the work off the loop ran out keeps the record of
// that work: the work may still run (its agents in herdr), so a drain read
// after the daemon exited must not take it for a clean stop. A stop whose
// work ended removes the record, as before.
func TestAStopWhoseWorkOffTheLoopDidNotEndKeepsItsRecord(t *testing.T) {
	h := newHarness(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	end := h.e.beginWork(WorkClaim, 1, "claim of a copy for PR-12610")
	defer end()
	h.e.bg.Go(func() { <-block })
	h.e.drainAt.Store(time.Now().Add(-drainBudget + 200*time.Millisecond).UnixNano()) // 200 ms of the stop's wait left

	h.e.stopBackground()
	h.e.closeWork()

	rec, ok, err := ReadWork(h.layout)
	if err != nil || !ok || !slices.Contains(whats(rec), "claim: claim of a copy for PR-12610") {
		t.Fatalf("the record after a stop whose work did not end: %+v (%v, %v), want it kept naming the claim", rec, ok, err)
	}
}
