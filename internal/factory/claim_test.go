package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/slots"
)

// A claim can run a copy's setup for most of an hour, and the loop is
// sequential: the claim runs off the loop, and the task waits, at no cost,
// "copy: preparing <copy> (setup <elapsed>)" until a later tick picks the
// copy up. The other tasks advance meanwhile.
func TestAClaimRunsOffTheLoopAndTheTaskWaitsWhileItPrepares(t *testing.T) {
	h := newHarness(t)
	h.copies.block = make(chan struct{})
	h.copies.claimed = Copy{Name: "factory2", Path: "/copies/factory2", Number: 2}
	task := h.task("PR-12331", registry.TaskActive, "investigate")
	other := h.task("PR-12332", registry.TaskActive, "investigate")
	var got Copy
	var claimErr error
	var seen calls
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		seen.add(t.ID, h.clock.Now())
		if t.ID != task.ID {
			return false, nil
		}
		got, claimErr = h.e.claimCopy(ctx, t)
		return false, claimErr
	})

	done := make(chan struct{})
	go func() { h.tick(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick waited for the claim")
	}
	if !errors.Is(claimErr, ErrHeld) {
		t.Fatalf("claimCopy while the claim runs = %v, want a wait that costs nothing", claimErr)
	}
	if w := h.reload(task.ID); w.WaitsWho != "copy" || w.WaitsWhat != "preparing a copy (setup <1m)" {
		t.Fatalf("the task waits %q: %q, want copy: preparing a copy (setup <1m)", w.WaitsWho, w.WaitsWhat)
	}
	if ids := seen.list(); len(ids) != 2 || ids[1] != other.ID {
		t.Fatalf("the tick advanced %v, want the other task too", ids)
	}

	h.clock.Add(3 * time.Minute)
	h.tick()
	if w := h.reload(task.ID); w.WaitsWhat != "preparing a copy (setup 3m)" {
		t.Fatalf("after 3 minutes the task waits %q", w.WaitsWhat)
	}
	waitFor(t, "the claim's goroutine to call Claim", func() bool { return h.copies.claimCount() >= 1 })
	if n := h.copies.claimCount(); n != 1 {
		t.Fatalf("%d claims, want the one that still runs", n)
	}
	if evs := h.events(task.ID, "advance"); len(evs) != 0 {
		t.Fatalf("the wait counted as a failure: %+v", evs)
	}

	close(h.copies.block)
	h.e.waitBackground()
	h.tick()
	if claimErr != nil || got.Name != "factory2" {
		t.Fatalf("claimCopy after the claim = %+v, %v", got, claimErr)
	}
	w := h.reload(task.ID)
	if w.Copy != "factory2" || w.WaitsWho != "" || w.WaitsWhat != "" {
		t.Fatalf("after the claim the task holds %q and waits %q: %q", w.Copy, w.WaitsWho, w.WaitsWhat)
	}
	begin, ok := h.events(task.ID, "claim"), false
	for _, ev := range begin {
		if ev.Phase == registry.PhaseOK && strings.Contains(ev.Text, "factory2") {
			ok = true
		}
	}
	if len(begin) < 2 || begin[0].Phase != registry.PhaseBegin || !ok {
		t.Fatalf("claim events %+v, want a begin, then ok naming factory2", begin)
	}
}

