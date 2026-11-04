package factory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
)

func TestAPanicStopsOnlyItsTask(t *testing.T) {
	h := newHarness(t)
	bad := h.task("PR-12310", registry.TaskActive, "investigate")
	good := h.task("PR-12311", registry.TaskActive, "investigate")
	var seen calls
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		if t.ID == bad.ID {
			roles := strings.Fields(t.Kind)
			_ = roles[len(t.Title)] // an index out of range: a real bug, not a test's panic(…)
		}
		seen.add(t.ID, h.clock.Now())
		return false, nil
	})

	h.tick()

	if got := seen.list(); !slices.Equal(got, []int64{good.ID}) {
		t.Fatalf("advanced %v in the tick of the panic, want task %d too", got, good.ID)
	}
	evs := h.events(bad.ID, "panic")
	if len(evs) != 1 || evs[0].Phase != registry.PhaseFail {
		t.Fatalf("panic events %+v, want one fail", evs)
	}
	if !strings.Contains(evs[0].Text, "index out of range") || strings.Contains(evs[0].Text, "goroutine") {
		t.Fatalf("panic event %q: want the panic's message and no stack", evs[0].Text)
	}
	if got := h.reload(bad.ID); got.State != registry.TaskActive {
		t.Fatalf("one panic moved the task to %s", got.State)
	}
}

