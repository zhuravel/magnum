package runtime

// The runtime drives the factory's agents through magnum's agents.Manager
// over the factory's own agents store (Layout.FactoryAgentsDB): one owner row
// per task (owner.go), the launch of a stage's roles, recorded on the attempt
// before any workspace exists, the prompts it sends through its ledger
// (deliver.go), the observe pass that turns the panes into events and heals
// what it can (health.go), the watchdog with its recovery steps
// (watchdog.go), and the heal of a herdr server macOS denies the copies'
// volume (volume.go). The real state of a task is the factory registry's; the
// runtime keeps in memory only what a restart may forget (a watchdog's steps,
// a pause's end), at worst starting a ladder over.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/scrub"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/usage"
)

// Tag marks the factory's agents (agents.Deps.Tag): it leads their titles
// and pane labels and joins their herdr names' hash, so they never collide
// with a review's.
const Tag = "factory"

// The runtime's timing and budgets (spec sections 5 and 10.1).
const (
	// Stagger is how far apart launches start while the Mac's memory
	// pressure is at warn.
	Stagger = 20 * time.Second
	// RedeliverAfter is the least time between the two reads of a
	// transcript that prove a prompt never arrived (Reconcile).
	RedeliverAfter = 2 * time.Minute
	// UnknownAfter is how long a prompt's delivery may stay unknown before
	// the operator is asked to look at the pane (EventDeliveryUnknown).
	UnknownAfter = 15 * time.Minute
	// NoProgressAfter is how long a run's transcript and screen stay the
	// same before the watchdog takes its next step, and how long a
	// recovery waits for progress before the next one.
	NoProgressAfter = 10 * time.Minute
	// MaxStagePrompts is how many prompts a role gets in one stage before a
	// loop without progress is assumed; MaxContext the share of its context
	// (percent) past which it is; SameFailures how often the same failing
	// output in a row says so.
	MaxStagePrompts = 6
	MaxContext      = 85
	SameFailures    = 3

	// loginEvery bounds how often Observe reads Codex's login and re-checks
	// a login pause; usageFallback is a usage pause without a named reset;
	// overloadWait an overload's.
	loginEvery    = time.Minute
	usageFallback = time.Hour
	overloadWait  = 2 * time.Minute
	versionTTL    = time.Hour
	versionWait   = 10 * time.Second

	// fullObserveEvery is how often at most an observe pass reads every live
	// session (agents.ObserveSnapshotAt); the passes between read the
	// sessions with a turn in flight alone (observeSessions).
	fullObserveEvery = 10 * time.Second
)

// ErrOperatorInPane is Send's answer while the operator types in the role's
// pane (agents.ErrHumanActive): the prompt waits; nothing was sent.
var ErrOperatorInPane = errors.New("you're in that pane")

// ErrDeliveryUnknown is Send's answer while one of the role's prompts has a
// delivery nobody can prove either way (Reconcile): a prompt is never sent
// twice.
var ErrDeliveryUnknown = errors.New("the last prompt's delivery is unknown")

// ErrNoProgress is Send's answer once a loop without progress outlived its
// compaction and its fresh session: the task needs the operator
// (EventNoProgress).
var ErrNoProgress = errors.New("the agent makes no progress")

// WaitError is a launch or a prompt that waits on something outside the
// task: Codex's budget, the Mac's memory, a usage limit, a login, an
// overload. It is no failure: the task waits with Who and What, as its wait
// says them ("usage: codex at 96%, waits till 16:00"), until Until when it
// is known.
type WaitError struct {
	Who, What string
	Until     time.Time
}

// Error implements error.
func (w *WaitError) Error() string { return w.Who + ": " + w.What }

// Deps are the runtime's collaborators. Config, Herdr, Store, Registry and
// Runner are required. Store is the factory's agents store
// (Layout.FactoryAgentsDB). Memory nil reads no memory (taken as normal);
// MayStart is the engine's holds of a launch (a disk or memory hold, a drain
// for an install: who and what, as a task's wait says them), which a start
// outside a launch passes too (mayStart), called on the engine's loop; nil
// holds nothing. MayPrompt is the engine's hold of new prompts (a drain for
// a reboot), which Send asks before anything else, called on the engine's
// loop; nil holds none. Clock nil is time.Now; Log nil discards.
type Deps struct {
	Config    *config.Config
	Layout    paths.Layout
	Herdr     agents.Herdr
	Store     *store.Store
	Registry  *registry.Registry
	Runner    execx.Runner
	Memory    func(ctx context.Context) engine.MemorySample
	MayStart  func() (who, what string, ok bool)
	MayPrompt func() (who, what string, ok bool)
	Clock     func() time.Time
	Log       *slog.Logger
}

// seams are what New takes from the machine and a test replaces: the sleep
// (stagger, quits, switches), the reading of Codex's budget and login, the
// signal StopShell may send, the read of a transcript's user turns, the
// daemon's read of a copy's directory (a dirProber's), and the CLIs' homes
// the agents manager reads transcripts from ("" = the CLI's own default).
type seams struct {
	sleep                func(ctx context.Context, d time.Duration) error
	usage                func(ctx context.Context, now time.Time) (usage.Snapshot, error)
	login                func() (usage.Login, error)
	signal               func(pid int, sig syscall.Signal) error
	userTurns            func(kind, path string) ([]agents.UserTurn, error)
	readable             func(dir string) bool
	claudeDir, codexHome string
}

// Runtime is the factory's agent runtime (package doc). It is safe for
// concurrent use; the engine calls it from its loop.
type Runtime struct {
	d   Deps
	cfg *config.Config // the agents manager's: the factory's roles as its roles, the factory's human cooldown
	mgr *agents.Manager
	st  *store.Store
	reg *registry.Registry
	log *slog.Logger
	s   seams

	// launchMu makes the staggered launches take turns, talkMu the liaisons'
	// starts (Talk). A Talk also holds its task's lock (talkLock) for its
	// whole run, which parkLiaison only tries.
	launchMu sync.Mutex
	talkMu   sync.Mutex
	// drained closes when the daemon drains (BeginDrain): a launch that
	// waits for the stagger then starts nothing.
	drained   chan struct{}
	drainOnce sync.Once
	// soon asks the daemon's loop for an observe pass now (ObserveSoon):
	// one slot, a pending request absorbs the others.
	soon chan struct{}

	mu        sync.Mutex
	lastStart time.Time
	keys      map[int64]string      // task id -> its key, the owner name in titles
	talkTasks map[int64]*sync.Mutex // task id -> its Talk's lock (talkLock)
	roles     map[roleKey]*roleState
	loops     map[loopKey]*loopState
	pauses    map[string]*pause        // agent kind -> its pause
	misses    map[int64]time.Time      // ledger entry -> the first read that did not find it
	shifts    map[string]time.Duration // run id -> the sleeps its deadline moved by
	once      map[string]bool          // events and records written once
	checked   map[int64]time.Time      // Codex session -> when TookNoPrompt last read it
	pending   []Event                  // events found outside Observe
	refused   map[int64]bool           // task -> a Codex refusal: its Codex roles never run again
	idleSeen  map[int64]time.Time      // liaison session -> the pass that first saw it idle with no time to tell (liaisonIdleSince)
	loginFP   string
	loginRead time.Time
	// codexRestart: a Codex login changed while a drain for a reboot held
	// new prompts, so the idle Codex sessions' restart waits for its lift.
	codexRestart bool
	reconciled   time.Time
	versions     map[string]cliVersion
	typedRead    time.Time             // the last read of the turns typed into the panes
	fullAt       time.Time             // the capture time of the last observe pass over every session (observeSessions)
	seen         map[string]fileMark   // conversation -> its transcript as the last read that took it whole saw it
	youAt        map[int64][]time.Time // session -> when herdr showed you in its pane (sawYou)
	commands     map[string]bool       // the slash commands magnum types (agents.MagnumCommands)
	focus        PaneFocus             // you in the factory's panes, as the observe passes saw you (noteFocus)
	heals        map[int64]*heal       // task -> its heal of the factory's herdr server that cannot read its copy's volume (volume.go)
	lastStop     time.Time             // the factory's herdr server's last stop by a heal (recentStop)
	marks        map[string]string     // pane -> what it showed before its last launch (paneMark)
}

