package factory

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

var t0 = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

// fakeRuntime is the agent runtime as the engine sees it: it records the
// launches, parks and deadline shifts it was asked for.
type fakeRuntime struct {
	mu         sync.Mutex
	launches   []int64
	parks      []int64
	shifts     []time.Duration
	events     []runtime.Event // what Observe reports
	observeErr error
	// launchErr is what Launch answers; observePanic, when set, is what
	// Observe panics with; watched counts the Watchdog calls.
	launchErr    error
	observePanic any
	watched      int
	// letGo records the LetGo calls, "<task key>/<role>".
	letGo []string
}

func (f *fakeRuntime) Owner(context.Context, registry.Task) (store.PR, error) { return store.PR{}, nil }

func (f *fakeRuntime) Launch(_ context.Context, t registry.Task, _ registry.Attempt, _ Copy, _ []config.Role) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, t.ID)
	return f.launchErr
}

func (f *fakeRuntime) Send(context.Context, registry.Task, registry.Attempt, config.Role, string) (string, error) {
	return "", nil
}

func (f *fakeRuntime) Observe(context.Context) ([]runtime.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observePanic != nil {
		panic(f.observePanic)
	}
	return f.events, f.observeErr
}

func (f *fakeRuntime) Verify(context.Context, string, bool, string) error { return nil }

func (f *fakeRuntime) Park(_ context.Context, t registry.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parks = append(f.parks, t.ID)
	return nil
}

func (f *fakeRuntime) Reconcile(context.Context, registry.LedgerEntry) (string, error) {
	return registry.DeliveredUnknown, nil
}

func (f *fakeRuntime) Watchdog(context.Context, time.Time) ([]runtime.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watched++
	return nil, nil
}

func (f *fakeRuntime) RecoverLost(context.Context, registry.Task, registry.Attempt, config.Role) error {
	return nil
}

func (f *fakeRuntime) LetGo(_ context.Context, t registry.Task, role string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.letGo = append(f.letGo, TaskKey(t)+"/"+role)
	return 1, nil
}

func (f *fakeRuntime) Shift(gap time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shifts = append(f.shifts, gap)
}

func (f *fakeRuntime) launched() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.launches...)
}

func (f *fakeRuntime) parked() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.parks...)
}

// fakeCopies records the servers it was asked to start and stop and the
// releases; a claim answers claimed, or claimErr, after block (when set)
// is closed or ctx ends, and counts itself in claims.
type fakeCopies struct {
	mu       sync.Mutex
	started  []string
	stopped  []string
	released []int64
	claimed  Copy
	claimErr error
	claims   int
	block    chan struct{}
	// ended counts the claims that returned, cancelled those that returned
	// because their context ended.
	ended, cancelled int
}

func (f *fakeCopies) Claim(ctx context.Context, _ registry.Task) (Copy, error) {
	f.mu.Lock()
	f.claims++
	block := f.block
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.ended++
		f.mu.Unlock()
	}()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.cancelled++
			f.mu.Unlock()
			return Copy{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return Copy{}, f.claimErr
	}
	return f.claimed, nil
}

// endedClaims are how many claims returned, and how many of them because
// their context ended.
func (f *fakeCopies) endedClaims() (ended, cancelled int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended, f.cancelled
}

func (f *fakeCopies) releases() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.released...)
}

func (f *fakeCopies) claimCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims
}

func (f *fakeCopies) Release(_ context.Context, t registry.Task, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, t.ID)
	return nil
}

func (f *fakeCopies) Free(context.Context) (int, error) { return 1, nil }

func (f *fakeCopies) StartServer(_ context.Context, c Copy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, c.Name)
	return nil
}

func (f *fakeCopies) StopServer(_ context.Context, c Copy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, c.Name)
	return nil
}

func (f *fakeCopies) Reconcile(context.Context) error { return nil }

// harness is an engine on a test registry, fake collaborators and a clock
// the test moves; no real process, socket or directory outside t.TempDir.
type harness struct {
	t      *testing.T
	ctx    context.Context
	clock  *storetest.Clock
	reg    *registry.Registry
	cfg    *config.Config
	rt     *fakeRuntime
	copies *fakeCopies
	layout paths.Layout
	e      *Engine
}

