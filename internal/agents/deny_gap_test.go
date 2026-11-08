package agents

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// After a deny, the continuation waits for a screen it may type into (no
// dialog, an empty composer), and the screen may lag herdr's idle or not
// read at all for a moment. With the tick's readings a minute apart it had
// a whole tick for that; with the observe clock's a few seconds apart, the
// gap alone would give it one more look 3 s later and then end the run
// unfinished (the report a round lost so). While a deny awaits its
// continuation, the run ends only denyContinueGap after its streak's first
// idle reading.
func TestADenyAwaitingItsContinuationHoldsTheRunsEndLonger(t *testing.T) {
	for _, tc := range []struct {
		name    string
		catchUp bool // the screen shows the rejected prompt after the hold's second look
	}{
		{"the screen catches up", true},
		{"the screen never does", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.started()
			run := e.denyThenIdle()
			e.idleAfterDeny(claudeRmPrompt) // herdr says idle; the screen still shows the dialog

			e.clock.Add(5 * time.Second)
			first := e.clock.Now()
			o := e.observe()[RoleClaude]
			if o.Kind != "" || o.Session.IdleTicks != 1 {
				t.Fatalf("first idle reading after the deny = %+v", o)
			}
			if want := first.Add(denyContinueGap); !o.SecondReadingAt.Equal(want) {
				t.Fatalf("first idle reading: next reading due %v, want %v (the deny's gap)", o.SecondReadingAt, want)
			}
			e.clock.Add(CompletionGap)
			if o := e.observe()[RoleClaude]; o.Kind != "" {
				t.Fatalf("reading %s after the first, the screen still lagging = %+v, want no end", CompletionGap, o)
			}
			if r := e.run1(run); r.State == store.RunEnded {
				t.Fatalf("run ended %v with its continuation still to send", r.EndedAt)
			}
			if tc.catchUp {
				e.idleAfterDeny(claudeRejected)
			}
			e.clock.Add(denyContinueGap - CompletionGap)
			o = e.observe()[RoleClaude]
			sent := len(e.continuations())
			switch {
			case tc.catchUp && (o.Kind != ObsDenyContinued || sent != 1):
				t.Fatalf("reading once the screen caught up = %+v, %d continuations; want it continued once", o, sent)
			case !tc.catchUp && (o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != run || sent != 0):
				t.Fatalf("reading %s after the first = %+v, %d continuations; want the run ended, none sent", denyContinueGap, o, sent)
			}
		})
	}
}

// The factory's readings are 10 s apart: one with a deny's continuation
// still to send ends the run on the second idle reading, as before.
func TestReadingsTenSecondsApartEndARunWithoutItsContinuationOnTheSecond(t *testing.T) {
	e := newEnv(t)
	e.started()
	run := e.denyThenIdle()
	e.idleAfterDeny(claudeRmPrompt)
	e.clock.Add(10 * time.Second)
	e.observe()
	e.clock.Add(10 * time.Second)
	if o := e.observe()[RoleClaude]; o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != run {
		t.Fatalf("second idle reading 10s after the first = %+v, want completed", o)
	}
}