func TestTwoPanicsSendTheTaskToTheOperator(t *testing.T) {
	h := newHarness(t)
	bad := h.task("PR-12312", registry.TaskActive, "investigate")
	h.stage(config.FactoryStageAgents, func(context.Context, registry.Task, config.FactoryStage) (bool, error) {
		panic("stage exploded")
	})

	h.tick()
	h.clock.Add(time.Minute)
	h.tick()

	got := h.reload(bad.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWho != "you" || got.WaitsWhat != "the factory failed twice on this task" {
		t.Fatalf("after two panics the task is %s, waits %q: %q", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if n := len(h.events(bad.ID, "panic")); n != 2 {
		t.Fatalf("%d panic events, want 2", n)
	}
	if len(h.events(bad.ID, "alert")) != 1 {
		t.Fatal("no alert when the task moved to NEEDS YOU")
	}
	h.clock.Add(time.Minute)
	h.tick() // a task that needs you is not advanced: no third panic
	if n := len(h.events(bad.ID, "panic")); n != 2 {
		t.Fatalf("%d panic events after the move, want 2", n)
	}
}

// failingStage returns err for every task until ok is set, and records the
// calls.
func failingStage(c *calls, h *harness, err func(t registry.Task) error, ok *atomic.Bool) stageRunner {
	return func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		c.add(t.ID, h.clock.Now())
		if ok != nil && ok.Load() {
			return false, nil
		}
		return false, err(t)
	}
}

var sshRefused = func(registry.Task) error {
	return errors.New("git fetch origin: git@github.com: Permission denied (publickey)")
}

func TestAWakeAfterSleepMovesTurnDeadlines(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12313", registry.TaskActive, "investigate")
	var c calls
	var ok atomic.Bool
	ok.Store(true)
	h.stage(config.FactoryStageAgents, failingStage(&c, h, sshRefused, &ok))
	h.tick() // the tick before the Mac slept

	h.clock.Add(2 * time.Hour)
	ok.Store(false) // the network is not up yet on the first tick after the wake
	h.tick()

	if !slices.Equal(h.rt.shifts, []time.Duration{2 * time.Hour}) {
		t.Fatalf("Shift calls %v, want one of 2h", h.rt.shifts)
	}
	if _, held := h.e.holds[HoldInfra]; held {
		t.Fatal("a failed read on the first tick after a wake started an infrastructure hold")
	}
	if evs := h.events(task.ID, "advance"); len(evs) != 0 {
		t.Fatalf("the failure after the wake was counted: %+v", evs)
	}

	h.clock.Add(time.Minute)
	h.tick() // the same failure a minute later counts
	if _, held := h.e.holds[HoldInfra]; !held {
		t.Fatal("the same SSH error one tick later started no hold")
	}
	if !slices.Equal(h.rt.shifts, []time.Duration{2 * time.Hour}) {
		t.Fatalf("Shift calls %v after a normal tick", h.rt.shifts)
	}
}

func TestAnInfraFailureHoldsEveryStageAtNoCost(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12314", registry.TaskActive, "investigate")
	var c calls
	var ok atomic.Bool
	h.stage(config.FactoryStageAgents, failingStage(&c, h, sshRefused, &ok))

	for m := range 91 {
		h.clock.Set(t0.Add(time.Duration(m) * time.Minute))
		if m == 90 {
			ok.Store(true) // the key is back: the try at 90m passes
		}
		h.tick()
		if m == 0 {
			hd, held := h.e.holds[HoldInfra]
			if !held || hd.Reason != "SSH key refused" || !hd.Next.Equal(t0.Add(2*time.Minute)) {
				t.Fatalf("hold %+v (%v), want SSH key refused, next at 2m", hd, held)
			}
		}
	}

	var minutes []int
	for _, at := range c.at {
		minutes = append(minutes, int(at.Sub(t0)/time.Minute))
	}
	if want := []int{0, 2, 6, 14, 30, 60, 90}; !slices.Equal(minutes, want) {
		t.Fatalf("the stage ran at minutes %v, want %v (tries 2, 4, 8, 16, then every 30 minutes)", minutes, want)
	}
	if _, held := h.e.holds[HoldInfra]; held {
		t.Fatal("the hold outlived a try that passed")
	}
	got := h.reload(task.ID)
	if got.Attempt != 0 || got.State != registry.TaskActive {
		t.Fatalf("the hold cost the task: attempt %d, state %s", got.Attempt, got.State)
	}
	if as, _ := h.reg.Attempts(h.ctx, task.ID); len(as) != 0 {
		t.Fatalf("attempts %+v", as)
	}
	if evs := h.events(task.ID, "advance"); len(evs) != 0 {
		t.Fatalf("an infrastructure failure was charged to the task: %+v", evs)
	}
	holds := h.events(0, "hold")
	if len(holds) < 2 || holds[0].Phase != registry.PhaseBegin || holds[len(holds)-1].Phase != registry.PhaseOK {
		t.Fatalf("hold events %+v, want a begin and, last, an ok", holds)
	}
}

func TestAnInfraHoldKeepsItsBackoffPastTwelveHours(t *testing.T) {
	h := newHarness(t)
	h.task("PR-12328", registry.TaskActive, "investigate")
	var c calls
	h.stage(config.FactoryStageAgents, failingStage(&c, h, sshRefused, nil))
	h.tick() // the hold starts; its first try is 2 minutes later

	// 70 tries run 33 hours: past try 27 (12 hours), where 2m<<27 overflows,
	// and try 63, from which a shift gives 0.
	for try := 1; try <= 70; try++ {
		before := *h.e.holds[HoldInfra]
		h.clock.Set(before.Next.Add(-45 * time.Second))
		h.tick() // a tick between tries runs no stage
		h.clock.Set(before.Next)
		h.tick()
		after, held := h.e.holds[HoldInfra]
		if !held {
			t.Fatalf("try %d lifted the hold", try)
		}
		want := infraBackoffMax
		if waits := []time.Duration{4 * time.Minute, 8 * time.Minute, 16 * time.Minute}; try <= len(waits) {
			want = waits[try-1]
		}
		if got := after.Next.Sub(before.Next); got != want {
			t.Fatalf("try %d at %v waits %v, want %v", try, before.Next.Sub(t0), got, want)
		}
	}
	if n := len(c.list()); n != 71 {
		t.Fatalf("the stage ran %d times over 70 tries, want 71 (the failure and each try)", n)
	}
}

func TestAHeavyStageThatPanicsFreesItsSlot(t *testing.T) {
	h := newHarness(t)
	bad := h.task("PR-12364", registry.TaskActive, "local_review")
	other := h.task("PR-12365", registry.TaskActive, "local_review")
	var holders []int64
	runner := func(ctx context.Context, task registry.Task, _ config.FactoryStage) (bool, error) {
		if _, ok := h.e.heavy(ctx, task, "local review"); !ok {
			return false, nil
		}
		if !slices.Contains(holders, task.ID) {
			holders = append(holders, task.ID)
		}
		if task.ID == bad.ID {
			if as, _ := h.reg.Attempts(ctx, task.ID); len(as) == 0 { // the round's attempt, left running by the panic
				a, err := h.reg.BeginAttempt(ctx, task.ID, "local_review")
				if err == nil {
					err = h.reg.TransitionAttempt(ctx, a.ID, nil, registry.AttemptRunning, nil)
				}
				if err != nil {
					t.Errorf("attempt: %v", err)
				}
			}
			panic("the round's result parser exploded")
		}
		return false, nil
	}
	h.stage(config.FactoryStageReview, runner)

	h.tick()
	h.clock.Add(time.Minute)
	h.tick() // the second panic: bad needs you
	if got := h.reload(bad.ID).State; got != registry.TaskNeedsYou {
		t.Fatalf("after two panics the task is %s", got)
	}
	h.clock.Add(time.Minute)
	h.tick()
	if !slices.Contains(holders, other.ID) {
		got := h.reload(other.ID)
		t.Fatalf("the other task never got the slot: it waits %q: %q", got.WaitsWho, got.WaitsWhat)
	}

	// A restart does not give the slot back to the task that needs you.
	h.e = New(h.e.d)
	h.stage(config.FactoryStageReview, runner)
	if err := h.e.startup(h.ctx); err != nil {
		t.Fatal(err)
	}
	release, ok := h.e.heavy(h.ctx, h.reload(other.ID), "local review") // before any tick could sweep
	if !ok {
		t.Fatalf("startup adopted the slot of the task that needs you: %+v", h.e.heavyHeld[bad.ID])
	}
	release()
	holders = nil
	h.clock.Add(time.Minute)
	h.tick()
	if !slices.Equal(holders, []int64{other.ID}) {
		got := h.reload(other.ID)
		t.Fatalf("after the restart holders %v; the other task waits %q: %q", holders, got.WaitsWho, got.WaitsWhat)
	}
}

func TestHeavyIsOneHoldPerTaskAndUse(t *testing.T) {
	h := newHarness(t)
	first := h.task("PR-12366", registry.TaskActive, "local_review")
	second := h.task("PR-12367", registry.TaskActive, "local_review")
	var release func()
	firstDone, secondGot := false, false
	h.stage(config.FactoryStageReview, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		switch {
		case t.ID == first.ID && !firstDone:
			if rel, ok := h.e.heavy(ctx, t, "local review"); ok {
				release = rel // a stage that asks on every tick and keeps its last release
			}
		case t.ID == second.ID:
			_, secondGot = h.e.heavy(ctx, t, "local review")
		}
		return false, nil
	})
	for range 3 {
		h.tick()
		h.clock.Add(time.Minute)
	}
	if secondGot {
		t.Fatal("the second task got the slot while the first held it")
	}

	release()
	firstDone = true
	h.tick()
	if !secondGot {
		got := h.reload(second.ID)
		t.Fatalf("one release after three asks left the slot held: the second task waits %q: %q", got.WaitsWho, got.WaitsWhat)
	}
}

func TestAHeavySlotIsFreedWhenItsTaskLeavesTheStage(t *testing.T) {
	h := newHarness(t)
	first := h.task("PR-12368", registry.TaskActive, "local_review")
	second := h.task("PR-12369", registry.TaskActive, "local_review")
	secondGot := false
	h.stage(config.FactoryStageReview, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, ok := h.e.heavy(ctx, t, "local review")
		if t.ID == second.ID {
			secondGot = ok
		}
		return t.ID == first.ID && ok, nil // the first round passes, and its stage never released its slot
	})

	h.tick()
	if got := h.reload(first.ID).Stage; got != "pr_body" || secondGot {
		t.Fatalf("first at %q, second got the slot %v", got, secondGot)
	}
	h.clock.Add(time.Minute)
	h.tick()
	if !secondGot {
		got := h.reload(second.ID)
		t.Fatalf("the slot of a task that left its local review stayed taken: the second task waits %q: %q", got.WaitsWho, got.WaitsWhat)
	}
}

