package factory

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/slots"
)

// claimEvents counts the task's claim events by phase.
func (h *harness) claimEvents(id int64) map[string]int {
	h.t.Helper()
	n := map[string]int{}
	for _, ev := range h.events(id, "claim") {
		n[ev.Phase]++
	}
	return n
}

// wantClaimsClosed checks that each claim's begin event ended with an ok or
// a fail one: a begin without an end means a claim a stop of the daemon cut.
func (h *harness) wantClaimsClosed(id int64) {
	h.t.Helper()
	n := h.claimEvents(id)
	if n[registry.PhaseBegin] == 0 || n[registry.PhaseBegin] != n[registry.PhaseOK]+n[registry.PhaseFail] {
		h.t.Fatalf("claim events %v: every begin needs an ok or a fail", n)
	}
}

// A stop during a claim cancels the claim (its commands end with it), and
// the copy the claim reserved is released once the claim has ended,
// whether the claim got it or not; the claim's begin ends in a fail.
func TestAStopCancelsItsRunningClaimAndReleasesTheCopy(t *testing.T) {
	h := newHarness(t)
	h.copies.block = make(chan struct{})
	task := h.task("PR-12336", registry.TaskActive, "investigate")
	if _, err := h.e.claimCopy(h.ctx, task); !errors.Is(err, ErrHeld) {
		t.Fatalf("claimCopy = %v, want the wait while the claim runs", err)
	}
	if _, err := h.e.stopTask(h.ctx, StopPayload{TaskID: task.ID, Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the claim to end", func() bool { ended, _ := h.copies.endedClaims(); return ended == 1 })
	if _, cancelled := h.copies.endedClaims(); cancelled != 1 {
		t.Fatal("the stop did not cancel the claim")
	}
	h.e.waitBackground() // the fake counts its end before the claim's goroutine closes it
	h.tick()
	if got := h.copies.releases(); len(got) != 1 || got[0] != task.ID {
		t.Fatalf("released %v, want the stopped task's reserved copy", got)
	}
	h.wantClaimsClosed(task.ID)
}

// A claim that ended in a wait (a guard's hold, no copy free) keeps the copy
// it reserved for the task's next claim; a stop releases it, although the
// task's row names no copy.
func TestAStopReleasesTheCopyAHeldClaimReserved(t *testing.T) {
	h := newHarness(t)
	h.copies.claimErr = slots.ErrHold{Reason: slots.HoldDirtyWorktree, Detail: "factory1 has changes"}
	task := h.task("PR-12337", registry.TaskActive, "investigate")
	h.claimNow(task)
	if _, err := h.e.claimCopy(h.ctx, h.reload(task.ID)); !errors.Is(err, ErrHeld) {
		t.Fatalf("claimCopy = %v, want the hold's wait", err)
	}
	if got := h.reload(task.ID); got.Copy != "" {
		t.Fatalf("the task names copy %q", got.Copy)
	}
	if _, err := h.e.stopTask(h.ctx, StopPayload{TaskID: task.ID, Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	if got := h.copies.releases(); len(got) != 1 || got[0] != task.ID {
		t.Fatalf("released %v, want the copy the held claim reserved", got)
	}
	h.wantClaimsClosed(task.ID)
}

// The sweep releases the copy of a claim that ended after its task ended
// some other way (done, so no cancel reached the claim), whatever the
// claim's result, once, and ends the claim's begin.
func TestTheSweepReleasesTheCopyOfAClaimWhoseTaskEnded(t *testing.T) {
	for name, claimErr := range map[string]error{
		"claimed": nil,
		"held":    slots.ErrHold{Reason: slots.HoldForeignAgent, Detail: "pane w1"},
		"failed":  errors.New("setup failed"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.copies.block = make(chan struct{})
			h.copies.claimErr = claimErr
			h.copies.claimed = Copy{Name: "factory1", Path: "/copies/factory1", Number: 1}
			task := h.task("PR-12338", registry.TaskActive, "investigate")
			if _, err := h.e.claimCopy(h.ctx, task); !errors.Is(err, ErrHeld) {
				t.Fatalf("claimCopy = %v", err)
			}
			if err := h.reg.TransitionTask(h.ctx, task.ID, nil, registry.TaskDone, nil); err != nil {
				t.Fatal(err)
			}
			h.tick() // the claim still runs: nothing to release yet
			if got := h.copies.releases(); len(got) != 0 {
				t.Fatalf("released %v while the claim ran", got)
			}
			close(h.copies.block)
			h.e.waitBackground()
			h.tick()
			if got := h.copies.releases(); len(got) != 1 || got[0] != task.ID {
				t.Fatalf("released %v, want the ended task's copy", got)
			}
			h.wantClaimsClosed(task.ID)
			h.tick()
			if got := h.copies.releases(); len(got) != 1 {
				t.Fatalf("released %v, want once", got)
			}
		})
	}
}

// A drain cancels the claims that run (their process groups end with the
// context) and waits for them before Run returns, so no setup or install
// goes on in a copy after the daemon exits; their begin events end.
func TestADrainCancelsTheClaimsThatRunAndWaitsForThem(t *testing.T) {
	h := newHarness(t)
	h.copies.block = make(chan struct{})
	task := h.task("PR-12339", registry.TaskActive, "investigate")
	started := make(chan struct{}, 1)
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, err := h.e.claimCopy(ctx, t)
		select {
		case started <- struct{}{}:
		default:
		}
		return false, err
	})
	sigs := h.captureSignals()
	done := make(chan error, 1)
	go func() { done <- h.e.Run(h.ctx) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no claim started")
	}
	(<-sigs) <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
	if ended, cancelled := h.copies.endedClaims(); ended != 1 || cancelled != 1 {
		t.Fatalf("after Run returned %d claims ended, %d cancelled; want the running one cancelled and ended", ended, cancelled)
	}
	h.wantClaimsClosed(task.ID)
}

// A stop that cancels a running claim leaves the release to the sweep; a
// drain right after that tick still releases the copy the claim reserved
// before Run returns (a stopped task is no restart's), and the claim's
// begin ends without promising a next claim.
func TestADrainAfterAStopReleasesTheCopyOfTheRunningClaim(t *testing.T) {
	h := newHarness(t)
	h.copies.block = make(chan struct{})
	claimer := h.task("PR-12341", registry.TaskActive, "investigate")
	helper := h.task("PR-12342", registry.TaskActive, "investigate")
	started := make(chan struct{}, 1)
	term := make(chan chan<- os.Signal, 1)
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		switch t.ID {
		case claimer.ID:
			_, err := h.e.claimCopy(ctx, t)
			select {
			case started <- struct{}{}:
			default:
			}
			return false, err
		case helper.ID:
			// SIGTERM lands in the tick that stops the claimer. A tick handles
			// the requests before the tasks' passes, so that is the first pass
			// of the helper that finds the claimer stopped: the tick that
			// started the claim may still run, its requests read, when the stop
			// is asked and term is sent, and a SIGTERM there would drain the
			// daemon before any tick took the stop (under load, the claimer
			// active at the end).
			if cur, err := h.reg.TaskByID(ctx, claimer.ID); err != nil || cur.State != registry.TaskStopped {
				return false, nil
			}
			select {
			case sigs := <-term:
				sigs <- syscall.SIGTERM
				for i := 0; i < 2000 && !h.e.draining.Load(); i++ { // the signal's handler, not a time, ends the wait
					time.Sleep(5 * time.Millisecond)
				}
			default:
			}
		}
		return false, nil
	})
	sigs := h.captureSignals()
	done := make(chan error, 1)
	go func() { done <- h.e.Run(h.ctx) }()
	sigCh := <-sigs
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no claim started")
	}
	if _, err := h.reg.EnqueueRequest(h.ctx, ReqStop, StopPayload{TaskID: claimer.ID, Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	term <- sigCh
	h.e.Kick()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
	if got := h.reload(claimer.ID); got.State != registry.TaskStopped {
		t.Fatalf("the claimer is %s, want stopped", got.State)
	}
	if got := h.copies.releases(); len(got) != 1 || got[0] != claimer.ID {
		t.Fatalf("released %v before Run returned, want the stopped task's reserved copy", got)
	}
	h.wantClaimsClosed(claimer.ID)
	for _, ev := range h.events(claimer.ID, "claim") {
		if strings.Contains(ev.Text, "next claim") {
			t.Fatalf("a stopped task's claim promises a next claim: %q", ev.Text)
		}
	}
}

// A release the copy's guard refuses (your changes or your console in it)
// is a debt: it is tried again after 1, 2, 4 and then every 5 minutes until
// the copy is free, and after 30 minutes one alert names the copy and the
// guard's reason. The debt is read from the reservations, not memory.
func TestARefusedReleaseIsTriedAgainUntilTheCopyIsFree(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12343", registry.TaskActive, "investigate")
	h.copies.reserve(task.ID, "factory2")
	h.copies.setReleaseErr(slots.ErrHold{Reason: slots.HoldDirtyWorktree, Detail: "factory2 has your changes"})
	if _, err := h.e.stopTask(h.ctx, StopPayload{TaskID: task.ID, Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	if got := h.copies.releases(); len(got) != 1 {
		t.Fatalf("the stop released %v, want one try", got)
	}
	alerts := func() []registry.Event {
		var out []registry.Event
		for _, ev := range h.events(task.ID, "alert") {
			if strings.Contains(ev.Text, "factory2") {
				out = append(out, ev)
			}
		}
		return out
	}
	stop := h.clock.Now()
	for _, s := range []struct {
		at    time.Duration
		tries int
	}{
		{30 * time.Second, 1}, {time.Minute, 2}, {2*time.Minute + 59*time.Second, 2}, {3 * time.Minute, 3},
		{7 * time.Minute, 4}, {12 * time.Minute, 5}, {17 * time.Minute, 6}, {22 * time.Minute, 7}, {27 * time.Minute, 8},
	} {
		h.clock.Set(stop.Add(s.at))
		h.tick()
		if got := len(h.copies.releases()); got != s.tries {
			t.Fatalf("at +%s: %d tries of the release, want %d", s.at, got, s.tries)
		}
	}
	if a := alerts(); len(a) != 0 {
		t.Fatalf("an alert before 30 minutes: %+v", a)
	}
	h.clock.Set(stop.Add(32 * time.Minute))
	h.tick()
	a := alerts()
	if len(a) != 1 || !strings.Contains(a[0].Text, "dirty_worktree") || !strings.Contains(a[0].Text, "your changes") {
		t.Fatalf("alerts %+v, want one naming factory2 and the guard's reason", a)
	}
	h.clock.Set(stop.Add(37 * time.Minute))
	h.tick()
	if got := len(alerts()); got != 1 {
		t.Fatalf("%d alerts, want one: it keeps trying, it alerts once", got)
	}
	if got := len(h.copies.releases()); got != 10 {
		t.Fatalf("%d tries, want 10: it keeps trying", got)
	}
	h.copies.setReleaseErr(nil)
	h.clock.Set(stop.Add(42 * time.Minute))
	h.tick()
	h.clock.Set(stop.Add(60 * time.Minute))
	h.tick()
	if got := len(h.copies.releases()); got != 11 {
		t.Fatalf("%d tries, want the one that freed the copy and no more", got)
	}
}

// A release debt is read from the registry and the copies' reservations:
// after a restart, the copies tasks that stopped or are done still hold are
// released on the first tick; a task at work, or one that waits on you,
// keeps its copy.
func TestAReleaseDebtSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	stopped := h.task("PR-12344", registry.TaskStopped, "investigate")
	done := h.task("PR-12345", registry.TaskDone, "publish")
	working := h.task("PR-12346", registry.TaskActive, "coding")
	waiting := h.task("PR-12347", registry.TaskNeedsYou, "questions")
	for i, task := range []registry.Task{stopped, done, working, waiting} {
		h.copies.reserve(task.ID, "factory"+string(rune('1'+i)))
	}
	h.restart()
	h.tick()
	got := h.copies.releases()
	slices.Sort(got)
	if !slices.Equal(got, []int64{stopped.ID, done.ID}) {
		t.Fatalf("released %v, want the stopped and the done tasks' copies", got)
	}
	h.clock.Add(time.Hour)
	h.tick()
	if got := h.copies.releases(); len(got) != 2 {
		t.Fatalf("released %v, want no second release", got)
	}
}

// A claim that ends in a wait keeps that wait's reason, at no cost, and is
// tried again after 1, 2, 4 and then every 5 minutes, not on every tick;
// a retry that runs keeps the reason it waits on, so the wait never
// flickers back to "preparing". The claim that gets a copy clears it.
func TestAClaimThatWaitsKeepsItsReasonAndTriesAgainLater(t *testing.T) {
	h := newHarness(t)
	h.copies.claimErr = noCopy{what: "all 3 busy"}
	task := h.task("PR-12340", registry.TaskActive, "investigate")
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, err := h.e.claimCopy(ctx, t)
		return false, err
	})
	h.tick() // the first claim
	h.e.waitBackground()
	h.tick() // its answer: all 3 busy
	start := h.clock.Now()
	for _, s := range []struct {
		at     time.Duration
		claims int
	}{
		{45 * time.Second, 1}, {time.Minute, 2}, // a minute after the answer
		{2*time.Minute + 59*time.Second, 2}, {3 * time.Minute, 3}, // then 2
		{6*time.Minute + 59*time.Second, 3}, {7 * time.Minute, 4}, // then 4
		{12 * time.Minute, 5}, {16 * time.Minute, 5}, {17 * time.Minute, 6}, // then 5
	} {
		h.clock.Set(start.Add(s.at))
		h.tick()
		h.e.waitBackground()
		if n := h.copies.claimCount(); n != s.claims {
			t.Fatalf("at +%s: %d claims, want %d", s.at, n, s.claims)
		}
		h.tick() // a retry's answer
		if w := h.reload(task.ID); w.WaitsWho != "copy" || w.WaitsWhat != "all 3 busy" {
			t.Fatalf("at +%s the task waits %q: %q, want copy: all 3 busy", s.at, w.WaitsWho, w.WaitsWhat)
		}
	}
	spans, err := h.reg.Spans(h.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	preparing := 0
	for _, sp := range spans {
		if strings.HasPrefix(sp.WaitsWhat, "preparing") {
			preparing++
		}
	}
	if preparing != 1 {
		t.Fatalf("%d spans of preparing, want the first claim's only: the wait flickered", preparing)
	}
	if evs := h.events(task.ID, "advance"); len(evs) != 0 {
		t.Fatalf("the waits counted as failures: %+v", evs)
	}
	h.wantClaimsClosed(task.ID)

	h.copies.mu.Lock()
	h.copies.claimErr = nil
	h.copies.claimed = Copy{Name: "factory3", Path: "/copies/factory3", Number: 3}
	h.copies.mu.Unlock()
	h.clock.Add(5 * time.Minute)
	h.tick()
	h.e.waitBackground()
	h.tick()
	if w := h.reload(task.ID); w.Copy != "factory3" || w.WaitsWho != "" {
		t.Fatalf("after a free copy the task holds %q and waits %q", w.Copy, w.WaitsWho)
	}
	h.wantClaimsClosed(task.ID)
}

// waitFor polls cond for up to 10 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
