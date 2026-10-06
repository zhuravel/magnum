// Package engine is magnum's daemon: one tick loop that polls GitHub,
// observes herdr, applies health pauses, consumes CLI requests, dispatches
// review rounds into slots (each round in its own goroutine), releases closed
// PRs after their grace and reconciles the registry with disk, MySQL and herdr.
// Slow slot work (provisioning, eviction, cleanup, reconcile) runs one job at a
// time on a background heavy worker that reports only through store rows.
//
// The engine owns the PR state machine on top of the store's compare-and-set
// transitions; eligibility.Throttle is the single source of truth for
// re-review timing (prs.next_eligible_at, prs.pending_since), and
// store.Candidates only backstops it.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/reveal"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/usage"
)

// GitHub is the part of *github.Client the engine uses (one client per
// identity): the poller's reads, and the dismissal of an App approval the
// head left behind (followApproval), made as the App identity.
type GitHub interface {
	Radar(ctx context.Context, org string) ([]github.RepoRadar, github.RateLimit, error)
	// CIStates reads the head rollups of the watched repositories' open PRs
	// on every poll (readCI, poll.go); the radar carries none.
	CIStates(ctx context.Context, prs []github.PRRadar) (map[string]string, github.RateLimit, error)
	Details(ctx context.Context, owner, repo string, numbers []int) (map[int]github.PRDetails, []int, error)
	ConfirmStates(ctx context.Context, owner, repo string, numbers []int) (map[int]github.PRState, []int, error)
	Compare(ctx context.Context, owner, repo, base, head string) (github.CompareStats, error)
	// CompareFiles reads base...head's changed files with their patches
	// (the PR's own diff against its base, base_merge.go; triage; reruns).
	CompareFiles(ctx context.Context, owner, repo, base, head string) ([]github.FileDelta, error)
	// ComparePush is CompareFiles with GitHub's status and whether the
	// range has a merge commit (the trivial-delta check, delta.go).
	ComparePush(ctx context.Context, owner, repo, base, head string) (github.PushComparison, error)
	// ReviewsWithMarker with an empty marker lists a PR's last 30 reviews:
	// the since_review fallback when Details' latestReviews was truncated.
	ReviewsWithMarker(ctx context.Context, owner, repo string, number int, marker string) ([]github.Review, error)
	DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) error
	// CreateReview posts a manual verdict (verdict.go).
	CreateReview(ctx context.Context, owner, repo string, number int, commitID, event, body string) (github.RESTReview, error)
	// RequiredChecks reads the checks a branch requires (required.go).
	RequiredChecks(ctx context.Context, owner, repo, branch string) ([]string, bool, error)
}

// Herdr is the part of *herdr.Client the tick reads: one snapshot per tick
// (agent statuses for completion, the working-Codex gate, liveness).
type Herdr interface {
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
}

// Agents is the part of *agents.Manager the engine drives.
type Agents interface {
	ObserveSnapshotAt(ctx context.Context, snap herdr.Snapshot, capturedAt time.Time) ([]agents.Observation, error)
	EnsureWorkspace(ctx context.Context, pr store.PR, slotPath string, env map[string]string, label string, roles []config.Role) (agents.Workspace, error)
	EnsurePane(ctx context.Context, pr store.PR, ws agents.Workspace, slotPath string, env map[string]string, role config.Role) (agents.Workspace, error)
	StartAgent(ctx context.Context, pr store.PR, role config.Role, paneID, resume string) error
	ResumeID(ctx context.Context, prID int64, role agents.Role) (string, error)
	Preflight(ctx context.Context, kind string) error
	Park(ctx context.Context, pr store.PR) error
	// Quit stops one session's agent and parks its conversation (a cold
	// judge, coldJudge; a session parkReloading takes out of the checkout's
	// way).
	Quit(ctx context.Context, s store.Session) error
	// ReloadsProject reports whether a session's agent reloads the
	// checkout's project config it loaded while it runs (parkReloading).
	ReloadsProject(ctx context.Context, s store.Session) bool
	Recover(ctx context.Context, pr store.PR) ([]agents.Recovered, error)
}