func TestAHoldIsNotLiftedByATickThatTriedNothing(t *testing.T) {
	var herdrDown atomic.Bool
	h := newHarness(t, func(d *Deps) {
		d.HerdrUp = func(context.Context) error {
			if herdrDown.Load() {
				return errors.New("herdr: connect: connection refused")
			}
			return nil
		}
	})
	h.task("PR-12327", registry.TaskActive, "investigate")
	var c calls
	h.stage(config.FactoryStageAgents, failingStage(&c, h, sshRefused, nil))
	h.tick() // the hold starts, its try due at 2m

	herdrDown.Store(true)
	h.clock.Add(3 * time.Minute)
	h.tick() // the try is due, but herdr is down: nothing was tried
	if _, held := h.e.holds[HoldInfra]; !held {
		t.Fatal("a tick that advanced nothing (herdr down) lifted the infrastructure hold")
	}

	herdrDown.Store(false)
	h.clock.Add(2 * time.Hour) // the Mac slept; its first tick counts no failure
	h.tick()
	if _, held := h.e.holds[HoldInfra]; !held {
		t.Fatal("the uncounted tick after a wake lifted the infrastructure hold")
	}
	since := h.e.holds[HoldInfra].Since
	h.clock.Add(time.Minute)
	h.tick()
	if hd, held := h.e.holds[HoldInfra]; !held || !hd.Since.Equal(since) || hd.Tries != 1 {
		t.Fatalf("hold %+v (%v): want the same hold, since %v, one try failed", hd, held, since)
	}
}

