package runtime

import (
	"errors"
	"slices"
	"testing"
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
