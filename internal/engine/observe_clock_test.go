package engine

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// clockGuard bounds a channel wait that only a missing pass, wake or log
// line would make long: a test's verdict never depends on it.
const clockGuard = 20 * time.Second

// observeSteps is the observe clock's wait (Deps.ObserveSleep) under the
// test's hand: each wait says how long it is (waits) and lasts until the
// test steps it (step) or the clock's context ends.
type observeSteps struct {
	t       *testing.T
	waits   chan time.Duration
	release chan struct{}
	calls   atomic.Int32
}

func newObserveSteps(t *testing.T) *observeSteps {
	t.Helper()
	return &observeSteps{t: t, waits: make(chan time.Duration), release: make(chan struct{})}
}

func (s *observeSteps) sleep(ctx context.Context, d time.Duration) error {
	s.calls.Add(1)
	select {
	case s.waits <- d:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// next is the clock's next wait, once it began.
func (s *observeSteps) next() time.Duration {
	s.t.Helper()
	select {
	case d := <-s.waits:
		return d
	case <-time.After(clockGuard):
		s.t.Fatal("the observe clock never waited for its next pass")
		return 0
	}
}

// pass ends the wait the clock is in (after next) and waits until its pass
// is over and the next wait began, which it returns.
func (s *observeSteps) pass() time.Duration {
	s.t.Helper()
	select {
	case s.release <- struct{}{}:
	case <-time.After(clockGuard):
		s.t.Fatal("the observe clock was not waiting")
	}
	return s.next()
}

// goClock runs the observe clock every interval until the test ends.
func (h *harness) goClock(every time.Duration) {
	ctx, cancel := context.WithCancel(h.ctx)
	h.t.Cleanup(cancel)
	h.runs.Go(func() { h.e.observeClock(ctx, every) })
}

// prInFlight gives PR #n, which the first tick takes as a baseline, a
// live session of role whose agent is name in pane, and a run of it at
// work; it returns the run.
func (h *harness) prInFlight(n int, role, name, pane, cwd string) store.Run {
	h.t.Helper()
	h.open(prSpec{n: n, head: "head111"})
	h.startup()
	h.tick() // first sync: a baseline, which no round reviews
	pr := h.pr(n)
	s, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: role, AgentName: new(name), AgentKind: new(agents.KindClaude),
		HerdrWorkspaceID: new("w1"), HerdrPaneID: new(pane), Cwd: new(cwd), State: store.SessionLive,
		StartedAt: h.clock.Now().Add(-time.Hour)})
	if err != nil {
		h.t.Fatal(err)
	}
	r, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: 1, Role: role, Kind: store.RunInitial, TargetSHA: "head111",
		Identity: "zhuravel", State: store.RunWorking, SessionID: &s.ID})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *fakeHerdr) snapshots() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// realAgents gives the harness agents.Manager on eh, the herdr the test
// drives, with the harness's clock.
func realAgents(hs *harness, eh *evalHerdr) {
	run := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"zsh", "-ic", "whence -w claude"}, Result: execx.Result{Stdout: []byte("claude: function\n")}},
	}}
	cli := hs.t.TempDir()
	hs.d.Herdr = eh
	hs.d.Agents = agents.New(agents.Deps{Herdr: eh, Store: hs.st, Runner: run, Config: hs.cfg, Layout: hs.layout,
		Clock: hs.clock.Now, CodexConfig: filepath.Join(cli, "codex.toml"), ClaudeConfig: filepath.Join(cli, "claude.json"),
		ClaudeSettings: filepath.Join(cli, "settings.json"), ClaudeDir: cli, CodexHome: cli,
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }})
}