// roleKey names a role of an attempt, as it runs (after a Codex fallback).
type roleKey struct{ attempt, role string }

// roleState is what the runtime keeps of a role of an attempt.
type roleState struct {
	key    roleKey
	taskID int64
	stage  string
	role   config.Role
	// copy is the checkout the role works in, env its panes' environment
	// (paneEnv; after a restart, the one its last session kept).
	copy string
	env  map[string]string
	// first is the attempt's first prompt to the role, as sent: what a
	// fresh session starts from.
	first string
	// The watchdog: the last reading of the run's transcript and screen,
	// when it last changed, the steps taken on a hung turn, and whether the
	// steps ran out.
	sig   string
	since time.Time
	step  int
	stuck bool
	// Recovery of a lost pane: whether a recovery waits for progress, and
	// whether the recoveries ran out; ranOut is when the steps ran out (lost,
	// stuck, or a heal told to you), which your retry after it starts over;
	// noPrompt counts the fresh starts of a Codex session that never took its
	// first prompt. dueAt is when the next recovery runs, after its wait
	// (RecoveryWaits; zero: none is scheduled); the recoveries taken are
	// counted from the task's events (recoveriesUsed).
	recovering bool
	lost       bool
	ranOut     time.Time
	dueAt      time.Time
	// pending: a recovery quit the role's session (or found it gone) and
	// abandoned its runs, and no new run of it was sent yet (handOver): the
	// watchdog keeps the role due its next step.
	pending  bool
	noPrompt int
	nudges   int
	// refused: the role's turn ended on a refusal; it never runs again in
	// the attempt.
	refused bool
}

// loopKey names a role in a stage of a task: a loop without progress is
// counted per stage, across its attempts.
type loopKey struct {
	task        int64
	stage, role string
}

// loopState is the ladder of a loop without progress: the steps taken
// (compact, fresh session, the operator), the prompt count at the last
// step, and the latest failing outputs.
type loopState struct {
	step, base int
	fails      []string
	// reset: a verified run showed progress; the next prompt sets base.
	reset bool
	// attempt is the attempt of the stage the last prompt went to.
	attempt string
}

// cliVersion is a CLI's version as read at a time.
type cliVersion struct {
	v  string
	at time.Time
}

// New builds the runtime over d.
func New(d Deps) (*Runtime, error) {
	return newWith(d, seams{})
}

// newWith is New with seams; a nil seam is the machine's.
func newWith(d Deps, s seams) (*Runtime, error) {
	if d.Config == nil || d.Herdr == nil || d.Store == nil || d.Registry == nil || d.Runner == nil {
		return nil, errors.New("factory runtime: Config, Herdr, Store, Registry and Runner are required")
	}
	cfg := *d.Config
	cfg.Roles = d.Config.Factory.Roles // the manager knows the factory's roles by name
	cfg.Daemon.HumanCooldown = d.Config.Factory.HumanCooldown
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if s.sleep == nil {
		s.sleep = sleepCtx
	}
	if s.usage == nil {
		home := d.Config.Usage.CodexHome
		s.usage = func(ctx context.Context, now time.Time) (usage.Snapshot, error) { return usage.Codex(ctx, home, now) }
	}
	if s.login == nil {
		home := d.Config.Usage.CodexHome
		s.login = func() (usage.Login, error) { return usage.CodexLogin(home) }
	}
	if s.signal == nil {
		s.signal = syscall.Kill
	}
	if s.userTurns == nil {
		s.userTurns = readUserTurns
	}
	if s.readable == nil {
		s.readable = newDirProber(openDir, readableWait).readable
	}
	if s.codexHome == "" {
		s.codexHome = d.Config.Usage.CodexHome
	}
	r := &Runtime{d: d, cfg: &cfg, st: d.Store, reg: d.Registry, log: log, s: s,
		keys: map[int64]string{}, roles: map[roleKey]*roleState{}, loops: map[loopKey]*loopState{},
		pauses: map[string]*pause{}, misses: map[int64]time.Time{}, shifts: map[string]time.Duration{},
		once: map[string]bool{}, checked: map[int64]time.Time{}, refused: map[int64]bool{}, versions: map[string]cliVersion{},
		seen: map[string]fileMark{}, youAt: map[int64][]time.Time{}, idleSeen: map[int64]time.Time{}, heals: map[int64]*heal{},
		marks:    map[string]string{},
		commands: agents.MagnumCommands(&cfg), drained: make(chan struct{}), soon: make(chan struct{}, 1)}
	r.mgr = agents.New(agents.Deps{Herdr: d.Herdr, Store: d.Store, Runner: d.Runner, Config: &cfg, Layout: d.Layout,
		Clock: d.Clock, Sleep: s.sleep, ClaudeDir: s.claudeDir, CodexHome: s.codexHome, Log: slogLogger{log},
		Tag: Tag, OwnerName: r.ownerName, Signal: s.signal, SkipGoneDirs: true, HookFailure: r.hookFailure})
	return r, nil
}

// ErrDraining is a launch the daemon's drain stopped while it waited for the
// stagger (BeginDrain): no agent started.
var ErrDraining = errors.New("factory runtime: the daemon drains: no new launch")

// BeginDrain stops the launches that wait for the stagger, and any that
// would: the daemon drains on SIGTERM, and launchd kills it ExitTimeOut
// after, so a tick must not sleep Stagger per role meanwhile. A second call
// does nothing.
func (r *Runtime) BeginDrain() { r.drainOnce.Do(func() { close(r.drained) }) }

// staggerSleep sleeps d between two launches, cut by the drain
// (ErrDraining).
func (r *Runtime) staggerSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-r.drained:
		return ErrDraining
	default:
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-r.drained:
			cancel()
		case <-sctx.Done():
		}
	}()
	err := r.s.sleep(sctx, d)
	select {
	case <-r.drained:
		return ErrDraining
	default:
		return err
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Runtime) now() time.Time {
	if r.d.Clock != nil {
		return r.d.Clock()
	}
	return time.Now()
}

// HerdrUp asks the factory's herdr session whether it answers (a snapshot):
// the engine's herdr hold.
func (r *Runtime) HerdrUp(ctx context.Context) error {
	_, err := r.d.Herdr.Snapshot(ctx)
	return err
}

// WorkspaceLabel is the herdr workspace label of a task's attempt, unique
// per task and attempt: "factory <key> <attempt id>". A restart finds a
// workspace no row names by it.
func WorkspaceLabel(t registry.Task, a registry.Attempt) string {
	return Tag + " " + taskKey(t) + " " + a.ID
}

// LiaisonRole is the factory role of a task's liaison, the agent that talks
// with you about the task (Talk); no stage names it.
const LiaisonRole = "fx-liaison"

// liaisonLabel is the herdr workspace label of task t's liaison: "factory
// liaison <key>", one per task, and no attempt's label (labelAttempt), so a
// restart tells a liaison's leftover from an attempt's (SettleWorkspaces).
func liaisonLabel(t registry.Task) string { return Tag + " liaison " + taskKey(t) }

// labelLiaison is the task key a liaison's workspace label names
// (liaisonLabel); false for any other label.
func labelLiaison(label string) (string, bool) {
	key, ok := strings.CutPrefix(label, Tag+" liaison ")
	return key, ok && key != ""
}

// DBLockCommand is the command prefix a role runs every command that
// touches its copy's databases with (`magnum db-lock --checkout <copy>
// --role <role> --`, the prompts' .DBLockCommand): the roles of a task take
// turns on the copy's databases.
func (r *Runtime) DBLockCommand(copyPath string, role config.Role) string {
	return agents.DBLockLine(r.d.Layout.Binary(), copyPath, role.Name)
}

// RunsAs is the role that runs for role on task t: role itself, or, on a
// task Codex never works on again (Task.CodexNever, or a refusal this
// runtime saw before the engine marked the task), a Codex role's
// codex_fallback. A Codex role without a fallback there is an error: it
// cannot run.
func (r *Runtime) RunsAs(t registry.Task, role config.Role) (config.Role, error) {
	if role.AgentKind() != agents.KindCodex || !r.codexNever(t) { // a Claude role reads nothing
		return role, nil
	}
	name := r.d.Config.Factory.CodexFallback[role.Name]
	if fb, ok := r.d.Config.FactoryRole(name); name != "" && ok {
		return fb, nil
	}
	return config.Role{}, fmt.Errorf("%s runs on Codex, which never works on %s again, and [factory] codex_fallback names no role for it",
		role.Name, taskKey(t))
}