func TestTheSameFailureOnTwoTasksHoldsEverything(t *testing.T) {
	h := newHarness(t)
	a := h.task("PR-12315", registry.TaskActive, "investigate")
	var c calls
	setupFails := func(t registry.Task) error {
		return fmt.Errorf("%s: bin/worktree-setup: exit 1: Bundler could not find compatible versions", t.JiraKey)
	}
	h.stage(config.FactoryStageAgents, failingStage(&c, h, setupFails, nil))

	h.tick()
	h.clock.Add(time.Minute)
	h.tick()
	if _, held := h.e.holds[HoldInfra]; held {
		t.Fatal("one task failing twice held everything")
	}

	b := h.task("PR-12316", registry.TaskActive, "investigate")
	h.clock.Add(time.Minute)
	h.tick()
	hd, held := h.e.holds[HoldInfra]
	if !held || !strings.Contains(hd.Reason, "the same failure on 2 tasks") {
		t.Fatalf("hold %+v (%v), want one for the same failure on 2 tasks", hd, held)
	}
	before := len(c.list())
	h.clock.Add(time.Minute)
	h.tick()
	if got := len(c.list()); got != before {
		t.Fatalf("the stage ran %d more times during the hold, for tasks %d and %d", got-before, a.ID, b.ID)
	}
}

func TestAHoldOver30MinutesNeedsYou(t *testing.T) {
	h := newHarness(t)
	h.task("PR-12317", registry.TaskActive, "investigate")
	var c calls
	h.stage(config.FactoryStageAgents, failingStage(&c, h, sshRefused, nil))

	for m := range 30 {
		h.clock.Set(t0.Add(time.Duration(m) * time.Minute))
		h.tick()
	}
	if got := h.e.Blocking(); len(got) != 0 {
		t.Fatalf("blocking %v before 30 minutes", got)
	}
	if n := len(h.events(0, "alert")); n != 0 {
		t.Fatalf("%d alerts before 30 minutes", n)
	}

	h.clock.Set(t0.Add(30 * time.Minute))
	h.tick()
	got := h.e.Blocking()
	if len(got) != 1 || !strings.HasPrefix(got[0], "infra: SSH key refused") {
		t.Fatalf("blocking %v, want the infrastructure hold", got)
	}
	if n := len(h.events(0, "alert")); n != 1 {
		t.Fatalf("%d alerts, want one", n)
	}
	h.clock.Add(time.Minute)
	h.tick()
	if n := len(h.events(0, "alert")); n != 1 {
		t.Fatalf("%d alerts after one more tick, want still one", n)
	}
}

func TestHerdrDownOpensTheSessionAtMostTwiceAnHour(t *testing.T) {
	var up atomic.Bool
	var reveals []time.Time
	var h *harness
	h = newHarness(t, func(d *Deps) {
		d.HerdrUp = func(context.Context) error {
			if up.Load() {
				return nil
			}
			return errors.New("herdr: dial unix /tmp/herdr-factory.sock: connect: no such file or directory")
		}
		d.Reveal = func(context.Context) error {
			reveals = append(reveals, h.clock.Now())
			return nil
		}
	})
	task := h.task("PR-12318", registry.TaskActive, "investigate")
	var c calls
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		c.add(t.ID, h.clock.Now())
		return false, nil
	})

	for m := range 121 {
		h.clock.Set(t0.Add(time.Duration(m) * time.Minute))
		h.tick()
	}

	for _, at := range reveals {
		n := 0
		for _, other := range reveals {
			if !other.Before(at) && other.Sub(at) < time.Hour {
				n++
			}
		}
		if n > 2 {
			t.Fatalf("reveals at %v: %d within an hour of %v", reveals, n, at)
		}
	}
	if len(reveals) < 3 {
		t.Fatalf("reveals at %v: the factory gave up opening herdr", reveals)
	}
	if got := c.list(); len(got) != 0 {
		t.Fatalf("stages ran for %v while herdr was down", got)
	}
	if hd, held := h.e.holds[HoldHerdr]; !held || !strings.Contains(hd.Reason, "factory") {
		t.Fatalf("herdr hold %+v (%v), want one naming the factory session", hd, held)
	}

	up.Store(true)
	h.clock.Add(time.Minute)
	h.tick()
	if _, held := h.e.holds[HoldHerdr]; held {
		t.Fatal("the herdr hold outlived herdr coming back")
	}
	if got := c.list(); !slices.Equal(got, []int64{task.ID}) {
		t.Fatalf("after herdr came back the stage ran for %v", got)
	}
}

