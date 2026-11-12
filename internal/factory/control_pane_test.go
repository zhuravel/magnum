package factory

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
)

// paneLiaisonRuntime is the fake runtime whose park for a reboot finds you
// in the task's liaison pane: idle (left live) until working is set, then
// at work on what you typed.
type paneLiaisonRuntime struct {
	*fakeRuntime
	mu      sync.Mutex
	working bool
}

func (r *paneLiaisonRuntime) ParkForReboot(_ context.Context, t registry.Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.working {
		return fmt.Errorf("the liaison of %s is working and you are in its pane: %w", TaskKey(t), agents.ErrBusy)
	}
	return fmt.Errorf("the liaison of %s: %w", TaskKey(t), runtime.ErrLiaisonLeftLive)
}

// A liaison left live because you are in its pane is never counted as
// parked: each tick looks at it again, the acknowledgement names it while it
// is idle, and once you sent it another message (a turn with no run row)
// the drain is no longer safe.
func TestALiaisonLeftLiveWithYouInItsPaneIsCheckedAtEveryTick(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	task := h.task("PR-12620", registry.TaskNeedsYou, "report")
	rt := &paneLiaisonRuntime{fakeRuntime: h.rt}
	h.e.d.Runtime = rt
	d := h.rebootDrain(90, engine.DrainUntilBoot, factoryBoot)

	h.tickAndWait()
	ack := h.workRecord().RebootDrain
	if ack == nil || !ack.Done(d.Generation, os.Getpid()) ||
		!slices.ContainsFunc(ack.Left, func(l string) bool { return strings.Contains(l, TaskKey(task)+": your liaison pane, left live") }) {
		t.Fatalf("acknowledgement %+v with you in the idle liaison's pane, want parked with the pane named as left live", ack)
	}

	rt.mu.Lock()
	rt.working = true // you typed another message
	rt.mu.Unlock()
	h.tickAndWait()
	if ack := h.workRecord().RebootDrain; ack == nil || ack.Done(d.Generation, os.Getpid()) ||
		!slices.ContainsFunc(ack.Work, func(w string) bool { return strings.Contains(w, TaskKey(task)+" at work in herdr") }) {
		t.Fatalf("acknowledgement %+v once the liaison works on your message, want it not certified, naming it", ack)
	}
}
