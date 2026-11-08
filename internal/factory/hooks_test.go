package factory

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/paths"
)

// UseHookSocket records the socket the panes are to name.
func (f *fakeRuntime) UseHookSocket(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hookSocket = path
	f.order = append(f.order, "hooks")
}

// Wake records the event and its context, then runs onWake.
func (f *fakeRuntime) Wake(ctx context.Context, ev agenthooks.Event) {
	f.mu.Lock()
	f.wakes = append(f.wakes, ev)
	f.wakeCtxs = append(f.wakeCtxs, ctx)
	f.order = append(f.order, "wake "+ev.Name)
	on := f.onWake
	f.mu.Unlock()
	if on != nil {
		on(ev)
	}
}

// WaitWakes records the call and runs onWaitWakes.
func (f *fakeRuntime) WaitWakes() {
	f.mu.Lock()
	f.waitWakes++
	f.order = append(f.order, "waitwakes")
	on := f.onWaitWakes
	f.mu.Unlock()
	if on != nil {
		on()
	}
}

// noteOrder adds what to the fake's order (a stage's pass, from a test).
func (f *fakeRuntime) noteOrder(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, what)
}

func (f *fakeRuntime) orderList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.order)
}

func (f *fakeRuntime) wakeList() []agenthooks.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.wakes)
}

func (f *fakeRuntime) observeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observes
}

func (f *fakeRuntime) usedSocket() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hookSocket
}

// shortDir is a temporary directory short enough for a Unix socket's path
// under it (t.TempDir's is too long on macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fh") //nolint:usetesting // t.TempDir's path is too long for a Unix socket's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// hookHarness is the harness on a layout whose hook socket fits a Unix
// socket's path (edit may change it), with the engine's log in the returned
// buffer, its signals captured, and a poll and an observe of an hour: only a
// hook makes an observe pass, and only the start or a kick a tick.
func hookHarness(t *testing.T, edit ...func(d *Deps)) (*harness, *syncBuffer) {
	t.Helper()
	dir, log := shortDir(t), &syncBuffer{}
	edits := append([]func(d *Deps){func(d *Deps) {
		d.Layout = paths.Layout{Home: dir}
		d.Log = slog.New(slog.NewTextHandler(log, nil))
	}}, edit...)
	h := newHarness(t, edits...)
	h.layout = h.deps.Layout
	h.cfg.Factory.Poll = config.Duration{Duration: time.Hour}
	h.cfg.Factory.Observe = config.Duration{Duration: time.Hour}
	h.captureSignals()
	return h, log
}

// runDaemon runs the harness's daemon (Run, which takes the lock) until stop
// or the test's end; stop returns Run's answer.
func (h *harness) runDaemon() (stop func() error) {
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan error, 1)
	go func() { done <- h.e.Run(ctx) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				h.t.Error("Run did not return after its context ended")
			}
		})
		return err
	}
	h.t.Cleanup(func() { _ = stop() })
	return stop
}

// passes has the agents stage of task note "tick" in the fake's order and
// signal each pass on the returned channel.
func (h *harness) passes(task registry.Task) <-chan struct{} {
	ch := make(chan struct{}, 16)
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		if t.ID == task.ID {
			h.rt.noteOrder("tick")
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		return false, nil
	})
	return ch
}

// waitPass waits for one pass of passes.
func waitPass(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("no tick: %s", what)
	}
}

// hookStop is a Stop event of pane w1:p1, as an agent's hook sends it.
var hookStop = agenthooks.Event{V: agenthooks.Version, Agent: agenthooks.AgentClaude, Name: agenthooks.NameStop, Pane: "w1:p1",
	SessionID: "4f1c2d3e-0000-4000-8000-000000000001"}

// The factory daemon opens its hook socket while it holds its lock and
// before its startup touches an agent: the socket is there while the daemon
// runs, and the runtime names it in the panes (UseHookSocket) before the
// startup's recovery, so every pane the daemon makes carries it. A second
// daemon, which the lock refuses, never touches the first one's socket.
func TestTheDaemonListensForHooksUnderItsLockBeforeItsStartup(t *testing.T) {
	h, _ := hookHarness(t)
	task := h.task("PR-12600", registry.TaskActive, "investigate")
	ticks := h.passes(task)
	h.runDaemon()
	waitPass(t, ticks, "Run never ticked")

	path := h.layout.FactoryHookSocket()
	if got := h.rt.usedSocket(); got != path {
		t.Fatalf("the panes name the hook socket %q, want %q", got, path)
	}
	if st, err := os.Lstat(path); err != nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("no socket at %s while the daemon runs: %v", path, err)
	}
	order := h.rt.orderList()
	if hooks, recovered := slices.Index(order, "hooks"), slices.Index(order, "recover PR-12600"); hooks < 0 || recovered < 0 || hooks > recovered {
		t.Fatalf("the calls ran %v: the panes must name the socket before the startup's recovery", order)
	}

	second := newHarness(t, func(d *Deps) { d.Layout = h.layout })
	procs := &fakeProcesses{table: map[int][2]string{os.Getpid(): {"/opt/bin/magnum", "/opt/bin/magnum factory daemon"}}}
	second.e.procs = procs.processes()
	if _, ok := errors.AsType[ErrRunning](second.e.Run(h.ctx)); !ok {
		t.Fatal("a second daemon ran beside the first")
	}
	if got := second.rt.usedSocket(); got != "" {
		t.Fatalf("the refused daemon set the hook socket %q", got)
	}
	if err := agenthooks.Send(path, hookStop); err != nil {
		t.Fatalf("the first daemon's socket stopped listening after a refused second daemon: %v", err)
	}
	waitFor(t, "the first daemon to take the event", func() bool { return len(h.rt.wakeList()) == 1 })
}

