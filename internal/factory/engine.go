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
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	ossignal "os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/scrub"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/usage"
)

// LaunchLabel is the launchd label of the factory daemon's LaunchAgent,
// apart from the review daemon's (launchd.DefaultLabel), so either restarts
// without the other.
const LaunchLabel = "zhuravel.magnum.factory"

// Runtime is the factory's agent runtime (internal/factory/runtime): the
// owner row of a task in the factory's agents store, the launch of a
// stage's roles (recorded before any workspace exists), the prompts it
// sends through its ledger, the panes it observes and the watchdog over
// them, the verification of a run's end (or its setting aside, a turn that
// asked questions: Void), parking a task's sessions, the
// reconcile of a prompt of unknown delivery, the recovery of a lost pane
// or of a role a recovery left without a turn (RecoverLost, which a rebuilt
// stage calls),
// the shift of every turn deadline after the Mac slept, the role that runs
// for a role (its Claude fallback on a task Codex never works on), an agent
// kind's pause in force (Paused: Codex's usage limit yields its roles), the quit
// of such a task's Codex sessions, the quit of one role's sessions (a QA
// walker whose run was cut, QuitRole), a run's state as the agents store keeps
// it across a restart, who is at work in a task's copy now (a run in flight,
// you in a pane: the factory's own commands that change the copy wait for
// them), and who could have changed it since a time (the factory puts back
// what its checks changed only when nobody else could have). After a
// restart it binds a task's sessions to the panes herdr shows again
// (Recover) and settles the workspaces a crash left that no row names
// (SettleWorkspaces: kept for their attempt's launch to adopt, or closed).
// Proving says whether the proof that a prompt of unknown delivery never
// arrived is under way (its read after RedeliverAfter settles it).
// PaneFocus is what the observe passes saw of you in the factory's panes,
// from the snapshot they read anyway, and Workspaces the herdr workspaces
// of a task's live sessions: the alerts' toasts hold while you are in a
// factory pane, and a task's sidebar token goes on its workspaces
// (alerts.go). UseHookSocket has the panes it makes from then on name the
// factory's hook socket, Wake looks at the pane an agent's hook event names,
// and WaitWakes waits for the later looks a Wake started (hooks.go).
type Runtime interface {
	Owner(ctx context.Context, t registry.Task) (store.PR, error)
	Launch(ctx context.Context, t registry.Task, a registry.Attempt, c Copy, roles []config.Role) error
	Send(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, text string) (runID string, err error)
	SendFresh(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, then string, recoveriesOver bool) (runID string, err error)
	MadeProgress(t registry.Task, a registry.Attempt, role string)
	StartOver(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, why string) error
	Observe(ctx context.Context) ([]runtime.Event, error)
	Verify(ctx context.Context, runID string, ok bool, why string) error
	Void(ctx context.Context, runID, why string) error
	Park(ctx context.Context, t registry.Task) error
	Reconcile(ctx context.Context, e registry.LedgerEntry) (string, error)
	Watchdog(ctx context.Context, now time.Time) ([]runtime.Event, error)
	RecoverLost(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role) error
	LetGo(ctx context.Context, t registry.Task, role string) (int, error)
	Shift(gap time.Duration)
	BeginDrain()
	RunsAs(t registry.Task, role config.Role) (config.Role, error)
	Paused(kind string) *runtime.WaitError
	QuitCodex(ctx context.Context, t registry.Task) error
	QuitRole(ctx context.Context, t registry.Task, role, why string) error
	RunState(ctx context.Context, runID string) (state, why string, err error)
	AtWork(ctx context.Context, t registry.Task) (role string, you bool, err error)
	TouchedSince(ctx context.Context, t registry.Task, since time.Time) (who string, err error)
	Recover(ctx context.Context, t registry.Task) (changed []string, err error)
	SettleWorkspaces(ctx context.Context) ([]runtime.Orphan, error)
	Proving(entry int64) bool
	PaneFocus() runtime.PaneFocus
	Workspaces(ctx context.Context, t registry.Task) ([]string, error)
	Talk(ctx context.Context, t registry.Task, prompt string) (pane string, err error)
	UseHookSocket(path string)
	Wake(ctx context.Context, ev agenthooks.Event)
	WaitWakes()
	Ended() <-chan struct{}
	ObserveSoon() <-chan struct{}
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
// the factory's headless Chrome for a copy's QA run (its profile in dir/chrome,
// cdp its DevTools endpoint, http://127.0.0.1:<port>), the reconcile of
// the servers and the browsers after a restart, and the broken copies (the
// one whose backoff repair is due, its repair, and the repositories whose
// every copy broke the same way: repair.go). The fixes of your PRs claim
// checkouts of their own (fix_copies.go): a fix copy, set apart from the
// tasks', or a per-PR checkout without databases, at the PR's remote head,
// its release (a per-PR checkout's removal) and the checkouts the PRs'
// fix work holds.
type Copies interface {
	Claim(ctx context.Context, t registry.Task) (Copy, error)
	Release(ctx context.Context, t registry.Task, reason string) error
	Free(ctx context.Context) (int, error)
	Reserved(ctx context.Context) ([]Reservation, error)
	ClaimFix(ctx context.Context, w registry.PRWork) (FixCheckout, error)
	ReleaseFix(ctx context.Context, w registry.PRWork, reason string) error
	FixReserved(ctx context.Context) ([]FixReservation, error)
	StartServer(ctx context.Context, c Copy) error
	StopServer(ctx context.Context, c Copy) error
	StartBrowser(ctx context.Context, c Copy, dir string) (cdp string, err error)
	StopBrowser(ctx context.Context, c Copy) error
	Reconcile(ctx context.Context) error
	RepairDue(ctx context.Context) (string, error)
	Repair(ctx context.Context, name string) error
	Outages(ctx context.Context) ([]Outage, error)
}

// Reservation is a copy a task's owner row holds, from its claim (one that
// got the copy, waits or failed) until its release.
type Reservation struct {
	TaskID int64
	Copy   string
}

// Jira reads a task's ticket, searches issues by the words of a free
// prompt, reads where a task's issue stands after the read before
// (tracking.go) and the moves its workflow offers it, which a plan of a
// move reads (jira_plans.go). It only reads: the factory writes nothing to
// Jira.
type Jira interface {
	Ticket(ctx context.Context, key string) (jira.Ticket, error)
	Search(ctx context.Context, words string, limit int) ([]jira.Ticket, error)
	StatusSince(ctx context.Context, key string, prior jira.IssueStatus) (jira.IssueStatus, error)
	Transitions(ctx context.Context, key string) ([]jira.Transition, error)
}

// PRView is a pull request as the factory reads it back after it opened it.
type PRView struct {
	Number                     int
	URL, Author, Base, HeadSHA string
	Draft                      bool
}

// GitHub opens a task's PR (ready for review, never a draft) and reads it
// back, by its branch or its number; ActiveLogin is the login gh acts as
// for repo now (the account active in your gh config dir), and
// MergeMethods the ways repo's settings let a PR merge, which the prompts
// of the roles that ask you questions name.
type GitHub interface {
	CreatePR(ctx context.Context, repo, base, branch, title, bodyFile string) (PRView, error)
	PRByBranch(ctx context.Context, repo, branch string) (PRView, bool, error)
	ViewPR(ctx context.Context, repo string, number int) (PRView, error)
	ActiveLogin(ctx context.Context, repo string) (string, error)
	MergeMethods(ctx context.Context, repo string) (MergeMethods, error)
}

// MergeMethods are the ways a repository's GitHub settings let a pull
// request merge: a merge commit, a squash, a rebase.
type MergeMethods struct{ Merge, Squash, Rebase bool }

// String is the methods m allows, as a prompt names them: "squash",
// "merge commit or squash", "merge commit, squash or rebase"; "" for none.
func (m MergeMethods) String() string {
	var on []string
	for _, w := range []struct {
		on   bool
		name string
	}{{m.Merge, "merge commit"}, {m.Squash, "squash"}, {m.Rebase, "rebase"}} {
		if w.on {
			on = append(on, w.name)
		}
	}
	if len(on) < 2 {
		return strings.Join(on, "")
	}
	return strings.Join(on[:len(on)-1], ", ") + " or " + on[len(on)-1]
}

// Magnum is the review daemon as the factory sees it: busy while its real
// rounds in flight and due fill its limit (max_concurrent_reviews), so a
// local round takes a free round or waits; and what its registry says of
// the factory's PRs (PRStates: the row, the verdict, the loop), which the
// screen draws and the tick follows, of their fix loops over time
// (FixRecords), which the PRs' status files count, of your own open PRs
// (OwnPRs), which the screen lists, of its first round on a PR that
// posted a review since a time (FirstPostedRound), which the escape check
// of a published task's local review reads (escapes.go), and of your
// overrule of Codex's reading (CodexOverrule: `magnum resume --tool
// codex`, zero for none), which the local review's and the stages' waits
// on Codex's reading honor, as magnum's caps do (runtime.CodexOverrule).
type Magnum interface {
	Busy(ctx context.Context) (busy bool, why string, err error)
	PRStates(ctx context.Context, refs []PRRef) (map[PRRef]PRState, error)
	OwnPRs(ctx context.Context) ([]PRState, error)
	FixRecords(ctx context.Context, refs []PRRef) (map[PRRef]FixRecord, error)
	FirstPostedRound(ctx context.Context, ref PRRef, since time.Time) (PostedRound, bool, error)
	CodexOverrule(ctx context.Context) (time.Time, error)
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
	// ReviewCandidate is the directory of the review candidate the local
	// rounds run with (WriteReviewCandidate): its skill's hash goes on each
	// round's cycle. Usage reads Codex's budget (nil: usage.Codex of [usage]
	// codex_home), which a local round with a Codex role yields to at
	// [usage] codex_soft.
	ReviewCandidate string
	Usage           func(ctx context.Context, now time.Time) (usage.Snapshot, error)
	Git             *gitx.Client
	// Run runs the overlap line's reads (Overlap: git log in the main
	// clone, gh pr list as your gh login) and the factory's own commands in
	// a task's copy (its precheck and its proof), every one through the deny
	// list (Guard: New wraps it). The daemon builds Git over the same
	// guarded runner.
	Run execx.Runner
	// Memory reads the machine's memory (engine.ReadMemory); Disk the free
	// bytes of the filesystem that holds path; MemProcs the process table
	// (ReadMemProcs), whose largest processes and Codex brokers the memory
	// hold names (nil names none).
	Memory   func(ctx context.Context) engine.MemorySample
	Disk     func(path string) (free uint64, err error)
	MemProcs func(ctx context.Context) ([]MemProc, error)
	// MySQL pings the copies' database server; HerdrUp asks the factory's
	// herdr session whether it answers; Reveal opens a terminal on it
	// (`herdr --session <herdr_session>`, internal/reveal).
	MySQL   func(ctx context.Context) error
	HerdrUp func(ctx context.Context) error
	Reveal  func(ctx context.Context) error
	// ReviewHerdrUp asks the herdr session of magnum's review panes, where
	// the local review opens its panes (config.Config.ReviewHerdr: [herdr]
	// review_session, yours by default), whether it answers; nil takes it
	// as up. ReviewReveal opens a client of it in a new terminal window,
	// which starts its server; nil while that session is yours, which the
	// factory never opens.
	ReviewHerdrUp func(ctx context.Context) error
	ReviewReveal  func(ctx context.Context) error
	// Notifier shows the alerts' toasts (alerts.go): magnum's notify, through
	// your herdr session (osascript after), deduplicated in a store of
	// magnum's schema (the factory's agents store); the delivery toasts
	// through a copy of it that waits out less of herdr's rate limit, so a
	// toast fits its bound on the loop. nil shows none, and the alerts wait
	// for a build that has one. Sidebar reports a task's sidebar
	// token on a workspace of the factory's herdr session
	// (notify.Notifier.Sidebar); nil reports none.
	Notifier *notify.Notifier
	Sidebar  func(ctx context.Context, workspaceID string, tokens map[string]string) error
	// Staging reads where the published tasks' PRs stand on their
	// repositories' staging envs (staging.go: NewStaging, over
	// internal/staging's client, which the daemon builds only when a
	// [[factory.repo]] has staging entries); nil reads none, and no row has
	// staging cells.
	Staging Staging
	// PRWork is the work behind the factory's PR requests (fix_requests.go:
	// a fix, its switches, a CI fix, a merge of the parent, a stop, a
	// hand-off), the fix lane's; nil answers each that this build does not
	// fix PRs yet. While [factory] fix is off no request reaches it.
	PRWork PRWorker
	// Build is the build the daemon runs (engine.Build.Label), which
	// work.json names for the CLI's status; Boot reads the Mac's boot at the
	// start (engine.ReadBoot), which ends a drain for a reboot begun on
	// another boot (control.go); nil leaves it unknown.
	Build string
	Boot  func(ctx context.Context) (engine.BootID, error)
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
	// drainAt is when the stop signal came (Unix nanoseconds, 0 before):
	// the drain's budget counts from it (drainLeft).
	drainAt atomic.Int64
	// observeNow asks the loop for an observe pass now (requestObserve: an
	// agent's hook event); one pending absorbs the others, as a kick does.
	// hookGap is the least time between two such passes (hookPassGap; a
	// test's seam).
	observeNow chan struct{}
	hookGap    time.Duration
	// passSeen, when set, is told of each observe pass the loop runs:
	// whether the hooks asked for it, and when it began (a test's seam).
	passSeen func(hook bool, at time.Time)

	// Seams: the signal subscription, the process table that names the
	// daemon that holds the lock, whether the command that asked for a
	// drain still runs (liveDrainer), a process's start (procStart:
	// execx.ProcessStart) and the write of work.json (workWrite; nil:
	// fsx.ReplaceFile).
	notify       func(c chan<- os.Signal, sig ...os.Signal)
	stopNotify   func(c chan<- os.Signal)
	procs        engine.Processes
	drainerAlive func(d Drain) bool
	procStart    func(pid int) (string, error)
	workWrite    func(path string, b []byte) error
	// getenv reads the daemon's environment (os.Getenv), which the copies'
	// commands inherit (checksPrint).
	getenv func(string) string

	// What follows is the loop's own: Run's goroutine (or a test's) reads
	// and writes it, except the heavy slots (heavyMu), which a stage's
	// goroutine may release.
	tickAt    time.Time // when the current tick started
	ticks     uint64    // the ticks begun, the current one's number
	tried     bool      // the current tick advanced the tasks: a try of the infrastructure hold
	lastAlive time.Time // when the last tick or observe pass ended (wall clock)
	uncounted bool      // a wake: this tick's failures count none (wake)
	panics    map[int64]int
	lastFail  map[int64]*stageFails
	// helds are the live tasks' held waits as the log knows them (logHeld).
	helds map[int64]*heldLog
	// culprits is what the memory hold names after its reason, read at
	// culpritsAt (memoryCulprits); both are zero without a memory hold.
	culprits   string
	culpritsAt time.Time
	// publishFails counts the failed tries of each task's publish effect.
	publishFails map[int64]publishFail
	readFails    []error
	inbox        map[int64][]runtime.Event
	holds        map[string]*Hold
	blockers     map[string]string
	recent       map[string]recentFailure
	// envFails are the crashes of the machine's environment the tasks'
	// checks met in this tick (envCrash), which the infrastructure hold
	// weighs with its other failures; envCrashed has the tasks whose last
	// check crashed so (at which head, how many times in a row), whose next
	// check is the hold's try, and holdTry says that such a check runs in
	// this tick: the hold neither lifts nor counts a failed try before it
	// ends.
	envFails   []error
	envCrashed map[int64]envCrashes
	holdTry    bool
	// holdsWritten is the holds as this daemon last saved them for the
	// screen (saveHolds), holdsSaved whether it saved them yet.
	holdsWritten []byte
	holdsSaved   bool
	// pruned is when the screen's own requests were last pruned
	// (pruneScreenRequests), savesPruned the checkouts' saves (pruneSaves).
	pruned, savesPruned time.Time
	// lastReveal is when a herdr hold last opened herdr's session;
	// reviewRevealed when the local review last opened magnum's review
	// session (reviewHerdrDown).
	lastReveal     time.Time
	reviewRevealed time.Time
	// reviewPing is Deps.ReviewHerdrUp's answer in tick `tick`
	// (reviewHerdrDown): one ping a tick, however many tasks ask.
	reviewPing struct {
		tick uint64
		err  error
	}
	// repairing is the backoff repair of a broken copy that runs off the
	// loop (repairCopies), one at a time, and repairCauses the cause each
	// copy's failed repairs last logged (said once while it stays): the
	// loop's own.
	repairing    *copyRepair
	repairCauses map[string]string
	// recovery is what the agents' recovery has left to do after a failure
	// (nil: nothing); a tick herdr answers runs it once due (retryRecovery).
	recovery *agentsRecovery
	parked   map[int64]bool
	servers  map[int64]server
	// beforeMark runs right before codexRefused writes its mark (a test's
	// seam: another writer moving the task meanwhile); nil in a daemon.
	beforeMark func()
	// jiraTries are the next tries of the tasks that wait for Jira
	// (retryJira).
	jiraTries map[int64]*jiraTry
	// jiraLeft is how many of those tries the tick may still make
	// (jiraTriesPerTick, none after a try Jira did not answer); the batch of
	// reads of the tasks' issues takes what the tries leave (trackJira).
	// trk is that tracking (the loop's own, but for its batch's answers and
	// marks); trackReadBound and trackBatchBound bound one read and one
	// batch, and trackSendWait how long a batch's answers wait for their
	// place (trackBound, batchBound, sendWait; a test's seams).
	jiraLeft        int
	trk             trackState
	trackReadBound  time.Duration
	trackBatchBound time.Duration
	trackSendWait   time.Duration
	// overlaps are the overlap lines the new-task previews read, by
	// repository and key, and jiraReads Jira's answers to their reads of a
	// key, a failure included, both kept for previewOverlapTTL
	// (previewOverlap, previewJira).
	overlapMu sync.Mutex
	overlaps  map[string]overlapRead
	jiraReads map[string]jiraRead
	// merges are the merge methods of each repository the prompts name, a
	// failed read included (mergeMethods). mergeBound bounds a read of them
	// (0: mergeMethodsBound), a test's seam: a timeout without its 10
	// seconds.
	mergeMu    sync.Mutex
	merges     map[string]mergeRead
	mergeBound time.Duration

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
	// (payReleaseDebts); claimMu guards the map, and a debt's fields are
	// the loop's own.
	debts map[int64]*releaseDebt
	// The same for the fixes of your PRs, by pr_work id (claimFix): the
	// claims of their checkouts off the loop, the waits of those that ended
	// without one, the checkout each fix holds (for its cycle), and the
	// releases of the checkouts of fix work that ended that did not go
	// through (payFixDebts); claimMu guards the four.
	fixClaims map[int64]*claimRun
	fixWaits  map[int64]*claimWait
	fixHeld   map[int64]heldFix
	fixDebts  map[int64]*releaseDebt

	// talking are the tasks whose talk request runs off the loop
	// (talkRequest); talkMu guards the set.
	talkMu  sync.Mutex
	talking map[int64]bool

	// agentRuns are the agents stages' attempts in progress, by task
	// (runAgents): the loop's own, rebuilt from the registry after a
	// restart.
	agentRuns map[int64]*agentsRun
	// codexUnread says that the last read of Codex's budget before a stage's
	// launch failed, which codexYield logged once: the next failure is not
	// logged again until a read goes through.
	codexUnread bool
	// overrule is your overrule of Codex's reading in magnum's registry
	// (Magnum.CodexOverrule), which codexSoft and codexYield honor.
	overrule *runtime.CodexOverrule

	// qaSessions are the qa stages' runs that hold the Mac, by task (qa.go):
	// the loop's own but for each boot's answer, which its goroutine writes
	// before its done closes; qaMu guards the map. qaProbe asks a copy's
	// server whether it answers (nil: httpQAProbe), a seam of the tests.
	qaMu       sync.Mutex
	qaSessions map[int64]*qaSession
	qaProbe    func(ctx context.Context, url, host string) (status int, err error)

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
	// keptCopies are the copies base runs left detached that resumeDetached
	// keeps for you, by task (keptCopy: said once, looked at again only
	// when due, keptDue): the loop's own.
	keptCopies map[int64]keptCopy
	// keptMem are the records of kept changes the factory could not write
	// to kept.json, by task (keepFor runs on a check's goroutine: keptMu);
	// keptWrite writes kept.json (nil: an atomic write), a seam of the
	// tests.
	keptMu    sync.Mutex
	keptMem   map[int64]keptUnwritten
	keptWrite func(path string, b []byte) error
	lockPoll  time.Duration
	after     func(d time.Duration) <-chan time.Time
	// liveLimit bounds a run's output in live.log (0: liveMax), a seam of
	// the tests.
	liveLimit int

	// The local review's rounds that run off the loop, one per task
	// (launchRound), which reviewMu guards; roundCap ends a round that runs
	// longer (LocalRoundCap; a seam of the tests).
	reviewMu sync.Mutex
	reviews  map[int64]*localRound
	roundCap time.Duration
	// magnumNotes is what Magnum.Busy last said of each task's wait while
	// not busy (said once, as an event): the loop's own.
	magnumNotes map[int64]string
	// followErr is the failed read of magnum's registry followPRs last
	// logged ("" after a read that worked): logged once while it stays.
	// parkOwed are the tasks that ended while herdr was down, whose agents
	// a tick with herdr up parks (parkEnded, payParkDebts). Both the loop's
	// own.
	followErr string
	parkOwed  map[int64]bool
	// statusErrs are the tasks whose status file failed since its last
	// write that worked, logged once; statusReadErr the failed read of
	// magnum's registry for them ("" after a read that worked); statusFinal
	// the ended tasks whose last write is done or that never had a file
	// (writePRStatuses). The loop's own.
	statusErrs    map[int64]bool
	statusReadErr string
	statusFinal   map[int64]bool
	// stg is the record of the staging Refreshes that run off the loop and
	// of the screen's staging.json (staging.go); its mutex guards it.
	// stagingWrite writes staging.json (nil: fsx.ReplaceFile), a seam of the
	// tests.
	stg          stagingReads
	stagingWrite func(path string, b []byte) error
	// esc is the record of the escape checks that run off the loop
	// (escapes.go); its mutex guards it. ciSeen is, per published task, the
	// head and result of the PR's CI the follow last wrote to its checks,
	// and ciErr the failed write it last logged ("" after one that worked):
	// the loop's own (copyCI).
	esc    escapeChecks
	ciSeen map[int64]string
	ciErr  string
	// beforeStop runs between the end of a loop's cycle that needs you
	// (unavailable, the cap) and the task's move to you, or the go-on's
	// word of one that goes on by itself (goOnAtCap); an error stops there
	// (a test's seam: the daemon stopping in between). nil in a daemon.
	beforeStop func() error
	// beforeSecond runs after the write of a second opinion
	// (secondOpinionFile) before its step: "event" (the decision's event)
	// and "end" (the end of the clean round's cycle it follows); an error
	// stops there (a test's seam). nil in a daemon.
	beforeSecond func(step string) error

	// alerts is the delivery of the alerts (alerts.go): the loop's own.
	// screenLive says whether a focus report is of a screen that still runs
	// (nil: FocusPayload.Live; a test's seam).
	alerts     alertDelivery
	screenLive func(FocusPayload) bool

	// The work a restart would cut (beginWork), by its sequence, and the
	// record of it the daemon keeps for `magnum factory install`
	// (work.json: workSaved is what it last wrote, workClosed that it stopped
	// writing as the daemon stops). workMu guards them: the work's
	// goroutines end their own. drainHeld is the drain install asked for
	// that the daemon holds (a string, "" for none): takeDrain sets it, and
	// the start gates read it without a lock. drainWarned is the last
	// warning about a drain.json the daemon could not read (said once):
	// the loop's own.
	workMu      sync.Mutex
	work        map[int]WorkItem
	workSeq     int
	workSaved   []byte
	workClosed  bool
	drainHeld   atomic.Value
	drainWarned string
	// ctl is the pause and the drain for a reboot the daemon holds
	// (control.go).
	ctl controlState

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
		runners:      map[string]stageRunner{},
		kick:         make(chan struct{}, 1),
		observeNow:   make(chan struct{}, 1),
		hookGap:      hookPassGap,
		notify:       ossignal.Notify,
		getenv:       os.Getenv,
		stopNotify:   ossignal.Stop,
		procs:        engine.SystemProcesses(),
		panics:       map[int64]int{},
		lastFail:     map[int64]*stageFails{},
		helds:        map[int64]*heldLog{},
		publishFails: map[int64]publishFail{},
		inbox:        map[int64][]runtime.Event{},
		holds:        map[string]*Hold{},
		blockers:     map[string]string{},
		recent:       map[string]recentFailure{},
		parked:       map[int64]bool{},
		repairCauses: map[string]string{},
		servers:      map[int64]server{},
		heavyHeld:    map[int64]*heavySlot{},
		jiraTries:    map[int64]*jiraTry{},
		talking:      map[int64]bool{},
		claims:       map[int64]*claimRun{},
		claimWaits:   map[int64]*claimWait{},
		held:         map[int64]Copy{},
		debts:        map[int64]*releaseDebt{},
		fixClaims:    map[int64]*claimRun{},
		fixWaits:     map[int64]*claimWait{},
		fixHeld:      map[int64]heldFix{},
		fixDebts:     map[int64]*releaseDebt{},
		statusErrs:   map[int64]bool{},
		statusFinal:  map[int64]bool{},
		stg:          newStagingReads(),
		esc:          newEscapeChecks(),
		ciSeen:       map[int64]string{},
		agentRuns:    map[int64]*agentsRun{},
		qaSessions:   map[int64]*qaSession{},
		run:          Guard(d.Run, d.Config),
		checks:       map[int64]*headCheck{},
		checkNotes:   map[int64]string{},
		checkFails:   map[int64]int{},
		envCrashed:   map[int64]envCrashes{},
		keptCopies:   map[int64]keptCopy{},
		keptMem:      map[int64]keptUnwritten{},
		lockPoll:     specsLockPoll,
		reviews:      map[int64]*localRound{},
		work:         map[int]WorkItem{},
		magnumNotes:  map[int64]string{},
		parkOwed:     map[int64]bool{},
		roundCap:     LocalRoundCap,

		trackReadBound:  trackBound,
		trackBatchBound: batchBound,
		trackSendWait:   sendWait,
	}
	e.trk.done = make(chan trackBatch, 1)
	e.overrule = &runtime.CodexOverrule{Read: e.readOverrule, Log: log}
	e.drainerAlive, e.procStart = e.liveDrainer, processStart
	e.runners[config.FactoryStageAgents] = e.runAgents
	e.runners[config.FactoryStageGate] = e.runGate
	e.runners[config.FactoryStageReview] = e.runReview
	e.runners[config.FactoryStageDecision] = e.runDecision
	e.runners[config.FactoryStageEffect] = e.runEffect
	e.runners[config.FactoryStageQA] = e.runQA
	e.alerts = e.newAlertDelivery(d.Notifier)
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
	return 5 * time.Second
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
// SIGTERM or SIGINT, or until ctx ends: it writes the pidfile, opens its
// hook socket (listenHooks), runs startup, serves the socket (serveHooks),
// then runs a tick at once, every [factory] poll and on SIGUSR1, and an
// observe pass every [factory] observe and, at most once a second, at once
// when the agents' hooks ask for one (hookPasses; one that saw an event a
// stage acts on kicks a tick right after it), none once the stop signal
// came. A stop signal drains: no new launch or effect starts (a launch that
// waits for the stagger is cut), the tick in progress ends with the task it
// advances (the others, and the alerts, wait for the next daemon), the
// waits for the hooks' looks and for the work off the loop end within
// drainBudget of the signal, and it returns nil, so launchd (KeepAlive on a
// failed exit only) starts the daemon again only after a crash.
func (e *Engine) RunLocked(ctx context.Context) error {
	// The record of the work a restart would cut comes before the pidfile:
	// install reads it for the daemon the pidfile names.
	e.saveWork()
	defer e.closeWork()
	pidFile := e.d.Layout.FactoryPid()
	if err := fsx.WriteFileAtomic(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("factory: pidfile: %w", err)
	}
	defer os.Remove(pidFile)
	// The work off the loop ends with the daemon: a claim's setup or install
	// must not go on in a copy after it exits. The staging client closes
	// after it, once no Refresh runs.
	defer e.closeStaging()
	defer e.stopBackground()
	// A QA run's server and browser stop with the daemon, a drain's or a
	// signal's, on a context of their own (it runs before stopBackground).
	defer e.stopQASessions(context.WithoutCancel(ctx))
	e.endDrainAtStart(ctx)
	e.endRebootDrainAtStart(ctx) // control.go: a drain for a reboot ends at the first start on another boot
	e.takeControls(ctx)          // control.go: one that stays holds from the start, before the startup's recovery

	stop := make(chan struct{})
	sigs := make(chan os.Signal, 4)
	e.notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	defer e.stopNotify(sigs)
	sigCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.signals(sigCtx, sigs, stop)

	e.log.Info("magnum factory daemon starting", "pid", os.Getpid())
	// The hook socket opens under the lock and before the startup's first
	// pane, so every pane names it; it is served once the startup is done
	// (events sent meanwhile wait in the socket), and its stop comes first
	// as the daemon stops.
	hooks := e.listenHooks()
	if hooks != nil {
		defer hooks.Close() // a startup that fails never serves it
	}
	if err := e.startup(ctx); err != nil {
		e.log.Error("magnum factory daemon refusing to start", "err", err)
		return err
	}
	if hooks != nil {
		stopHooks := e.serveHooks(ctx, hooks)
		defer stopHooks()
	}
	tick := time.NewTimer(0)
	defer tick.Stop()
	observe := time.NewTimer(e.observeEvery())
	defer observe.Stop()
	asked := e.newHookPasses()
	defer asked.floor.Stop()
	for {
		hook := false // the hooks ask for a pass (hookPasses)
		select {
		case <-ctx.Done():
			return nil
		case <-stop:
			e.log.Info("magnum factory daemon stopped")
			return nil
		case <-observe.C:
			if e.draining.Load() { // the stop signal came: it ends the loop, not a pass
				e.log.Info("magnum factory daemon stopped")
				return nil
			}
			e.observePass(ctx, asked, false)
			observe.Reset(e.observeEvery())
			continue
		case <-e.observeNow:
			hook = true
		case <-asked.asked:
			hook = true
		case <-asked.ended:
			asked.ended, hook = nil, true // closed: the pass takes a fresh one
		case <-asked.floor.C:
			asked.armed, hook = false, true
		case <-tick.C:
		case <-e.kick:
			tick.Stop()
		}
		if hook {
			if e.draining.Load() {
				e.log.Info("magnum factory daemon stopped")
				return nil
			}
			if asked.due(time.Now()) {
				e.observePass(ctx, asked, true) // the clock keeps its pace
			}
			continue
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
			e.drainAt.Store(time.Now().UnixNano())
			e.draining.Store(true)
			if e.d.Runtime != nil {
				e.d.Runtime.BeginDrain() // a launch waiting for the stagger starts nothing
			}
			close(stop)
			return
		}
	}
}

// liveStates are the states of a task the loop advances.
var liveStates = []string{registry.TaskActive, registry.TaskWaiting}

// heldWarnAfter is how long a task's held wait logs at INFO alone: past it
// the wait logs at WARN, and again after twice as long each time (logHeld).
const heldWarnAfter = 30 * time.Minute

// heldLog is a task's held wait as the log knows it: the stage it held,
// whom it waits on and for what (the held error's who and what, and its
// whole text), since when it holds, when it next logs at WARN and the gap
// after that.
type heldLog struct {
	stage, who, what, wait string
	since, warnAt          time.Time
	gap                    time.Duration
}

// logHeld logs task t's held wait err (an ErrHeld, which costs the task
// nothing and is no failure of the tick): at INFO when it begins, nothing
// while it holds the same (at the same stage, sameWait), and at WARN once
// it has held heldWarnAfter, then after twice as long each time (30
// minutes, 1 hour, 2 hours later). A wait that differs ends the one before
// (endHeld) and begins. A wait the registry already records (it held
// before a restart) begins at its span (waitBegan). A first wait at no
// stage takes the stage the pass recorded: advance names a new task's first
// stage on its own copy of the task.
func (e *Engine) logHeld(ctx context.Context, t registry.Task, err error, now time.Time) {
	wait, who, what := err.Error(), "", err.Error()
	if he, ok := errors.AsType[heldError](err); ok {
		who, what = he.who, he.what
	}
	h := e.helds[t.ID]
	if h != nil && ((h.stage != "" && h.stage != t.Stage) || h.who != who || !sameWait(who, h.what, what)) {
		e.endHeld(t.ID, now)
		h = nil
	}
	if h == nil {
		since := e.waitBegan(ctx, t, who, what, now)
		h = &heldLog{stage: t.Stage, who: who, since: since, warnAt: since.Add(heldWarnAfter), gap: heldWarnAfter}
		e.helds[t.ID] = h
		attrs := []any{"task", t.ID, "stage", t.Stage, "wait", wait}
		if since.Before(now) {
			attrs = append(attrs, "since", since)
		}
		e.log.Info("a factory task waits", attrs...)
	}
	h.stage, h.what, h.wait = cmp.Or(h.stage, t.Stage), what, wait
	if !now.Before(h.warnAt) {
		h.gap *= 2
		h.warnAt = now.Add(h.gap)
		e.log.Warn("a factory task still waits", "task", t.ID, "stage", h.stage, "wait", wait, "for", now.Sub(h.since).Round(time.Second))
	}
}

// waitBegan is when task t's wait on who for what began: the task's last
// span, when the registry records the task waiting so since it (the wait
// held before this daemon started); else now.
func (e *Engine) waitBegan(ctx context.Context, t registry.Task, who, what string, now time.Time) time.Time {
	if who == "" || t.WaitsWho != who || !sameWait(who, t.WaitsWhat, what) {
		return now
	}
	spans, err := e.reg.Spans(ctx, t.ID)
	if err != nil || len(spans) == 0 {
		return now
	}
	last := spans[len(spans)-1]
	if last.WaitsWho != who || !sameWait(who, last.WaitsWhat, what) || last.At.After(now) {
		return now
	}
	return last.At
}

// sameWait reports whether two texts of a wait on who are one wait: the same
// words, numbers aside (sameWords), or any two of the memory hold's, whose
// names of processes change while it holds (memoryCulprits).
func sameWait(who, a, b string) bool { return who == HoldMemory || sameWords(a, b) }

// endHeld logs the end of task id's held wait, when it has one, with how
// long it held.
func (e *Engine) endHeld(id int64, now time.Time) {
	h := e.helds[id]
	if h == nil {
		return
	}
	delete(e.helds, id)
	e.log.Info("a factory task's wait ended", "task", id, "stage", h.stage, "wait", h.wait, "after", now.Sub(h.since).Round(time.Second))
}

// Tick runs one pass of the loop: the holds (herdr, disk and memory, then
// the infrastructure hold and a MySQL ping before a pass that advances),
// the requests, the published tasks' PRs (followPRs, whether or not the
// pass advances), their staging status (stagingPass, whose Refreshes run
// off the loop), the answers of the last batch of reads of the tasks' Jira
// issues (takeTracking) and the PRs' status files (writePRStatuses), then
// each live task's advance under its own panic guard (safely), then the
// Jira comment each task's milestone would keep, in shadow (jiraComments),
// and the next batch of those reads, off the loop, with what the intake's
// tries left of the tick's Jira budget (trackJira), the reads of the plans
// of the Jira moves among them (jira_plans.go). During an infrastructure or a herdr hold no task advances
// except on the hold's tries, and while herdr does not answer no copy a
// base run left detached is swept (sweepDetached, each under safely too);
// a failure that a hold covers costs its task nothing. Last, the alerts
// are delivered (deliverAlerts). The returned error joins what failed, for
// the log; each step records its own. A task's held wait is no error of
// the tick: logHeld logs it once, at WARN only past heldWarnAfter. A task
// whose stage failed for a cause of its own is advanced no sooner than a
// poll after the failure (retryLater).
func (e *Engine) Tick(ctx context.Context) error {
	now := e.now()
	e.tickAt = now
	e.ticks++
	e.holdTry = false
	e.jiraLeft = jiraTriesPerTick
	e.wake(now)
	defer e.alive()
	defer e.saveHolds() // the screen draws the holds in force
	e.takeDrain(ctx)    // before anything starts in this tick
	e.takeControls(ctx) // control.go: the pause and the drain for a reboot, before anything starts too
	var errs []error

	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		return fmt.Errorf("factory tick: %w", err)
	}
	e.tendRebootDrain(ctx, tasks) // control.go: what still runs from the ticks before, the park, the acknowledgement
	e.sweepQA(ctx, tasks)
	e.sweepHeavy(ctx, tasks)
	e.sweepClaims(ctx, tasks)
	e.sweepChecks(tasks)
	e.sweepRounds(tasks)
	e.payReleaseDebts(ctx, tasks)
	e.payFixDebts(ctx)
	herdrUp := e.checkHerdr(ctx, now)
	if herdrUp { // herdr says who is in a copy: without it no copy is touched
		e.retryRecovery(ctx, tasks)
		e.sweepDetached(ctx, tasks)
		e.payParkDebts(ctx)
	}
	e.pressure(ctx, now, tasks)
	e.repairCopies(ctx)   // after the disk hold's check: none starts under it
	e.copyHolds(ctx, now) // after the repair that ended: a copy back in the pool lifts its repository's hold
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
	e.pruneScreenRequests(ctx, now)
	e.pruneSaves(now) // saves_prune.go
	e.admit(ctx)
	var refs map[int64]PRRef // the published tasks' PRs, read once for the passes that follow them
	if e.d.Magnum != nil || e.d.Staging != nil {
		refs = e.publishedRefs(ctx, tasks)
	}
	states := e.followPRs(ctx, tasks, refs) // its moves are compare-and-set from published: a request's stop since wins
	stagings := e.stagingPass(tasks, refs, states)
	e.takeTracking(ctx) // the last batch's reads of the issues, which the status files carry
	e.writePRStatuses(ctx, tasks, states, stagings)

	if advance {
		if tasks, err = e.reg.Tasks(ctx); err != nil { // the requests may have moved some
			return fmt.Errorf("factory tick: %w", err)
		}
		lt := live(tasks)
		for _, t := range lt {
			if e.draining.Load() { // the task in progress ended its pass; the next daemon advances the others
				break
			}
			if e.retryLater(t, now) {
				continue
			}
			err := e.safely(ctx, t, func(ctx context.Context) error { return e.advance(ctx, t) })
			if next, ok := e.passedOn(ctx, t, err); ok { // the next stage's first pass, in this tick, once
				delete(e.lastFail, t.ID)
				e.endHeld(t.ID, now)
				t, err = next, e.safely(ctx, next, func(ctx context.Context) error { return e.advance(ctx, next) })
			}
			switch {
			case err == nil:
				delete(e.lastFail, t.ID)
				e.endHeld(t.ID, now)
			case errors.Is(err, ErrHeld):
				e.logHeld(ctx, t, err, now) // a wait, logged by itself: no error of the tick, so no WARN each tick
			case errors.Is(err, errPanicked), herdrDown(err), ctx.Err() != nil:
				errs = append(errs, err) // herdr down inside the pass: herdr's hold covers it, no failure of the task
			default:
				e.endHeld(t.ID, now)
				failures = append(failures, taskFailure{task: t, err: err})
			}
		}
		for id := range e.helds { // a task that left the live states waits no more
			if !slices.ContainsFunc(lt, func(t registry.Task) bool { return t.ID == id }) {
				e.endHeld(id, now)
			}
		}
	}
	e.jiraComments(ctx, tasks) // the Jira comment of each task's milestone, in shadow: a file in its folder
	e.trackJira(ctx, tasks)    // a batch off the loop reads the issues, with what the intake's tries left of the tick's Jira budget
	failures = append(failures, e.envFails...)
	failures = append(failures, e.readFails...)
	e.envFails, e.readFails = nil, nil
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
	if !e.draining.Load() { // a toast and the sidebar may take 25 s: alerts.json lets the next daemon deliver them
		e.deliverAlerts(ctx, tasks) // after every step that may have alerted
	}
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
// stage passes (nextStage: a qa stage of a repository without
// [factory.repo.qa], and an agents stage for the other width of plan, are
// skipped, a stage event says so). The last stage's own effect ends the
// task. A task that waits for Jira before its first stage tries Jira
// instead (retryJira), and one whose key has no ticket.md yet (your answer
// linked it) reads its ticket first (linkedTicket). A task past its coding
// stage whose answer changed the plan after coding went on the recommended
// one goes back to coding once its stage has nothing in flight
// (changePastCoding). Your answers to its stop questions are acted on
// before the stage's pass (decideStops: a drop ends the task). A copy a base
// run left
// detached is restored before anything else (resumeDetached), and what a
// check left in the copy for you is settled (settleKept): put back under
// its guard, or the task needs you again for it, never the writer.
func (e *Engine) advance(ctx context.Context, t registry.Task) error {
	if t.Stage == "" && t.State == registry.TaskWaiting && t.WaitsWho == waitJira {
		return e.retryJira(ctx, t)
	}
	if err := e.resumeDetached(ctx, t); err != nil {
		return err
	}
	if why, err := e.settleKept(ctx, t); err != nil {
		return err
	} else if why != "" {
		e.toOperator(ctx, t, why, "a check left changes for you")
		return heldError{who: waitsYou, what: why}
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
		if wrote := e.restoreData(ctx, t); !wrote {
			return nil // writeData sent the task to you with the cause
		}
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
	if ended, err := e.decideStops(ctx, t); err != nil || ended { // your answers to its stop questions, first
		return err
	}
	if back, err := e.changePastCoding(ctx, t, st, i); err != nil || back {
		return err
	}
	done, err := run(ctx, t, st)
	if err != nil || !done || i+1 == len(kind.Stages) {
		return err
	}
	next, skipped := e.nextStage(t, kind, i)
	if next == "" {
		return nil
	}
	if err := e.reg.TransitionTask(ctx, t.ID, []string{t.State}, "", func(u *registry.TaskUpdate) {
		u.Set("stage", next)
		u.Set("attempt", 0)
		u.Because(st.Name + " passed")
	}); err != nil {
		return err
	}
	for _, sk := range skipped {
		e.event(ctx, t.ID, "stage", registry.PhaseInfo, sk.stage+" skipped: "+sk.why)
	}
	return nil
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

// passedOn is task t read again after a pass of its stage that ended with
// err, when that pass moved it to a later stage of its kind (the stage
// passed and the move landed), and it is still live and waits on no one but
// its agents: Tick then runs the next stage's first pass in the same tick,
// under the guards of any pass (safely, the per-task handling, a launch's
// holds), so the next agent's prompt goes at once, not a poll later. Only
// the pass a tick began with chains: a task moves at most one stage further
// a tick. ok is false after an error, a stage that did not pass, a move that
// failed, a pass that ended the task or sent it to you, and while the daemon
// drains.
func (e *Engine) passedOn(ctx context.Context, t registry.Task, err error) (registry.Task, bool) {
	kind := e.cfg.Factory.Kinds[t.Kind]
	if err != nil || e.draining.Load() || len(kind.Stages) == 0 {
		return t, false
	}
	next, err := e.reg.TaskByID(ctx, t.ID)
	if err != nil || stageIndex(kind, next.Stage) <= stageIndex(kind, e.stageRan(t)) ||
		!slices.Contains(liveStates, next.State) || waitsOnYou(next) {
		return t, false
	}
	return next, true
}

// stageRan is the stage a pass of task t runs: its stage, or for a task
// without one yet its kind's first, which advance records on the task's
// own copy alone (Tick's copy keeps the empty stage).
func (e *Engine) stageRan(t registry.Task) string {
	if k := e.cfg.Factory.Kinds[t.Kind]; t.Stage == "" && len(k.Stages) > 0 {
		return k.Stages[0].Name
	}
	return t.Stage
}

// observe is the pass between ticks: it reads the agents' panes and the
// watchdog (Runtime.Observe and Watchdog) and keeps their events for each
// task's next advance (takeEvents), except the turns typed into the panes,
// which it records (typed), and a heal's stop of the factory's herdr server
// (runtime.EventHerdrStopped), after which the next tick opens the session
// again at once, whenever it opened it last (checkHerdr's reveal), so the
// fresh server the heal relaunches the roles in comes up. An event a stage
// acts on when it takes it (stageEvents: a run's end, a pause, a dialog, a
// lost pane) kicks the loop, so the tick takes it now rather than at its
// poll. A failed read is a failure the next tick weighs (an infrastructure
// hold), unless the Mac just woke.
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
		if ev.Kind == runtime.EventTyped {
			e.typed(ctx, ev) // a record of what you typed: no stage waits for it
			continue
		}
		if ev.Kind == runtime.EventHerdrStopped {
			e.lastReveal = time.Time{} // the heal's stop: the session opens again on the next tick
			e.Kick()
			continue
		}
		e.inbox[ev.TaskID] = append(e.inbox[ev.TaskID], ev)
		if box := e.inbox[ev.TaskID]; len(box) > maxInbox {
			e.inbox[ev.TaskID] = box[len(box)-maxInbox:]
		}
		if slices.Contains(stageEvents, ev.Kind) {
			e.Kick() // the stage takes it on the tick right after this pass
		}
		switch ev.Kind {
		case runtime.EventDeliveryUnknown:
			e.deliveryUnknown(ctx, ev)
		case runtime.EventUndelivered:
			e.undelivered(ctx, ev)
		case runtime.EventDelivered:
			e.delivered(ctx, ev)
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
// the task is in, it would stall there in silence otherwise. The wait is
// settleUnknown's, from the ledger: it names every prompt of the task to
// look at, one that comes while the task waits on you for another names
// both, and none whose proof is under way. A task that waits on you for
// something else keeps that reason; its stage holds the role once it goes
// on (promptRoles), so none is dropped.
func (e *Engine) deliveryUnknown(ctx context.Context, ev runtime.Event) {
	t, err := e.reg.TaskByID(ctx, ev.TaskID)
	if err != nil {
		return
	}
	if err := e.settleUnknown(ctx, t, "a prompt's delivery is unknown", true); err != nil {
		e.log.Warn("name the prompts of unknown delivery", "task", t.ID, "err", err)
	}
}

// delivered takes a prompt a later read found in its transcript
// (runtime.EventDelivered): its run is watched as submitted, so the stage
// takes its end as any run's and sends nothing again; a task that waited on
// you for it waits on the rest, or goes back to work (settleUnknown).
func (e *Engine) delivered(ctx context.Context, ev runtime.Event) {
	e.event(ctx, ev.TaskID, "prompt", registry.PhaseOK, ev.Reason)
	t, err := e.reg.TaskByID(ctx, ev.TaskID)
	if err != nil {
		return
	}
	if err := e.settleUnknown(ctx, t, "the prompt to "+ev.Role+" arrived: its run is watched", false); err != nil {
		e.log.Warn("settle a task's wait on its unknown deliveries", "task", t.ID, "err", err)
	}
}

// undelivered takes a prompt proven never to have arrived
// (runtime.EventUndelivered): the stage forgets what it knew of its attempt
// and rebuilds it from the ledger on its next pass (agentsRunOf), where
// that prompt counts as never sent, so the role gets what it is due once
// more; a task that waited on you for it waits on the rest, or goes back to
// work (settleUnknown).
func (e *Engine) undelivered(ctx context.Context, ev runtime.Event) {
	delete(e.agentRuns, ev.TaskID)
	e.event(ctx, ev.TaskID, "prompt", registry.PhaseInfo, ev.Reason)
	t, err := e.reg.TaskByID(ctx, ev.TaskID)
	if err != nil {
		return
	}
	if err := e.settleUnknown(ctx, t, "the prompt to "+ev.Role+" never arrived: it goes once more", false); err != nil {
		e.log.Warn("settle a task's wait on its unknown deliveries", "task", t.ID, "err", err)
	}
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
	return e.launchUnder(ctx, e.mayLaunch, t, a, c, roles)
}

// launchUnder is launch under the holds may names: mayLaunch for a stage's
// roles, MayLaunch for the walker of a QA session that exists already,
// which `magnum pause` lets go on, since its walk is no new QA run and the
// pause cuts none (qaGoesOn).
func (e *Engine) launchUnder(ctx context.Context, may func() (string, string, bool), t registry.Task, a registry.Attempt, c Copy,
	roles []config.Role) error {
	if who, what, ok := may(); !ok {
		if who != holdRestart {
			e.waitOn(ctx, t, who, what)
		}
		return heldError{who: who, what: what}
	}
	if e.d.Runtime == nil {
		return heldError{who: "agent", what: "this build runs no agents yet"}
	}
	if err := e.d.Runtime.Launch(ctx, t, a, c, roles); err != nil {
		if errors.Is(err, runtime.ErrDraining) { // the drain cut the stagger: the next daemon launches
			return heldError{who: holdRestart, what: drainWhat}
		}
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
// never while the daemon drains (a stop's drain, install's, or one for a
// reboot); a pause lets your push go out.
func (e *Engine) beginEffect(ctx context.Context, key, kind string, taskID int64) (registry.Effect, bool, error) {
	if e.drainsHeld() {
		return registry.Effect{}, false, heldError{who: holdRestart, what: e.heldWhat()}
	}
	return e.reg.BeginEffect(ctx, key, kind, taskID)
}

// sameWords reports whether two waits differ in their numbers only ("codex
// at 96%" and "codex at 97.5%", "9.5 GB free" and "9.4 GB free").
func sameWords(a, b string) bool {
	return numbers.ReplaceAllString(a, "#") == numbers.ReplaceAllString(b, "#")
}

// numbers are the numbers of a wait's words, a decimal point included.
var numbers = regexp.MustCompile(`[0-9]+(?:[.,][0-9]+)*`)

// waitOn records who the task waits on and for what, when that changed: a
// change of a number alone in the same wait, or of the memory hold's names
// of processes, writes no span (sameWait).
func (e *Engine) waitOn(ctx context.Context, t registry.Task, who, what string) {
	if t.WaitsWho == who && t.WaitsWhat == what {
		return
	}
	if err := e.reg.TransitionTask(ctx, t.ID, []string{t.State}, "", func(u *registry.TaskUpdate) {
		u.Set("waits_who", who)
		u.Set("waits_what", what)
		if t.WaitsWho == who && sameWait(who, t.WaitsWhat, what) { // only a number (or a held process) moved: no span each tick
			u.Quiet()
		}
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

// alert records an alert: a task, or the factory (taskID 0), moved into
// NEEDS YOU. The daemon's loop delivers it (deliverAlerts): a toast and
// the task's sidebar token.
func (e *Engine) alert(ctx context.Context, taskID int64, text string) {
	e.event(ctx, taskID, "alert", registry.PhaseInfo, text)
}

// event writes an audit row, scrubbed (scrub.Text: secrets, emails and
// hosts, as all the text the factory keeps); a failed write is logged.
func (e *Engine) event(ctx context.Context, taskID int64, kind, phase, text string) {
	if err := e.reg.AppendEvent(ctx, taskID, kind, phase, scrub.Text(text)); err != nil {
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

// A stage that keeps failing for a cause of its own (no infrastructure's)
// is tried again a poll after each failure (retryLater), and after
// stageFailLimit failures in a row, or failures for stageFailSpan, the task
// needs you with the cause.
const (
	stageFailLimit = 5
	stageFailSpan  = 5 * time.Minute
)

// stageFails are a task's failures in a row at one stage: how many, since
// when, when the last one was, and its text.
type stageFails struct {
	stage, text string
	n           int
	first, last time.Time
}

// retryLater reports whether the stage task t's pass runs (stageRan)
// failed less than a poll interval before now: its next try waits for the
// first tick no sooner than a poll after the failure. The ticks between the
// polls (an observe pass that saw another task's turn end, a request) try a
// failing stage no more often than the poll did, so its stageFailLimit
// tries still take four polls, and a failing publish's GitHub calls stay as
// few.
func (e *Engine) retryLater(t registry.Task, now time.Time) bool {
	sf := e.lastFail[t.ID]
	return sf != nil && sf.stage == e.stageRan(t) && now.Sub(sf.last) < e.poll()
}

// taskFailed records a task's own failure as an `advance` fail event, once
// per distinct failure in a row, and counts the failures in a row of its
// stage: the stageFailLimit-th, or one stageFailSpan after the first, sends
// the task to you with the cause and an alert (a pass that goes through, or
// your retry, starts the count again). An error the infrastructure hold
// covers never comes here (Tick).
func (e *Engine) taskFailed(ctx context.Context, f taskFailure) {
	text := textx.Clip(textx.FirstLine(scrub.Text(f.err.Error())), 300)
	now := e.now()
	stage := e.stageRan(f.task) // a new task's first pass ran its kind's first stage
	sf := e.lastFail[f.task.ID]
	if sf == nil || sf.stage != stage {
		sf = &stageFails{stage: stage, first: now}
		e.lastFail[f.task.ID] = sf
	}
	sf.n, sf.last = sf.n+1, now
	if sf.text != text {
		sf.text = text
		e.log.Warn("a task's step failed", "task", f.task.ID, "err", f.err)
		e.event(ctx, f.task.ID, "advance", registry.PhaseFail, text)
	}
	if sf.n < stageFailLimit && now.Sub(sf.first) < stageFailSpan {
		return
	}
	delete(e.lastFail, f.task.ID)
	e.toOperator(ctx, f.task, fmt.Sprintf("the %s stage failed %d times in a row: %s; fix its cause (a change of the configuration "+
		"takes effect after `magnum factory install`, one of magnum's code after `make build && magnum factory install`)%s",
		stageWhat(cmp.Or(stage, "first")), sf.n, text, retryHint(f.task)), "a stage that keeps failing")
}

// stageWhat is a stage's name as a wait says it ("local_review": "local
// review").
func stageWhat(name string) string { return strings.ReplaceAll(name, "_", " ") }
