package agents

import (
	"os"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

const turnJudge = "mg-11920-codex-judge-5d01cf"

// codexTurn starts the env's agents with a Codex home of their own, herdr
// reporting the judge's session id when withID, and prompts the judge: its
// run, which one tick sees working.
func (e *env) codexTurn(withID bool) (home string, run store.Run) {
	e.t.Helper()
	home = e.t.TempDir()
	e.m.d.CodexHome = home
	e.started()
	if withID {
		e.h.setAgentSession(turnJudge, codexSID)
	}
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review")
	if err != nil {
		e.t.Fatal(err)
	}
	e.h.setAgentStatus(turnJudge, herdr.StatusWorking)
	e.clock.Add(10 * time.Second)
	e.observe()
	return home, e.run1(id)
}

// rolloutAt writes the judge's rollout with lines and sets its last write
// to at.
func (e *env) rolloutAt(home string, at time.Time, lines ...map[string]any) {
	e.t.Helper()
	codexRollout(e.t, home, t0.Add(-time.Hour), lines...)
	path, err := rolloutIn(home, codexSID)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		e.t.Fatal(err)
	}
}

func agentMessage(at time.Time) map[string]any {
	return map[string]any{"timestamp": at.UTC().Format("2006-01-02T15:04:05.000Z"), "type": "event_msg",
		"payload": map[string]any{"type": "agent_message", "message": "Checking the retry path."}}
}

// codexIdleTicks reports herdr's agent idle on n ticks 10 s apart, and returns
// the tick (1-based) whose observation ended the run, 0 for none.
func (e *env) codexIdleTicks(n int, each func()) int {
	e.t.Helper()
	e.h.setAgentStatus(turnJudge, herdr.StatusIdle)
	for i := 1; i <= n; i++ {
		e.clock.Add(10 * time.Second)
		if each != nil {
			each()
		}
		if o := e.observe()[RoleJudge]; o.Kind == ObsCompleted {
			return i
		}
	}
	return 0
}

// On 2026-10-22 herdr showed judges idle while Codex still worked, and two
// idle ticks ended their turns. A Codex turn whose rollout has no
// task_complete since its prompt (an earlier turn's does not count) and
// grew 30 s ago is still working: the run stays open on idle ticks, which
// is logged once, and ends on the usual two idle ticks once the
// task_complete lands.
func TestAnIdleCodexTurnWhoseRolloutStillGrowsStaysOpen(t *testing.T) {
	e := newEnv(t)
	home, run := e.codexTurn(true)
	grow := func() {
		now := e.clock.Now()
		e.rolloutAt(home, now.Add(-30*time.Second), taskComplete(run.CreatedAt.Add(-time.Hour), "", ""),
			agentMessage(now.Add(-30*time.Second)))
	}
	if tick := e.codexIdleTicks(4, grow); tick != 0 {
		t.Fatalf("the run ended on idle tick %d while its rollout grew", tick)
	}
	if got := e.run1(run.ID).State; got != store.RunWorking {
		t.Fatalf("run state = %s, want working", got)
	}
	if recs := e.logs.records("but its Codex rollout has no task_complete since run " + run.ID); len(recs) != 1 {
		t.Fatalf("logs = %q, want the hold said once", e.logs.all())
	}
	done := func() {
		now := e.clock.Now()
		e.rolloutAt(home, now, agentMessage(now.Add(-20*time.Second)), taskComplete(now, "", ""))
	}
	if tick := e.codexIdleTicks(3, done); tick != CompletionIdleTicks {
		t.Fatalf("the run ended on idle tick %d after its task_complete, want %d", tick, CompletionIdleTicks)
	}
}

// A turn whose rollout holds its task_complete (or turn_aborted) ends on
// herdr's idle with no added tick, and so does one with no Codex session
// id, no rollout, a rollout it cannot read or a rollout that stopped
// growing: herdr's word as before.
func TestAnIdleCodexTurnEndsAsBeforeWhenItsRolloutSaysNothingElse(t *testing.T) {
	cases := []struct {
		name   string
		withID bool
		write  func(e *env, home string, run store.Run)
	}{
		{"task_complete since the prompt", true, func(e *env, home string, run store.Run) {
			e.rolloutAt(home, e.clock.Now(), agentMessage(run.CreatedAt.Add(time.Second)), taskComplete(run.CreatedAt.Add(5*time.Second), "", ""))
		}},
		{"turn_aborted since the prompt", true, func(e *env, home string, run store.Run) {
			e.rolloutAt(home, e.clock.Now(), agentMessage(run.CreatedAt.Add(time.Second)), map[string]any{
				"timestamp": run.CreatedAt.Add(5 * time.Second).UTC().Format("2006-01-02T15:04:05.000Z"), "type": "event_msg",
				"payload": map[string]any{"type": "turn_aborted", "reason": "interrupted"}})
		}},
		{"no session id", false, func(e *env, home string, run store.Run) {
			e.rolloutAt(home, e.clock.Now(), agentMessage(e.clock.Now()))
		}},
		{"no rollout", true, func(*env, string, store.Run) {}},
		{"an unreadable rollout", true, func(e *env, home string, run store.Run) {
			e.rolloutAt(home, e.clock.Now(), agentMessage(e.clock.Now()))
			path, err := rolloutIn(home, codexSID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil { // found, but no file to read
				t.Fatal(err)
			}
			if err := os.Chtimes(path, e.clock.Now(), e.clock.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{"a rollout last written 2 minutes ago", true, func(e *env, home string, run store.Run) {
			e.rolloutAt(home, e.clock.Now().Add(-2*time.Minute), agentMessage(run.CreatedAt.Add(time.Second)))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			home, run := e.codexTurn(c.withID)
			c.write(e, home, run)
			if tick := e.codexIdleTicks(4, nil); tick != CompletionIdleTicks {
				t.Fatalf("the run ended on idle tick %d, want %d", tick, CompletionIdleTicks)
			}
		})
	}
}