func TestSIGTERMDrainsWithoutNewLaunches(t *testing.T) {
	h := newHarness(t)
	first := h.task("PR-12319", registry.TaskActive, "investigate")
	second := h.task("PR-12320", registry.TaskActive, "investigate")
	sigs := h.captureSignals()
	var ticks atomic.Int32
	var launchErr, effectErr error
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		switch t.ID {
		case first.ID:
			ticks.Add(1)
			(<-sigs) <- syscall.SIGTERM
			deadline := time.Now().Add(5 * time.Second)
			for !h.e.draining.Load() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		case second.ID:
			launchErr = h.e.launch(ctx, t, registry.Attempt{}, "", nil)
			_, _, effectErr = h.e.beginEffect(ctx, "publish:2:abc", "publish", t.ID)
		}
		return false, nil
	})

	done := make(chan error, 1)
	go func() { done <- h.e.Run(h.ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil after the drain", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not drain")
	}

	if got := ticks.Load(); got != 1 {
		t.Fatalf("%d ticks, want the one that got SIGTERM, ended", got)
	}
	if !errors.Is(launchErr, ErrHeld) || !errors.Is(effectErr, ErrHeld) {
		t.Fatalf("launch %v, effect %v: want both held during the drain", launchErr, effectErr)
	}
	if got := h.rt.launched(); len(got) != 0 {
		t.Fatalf("launched %v during the drain", got)
	}
	if _, err := h.reg.EffectByKey(h.ctx, "publish:2:abc"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("the drain began an effect: %v", err)
	}
}

