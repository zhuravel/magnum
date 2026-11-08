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

// A reading neither idle nor busy (herdr's "unknown", or no status at all)
// keeps the streak, but it is not the reading the gap waits for: it names no
// second reading, whose time has passed, or the observe clock would read
// herdr every second for as long as herdr cannot tell the agent's status.
func TestAReadingNeitherIdleNorBusyNamesNoSecondReading(t *testing.T) {
	e := newEnv(t)
	id := e.judgeAtWork()

	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
	e.clock.Add(time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != "" || o.SecondReadingAt.IsZero() {
		t.Fatalf("first idle reading = %+v, want the second reading due", o)
	}
	e.h.setAgentStatus(judgeAgent, herdr.StatusUnknown)
	for i := range 3 {
		e.clock.Add(5 * time.Second)
		if o := e.observe()[RoleJudge]; o.Kind != "" || !o.SecondReadingAt.IsZero() {
			t.Fatalf("unknown reading %d = kind %q, second reading due %v; want neither", i+1, o.Kind, o.SecondReadingAt)
		}
	}
	if r := e.run1(id); r.State != store.RunWorking {
		t.Fatalf("run after unknown readings = %s, want working", r.State)
	}
}

// A prompt begins a new turn: Submit sets idle_ticks to 0, so the next idle
// reading begins a new streak even when herdr showed the agent idle all
// along (a short turn, or herdr's status behind the prompt), and the gap
// counts from it, not from the last turn's streak.
func TestAPromptBeginsANewIdleStreak(t *testing.T) {
	e := newEnv(t)
	first := e.judgeAtWork()
	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
	e.clock.Add(10 * time.Second)
	e.observe()
	e.clock.Add(10 * time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != first {
		t.Fatalf("second idle reading of the first turn = %+v, want completed", o)
	}
	for range 2 { // idle between the turns: the old streak goes on
		e.clock.Add(10 * time.Second)
		e.observe()
	}

	e.clock.Add(time.Minute)
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunRereview, "review again")
	if err != nil {
		t.Fatal(err)
	}
	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle) // no working reading after the prompt
	e.clock.Add(time.Second)
	start := e.clock.Now()
	if o := e.observe()[RoleJudge]; o.Kind != "" || o.Session.IdleTicks != 1 {
		t.Fatalf("first idle reading after the prompt = %+v, want the streak begun", o)
	}
	e.clock.Add(time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != "" {
		t.Fatalf("second idle reading 1s after the new streak began = %+v, want no end", o)
	}
	if r := e.run1(id); r.State == store.RunEnded {
		t.Fatalf("run %s ended %v, 2s after the prompt", id, r.EndedAt)
	}
	e.clock.Add(CompletionGap - time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
		t.Fatalf("reading %s after the new streak began = %+v, want completed", CompletionGap, o)
	}
	if r := e.run1(id); r.EndedAt == nil || !r.EndedAt.Equal(start.Add(CompletionGap)) {
		t.Fatalf("run ended %v, want %v", r.EndedAt, start.Add(CompletionGap))
	}
}

// The observe clock reads herdr every few seconds to see the turns in flight
// end. A pass over every live session would also write each one's row (its
// idle count) every few seconds, most of them agents with nothing to do:
// ObserveRunsAt observes the sessions with a run submitted or at work, and
// leaves the others' rows to the tick's pass.
func TestObservingTheRunsInFlightLeavesTheOtherSessionsAlone(t *testing.T) {
	e := newEnv(t)
	id := e.judgeAtWork() // the judge's run is at work; claude-review has none
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
	before := e.session(RoleClaude)

	e.clock.Add(5 * time.Second)
	snap, err := e.h.Snapshot(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := e.m.ObserveRunsAt(e.ctx, snap, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Role != RoleJudge || obs[0].Run == nil || obs[0].Run.ID != id || obs[0].Session.IdleTicks != 1 {
		t.Fatalf("observations = %+v, want the judge's alone, its first idle reading", obs)
	}
	after := e.session(RoleClaude)
	if after.IdleTicks != before.IdleTicks || store.Deref(after.AgentStatus) != store.Deref(before.AgentStatus) {
		t.Fatalf("claude-review, no run in flight: idle_ticks %d -> %d, status %q -> %q; want its row untouched",
			before.IdleTicks, after.IdleTicks, store.Deref(before.AgentStatus), store.Deref(after.AgentStatus))
	}
	if o, ok := e.observe()[RoleClaude]; !ok || o.Session.IdleTicks != before.IdleTicks+1 {
		t.Fatalf("the tick's pass: claude-review = %+v (observed %v), want its idle reading counted", o, ok)
	}
}
