package factory

// The factory daemon's loop (docs/superpowers/specs/2026-11-03-factory-design.md,
// sections 5, 10 and 10.1): one process per home under its own launchd job,
// holding State/factory.lock for its whole life. Each tick reads the holds
// of the machine (herdr, disk, memory, the infrastructure), answers the
// CLI's and the screen's requests, then advances each live task one step,
// each under a panic guard of its own. An observe pass between the ticks
// reads the agents' panes. The engine takes every collaborator as an
// interface or a function field (Deps), so the tests run it on fakes and a
// fake clock; a nil collaborator is a part a build does not have yet, and
// the steps that need it wait.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	ossignal "os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// LaunchLabel is the launchd label of the factory daemon's LaunchAgent,
// apart from the review daemon's (launchd.DefaultLabel), so either restarts
// without the other.
const LaunchLabel = "zhuravel.magnum.factory"

// Runtime is the factory's agent runtime (internal/factory/runtime): the
// owner row of a task in the factory's agents store, the launch of a
// stage's roles (recorded before any workspace exists), the prompts it
// sends through its ledger, the panes it observes and the watchdog over
// them, the verification of a run's end, parking a task's sessions, the
// reconcile of a prompt of unknown delivery, the recovery of a lost pane,
// and the shift of every turn deadline after the Mac slept.
type Runtime interface {
	Owner(ctx context.Context, t registry.Task) (store.PR, error)
	Launch(ctx context.Context, t registry.Task, a registry.Attempt, copyPath string, roles []config.Role) error
	Send(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, text string) (runID string, err error)
	Observe(ctx context.Context) ([]runtime.Event, error)
	Verify(ctx context.Context, runID string, ok bool, why string) error
	Park(ctx context.Context, t registry.Task) error
	Reconcile(ctx context.Context, e registry.LedgerEntry) (string, error)
	Watchdog(ctx context.Context, now time.Time) ([]runtime.Event, error)
	RecoverLost(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role) error
	LetGo(ctx context.Context, t registry.Task, role string) (int, error)
	Shift(gap time.Duration)
}

// The runtime the daemon wires (internal/factory/runtime) is the engine's.
var _ Runtime = (*runtime.Runtime)(nil)

// Copy is a task's copy of a repository: its name and path, its number
// (copy {n}), the port of its server and its environment.
type Copy struct {
	Name, Path   string
	Number, Port int
	Env          map[string]string
}

// Copies are the repositories' copies the tasks work in: a claim for a
// task (the copy it used last first), its release, how many are free, the
// copy's local server, and the reconcile of servers and copies after a
// restart.
type Copies interface {
	Claim(ctx context.Context, t registry.Task) (Copy, error)
	Release(ctx context.Context, t registry.Task, reason string) error
	Free(ctx context.Context) (int, error)
	StartServer(ctx context.Context, c Copy) error
	StopServer(ctx context.Context, c Copy) error
	Reconcile(ctx context.Context) error
}

// Jira reads a task's ticket, and searches issues by the words of a free
// prompt.
type Jira interface {
	Ticket(ctx context.Context, key string) (jira.Ticket, error)
	Search(ctx context.Context, words string, limit int) ([]jira.Ticket, error)
}

// PRView is a pull request as the factory reads it back after it opened it.
type PRView struct {
	Number                     int
	URL, Author, Base, HeadSHA string
	Draft                      bool
}

// GitHub opens a task's PR (ready for review, never a draft) and reads it
// back, by its branch or its number.
type GitHub interface {
	CreatePR(ctx context.Context, repo, base, branch, title, bodyFile string) (PRView, error)
	PRByBranch(ctx context.Context, repo, branch string) (PRView, bool, error)
	ViewPR(ctx context.Context, repo string, number int) (PRView, error)
}

// Magnum is the review daemon as the factory's local review sees it: busy
// while a real round is in flight or due, so a local round yields to it.
type Magnum interface {
	Busy(ctx context.Context) (busy bool, why string, err error)
}

// LocalReviewer runs one round of magnum's review on a task's change in its
// copy, as a blind dry run (evalrun.LocalCase).
type LocalReviewer interface {
	Round(ctx context.Context, c evalrun.LocalCase) (evalrun.LocalResult, error)
}

