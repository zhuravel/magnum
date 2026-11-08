package agents

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// judgeAtWork prompts the judge and observes it working once.
func (e *env) judgeAtWork() string {
	e.t.Helper()
	e.started()
	e.clock.Add(3 * time.Minute)
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review")
	if err != nil {
		e.t.Fatal(err)
	}
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleJudge]; o.Status != herdr.StatusWorking || o.Kind != "" {
		e.t.Fatalf("working reading = %+v", o)
	}
	return id
}

// The review daemon observes every few seconds now, so two idle readings in
// a row could be a second apart, and a status that flickers idle for a
// moment would end the turn. The second reading counts only CompletionGap
// after the first, and the observation says when it is due.
func TestASecondIdleReadingCloserThanTheGapDoesNotEndTheTurn(t *testing.T) {
	e := newEnv(t)
	id := e.judgeAtWork()

	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
	e.clock.Add(time.Second)
	first := e.clock.Now()
	o := e.observe()[RoleJudge]
	if o.Kind != "" || o.Session.IdleTicks != 1 {
		t.Fatalf("first idle reading = %+v", o)
	}
	if want := first.Add(CompletionGap); !o.SecondReadingAt.Equal(want) {
		t.Fatalf("first idle reading: second reading due %v, want %v", o.SecondReadingAt, want)
	}

	e.clock.Add(CompletionGap - time.Second)
	o = e.observe()[RoleJudge]
	if o.Kind != "" || o.Session.IdleTicks < CompletionIdleTicks {
		t.Fatalf("second idle reading %s after the first = %+v, want no end", CompletionGap-time.Second, o)
	}
	if r := e.run1(id); r.State != store.RunWorking || r.EndedAt != nil {
		t.Fatalf("run after a reading closer than the gap = %+v, want working", r)
	}
	if want := first.Add(CompletionGap); !o.SecondReadingAt.Equal(want) {
		t.Fatalf("reading closer than the gap: second reading due %v, want %v", o.SecondReadingAt, want)
	}

	e.clock.Add(time.Second)
	o = e.observe()[RoleJudge]
	if o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
		t.Fatalf("reading %s after the first = %+v, want completed", CompletionGap, o)
	}
	if r := e.run1(id); r.State != store.RunEnded || r.EndedAt == nil || !r.EndedAt.Equal(first.Add(CompletionGap)) {
		t.Fatalf("run = %+v, want ended at %v", r, first.Add(CompletionGap))
	}
	if !o.SecondReadingAt.IsZero() {
		t.Fatalf("completed reading: second reading due %v, want none", o.SecondReadingAt)
	}
}

// A flicker back to working begins the streak again: the gap counts from
// the first idle reading after it.
func TestAWorkingReadingBeginsTheIdleGapAgain(t *testing.T) {
	e := newEnv(t)
	id := e.judgeAtWork()

	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
	e.clock.Add(time.Second)
	e.observe()
	e.h.setAgentStatus(judgeAgent, herdr.StatusWorking)
	e.clock.Add(time.Second)
	e.observe()
	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
	e.clock.Add(time.Second)
	e.observe() // the streak's first reading again
	e.clock.Add(2 * time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != "" {
		t.Fatalf("second reading 2s after the streak began again (4s after the first idle one) = %+v, want no end", o)
	}
	e.clock.Add(time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
		t.Fatalf("reading %s after the streak began again = %+v, want completed", CompletionGap, o)
	}
}

// The factory's readings are 10 s apart and a restart of either daemon
// keeps the session's idle count: the first reading of a fresh manager ends
// a turn whose first idle reading an earlier one took long enough before,
// as it did before the gap.
func TestAnIdleStreakFromBeforeARestartKeepsItsFirstReading(t *testing.T) {
	e := newEnv(t)
	id := e.judgeAtWork()

	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
	e.clock.Add(10 * time.Second)
	if o := e.observe()[RoleJudge]; o.Session.IdleTicks != 1 {
		t.Fatalf("first idle reading = %+v", o)
	}
	e.restartDaemon()
	e.clock.Add(10 * time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
		t.Fatalf("first reading after the restart, 10s after the first idle one = %+v, want completed", o)
	}
}

// Readings 30 s apart, the factory's and the tick's, end a turn on the
// second idle reading as before: the gap is always met.
func TestReadingsFartherApartThanTheGapEndATurnOnTheSecond(t *testing.T) {
	e := newEnv(t)
	id := e.judgeAtWork()
	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
	e.clock.Add(10 * time.Second)
	e.observe()
	e.clock.Add(10 * time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
		t.Fatalf("second idle reading 10s after the first = %+v, want completed", o)
	}
}