// Rounds runs one review round (*pipeline.Runner, one per identity) and
// edits the reviews its identity posted.
type Rounds interface {
	RunRound(ctx context.Context, in pipeline.RoundInput) (pipeline.RoundResult, error)
	// AppendToReview adds a last paragraph to a review the identity posted.
	AppendToReview(ctx context.Context, owner, repo string, number int, reviewID int64, text string) error
}

// Slots is the part of *slots.Manager the engine drives.
type Slots interface {
	Claim(ctx context.Context, pr store.PR, pool config.Pool) (store.Slot, error)
	Checkout(ctx context.Context, slot store.Slot, pr store.PR, pool config.Pool, targetSHA string) error
	// Fetch and CheckoutFetched split Checkout around its fetch, for a
	// round that reads the head before the checkout moves (parkReloading).
	Fetch(ctx context.Context, slot store.Slot, pr store.PR, pool config.Pool) (string, error)
	CheckoutFetched(ctx context.Context, slot store.Slot, pr store.PR, pool config.Pool, targetSHA, fetched string) error
	CreatePRWorktree(ctx context.Context, watch config.Watch, repo string, pr store.PR, targetSHA string) (store.Slot, error)
	Release(ctx context.Context, slot store.Slot, pool config.Pool, reason string) error
	ProvisionPool(ctx context.Context, pool config.Pool, n int) error
	NextSlotNumber(ctx context.Context, pool config.Pool) (int, error)
	Pin(ctx context.Context, slot store.Slot) error
	Unpin(ctx context.Context, slot store.Slot) error
	// ClearPin and Guard serve a review request of a pinned PR
	// (review_unpin.go): the pin goes, the guard's holds stay and are named.
	ClearPin(ctx context.Context, slot store.Slot) error
	Guard(ctx context.Context, slot store.Slot) error
	// Reserve, Repair and Adopt serve the open, repair and adopt requests.
	Reserve(ctx context.Context, pr store.PR, pool config.Pool) (store.Slot, error)
	Repair(ctx context.Context, slot store.Slot, pool config.Pool) error
	Adopt(ctx context.Context, pool config.Pool, path string) (store.Slot, error)
	// CheckSchema, ForgetSchema and RecordSchema decide and record a round's
	// schema reload (readinessPlan); EnsureSchema reloads for magnum open.
	CheckSchema(ctx context.Context, slot store.Slot, pool config.Pool) (slots.SchemaCheck, error)
	ForgetSchema(ctx context.Context, slot store.Slot) error
	RecordSchema(ctx context.Context, slot store.Slot, c slots.SchemaCheck) error
	EnsureSchema(ctx context.Context, slot store.Slot, pool config.Pool) (string, error)
}

// Git is the part of *gitx.Client the engine reads (round context, clone
// discovery for repos.clone_path).
type Git interface {
	MergeBase(ctx context.Context, dir, a, b string) (string, error)
	FindClone(ctx context.Context, cloneRoot, owner, name string) (string, error)
	// RevParse and FetchCommit find the first parent of a merged PR's merge
	// commit, the base a post-merge round reviews from (postMergeBase).
	RevParse(ctx context.Context, dir, ref string) (string, error)
	FetchCommit(ctx context.Context, mainClone, sha string, number int) error
	// ChangedUnder tells whether a head changes the project config a
	// running agent reloads (parkReloading).
	ChangedUnder(ctx context.Context, dir, base, head string, paths ...string) ([]string, error)
}

// Cleaner plans and applies storage cleanup (*cleanup.Planner). PlanFrom
// plans over an inventory the reconcile already scanned.
type Cleaner interface {
	Plan(ctx context.Context, opts cleanup.Options) (cleanup.Plan, error)
	PlanFrom(ctx context.Context, opts cleanup.Options, inv inventory.Inventory) (cleanup.Plan, error)
	Apply(ctx context.Context, plan cleanup.Plan, confirmed bool) (cleanup.Report, error)
}

// Inventory is the reconcile scanner (*inventory.Scanner).
type Inventory interface {
	Scan(ctx context.Context, opts inventory.Options) (inventory.Inventory, error)
	UpsertSlotDatabases(ctx context.Context, inv inventory.Inventory) (inventory.SyncResult, error)
}