// Deps are the engine's collaborators. Config, Layout and Registry are
// required; any other may be nil, and what needs it waits (a collaborator
// a build does not wire yet) or is skipped (a function field: no memory
// read, no disk check, no MySQL ping, herdr taken as up, no reveal).
type Deps struct {
	Config   *config.Config
	Layout   paths.Layout
	Registry *registry.Registry
	Runtime  Runtime
	Copies   Copies
	Jira     Jira
	GitHub   GitHub
	Magnum   Magnum
	Review   LocalReviewer
	Git      *gitx.Client
	// Memory reads the machine's memory (engine.ReadMemory); Disk the free
	// bytes of the filesystem that holds path.
	Memory func(ctx context.Context) engine.MemorySample
	Disk   func(path string) (free uint64, err error)
	// MySQL pings the copies' database server; HerdrUp asks the factory's
	// herdr session whether it answers; Reveal opens a terminal on it
	// (`herdr --session <herdr_session>`, internal/reveal).
	MySQL   func(ctx context.Context) error
	HerdrUp func(ctx context.Context) error
	Reveal  func(ctx context.Context) error
	// Clock returns the current time; nil means time.Now.
	Clock func() time.Time
	Log   *slog.Logger
}

// stageRunner runs one pass of a stage of a task; done reports that the
// stage's end fact holds, and the task moves to the next stage.
type stageRunner func(ctx context.Context, t registry.Task, st config.FactoryStage) (done bool, err error)

// Engine is the factory daemon's loop. New builds it, Run runs it until a
// stop signal, and Tick is one pass of it.
type Engine struct {
	d   Deps
	cfg *config.Config
	reg *registry.Registry
	log *slog.Logger

	// runners run a stage by its type (config.FactoryStage*); a type
	// without one waits for the build that brings it.
	runners map[string]stageRunner

	kick     chan struct{}
	draining atomic.Bool

	// Seams: the signal subscription, and the process table that names the
	// daemon that holds the lock.
	notify     func(c chan<- os.Signal, sig ...os.Signal)
	stopNotify func(c chan<- os.Signal)
	procs      engine.Processes

	// What follows is the loop's own: Run's goroutine (or a test's) reads
	// and writes it, except the heavy slots (heavyMu), which a stage's
	// goroutine may release.
	tickAt    time.Time // when the current tick started
	tried     bool      // the current tick advanced the tasks: a try of the infrastructure hold
	lastAlive time.Time // when the last tick or observe pass ended (wall clock)
	uncounted bool      // a wake: this tick's failures count none (wake)
	panics    map[int64]int
	lastFail  map[int64]string
	readFails []error
	inbox     map[int64][]runtime.Event
	holds     map[string]*Hold
	blockers  map[string]string
	recent    map[string]recentFailure
	// lastReveal is when a herdr hold last opened herdr's session.
	lastReveal time.Time
	parked     map[int64]bool
	servers    map[int64]server

	heavyMu   sync.Mutex
	heavyHeld map[int64]*heavySlot
	heavySeq  int
}

// New builds the engine over d.
func New(d Deps) *Engine {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Engine{
		d: d, cfg: d.Config, reg: d.Registry, log: log,
		runners:    map[string]stageRunner{},
		kick:       make(chan struct{}, 1),
		notify:     ossignal.Notify,
		stopNotify: ossignal.Stop,
		procs:      engine.SystemProcesses(),
		panics:     map[int64]int{},
		lastFail:   map[int64]string{},
		inbox:      map[int64][]runtime.Event{},
		holds:      map[string]*Hold{},
		blockers:   map[string]string{},
		recent:     map[string]recentFailure{},
		parked:     map[int64]bool{},
		servers:    map[int64]server{},
		heavyHeld:  map[int64]*heavySlot{},
	}
}

func (e *Engine) now() time.Time {
	if e.d.Clock != nil {
		return e.d.Clock()
	}
	return time.Now()
}

// poll is the interval of the main tick ([factory] poll; config keeps it
// positive), observe the interval of the observe pass.
func (e *Engine) poll() time.Duration {
	if d := e.cfg.Factory.Poll.Duration; d > 0 {
		return d
	}
	return 45 * time.Second
}