// codexNever reports whether Codex never works on task t again: one of its
// Codex roles was refused in this runtime, or the task is marked
// (Task.CodexNever), on the copy given or in the registry, which a restart
// keeps: a stale copy of the task never lets Codex back on it. A registry
// that cannot be read fails closed (the operator's account-risk rule): the
// mark may be there, so Codex stays off the task for this call, and the next
// call reads again.
func (r *Runtime) codexNever(t registry.Task) bool {
	r.mu.Lock()
	known := t.CodexNever || r.refused[t.ID]
	r.mu.Unlock()
	if !known && t.ID != 0 {
		stored, err := r.reg.TaskByID(context.Background(), t.ID)
		if err != nil {
			r.log.Warn("read whether Codex may work on a task; it may not meanwhile", "task", t.ID, "err", err)
			return true
		}
		known = stored.CodexNever
	}
	if known {
		r.mu.Lock()
		r.refused[t.ID] = true
		r.mu.Unlock()
	}
	return known
}

// errRefusedRole is a restart of a role that may never run again: it was
// refused, or it runs on Codex on a task Codex never works on again.
var errRefusedRole = errors.New("the role was refused on this task: it never runs again")

// mayRestart says why role st of task t may not be restarted, resumed or
// compacted now (nil when it may): it was refused, or runs on Codex on a
// task Codex never works on again (errRefusedRole); its kind is paused (the
// pause's *WaitError); the operator is in its pane (ErrOperatorInPane).
func (r *Runtime) mayRestart(ctx context.Context, t registry.Task, st *roleState) error {
	r.mu.Lock()
	refused := st.refused
	r.mu.Unlock()
	if refused || (st.role.AgentKind() == agents.KindCodex && r.codexNever(t)) {
		return errRefusedRole
	}
	if w := r.paused(st.role.AgentKind()); w != nil {
		return w
	}
	if r.operatorIn(ctx, st) {
		return fmt.Errorf("%s: %w", st.role.Name, ErrOperatorInPane)
	}
	return nil
}

// operatorIn reports whether the operator is in role st's pane: they typed
// into one of the task's panes within the factory's human cooldown (the
// owner row's human_active_at, as Send's refusal reads it), or herdr shows
// the role's pane focused while it showed them at work in a factory pane
// within that cooldown (PaneFocus.Since). herdr's focus alone proves
// nothing of them: it rests where it was when nobody is attached, and a
// task's copy and prompts waited on it for good. A herdr that cannot say
// counts as in it: nothing is quit on a guess.
func (r *Runtime) operatorIn(ctx context.Context, st *roleState) bool {
	pr, err := r.ownerRow(ctx, st.taskID)
	if err != nil {
		return false
	}
	return r.operatorOn(ctx, pr, st.role.Name)
}

// operatorOn is operatorIn for role's pane of owner row pr (a task's, or
// its liaison's).
func (r *Runtime) operatorOn(ctx context.Context, pr store.PR, role string) bool {
	if r.now().Before(r.cfg.Daemon.CooldownUntil(pr.HumanActiveAt)) {
		return true
	}
	s, err := r.st.LiveSessionByPRRole(ctx, pr.ID, role)
	if err != nil {
		return false // no pane to be in
	}
	pane := store.Deref(s.HerdrPaneID)
	snap, err := r.d.Herdr.Snapshot(ctx)
	if err != nil {
		return true
	}
	if since := r.PaneFocus().Since; since.IsZero() || !r.now().Before(r.cfg.Daemon.CooldownUntil(&since)) {
		return false // no proof of you in any factory pane lately: the focus rests
	}
	if pane != "" && snap.FocusedPaneID == pane {
		return true
	}
	for _, p := range snap.Panes {
		if p.ID == pane && p.Focused {
			return true
		}
	}
	return false
}

// calm ends the watchdog's and the recoveries' work on role of attempt: its
// turn ended on a pause or a refusal, which no restart helps; refused marks
// it never to run again (and the task never for Codex, for a Codex role).
func (r *Runtime) calm(attempt, role string, taskID int64, kind string, refused bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.roles[roleKey{attempt, role}]; st != nil {
		st.recovering, st.pending, st.step, st.sig, st.since = false, false, 0, "", time.Time{}
		st.dueAt = time.Time{}
		st.refused = st.refused || refused
	}
	if refused && kind == agents.KindCodex {
		r.refused[taskID] = true
	}
}

// Copy is a task's copy of a repository as the factory's copies claimed it
// for the task (factory.Copies): its name (factory2) and checkout, its
// number (copy {n}), the port of its local server (0 when the copy's env
// names none) and its environment, the repository's env rendered for the
// copy (its name, its number, its Redis offset). Its panes run in Path with
// Env.
type Copy struct {
	Name, Path   string
	Number, Port int
	Env          map[string]string
}

// Launch starts the roles of attempt a of task t in copy c, the copy the
// task claimed. It records the attempt's workspace label first (the attempt stays
// launching), then, before any pane exists, reads the memory and, for a
// Codex role, the budget for every role it launches: critical memory, a
// Codex budget at its soft cap and a kind's pause are a WaitError, no
// failure. A logged-out CLI pauses its kind. Then the task's workspace with
// a pane per role (EnsureWorkspace; the attempt's workspace a crash left
// before its first row is adopted, adopt), each agent started with the resume id
// of the role's earlier session when it has one, launches Stagger apart
// while the memory pressure is at warn, the RoleRun facts of each, and the
// attempt running.
func (r *Runtime) Launch(ctx context.Context, t registry.Task, a registry.Attempt, c Copy, roles []config.Role) error {
	if a.ID == "" || a.TaskID != t.ID {
		return fmt.Errorf("launch %s: attempt %q is not the task's", taskKey(t), a.ID)
	}
	if len(roles) == 0 {
		return fmt.Errorf("launch %s: no roles", a.ID)
	}
	label := WorkspaceLabel(t, a)
	if err := r.reg.TransitionAttempt(ctx, a.ID, []string{registry.AttemptLaunching, registry.AttemptRunning}, "",
		func(u *registry.AttemptUpdate) { u.Set("workspace_label", label) }); err != nil {
		return fmt.Errorf("launch %s: %w", a.ID, err)
	}
	run := make([]config.Role, 0, len(roles))
	levels := make([]int, 0, len(roles))
	for _, role := range roles {
		role, err := r.RunsAs(t, role)
		if err != nil {
			return fmt.Errorf("launch %s: %w", a.ID, err)
		}
		level, err := r.gate(ctx, role)
		if err != nil {
			return err
		}
		run, levels = append(run, role), append(levels, level)
	}
	for _, role := range run {
		if err := r.preflight(ctx, role); err != nil {
			return err
		}
	}
	pr, err := r.owner(ctx, t, a.ID)
	if err != nil {
		return err
	}
	env := paneEnv(t, c)
	if err := r.adopt(ctx, pr, a, label, run, c.Path, env); err != nil {
		return fmt.Errorf("launch %s: %w", a.ID, err)
	}
	ws, err := r.mgr.EnsureWorkspace(ctx, pr, c.Path, env, label, run)
	if err != nil {
		return fmt.Errorf("launch %s: %w", a.ID, err)
	}
	for i, role := range run {
		resume, err := r.mgr.ResumeID(ctx, pr.ID, agents.Role(role.Name))
		if err != nil {
			return fmt.Errorf("launch %s: %w", a.ID, err)
		}
		if err := r.start(ctx, pr, a, role, ws.Panes[agents.Role(role.Name)], resume, levels[i] == engine.MemoryWarn, true); err != nil {
			return fmt.Errorf("launch %s: %w", a.ID, err)
		}
		r.track(t, a, role, c.Path, env)
	}
	err = r.reg.TransitionAttempt(ctx, a.ID, []string{registry.AttemptLaunching}, registry.AttemptRunning, nil)
	if err != nil && !errors.Is(err, registry.ErrConflict) {
		return fmt.Errorf("launch %s: %w", a.ID, err)
	}
	return nil
}

