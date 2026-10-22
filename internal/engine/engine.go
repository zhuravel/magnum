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
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eval"
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
	// CIStates reads the head rollups (state and counts by state) of the
	// watched repositories' open PRs on every poll (readCI, poll.go); the
	// radar carries none.
	CIStates(ctx context.Context, prs []github.PRRadar) (map[string]github.CIState, github.RateLimit, error)
	Details(ctx context.Context, owner, repo string, numbers []int) (map[int]github.PRDetails, []int, error)
	// ReviewAnswers reads what the inline comments of reviews answer, by
	// node id: the reviews since magnum's latest review that Details left
	// unresolved (resolveAnswers, poll.go).
	ReviewAnswers(ctx context.Context, ids []string) (map[string]github.ReviewAnswer, error)
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
	// AllReviews lists every review of a PR and whether the list is
	// complete: what a former identity left standing (dismissFormer).
	AllReviews(ctx context.Context, owner, repo string, number int) ([]github.Review, bool, error)
	DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) error
	// CreateReview posts a manual verdict (verdict.go).
	CreateReview(ctx context.Context, owner, repo string, number int, commitID, event, body string) (github.RESTReview, error)
	// RequiredChecks reads the checks a branch requires (required.go).
	RequiredChecks(ctx context.Context, owner, repo, branch string) ([]string, bool, error)
	// CommitChecks reads a commit's checks: auto-approval's read of a
	// PR's base tip, whose failing checks do not hold the approval of a
	// head that fails them too (autoapprove_ci_base.go).
	CommitChecks(ctx context.Context, owner, repo, sha string) ([]github.Check, error)
	// CreateIssueComment and EditIssueComment post and edit a PR's review
	// status comment as its posting identity (status_comment.go).
	CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (github.IssueComment, error)
	EditIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) (github.IssueComment, error)
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
	// LoginStamp fingerprints the kind's login file ("" = none): a login
	// pause a run proved holds until it changes (health).
	LoginStamp(kind string) string
	Park(ctx context.Context, pr store.PR) error
	// ParkHard is Park for an agent that will not quit: one still its
	// pane's foreground process after ctrl+c and its kind's exit keys has
	// that pane closed alone and its session closed (a fresh start's second
	// try, parkFresh; a stop's last, parkForStop).
	ParkHard(ctx context.Context, pr store.PR) error
	// ParkSessions parks the PR's live sessions ids names and only them,
	// with Park's guards, each once its quit reached an idle shell and its
	// own pane was closed: the idle park's early park of the agents past
	// their threshold while another agent of the PR stays (parkEarly). It
	// never parks hard, and returns the sessions it parked beside the
	// failures of the others.
	ParkSessions(ctx context.Context, pr store.PR, ids []int64) ([]store.Session, error)
	// CloseRetiredPanes closes the idle panes of the PR's parked, closed
	// and lost sessions that no starting or live session of the PR
	// records, after every park that worked and a session marked lost
	// (closeRetiredPanes, closePanesJob).
	CloseRetiredPanes(ctx context.Context, pr store.PR) error
	// CloseRetiredPanesIn is CloseRetiredPanes over a snapshot the
	// reconcile took once for every PR with a pane (closeLeftoverPanes).
	CloseRetiredPanesIn(ctx context.Context, pr store.PR, snap herdr.Snapshot) error
	// Quit stops one session's agent and parks its conversation (a cold
	// judge, coldJudge; a session that served its max_session_rounds,
	// rotateSessions; a session parkReloading takes out of the checkout's
	// way).
	Quit(ctx context.Context, s store.Session) error
	// ReloadsProject reports whether a session's agent reloads the
	// checkout's project config it loaded while it runs (parkReloading).
	ReloadsProject(ctx context.Context, s store.Session) bool
	// ClearDeadLaunch closes the pane of a launch that never came up which
	// herdr lists under the agent name of session s over an idle shell,
	// replacing s's pane when it is that one, and reports whether the name
	// is gone and the role keeps a pane (quitAgentGone).
	ClearDeadLaunch(ctx context.Context, s store.Session) (bool, error)
	Recover(ctx context.Context, pr store.PR) ([]agents.Recovered, error)
	// NewRun, FixPrompt and Submit start the fix job's run (fix.go); Tell
	// gives a fixer whose time ran out its last call within the run.
	NewRun(ctx context.Context, pr store.PR, role agents.Role, kind string, round int) (store.Run, error)
	FixPrompt(pr store.PR, role agents.Role, run store.Run, in agents.FixPromptInput) (string, error)
	// WhiteboardPrompt renders the whiteboard job's prompt (whiteboard.go),
	// whose run NewRun and Submit start as the fix job's.
	WhiteboardPrompt(pr store.PR, role agents.Role, run store.Run, in agents.WhiteboardPromptInput) (string, error)
	Submit(ctx context.Context, run store.Run, text string) error
	Tell(ctx context.Context, run store.Run, text string) error
	// ContextUsed reads how full a session's context is from its pane's
	// status lines (ok false: no figure), and Compact has its agent compact
	// its conversation and waits for that turn to end: the fix job's
	// compaction before a cycle that resumes the fixer (fix_compact.go).
	ContextUsed(ctx context.Context, s store.Session) (int, bool)
	Compact(ctx context.Context, s store.Session) error
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
	// Resync puts the slot back in order after the fix job's fixer committed
	// in it: detached at sha (the pushed head, or the old one), recorded as
	// checked out.
	Resync(ctx context.Context, slot store.Slot, sha string) error
	// Reserve, Repair and Adopt serve the open, repair and adopt requests.
	Reserve(ctx context.Context, pr store.PR, pool config.Pool) (store.Slot, error)
	Repair(ctx context.Context, slot store.Slot, pool config.Pool) error
	Adopt(ctx context.Context, pool config.Pool, path string) (store.Slot, error)
	// CheckSchema, ForgetSchema and RecordSchema decide and record a round's
	// schema reload (readinessPlan), ResetFailed counts a reload that failed
	// (RecordSchema clears the count); EnsureSchema reloads for magnum open.
	CheckSchema(ctx context.Context, slot store.Slot, pool config.Pool) (slots.SchemaCheck, error)
	ForgetSchema(ctx context.Context, slot store.Slot) error
	RecordSchema(ctx context.Context, slot store.Slot, c slots.SchemaCheck) error
	ResetFailed(ctx context.Context, slot store.Slot, cause string) (int, error)
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
	// DiffStat measures the PR's size at a full pass's baseline, in the
	// round's checkout (prSize, fullpass.go).
	DiffStat(ctx context.Context, dir, base, head string) ([]gitx.FileStat, error)
	// ChangedBetween names the files that differ between two heads: the
	// PR's evidence map marks what they held stale (evidence.go).
	ChangedBetween(ctx context.Context, dir, from, to string) ([]string, error)
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
	// LookPath finds an agent kind's CLI on the daemon's PATH (exec.LookPath,
	// which FromApp sets for the daemon), whose version Runner reads at the
	// start and at each health check (cli_version.go); nil = no version is
	// read.
	LookPath func(file string) (string, error)
	// MemoryRunner runs the memory probe's two sysctl commands at the health
	// checks (memory.go); FromApp sets the App's runner, which Runner is too.
	// A port of its own keeps the probe out of the commands a test scripts
	// for Runner. nil = no memory sample.
	MemoryRunner execx.Runner
	// Classifier sets up the retro's classifier for one retro run ([learn],
	// retro.go); nil = the candidates are stored unclassified.
	Classifier func(ctx context.Context, run RetroRun) (Classifier, error)
	// Curator sets up the notes curator for one curation ([notes],
	// notes_curate.go); nil = no curation runs.
	Curator func(ctx context.Context, run CurateRun) (Curator, error)
	// Critic sets up the notes critic for one critique ([notes] critic,
	// notes_critic.go); nil = no critique runs.
	Critic func(ctx context.Context, run CriticRun) (Critic, error)
	// Editor sets up the skill editor for one run ([learn] editor_*,
	// learn_edit.go); nil = no edit runs. It works on Checkout, magnum's own
	// checkout (Layout.Home; "" for an installed binary: no edit), through
	// LearnGit, and GoTest runs the format gate's commands there: go or
	// make (args[0]) on dir with the checkout's mise environment.
	Editor   func(ctx context.Context, run EditRun) (Editor, error)
	Checkout string
	LearnGit LearnGit
	GoTest   func(ctx context.Context, dir string, args ...string) (string, error)
	// The gate of a skill proposal (learn_gate.go) replays eval cases
	// in-process; the daemon's wiring sets these to internal/evalrun's (the
	// engine cannot import it: evalrun runs the engine). EvalCase replays
	// one case (evalrun.RunCase), EvalInputs hashes what the review reads
	// with candidate's prompts and skill (evalrun.Inputs and ReviewerInputs
	// of evalrun.WithCandidate; "" = the config itself), EvalRuns lists the
	// stored runs, oldest first (eval.ListRuns of Layout.Eval), and
	// EvalCorpus reads the corpora `magnum eval run` reads by default, with
	// the operator's file ("" = none). Any of them nil: no gate runs and a
	// proposal stays gating.
	EvalCase   func(ctx context.Context, c eval.Case, o EvalGateOptions) eval.CaseRun
	EvalInputs func(candidate string) (inputs, reviewerInputs string, err error)
	EvalRuns   func() ([]eval.Run, error)
	EvalCorpus func() (corpus *eval.Corpus, path string, err error)

	// Usage reads Codex's rate-limit snapshot (usage.Codex, which FromApp
	// sets); nil = no budget gauge and no caps.
	Usage func(ctx context.Context, codexHome string, now time.Time) (usage.Snapshot, error)
	// UsageAt reads Codex's rate-limit snapshot at or before a past time
	// (usage.CodexAt, which FromApp sets); nil = no last-day pace next to
	// the window's average.
	UsageAt func(ctx context.Context, codexHome string, at time.Time) (usage.Snapshot, error)
	// CodexLogin reads the login of Codex's auth.json under a CODEX_HOME
	// (usage.CodexLogin, which FromApp sets: a fingerprint of its account,
	// never a token); nil = no restart of the Codex sessions when it
	// changes (codex_login.go).
	CodexLogin func(codexHome string) (usage.Login, error)
	// CodexHome is the Codex home a session of a role reads
	// (agents.Manager.CodexHome, which FromApp sets); nil = herdr's Codex
	// session hook is not watched (codex_session_hook.go).
	CodexHome func(role config.Role) string
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

	// Webhooks reads magnum-relay's events in [webhooks] shadow mode
	// (webhooks.go, which FromApp sets); nil = no consumer.
	Webhooks WebhookSource
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
	// ShellPATH is what the daemon command took from the login shell's PATH
	// before the engine was built ([daemon] shell_path), recorded at startup
	// (KVDaemonShellPATH, event daemon.shell_path); nil with the key off.
	ShellPATH *ShellPATH
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
	// workEndedAt is when each PR's last round, fix job or whiteboard job
	// ended in this daemon (workEnded, kvPRWorkEnded's copy): a park planned
	// before it never takes the PR (reserveParkWork, park.go).
	workEndedAt map[int64]time.Time
	// autofixNoRole are the PRs autofix found no fix role for, logged once
	// each (fix_loop.go).
	autofixNoRole map[int64]bool
	// drainHeld are the PRs whose fix loop a drain held before its next
	// cycle, or whose `magnum fix` it held (holdFix), guarded by mu: a drain
	// that ends without a restart continues them (continueAfterDrain), as
	// the restart's recovery would.
	drainHeld map[int64]bool
	// budgetHeld are the PRs whose fix loop's next cycle, `magnum fix` or
	// autofix a GitHub budget below the floor held (githubBudgetError),
	// guarded by mu: the tick that finds the budget recovered goes on with
	// them (continueBudgetHeld, github_floor.go). budgetWhiteboards are the
	// identities whose low budget left a whiteboard pending (guarded by mu).
	budgetHeld        map[int64]bool
	budgetWhiteboards map[string]bool
	// pauseHeld are the PRs whose held fix waits for its fixer's paused
	// agent kind (heldPause), guarded by mu: the tick that finds the kind no
	// longer paused starts it (continuePauseHeld, fix_held.go).
	pauseHeld map[int64]bool
	// yieldCarry is, by request id, what a request read before a whiteboard
	// job yielded the PR to it (whiteboardHeld.carry), handed to the request
	// when it is answered after the yield (answerYielding), guarded by mu.
	yieldCarry map[int64]any
	// drainWhiteboards is set (guarded by mu) when a drain skipped a
	// whiteboard after a posted review and remembered it
	// (kvPRWhiteboardPending): a drain that ends without a restart makes the
	// pending whiteboards (continueAfterDrain), as the restart's start would.
	drainWhiteboards bool
	// pendingWhiteboardsMu keeps two passes over the pending whiteboards
	// (startPendingWhiteboards) from taking the same entry.
	pendingWhiteboardsMu sync.Mutex
	// inflight are the requests answered asynchronously, which handleRequests
	// skips until they complete: those handed to the heavy worker
	// (heavyRequest), an open's (open.go) and an abort's or an ignore's
	// (abort.go).
	inflight map[int64]bool
	roundWG  sync.WaitGroup
	// roundTails counts the round goroutines that have not ended: from
	// before a round drops its reservation until its post-round fix cycle
	// and whiteboard have reserved the PR (or found nothing to do), so an
	// idle restart never falls into the gap between the two (restartBusy).
	roundTails atomic.Int32

	heavy     chan heavyJob
	heavyMu   sync.Mutex
	heavyKeys map[string]bool

	kick          chan struct{}
	batch         *notify.Batcher
	lastReconcile time.Time
	midPoll       bool // requestsMidPoll is answering requests (the tick's goroutine only)
	herdrUp       *bool
	starts        starter
	dryRounds     int // rounds a dry run planned this tick
	lastSeen      map[string]string
	cleanupTried  map[int64]time.Time // close-grace cleanup attempts per PR
	// logged: when each repeating error was last logged (logOnce, util.go).
	logged map[string]time.Time
	// compares are this tick's GitHub comparisons (compare.go).
	compares compareMemo
	// ghMeter is the GitHub cost meter: what every identity's calls cost,
	// and each identity's rate limit (its lowest live reading), which the
	// floor reads (github_cost.go).
	ghMeter githubMeter

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
	// pinWaits are the PRs whose round waits on their pinned slot, since
	// when and whether that was toasted (notePinWaits, operator.go).
	pinWaits map[int64]pinWaitSeen
	// needsMe are the toast keys of the PRs that need the operator offered
	// to the batcher in this run (needs_me.go).
	needsMe map[string]bool
	// pollToasted is the toast key of each watch owner's (lower case)
	// failure streak offered to the batcher in this run (poll_health.go).
	pollToasted map[string]string
	// tickEnded is when the last tick ended, by the wall clock, and
	// afterSleep says that the wait since lasted long enough for a sleep of
	// the Mac (poll_health.go). Tick goroutine only.
	tickEnded  time.Time
	afterSleep bool
	// machineLast is the newest round.environment event noteMachine read,
	// and machineToasted the groups it offered to the batcher in this run
	// and when (environment.go). Tick goroutine only.
	machineLast    int64
	machineToasted map[string]time.Time
	// cliRead is, by agent kind, the binary whose version this run read or
	// found recorded (cli_version.go): the version command runs again only
	// once the binary changed; cliFails, by agent kind, the reads that
	// failed and left no stamp, tried again at every health check. Tick
	// goroutine only (startup and health).
	cliRead  map[string]cliStamp
	cliFails map[string]*cliReadFailures
	// mem is the machine's memory as the health checks sampled it
	// (memory.go), tick goroutine only; memWG waits for the sample in
	// flight, which runs on a goroutine of its own.
	mem   memoryWatch
	memWG sync.WaitGroup
	// autoSeen and autoFollow remember, by PR and by automatic approval,
	// what auto-approval last read GitHub's reviews for (autoapprove.go): a
	// PR whose latest round and updatedAt did not move since is not asked
	// again; autoRefused, by PR, the refusal it last recorded for the card
	// (KVPRAutoApproveRefused; "" = none). autoMu guards them and
	// serializes auto-approval, which the tick and a round's goroutine at
	// its end (autoApproveRound) both run. baseChecks are the base tips'
	// checks the gates read, by repository and commit, for baseChecksTTL
	// (autoapprove_ci_base.go), also under autoMu. autoRetry is, by PR,
	// when a read GitHub refused for its rate limit may be made again,
	// autoHeld, by identity, the end of the budget hold last logged, and
	// autoPollPaused the poll's rate-limit pause the pass found
	// (autoapprove_budget.go), under autoMu too.
	autoMu                            sync.Mutex
	autoSeen, autoFollow, autoRefused map[int64]string
	baseChecks                        map[baseTip]baseTipRead
	autoRetry                         map[int64]time.Time
	autoHeld                          map[string]time.Time
	autoPollPaused                    time.Time

	infraMu  sync.Mutex // infrastructure failures (infra.go)
	depsFail depsFailure

	// unwritten are the Codex flags a refused round could not write, by PR:
	// they count as written and are written again every tick until the
	// write takes (codex_flag.go); releaseFailed, when the release of a
	// flagged PR's slot last failed (park.go). flagMu guards them: a
	// round's goroutine and the heavy worker add to them, the tick reads.
	flagMu        sync.Mutex
	unwritten     map[int64]CodexFlag
	releaseFailed map[int64]time.Time

	// parkFails are the PRs whose last park failed, with the backoff of
	// their next try (park.go). parkMu guards them: the heavy worker writes,
	// the tick reads.
	parkMu    sync.Mutex
	parkFails map[int64]parkBackoff

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
	// restartRefused is the user config on disk and binary an idle restart
	// was last refused on (restartSafe's key; zero for none) and when, so
	// later ticks check them again only after restartRefuseRetry and never
	// say so again (config_restart.go).
	restartRefused restartRefusal
	// shellPATH is Options.ShellPATH (shell_path.go).
	shellPATH *ShellPATH
	// run is what this daemon runs (its build, config and prompts), taken
	// once, at its first use: the start (retry_parked.go).
	runOnce sync.Once
	run     runFingerprint

	once         bool       // Run with Options.Once: no daily retro (retro.go)
	retroMu      sync.Mutex // the running retro (retro.go)
	retroCancel  context.CancelFunc
	retroStarted time.Time
	retroRetry   time.Time // the daily retro GitHub stopped starts again then
	retroWG      sync.WaitGroup

	curateMu      sync.Mutex // the running notes curation (notes_curate.go)
	curateCancel  context.CancelFunc
	curateStarted time.Time
	curateRepo    string
	curateWG      sync.WaitGroup
	curateTried   map[int64]time.Time // repositories whose last curation stored nothing, and when
	curateChecked time.Time           // the last scan for a curation due (the tick's goroutine only)

	criticMu       sync.Mutex // the running notes critique (notes_critic.go)
	criticCancel   context.CancelFunc
	criticStarted  time.Time
	criticRepo     string
	criticProposal int64
	criticWG       sync.WaitGroup
	criticTried    map[int64]time.Time // proposals whose last critique stopped short, and when

	editMu       sync.Mutex // the running skill edit (learn_edit.go)
	editCancel   context.CancelFunc
	editStarted  time.Time
	editWhat     string // what runs: the skill edit, or the revert of a version (learnRunning)
	editWG       sync.WaitGroup
	gateMu       sync.Mutex // the running gate of a skill proposal (learn_gate.go)
	gateCancel   context.CancelFunc
	gateStarted  time.Time
	gateProposal int64         // the proposal the running gate decides
	gateDone     chan struct{} // closed when the running gate ended (stopGateOf)
	gateRetry    time.Time     // a gate that stopped short starts again after it
	gateWG       sync.WaitGroup
	// learnMu serializes the starts of the learn jobs (the retro, a notes
	// curation, a skill edit, a gate; learn_schedule.go): a start holds it
	// while it checks that no other runs and marks its own running. It is
	// taken before a job's own mutex, never after.
	learnMu      sync.Mutex
	learnChecked time.Time  // the loop's last scan (maybeLearnScan; the tick's goroutine only)
	landRetry    time.Time  // a landing that failed short of a hold tries again after it (the tick's goroutine only)
	corpusMu     sync.Mutex // one rewrite of the learned corpus at a time (learn_corpus.go)

	usageMu   sync.Mutex // the Codex budget (budget.go)
	usageRead time.Time
	usageSnap *usage.Snapshot
	// usageRecorded is what recordBudget last wrote to kv.
	usageRecorded string
	// usageOverruled is when the operator lifted a budget-cap pause
	// (overruleBudget): the caps wait for a snapshot stamped after it.
	usageOverruled time.Time
	usagePastRead  time.Time
	usagePast      *usage.Snapshot // Codex's reading from recentSpan ago (readPast)
	// usagePaceRecorded is what recordRecentPace last wrote to kv ("none":
	// deleted).
	usagePaceRecorded string

	// codexLoginRead is when the tick last read Codex's login
	// (codex_login.go; the tick's goroutine only).
	codexLoginRead time.Time
	// codexHookRead is when the tick last checked the Codex sessions' ids
	// and herdr's Codex session hook, and codexHooksSeen the modification
	// time of each Codex home's hooks.json it saw then (zero: none), by home
	// (codex_session_hook.go; the tick's goroutine only).
	codexHookRead  time.Time
	codexHooksSeen map[string]time.Time
	// restarts are the sessions marked to park once idle, so that their
	// next prompt resumes them in new processes, by session id, loaded from
	// kv on first use (session_restart.go). restartMu guards them: the
	// tick, a round's settle and the heavy worker change them.
	restartMu sync.Mutex
	restarts  map[int64]restartMark

	// hooks is the webhooks consumer's memory in shadow mode (webhooks.go).
	hooks webhookState
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
		rounds:            map[int64]*roundHandle{},
		evicting:          map[int64]string{},
		workEndedAt:       map[int64]time.Time{},
		autofixNoRole:     map[int64]bool{},
		drainHeld:         map[int64]bool{},
		budgetHeld:        map[int64]bool{},
		budgetWhiteboards: map[string]bool{},
		pauseHeld:         map[int64]bool{},
		yieldCarry:        map[int64]any{},
		heavy:             make(chan heavyJob, 64),
		heavyKeys:         map[string]bool{},
		inflight:          map[int64]bool{},
		lastSeen:          map[string]string{},
		logged:            map[string]time.Time{},
		cleanupTried:      map[int64]time.Time{},
		curateTried:       map[int64]time.Time{},
		criticTried:       map[int64]time.Time{},
		netRuns:           map[string]netRun{},
		cliRead:           map[string]cliStamp{},
		cliFails:          map[string]*cliReadFailures{},
		kick:              make(chan struct{}, 1),
		starts:            starter{codex: make(chan struct{}, maxCodexStarts)},
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