func (e *Engine) observeEvery() time.Duration {
	if d := e.cfg.Factory.Observe.Duration; d > 0 {
		return d
	}
	return 10 * time.Second
}

// Kick asks the loop for a tick now (SIGUSR1, `magnum factory start` and
// the screen's requests); a kick already pending absorbs it.
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// ErrRunning is Run's answer when another factory daemon holds the lock:
// PID is that daemon's, from its pidfile (0 when it could not be read).
type ErrRunning struct {
	PID  int
	Lock string
}

// Error implements error.
func (e ErrRunning) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("factory: a factory daemon already runs (pid %d)", e.PID)
	}
	return "factory: a factory daemon already holds " + e.Lock
}

// Lock takes the factory daemon's lock (Layout.FactoryLock), held for the
// daemon's whole life; another daemon holding it is ErrRunning, naming that
// daemon's pid. The daemon command takes it before it opens its log or a
// store, so a second daemon touches neither (RunLocked).
func Lock(l paths.Layout) (unlock func(), err error) {
	return lockWith(l, engine.SystemProcesses())
}

func lockWith(l paths.Layout, procs engine.Processes) (unlock func(), err error) {
	lock := l.FactoryLock()
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return nil, fmt.Errorf("factory: state dir: %w", err)
	}
	unlock, held, err := engine.AcquireLock(lock)
	if err != nil {
		return nil, fmt.Errorf("factory: lock %s: %w", lock, err)
	}
	if held {
		pid, _ := procs.PidFromFile(l.FactoryPid())
		return nil, ErrRunning{PID: pid, Lock: lock}
	}
	return unlock, nil
}

// Run takes the factory lock (Lock; another daemon holding it is
// ErrRunning) and runs the daemon (RunLocked) until it stops.
func (e *Engine) Run(ctx context.Context) error {
	unlock, err := lockWith(e.d.Layout, e.procs)
	if err != nil {
		return err
	}
	defer unlock()
	return e.RunLocked(ctx)
}

// RunLocked runs the daemon, whose lock the caller holds (Lock), until
// SIGTERM or SIGINT, or until ctx ends: it writes the pidfile, runs startup,
// then a tick at once, every [factory] poll and on SIGUSR1, and an observe
// pass every [factory] observe. A stop signal drains: no new launch or
// effect starts, the tick in progress ends, and it returns nil, so launchd
// (KeepAlive on a failed exit only) starts the daemon again only after a
// crash.
func (e *Engine) RunLocked(ctx context.Context) error {
	pidFile := e.d.Layout.FactoryPid()
	if err := fsx.WriteFileAtomic(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("factory: pidfile: %w", err)
	}
	defer os.Remove(pidFile)

	stop := make(chan struct{})
	sigs := make(chan os.Signal, 4)
	e.notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	defer e.stopNotify(sigs)
	sigCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.signals(sigCtx, sigs, stop)

	e.log.Info("magnum factory daemon starting", "pid", os.Getpid())
	if err := e.startup(ctx); err != nil {
		e.log.Error("magnum factory daemon refusing to start", "err", err)
		return err
	}
	tick := time.NewTimer(0)
	defer tick.Stop()
	observe := time.NewTimer(e.observeEvery())
	defer observe.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-stop:
			e.log.Info("magnum factory daemon stopped")
			return nil
		case <-observe.C:
			e.observe(ctx)
			observe.Reset(e.observeEvery())
			continue
		case <-tick.C:
		case <-e.kick:
			tick.Stop()
		}
		if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
			e.log.Warn("factory tick", "err", err)
		}
		if e.draining.Load() {
			e.log.Info("magnum factory daemon drained; stopping")
			return nil
		}
		tick.Reset(e.poll())
	}
}

// signals answers the daemon's signals: SIGUSR1 is a kick, SIGTERM and
// SIGINT start the drain (no new launch or effect) and close stop, which
// ends Run between ticks; a tick in progress ends first.
func (e *Engine) signals(ctx context.Context, sigs <-chan os.Signal, stop chan<- struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-sigs:
			if s == syscall.SIGUSR1 {
				e.Kick()
				continue
			}
			e.log.Info("draining: no new launch or effect; the tick in progress ends", "signal", s.String())
			e.draining.Store(true)
			close(stop)
			return
		}
	}
}

