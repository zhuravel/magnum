package app

import "testing"

// Each identity's rounds wait on the agents manager's run-ended broadcast:
// the observe pass that ends a run wakes the round's wait at once.
func TestTheRoundsWaitOnTheObservePassesRunEnds(t *testing.T) {
	a, _ := testApp(t, Options{})
	for name, r := range a.Pipeline {
		if r.Ended == nil {
			t.Fatalf("the %s runner has no run-ended broadcast: its waits poll alone", name)
		}
		if got, want := r.Ended(), a.Agents.Ended(); got != want {
			t.Fatalf("the %s runner waits on another channel than the agents manager's", name)
		}
	}
}
