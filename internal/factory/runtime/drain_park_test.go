package runtime

import (
	"errors"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
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