// Compile-time checks that the real components satisfy the ports.
var (
	_ GitHub    = (*github.Client)(nil)
	_ Herdr     = (*herdr.Client)(nil)
	_ Agents    = (*agents.Manager)(nil)
	_ Rounds    = (*pipeline.Runner)(nil)
	_ Slots     = (*slots.Manager)(nil)
	_ Git       = (*gitx.Client)(nil)
	_ Cleaner   = (*cleanup.Planner)(nil)
	_ Inventory = (*inventory.Scanner)(nil)
)

// Deps are the engine's collaborators. Config, Store and Logger are
// required; a nil port disables the steps that need it.
type Deps struct {
	Config *config.Config
	Layout paths.Layout
	Store  *store.Store
	Logger *slog.Logger
	// Now is the clock (nil = time.Now); Sleep waits honouring ctx (nil = a
	// timer). Tests replace both.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// DryRun: every decision is made (against the store the caller passes,
	// which app.New makes a private copy) but nothing outside the store is
	// changed: no herdr calls besides the snapshot, no agents, no rounds, no
	// slot or cleanup side effects, no toasts. See Recorder.
	DryRun bool

	GitHub     func(identity string) GitHub // poll client per identity (nil = none)
	Herdr      Herdr
	Agents     Agents
	Rounds     func(identity string) Rounds // round runner per identity (nil = none)
	Slots      Slots
	Git        Git
	Inventory  Inventory
	Cleanup    Cleaner
	Notifier   *notify.Notifier
	Identities map[string]identity.Source
	// Runner runs the triage command ([triage]); nil = a round that would
	// triage runs every role.
	Runner execx.Runner
	// Classifier sets up the retro's classifier for one retro run ([learn],
	// retro.go); nil = the candidates are stored unclassified.
	Classifier func(ctx context.Context, run RetroRun) (Classifier, error)
	// Curator sets up the notes curator for one curation ([notes],
	// notes_curate.go); nil = no curation runs.
	Curator func(ctx context.Context, run CurateRun) (Curator, error)

	// Usage reads Codex's rate-limit snapshot (usage.Codex, which FromApp
	// sets); nil = no budget gauge and no caps.
	Usage func(ctx context.Context, codexHome string, now time.Time) (usage.Snapshot, error)
	// Probe checks that a clone's origin answers (`git ls-remote` with the
	// environment gitx gives git); it lifts an infrastructure pause. nil =
	// no probe: the pause lifts when its backoff ends.
	Probe func(ctx context.Context, dir string) error

	// Focus focuses a herdr agent or pane ((*herdr.Client).AgentFocus) and
	// Reveal brings the herdr client to the front of the terminal
	// (reveal.Reveal). With [terminal] reveal_on_attention both run when a
	// PR needs attention or an agent is blocked; nil skips them.
	Focus  func(ctx context.Context, target string) error
	Reveal func(ctx context.Context) error
}

// Options tune Run.
type Options struct {
	// Once runs startup and a single tick, then keeps observing herdr only
	// until the rounds that tick launched are finished and the heavy jobs
	// queued meanwhile are done (the heavy worker runs alongside, as in the
	// daemon, so rounds are observed while heavy work runs), and returns.
	Once bool
	// NoSignals skips installing SIGTERM/SIGINT (stop) and SIGUSR1 (tick now)
	// handlers.
	NoSignals bool
	// Build is this daemon's build (CurrentBuild), recorded at startup as
	// KVDaemonBuild.
	Build Build
	// CheckBuild checks a binary found on disk before the daemon restarts on
	// it ([daemon] restart_on_new_build): it returns the binary's version, or
	// why the binary refuses the configuration. nil = no restart for a new
	// build.
	CheckBuild func(ctx context.Context, path string) (version string, err error)
	// Supervised: launchd runs this daemon and starts it again when it exits
	// non-zero, so it may exit for a new build.
	Supervised bool
}

