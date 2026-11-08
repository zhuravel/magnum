package agents

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
)

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A waiter takes Ended before it reads its run: the observe pass that ends
// the run closes that channel, also when the waiter selects on it only
// afterwards, so no end is lost between the read and the wait.
func TestARunEndClosesTheChannelTakenBeforeIt(t *testing.T) {
	e := newEnv(t)
	e.judgeAtWork()
	before := e.m.Ended()

	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
	e.clock.Add(time.Second)
	e.observe()
	if closed(before) {
		t.Fatal("the first idle reading closed the channel; no run ended")
	}
	e.clock.Add(CompletionGap)
	if o := e.observe()[RoleJudge]; o.Kind != ObsCompleted {
		t.Fatalf("second idle reading = %+v, want completed", o)
	}
	if !closed(before) {
		t.Fatal("the run ended and the channel taken before it is still open")
	}
	if after := e.m.Ended(); closed(after) {
		t.Fatal("the channel taken after the end is closed: the next end would wake nobody")
	}
}

// A waiter returns when its run's session is lost, so a lost session wakes
// it too.
func TestALostSessionClosesTheChannelTakenBeforeIt(t *testing.T) {
	e := newEnv(t)
	e.judgeAtWork()
	before := e.m.Ended()
	e.h.removeAgent(judgeAgent)
	e.clock.Add(LostGrace + time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != ObsLost {
		t.Fatalf("reading without the agent = %+v, want lost", o)
	}
	if !closed(before) {
		t.Fatal("the session was lost and the channel taken before it is still open")
	}
}

// Readings that end nothing close nothing: waiters sleep their poll.
func TestReadingsThatEndNothingCloseNothing(t *testing.T) {
	e := newEnv(t)
	e.judgeAtWork()
	ch := e.m.Ended()
	for range 3 {
		e.clock.Add(10 * time.Second)
		e.observe()
	}
	if closed(ch) {
		t.Fatal("working readings closed the channel")
	}
}