func newHarness(t *testing.T, edit ...func(d *Deps)) *harness {
	t.Helper()
	storetest.Parallel(t)
	clock := storetest.NewClock(t0)
	reg := registrytest.OpenWith(t, registry.Options{Clock: clock.Now})
	cfg := config.Defaults()
	h := &harness{t: t, ctx: t.Context(), clock: clock, reg: reg, cfg: cfg, rt: &fakeRuntime{}, copies: &fakeCopies{},
		layout: paths.Layout{Home: t.TempDir()}}
	d := Deps{Config: cfg, Layout: h.layout, Registry: reg, Runtime: h.rt, Copies: h.copies, Clock: clock.Now,
		Log: slog.New(slog.DiscardHandler)}
	for _, f := range edit {
		f(&d)
	}
	h.e = New(d)
	t.Cleanup(h.e.stopBackground) // no claim or overlap outlives the test
	return h
}

// task creates a task of kind bug, in state and at stage.
func (h *harness) task(key, state, stage string) registry.Task {
	h.t.Helper()
	t, err := h.reg.CreateTask(h.ctx, registry.Task{JiraKey: key, Kind: "bug", Repo: "example/shop", Title: key + " title",
		State: state, Stage: stage})
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

// stage makes fn the runner of every stage of type typ.
func (h *harness) stage(typ string, fn stageRunner) { h.e.runners[typ] = fn }

func (h *harness) tick() {
	h.t.Helper()
	_ = h.e.Tick(h.ctx) // a tick's errors are the steps' own, logged and recorded by them
}

func (h *harness) reload(id int64) registry.Task {
	h.t.Helper()
	t, err := h.reg.TaskByID(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

// events returns the task's events of kind (taskID 0: the daemon's own).
func (h *harness) events(taskID int64, kind string) []registry.Event {
	h.t.Helper()
	evs, err := h.reg.Events(h.ctx, taskID, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []registry.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// calls records which tasks a stage runner saw, in order, and when.
type calls struct {
	mu  sync.Mutex
	ids []int64
	at  []time.Time
}

func (c *calls) add(id int64, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, id)
	c.at = append(c.at, at)
}

func (c *calls) list() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.ids...)
}

func TestFactoryTickHandlesRequestsBeforeTasks(t *testing.T) {
	h := newHarness(t)
	stopMe := h.task("PR-12301", registry.TaskActive, "investigate")
	other := h.task("PR-12302", registry.TaskActive, "investigate")
	var seen calls
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		seen.add(t.ID, h.clock.Now())
		return false, nil
	})
	id, err := h.reg.EnqueueRequest(h.ctx, ReqStop, StopPayload{TaskID: stopMe.ID, Surface: "cli"})
	if err != nil {
		t.Fatal(err)
	}

	h.tick()

	req, err := h.reg.RequestByID(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != registry.RequestDone {
		t.Fatalf("stop request is %s (%s), want done", req.State, req.Result)
	}
	if got := h.reload(stopMe.ID).State; got != registry.TaskStopped {
		t.Fatalf("stopped task is %s", got)
	}
	if got := seen.list(); len(got) != 1 || got[0] != other.ID {
		t.Fatalf("the stage ran for %v, want only task %d: the stop came first", got, other.ID)
	}
	if got := h.rt.parked(); len(got) != 1 || got[0] != stopMe.ID {
		t.Fatalf("parked %v, want the stopped task's agents", got)
	}
}

func TestFactoryRunRefusesASecondDaemon(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(h.layout.State(), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, held, err := engine.AcquireLock(h.layout.FactoryLock())
	if err != nil || held {
		t.Fatalf("lock: held %v, %v", held, err)
	}
	defer unlock()
	if err := os.WriteFile(h.layout.FactoryPid(), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	procs := &fakeProcesses{table: map[int][2]string{4242: {"/opt/bin/magnum", "/opt/bin/magnum factory daemon"}}}
	h.e.procs = procs.processes()

	err = h.e.Run(h.ctx)
	if err == nil || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("Run = %v, want an error naming pid 4242", err)
	}
	if b, _ := os.ReadFile(h.layout.FactoryPid()); strings.TrimSpace(string(b)) != "4242" {
		t.Fatalf("the refused daemon rewrote the pidfile: %q", b)
	}
}

func TestTheRunLoopTicksWritesItsPidAndRemovesIt(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12303", registry.TaskActive, "investigate")
	ticked := make(chan struct{}, 8)
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		if t.ID == task.ID {
			select {
			case ticked <- struct{}{}:
			default:
			}
		}
		return false, nil
	})
	sigs := h.captureSignals()
	done := make(chan error, 1)
	go func() { done <- h.e.Run(h.ctx) }()
	select {
	case <-ticked:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never ticked")
	}
	b, err := os.ReadFile(h.layout.FactoryPid())
	if err != nil || strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pidfile %q (%v), want this process", b, err)
	}
	(<-sigs) <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after SIGTERM = %v, want nil (launchd must not restart a clean stop)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
	if _, err := os.Stat(h.layout.FactoryPid()); !os.IsNotExist(err) {
		t.Fatalf("the pidfile outlived the daemon: %v", err)
	}
}