// Engine is the daemon. Create it with New or FromApp.
type Engine struct {
	d   Deps
	cfg *config.Config
	st  *store.Store
	log *slog.Logger
	rec *Recorder

	mu       sync.Mutex
	rounds   map[int64]*roundHandle // PRs reserved by a round or an open (reserve)
	evicting map[int64]string       // PRs under slot work (an eviction, a park) and why (reserveSlotWork)
	roundWG  sync.WaitGroup

	heavy     chan heavyJob
	heavyMu   sync.Mutex
	heavyKeys map[string]bool
	inflight  map[int64]bool // async requests handed to the heavy worker

	kick          chan struct{}
	batch         *notify.Batcher
	lastReconcile time.Time
	midPoll       bool // requestsMidPoll is answering requests (the tick's goroutine only)
	herdrUp       *bool
	starts        starter
	dryRounds     int // rounds a dry run planned this tick
	lastSeen      map[string]string
	cleanupTried  map[int64]time.Time // close-grace cleanup attempts per PR
	// deltasRechecked: the first poll checked the old delta records again
	// (recheckDeltas, delta.go).
	deltasRechecked bool
	// logged: when each repeating error was last logged (logOnce, util.go).
	logged map[string]time.Time
	// compares are this tick's GitHub comparisons (compare.go).
	compares compareMemo

	// Urgent toasts run in their own goroutines on toastCtx (surface.go).
	toastMu      sync.Mutex
	toastsClosed bool
	toastWG      sync.WaitGroup
	toastCtx     context.Context
	toastStop    context.CancelFunc
	toastWait    time.Duration // how long shutdown waits for them (0 = toastDrain)
	lastTabBar   string
	// held and pauseToasted are the tick's memory of what it told the
	// operator (operator.go): requested rounds held by them, and the
	// review requests the running pause holds that were toasted.
	held         map[int64]heldWait
	pauseToasted map[string]bool
	// needsMe are the toast keys of the PRs that need the operator offered
	// to the batcher in this run (needs_me.go).
	needsMe map[string]bool
	// autoSeen and autoFollow remember, by PR and by automatic approval,
	// what auto-approval last read GitHub's reviews for (autoapprove.go): a
	// PR whose latest round and updatedAt did not move since is not asked
	// again. Tick goroutine only.
	autoSeen, autoFollow map[int64]string

	infraMu  sync.Mutex // infrastructure failures (infra.go)
	depsFail depsFailure

	// netRuns are the identity checks and token refreshes that could not
	// reach GitHub, retried with backoff (identity_net.go).
	netMu   sync.Mutex
	netRuns map[string]netRun

	// build is this daemon's build, checkBuild and supervised come from
	// Options, and builds tracks new builds on disk (build.go).
	build      Build
	checkBuild func(ctx context.Context, path string) (string, error)
	supervised bool
	builds     buildWatch

	once         bool       // Run with Options.Once: no daily retro (retro.go)
	retroMu      sync.Mutex // the running retro (retro.go)
	retroCancel  context.CancelFunc
	retroStarted time.Time
	retroWG      sync.WaitGroup

	curateMu      sync.Mutex // the running notes curation (notes_curate.go)
	curateCancel  context.CancelFunc
	curateStarted time.Time
	curateRepo    string
	curateWG      sync.WaitGroup
	curateTried   map[int64]time.Time // repositories whose last curation stored nothing, and when
	curateChecked time.Time           // the last scan for a curation due (the tick's goroutine only)

	usageMu   sync.Mutex // the Codex budget (budget.go)
	usageRead time.Time
	usageSnap *usage.Snapshot
	// usageRecorded is what recordBudget last wrote to kv.
	usageRecorded string
}