// A hook socket path that cannot be a Unix socket (104 bytes or more) or a
// listener that fails (a file that is no socket at its path, which is left
// alone) costs the daemon nothing but its hooks: no pane names a socket, one
// line says so, and the observe passes go on at their clock.
func TestWithoutAUsableHookSocketTheDaemonPollsAndSaysSoOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layout func(dir string) paths.Layout
		ready  func(t *testing.T, l paths.Layout)
	}{
		{"a path too long", func(dir string) paths.Layout {
			return paths.Layout{Home: dir, StateDir: filepath.Join(dir, strings.Repeat("s", 90))}
		}, func(*testing.T, paths.Layout) {}},
		{"a listener that fails", func(dir string) paths.Layout { return paths.Layout{Home: dir} }, func(t *testing.T, l paths.Layout) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(l.FactoryHookSocket()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(l.FactoryHookSocket(), []byte("not a socket\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortDir(t)
			h, log := hookHarness(t, func(d *Deps) { d.Layout = tc.layout(dir) })
			tc.ready(t, h.layout)
			h.cfg.Factory.Observe = config.Duration{Duration: 20 * time.Millisecond}
			h.task("PR-12601", registry.TaskActive, "investigate")
			h.runDaemon()
			waitFor(t, "an observe pass at its clock", func() bool { return h.rt.observeCount() >= 2 })

			if got := h.rt.usedSocket(); got != "" {
				t.Fatalf("the panes name the hook socket %q, which nothing listens on", got)
			}
			if n := strings.Count(log.String(), "listens for no agent hooks"); n != 1 {
				t.Fatalf("%d lines say the daemon listens for no hooks, want 1:\n%s", n, log.String())
			}
			if tc.name == "a listener that fails" {
				if b, err := os.ReadFile(h.layout.FactoryHookSocket()); err != nil || string(b) != "not a socket\n" {
					t.Fatalf("the file at the socket's path is %q (%v), want it left alone", b, err)
				}
			}
		})
	}
}

// An agent's hook event goes to the runtime's Wake with the daemon's
// context, which outlives the event (the later looks run on it) and ends
// with the daemon; an observe pass follows at once, not at its clock (an
// hour here), and the turn's end it returns kicks the tick, which takes it.
func TestAHookEventWakesThePaneThenObservesThenTicks(t *testing.T) {
	h, _ := hookHarness(t)
	task := h.task("PR-12602", registry.TaskActive, "investigate")
	ticks := h.passes(task)
	stop := h.runDaemon()
	waitPass(t, ticks, "Run never ticked")
	h.rt.mu.Lock()
	h.rt.onWake = func(agenthooks.Event) { // the look saw the turn end: the next pass returns it
		h.rt.setEvents([]runtime.Event{{TaskID: task.ID, Role: "fx-repro", RunID: "r1", Kind: runtime.EventEnded}})
	}
	h.rt.mu.Unlock()
	before := len(h.rt.orderList())

	if err := agenthooks.Send(h.layout.FactoryHookSocket(), hookStop); err != nil {
		t.Fatal(err)
	}
	waitPass(t, ticks, "the hook's event led to no tick: its stage waits for the poll")

	got := h.rt.orderList()[before:]
	want := []string{"wake stop", "observe", "tick"}
	if !slices.Equal(got[:min(len(got), 3)], want) {
		t.Fatalf("after the hook's event the daemon did %v, want %v", got, want)
	}
	h.rt.mu.Lock()
	ctx := h.rt.wakeCtxs[0]
	h.rt.mu.Unlock()
	if ctx.Err() != nil {
		t.Fatalf("Wake's context ended with the event (%v): the later looks need the daemon's", ctx.Err())
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("Wake's context outlived the daemon")
	}
}

// Events that come while an observe pass runs ask for one more pass, not one
// each: a pending request absorbs the others, as a kick does.
func TestABurstOfHookEventsMergesIntoOneObservePass(t *testing.T) {
	h, _ := hookHarness(t)
	task := h.task("PR-12603", registry.TaskActive, "investigate")
	ticks := h.passes(task)
	h.runDaemon()
	waitPass(t, ticks, "Run never ticked")
	entered, release := make(chan struct{}, 1), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce) // a failed test leaves no pass hanging
	var first atomic.Bool
	h.rt.mu.Lock()
	h.rt.onObserve = func() {
		if first.CompareAndSwap(false, true) {
			entered <- struct{}{}
			<-release
		}
	}
	h.rt.mu.Unlock()

	path := h.layout.FactoryHookSocket()
	if err := agenthooks.Send(path, hookStop); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the hook's event led to no observe pass")
	}
	// The burst goes through the listener's handler on this goroutine, so
	// each of its requests is in before the pass ends.
	for i := range 5 {
		ev := hookStop
		ev.Pane = "w1:p" + string(rune('2'+i))
		h.e.hookEvent(h.ctx, ev)
	}
	if n := len(h.rt.wakeList()); n != 6 {
		t.Fatalf("%d wakes, want the six events'", n)
	}
	releaseOnce()
	waitFor(t, "the burst's observe pass", func() bool { return h.rt.observeCount() >= 2 })
	time.Sleep(200 * time.Millisecond)
	if n := h.rt.observeCount(); n != 2 {
		t.Fatalf("%d observe passes for six events, five of them during the first pass: want 2", n)
	}
}

