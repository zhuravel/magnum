package factory

import (
	"context"
	"errors"
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
	// own, when set, is how the park of the task's own agents fails, beside
	// the liaison left live (runtime.Runtime.park joins them).
	own error
}

func (r *paneLiaisonRuntime) ParkForReboot(_ context.Context, t registry.Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.working {
		return errors.Join(r.own, fmt.Errorf("the liaison of %s is working and you are in its pane: %w", TaskKey(t), agents.ErrBusy))
	}
	return errors.Join(r.own, fmt.Errorf("the liaison of %s: %w", TaskKey(t), runtime.ErrLiaisonLeftLive))
}

// A task whose own agents fail to park, for another cause than work, while
// you are in its idle liaison's pane is left after parkTries ticks, with
// why, and its liaison is still named as left live and looked at again at
// every tick: once you sent it another message the drain is no longer safe.
func TestATaskLeftWithItsLiaisonLeftLiveIsStillCheckedAtEveryTick(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	task := h.task("PR-12621", registry.TaskNeedsYou, "report")
	rt := &paneLiaisonRuntime{fakeRuntime: h.rt, own: errors.New("agents: park pr 1: close workspace w1: herdr: the workspace does not close")}
	h.e.d.Runtime = rt
	d := h.rebootDrain(91, engine.DrainUntilBoot, factoryBoot)

	for range parkTries {
		h.tickAndWait()
	}
	ack := h.workRecord().RebootDrain
	if ack == nil || !ack.Done(d.Generation, os.Getpid()) ||
		!slices.ContainsFunc(ack.Left, func(l string) bool {
			return strings.HasPrefix(l, TaskKey(task)+": its agents could not be parked (") && strings.Contains(l, "does not close")
		}) ||
		!slices.ContainsFunc(ack.Left, func(l string) bool { return strings.Contains(l, TaskKey(task)+": your liaison pane, left live") }) {
		t.Fatalf("acknowledgement %+v after %d failed parks, want the task left with why and its liaison named as left live", ack, parkTries)
	}

	rt.mu.Lock()
	rt.working = true // you typed another message
	rt.mu.Unlock()
	h.tickAndWait()
	if ack := h.workRecord().RebootDrain; ack == nil || ack.Done(d.Generation, os.Getpid()) ||
		!slices.ContainsFunc(ack.Work, func(w string) bool { return strings.Contains(w, TaskKey(task)+" at work in herdr") }) {
		t.Fatalf("acknowledgement %+v once the liaison of the left task works on your message, want it not certified, naming it", ack)
	}
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