// New returns an engine over d.
func New(d Deps) *Engine {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = sleepCtx
	}
	e := &Engine{
		d: d, cfg: d.Config, st: d.Store, log: d.Logger,
		rounds:       map[int64]*roundHandle{},
		evicting:     map[int64]string{},
		heavy:        make(chan heavyJob, 64),
		heavyKeys:    map[string]bool{},
		inflight:     map[int64]bool{},
		lastSeen:     map[string]string{},
		logged:       map[string]time.Time{},
		cleanupTried: map[int64]time.Time{},
		curateTried:  map[int64]time.Time{},
		netRuns:      map[string]netRun{},
		kick:         make(chan struct{}, 1),
		starts:       starter{codex: make(chan struct{}, maxCodexStarts)},
	}
	e.rec = &Recorder{log: d.Logger, st: d.Store, now: d.Now}
	e.toastCtx, e.toastStop = context.WithCancel(context.Background())
	if d.Notifier != nil && !d.DryRun {
		// Informational toasts (reviews posted, new repositories) share one
		// batch; its summary title counts each kind.
		e.batch = &notify.Batcher{Notifier: d.Notifier, Key: "info", Now: d.Now}
	}
	return e
}

// FromApp wires an engine to the App's real components.
func FromApp(a *app.App) *Engine {
	d := Deps{
		Config: a.Config, Layout: a.Layout, Store: a.Store, Logger: a.Logger, DryRun: a.DryRun,
		Herdr: a.Herdr, Agents: a.Agents, Slots: a.Slots, Git: a.Git, Inventory: a.Inventory,
		Cleanup: a.Cleanup, Notifier: a.Notify, Identities: a.Identities, Usage: usage.Codex, Runner: a.Runner,
	}
	d.GitHub = func(id string) GitHub {
		if c := a.GitHub(id); c != nil {
			return c
		}
		return nil
	}
	d.Rounds = func(id string) Rounds {
		if r := a.Pipeline[id]; r != nil {
			return r
		}
		return nil
	}
	if a.Herdr != nil {
		d.Focus = a.Herdr.AgentFocus
	}
	if h := a.Herdr; h != nil && !a.DryRun && a.AgentTag == "" && a.Config != nil {
		// The retro's agent (retro_pane.go): an agents manager of its own,
		// over a scratch registry, tagged apart from the PRs' agents.
		pd := paneDeps{
			Config: a.Config, Layout: a.Layout, Logger: a.Logger, Herdr: h, Keys: h.AgentSendKeys, CloseWorkspace: h.WorkspaceClose,
			Agents: func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
				return agents.New(agents.Deps{Herdr: h, Store: st, Runner: a.Runner, Config: cfg, Layout: layout, Tag: LearnAgentTag,
					Log: app.Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "learn"}})
			},
		}
		d.Classifier = paneClassifiers(pd)
		// The notes curator (notes_curate.go) is the same kind of agent.
		d.Curator = paneCurators(pd)
	}
	if a.Git != nil {
		d.Probe = gitProbe(a.Git)
	}
	if a.Runner != nil && a.Config != nil {
		terminal, herdrBin := a.Config.Terminal, os.Getenv("HERDR_BIN_PATH")
		d.Reveal = func(ctx context.Context) error {
			_, err := reveal.Reveal(ctx, a.Runner, terminal, herdrBin, reveal.Options{})
			return err
		}
	}
	return New(d)
}

// Planned returns what a dry run would have done (empty otherwise).
func (e *Engine) Planned() []PlannedOp { return e.rec.Ops() }

// Kick asks the running loop for a tick now (what SIGUSR1 does).
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// ErrSchemaChanged is what Tick and Run return once another magnum binary
// migrated the registry under the running daemon (store.SchemaChanged): this
// binary's queries target the old schema, so the daemon exits non-zero and
// launchd starts it again from the new binary.
var ErrSchemaChanged = errors.New("schema migrated under the daemon; exiting for launchd to restart")

// ErrOpsLockHeld is what Run returns when the daemon lock is held by a magnum
// command running slot work in-process (it holds layout.OpsLock() too): the
// daemon exits non-zero so launchd starts it again once that work is done.
var ErrOpsLockHeld = errors.New("a magnum command holds state/ops.lock for in-process slot work; exiting so launchd starts the daemon again")

