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
// the shift of every turn deadline after the Mac slept, the role that runs
// for a role (its Claude fallback on a task Codex never works on), the quit
// of such a task's Codex sessions, a run's state as the agents store keeps
// it across a restart, who is at work in a task's copy now (a run in flight,
// you in a pane: the factory's own commands that change the copy wait for
// them), and who could have changed it since a time (the factory puts back
// what its checks changed only when nobody else could have).
type Runtime interface {
	Owner(ctx context.Context, t registry.Task) (store.PR, error)
	Launch(ctx context.Context, t registry.Task, a registry.Attempt, c Copy, roles []config.Role) error
	Send(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, text string) (runID string, err error)
	Observe(ctx context.Context) ([]runtime.Event, error)
	Verify(ctx context.Context, runID string, ok bool, why string) error
	Park(ctx context.Context, t registry.Task) error
	Reconcile(ctx context.Context, e registry.LedgerEntry) (string, error)
	Watchdog(ctx context.Context, now time.Time) ([]runtime.Event, error)
	RecoverLost(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role) error
	LetGo(ctx context.Context, t registry.Task, role string) (int, error)
	Shift(gap time.Duration)
	RunsAs(t registry.Task, role config.Role) (config.Role, error)
	QuitCodex(ctx context.Context, t registry.Task) error
	RunState(ctx context.Context, runID string) (state, why string, err error)
	AtWork(ctx context.Context, t registry.Task) (role string, you bool, err error)
	TouchedSince(ctx context.Context, t registry.Task, since time.Time) (who string, err error)
}

// The runtime the daemon wires (internal/factory/runtime) is the engine's.
var _ Runtime = (*runtime.Runtime)(nil)

// Copy is a task's copy of a repository: its name and path, its number
// (copy {n}), the port of its server and its environment (runtime.Copy: a
// launch runs the task's panes in it).
type Copy = runtime.Copy

// Copies are the repositories' copies the tasks work in (copies.go): a
// claim for a task (the copy it used last first), its release, how many are
// free, the copies reserved and for which task, the copy's local server,
// and the reconcile of the servers after a restart.
type Copies interface {
	Claim(ctx context.Context, t registry.Task) (Copy, error)
	Release(ctx context.Context, t registry.Task, reason string) error
	Free(ctx context.Context) (int, error)
	Reserved(ctx context.Context) ([]Reservation, error)
	StartServer(ctx context.Context, c Copy) error
	StopServer(ctx context.Context, c Copy) error
	Reconcile(ctx context.Context) error
}

// Reservation is a copy a task's owner row holds, from its claim (one that
// got the copy, waits or failed) until its release.
type Reservation struct {
	TaskID int64
	Copy   string
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
	// Run runs the overlap line's reads (Overlap: git log in the main
	// clone, gh pr list as your gh login) and the factory's own commands in
	// a task's copy (its precheck and its proof), every one through the deny
	// list (Guard: New wraps it). The daemon builds Git over the same
	// guarded runner.
	Run execx.Runner
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
	// beforeMark runs right before codexRefused writes its mark (a test's
	// seam: another writer moving the task meanwhile); nil in a daemon.
	beforeMark func()
	// jiraTries are the next tries of the tasks that wait for Jira
	// (retryJira).
	jiraTries map[int64]*jiraTry

	heavyMu   sync.Mutex
	heavyHeld map[int64]*heavySlot
	heavySeq  int

	// The claims of copies that run off the loop (claimCopy), the waits of
	// the claims that ended without a copy and the copy each task holds once
	// its claim handed it over; claimMu guards the three, a claim's
	// goroutine ends its claimRun.
	claimMu    sync.Mutex
	claims     map[int64]*claimRun
	claimWaits map[int64]*claimWait
	held       map[int64]Copy
	// debts are the releases of ended tasks' copies that did not go through
	// (payReleaseDebts); claimMu guards them too.
	debts map[int64]*releaseDebt

	// agentRuns are the agents stages' attempts in progress, by task
	// (runAgents): the loop's own, rebuilt from the registry after a
	// restart.
	agentRuns map[int64]*agentsRun

	// run is Deps.Run behind the deny list (Guard). The factory's checks of
	// a task's head (its proof and its precheck) run off the loop, one per
	// task (checks); checkNotes say what each does now, the task's wait
	// (checksMu guards both). lockPoll is how often a check waiting for the
	// machine's spec lock tries it again, after the timer of a precheck's
	// cap (nil: time.After): seams of the tests.
	run        execx.Runner
	checksMu   sync.Mutex
	checks     map[int64]*headCheck
	checkNotes map[int64]string
	checkFails map[int64]int // checks in a row that did not run to their end, by task
	// keptSaid is the last copy kept for you each task's resumeDetached
	// said (an event and an alert once, not each tick): the loop's own.
	keptSaid map[int64]string
	lockPoll time.Duration
	after    func(d time.Duration) <-chan time.Time

	// The work off the loop (claims, the overlap line, a prompt's search)
	// runs on bgCtx, the engine's own: a drain cancels it and waits for bg
	// (stopBackground), so none of it outlives the daemon.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup
}