// The live numbers (2026-11-07): an agent stopped and the review daemon
// saw it 72 s later (median, n=1247), since the observation ran inside the
// GitHub poll's tick (every ~47 s) and needed two idle readings. The
// observe clock reads herdr every observe_interval between the ticks, and
// takes the second reading CompletionGap after the first: an agent that
// stops right after a pass has its run ended one interval and the gap
// later, with no tick.
func TestTheObserveClockEndsARunOneIntervalAndTheGapAfterTheStop(t *testing.T) {
	steps := newObserveSteps(t)
	eh := newEvalHerdr()
	cwd := t.TempDir()
	h := newHarness(t, func(hs *harness) {
		realAgents(hs, eh)
		hs.d.ObserveSleep = steps.sleep
	})
	ws := eh.addWorkspace("PR #1", cwd)
	pane := ws + ":p1"
	const name = "mg-1-claude-review-abc123"
	eh.addAgent(pane, name, agents.KindClaude, herdr.StatusWorking)
	r := h.prInFlight(1, config.RoleClaudeReview, name, pane, cwd)
	every := 5 * time.Second
	h.goClock(every)

	if d := steps.next(); d != every {
		t.Fatalf("first wait = %s, want %s", d, every)
	}
	h.clock.Add(every)
	if d := steps.pass(); d != every { // working: nothing due
		t.Fatalf("wait after a working reading = %s, want %s", d, every)
	}
	stop := h.clock.Now()
	eh.setStatus(config.RoleClaudeReview, herdr.StatusIdle)
	h.clock.Add(every)
	d := steps.pass() // the first idle reading
	if d != agents.CompletionGap {
		t.Fatalf("wait after the first idle reading = %s, want the gap %s", d, agents.CompletionGap)
	}
	if run, _ := h.st.RunByID(h.ctx, r.ID); run.State != store.RunWorking {
		t.Fatalf("run after one idle reading = %s, want working", run.State)
	}
	h.clock.Add(d)
	if d := steps.pass(); d != every {
		t.Fatalf("wait after the run ended = %s, want %s", d, every)
	}
	run, err := h.st.RunByID(h.ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := stop.Add(every + agents.CompletionGap); run.State != store.RunEnded || run.EndedAt == nil || !run.EndedAt.Equal(want) {
		t.Fatalf("run = %s ended %v, want ended at %v (stop + %s + %s)", run.State, run.EndedAt, want, every, agents.CompletionGap)
	}
}

// The observe pass costs a herdr snapshot: with no run in flight there is
// no turn to end, and an idle daemon makes no socket call for it.
func TestNoObservePassRunsWithNoRunInFlight(t *testing.T) {
	steps := newObserveSteps(t)
	h := newHarness(t, func(hs *harness) { hs.d.ObserveSleep = steps.sleep })
	h.goClock(5 * time.Second)
	steps.next()
	for range 3 {
		steps.pass()
	}
	if n, calls := h.ag.count("observe"), h.hd.snapshots(); n != 0 || calls != 0 {
		t.Fatalf("with no run in flight: %d observe passes, %d herdr snapshots; want none", n, calls)
	}
	h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "p1", t.TempDir())
	passes, calls := h.ag.count("observe"), h.hd.snapshots() // the tick's
	steps.pass()
	if n, c := h.ag.count("observe")-passes, h.hd.snapshots()-calls; n != 1 || c != 1 {
		t.Fatalf("with a run in flight: %d observe passes, %d herdr snapshots; want one each", n, c)
	}
}

// The daemon runs the clock beside its ticks and stops it with them.
func TestTheDaemonObservesBetweenItsTicks(t *testing.T) {
	steps := newObserveSteps(t)
	h := newHarness(t, func(hs *harness) { hs.d.ObserveSleep = steps.sleep })
	h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "p1", t.TempDir())
	before := h.ag.count("observe")
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true})
	if d := steps.next(); d != h.cfg.Daemon.ObserveInterval.Duration {
		t.Fatalf("the clock waits %s, want observe_interval %s", d, h.cfg.Daemon.ObserveInterval.Duration)
	}
	for deadline := time.Now().Add(clockGuard); h.ag.count("observe") <= before; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first tick never observed")
		}
	}
	ticks := h.ag.count("observe") // with the first tick's (the next one is a poll interval away)
	for range 3 {
		steps.pass()
	}
	if n := h.ag.count("observe") - ticks; n != 3 {
		t.Fatalf("observe passes between the ticks = %d, want 3", n)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(clockGuard):
		t.Fatal("Run did not stop")
	}
}

// With observe_interval "0", and in --once, the tick observes alone, as
// before the clock: Run never starts it.
func TestTheTickObservesAloneWithTheClockOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  func(*harness)
		opts Options
	}{
		{"observe_interval 0", func(hs *harness) { hs.cfg.Daemon.ObserveInterval = config.Duration{} }, Options{NoSignals: true}},
		{"once", func(*harness) {}, Options{NoSignals: true, Once: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := newObserveSteps(t)
			h := newHarness(t, func(hs *harness) {
				hs.d.ObserveSleep = steps.sleep
				tc.off(hs)
			})
			h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "p1", t.TempDir())
			before := h.ag.count("observe")
			ctx, cancel := context.WithCancel(h.ctx)
			done := h.goRun(ctx, tc.opts)
			for deadline := time.Now().Add(clockGuard); h.ag.count("observe") <= before; time.Sleep(2 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the tick never observed")
				}
			}
			cancel()
			select {
			case <-done:
			case <-time.After(clockGuard):
				t.Fatal("Run did not stop")
			}
			if n := steps.calls.Load(); n != 0 {
				t.Fatalf("the observe clock waited %d times; want it never started", n)
			}
		})
	}
}