func TestAStaleRequestExpires(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12321", registry.TaskActive, "investigate")
	stale, err := h.reg.EnqueueRequest(h.ctx, ReqStop, StopPayload{TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Add(31 * time.Minute)
	fresh, err := h.reg.EnqueueRequest(h.ctx, ReqStart, map[string]string{"Text": "PR-12322"})
	if err != nil {
		t.Fatal(err)
	}

	h.tick()

	req, err := h.reg.RequestByID(h.ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != registry.RequestFailed || req.Result != "expired after 31m" {
		t.Fatalf("stale request %s: %q, want failed: expired after 31m", req.State, req.Result)
	}
	if got := h.reload(task.ID).State; got != registry.TaskActive {
		t.Fatalf("an expired stop moved the task to %s", got)
	}
	if req, _ := h.reg.RequestByID(h.ctx, fresh); req.State == registry.RequestPending || strings.Contains(req.Result, "expired") {
		t.Fatalf("the fresh request: %s %q", req.State, req.Result)
	}
}

func TestARequestWithAnUnknownFieldIsRefused(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12323", registry.TaskActive, "investigate")
	id, err := h.reg.EnqueueRequest(h.ctx, ReqStop, map[string]any{"TaskID": task.ID, "Purge": true})
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	req, _ := h.reg.RequestByID(h.ctx, id)
	if req.State != registry.RequestFailed || !strings.Contains(req.Result, "Purge") {
		t.Fatalf("request %s: %q, want refused naming the field", req.State, req.Result)
	}
	if got := h.reload(task.ID).State; got != registry.TaskActive {
		t.Fatalf("a refused request moved the task to %s", got)
	}
}

// diskHarness is a harness with one factory repository whose copies live
// under a temporary directory with a 15 GB floor, and a disk that reports
// free bytes.
func diskHarness(t *testing.T, free *atomic.Uint64) (*harness, string) {
	t.Helper()
	dir := t.TempDir()
	h := newHarness(t, func(d *Deps) {
		d.Disk = func(string) (uint64, error) { return free.Load(), nil }
	})
	h.cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Copies: filepath.Join(dir, "shop.factory{n}"), MinFreeDiskGB: 15}}
	return h, dir
}

const gb = 1 << 30

func TestLowDiskHoldsLaunchesAndParksIdleAgentsThatWaitOnYou(t *testing.T) {
	var free atomic.Uint64
	free.Store(9 * gb)
	h, dir := diskHarness(t, &free)
	working := h.task("PR-12324", registry.TaskActive, "investigate")
	waiting := h.task("PR-12325", registry.TaskActive, "investigate")
	if err := h.reg.TransitionTask(h.ctx, waiting.ID, nil, registry.TaskNeedsYou, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "you")
		u.Set("waits_what", "3 questions before coding")
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.e.startServer(h.ctx, h.reload(waiting.ID), Copy{Name: "factory2"}); err != nil {
		t.Fatal(err)
	}
	var launchErr error
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		launchErr = h.e.launch(ctx, t, registry.Attempt{}, "", nil)
		return false, launchErr
	})

	h.tick()
	h.clock.Add(time.Minute)
	h.tick()

	if got := h.rt.launched(); len(got) != 0 {
		t.Fatalf("launched %v on a full disk", got)
	}
	if !errors.Is(launchErr, ErrHeld) {
		t.Fatalf("launch = %v, want held", launchErr)
	}
	got := h.reload(working.ID)
	if want := "9.0 GB free under " + dir + ", needs 15 GB"; got.WaitsWho != "disk" || got.WaitsWhat != want {
		t.Fatalf("the working task waits %q: %q, want disk: %q", got.WaitsWho, got.WaitsWhat, want)
	}
	if got := h.rt.parked(); !slices.Equal(got, []int64{waiting.ID}) {
		t.Fatalf("parked %v over two ticks, want the task that waits on you, once", got)
	}
	if !slices.Equal(h.copies.stopped, []string{"factory2"}) {
		t.Fatalf("stopped servers %v, want the idle one", h.copies.stopped)
	}
	if evs := h.events(working.ID, "advance"); len(evs) != 0 {
		t.Fatalf("a held launch was charged to the task: %+v", evs)
	}
	if got := h.e.Blocking(); len(got) != 0 {
		t.Fatalf("blocking %v above half the floor", got)
	}

	free.Store(40 * gb)
	h.clock.Add(time.Minute)
	h.tick()
	if got := h.rt.launched(); !slices.Equal(got, []int64{working.ID}) {
		t.Fatalf("launched %v after the disk freed, want the working task", got)
	}
}

func TestHalfTheDiskFloorNeedsYou(t *testing.T) {
	var free atomic.Uint64
	free.Store(7 * gb)
	h, dir := diskHarness(t, &free)
	h.task("PR-12326", registry.TaskActive, "investigate")

	h.tick()

	got := h.e.Blocking()
	if want := "disk: 7.0 GB free under " + dir + ", below half of 15 GB"; len(got) != 1 || got[0] != want {
		t.Fatalf("blocking %v, want %q", got, want)
	}
	if n := len(h.events(0, "alert")); n != 1 {
		t.Fatalf("%d alerts, want one", n)
	}
	h.clock.Add(time.Minute)
	h.tick()
	if n := len(h.events(0, "alert")); n != 1 {
		t.Fatalf("%d alerts after another tick, want one", n)
	}

	free.Store(40 * gb)
	h.clock.Add(time.Minute)
	h.tick()
	if got := h.e.Blocking(); len(got) != 0 {
		t.Fatalf("blocking %v after the disk freed", got)
	}
}

