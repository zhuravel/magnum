package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
)

// The park a drain for a reboot asks for reports a liaison at work (its turn
// in flight) as busy, where the task's own Park leaves it live and goes
// through: a reboot would cut that turn, so the drain must see it.
func TestParkForRebootReportsALiaisonAtWork(t *testing.T) {
	g := newRig(t)
	g.talk() // its turn in flight

	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park with the liaison at work: %v (the task's park goes through)", err)
	}
	err := g.r.ParkForReboot(g.ctx, g.task)
	if !errors.Is(err, agents.ErrBusy) {
		t.Fatalf("ParkForReboot with the liaison at work = %v, want it busy", err)
	}
	if !g.liaisonLive() {
		t.Fatal("the park parked the liaison at work")
	}

	g.endLiaisonTurn()
	g.clock.Add(g.cfg.Factory.HumanCooldown.Duration + 1)
	if err := g.r.ParkForReboot(g.ctx, g.task); err != nil {
		t.Fatalf("ParkForReboot once the liaison's turn ended = %v, want it parked", err)
	}
	if g.liaisonLive() {
		t.Fatal("the idle liaison is still live")
	}
}

// You in the liaison's pane, its agent at work on what you typed (a turn
// with no run row): the park for a reboot reports it busy, since the reboot
// would cut that turn; idle, with you still in the pane, it stays live and
// the park goes through.
func TestParkForRebootReportsALiaisonAtWorkWhileYouAreInItsPane(t *testing.T) {
	g := newRig(t)
	g.talk()
	g.endLiaisonTurn()
	g.clock.Add(2 * time.Minute) // past a start's grace: herdr showing the agent at work is you
	g.h.focus(g.liaisonAgent())  // you in its pane, typing
	g.h.setStatus(g.liaisonAgent(), herdr.StatusWorking)
	g.observe()
	if at := g.liaisonRowOf().HumanActiveAt; at == nil {
		t.Fatal("herdr did not show you in the liaison's pane")
	}

	err := g.r.ParkForReboot(g.ctx, g.task)
	if !errors.Is(err, agents.ErrBusy) || !strings.Contains(err.Error(), "you are in its pane") {
		t.Fatalf("ParkForReboot with you in the liaison's pane and it at work = %v, want it busy", err)
	}
	if !g.liaisonLive() {
		t.Fatal("the park parked the liaison at work")
	}

	g.h.setStatus(g.liaisonAgent(), herdr.StatusIdle)
	if err := g.r.ParkForReboot(g.ctx, g.task); err != nil {
		t.Fatalf("ParkForReboot with you in the idle liaison's pane = %v, want it to go through", err)
	}
	if !g.liaisonLive() {
		t.Fatal("the park parked the liaison you are in")
	}
}
