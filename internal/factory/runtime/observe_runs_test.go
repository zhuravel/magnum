package runtime

import (
	"reflect"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
)

// observeAfter runs one observe pass d after the last.
func (g *rig) observeAfter(d time.Duration) []Event {
	g.t.Helper()
	g.clock.Add(d)
	evs, err := g.r.Observe(g.ctx)
	if err != nil {
		g.t.Fatalf("Observe: %v", err)
	}
	return evs
}

// Between its full passes, at most fullObserveEvery apart, the observe pass
// reads only the sessions with a turn in flight (agents.ObserveRunsAt): on
// the 5 s observe a pass over every live session would write each idle
// one's row (its idle count) twice as often as at 10 s, the cadence before.
// A session without a turn in flight (a pane an earlier turn left live) is
// read on the full passes alone, and a turn in flight ends on a narrow one.
func TestTheObservePassReadsOnlyTheTurnsInFlightBetweenFullPasses(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro", "fx-history")
	g.send("fx-repro", "reproduce the bug")
	id := g.send("fx-history", "read the history")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.observeN(2) // fx-repro's run ended: its session stays live with no turn in flight
	g.h.setStatus(g.agent("fx-history"), herdr.StatusIdle)
	g.observe() // a full pass, 10 s after the last: fx-history's first idle reading
	idle := g.session("fx-repro").IdleTicks

	evs := g.observeAfter(5 * time.Second) // 5 s after the full pass: the turns in flight alone
	want := Event{TaskID: g.task.ID, Role: "fx-history", RunID: id, Kind: EventEnded}
	if !reflect.DeepEqual(evs, []Event{want}) {
		t.Fatalf("the pass between the full ones gave %+v, want %+v", evs, want)
	}
	if got := g.session("fx-repro").IdleTicks; got != idle {
		t.Fatalf("the pass between the full ones wrote the idle count of a session with no turn in flight: %d, was %d", got, idle)
	}

	g.observeAfter(5 * time.Second) // 10 s after the last full pass: every session
	if got := g.session("fx-repro").IdleTicks; got != idle+1 {
		t.Fatalf("the full pass left the idle session's idle count at %d, want %d", got, idle+1)
	}
}