// A stage claims its task's copy once and keeps it: a claim of the copy a
// task holds would run the claim protocol again (an install in the middle
// of the task). The copy goes with the task's release.
func TestAStageClaimsItsCopyOnceAndKeepsIt(t *testing.T) {
	h := newHarness(t)
	h.copies.claimed = Copy{Name: "factory1", Path: "/copies/factory1", Number: 1}
	task := h.task("PR-12333", registry.TaskActive, "investigate")
	h.claimNow(task)
	for _, read := range []registry.Task{h.reload(task.ID), h.reload(task.ID), task} { // task: read before the claim was noted
		c, err := h.e.claimCopy(h.ctx, read)
		if err != nil || c.Name != "factory1" {
			t.Fatalf("claimCopy = %+v, %v", c, err)
		}
	}
	if n := h.copies.claimCount(); n != 1 {
		t.Fatalf("%d claims for one task, want 1", n)
	}
	if _, err := h.e.stopTask(h.ctx, StopPayload{TaskID: task.ID, Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, kept := h.e.heldCopy(task.ID); kept {
		t.Fatal("a stopped task kept its copy")
	}
	if len(h.copies.released) != 1 || h.copies.released[0] != task.ID {
		t.Fatalf("released %v, want the task's copy", h.copies.released)
	}
}

// A guard that holds the copy (your agent or console in it, your changes)
// and a factory pane in it are waits with their reason, never failures; the
// claim is tried again on a later tick. Any other claim error is the task's
// failure.
func TestAGuardHoldDuringAClaimIsAWaitNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		what string
	}{
		{"your agent", slots.ErrHold{Reason: slots.HoldForeignAgent, Detail: "pane w1: claude agent is working in /copies/factory1"},
			"held: foreign_agent: pane w1: claude agent is working in /copies/factory1"},
		{"a factory pane", fmt.Errorf("%w: the fx-repro session works in /copies/factory1", ErrPaneInCopy),
			"a pane runs in it: the fx-repro session works in /copies/factory1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.copies.claimErr = tc.err
			task := h.task("PR-12334", registry.TaskActive, "investigate")
			h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
				_, err := h.e.claimCopy(ctx, t)
				return false, err
			})
			h.tick() // starts the claim
			h.e.waitBackground()
			h.tick() // picks its answer up
			w := h.reload(task.ID)
			if w.State != registry.TaskActive || w.WaitsWho != "copy" || w.WaitsWhat != tc.what {
				t.Fatalf("the task is %s waiting %q: %q, want active waiting copy: %s", w.State, w.WaitsWho, w.WaitsWhat, tc.what)
			}
			if evs := h.events(task.ID, "advance"); len(evs) != 0 {
				t.Fatalf("the hold counted as the task's failure: %+v", evs)
			}
			h.tick() // not yet: the next claim comes a minute after the hold
			h.e.waitBackground()
			if n := h.copies.claimCount(); n != 1 {
				t.Fatalf("%d claims, want no try on the next tick", n)
			}
			h.clock.Add(time.Minute)
			h.tick() // tries again
			h.e.waitBackground()
			if n := h.copies.claimCount(); n != 2 {
				t.Fatalf("%d claims, want a second try", n)
			}
			h.copies.mu.Lock()
			h.copies.claimErr = nil
			h.copies.claimed = Copy{Name: "factory1", Path: "/copies/factory1", Number: 1}
			h.copies.mu.Unlock()
			h.tick() // the second try's hold
			h.clock.Add(2 * time.Minute)
			h.tick() // a third claim, which goes through
			h.e.waitBackground()
			h.tick()
			if w := h.reload(task.ID); w.Copy != "factory1" || w.WaitsWho != "" {
				t.Fatalf("after the hold lifted the task holds %q and waits %q", w.Copy, w.WaitsWho)
			}
		})
	}

	h := newHarness(t)
	h.copies.claimErr = errors.New("claim factory1 for PR-12335: setup failed")
	task := h.task("PR-12335", registry.TaskActive, "investigate")
	h.claimNow(task)
	_, err := h.e.claimCopy(h.ctx, h.reload(task.ID))
	if err == nil || errors.Is(err, ErrHeld) || !strings.Contains(err.Error(), "setup failed") {
		t.Fatalf("a failed claim = %v, want the task's failure", err)
	}
}

// claimNow runs a claim for task to its end, as two ticks of a stage would.
func (h *harness) claimNow(task registry.Task) {
	h.t.Helper()
	if _, err := h.e.claimCopy(h.ctx, task); !errors.Is(err, ErrHeld) {
		h.t.Fatalf("the first claimCopy = %v, want the wait while the claim runs", err)
	}
	h.e.waitBackground()
}