// FromApp wires an engine to the App's real components; mods then change
// its Deps (the daemon's wiring sets the gate's eval seams there).
func FromApp(a *app.App, mods ...func(*Deps)) *Engine {
	d := Deps{
		Config: a.Config, Layout: a.Layout, Store: a.Store, Logger: a.Logger, DryRun: a.DryRun,
		Herdr: a.Herdr, Agents: a.Agents, Slots: a.Slots, Git: a.Git, Inventory: a.Inventory,
		Cleanup: a.Cleanup, Notifier: a.Notify, Identities: a.Identities, Usage: usage.Codex, UsageAt: usage.CodexAt, Runner: a.Runner,
		MemoryRunner: a.Runner, CodexLogin: usage.CodexLogin,
	}
	if a.Agents != nil {
		d.CodexHome = a.Agents.CodexHome
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
		// over a scratch registry, tagged apart from the PRs' agents; the
		// live registry shares the per-model limits with the rounds.
		pd := paneDeps{
			Config: a.Config, Layout: a.Layout, Logger: a.Logger, Herdr: h, Keys: h.AgentSendKeys, CloseWorkspace: h.WorkspaceClose,
			Store: a.Store,
			Agents: func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
				return agents.New(agents.Deps{Herdr: h, Store: st, Runner: a.Runner, Config: cfg, Layout: layout, Tag: LearnAgentTag,
					Log: app.Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "learn"}})
			},
		}
		d.Classifier = paneClassifiers(pd)
		// The notes curator (notes_curate.go) and the skill editor
		// (learn_edit.go) are the same kind of agent.
		d.Curator = paneCurators(pd)
		d.Critic = paneCritics(pd)
		d.Editor = paneEditors(pd)
	}
	if a.Layout.Home != "" && !a.DryRun && a.AgentTag == "" && a.Git != nil && a.Runner != nil && a.Config != nil {
		// The skill editor works on the checkout the daemon runs from.
		d.Checkout, d.LearnGit = a.Layout.Home, a.Git
		var keys []string
		for _, id := range a.Config.Identities {
			if id.PrivateKeyEnv != "" {
				keys = append(keys, id.PrivateKeyEnv)
			}
		}
		d.GoTest = goTestRunner(a.Runner, a.Mise, a.Layout.Home, a.Layout.DB(), keys)
	}
	if a.Git != nil {
		d.Probe = gitProbe(a.Git)
	}
	d.Webhooks = webhookSource(a)
	if !a.DryRun && a.AgentTag == "" {
		d.LookPath = exec.LookPath // the daemon's own agent CLIs (cli_version.go)
	}
	if a.Runner != nil && a.Config != nil {
		terminal, herdrBin := a.Config.Terminal, os.Getenv("HERDR_BIN_PATH")
		d.Reveal = func(ctx context.Context) error {
			_, err := reveal.Reveal(ctx, a.Runner, terminal, herdrBin, reveal.Options{})
			return err
		}
	}
	for _, m := range mods {
		m(&d)
	}
	e := New(d)
	if a.Config != nil && a.GitHub != nil {
		for _, id := range a.Config.Identities {
			if c := a.GitHub(id.Name); c != nil {
				c.Meter = e.meterGitHub(id.Name) // github_cost.go: before the clients' first call
			}
		}
	}
	for _, r := range a.Pipeline {
		if r != nil {
			r.OnPause = e.PauseTool // a run that proves a kind logged out pauses it before the round ends
		}
	}
	return e
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
		// A signal handler that panicked would leave SIGTERM caught and
		// unanswered: the daemon stops instead.
		go e.safely(ctx, "signal handler", "", func() {
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
		}, func(context.Context, string) { cancel() })
	}
	defer e.shutdown()

	e.once = opts.Once
	e.build, e.checkBuild, e.supervised = opts.Build, opts.CheckBuild, opts.Supervised
	e.shellPATH = opts.ShellPATH
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
	hooks := e.goWebhooks(ctx) // webhooks.go: shadow mode's consumer
	defer func() { cancel(); <-worker; <-hooks }()

	interval := e.cfg.Daemon.PollInterval.Duration // Config.Validate keeps it positive
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		case <-e.kick:
			timer.Stop()
		}
		if err := e.Tick(ctx); errors.Is(err, ErrSchemaChanged) || errors.Is(err, ErrRestartForBuild) || errors.Is(err, ErrRestartForLearn) ||
			errors.Is(err, ErrRestartForConfig) {
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
// running retro, notes curation, skill edit and gate, waits for them and a
// memory sample in flight (which Run's context ends, or its timeouts)
// briefly, flushes pending toasts and gives urgent toasts still retrying a
// bounded time before cancelling them.
func (e *Engine) shutdown() {
	e.mu.Lock()
	for _, h := range e.rounds {
		h.cancel()
	}
	e.mu.Unlock()
	e.stopRetro()
	e.stopCurate()
	e.stopCritic()
	e.stopEdit()
	e.stopGate()
	done := make(chan struct{})
	go func() {
		e.roundWG.Wait()
		e.retroWG.Wait()
		e.curateWG.Wait()
		e.criticWG.Wait()
		e.editWG.Wait()
		e.gateWG.Wait()
		e.memWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		e.log.Warn("rounds, the retro, the notes curation, the skill edit, the skill gate or the memory sample did not stop within 30s")
	}
	e.noteRetroRunning(context.Background()) // a retro that stopped no longer holds the CLI
	e.flushGitHubCost(context.Background(), true)
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
// (ErrSchemaChanged), and so does an idle restart (restartForBuild): on a
// new build (ErrRestartForBuild, restart_on_new_build), a landed skill
// version (ErrRestartForLearn) or a saved config (ErrRestartForConfig).
func (e *Engine) Tick(ctx context.Context) error {
	if changed, err := e.st.SchemaChanged(ctx); err != nil {
		e.warnUnlessStopped(ctx, err, "read schema version")
	} else if changed {
		e.log.Error(ErrSchemaChanged.Error())
		return ErrSchemaChanged
	}
	if err := e.restartForBuild(ctx); err != nil {
		return err
	}
	now := e.now()
	e.afterSleep = e.sleptBefore(now)
	defer func() { e.tickEnded = e.now().Round(0) }()
	var errs []error
	e.compares.reset() // a comparison serves the tick that made it
	e.setKV(ctx, store.KVDaemonLastTick, store.FormatTime(now))
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
	// github_floor.go: the work the GitHub budget held, once it recovered,
	// before dispatch gives the PR to another round.
	e.continueBudgetHeld(ctx)
	e.continuePauseHeld(ctx)   // fix_held.go: the held fixes the fixer's pause held, once it ended
	e.endOpenHolds(ctx)        // open_hold.go: a PR whose hold ended goes on in this dispatch
	e.restartSessions(ctx, ts) // session_restart.go: a PR whose marked sessions park now waits in dispatch
	e.dispatch(ctx, ts)
	e.noteWaits(ctx, ts)
	e.noteStatusComments(ctx) // status_comment.go: after the waits it names
	e.parkIdle(ctx, ts)
	e.flagTick(ctx, ts) // codex_flag.go
	e.maybeReconcile(ctx)
	e.maybeRetro(ctx)
	e.noteRetroRunning(ctx) // inflight.go
	e.maybeCurate(ctx)
	e.maybeCritic(ctx)    // notes_critic.go: after a curation could start, which goes first
	e.maybeLearnScan(ctx) // learn_schedule.go: the scheduled editor
	e.maybeGate(ctx)      // learn_gate.go
	e.maybeLearnLand(ctx) // learn_land.go
	e.autoApprove(ctx)    // before noteNeedsMe: a PR approved as the operator no longer needs them
	e.noteNeedsMe(ctx)
	e.noteMachine(ctx)
	e.noteGitHubBudget(ctx)  // github_cost.go: a run of low budget begins or ends
	e.noteGitHubOutside(ctx) // github_cost.go: a window's spend outside magnum reached a fifth
	e.flushGitHubCost(ctx, false)
	e.surface(ctx)
	return errors.Join(errs...)
}

// startup runs once before the first tick: identity warm-up, crash recovery,
// a retry of the PRs failed rounds parked under another build, config or
// prompts, a re-classification of ineligible PRs under the config just
// loaded and an inventory reconcile (queued on the heavy worker).
func (e *Engine) startup(ctx context.Context) {
	// The previous daemon's last tick, before this one writes its own:
	// requests queued after it were never seen by a daemon (expireRequests).
	lastTick, _ := e.kvTime(ctx, store.KVDaemonLastTick)
	e.setKV(ctx, store.KVDaemonStartedAt, store.FormatTime(e.now()))
	if !e.d.DryRun {
		e.setKV(ctx, store.KVDaemonPid, strconv.Itoa(os.Getpid()))
	}
	e.recordBuild(ctx)
	e.recordConfig(ctx)       // config_restart.go: the user config this start runs; a saved config's restart is done
	e.recordShellPATH(ctx)    // shell_path.go
	e.checkCLIVersions(ctx)   // cli_version.go: on the PATH just recorded
	e.loadBudgetOverrule(ctx) // budget.go: a resume the CLI recorded while no daemon ran
	e.loadGitHubCost(ctx)     // github_cost.go: the hours and rate limits an earlier daemon metered
	e.noteLearnRestarted(ctx) // learn_land.go: this start runs the landed skill version
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
	// open_hold.go: a lasting pin of an earlier open becomes a hold.
	e.convertOpenPins(ctx)
	e.recoverFixes(ctx)   // fix.go: a fixer at work never survives a restart
	e.startHeldFixes(ctx) // fix_held.go: the `magnum fix` requests a drain held for this start
	e.retryParked(ctx)    // retry_parked.go: what failed rounds parked under another build, config or prompts
	e.reclassifyIneligible(ctx)
	e.recoverWhiteboards(ctx)
	// whiteboard_pending.go: the whiteboards the drain this start ended skipped.
	e.startPendingWhiteboards(ctx, "after the restart")
	e.recoverNotesApplies(ctx) // notes_apply.go: an apply a crash cut short, before the notes are imported
	e.syncNotes(ctx, true)     // notes_record.go: the first start imports every repository's notes
	// No curation, retro, skill edit or gate runs yet: a mark left behind
	// is a crashed daemon's (a gating proposal keeps its state and its gate
	// starts again at a tick, maybeGate), and so is the gate's worktree of
	// the base (learn_gate.go).
	e.delKV(ctx, KVNotesCurating, KVNotesCritic, KVRetroRunning, KVLearnEditing, KVLearnGating)
	// notes_critic.go: a critique a stop cut short is retried, uncounted.
	if _, err := e.st.StopOpenCritiques(ctx, "the daemon stopped during the critique", e.now()); err != nil {
		e.warnUnlessStopped(ctx, err, "notes critic: end the critiques a stop cut short")
	}
	e.dropStaleGateBase(ctx)
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