// startup runs once before the first tick. It counts the heavy stages
// whose attempts still run (heavy); the recovery of what a stopped daemon
// left comes on top of it.
func (e *Engine) startup(ctx context.Context) error {
	return e.adoptHeavy(ctx)
}

// liveStates are the states of a task the loop advances.
var liveStates = []string{registry.TaskActive, registry.TaskWaiting}

// Tick runs one pass of the loop: the holds (herdr, disk and memory, then
// the infrastructure hold and a MySQL ping before a pass that advances),
// the requests, then each live task's advance under its own panic guard
// (safely). During an infrastructure or a herdr hold no task advances
// except on the hold's tries; a failure that a hold covers costs its task
// nothing. The returned error joins what failed, for the log; each step
// records its own.
func (e *Engine) Tick(ctx context.Context) error {
	now := e.now()
	e.tickAt = now
	e.wake(now)
	defer e.alive()
	var errs []error

	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		return fmt.Errorf("factory tick: %w", err)
	}
	e.sweepHeavy(ctx, tasks)
	herdrUp := e.checkHerdr(ctx, now)
	e.pressure(ctx, now, tasks)
	infra, held := e.holds[HoldInfra]
	advance := herdrUp && (!held || !now.Before(infra.Next))
	e.tried = advance // a pass that advances is the infrastructure hold's try (hold)
	var failures []error
	if advance && e.d.MySQL != nil && len(live(tasks)) > 0 {
		if err := e.d.MySQL(ctx); err != nil {
			failures = append(failures, mysqlFailure{err})
			advance = false
		}
	}

	e.handleRequests(ctx)

	if advance {
		if tasks, err = e.reg.Tasks(ctx); err != nil { // the requests may have moved some
			return fmt.Errorf("factory tick: %w", err)
		}
		for _, t := range live(tasks) {
			err := e.safely(ctx, t, func(ctx context.Context) error { return e.advance(ctx, t) })
			switch {
			case err == nil:
				delete(e.lastFail, t.ID)
			case errors.Is(err, ErrHeld), errors.Is(err, errPanicked), ctx.Err() != nil:
				errs = append(errs, err)
			default:
				failures = append(failures, taskFailure{task: t, err: err})
			}
		}
	}
	failures = append(failures, e.readFails...)
	e.readFails = nil
	if e.uncounted {
		if len(failures) > 0 {
			e.log.Info("failures on the first tick after a wake count none", "failures", errors.Join(failures...))
		}
		failures = nil
		e.uncounted = false
		e.tried = false // a pass whose failures count none tried nothing either
	}
	for _, f := range failures {
		if tf, ok := errors.AsType[taskFailure](f); ok && infraCause(tf.err) == "" {
			e.taskFailed(ctx, tf)
		}
		errs = append(errs, f)
	}
	e.hold(ctx, failures)
	e.escalate(ctx, now)
	return errors.Join(errs...)
}

// live are the tasks the loop advances, oldest first.
func live(tasks []registry.Task) []registry.Task {
	var out []registry.Task
	for _, t := range tasks {
		for _, s := range liveStates {
			if t.State == s {
				out = append(out, t)
			}
		}
	}
	return out
}

// advance runs one pass of the task's current stage (an empty stage is its
// kind's first, recorded) and moves the task to the next stage once the
// stage passes. The last stage's own effect ends the task.
func (e *Engine) advance(ctx context.Context, t registry.Task) error {
	kind, ok := e.cfg.Factory.Kinds[t.Kind]
	if !ok || len(kind.Stages) == 0 {
		return fmt.Errorf("task %d: the kind of task %q is not configured", t.ID, t.Kind)
	}
	i := 0
	if t.Stage != "" {
		i = stageIndex(kind, t.Stage)
		if i < 0 {
			return fmt.Errorf("task %d: the %s kind has no stage %q", t.ID, t.Kind, t.Stage)
		}
	} else {
		first := kind.Stages[0].Name
		if err := e.reg.TransitionTask(ctx, t.ID, []string{t.State}, "", func(u *registry.TaskUpdate) {
			u.Set("stage", first)
			u.Because("the task starts")
		}); err != nil {
			return err
		}
		t.Stage = first
	}
	st := kind.Stages[i]
	run := e.runners[st.Type]
	if run == nil {
		return nil
	}
	done, err := run(ctx, t, st)
	if err != nil || !done || i+1 == len(kind.Stages) {
		return err
	}
	next := kind.Stages[i+1].Name
	return e.reg.TransitionTask(ctx, t.ID, []string{t.State}, "", func(u *registry.TaskUpdate) {
		u.Set("stage", next)
		u.Set("attempt", 0)
		u.Because(st.Name + " passed")
	})
}

