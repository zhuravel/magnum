package runtime

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
)

// A reboot cuts every agent's turn in flight: the task's own roles' and its
// liaison's are listed, each with its task, and a turn that ended is not.
func TestTurnsInFlightNameTheTasksRolesAndItsLiaison(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")
	g.talk()

	turns, err := g.r.TurnsInFlight(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tu := range turns {
		if tu.TaskID != g.task.ID || tu.Since.IsZero() {
			t.Fatalf("turn %+v, want the rig's task's, with its start", tu)
		}
		who := tu.Role
		if tu.Liaison {
			who = "liaison " + who
		}
		got = append(got, who)
	}
	slices.Sort(got)
	if want := []string{"fx-repro", "liaison " + LiaisonRole}; !slices.Equal(got, want) {
		t.Fatalf("turns in flight %q, want %q", got, want)
	}

	g.endRun("fx-repro")
	turns, err = g.r.TurnsInFlight(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(turns, func(tu TurnInFlight) bool { return !tu.Liaison }) {
		t.Fatalf("turns %+v: the role whose turn ended still counts", turns)
	}
}

// A drain for a reboot holds every new prompt (Deps.MayPrompt): Send
// refuses with the hold's wait before anything reaches the ledger or the
// pane.
func TestADrainForARebootHoldsANewPrompt(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.r.d.MayPrompt = func() (string, string, bool) { return "restart", "the factory drains for a reboot", false }

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")

	w, ok := errors.AsType[*WaitError](err)
	if !ok || w.Who != "restart" || w.What != "the factory drains for a reboot" {
		t.Fatalf("Send = %v, want the drain's wait", err)
	}
	if n := len(g.h.promptTexts()); n != 0 {
		t.Fatalf("%d prompts reached the agent under the drain", n)
	}
	if n := len(g.ledger()); n != 0 {
		t.Fatalf("%d ledger entries under the drain, want none", n)
	}
}

// draining has the rig's engine hold new prompts for a drain for a reboot
// (on) or lift it.
func (g *rig) draining(on bool) {
	g.t.Helper()
	if !on {
		g.r.d.MayPrompt = nil
		return
	}
	g.r.d.MayPrompt = func() (string, string, bool) { return "restart", "the factory drains for a reboot", false }
}

// idleAfterATurn launches the role named name with conversation sid, sends
// it a prompt and ends its turn: no run in flight, as a drain finds it.
func (g *rig) idleAfterATurn(name, sid string) {
	g.t.Helper()
	g.launch(name)
	g.h.setSession(g.agent(name), sid)
	g.send(name, "reproduce the bug")
	g.observe()
	g.h.setStatus(g.agent(name), herdr.StatusIdle)
	g.observeN(2)
}

// A pane lost under a drain for a reboot is not recovered: no agent starts
// and no prompt goes, its role stays due, and the watchdog leaves it so while
// the drain holds. Once the drain is lifted the watchdog recovers it.
func TestALostPaneIsNotRecoveredUnderTheDrain(t *testing.T) {
	g := newRig(t)
	g.idleAfterATurn("fx-repro", "sid-1")
	starts, prompts := len(g.h.startCalls()), len(g.h.promptTexts())
	g.draining(true)

	g.h.losePane(g.agent("fx-repro"))
	g.observeN(2)
	for range 3 { // the watchdog's passes over the role due
		g.clock.Add(NoProgressAfter + time.Minute)
		g.watchdog()
	}

	if n := len(g.h.startCalls()); n != starts {
		t.Fatalf("%d agent starts under the drain, want none", n-starts)
	}
	if p := g.h.promptTexts()[prompts:]; len(p) != 0 {
		t.Fatalf("prompts %q under the drain, want none", p)
	}

	g.draining(false)
	for i := 0; i < 3 && len(g.h.startCalls()) == starts; i++ {
		g.clock.Add(NoProgressAfter + time.Minute)
		g.watchdog()
	}
	if s := g.h.startCalls(); len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts %+v once the drain was lifted, want the lost conversation resumed", s[starts:])
	}
}

// A stage that rebuilds a role a recovery left without a turn
// (RecoverLost) under the drain hears the drain's wait: nothing starts, and
// the role stays due.
func TestRecoverLostWaitsUnderTheDrain(t *testing.T) {
	g := newRig(t)
	g.idleAfterATurn("fx-repro", "sid-1")
	starts, prompts := len(g.h.startCalls()), len(g.h.promptTexts())
	g.draining(true)

	err := g.r.RecoverLost(g.ctx, g.task, g.attempt, g.role("fx-repro"))

	if _, ok := errors.AsType[*WaitError](err); !ok {
		t.Fatalf("RecoverLost under the drain = %v, want the drain's wait", err)
	}
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
	due := st.recovering
	g.r.mu.Unlock()
	if !due {
		t.Fatal("the role is not due its recovery after the drain's wait")
	}
}

// A Codex session that never took its first prompt is not started afresh
// under the drain; once the drain is lifted it is.
func TestAFreshStartWaitsForTheDrain(t *testing.T) {
	g := newRig(t)
	g.launch("fx-cause")
	g.send("fx-cause", "find the cause")
	g.observe()
	g.draining(true)

	g.clock.Add(2 * time.Minute)
	g.observe()
	if n := len(g.h.startCalls()); n != 1 {
		t.Fatalf("%d starts under the drain, want the launch alone", n)
	}

	g.draining(false)
	g.clock.Add(2 * time.Minute)
	g.observe()
	if n := len(g.h.startCalls()); n != 2 {
		t.Fatalf("%d starts once the drain was lifted, want the fresh start", n)
	}
}

// A turn that hit its model's limit under the drain is switched to the
// fallback model, but its continuation does not go: the role pauses, and the
// stage's prompt to go on waits for the drain like any prompt.
func TestAFallbackContinuationWaitsForTheDrain(t *testing.T) {
	g := newRig(t)
	first := g.modelLimited(func(string) error { return nil })
	g.draining(true)

	evs := g.observeN(2)

	if len(evs) != 1 || evs[0].Kind != EventPaused || evs[0].RunID != first {
		t.Fatalf("events %+v, want the limited run paused", evs)
	}
	for _, p := range g.h.promptTexts() {
		if strings.Contains(p, "switched this session to sonnet") {
			t.Fatalf("the continuation %q went under the drain", p)
		}
	}
}

// A role the watchdog is climbing with no turn in flight (its resume quit
// the hung session, then its start failed) takes no next step under the
// drain: that step would start a fresh agent and a recovery prompt after
// the acknowledgement said quiet and parked. The role stays due, and the
// step comes once the drain is lifted. The climb of a turn in flight is
// left as it is: the drain waits for that turn.
func TestAClimbWithNoTurnInFlightWaitsForTheDrain(t *testing.T) {
	g := newRig(t)
	first := "reproduce the bug"
	hung := g.hungTurn(first)
	g.h.startErr = func(herdr.AgentStartOptions) error {
		return &herdr.Error{Method: "agent.start", Code: "agent_start_failed", Message: "the agent did not come up"}
	}
	g.clock.Add(NoProgressAfter)
	g.watchdog() // the resume quits the hung session, and its start fails
	g.hungRunLeft(hung, false, "after the failed resume")
	g.h.startErr = nil
	starts, prompts := len(g.h.startCalls()), len(g.h.promptTexts())
	g.draining(true)

	for range 3 {
		g.clock.Add(NoProgressAfter)
		g.watchdog()
	}
	if n := len(g.h.startCalls()); n != starts {
		t.Fatalf("%d agent starts under the drain, want none", n-starts)
	}
	if p := g.h.promptTexts()[prompts:]; len(p) != 0 {
		t.Fatalf("prompts %q under the drain, want none", p)
	}

	g.draining(false)
	for i := 0; i < 3 && len(g.h.startCalls()) == starts; i++ {
		g.clock.Add(NoProgressAfter)
		g.watchdog()
	}
	if len(g.h.startCalls()) == starts {
		t.Fatal("the climb took no step once the drain was lifted")
	}
	if p := g.h.promptTexts(); p[len(p)-1] != first {
		t.Fatalf("the fresh session got %q, want the first prompt", p[len(p)-1])
	}
}

// A Codex account switch under the drain restarts no idle Codex session: a
// new process would start after the acknowledgement said quiet and parked,
// and the reboot restarts them anyway. Once the drain is lifted, they are
// restarted on the new login.
func TestAnAccountSwitchUnderTheDrainRestartsNoSessionUntilItsLift(t *testing.T) {
	g := newRig(t)
	g.idleAfterATurn("fx-cause", "11111111-1111-4111-8111-111111111111")
	starts := len(g.h.startCalls())
	g.draining(true)

	g.setLogin("acct-2")
	g.clock.Add(time.Minute)
	g.observe()
	g.clock.Add(time.Minute)
	g.observe()
	if n := len(g.h.startCalls()); n != starts {
		t.Fatalf("%d starts under the drain after a login change, want none", n-starts)
	}

	g.draining(false)
	g.clock.Add(time.Minute)
	g.observe()
	s := g.h.startCalls()
	if len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "resume 11111111-1111-4111-8111-111111111111") {
		t.Fatalf("starts %+v once the drain was lifted, want fx-cause resumed in a new process", s[starts:])
	}
}
