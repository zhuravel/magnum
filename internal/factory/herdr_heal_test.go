package factory

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
)

// deniedLine is the line a task waits on you for once macOS still blocks
// the factory's herdr server from reading the copies' volume after its heal.
const deniedLine = "macOS blocks the factory's herdr server from reading /Volumes/Work: allow herdr and magnum (and iTerm2, which " +
	"starts the server) in System Settings > Privacy & Security > Files and Folders > Removable Volumes (or Full Disk Access, if " +
	"that toggle is absent), then retry"

// A prompt the runtime answers with the denial's line (the heal did not
// help) sends the task to you with that line, never a silent wait, and your
// retry is accepted: the task goes on, and the role's prompt goes again.
func TestADeniedVolumeSendsTheTaskToYouAndYourRetryGoesOn(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12530", "bug", "investigate")
	h.rt.setSendErr(fmt.Errorf("send to fx-repro: %w", &runtime.DeniedError{Volume: "/Volumes/Work", App: "iTerm2"}))
	h.startStage(task)
	if got := h.reload(task.ID); got.State != registry.TaskNeedsYou || got.WaitsWho != "you" || got.WaitsWhat != deniedLine {
		t.Fatalf("task %s waits %q: %q, want needs_you, you: %s", got.State, got.WaitsWho, got.WaitsWhat, deniedLine)
	}
	if evs := h.events(task.ID, "advance"); len(evs) != 0 {
		t.Fatalf("advance failures %+v, want none", evs)
	}

	h.rt.setSendErr(nil) // you allowed herdr
	if q := h.retry(task.ID); q.State != registry.RequestDone {
		t.Fatalf("retry %s: %s, want it accepted", q.State, q.Result)
	}
	h.tick()
	if got := h.reload(task.ID); got.State != registry.TaskActive {
		t.Fatalf("task %s after your retry, want active", got.State)
	}
	if len(h.rt.promptsTo("fx-repro")) == 0 {
		t.Fatal("no prompt to fx-repro after your retry")
	}
}

// The observe pass's word of the same (a pane lost before its first
// prompt, the heal spent) sends the task to you with the line.
func TestADeniedVolumeEventSendsTheTaskToYou(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12531", "bug", "investigate")
	h.startStage(task)
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventVolumeDenied, Reason: deniedLine})
	if !h.kicked() {
		t.Fatal("the denial asked for no tick")
	}
	h.tick()
	if got := h.reload(task.ID); got.State != registry.TaskNeedsYou || got.WaitsWhat != deniedLine {
		t.Fatalf("task %s waits %q, want needs_you: %s", got.State, got.WaitsWhat, deniedLine)
	}
}

// A role whose pane was lost before its first prompt and whose recoveries
// ran out sends the task to you (EventLost), so your retry is accepted, and
// the stage prompts the role again after it: the runtime brings a role with
// no session back on that prompt.
func TestALostRoleGoesToYouAndYourRetryPromptsItAgain(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12532", "bug", "investigate")
	h.rt.setSendErr(fmt.Errorf("send to fx-repro: %w", agents.ErrNoSession)) // the agents exited at their start
	h.startStage(task)
	if got := h.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho != waitAgent {
		t.Fatalf("task %s waits %q: %q, want an agent's wait", got.State, got.WaitsWho, got.WaitsWhat)
	}
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventLost, Reason: "no progress after 1 recoveries"})
	h.tick()
	want := "the fx-repro agent's pane was lost: no progress after 1 recoveries"
	if got := h.reload(task.ID); got.State != registry.TaskNeedsYou || got.WaitsWho != "you" || got.WaitsWhat != want {
		t.Fatalf("task %s waits %q: %q, want needs_you, you: %s", got.State, got.WaitsWho, got.WaitsWhat, want)
	}

	h.rt.setSendErr(nil)
	if q := h.retry(task.ID); q.State != registry.RequestDone {
		t.Fatalf("retry %s: %s, want it accepted", q.State, q.Result)
	}
	h.tick()
	if len(h.rt.promptsTo("fx-repro")) == 0 {
		t.Fatal("no prompt to fx-repro after your retry: the lost role stays silent")
	}
}

// A heal's stop of the factory's herdr server has the engine open the
// session again at once, whenever it opened it last: the roles are
// relaunched in the fresh server it starts.
func TestAHealsServerStopOpensTheSessionAtOnce(t *testing.T) {
	var up atomic.Bool
	var reveals atomic.Int32
	up.Store(true)
	h := newHarness(t, func(d *Deps) {
		d.HerdrUp = func(context.Context) error {
			if up.Load() {
				return nil
			}
			return errors.New("herdr session.snapshot: herdr unavailable: no such file or directory")
		}
		d.Reveal = func(context.Context) error { reveals.Add(1); return nil }
	})
	task := h.task("PR-12533", registry.TaskActive, "investigate")
	h.stage(config.FactoryStageAgents, idleStage)
	up.Store(false)
	h.tick() // herdr went down: its first reveal
	if n := reveals.Load(); n != 1 {
		t.Fatalf("%d reveals once herdr went down, want 1", n)
	}
	up.Store(true)
	h.tick()

	h.kicked()
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventHerdrStopped, Reason: "/Volumes/Work"})
	if !h.kicked() {
		t.Fatal("the stop asked for no tick")
	}
	up.Store(false)
	h.tick()
	if n := reveals.Load(); n != 2 {
		t.Fatalf("%d reveals after the heal's stop, want one more at once", n)
	}
}

// A prompt the daemon's drain held (the runtime would start a role again
// first, and no launch starts while the daemon drains) costs the task
// nothing: no failure, and no wait of the drain's on the task.
func TestAPromptTheDrainHoldsCostsTheTaskNothing(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12534", "bug", "investigate")
	h.rt.setSendErr(fmt.Errorf("send to fx-repro: %w", runtime.ErrDraining))
	h.startStage(task)
	h.tick()
	if got := h.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho == holdRestart {
		t.Fatalf("task %s waits %q: %q, want active with no wait of the drain's", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if evs := h.events(task.ID, "advance"); len(evs) != 0 {
		t.Fatalf("advance failures %+v, want none", evs)
	}
}

// The runtime asks the engine's holds before it starts a role outside a
// launch (Deps.MayStart): MayLaunch says the hold, in its words.
func TestMayLaunchSaysTheHoldThatStopsAStart(t *testing.T) {
	h := newHarness(t)
	if who, _, ok := h.e.MayLaunch(); !ok || who != "" {
		t.Fatalf("MayLaunch with no hold = %q, %v; want ok", who, ok)
	}
	h.e.holds[HoldMemory] = &Hold{Kind: HoldMemory, Reason: "the Mac's memory pressure is critical"}
	if who, what, ok := h.e.MayLaunch(); ok || who != HoldMemory || what != "the Mac's memory pressure is critical" {
		t.Fatalf("MayLaunch under the memory hold = %q, %q, %v", who, what, ok)
	}
}