// Run is the daemon: lock, pidfile, startup, then a tick every
// daemon.poll_interval (or on Kick/SIGUSR1) until ctx ends or SIGTERM. It
// returns nil when another daemon holds the lock ("already running"), so
// launchd does not restart it in a loop, but ErrOpsLockHeld when a magnum
// command holds it for in-process slot work, and ErrSchemaChanged when the
// registry was migrated under it, so launchd starts it again. A dry run
// takes neither the lock nor the pidfile (it changes nothing a real daemon
// could see).
func (e *Engine) Run(ctx context.Context, opts Options) error {
	if !e.d.DryRun {
		unlock, held, err := AcquireLock(e.d.Layout.Lock())
		if err != nil {
			return fmt.Errorf("engine: lock: %w", err)
		}
		if held {
			return e.lockHeld()
		}
		defer unlock()
		if err := writePid(e.d.Layout.Pid()); err != nil {
			return fmt.Errorf("engine: pidfile: %w", err)
		}
		defer os.Remove(e.d.Layout.Pid())
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !opts.NoSignals {
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
		defer signal.Stop(sigs)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case s := <-sigs:
					if s == syscall.SIGUSR1 {
						e.Kick()
						continue
					}
					e.log.Info("stopping", "signal", s.String())
					cancel()
					return
				}
			}
		}()
	}
	defer e.shutdown()

	e.once = opts.Once
	e.build, e.checkBuild, e.supervised = opts.Build, opts.CheckBuild, opts.Supervised
	e.log.Info("magnum daemon starting", "pid", os.Getpid(), "dry_run", e.d.DryRun, "once", opts.Once)
	if err := e.loadPrompts(ctx); err != nil {
		e.log.Error("magnum daemon refusing to start", "err", err)
		return err
	}
	e.startup(ctx)
	if opts.Once {
		idle := make(chan struct{})
		worker := make(chan struct{})
		go func() {
			defer close(worker)
			e.heavyWorkerUntil(ctx, idle)
		}()
		if err := e.Tick(ctx); errors.Is(err, ErrSchemaChanged) {
			cancel()
			close(idle)
			<-worker
			return err
		} else if err != nil {
			e.warnTick(err)
		}
		e.observeUntilIdle(ctx)
		close(idle)
		<-worker
		return nil
	}

	worker := make(chan struct{})
	go func() {
		defer close(worker)
		e.heavyWorker(ctx)
	}()
	defer func() { cancel(); <-worker }()

	interval := e.cfg.Daemon.PollInterval.Duration // Config.Validate keeps it positive
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		case <-e.kick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if err := e.Tick(ctx); errors.Is(err, ErrSchemaChanged) || errors.Is(err, ErrRestartForBuild) {
			return err
		} else if err != nil && ctx.Err() == nil {
			e.warnTick(err)
		}
		timer.Reset(interval)
	}
}

// lockHeld decides Run's answer when the daemon lock is taken: by another
// daemon (nil, "already running"), or by a magnum command running slot work
// in-process, which holds ops.lock while it holds the daemon lock
// (ErrOpsLockHeld). Probing ops.lock takes it for an instant.
func (e *Engine) lockHeld() error {
	ops := e.d.Layout.OpsLock()
	unlock, held, err := AcquireLock(ops)
	switch {
	case err != nil:
		e.log.Warn("magnum daemon already running (ops.lock unreadable); exiting", "lock", e.d.Layout.Lock(), "ops_lock", ops, "err", err)
		return nil
	case held:
		e.log.Info(ErrOpsLockHeld.Error(), "ops_lock", ops)
		return ErrOpsLockHeld
	}
	unlock()
	e.log.Info("magnum daemon already running; exiting", "lock", e.d.Layout.Lock())
	return nil
}

// shutdown stops rounds (their runs stay observed, never re-sent), a
// running retro and a running notes curation, waits for them briefly,
// flushes pending toasts and gives urgent toasts still retrying a bounded
// time before cancelling them.
func (e *Engine) shutdown() {
	e.mu.Lock()
	for _, h := range e.rounds {
		h.cancel()
	}
	e.mu.Unlock()
	e.stopRetro()
	e.stopCurate()
	done := make(chan struct{})
	go func() { e.roundWG.Wait(); e.retroWG.Wait(); e.curateWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		e.log.Warn("rounds, the retro or the notes curation did not stop within 30s")
	}
	if e.batch != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := e.batch.Flush(ctx); err != nil {
			e.log.Warn("flush toasts", "err", err)
		}
	}
	e.stopToasts()
	e.log.Info("magnum daemon stopped")
}