// A dry run changes nothing outside the store and observes nothing (its
// tick reads herdr only for the Codex count), and an engine without herdr
// or agents has nothing to observe: none starts the clock.
func TestTheObserveClockIsOffWhereNothingIsObserved(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*harness)
		want time.Duration
	}{
		{"the daemon", func(*harness) {}, 5 * time.Second},
		{"a dry run", func(hs *harness) { hs.d.DryRun = true }, 0},
		{"no herdr", func(hs *harness) { hs.d.Herdr = nil }, 0},
		{"no agents", func(hs *harness) { hs.d.Agents = nil }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mod)
			if got := h.e.observeClockEvery(); got != tc.want {
				t.Fatalf("observe clock = %s, want %s", got, tc.want)
			}
		})
	}
}

// holdAgents holds the observe pass the test arms inside its
// ObserveSnapshotAt or ObserveRunsAt until released, and counts any pass
// that enters while one is held.
type holdAgents struct {
	Agents
	armed    atomic.Bool
	held     chan struct{} // an armed pass is held
	release  chan struct{}
	inside   atomic.Int32
	overlaps chan struct{} // a pass entered while another was inside
	passes   int           // unguarded on purpose: -race flags passes that are not serialized
}

func (a *holdAgents) ObserveSnapshotAt(ctx context.Context, snap herdr.Snapshot, at time.Time) ([]agents.Observation, error) {
	defer a.enter()()
	return a.Agents.ObserveSnapshotAt(ctx, snap, at)
}

// ObserveRunsAt is the observe clock's pass, held and counted as
// ObserveSnapshotAt is.
func (a *holdAgents) ObserveRunsAt(ctx context.Context, snap herdr.Snapshot, at time.Time) ([]agents.Observation, error) {
	defer a.enter()()
	return a.Agents.ObserveRunsAt(ctx, snap, at)
}

// enter counts a pass, holds it when armed, and returns its leave.
func (a *holdAgents) enter() func() {
	if a.inside.Add(1) > 1 {
		select {
		case a.overlaps <- struct{}{}:
		default:
		}
	}
	a.passes++
	if a.armed.CompareAndSwap(true, false) {
		a.held <- struct{}{}
		<-a.release
	}
	return func() { a.inside.Add(-1) }
}

// logSignal is a logger whose records of msg are signalled on seen.
type logSignal struct {
	slog.Handler
	msg  string
	seen chan slog.Record
}

func (l logSignal) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == l.msg {
		select {
		case l.seen <- r:
		default:
		}
	}
	return l.Handler.Handle(ctx, r)
}

func (l logSignal) WithAttrs(as []slog.Attr) slog.Handler {
	return logSignal{Handler: l.Handler.WithAttrs(as), msg: l.msg, seen: l.seen}
}

func (l logSignal) WithGroup(name string) slog.Handler {
	return logSignal{Handler: l.Handler.WithGroup(name), msg: l.msg, seen: l.seen}
}

// The observe clock's pass and the tick's touch the same session and run
// rows and the same herdr panes: the tick's observe waits for the clock's
// pass in flight (and says so in the debug log) instead of applying a
// snapshot beside it.
func TestTheTickAndTheObserveClockNeverObserveAtOnce(t *testing.T) {
	steps := newObserveSteps(t)
	hold := &holdAgents{held: make(chan struct{}), release: make(chan struct{}), overlaps: make(chan struct{}, 1)}
	waits := make(chan slog.Record, 1)
	h := newHarness(t, func(hs *harness) {
		hs.d.ObserveSleep = steps.sleep
		hold.Agents = hs.ag
		hs.d.Agents = hold
		hs.d.Logger = slog.New(logSignal{Handler: hs.d.Logger.Handler(), msg: observeWaitsMsg, seen: waits})
	})
	var released sync.Once
	unhold := func() { released.Do(func() { close(hold.release) }) }
	t.Cleanup(unhold) // a failure below leaves no pass held
	h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "p1", t.TempDir())
	base := hold.passes // the seeding tick's
	h.goClock(5 * time.Second)
	steps.next()

	hold.armed.Store(true)
	go func() { steps.release <- struct{}{} }()
	select {
	case <-hold.held:
	case <-time.After(clockGuard):
		t.Fatal("the clock's pass never observed")
	}
	tick := make(chan error, 1)
	go func() { tick <- h.e.Tick(h.ctx) }()
	select {
	case r := <-waits:
		pass := ""
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "pass" {
				pass = a.Value.String()
			}
			return true
		})
		if pass != "the tick" {
			t.Fatalf("the pass that waited = %q, want the tick", pass)
		}
	case <-hold.overlaps:
		t.Fatal("the tick observed while the clock's pass was in flight")
	case err := <-tick:
		t.Fatalf("the tick ended (%v) while the clock's pass was in flight", err)
	case <-time.After(clockGuard):
		t.Fatal("the tick neither waited for the clock's pass nor observed")
	}
	unhold()
	steps.next()
	select {
	case err := <-tick:
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
	case <-time.After(clockGuard):
		t.Fatal("the tick never ended after the clock's pass")
	}
	h.settle()
	select {
	case <-hold.overlaps:
		t.Fatal("two observe passes ran at once")
	default:
	}
	if n := hold.passes - base; n != 2 {
		t.Fatalf("observe passes = %d, want the clock's and the tick's", n)
	}

	// Many of both, under -race: an unserialized pass is a data race on
	// passes.
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 5 {
			steps.pass()
		}
	})
	for range 5 {
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	wg.Wait()
	h.settle()
	if n := hold.passes - base; n != 12 {
		t.Fatalf("observe passes = %d, want 12", n)
	}
}