// Talk starts or finds task t's liaison (LiaisonRole, on a row of its own:
// liaisonRow) and returns its pane. A live liaison whose agent herdr shows
// in its pane is kept as it is. A starting or live row herdr does not show
// so (its agent exited and left a shell, before an observe pass marked it
// lost; a restart moved it) is reconciled with herdr first
// (agents.Manager.Recover): a moved agent is bound to its pane again, a
// conversation herdr restored is live again, and a gone agent's row is
// lost, so its conversation is resumed rather than a fresh agent taking the
// row. Otherwise the liaison gets a workspace of its own in the factory's
// herdr session (liaisonLabel; the one its row's sessions hold is reused)
// whose working directory is the task's folder (Layout.FactoryTaskDir),
// never the copy, so it starts as well once a push or a drop freed the
// copy, and whose env names the daemon's MAGNUM_HOME (liaisonEnv). Its
// agent resumes the role's newest parked or lost conversation (ResumeID).
// Either way the conversation gets prompt, once, as its first
// (store.RunInitial), while no prompt of it ever went out (firstPromptDue):
// a fresh one, or one whose first prompt was refused or rejected before it
// was sent; a conversation that had one gets nothing, and the talk is you
// in the liaison's pane (its row's human_active_at, which operatorOn reads:
// a park leaves it for the human cooldown). When that prompt
// fails, the liaison is live in its pane all the same: Talk returns the
// pane with the error (you may step in; agents.ErrHumanActive while you
// typed into it), and the next Talk sends the prompt. Talks take turns, so
// two never start two liaisons, and a park of the task that comes meanwhile
// leaves its liaison alone (parkLiaison; the engine parks it again when the
// task ended while Talk ran, and an observe pass once it may:
// parkLeftLiaisons).
func (r *Runtime) Talk(ctx context.Context, t registry.Task, prompt string) (string, error) {
	r.talkMu.Lock()
	defer r.talkMu.Unlock()
	tl := r.talkLock(t.ID)
	tl.Lock()
	defer tl.Unlock()
	spec, ok := r.d.Config.FactoryRole(LiaisonRole)
	if !ok {
		return "", fmt.Errorf("the liaison of %s: no factory role %s", taskKey(t), LiaisonRole)
	}
	role, err := r.RunsAs(t, spec)
	if err != nil {
		return "", fmt.Errorf("the liaison of %s: %w", taskKey(t), err)
	}
	pr, err := r.liaisonRow(ctx, t)
	if err != nil {
		return "", err
	}
	s, live, err := r.liveLiaison(ctx, pr, role.Name)
	if err == nil && !live && s.ID != 0 { // a row herdr does not show in its pane
		if _, err = r.mgr.Recover(ctx, pr); err == nil {
			s, live, err = r.liveLiaison(ctx, pr, role.Name)
		}
	}
	if err != nil {
		return "", fmt.Errorf("the liaison of %s: %w", taskKey(t), err)
	}
	if !live {
		if s, err = r.startLiaison(ctx, t, pr, role); err != nil {
			return "", fmt.Errorf("the liaison of %s: %w", taskKey(t), err)
		}
	}
	pane := store.Deref(s.HerdrPaneID)
	due, err := r.firstPromptDue(ctx, pr, s)
	if err != nil {
		return pane, fmt.Errorf("the liaison of %s: %w", taskKey(t), err)
	}
	if due {
		if _, err := r.mgr.Prompt(ctx, pr, agents.Role(role.Name), store.RunInitial, prompt); err != nil {
			return pane, fmt.Errorf("the liaison of %s: its first prompt: %w", taskKey(t), err)
		}
		return pane, nil
	}
	// No prompt went out, so no turn starts: the talk takes you into a pane
	// that may have idled for long, and it is you in it, as your typing is.
	if err := r.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", r.now()) }); err != nil {
		r.log.Warn("note you in a task's liaison after a talk", "task", t.ID, "err", err)
	}
	return pane, nil
}

// talkLock is task id's lock: a Talk of it holds it for its whole run, a
// park of its liaison only tries it. Park runs on the engine's loop, which a
// Talk of minutes must not hold up, and parking a liaison between its start
// and its first prompt would quit the agent the Talk is about to prompt.
func (r *Runtime) talkLock(id int64) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.talkTasks[id]
	if !ok {
		if r.talkTasks == nil {
			r.talkTasks = map[int64]*sync.Mutex{}
		}
		m = &sync.Mutex{}
		r.talkTasks[id] = m
	}
	return m
}

// startLiaison starts role, task t's liaison, on its row pr: its workspace
// in the task's folder, its agent resumed from the role's newest parked or
// lost conversation when it has one. It returns the live session, in the
// pane StartAgent found the conversation in.
func (r *Runtime) startLiaison(ctx context.Context, t registry.Task, pr store.PR, role config.Role) (store.Session, error) {
	dir := r.d.Layout.FactoryTaskDir(t.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return store.Session{}, err
	}
	ws, err := r.mgr.EnsureWorkspace(ctx, pr, dir, r.liaisonEnv(t), liaisonLabel(t), []config.Role{role})
	if err != nil {
		return store.Session{}, err
	}
	resume, err := r.mgr.ResumeID(ctx, pr.ID, agents.Role(role.Name))
	if err != nil {
		return store.Session{}, err
	}
	if err := r.mgr.StartAgent(ctx, pr, role, ws.Panes[agents.Role(role.Name)], resume); err != nil {
		return store.Session{}, err
	}
	r.log.Info("started a task's liaison", "task", t.ID, "resumed", resume != "")
	return r.st.LiveSessionByPRRole(ctx, pr.ID, role.Name)
}

// liaisonEnv is the env of task t's liaison's pane: the task's
// (MAGNUM_FACTORY_TASK, paneEnv), and the daemon's MAGNUM_HOME when the
// daemon runs under one (the checkout layout, which MAGNUM_HOME selects, as
// the LaunchAgent's env names it), so the liaison's `magnum factory`
// commands read the daemon's registry. Under the XDG layout the daemon's
// binary finds the same directories by itself, and the pane gets none.
// Nor does it get MAGNUM_CONFIG, which the LaunchAgent's env carries under a
// non-default config file: the commands the liaison runs (`factory answer`,
// `decide`, `retry`) load no configuration, only the registry and the
// task's folder (internal/cli's TestTheLiaisonsCommandsLoadNoConfig holds
// that); one of them that did would need it here.
func (r *Runtime) liaisonEnv(t registry.Task) map[string]string {
	env := paneEnv(t, Copy{})
	if l := r.d.Layout; l.CheckoutLayout() && l.Home != "" {
		env["MAGNUM_HOME"] = l.Home
	}
	return env
}

// firstPromptDue reports whether no prompt of the conversation of s, the
// liaison's live session, ever went out: no run of the rows that carried it
// (s, and the rows of the conversation it resumed: its conversation id) was
// submitted. A prompt refused or rejected before it was sent leaves it
// due; one that may have arrived (submitted, stalled, timed out) does not,
// so no prompt goes twice.
func (r *Runtime) firstPromptDue(ctx context.Context, pr store.PR, s store.Session) (bool, error) {
	rows := map[int64]bool{s.ID: true}
	if sid := store.Deref(s.SessionID); sid != "" {
		sessions, err := r.st.SessionsByPR(ctx, pr.ID)
		if err != nil {
			return false, err
		}
		for _, x := range sessions {
			if store.Deref(x.SessionID) == sid {
				rows[x.ID] = true
			}
		}
	}
	runs, err := r.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(runs, func(run store.Run) bool {
		return run.SubmittedAt != nil && run.SessionID != nil && rows[*run.SessionID]
	}), nil
}

// liveLiaison is row pr's starting or live session of role (the zero
// session for none), and whether it is live with herdr showing its agent
// in its pane (false for a row whose agent is gone, moved or never
// started).
func (r *Runtime) liveLiaison(ctx context.Context, pr store.PR, role string) (store.Session, bool, error) {
	s, err := r.st.LiveSessionByPRRole(ctx, pr.ID, role)
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, false, nil
	}
	if err != nil {
		return store.Session{}, false, err
	}
	pane, name := store.Deref(s.HerdrPaneID), store.Deref(s.AgentName)
	if s.State != store.SessionLive || pane == "" {
		return s, false, nil
	}
	snap, err := r.d.Herdr.Snapshot(ctx)
	if err != nil {
		return store.Session{}, false, err
	}
	live := slices.ContainsFunc(snap.Agents, func(a herdr.AgentInfo) bool { return a.PaneID == pane && (name == "" || a.Name == name) })
	return s, live, nil
}

