package factory

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// busyLiaisonRuntime is the fake runtime whose park for a reboot finds the
// task's liaison at work, as runtime.Runtime.ParkForReboot reports it.
type busyLiaisonRuntime struct{ *fakeRuntime }

func (r *busyLiaisonRuntime) ParkForReboot(_ context.Context, t registry.Task) error {
	return fmt.Errorf("the liaison of %s: agents: park pr %d: run r-9 is working: %w", TaskKey(t), t.ID, agents.ErrBusy)
}

// An agent at work in herdr with no run row (you typed into its pane, a
// build in a shell) refuses its park as busy: the drain never leaves such a
// task however long it lasts, and names it as work, so "safe to reboot"
// never cuts it. The same for a task's liaison at work.
func TestABusyAgentKeepsTheFactoryDrainFromSayingParked(t *testing.T) {
	for _, c := range []struct {
		name string
		busy func(h *harness)
	}{
		{"an agent", func(h *harness) {
			h.rt.mu.Lock()
			h.rt.parkErr = fmt.Errorf("agents: park pr 7: fx-writer is working: %w", agents.ErrBusy)
			h.rt.mu.Unlock()
		}},
		{"the liaison", func(h *harness) { h.e.d.Runtime = &busyLiaisonRuntime{h.rt} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.stage(config.FactoryStageAgents, idleStage)
			task := h.task("PR-12602", registry.TaskNeedsYou, "report")
			c.busy(h)
			d := h.rebootDrain(84, engine.DrainUntilBoot, factoryBoot)
			for range parkTries + 3 {
				h.tickAndWait()
			}
			ack := h.workRecord().RebootDrain
			if ack == nil || ack.Done(d.Generation, os.Getpid()) || len(ack.Left) != 0 ||
				!slices.ContainsFunc(ack.Work, func(w string) bool { return strings.Contains(w, TaskKey(task)+" at work in herdr") }) {
				t.Fatalf("acknowledgement %+v, want not parked, the busy task named as work, nothing left", ack)
			}
		})
	}
}