// observeUntilIdle keeps observing herdr every poll interval while rounds
// launched by a --once tick are running (polling is their completion
// signal); the heavy worker runs alongside.
func (e *Engine) observeUntilIdle(ctx context.Context) {
	interval := e.cfg.Daemon.PollInterval.Duration // Config.Validate keeps it positive
	for e.activeRounds() > 0 {
		if err := e.d.Sleep(ctx, interval); err != nil {
			return
		}
		e.observe(ctx)
	}
	e.roundWG.Wait()
}

// Tick runs one iteration of the daemon loop. Steps log their own failures;
// the returned error joins them for the caller's information. A registry
// migrated by another binary stops the tick before any step
// (ErrSchemaChanged), and so does a new build the daemon restarts on
// (ErrRestartForBuild, restart_on_new_build).
func (e *Engine) Tick(ctx context.Context) error {
	if changed, err := e.st.SchemaChanged(ctx); err != nil {
		e.log.Warn("read schema version", "err", err)
	} else if changed {
		e.log.Error(ErrSchemaChanged.Error())
		return ErrSchemaChanged
	}
	if err := e.restartForBuild(ctx); err != nil {
		return err
	}
	now := e.now()
	var errs []error
	e.compares.reset() // a comparison serves the tick that made it
	e.setKV(ctx, kvLastTick, store.FormatTime(now))
	e.heartbeat()
	// Requests come first: the GitHub poll takes seconds (about 12 with
	// several watches), and a CLI or screen waiting for an answer gave up
	// before it came. The requests that arrive during the poll are handled
	// between its GitHub calls (requestsMidPoll, poll.go).
	e.handleRequests(ctx)
	e.refreshIdentities(ctx)
	e.retryIdentities(ctx)
	if err := e.poll(ctx); err != nil {
		errs = append(errs, err)
	}
	ts := e.observe(ctx)
	e.health(ctx)
	e.handleRequests(ctx)
	e.closeGrace(ctx)
	e.dispatch(ctx, ts)
	e.noteWaits(ctx, ts)
	e.parkIdle(ctx, ts)
	e.maybeReconcile(ctx)
	e.maybeRetro(ctx)
	e.maybeCurate(ctx)
	e.autoApprove(ctx) // before noteNeedsMe: a PR approved as the operator no longer needs them
	e.noteNeedsMe(ctx)
	e.surface(ctx)
	return errors.Join(errs...)
}

// startup runs once before the first tick: identity warm-up, crash recovery,
// a re-classification of ineligible PRs under the config just loaded and an
// inventory reconcile (queued on the heavy worker).
func (e *Engine) startup(ctx context.Context) {
	// The previous daemon's last tick, before this one writes its own:
	// requests queued after it were never seen by a daemon (expireRequests).
	lastTick, _ := e.kvTime(ctx, kvLastTick)
	e.setKV(ctx, kvStartedAt, store.FormatTime(e.now()))
	if !e.d.DryRun {
		e.setKV(ctx, kvPid, strconv.Itoa(os.Getpid()))
	}
	e.recordBuild(ctx)
	e.expireRequests(ctx, lastTick)
	// A drain (daemon-restart --drain) ends with the restart: this daemon
	// is the one that was waited for.
	if v, ok := e.getKV(ctx, KVDaemonDraining); ok && v != "" {
		e.delKV(ctx, KVDaemonDraining)
		e.event(ctx, "info", "", "daemon.drain_ended", "drain ended: the daemon restarted", nil)
	}
	e.noteRequestsSince(ctx)
	e.noteRepliesSince(ctx)
	e.warmIdentities(ctx, true)
	e.recoverRows(ctx)
	e.reclassifyIneligible(ctx)
	e.syncNotes(ctx, true) // notes_record.go: the first start imports every repository's notes
	// No curation runs yet: a mark left behind is a crashed daemon's.
	e.delKV(ctx, KVNotesCurating)
	e.lastReconcile = e.now()
	e.enqueueHeavy("reconcile", e.reconcile)
}

func (e *Engine) now() time.Time { return e.d.Now() }

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