// gate reads what a launch of role waits on: a pause of its kind, the
// Mac's memory (critical waits; the level comes back for the stagger) and,
// for Codex, the budget, judged by [usage]: at its soft cap the factory's
// new stages wait before magnum's reviews do.
func (r *Runtime) gate(ctx context.Context, role config.Role) (level int, err error) {
	kind := role.AgentKind()
	if w := r.paused(kind); w != nil {
		return 0, w
	}
	level = engine.MemoryNormal
	if r.d.Memory != nil {
		if s := r.d.Memory(ctx); s.Level != engine.MemoryUnknown {
			level = s.Level
		}
		if level == engine.MemoryCritical {
			return level, &WaitError{Who: "memory", What: "the Mac's memory pressure is critical"}
		}
	}
	if kind != agents.KindCodex {
		return level, nil
	}
	u := r.d.Config.Usage
	if u.CodexSoft <= 0 && u.CodexHard <= 0 {
		return level, nil
	}
	snap, err := r.s.usage(ctx, r.now())
	if err != nil {
		if !errors.Is(err, usage.ErrNoData) {
			r.log.Warn("codex usage unread; the launch goes on", "role", role.Name, "err", err)
		}
		return level, nil
	}
	if w := CodexBudgetWait(u, snap, r.now()); w != nil {
		return level, w
	}
	return level, nil
}

// CodexBudgetWait is the wait of a launch of a Codex role while Codex's
// budget snap is at or past [usage] codex_soft (or codex_hard), now being
// the clock's time ("codex at 96%, waits till 16:00", until the binding
// window's reset when snap names one); nil below the caps. Runtime.Launch
// holds every Codex role on it, so a stage's yield (the engine's) comes
// before the launch.
func CodexBudgetWait(u config.Usage, snap usage.Snapshot, now time.Time) *WaitError {
	if usage.Decide(snap, u.CodexSoft, u.CodexHard) == usage.OK {
		return nil
	}
	reset := binding(snap).ResetsAt
	what := fmt.Sprintf("codex at %.0f%%", snap.Used())
	if !reset.IsZero() {
		what += ", waits till " + clockAt(reset, now)
	} else {
		what += fmt.Sprintf(", waits till it is below %.0f%%", u.CodexSoft)
	}
	return &WaitError{Who: "usage", What: what, Until: reset}
}

// binding is the window of snap that runs out first.
func binding(snap usage.Snapshot) usage.Window {
	if snap.Secondary != nil && snap.Secondary.UsedPercent > snap.UsedPercent {
		return *snap.Secondary
	}
	return snap.Window
}

// clock is t as a wait says it: "16:00" today, "Mon 16:00" another day, in
// the runtime clock's zone.
func (r *Runtime) clock(t time.Time) string { return clockAt(t, r.now()) }

