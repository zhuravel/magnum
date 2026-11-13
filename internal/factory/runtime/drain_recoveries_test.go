package runtime

import (
	"errors"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/herdr"
)

// A waited recovery of a lost pane whose wait ends under a drain for a
// reboot, with no turn of the role in flight, starts nothing: the role stays
// due, and the watchdog recovers it once the drain is lifted.
func TestAWaitedRecoveryWithNoTurnInFlightStaysDueUnderTheDrain(t *testing.T) {
	g := newRig(t)
	g.idleAfterATurn("fx-repro", "sid-1")
	g.h.losePane(g.agent("fx-repro"))
	g.observe() // recovery 1, at once: a resume and its recovery turn
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.observeN(2) // the recovery turn ends
	g.h.losePane(g.agent("fx-repro"))
	g.observe() // recovery 2 waits its minute
	if turns, err := g.r.TurnsInFlight(g.ctx); err != nil || len(turns) != 0 {
		t.Fatalf("turns in flight %+v, %v; want none", turns, err)
	}
	starts, prompts := len(g.h.startCalls()), len(g.h.promptTexts())
	g.draining(true)

	g.afterWait(2)
	g.afterWait(2)

	if n := len(g.h.startCalls()); n != starts {
		t.Fatalf("%d agent starts under the drain, want none", n-starts)
	}
	if p := g.h.promptTexts()[prompts:]; len(p) != 0 {
		t.Fatalf("prompts %q under the drain, want none", p)
	}
	st, err := g.r.stateFor(g.ctx, g.attempt.ID, "fx-repro")
	if err != nil {
		t.Fatal(err)
	}
	g.r.mu.Lock()
	due := st.recovering && !st.dueAt.IsZero()
	g.r.mu.Unlock()
	if !due {
		t.Fatal("the role is not due its waited recovery under the drain")
	}

	g.draining(false)
	g.watchdog()
	if n := len(g.h.startCalls()); n != starts+1 {
		t.Fatalf("%d starts once the drain was lifted, want the waited recovery", n-starts)
	}
}

// The fresh session your answer or the writer's fix budget asks for
// (SendFresh) waits for a drain for a reboot: the drain's wait comes back,
// no agent starts, no prompt goes and the role's recoveries do not start
// over. Once the drain is lifted it starts the role afresh.
func TestASendFreshWaitsForTheDrain(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	g.loseAll("fx-repro")
	starts, prompts := len(g.h.startCalls()), len(g.h.promptTexts())
	overs := len(g.recoverEvents(startOverWord))
	g.draining(true)

	id, err := g.r.SendFresh(g.ctx, g.task, g.attempt, g.role("fx-repro"), "look at the seed data first", true)

	if _, ok := errors.AsType[*WaitError](err); !ok || id != "" {
		t.Fatalf("SendFresh under the drain = %q, %v; want the drain's wait", id, err)
	}
	if n := len(g.h.startCalls()); n != starts {
		t.Fatalf("%d agent starts under the drain, want none", n-starts)
	}
	if p := g.h.promptTexts()[prompts:]; len(p) != 0 {
		t.Fatalf("prompts %q under the drain, want none", p)
	}
	if n := len(g.recoverEvents(startOverWord)); n != overs {
		t.Fatalf("%d start-overs under the drain, want none", n-overs)
	}

	g.draining(false)
	if id, err := g.r.SendFresh(g.ctx, g.task, g.attempt, g.role("fx-repro"), "look at the seed data first", true); err != nil || id == "" {
		t.Fatalf("SendFresh once the drain was lifted = %q, %v", id, err)
	}
	if n := len(g.h.startCalls()); n != starts+1 {
		t.Fatalf("%d starts once the drain was lifted, want the fresh session", n-starts)
	}
}

// Your answer's start of a role lost before its first prompt (StartOver)
// waits for a drain for a reboot: the drain's wait comes back and the role
// stays lost, its recoveries spent, until the drain is lifted.
func TestStartOverWaitsForTheDrain(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	g.loseAll("fx-repro")
	overs := len(g.recoverEvents(startOverWord))
	g.draining(true)

	err := g.r.StartOver(g.ctx, g.task, g.attempt, g.role("fx-repro"), "your answer to its lost pane's question")

	if _, ok := errors.AsType[*WaitError](err); !ok {
		t.Fatalf("StartOver under the drain = %v, want the drain's wait", err)
	}
	st, serr := g.r.stateFor(g.ctx, g.attempt.ID, "fx-repro")
	if serr != nil {
		t.Fatal(serr)
	}
	g.r.mu.Lock()
	lost := st.lost
	g.r.mu.Unlock()
	if n := len(g.recoverEvents(startOverWord)); !lost || n != overs {
		t.Fatalf("lost %v, %d start-overs under the drain; want the role lost, none", lost, n-overs)
	}

	g.draining(false)
	if err := g.r.StartOver(g.ctx, g.task, g.attempt, g.role("fx-repro"), "your answer to its lost pane's question"); err != nil {
		t.Fatalf("StartOver once the drain was lifted = %v", err)
	}
	if n := len(g.recoverEvents(startOverWord)); n != overs+1 {
		t.Fatalf("%d start-overs once the drain was lifted, want one", n-overs)
	}
}

// recoverEvents are the task's recover events whose text holds word.
func (g *rig) recoverEvents(word string) []string {
	g.t.Helper()
	evs, err := g.reg.TaskEvents(g.ctx, g.task.ID, recoverKind, 0)
	if err != nil {
		g.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if strings.Contains(ev.Text, word) {
			out = append(out, ev.Text)
		}
	}
	return out
}