func stageIndex(k config.FactoryKind, name string) int {
	for i, st := range k.Stages {
		if st.Name == name {
			return i
		}
	}
	return -1
}

// stageOf is the configured stage named name of the task's kind.
func (e *Engine) stageOf(t registry.Task, name string) (config.FactoryStage, bool) {
	kind := e.cfg.Factory.Kinds[t.Kind]
	if i := stageIndex(kind, name); i >= 0 {
		return kind.Stages[i], true
	}
	return config.FactoryStage{}, false
}

// observe is the pass between ticks: it reads the agents' panes and the
// watchdog (Runtime.Observe and Watchdog) and keeps their events for each
// task's next advance (takeEvents). A failed read is a failure the next
// tick weighs (an infrastructure hold), unless the Mac just woke.
func (e *Engine) observe(ctx context.Context) {
	now := e.now()
	e.wake(now)
	defer e.alive()
	if e.d.Runtime == nil {
		return
	}
	if _, down := e.holds[HoldHerdr]; down {
		return
	}
	var evs, wd []runtime.Event
	if err := e.contained("observe", func() (err error) { evs, err = e.d.Runtime.Observe(ctx); return err }); err != nil {
		e.readFails = append(e.readFails, fmt.Errorf("observe: %w", err))
	}
	if err := e.contained("watchdog", func() (err error) { wd, err = e.d.Runtime.Watchdog(ctx, now); return err }); err != nil {
		e.readFails = append(e.readFails, fmt.Errorf("watchdog: %w", err))
	}
	for _, ev := range append(evs, wd...) {
		e.inbox[ev.TaskID] = append(e.inbox[ev.TaskID], ev)
		if box := e.inbox[ev.TaskID]; len(box) > maxInbox {
			e.inbox[ev.TaskID] = box[len(box)-maxInbox:]
		}
		if ev.Kind == runtime.EventDeliveryUnknown {
			e.deliveryUnknown(ctx, ev)
		}
	}
}

// deliveryUnknown sends a live task whose prompt may or may not have
// arrived to you: no transcript can tell, and its role gets no prompt until
// you looked at the pane and let the prompt go (ReqLetGo). Whatever stage
// the task is in, it would stall there in silence otherwise.
func (e *Engine) deliveryUnknown(ctx context.Context, ev runtime.Event) {
	t, err := e.reg.TaskByID(ctx, ev.TaskID)
	if err != nil || (t.State != registry.TaskActive && t.State != registry.TaskWaiting) {
		return
	}
	e.toOperator(ctx, t, ev.Reason, "a prompt's delivery is unknown")
}

// maxInbox bounds the events kept for a task between its advances.
const maxInbox = 64

// takeEvents returns the events observe kept for task id and forgets them.
func (e *Engine) takeEvents(id int64) []runtime.Event {
	evs := e.inbox[id]
	delete(e.inbox, id)
	return evs
}

// launch is how a stage starts its roles (Runtime.Launch): never while the
// daemon drains or a hold of the machine holds launches (heldError, which
// costs the task nothing; the task waits with the hold's reason). A
// successful launch clears such a wait.
func (e *Engine) launch(ctx context.Context, t registry.Task, a registry.Attempt, copyPath string, roles []config.Role) error {
	if who, what, ok := e.mayLaunch(); !ok {
		if who != holdRestart {
			e.waitOn(ctx, t, who, what)
		}
		return heldError{who: who, what: what}
	}
	if e.d.Runtime == nil {
		return heldError{who: "agent", what: "this build runs no agents yet"}
	}
	if err := e.d.Runtime.Launch(ctx, t, a, copyPath, roles); err != nil {
		// A launch that waits on Codex's budget, the memory or a CLI's pause
		// costs the task nothing either: it waits with the reason.
		if w, ok := errors.AsType[*runtime.WaitError](err); ok {
			e.waitOn(ctx, t, w.Who, w.What)
			return heldError{who: w.Who, what: w.What}
		}
		return err
	}
	if heldWait(t.WaitsWho) {
		e.waitOn(ctx, t, "", "")
	}
	return nil
}