// clockAt is t as a wait says it at now (Runtime.clock).
func clockAt(t, now time.Time) string {
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

// preflight checks role's CLI login (agents.PreflightRole): a logged-out
// CLI pauses its kind and waits.
func (r *Runtime) preflight(ctx context.Context, role config.Role) error {
	err := r.mgr.PreflightRole(ctx, role)
	if errors.Is(err, agents.ErrLoginRequired) {
		return r.pause(ctx, role.AgentKind(), pauseLogin, time.Time{})
	}
	if err != nil {
		return fmt.Errorf("preflight %s: %w", role.Name, err)
	}
	return nil
}

// start starts role's agent in pane (resumed from resume when it is set),
// Stagger after the last start when stagger, and records its RoleRun facts,
// with the hashes of its prompt and its configuration when launch (the
// attempt's launch of the role, as the stage runs it; a recovery's start
// keeps them).
func (r *Runtime) start(ctx context.Context, pr store.PR, a registry.Attempt, role config.Role, pane, resume string, stagger, launch bool) error {
	if stagger {
		r.launchMu.Lock()
		defer r.launchMu.Unlock()
		r.mu.Lock()
		last := r.lastStart
		r.mu.Unlock()
		if wait := Stagger - r.now().Sub(last); !last.IsZero() && wait > 0 {
			if err := r.staggerSleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	launched := r.now()
	r.markPane(ctx, pane)
	if err := r.mgr.StartAgent(ctx, pr, role, pane, resume); err != nil {
		return err
	}
	r.mu.Lock()
	r.lastStart = r.now()
	r.mu.Unlock()
	r.recordLaunch(ctx, pr, a, role, launched, launch)
	return nil
}

// recordLaunch writes the RoleRun facts of role's start in attempt a: the
// CLI and its version, the model, magnum's build, the session id and the
// transcript path when known, and, for the attempt's launch of the role
// (launch), the hashes of its prompt and its configuration. role is the
// role as the stage runs it (its prompt the stage's own when the stage
// names one), which a recovery after a restart reads afresh from the
// configuration: its start keeps the hashes the launch recorded. Failures
// are logged: the evidence is no reason to stop the launch.
func (r *Runtime) recordLaunch(ctx context.Context, pr store.PR, a registry.Attempt, role config.Role, launched time.Time, launch bool) {
	model, _, _ := agents.LaunchModel(ctx, r.st, r.cfg, role, r.now())
	rr := registry.RoleRun{AttemptID: a.ID, Role: role.Name, CLI: role.AgentKind(),
		CLIVersion: r.cliVersion(ctx, role), Model: cmp.Or(model, r.cfg.RoleModel(role), agents.UnknownModel),
		MagnumBuild: engine.CurrentBuild("", r.d.Layout.Binary()).Label(), Launched: launched}
	if launch {
		rr.PromptHash, rr.ConfigHash = r.promptHash(role), configHash(role)
		r.forget(outsideKey(a.ID, role.Name)) // the observe pass records its sessions now (launchedIn)
	}
	if s, err := r.st.LiveSessionByPRRole(ctx, pr.ID, role.Name); err == nil {
		rr.SessionID = store.Deref(s.SessionID)
		if rr.SessionID != "" {
			rr.Transcript, _ = r.mgr.TranscriptPath(ctx, s)
		}
	}
	if err := r.reg.RecordRoleRun(ctx, rr); err != nil {
		r.log.Warn("record a role's launch", "attempt", a.ID, "role", role.Name, "err", err)
	}
}

// cliVersion is `<kind> --version` of role's CLI, with the role's pane env,
// read at most once an hour per kind ("" when it does not answer).
func (r *Runtime) cliVersion(ctx context.Context, role config.Role) string {
	kind := role.AgentKind()
	now := r.now()
	r.mu.Lock()
	v, ok := r.versions[kind]
	r.mu.Unlock()
	if ok && now.Sub(v.at) < versionTTL {
		return v.v
	}
	res, err := r.d.Runner.Run(ctx, execx.Cmd{Name: kind, Args: []string{"--version"}, Env: r.cfg.RoleEnv(role),
		Timeout: versionWait, NoTTY: true, Label: kind + " --version", Quiet: true})
	out := ""
	if err == nil {
		out = textx.Clip(textx.FirstLine(strings.TrimSpace(string(res.Stdout))), 80)
	}
	r.mu.Lock()
	r.versions[kind] = cliVersion{v: out, at: now}
	r.mu.Unlock()
	return out
}

// promptHash is the first 12 hex digits of the sha256 of role's prompt
// file as the daemon loaded it (its shared blocks included); "" when it
// cannot be read.
func (r *Runtime) promptHash(role config.Role) string {
	if role.Prompt == "" {
		return ""
	}
	p, err := r.cfg.ResolvePrompt(role.Prompt)
	if err != nil {
		return ""
	}
	return shortHash(p.Shared + "\x00" + p.Text)
}

// configHash is the first 12 hex digits of the sha256 of role's
// configuration.
func configHash(role config.Role) string {
	b, err := json.Marshal(role)
	if err != nil {
		return ""
	}
	return shortHash(string(b))
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// paneEnv is the environment of task t's panes in copy c: the copy's env
// (the repository's, rendered for the copy) and MAGNUM_FACTORY_TASK.
// sessions.env_json keeps it, so a secret never gets there: a GitHub token's
// variable (slots.DefaultStripEnv) and a value execx.Redact would mask are
// left out.
func paneEnv(t registry.Task, c Copy) map[string]string {
	env := make(map[string]string, len(c.Env)+1)
	for k, v := range c.Env {
		if slices.Contains(slots.DefaultStripEnv, k) || execx.Redact(v) != v {
			continue
		}
		env[k] = v
	}
	env["MAGNUM_FACTORY_TASK"] = strconv.FormatInt(t.ID, 10)
	return env
}

// track notes role of attempt a as launched in the copy at path with the
// pane env env.
func (r *Runtime) track(t registry.Task, a registry.Attempt, role config.Role, path string, env map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := roleKey{a.ID, role.Name}
	if st := r.roles[k]; st != nil {
		st.copy, st.env = path, env
		return
	}
	r.roles[k] = &roleState{key: k, taskID: t.ID, stage: a.Stage, role: role, copy: path, env: env}
}

// copyOf is the copy role st works in and the env of its panes.
func (r *Runtime) copyOf(st *roleState) (path string, env map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return st.copy, maps.Clone(st.env)
}

// stateFor is the state of role (as it runs) in attempt id: the one Launch
// noted, else one rebuilt from the registry and the role's newest session
// (after a restart).
func (r *Runtime) stateFor(ctx context.Context, attempt, role string) (*roleState, error) {
	k := roleKey{attempt, role}
	r.mu.Lock()
	st := r.roles[k]
	r.mu.Unlock()
	if st != nil {
		return st, nil
	}
	a, err := r.reg.AttemptByID(ctx, attempt)
	if err != nil {
		return nil, err
	}
	spec, ok := r.d.Config.FactoryRole(role)
	if !ok {
		return nil, fmt.Errorf("attempt %s: no factory role %q", attempt, role)
	}
	st = &roleState{key: k, taskID: a.TaskID, stage: a.Stage, role: spec}
	if pr, err := r.ownerRow(ctx, a.TaskID); err == nil {
		if ss, err := r.st.SessionsByPR(ctx, pr.ID); err == nil {
			for _, s := range ss {
				if s.Role == role && store.Deref(s.Cwd) != "" {
					st.copy, st.env = store.Deref(s.Cwd), s.Env
				}
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev := r.roles[k]; prev != nil {
		return prev, nil
	}
	r.roles[k] = st
	return st, nil
}

// Verify settles an ended run: verified when ok, failed with why otherwise,
// so the agents store's active runs stay few. It records the RoleRun's end:
// the stage's end fact and why, the outcome, the nudges, the tokens by model
// and the active and idle seconds. A failure feeds the loop detector: the
// same why SameFailures times in a row is a loop without progress.
func (r *Runtime) Verify(ctx context.Context, runID string, ok bool, why string) error {
	outcome := outcomeFailed
	if ok {
		outcome = outcomeVerified
	}
	return r.settleRun(ctx, runID, outcome, why)
}

// Void settles an ended run whose turn did what it was asked and is set
// aside, neither verified nor failed: it ended on questions, which wait for
// your answers or got them since. The agents store keeps it failed with
// why and the outcome voided, so a stage rebuilt after a restart takes its
// end again (a failed run's), and nothing that counts failed turns counts
// it; its RoleRun records its end as Verify's does, the outcome voided. It
// feeds the loop detector nothing: it is no failure, and no progress
// either.
func (r *Runtime) Void(ctx context.Context, runID, why string) error {
	return r.settleRun(ctx, runID, outcomeVoided, why)
}

// The outcomes settleRun gives a run that ended (store.Run.Outcome; a
// RoleRun's failed one carries its cause).
const (
	outcomeVerified = "verified"
	outcomeFailed   = "failed"
	outcomeVoided   = "voided"
)

// settleRun is Verify and Void: run runID, ended, moves to verified (that
// outcome) or failed (the others), and its RoleRun records the end.
func (r *Runtime) settleRun(ctx context.Context, runID, outcome, why string) error {
	verb := "verify"
	if outcome == outcomeVoided {
		verb = "void"
	}
	run, err := r.st.RunByID(ctx, runID)
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, runID, err)
	}
	now := r.now()
	ok := outcome == outcomeVerified
	to := store.RunVerified
	if !ok {
		to = store.RunFailed
	}
	why = scrub.Text(strings.TrimSpace(why))
	if err := r.st.TransitionRun(ctx, runID, []string{store.RunEnded}, to, func(u *store.RunUpdate) {
		u.Set("outcome", outcome)
		if ok {
			u.Set("verified_at", now)
		} else {
			u.Set("error", why)
		}
	}); err != nil {
		return fmt.Errorf("%s %s: %w", verb, runID, err)
	}
	st, err := r.stateFor(ctx, run.TargetSHA, run.Role)
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, runID, err)
	}
	rr := registry.RoleRun{AttemptID: run.TargetSHA, Role: run.Role, EndFact: r.endFact(ctx, st), EndWhy: textx.Clip(why, 300)}
	if run.EndedAt != nil {
		rr.Ended = *run.EndedAt
	}
	switch outcome {
	case outcomeVerified:
		rr.Outcome, rr.Verified = outcomeVerified, now
	case outcomeVoided:
		rr.Outcome = outcomeVoided
	default:
		rr.Outcome = "failed:" + cmp.Or(textx.Clip(textx.FirstLine(why), 80), "unverified")
	}
	r.endFacts(ctx, &rr, run, st, now)
	if err := r.reg.RecordRoleRun(ctx, rr); err != nil {
		r.log.Warn("record a role's end", "run", runID, "err", err)
	}
	if outcome == outcomeVoided {
		return nil
	}
	r.mu.Lock()
	lk := loopKey{st.taskID, st.stage, run.Role}
	l := r.loops[lk]
	if l == nil {
		l = &loopState{}
		r.loops[lk] = l
	}
	if ok { // progress: the ladder starts over from here, and a recovery that waited is void
		l.fails, l.step = nil, 0
		l.reset = true
		delete(r.once, noProgressKey(lk))
		st.dueAt, st.recovering = time.Time{}, false
	} else {
		l.fails = append(l.fails, why)
		if len(l.fails) > SameFailures {
			l.fails = l.fails[len(l.fails)-SameFailures:]
		}
	}
	r.mu.Unlock()
	return nil
}

// endFact is the configured end fact of st's stage ("findings", "proof").
func (r *Runtime) endFact(ctx context.Context, st *roleState) string {
	t, err := r.reg.TaskByID(ctx, st.taskID)
	if err != nil {
		return ""
	}
	for _, s := range r.d.Config.Factory.Kinds[t.Kind].Stages {
		if s.Name == st.stage {
			return s.End
		}
	}
	return ""
}

// endFacts fills the counts of rr from the role's runs in the attempt: the
// nudges (its nudge and recovery runs and the watchdog's nudges), the
// tokens by model since its launch, and the active seconds (each run's
// submit to end) and the idle ones (the rest since the launch).
func (r *Runtime) endFacts(ctx context.Context, rr *registry.RoleRun, run store.Run, st *roleState, now time.Time) {
	r.mu.Lock()
	rr.Nudges = st.nudges
	r.mu.Unlock()
	runs, err := r.st.RunsByPR(ctx, run.PRID)
	if err != nil {
		return
	}
	var active time.Duration
	first := time.Time{}
	for _, x := range runs {
		if x.Role != run.Role || x.TargetSHA != run.TargetSHA {
			continue
		}
		if first.IsZero() || x.CreatedAt.Before(first) {
			first = x.CreatedAt
		}
		if x.Kind == store.RunNudge || x.Kind == store.RunRecovery {
			rr.Nudges++
		}
		if x.SubmittedAt != nil && x.EndedAt != nil && x.EndedAt.After(*x.SubmittedAt) {
			active += x.EndedAt.Sub(*x.SubmittedAt)
		}
	}
	rr.ActiveSecs = int(active / time.Second)
	if !first.IsZero() {
		rr.IdleSecs = max(0, int(now.Sub(first)/time.Second)-rr.ActiveSecs)
	}
	if run.SessionID != nil {
		if s, err := r.st.SessionByID(ctx, *run.SessionID); err == nil {
			if path, err := r.mgr.TranscriptPath(ctx, s); err == nil {
				rr.Tokens = tokensSince(s, path, first)
			}
		}
	}
}

// Park parks the agents of task t (agents.Manager.Park), then its liaison
// (parkLiaison): their conversations stay resumable, their panes are
// released. A run in flight or an agent at work on the task's row refuses
// with agents.ErrBusy; the liaison never stops the task's own park.
func (r *Runtime) Park(ctx context.Context, t registry.Task) error { return r.park(ctx, t, false) }

// ParkForReboot is Park for a drain for a reboot (drain.go): a liaison at
// work refuses it with agents.ErrBusy too, since the reboot would cut its
// turn, where Park leaves such a liaison live and goes through.
func (r *Runtime) ParkForReboot(ctx context.Context, t registry.Task) error {
	return r.park(ctx, t, true)
}

// park is Park; strict refuses a liaison at work (ParkForReboot).
func (r *Runtime) park(ctx context.Context, t registry.Task, strict bool) error {
	var errs []error
	pr, err := r.ownerRow(ctx, t.ID)
	switch {
	case errors.Is(err, store.ErrNotFound): // the task never launched an agent
	case err != nil:
		errs = append(errs, err)
	default:
		if err := r.mgr.Park(ctx, pr); err != nil {
			errs = append(errs, err)
		}
	}
	left, err := r.parkLiaison(ctx, t, strict)
	if left != "" {
		r.log.Info("left a task's liaison live: "+left, "task", t.ID)
	}
	if err != nil { // drain.go: ErrLiaisonLeftLive too, beside a failure of the task's own park, so the drain looks at the liaison again
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// parkLiaison parks task t's liaison unless you are with it: you in its
// pane (operatorOn: the factory leaves the panes you are in alone), or its
// turn in flight or its agent at work (agents.ErrBusy: it may be running
// the decide that parks the task), and one a Talk of the task works on
// right now (talkLock: Park does not wait for a Talk, which can take
// minutes). Such a liaison stays live, and left says why; the liaison of a
// task that ended, or that was published while it was live, is parked by
// a later observe pass once it may be (parkLeftLiaisons), and any other
// one by the next park, or you. A task you never talked about has no
// liaison to park. strict (ParkForReboot) makes a liaison at work an
// agents.ErrBusy, not one left.
func (r *Runtime) parkLiaison(ctx context.Context, t registry.Task, strict bool) (left string, err error) {
	tl := r.talkLock(t.ID)
	if !tl.TryLock() {
		return "a talk is starting it", nil
	}
	defer tl.Unlock()
	pr, err := liaisonRowIn(ctx, r.st, t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if r.operatorOn(ctx, pr, LiaisonRole) {
		if strict { // drain.go: what you typed runs a turn the reboot would cut, and your next message would
			if status, busy := r.liaisonAtWork(ctx, pr); busy {
				return "", fmt.Errorf("the liaison of %s is %s and you are in its pane: %w", taskKey(t), status, agents.ErrBusy)
			}
			return "", fmt.Errorf("the liaison of %s: %w", taskKey(t), ErrLiaisonLeftLive)
		}
		return "you are in its pane", nil
	}
	err = r.mgr.Park(ctx, pr)
	if errors.Is(err, agents.ErrBusy) {
		if strict {
			return "", fmt.Errorf("the liaison of %s: %w", taskKey(t), err)
		}
		return "it is at work (" + err.Error() + ")", nil
	}
	return "", err
}

// parkLeftLiaisons is the observe pass's park of the liaisons their task's
// park left live (parkLiaison) and nothing else would park: a live liaison
// of a task that ended (done, stopped, failed), which no talk starts again,
// and of a published task the liaison that was live before the publish
// (liaisonOwesPark). The decide you ask the liaison for in its chat (push
// it, drop it) comes with that park while you are in its pane and its turn
// is in flight, so the park leaves it, and the engine parks such a task no
// more. A pass parks such a liaison only once it has been idle for the
// human cooldown (liaisonIdleSince): your pick in its AskUserQuestion
// dialog and the decide it runs come within the turn the talk's prompt
// started, so nothing notes you in its pane, and you read its last message
// after that turn ended. Then parkLiaison's own guards hold: a talk at work,
// you in its pane (you typed into it, or a talk took you into it, within
// the cooldown), its agent at work. Each pass tries again; it logs the parks
// it makes, never a liaison it leaves (Park's log says why the first time),
// and a park that fails once per liaison per daemon start.
func (r *Runtime) parkLeftLiaisons(ctx context.Context) {
	live, err := r.st.LiveSessions(ctx)
	if err != nil {
		if r.first("liaisons-park") {
			r.log.Warn("park the liaisons their tasks' parks left live", "err", err)
		}
		return
	}
	now := r.now()
	tried, seen := map[int64]bool{}, map[int64]bool{}
	for _, s := range live {
		if s.Role != LiaisonRole {
			continue
		}
		seen[s.ID] = true
		pr, err := r.st.PRByID(ctx, s.PRID)
		if err != nil {
			continue
		}
		id, ok := LiaisonTask(pr.NodeID)
		if !ok || tried[id] {
			continue
		}
		tried[id] = true
		t, err := r.reg.TaskByID(ctx, id)
		if err != nil {
			continue
		}
		owed, err := r.liaisonOwesPark(ctx, t, s)
		var since time.Time
		idle := false
		if err == nil && owed {
			since, idle, err = r.liaisonIdleSince(ctx, pr, s, now)
		}
		if err != nil {
			r.parkFailed(t, err)
			continue
		}
		if !owed || !idle || now.Before(r.cfg.Daemon.CooldownUntil(&since)) {
			continue
		}
		switch left, err := r.parkLiaison(ctx, t, false); {
		case err != nil:
			r.parkFailed(t, err)
		case left == "":
			r.log.Info("parked the liaison its task's park left live", "task", t.ID, "state", t.State)
		}
	}
	r.mu.Lock()
	maps.DeleteFunc(r.idleSeen, func(id int64, _ time.Time) bool { return !seen[id] })
	r.mu.Unlock()
}

// parkFailed logs that the park of task t's left liaison failed with err,
// once per liaison per daemon start: each observe pass tries again.
func (r *Runtime) parkFailed(t registry.Task, err error) {
	if r.first(fmt.Sprintf("liaison-park:%d", t.ID)) {
		r.log.Warn("park a liaison its task's park left live", "task", t.ID, "err", err)
	}
}

// liaisonIdleSince is when s, the live session of a liaison on row pr, went
// idle: the later of the time its agent's status last changed, while herdr
// shows it neither at work nor blocked on a dialog (agent_status_at), and
// the end of the row's newest run that ended. Not idle (false) while its
// agent works or waits on a dialog. A time neither tells is the pass that
// first saw it so (now, kept until its session is live no more or works
// again), so it waits a whole cooldown from then.
func (r *Runtime) liaisonIdleSince(ctx context.Context, pr store.PR, s store.Session, now time.Time) (time.Time, bool, error) {
	if st := herdr.Status(store.Deref(s.AgentStatus)); st == herdr.StatusWorking || st == herdr.StatusBlocked {
		r.mu.Lock()
		delete(r.idleSeen, s.ID)
		r.mu.Unlock()
		return time.Time{}, false, nil
	}
	var since time.Time
	if s.AgentStatusAt != nil {
		since = *s.AgentStatusAt
	}
	runs, err := r.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return time.Time{}, false, err
	}
	for _, run := range runs {
		if run.EndedAt != nil && run.EndedAt.After(since) {
			since = *run.EndedAt
		}
	}
	if since.IsZero() {
		r.mu.Lock()
		at, ok := r.idleSeen[s.ID]
		if !ok {
			at = now
			r.idleSeen[s.ID] = at
		}
		r.mu.Unlock()
		since = at
	}
	return since, true, nil
}

// liaisonOwesPark reports whether s, task t's live liaison, is owed the
// park its task's end or publish came with: any of a task that ended
// (done, stopped, failed: a talk refuses such a task, so its liaison was
// live before the end, or a talk the end raced started it), and of a
// published task one that started before the task was published, never
// one a talk started since (you talk with a published task's liaison, and
// it does not park by itself).
func (r *Runtime) liaisonOwesPark(ctx context.Context, t registry.Task, s store.Session) (bool, error) {
	switch t.State {
	case registry.TaskDone, registry.TaskStopped, registry.TaskFailed:
		return true, nil
	case registry.TaskPublished:
	default:
		return false, nil
	}
	spans, err := r.reg.Spans(ctx, t.ID)
	if err != nil {
		return false, err
	}
	for _, sp := range slices.Backward(spans) {
		if sp.To == registry.TaskPublished && sp.From != registry.TaskPublished {
			return s.StartedAt.Before(sp.At), nil // the move into published
		}
	}
	return false, nil
}

// RunState is run id as the agents store keeps it: its state (store.Run*:
// pending, submitted or working while in flight, then ended, verified,
// failed or abandoned) and, for a run that failed or was given up, why (a
// run its CLI's pause ended is abandoned with PausedPrefix and the pause).
// The store holds it across a restart, which the events the observe pass
// gave, and the pauses, do not survive. An unknown run wraps
// store.ErrNotFound.
func (r *Runtime) RunState(ctx context.Context, id string) (state, why string, err error) {
	run, err := r.st.RunByID(ctx, id)
	if err != nil {
		return "", "", fmt.Errorf("run %s: %w", id, err)
	}
	return run.State, store.Deref(run.Error), nil
}

// AtWork names who is at work in task t's copy, "" for nobody: the role
// whose run is in flight (pending, submitted or working: its agent may be
// changing the copy), or, you true, the role whose pane you are in (you
// typed into one of the task's panes within the human cooldown, or herdr
// shows its pane focused; a herdr that cannot say counts as in it), where
// your edits are. The factory's own commands that change the task's copy
// (its proof's base run, its restore) wait for both. A task that never
// launched has nobody at work.
func (r *Runtime) AtWork(ctx context.Context, t registry.Task) (role string, you bool, err error) {
	pr, err := r.ownerRow(ctx, t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("the runs of %s: %w", taskKey(t), err)
	}
	runs, err := r.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return "", false, fmt.Errorf("the runs of %s: %w", taskKey(t), err)
	}
	for _, run := range runs {
		switch run.State {
		case store.RunPending, store.RunSubmitted, store.RunWorking:
			return run.Role, false, nil
		}
	}
	sessions, err := r.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return "", false, fmt.Errorf("the sessions of %s: %w", taskKey(t), err)
	}
	for _, s := range sessions {
		if s.State != store.SessionStarting && s.State != store.SessionLive {
			continue
		}
		if r.operatorIn(ctx, &roleState{taskID: t.ID, role: config.Role{Name: s.Role}}) {
			return s.Role, true, nil
		}
	}
	return "", false, nil
}

// TouchedSince names who could have changed task t's copy since the time
// since, "" for nobody: a prompt to one of its agents after it (a run made
// after it), or your input into one of its panes after it (the owner row's
// human_active_at). The factory's checks ask it before they put back what
// their own commands changed: with somebody else at work since they began,
// they cannot tell their changes from that person's or that agent's.
func (r *Runtime) TouchedSince(ctx context.Context, t registry.Task, since time.Time) (string, error) {
	pr, err := r.ownerRow(ctx, t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("the runs of %s: %w", taskKey(t), err)
	}
	if at := pr.HumanActiveAt; at != nil && at.After(since) {
		return "you typed into a pane of " + taskKey(t) + " at " + at.Local().Format("15:04:05"), nil
	}
	runs, err := r.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return "", fmt.Errorf("the runs of %s: %w", taskKey(t), err)
	}
	for _, run := range slices.Backward(runs) {
		if run.CreatedAt.After(since) {
			return "the " + run.Role + " agent got a prompt at " + run.CreatedAt.Local().Format("15:04:05"), nil
		}
	}
	return "", nil
}

// QuitCodex quits the live Codex sessions of task t, once Codex never works
// on it again (Task.CodexNever, a refusal): none of them gets another
// prompt, and no conversation of theirs is resumed (a restart or a
// recovery of a Codex role refuses on such a task, mayRestart). Each
// session's runs in flight are abandoned first, and the session is quit
// hard (agents.Manager.QuitHard): a Codex agent at work on the task must
// stop. A session whose pane you are in is left for a later call
// (ErrOperatorInPane in the error), its runs abandoned all the same, and a
// quit that fails does not stop the others; the errors come back joined. A task that never launched has
// nothing to quit.
func (r *Runtime) QuitCodex(ctx context.Context, t registry.Task) error {
	return r.quitSessions(ctx, t, "Codex", func(s store.Session) bool { return store.Deref(s.AgentKind) == agents.KindCodex },
		"Codex never works on "+taskKey(t)+" again")
}

// QuitRole quits the live sessions of task t's role (as it runs), whatever
// its kind, because why: a qa stage's walker whose run was cut (its browser
// and its server stopped) must not go on driving them, nor hold the Mac's
// memory. Its runs in flight are abandoned first, and each session is quit
// hard, as QuitCodex quits; its conversation stays resumable. A session
// whose pane you are in is left (ErrOperatorInPane in the error), its runs
// abandoned all the same; a task that never launched has nothing to quit.
func (r *Runtime) QuitRole(ctx context.Context, t registry.Task, role, why string) error {
	return r.quitSessions(ctx, t, role, func(s store.Session) bool { return s.Role == role }, why)
}

// quitSessions quits task t's starting and live sessions that match (what
// names them in errors and the log), each run in flight abandoned first
// with why, and leaves those whose pane you are in. Their runs are
// abandoned too: the caller waits for them no more, an abandon is the
// agents store's (no key reaches your pane), and a run left in flight would
// have the watchdog take its steps in a pane the factory gave up.
func (r *Runtime) quitSessions(ctx context.Context, t registry.Task, what string, match func(store.Session) bool, why string) error {
	pr, err := r.ownerRow(ctx, t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quit the %s sessions of %s: %w", what, taskKey(t), err)
	}
	sessions, err := r.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return fmt.Errorf("quit the %s sessions of %s: %w", what, taskKey(t), err)
	}
	var errs []error
	for _, s := range sessions {
		if (s.State != store.SessionStarting && s.State != store.SessionLive) || !match(s) {
			continue
		}
		r.abandonRuns(ctx, pr, s.Role, why)
		if r.operatorIn(ctx, &roleState{taskID: t.ID, role: config.Role{Name: s.Role}}) {
			errs = append(errs, fmt.Errorf("quit %s's %s session: %w", s.Role, what, ErrOperatorInPane))
			continue
		}
		if err := r.mgr.QuitHard(ctx, s); err != nil {
			errs = append(errs, fmt.Errorf("quit %s's %s session: %w", s.Role, what, err))
			continue
		}
		r.log.Info("quit a "+what+" session: "+why, "task", t.ID, "role", s.Role)
	}
	return errors.Join(errs...)
}

// Shift moves every turn deadline and progress window by gap, the time the
// Mac slept (the engine's wake): no run counts as hung for a sleep.
func (r *Runtime) Shift(gap time.Duration) {
	if gap <= 0 {
		return
	}
	runs, err := r.st.ActiveRuns(context.Background())
	if err != nil {
		r.log.Warn("shift the turn deadlines", "err", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.roles {
		if !st.since.IsZero() {
			st.since = st.since.Add(gap)
		}
	}
	for _, run := range runs {
		r.shifts[run.ID] += gap
	}
}

// slogLogger is the agents manager's execx.AttrLogger over the runtime's
// slog.Logger.
type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Printf(format string, args ...any) { s.Logf(slog.LevelInfo, format, args...) }

func (s slogLogger) Logf(level slog.Level, format string, args ...any) {
	s.l.Log(context.Background(), level, fmt.Sprintf(format, args...), "src", "agents")
}

func (s slogLogger) LogAttrs(level slog.Level, msg string, attrs ...slog.Attr) {
	s.l.LogAttrs(context.Background(), level, msg, append([]slog.Attr{slog.String("src", "agents")}, attrs...)...)
}