func TestHeavyStagesRunOneAtATime(t *testing.T) {
	h := newHarness(t)
	first := h.task("PR-12360", registry.TaskActive, "local_review")
	second := h.task("PR-12361", registry.TaskActive, "local_review")
	releases := map[int64]func(){}
	finished := map[int64]bool{}
	var running []int64
	h.stage(config.FactoryStageReview, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		if finished[t.ID] {
			return true, nil // its round ended: the stage passes and the task moves on
		}
		release, ok := h.e.heavy(ctx, t, "local review")
		if ok {
			if releases[t.ID] == nil {
				running = append(running, t.ID)
			}
			releases[t.ID] = release
		}
		return false, nil
	})

	h.tick()

	if !slices.Equal(running, []int64{first.ID}) {
		t.Fatalf("running %v, want only the first task's round", running)
	}
	got := h.reload(second.ID)
	if got.WaitsWho+": "+got.WaitsWhat != "heavy: PR-12360's local review runs" {
		t.Fatalf("the second task waits %q: %q", got.WaitsWho, got.WaitsWhat)
	}

	releases[first.ID]()
	releases[first.ID]() // a release twice frees one slot, once
	finished[first.ID] = true
	h.clock.Add(time.Minute)
	h.tick()
	if !slices.Equal(running, []int64{first.ID, second.ID}) {
		t.Fatalf("running %v after the first round ended", running)
	}
	if got := h.reload(second.ID); got.WaitsWho == "heavy" {
		t.Fatalf("the second task still waits %q: %q", got.WaitsWho, got.WaitsWhat)
	}
}

func TestStartupCountsTheHeavyStagesThatStillRun(t *testing.T) {
	h := newHarness(t)
	first := h.task("PR-12362", registry.TaskActive, "local_review")
	second := h.task("PR-12363", registry.TaskActive, "local_review")
	a, err := h.reg.BeginAttempt(h.ctx, first.ID, "local_review")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.TransitionAttempt(h.ctx, a.ID, nil, registry.AttemptRunning, nil); err != nil {
		t.Fatal(err)
	}

	if err := h.e.startup(h.ctx); err != nil {
		t.Fatal(err)
	}

	if _, ok := h.e.heavy(h.ctx, h.reload(second.ID), "local review"); ok {
		t.Fatal("a second round started while a round from before the restart still runs")
	}
	if got := h.reload(second.ID); got.WaitsWhat != "PR-12362's local review runs" {
		t.Fatalf("the second task waits %q", got.WaitsWhat)
	}
	release, ok := h.e.heavy(h.ctx, h.reload(first.ID), "local review")
	if !ok {
		t.Fatal("the round that still runs could not take its own slot back")
	}
	release()
	if _, ok := h.e.heavy(h.ctx, h.reload(second.ID), "local review"); !ok {
		t.Fatal("the slot stayed taken after the adopted round released it")
	}
}

// The observe pass runs outside any task's step: a panic in the runtime's
// Observe stops neither the daemon nor its watchdog, and comes back as a
// failed read the next tick weighs.
func TestAPanicInTheObservePassIsContained(t *testing.T) {
	h := newHarness(t)
	h.rt.observePanic = "index out of range [3] with length 3"

	h.e.observe(h.ctx)

	if len(h.e.readFails) != 1 || !errors.Is(h.e.readFails[0], errPanicked) ||
		!strings.Contains(h.e.readFails[0].Error(), "index out of range") {
		t.Fatalf("read failures %v, want the contained panic", h.e.readFails)
	}
	if h.rt.watched != 1 {
		t.Fatalf("the watchdog ran %d times after the panic, want 1", h.rt.watched)
	}
}

// A launch the runtime holds back on Codex's budget is no failure: the
// task waits with the runtime's reason, and the next launch that goes
// through clears it.
func TestALaunchThatWaitsOnCodexsBudgetCostsTheTaskNothing(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12371", registry.TaskActive, "investigate")
	a, err := h.reg.BeginAttempt(h.ctx, task.ID, "investigate")
	if err != nil {
		t.Fatal(err)
	}
	h.rt.launchErr = &runtime.WaitError{Who: "usage", What: "codex at 96%, waits till 16:00"}

	err = h.e.launch(h.ctx, task, a, "/copies/shop1", nil)

	if !errors.Is(err, ErrHeld) {
		t.Fatalf("launch = %v, want a hold", err)
	}
	if got := h.reload(task.ID); got.WaitsWho != "usage" || got.WaitsWhat != "codex at 96%, waits till 16:00" {
		t.Fatalf("the task waits %q: %q, want usage: codex at 96%%, waits till 16:00", got.WaitsWho, got.WaitsWhat)
	}

	h.rt.launchErr = nil
	if err := h.e.launch(h.ctx, h.reload(task.ID), a, "/copies/shop1", nil); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(task.ID); got.WaitsWho != "" || got.WaitsWhat != "" {
		t.Fatalf("after a launch that went through the task still waits %q: %q", got.WaitsWho, got.WaitsWhat)
	}
}
