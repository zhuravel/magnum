package engine

import (
	"errors"
	"testing"
)

// A round goroutine drops the PR's reservation before its fix cycle or
// whiteboard reserves the PR again: while it is between the two
// (roundTails), the idle restart waits, so it never exits in that gap.
func TestAnIdleRestartWaitsForARoundsPostRoundHooks(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	h.startup()
	h.saveUserConfig("[daemon]\nmax_concurrent_reviews = 2\n", h.clock.Now())
	h.e.roundTails.Add(1) // a round between its unreserve and its fix cycle's reserve
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("tick: %v, want the daemon to wait for the round's hooks", err)
	}
	h.e.roundTails.Add(-1)
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrRestartForConfig) {
		t.Fatalf("tick with nothing in flight: %v, want ErrRestartForConfig", err)
	}
}