// beginEffect is how a stage begins an external effect (a push, a PR):
// never while the daemon drains.
func (e *Engine) beginEffect(ctx context.Context, key, kind string, taskID int64) (registry.Effect, bool, error) {
	if e.draining.Load() {
		return registry.Effect{}, false, heldError{who: holdRestart, what: drainWhat}
	}
	return e.reg.BeginEffect(ctx, key, kind, taskID)
}

// waitOn records who the task waits on and for what, when that changed.
func (e *Engine) waitOn(ctx context.Context, t registry.Task, who, what string) {
	if t.WaitsWho == who && t.WaitsWhat == what {
		return
	}
	if err := e.reg.TransitionTask(ctx, t.ID, []string{t.State}, "", func(u *registry.TaskUpdate) {
		u.Set("waits_who", who)
		u.Set("waits_what", what)
	}); err != nil {
		e.log.Warn("record what a task waits on", "task", t.ID, "waits", who+": "+what, "err", err)
	}
}

// toOperator moves a live task to NEEDS YOU, waiting on you for what, frees
// its heavy slot (nothing advances it to release one), and alerts.
func (e *Engine) toOperator(ctx context.Context, t registry.Task, what, because string) {
	err := e.reg.TransitionTask(ctx, t.ID, liveStates, registry.TaskNeedsYou, func(u *registry.TaskUpdate) {
		u.Set("waits_who", waitsYou)
		u.Set("waits_what", what)
		u.Because(because)
	})
	if err != nil {
		e.log.Warn("move a task to NEEDS YOU", "task", t.ID, "err", err)
		return
	}
	e.freeHeavy(ctx, t, "its task waits on you")
	e.alert(ctx, t.ID, TaskKey(t)+": you: "+what)
}

// waitsYou is the WaitsWho of a task that waits on the operator.
const waitsYou = "you"

// alert records an alert: a task, or the factory (taskID 0), moved into
// NEEDS YOU. The notifier of the screen's build turns alerts into toasts.
func (e *Engine) alert(ctx context.Context, taskID int64, text string) {
	e.event(ctx, taskID, "alert", registry.PhaseInfo, text)
}

// event writes an audit row (redacted); a failed write is logged.
func (e *Engine) event(ctx context.Context, taskID int64, kind, phase, text string) {
	if err := e.reg.AppendEvent(ctx, taskID, kind, phase, execx.Redact(text)); err != nil {
		e.log.Warn("factory event", "task", taskID, "kind", kind, "err", err)
	}
}

// alive notes the end of a tick or an observe pass (wake measures from it).
func (e *Engine) alive() { e.lastAlive = e.now().Round(0) }

// TaskKey names a task as the screen and the waits do: its Jira key, else
// "task <id>".
func TaskKey(t registry.Task) string {
	if t.JiraKey != "" {
		return t.JiraKey
	}
	return "task " + strconv.FormatInt(t.ID, 10)
}

// taskFailed records a task's own failure as an `advance` fail event, once
// per distinct failure in a row.
func (e *Engine) taskFailed(ctx context.Context, f taskFailure) {
	text := textx.Clip(textx.FirstLine(execx.Redact(f.err.Error())), 300)
	if e.lastFail[f.task.ID] == text {
		return
	}
	e.lastFail[f.task.ID] = text
	e.log.Warn("a task's step failed", "task", f.task.ID, "err", f.err)
	e.event(ctx, f.task.ID, "advance", registry.PhaseFail, text)
}

// stageWhat is a stage's name as a wait says it ("local_review": "local
// review").
func stageWhat(name string) string { return strings.ReplaceAll(name, "_", " ") }