// captureSignals replaces the engine's signal subscription: the channel Run
// subscribes arrives on the returned channel, for the test to send to.
func (h *harness) captureSignals() <-chan chan<- os.Signal {
	got := make(chan chan<- os.Signal, 1)
	h.e.notify = func(c chan<- os.Signal, _ ...os.Signal) { got <- c }
	h.e.stopNotify = func(chan<- os.Signal) {}
	return got
}

func TestAPassedStageMovesTheTaskToTheNextStage(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12304", registry.TaskActive, "investigate")
	h.stage(config.FactoryStageAgents, func(context.Context, registry.Task, config.FactoryStage) (bool, error) {
		return true, nil
	})

	h.tick()

	got := h.reload(task.ID)
	if got.Stage != "judge" || got.State != registry.TaskActive {
		t.Fatalf("after investigate passed the task is %s at %q, want active at judge", got.State, got.Stage)
	}
}

func TestATaskWithoutAStageStartsAtItsKindsFirstStage(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12305", registry.TaskActive, "")
	var seen []string
	h.stage(config.FactoryStageAgents, func(_ context.Context, _ registry.Task, st config.FactoryStage) (bool, error) {
		seen = append(seen, st.Name)
		return false, nil
	})

	h.tick()

	if len(seen) != 1 || seen[0] != "investigate" {
		t.Fatalf("ran stages %v, want investigate (the bug kind's first)", seen)
	}
	if got := h.reload(task.ID).Stage; got != "investigate" {
		t.Fatalf("stage %q, want investigate recorded", got)
	}
}

func TestObserveKeepsEachTasksEventsForItsNextAdvance(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12306", registry.TaskActive, "investigate")
	h.rt.events = []runtime.Event{{TaskID: task.ID, Role: "fx-cause", RunID: "r1", Kind: runtime.EventEnded}}
	h.rt.observeErr = errors.New("transcript: dial tcp: lookup api.example.com: no such host")

	h.e.observe(h.ctx)

	got := h.e.takeEvents(task.ID)
	if len(got) != 1 || got[0].RunID != "r1" || got[0].Kind != runtime.EventEnded {
		t.Fatalf("events %+v, want the ended run", got)
	}
	if again := h.e.takeEvents(task.ID); len(again) != 0 {
		t.Fatalf("events taken twice: %+v", again)
	}
	h.tick() // the failed read of the observe pass counts on the next tick
	if hd, held := h.e.holds[HoldInfra]; !held || hd.Reason != "DNS lookup failed" {
		t.Fatalf("hold %+v (%v), want a DNS hold from the observe pass", hd, held)
	}
}

func TestErrHeldIsNotAFailure(t *testing.T) {
	err := heldError{who: "disk", what: "9.0 GB free"}
	if !errors.Is(err, ErrHeld) || err.Error() != "disk: 9.0 GB free" {
		t.Fatalf("heldError %q is ErrHeld: %v", err.Error(), errors.Is(err, ErrHeld))
	}
}
