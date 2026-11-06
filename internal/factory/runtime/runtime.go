package runtime

// The runtime drives the factory's agents through magnum's agents.Manager
// over the factory's own agents store (Layout.FactoryAgentsDB): one owner row
// per task (owner.go), the launch of a stage's roles, recorded on the attempt
// before any workspace exists, the prompts it sends through its ledger
// (deliver.go), the observe pass that turns the panes into events and heals
// what it can (health.go), and the watchdog with its recovery steps
// (watchdog.go). The real state of a task is the factory registry's; the
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
	"github.com/zhuravel/magnum/internal/paths"
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
	// MaxResumes is how often a lost pane's conversation is resumed in one
	// attempt before a fresh session starts from the files.
	MaxResumes = 2
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
// Clock nil is time.Now; Log nil discards.
type Deps struct {
	Config   *config.Config
	Layout   paths.Layout
	Herdr    agents.Herdr
	Store    *store.Store
	Registry *registry.Registry
	Runner   execx.Runner
	Memory   func(ctx context.Context) engine.MemorySample
	Clock    func() time.Time
	Log      *slog.Logger
}

// seams are what New takes from the machine and a test replaces: the sleep
// (stagger, quits, switches), the reading of Codex's budget and login, the
// signal StopShell may send, the read of a transcript's user turns, and the
// CLIs' homes the agents manager reads transcripts from ("" = the CLI's own
// default).
type seams struct {
	sleep                func(ctx context.Context, d time.Duration) error
	usage                func(ctx context.Context, now time.Time) (usage.Snapshot, error)
	login                func() (usage.Login, error)
	signal               func(pid int, sig syscall.Signal) error
	userTurns            func(kind, path string) ([]agents.UserTurn, error)
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

	// launchMu makes the staggered launches take turns.
	launchMu sync.Mutex
	// drained closes when the daemon drains (BeginDrain): a launch that
	// waits for the stagger then starts nothing.
	drained   chan struct{}
	drainOnce sync.Once

	mu         sync.Mutex
	lastStart  time.Time
	keys       map[int64]string // task id -> its key, the owner name in titles
	roles      map[roleKey]*roleState
	loops      map[loopKey]*loopState
	pauses     map[string]*pause        // agent kind -> its pause
	misses     map[int64]time.Time      // ledger entry -> the first read that did not find it
	shifts     map[string]time.Duration // run id -> the sleeps its deadline moved by
	once       map[string]bool          // events and records written once
	checked    map[int64]time.Time      // Codex session -> when TookNoPrompt last read it
	pending    []Event                  // events found outside Observe
	refused    map[int64]bool           // task -> a Codex refusal: its Codex roles never run again
	loginFP    string
	loginRead  time.Time
	reconciled time.Time
	versions   map[string]cliVersion
	typedRead  time.Time             // the last read of the turns typed into the panes
	seen       map[string]fileMark   // conversation -> its transcript as the last read that took it whole saw it
	youAt      map[int64][]time.Time // session -> when herdr showed you in its pane (sawYou)
	commands   map[string]bool       // the slash commands magnum types (agents.MagnumCommands)
	focus      PaneFocus             // you in the factory's panes, as the observe passes saw you (noteFocus)
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
	// Recovery of a lost pane: resumes used, whether the fresh session
	// started, whether a recovery waits for progress, and whether the
	// recoveries ran out; noPrompt counts the fresh starts of a Codex
	// session that never took its first prompt.
	resumes    int
	fresh      bool
	recovering bool
	lost       bool
	noPrompt   int
	nudges     int
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
	if s.codexHome == "" {
		s.codexHome = d.Config.Usage.CodexHome
	}
	r := &Runtime{d: d, cfg: &cfg, st: d.Store, reg: d.Registry, log: log, s: s,
		keys: map[int64]string{}, roles: map[roleKey]*roleState{}, loops: map[loopKey]*loopState{},
		pauses: map[string]*pause{}, misses: map[int64]time.Time{}, shifts: map[string]time.Duration{},
		once: map[string]bool{}, checked: map[int64]time.Time{}, refused: map[int64]bool{}, versions: map[string]cliVersion{},
		seen: map[string]fileMark{}, youAt: map[int64][]time.Time{}, commands: agents.MagnumCommands(&cfg),
		drained: make(chan struct{})}
	r.mgr = agents.New(agents.Deps{Herdr: d.Herdr, Store: d.Store, Runner: d.Runner, Config: &cfg, Layout: d.Layout,
		Clock: d.Clock, Sleep: s.sleep, ClaudeDir: s.claudeDir, CodexHome: s.codexHome, Log: slogLogger{log},
		Tag: Tag, OwnerName: r.ownerName, Signal: s.signal})
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
	if r.now().Before(r.cfg.Daemon.CooldownUntil(pr.HumanActiveAt)) {
		return true
	}
	s, err := r.st.LiveSessionByPRRole(ctx, pr.ID, st.role.Name)
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
		st.recovering, st.step, st.sig, st.since = false, 0, "", time.Time{}
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
		if err := r.start(ctx, pr, a, role, ws.Panes[agents.Role(role.Name)], resume, levels[i] == engine.MemoryWarn); err != nil {
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
	if usage.Decide(snap, u.CodexSoft, u.CodexHard) == usage.OK {
		return level, nil
	}
	reset := binding(snap).ResetsAt
	what := fmt.Sprintf("codex at %.0f%%", snap.Used())
	if !reset.IsZero() {
		what += ", waits till " + r.clock(reset)
	} else {
		what += fmt.Sprintf(", waits till it is below %.0f%%", u.CodexSoft)
	}
	return level, &WaitError{Who: "usage", What: what, Until: reset}
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
func (r *Runtime) clock(t time.Time) string {
	now := r.now()
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
// Stagger after the last start when stagger, and records its RoleRun facts.
func (r *Runtime) start(ctx context.Context, pr store.PR, a registry.Attempt, role config.Role, pane, resume string, stagger bool) error {
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
	if err := r.mgr.StartAgent(ctx, pr, role, pane, resume); err != nil {
		return err
	}
	r.mu.Lock()
	r.lastStart = r.now()
	r.mu.Unlock()
	r.recordLaunch(ctx, pr, a, role, launched)
	return nil
}

// recordLaunch writes the RoleRun facts of role's launch in attempt a: the
// CLI and its version, the model, magnum's build, the prompt's and the
// role's hashes, the session id and the transcript path when known.
// Failures are logged: the evidence is no reason to stop the launch.
func (r *Runtime) recordLaunch(ctx context.Context, pr store.PR, a registry.Attempt, role config.Role, launched time.Time) {
	model, _, _ := agents.LaunchModel(ctx, r.st, r.cfg, role, r.now())
	rr := registry.RoleRun{AttemptID: a.ID, Role: role.Name, CLI: role.AgentKind(),
		CLIVersion: r.cliVersion(ctx, role), Model: cmp.Or(model, r.cfg.RoleModel(role), agents.UnknownModel),
		MagnumBuild: engine.CurrentBuild("", r.d.Layout.Binary()).Label(), PromptHash: r.promptHash(role),
		ConfigHash: configHash(role), Launched: launched}
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
	run, err := r.st.RunByID(ctx, runID)
	if err != nil {
		return fmt.Errorf("verify %s: %w", runID, err)
	}
	now := r.now()
	to := store.RunVerified
	if !ok {
		to = store.RunFailed
	}
	why = execx.Redact(strings.TrimSpace(why))
	if err := r.st.TransitionRun(ctx, runID, []string{store.RunEnded}, to, func(u *store.RunUpdate) {
		if ok {
			u.Set("verified_at", now)
			u.Set("outcome", "verified")
		} else {
			u.Set("error", why)
			u.Set("outcome", "failed")
		}
	}); err != nil {
		return fmt.Errorf("verify %s: %w", runID, err)
	}
	st, err := r.stateFor(ctx, run.TargetSHA, run.Role)
	if err != nil {
		return fmt.Errorf("verify %s: %w", runID, err)
	}
	rr := registry.RoleRun{AttemptID: run.TargetSHA, Role: run.Role, EndFact: r.endFact(ctx, st), EndWhy: textx.Clip(why, 300)}
	if run.EndedAt != nil {
		rr.Ended = *run.EndedAt
	}
	if ok {
		rr.Outcome, rr.Verified = "verified", now
	} else {
		rr.Outcome = "failed:" + cmp.Or(textx.Clip(textx.FirstLine(why), 80), "unverified")
	}
	r.endFacts(ctx, &rr, run, st, now)
	if err := r.reg.RecordRoleRun(ctx, rr); err != nil {
		r.log.Warn("record a role's end", "run", runID, "err", err)
	}
	r.mu.Lock()
	lk := loopKey{st.taskID, st.stage, run.Role}
	l := r.loops[lk]
	if l == nil {
		l = &loopState{}
		r.loops[lk] = l
	}
	if ok { // progress: the ladder starts over from here
		l.fails, l.step = nil, 0
		l.reset = true
		delete(r.once, noProgressKey(lk))
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

// Park parks the agents of task t (agents.Manager.Park): their
// conversations stay resumable, their panes are released. A run in flight
// or an agent at work refuses with agents.ErrBusy.
func (r *Runtime) Park(ctx context.Context, t registry.Task) error {
	pr, err := r.ownerRow(ctx, t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // the task never launched an agent
	}
	if err != nil {
		return err
	}
	return r.mgr.Park(ctx, pr)
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
// (ErrOperatorInPane in the error), and a quit that fails does not stop the
// others; the errors come back joined. A task that never launched has
// nothing to quit.
func (r *Runtime) QuitCodex(ctx context.Context, t registry.Task) error {
	pr, err := r.ownerRow(ctx, t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quit the Codex sessions of %s: %w", taskKey(t), err)
	}
	sessions, err := r.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return fmt.Errorf("quit the Codex sessions of %s: %w", taskKey(t), err)
	}
	var errs []error
	for _, s := range sessions {
		if (s.State != store.SessionStarting && s.State != store.SessionLive) || store.Deref(s.AgentKind) != agents.KindCodex {
			continue
		}
		if r.operatorIn(ctx, &roleState{taskID: t.ID, role: config.Role{Name: s.Role}}) {
			errs = append(errs, fmt.Errorf("quit %s's Codex session: %w", s.Role, ErrOperatorInPane))
			continue
		}
		r.abandonRuns(ctx, pr, s.Role, "Codex never works on "+taskKey(t)+" again")
		if err := r.mgr.QuitHard(ctx, s); err != nil {
			errs = append(errs, fmt.Errorf("quit %s's Codex session: %w", s.Role, err))
			continue
		}
		r.log.Info("quit a Codex session: Codex never works on the task again", "task", t.ID, "role", s.Role)
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