// New builds the engine over d.
func New(d Deps) *Engine {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	e := &Engine{
		d: d, cfg: d.Config, reg: d.Registry, log: log,
		bgCtx: bgCtx, bgCancel: bgCancel,
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
		jiraTries:  map[int64]*jiraTry{},
		claims:     map[int64]*claimRun{},
		claimWaits: map[int64]*claimWait{},
		held:       map[int64]Copy{},
		debts:      map[int64]*releaseDebt{},
		agentRuns:  map[int64]*agentsRun{},
		run:        Guard(d.Run, d.Config),
		checks:     map[int64]*headCheck{},
		checkNotes: map[int64]string{},
		checkFails: map[int64]int{},
		keptSaid:   map[int64]string{},
		lockPoll:   specsLockPoll,
	}
	e.runners[config.FactoryStageAgents] = e.runAgents
	e.runners[config.FactoryStageGate] = e.runGate
	return e
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
	// The work off the loop ends with the daemon: a claim's setup or install
	// must not go on in a copy after it exits.
	defer e.stopBackground()

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

// startup runs once before the first tick. It restores the branch of each
// copy a base run left detached when the daemon stopped in its middle
// (resumeDetached: before anything else runs in that copy), counts the
// heavy stages whose attempts still run (heavy) and ends the copies'
// servers a stopped daemon left (Copies.Reconcile: no stage owns one after
// a restart); the recovery of what else a stopped daemon left comes on top
// of it.
func (e *Engine) startup(ctx context.Context) error {
	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		return fmt.Errorf("factory startup: %w", err)
	}
	for _, t := range tasks {
		if err := e.resumeDetached(ctx, t); err != nil && !errors.Is(err, ErrHeld) {
			e.log.Warn("restore a copy a base run left detached", "task", t.ID, "err", err)
		}
	}
	if e.d.Copies != nil {
		if err := e.d.Copies.Reconcile(ctx); err != nil {
			e.log.Warn("reconcile the copies' servers", "err", err)
			e.event(ctx, 0, "server", registry.PhaseFail, "reconcile: "+err.Error())
		}
	}
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
	e.sweepClaims(ctx, tasks)
	e.sweepChecks(tasks)
	e.sweepDetached(ctx, tasks)
	e.payReleaseDebts(ctx, tasks)
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
	e.admit(ctx)

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
// stage passes. The last stage's own effect ends the task. A task that
// waits for Jira before its first stage tries Jira instead (retryJira), and
// one whose key has no ticket.md yet (your answer linked it) reads its
// ticket first (linkedTicket). A copy a base run left detached is restored
// before anything else (resumeDetached).
func (e *Engine) advance(ctx context.Context, t registry.Task) error {
	if t.Stage == "" && t.State == registry.TaskWaiting && t.WaitsWho == waitJira {
		return e.retryJira(ctx, t)
	}
	if err := e.resumeDetached(ctx, t); err != nil {
		return err
	}
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
	if ready, err := e.linkedTicket(ctx, &t); err != nil || !ready {
		return err
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
		switch ev.Kind {
		case runtime.EventDeliveryUnknown:
			e.deliveryUnknown(ctx, ev)
		case runtime.EventRefused:
			e.codexRefused(ctx, ev)
		}
	}
}

// codexRefused marks the task of a refused Codex role never for Codex again
// (Task.CodexNever), at once and in the registry, so no restart and no stage
// lets Codex back on it: a cybersecurity or abuse warning is never retried
// (the account is at stake). The update sets the mark in whatever state the
// task is (no state to compare: the task may move meanwhile), with a
// codex_never event row, and is tried again after a failure until it lands,
// the task is gone, or ctx ends (markTries at most); one that gives up says
// so in the log and in a codex_never fail event. A refusal of another kind's
// role marks nothing.
func (e *Engine) codexRefused(ctx context.Context, ev runtime.Event) {
	if role, ok := e.cfg.FactoryRole(ev.Role); !ok || role.AgentKind() != config.KindCodex {
		return
	}
	var err error
	for try := 0; try < markTries && ctx.Err() == nil; try++ {
		if try > 0 && !sleepFor(ctx, markRetry) {
			break
		}
		var t registry.Task
		if t, err = e.reg.TaskByID(ctx, ev.TaskID); errors.Is(err, registry.ErrNotFound) {
			return
		} else if err != nil {
			continue
		}
		if t.CodexNever {
			return
		}
		if e.beforeMark != nil {
			e.beforeMark()
		}
		if err = e.reg.TransitionTask(ctx, t.ID, nil, "", func(u *registry.TaskUpdate) {
			u.Set("codex_never", true)
			u.Because("a Codex refusal")
		}); err == nil {
			e.event(ctx, t.ID, "codex_never", registry.PhaseInfo,
				ev.Role+" was refused by Codex; Codex never works on "+TaskKey(t)+" again: "+ev.Reason)
			return
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	e.log.Error("a Codex refusal could not mark its task never for Codex; its Codex roles must not run", "task", ev.TaskID,
		"role", ev.Role, "err", err)
	e.event(context.WithoutCancel(ctx), ev.TaskID, "codex_never", registry.PhaseFail,
		fmt.Sprintf("%s was refused by Codex, but the task could not be marked never for Codex: %v", ev.Role, err))
}

// How codexRefused tries again.
const (
	markTries = 20
	markRetry = 250 * time.Millisecond
)

// sleepFor waits d or until ctx ends; false when ctx ended.
func sleepFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
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
func (e *Engine) launch(ctx context.Context, t registry.Task, a registry.Attempt, c Copy, roles []config.Role) error {
	if who, what, ok := e.mayLaunch(); !ok {
		if who != holdRestart {
			e.waitOn(ctx, t, who, what)
		}
		return heldError{who: who, what: what}
	}
	if e.d.Runtime == nil {
		return heldError{who: "agent", what: "this build runs no agents yet"}
	}
	if err := e.d.Runtime.Launch(ctx, t, a, c, roles); err != nil {
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