// `magnum doctor` pings the socket to see that it listens: a ping wakes no
// pane and asks for no observe pass, whether the listener drops it (it
// knows no ping) or hands it on.
func TestAHookPingIsDropped(t *testing.T) {
	h, _ := hookHarness(t)
	h.e.hookEvent(h.ctx, agenthooks.Event{V: agenthooks.Version, Agent: agenthooks.AgentClaude, Name: hookPing, Pane: "w1:p1"})
	if w := h.rt.wakeList(); len(w) != 0 {
		t.Fatalf("a ping woke %v", w)
	}
	select {
	case <-h.e.observeNow:
		t.Fatal("a ping asked for an observe pass")
	default:
	}

	task := h.task("PR-12604", registry.TaskActive, "investigate")
	ticks := h.passes(task)
	h.runDaemon()
	waitPass(t, ticks, "Run never ticked")
	path := h.layout.FactoryHookSocket()
	if err := agenthooks.Send(path, agenthooks.Event{V: agenthooks.Version, Name: hookPing}); err != nil {
		t.Fatal(err)
	}
	if err := agenthooks.Send(path, hookStop); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the Stop after the ping", func() bool { return len(h.rt.wakeList()) > 0 })
	if w := h.rt.wakeList(); len(w) != 1 || w[0].Name != agenthooks.NameStop {
		t.Fatalf("the ping and the Stop woke %v, want the Stop alone", w)
	}
}

// When the daemon stops, it waits for the later looks the events' Wake
// calls started (WaitWakes), after its listener stopped, so none outlives
// the stores; and no longer than what is left of the drain's budget, so a
// drain or `magnum factory install --drain` stays as fast as before.
func TestShutdownWaitsForTheHookLooksWithinTheDrain(t *testing.T) {
	t.Run("the looks end", func(t *testing.T) {
		h, _ := hookHarness(t)
		task := h.task("PR-12605", registry.TaskActive, "investigate")
		ticks := h.passes(task)
		var ended, listening atomic.Bool
		h.rt.onWaitWakes = func() {
			// The listener stopped first: an event sent now finds no reader.
			listening.Store(agenthooks.Send(h.layout.FactoryHookSocket(), hookStop) == nil)
			time.Sleep(300 * time.Millisecond)
			ended.Store(true)
		}
		stop := h.runDaemon()
		waitPass(t, ticks, "Run never ticked")
		if err := stop(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		if !ended.Load() {
			t.Fatal("Run returned before the hook looks it waited for ended")
		}
		if got := h.rt.orderList(); len(slices.DeleteFunc(slices.Clone(got), func(c string) bool { return c != "waitwakes" })) != 1 {
			t.Fatalf("the calls ran %v, want one WaitWakes", got)
		}
		if listening.Load() {
			t.Fatal("the hook listener still read events while the daemon waited for the looks")
		}
		if w := h.rt.wakeList(); len(w) != 0 {
			t.Fatalf("an event sent during the wait woke %v", w)
		}
	})
	t.Run("the looks outlive the drain", func(t *testing.T) {
		h, log := hookHarness(t)
		task := h.task("PR-12606", registry.TaskActive, "investigate")
		ticks := h.passes(task)
		hang := make(chan struct{})
		t.Cleanup(func() { close(hang) })
		h.rt.onWaitWakes = func() { <-hang }
		stop := h.runDaemon()
		waitPass(t, ticks, "Run never ticked")
		h.e.drainAt.Store(time.Now().Add(-drainBudget + 300*time.Millisecond).UnixNano()) // 300 ms of the budget left
		began := time.Now()
		if err := stop(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		if took := time.Since(began); took > 5*time.Second {
			t.Fatalf("the daemon waited %s for hook looks that never end: past the drain's budget", took)
		}
		if !strings.Contains(log.String(), "hook looks did not end within the drain") {
			t.Fatalf("the log does not say the hook looks were cut:\n%s", log.String())
		}
	})
}