// slowHerdr makes each snapshot take took on the harness's clock.
type slowHerdr struct {
	*fakeHerdr
	clock *storetest.Clock
	took  time.Duration
}

func (h *slowHerdr) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	h.clock.Add(h.took)
	return h.fakeHerdr.Snapshot(ctx)
}

// Each pass logs its duration at debug level; one that took longer than
// the interval warns, at most once a minute (a loaded machine would warn
// every 5 s otherwise).
func TestASlowObservePassWarnsAtMostOnceAMinute(t *testing.T) {
	steps := newObserveSteps(t)
	logs := &recordLog{}
	h := newHarness(t, func(hs *harness) {
		hs.d.ObserveSleep = steps.sleep
		hs.d.Herdr = &slowHerdr{fakeHerdr: hs.hd, clock: hs.clock, took: 2 * time.Second}
		hs.d.Logger = slog.New(logs)
	})
	h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "p1", t.TempDir())
	slow := h.d.Herdr.(*slowHerdr)
	h.goClock(5 * time.Second)
	steps.next()
	steps.pass()
	if n := logs.count(slog.LevelDebug, observePassMsg); n != 1 {
		t.Fatalf("debug lines of a pass = %d, want 1", n)
	}
	if n := logs.count(slog.LevelWarn, observeSlowMsg); n != 0 {
		t.Fatalf("a 2s pass warned %d times, want none", n)
	}
	slow.took = 6 * time.Second
	for range 3 { // 18 s of slow passes
		steps.pass()
	}
	if n := logs.count(slog.LevelWarn, observeSlowMsg); n != 1 {
		t.Fatalf("three slow passes within a minute warned %d times, want once", n)
	}
	h.clock.Add(time.Minute)
	steps.pass()
	if n := logs.count(slog.LevelWarn, observeSlowMsg); n != 2 {
		t.Fatalf("a slow pass a minute later warned %d times in all, want twice", n)
	}
}

// recordLog keeps every record's level and message.
type recordLog struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *recordLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *recordLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, r.Clone())
	return nil
}
func (l *recordLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *recordLog) WithGroup(string) slog.Handler      { return l }

func (l *recordLog) count(level slog.Level, msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.recs {
		if r.Level == level && r.Message == msg {
			n++
		}
	}
	return n
}

// The clock's pass observes the sessions with a run in flight alone
// (agents.ObserveRunsAt): a pass over every live session writes each one's
// row, which the tick does every poll and the clock would do every few
// seconds, mostly for agents with nothing to do. A dialog the clock's pass
// sees is the tick's to report: at the clock's pace its warning, urgent
// toast and reveal would come every few seconds.
func TestTheObserveClockReadsTheRunsInFlightAndLeavesDialogsToTheTick(t *testing.T) {
	steps := newObserveSteps(t)
	logs := &recordLog{}
	h := newHarness(t, func(hs *harness) {
		hs.d.ObserveSleep = steps.sleep
		hs.d.Logger = slog.New(logs)
	})
	r := h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "p1", t.TempDir())
	s, err := h.st.SessionByID(h.ctx, *r.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	blocked := agents.Observation{Kind: agents.ObsBlocked, Role: agents.Role(config.RoleClaudeReview), PRID: r.PRID, Session: s, Run: &r}
	const dialog = "agent waits on a dialog"
	full, runs := h.ag.count("observe"), h.ag.count("observe runs")
	h.goClock(5 * time.Second)
	steps.next()
	h.ag.queueObs(blocked)
	steps.pass()
	if n, m := h.ag.count("observe runs")-runs, h.ag.count("observe")-full; n != 1 || m != 1 {
		t.Fatalf("the clock's pass: %d passes over the runs in flight, %d in all; want the one over the runs", n, m)
	}
	if n := logs.count(slog.LevelWarn, dialog); n != 0 {
		t.Fatalf("the clock's pass warned of the dialog %d times, want none", n)
	}
	h.ag.queueObs(blocked)
	h.tick()
	if n := logs.count(slog.LevelWarn, dialog); n != 1 {
		t.Fatalf("the tick's pass warned of the dialog %d times, want once", n)
	}
}
