# magnum internal API

Exported surface of every package under `internal/`. The package sections below are generated verbatim from `go doc -all ./internal/<pkg>` by `make api-doc`; only the text above the generated marker is hand-written (it is kept on regeneration). Run `make api-doc` whenever an exported signature or doc comment changes. `go doc ./internal/<pkg> <Symbol>` prints a single symbol.

Module: `github.com/zhuravel/magnum` (Go 1.27). Import paths are `github.com/zhuravel/magnum/internal/<pkg>`. Every package is implemented.

## Cross-cutting conventions

- Every subprocess goes through `execx.Runner` (`Real`, `Fake`, `DryRun`). Commands that change state set `Cmd.Mutates = true`, so `--dry-run` only prints them. Packages that run subprocesses take a runner: `gitx`, `github`, `identity`, `launchd`, `reveal`, `slots`, `inventory`, `agents`, `pipeline`, `cleanup`, `notify` (osascript). `execx.Real` refuses a missing `Cmd.Dir` with a `working directory` error (code -1) before starting the process.
- `mysqlx` (database/sql), `herdr` (Unix socket) and `identity.App`'s REST calls (net/http) do not use execx, so `execx.DryRun` does not stop them. Under `daemon --dry-run` the engine therefore never calls `agents`, `pipeline.RunRound`, `notify` toasts/`Sidebar` or identity refresh, and writes no tab-bar file; `slots` and `cleanup` have their own dry-run flags (`slots.Deps.DryRun`, `cleanup.Options.DryRun`), and `app.New` opens a private `VACUUM INTO` copy of `state/magnum.db`.
- One `app.App` per process wires every layer (`app.New(cfg, layout, app.Options{...})`, then `engine.FromApp(a)` for the daemon). Non-daemon commands pass `Options.Logger`, otherwise the App logs to `state/logs/daemon.log`.
- Commands: `cli` exports only `Main` and `Context`. Every command is a cobra command built per call by `newRoot` from its own `new*Cmd(c)` constructor in its own file (unexported helpers carry a per-group prefix: `act*`, `insp*`/`status*`/`doctor*`, `daemon*`); `magnum --help` lists them by group and README.md summarizes their flags. `status --watch`, `pick`, `cleanup` and `watch` run a `tui` screen when stdin and stdout are terminals (`tui.IsTerminal`) and keep their plain-text output anywhere else and with `--json`; the cli adapts its own types into the screen's structs, and actions run after the screen exits (pick) or capture their output (the dashboard).
- CLI and daemon hand-off: mutating commands call `store.EnqueueRequest(engine.Req*, payload)` and then `engine.KickDaemon(layout)` (pid 0 = no daemon). In-process slot or cleanup work takes `engine.AcquireLock(layout.Lock())`; `held == true` means a daemon runs, so send a request instead (`slots provision|repair|adopt` → `engine.ReqProvision|ReqRepair|ReqAdopt`, `magnum open` of a parked PR → `engine.ReqOpen`). Never probe the lock.
- Per-PR repositories: the main clone is whatever `(*gitx.Client).FindClone` finds under the watch's `clone_root` (`<name>`, `<owner>-<name>`, `<owner>_<name>`, else any directory whose origin is the repository; folders without `.git` are never handed to git). The engine records it in `repos.clone_path`; per-PR worktrees live next to it at `<clone>__worktrees/pr-N` (`slots.PRWorktreePath`).
- Logs: the daemon writes and rotates `state/logs/daemon.log`; the LaunchAgent's stdout/stderr go to `state/logs/launchd.log` (`launchd.LogPath`).
- Secrets: pass free text through `execx.Redact` before logging it or storing it in events. Session env (`sessions.env_json`) holds only non-secret values.
- Timestamps written to SQLite must go through `store.FormatTime` (fixed-width, so text order is time order).
- Each package has its own sentinel errors; match them with `errors.Is`: `store.ErrNotFound`/`ErrConflict`, `github.ErrNotFound`/`ErrUnauthorized`/`ErrForbidden`/`ErrRateLimited`, `gitx.ErrNoSuchRef`/`ErrNoMergeBase`, `mysqlx.ErrGuard`/`ErrNotFound`, `herdr.ErrUnavailable`/`ErrTimeout`, plus the `Err*` values of `slots`, `agents`, `pipeline` and `cleanup` listed below.
- GitHub logins: GraphQL returns `talkable`, REST and config use `talkable[bot]`. Compare with `github.SameLogin`.
- Run rows: `agents` moves runs pending → submitted/working/failed/abandoned and submitted/working → ended; `pipeline` owns ended → verified/failed and the `codex_review` run; the engine closes out stale runs of a PR before each new round. A submitted run is observed, never re-sent.
- Completion signal: the engine calls `agents.ObserveSnapshot` exactly once per tick (each call counts one idle tick); `pipeline` only reads run and session rows.
- Magnum's herdr agents are named `mg-…` (`inventory.AgentPrefix`); `slots.Guard` ignores panes recorded in live sessions.
- Pool database drop guard: `Prefix` = the common start of `pool.databases` before `{` (`talkable_`), `AllowRegexp` = `slot_name` with `{n}` as `[0-9]+` (`^review[0-9]+$`). One helper derives it, `slots.DropGuard(pool)`, used by `slots`, `inventory` and `cleanup`.
- Event subjects (audit log): `pr:<owner>/<name>#<N>` (engine, pipeline), `pr:<store id>` (cleanup), `slot:<name>` and `slot:<name>:…` (slots, cleanup), `slot:<owner>/<name>#<N>` (per-PR slots), `slug:<slug>`, `path:<dir>`, `tool:<codex|claude>`, `identity:<name>`, `pool:<repo>`, `repo:<owner/name>`, `request:<id>`. `magnum logs <ref>` matches every PR form.
- kv keys read by `magnum status`/`doctor`: `daemon.paused|paused_reason|paused_until|started_at|last_tick|last_poll|last_reconcile|pid`, `<codex|claude>.paused_until|paused_reason|paused_detail|backoff`, `identity.<name>.check|error|tick_error|token_expiry`, `watch.<owner>.paused`, `gh.remaining|limit|reset|poll_paused_until`, `herdr.up`, plus per-PR `pr.<id>.simplify|fresh|dry_run`. Their names are `store.KV*` constants and functions, shared by the engine (which keeps its older `engine.KV*` aliases) and `cli`; never spell them as literals.
- Review pipeline: `config.Config.Roles` (`[[role]]`) and `Kinds` (`[kinds.<name>]`) define the agents of a round; read them through `RolesFor`, `JudgeFor`, `RoleByNameOrAlias`, `Stages` and `KindSpec`, never by hard-coded role names. Prompt templates live in the top-level `prompts/` directory (package `github.com/zhuravel/magnum/prompts` embeds the defaults); `Config.ResolvePrompt` reads `[pipeline] prompts_dir` first. `prompts/README.md` documents the variables.

## Packages

<!-- generated by `make api-doc` below this line; edit only the text above -->

| Package | Synopsis |
|---|---|
| [agents](#agents) | Package agents runs magnum's review agents inside herdr: one workspace per PR with a pane per configured role (config.Role: the judge plus reviewers such as claude-review, the codex-review shell and claude-simplify), agent start (fresh or resumed) on any configured kind (config.Kind: codex, claude, droid, omp, ...), prompts, per-tick completion tracking, pane-output health classification, login preflight, quit, park and crash recovery. |
| [app](#app) | Package app wires magnum's components from config.toml: the store, the subprocess runner (wrapped in execx.DryRun under --dry-run), the herdr, GitHub, MySQL and git clients, the identities, and the slots, inventory, agents, pipeline, cleanup and notify layers built on top of them. |
| [attention](#attention) | Package attention explains why a PR needs the user. |
| [cleanup](#cleanup) | Package cleanup builds and applies storage cleanup plans: releasing the slots of closed PRs, removing per-PR worktrees and surplus pool slots, dropping orphan per-worktree databases and resetting a manual worktree (talkable.repoN). |
| [cli](#cli) | Package cli wires the subcommands into a cobra command tree built per call. |
| [config](#config) | Package config loads and validates config.toml (kept in the repository). |
| [eligibility](#eligibility) | Package eligibility is magnum's pure decision logic: which pull requests a watch picks up (Classify), when a picked-up PR may start its next review round (Throttle) and whether the daemon is inside its quiet hours (QuietHours). |
| [engine](#engine) | Package engine is magnum's daemon: one tick loop that polls GitHub, observes herdr, applies health pauses, consumes CLI requests, dispatches review rounds into slots (each round in its own goroutine), releases closed PRs after their grace and reconciles the registry with disk, MySQL and herdr. |
| [eval](#eval) | Package eval scores an evaluation replay of magnum's pull request review against a corpus of pull requests with known ("seeded") defects. |
| [execx](#execx) | Package execx is the single choke point for every subprocess magnum runs (git, gh, mysql, mise exec, launchctl, osascript, codex/claude probes). |
| [fsx](#fsx) | Package fsx holds the file helpers several packages share: an atomic file write, plain or confined to an os.Root, and an existence check. |
| [github](#github) | Package github is magnum's GitHub access layer. |
| [gitx](#gitx) | Package gitx holds every git operation magnum needs: fetching PR heads into refs/magnum/pr/N, detached switches and placeholder resets in review slots, worktree management, dirty/unpushed guards and the diff queries behind "did this PR touch db/". |
| [herdr](#herdr) | Package herdr is magnum's client for the herdr terminal multiplexer's Unix-socket API (verified against herdr 0.9.x, protocol 22). |
| [identity](#identity) | Package identity provides the GitHub identities magnum acts as. |
| [inventory](#inventory) | Package inventory is the read-mostly reconciliation view: the registry (slots, assignments, sessions) compared with what is really on disk (`git worktree list` of every watched main clone), in MySQL (per-worktree databases), in herdr (agents per path) and, on request, on GitHub (states of PRs checked out in manual worktrees). |
| [launchd](#launchd) | Package launchd writes and manages magnum's LaunchAgent (label zhuravel.magnum): rendering the plist, installing it into the user's GUI domain, and querying, restarting and removing the job. |
| [learn](#learn) | Package learn is the deterministic half of the retro (DECISIONS "Learning loop: daily retro"): after a pull request magnum reviewed closes, Build turns what other reviewers said about it into candidates for a classifier, after dropping what cannot be a miss (the author's and magnum's own comments, short approvals, comments on code magnum never saw, findings magnum already posted), and ParseOutput reads the classifier's answer back, refusing anything off schema and any lesson that retells the pull request instead of teaching (ScrubLesson). |
| [mysqlx](#mysqlx) | Package mysqlx inventories and drops the per-worktree MySQL databases that Talkable's bin/worktree-setup creates (talkable_<env>[_<role>]__<slug>) on the local DBngin server. |
| [notes](#notes) | Package notes measures, versions and curates the repository notes (the file every review role reads first and the judge rewrites, engine.NotesPath) and their harness directory of QA scripts. |
| [notify](#notify) | Package notify is magnum's user-facing status surface: toasts and herdr sidebar tokens. |
| [paths](#paths) | Package paths defines magnum's on-disk layout. |
| [pipeline](#pipeline) | Package pipeline runs one review round for a PR inside its herdr workspace: a readiness step in the checkout (the repository's prepare commands and ready probes and a Ruby check, readiness.go; failures only inform the judge), then the round's configured roles (config.Role: agent sessions on any configured kind, or shell commands) in stages (config.Config.Stages: the roles of a stage in parallel, then a check that they left HEAD and the tree as they found them, restoring both when not: no role may edit the checkout), then the judge, then verification on GitHub (the oracle for "review posted") and the optional dismissal of the identity's own stale CHANGES_REQUESTED. |
| [postreview](#postreview) | Package postreview is `magnum post-review`, the judge's posting tool. |
| [reveal](#reveal) | Package reveal brings the herdr client to the front of the user's terminal: focus the existing client when there is one, otherwise open a new tab or window running `herdr --session <name>`. |
| [slots](#slots) | Package slots manages the checkouts magnum reviews in: the Talkable pool slots (~/Projects/talkable.reviewN, provisioned once with bin/worktree-setup and reused) and per-PR worktrees for small repositories, next to the repository's main clone whatever it is named (~/Projects/<owner>-<name>__worktrees/pr-N). |
| [steps](#steps) | Package steps makes multi-step side effects resumable after a crash. |
| [store](#store) | Package store is magnum's SQLite registry: repos, PRs, slots, assignments, slot databases, agent sessions, review runs, CLI requests, the audit event log, small key/value state and notification dedup. |
| [textx](#textx) | Package textx holds the small text helpers several packages share: clipping to a number of runes, the first line, a short commit SHA, plurals and login folding. |
| [tui](#tui) | Package tui holds magnum's interactive terminal screens, built on Bubble Tea: the status dashboard (RunDashboard), the PR board (RunPRBoard), the PR picker (RunPicker), the cleanup plan review (RunCleanupPlan) and the read-only pane mirror (RunWatch). |
| [usage](#usage) | Package usage reads how much of an agent CLI's subscription budget is used. |

## agents

```text
package agents // import "github.com/zhuravel/magnum/internal/agents"

Package agents runs magnum's review agents inside herdr: one workspace per PR
with a pane per configured role (config.Role: the judge plus reviewers such as
claude-review, the codex-review shell and claude-simplify), agent start (fresh
or resumed) on any configured kind (config.Kind: codex, claude, droid, omp,
...), prompts, per-tick completion tracking, pane-output health classification,
login preflight, quit, park and crash recovery.

Durable state lives in the store's sessions and runs rows; herdr is only
observed. Prompts are rendered from the configured prompt files (see
RenderPrompt and Manager.RolePrompt); PR titles and bodies are never
interpolated into them.

Agent CLIs are often launched through the user's zsh wrapper functions (herdr
types the agent command into the pane's shell), so magnum passes only the args
Kind.Argv builds; a kind's args are appended only when it is not a wrapper (see
Manager.Wrapper).

CONSTANTS

const (
	KindCodex  = config.KindCodex
	KindClaude = config.KindClaude
	KindDroid  = config.KindDroid
	KindOMP    = config.KindOMP
	KindShell  = config.KindShell // sessions.agent_kind of a shell role's pane
)
    Agent kinds (herdr agent.start --kind, sessions.agent_kind) of the built-in
    kinds; any [kinds.<name>] is a kind too.

const (
	AgentStartTimeout   = 120 * time.Second // agent.start timeout_ms
	PromptAckTimeout    = 60 * time.Second  // agent.prompt --wait --until working|blocked
	IdleShellTimeout    = 60 * time.Second  // a fresh pane's shell (oh-my-zsh + mise) must be idle by then
	QuitIdleTimeout     = 10 * time.Second  // after two ctrl+c the pane must be a shell again
	PreflightTimeout    = 30 * time.Second
	WrapperProbeTimeout = 20 * time.Second
)
    Timeouts used against herdr and the CLIs.

const (
	EventHooksTrusted  = "agents.hooks_trusted"
	EventHooksDeclined = "agents.hooks_declined"
)
    Event kinds recorded each time magnum answers a hooks review.

const (
	// DefaultModelLimitCooldown is used when daemon.model_limit_cooldown is
	// unset (zero).
	DefaultModelLimitCooldown = 5 * time.Hour
	// SwitchModelTimeout bounds the wait for an agent to be idle again after
	// the switch command.
	SwitchModelTimeout = 30 * time.Second
)
    Model switching timing.

const (
	EventModelSwitched = "agent.model_switched" // data: session, role, from, to, reason
	EventModelLimited  = "kind.model_limited"   // data: kind, model, until
	EventRebound       = "agent.rebound"        // data: session, role, agent, pane (Submit, see rebindLost)
)
    Event kinds of per-model limits, and of a lost session made live again.

const (
	SwitchLimitHit = "model_limit"   // the model hit its limit during a run
	SwitchLimited  = "model_limited" // the model was already limited (before a prompt, or at start)
	SwitchExpired  = "limit_expired" // the role's model is no longer limited: back to it
)
    SwitchModel reasons (EventModelSwitched's reason).

const (

	// NotesHarnessMax bounds the harness entries a judge prompt lists.
	NotesHarnessMax = 40
)
    Repository notes (engine.NotesPath) are rewritten by the judge of every
    round of a repository, and rounds of different PRs of one repository run at
    the same time. Three judges once rewrote the same file within four minutes,
    each from the text it had read half an hour earlier, and the result
    named one of six harness scripts. So the judge takes a lock for its
    read-merge-write: a directory created with mkdir (atomic, and it outlives
    the shell command that made it, which an flock would not: the judge reads,
    merges and writes in separate tool calls), waited for at most three minutes
    and taken over when older than ten (its judge died holding it).

const (
	// EventPromptDenied: magnum answered No to an agent's permission prompt.
	EventPromptDenied = "agent.prompt_denied"
	// EventPromptDeniedLimit: a run reached MaxPromptDenies; magnum stopped
	// answering and the agent stays blocked for the human.
	EventPromptDeniedLimit = "agent.prompt_denied_limit"
	// EventDenyContinued: an agent that went idle after a deny was sent its
	// kind's after_deny_prompt (see continueAfterDeny).
	EventDenyContinued = "agent.deny_continued"
)
    Permission-prompt events, recorded on the PR subject
    ("pr:<owner>/<name>#<N>").

const (
	// ReadinessResetDB is a [[pool]] reset_db command, run first when the
	// slot's databases carry another schema than the checkout's: it loads
	// the PR's schema into them.
	ReadinessResetDB = "reset_db"
	ReadinessPrepare = "prepare" // a [[repo]]/[[pool]] prepare command
	ReadinessReady   = "ready"   // a [[repo]]/[[pool]] ready probe (exit 0 = ready)
	ReadinessRuby    = "ruby"    // the built-in check that the login shell runs the Ruby the checkout pins
)
    Readiness check kinds (ReadinessCheck.Kind).

const (
	ReadinessOK      = "ok"
	ReadinessFailed  = "failed"  // a non-zero exit, a failed start, or (ruby) the wrong version
	ReadinessTimeout = "timeout" // the readiness budget (ready_timeout) ran out while it ran
	ReadinessSkipped = "skipped" // not run: the budget was spent, or the round was cancelled
)
    Readiness check statuses (ReadinessCheck.Status).

const (
	PhaseOwnPass    = "own_pass"   // the judge's own pass, while the reviewers work
	PhaseCandidates = "candidates" // the judge judges the reviewers' reports against its own pass
)
    Judge phases of JudgeData.Phase.

const (
	ModeInitial  = config.PromptInitial  // the role's first run for the PR
	ModeRereview = config.PromptRereview // a new head after the role's earlier run
	ModeRestart  = config.PromptRestart  // a push cut the role's turn short; the round restarted on the new head
)
    Prompt modes of RoleData.Mode.

const (
	// TrustWindow: the fallback answers a trust dialog only this soon after
	// the agent started (it is a first-launch dialog), and only before the
	// session was ever prompted.
	TrustWindow = 90 * time.Second
	// TrustReadyTimeout bounds the wait for the agent to be idle after the
	// dialog was answered.
	TrustReadyTimeout = 60 * time.Second
)
    Trust-dialog fallback timing.

const CompletionIdleTicks = 2
    CompletionIdleTicks is how many consecutive idle|done observations end a
    turn.

const EventBackgroundWait = "agent.background_wait"
    EventBackgroundWait is recorded once per run when background work holds an
    idle claude agent's run open (data: role, run, tasks).

const EventDefaultModelRestored = "agent.default_model_restored"
    EventDefaultModelRestored is recorded when SwitchModel put the Claude
    settings' default model back (data: session, role, file, model, found;
    model and found are null when the key was absent).

const EventTrustDialogAnswered = "agents.trust_dialog_answered"
    EventTrustDialogAnswered is the event kind recorded (subject
    "pr:<owner>/<name>#<N>") each time a trust dialog is answered.

const FallbackPromptName = "model-fallback.md"
    FallbackPromptName is the prompt file the continuation after a model switch
    renders (with FallbackData).

const HealthLines = 80
    HealthLines is how much pane output CheckHealth classifies.

const LostGrace = 15 * time.Second
    LostGrace keeps an observe tick from judging a session lost from a snapshot
    that may predate its agent: a session whose started_at, agent_status_at or
    live mark (StartAgent, a rebind in Submit) is after, or within LostGrace
    before, the snapshot's capture time is left for the next tick. A round once
    failed on a judge marked lost in the second it started: the tick's snapshot
    was taken before StartAgent made the session live.

const MaxPromptDenies = 10
    MaxPromptDenies caps the permission prompts magnum denies, plus the
    after-deny continuations it sends, per run: an agent that keeps asking is
    misbehaving and is left blocked.

const MaxReset = 24 * time.Hour
    MaxReset caps a usage-limit reset read from pane text: the text may quote
    or fake a far-off date, so a pause never lasts longer than this from one
    reading (a longer limit is simply hit again).

const OwnFindingsFile = "judge-own.md"
    OwnFindingsFile is the file the judge's own pass writes in the round's
    report directory (JudgeData.OwnFindings): its findings, their proofs and the
    checks it ran.

const PostReviewFile = "review.json"
    PostReviewFile is the file the judge writes its review to for
    `magnum post-review`, in the report directory next to its result file
    (JudgeData.ReviewFile).

const ShellStatusUnknown = -1
    ShellStatusUnknown is RunShell's status when the done marker carried no exit
    status (a full-line template that echoes the bare marker).

const TitleAttempts = 5
    TitleAttempts caps the rename attempts per session row (one generation;
    a restart creates a new row and a fresh budget).

const UnknownModel = "default"
    UnknownModel names the model of a session whose model magnum does not
    know (a role without model whose CLI default never named itself in a limit
    message).


VARIABLES

var (
	// ErrBlocked: the agent waits on an approval/trust dialog (herdr
	// agent_blocked, or the prompt ack came back blocked). The CLIs'
	// first-launch folder-trust dialogs are answered Yes (EnsureTrust
	// prevents them; AnswerTrustDialog is the fallback); a permission prompt
	// during a run is answered No by the observer when the kind's
	// on_permission_prompt is "deny"; nothing is ever approved otherwise.
	ErrBlocked = errors.New("agent blocked")
	// ErrStalled: herdr accepted the text but the agent never started working
	// (agent_prompt_stalled). The run is left submitted, never re-sent.
	ErrStalled = errors.New("agent prompt stalled")
	// ErrTimeout: herdr's own timeout or a client deadline.
	ErrTimeout = errors.New("agent timed out")
	// ErrLoginRequired: Preflight found the CLI logged out.
	ErrLoginRequired = errors.New("agent login required")
	// ErrBusy: an agent is working/blocked, a shell runs a command, or a run is
	// in flight, so the requested change (park, quit, move, shell command) waits.
	ErrBusy = errors.New("agent busy")
	// ErrHumanActive: a human typed into the session within daemon.human_cooldown.
	ErrHumanActive = errors.New("human active in agent pane")
	// ErrNoSession: the role has no live session (EnsureWorkspace/StartAgent first).
	ErrNoSession = errors.New("no live agent session")
	// ErrNotAgent: the role is a shell role (kind "shell", e.g. codex-review),
	// whose pane has no agent.
	ErrNotAgent = errors.New("role has no agent")
	// ErrUnknownTemplate: a prompt name exists neither in pipeline.prompts_dir
	// nor among the embedded defaults (the same sentinel as
	// config.ErrPromptNotFound).
	ErrUnknownTemplate = config.ErrPromptNotFound
)
    Sentinel errors; match with errors.Is.

var ErrNoModelSwitch = errors.New("agent kind cannot switch models")
    ErrNoModelSwitch: the session's kind has no switch_model command.


FUNCTIONS

func AgentName(repo string, number int, role Role) string
    AgentName is the herdr agent name of a PR role: "mg-<N>-<role>-<hash>" (e.g.
    mg-11920-codex-judge-1a2b3c), where repo is "owner/name" and hash is the
    first 6 hex digits of the SHA-256 of its lower-cased form. herdr agent names
    are global, so the hash keeps the same PR number of two repositories apart.
    The role is sanitized (lowercase, other characters than [a-z0-9_-]
    become "-") and trimmed when the name would exceed 32 characters;
    the hash of a trimmed role covers the role's full name too, so the name
    stays unique per (owner, repo, number, role) and always matches herdr's
    ^[a-z][a-z0-9_-]{0,31}$.

func CommandAnchor(marker string) string
    CommandAnchor is text that only the pane's echo of a shell line built with
    marker contains (the done printf's format, which the output shows with a
    number instead): pane text after it is the command's output.

func CountWorking(agents []herdr.AgentInfo, kind string) int
    CountWorking counts agents of kind (codex, claude, ...) whose status is
    working, magnum's or not (daemon.max_total_working_codex gate).

func DoneMarker(runID string) string
    DoneMarker is the line a shell role's line prints when its command finished,
    followed by the command's exit status ("<marker> 0").

func KVKindCLIModel(kind string) string
    KVKindCLIModel is the kv key holding the CLI's own default model of
    kind (what a session runs when neither its role's model nor the kind's
    default_model is set), as a limit message of such a session named it.

func KVModelLimited(kind, model string) string
    KVModelLimited is the kv key holding when kind's limit on model ends
    (store.FormatTime): "kind.<kind>.model_limited.<model>".

func KVModelLimits(kind string) string
    KVModelLimits is the kv key listing (comma-separated) the models of kind
    that have a KVModelLimited row, so status can find them.

func KVSessionModel(sessionID int64) string
    KVSessionModel is the kv key holding the model magnum switched a session row
    to (absent while the session runs its role's model).

func NewRunID(now time.Time) string
    NewRunID is a run id: "r-<UTC yyyymmddThhmmss>-<6 random hex digits>",
    sortable by creation second. The judge's review carries it as its magnum:run
    marker, and a review with the marker posted by another login is an identity
    leak, so the random part keeps outsiders from guessing it.

func NotesFiles(notesPath string) (dir, lock string)
    NotesFiles returns the harness directory and the lock of the repository
    notes file notesPath: the same path without ".md", and that with ".lock"
    (engine.NotesDir and engine.NotesLockPath). Both are "" for "".

func NotesHarness(dir string) (names []string, more int)
    NotesHarness lists the harness directory dir for a judge prompt
    (JudgeData.NotesHarness): entry names sorted, a directory with a trailing
    "/", at most NotesHarnessMax, and how many more there are. A missing or
    unreadable directory lists nothing.

func NotesLockLine(lock string) string
    NotesLockLine is the shell line a judge runs to take the notes lock (a
    directory): it prints "notes locked" once it holds it, or "notes busy" after
    three minutes. A lock older than ten minutes is removed first.

func NotesUnlockLine(lock string) string
    NotesUnlockLine is the shell line that releases the notes lock.

func PostReviewLine(d JudgeData) string
    PostReviewLine is the shell line the judge runs to post its review
    (JudgeData.PostReviewCommand, `post_review` in the <magnum> block):
    `magnum post-review` with the run's facts as flags, each value shell-quoted.
    --gh-config-dir only for an identity with its own gh config; --dry-run in
    a dry run; --local-base (the base the blind replay's diff starts at) in a
    blind one, so the tool reads nothing from GitHub.

func RenderPrompt(p config.Prompt, data any) (string, error)
    RenderPrompt executes a resolved prompt template
    (config.Config.ResolvePrompt or RolePrompt) with data: JudgeData for the
    judge's prompts, RoleData for every other session role and ShellData for
    a shell role's full-line template (values or pointers). Prompts are Go
    text/template files; a field the template needs but data lacks is an error.
    JudgeData is completed first: its Reports (see Report) and the notes
    fields NotesPath implies (NotesDir, NotesLock and the lock commands; see
    NotesFiles). ShellData values are shell-quoted when needed (see ShellLine;
    use it to build a shell role's line). Trailing newlines are trimmed:
    a prompt or typed command must not end with one (a shell line would submit
    an extra empty command).

func ReportMarker(runID string) string
    ReportMarker is the line a role's report starts with to say which run wrote
    it (`<!-- magnum:run=<runID> -->`, an HTML comment Markdown does not show):
    report paths are per head, not per run, so a reviewer that kept working
    after its run ended can write where a later run on the same head looks.
    The reviewer prompts ask for it (RoleData.RunID), and a shell role's line
    with capture "stdout" prints it before the output it tees into the report
    (ShellLine).

func ReportRun(report []byte) (runID string, rest []byte)
    ReportRun reads the run a report's ReportMarker names, on its first line
    that is not blank (a byte-order mark aside), and returns the report after
    that line; a report whose first line is no marker returns "" and itself.

func SameModel(a, b string) bool
    SameModel reports whether two model names mean the same model as limit
    messages and configs spell them: equal ignoring case, or one is a word of
    the other ("opus" and "claude-opus-4-5"). "" means UnknownModel.

func ShellLine(role config.Role, d ShellData) (string, error)
    ShellLine builds the one-line command typed into a shell role's pane (see
    RunShell). With role.Command set (the role's command template overridable by
    d.Command) the line is

        [printf '\033]0;%s\007' <Title>; DISABLE_AUTO_TITLE=true; ]set -o pipefail; [{ printf '<ReportMarker(RunID)>\n'; ]<command> <args...>[; } | tee <ReportPath>]; printf '\n<Marker> %d\n' "$?"

    where <command> is the command template executed with d (every value
    shell-quoted, e.g. `command codex review --base {{.BaseSHA}}`), the args
    (role.Args, or d.Args) are shell-quoted and appended, and the braces and
    the tee are there only for capture "stdout" (ReportPath is then required):
    the report starts with the run's ReportMarker, then the command's output.
    The optional prefix sets the pane's terminal title (OSC 0), which herdr's
    sidebar shows, and stops oh-my-zsh from resetting it after the command
    (its title hooks honour DISABLE_AUTO_TITLE at run time). pipefail makes
    the status the command's, not tee's; the final printf puts the marker on
    a line of its own (also after output without a trailing newline) followed
    by that status, which RunShell returns (config.Role.StatusOK judges it).
    With role.Prompt set instead, that full-line .sh template (d.Template,
    else the embedded default of that name) is rendered with d and must contain
    the marker (prompts/codex-review.sh shows the same ending). Marker must
    match [A-Za-z0-9._-]+; trailing newlines are trimmed.

func TaggedAgentName(tag string, repo string, number int, role Role) string
    TaggedAgentName is AgentName for agents started under a tag (Deps.Tag,
    "eval" for magnum eval): the tag joins the hashed key, so a replay of a PR
    never shares, adopts or collides with the agents of the PR's own rounds.
    An empty tag is AgentName.

func TaggedPaneLabel(tag string, number int, r Role) string
    TaggedPaneLabel is the herdr pane name for a role under a tag (Deps.Tag):
    "PR #N <role>", e.g. "PR #729 codex-judge" (TaggedTitle without a repo).

func TaggedTitle(tag, repo string, number int, role Role) string
    TaggedTitle is Title under a tag (Deps.Tag): "eval PR #729 codex-judge -
    talkable". An empty tag is Title.

func Title(repo string, number int, role Role) string
    Title is the terminal title of a PR role's pane, which herdr's agents
    sidebar shows (terminal_title_stripped): "PR #<N> <role> - <repo>", e.g.
    "PR #729 codex-judge - talkable", "PR #729 claude-review - talkable". repo
    is the name part of "owner/name" (or a bare name); without one the title is
    the pane label "PR #<N> <role>".

    A kind with name args gets it at launch (claude --name, see StartAgent),
    a kind with only a rename command through that command typed while it works
    (codex /rename, see nameAgent), and a shell role from the OSC 0 prefix of
    its command line (ShellData.Title).


TYPES

type Deps struct {
	Herdr  Herdr
	Store  *store.Store
	Runner execx.Runner // wrapper probe and preflight (read-only commands)
	Config *config.Config
	Layout paths.Layout
	Clock  func() time.Time // nil = time.Now
	// Sleep waits between polls (quit, trust-dialog readiness); nil = a
	// timer honouring ctx.
	Sleep func(ctx context.Context, d time.Duration) error

	// CodexConfig and ClaudeConfig are the CLI config files EnsureTrust
	// edits; "" = the CLIs' defaults ($CODEX_HOME/config.toml or
	// ~/.codex/config.toml; $CLAUDE_CONFIG_DIR/.claude.json or
	// ~/.claude.json). A test binary never falls back to the defaults.
	CodexConfig  string
	ClaudeConfig string
	// ClaudeSettings is the Claude Code settings file whose default model
	// SwitchModel restores after a /model switch; "" = the CLI's default
	// ($CLAUDE_CONFIG_DIR/settings.json or ~/.claude/settings.json). A test
	// binary never falls back to the defaults.
	ClaudeSettings string
	// ClaudeDir is Claude Code's config directory, whose projects/ holds
	// the session transcripts the observer reads for background work (see
	// backgroundWait); "" = $CLAUDE_CONFIG_DIR, else ~/.claude. A session
	// whose pane env sets CLAUDE_CONFIG_DIR uses that. A test binary never
	// falls back to the defaults.
	ClaudeDir string
	// Log receives one line per trust entry added and per trust dialog
	// answered (optional).
	Log execx.Logger
	// Tag marks agents started outside the PR's own rounds ("eval" for
	// magnum eval): it joins their names' hash (TaggedAgentName) and leads
	// their titles and pane labels (TaggedTitle). "" = the PR's own agents.
	Tag string
}
    Deps are the Manager's collaborators.

type FallbackData struct {
	Model      string // the model the session runs now
	Previous   string // the model that hit its limit
	Role       string
	URL        string
	HeadSHA    string
	ReportPath string // the role's report or result file; "" = the prompt names none
}
    FallbackData feeds the model-fallback prompt.

type Health struct {
	Kind    HealthKind
	ResetAt *time.Time // usage_limit and model_limit, when the text names a reset time
	// Model is the limited model a model_limit pattern captured (its "model"
	// group, lower case); "" = the session's current model.
	Model  string
	Detail string // the matching line, redacted, at most 300 bytes
}
    Health is the classifier's verdict.

func ClassifyAt(text string, now time.Time) Health
    ClassifyAt is ClassifyWith with config.DefaultHealthPatterns.

func ClassifyWith(rx config.HealthRegexps, text string, now time.Time) Health
    ClassifyWith classifies recent pane output of an agent session with a
    kind's compiled health patterns (config.HealthPatterns.Compile) plus
    the built-in trust-dialog, approval and stalled patterns. The match on
    the latest line wins (pane text keeps old errors above newer output);
    on one line login_required > model_limit > usage_limit > trust_dialog
    > blocked > overloaded > stalled. A model_limit match sets Model from
    the pattern's "model" group. For usage_limit and model_limit, ResetAt
    comes from "try again at 3:45 PM", "try again at Oct 5th, 2026 9:05 AM",
    "try again in 1 day 4 hours", "resets 5pm (Europe/Kyiv)", "resets Oct 7,
    9am" or "limit reached|<epoch>", read relative to now (bare clock times
    in now's zone unless the text names one; a time more than 12h past means
    tomorrow) and capped at MaxReset ahead. No match is HealthOK.

type HealthKind string
    HealthKind classifies an agent pane's recent output.

const (
	HealthOK            HealthKind = "ok"
	HealthLoginRequired HealthKind = "login_required" // pause the kind; e.g. `codex login` / `claude auth login`
	HealthModelLimit    HealthKind = "model_limit"    // one model's cap: switch the session to a fallback model (Model, ResetAt)
	HealthUsageLimit    HealthKind = "usage_limit"    // pause until ResetAt (fallback 1h, doubling)
	HealthOverloaded    HealthKind = "overloaded"     // 5xx/429/reconnecting: retry with backoff
	HealthStalled       HealthKind = "stalled"        // turn interrupted / context full
	HealthBlocked       HealthKind = "blocked"        // approval (or other trust) dialog: needs the human
	HealthTrustDialog   HealthKind = "trust_dialog"   // Codex/Claude first-launch folder trust: AnswerTrustDialog
)
    Health kinds.

type Herdr interface {
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
	WorkspaceCreate(ctx context.Context, o herdr.WorkspaceCreateOptions) (herdr.WorkspaceCreated, error)
	WorkspaceClose(ctx context.Context, workspaceID string) error
	PaneSplit(ctx context.Context, paneID string, o herdr.SplitOptions) (herdr.Pane, error)
	PaneRename(ctx context.Context, paneID, label string) error
	PaneRun(ctx context.Context, paneID, command string) error
	PaneWaitOutput(ctx context.Context, paneID string, o herdr.WaitOutputOptions) (herdr.OutputMatch, error)
	PaneRead(ctx context.Context, paneID string, o herdr.ReadOptions) (herdr.ReadResult, error)
	PaneGet(ctx context.Context, paneID string) (herdr.Pane, error)
	PaneSendKeys(ctx context.Context, paneID string, keys ...string) error
	PaneProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	WaitIdleShell(ctx context.Context, paneID string, timeout time.Duration) (herdr.ProcessInfo, error)
	AgentStart(ctx context.Context, o herdr.AgentStartOptions) (herdr.AgentStarted, error)
	AgentPrompt(ctx context.Context, target, text string, wait *herdr.PromptWait) (herdr.AgentInfo, error)
	AgentRead(ctx context.Context, target string, o herdr.ReadOptions) (herdr.ReadResult, error)
	AgentRename(ctx context.Context, target, name string) error
	AgentSendKeys(ctx context.Context, target string, keys ...string) error
}
    Herdr is the subset of *herdr.Client this package uses.

type JudgeData struct {
	// Readiness is what magnum's prepare/ready probes found before the
	// reviewers (status and reason only; command output stays in
	// readiness.json because it is PR text).
	Readiness Readiness

	RunID         string
	Owner, Repo   string
	Number        int
	URL           string
	HeadSHA       string
	BaseRef       string
	BaseSHA       string
	Checkout      string // absolute checkout path the judge works in
	IdentityKind  string // gh | app
	ReviewerLogin string // REST form, e.g. talkable[bot]
	GhConfigDir   string // "" for the gh identity
	// NoFindingsEvent is APPROVE or COMMENT: COMMENT whenever a reviewer
	// of the round left no usable report (pipeline.JudgeEvents).
	NoFindingsEvent string
	BlockingEvent   string // REQUEST_CHANGES | COMMENT
	SelfAuthored    bool
	Reports         []Report // one per non-judge role of the round, in pipeline order
	ResultFile      string   // <report dir>/<the judge's output>, e.g. codex-judge.json
	// Magnum is the magnum executable the judge's post_review line runs
	// (the daemon's own, absolute; "" = magnum on PATH). ReviewFile is
	// where the judge writes the review it posts (<report dir>/review.json,
	// derived from ResultFile when empty), and PostReviewCommand the line
	// that posts it, rendered as `post_review` (PostReviewLine; always
	// derived).
	Magnum, ReviewFile, PostReviewCommand string
	DryRun                                bool
	// Blind: an evaluation replay (pipeline.RoundInput.Blind), rendered as
	// `blind: true`; the skill then judges the local diff of HeadSHA only.
	Blind bool
	// PostMerge: GitHub merged the PR before magnum reviewed HeadSHA
	// (pipeline.RoundInput.PostMerge), rendered as `post_merge: true` only
	// then; the skill posts a COMMENT that asks for follow-ups, and
	// NoFindingsEvent and BlockingEvent are both COMMENT.
	PostMerge     bool
	SkillPath     string // the role's skill, absolute
	Model, Effort string // the judge role's model and this round's effort (config.Role.EffortFor)
	// EffortInPrompt: the agent's kind sets the effort only at launch (its
	// effort args) and this round's Effort differs from the role's, so a
	// running session cannot switch to it; the prompt asks for it in words.
	EffortInPrompt bool
	// NotesPath is the repository notes file: the judge reads it first and
	// rewrites it at the end of a round that taught something durable. ""
	// = no notes, and the prompts leave the notes fields out. The judge
	// prompts pass it and the fields below as `notes`, `notes_dir`,
	// `notes_harness`, `notes_lock` and `notes_unlock` in the <magnum>
	// block; the steps are the skill's.
	NotesPath string
	// NotesDir is the harness directory next to NotesPath and NotesLock the
	// lock the judge holds while it rewrites the notes (NotesFiles; derived
	// from NotesPath when empty). NotesLockCommand and NotesUnlockCommand
	// are the shell lines that take and release it (NotesLockLine,
	// NotesUnlockLine; always derived).
	NotesDir, NotesLock                  string
	NotesLockCommand, NotesUnlockCommand string
	// NotesHarness names the harness directory's entries (sorted; a
	// directory ends in "/"), at most NotesHarnessMax of them, so the judge
	// sees scripts the notes no longer mention; NotesHarnessMore counts the
	// rest.
	NotesHarness     []string
	NotesHarnessMore int

	// Re-review / recovery.
	PreviousReviewID int64
	PreviousEvent    string
	PreviousHeadSHA  string
	Since            string // RFC3339: read every comment since then
	ForcePushed      bool
	// PreviousHeadShort is PreviousHeadSHA cut to 7 characters (always
	// derived; see textx.ShortSHA).
	PreviousHeadShort string
	// BaseMerged (rereview): the commits since PreviousHeadSHA merged a
	// branch in, usually the base, so PreviousHeadSHA..HeadSHA carries its
	// commits: the prompt compares the PR's own diff before and after.
	BaseMerged bool
	// DeltaCheck (rereview, recovery): the round is a delta check: the judge
	// alone reviews the commits since its last review, DeltaLines changed
	// code lines in the files DeltaFile lists (a JSON file in the report
	// directory, "" when it could not be written: the file names are PR
	// content); in a recovery its fresh session first reads its previous
	// review and threads. Rendered as `delta_check: true` and one
	// instruction, only then.
	DeltaCheck      bool
	DeltaLines      int
	DeltaFile       string
	MovedFrom       string // previous checkout path when the PR changed slots
	PreviousReviews []PreviousReview
	// Threads are the inline threads the reviewer login started on the PR,
	// each reply classified (re-review). Replies are PR content, so no
	// prompt prints them: magnum writes Threads to ThreadsFile and a prompt
	// names that file and ThreadSummary.
	Threads []ReviewThread
	// ThreadsFile is the JSON file holding Threads; "" = magnum did not read
	// the threads, and the judge reads the replies itself.
	ThreadsFile string
	// ThreadSummary counts Threads and their replies by class, e.g. "3
	// threads (1 resolved, 1 outdated); replies: 1 fixed, 1 not a bug; 1
	// thread without a reply".
	ThreadSummary string
	// FormerLogins are the logins (REST form) this PR's earlier reviews were
	// posted as before magnum moved it to ReviewerLogin (an identity
	// migration): their reviews and threads are the judge's own history,
	// while every new write goes as ReviewerLogin. Empty for most PRs.
	FormerLogins []string

	// Mode is the round's kind (initial, rereview or recovery), which the
	// own-pass prompt renders as `mode` (each other judge prompt serves one
	// kind and names it itself).
	Mode string
	// Phase is the judge's phase of a round that prompts it twice
	// ([pipeline] judge_own_pass = "parallel"): PhaseOwnPass for its own
	// pass, prompted with the reviewers, PhaseCandidates for judging their
	// reports once both ended; "" for a round with one judge prompt.
	// Rendered as `phase` only when set.
	Phase string
	// OwnFindings is the file the own pass writes (OwnFindingsFile in the
	// report directory) and the candidates phase starts from, rendered as
	// `own_findings`; "" outside a two-phase round. OwnFindingsMissing
	// (candidates phase): the own pass left no such file, or an empty one,
	// so the prompt asks for the pass now.
	OwnFindings        string
	OwnFindingsMissing bool
	// RestartedFrom (own pass): a push cut the judge's own pass on this
	// head short and the round restarted on HeadSHA; the prompt asks it to
	// reuse what still applies.
	RestartedFrom string
	// RelatedPRs is related.json in the report directory: the repository's
	// other open PRs, and those merged lately, that change the same paths
	// (numbers, URLs, heads, the shared paths and magnum's reviews of them;
	// never PR text), rendered as `related_prs`; "" when there is none, and
	// always in a blind replay.
	RelatedPRs string
}
    JudgeData feeds every judge prompt (judge-*.md). Fields a template does not
    use may stay zero.

type Manager struct {
	// Has unexported fields.
}
    Manager drives agent sessions. Safe for concurrent use: the daemon tick
    (Observe) and per-round goroutines (Prompt, RunShell) share it.

func New(d Deps) *Manager
    New returns a Manager.

func (m *Manager) AnswerTrustDialog(ctx context.Context, s store.Session) (bool, error)
    AnswerTrustDialog is the fallback for an agent stopped at its CLI's
    first-launch folder-trust dialog (EnsureTrust normally prevents it). It
    acts only within TrustWindow of the session's start (StartedAt) and before
    the session was ever prompted (LastPromptAt), and only when the pane's
    visible screen shows the whole dialog of the session's agent kind (see
    detectTrustDialog) with the cursor on one of its options: Codex ("Folder
    access", "Trust this folder?", "Trust and continue" / "Quit"): Enter;
    Claude ("Accessing workspace:", "No, exit" / "Yes, I trust this folder"):
    Down (or Up) to the trust option, confirmed by a re-read, then Enter. It
    records an EventTrustDialogAnswered event and waits up to TrustReadyTimeout
    for the agent to be idle. It reports whether it answered; any other dialog
    or screen, a shell role and any kind other than codex and claude (whose
    trust handling is unknown) are left untouched (false, nil).

func (m *Manager) BackgroundTasks(ctx context.Context, run store.Run) (int, bool)
    BackgroundTasks counts the background work the claude agent of run's session
    started during run and left running, as its transcript shows now (ok false:
    not a claude session, or no transcript to read).

func (m *Manager) CheckHealth(ctx context.Context, s store.Session) (Health, error)
    CheckHealth classifies the session's recent pane output with the health
    patterns of its kind ([kinds.<kind>] health_patterns; a shell role's Tool,
    the defaults when it has none) and the manager's clock.

func (m *Manager) EnsurePane(ctx context.Context, pr store.PR, ws Workspace, slotPath string, env map[string]string, role config.Role) (Workspace, error)
    EnsurePane adds a pane for role to ws unless a live session of the role
    already has a pane in that workspace (e.g. claude-simplify, which runs on
    demand and is not in EnsureWorkspace's roles). A new pane is laid out as
    the next pane of ws.Roles (see EnsureWorkspace), gets env with the role's
    env laid over it, a starting sessions row and the "PR #N <role>" name;
    start an agent role with StartAgent. The returned Workspace is a copy of ws
    with the pane (ws itself is not modified).

func (m *Manager) EnsureTrust(ctx context.Context, kind, dir string) error
    EnsureTrust records dir as trusted in the agent CLI's own config, so a
    fresh agent started there does not stop at its first-launch trust dialog
    (which blocks every prompt with agent_blocked). StartAgent calls it before
    agent.start; slots/engine may call it when they prepare a checkout.

      - codex: [projects."<path>"] trust_level = "trusted" in the Codex
        config (Deps.CodexConfig, else $CODEX_HOME/config.toml, else
        ~/.codex/config.toml) for dir and for its repository root (the main
        clone: the parent of `git -C dir rev-parse --path-format=absolute
        --git-common-dir` when that ends in /.git), which is where Codex
        applies the trust. A missing table is appended as two lines; a table
        without trust_level gets the line after its header; an explicit other
        trust_level is left alone. The file is otherwise untouched.
      - claude: projects["<dir>"].hasTrustDialogAccepted = true in the Claude
        config (Deps.ClaudeConfig, else $CLAUDE_CONFIG_DIR/.claude.json,
        else ~/.claude.json), merged into an existing project object or created
        as {"allowedTools":[],"hasTrustDialogAccepted":true}. Every other key
        and the key order are kept; the file stays indented (two spaces) when it
        was, compact otherwise. A missing file is left missing (Claude has not
        been set up there).

    The symlink-resolved form of each path is trusted too when it differs.
    Writes are atomic (temp file + rename, keeping the file mode, through a
    symlinked config to its target) and happen only when an entry is missing,
    with one log line naming what was added. In a test binary the default paths
    are never used: without Deps.CodexConfig / Deps.ClaudeConfig EnsureTrust
    does nothing.

    Trusting magnum's own checkouts (and, for codex, the repository root, i.e.
    the user's main clone) is deliberate, and the entries are never removed:
    the agents work there like the user's own sessions do, project settings
    included, and a round runs the PR's code anyway (setup hooks, the judge's
    specs); watches skip cross-repository PRs by default.

    Every other kind (shell, droid, omp, any user-declared kind) is a no-op that
    returns nil: how droid and omp record trusted folders, and whether they ask
    at all, is unknown, so magnum neither edits their config nor answers their
    dialogs (see AnswerTrustDialog); a blocked start or prompt is left for the
    human.

func (m *Manager) EnsureWorkspace(ctx context.Context, pr store.PR, slotPath string, env map[string]string, label string, roles []config.Role) (Workspace, error)
    EnsureWorkspace returns the PR's workspace with a pane per role in roles
    (the watch's roles that need a pane now, e.g. Config.RolesFor minus the
    on-demand ones), creating what is missing:

      - The workspace of the PR's newest starting/live/lost session is reused
        while herdr still has it and its checkout equals slotPath. Live panes
        are kept wherever they are; a missing role pane is re-split (a lost
        session's surviving pane is reused).
      - If that workspace belongs to another checkout, the PR is parked first
        (ErrBusy while an agent works) and MovedFrom is set.
      - Otherwise a new workspace is created (cwd slotPath, label, no focus):
        its root pane serves the judge (the first role when none is a judge)
        and every other role gets a split, in the given order, laid out in two
        columns: each pane joins the column with fewer panes, the right one on a
        tie (the judge heads the left column). The first other role splits right
        of the judge, the second down from the first (the classic codex-judge |
        claude-review over codex-review), the third below the judge, the fourth
        down the right column again, and so on.

    Every pane gets env with the role's env (Config.RoleEnv) laid over it. Every
    newly assigned pane gets a sessions row (state starting, agent name/kind or
    agent_kind "shell", pane/workspace/tab ids, cwd, env_json) and is renamed
    "PR #N <role>" (TaggedPaneLabel); a new workspace whose root pane cannot
    be recorded is closed again (nothing else would find it). Live rows whose
    pane is gone are marked lost first, keeping their session_id for ResumeID.
    Pane envs are stored in sessions.env_json, so they must not carry secrets
    (identity.Source.Env never does).

func (m *Manager) FallbackModel(ctx context.Context, s store.Session, tried []string) (string, bool)
    FallbackModel is the model session s switches to next: the first of its
    kind's fallback_models that is not limited, not the session's current model
    and not among tried. ok is false when the kind cannot switch (switch_model
    unset) or every fallback is used up.

func (m *Manager) FallbackPrompt(d FallbackData) (string, error)
    FallbackPrompt renders FallbackPromptName from pipeline.prompts_dir or the
    embedded defaults.

func (m *Manager) LatestRound(ctx context.Context, prID int64) (int, error)
    LatestRound is the highest run round of a PR (0 when it has no runs).

func (m *Manager) NewRun(ctx context.Context, pr store.PR, role Role, kind string, round int) (store.Run, error)
    NewRun inserts a pending run for pr's role (a configured role name or
    alias; the run gets the name): id = NewRunID, target_sha = pr.HeadSHA,
    prev_reviewed_sha = pr.ReviewedSHA, identity = pr.Identity, reviewer_login
    from the identity config, report_path = Layout.ReviewDir(...)/<the
    role's ReportFile> (OwnFindingsFile for the judge's own pass, kind
    store.RunOwnPass), session_id = the role's live session when there is one.
    Create the run first when the prompt must quote its id (judge templates),
    then Submit.

func (m *Manager) NoteModelLimit(ctx context.Context, s store.Session, h Health) (ModelLimit, error)
    NoteModelLimit records that the model of session s hit its own limit (h,
    a HealthModelLimit verdict on its pane): h.Model, else the session's current
    model, is limited until h.ResetAt, else for daemon.model_limit_cooldown,
    never shortening a later recorded end. A session on its CLI's default model
    also teaches magnum which model that default is (KVKindCLIModel). It appends
    a kind.model_limited event.

func (m *Manager) Observe(ctx context.Context) ([]Observation, error)
    Observe takes one herdr snapshot and runs ObserveSnapshotAt with the time
    it was taken. The daemon calls ObserveSnapshotAt itself, once per tick,
    with the snapshot that tick also counts working agents from (polling is the
    completion signal).

func (m *Manager) ObserveSnapshot(ctx context.Context, snap herdr.Snapshot) ([]Observation, error)
    ObserveSnapshot is ObserveSnapshotAt with the capture time now (a snapshot
    of unknown age).

func (m *Manager) ObserveSnapshotAt(ctx context.Context, snap herdr.Snapshot, capturedAt time.Time) ([]Observation, error)
    ObserveSnapshotAt updates every starting/live session from snap, taken at
    capturedAt (read the clock before herdr's snapshot call), and returns one
    Observation per session:

      - agent found (by name, else by pane): status/status_at stored; idle|done
        increments idle_ticks, working|blocked resets it; a newly reported
        agent_session.value becomes session_id; a starting session becomes
        live. working + a submitted run -> run working (working_seen_at).
        idle_ticks >= CompletionIdleTicks + a submitted/working run -> run
        ended, ObsCompleted. working with no pending/submitted/working
        run (outside a short grace after start/prompt) -> ObsHumanActive,
        unless the agent is a claude agent whose transcript shows a task
        notification began the turn (background work of an earlier run resumed
        it; see notificationTurn). blocked -> ObsBlocked, or ObsPromptDenied
        when a pending/submitted/working run is in flight, the kind's
        on_permission_prompt is "deny" and the screen shows a permission prompt,
        which is answered No (see answerPermission; never without a run in
        flight, where the human may be driving the agent, so an agent that
        resumes working after a deny is never human_active). idle after such
        a deny, with that run still in flight and the agent not seen working
        since -> the kind's after_deny_prompt is sent (see continueAfterDeny),
        ObsDenyContinued, and the run does not end. A claude agent idle with a
        submitted/working run whose transcript shows background work started
        during the run still running, or a task notification not answered yet,
        counts as working for completion (idle_ticks 0, Background set; see
        backgroundWait), until the pipeline's TimeUp tells it to stop waiting
        for that work. An agent of a kind named by a rename command (codex),
        working on a submitted/working run, whose terminal title lacks Title
        gets that command (`/rename <Title>`) typed into its pane (at most once
        per call, TitleAttempts per session row; see nameAgent).
      - live agent session without its agent, or any session whose pane is gone
        -> session lost, ObsLost. A starting session (agent not started yet)
        with its pane still present is left alone, and so is a session that
        started or became live too close to capturedAt (LostGrace).

func (m *Manager) Park(ctx context.Context, pr store.PR) error
    Park releases a PR's panes while keeping its conversations resumable.
    It refuses with ErrBusy while a run of the PR is pending/submitted/working,
    an agent is working or blocked, a shell role's pane runs a command, or any
    pane of a workspace it would close runs something other than an idle shell
    or an idle agent (a lost session's pane where the user started a build,
    say). Session ids visible in herdr are recorded first. A workspace of the
    PR's live or lost sessions that holds only the PR's own panes is closed;
    one with foreign panes (the user split it) stays, and only the PR's live
    agents are quit. Live sessions with a session_id become parked, the rest
    closed (see Quit); lost sessions stay lost. No live or lost sessions: no-op.

func (m *Manager) Preflight(ctx context.Context, kind string) error
    Preflight checks that the agent CLI of kind is logged in before a start or
    prompt, with the kind's read-only login_check command (split on spaces,
    run without a shell, with the kind's env laid over the daemon's) judged
    by its login_ok rule (config.Kind.LoggedIn): codex `codex login status`
    must print "Logged in" (on either stream; its exit code is unreliable),
    claude `claude auth status` JSON must have "loggedIn": true. A kind without
    login_check (droid, omp) and the shell kind are not checked. A logged-out
    CLI wraps ErrLoginRequired; a CLI that could not run (missing binary,
    timeout) returns that error instead; output the rule cannot read (a format
    change) is logged and taken as logged in, so it never blocks the reviews.
    An undeclared kind is an error. PreflightRole checks with a role's own
    environment.

func (m *Manager) PreflightRole(ctx context.Context, role config.Role) error
    PreflightRole is Preflight for the agent kind of role
    (config.Role.AgentKind: a shell role's tool) with the environment the role's
    pane gets (config.Config.RoleEnv: the kind's env, then the role's), so a
    role that points its CLI at another account (CODEX_HOME, CLAUDE_CONFIG_DIR)
    is checked in that account.

func (m *Manager) Prompt(ctx context.Context, pr store.PR, role Role, kind, text string) (string, error)
    Prompt creates a run of kind (store.RunInitial, RunRereview, RunContinue,
    RunNudge, RunRecovery) in the PR's latest round (1 when none) and submits
    text to the role's live agent (see Submit). It returns the run id, also
    when Submit fails after the run was created; a run refused before sending
    (ErrNoSession, ErrHumanActive) is marked abandoned. Start a new round,
    or quote the run id in the text, with NewRun + Submit.

func (m *Manager) Quit(ctx context.Context, s store.Session) error
    Quit stops a session's agent the way a human would: ctrl+c, 1 s, ctrl+c,
    then waits up to QuitIdleTimeout for the pane to be an idle shell again
    (ErrBusy otherwise, session unchanged). The session id is captured first
    (RecordSessionID); the session becomes parked when it is resumable (an
    agent session with a session id whose kind reports ids), else closed.
    A pane or agent that is already gone counts as quit. For a shell role's pane
    this interrupts the running command.

func (m *Manager) ReadRecent(ctx context.Context, s store.Session, lines int) (string, error)
    ReadRecent returns the last lines of a session's pane output (unwrapped
    text): agent.read by agent name, falling back to pane.read (shell panes,
    agents that exited).

func (m *Manager) RecordSessionID(ctx context.Context, s store.Session) (string, error)
    RecordSessionID reads the session's pane and stores agent_session.value as
    session_id when herdr reports one (after the first prompt) and it differs.
    It returns the id now known ("" before the first prompt).

func (m *Manager) Recover(ctx context.Context, pr store.PR) ([]Recovered, error)
    Recover reconciles a PR's sessions with herdr after a daemon or herdr
    restart, before anything is started (herdr may restore Codex itself): live
    rows whose agent is gone are rebound to the pane whose agent_session.value
    equals their session_id, else marked lost; for agent roles without a live
    row (every role with an agent session row of the PR, in order of first
    appearance), the newest resumable parked/lost session of the role's current
    kind (see ResumeID) whose id herdr shows in a pane becomes live there.
    Rebound agents are renamed to the role's agent name. Starting rows whose
    pane still exists are left alone. Runs are not touched: submitted runs stay
    observed, never re-sent.

func (m *Manager) ResumeID(ctx context.Context, prID int64, role Role) (string, error)
    ResumeID is the session id to resume for a PR role: the newest parked or
    lost session of that role that recorded one and ran the role's current
    agent kind ("" when none, and always "" for a shell role or a kind with
    session_source "none"; a conversation of the role's previous kind is never
    resumed by another CLI).

func (m *Manager) RolePrompt(role config.Role, promptKind string, data any) (string, error)
    RolePrompt resolves the role's prompt of a prompt kind
    (config.PromptInitial, PromptRereview, PromptContinue, PromptRecovery,
    PromptNudge, PromptStop; see config.Role.PromptFile) in pipeline.prompts_dir
    or the embedded defaults and renders it with data (see RenderPrompt).
    A role without such a prompt, or a name that resolves nowhere, wraps
    ErrUnknownTemplate (config.ErrPromptNotFound).

func (m *Manager) RunShell(ctx context.Context, pr store.PR, role config.Role, paneID, line, marker string, timeout time.Duration) (int, error)
    RunShell types line (see ShellLine) into the pane of a shell role and blocks
    until the done marker (DoneMarker(runID)) appears on a line of its own,
    optionally followed by the command's exit status, or timeout (ErrTimeout).
    It returns that status (ShellStatusUnknown without one). The pane must be an
    idle shell (ErrBusy otherwise); like Submit it refuses with ErrHumanActive
    inside daemon.human_cooldown after a human typed into the PR's panes. The
    shell's line editor is cleared (ctrl+u) first, so text a human left there
    never joins the command. The match is a line-anchored regex on unwrapped
    output, so the echoed command line itself never matches. The role's session
    (agent_kind "shell") becomes live with last_prompt_at; the run row is the
    caller's. An agent role is refused with an error.

func (m *Manager) ShellLine(role config.Role, d ShellData) (string, error)
    ShellLine is ShellLine with the role's full-line template (role.Prompt)
    resolved through the configuration (pipeline.prompts_dir, then the embedded
    defaults) unless d.Template is set.

func (m *Manager) StartAgent(ctx context.Context, pr store.PR, role config.Role, paneID, resume string) error
    StartAgent launches (or adopts) the agent of an agent role in paneID and
    marks its session live. The role's kind (role.AgentKind(), a declared
    [kinds.<name>]) says how:

      - An agent already carrying the role's name is adopted as is, when it
        is of the role's kind and its pane works in the PR's checkout (the
        session's cwd, else paneID's); otherwise StartAgent fails, since the
        name is taken.
      - With resume set (ignored for a kind with session_source "none"),
        a pane whose agent_session.value equals resume (herdr restored it) is
        adopted instead of starting a second copy, and the agent is renamed to
        the role's name so the conversation is never forked; a restored agent of
        another kind or checkout is not adopted (logged) and the conversation is
        resumed in paneID instead.
      - Otherwise it waits for an idle shell (ErrBusy after IdleShellTimeout),
        resolves the kind's wrapper mode (Wrapper), trusts the session's
        cwd in the CLI's config (EnsureTrust; a failure is only logged;
        a no-op for kinds other than codex and claude) and runs agent.start with
        Name=AgentName, Kind=the kind, Timeout AgentStartTimeout and the args
        Kind.Argv builds: resume args, name args with Title (claude --name;
        kinds without name args but with a rename command are named later,
        by ObserveSnapshot), the role's model and effort args, the kind's
        start args, its args only when it is not a wrapper, then role.Args.
        While the role's model is limited (NoteModelLimit), the model args name
        the kind's first fallback model that is not, and the session records it
        (KVSessionModel, an agent.model_switched event).
      - An agent that comes up blocked (agent.start agent_not_ready, timeout,
        or status blocked, also when adopted) gets the trust-dialog fallback:
        within TrustWindow of its start and before its first prompt,
        a Codex/Claude first-launch trust dialog on screen is answered and the
        agent awaited until idle (see AnswerTrustDialog); any other dialog is
        left for the human.

    herdr's agent.start takes no environment: the role's env (Config.RoleEnv)
    reaches the agent through its pane, which EnsureWorkspace or EnsurePane
    created with it.

    The session row (created by EnsureWorkspace, or here if missing) gets the
    pane, agent name, agent_kind = the kind, session_id/resumed_from = resume,
    started_at = the launch (an agent launched here) and state live. A fresh
    session's id appears only after the first prompt; Prompt, Observe and
    RecordSessionID record it then. Preflight is the caller's job.

func (m *Manager) Submit(ctx context.Context, run store.Run, text string) error
    Submit sends text to the live agent of run's role and records the
    outcome. The run must be pending (a run once submitted is observed,
    never re-sent: store.ErrConflict). It refuses with ErrHumanActive inside
    daemon.human_cooldown after a human typed into the session, and with
    ErrNoSession unless the role's session is live; both leave the run pending
    for the caller to retry or abandon. A role whose newest session is lost
    while a fresh herdr snapshot still shows its agent gets that session back
    live (rebindLost) instead of a refusal. Before sending, a session on a
    limited model switches to a fallback, and one magnum switched earlier
    switches back once its role's model is no longer limited (ensureModel). The
    prompt waits for the agent to reach working or blocked (PromptAckTimeout):

      - working: run working (submitted_at, working_seen_at).
      - blocked after submission: run submitted, ErrBlocked.
      - herdr agent_blocked (rejected before sending): run failed, ErrBlocked.
        Within TrustWindow of the session's start and before its first
        prompt, a first-launch trust dialog on screen is answered first
        (AnswerTrustDialog) and the prompt re-sent once.
      - agent_prompt_stalled: run submitted with error, ErrStalled.
      - timeout, or ctx ending while the prompt was in flight: run submitted
        with error (the text may have arrived), ErrTimeout or ctx's error.
      - anything else (herdr unavailable, agent gone): run failed.

    Once the prompt was sent, the run and session rows are recorded even when
    ctx ends meanwhile, so a delivered prompt never stays pending.

    The session gets last_prompt_at, idle_ticks 0, the acked status and,
    once herdr reports it, session_id. prompt_text is stored redacted. An agent
    named by a rename command (codex) acked as working is named right away when
    its title lacks Title (nameAgent). A shell role refuses with ErrNotAgent
    (see RunShell).

func (m *Manager) SwitchModel(ctx context.Context, s store.Session, model, reason string) error
    SwitchModel switches the idle agent of session s to model with its kind's
    switch_model command (claude: `/model <model>`) typed into its pane.
    In a session with history Claude Code asks first ("Switch model?", "❯ 1.
    Yes, switch to Opus 5.5", "2. No, go back"): magnum caused that dialog,
    so it presses Enter, once and only with the cursor on the "Yes, switch to"
    option (never confused with a permission prompt, which the observer still
    always denies; the observer leaves the session alone while it switches).
    The switch counts once the dialog is gone, the agent is idle and the visible
    screen's status line names the model ("Opus 5.5 | <cwd> | …"; for the
    kind's reset_model, the CLI's default model as a limit message named it,
    unverified when that is unknown). Not confirmed within SwitchModelTimeout
    is ErrTimeout (a dialog still open is backed out of with Esc). A confirmed
    switch records the session's model (KVSessionModel; removed when model is
    the role's configured one, or the kind's reset_model for a role without
    one) and appends an agent.model_switched event with reason (SwitchLimitHit,
    SwitchLimited, SwitchExpired). A kind without switch_model returns
    ErrNoModelSwitch, an agent that is not idle ErrBusy.

    Claude Code also saves a /model choice as the default for new sessions:
    for a claude session SwitchModel snapshots the settings file's model before
    it types the command and restores exactly that afterwards (holdDefaultModel;
    EventDefaultModelRestored when it had changed). Claude switches run one at a
    time for that.

func (m *Manager) TimeUp(ctx context.Context, run store.Run, text string) error
    TimeUp sends text to the agent of run's session within that run, without
    a new run (as continueAfterDeny sends after_deny_prompt): the pipeline's
    last call to a reviewer whose time ran out to stop its background tasks
    and write its report now, or, once the reviewer was interrupted, to stop
    the background tasks it left running. From then on the agent's background
    work no longer holds the run open (the text tells it to stop that work);
    a task notification it has not answered still does. The session's idle_ticks
    are reset and its last_prompt_at set (so the turn the text starts is not
    taken for someone typing). A run without a live agent session fails with
    ErrNoSession (ErrNotAgent for a shell role's pane).

func (m *Manager) Wrapper(ctx context.Context, kind string) (bool, error)
    Wrapper reports whether the kind's command is a zsh wrapper function (or
    alias) that supplies its own flags, per [kinds.<kind>] wrapper: "true"
    and "false" are taken as is; "auto" (or empty) probes `zsh -ic 'whence -w
    <kind>'` once per kind and caches the answer. A probe that cannot tell
    assumes a wrapper (the M0 finding) and is retried next time. An undeclared
    kind is an error.

type ModelLimit struct {
	Kind  string
	Model string
	Until time.Time
	// Using is the first of the kind's fallback_models that is not limited
	// itself ("" = none).
	Using string
}
    ModelLimit is one model of a kind that hit its own limit.

func ModelLimits(ctx context.Context, st *store.Store, cfg *config.Config, kind string, now time.Time) []ModelLimit
    ModelLimits lists the per-model limits of kind recorded in st that end
    after now, by model name, each with the fallback a session uses meanwhile.
    cfg may be nil (Using then stays empty).

type Observation struct {
	Kind    ObservationKind
	Role    Role
	PRID    int64
	Status  herdr.Status  // agent status this tick ("" for shell panes and missing agents)
	Session store.Session // row after this tick's update
	Run     *store.Run    // newest run in flight for the session (completed run for ObsCompleted)
	// Background is the work an idle claude agent started in the background
	// during Run and left running, as its transcript shows (see
	// backgroundWait); while there is any, the run does not end.
	Background int
}
    Observation is the result of one tick for one starting/live session.

type ObservationKind string
    ObservationKind is what one tick noticed about a session.

const (
	// ObsCompleted: a submitted/working run's agent was idle|done on
	// CompletionIdleTicks consecutive ticks. The run is now ended; verify it
	// (reports on disk, the judge's review on GitHub).
	ObsCompleted ObservationKind = "completed"
	// ObsHumanActive: the agent works with no magnum run in flight; prs.human_active_at
	// was set to now, so Submit refuses for daemon.human_cooldown.
	ObsHumanActive ObservationKind = "human_active"
	// ObsBlocked: the agent waits on a dialog magnum leaves to the human (a
	// kind with on_permission_prompt "wait", a dialog that is not a
	// recognised permission prompt, a run past MaxPromptDenies): tell the
	// human.
	ObsBlocked ObservationKind = "blocked"
	// ObsPromptDenied: the agent was blocked on a permission prompt during
	// one of magnum's runs and magnum answered No (this tick, or the tick
	// before while the screen catches up; see answerPermission); the human
	// is not needed.
	ObsPromptDenied ObservationKind = "prompt_denied"
	// ObsDenyContinued: the agent went idle after such a deny, during the
	// same run, and was sent its kind's after_deny_prompt (see
	// continueAfterDeny); the run goes on.
	ObsDenyContinued ObservationKind = "deny_continued"
	// ObsLost: the pane or the agent is gone; the session is now lost (its
	// session_id kept for ResumeID). Run carries the run that was in flight.
	ObsLost ObservationKind = "lost"
)
    Observation kinds; "" means nothing to act on.

type PreviousReview struct {
	ID          int64
	Event       string // APPROVE, COMMENT, CHANGES_REQUESTED, ...
	SHA         string // commit the review was on (short is fine)
	SubmittedAt string // RFC3339
}
    PreviousReview is an earlier review by the reviewer login (recovery mode).

type Readiness struct {
	Checks []ReadinessCheck
	Failed int    // checks whose Status is not ReadinessOK
	File   string // the JSON file with every check (readiness.json in the report directory); "" when it could not be written
}
    Readiness is the readiness step's outcome as a judge prompt shows it.
    The zero value means no step ran (nothing configured, a continued turn).

type ReadinessCheck struct {
	Kind    string `json:"kind"`    // ReadinessResetDB, ReadinessPrepare, ReadinessReady or ReadinessRuby
	Command string `json:"command"` // as configured (ruby: the version command)
	OK      bool   `json:"ok"`
	Status  string `json:"status"` // ReadinessOK, ReadinessFailed, ReadinessTimeout or ReadinessSkipped
	// Detail is magnum's own one-line account ("exit 1", "the checkout pins
	// Ruby 3.3.4 (.ruby-version); `zsh -lc` runs 3.2.2"): never command
	// output, so prompts may show it.
	Detail string `json:"detail,omitempty"`
	// LastLine is the last non-empty line the command printed (redacted,
	// shortened). It comes from code in the PR's checkout, so it is data:
	// it goes into the readiness file only, never into a prompt.
	LastLine string `json:"last_line,omitempty"`
	Duration string `json:"duration"` // e.g. "4.2s"
}
    ReadinessCheck is one command of the readiness step.

type RecoverAction string
    RecoverAction says what Recover did with a session.

const (
	RecoverOK       RecoverAction = "ok"       // pane and agent are where the row says
	RecoverRebound  RecoverAction = "rebound"  // live row moved to the pane now holding its session id
	RecoverRestored RecoverAction = "restored" // parked/lost row is live again: herdr restored its conversation
	RecoverLost     RecoverAction = "lost"     // pane/agent gone and not found elsewhere
)
    Recover actions.

type Recovered struct {
	Role    Role
	Action  RecoverAction
	Session store.Session // row before the change
}
    Recovered is one Recover outcome.

type Report struct {
	Role    string // the role's name, e.g. claude-review
	Label   string // how the prompt names it ("" = Role)
	Path    string // the report file (absolute); "" when Missing
	Status  string // the role's outcome this round (ok, failed, timeout, ...); informational
	Missing bool   // no usable report this round
	Detail  string // why it is missing, e.g. "timed out after 40m"
	// Reason is Detail under its older name, for prompt files that still
	// render `missing ({{.Reason}})`.
	Reason string
}
    Report is one candidate report listed in a judge prompt, one per non-judge
    role of the round in pipeline order. RenderPrompt completes it: Label
    defaults to Role; a report without Path is Missing; a Missing report has
    no Path; Detail and Reason fill each other, then default to Status (or "no
    report") for a missing one.

type ReviewThread struct {
	ID string `json:"id"` // GraphQL node id
	// CommentID is the REST id of the thread's first comment: a reply goes
	// to POST repos/{o}/{r}/pulls/{n}/comments/{CommentID}/replies.
	CommentID int64         `json:"comment_id"`
	URL       string        `json:"url"`
	Finding   string        `json:"finding"`  // the first line of the thread's first comment
	Location  string        `json:"location"` // path:line; the original line when outdated
	Resolved  bool          `json:"resolved"`
	Outdated  bool          `json:"outdated"`
	Replies   []ThreadReply `json:"replies"` // oldest first
}
    ReviewThread is an inline thread the reviewer login started on the PR
    (JudgeData.Threads), as magnum writes it to the threads file.

type Role string
    Role is a review role's configured name (config.Role.Name), the value
    stored in sessions.role and runs.role. Resolve what users type with
    config.Config.RoleByNameOrAlias; the role's definition (kind, prompts,
    output, ...) is the config.Role of that name.

const (
	RoleJudge       Role = config.RoleCodexJudge     // codex-judge: persistent Codex session that posts the review
	RoleClaude      Role = config.RoleClaudeReview   // claude-review: persistent Claude session running /code-review
	RoleCodexReview Role = config.RoleCodexReview    // codex-review: shell pane running `command codex review`
	RoleSimplify    Role = config.RoleClaudeSimplify // claude-simplify: on-demand Claude session running /simplify
)
    The built-in roles (config.DefaultRoles), for defaults and tests.

func (r Role) Label() string
    Label is the role's user-visible name: the role name itself (pane labels,
    titles, agent names and CLI output use it).

type RoleData struct {
	URL             string // the pull request
	Owner, Repo     string // repo is the name part, e.g. talkable
	Number          int
	HeadSHA         string // the commit under review (checked out, detached)
	PreviousHeadSHA string // the head of the role's previous run (rereview)
	BaseSHA         string // the merge base; `git diff <BaseSHA>..HEAD` is the PR's diff
	BaseRef         string // the base ref, e.g. origin/master or a stacked PR's parent branch
	ReportPath      string // where the role writes its report (its output, absolute)
	Model, Effort   string // the role's model and this round's effort (config.Role.EffortFor; claude-review passes it to /code-review)
	// EffortInPrompt: the agent's kind sets the effort only at launch and
	// this round's Effort differs from the role's (see JudgeData).
	EffortInPrompt bool
	Mode           string // ModeInitial, ModeRereview or ModeRestart
	Since          string // RFC3339: the role's previous run (rereview)
	// ForcePushed: PreviousHeadSHA is no longer in the branch (rereview).
	ForcePushed bool
	// BaseMerged: the commits since PreviousHeadSHA merged a branch in,
	// usually the base (rereview), so PreviousHeadSHA..HeadSHA carries its
	// commits: the prompt compares the PR's own diff before and after.
	BaseMerged bool
	// RestartedFrom is the head the role was reviewing when a push cut its
	// turn short (ModeRestart); HeadSHA is the new head.
	RestartedFrom string
	// NotesPath is the repository notes file the role reads first (hints from
	// earlier reviews of the repository); "" = no notes.
	NotesPath string
	// Blind: an evaluation replay (pipeline.RoundInput.Blind); the role
	// must not read reviews, comments or commits after HeadSHA.
	Blind bool
	// PostMerge: GitHub merged the PR before magnum reviewed HeadSHA
	// (pipeline.RoundInput.PostMerge); the prompts that name the PR say it
	// is merged and to review it anyway.
	PostMerge bool
	// Budget is the role's turn timeout in words ("40 minutes"): the
	// prompts tell the agent to end its turn with the report written
	// within it.
	Budget string
	// RunID is the run the prompt starts: the shipped prompts ask for its
	// marker, ReportMarker(RunID), as the report's first line, and a role
	// whose prompt names it has a report only when the report carries it.
	RunID string
}
    RoleData feeds the prompts of every non-judge session role (claude-review,
    claude-simplify, a droid or omp reviewer, ...): the union of what those
    prompts may use. Fields a template does not use may stay zero.

type ShellData struct {
	Title      string   // the pane title, e.g. "PR #729 codex-review - talkable"; "" = no title prefix
	Command    string   // the role's command template ("" = role.Command)
	Args       []string // the role's args (nil = role.Args)
	Capture    string   // the role's capture ("" = role.Capture)
	ReportPath string   // the role's output, absolute
	Marker     string   // the done marker, DoneMarker(runID); [A-Za-z0-9._-]+
	BaseRef    string   // e.g. origin/master, or the parent branch of a stacked PR
	// BaseSHA is the merge base of HeadSHA and the base; "" when unknown.
	// Unlike BaseRef it does not move when the base branch gains commits,
	// so `codex review --base {{if .BaseSHA}}{{.BaseSHA}}{{else}}{{.BaseRef}}{{end}}`
	// reviews exactly the PR's diff.
	BaseSHA string
	HeadSHA string
	URL     string

	// RunID and ExtraArgs keep full-line templates written for the old
	// codex_review.sh data working: RunID is Marker without its
	// MAGNUM_DONE_ prefix (or, when Marker is empty, sets it), ExtraArgs
	// defaults to Args.
	RunID     string
	ExtraArgs []string

	// Template is the resolved full-line template of a role with Prompt
	// (config.Config.RolePrompt(role, config.PromptInitial)); nil = resolve
	// role.Prompt among the embedded defaults. Manager.ShellLine fills it
	// from the configuration. Not a template variable.
	Template *config.Prompt
}
    ShellData feeds a shell role's line (ShellLine): the role's command
    template, or a full-line .sh prompt template. Every value is shell-quoted
    when needed before it is substituted, because the result is typed into a
    shell.

type ThreadReply struct {
	ID     int64  `json:"id"` // REST comment id
	Author string `json:"author"`
	// Own: the reviewer login wrote it (an earlier rebuttal); it has no Class.
	Own bool `json:"own,omitempty"`
	// Class is what the reply's first clause claims (past an
	// acknowledgement such as "Good catch,"): fixed, not a bug, won't fix or
	// other.
	Class     string `json:"class,omitempty"`
	Body      string `json:"body"`                // an excerpt: at most 600 characters
	Truncated bool   `json:"truncated,omitempty"` // Body was cut
}
    ThreadReply is one reply of a ReviewThread.

type Workspace struct {
	WorkspaceID string
	TabID       string
	// Panes maps a role name to its herdr pane id, for agent and shell roles
	// alike (a shell role's pane runs its command; see RunShell).
	Panes map[Role]string
	// Roles is the layout order: the root pane's role (the judge) first, then
	// the other roles in the order they were laid out (EnsurePane appends).
	Roles []Role
	// Created is true when this call created the workspace (agents must be
	// started; resume ids come from ResumeID).
	Created bool
	// MovedFrom is the previous checkout when the PR's old workspace pointed
	// at another slot and was parked; pass it to the judge as moved_from.
	MovedFrom string
}
    Workspace is a PR's herdr workspace and the pane serving each role.

```

## app

```text
package app // import "github.com/zhuravel/magnum/internal/app"

Package app wires magnum's components from config.toml: the store,
the subprocess runner (wrapped in execx.DryRun under --dry-run), the herdr,
GitHub, MySQL and git clients, the identities, and the slots, inventory, agents,
pipeline, cleanup and notify layers built on top of them. The daemon (package
engine) and the CLI share one App per process.

CONSTANTS

const (
	LogMaxBytes = 10 << 20 // rotate state/logs/daemon.log at 10 MB
	LogKeep     = 3        // keep daemon.log.1 … daemon.log.3
)
    Log rotation defaults for the daemon log.


FUNCTIONS

func Interactive(f *os.File) bool
    Interactive reports whether f is a terminal (so logs are mirrored to it
    and prompts may be shown). It asks the terminal driver (the termios ioctl
    behind isatty), so /dev/null, which launchd and background jobs hand out as
    stdin/stderr and which is also a character device, is not a terminal.

func LookupPR(ctx context.Context, st *store.Store, p RefParser, ref string) (store.Repo, store.PR, error)
    LookupPR resolves ref with p and loads the repository and PR rows from st.
    A repository or PR the registry does not know wraps store.ErrNotFound.

func NewLogger(file io.Writer, stderr io.Writer, level slog.Leveler) *slog.Logger
    NewLogger builds magnum's logger: JSON lines to file (when non-nil) plus
    human-readable text to stderr (when non-nil), both behind RedactHandler.


TYPES

type App struct {
	Config *config.Config
	Layout paths.Layout
	Store  *store.Store
	// Runner is the subprocess runner every component shares; under DryRun it
	// is DryRunner.
	Runner    execx.Runner
	DryRunner *execx.DryRun // non-nil under DryRun: the planned mutating commands
	DryRun    bool
	// AgentTag is Options.AgentTag.
	AgentTag string
	// Mise is Options.Mise.
	Mise string

	Herdr *herdr.Client
	// GitHub returns the gh client acting as the named identity (nil for an
	// unknown name). Clients are created once and shared.
	GitHub     func(identity string) *github.Client
	Identities map[string]identity.Source
	// MySQL is the local DBngin client. mysqlx.Open never connects, so it is
	// non-nil even while MySQL is down (calls then fail and consumers degrade);
	// Ping probes it. Nil only when the DSN is invalid.
	MySQL *mysqlx.Client
	Git   *gitx.Client

	Slots     *slots.Manager
	Inventory *inventory.Scanner
	Agents    *agents.Manager
	// Pipeline holds one round runner per identity name (its GitHub client
	// acts as that identity).
	Pipeline map[string]*pipeline.Runner
	Cleanup  *cleanup.Planner
	Notify   *notify.Notifier

	Logger *slog.Logger

	// Has unexported fields.
}
    App is the wired set of components one process uses.

func New(cfg *config.Config, layout paths.Layout, opts Options) (*App, error)
    New builds the App: it creates the state tree, opens the store (a private
    copy under DryRun), builds every client from cfg and wires the layers.

func (a *App) Close() error
    Close releases everything New opened (store, MySQL, log file, the dry-run
    store copy), in reverse order.

func (a *App) IdentityNames() []string
    IdentityNames lists the configured identity names in config order.

func (a *App) Refs() RefParser
    Refs is the App's RefParser (cfg.Daemon.DefaultRepo).

type Options struct {
	// DryRun wraps the runner in execx.DryRun, puts slots in dry-run mode,
	// disables toasts and opens a private copy of the registry, so nothing
	// outside the process changes.
	DryRun bool
	// Daemon marks the process as the daemon, which owns daemon.log rotation.
	// Any other process (CLI commands) appends to daemon.log without ever
	// rotating it.
	Daemon bool
	// Logger replaces the default logger (JSON to state/logs/daemon.log,
	// rotated by size when Daemon is set, plus text to Stderr). Under DryRun
	// the default logs to Stderr only.
	Logger *slog.Logger
	// Stderr receives human-readable logs; nil means os.Stderr when it is a
	// terminal (or always under DryRun).
	Stderr io.Writer
	// LogLevel defaults to info.
	LogLevel slog.Leveler
	// Runner replaces the real subprocess runner (tests).
	Runner execx.Runner
	// MySQLDSN defaults to $MAGNUM_MYSQL_DSN, else mysqlx.DefaultDSN.
	MySQLDSN string
	// HTTPClient and Getenv are passed to App identities (tests). A nil
	// HTTPClient means the [github] transport client (see githubHTTPClient).
	HTTPClient *http.Client
	Getenv     func(string) string
	// HerdrSocket overrides config [herdr] socket (tests).
	HerdrSocket string
	// AgentTag tags the agents this App starts (agents.Deps.Tag,
	// pipeline.Runner.AgentTag) and turns its toasts off: magnum eval uses
	// "eval" with a scratch layout, so its rounds never meet the PR's own
	// agents or notify like a real review.
	AgentTag string
	// Mise is the mise executable that runs the pool's scripts: the slots
	// manager's (slots.Deps.Mise) and the readiness step's reset_db commands'
	// (pipeline.Runner.Mise). "" means "mise" on PATH.
	Mise string
	// BeforeMigrate is store.Options.BeforeMigrate for the real registry
	// (the CLI refuses to migrate it under a running daemon); a dry-run copy
	// migrates freely.
	BeforeMigrate func(from, to int) error
}
    Options tune New.

type Printf struct {
	Logger *slog.Logger
	Level  slog.Level
	Src    string
}
    Printf adapts a slog.Logger to execx.Logger (and herdr/slots/notify
    loggers): each line becomes one record at level with the given message
    prefix as the "src" attribute.

func (p Printf) Logf(level slog.Level, format string, args ...any)
    Logf implements execx.LevelLogger: the runner picks the level (successful
    commands at Debug, failures at Warn).

func (p Printf) Printf(format string, args ...any)
    Printf implements execx.Logger.

type RedactHandler struct{ Inner slog.Handler }
    RedactHandler wraps a slog.Handler and passes the message and every string
    attribute (errors and Stringers included) through execx.Redact, so a token
    that slips into a log call never reaches a sink.

func (h RedactHandler) Enabled(ctx context.Context, l slog.Level) bool
    Enabled implements slog.Handler.

func (h RedactHandler) Handle(ctx context.Context, r slog.Record) error
    Handle implements slog.Handler.

func (h RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler
    WithAttrs implements slog.Handler.

func (h RedactHandler) WithGroup(name string) slog.Handler
    WithGroup implements slog.Handler.

type RefParser struct{ DefaultRepo string }
    RefParser resolves the PR references users type (URL, owner/repo#N, repo#N,
    #N, N) against a default repository ("owner/name").

func (p RefParser) ResolvePR(_ context.Context, ref string) (owner, repo string, number int, err error)
    ResolvePR parses ref with github.ParseRef and the parser's default repo.

type RotatingFile struct {
	Path string
	Max  int64
	Keep int

	// Has unexported fields.
}
    RotatingFile is an io.Writer that appends to a file and rotates it by size:
    when a write would push the file past Max bytes, path.N-1 becomes path.N
    (the oldest beyond Keep is dropped), path becomes path.1 and a new file is
    started. Safe for concurrent use.

func OpenRotating(path string, max int64, keep int) (*RotatingFile, error)
    OpenRotating opens (or creates, 0600) path for appending.

func (r *RotatingFile) Close() error
    Close closes the file.

func (r *RotatingFile) Write(p []byte) (int, error)
    Write implements io.Writer.

```

## attention

```text
package attention // import "github.com/zhuravel/magnum/internal/attention"

Package attention explains why a PR needs the user. The engine stores the error
that sent a PR to needs_attention as it came: often a chain of contexts ending
in a command's whole output (a 300-line Ruby build log, a git transcript),
whose first line says nothing useful. Explain turns that text into a stage,
the one line of the output that names the cause, a one-line summary and the
next step, so every screen can say why in a line and what to do in another.
It is pure text work, so rows written before it existed are explained too.

CONSTANTS

const (
	KindFailed        = "failed"          // setup failed maxAttempts times on one head
	KindOverloaded    = "overloaded"      // the agent API stayed overloaded
	KindBlocked       = "blocked"         // the judge reported a blocker or waits on a dialog
	KindNoReview      = "needs_attention" // the judge posted nothing after a nudge, or its result was inconsistent
	KindIdentityError = "identity_error"  // the judge's identity check failed
	KindIdentityLeak  = "identity_leak"   // a review with the run's marker came from another login
)
    Kinds: why the engine parked the PR (engine needsAttention's why).

const (
	StageFetch        = "fetch"
	StageWorktree     = "worktree"
	StageCheckout     = "checkout"
	StageDependencies = "dependencies"
	StageAgents       = "agents"
	StageJudge        = "judge"
	StageIdentity     = "identity"
	StageReview       = "review"
)
    Stages: where the failure happened.

const SummaryMax = 160
    SummaryMax bounds Summary and Cause (runes).

const TailLines = 8
    TailLines bounds Reason.Tail.


TYPES

type Reason struct {
	Kind     string `json:"kind,omitempty"`
	Stage    string `json:"stage"`
	Attempts int    `json:"attempts,omitempty"`
	Head     string `json:"head,omitempty"` // the short sha the attempts were on
	// Step is the configured step that failed (a dependency command), "" when
	// not one.
	Step string `json:"step,omitempty"`
	// Cause is the line of the error that names what went wrong.
	Cause string `json:"cause"`
	// Summary is one line: the stage, the attempts and the cause.
	Summary string `json:"summary"`
	// Fix is the next step, with the PR's reference filled in.
	Fix string `json:"fix"`
	// Tail is the end of the failing command's output, cleaned (at most
	// TailLines lines), for a detail view; empty when there was none.
	Tail []string `json:"tail,omitempty"`
}
    Reason is why a PR needs the user, in screen-sized parts.

func Explain(kind, msg, ref string) Reason
    Explain reads why a PR needs the user from the stored error msg (the PR's
    last_error) and kind (the engine's why, "" when unknown: then inferred).
    ref is how the user names the PR (talkable#9992), used in Fix.

```

## cleanup

```text
package cleanup // import "github.com/zhuravel/magnum/internal/cleanup"

Package cleanup builds and applies storage cleanup plans: releasing the slots
of closed PRs, removing per-PR worktrees and surplus pool slots, dropping
orphan per-worktree databases and resetting a manual worktree (talkable.repoN).
The daemon and the CLI share it: the daemon applies the default plan every tick,
the CLI prints a plan (Render) and applies it after confirmation.

Plan is read-only: it reads the registry, one inventory scan and, for the
directories it would delete, `du -sk`. Apply re-checks every action against the
current registry (a plan may be minutes old by the time a human confirms it),
scans the inventory again before a database drop or an external reset, executes
it through the slots manager, MySQL or git, writes an audit event per action and
keeps going past failures. Nothing is dropped or reset on incomplete facts (a
source that could not be read).

CONSTANTS

const (
	KindRelease        = "release"         // pool slot back to the pool (placeholder reset, schema reload)
	KindRemoveSlot     = "remove_slot"     // pool slot torn down (teardown, databases, worktree, branch)
	KindRemoveWorktree = "remove_worktree" // per-PR worktree removed (its teardown/pre-remove hooks run first)
	KindDropDBs        = "drop_dbs"        // orphan databases of one slug dropped
	KindResetExternal  = "reset_external"  // manual worktree reset to origin/<base>; databases kept
)
    Action kinds.

const (
	SkipPinned       = "pinned"               // slot or PR pinned (unpin first)
	SkipHoldReason   = "hold_reason"          // slots.hold_reason is set (Detail: the reason)
	SkipGraceLeft    = "grace_left"           // closed PR inside daemon.close_grace (Detail: time left)
	SkipNotManaged   = "not_managed"          // not magnum's to touch (external slot, non-allowlisted slug)
	SkipDirty        = "dirty_without_force"  // a human's changes would be lost; --force discards them (removals)
	SkipActiveRun    = "active_run"           // a review round is in flight (run, PR state or busy slot)
	SkipAgentWorking = "agent_working"        // a human's herdr agent is in the slot, or any agent works in a manual worktree
	SkipUnpushed     = "unpushed_commits"     // the reset would orphan commits; --force accepts that
	SkipSlotState    = "slot_state"           // the slot's state does not allow the operation
	SkipInUse        = "in_use"               // the slug's databases belong to a slot or worktree
	SkipNotFound     = "not_found"            // the named PR, slot, worktree or slug does not exist
	SkipIncomplete   = "discovery_incomplete" // a source (MySQL, git worktree lists, herdr) could not be read
)
    Skip reasons: why something the options selected is not in Actions.

const (
	StatusDone        = "done"
	StatusFailed      = "failed"
	StatusUnconfirmed = "unconfirmed" // needs typed confirmation; Apply was called without it
	StatusPlanned     = "planned"     // dry-run plan: nothing executed
)
    Result statuses.

const DroppedBy = "cleanup"
    DroppedBy is slot_databases.dropped_by for databases cleanup dropped.


VARIABLES

var (
	// ErrOptions: the Options combination is invalid.
	ErrOptions = errors.New("cleanup: invalid options")
	// ErrUnconfirmed: an action needs typed confirmation that Apply was not given.
	ErrUnconfirmed = errors.New("cleanup: needs typed confirmation")
)

FUNCTIONS

func Render(p Plan) string
    Render prints a plan for the CLI:

        Plan: free ~2.1G disk, 800M MySQL (3 actions)
          release          talkable#11700 (merged 14m ago) review5
          drop_dbs         slug:review9 (orphan databases ...): 8 databases, 800M MySQL
        Skipped:
          talkable#11701   grace_left (6m left)

func RenderReport(r Report) string
    RenderReport prints the outcome of Apply, one line per action plus a
    summary.


TYPES

type Action struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"` // owner/name#N, slot:<name>, slug:<slug>, path:<dir>
	Why     string `json:"why"`
	// Bytes is the disk space freed (du estimate, 0 = none or unknown);
	// DBBytes the MySQL space freed.
	Bytes   int64    `json:"bytes"`
	DBBytes int64    `json:"db_bytes"`
	DBNames []string `json:"db_names,omitempty"`

	Repo   string `json:"repo,omitempty"` // owner/name (pool lookup)
	Slot   string `json:"slot,omitempty"`
	SlotID int64  `json:"slot_id,omitempty"`
	PRID   int64  `json:"pr_id,omitempty"`
	// PRState is the PR's state at plan time; a closed/releasing PR moves to
	// releasing and then released around the slot operation.
	PRState string `json:"pr_state,omitempty"`
	Reason  string `json:"reason,omitempty"` // assignment end reason: merged, closed or cleanup
	Path    string `json:"path,omitempty"`
	Slug    string `json:"slug,omitempty"`
	// reset_external
	MainClone string `json:"main_clone,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Base      string `json:"base,omitempty"`

	Force   bool `json:"force,omitempty"`
	Confirm bool `json:"confirm,omitempty"` // needs typed confirmation (Apply's confirmed)
}
    Action is one planned change. The identifying fields (SlotID, PRID, Path,
    Slug, ...) let Apply re-check it against the current state.

type Git interface {
	FetchBranch(ctx context.Context, mainClone, base string) error
	ResetPlaceholder(ctx context.Context, dir, branch, base string) error
	Status(ctx context.Context, dir string) (gitx.Status, error)
	Unpushed(ctx context.Context, dir string) (int, error)
}
    Git is the part of *gitx.Client cleanup uses (dirty checks and the external
    reset).

type Inventory interface {
	Scan(ctx context.Context, opts inventory.Options) (inventory.Inventory, error)
}
    Inventory is the part of *inventory.Scanner cleanup reads.

type MySQL interface {
	DropAll(ctx context.Context, names []string, g mysqlx.Guard) []mysqlx.DropResult
}
    MySQL is the part of *mysqlx.Client cleanup uses.

type Options struct {
	DryRun bool   `json:"dry_run,omitempty"` // Apply executes nothing
	PR     *PRRef `json:"pr,omitempty"`      // release (pool) or remove (per-PR) this PR's slot
	// Slot names a managed slot to release (or, with Remove, to remove); with
	// External it names a manual worktree (repoN, its directory name or path).
	Slot     string `json:"slot,omitempty"`
	Remove   bool   `json:"remove,omitempty"`
	Orphans  bool   `json:"orphans,omitempty"` // orphan databases on the automatic allowlist
	Slug     string `json:"slug,omitempty"`    // with Orphans: also this slug's orphans (typed confirmation)
	Shrink   *int   `json:"shrink,omitempty"`  // remove free slots beyond max(pool.min, N), oldest idle first
	Idle     bool   `json:"idle,omitempty"`    // with Shrink: only slots idle longer than pool.idle_remove_after
	External bool   `json:"external,omitempty"`
	// Force discards a human's tracked changes and untracked files on a
	// removal (magnum's residue goes without it) and, for reset_external,
	// any change and unpushed commits; it also lets an explicitly
	// named PR be released inside its close grace. It never overrides pins,
	// hold reasons, agents, in-flight rounds or incomplete discovery.
	Force bool `json:"force,omitempty"`
}
    Options selects what a plan covers. With no selector (PR, Slot, Orphans,
    Shrink) the plan is the default one: slots of closed PRs past their grace
    plus orphan databases on the automatic allowlist. Selectors combine.

type PRRef struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}
    PRRef names a pull request: Repo is "owner/name".

func (r PRRef) String() string

type Plan struct {
	CreatedAt time.Time `json:"created_at"`
	DryRun    bool      `json:"dry_run,omitempty"`
	Options   Options   `json:"options"`
	Actions   []Action  `json:"actions"`
	Skipped   []Skip    `json:"skipped"`
	Totals    Totals    `json:"totals"`
	Warnings  []string  `json:"warnings,omitempty"` // inventory sources that could not be read
}
    Plan is a deterministic list of actions plus what was skipped and why.

func (p Plan) NeedsConfirmation() bool
    NeedsConfirmation reports whether any action needs typed confirmation.

type Planner struct {
	Store     *store.Store
	Slots     Slots
	Inventory Inventory
	MySQL     MySQL
	Git       Git
	// Runner runs `du -sk` on directories an action deletes (read-only);
	// nil leaves Action.Bytes at 0 (unknown).
	Runner execx.Runner
	Config *config.Config
	// Clock defaults to time.Now.
	Clock func() time.Time
	// Park, when set, is called before the slot of a PR is released or
	// removed (e.g. (*agents.Manager).Park); an error fails that action and
	// leaves the PR's state as it was.
	Park func(ctx context.Context, pr store.PR) error
	// Log receives one line per applied action (optional).
	Log execx.Logger
}
    Planner builds and applies plans. Store, Slots, Inventory and Config are
    required; MySQL is needed to apply drop_dbs, Git for dirty checks of per-PR
    worktrees without inventory facts and for reset_external.

func (p *Planner) Apply(ctx context.Context, plan Plan, confirmed bool) (Report, error)
    Apply executes plan in order. A dry-run plan executes nothing (every
    result is planned); an action that needs typed confirmation runs only when
    confirmed is true (otherwise unconfirmed, not an error). Each action is
    re-checked against the current registry first, so a stale plan fails that
    action (wrapping store.ErrConflict) instead of touching something that
    changed. Apply keeps going past failed actions, writes one audit event per
    executed action and returns the report plus the failures joined (nil when
    none failed).

func (p *Planner) Plan(ctx context.Context, opts Options) (Plan, error)
    Plan builds the plan for opts. It changes nothing: it reads the registry,
    scans the inventory once (with External for --external) and runs `du -sk` on
    the directories an action would delete. Only a store or inventory failure is
    an error; anything the options select but the plan leaves alone is listed in
    Skipped with a reason.

func (p *Planner) PlanFrom(ctx context.Context, opts Options, inv inventory.Inventory) (Plan, error)
    PlanFrom is Plan over an inventory the caller already scanned (with
    inventory.Options{External: opts.External}), so a reconcile that has just
    scanned plans its cleanups without scanning again. The inventory may be a
    little old: Apply re-checks every action against the current registry.

type Report struct {
	Results     []Result `json:"results"`
	Done        int      `json:"done"`
	Failed      int      `json:"failed"`
	Unconfirmed int      `json:"unconfirmed"`
	Planned     int      `json:"planned"`
	// Freed sums the estimates of the done actions.
	DiskBytes  int64    `json:"disk_bytes"`
	MySQLBytes int64    `json:"mysql_bytes"`
	DroppedDBs []string `json:"dropped_dbs,omitempty"`
}
    Report is the outcome of Apply.

type Result struct {
	Action Action `json:"action"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}
    Result is the outcome of one action.

type Skip struct {
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}
    Skip is something the options selected that the plan leaves alone.

type Slots interface {
	Release(ctx context.Context, slot store.Slot, pool config.Pool, reason string) error
	Remove(ctx context.Context, slot store.Slot, pool config.Pool, force bool) error
	RemovePRWorktree(ctx context.Context, slot store.Slot, force bool) error
	Guard(ctx context.Context, slot store.Slot) error
	// HumanEvidence says why changes in the slot's tree are a human's (its
	// PR's human_active_at after the checkout), "" when they are magnum's
	// residue, which a release or removal discards.
	HumanEvidence(ctx context.Context, slot store.Slot) (string, error)
}
    Slots is the part of *slots.Manager cleanup drives.

type Totals struct {
	Actions    int   `json:"actions"`
	DiskBytes  int64 `json:"disk_bytes"`
	MySQLBytes int64 `json:"mysql_bytes"`
	Databases  int   `json:"databases"`
}
    Totals sums a plan's estimates.

```

## cli

```text
package cli // import "github.com/zhuravel/magnum/internal/cli"

Package cli wires the subcommands into a cobra command tree built per call.
Each command lives in its own file and stays a thin layer over the engine,
slots, inventory and store packages.

FUNCTIONS

func Main(version string, args []string, stdout, stderr io.Writer) int
    Main runs args (without the program name) and returns the exit code:
    0 on success, 1 when a command fails and 2 on a usage error.


TYPES

type Context struct {
	Version string
	Layout  paths.Layout
	Config  *config.Config // nil until LoadConfig
	Stdout  io.Writer
	Stderr  io.Writer

	// Has unexported fields.
}
    Context carries what every command needs.

func (c *Context) LoadConfig() error
    LoadConfig loads config.toml once.

```

## config

```text
package config // import "github.com/zhuravel/magnum/internal/config"

Package config loads and validates config.toml (kept in the repository).

CONSTANTS

const (
	// PermissionDeny: answer the prompt's No option, once per prompt and at
	// most a bounded number of times per run, so a review agent that hits a
	// prompt its CLI shows even in its no-approval mode (Claude Code's
	// "Dangerous rm operation" check) carries on instead of stalling the round.
	PermissionDeny = "deny"
	// PermissionWait: never answer; the agent stays blocked until the human
	// answers or the round times out.
	PermissionWait = "wait"
)
    Kind.OnPermissionPrompt values.

const (
	// HooksTrustOwn: pick "Trust all and continue" when the checkout carries
	// no hooks of its own (no .codex/hooks.json, no hooks or plugins in its
	// .codex/config.toml), so every hook listed comes from the user's Codex
	// home or an installed plugin; "Continue without trusting" otherwise.
	HooksTrustOwn = "trust_own"
	// HooksDecline: always "Continue without trusting": the session runs
	// without the untrusted hooks until the user trusts them in their own
	// Codex.
	HooksDecline = "decline"
)
    Kind.OnHooksReview values.

const (
	CurateOverLimit = "over_limit" // a repository past a limit, once its notes changed since the last curation
	CurateWeekly    = "weekly"     // every repository with notes, once a week, once they changed
	CurateMisses    = "misses"     // a retro recorded misses of the repository for its notes (class miss, scope repo)
	// CurateOff, alone, is no trigger: only `magnum notes <repo> --curate`.
	CurateOff = "off"
)
    Curation triggers ([notes] curate).

const (
	OwnPassParallel = "parallel" // the judge's own pass runs with the reviewers
	OwnPassAfter    = "after"    // one judge prompt after the reviewers
)
    Values of [pipeline] judge_own_pass.

const (
	// KindShell is the Role.Kind of a role that types a shell command into
	// a plain pane instead of driving an agent CLI (codex-review).
	KindShell = "shell"

	ModeSession = "session" // an interactive agent session (every agent kind)
	ModeShell   = "shell"   // a shell pane running Role.Command (kind = "shell")

	RunsAlways = "always" // every round that runs reviewers
	RunsFirst  = "first"  // until the role has completed once for the PR, then only on request
	RunsManual = "manual" // only when requested (today `magnum review --simplify`, for claude-simplify)
	RunsNever  = "never"  // disabled; requests are refused

	CaptureFile   = "file"   // the agent (or command) writes ReportFile itself
	CaptureStdout = "stdout" // shell roles: magnum tees the command's stdout into ReportFile (stderr stays in the pane)

	WrapperAuto  = "auto"  // probe `zsh -ic 'whence -w <kind>'` once: a function or alias is a wrapper
	WrapperTrue  = "true"  // the command is a wrapper that supplies its own flags
	WrapperFalse = "false" // a plain binary: Kind.Args are appended too

	SessionHerdr = "herdr" // herdr reports the CLI's session id (pane agent_session); resume uses it
	SessionNone  = "none"  // no session id: the role always starts fresh
)
    Role kinds, modes and option values.

const (
	PromptInitial  = "initial"  // Role.Prompt: first review of a PR (shell roles: the full command line)
	PromptRereview = "rereview" // Role.Rereview: a new head after an earlier review
	PromptRestart  = "restart"  // Role.Restart: a push cut the reviewer's turn short; the round restarted on the new head
	PromptContinue = "continue" // Role.ContinuePrompt: a pause ended mid-turn (judge)
	PromptRecovery = "recovery" // Role.Recovery: a fresh session after the old one was lost (judge)
	PromptNudge    = "nudge"    // Role.Nudge: the agent stopped without a result (judge)
	PromptOwnPass  = "own_pass" // Role.OwnPass: the judge's own pass, prompted with the reviewers (judge)
)
    Prompt kinds accepted by Role.PromptFile.

const (
	RoleCodexJudge     = "codex-judge"
	RoleClaudeReview   = "claude-review"
	RoleCodexReview    = "codex-review"
	RoleClaudeSimplify = "claude-simplify"

	KindCodex  = "codex"
	KindClaude = "claude"
	KindDroid  = "droid"
	KindOMP    = "omp"
)
    Built-in role names (DefaultRoles) and kinds (DefaultKinds).

const (
	PlaceholderSession = "{session}"
	PlaceholderTitle   = "{title}"
	PlaceholderModel   = "{model}"
	PlaceholderEffort  = "{effort}"
)
    Placeholders substituted by Kind.Argv and in Kind.Rename.

const BuiltinDefaults = "built-in defaults (config.defaults.toml)"
    BuiltinDefaults names the embedded base in messages and Sources.

const DatabaseSuffix = "__{slug}"
    DatabaseSuffix ends every pool database template ([[pool]] databases):
    the slot's slug follows the last "__", which is how magnum lists and guards
    a slot's databases.

const DefaultAfterDenyPrompt = "magnum denied that command: review roles never run approval-gated or destructive commands. " +
	"Continue the task without it and finish as instructed."
    DefaultAfterDenyPrompt is every built-in and declared kind's
    after_deny_prompt.

const DefaultIdleRemoveAfter = 168 * time.Hour
    DefaultIdleRemoveAfter is a pool's idle_remove_after when it sets none (or
    0): a free slot above pool.min is removed once it has been idle this long.
    Without it every reconcile removed the surplus at once, and the next
    provision wrote about 1 GB again.

const DefaultLearnPrompt = "retro.md"
    DefaultLearnPrompt is the retro prompt file.

const DefaultNotesPrompt = "notes-curate.md"
    DefaultNotesPrompt is the curator's prompt file.

const DefaultReadyTimeout = 5 * time.Minute
    DefaultReadyTimeout bounds a round's whole readiness step when neither the
    [[repo]] nor the [[pool]] sets ready_timeout.

const DefaultReviewFooter = "**Reviewed commit:** `{{.Short}}`\n" +
	"\n" +
	"<details><summary>ℹ️ About Magnum</summary>\n" +
	"\n" +
	"Automated review by [Magnum](https://github.com/zhuravel/magnum). Reply on a thread with `fixed`, `not a bug: <why>` or `won't fix: <why>`" +
	"{{if .Simplify}}; simplifications are optional{{end}}. New pushes are re-reviewed automatically.\n" +
	"\n" +
	"</details>"
    DefaultReviewFooter is the footer template of every identity that sets
    no review_footer: the reviewed commit, then what the review is and how
    to answer it, in the words the reply classifier knows, collapsed under
    <details> (config.defaults.toml documents it word for word).

const DefaultSimplifyRerunLines = 150
    DefaultSimplifyRerunLines is claude-simplify's rerun_min_lines: about two
    new functions' worth of code since it last looked.

const DefaultSkill = "{{repo}}/skills/magnum-review/SKILL.md"
    DefaultSkill is the judge's default skill path ({{repo}} = magnum's home).

const DefaultTriagePrompt = "triage.md"
    DefaultTriagePrompt is the triage prompt file.

const EmbeddedSkill = "builtin:skills/magnum-review/SKILL.md"
    EmbeddedSkill names the binary's own copy of the judge skill (magnum.Skill)
    as a skill source: what a judge gets without a checkout.

const LearnRoleName = "retro"
    LearnRoleName is the name of the role the retro's classifying agent runs as
    (LearnRole).

const NotesRoleName = "notes"
    NotesRoleName is the name of the role the curator runs as (NotesRole).

const PlaceholderNum = "{num}"
    PlaceholderNum is the issue number in a [board] trackers template.

const RequiredWorkflowPrefix = "workflow:"
    RequiredWorkflowPrefix marks a required_checks entry naming a whole GitHub
    Actions workflow ("workflow:CI").

const ReviewFooterMax = 2000
    ReviewFooterMax bounds review_footer, in characters.

const SkillCopyName = "SKILL.md"
    SkillCopyName is the file name of a judge skill's startup copy (<skill
    dir>/<first 12 hex of its SHA-256>/SKILL.md).


VARIABLES

var BadgeColors = []string{"red", "green", "yellow", "blue", "magenta", "cyan", "gray", "grey"}
    BadgeColors are the colors a badge may ask for: the board's ANSI colors.

var ErrPromptNotFound = errors.New("prompt not found")
    ErrPromptNotFound: a prompt name exists neither in prompts_dir nor among the
    embedded defaults.

var PromptKinds = []string{PromptInitial, PromptRereview, PromptRestart, PromptContinue, PromptRecovery, PromptNudge, PromptOwnPass}
    PromptKinds lists every prompt kind a role may name (Role.PromptFile).

var TrivialDeltaClasses = []string{"comments", "whitespace", "docs", "base"}
    TrivialDeltaClasses are the values of skip_trivial_deltas: a push that only
    changes comment lines, only whitespace (blank lines, re-indented code where
    indentation carries no meaning), only documentation files, or only merges
    the base branch (or rebases onto it) and leaves the PR's own diff against
    its base as it was (base).


FUNCTIONS

func DefaultKinds() map[string]Kind
    DefaultKinds returns the built-in kinds (fresh copies):

      - codex (0.160): args ["--dangerously-bypass-approvals-and-sandbox"],
        resume ["resume","{session}"], model ["--model","{model}"], effort
        ["-c","model_reasoning_effort={effort}"], rename "/rename {title}",
        login_check "codex login status" + login_ok "text:Logged in".
      - claude: args ["--dangerously-skip-permissions"], resume
        ["--resume","{session}"], model ["--model","{model}"], name
        ["--name","{title}"], login_check "claude auth status" + login_ok
        "json:loggedIn", switch_model "/model {model}", fallback_models
        ["opus","sonnet"], reset_model "default"; no effort flag (claude-review
        passes its effort to /code-review in the prompt).
      - droid (0.232): resume ["--resume","{session}"]; no model, effort or name
        flag in interactive mode, no login check.
      - omp (18.4): resume ["--resume={session}"], model ["--model={model}"],
        effort ["--thinking={effort}"]; no login check.

    All use wrapper "auto", session_source "herdr", on_permission_prompt
    "deny", on_hooks_review "trust_own", DefaultAfterDenyPrompt and
    DefaultHealthPatterns. The codex and claude args make a plain binary run
    without approval prompts and (codex) without its sandbox, as the user's
    zsh wrappers do: review agents run tests and `gh`, and magnum answers every
    approval prompt No. Args apply only without a wrapper, so a wrapper's own
    flags are never doubled.

func DefaultLearnModel(kind string) string
    DefaultLearnModel is the [learn] model of a kind the config names without
    choosing a model: sonnet for claude, the cheap model the retro was written
    for, and "" for every other kind, which then runs on its own default model
    (a Claude model name passed to another CLI fails every PR's retro).

func DefaultRelatedIgnore() []string
    DefaultRelatedIgnore is [pipeline] related_ignore's default: lockfiles,
    which most dependency changes touch whatever else they do.

func Issue(title string, trackers []Tracker) (key, url string)
    Issue finds the first issue key in title that one of trackers knows
    (leftmost; at one position, the earlier tracker) and returns the key
    ("PS-38553") and its URL; "" and "" when there is none.

func MatchPath(glob, name string) bool
    MatchPath reports whether the "/"-separated name matches glob.
    A "**" segment matches zero or more whole segments; every other segment
    is matched with path.Match, so "*" and "?" never cross a "/". Matching
    is case-sensitive (git paths are). A malformed glob matches nothing;
    see ValidatePathGlob.

func ParseDuration(s string) (time.Duration, error)
    ParseDuration is time.ParseDuration plus a leading whole-day count: "7d",
    "30d", "1d12h". A day is 24 hours.

func RenderFooter(tmpl string, d FooterData) (string, error)
    RenderFooter renders footer template tmpl with d, trimmed ("" = the footer
    is left out).

func SkillPath(skill string, layout paths.Layout) string
    SkillPath is a judge's skill as a round names it: skill with {{repo}}
    replaced by layout's checkout, or layout.Skill() when skill is ""; the
    embedded skill (EmbeddedSkill) when either needs a checkout the layout lacks
    (Load expands {{repo}} and ~ already; Defaults keeps them).

func ValidatePathGlob(glob string) error
    ValidatePathGlob checks a skip_paths entry: it must not be blank,
    no segment may be empty (a leading, trailing or doubled "/" would silently
    match nothing), every segment must be a well-formed path.Match pattern,
    and "**" is only allowed as a whole segment ("a**b" is rejected).


TYPES

type BadgeSpec struct {
	Text  string
	Color string // BadgeColors; "" = the terminal's (an emoji keeps its own)
}
    BadgeSpec is one badge: its text and, optionally, its color.

func (b *BadgeSpec) UnmarshalTOML(v any) error
    UnmarshalTOML reads a badge written as a text or as { text, color }.

type Board struct {
	// Badges map a GitHub label to what the board shows for it: a text
	// ({ "Flagged" = "🚩" }) or a text with a color ({ "Schema Migration" =
	// { text = "\uf1c0", color = "yellow" } }). The badge goes before the
	// title, the card shows it with the label, the summary counts it. A key
	// matches a label whatever their case and leading emoji or symbols
	// ("Flagged" matches "🚩 Flagged").
	Badges map[string]BadgeSpec `toml:"badges"`
	// Trackers link a PR to its issue: URL templates with {num} right after
	// the issue key's prefix ("https://linear.app/example/issue/DMA-{num}",
	// "https://example.atlassian.net/browse/PS-{num}"). The first issue key
	// of any of them in a PR's title ("[PS-38553] …") names the issue: t on
	// the board opens it, the card shows it. A [[watch]] or [[repo]] block's
	// trackers win for its PRs (Config.TrackersFor).
	Trackers []string `toml:"trackers"`
	// RecentClosed is how long a merged or closed PR stays on the board, in
	// its own section after the open PRs (merged_at, else closed_at, within
	// it); 0 turns the section off. `magnum prs --all` lists every closed PR
	// whatever it says.
	RecentClosed Duration `toml:"recent_closed"`
	// Shimmer slides a rainbow across the state cell of a PR magnum approved
	// that GitHub still blocks on the operator's approval ("✓ needs you",
	// "✓ lift your ✗") while one is on screen; false keeps it still. A
	// terminal without colors (NO_COLOR) shows it in reverse video either way.
	Shimmer bool `toml:"shimmer"`
}
    Board tunes the PR board ([board]).

type Config struct {
	Daemon     Daemon     `toml:"daemon"`
	Herdr      Herdr      `toml:"herdr"`
	Terminal   Terminal   `toml:"terminal"`
	GitHub     GitHub     `toml:"github"`
	Pipeline   Pipeline   `toml:"pipeline"`
	Usage      Usage      `toml:"usage"`
	Triage     Triage     `toml:"triage"`
	Learn      Learn      `toml:"learn"`
	Notes      Notes      `toml:"notes"`
	Board      Board      `toml:"board"`
	Identities []Identity `toml:"identity"`
	Watches    []Watch    `toml:"watch"`
	Pools      []Pool     `toml:"pool"`
	Repos      []Repo     `toml:"repo"`

	// Kinds are the agent CLIs ([kinds.<name>]) and Roles the review
	// pipeline ([[role]]). After Load or Defaults both hold the merged,
	// normalized result: built-in defaults, then the base
	// (config.defaults.toml or a file), then the user config. Read them
	// through KindSpec, RolesFor, JudgeFor and RoleByNameOrAlias.
	Kinds map[string]Kind `toml:"kinds"`
	Roles []Role          `toml:"role"`

	// Layout is filled by Load; it is not part of the file.
	Layout paths.Layout `toml:"-"`
	// Sources are what Load read, base first: a file, or BuiltinDefaults,
	// then the user layer when there was one.
	Sources []string `toml:"-"`

	// Has unexported fields.
}

func Defaults() *Config
    Defaults returns the built-in defaults (Kinds and Roles normalized, {{repo}}
    paths unexpanded); Load overlays the file on top.

func Load(layout paths.Layout, file string) (*Config, error)
    Load reads the configuration (see LoadWithOptions) with the zero
    LoadOptions.

func LoadWithOptions(layout paths.Layout, file string, opts LoadOptions) (*Config, error)
    LoadWithOptions reads the configuration in two layers and validates it.

    The base is file (or $MAGNUM_CONFIG): a complete configuration replacing
    the built-in defaults; else a config.toml in the layout's home (a checkout
    from before the defaults were built in); else the built-in defaults,
    the repository's config.defaults.toml embedded in the binary.

    The user layer goes over it unless opts.NoOverlay: the layout's UserConfig
    (~/.config/magnum/config.toml) when it exists. It appends [[identity]],
    [[watch]], [[pool]] and [[repo]], and overrides keys (see applyOverlay).
    cfg.Sources lists what was read.

func (c *Config) CommentsWhenClean(repo, identity string) bool
    CommentsWhenClean reports whether the identity named identity posts
    a comment, not an approval, for a clean verdict on repository repo
    ("owner/name"): VerdictsFor's no-findings event is COMMENT.

func (c *Config) IdentityByName(name string) *Identity
    IdentityByName returns the identity or nil.

func (c *Config) JudgeFor(w *Watch) Role
    JudgeFor returns the watch's judge (validation guarantees exactly one);
    the zero Role when there is none.

func (c *Config) JudgeOwnPassFor(w *Watch) string
    JudgeOwnPassFor is the judge_own_pass that applies to w's PRs: the watch's
    when it sets one, else [pipeline]'s, else OwnPassParallel.

func (c *Config) KeepApprovals(fullName string) bool
    KeepApprovals reports whether an App identity's approval on a PR of
    repository fullName stays when the PR gets new commits: the [[repo]]
    block's keep_approvals when set, else the covering watch's (default false:
    magnum dismisses it until the re-review posts).

func (c *Config) KindNames() []string
    KindNames lists the declared kinds, sorted.

func (c *Config) KindSpec(name string) (Kind, bool)
    KindSpec returns the merged [kinds.<name>] spec (user keys over
    DefaultKinds); false for "shell" and undeclared names.

func (c *Config) LearnRole() Role
    LearnRole is the role the retro's classifying agent runs as: LearnRoleName,
    the [learn] kind in session mode with its model, effort, args, prompt and
    timeout, writing retro.json, normalized like a configured role.

func (c *Config) Normalize()
    Normalize fills the defaulted fields of Kinds and Roles in place (Load
    and Defaults call it; call it again after editing either in code). It is
    idempotent and never overrides a value that is set.

func (c *Config) NotesRole() Role
    NotesRole is the role the curator runs as: NotesRoleName, the [notes] kind
    in session mode with its model, effort, args, prompt and timeout, normalized
    like a configured role.

func (c *Config) PoolFor(fullName string) *Pool
    PoolFor returns the pool for owner/name, or nil (per-PR worktree repos).

func (c *Config) PromptSnapshot() *PromptSnapshot
    PromptSnapshot returns the snapshot SnapshotPrompts installed; nil when
    prompts are read from disk (every CLI command).

func (c *Config) ReadinessFor(fullName string) Readiness
    ReadinessFor returns the readiness step of repository fullName
    ("owner/name"): each of prepare, ready and ready_timeout from its [[repo]]
    block when the block sets it, else from its [[pool]]; the timeout defaults
    to DefaultReadyTimeout.

func (c *Config) RelatedFor(w *Watch) Related
    RelatedFor is the related_lookback and related_ignore that apply to w's PRs:
    the watch's when it sets them (a positive lookback; any list, [] included),
    else [pipeline]'s. A nil w is [pipeline]'s.

func (c *Config) RepoFor(fullName string) *Repo
    RepoFor returns the [[repo]] block for owner/name, or nil.

func (c *Config) RequiredChecks(fullName string) []string
    RequiredChecks returns the [[repo]] block's required_checks for repository
    fullName (nil when none): check-name globs and "workflow:<glob>" entries
    that replace GitHub's list (store.Store.RequiredChecks).

func (c *Config) ResolvePrompt(name string) (Prompt, error)
    ResolvePrompt reads a prompt by name: <pipeline.prompts_dir>/<name> when
    that file exists, else the embedded default of the same name; an unknown
    name wraps ErrPromptNotFound. Names are plain file names (no directories).
    In the daemon, which loaded its prompts at startup (SnapshotPrompts), a name
    the snapshot holds resolves to the text loaded then, whatever the file holds
    now.

func (c *Config) RoleByNameOrAlias(w *Watch, s string) (Role, bool)
    RoleByNameOrAlias finds one of the watch's roles by name or alias (case and
    surrounding space ignored, so stored ids such as codex_review match too);
    "" is the watch's judge.

func (c *Config) RoleEnv(r Role) map[string]string
    RoleEnv is the pane environment of a role: its kind's env (the Tool's for
    shell roles) overlaid by the role's env.

func (c *Config) RoleModel(r Role) string
    RoleModel is the model a role's sessions are configured to run: the role's
    own model, else its kind's default_model ("" = the CLI's default; a shell
    role: its own model only).

func (c *Config) RolePrompt(r Role, kind string) (Prompt, error)
    RolePrompt resolves the role's prompt of a prompt kind (see PromptFile);
    a role without one wraps ErrPromptNotFound.

func (c *Config) RolesFor(w *Watch) []Role
    RolesFor returns the roles a watch runs, in [[role]] order: every role
    when w is nil or w.Roles is empty, else the ones w.Roles names (names or
    aliases).

func (c *Config) SelfLogins() []string
    SelfLogins are the logins that count as the operator's own (the board's
    "mine", ★): every watch's posting identity and every gh identity (the user),
    each once (textx.FoldLogin tells them apart).

func (c *Config) SelfMatch() func(login string) bool
    SelfMatch returns a test of whether a login is one of SelfLogins
    (textx.FoldLogin: case, "@" and "[bot]" do not matter).

func (c *Config) SkillFile(src string) string
    SkillFile is the file a judge prompt names for the skill at src (SkillPath):
    its startup copy when the daemon's snapshot holds one, else src itself.

func (c *Config) SnapshotPrompts(skillDir string, now time.Time, extra ...string) (*PromptSnapshot, error)
    SnapshotPrompts loads every prompt file the roles name (each prompt kind)
    and the extra names (model-fallback.md), copies each judge's skill file to
    skillDir/<hash>/SKILL.md ("" = no copies; the judges keep their configured
    skill paths), removes copies older than a week that no judge uses,
    and installs the result: from now on ResolvePrompt answers from it and
    SkillFile names the copies. A prompt that does not resolve is an error
    and nothing is installed; a skill that cannot be copied is a warning
    (PromptSnapshot.Warnings). Call it once, before the goroutines that resolve
    prompts start.

func (c *Config) Stages(w *Watch) [][]Role
    Stages orders the watch's roles for a round: non-judge roles layered by
    After (a stage holds the roles whose dependencies all ran in earlier stages;
    dependencies outside the watch's set are ignored), then the judge alone.
    Validation guarantees After is acyclic.

func (c *Config) ThrottleFor(w *Watch) Daemon
    ThrottleFor is the [daemon] section with w's burst, re-review delta and
    delta check overrides applied: the throttle settings (eligibility.Throttle)
    of w's PRs. A nil w is the daemon's.

func (c *Config) TrackersFor(fullName string) []Tracker
    TrackersFor returns the issue trackers of repository fullName's PRs:
    the [[repo]] block's, then the covering [[watch]]'s, then [board]'s,
    each issue key prefix taken from the first of them that names it. So one
    owner's "PR-" may lead to Linear while every other owner's leads to Jira.
    Templates that do not parse are left out (Validate reports them).

func (c *Config) TrivialDeltas(w *Watch) []string
    TrivialDeltas is the skip_trivial_deltas that applies to w's PRs:
    the watch's when it sets one, else the daemon's. Empty = every push is
    re-reviewed.

func (c *Config) Validate() error
    Validate checks cross references and required fields.

func (c *Config) VerdictsFor(fullName string, id *Identity) (noFindings, blocking string)
    VerdictsFor returns the review events the judge posts for repository
    fullName ("owner/name") as identity id: noFindings when it finds
    nothing (APPROVE | COMMENT), blocking when it finds a blocking issue
    (REQUEST_CHANGES | COMMENT). Each comes from the repository's [[repo]] block
    when it sets it, else from the identity, else the default: APPROVE for a gh
    identity and COMMENT for an app or an unknown one (a bot approval must not
    unlock a merge by accident), and REQUEST_CHANGES.

func (c *Config) Warnings() []string
    Warnings lists human-readable problems that do not make the config invalid:
    zero [[identity]] or zero [[watch]] blocks are valid (the built-in defaults
    have neither; they live in the user's ~/.config/magnum/config.toml), but
    magnum then cannot post or reviews nothing. Print them from `magnum doctor`
    and `magnum config`.

func (c *Config) WatchFor(fullName string) *Watch
    WatchFor returns the watch that covers owner/name, or nil.

type CurateTriggers []string
    CurateTriggers is [notes] curate: the triggers of a curation, written as
    a list (`["over_limit", "misses"]`, `[]` for none). The earlier string
    form still reads as it meant: "over_limit" that trigger alone, "weekly"
    over_limit and weekly, "off" none.

func (c CurateTriggers) Has(t string) bool
    Has reports whether trigger t is on.

func (c CurateTriggers) String() string
    String is the list as the config writes it.

func (c *CurateTriggers) UnmarshalTOML(v any) error
    UnmarshalTOML reads a list of trigger names or one of the earlier strings.
    Values are checked by Validate.

type Daemon struct {
	PollInterval             Duration `toml:"poll_interval"`
	MaxConcurrentReviews     int      `toml:"max_concurrent_reviews"`
	MaxTotalWorkingCodex     int      `toml:"max_total_working_codex"`
	PushQuietPeriod          Duration `toml:"push_quiet_period"`
	MinRereviewInterval      Duration `toml:"min_rereview_interval"`
	DraftMinRereviewInterval Duration `toml:"draft_min_rereview_interval"`
	MaxRoundsPerPRPerDay     int      `toml:"max_rounds_per_pr_per_day"`
	CloseGrace               Duration `toml:"close_grace"`
	ReviewerTimeout          Duration `toml:"reviewer_timeout"` // default timeout of non-judge roles (a role's own timeout wins)
	JudgeTimeout             Duration `toml:"judge_timeout"`    // default timeout of judge roles
	AgentStartStagger        Duration `toml:"agent_start_stagger"`
	MinWarm                  Duration `toml:"min_warm"`
	HumanCooldown            Duration `toml:"human_cooldown"`
	ReconcileInterval        Duration `toml:"reconcile_interval"`
	DefaultRepo              string   `toml:"default_repo"`
	QuietHours               string   `toml:"quiet_hours"` // "01:00-07:00" local, optional
	MinFreeDiskGB            int      `toml:"min_free_disk_gb"`
	// KeepEvents and KeepRequests are how long the daemon keeps audit events
	// and handled CLI requests (0 = forever); it prunes older rows on every
	// reconcile.
	KeepEvents   Duration `toml:"keep_events"`
	KeepRequests Duration `toml:"keep_requests"`
	// MaxRoundRestarts bounds how often one round starts its reviewers over
	// on a head pushed while they run (before the judge is prompted); later
	// pushes leave the round on the head it has. 0 = never restart.
	MaxRoundRestarts int `toml:"max_round_restarts"`
	// BurstQuietPeriod replaces PushQuietPeriod (when longer) for a PR whose
	// head changed BurstPushes times or more within BurstWindow up to its
	// last push: an author pushing in a burst gets a longer quiet period
	// before the next round. A zero value turns the rule off. A [[watch]]
	// may override each (Config.ThrottleFor).
	BurstQuietPeriod Duration `toml:"burst_quiet_period"`
	BurstPushes      int      `toml:"burst_pushes"`
	BurstWindow      Duration `toml:"burst_window"`
	// ModelLimitCooldown is how long a model that hit its own limit
	// (health pattern model_limit) counts as limited when the pane text
	// names no reset time; sessions use the kind's fallback_models meanwhile.
	ModelLimitCooldown Duration `toml:"model_limit_cooldown"`
	// ParkIdleAfter parks the live agents of a reviewed PR once all of them
	// have been idle this long (they resume on the PR's next round); 0 = never.
	// A PR waiting for a round is parked the same way when its wait is a
	// pause, a drain, the daily cap or ends more than this far away.
	// Pinned PRs and PRs with human activity within HumanCooldown are left alone.
	ParkIdleAfter Duration `toml:"park_idle_after"`
	// SkipTrivialDeltas are the kinds of change a push may consist of
	// without a re-review (TrivialDeltaClasses: comments, whitespace,
	// docs, base; default all four, [] = re-review every push): the review
	// of the earlier commit then stands for the new head. A [[watch]] may
	// override it (Config.TrivialDeltas).
	SkipTrivialDeltas []string `toml:"skip_trivial_deltas"`
	// RequestDebounce is how long a review round waits after a review
	// request for the poll login or a posting identity (or a team in a
	// watch's request_teams), or after a draft became ready for review,
	// counted from the later of that and the last push. Such a requested
	// round skips every other timing rule and the daily cap; 0 = no wait.
	RequestDebounce Duration `toml:"request_debounce"`
	// RereviewMinLines is the smallest unreviewed delta an automatic
	// re-review runs for after the quiet period: changed lines (additions
	// plus deletions) the trivial-delta classifier counts as code, since the
	// reviewed commit (after a push that merged the base branch or rebased,
	// the change in the PR's own diff against its base). A smaller delta
	// without an added file waits for more pushes or RereviewMaxWait since
	// its first push, whichever is first.
	// 0 = no threshold. A [[watch]] may override both (Config.ThrottleFor).
	RereviewMinLines int      `toml:"rereview_min_lines"`
	RereviewMaxWait  Duration `toml:"rereview_max_wait"`
	// DeltaCheck (default true): a re-review whose delta since the reviewed
	// commit has more than 0 and fewer than RereviewMinLines changed code
	// lines and adds no file (modified binary files count 0 lines) does not
	// wait for RereviewMaxWait: after the quiet period only the judge
	// checks those commits, in its own session at its rereview_effort, and
	// an App's approval stands meanwhile (for at most an hour after the
	// push). false keeps the threshold's wait and a full round. A [[watch]]
	// may override it (Config.ThrottleFor).
	DeltaCheck bool `toml:"delta_check"`
	// RestartOnNewBuild lets the daemon restart itself on a new binary
	// on disk (the one launchd starts) once it passes its configuration
	// check: at the first tick no round is claiming, reviewing or
	// verifying, it exits for launchd to start the new build. Dispatch never
	// stops for it. Needs launchd (`magnum install`).
	RestartOnNewBuild bool `toml:"restart_on_new_build"`
}

type Duration struct{ time.Duration }
    Duration is a time.Duration that unmarshals from Go syntax ("30s", "5m",
    "2h") with whole days allowed in front ("30d", "1d12h").

func (d *Duration) UnmarshalText(b []byte) error

type FooterData struct {
	SHA    string // the reviewed commit
	Short  string // its first 10 characters
	Repo   string // owner/name
	Number int    // the PR's number
	Login  string // the login that posted the review
	// Simplify: the PR's watch runs a role answering to the alias
	// "simplify", so the review may carry optional simplifications.
	Simplify bool
	// Clean: the review has no findings (still-open earlier ones included)
	// and no simplifications, by the judge's result counts.
	Clean      bool
	Event      string // APPROVE, COMMENT or REQUEST_CHANGES
	PostMerge  bool   // a review of commits GitHub merged before magnum reviewed them
	DeltaCheck bool   // the judge alone checked a small delta since its last review
}
    FooterData is what a review_footer template renders with: the verified
    review magnum appends the footer to and the round that posted it.

type GitHub struct {
	// Transport is "gh" (default: requests run as `gh api --include`, so only
	// the signed gh binary talks to GitHub, which outbound firewalls such as
	// Little Snitch already allow) or "direct" (Go net/http).
	Transport string `toml:"transport"`
}
    GitHub tunes how magnum reaches the GitHub REST API itself (App JWT and
    installation-token calls; everything else already runs through gh).

type HealthPatterns struct {
	LoginRequired []string `toml:"login_required"` // pause the kind until the human logs in
	ModelLimit    []string `toml:"model_limit"`    // switch the session to a fallback model (see Kind.FallbackModels)
	UsageLimit    []string `toml:"usage_limit"`    // pause until the reset time the text names
	Overloaded    []string `toml:"overloaded"`     // retry with backoff
}
    HealthPatterns are regular expressions (RE2, matched case-insensitively)
    that classify an agent pane's recent output. Setting one list replaces that
    list only.

    ModelLimit patterns name a cap on one model while the account still has
    usage left ("You've reached your Fable limit"). They are checked before
    UsageLimit; a named group "model" captures the model, else the session's
    current model is the limited one.

func DefaultHealthPatterns() HealthPatterns
    DefaultHealthPatterns returns the classifier patterns magnum has always used
    for Codex and Claude (a fresh copy); every kind starts from them.

func (h HealthPatterns) Compile() (HealthRegexps, error)
    Compile compiles every pattern with the (?i) flag.

type HealthRegexps struct {
	LoginRequired, ModelLimit, UsageLimit, Overloaded []*regexp.Regexp
}
    HealthRegexps are compiled HealthPatterns.

type Herdr struct {
	Socket           string `toml:"socket"`
	Notify           bool   `toml:"notify"`
	ToastEveryReview bool   `toml:"toast_every_review"`
}

type Identity struct {
	Name            string `toml:"name"`
	Kind            string `toml:"kind"` // gh | app
	Login           string `toml:"login"`
	AppID           int64  `toml:"app_id"`
	ClientID        string `toml:"client_id"`
	InstallationID  int64  `toml:"installation_id"`
	PrivateKeyEnv   string `toml:"private_key_env"`   // the env var holding the PEM (text or a path)
	PrivateKeyFile  string `toml:"private_key_file"`  // the PEM file (~ and paths relative to the user config's directory resolved); wins over private_key_env
	NoFindingsEvent string `toml:"no_findings_event"` // APPROVE | COMMENT
	BlockingEvent   string `toml:"blocking_event"`    // REQUEST_CHANGES | COMMENT
	DismissOwnStale *bool  `toml:"dismiss_own_stale_change_requests"`
	// ReviewFooter is the template of the footer magnum appends to the
	// identity's reviews, for the PR's author (nil = DefaultReviewFooter,
	// "" = none); see Footer and FooterData.
	ReviewFooter *string `toml:"review_footer"`
}

func (i Identity) DismissStale() bool
    DismissStale reports whether the identity dismisses its own stale
    CHANGES_REQUESTED.

func (i Identity) Footer() string
    Footer is the identity's footer template, which magnum renders
    (RenderFooter) and appends to every review the identity posts once it is
    verified: review_footer, trimmed, else DefaultReviewFooter; "" = no footer.

type Kind struct {
	// Start: extra args always appended.
	Start []string `toml:"start"`
	// Args: appended only when the command is not a wrapper (wrapper
	// "false", or "auto" found a plain binary); the old [codex]/[claude] args.
	Args []string `toml:"args"`
	// Resume: args that resume session {session}, e.g. ["resume", "{session}"].
	Resume []string `toml:"resume"`
	// Model: args that select model {model}; empty = the kind cannot pick a
	// model (a role setting model, or DefaultModel, is then invalid).
	Model []string `toml:"model"`
	// DefaultModel: the model of the kind's roles that set none, passed
	// through Model (e.g. "gpt-6.1-sol" for codex); "" = the CLI's own
	// default.
	DefaultModel string `toml:"default_model"`
	// Effort: args that set reasoning effort {effort}; empty = the effort
	// reaches prompts only (Role.Effort is a template variable either way).
	Effort []string `toml:"effort"`
	// Name: args that name the session {title} at launch (claude --name).
	Name []string `toml:"name"`
	// Rename: a slash command typed while the agent works to (re)name its
	// session, e.g. "/rename {title}" (Codex has no name flag); "" = none.
	Rename string `toml:"rename"`
	// LoginCheck: a read-only command proving the CLI is logged in, split on
	// spaces (no shell, no quoting), e.g. "codex login status"; "" = none.
	LoginCheck string `toml:"login_check"`
	// LoginOK: how LoginCheck's output reads as logged in (see LoggedIn):
	// "text:<substring>", "regex:<expr>", "json:<dotted.path>" (a boolean in
	// the first JSON object of stdout) or "" (exit status 0).
	LoginOK string `toml:"login_ok"`
	// Wrapper: "auto" (default), "true" or "false" (see WrapperAuto).
	Wrapper string `toml:"wrapper"`
	// Env: extra environment for the kind's panes (a role's env wins).
	Env map[string]string `toml:"env"`
	// HealthPatterns classify the pane's output (the trust-dialog, approval
	// and stalled patterns stay built in).
	HealthPatterns HealthPatterns `toml:"health_patterns"`
	// SessionSource: "herdr" (default) or "none" (see SessionHerdr).
	SessionSource string `toml:"session_source"`
	// OnPermissionPrompt: what magnum does when the agent stops at a
	// permission (approval) prompt during one of magnum's runs: "deny"
	// (default) answers No, "wait" leaves it for the human (see
	// PermissionDeny). The first-launch folder-trust dialog is handled
	// separately either way.
	OnPermissionPrompt string `toml:"on_permission_prompt"`
	// OnHooksReview: how magnum answers Codex's startup hooks review (hooks
	// new or changed since Codex last trusted them): "trust_own" (default)
	// trusts them when the checkout declares no hooks of its own, so all are
	// the user's, and declines them otherwise; "decline" always declines
	// (see HooksTrustOwn). Only a kind whose CLI shows that dialog uses it.
	OnHooksReview string `toml:"on_hooks_review"`
	// AfterDenyPrompt: the message sent (once per denied prompt, through
	// herdr agent.prompt, within the same run) when an agent whose
	// permission prompt magnum denied stops its turn and goes idle, so it
	// finishes the task without the command (Claude Code ends its turn on a
	// No). Default DefaultAfterDenyPrompt; "" sends nothing.
	AfterDenyPrompt string `toml:"after_deny_prompt"`
	// SwitchModel: the slash command typed into an idle agent's pane to
	// switch its session to model {model} (claude: "/model {model}"); "" =
	// the kind cannot switch in-session, so a per-model limit (health
	// pattern model_limit) pauses the kind like a usage limit.
	SwitchModel string `toml:"switch_model"`
	// FallbackModels: the models a session switches to, in order, when its
	// model hits a per-model limit (claude: ["opus", "sonnet"]); unused
	// without SwitchModel, so switch_model = "" alone turns switching off.
	// Empty = no fallback.
	FallbackModels []string `toml:"fallback_models"`
	// ResetModel: the SwitchModel argument that returns a session to the
	// CLI's own default model once the limit is over, for a role (and kind)
	// without a model (claude: "default"); "" = such a session stays on its
	// fallback.
	ResetModel string `toml:"reset_model"`
}
    Kind is a [kinds.<name>] block: how magnum drives one agent CLI that
    herdr knows by that name (`herdr agent start --kind <name>`). User blocks
    are merged key by key onto DefaultKinds (a key that is not set keeps the
    default); a name without a default starts from DefaultHealthPatterns,
    wrapper "auto" and session_source "herdr". KindSpec returns the result.

    herdr types the command into the pane's shell, so the CLI may be a zsh
    wrapper function or alias that adds its own flags. The launch argv is built
    by Argv in this order: Resume (resumed sessions only), Name (when a title
    is known), Model (the role's model, else DefaultModel) and Effort (when the
    role sets it), Start, Args (only without a wrapper), then the role's args.

func (k Kind) Argv(a LaunchArgs) []string
    Argv builds the args after the command name (herdr shell-quotes each one):
    Resume, Name, Model (a.Model, else DefaultModel), Effort, Start, Args (no
    wrapper only), then a.Extra. A group whose value is empty is skipped;
    placeholders are replaced in every element.

func (k Kind) LoggedIn(stdout, stderr string, exitOK bool) (loggedIn, readable bool)
    LoggedIn reads LoginCheck's output per LoginOK. exitOK is whether the
    command exited 0. readable is false when the output could not be read (json:
    no JSON object in stdout, or the path is not a boolean); the caller then
    decides between "not logged in" and "the check could not run".

        text:<s>   loggedIn = stdout+stderr contains s (case-sensitive)
        regex:<re> loggedIn = stdout+stderr matches re
        json:<p>   loggedIn = the boolean at dotted path p of stdout's first JSON object
        ""         loggedIn = exitOK

func (k Kind) LoginArgv() []string
    LoginArgv splits LoginCheck on spaces: the command name and its args (nil
    when the kind has no login check).

func (k Kind) RenameCommand(title string) string
    RenameCommand is Rename with {title} replaced ("" when the kind has none).

func (k Kind) SwitchModelCommand(model string) string
    SwitchModelCommand is SwitchModel with {model} replaced ("" when the kind
    cannot switch or model is empty).

type LaunchArgs struct {
	Session string   // session id to resume; "" = a fresh session (Resume is skipped)
	Title   string   // the pane title; "" skips Name
	Model   string   // Role.Model; "" = the kind's DefaultModel, and Model is skipped when that is "" too
	Effort  string   // Role.Effort; "" skips Effort
	Wrapper bool     // the command is a wrapper function (Args are skipped)
	Extra   []string // Role.Args, appended last as given
}
    LaunchArgs are the per-start values Kind.Argv substitutes.

type Learn struct {
	// Enabled turns the daily schedule on; off by default (it spends a model
	// turn per candidate PR). `magnum retro` runs regardless.
	Enabled bool `toml:"enabled"`
	// DailyAt is the local time of day, "HH:MM", after which the daily retro
	// runs once.
	DailyAt string `toml:"daily_at"`
	// Lookback: PRs merged or closed within it are candidates.
	Lookback Duration `toml:"lookback"`
	// Settle: the daily retro and a plain `magnum retro` take a PR only once
	// it was closed or merged at least this long ago, so the reviews posted
	// after it closed are in (0 = at once). `magnum retro <ref>` ignores it.
	Settle Duration `toml:"settle"`
	// MaxPRs bounds the PRs classified per retro, newest closed first.
	MaxPRs int `toml:"max_prs"`
	// MinCommentChars: comments shorter than this are dropped.
	MinCommentChars int `toml:"min_comment_chars"`
	// IncludeBots counts bot accounts other than magnum's own as reviewers.
	IncludeBots bool `toml:"include_bots"`
	// Kind is the classifying agent's [kinds.<name>]; Model, Effort and Args
	// are passed to it like a role's (LearnRole). Model "" is the kind's own
	// default (its default_model, else the CLI's). A config that names a kind
	// but no model gets DefaultLearnModel(kind): sonnet for claude, "" for any
	// other kind, whose CLI would not know a Claude model name.
	Kind   string   `toml:"kind"`
	Model  string   `toml:"model"`
	Effort string   `toml:"effort"`
	Args   []string `toml:"args"`
	// Prompt is the template file (Config.ResolvePrompt); see prompts/README.md.
	Prompt string `toml:"prompt"`
	// Timeout bounds the agent's turn on one PR.
	Timeout Duration `toml:"timeout"`
}
    Learn is the [learn] section: the daily retro. After a PR closes,
    magnum collects what the other reviewers commented on it, drops what its own
    review already posted, and has an interactive agent classify the rest; the
    real misses go to the registry's misses table (`magnum misses` lists them).
    Enabled runs it once a day after DailyAt; `magnum retro` runs one whether
    or not it is enabled (internal/engine/retro.go, DECISIONS "Learning loop:
    daily retro").

func DefaultLearn() Learn
    DefaultLearn returns the built-in [learn] values: off, a week of lookback,
    a day to settle, Claude sonnet classifying.

func (l Learn) DailyTime(day time.Time) (time.Time, error)
    DailyTime is the daily_at time on the local calendar day of day (in day's
    location). daily_at must be "HH:MM", 00:00 to 23:59, two digits each.

type LoadOptions struct {
	// NoOverlay skips the user layer (the user config) even when it exists,
	// so the result depends on the base alone.
	NoOverlay bool
}
    LoadOptions tunes LoadWithOptions.

type Notes struct {
	MaxBytes        int64 `toml:"max_bytes"`
	MaxLine         int   `toml:"max_line"`
	MaxHarnessFiles int   `toml:"max_harness_files"`
	MaxHarnessBytes int64 `toml:"max_harness_bytes"`
	// Curate lists the curation triggers (CurateOverLimit, CurateWeekly,
	// CurateMisses); empty: only on demand.
	Curate CurateTriggers `toml:"curate"`
	// Kind is the curator's [kinds.<name>]; a config that names a kind but no
	// model gets DefaultLearnModel(kind), as [learn] does.
	Kind   string   `toml:"kind"`
	Model  string   `toml:"model"`
	Effort string   `toml:"effort"`
	Args   []string `toml:"args"`
	// Prompt is the curator's template file (Config.ResolvePrompt).
	Prompt string `toml:"prompt"`
	// Timeout bounds the curator's turn.
	Timeout Duration `toml:"timeout"`
}
    Notes is the [notes] section: the repository notes every review role
    reads first and the judge rewrites (engine.NotesPath). The four limits are
    curation triggers, not caps: after each judge round (and at startup) magnum
    measures the notes and the harness directory of QA scripts beside them,
    and a repository past any limit is marked for curation (a notes.over_limit
    event). Nothing blocks a review, and a curated proposal may stay above a
    limit when what it keeps helps future reviews. Curate lists what starts a
    curation besides `magnum notes <repo> --curate`: a repository marked past
    a limit, once a week, or a retro that recorded misses of the repository for
    its notes. The curator is an interactive agent like the retro's ([learn]):
    Kind, Model, Effort and Args as a role's (NotesRole).

func DefaultNotes() Notes
    DefaultNotes returns the built-in [notes] values.

type Pipeline struct {
	// PromptsDir holds the editable prompt files; default "{{repo}}/prompts"
	// ({{repo}} = magnum's home; Load expands ~ and makes a relative path
	// relative to the home). A prompt name resolves to <PromptsDir>/<name>
	// when that file exists, else to the embedded default of the same name
	// (package prompts); see ResolvePrompt. A missing directory leaves only
	// the embedded defaults.
	PromptsDir string `toml:"prompts_dir"`
	// JudgeOwnPass is when the judge does its own pass of a round:
	// OwnPassParallel (default) prompts it for that pass together with the
	// reviewer roles (its own-pass prompt, Role.OwnPass) and for the
	// candidates once both ended; OwnPassAfter prompts it once, after the
	// reviewers, for both. A [[watch]] may override it (Watch.JudgeOwnPass);
	// see Config.JudgeOwnPassFor.
	JudgeOwnPass string `toml:"judge_own_pass"`
	// RelatedLookback is how long a merged PR stays related: the judge's
	// related.json lists the open PRs of the repository and those merged
	// within it that change the same paths (default 14 days; 0 = open PRs
	// only). A [[watch]] may override it (Watch.RelatedLookback).
	RelatedLookback Duration `toml:"related_lookback"`
	// RelatedIgnore are path globs (see MatchPath) whose paths never make
	// two PRs related (default DefaultRelatedIgnore, the lockfiles; [] =
	// none). A [[watch]] may override it (Watch.RelatedIgnore).
	RelatedIgnore []string `toml:"related_ignore"`
}
    Pipeline is the [pipeline] section.

type Pool struct {
	Repo            string            `toml:"repo"` // owner/name
	MainClone       string            `toml:"main_clone"`
	SlotName        string            `toml:"slot_name"` // review{n}
	SlotPath        string            `toml:"slot_path"` // ~/Projects/talkable.review{n}
	Base            string            `toml:"base"`
	Min             int               `toml:"min"`
	Max             int               `toml:"max"`
	IdleRemoveAfter Duration          `toml:"idle_remove_after"`
	MinFreeDiskGB   int               `toml:"min_free_disk_gb"`
	CopyFiles       []string          `toml:"copy_files"`
	StripEnv        []string          `toml:"strip_env"`
	Setup           []string          `toml:"setup"`
	Teardown        []string          `toml:"teardown"`
	SchemaPaths     []string          `toml:"schema_paths"`
	ResetDB         []string          `toml:"reset_db"`
	PostCheckout    []string          `toml:"post_checkout"`
	Databases       []string          `toml:"databases"`
	Env             map[string]string `toml:"env"`
	// ResetDBOnSchemaChange runs ResetDB before the reviewers of a round
	// whose checkout's files under SchemaPaths differ from those the slot's
	// databases were last loaded from (slots.CheckSchema), as the first
	// commands of the readiness step (see Prepare), so they carry the PR's
	// schema; they have the release's reset_db budget of their own, and
	// ReadyTimeout starts after them. The release then keeps the databases
	// as they are; false loads the base schema at release instead, as
	// before. nil = true; read it through ResetsDBOnSchemaChange.
	ResetDBOnSchemaChange *bool `toml:"reset_db_on_schema_change"`
	// Prepare and Ready make a round's checks work: before the reviewers
	// start, the round runs each prepare command (`bin/rails
	// db:test:prepare`), then each ready probe (exit 0 = ready), in the
	// checkout as `zsh -lc <command>` (the login shell the agents' tools
	// use) with the slot's env, all within ReadyTimeout (default 5m). A
	// failure never stops the round: the judge's prompt lists it, so the
	// judge does not spend its time finding out what cannot run. A [[repo]]
	// block's keys of the same names replace these. See Config.ReadinessFor.
	Prepare      []string `toml:"prepare"`
	Ready        []string `toml:"ready"`
	ReadyTimeout Duration `toml:"ready_timeout"`
}

func (p Pool) DBNames(slug string) []string
    DBNames renders the database names for a slug.

func (p Pool) Path(n int) string
    Path renders the checkout path of slot n.

func (p Pool) ResetsDBOnSchemaChange() bool
    ResetsDBOnSchemaChange is ResetDBOnSchemaChange with its default (true).

func (p Pool) Slot(n int) string
    Slot renders the name of slot n.

func (p Pool) SlotEnv(slot string) map[string]string
    SlotEnv renders [pool.env] for a slot (blank values stay blank on purpose).

type Prompt struct {
	Name     string // as configured, e.g. "judge-initial.md"
	Path     string // the file read (<prompts_dir>/<name>); "" when embedded
	Embedded bool   // the embedded default (package prompts) was used
	Text     string // the template source, unrendered
}
    Prompt is a resolved prompt template.

type PromptSnapshot struct {
	LoadedAt time.Time
	// Warnings name the skills that could not be copied (a missing or
	// unreadable file): their judges keep the configured path.
	Warnings []string

	// Has unexported fields.
}
    PromptSnapshot is the prompt text and judge skills a daemon loaded once,
    at startup (Config.SnapshotPrompts), right after it checked that this build
    renders them. While a Config holds one, ResolvePrompt answers from it and
    SkillFile names the skill's copy, so a file edited on disk reaches rounds
    at the next restart, the one that checks it again (DECISIONS "Prompts are
    loaded once, at startup"). Changed tells which files differ on disk since.
    The CLI never takes one: `magnum config` and doctor read the files on disk.
    A snapshot is immutable once installed.

func (s *PromptSnapshot) Changed() []string
    Changed lists what differs on disk from the snapshot, sorted: the names of
    prompts whose file changed or disappeared, or (an embedded default) that a
    file in prompts_dir would now override, and the paths of skills whose file
    changed or disappeared. The next restart loads them.

func (s *PromptSnapshot) Files() int
    Files counts the files the snapshot holds: prompts (embedded defaults
    included) and skill copies.

type QuietWindow struct{ Start, End int }
    QuietWindow is a daemon.quiet_hours window in minutes since midnight.
    It includes Start and excludes End, and wraps past midnight when End is not
    after Start.

func ParseQuietHours(spec string) (w QuietWindow, ok bool, err error)
    ParseQuietHours reads a daemon.quiet_hours spec, a local "HH:MM-HH:MM" such
    as "01:00-07:00" (each clock "H:MM" or "HH:MM", 00:00 through 23:59, start
    and end different). A blank spec is no window: ok is false, with no error.

type Readiness struct {
	Prepare []string
	Ready   []string
	Timeout time.Duration // the budget of all of them together
}
    Readiness is a repository's verification readiness step: the commands a
    round runs in the checkout before the reviewers (see Pool.Prepare).

type Related struct {
	Lookback time.Duration // a merged PR within it is related
	Ignore   []string      // path globs that never relate two PRs
}
    Related is what decides the related PRs of a watch's PR (Config.RelatedFor).

type Repo struct {
	Repo      string            `toml:"repo"`       // owner/name
	Setup     []string          `toml:"setup"`      // after the worktree is created, before the review starts
	Teardown  []string          `toml:"teardown"`   // before the worktree is removed (failures are logged)
	WTHooks   *bool             `toml:"wt_hooks"`   // default true: use .config/wt.toml when setup/teardown are empty
	CopyFiles []string          `toml:"copy_files"` // relative to the main clone, copied when present
	StripEnv  []string          `toml:"strip_env"`  // keys dropped from the rendered .mise.local.toml
	Env       map[string]string `toml:"env"`        // {slug} {path} {clone}

	// The repository's verdicts, overriding the posting identity's (see
	// Config.VerdictsFor). Unlike the keys above, which configure per-PR
	// worktrees, these may also be set for a repository with a [[pool]].
	NoFindingsEvent string `toml:"no_findings_event"` // APPROVE | COMMENT
	BlockingEvent   string `toml:"blocking_event"`    // REQUEST_CHANGES | COMMENT

	// Verification readiness (see Pool.Prepare), with or without a
	// [[pool]]: each key that is set replaces the pool's.
	Prepare      []string `toml:"prepare"`
	Ready        []string `toml:"ready"`
	ReadyTimeout Duration `toml:"ready_timeout"`

	// KeepApprovals keeps an approval the posting App identity gave when the
	// PR gets new commits; by default magnum dismisses it until the
	// re-review posts. Overrides the watch's keep_approvals (see
	// Config.KeepApprovals).
	KeepApprovals *bool `toml:"keep_approvals"`

	// RequiredChecks are the checks a PR must pass to be ready, replacing
	// the ones GitHub requires on the default branch (rulesets or branch
	// protection, which a private repository on a free plan does not
	// expose): globs (path.Match, case-sensitive) over check run names and
	// commit status contexts, or RequiredWorkflowPrefix and a glob over
	// GitHub Actions workflow names (every check of the workflow). The board
	// shows their state (see Config.RequiredChecks).
	RequiredChecks []string `toml:"required_checks"`

	// Trackers are [board] trackers templates for this repository's PRs; an
	// issue key prefix named here wins over its watch's and [board]'s (see
	// Config.TrackersFor).
	Trackers []string `toml:"trackers"`
}
    Repo is a [[repo]] block: the repository's verdicts (any watched repository,
    pooled or not) and setup for the per-PR worktrees of a repository
    without a pool. Without a block, a per-PR worktree runs the main clone's
    .config/wt.toml hooks (worktrunk); setup or teardown commands here replace
    those hooks (both of them), and wt_hooks = false ignores them without a
    replacement. Commands run in the worktree with WT_BRANCH=magnum-pr-<N> and
    the rendered env; they are not templated (use $WT_BRANCH).

func (r Repo) HasCommands() bool
    HasCommands reports whether the block supplies its own setup or teardown.

func (r Repo) RenderEnv(slug, path, clone string) map[string]string
    RenderEnv renders [repo.env] for a per-PR worktree: {slug} is the workspace
    name (magnum-pr-<N>), {path} the worktree and {clone} the main clone.
    Blank values stay blank on purpose.

func (r Repo) WTHooksEnabled() bool
    WTHooksEnabled reports whether .config/wt.toml hooks may run (wt_hooks,
    default true).

type Role struct {
	// Name: unique, ^[a-z][a-z0-9-]{0,23}$; pane labels, titles, agent
	// names and the --role flag of magnum open/watch use it.
	Name string `toml:"name"`
	// Kind: a declared [kinds.<name>] (codex, claude, droid, omp, ...) or
	// "shell".
	Kind string `toml:"kind"`
	// Mode: "session" (agent kinds) or "shell" (kind = "shell"); derived
	// from Kind when empty, and must agree with it.
	Mode string `toml:"mode"`
	// Judge: the role that posts the review; exactly one per watch. A
	// judge is a session role with runs = "always", capture = "file" and no
	// After (it always runs last).
	Judge bool `toml:"judge"`
	// Summary is a one-line description of what the role checks, shown to the
	// triage model ([triage]; Removable). A role without one is never
	// removed from a round, and neither is the judge.
	Summary string `toml:"summary"`
	// Runs: "always" (default), "first", "manual" or "never" (see RunsAlways).
	Runs string `toml:"runs"`
	// RerunMinLines, for runs = "first": the role runs again once the code
	// lines changed since the head of its last completed run reach this
	// many (comments, blank lines, whitespace moves and documentation do
	// not count; the re-review threshold's measure). 0 = only the first
	// round, then on request. Ignored for the other runs values.
	RerunMinLines int `toml:"rerun_min_lines"`
	// Identity: the [[identity]] whose GitHub env the role's pane gets;
	// "" = the watch's identity.
	Identity string `toml:"identity"`
	// Model and Effort are passed through the kind's model/effort args
	// (Kind.Argv) and are template variables ({{.Model}}, {{.Effort}}).
	Model  string `toml:"model"`
	Effort string `toml:"effort"`
	// RereviewEffort is the effort of a re-review round (a new head after
	// an earlier review); "" = Effort. The first review and a recovery keep
	// Effort. See EffortFor.
	RereviewEffort string `toml:"rereview_effort"`
	// Args: session roles append them to the CLI's launch args; shell roles
	// append them, shell-quoted, to the rendered Command.
	Args []string `toml:"args"`
	// Env: extra pane environment (wins over the kind's env).
	Env map[string]string `toml:"env"`

	// Prompt files (names resolved by Config.ResolvePrompt). Defaults:
	// judges use judge-initial.md, judge-rereview.md, judge-continue.md,
	// judge-recovery.md and judge-nudge.md; other session roles use
	// <name>.md, <name>-rereview.md (only when it exists, else Prompt) and
	// <name>-restart.md (only when it exists, else none). A shell role may
	// name a full-line shell template (codex-review.sh) in Prompt instead of
	// setting Command.
	Prompt   string `toml:"prompt"`
	Rereview string `toml:"rereview"`
	// Restart: a session reviewer whose turn a push cut short (the round
	// restarted on the new head before the judge was prompted); "" = the
	// role gets the prompt it ran (Prompt or Rereview) on the new head.
	Restart        string `toml:"restart"`
	ContinuePrompt string `toml:"continue_prompt"`
	Recovery       string `toml:"recovery"`
	Nudge          string `toml:"nudge"`
	// OwnPass: the judge's own pass, prompted together with the reviewer
	// roles ([pipeline] judge_own_pass = "parallel"); judges default to
	// judge-own-pass.md. Judges only.
	OwnPass string `toml:"own_pass"`
	// Stop is ignored: magnum never sent the judge a stop prompt. The key
	// is still accepted so a config that names one (judge-stop.md, gone
	// since) loads.
	//
	// Deprecated: no prompt kind reads it.
	Stop string `toml:"stop"`

	// Skill: the judge's skill file (template variable {{.SkillPath}});
	// default DefaultSkill, expanded by Load. Judges only.
	Skill string `toml:"skill"`

	// Command: shell roles only; a Go text/template over the round's
	// variables (prompts/README.md), every value shell-quoted. magnum types
	// it wrapped as
	//
	//	[printf '\033]0;%s\007' <title>; DISABLE_AUTO_TITLE=true; ]set -o pipefail; <command> <args...>[ | tee <report>]; printf '\nMAGNUM_DONE_<run> %d\n' "$?"
	//
	// with the tee only for capture = "stdout" (see agents.ShellLine).
	// Exactly one of Command and Prompt (a full-line template that must
	// print the done marker itself) is set on a shell role.
	Command string `toml:"command"`
	// OKStatus: shell roles only; the exit statuses of Command that count
	// as a finished report (each 0..255). Empty means only 0; any other
	// status fails the role. See StatusOK.
	OKStatus []int `toml:"ok_status"`
	// Tool: shell roles only; the agent kind the command runs (codex for
	// codex-review), whose login preflight, pauses and health patterns then
	// apply to the role. "" = none. See AgentKind.
	Tool string `toml:"tool"`

	// Output: the report file name in the round's directory. Default
	// <name>.json for a judge, else <name>.md.
	Output string `toml:"output"`
	// Capture: "file" or "stdout" (see CaptureFile). Default "stdout" for
	// shell roles, else "file". "stdout" is for shell roles only; a judge
	// uses "file". No role may edit the checkout: a round resets a tree a
	// stage left modified (pipeline).
	Capture string `toml:"capture"`
	// Timeout per turn. Default daemon.judge_timeout (90m) for a judge,
	// daemon.reviewer_timeout (40m) otherwise.
	Timeout Duration `toml:"timeout"`
	// After: roles (names or aliases) that must finish before this one
	// starts; roles outside the watch's set are ignored. Load rewrites
	// aliases to names.
	After []string `toml:"after"`
	// Aliases: other names accepted for the role (old names such as judge,
	// claude, codex, simplify and the store ids such as codex_review).
	Aliases []string `toml:"aliases"`
}
    Role is a [[role]] block: one agent (or shell command) of a review round.
    After Load (or Normalize) every defaulted field below holds its resolved
    value, so consumers read fields directly.

    A round runs the watch's roles (Config.RolesFor) in Stages: non-judge
    roles in parallel unless After orders them, the judge last. Each role
    writes ReportFile in the round's report directory (paths.Layout.ReviewDir);
    the judge reads the others' reports and posts the review.

    A [[role]] in config.toml named like a built-in role (DefaultRoles) inherits
    that role's fields for every key it does not set. Declaring any [[role]] in
    the base (config.defaults.toml, or a --config file) replaces the built-in
    list; the user config [[role]] blocks merge into it by name (or are
    appended).

func DefaultRoles() []Role
    DefaultRoles returns the built-in roles in display order (fresh copies,
    before normalization; Normalize fills Mode, Runs, Output, Capture, Timeout
    and the judge's prompt names):

      - codex-judge: codex, judge, effort xhigh, rereview_effort high,
        skill DefaultSkill, prompts judge-*.md, timeout daemon.judge_timeout;
        aliases judge.
      - claude-review: claude, prompt claude-review.md, rereview
        claude-rereview.md, restart claude-restart.md, effort high,
        rereview_effort medium; aliases claude.
      - codex-review: shell, tool codex, command "command codex review --base
        {{if .BaseSHA}}{{.BaseSHA}}{{else}}{{.BaseRef}}{{end}}" (the merge base,
        else the base ref), ok_status [0], capture stdout; aliases codex,
        codex_review.
      - claude-simplify: claude, runs first, rerun_min_lines
        DefaultSimplifyRerunLines, prompt claude-simplify.md (read-only:
        it writes its proposals to claude-simplify.md), no after, so it runs in
        parallel with the reviewers; aliases simplify.

    Each non-judge role carries a Summary, which makes it a candidate for triage
    ([triage]).

func (r Role) AgentKind() string
    AgentKind is the agent CLI whose login preflight, pauses and health patterns
    apply to the role: Kind for session roles, Tool for shell roles ("" when the
    command uses none).

func (r Role) EffortFor(rereview bool) string
    EffortFor is the role's effort for a round: RereviewEffort (when set) for a
    re-review round, else Effort.

func (r Role) IsAgent() bool
    IsAgent reports whether the role runs an interactive agent session.

func (r Role) IsShell() bool
    IsShell reports whether the role runs a shell command (kind "shell").

func (r Role) Matches(s string) bool
    Matches reports whether s (case and surrounding space ignored) is the role's
    name or one of its aliases.

func (r Role) PromptFile(kind string) string
    PromptFile returns the prompt file name for a prompt kind (PromptInitial,
    PromptRereview, PromptRestart, PromptContinue, PromptRecovery, PromptNudge,
    PromptOwnPass); "" when the role has none (shell roles driven by Command,
    non-judge roles for continue/recovery/nudge/own_pass unless set, a role
    without a restart prompt, unknown kinds). Rereview falls back to the initial
    prompt.

func (r Role) Removable() bool
    Removable is whether triage may drop the role from a round: not the judge,
    and it has a Summary to show the model.

func (r Role) ReportFile() string
    ReportFile is the file the role writes in the round's report directory.

func (r Role) ShouldRun(ranBefore, requested bool) bool
    ShouldRun applies Runs: ranBefore is whether the role already completed for
    the PR, requested whether the user asked for it this round.

func (r Role) StatusOK(status int) bool
    StatusOK reports whether a shell command's exit status counts as a finished
    report: only 0 when OKStatus is empty, else any status in it.

type Terminal struct {
	App               string `toml:"app"`      // Terminal | iTerm2 | Ghostty | WezTerm | custom | generic
	Session           string `toml:"session"`  // herdr session name
	Launcher          string `toml:"launcher"` // custom launcher template with {herdr} {args} {command}
	RevealOnAttention bool   `toml:"reveal_on_attention"`
	// Mouse enables mouse support on the PR board and the status dashboard
	// (wheel, clicks, header sort, column drag, right-click menu); the m key
	// toggles it live.
	Mouse bool `toml:"mouse"`
	// Icons is the symbols the PR board and the status dashboard draw:
	// "nerd" Nerd Font icons and emoji (colored marks for states, verdicts
	// and finding priorities; needs a Nerd Font), "unicode" Unicode symbols
	// any font has (the default, also when empty), "ascii" plain ASCII.
	Icons string `toml:"icons"`
}

type Tracker struct {
	Prefix string // the issue key before the number: "DMA-"
	URL    string // the template
	// Has unexported fields.
}
    Tracker is one [board] trackers template, ready to find its issue keys.

func ParseTracker(tmpl string) (Tracker, error)
    ParseTracker reads a [board] trackers template: an http(s) URL with {num}
    once, right after the issue key's prefix.

type Triage struct {
	// Enabled turns triage on; off by default (it spends a model call per round).
	Enabled bool `toml:"enabled"`
	// MaxLines: a round whose diff changes more lines (added plus deleted)
	// than this runs every role, unasked.
	MaxLines int `toml:"max_lines"`
	// Command is the model's CLI, an argument vector: the prompt arrives on
	// its stdin and its stdout is the answer.
	Command []string `toml:"command"`
	// Timeout bounds the command; a timeout runs every role.
	Timeout Duration `toml:"timeout"`
	// Prompt is the template file (Config.ResolvePrompt); see prompts/README.md.
	Prompt string `toml:"prompt"`
}
    Triage is the [triage] section: before a round starts its agents,
    a cheap model is asked which of the round's reviewers its diff needs,
    so a one-line fix does not wake every reviewer. Rounds with more than
    MaxLines changed lines run every role without asking; the judge always runs,
    the model can only remove roles that have a Summary, and any failure runs
    every role (internal/engine/triage.go, DECISIONS "Triage of small diffs").

func DefaultTriage() Triage
    DefaultTriage returns the built-in [triage] values: off, Claude haiku with
    no tools and no session kept.

type Usage struct {
	// CodexSoft: at or above this share (percent) of the Codex budget used,
	// first reviews wait; re-reviews and forced reviews still run. 0 = off.
	CodexSoft float64 `toml:"codex_soft"`
	// CodexHard: at or above this share every kind backed by Codex pauses
	// until the budget drops below it again. 0 = off.
	CodexHard float64 `toml:"codex_hard"`
	// CodexHome is the CODEX_HOME whose sessions are read; "" = $CODEX_HOME,
	// else ~/.codex.
	CodexHome string `toml:"codex_home"`
}
    Usage is the [usage] section: subscription budgets the scheduler watches.
    Codex's budget is read from Codex's own session files (internal/usage).

type Watch struct {
	Owner               string   `toml:"owner"`
	Include             []string `toml:"include"`
	Exclude             []string `toml:"exclude"`
	Identity            string   `toml:"identity"`
	PollIdentity        string   `toml:"poll_identity"`
	CloneRoot           string   `toml:"clone_root"`
	IncludeDrafts       *bool    `toml:"include_drafts"`
	IncludeOwn          *bool    `toml:"include_own"`
	SkipBotAuthors      *bool    `toml:"skip_bot_authors"`
	SkipAuthors         []string `toml:"skip_authors"`
	SkipLabels          []string `toml:"skip_labels"`
	SkipCrossRepository *bool    `toml:"skip_cross_repository"`
	// SkipDepartedAuthors (default true): a PR from a branch of the
	// repository whose author is no longer an owner, member or collaborator
	// of it (GitHub's authorAssociation) was opened by someone who left; it
	// is not reviewed. Fork PRs are skip_cross_repository's business.
	SkipDepartedAuthors *bool `toml:"skip_departed_authors"`
	// SkipPaths are path globs ("**" matches whole directories, "*" never
	// crosses "/"; see MatchPath): a PR is not reviewed when every file it
	// changes (a rename counts both names) matches one of them. A PR whose
	// file list is unknown or longer than GitHub lists is never skipped.
	SkipPaths []string `toml:"skip_paths"`
	// Roles names the [[role]]s (names or aliases) this watch runs; empty
	// = every role. Exactly one of them must be a judge.
	Roles []string `toml:"roles"`
	// BurstQuietPeriod, BurstPushes and BurstWindow override the [daemon]
	// keys of the same names for this watch's PRs: a zero duration or an
	// unset burst_pushes keeps the daemon's, burst_pushes = 0 turns the rule
	// off for the watch. See Config.ThrottleFor.
	BurstQuietPeriod Duration `toml:"burst_quiet_period"`
	BurstPushes      *int     `toml:"burst_pushes"`
	BurstWindow      Duration `toml:"burst_window"`
	// KeepApprovals keeps the approvals an App identity posted on the
	// watch's PRs when they get new commits (default false: magnum
	// dismisses them until the re-review posts). A [[repo]] block's
	// keep_approvals overrides it.
	KeepApprovals bool `toml:"keep_approvals"`
	// Trackers are [board] trackers templates for the watch's PRs: an issue
	// key prefix named here wins over [board]'s ("PR-" in another tracker
	// than the other owners'); a [[repo]] block's wins over these. See
	// Config.TrackersFor.
	Trackers []string `toml:"trackers"`
	// SkipTrivialDeltas overrides [daemon] skip_trivial_deltas for the
	// watch's PRs: nil (unset) keeps the daemon's, [] re-reviews every push.
	SkipTrivialDeltas []string `toml:"skip_trivial_deltas"`
	// RereviewMinLines and RereviewMaxWait override the [daemon] keys of the
	// same names for this watch's PRs: unset rereview_min_lines or a zero
	// duration keeps the daemon's, rereview_min_lines = 0 turns the
	// threshold off for the watch.
	RereviewMinLines *int     `toml:"rereview_min_lines"`
	RereviewMaxWait  Duration `toml:"rereview_max_wait"`
	// DeltaCheck overrides [daemon] delta_check for this watch's PRs (unset
	// keeps the daemon's).
	DeltaCheck *bool `toml:"delta_check"`
	// JudgeOwnPass overrides [pipeline] judge_own_pass for this watch's PRs
	// ("" keeps the pipeline's; see Config.JudgeOwnPassFor).
	JudgeOwnPass string `toml:"judge_own_pass"`
	// RelatedLookback and RelatedIgnore override [pipeline]
	// related_lookback and related_ignore for this watch's PRs: a zero
	// duration or an unset related_ignore keeps the pipeline's, [] ignores
	// no path. See Config.RelatedFor.
	RelatedLookback Duration `toml:"related_lookback"`
	RelatedIgnore   []string `toml:"related_ignore"`
	// RequestTeams are team slugs whose review requests count like a
	// request for the poll login (request_debounce); other teams' do not.
	RequestTeams []string `toml:"request_teams"`
}

func (w Watch) BotsSkipped() bool

func (w Watch) CrossRepoSkipped() bool

func (w Watch) DepartedAuthorsSkipped() bool
    DepartedAuthorsSkipped is SkipDepartedAuthors with its default (true).

func (w Watch) DraftsIncluded() bool

func (w Watch) Matches(owner, name string) bool
    Matches reports whether owner/name is covered by this watch: the owner
    equals w.Owner, no Exclude glob matches the name and an Include glob does
    (path.Match globs; case is ignored throughout).

func (w Watch) OwnIncluded() bool

func (w Watch) PathsSkipped(files []string) bool
    PathsSkipped reports whether every file of a pull request matches at least
    one skip_paths glob. It is false without globs and without files: a PR whose
    changes are unknown is never skipped.

```

## eligibility

```text
package eligibility // import "github.com/zhuravel/magnum/internal/eligibility"

Package eligibility is magnum's pure decision logic: which pull requests a
watch picks up (Classify), when a picked-up PR may start its next review round
(Throttle) and whether the daemon is inside its quiet hours (QuietHours).

Nothing here does I/O, reads the clock or touches the store. Callers build a
PRFacts from their rows, pass the current time explicitly, and persist or act on
the returned decision.

CONSTANTS

const (
	// ReasonRequested starts the reason of a requested round's debounce.
	ReasonRequested = "review requested"
	// ReasonSmallDelta starts the reason of the re-review threshold.
	ReasonSmallDelta = "small delta"
)
    Reason prefixes of two rules (ThrottleDecision.Rule is the code to tell the
    rules apart by).


FUNCTIONS

func DeltaCheck(d config.Daemon, f PRFacts) bool
    DeltaCheck reports whether a re-review's delta gets a delta check,
    a judge-only round on the commits since the review, instead of a full round
    after the threshold's wait: DeltaCheck (delta_check) and the threshold
    (RereviewMinLines) are on, and the delta since the reviewed commit is
    readable (DeltaReadable), adds no file and has more than 0 and fewer
    than RereviewMinLines changed code lines. Whether the round is forced or
    requested (then it runs in full) is the caller's business.

func QuietHours(spec string, now time.Time) bool
    QuietHours reports whether now falls inside the quiet-hours window spec,
    a local "HH:MM-HH:MM" such as "01:00-07:00" (config.ParseQuietHours).
    The window includes its start and excludes its end, and wraps past midnight
    when the end is not after the start ("22:00-06:00" covers 22:00 through
    05:59:59).

    The wall-clock time is read from now itself, in now's location; pass
    time.Now() to get local quiet hours. An empty spec means no quiet hours,
    and so does an invalid one (QuietHours cannot report errors; Config.Validate
    rejects one when the config loads).


TYPES

type Decision struct {
	Eligible bool
	Reason   string // why the PR is ineligible, in words that name the config key; empty when eligible
}
    Decision is Classify's verdict.

func Classify(w config.Watch, f PRFacts) Decision
    Classify applies the watch's filters to a PR. It looks at Muted, the author
    (bots, skip_authors, own), Labels, IsDraft, IsCrossRepo and the author's
    association (skip_departed_authors), in that order, and reports the first
    rule that rejects the PR.

    Forced is deliberately not consulted: the filters describe what the daemon
    picks up on its own, and whether a manual request overrides them is the
    caller's decision.

type PRFacts struct {
	Number      int
	AuthorLogin string // GraphQL or REST form; a trailing "[bot]" is understood
	AuthorIsBot bool   // GraphQL __typename == "Bot"
	IsDraft     bool
	IsCrossRepo bool // opened from a fork
	// AuthorAssociation is GitHub's authorAssociation of the author with the
	// repository (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, …); "" = unknown.
	AuthorAssociation string
	Labels            []string
	SelfLogin         string // login of the user magnum polls as, for include_own

	HeadSHA     string // informational; no rule reads it
	ReviewedSHA string // "" until a round has been verified; selects first review vs re-review
	State       string // informational; no rule reads it

	HeadChangedAt time.Time // when the head last moved (the last push)
	// PushTimes are the PR's recent head changes (any order): Throttle
	// counts the ones within BurstWindow of the latest for the burst quiet
	// period.
	PushTimes          []time.Time
	PendingSince       time.Time // when the PR started waiting for a re-review
	LastRoundStartedAt time.Time
	RoundsToday        int // automatic rounds started on the current local day

	// RequestedAt is when a review request (or the PR's move from draft to
	// ready for review) arrived that no round has started for yet; zero =
	// none. Throttle then holds the PR only for the request debounce.
	RequestedAt time.Time

	// The unreviewed delta (ReviewedSHA...HeadSHA) for the re-review
	// threshold. DeltaKnown is false when it was not measured, or a file of
	// it had no complete patch: the threshold then never holds the PR.
	DeltaKnown      bool
	DeltaLines      int       // changed lines the trivial-delta classifier counts as code
	DeltaAddedFiles int       // files added (or renamed, copied) since the review
	DeltaSince      time.Time // the first push the review does not cover
	// DeltaReadable: the delta was measured and every file of it was read
	// in full or is a modified binary file, counted as 0 lines (what a delta
	// check needs; DeltaKnown is false with such a file).
	DeltaReadable bool

	Forced bool // manual `magnum review`: Throttle lets it through
	Muted  bool // automation stopped for this PR: Classify rejects it
	Pinned bool // slot/session hold; informational, neither decision reads it
}
    PRFacts is the snapshot of one pull request that the decisions need.
    Zero time values mean "unknown" or "never".

type Rule string
    Rule is a timing rule of Throttle (ThrottleDecision.Rule).

const (
	RuleRequested     Rule = "requested"      // a requested round's debounce
	RuleQuiet         Rule = "quiet"          // the push quiet period
	RuleBurst         Rule = "burst"          // the longer quiet period after a burst of pushes
	RuleInterval      Rule = "interval"       // the minimum re-review interval since the last round
	RuleDraftInterval Rule = "draft_interval" // the same for a draft
	RuleCap           Rule = "cap"            // the daily round cap
	RuleSmallDelta    Rule = "small_delta"    // the re-review threshold
)
    The rules Throttle applies.

type ThrottleDecision struct {
	Ready bool
	// NextEligibleAt is the earliest time every timing rule is satisfied. When
	// Ready it equals the now that was passed in, never the zero time.
	NextEligibleAt time.Time
	// Reason names the rule that is holding the PR back (the one that clears
	// last); empty when Ready.
	Reason string
	// Rule is that rule as a code, for callers that tell the rules apart
	// (the engine's wait reasons); empty when Ready.
	Rule Rule
}
    ThrottleDecision is Throttle's verdict.

func Throttle(d config.Daemon, f PRFacts, now time.Time) ThrottleDecision
    Throttle decides whether a PR that is already eligible may start a review
    round at now. It only looks at timing: Muted, filters and quiet hours are
    other decisions. HeadChangedAt, PendingSince and LastRoundStartedAt are
    compared against now; the daily cap day is the calendar day of now in its
    own location, so pass a local time.

      - First review (ReviewedSHA == ""): ready once the head has been quiet
        for PushQuietPeriod, i.e. HeadChangedAt + PushQuietPeriod <= now.
        The re-review interval and the daily cap do not apply.
      - Burst: when at least BurstPushes of PushTimes fall within BurstWindow
        of the latest one, the quiet period is BurstQuietPeriod instead (when
        longer). A zero BurstPushes, BurstWindow or BurstQuietPeriod turns the
        rule off.
      - Re-review: the same quiet period (counted from the later of
        HeadChangedAt and PendingSince), and LastRoundStartedAt +
        MinRereviewInterval (DraftMinRereviewInterval for drafts; zero =
        MinRereviewInterval) <= now, and RoundsToday < MaxRoundsPerPRPerDay.
        A cap of zero or less means no cap; a daily cap that is reached holds
        the PR until the next local midnight.
      - Small delta (re-review only): while the unreviewed delta is known
        (DeltaKnown), adds no file and has fewer than RereviewMinLines
        changed lines, the PR waits until DeltaSince + RereviewMaxWait;
        a later push that reaches the threshold is measured again by the caller.
        A zero RereviewMinLines turns the rule off, and so does a delta that
        gets a delta check (DeltaCheck): it is cheap, so it does not wait.
      - Requested (RequestedAt set): every rule above is skipped; the PR waits
        only RequestDebounce after the later of RequestedAt and HeadChangedAt.
      - Forced bypasses all of it: always ready.

    A zero timestamp or zero duration never blocks. When several rules hold
    the PR back, NextEligibleAt is the latest of them and Reason names that one
    (ties go to quiet period, then interval, then cap, then small delta).

```

## engine

```text
package engine // import "github.com/zhuravel/magnum/internal/engine"

Package engine is magnum's daemon: one tick loop that polls GitHub, observes
herdr, applies health pauses, consumes CLI requests, dispatches review rounds
into slots (each round in its own goroutine), releases closed PRs after their
grace and reconciles the registry with disk, MySQL and herdr. Slow slot work
(provisioning, eviction, cleanup, reconcile) runs one job at a time on a
background heavy worker that reports only through store rows.

The engine owns the PR state machine on top of the store's compare-and-set
transitions; eligibility.Throttle is the single source of truth for re-review
timing (prs.next_eligible_at, prs.pending_since), and store.Candidates only
backstops it.

CONSTANTS

const (
	ReqAbort  = "abort"
	ReqIgnore = "ignore"
)
    Request kinds of `magnum abort` and `magnum ignore` (TargetPayload with a
    PR): see requestAbort.

const (

	// BudgetPauseReason is the tool-pause reason of a kind paused at the
	// Codex hard cap ([usage] codex_hard).
	BudgetPauseReason = "budget_cap"
)
const (
	// KVDaemonBuild is the running daemon's Build as JSON, written at
	// startup.
	KVDaemonBuild = "daemon.build"
)
const (
	// ReqNotesCurate starts a curation of a repository's notes now
	// (NotesCuratePayload).
	ReqNotesCurate = "notes_curate"

	// Curation triggers (notes_proposals.trigger_reason).
	CurateTriggerOverLimit = config.CurateOverLimit
	CurateTriggerWeekly    = config.CurateWeekly
	CurateTriggerMisses    = config.CurateMisses
	CurateTriggerRequest   = "request"

	// ProposalTTL is how long a proposal waits for the operator before it
	// expires.
	ProposalTTL = 7 * 24 * time.Hour
	// NotesLockWait is how long a curation's copy and an apply wait for the
	// notes lock, as long as a judge waits for it.
	NotesLockWait = 3 * time.Minute
)
const (
	// KVNotesCurating holds the curation running now (CurateMark as JSON),
	// deleted when it ends and at every start of the daemon.
	KVNotesCurating = "notes.curating"
	// KVNotesCurateQueue holds the curations waiting for a start
	// ([]CurateQueued as JSON, oldest first, one per repository).
	KVNotesCurateQueue = "notes.curate_queue"

	// CurateTriggerStale is the daemon's follow-up of a stale proposal
	// (notes_proposals.trigger_reason).
	CurateTriggerStale = "stale"

	// Why a curation waits (CurateQueued.Why).
	QueuedJudge = "judge" // a round of the repository is in its judge stage
	QueuedBusy  = "busy"  // another curation runs
)
const (
	KVDaemonPausedAt   = "daemon.paused_at"
	KVDaemonPausedHeld = "daemon.paused_held"
)
    KVDaemonPausedAt holds when the running `magnum pause` began
    (store.FormatTime); a pause renewed while it runs keeps it.
    KVDaemonPausedHeld is how many review requests the pause holds
    (pauseHeldRequest), rewritten by every tick while paused. Both go with the
    pause.

const (
	// KVPromptsLoadedAt is when the running daemon loaded its prompts
	// (store.FormatTime). KVPromptsChanged counts the prompt and skill files
	// that differ on disk since (absent when none) and KVPromptsChangedFiles
	// names them, comma-separated. See PromptsLine.
	KVPromptsLoadedAt     = "daemon.prompts_loaded_at"
	KVPromptsChanged      = "daemon.prompts_changed"
	KVPromptsChangedFiles = "daemon.prompts_changed_files"
)
const (
	RefundStopped     = "stopped"      // a shutdown, restart, drain or abort stopped the round
	RefundUnprompted  = "unprompted"   // the round failed before any agent was prompted
	RefundRender      = "render"       // a prompt did not render (a prompt edited for another build)
	RefundModelLimits = "model_limits" // every fallback model was limited too: the kind paused
	RefundInfra       = "infra"        // an infrastructure failure paused dispatch
)
    Refund reasons (the round.refunded event's data).

const (
	ReqReview  = "review"
	ReqRelease = "release"
	ReqPin     = "pin"
	ReqUnpin   = "unpin"
	ReqMute    = "mute"
	ReqUnmute  = "unmute"
	ReqCleanup = "cleanup"
	ReqKick    = "kick"
	ReqPause   = "pause"
	ReqResume  = "resume"
	// ReqOpen restores a parked PR for a human (magnum open): see
	// OpenPayload.
	ReqOpen = "open"
	// ReqProvision, ReqRepair and ReqAdopt are `magnum slots provision |
	// repair | adopt` handed to the running daemon (heavy worker).
	ReqProvision = "provision"
	ReqRepair    = "repair"
	ReqAdopt     = "adopt"
	// ReqIdentityVerdict records a `magnum identities check` verdict
	// (IdentityVerdictPayload) as the daemon records its own checks.
	ReqIdentityVerdict = "identity_verdict"
	// ReqRetro starts a retro now (RetroPayload, retro.go).
	ReqRetro = "retro"
)
    Request kinds (requests.kind) the daemon consumes.

const (
	// KVRetroDay is the local day (store.DayKey) of the last daily retro
	// that finished; the next one waits for another day.
	KVRetroDay = "learn.retro_day"
	// KVRetroLast is the last retro's RetroSummary (JSON).
	KVRetroLast = "learn.retro_last"
)
const (
	// LearnAgentTag tags the retro's agent (agents.Deps.Tag), as "eval"
	// tags a replay's.
	LearnAgentTag = "learn"
)
const (
	DeltaComments   = "comments"
	DeltaWhitespace = "whitespace"
	DeltaDocs       = "docs"
	// DeltaBase is a push that only merged the base branch into the PR (or
	// rebased it onto the base) and left the PR's own diff against its base
	// as it was (base_merge.go). TrivialDelta never returns it: checkDelta
	// does, from the PR's own diff.
	DeltaBase = "base"
)
    Delta classes ([daemon] skip_trivial_deltas).

const (

	// KVDaemonPaused is "1" while automation is paused (magnum pause);
	// KVDaemonPausedReason and KVDaemonPausedUntil (optional) explain it.
	KVDaemonPaused       = store.KVDaemonPaused
	KVDaemonPausedReason = store.KVDaemonPausedReason
	KVDaemonPausedUntil  = store.KVDaemonPausedUntil
)
    kv keys the engine maintains (never secrets). The names live in store
    (store.KV*) so the CLI reads exactly what the daemon writes.

const (
	// KVDaemonDraining is set (to the time it started) by `magnum
	// daemon-restart --drain`: no new round starts; the restart, or the
	// command giving up, deletes it.
	KVDaemonDraining = "daemon.draining"
	// KVInfraPausedUntil holds when the infrastructure probe runs next while
	// dispatch is paused for an infrastructure failure (infra.go);
	// KVInfraPausedReason names the cause, KVInfraPausedDetail is the
	// failure's redacted text and KVInfraBackoff the current wait.
	KVInfraPausedUntil  = "daemon.infra_paused_until"
	KVInfraPausedReason = "daemon.infra_paused_reason"
	KVInfraPausedDetail = "daemon.infra_paused_detail"
	KVInfraBackoff      = "daemon.infra_backoff"

	// KVUsageCodexPercent is the Codex budget used (percent, no decimals)
	// as the newest Codex session reported it; KVUsageCodexResetsAt is when
	// the binding window resets (store.FormatTime), KVUsageCodexWindow its
	// length in minutes, KVUsageCodexPlan the plan and KVUsageCodexAt when
	// Codex reported it (budget.go).
	KVUsageCodexPercent  = "usage.codex_percent"
	KVUsageCodexResetsAt = "usage.codex_resets_at"
	KVUsageCodexWindow   = "usage.codex_window_minutes"
	KVUsageCodexPlan     = "usage.codex_plan"
	KVUsageCodexAt       = "usage.codex_at"
)
    kv keys of the daemon's own operations, read by `magnum status`.

const (
	ReqApprove        = "approve"
	ReqRequestChanges = "request_changes"
)
    Manual verdicts: `magnum approve` and `magnum request-changes` (and
    the board's A and C) post the reviewer's own verdict on the head magnum
    reviewed, as the PR's posting identity, for a repository whose policy only
    lets magnum comment or when the reviewer disagrees with its event.

const (
	WaitQuiet         = "quiet"          // the push quiet period
	WaitBurst         = "burst"          // the longer quiet period after a burst of pushes
	WaitInterval      = "interval"       // the minimum re-review interval since the last round
	WaitDraftInterval = "draft_interval" // the same for a draft
	WaitCap           = "cap"            // the daily round cap
	WaitDelta         = "delta"          // the re-review threshold: a small delta waits for more
	WaitRequested     = "requested"      // a review request (or ready for review): the debounce, then the next dispatch
	WaitRetry         = "retry"          // the backoff after a failed round
	WaitMuted         = "muted"          // magnum mute
	WaitQuietHours    = "quiet_hours"    // [daemon] quiet_hours
	WaitPaused        = "paused"         // magnum pause, or an infrastructure pause past its probe time
	WaitDraining      = "draining"       // magnum daemon-restart --drain: no round starts until the restart
	WaitInfra         = "infra"          // an infrastructure failure paused dispatch
	WaitHerdr         = "herdr"          // herdr is unreachable
	WaitKind          = "kind"           // an agent kind the round needs is paused
	WaitBudget        = "budget"         // the Codex budget's soft cap holds first reviews
	WaitIdentity      = "identity"       // the posting identity is unhealthy
	WaitSlot          = "slot"           // the PR's slot, or a free pool slot
	WaitCapacity      = "capacity"       // max_concurrent_reviews or max_total_working_codex
	WaitOther         = "other"          // any other dispatch gate (details, a human, a paused watch, ...)
	WaitNext          = "next"           // nothing holds it: the next dispatch starts it
)
    Wait reasons (Wait.Reason).

const ApprovalDismissMessage = "magnum: new commits since this approval; a re-review follows"
    ApprovalDismissMessage is the reason a dismissed approval shows on GitHub.

const FormerDismissMessage = "magnum: superseded by the review of %s posted as %s (this PR's reviewer changed)"
    FormerDismissMessage is the reason a dismissed review of a former identity
    shows on GitHub; the arguments are the new review's commit (short) and the
    login it was posted as.

const RequestReadyForReview = "(ready for review)"
    RequestReadyForReview is KVPRRequestBy for a draft that became ready for
    review.

const SkipIgnored = skipIgnored
    SkipIgnored is the skip_reason of a PR `magnum ignore` muted: the board
    shows such a PR as ignored.


VARIABLES

var DeltaClasses = config.TrivialDeltaClasses
    DeltaClasses lists every class, the default of skip_trivial_deltas
    (config.TrivialDeltaClasses, the list the configuration accepts).

var ErrClassifierDown = errors.New("engine: the retro's classifier is not available")
    ErrClassifierDown wraps a Classifier's error that is not the PR's: the agent
    could not be set up or started, or went away (herdr down, a prompt refused
    before it was sent). The retro stops without recording the PR.

var ErrCuratorDown = errors.New("engine: the notes curator is not available")
    ErrCuratorDown wraps a Curator's error that is not the proposal's: the agent
    could not be set up or started, or went away.

var ErrEvalLayout = errors.New("engine: magnum eval needs a scratch layout (paths.Layout.Scratch), never the live registry")
    ErrEvalLayout: RunEval needs an engine built on a scratch layout.

var ErrOpsLockHeld = errors.New("a magnum command holds state/ops.lock for in-process slot work; exiting so launchd starts the daemon again")
    ErrOpsLockHeld is what Run returns when the daemon lock is held by a magnum
    command running slot work in-process (it holds layout.OpsLock() too):
    the daemon exits non-zero so launchd starts it again once that work is done.

var ErrRestartForBuild = errors.New("a new build is on disk and no round is in flight; exiting for launchd to start it")
    ErrRestartForBuild is what Tick and Run return when the daemon exits for a
    new build on disk ([daemon] restart_on_new_build): the exit is non-zero,
    so launchd starts the new binary.

var ErrSchemaChanged = errors.New("schema migrated under the daemon; exiting for launchd to restart")
    ErrSchemaChanged is what Tick and Run return once another magnum binary
    migrated the registry under the running daemon (store.SchemaChanged):
    this binary's queries target the old schema, so the daemon exits non-zero
    and launchd starts it again from the new binary.

var EvalObserveEvery = 10 * time.Second
    EvalObserveEvery is how often RunEval observes herdr during a replay.

var RetroObserveEvery = 10 * time.Second
    RetroObserveEvery is how often the retro's agent is observed.


FUNCTIONS

func AcquireLock(path string) (unlock func(), held bool, err error)
    AcquireLock takes a non-blocking exclusive flock on path (the daemon uses
    layout.Lock(); the CLI takes the same lock before running slot or cleanup
    operations in-process). held reports that another process has it; unlock
    releases it.

func CheckPrompts(cfg *config.Config) (int, error)
    CheckPrompts renders, with representative data and this binary's renderer,
    every prompt file the configured roles name (judges with agents.JudgeData,
    other session roles with agents.RoleData in each mode, shell roles' command
    or full-line template through agents.ShellLine), the model-fallback prompt,
    the triage prompt, the retro prompt and every identity's review footer
    template (config.RenderFooter, which magnum renders after each verified
    review). Each template is rendered twice, once with every field set and once
    with the optional ones empty, so both sides of an {{if}} run. It returns
    how many renders passed and every failure, joined: a template field this
    binary's data lacks (a prompt edited for a newer build) fails here instead
    of in a round.

func ClearModelLimits(ctx context.Context, st *store.Store, kind string) ([]string, error)
    ClearModelLimits deletes the per-model limits recorded for kind
    (agents.KVModelLimited rows indexed by agents.KVModelLimits), so new
    sessions start on their roles' models again and live ones switch back before
    their next prompt. It returns the models that were limited. `magnum resume
    --tool` runs it (through the daemon, or directly when no daemon runs).

func ContentOf(s notes.State) store.NotesContent
    ContentOf is a notes state as the registry stores it.

func DaemonPID(layout paths.Layout) (int, error)
    DaemonPID returns the pid of the running daemon from the pidfile, or 0 when
    there is no pidfile or that process is gone. It never touches the lock
    (probing it could make a starting daemon exit as "already running").

func DeltaLabel(classes []string) string
    DeltaLabel is how a review note and events name the classes: "comments
    only", "whitespace only", "docs only", "base merge only", "comments and
    whitespace only", "comments, whitespace and docs only". The order is always
    comments, whitespace, docs, base, whatever the order given; unknown classes
    are left out and no known class at all is "".

func DrainerAlive(pid int) bool
    DrainerAlive reports whether the drainer pid still runs as a magnum process.
    A pid that is gone, or that now belongs to another program (pids are
    reused), is not; a process ps cannot describe counts as alive (the drain
    stays: lifting it by mistake would start rounds the restart then abandons).

func FileModTime(path string) (time.Time, bool)
    FileModTime is SkewNote's reader of a binary's mtime on disk.

func KVIdentityCheck(name string) string
    KVIdentityCheck is the kv key holding "pass" or "fail" from the
    identity's last Check. A tick-time token refresh failure is kept under
    identity.<name>.tick_error; an identity is healthy unless either says so.
    Same as store.KVIdentityCheck.

func KVIdentityError(name string) string
    KVIdentityError is the kv key holding why the identity's Check failed.
    Same as store.KVIdentityError.

func KVNotesMisses(fullName string) string
    KVNotesMisses marks a repository whose retro recorded misses for its notes:
    the time of the retro that did (store.FormatTime). The misses trigger
    curates it, and a curation that began after that time clears it.

func KVNotesOver(fullName string) string
    KVNotesOver holds the notes limits a repository's notes are past,
    comma-separated ("max_bytes,max_line"): the mark a curation looks for.
    It is set when they are measured past a limit and deleted when they are back
    within all of them.

func KVPRAttention(prID int64) string
    KVPRAttention holds why the engine parked a PR in needs_attention (the kind
    attention.Explain takes: failed, blocked, identity_error, …); read only
    while the PR is in that state.

func KVPRDelta(prID int64) string
    KVPRDelta holds the size of a PR's unreviewed delta (DeltaRecord as JSON),
    which the re-review threshold reads ([daemon] rereview_min_lines).

func KVPRDeltaCheck(prID int64) string
    KVPRDeltaCheck holds the delta check a PR's round in flight runs
    (DeltaCheckRound as JSON), which `magnum status` reads while the PR is in
    flight; the round's end deletes it.

func KVPRFormerIdentities(prID int64) string
    KVPRFormerIdentities holds the identities a PR posted as before it migrated
    to its current one (a JSON list of identity names, newest last).

func KVPRIdentityPinned(prID int64) string
    KVPRIdentityPinned holds the identity `magnum review --as` pinned the PR to;
    a pinned PR never migrates.

func KVPRManualVerdict(prID int64) string
    KVPRManualVerdict holds the id of the review a manual verdict posted: rounds
    never dismiss it as their own stale review (it is the reviewer's decision),
    while an approval still follows the head (approval.go).

func KVPRRequestAt(prID int64) string
    KVPRRequestAt holds the time of the newest review request magnum handled
    for a PR (the edge it reacted to); KVPRRequestBy who asked: the login of the
    request's actor (else the reviewer it named), or RequestReadyForReview.

func KVPRRequestBy(prID int64) string
func KVPRSkippedBaseline(prID int64) string
    KVPRSkippedBaseline marks a baseline PR skipBaseline made ineligible:
    its value is the head it was skipped on.

func KVPRTrivial(prID int64) string
    KVPRTrivial holds the last push magnum skipped as trivial for a PR
    (TrivialSkip as JSON), for the PR card; a review posted afterwards deletes
    it.

func KVPRWait(prID int64) string
    KVPRWait holds why a waiting PR has no round yet (Wait as JSON).

func KVToolPausedReason(tool string) string
    KVToolPausedReason is the kv key holding why a tool is paused (usage_limit |
    login_required). Same as store.KVToolPausedReason.

func KVToolPausedUntil(tool string) string
    KVToolPausedUntil is the kv key holding when a tool's pause ends ("codex",
    "claude"; store.FormatTime). Same as store.KVToolPausedUntil.

func KVWatchPaused(owner string) string
    KVWatchPaused holds the reason a watch owner's automation was
    paused (identity leak); magnum resume --watch clears it. Same as
    store.KVWatchPaused.

func KickDaemon(layout paths.Layout) (int, error)
    KickDaemon asks the running daemon for a tick now (SIGUSR1, `magnum kick`);
    it returns the daemon's pid, or 0 when none runs. The pidfile can outlive
    the daemon (SIGKILL) and its pid be reused, and SIGUSR1 terminates a process
    that does not handle it: the pid is signalled only after ps shows a magnum
    daemon, as `magnum daemon stop` checks.

func LooksLikeDaemon(comm, args string) bool
    LooksLikeDaemon reports whether a process is a magnum daemon from what
    ProcessCommand read: the executable (comm) is named magnum, the command
    line starts with it (so a path with spaces is one word) and its first
    argument that is not a flag is "daemon" (--config takes a value). An editor
    opening a file named magnum ("/usr/bin/vim /tmp/magnum daemon") is not one:
    its executable is vim.

func NotesDir(l paths.Layout, owner, repo string) string
    NotesDir is the harness directory next to the notes file: the notes path
    without ".md". "" under the same conditions as NotesPath.

func NotesLockPath(l paths.Layout, owner, repo string) string
    NotesLockPath is the lock of the notes file: the harness directory with
    ".lock". "" under the same conditions as NotesPath. The judge takes it
    with agents.NotesLockLine, which derives the same path from the notes file
    (agents.NotesFiles).

func NotesPath(l paths.Layout, owner, repo string) string
    NotesPath is the repository notes file of owner/repo under l, "" when l
    has no home or owner or repo is not a single plain path element (empty,
    "." or "..", or containing a path separator), which GitHub names never are.

func NotesRoot(l paths.Layout) string
    NotesRoot is the directory holding every repository's notes, "" when l names
    no place (paths.Layout.Valid).

func ProcessCommand(ctx context.Context, run execx.Runner, pid int) (comm, args string, err error)
    ProcessCommand reads what ps knows about pid: comm (`ps -o comm=`, the
    executable as started, its full path when it was started by path) and args
    (`ps -o args=`, the whole command line). LooksLikeDaemon judges them.

func PromptsLine(loadedAt time.Time, changed int) string
    PromptsLine is what `magnum status` shows after "prompts: ": when the daemon
    loaded them, and how many files changed on disk since (they take effect at
    the next restart). "" when loadedAt is zero (no daemon has recorded it).

func ProposalStale(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal) (bool, error)
    ProposalStale reports whether p is stale: the notes on disk are not the
    state of its base version. It reads the version's listing from the registry
    and hashes the files on disk, never a version's content.

func SkewNote(daemon Build, cliVersion string, modTime func(path string) (time.Time, bool)) string
    SkewNote says when the running daemon is older than what is built: the CLI's
    version (cliVersion) differs from the daemon's, or the daemon's binary on
    disk changed since it started (its mtime; modTime reads it, false when it
    cannot). "" when neither. A version that is empty or "dev" (a build `make
    build` did not stamp) is not compared.

func SkillCopyDir(state string) string
    SkillCopyDir is where the daemon keeps the copies of the judges' skills it
    took at startup (<state>/skill/<hash>/SKILL.md).

func StateOf(c store.NotesContent) notes.State
    StateOf is a version's content as a notes state.

func TrivialDelta(files []github.FileDelta, allowed []string) (classes []string, trivial bool)
    TrivialDelta reports whether a delta needs no re-review under the allowed
    classes, and the classes it used (sorted, unique; nil when not trivial).
    The delta is trivial when it has at least one file and every file is:
    a modified file with a complete patch that is documentation (docs), or whose
    every added and removed line is blank or differs from its counterpart only
    in whitespace (whitespace) or is a comment of the file's type (comments).
    A class missing from allowed makes the files that need it non-trivial,
    so an empty allowed never skips.

func WriteTabBarFile(path string, at time.Time, maxAge time.Duration, text string) error
    WriteTabBarFile atomically replaces the tab-bar file with two lines:
    when it was written and how long it stays fresh (unix seconds and seconds,
    "1791710538 90"), then text flattened to one plain line (secrets redacted).
    herdr renders only the last line a tab-bar command prints, so `cat` of the
    file still shows the status; the command magnum suggests (TabBarCommand in
    the cli) prints "magnum down" once the first line is older than its own max
    age.


TYPES

type AdoptPayload struct {
	Pool string `json:"pool,omitempty"`
	Path string `json:"path"`
}
    AdoptPayload registers the existing checkout at Path as a free slot of the
    pool of repository Pool ("" = the pool whose slot_path matches Path).

type Agents interface {
	ObserveSnapshotAt(ctx context.Context, snap herdr.Snapshot, capturedAt time.Time) ([]agents.Observation, error)
	EnsureWorkspace(ctx context.Context, pr store.PR, slotPath string, env map[string]string, label string, roles []config.Role) (agents.Workspace, error)
	EnsurePane(ctx context.Context, pr store.PR, ws agents.Workspace, slotPath string, env map[string]string, role config.Role) (agents.Workspace, error)
	StartAgent(ctx context.Context, pr store.PR, role config.Role, paneID, resume string) error
	ResumeID(ctx context.Context, prID int64, role agents.Role) (string, error)
	Preflight(ctx context.Context, kind string) error
	Park(ctx context.Context, pr store.PR) error
	Recover(ctx context.Context, pr store.PR) ([]agents.Recovered, error)
}
    Agents is the part of *agents.Manager the engine drives.

type Build struct {
	Version   string    `json:"version"`
	Revision  string    `json:"revision,omitempty"`
	Path      string    `json:"path,omitempty"`
	ModTime   time.Time `json:"mtime,omitzero"`
	StartedAt time.Time `json:"started_at,omitzero"`
}
    Build identifies a magnum binary: the version `make build` stamps
    (main.version), the commit Go recorded (vcs.revision), and the binary
    launchd starts (paths.Layout.Binary) with its modification time.

func CurrentBuild(version, path string) Build
    CurrentBuild is this process's build: version as stamped, the revision from
    the binary's build info, and path with its current mtime (both left empty
    when path is "" or cannot be read).

func ReadDaemonBuild(ctx context.Context, st *store.Store) (Build, bool)
    ReadDaemonBuild reads the build the daemon recorded at startup (false when
    none did: a daemon older than the record, or none ever ran).

func (b Build) Label() string
    Label is how messages name the build: its version, with the commit when the
    version does not say it ("dev (8ad5bb2)").

type Classifier interface {
	Classify(ctx context.Context, job ClassifyJob) (ClassifyResult, error)
	Close(ctx context.Context) error
}
    Classifier sorts the candidates of one PR at a time, writing
    ClassifyJob.OutputPath. One serves a whole retro; Close ends it.

type ClassifyJob struct {
	Dir            string
	CandidatesPath string
	OutputPath     string
	PR             store.PR
	Repo           store.Repo
	// ReviewedSHAs are the commits magnum reviewed, oldest first.
	ReviewedSHAs []string
	// Candidates are the candidates file's, for checking the answer.
	Candidates []learn.Candidate
}
    ClassifyJob is one PR's candidates for a Classifier: Dir holds
    CandidatesPath (learn.Candidates) and the commented files (learn.FilePath);
    the classifier writes its answer (learn.Output) to OutputPath.

type ClassifyResult struct {
	// Unclassified: nothing classified the candidates (no classifier is
	// set up); they are stored unclassified, not as a failure.
	Unclassified bool
	// Pause: the classifier's agent hit a limit (Kind: usage_limit,
	// login_required, model_limit or overloaded). The retro stops without
	// recording the PR, which stays due; a usage limit and a logout also
	// pause the tool, as after a round.
	Pause *pipeline.Pause
}
    ClassifyResult is how a Classifier's turn ended, besides its error.

type Cleaner interface {
	Plan(ctx context.Context, opts cleanup.Options) (cleanup.Plan, error)
	PlanFrom(ctx context.Context, opts cleanup.Options, inv inventory.Inventory) (cleanup.Plan, error)
	Apply(ctx context.Context, plan cleanup.Plan, confirmed bool) (cleanup.Report, error)
}
    Cleaner plans and applies storage cleanup (*cleanup.Planner). PlanFrom plans
    over an inventory the reconcile already scanned.

type CleanupPayload struct {
	Options   cleanup.Options `json:"options"`
	Plan      *cleanup.Plan   `json:"plan,omitempty"`
	Confirmed bool            `json:"confirmed,omitempty"`
}
    CleanupPayload is a cleanup request: Plan (re-checked by Apply) or Options
    (planned again by the daemon). Confirmed is the typed confirmation for
    actions that need it.

type CurateJob struct {
	Repo    string
	Scratch notes.Scratch
	Prompt  string
	Check   func() (notes.Proposal, []string)
}
    CurateJob is what a Curator works on: the scratch directory, the rendered
    prompt (prompts/notes-curate.md), and Check, which reads what the curator
    wrote and validates it (no problems: a valid proposal).

type CurateMark struct {
	Repo    string    `json:"repo"`
	Trigger string    `json:"trigger"`
	Started time.Time `json:"started"`
}
    CurateMark is the curation running now (KVNotesCurating).

func ReadCurating(ctx context.Context, st *store.Store) *CurateMark
    ReadCurating is the curation running now, nil when none runs (or the mark is
    unreadable).

type CurateQueued struct {
	Repo    string    `json:"repo"`
	Trigger string    `json:"trigger"`
	Why     string    `json:"why"` // QueuedJudge | QueuedBusy
	At      time.Time `json:"at"`
}
    CurateQueued is a curation waiting for a start (KVNotesCurateQueue).

func ReadCurateQueue(ctx context.Context, st *store.Store) []CurateQueued
    ReadCurateQueue lists the curations waiting for a start, oldest first.

type CurateResult struct {
	Proposal notes.Proposal
	Problems []string
	Pause    *pipeline.Pause
}
    CurateResult is how a curator's turns ended: a proposal, valid when Problems
    is empty, or a Pause (a limit the agent hit).

type CurateRun struct {
	ID  string
	Dir string
}
    CurateRun is one curation: its id and scratch directory.

type Curator interface {
	Curate(ctx context.Context, job CurateJob) (CurateResult, error)
	Close(ctx context.Context) error
}
    Curator proposes curated notes for one curation; Close ends it.

type DeltaCheckRound struct {
	Lines  int    `json:"lines"`  // the delta's changed code lines
	Files  int    `json:"files"`  // its files
	Target string `json:"target"` // the commit the check reviews
}
    DeltaCheckRound is a delta check in flight (KVPRDeltaCheck).

func ParseDeltaCheckRound(s string) (DeltaCheckRound, bool)
    ParseDeltaCheckRound reads a KVPRDeltaCheck value; ok is false for "" or a
    value it cannot read.

type DeltaRecord struct {
	// Version is deltaRecordVersion for a delta measured with the PR's own
	// diff in view (base_merge.go); 0, a record from before, is checked
	// again once (recheckDeltas).
	Version int    `json:"version,omitempty"`
	From    string `json:"from"`
	To      string `json:"to"`
	DeltaSize
	// Since is the first push the review does not cover: kept while From
	// stays the reviewed commit.
	Since time.Time `json:"since"`
}
    DeltaRecord is the measured delta of a PR from its reviewed commit to a head
    (KVPRDelta). It describes the PR only while From is its reviewed_sha and To
    its head.

type DeltaSize struct {
	// Lines counts the added and removed lines that change code: not
	// blank, not a comment, not a line that only moved in whitespace
	// within its run of changes, not in a documentation file.
	Lines int `json:"lines"`
	// AddedFiles counts the files added, renamed or copied.
	AddedFiles int `json:"added_files"`
	// Complete is false when a file other than an added one had no
	// complete patch (binary, too large, a cut-off file list): its lines
	// are unknown, so the threshold cannot hold the delta back.
	Complete bool `json:"complete"`
	// Binaries are the files of the delta modified without a patch whose
	// type is binary (binaryDeltaPath: an image, a font, an archive): they
	// make it incomplete like any file without a patch, but a delta check
	// (eligibility.DeltaCheck) reads them as 0 lines and names them to the
	// judge. Unread counts the other files without a complete patch (too
	// large, a cut-off file list, a removed binary).
	Binaries []string `json:"binaries,omitempty"`
	Unread   int      `json:"unread,omitempty"`
}
    DeltaSize is how big a delta is for the re-review threshold ([daemon]
    rereview_min_lines).

func MeasureDelta(files []github.FileDelta) DeltaSize
    MeasureDelta counts a delta's lines the way TrivialDelta judges them, with
    every class allowed, but line by line instead of all or nothing: a comment,
    a blank line, a documentation file and a line whose counterpart in the
    same run of changes differs only in whitespace count nothing; every other
    added or removed line counts one, and so does each line of a run whose code
    lines only changed order. A context line that one side has inside a block
    comment and the other does not (code commented out, or back in) counts one,
    like a line it cannot read.

func (s DeltaSize) Readable() bool
    Readable reports whether every file of the delta is read in full or is a
    modified binary file (Binaries): what a delta check needs.

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
    Deps are the engine's collaborators. Config, Store and Logger are required;
    a nil port disables the steps that need it.

type Drain struct {
	Since time.Time `json:"since"`
	PID   int       `json:"pid,omitempty"`
}
    Drain is a drain in progress, the KVDaemonDraining value: when it started
    and the pid of the command that drains (0 = an older CLI did not say).

func ParseDrain(v string) (Drain, bool)
    ParseDrain reads a KVDaemonDraining value: Drain.Value, or the bare start
    time an older CLI wrote. false for "" or anything else.

func (d Drain) Value() string
    Value is d as KVDaemonDraining stores it.

type Engine struct {
	// Has unexported fields.
}
    Engine is the daemon. Create it with New or FromApp.

func FromApp(a *app.App) *Engine
    FromApp wires an engine to the App's real components.

func New(d Deps) *Engine
    New returns an engine over d.

func (e *Engine) Kick()
    Kick asks the running loop for a tick now (what SIGUSR1 does).

func (e *Engine) Planned() []PlannedOp
    Planned returns what a dry run would have done (empty otherwise).

func (e *Engine) ReleaseEval(ctx context.Context, pr store.PR) error
    ReleaseEval quits the agents of an eval round's PR and closes its workspace
    (agents.Manager.Park); the conversations stay resumable in the agents' own
    history.

func (e *Engine) Run(ctx context.Context, opts Options) error
    Run is the daemon: lock, pidfile, startup, then a tick every
    daemon.poll_interval (or on Kick/SIGUSR1) until ctx ends or SIGTERM.
    It returns nil when another daemon holds the lock ("already running"),
    so launchd does not restart it in a loop, but ErrOpsLockHeld when a magnum
    command holds it for in-process slot work, and ErrSchemaChanged when the
    registry was migrated under it, so launchd starts it again. A dry run takes
    neither the lock nor the pidfile (it changes nothing a real daemon could
    see).

func (e *Engine) RunEval(ctx context.Context, c EvalCase) (pipeline.RoundResult, store.PR, error)
    RunEval runs one blind dry-run round for c and returns the pipeline's result
    and the agents' PR row (for Release). It refuses an engine whose layout has
    no Scratch, so a replay never writes the live registry, reports or notes.
    It runs the engine's own round preparation (preflight, pane environment,
    workspace "eval <repo>#<N>", agents, readiness probes) and pipeline,
    then stops: no pause, refund, toast or dismissal follows, those belong to
    the PR's real rounds. The registry rows it seeds (repo, PR in claiming,
    a per-PR slot at Checkout) live in the scratch store.

func (e *Engine) Tick(ctx context.Context) error
    Tick runs one iteration of the daemon loop. Steps log their own
    failures; the returned error joins them for the caller's information.
    A registry migrated by another binary stops the tick before any step
    (ErrSchemaChanged), and so does a new build the daemon restarts on
    (ErrRestartForBuild, restart_on_new_build).

type EvalCase struct {
	Owner, Repo string
	Number      int
	URL         string
	Head        string // the pinned head, checked out in Checkout
	BaseRef     string // the PR's base branch ("" = DefaultBranch)
	// DefaultBranch is the repository's default branch.
	DefaultBranch string
	Title, Author string
	AuthorType    string // "User" or "Bot" ("" = User)
	Checkout      string // absolute path of the worktree at Head
	MainClone     string // the clone the worktree belongs to
	Watch         config.Watch
	Identity      string // the posting identity whose rights the judge checks (it posts nothing)
	// Notes gives the judge the scratch copy of the repository notes
	// (paths.Layout.Notes under Scratch; the caller copies the live file).
	Notes bool
}
    EvalCase is one replay of `magnum eval run`: a PR at a pinned head that the
    caller already checked out (detached) in Checkout, reviewed by the watch's
    roles as a blind dry run. The PR fields come from GitHub; nothing here is
    written to the live registry.

type Git interface {
	MergeBase(ctx context.Context, dir, a, b string) (string, error)
	FindClone(ctx context.Context, cloneRoot, owner, name string) (string, error)
	// RevParse and FetchCommit find the first parent of a merged PR's merge
	// commit, the base a post-merge round reviews from (postMergeBase).
	RevParse(ctx context.Context, dir, ref string) (string, error)
	FetchCommit(ctx context.Context, mainClone, sha string, number int) error
}
    Git is the part of *gitx.Client the engine reads (round context, clone
    discovery for repos.clone_path).

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
    GitHub is the part of *github.Client the engine uses (one client per
    identity): the poller's reads, and the dismissal of an App approval the head
    left behind (followApproval), made as the App identity.

type Herdr interface {
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
}
    Herdr is the part of *herdr.Client the tick reads: one snapshot per tick
    (agent statuses for completion, the working-Codex gate, liveness).

type IdentityVerdictPayload struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Reason string `json:"reason,omitempty"`
}
    IdentityVerdictPayload is the verdict of `magnum identities check` for the
    configured identity Name; Reason says why a failed check failed.

type Inventory interface {
	Scan(ctx context.Context, opts inventory.Options) (inventory.Inventory, error)
	UpsertSlotDatabases(ctx context.Context, inv inventory.Inventory) (inventory.SyncResult, error)
}
    Inventory is the reconcile scanner (*inventory.Scanner).

type NotesCuratePayload struct {
	Repo      string `json:"repo"` // owner/name
	Supersede int64  `json:"supersede,omitempty"`
}
    NotesCuratePayload is a `magnum notes <repo> --curate` request, or the
    new curation `magnum notes <repo> --review` asks for instead of a stale
    proposal: Supersede is that proposal's id.

type OpenPayload struct {
	PRTarget
	Role string `json:"role,omitempty"` // a role of the PR's watch, by name or alias; "" = its judge
}
    OpenPayload is a `magnum open` request for a PR without a live session:
    the daemon gives it a slot (its own, a free pool slot or a per-PR worktree),
    checks out reviewed_sha (else the head) when the slot holds something else,
    starts or resumes the judge and Role's agent (an on-demand role gets its
    pane), pins the PR and its slot, and completes with the pane id of Role.
    A PR whose session is live, or whose round is running, completes at once.

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
    Options tune Run.

type PRTarget struct {
	Ref    string `json:"ref,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
}
    PRTarget names a PR in a request: Repo ("owner/name") and Number win;
    otherwise Ref is any form github.ParseRef accepts, resolved against
    daemon.default_repo.

type PausePayload struct {
	Reason string     `json:"reason,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
	Tool   string     `json:"tool,omitempty"`
	Watch  string     `json:"watch,omitempty"`
}
    PausePayload pauses (pause) or resumes (resume) automation. For resume,
    Tool (an agent kind such as "codex", "claude", "droid", or "all") lifts that
    kind's pause and Watch a watch owner's identity-leak pause instead of the
    daemon pause.

type PlannedOp struct {
	At      time.Time `json:"at"`
	Subject string    `json:"subject"`
	Action  string    `json:"action"`
	Detail  string    `json:"detail,omitempty"`
}
    PlannedOp is one side effect a dry run decided on but did not perform.

type ProvisionPayload struct {
	Pool  string `json:"pool,omitempty"`
	Count int    `json:"count,omitempty"`
}
    ProvisionPayload provisions Count pool slots (resuming interrupted ones
    first; Count <= 0 = up to pool.min) of the pool of repository Pool ("" =
    daemon.default_repo, else the only pool).

type Recorder struct {
	// Has unexported fields.
}
    Recorder collects a dry run's planned side effects: it logs "would …",
    keeps the ops for Engine.Planned and writes a dryrun.<action> event (into
    the dry run's private store copy).

func (r *Recorder) Ops() []PlannedOp
    Ops returns a copy of the recorded ops.

func (r *Recorder) Record(ctx context.Context, subject, action, detail string)
    Record notes one planned side effect.

type RepairPayload struct {
	Slot string `json:"slot"`
}
    RepairPayload re-runs provisioning of a free, broken, provisioning or lost
    pool slot.

type Request struct {
	At time.Time
	By string // a login, or RequestReadyForReview
}
    Request is a review request magnum handled for a PR.

func (r Request) Phrase() string
    Phrase is how screens name the request: "requested by alice" or "ready for
    review".

type RetroGitHub interface {
	ReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, error)
	Reviews(ctx context.Context, owner, repo string, number int) ([]github.Review, error)
	CompareFilesStatus(ctx context.Context, owner, repo, base, head string) (string, []github.FileDelta, error)
	FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
}
    RetroGitHub is what a retro reads from GitHub (*github.Client): the review
    threads, every review, the comparison of a reviewed commit with a later one
    and the commented files.

type RetroPayload struct {
	// PRs limits the retro to these pull requests (registry ids), whenever
	// they closed, and implies Again for them; empty = every PR due within
	// the lookback.
	PRs []int64 `json:"prs,omitempty"`
	// Again also looks at PRs a retro already looked at.
	Again bool `json:"again,omitempty"`
	// Lookback replaces [learn] lookback (config.ParseDuration: "14d").
	Lookback string `json:"lookback,omitempty"`
}
    RetroPayload is a `magnum retro` request: a retro now, whatever learn
    enabled, daily_at and `magnum pause` say (a drain or an infrastructure pause
    still holds it).

type RetroRun struct {
	ID  string
	Dir string
}
    RetroRun is one retro: its id and directory (learn/retro/<id>).

type RetroSummary struct {
	Run        string    `json:"run"` // its directory under learn/retro
	At         time.Time `json:"at"`
	Finished   time.Time `json:"finished"`
	PRs        int       `json:"prs"`        // PRs looked at
	Classified int       `json:"classified"` // PRs whose candidates were classified
	Misses     int       `json:"misses"`     // candidates classified as a miss
	Caught     int       `json:"caught"`     // comments magnum had already posted
	Failed     int       `json:"failed"`     // PRs that failed
	Stopped    string    `json:"stopped,omitempty"`
}
    RetroSummary is what the last retro did (KVRetroLast).

type ReviewPayload struct {
	PRTarget
	// Again is ignored: a forced round reviews the head even when it was
	// reviewed already. No CLI sends it any more; it stays so a request an
	// older CLI queued with --again still decodes (decode refuses unknown fields).
	Again bool `json:"again,omitempty"`
	Fresh bool `json:"fresh,omitempty"` // park the sessions and start new conversations
	// Simplify asks for the role answering to "simplify" (claude-simplify
	// by default) this round, whatever its runs; kept for older CLIs, the
	// same as Roles ["simplify"].
	Simplify bool `json:"simplify,omitempty"`
	// Roles asks for these roles of the PR's watch (names or aliases) this
	// round, whatever their runs ("first" or "manual"; a "never" role is
	// refused). The daemon stores the resolved names until a review posts.
	Roles []string `json:"roles,omitempty"`
	As    string   `json:"as,omitempty"` // post as this identity from now on
	// DryRun runs the round with pipeline.RoundInput.DryRun: the judge
	// writes its planned review and posts nothing; afterwards the PR returns
	// to the state it had (reviewed_sha does not move).
	DryRun bool `json:"dry_run,omitempty"`
}
    ReviewPayload is a `magnum review` request: a forced round that bypasses
    eligibility, the throttle and quiet hours. On a PR GitHub merged it is a
    post-merge review (post_merge.go).

type Rounds interface {
	RunRound(ctx context.Context, in pipeline.RoundInput) (pipeline.RoundResult, error)
	// AppendToReview adds a last paragraph to a review the identity posted.
	AppendToReview(ctx context.Context, owner, repo string, number int, reviewID int64, text string) error
}
    Rounds runs one review round (*pipeline.Runner, one per identity) and edits
    the reviews its identity posted.

type Slots interface {
	Claim(ctx context.Context, pr store.PR, pool config.Pool) (store.Slot, error)
	Checkout(ctx context.Context, slot store.Slot, pr store.PR, pool config.Pool, targetSHA string) error
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
    Slots is the part of *slots.Manager the engine drives.

type StaleCheck struct {
	Stale          bool
	Live           notes.State // the notes now
	Base, Proposed notes.State
	Merge          notes.StateMerge // set when Stale
}
    StaleCheck is a pending proposal against the notes now: whether it is stale
    and, when it is, the three-way merge of the notes' changes since its base
    and its own.

func CheckStale(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal) (StaleCheck, error)
    CheckStale reads p's base and proposed states and the notes now and,
    when the notes are no longer p's base, merges both sides' changes.

type TargetPayload struct {
	PRTarget
	Slot  string `json:"slot,omitempty"`
	Force bool   `json:"force,omitempty"` // release: see cleanup.Options.Force
}
    TargetPayload names a PR or a slot (release, pin, unpin, mute, unmute).

type TrivialSkip struct {
	From    string    `json:"from"`    // the reviewed commit
	To      string    `json:"to"`      // the head the review now stands for
	Classes []string  `json:"classes"` // DeltaClasses the push used
	Files   int       `json:"files"`
	At      time.Time `json:"at"`
}
    TrivialSkip is a push magnum did not re-review (KVPRTrivial).

func ParseTrivialSkip(s string) (TrivialSkip, bool)
    ParseTrivialSkip reads a KVPRTrivial value; ok is false for "" or a value it
    cannot read.

func (t TrivialSkip) Note() string
    Note is the PR card's line for the skip, e.g. "comment-only push skipped
    (a7b3f8c → 602da9d)".

type VerdictPayload struct {
	PRTarget
	// Message is the reviewer's own words, put before magnum's line.
	Message string `json:"message,omitempty"`
	// Force posts on the reviewed head although the PR moved on since.
	Force bool `json:"force,omitempty"`
}
    VerdictPayload is a manual verdict request.

type Wait struct {
	Reason   string    `json:"reason"`
	Rereview bool      `json:"rereview"`
	Forced   bool      `json:"forced,omitempty"`
	Until    time.Time `json:"until,omitzero"`    // when it ends, when known
	Count    int       `json:"count,omitempty"`   // WaitCap: automatic rounds today; WaitDelta: changed lines
	Max      int       `json:"max,omitempty"`     // WaitCap: the cap; WaitDelta: rereview_min_lines
	Subject  string    `json:"subject,omitempty"` // WaitKind: the paused kind; WaitRequested: Request.Phrase
	// Detail is the reason in words, without the round's kind or the time
	// ("the push quiet period (5m)").
	Detail string `json:"detail"`
	// PostMerge: GitHub merged the PR; the round it waits for is a
	// post-merge review (`magnum review` of a merged PR, always forced).
	PostMerge bool `json:"post_merge,omitempty"`
	// DeltaCheck: the round it waits for is a delta check, the judge alone
	// on a small delta (deltaCheckDue).
	DeltaCheck bool `json:"delta_check,omitempty"`
}
    Wait is why a PR waiting for a round has none yet (KVPRWait).

func ParseWait(s string) (Wait, bool)
    ParseWait reads a KVPRWait value; ok is false for "" or a value it cannot
    read.

func (w Wait) Sentence(ref string, now time.Time) string
    Sentence is the full account for a card or `magnum status <ref>`, with the
    hint that lifts it: "re-review waits for the daily round cap (6 of 6 today)
    until 00:00; `magnum review talkable#729` runs it now". ref is how the PR is
    named on the command line.

func (w Wait) Short(now time.Time) string
    Short is the compact form for a table cell, e.g. "re-review · cap 6/6 →
    00:00", "re-review · quiet → 14:09", "review · codex paused → 15:00",
    "re-review · small delta 8/30 lines → 16:40", "re-review · requested by
    alice → now", "delta check · quiet → 14:09".

```

## eval

```text
package eval // import "github.com/zhuravel/magnum/internal/eval"

Package eval scores an evaluation replay of magnum's pull request review against
a corpus of pull requests with known ("seeded") defects.

The corpus (corpus.go) lists, per pull request, the reviewed head and the
defects a good review must report. The judge's result file of a no-post run
(findings.go) is parsed into findings, and ScoreCase (score.go) tells which
defects were found, at which severity, and how many findings matched nothing
(noise). Runs (report.go) collect the scores of one replay, persist them, can be
rescored after the corpus is corrected, and render a text or markdown report.

The package is pure: it starts no process, touches no network and knows nothing
of the registry. Its only file access is the corpus file and the run directories
it is told about.

FUNCTIONS

func SaveRun(dir string, r Run) error
    SaveRun writes dir/run.json atomically (a temporary file in dir, then a
    rename), mode 0600; dir is created with mode 0700.

func WriteMarkdown(w io.Writer, r Run)
    WriteMarkdown writes the report of r as markdown (report.md): a summary
    table, then per case each defect as found or missed with its severity,
    and the findings that matched no defect.

func WriteText(w io.Writer, r Run, prev *Run)
    WriteText writes the human report of r: a table with one row per case and a
    total, then, when prev is not nil, how the totals moved since it, then one
    line per missed defect and per case error.


TYPES

type Case struct {
	Name    string   `toml:"name"` // unique, [a-z0-9][a-z0-9-]{0,47}
	PR      string   `toml:"pr"`   // owner/repo#N
	Head    string   `toml:"head"` // the reviewed head, 40 lower-case hex
	Base    string   `toml:"base"` // base branch name, optional
	Why     string   `toml:"why"`  // why the case is in the corpus, optional
	Defects []Defect `toml:"defect"`

	// Owner, Repo and Number are parsed from PR.
	Owner  string `toml:"-"`
	Repo   string `toml:"-"`
	Number int    `toml:"-"`
}
    Case is one pull request pinned to the head that is reviewed, with the
    defects a review must find.

type CaseRun struct {
	Case       string    `json:"case"`
	PR         string    `json:"pr"`
	Head       string    `json:"head"`
	Outcome    string    `json:"outcome"` // dry_run, error, needs_attention, usage_limit, skipped, ...
	Error      string    `json:"error,omitempty"`
	ResultFile string    `json:"result_file,omitempty"` // absolute path of the judge result
	ReportDir  string    `json:"report_dir,omitempty"`
	Started    time.Time `json:"started,omitzero"`
	Finished   time.Time `json:"finished,omitzero"`
	Score      *Score    `json:"score,omitempty"` // nil when there was no result to score
}
    CaseRun is one case of a replay: what became of the review round and,
    when it left a result, the score.

type Corpus struct {
	Cases []Case `toml:"case"`
}
    Corpus is the set of pull requests with seeded defects a replay is scored
    against.

func LoadCorpus(path string) (*Corpus, error)
    LoadCorpus reads and parses the corpus file at path.

func ParseCorpus(data []byte) (*Corpus, error)
    ParseCorpus parses and validates a corpus. It is strict: an unknown key
    is an error, and every problem found is reported (joined), each naming the
    case, the defect and the rule broken.

func (c *Corpus) Select(names []string) ([]Case, error)
    Select returns the cases called names, in corpus order; no names selects
    every case. A name the corpus does not know is an error.

type Defect struct {
	ID       string   `toml:"id"`       // unique within the case, same charset as a case name
	Title    string   `toml:"title"`    // shown in reports
	Severity string   `toml:"severity"` // weakest severity that still counts as right, P0..P3, optional
	Paths    []string `toml:"paths"`    // globs on the slash path, "**" spans directories
	Lines    []int    `toml:"lines"`    // inclusive head-side range [from, to]
	Match    []string `toml:"match"`    // case-insensitive regexps on the finding body
	Body     bool     `toml:"body"`     // a mention in the review body also counts; needs Match

	// Has unexported fields.
}
    Defect is one thing a review must report. A finding matches it when its
    path matches one of Paths (any path when empty), its range comes within
    three lines of Lines (any line when empty) and one of Match matches its body
    (anything when empty). Paths and Match must not both be empty.

type DefectResult struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Want       string `json:"want,omitempty"` // the defect's severity
	Found      bool   `json:"found"`
	By         []int  `json:"by,omitempty"`       // indexes into Result.Findings that matched
	Severity   string `json:"severity,omitempty"` // the strongest among matching inline findings, "" if none
	SeverityOK bool   `json:"severity_ok"`        // found, and at Want or stronger (or no Want)
}
    DefectResult is the outcome for one defect of a case.

type Finding struct {
	Path           string `json:"path,omitempty"`
	Line           int    `json:"line,omitempty"`
	StartLine      int    `json:"start_line,omitempty"` // 0 = single line
	Body           string `json:"body"`
	Severity       string `json:"severity,omitempty"`       // P0..P3 or ""
	Simplification bool   `json:"simplification,omitempty"` // an optional suggestion, never a defect
	InBody         bool   `json:"in_body,omitempty"`        // the review body rather than an inline comment
}
    Finding is one thing the review reported: an inline comment, or the review
    body as a whole.

type Result struct {
	Status   string
	Event    string
	Counts   map[string]int // the "findings" object: severity -> number
	Findings []Finding      // inline comments in order, then the review body when it is not empty
	Planned  bool           // planned_review was present
}
    Result is what a judge result file says about one round.

func ParseResult(data []byte) (Result, error)
    ParseResult reads the judge's result file. The inline comments of
    planned_review become findings in order, followed by one InBody finding
    holding the review body when it is not empty.

    The severity of a comment comes from the provenance entry that was posted
    (or has no verdict) on the same path and line, else from the first P0..P3
    token in the first 80 characters of its body.

type Run struct {
	ID       string            `json:"id"`               // sortable, e.g. 20261004-153000
	Label    string            `json:"label"`            // free text
	Commit   string            `json:"commit"`           // magnum's git HEAD (short) when the run started, "" unknown
	Dirty    bool              `json:"dirty"`            // magnum's checkout had uncommitted changes
	Models   map[string]string `json:"models,omitempty"` // role name -> model, "" = the kind's default
	Corpus   string            `json:"corpus"`           // corpus path
	Started  time.Time         `json:"started,omitzero"`
	Finished time.Time         `json:"finished,omitzero"`
	Cases    []CaseRun         `json:"cases"`
}
    Run is one replay of a corpus.

func ListRuns(root string) ([]Run, error)
    ListRuns loads every root/*/run.json, oldest ID first. A directory without a
    readable run.json is skipped; a missing root has no runs.

func LoadRun(dir string) (Run, error)
    LoadRun reads dir/run.json.

func Rescore(r Run, c *Corpus, read func(path string) ([]byte, error)) Run
    Rescore recomputes the score of every case of r from its saved result
    file against the current defects of the corpus, so a corrected match
    expression takes effect without running the agents again. read loads a file
    (os.ReadFile when nil). The run it returns is a copy; r is not changed.

    A case the corpus no longer has, or that has no result file, keeps its
    score. A result that cannot be read or parsed keeps the old score too and,
    when there was one, sets Error to say so.

func (r Run) Totals() Totals
    Totals adds up the scores of the run. Cases without a score count in Cases
    only.

type Score struct {
	Case            string         `json:"case"`
	Status          string         `json:"status,omitempty"`
	Event           string         `json:"event,omitempty"`
	Defects         []DefectResult `json:"defects"`
	Found           int            `json:"found"`
	Total           int            `json:"total"`
	Noise           int            `json:"noise"`               // inline findings that matched no defect, simplifications aside
	Simplifications int            `json:"simplifications"`     // optional simplification suggestions, neither defect nor noise
	Unmatched       []Finding      `json:"unmatched,omitempty"` // the noise findings
}
    Score is the outcome for one case.

func ScoreCase(c Case, r Result) Score
    ScoreCase tells, for every defect of c, whether the findings of r report it.
    An inline finding matches a defect when its path matches one of the defect's
    paths (any when none), its line range comes within three lines of the
    defect's lines (when set) and its body matches one of the defect's regexps
    (when set). A review body matches only a defect with body = true, by its
    regexps alone. Simplification suggestions match nothing and are not noise.
    One finding may match several defects.

func (s Score) SeverityOK() int
    SeverityOK counts the found defects that were reported at the wanted
    severity or stronger.

type Totals struct {
	Cases           int // all cases of the run
	Scored          int // cases that have a score
	Found           int
	Total           int
	Noise           int
	Simplifications int
	SeverityOK      int
	Recall          float64 // Found/Total, 0 when Total is 0
}
    Totals sums the scored cases of a run.

```

## execx

```text
package execx // import "github.com/zhuravel/magnum/internal/execx"

Package execx is the single choke point for every subprocess magnum runs (git,
gh, mysql, mise exec, launchctl, osascript, codex/claude probes).

Every call carries a timeout, runs in its own process group so a cancelled
context terminates the whole tree (SIGTERM, then SIGKILL), gets an explicit
env overlay and a working directory, captures at most MaxOutput per stream,
and logs a redacted transcript. Tests use Fake; --dry-run wraps the real runner
in DryRun so mutating commands are printed instead of run.

CONSTANTS

const DefaultTimeout = 2 * time.Minute
    DefaultTimeout applies when Cmd.Timeout is zero.

const MaxOutput = 8 << 20
    MaxOutput caps each captured stream (stdout and stderr separately). Output
    past it is drained and discarded so the child never blocks on a full pipe;
    the kept prefix is followed by TruncationMarker and Result.Truncated is set.

const TruncationMarker = "\n[execx: output truncated at 8 MiB]\n"
    TruncationMarker is appended to a stream cut at MaxOutput.


FUNCTIONS

func MergeEnv(base []string, env map[string]string, unset []string) []string
    mergeEnv overlays env onto base: keys in env win (empty values are
    kept as KEY=), keys in unset that are not in env are dropped entirely.
    MergeEnv returns base with env's keys overriding and unset's keys removed;
    it is what Run gives every subprocess and what other runners (the CLI's TTY
    runner) use to honour Cmd.Env and Cmd.Unset the same way.

func Redact(s string) string
    Redact masks GitHub tokens, JWTs, Authorization header values and PEM
    private keys in free text. An Authorization header keeps its name.

func ShellQuote(s string) string
    ShellQuote single-quotes s unless it consists only of characters a POSIX
    shell never interprets; the empty string becomes two single quotes. Newlines
    and carriage returns inside quotes are kept verbatim; callers that need one
    log line should escape them first.


TYPES

type Cmd struct {
	Name string   // executable; resolved through PATH unless absolute
	Args []string // argv after the executable
	Dir  string   // working directory ("" = inherit)
	// Env overlays the parent environment. An empty value blanks the variable
	// (exported as KEY= so shells and Ruby .presence treat it as unset).
	Env map[string]string
	// Unset removes variables from the inherited environment entirely (git,
	// for one, treats an empty GIT_DIR differently from an absent one). A key
	// that is also in Env keeps its Env value.
	Unset []string
	// Timeout bounds the run; zero means DefaultTimeout.
	Timeout time.Duration
	// Mutates marks commands that change state. DryRun refuses to execute them.
	Mutates bool
	// Stdin, when non-nil, is written to the process.
	Stdin []byte
	// Label is a short human name used in logs ("git fetch pr 123").
	Label string
	// Probe marks a command whose non-zero exit is an expected answer (is
	// this a git repository? does this ref exist?): a failed exit is logged
	// at Debug instead of Warn. Start failures and timeouts still warn.
	Probe bool
	// NoTTY starts the process in a new session, without a controlling
	// terminal (setsid), as launchd starts the daemon. An interactive shell
	// (`zsh -ic`) run from a CLI in a terminal otherwise shares that
	// terminal from a background process group, where shell startup that
	// touches the terminal stops it until the timeout.
	NoTTY bool
}
    Cmd describes one subprocess.

func (c Cmd) String() string
    String renders the command the way a shell would show it: every argument
    that is not plainly shell-safe is single-quoted (see ShellQuote). The result
    is not redacted; pass it through Redact before logging.

type DryRun struct {
	Inner Runner
	Out   Logger

	// Planned records the mutating commands that would have run.
	Planned []Cmd
	// Has unexported fields.
}
    DryRun executes read-only commands through Inner and only prints mutating
    ones.

func (d *DryRun) Run(ctx context.Context, c Cmd) (Result, error)
    Run implements Runner.

type ExitError struct {
	Cmd    Cmd
	Code   int
	Stderr string
}
    ExitError is returned when the process ran but exited non-zero.

func (e *ExitError) Error() string

type Fake struct {
	Rules []Rule

	Calls []Cmd
	// Has unexported fields.
}
    Fake is a scripted Runner for tests. Unmatched commands fail loudly.

func (f *Fake) CallsWithPrefix(prefix ...string) []Cmd
    CallsWithPrefix returns the recorded calls whose argv starts with prefix.

func (f *Fake) Run(ctx context.Context, c Cmd) (Result, error)
    Run implements Runner.

type LevelLogger interface {
	Logger
	Logf(level slog.Level, format string, args ...any)
}
    LevelLogger is a Logger that also takes a level per line. Real logs a
    command that succeeded at slog.LevelDebug and one that failed (a non-zero
    exit, a failed start, a timeout) at slog.LevelWarn when its Log implements
    it; a plain Logger gets every line through Printf.

type Logger interface {
	Printf(format string, args ...any)
}
    Logger receives one line per finished command (already redacted).

type Real struct {
	// Log, when set, receives a redacted one-line transcript per command:
	// at debug level for a success and at warn level for a failure when it
	// is a LevelLogger.
	Log Logger
	// BaseEnv, when non-empty, replaces os.Environ() as the parent environment.
	BaseEnv []string
}
    Real runs commands with os/exec.

func (r *Real) Run(ctx context.Context, c Cmd) (Result, error)
    Run executes the command in its own process group. When the context is
    cancelled or the timeout elapses the whole group gets SIGTERM, then SIGKILL
    after killGrace. Descendants that outlive the leader while still holding its
    output pipes are killed too. Every error renders the command redacted.

type Result struct {
	Stdout   []byte
	Stderr   []byte
	Code     int
	Duration time.Duration
	// Truncated reports that a stream exceeded MaxOutput: the kept prefix is
	// followed by TruncationMarker and the rest was discarded.
	Truncated bool
}
    Result is the outcome of a finished subprocess.

func (r Result) Out() string
    Out returns trimmed stdout.

type Rule struct {
	Prefix []string
	Result Result
	Err    error
	// Fn, when set, computes the response from the actual command.
	Fn func(c Cmd) (Result, error)
}
    Rule scripts one Fake response. Prefix matches the leading argv words (Name
    followed by Args); the first matching rule wins.

type RunError struct {
	Cmd Cmd
	Err error
}
    RunError is a runner failure other than a non-zero exit: a missing working
    directory, a failed start, a cancelled or timed-out context. Its message
    shows the redacted command line; Unwrap exposes the cause (context errors,
    fs.ErrNotExist, exec.ErrNotFound) for errors.Is.

func (e *RunError) Error() string

func (e *RunError) Unwrap() error

type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}
    Runner executes commands. Implementations: Real, Fake, DryRun.

```

## fsx

```text
package fsx // import "github.com/zhuravel/magnum/internal/fsx"

Package fsx holds the file helpers several packages share: an atomic file write,
plain or confined to an os.Root, and an existence check. It imports only the
standard library.

FUNCTIONS

func Exists(path string) bool
    Exists reports whether path names something os.Stat can see (a symlink
    counts by its target).

func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error
    WriteFileAtomic replaces path with data, mode exactly perm whatever the
    umask: a temporary file with a unique name in path's directory is written,
    synced and renamed over path, then the directory is synced, so a reader (or
    a crash) sees the old content or the new, never part of a file. A symlink at
    path is replaced, not followed. The directory must exist; the temporary file
    is removed on failure.

func WriteFileAtomicIn(root *os.Root, name string, data []byte, perm fs.FileMode) (err error)
    WriteFileAtomicIn is WriteFileAtomic for name inside root: a name that is
    absolute or escapes root (.., or a symlink in a parent pointing outside) is
    an error and nothing is written outside root. name's directory is resolved
    once, so the temporary file and the rename land in the same one.

```

## github

```text
package github // import "github.com/zhuravel/magnum/internal/github"

Package github is magnum's GitHub access layer. Every call is a `gh api`
subprocess run through execx.Runner, so the identity is chosen by the client's
env (GH_CONFIG_DIR per identity), tests script gh with execx.Fake and --dry-run
turns writes into no-ops.

Reads use GraphQL (`gh api graphql --input -`): the per-owner Radar poll (a
user or an organization) and the CIStates of its pull requests' heads, batched
Details, ConfirmStates for PRs that left the OPEN list, ReviewsWithMarker for
verification, and Reviews and ReviewThreads for the whole conversation of one
pull request. A Radar page or CIStates call GitHub could not answer in time
is asked again once at half the size (retrySmaller). REST is used where only
REST carries the data (ReviewREST: the "[bot]"-suffixed author login; Compare,
CompareFiles and ComparePush: the size, the patches and the merge commits of
an arbitrary base...head range; FileAt: the raw content of a file at a ref,
a response that is bytes and not JSON) and for writes (DismissReview).

GraphQL partial errors are tolerated only for NOT_FOUND paths, which the
batched calls report as missing numbers; any other error fails the whole call,
so a half-read poll never looks like closed pull requests. Failures that reached
GitHub come back as *APIError, which matches ErrNotFound, ErrUnauthorized,
ErrForbidden and ErrRateLimited through errors.Is.

Every command carries --hostname github.com, so an inherited GH_HOST can never
send a call (or an identity's token) to another server. Client.Reauth lets the
caller refresh an identity's credentials after a 401.

CONSTANTS

const (
	CheckPassed  = "passed"  // a check run's SUCCESS or NEUTRAL, a status's SUCCESS
	CheckFailed  = "failed"  // FAILURE, TIMED_OUT, CANCELLED, ACTION_REQUIRED, STARTUP_FAILURE, STALE; a status's FAILURE or ERROR
	CheckPending = "pending" // a check run not COMPLETED, a status PENDING or EXPECTED
	CheckSkipped = "skipped" // SKIPPED
)
    Check.State values.

const CompareFileLimit = 300
    CompareFileLimit is the most files GitHub lists for one comparison;
    a comparison listing exactly this many is treated as truncated.

const ComparePushCommits = 100
    ComparePushCommits is the most commits ComparePush reads (one page at
    GitHub's per_page maximum).

const FileAtLimit = 512 << 10
    FileAtLimit caps FileAt: a larger file is not returned.

const PullFilesMaxPages = 30
    PullFilesMaxPages is how many pages of 100 PullFiles reads: GitHub's own cap
    for the endpoint, 3000 files.


VARIABLES

var (
	ErrNotFound     = errors.New("github: not found")
	ErrUnauthorized = errors.New("github: unauthorized")
	ErrForbidden    = errors.New("github: forbidden")
	ErrRateLimited  = errors.New("github: rate limited")
)
    Errors matched by *APIError through errors.Is.

var ErrFileTooLarge = errors.New("github: file too large")
    ErrFileTooLarge matches FileAt's error for a file above FileAtLimit; callers
    skip such a file.


FUNCTIONS

func Account(login, typename string) string
    Account is a login as REST names the account: a bot's GraphQL login
    ("zhuravel" with __typename "Bot") gets its "[bot]" suffix, so an App named
    like a user ("zhuravel[bot]", the user "zhuravel") stays another account.
    A user's login, one already suffixed and "" come back as they are. The
    registry keeps logins in this form.

func BetweenCalls(ctx context.Context)
    BetweenCalls runs the function WithBetweenCalls put into ctx, if any.
    The client calls it before each gh command; a fake GitHub can do the same.

func IsBot(typename, login string) bool
    IsBot reports whether an author is a bot: GraphQL __typename "Bot" or a REST
    login ending in "[bot]".

func NormalizeLogin(s string) string
    NormalizeLogin strips a trailing "[bot]" so REST logins ("talkable[bot]")
    compare equal to GraphQL logins ("talkable").

func ParseRef(s, defaultRepo string) (owner, repo string, number int, err error)
    ParseRef resolves a pull request reference: a URL
    https://github.com/o/r/pull/N[/...], "o/r#N", "r#N" (owner from
    defaultRepo), "#N" or "N" (both from defaultRepo, "owner/name").

func SameAccount(a, b string) bool
    SameAccount compares two logins in Account form case-insensitively:
    "zhuravel[bot]" and "zhuravel" are different accounts.

func SameLogin(a, b string) bool
    SameLogin compares two logins case-insensitively after NormalizeLogin:
    the same name whether or not either is a bot, so an App and a user of
    the same name match. Use it only next to a check of the kind (IsBot);
    SameAccount otherwise.

func WithBetweenCalls(ctx context.Context, fn func()) context.Context
    WithBetweenCalls returns ctx carrying fn, which the client runs before
    every gh command it makes with that context (BetweenCalls), on the caller's
    goroutine: the daemon's poll answers the requests a CLI queued meanwhile, so
    one waits for a single GitHub call, not the whole poll. fn gets no context:
    it must not make its calls with this one.


TYPES

type APIError struct {
	Op      string         // what magnum was doing ("details talkable/talkable")
	Status  int            // HTTP status when known, else 0
	Message string         // REST message or gh's stderr line
	Errors  []GraphQLError // GraphQL errors, if any
	// Details are the entries of a REST error body's "errors" array (a
	// string as it is, an object as its field and message), e.g. "Can not
	// approve your own pull request" under the message "Unprocessable
	// Entity".
	Details []string
}
    APIError is a failure GitHub reported: an HTTP status from gh, a REST error
    body, or GraphQL errors.

func (e *APIError) Error() string

func (e *APIError) Is(target error) bool
    Is maps the error onto the package sentinels.

type CIRollup struct {
	SHA   string // the commit; "" when GitHub listed none
	State string // GitHub's rollup: SUCCESS | FAILURE | PENDING | ERROR | EXPECTED; "" when the commit has no checks
	// Total is len(Checks) plus the checks GitHub did not return (past its
	// page of 100).
	Total int
	// Complete is true when Checks is every check: GitHub returned a single
	// page of at most 100.
	Complete bool
	// Checks is the latest run of each (Workflow, Name), in GitHub's order;
	// never nil. A re-run, or a SKIPPED run left from a draft, supersedes or
	// is superseded by the same job's other runs.
	Checks []Check
}
    CIRollup is the status check rollup of a pull request's head commit.

type Check struct {
	Name  string // the check run's name or the status's context
	State string // CheckPassed | CheckFailed | CheckPending | CheckSkipped
	// Workflow is the GitHub Actions workflow whose run the check belongs
	// to; "" for a commit status or another app's check run.
	Workflow string
	// At is when the check finished, else started (a check run), or was
	// posted (a commit status); zero for a check run not started yet.
	At time.Time
}
    Check is one check run or commit status of a CIRollup.

type Client struct {
	// Run executes gh. Required.
	Run execx.Runner
	// Env overlays gh's environment, e.g. GH_CONFIG_DIR for an identity.
	// It wins over the defaults (GH_PROMPT_DISABLED=1, GH_NO_UPDATE_NOTIFIER=1).
	Env map[string]string
	// Dir is gh's working directory ("" = inherit).
	Dir string
	// Reauth, when set, runs once after a call fails with ErrUnauthorized
	// (the identity's token was revoked or expired); the call is then retried
	// once. A 401 means GitHub applied nothing, so retrying a write is safe.
	// If Reauth fails the call returns the unauthorized error together with
	// Reauth's error. It may be called from several goroutines at once.
	Reauth func(ctx context.Context) error

	// Has unexported fields.
}
    Client runs gh as one identity. The zero value is unusable; set Run.

func (c *Client) CIStates(ctx context.Context, prs []PRRadar) (map[string]string, RateLimit, error)
    CIStates reads the check rollup of each pull request's head (SUCCESS,
    FAILURE, PENDING, ERROR, EXPECTED; "" when the head has no checks) by node
    id. A CI run does not move a pull request's updatedAt, so the radar alone
    cannot tell that it ended. The rollup is read through the head branch: it is
    the pull request's only while the branch's tip is the HeadRefOid the radar
    read, and a fork's pull request is not asked about (its branch carries the
    fork's checks, not the ones the pull request runs); a pull request missing
    from the result is unknown.

    Each call asks about ciPage pull requests; one GitHub could not answer in
    time is asked once more at half the size (retrySmaller), which the calls
    after it keep. A call that still fails ends the read: the states read before
    it come back with the error. The RateLimit sums Cost over the calls (1 point
    each).

func (c *Client) Compare(ctx context.Context, owner, repo, base, head string) (CompareStats, error)
    Compare reads GET /repos/{o}/{r}/compare/{base}...{head} with per_page=1:
    total_commits still counts every commit and the first page carries the
    whole (≤300) file list, but only one commit object comes back. A missing
    repository or commit is an error matching ErrNotFound.

func (c *Client) CompareFiles(ctx context.Context, owner, repo, base, head string) ([]FileDelta, error)
    CompareFiles reads the changed files of base...head with their patches
    in one call (GET /repos/{o}/{r}/compare/{base}...{head}, per_page=1:
    the first page carries the whole file list, at most CompareFileLimit files).
    A missing repository or commit is an error matching ErrNotFound.

func (c *Client) CompareFilesStatus(ctx context.Context, owner, repo, base, head string) (string, []FileDelta, error)
    CompareFilesStatus is CompareFiles that also returns the comparison's
    status as GitHub reports it: "ahead" (head descends from base), "identical",
    "behind" (base descends from head) or "diverged". The file list is three-dot
    (head's changes since the merge base), so only "ahead" and "identical" make
    it the difference between base and head.

func (c *Client) ComparePush(ctx context.Context, owner, repo, base, head string) (PushComparison, error)
    ComparePush is CompareFilesStatus that also reads up to ComparePushCommits
    of the commits (per_page=100: the first page still carries the whole file
    list) to tell whether the range brings a merge commit: a push that merged
    the base branch into the PR.

func (c *Client) ConfirmStates(ctx context.Context, owner, repo string, numbers []int) (map[int]PRState, []int, error)
    ConfirmStates reads state/merged/mergedAt/closedAt/headRefOid for pull
    requests that disappeared from the radar's OPEN list. Numbers GitHub cannot
    resolve are returned in notFound (callers treat them as UNKNOWN and never
    clean them up); any other error fails the call.

func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int, commitID, event, body string) (RESTReview, error)
    CreateReview submits a review without inline comments (POST
    /repos/{o}/{r}/pulls/{n}/reviews) on commitID as the client's identity.
    event is APPROVE, REQUEST_CHANGES or COMMENT; GitHub wants a body for the
    last two. GitHub counts each reviewer's latest review, so a new APPROVE
    or REQUEST_CHANGES supersedes the identity's earlier verdict. It is marked
    Mutates, so execx.DryRun only plans it.

func (c *Client) DeletePendingReview(ctx context.Context, owner, repo string, number int, reviewID int64) error
    DeletePendingReview deletes a review that was never submitted (DELETE
    /repos/{o}/{r}/pulls/{n}/reviews/{id}) as the client's identity; GitHub
    refuses a submitted review (an *APIError with status 422). It is marked
    Mutates, so execx.DryRun only plans it.

func (c *Client) Details(ctx context.Context, owner, repo string, numbers []int) (map[int]PRDetails, []int, error)
    Details fetches the full view of the given pull requests of owner/repo,
    batched as aliases p<N> (at most 40 per query). Numbers GitHub cannot
    resolve (NOT_FOUND, including a missing repository) are returned in missing
    instead of failing the call; any other error fails it.

func (c *Client) DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) error
    DismissReview dismisses a review (PUT
    /repos/{o}/{r}/pulls/{n}/reviews/{id}/dismissals) as the client's identity.
    It is marked Mutates, so execx.DryRun only plans it. A missing permission is
    an error matching ErrForbidden; callers report it and do not retry.

func (c *Client) FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
    FileAt reads the raw content of path at ref (a commit SHA, a branch
    or a tag): GET /repos/{o}/{r}/contents/{path}?ref={ref} with Accept:
    application/vnd.github.raw. The bytes come back verbatim; an empty file
    is an empty, non-nil slice. path is relative to the repository root
    ("app/models/order.rb"): empty, "." and ".." segments, backslashes and NULs
    are refused before any call, as is a ref that is not a plain ref name or
    SHA (the shape Compare accepts, without ".."). Call it only for paths that
    name files (a diff does): what GitHub answers for a directory is not file
    content. A missing file, ref or repository is an error matching ErrNotFound;
    a file above FileAtLimit an error matching ErrFileTooLarge (checked on the
    bytes received).

func (c *Client) LastRateLimit() RateLimit
    LastRateLimit returns the most conservative rate-limit snapshot seen by
    any GraphQL call of this client (latest reset window, lowest remaining);
    Cost is that call's cost. Zero before the first call.

func (c *Client) ListFiles(ctx context.Context, owner, repo string, number int) (files []string, complete bool, err error)
    ListFiles reads the files a pull request changes: GET
    /repos/{o}/{r}/pulls/{n}/files, up to three pages of 100. A rename
    contributes both its new and its previous name (moving a file out of src/
    into docs/ still touches src/). complete is false when the third page is
    full: the PR changes more files than were read and a caller must not draw
    conclusions from the list (GitHub itself caps this endpoint at 3000 files).
    A missing repository or pull request is an error matching ErrNotFound.

func (c *Client) PullFiles(ctx context.Context, owner, repo string, number int) (files []FileDelta, complete bool, err error)
    PullFiles reads the files a pull request changes with their patches as
    GitHub shows them in the PR's diff (GET /repos/{o}/{r}/pulls/{n}/files,
    up to PullFilesMaxPages pages of 100). A file GitHub sends no patch for
    (binary, or too large) is Truncated with an empty Patch. complete is false
    when the last page read was full: the PR changes more files than were read.
    A missing repository or pull request is an error matching ErrNotFound.

func (c *Client) PullSHAs(ctx context.Context, owner, repo string, number int) (base, head string, err error)
    PullSHAs reads a pull request's base and head commits as REST reports them
    (GET /repos/{o}/{r}/pulls/{n}: base.sha, head.sha). A missing repository or
    pull request is an error matching ErrNotFound.

func (c *Client) Radar(ctx context.Context, org string) ([]RepoRadar, RateLimit, error)
    Radar lists every non-archived repository owned by owner (an organization
    or a user) with all its open pull requests, following both the repository
    and the per-repository pull request cursors. It is all-or-nothing:
    any failed page fails the call and no repository list is returned, so the
    caller never mistakes a partial list for closed pull requests. A page GitHub
    could not answer in time is asked once more at half the size (retrySmaller),
    and the pages after it keep that size. The returned RateLimit sums Cost
    over all calls; the other fields are the most conservative snapshot seen.
    When a later page fails it still carries what the earlier pages reported,
    so the caller can pause on a low budget.

func (c *Client) RequiredChecks(ctx context.Context, owner, repo, branch string) (checks []string, known bool, err error)
    RequiredChecks reads the status checks a pull request into
    branch of owner/repo must pass: the contexts of the rulesets'
    required_status_checks rules (GET /repos/{o}/{r}/rules/branches/{branch},
    readable with read access), else of the classic branch protection
    (.../branches/{branch}/protection/required_status_checks, which usually
    answers 404 without admin access). known is false when GitHub does not say:
    403 (a private repository on a free plan) or 404 from the endpoint that
    would have to answer. Any other failure is an error.

func (c *Client) ReviewComments(ctx context.Context, owner, repo string, number int, reviewID int64) ([]ReviewComment, error)
    ReviewComments lists the inline comments of a review (GET
    /repos/{o}/{r}/pulls/{n}/reviews/{id}/comments), 100 a page until a page is
    not full, at most reviewCommentsMaxPages pages.

func (c *Client) ReviewREST(ctx context.Context, owner, repo string, number int, id int64) (RESTReview, error)
    ReviewREST reads one review over REST (GET
    /repos/{o}/{r}/pulls/{n}/reviews/{id}) for the "[bot]"-suffixed user.login.
    REST has no review lookup without the pull request number.

func (c *Client) ReviewThreads(ctx context.Context, owner, repo string, number int) ([]Thread, error)
    ReviewThreads lists the inline review threads of a pull request with
    their comments, oldest thread first (at most threadsMaxPages pages of
    threadsPageSize threads, threadCommentsMax comments each). The diff hunk
    and the original commit are read once per thread, for its first comment only
    (see ThreadComment), so replies cost no hunk. A missing repository or pull
    request is an error matching ErrNotFound.

func (c *Client) Reviews(ctx context.Context, owner, repo string, number int) ([]Review, error)
    Reviews lists every review of a pull request, oldest first: reviewsPageSize
    (100) per page, at most reviewsMaxPages (5) pages (later reviews beyond that
    are left out). A missing repository or pull request is an error matching
    ErrNotFound.

func (c *Client) ReviewsWithMarker(ctx context.Context, owner, repo string, number int, marker string) ([]Review, error)
    ReviewsWithMarker returns the last 30 reviews of a pull request whose
    body contains marker, oldest first; an empty marker returns all of them.
    A missing repository or pull request is an error matching ErrNotFound.

func (c *Client) SubmitReview(ctx context.Context, owner, repo string, number int, r ReviewRequest) (RESTReview, error)
    SubmitReview posts one review with its inline comments (POST
    /repos/{o}/{r}/pulls/{n}/reviews) as the client's identity, the request sent
    as JSON on gh's stdin so no review text reaches argv. It checks only what
    the request needs to be well formed (event, a full commit id); GitHub checks
    the rest. It is marked Mutates, so execx.DryRun only plans it.

func (c *Client) UpdateReviewBody(ctx context.Context, owner, repo string, number int, reviewID int64, body string) error
    UpdateReviewBody replaces the summary body of a review (PUT
    /repos/{o}/{r}/pulls/{n}/reviews/{id}) as the client's identity; GitHub lets
    only the review's author edit it, so another login's review fails (an error
    matching ErrForbidden or ErrNotFound). It is marked Mutates, so execx.DryRun
    only plans it.

type CompareStats struct {
	Commits   int // total_commits
	Files     int // changed files; -1 when GitHub truncated the file list (CompareFileLimit)
	Additions int // summed over the listed files: a lower bound when Files is -1
	Deletions int // likewise
}
    CompareStats is the size of base...head as GitHub's compare API reports it
    (three-dot: what head adds on top of its merge base with base).

type DraftComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Side      string `json:"side,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}
    DraftComment is one inline comment of a ReviewRequest, anchored on line
    (and, for a multi-line comment, from start_line) of path on side: RIGHT
    for the head's lines (added or context), LEFT for the base's (deleted or
    context).

type FileDelta struct {
	Path         string // the file's path at head
	PreviousPath string // its path at base when renamed or copied; "" otherwise
	Status       string // added | removed | modified | renamed | copied | changed | unchanged
	// Patch holds the file's unified-diff hunks ("@@ ... @@" lines and
	// " ", "+", "-" lines); "" when GitHub left it out.
	Patch string
	// BlobSHA is GitHub's sha of the file: its blob at head (for a removed
	// file, what GitHub lists; "" when GitHub sends null). It tells two
	// listings of a file without a patch apart (an empty or binary file).
	BlobSHA string
	// Truncated: the patch is missing or may be incomplete: GitHub left out
	// a patch too large to send (it still counts the lines), and a
	// comparison listing CompareFileLimit files may have dropped some, so
	// every file of it is marked. A file GitHub lists without a patch and
	// without a line added or removed (an empty or binary file), with its
	// blob, in a listing below the cap is complete: its diff is empty, not
	// missing.
	Truncated bool
}
    FileDelta is one changed file of a comparison (CompareFiles).

type GraphQLError struct {
	Type    string   // NOT_FOUND, FORBIDDEN, RATE_LIMITED, ...; "" for validation errors
	Message string   //
	Path    []string // e.g. ["repository", "p123"]
}
    GraphQLError is one entry of a GraphQL response's "errors" array.

func (g *GraphQLError) UnmarshalJSON(b []byte) error
    UnmarshalJSON accepts GitHub's mixed string/number paths.

type LatestReview struct {
	State       string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	SubmittedAt time.Time
	AuthorLogin string // "" for a ghost
	AuthorType  string
	CommitOid   string // "" when the commit is gone
}
    LatestReview is one entry of a pull request's latestReviews.

type PRDetails struct {
	NodeID      string
	Number      int
	Title       string
	URL         string
	AuthorLogin string // "" for a deleted (ghost) author
	AuthorType  string // GraphQL __typename: User, Bot, Mannequin, ...
	// AuthorAssociation is the author's relationship with the repository
	// (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR,
	// FIRST_TIMER, MANNEQUIN, NONE), as of now.
	AuthorAssociation string
	Labels            []string
	// LabelsComplete is true when Labels is every label of the pull request.
	// GitHub returned a single page of at most 100 labels; when the
	// connection reports more, the list is truncated and must not be used to
	// decide that a skip label is absent.
	LabelsComplete    bool
	HeadRefName       string
	BaseRefName       string
	IsCrossRepository bool
	State             string // OPEN | CLOSED | MERGED
	Merged            bool
	MergedAt          time.Time // zero when not merged
	ClosedAt          time.Time // zero when open
	UpdatedAt         time.Time
	IsDraft           bool
	HeadRefOid        string
	BaseRefOid        string   // the base branch's current tip
	Assignees         []string // logins; never nil
	ReviewRequests    []Reviewer
	LatestReviews     []LatestReview // latest review per reviewer; never nil
	// LatestReviewsComplete is true when LatestReviews has every reviewer's
	// latest review (one page of at most 100); when false, the
	// identity's own review may be missing from the list.
	LatestReviewsComplete bool
	// ReviewRequestEvents are the newest review requests of the timeline
	// (at most 10, oldest first); never nil.
	ReviewRequestEvents []ReviewRequestEvent
	// Size of the whole pull request (merge base of BaseRefOid ... HeadRefOid).
	Additions    int
	Deletions    int
	ChangedFiles int
	Commits      int
	// Files are the paths the pull request changes at HeadRefOid, in
	// GitHub's order: one page of at most 100 (a rename lists its new path
	// only); nil when GitHub returned no list. FilesComplete is true when
	// they are every changed file.
	Files         []string
	FilesComplete bool
	// CI is the head commit's checks.
	CI CIRollup
	// ActivityAt is the pull request's last activity: the latest of its
	// opening, a description edit, the head commit (its committer date, never
	// past UpdatedAt), a force push, a comment, a review (a reply in a thread
	// is one), a label added or removed, a review requested or removed, ready
	// for review or back to draft, a title rename, a base change, a close, a
	// reopen and a merge (activityTypes; bots count, checks do not). Unlike
	// UpdatedAt it does not move for what a reviewer never sees (a project
	// field, a resolved thread, someone's pending review, a deleted comment).
	// Zero when GitHub returned no timeline.
	ActivityAt time.Time
	// ReviewGate is what GitHub's merge gate says of the reviews; nil when
	// GitHub returned no reviewDecision or latestOpinionatedReviews.
	ReviewGate *ReviewGate
}
    PRDetails is what the poller stores for a pull request whose radar row
    changed. Logins are GraphQL logins, which never carry the "[bot]" suffix;
    compare them with SameLogin.

type PRRadar struct {
	NodeID      string
	Number      int
	IsDraft     bool
	UpdatedAt   time.Time
	HeadRefOid  string
	BaseRefName string
	// IsCrossRepository is true for a pull request from a fork.
	IsCrossRepository bool
	// CIState is the head commit's check rollup (SUCCESS, FAILURE, PENDING,
	// ERROR, EXPECTED), "" when it has no checks, and CIKnown says whether
	// it was read. Radar leaves both unset: the caller fills them from
	// CIStates.
	CIState string
	CIKnown bool
}
    PRRadar is the scalar-only view of an open pull request the poller diffs.

type PRState struct {
	State      string // OPEN | CLOSED | MERGED
	Merged     bool
	MergedAt   time.Time // zero when not merged
	ClosedAt   time.Time // zero when open
	HeadRefOid string
	// MergeCommitOid is the commit the merge put on the base branch: the
	// merge commit, the squash commit or the last rebased commit ("" until
	// merged). Its first parent is the base branch before the merge.
	MergeCommitOid string
}
    PRState is the closed/merged confirmation of one pull request.

type PushComparison struct {
	// Status is GitHub's: "ahead", "identical", "behind" or "diverged"
	// (CompareFilesStatus).
	Status string
	// Commits is total_commits: the commits head has since the merge base.
	Commits int
	// Merge: one of those commits has more than one parent, or GitHub
	// listed fewer of them than Commits (more than ComparePushCommits), so
	// a merge cannot be ruled out.
	Merge bool
	// SHAs are the commits GitHub listed, oldest first: all Commits of them
	// unless there are more than ComparePushCommits.
	SHAs  []string
	Files []FileDelta
	// Stats is what Compare reads of the same range (the commits, the
	// files, -1 at GitHub's file cap, and their additions and deletions),
	// so this comparison answers that question too.
	Stats CompareStats
}
    PushComparison is a comparison read with its commits (ComparePush).

type RESTReview struct {
	ID          int64
	NodeID      string
	UserLogin   string
	UserType    string // User | Bot
	State       string
	Body        string
	HTMLURL     string
	CommitID    string
	SubmittedAt time.Time
}
    RESTReview is a review as REST reports it; UserLogin keeps the "[bot]"
    suffix ("talkable[bot]"), which identifies the posting App.

type RateLimit struct {
	Limit     int
	Cost      int // points the call(s) cost
	Remaining int
	Used      int
	ResetAt   time.Time
}
    RateLimit is GitHub's GraphQL rateLimit{} block.

type RepoRadar struct {
	NodeID        string
	NameWithOwner string    // "talkable/talkable"
	PushedAt      time.Time // zero for an empty repository
	DefaultBranch string    // "" for an empty repository
	PRs           []PRRadar
}
    RepoRadar is one non-archived repository of an organization or user with its
    open pull requests.

type Review struct {
	DatabaseID  int64  // REST review id
	State       string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	Body        string
	URL         string
	SubmittedAt time.Time
	CommitOid   string // "" when the commit is gone
	AuthorLogin string // "" for a ghost
	AuthorType  string // GraphQL __typename
}
    Review is a pull request review as GraphQL reports it. AuthorLogin is the
    GraphQL login (no "[bot]" suffix); ReviewREST has the REST login.

type ReviewComment struct {
	ID      int64
	Path    string
	Line    int // 0 when GitHub reports none (an outdated comment)
	Body    string
	HTMLURL string
}
    ReviewComment is one inline comment of a review as REST reports it.

type ReviewGate struct {
	// Decision is GitHub's reviewDecision: APPROVED, CHANGES_REQUESTED or
	// REVIEW_REQUIRED; "" (null) when the base branch requires no review.
	Decision string
	// Opinions are latestOpinionatedReviews(writersOnly: true): the latest
	// approval or changes request (DISMISSED once dismissed) of each
	// reviewer with write access, which is what Decision counts; never nil.
	// SubmittedAt is not read.
	Opinions []LatestReview
	// Complete is true when Opinions is every one (one page of at most 100).
	Complete bool
}
    ReviewGate is what GitHub's branch protection makes of a pull request's
    reviews: its reviewDecision and the reviews that decision counts. A GitHub
    App's review never counts, so a PR an App approved can still wait for a
    person's approval.

type ReviewRequest struct {
	CommitID string         `json:"commit_id"`
	Event    string         `json:"event"` // APPROVE, REQUEST_CHANGES or COMMENT
	Body     string         `json:"body"`
	Comments []DraftComment `json:"comments"`
}
    ReviewRequest is the body of POST /repos/{o}/{r}/pulls/{n}/reviews:
    one submitted review with all its inline comments (SubmitReview).

type ReviewRequestEvent struct {
	CreatedAt time.Time
	// Actor is the login that asked for the review ("" for a ghost).
	Actor string
	// Reviewer is who was asked: Type User, Bot, Mannequin or Team (Login is
	// then the team's slug). GraphQL logins carry no "[bot]" suffix; compare
	// them with SameLogin.
	Reviewer Reviewer
}
    ReviewRequestEvent is one ReviewRequestedEvent of a pull request's timeline.

type Reviewer struct {
	Type  string // User | Bot | Mannequin | Team
	Login string
}
    Reviewer is a requested reviewer. For Type "Team", Login is the team slug.

type Thread struct {
	ID           string // GraphQL node id (PRRT_…)
	Path         string
	Line         int // 0 when GitHub reports none (an outdated thread); the last line of a multi-line thread
	OriginalLine int // the line the thread was started on (the last line of a multi-line range)
	// StartLine is the first line of a multi-line thread's range, so the
	// thread covers StartLine..Line; 0 for a single-line thread (and when
	// GitHub reports none, as for an outdated thread). OriginalStartLine is
	// the same for the range the thread was started on (OriginalLine is its
	// last line).
	StartLine         int
	OriginalStartLine int
	// DiffSide is the side of the diff the thread is on: "RIGHT" (the
	// new file's lines) or "LEFT" (deleted lines, numbered in the old file).
	DiffSide string
	Resolved bool
	Outdated bool
	// Comments are oldest first; Comments[0] started the thread and the rest
	// are its replies. At most threadCommentsMax per thread.
	Comments []ThreadComment
}
    Thread is an inline review thread of a pull request as GraphQL reports it.

type ThreadComment struct {
	ID          int64  // REST comment id (in_reply_to for a reply)
	AuthorLogin string // GraphQL login (no "[bot]" suffix); "" for a ghost
	AuthorType  string // GraphQL __typename: User | Bot
	Body        string
	URL         string
	CreatedAt   time.Time
	ReviewID    int64 // the review the comment belongs to; 0 when unknown
	// OriginalCommitOid is the commit the comment was made on, and DiffHunk
	// the diff hunk it was made on (the lines around the commented one, as
	// they were then). Both are set on a thread's first comment (Comments[0])
	// only, the comment that anchors the thread; replies carry neither, and
	// OriginalCommitOid is "" when GitHub reports no commit (it is
	// gone).
	OriginalCommitOid string
	DiffHunk          string
}
    ThreadComment is one comment of a review thread.

```

## gitx

```text
package gitx // import "github.com/zhuravel/magnum/internal/gitx"

Package gitx holds every git operation magnum needs: fetching PR heads into
refs/magnum/pr/N, detached switches and placeholder resets in review slots,
worktree management, dirty/unpushed guards and the diff queries behind "did this
PR touch db/".

All commands go through an execx.Runner (never os/exec), run as `git -C <dir>
...`, and are marked Mutates when they change repository state so --dry-run only
prints them. Inputs that end up as git arguments (refs, branch names, worktree
paths) are validated so a hostile branch name can never be parsed as an option.
The variables that redirect git to another repository (GIT_DIR and friends,
see ScrubbedEnv) are removed from the environment of every command.

CONSTANTS

const EvalRefPrefix = "refs/magnum/eval/"
    EvalRefPrefix holds the refs magnum eval fetches pinned commits into,
    apart from refs/magnum/pr/* (the daemon's PR heads), which it never touches.

const GHCredentialHelper = "!gh auth git-credential"
    GHCredentialHelper is the git credential helper that answers with gh's login
    for github.com (`gh auth git-credential`).

const WorktreesSuffix = "__worktrees"
    WorktreesSuffix is appended to a main clone's path to name the directory
    holding magnum's per-PR worktrees (<clone>__worktrees/pr-N). FindClone skips
    such directories.


VARIABLES

var (
	// ErrNoSuchRef is returned when a ref or revision does not resolve.
	ErrNoSuchRef = errors.New("gitx: no such ref")
	// ErrNoMergeBase is returned when two revisions share no history.
	ErrNoMergeBase = errors.New("gitx: no merge base")
)
var ErrNoClone = errors.New("gitx: no clone of the repository")
    ErrNoClone is returned by FindClone when no directory under the clone root
    is a git clone of the repository.


FUNCTIONS

func GitHubHTTPSURL(owner, repo string) string
    GitHubHTTPSURL is the https clone URL of github.com/<owner>/<repo>.

func GitHubSSHURL(owner, repo string) string
    GitHubSSHURL is the scp-like SSH clone URL of github.com/<owner>/<repo>.

func IsRepo(dir string) bool
    IsRepo reports whether dir has a .git entry (a directory for a clone,
    a file for a linked worktree or submodule). It never runs git.

func IsSSHURL(raw string) bool
    IsSSHURL reports whether a remote URL goes over SSH: an ssh:// (or
    git+ssh://) URL, or git's scp-like [user@]host:path form, which git
    recognises only when no slash comes before the first colon (so a local path
    is never one).

func NetworkRemote(originURL string, https bool) (opts []string, target string)
    NetworkRemote is how a network command (fetch, ls-remote) reaches a
    clone whose origin is originURL. With https set, a github.com SSH origin
    is reached at its HTTPS URL with gh as the only credential helper:
    opts go before the subcommand and target replaces "origin", so a locked or
    empty ssh-agent does not matter. Anything else (https off, an HTTPS origin,
    an SSH origin on another host, an unknown origin) names origin itself with
    no options.

func PRRef(number int) string
    PRRef is the local ref a PR head is fetched into.

func ScrubbedEnv() []string
    ScrubbedEnv returns the environment variables gitx removes from every git
    command it runs. Code that runs git outside gitx should put the same list in
    execx.Cmd.Unset.


TYPES

type Client struct {

	// HTTPSFetch makes fetches from a clone whose origin is a github.com SSH
	// URL go over HTTPS with gh as the credential helper (see fetchArgs).
	// The app turns it on; the zero value keeps fetching from origin.
	HTTPSFetch bool

	// Has unexported fields.
}
    Client runs git through an execx.Runner. It is safe for concurrent use;
    fetches into the same clone are serialized because concurrent fetches of
    the same ref (two PRs sharing a base branch) fail with "cannot lock ref".
    The lock only covers this process; a fetch that still loses the race to
    another process (the CLI next to the daemon) is retried once.

func New(r execx.Runner) *Client
    New returns a Client that executes commands with r.

func (c *Client) BranchDelete(ctx context.Context, mainClone, branch string, force bool) error
    BranchDelete deletes a local branch (force = -D, otherwise -d). A branch
    that does not exist counts as deleted; git's refusal to delete an existing
    branch (checked out, unmerged) is returned.

func (c *Client) BranchPR(ctx context.Context, dir, branch string) (number int, ok bool, err error)
    BranchPR returns the PR number recorded in `git config branch.<b>.pr` (the
    convention of Bohdan's `pull` helper; a PR URL is accepted too). ok is false
    when the key is not set.

func (c *Client) ChangedPaths(ctx context.Context, dir, base, head string, pathspecs ...string) ([]string, error)
    ChangedPaths lists the files head changed relative to its merge base with
    base (git diff base...head), optionally limited to pathspecs such as "db/".
    Renames are reported as a deletion plus an addition so a move out of a
    watched directory still shows up. The result is nil when nothing matches.

func (c *Client) Clone(ctx context.Context, repoURL, dest string) error
    Clone clones url into the absolute path dest (parents are created). URLs
    with embedded credentials are refused; authentication comes from the user's
    git credential helper or ssh agent.

func (c *Client) CloneWith(ctx context.Context, repoURL, dest string, opt CloneOptions) error
    CloneWith is Clone with options (see CloneOptions).

func (c *Client) FetchBranch(ctx context.Context, mainClone, base string) error
    FetchBranch updates refs/remotes/origin/<base> in mainClone from the remote.

func (c *Client) FetchCommit(ctx context.Context, mainClone, sha string, number int) error
    FetchCommit makes sha (a full commit id) present in mainClone for a magnum
    eval replay, or for a post-merge round whose clone lacks the PR's merge
    commit. A commit already there costs one rev-parse. Otherwise it fetches the
    commit by id into refs/magnum/eval/<sha>, and when the server refuses that,
    PR number's head into refs/magnum/eval/pr-<number> (the commit is there
    unless the PR was force-pushed past it). It never writes refs/magnum/pr/*,
    so the daemon's checkouts are unaffected.

func (c *Client) FetchPR(ctx context.Context, mainClone string, number int) (string, error)
    FetchPR fetches the PR head (refs/pull/N/head) into refs/magnum/pr/N
    of mainClone, forcing the update so a force-pushed PR moves the ref,
    and returns the commit sha it now points to. Under --dry-run the fetch is
    only planned, so the final lookup returns whatever refs/magnum/pr/N already
    holds (the sha of an earlier real fetch, possibly stale) or fails with
    ErrNoSuchRef.

func (c *Client) FileDiff(ctx context.Context, dir, base, head, path string) (string, error)
    FileDiff returns the unified diff of one file from base to head (git diff
    base head -- path; path is relative to the repository's top and taken
    literally) as GitHub shows a pull request's patch: three lines of context,
    hunks never merged across a gap, and none of the user's settings that change
    the hunks (an external diff, textconv, a context size). "" when the file did
    not change; a binary file has a header and no hunks.

func (c *Client) FindClone(ctx context.Context, cloneRoot, owner, name string) (string, error)
    FindClone returns the main clone of github.com/<owner>/<name> under
    cloneRoot (an absolute, already expanded directory). Candidates are tried
    in order: <root>/<name>, <root>/<owner>-<name>, <root>/<owner>_<name>; the
    first that is a git repository whose `git remote get-url origin` is a GitHub
    URL of <owner>/<name> wins (see originMatches; owner and name are compared
    case-insensitively). Otherwise every directory directly under the root (not
    recursively; names containing "__worktrees" are skipped) is checked the same
    way, in name order. A directory is asked once however many names reach it
    (symlinks, case-insensitive filesystems). A directory without a .git entry
    is never handed to git, so a same-named folder that is not a repository
    costs no subprocess and no warning.

    Origins are cached per directory until its git config changes, so repeated
    lookups cost a directory listing and a few stats. ErrNoClone (wrapped) means
    nothing matched; any other error is an unreadable root.

func (c *Client) LsRemote(ctx context.Context, dir string, timeout time.Duration, patterns ...string) (string, error)
    LsRemote runs `git ls-remote <origin> patterns...` in dir, reaching origin
    the way fetches do (NetworkRemote with HTTPSFetch), and returns its output.
    A failure is an expected answer (logged at Debug): callers probe with it.

func (c *Client) MergeBase(ctx context.Context, dir, a, b string) (string, error)
    MergeBase returns the best common ancestor of a and b, or ErrNoMergeBase.

func (c *Client) OwnerUsesSSH(ctx context.Context, cloneRoot, owner string) (bool, error)
    OwnerUsesSSH reports whether a clone directly under cloneRoot of any
    github.com/<owner>/* repository has an SSH origin. Directories are read as
    FindClone reads them (".git" required, "__worktrees" skipped, origins cached
    until the clone's config changes), so right after a FindClone miss over the
    same root it costs no subprocess. A missing root is (false, nil).

func (c *Client) RemoteURL(ctx context.Context, dir string) (string, error)
    RemoteURL returns the URL of origin in dir. Credentials embedded in the URL
    are stripped (see stripUserinfo) so the value is safe to log.

func (c *Client) ResetPlaceholder(ctx context.Context, dir, branch, base string) error
    ResetPlaceholder points the slot's placeholder branch at origin/<base>
    without upstream tracking, discarding tracked changes. It is idempotent.
    `switch -C --no-track` only stops git from creating tracking; a branch
    that already tracks something keeps it, so an existing upstream is unset
    afterwards (placeholders must not follow, or push to, a remote branch).

func (c *Client) RevParse(ctx context.Context, dir, ref string) (string, error)
    RevParse resolves ref to a full commit sha (tags are peeled). A ref that
    does not exist yields an error wrapping ErrNoSuchRef.

func (c *Client) Status(ctx context.Context, dir string) (Status, error)
    Status inspects the working tree at dir without taking the index lock.

func (c *Client) StatusPaths(ctx context.Context, dir string) ([]StatusEntry, error)
    StatusPaths lists the changes in the working tree at dir: tracked changes
    and untracked files (`--untracked-files=normal`, so a wholly untracked
    directory is one entry). Ignored files (.mise.local.toml, tmp/) are not
    requested. It is read-only and does not take the index lock.

func (c *Client) SwitchDetach(ctx context.Context, dir, ref string) error
    SwitchDetach detaches dir's HEAD at ref, discarding tracked changes
    (untracked files stay). Callers verify the result with RevParse.

func (c *Client) TreeFiles(ctx context.Context, dir, rev string, pathspecs ...string) ([]string, error)
    TreeFiles lists the files of rev (a commit) that pathspecs match,
    with the pathspec rules ChangedPaths has ("db/", a glob, pathspec magic),
    each as "<mode> <blob id> <path>", in git's order; nil when none matches.
    It is `git diff-tree` of the empty tree against rev, unabbreviated, so the
    list changes exactly when a matching file's content, mode or name does.
    A rev that does not resolve is ErrNoSuchRef.

func (c *Client) Unpushed(ctx context.Context, dir string) (int, error)
    Unpushed counts commits reachable from HEAD that exist on no remote-tracking
    ref and no refs/magnum/* ref, i.e. work a human committed in a slot that a
    reset would orphan. A HEAD sitting on a fetched PR ref counts as zero.

func (c *Client) UnpushedRef(ctx context.Context, dir, ref string) (int, error)
    UnpushedRef is Unpushed for an arbitrary ref (a placeholder branch,
    a detached sha): the commits reachable from ref that exist on no
    remote-tracking ref and no refs/magnum/* ref. Read-only.

func (c *Client) UpdateRefDelete(ctx context.Context, mainClone, ref string) error
    UpdateRefDelete deletes a ref under refs/magnum/ (for example PRRef(n)).
    It is idempotent: a missing ref is not an error. Other namespaces are
    refused so a bug can never delete a real branch or tag. A symbolic ref is
    never followed: one pointing outside refs/magnum/ is refused, and the delete
    runs with --no-deref so git removes the ref itself, not its target.

func (c *Client) WorktreeAdd(ctx context.Context, mainClone, path, ref string, detach bool, branch string) error
    WorktreeAdd creates a worktree at the absolute path, checking out ref.
    With detach it is a detached HEAD at ref; with branch it creates that new
    branch at ref without upstream tracking; with neither, git decides (a branch
    name checks the branch out, anything else detaches). Callers that must be
    idempotent check WorktreeList first, since git refuses an existing path.

func (c *Client) WorktreeList(ctx context.Context, mainClone string) ([]Worktree, error)
    WorktreeList returns every worktree of the repository, the main one first.
    A path containing a newline is not supported.

func (c *Client) WorktreePrune(ctx context.Context, mainClone string) error
    WorktreePrune drops administrative entries of worktrees whose directory
    is gone. Callers need it after a slot directory vanished: git refuses to
    add a worktree at that path or delete the branch "used by" it until the
    stale registration is forgotten. `git worktree prune` takes no path, so it
    also forgets other worktrees of the clone whose directory is missing (for
    example one on an unmounted volume). That is accepted instead of editing
    .git/worktrees/<id> by hand; a worktree locked with `git worktree lock` is
    never pruned.

func (c *Client) WorktreeRemove(ctx context.Context, mainClone, path string, force bool) error
    WorktreeRemove removes the worktree at path. force also removes a worktree
    with modified or untracked files (git refuses without it); a locked worktree
    is never removed.

type CloneOptions struct {
	// CredentialHelper, when set, is the only credential helper of the clone
	// command and of the new clone: it is passed as `git -c
	// credential.helper= -c credential.helper=<it> clone` (the empty value
	// clears the user's helpers, such as a keychain holding another login)
	// and written into the clone's local config with the same two
	// `clone --config` values, so later fetches use it too.
	CredentialHelper string
}
    CloneOptions adjust Clone.

type Remote struct {
	Host  string // lower-cased, e.g. github.com
	Owner string
	Repo  string // without a .git suffix
}
    Remote identifies a GitHub repository parsed from a remote URL.

func ParseRemote(raw string) (Remote, bool)
    ParseRemote understands https, http, git and ssh URLs and the scp-like
    git@host:owner/repo.git form. ok is false for local paths and anything that
    does not look like host/owner/repo.

func (r Remote) FullName() string
    FullName returns "owner/repo".

type Status struct {
	Tracked   int // entries with changes to tracked files (modified, added, deleted, renamed, conflicted)
	Untracked int // untracked files or directories
}
    Status summarizes `git status --porcelain` of a working tree. Ignored files
    (.mise.local.toml, tmp/) are not counted.

func (s Status) Dirty() bool
    Dirty reports whether anything differs from HEAD, untracked files included.

func (s Status) UntrackedOnly() bool
    UntrackedOnly reports whether the only dirt is untracked files.

type StatusEntry struct {
	// X and Y are the two status letters: X for the index, Y for the work
	// tree (' ' unchanged, 'M', 'A', 'D', 'R', 'C', 'U' unmerged, '?' for an
	// untracked file, which has both letters '?').
	X, Y byte
	// Path is relative to the top level of the work tree, not to the directory
	// StatusPaths was given. In -z mode it is raw: spaces, newlines and
	// non-ASCII bytes are not quoted.
	Path string
	// OrigPath is the source of a rename or copy; empty for every other entry.
	OrigPath string
}
    StatusEntry is one record of `git status --porcelain=v1 -z`.

func (e StatusEntry) Ignored() bool
    Ignored reports whether the entry is an ignored file. StatusPaths never asks
    for ignored files, so it never returns such an entry.

func (e StatusEntry) Untracked() bool
    Untracked reports whether the entry is an untracked file or directory.

type Worktree struct {
	Path       string // as reported by git (symlinks resolved); compare with FindWorktree
	Head       string // full commit sha; empty for a bare repository
	Branch     string // short branch name (refs/heads/ stripped); empty when detached or bare
	Detached   bool
	Bare       bool
	Locked     bool
	LockReason string
	Prunable   bool // administrative files are stale (for example the directory was deleted)
}
    Worktree is one entry of `git worktree list --porcelain`.

func FindWorktree(list []Worktree, path string) (Worktree, bool)
    FindWorktree returns the entry for path. Both sides are cleaned and,
    where they exist, symlink-resolved, so /var/... and /private/var/...
    spellings (and a trailing slash) match.

```

## herdr

```text
package herdr // import "github.com/zhuravel/magnum/internal/herdr"

Package herdr is magnum's client for the herdr terminal multiplexer's
Unix-socket API (verified against herdr 0.9.x, protocol 22).

Protocol: newline-delimited JSON. Each request {"id","method","params"}
goes out on a fresh connection; the server writes exactly one response,
{"id","result":{"type":…}} or {"id","error":{"code","message"}},
and closes the connection. events.subscribe is the exception: after the
{"type":"subscription_started"} ack the connection stays open and streams
{"event","data"} lines (see Subscribe).

Server errors surface as *Error carrying herdr's code (agent_blocked,
agent_prompt_stalled, timeout, ui_busy, invalid_key, …). An unreachable socket
wraps ErrUnavailable; a client-side deadline wraps ErrTimeout. Every call logs
the equivalent `herdr …` CLI command, redacted, through the optional Logger so a
human can replay what magnum did.

CONSTANTS

const (
	CodeAgentBlocked       = "agent_blocked"
	CodeAgentPromptStalled = "agent_prompt_stalled"
	CodeTimeout            = "timeout"
	CodeUIBusy             = "ui_busy"
	CodeInvalidKey         = "invalid_key"
	CodeInvalidRequest     = "invalid_request"
	CodeAgentNotFound      = "agent_not_found"
	CodePaneNotFound       = "pane_not_found"
	CodeTabNotFound        = "tab_not_found"
	CodeWorkspaceNotFound  = "workspace_not_found"
)
    Herdr error codes magnum reacts to.

const (
	SubPaneAgentStatusChanged = "pane.agent_status_changed"
	SubPaneExited             = "pane.exited"
	SubPaneClosed             = "pane.closed"
	SubWorkspaceClosed        = "workspace.closed"
)
    Subscription types (dot form, as sent in events.subscribe).
    SubPaneAgentStatusChanged requires PaneID; the others are global.

const (
	EventPaneAgentStatusChanged = "pane.agent_status_changed"
	EventPaneExited             = "pane_exited"
	EventPaneClosed             = "pane_closed"
	EventWorkspaceClosed        = "workspace_closed"
)
    Streamed event names, as they arrive in Event.Event. The pane-scoped
    status event uses the dot spelling of the bundled schema's
    subscription_event.SubscriptionEventKind (the envelope events.subscribe
    streams); the lifecycle events use the snake_case event_kinds spelling.

const (
	SplitRight = "right"
	SplitDown  = "down"
)
    Split directions for PaneSplit.

const (
	SourceVisible         = "visible"
	SourceRecent          = "recent"
	SourceRecentUnwrapped = "recent_unwrapped"
	SourceDetection       = "detection"
)
    Read sources for PaneRead/AgentRead/PaneWaitOutput.

const (
	FormatText = "text"
	FormatANSI = "ansi"
)
    Read formats.

const (
	PlacementPopup   = "popup"
	PlacementOverlay = "overlay"
	PlacementSplit   = "split"
	PlacementTab     = "tab"
	PlacementZoomed  = "zoomed"
)
    Plugin pane placements. The CLI rejects popup; the socket accepts it.

const DefaultTimeout = 15 * time.Second
    DefaultTimeout bounds a call that has no server-side wait when
    Client.Timeout is zero.

const Protocol = 22
    Protocol is the herdr socket protocol version this client was verified
    against (ServerInfo.Protocol); `magnum doctor` warns on any other.


VARIABLES

var (
	// ErrUnavailable wraps failures to reach the socket (no server running,
	// stale socket file, permission denied).
	ErrUnavailable = errors.New("herdr unavailable")
	// ErrTimeout wraps client-side deadlines (Client.Timeout, ctx deadline,
	// WaitIdleShell). IsTimeout also matches herdr's own "timeout" code.
	ErrTimeout = errors.New("herdr request timed out")
)

FUNCTIONS

func DefaultSocket() string
    DefaultSocket returns $HERDR_SOCKET_PATH (set for herdr plugins and panes)
    or ~/.config/herdr/herdr.sock.

func IdleShell(pi ProcessInfo) bool
    IdleShell reports whether the pane's foreground process group is its shell,
    i.e. nothing runs in it and agent.start may type into it. Unknown pids
    (null) are never idle.

func IsCode(err error, code string) bool
    IsCode reports whether err is (or wraps) a herdr *Error with the given code.

func IsTimeout(err error) bool
    IsTimeout reports whether err is a client-side deadline or herdr's own
    "timeout" error.

func RequiredMethods() []string
    RequiredMethods returns the socket methods magnum needs the herdr server to
    offer (`magnum doctor` checks them against `herdr api schema`).


TYPES

type AgentInfo struct {
	PaneID                 string            `json:"pane_id"`
	TerminalID             string            `json:"terminal_id"`
	WorkspaceID            string            `json:"workspace_id"`
	TabID                  string            `json:"tab_id"`
	Name                   string            `json:"name"` // set by agent.start / agent.rename
	Agent                  string            `json:"agent"`
	DisplayAgent           string            `json:"display_agent"`
	AgentStatus            Status            `json:"agent_status"`
	AgentSession           *AgentSession     `json:"agent_session"`
	Cwd                    string            `json:"cwd"`
	ForegroundCwd          string            `json:"foreground_cwd"`
	Focused                bool              `json:"focused"`
	InteractiveReady       bool              `json:"interactive_ready"`
	LaunchPending          bool              `json:"launch_pending"`
	ScreenDetectionSkipped bool              `json:"screen_detection_skipped"`
	CompletionSeq          *uint64           `json:"completion_seq"` // null on live agents in 0.9.x
	StateChangeSeq         uint64            `json:"state_change_seq"`
	Revision               uint64            `json:"revision"`
	Title                  string            `json:"title"`
	TerminalTitle          string            `json:"terminal_title"`
	TerminalTitleStripped  string            `json:"terminal_title_stripped"`
	Tokens                 map[string]string `json:"tokens,omitempty"`
}
    AgentInfo is a pane that carries an agent (agent.get/list, snapshot agents).

type AgentSession struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Source string `json:"source"` // e.g. "herdr:codex"
	Value  string `json:"value"`
}
    AgentSession identifies the agent conversation running in a pane: Kind "id"
    with a Codex/Claude session uuid in Value, or "path".

type AgentStartOptions struct {
	Name    string // [a-z][a-z0-9_-]{0,31}, unique per server
	Kind    string // codex | claude | …
	PaneID  string // must be an idle shell (see WaitIdleShell)
	Timeout time.Duration
	Args    []string // extra argv after the agent command
}
    AgentStartOptions configures AgentStart.

type AgentStarted struct {
	Agent AgentInfo `json:"agent"`
	Argv  []string  `json:"argv"`
}
    AgentStarted is the agent.start reply; Argv is what herdr typed/launched.

type Client struct {
	// Socket is the herdr.sock path; "" means DefaultSocket().
	Socket string
	// Timeout bounds calls without a server-side wait; zero means
	// DefaultTimeout. Calls that wait server-side get their wait on top.
	Timeout time.Duration
	// Logger, when set, receives one debug line per call with the equivalent
	// redacted `herdr …` CLI command, its duration and error.
	Logger execx.Logger
}
    Client talks to one herdr server. The zero value uses DefaultSocket and
    DefaultTimeout. A Client is safe for concurrent use: it holds no connection
    between calls.

func (c *Client) AgentFocus(ctx context.Context, target string) error
    AgentFocus focuses the agent's pane inside herdr.

func (c *Client) AgentGet(ctx context.Context, target string) (AgentInfo, error)
    AgentGet returns one agent by name or pane id.

func (c *Client) AgentList(ctx context.Context) ([]AgentInfo, error)
    AgentList returns every pane that carries an agent, magnum's or not.

func (c *Client) AgentPrompt(ctx context.Context, target, text string, wait *PromptWait) (AgentInfo, error)
    AgentPrompt submits text to an agent. With wait nil it returns once
    the text is accepted. Errors: agent_blocked (rejected before sending),
    agent_prompt_stalled (no working/blocked within 5 s), timeout.

func (c *Client) AgentRead(ctx context.Context, target string, o ReadOptions) (ReadResult, error)
    AgentRead returns recent output of an agent's pane.

func (c *Client) AgentRename(ctx context.Context, target, name string) error
    AgentRename renames an agent.

func (c *Client) AgentSendKeys(ctx context.Context, target string, keys ...string) error
    AgentSendKeys sends key presses to an agent's pane.

func (c *Client) AgentStart(ctx context.Context, o AgentStartOptions) (AgentStarted, error)
    AgentStart launches an agent in an idle shell pane and names it.

func (c *Client) Call(ctx context.Context, method string, params, out any) error
    Call sends one request and decodes the response's result object (the value
    of "result", including its "type" field) into out when out is non-nil.
    params nil sends {}. The call is bounded by Client.Timeout and ctx; use the
    typed methods for requests that wait server-side.

func (c *Client) NotificationShow(ctx context.Context, title, body string) (NotificationResult, error)
    NotificationShow shows a herdr toast (delivered per herdr's [ui.toast]).

func (c *Client) PaneGet(ctx context.Context, paneID string) (Pane, error)
    PaneGet returns one pane.

func (c *Client) PaneList(ctx context.Context, workspaceID string) ([]Pane, error)
    PaneList lists panes, optionally limited to one workspace.

func (c *Client) PaneProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error)
    PaneProcessInfo returns the pane's shell pid and foreground process group.

func (c *Client) PaneRead(ctx context.Context, paneID string, o ReadOptions) (ReadResult, error)
    PaneRead returns recent pane output.

func (c *Client) PaneRename(ctx context.Context, paneID, label string) error
    PaneRename sets a pane label; "" clears it.

func (c *Client) PaneRun(ctx context.Context, paneID, command string) error
    PaneRun types command into the pane followed by Enter, atomically (the CLI's
    `pane run`, which is pane.send_input on the socket).

func (c *Client) PaneSendKeys(ctx context.Context, paneID string, keys ...string) error
    PaneSendKeys sends key presses (herdr grammar: Enter, ctrl+c, "1", …).

func (c *Client) PaneSplit(ctx context.Context, paneID string, o SplitOptions) (Pane, error)
    PaneSplit splits paneID and returns the new pane (a fresh shell).

func (c *Client) PaneWaitOutput(ctx context.Context, paneID string, o WaitOutputOptions) (OutputMatch, error)
    PaneWaitOutput blocks server-side until the pane output matches. Existing
    output counts. herdr's own timeout surfaces as code "timeout".

func (c *Client) Ping(ctx context.Context) (ServerInfo, error)
    Ping checks the server and returns its version and protocol.

func (c *Client) PluginPaneOpen(ctx context.Context, o PluginPaneOptions) (PluginPane, error)
    PluginPaneOpen opens a plugin pane; only the socket accepts popup placement.

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error)
    Snapshot returns the whole live session (session.snapshot).

func (c *Client) Subscribe(ctx context.Context, subs []Subscription, handler func(Event)) error
    Subscribe opens an events.subscribe stream and calls handler, on this
    goroutine, for every event until ctx is done (ctx.Err() wrapped),
    the server closes the stream (io.EOF wrapped) or the connection fails.
    The ack must arrive within Client.Timeout; after that only ctx bounds the
    stream. Reconnecting (with jitter) and re-snapshotting are the caller's job.
    Malformed lines are logged and skipped.

func (c *Client) TabCreate(ctx context.Context, o TabCreateOptions) (TabCreated, error)
    TabCreate opens a tab with a fresh shell pane.

func (c *Client) TerminalTitleClear(ctx context.Context) (WindowTitleResult, error)
    TerminalTitleClear restores the client's terminal window title.

func (c *Client) TerminalTitleSet(ctx context.Context, title string) (WindowTitleResult, error)
    TerminalTitleSet sets the attached client's terminal window title (the CLI's
    `terminal title set`; used as a focus marker for Ghostty).

func (c *Client) WaitIdleShell(ctx context.Context, paneID string, timeout time.Duration) (ProcessInfo, error)
    WaitIdleShell polls pane.process_info until the pane is an idle shell
    or timeout elapses (ErrTimeout naming the foreground processes).
    The timeout bounds the whole operation, including every poll: a slow or late
    pane.process_info answer cannot outlast it. A non-positive timeout expires
    at once. It returns the last process info seen.

func (c *Client) WorkspaceClose(ctx context.Context, workspaceID string) error
    WorkspaceClose closes a workspace and every pane in it.

func (c *Client) WorkspaceCreate(ctx context.Context, o WorkspaceCreateOptions) (WorkspaceCreated, error)
    WorkspaceCreate opens a workspace; its root pane runs a fresh shell.

func (c *Client) WorkspaceRename(ctx context.Context, workspaceID, label string) error
    WorkspaceRename sets a workspace label.

func (c *Client) WorkspaceReportMetadata(ctx context.Context, workspaceID, source string, tokens map[string]string, ttl time.Duration) error
    WorkspaceReportMetadata publishes display-only tokens (sidebar/tab-bar) for
    a workspace under source. ttl zero means no expiry.

func (c *Client) WorktreeList(ctx context.Context, cwd string) (WorktreeListing, error)
    WorktreeList lists the git worktrees of the repository at cwd ("" lets herdr
    pick the focused workspace's repository).

type Error struct {
	Method  string `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
    Error is an error response from the herdr server.

func (e *Error) Error() string

type Event struct {
	Event       string // kind, e.g. EventPaneAgentStatusChanged or "pane_exited"
	PaneID      string
	WorkspaceID string
	AgentStatus Status // EventPaneAgentStatusChanged only
	Agent       string
	Data        json.RawMessage
}
    Event is one streamed event. The common fields are decoded from Data when
    present; Data keeps the full payload.

type NotificationResult struct {
	Shown  bool   `json:"shown"`
	Reason string `json:"reason"`
}
    NotificationResult is the notification.show reply. Reason is one of shown,
    disabled, rate_limited, no_foreground_client, busy.

type OutputMatch struct {
	PaneID      string     `json:"pane_id"`
	Revision    uint64     `json:"revision"`
	MatchedLine string     `json:"matched_line"`
	Read        ReadResult `json:"read"`
}
    OutputMatch is the pane.wait_for_output reply.

type Pane struct {
	ID                    string            `json:"pane_id"`
	TerminalID            string            `json:"terminal_id"`
	WorkspaceID           string            `json:"workspace_id"`
	TabID                 string            `json:"tab_id"`
	Focused               bool              `json:"focused"`
	Label                 string            `json:"label"`
	Agent                 string            `json:"agent"` // "" when no agent detected
	DisplayAgent          string            `json:"display_agent"`
	AgentStatus           Status            `json:"agent_status"`
	AgentSession          *AgentSession     `json:"agent_session"`
	Cwd                   string            `json:"cwd"`
	ForegroundCwd         string            `json:"foreground_cwd"`
	Title                 string            `json:"title"`
	TerminalTitle         string            `json:"terminal_title"`
	TerminalTitleStripped string            `json:"terminal_title_stripped"`
	RestoreError          string            `json:"restore_error"`
	Revision              uint64            `json:"revision"`
	Tokens                map[string]string `json:"tokens,omitempty"`
}
    Pane is a herdr pane (PaneInfo). Nullable strings decode to "".

type PluginPane struct {
	PluginID   string `json:"plugin_id"`
	Entrypoint string `json:"entrypoint"`
	Pane       Pane   `json:"pane"`
}
    PluginPane is the plugin.pane.open reply.

type PluginPaneOptions struct {
	PluginID     string
	Entrypoint   string
	Placement    string // PlacementPopup, …; "" = manifest default
	Width        string // popup size: cells ("30") or percent ("80%")
	Height       string
	WorkspaceID  string
	TargetPaneID string
	Direction    string // for split placement
	Cwd          string
	Env          map[string]string
	Focus        bool
}
    PluginPaneOptions configures PluginPaneOpen.

type Process struct {
	PID     int      `json:"pid"`
	Name    string   `json:"name"`
	Argv    []string `json:"argv"`
	Argv0   string   `json:"argv0"`
	Cmdline string   `json:"cmdline"`
	Cwd     string   `json:"cwd"`
}
    Process is one foreground process of a pane.

type ProcessInfo struct {
	PaneID         string    `json:"pane_id"`
	ShellPID       int       `json:"shell_pid"`
	ForegroundPGID int       `json:"foreground_process_group_id"`
	TTY            string    `json:"tty"`
	Foreground     []Process `json:"foreground_processes"`
}
    ProcessInfo describes a pane's shell and foreground process group.

type PromptWait struct {
	Until   []Status
	Timeout time.Duration
}
    PromptWait makes AgentPrompt wait server-side for the first matching status
    after submission. Empty Until matches idle, done or blocked; Timeout zero
    waits until ctx is done.

type ReadOptions struct {
	Source string
	Lines  int
	Format string
}
    ReadOptions configures PaneRead and AgentRead. Zero values mean Source
    recent_unwrapped, Format text, server-default line count.

type ReadResult struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	Source      string `json:"source"`
	Format      string `json:"format"`
	Text        string `json:"text"`
	Revision    uint64 `json:"revision"`
	Truncated   bool   `json:"truncated"`
}
    ReadResult is pane/agent output (pane.read, agent.read).

type ServerInfo struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}
    ServerInfo is the ping reply.

type Snapshot struct {
	Version            string      `json:"version"`
	Protocol           int         `json:"protocol"`
	Workspaces         []Workspace `json:"workspaces"`
	Tabs               []Tab       `json:"tabs"`
	Panes              []Pane      `json:"panes"`
	Agents             []AgentInfo `json:"agents"`
	FocusedWorkspaceID string      `json:"focused_workspace_id"`
	FocusedTabID       string      `json:"focused_tab_id"`
	FocusedPaneID      string      `json:"focused_pane_id"`
}
    Snapshot is the whole live session (session.snapshot). Layouts are omitted.

func (s Snapshot) AgentByName(name string) (AgentInfo, bool)
    AgentByName returns the agent started under name.

func (s Snapshot) PaneBySession(value string) (Pane, bool)
    PaneBySession returns the pane whose agent_session.value equals value (used
    to rebind sessions herdr restored on its own).

type SplitOptions struct {
	Direction string // SplitRight or SplitDown
	Cwd       string
	Env       map[string]string
	Focus     bool
}
    SplitOptions configures PaneSplit.

type Status string
    Status is an agent status as reported by herdr.

const (
	StatusIdle    Status = "idle"
	StatusWorking Status = "working"
	StatusBlocked Status = "blocked"
	StatusDone    Status = "done"
	StatusUnknown Status = "unknown"
)
    Agent statuses.

type Subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
}
    Subscription selects one event type. herdr rejects pane-scoped types
    (pane.agent_status_changed, pane.output_matched, pane.scroll_changed)
    without PaneID.

type Tab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	AgentStatus Status `json:"agent_status"`
}
    Tab is a herdr tab (TabInfo).

type TabCreateOptions struct {
	WorkspaceID string // "" = focused workspace
	Cwd         string
	Label       string
	Env         map[string]string
	Focus       bool
}
    TabCreateOptions configures TabCreate.

type TabCreated struct {
	Tab      Tab  `json:"tab"`
	RootPane Pane `json:"root_pane"`
}
    TabCreated is the tab.create reply.

type WaitOutputOptions struct {
	Match   string // literal substring, or a Rust regex when Regex is set
	Regex   bool
	Source  string        // "" = recent (the CLI default)
	Lines   int           // 0 = server default
	Timeout time.Duration // 0 = wait until ctx is done
}
    WaitOutputOptions configures PaneWaitOutput.

type WindowTitleResult struct {
	Changed bool   `json:"changed"`
	Reason  string `json:"reason"`
}
    WindowTitleResult is the client.window_title.* reply. Reason is one of set,
    cleared, no_foreground_client.

type Workspace struct {
	ID          string             `json:"workspace_id"`
	Number      int                `json:"number"`
	Label       string             `json:"label"`
	Focused     bool               `json:"focused"`
	PaneCount   int                `json:"pane_count"`
	TabCount    int                `json:"tab_count"`
	ActiveTabID string             `json:"active_tab_id"`
	AgentStatus Status             `json:"agent_status"`
	Tokens      map[string]string  `json:"tokens,omitempty"`
	Worktree    *WorkspaceWorktree `json:"worktree,omitempty"`
}
    Workspace is a herdr workspace (WorkspaceInfo).

type WorkspaceCreateOptions struct {
	Cwd   string
	Label string
	Env   map[string]string // pane environment for the root pane's shell
	Focus bool
}
    WorkspaceCreateOptions configures WorkspaceCreate.

type WorkspaceCreated struct {
	Workspace Workspace `json:"workspace"`
	Tab       Tab       `json:"tab"`
	RootPane  Pane      `json:"root_pane"`
}
    WorkspaceCreated is the workspace.create reply.

type WorkspaceWorktree struct {
	RepoKey          string `json:"repo_key"`
	RepoName         string `json:"repo_name"`
	RepoRoot         string `json:"repo_root"`
	CheckoutPath     string `json:"checkout_path"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
}
    WorkspaceWorktree describes the git checkout a worktree workspace shows.

type Worktree struct {
	Path             string `json:"path"`
	Label            string `json:"label"`
	Branch           string `json:"branch"` // "" when detached
	IsBare           bool   `json:"is_bare"`
	IsDetached       bool   `json:"is_detached"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
	IsPrunable       bool   `json:"is_prunable"`
	OpenWorkspaceID  string `json:"open_workspace_id"`
}
    Worktree is one git worktree known to herdr.

type WorktreeListing struct {
	Source    WorktreeSource `json:"source"`
	Worktrees []Worktree     `json:"worktrees"`
}
    WorktreeListing is the worktree.list reply.

type WorktreeSource struct {
	RepoKey            string `json:"repo_key"`
	RepoName           string `json:"repo_name"`
	RepoRoot           string `json:"repo_root"`
	SourceCheckoutPath string `json:"source_checkout_path"`
	SourceWorkspaceID  string `json:"source_workspace_id"`
}
    WorktreeSource is the repository a worktree.list call resolved.

```

## identity

```text
package identity // import "github.com/zhuravel/magnum/internal/identity"

Package identity provides the GitHub identities magnum acts as.

Two kinds exist, one per [identity] entry in config.toml:

  - gh: the user's own gh login. The token lives in the macOS keyring and is
    never read by magnum, only checked (`gh auth token --hostname github.com`).
    Panes and subprocesses need no extra environment.
  - app: a GitHub App installation. magnum signs an RS256 JWT with the App's
    private key (stdlib only), mints a one-hour installation token, refreshes it
    single-flight in the background when less than 15 minutes remain (a failed
    mint is not retried for 30 seconds), and keeps a private gh config dir
    (state/gh/<name>/hosts.yml + config.yml) in sync so panes and subprocesses
    run gh as the App via GH_CONFIG_DIR. App.Reauth drops the token and rewrites
    that dir after a 401. Its own REST calls run as `gh api` through GhTransport
    unless [github] transport = "direct".

Both kinds implement Source, including a health Check that prints PASS/FAIL
lines with the exact fix for anything missing. Tokens are never logged or put
into reports.

CONSTANTS

const DefaultBaseURL = "https://api.github.com"
    DefaultBaseURL is the GitHub REST API root.


VARIABLES

var ErrPlaceholderKey = errors.New("identity: placeholder private key")
    ErrPlaceholderKey matches (errors.Is) the error for a key variable that
    still holds the placeholder sentence the tracked .mise.toml sets, instead of
    the PEM text or key file path from .mise.local.toml.


FUNCTIONS

func NewGhClient(run execx.Runner, env map[string]string) *http.Client
    NewGhClient returns an http.Client whose requests run through gh (see
    GhTransport), with the same 30 s timeout the direct client uses. A request
    without a deadline of its own gets the same 30 s as the gh call's bound.

func WatchedRepos(c *config.Config, name string) []string
    WatchedRepos returns the literal "owner/name" repositories that watches
    posting as identity name include (glob patterns cannot be enumerated and are
    skipped), in config order without duplicates. App checks probe these.


TYPES

type App struct {
	// Has unexported fields.
}
    App is a GitHub App installation identity (kind = "app").

func NewApp(cfg config.Identity, layout paths.Layout, client *http.Client, getenv func(string) string, now func() time.Time, opts ...AppOption) *App
    NewApp returns the App identity described by cfg. The PEM comes from the
    env var cfg.PrivateKeyEnv read through getenv (PEM text or a file path). Nil
    client, getenv or now default to a 30 s http.Client, os.Getenv and time.Now.

func (a *App) Check(ctx context.Context) (Report, error)
    Check verifies, in order: the private key, the JWT (GET /app), the
    installation and its permissions (pull_requests must be write; contents
    is reported), minting a token, writing ConfigDir, the installation's
    repositories (every WithRepos repo must be listed), and that `gh api
    repos/<first repo>` works with GH_CONFIG_DIR. It keeps going after a failure
    where the next step can still run, so one pass shows every problem.

    Check proves minting works by minting a token itself; a failed mint leaves
    the daemon's cached token alone, a successful one replaces it only when it
    outlives it.

func (a *App) ConfigDir() string
    ConfigDir is the identity's private GH_CONFIG_DIR.

func (a *App) EnsureConfigDir(ctx context.Context) (string, error)
    EnsureConfigDir makes sure a fresh token exists and that ConfigDir holds
    it (hosts.yml + config.yml, 0600 files in a 0700 dir, written atomically).
    It rewrites the files whenever the token changed or a file is missing;
    call it on every daemon tick so long-lived panes never see an expired token.

func (a *App) Env(ctx context.Context) (map[string]string, error)
    Env implements Source: a fresh token written to ConfigDir, selected via
    GH_CONFIG_DIR.

func (a *App) Expiry() time.Time
    Expiry reports when the current installation token expires (zero if none).
    Safe to persist; the token itself never is.

func (a *App) Invalidate()
    Invalidate forgets the cached token's validity so the next Token call mints
    a new one, even right after a failed mint. Reauth is the 401 entry point.

func (a *App) Kind() string
    Kind implements Source.

func (a *App) Login() string
    Login implements Source.

func (a *App) Name() string
    Name implements Source.

func (a *App) Reauth(ctx context.Context) error
    Reauth discards the cached installation token (GitHub answered 401:
    it was revoked, or the installation was suspended and restored), mints a
    fresh one and rewrites ConfigDir so panes and gh subprocesses pick it up.
    Its signature matches github.Client.Reauth. A token minted less than 10
    seconds ago is kept, so parallel 401s share one mint.

func (a *App) Token(ctx context.Context) (string, error)
    Token returns a usable installation token, minting one when needed.
    Concurrent callers share a single mint. A token with at least 15 minutes
    left is returned at once; with 2 to 15 minutes left it is returned at once
    too and a refresh runs in the background (every new token is written to
    ConfigDir); with less, or none, callers wait for the mint, giving up when
    ctx ends. After a failed mint no new one starts for 30 seconds.

type AppOption func(*App)
    AppOption customizes an App.

func WithBaseURL(u string) AppOption
    WithBaseURL points the REST calls at another API root (tests).

func WithRepos(fullNames ...string) AppOption
    WithRepos sets the "owner/name" repositories Check expects in the
    installation (see WatchedRepos); the first one is probed with gh.

func WithRunner(run execx.Runner) AppOption
    WithRunner sets the runner Check uses for its `gh api repos/<repo>` probe.

type GH struct {
	// Has unexported fields.
}
    GH is the user's own gh login (kind = "gh"). magnum never reads or caches
    its token: gh itself picks it up from the keyring.

func NewGH(cfg config.Identity, run execx.Runner) *GH
    NewGH returns the gh identity described by cfg; every gh call goes through
    run.

func (g *GH) Check(ctx context.Context) (Report, error)
    Check verifies that gh holds a token for github.com and that `gh api user`
    is the configured login.

func (g *GH) Env(context.Context) (map[string]string, error)
    Env implements Source: gh already uses the user's keyring login; only
    GH_HOST is pinned to github.com (see pinnedHost).

func (g *GH) Kind() string
    Kind implements Source.

func (g *GH) Login() string
    Login implements Source.

func (g *GH) Name() string
    Name implements Source.

type GhTransport struct {
	// Run executes gh. Requests are never marked Mutates: minting a token
	// changes nothing a dry run must protect, exactly like the direct path.
	Run execx.Runner
	// Env overlays the gh environment (after the transport's own NO_COLOR,
	// CLICOLOR_FORCE and GH_FORCE_TTY, which keep the output parseable).
	Env map[string]string
}
    GhTransport is an http.RoundTripper that sends each request through `gh api
    --include` instead of opening a TLS connection itself.

    Some Macs run an outbound firewall (Little Snitch) that blocks HTTPS from
    unknown, unsigned binaries such as a freshly rebuilt magnum, while the
    signed gh binary is allowed; every other GitHub call magnum makes already
    goes through gh. See docs/spikes.md (2026-10-03, gh transport).

    The request's own headers are passed as `-H`. gh only adds its stored token
    when the request has no Authorization header, so an App JWT passed that
    way wins. An installation token (ghs_...) never touches argv: it travels
    in the GH_TOKEN variable of that one gh process, which gh sends as the
    Authorization header and other local users cannot read. gh still refuses
    to start without some login of its own (`gh auth login` or GH_TOKEN),
    so for JWT calls the user's normal gh auth must stay available; Env must not
    blank it.

    Non-2xx answers become ordinary responses: gh exits 1 on an HTTP error but
    still prints the status line, headers and body, so stdout is parsed whatever
    the exit code. Only api.github.com URLs are supported.

    The JWT travels in argv, so other local users could see it in `ps` for the
    second the call lasts. execx.Real redacts it (Authorization: <redacted>) in
    its log lines and errors from this transport never include argv.

func (t *GhTransport) RoundTrip(req *http.Request) (*http.Response, error)
    RoundTrip implements http.RoundTripper.

type Report struct {
	Pass  bool
	Lines []string
}
    Report is the outcome of a health check. Lines start with "PASS ", "FAIL ",
    "WARN " (something to fix that does not fail the check), "INFO ", or " fix:
    " (the exact remedy for the FAIL or WARN line above it).

func (r Report) String() string
    String joins the lines for printing.

type Source interface {
	// Name is the identity's name from config.toml.
	Name() string
	// Login is the GitHub login reviews are posted as (REST form, e.g. "talkable[bot]").
	Login() string
	// Kind is "gh" or "app".
	Kind() string
	// Env returns the environment overlay that makes gh act as this identity
	// in a pane or subprocess: GH_CONFIG_DIR for apps (with GH_TOKEN and
	// GITHUB_TOKEN blanked so an inherited token cannot take precedence),
	// and GH_HOST=github.com for both kinds so an inherited GH_HOST cannot
	// redirect gh. For apps it first makes sure the token is fresh and the
	// config dir is written.
	Env(ctx context.Context) (map[string]string, error)
	// Check runs the identity's health checks. Problems GitHub or gh report
	// become FAIL lines; the error is non-nil only when the check could not
	// run to completion (network failure, cancelled context).
	Check(ctx context.Context) (Report, error)
}
    Source is one GitHub identity magnum reads or writes as.

```

## inventory

```text
package inventory // import "github.com/zhuravel/magnum/internal/inventory"

Package inventory is the read-mostly reconciliation view: the registry (slots,
assignments, sessions) compared with what is really on disk (`git worktree
list` of every watched main clone), in MySQL (per-worktree databases), in herdr
(agents per path) and, on request, on GitHub (states of PRs checked out in
manual worktrees). Scan never changes anything; UpsertSlotDatabases is the only
write and touches slot_databases only.

Sources other than the store degrade: an unreachable MySQL, herdr socket,
main clone or GitHub becomes a line in Inventory.Warnings and the facts it would
have supplied stay empty (DatabasesListed, AgentsListed and ClonesListed say
which are unknown rather than empty). Dropped-database detection runs only when
MySQL answered (Inventory.DatabasesListed); orphan detection only on complete
ownership facts (Inventory.OrphansKnown: databases and every clone listed,
every slot directory and slug marker read conclusively), since cleanup drops
what it reports.

CONSTANTS

const (
	KindLostSlot       = "lost_slot"             // registry slot whose directory is missing
	KindUnknownSlotDir = "unknown_slot_dir"      // directory named like a slot that the registry does not own
	KindOrphanDB       = "orphan_db"             // suffixed databases whose slug no slot or worktree claims
	KindForeignAgent   = "foreign_agent"         // non-magnum herdr agent working inside a managed slot
	KindHeadDrift      = "head_drift"            // claimed/busy/held slot whose HEAD is not checked_out_sha
	KindMissingPR      = "assignment_missing_pr" // open assignment whose PR is gone from the registry or GitHub
	KindExternalClosed = "external_pr_closed"    // manual worktree whose PR is MERGED or CLOSED
)
    Finding kinds.

const (
	PRSourceGitConfig  = "git_config"  // git config branch.<b>.pr (authoritative)
	PRSourceBranchName = "branch_name" // /PR-(\d+)/ in the branch name: a guess, often a Jira key
)
    PRSource values: where ExternalView.PRNumber came from.

const (
	SlugSourceFile     = "file"     // <path>/tmp/.worktree-db-slug
	SlugSourceInferred = "inferred" // directory or branch name that matches existing databases
)
    SlugSource values: where ExternalView.Slug came from.

const (
	StateSourceStore  = "store"
	StateSourceGitHub = "github"
)
    StateSource values: where ExternalView.GHState came from.

const (
	// SlugFile is the worktree's database slug marker (slots.MarkerFile),
	// written by the Talkable repo's bin/worktree-setup.
	SlugFile = slots.MarkerFile
	// AgentPrefix starts every herdr agent name magnum gives (slots.AgentPrefix).
	AgentPrefix = slots.AgentPrefix
	// DroppedBy is slot_databases.dropped_by for databases that disappeared from MySQL.
	DroppedBy = "reconcile"
)

VARIABLES

var ErrNotListed = errors.New("inventory: mysql databases were not listed")
    ErrNotListed is returned by UpsertSlotDatabases when the scan could not list
    MySQL: recording would mark every database dropped.


FUNCTIONS

func DiskUsageKB(ctx context.Context, r execx.Runner, path, label string) (int64, error)
    DiskUsageKB is the size of path in KiB from `du -sk` (label names the
    command in logs). du exits non-zero on unreadable subdirectories but still
    prints the total, which is used.

func NaturalLess(a, b string) bool
    NaturalLess orders strings with embedded numbers numerically (review2 <
    review10), for sorting slots and paths.


TYPES

type AgentView struct {
	Name        string       `json:"name,omitempty"`
	Agent       string       `json:"agent"` // codex, claude, ...
	Status      herdr.Status `json:"status"`
	PaneID      string       `json:"pane_id"`
	WorkspaceID string       `json:"workspace_id"`
	Cwd         string       `json:"cwd"`
	SessionID   string       `json:"session_id,omitempty"` // agent_session.value
	Magnum      bool         `json:"magnum"`               // a live magnum session or an mg- name
}
    AgentView is a herdr agent whose pane or foreground cwd is inside a path.

type DBView struct {
	Name     string  `json:"name"`
	SizeMB   float64 `json:"size_mb"`
	Present  bool    `json:"present"`
	Expected bool    `json:"expected"` // rendered from the pool's database templates
}
    DBView is one database of a slot or worktree. Present is meaningful only
    when Inventory.DatabasesListed.

type ExternalView struct {
	Path        string      `json:"path"`
	MainClone   string      `json:"main_clone"`
	Repo        string      `json:"repo"` // owner/name
	Exists      bool        `json:"exists"`
	Head        string      `json:"head,omitempty"`
	Branch      string      `json:"branch,omitempty"`
	Detached    bool        `json:"detached"`
	Locked      bool        `json:"locked"`
	Prunable    bool        `json:"prunable"`
	PRNumber    int         `json:"pr_number,omitempty"`
	PRSource    string      `json:"pr_source,omitempty"`
	PRConfirmed bool        `json:"pr_confirmed"` // git config, or a branch-name guess whose PR head equals Head
	GHState     string      `json:"gh_state,omitempty"`
	StateSource string      `json:"state_source,omitempty"`
	MergedAt    *time.Time  `json:"merged_at,omitempty"`
	ClosedAt    *time.Time  `json:"closed_at,omitempty"`
	Slug        string      `json:"slug,omitempty"`
	SlugSource  string      `json:"slug_source,omitempty"`
	Databases   []DBView    `json:"databases"`
	Agents      []AgentView `json:"agents"`
	SizeKB      *int64      `json:"size_kb,omitempty"`
	Drift       []string    `json:"drift,omitempty"`
}
    ExternalView is a worktree of a watched main clone that magnum does not
    manage (talkable.repoN, talkable__worktrees/*); the main checkout is
    excluded.

type Finding struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"` // slot:<name>, path:<dir>, slug:<slug>
	Message string `json:"message"`
	Safe    bool   `json:"safe"`
}
    Finding is one drift item. Safe means magnum may fix it automatically (only
    magnum-owned resources); everything else is reported for a human.

type Git interface {
	WorktreeList(ctx context.Context, mainClone string) ([]gitx.Worktree, error)
	Status(ctx context.Context, dir string) (gitx.Status, error)
	BranchPR(ctx context.Context, dir, branch string) (number int, ok bool, err error)
	FindClone(ctx context.Context, cloneRoot, owner, name string) (string, error)
}
    Git is the subset of *gitx.Client the scanner reads.

type GitHub interface {
	ConfirmStates(ctx context.Context, owner, repo string, numbers []int) (map[int]github.PRState, []int, error)
}
    GitHub is the subset of *github.Client the scanner reads.

type Herdr interface {
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
}
    Herdr is the subset of *herdr.Client the scanner reads.

type Inventory struct {
	ScannedAt       time.Time         `json:"scanned_at"`
	Slots           []SlotView        `json:"slots"`
	External        []ExternalView    `json:"external,omitempty"`
	Databases       []mysqlx.Database `json:"databases"`        // schemas named <prefix><slug> by the pools' database templates
	DatabasesListed bool              `json:"databases_listed"` // false: database facts and orphans are unknown
	// AgentsListed: the herdr snapshot was read; false means agent facts
	// (SlotView.Agents, ExternalView.Agents) are unknown, not empty.
	AgentsListed bool `json:"agents_listed"`
	// ClonesListed: `git worktree list` (and clone discovery) succeeded for
	// every watched main clone; false means some worktrees are unknown.
	ClonesListed bool `json:"clones_listed"`
	// OrphansKnown: orphan detection ran on complete facts (DatabasesListed,
	// ClonesListed and every slot directory and slug marker read
	// conclusively). False: OrphanDBs is empty because ownership is unknown.
	OrphansKnown bool              `json:"orphans_known"`
	OrphanDBs    []mysqlx.Database `json:"orphan_dbs"`
	Drift        []Finding         `json:"drift"`
	Warnings     []string          `json:"warnings,omitempty"` // sources that could not be read (redacted)
}
    Inventory is one reconciliation snapshot. Slices are sorted (slots by
    natural name, external worktrees by natural path, databases by name,
    findings by kind then subject) and nil-safe for JSON.

type MySQL interface {
	ListSuffixed(ctx context.Context) ([]mysqlx.Database, error)
}
    MySQL is the subset of *mysqlx.Client the scanner reads.

type Options struct {
	Sizes        bool // `du -sk` every existing slot (and external worktree when External)
	External     bool // list worktrees of watched main clones that are not managed slots
	GitHubStates bool // with External: one ConfirmStates call per repo for their PRs
}
    Options selects the expensive parts of a scan.

type Scanner struct {
	Store  *store.Store
	Git    Git
	MySQL  MySQL          // per-worktree databases; nil: no database facts
	Herdr  Herdr          // agents per path; nil: no agent facts
	GitHub GitHub         // Options.GitHubStates; nil: states come from the store only
	Runner execx.Runner   // `du -sk` for Options.Sizes; nil: no sizes
	Config *config.Config // pools (slot path/name templates, DB templates) and watches (clone roots)
	Now    func() time.Time
}
    Scanner builds an Inventory. Store and Git are required; every other
    dependency is optional and its facts are skipped (with a warning) when nil.

func (s *Scanner) Scan(ctx context.Context, opts Options) (Inventory, error)
    Scan builds the inventory. Only a store failure (or a cancelled context) is
    an error; other sources degrade into Inventory.Warnings.

func (s *Scanner) UpsertSlotDatabases(ctx context.Context, inv Inventory) (SyncResult, error)
    UpsertSlotDatabases records inv's MySQL listing in slot_databases: every
    listed database is upserted with its size and, when it belongs to a managed
    slot, that slot id (other rows keep their stored slot_id); rows that are
    no longer listed are marked dropped. It is the inventory's only write and
    refuses (ErrNotListed) when the scan could not list MySQL.

type SlotView struct {
	Slot       store.Slot        `json:"slot"`
	Exists     bool              `json:"exists"`   // directory present
	Worktree   bool              `json:"worktree"` // listed by `git worktree list` of its main clone
	Head       string            `json:"head,omitempty"`
	Branch     string            `json:"branch,omitempty"` // "" when detached
	Detached   bool              `json:"detached"`
	Dirty      *bool             `json:"dirty,omitempty"` // nil when unknown
	Tracked    int               `json:"tracked"`
	Untracked  int               `json:"untracked"`
	PR         *store.PR         `json:"pr,omitempty"` // slot.pr_id, else the open assignment's PR
	Assignment *store.Assignment `json:"assignment,omitempty"`
	Slug       string            `json:"slug"` // slots.db_slug, else the sanitized slot name
	Databases  []DBView          `json:"databases"`
	Agents     []AgentView       `json:"agents"`
	SizeKB     *int64            `json:"size_kb,omitempty"`
	Drift      []string          `json:"drift,omitempty"` // finding kinds about this slot
}
    SlotView is a managed (pool or per-PR, not removed) registry slot with its
    live facts.

type SyncResult struct {
	Seen    int      `json:"seen"`    // databases upserted (last_seen_at = now)
	Dropped []string `json:"dropped"` // rows newly marked dropped (dropped_by = DroppedBy)
}
    SyncResult reports what UpsertSlotDatabases recorded.

```

## launchd

```text
package launchd // import "github.com/zhuravel/magnum/internal/launchd"

Package launchd writes and manages magnum's LaunchAgent (label zhuravel.magnum):
rendering the plist, installing it into the user's GUI domain, and querying,
restarting and removing the job.

All launchctl calls go through execx.Runner. The one external file this package
owns is ~/Library/LaunchAgents/<label>.plist; everything else magnum keeps lives
in the repository.

The plist is world-readable (0644), so never put secrets in Options.Env. The
daemon gets its credentials from mise (`mise -C <repo> exec -- ...`) instead.

CONSTANTS

const DefaultLabel = "zhuravel.magnum"
    DefaultLabel is the launchd label of the magnum daemon.

const ExitTimeOut = 45
    ExitTimeOut is the ExitTimeOut value (seconds) every plist carries: how long
    launchd waits after SIGTERM before SIGKILLing the job. launchd's own default
    is 5 s, far shorter than the daemon needs to cancel its rounds and kill
    the subprocesses it started (the CLI budgets 30 s for a SIGTERMed daemon),
    so a plain stop or restart would otherwise SIGKILL it mid-cleanup.

const LogName = "launchd.log"
    LogName is the file (in state/logs) that receives the job's stdout and
    stderr: launchd's own capture, kept apart from the daemon.log the daemon
    writes and rotates itself (two writers on one file would interleave and
    fight over rotation). Only crashes and pre-logger output end up here.


FUNCTIONS

func AgentPath(home, label string) string
    AgentPath returns the per-user LaunchAgent plist path for a label.

func Bootout(ctx context.Context, run execx.Runner, uid int, label string) error
    Bootout unloads the job (`launchctl bootout`) but keeps its plist,
    so `launchctl bootstrap` or `magnum install` can load it again. A bootout
    error is ignored only when Status confirms the job is not loaded; if Status
    itself fails, the job's state is unknown and the bootout error is returned.

func Install(ctx context.Context, run execx.Runner, uid int, path string, plist []byte) error
    Install (re)loads the job described by plist into gui/<uid>.

    It writes plist to path (0644, atomically), then runs

        launchctl bootout   gui/<uid>/<label>   (errors ignored: not loaded yet)
        launchctl print     gui/<uid>/<label>   (polled until the job is gone)
        launchctl enable    gui/<uid>/<label>   (best effort: undoes an old disable)
        launchctl bootstrap gui/<uid> <path>

    bootout does not wait for the old job to finish tearing down, and
    bootstrapping into that window fails with "Input/output error". Install
    therefore polls Status once a second until launchd no longer knows the job
    (at most 30 s; a job that is still loaded then is reported as such), and
    only then bootstraps. The transient error can still show up, so bootstrap is
    retried up to three times, one second apart, on that error only; any other
    failure is returned immediately. When the retries run out, the error blames
    a missing gui/<uid> domain (no console login session) only if `launchctl
    print gui/<uid>` itself fails. The label is read from the plist itself.

func Kickstart(ctx context.Context, run execx.Runner, uid int, label string) error
    Kickstart restarts the job now (`launchctl kickstart -k`), killing the
    running instance first if there is one.

func LogPath(logsDir string) string
    LogPath is LogName inside a logs directory (paths.Layout.Logs()).

func Plist(opts Options) []byte
    Plist renders opts as an XML property list. The output is deterministic:
    keys appear in a fixed order and Env is sorted, so unchanged options produce
    byte-identical files. RunAtLoad is always true and ExitTimeOut is always
    ExitTimeOut seconds.

func Uninstall(ctx context.Context, run execx.Runner, uid int, label, path string) error
    Uninstall unloads the job and deletes its plist at path. It is idempotent:
    a job that is not loaded and a missing file are not errors. If bootout fails
    while the job is still loaded, the error is returned and the file is kept so
    the daemon is not orphaned without its definition.


TYPES

type Info struct {
	State State
	// PID is the running process, 0 when none.
	PID int
	// Runs is how many times launchd has started the job.
	Runs int
	// LastExitCode is meaningful only when Exited is true; launchd prints
	// "(never exited)" or a non-numeric exit reason otherwise.
	LastExitCode int
	Exited       bool
}
    Info is the parsed result of `launchctl print gui/<uid>/<label>`.
    It deliberately carries no raw output: `print` echoes the job's arguments
    and environment.

func Status(ctx context.Context, run execx.Runner, uid int, label string) (Info, error)
    Status runs `launchctl print gui/<uid>/<label>`. Only launchd's "no
    such service" answer (exit 113, or "Could not find service" on stderr)
    yields State NotLoaded and a nil error. Every other failure (a domain or
    permission error, a missing binary, a timeout) is returned as an error,
    alongside NotLoaded as the zero Info: the job may well still be loaded,
    so callers such as Bootout and Uninstall must not read it as "gone".

func (s Info) Loaded() bool
    Loaded reports whether launchd knows the job.

type Options struct {
	// Label is the job label; required. By convention it is also the plist's
	// file name stem (see AgentPath).
	Label string
	// ProgramArguments is the full argv, including argv[0]; required.
	ProgramArguments []string
	// WorkingDir sets WorkingDirectory when non-empty.
	WorkingDir string
	// Env becomes EnvironmentVariables when non-empty, rendered in sorted key order.
	Env map[string]string
	// StdoutPath and StderrPath set StandardOutPath / StandardErrorPath when
	// non-empty. launchd creates the file but not its parent directory.
	StdoutPath, StderrPath string
	// KeepAlive restarts the job when it exits abnormally. It renders
	// KeepAlive = { SuccessfulExit = false }: a clean exit (the daemon exits 0
	// on SIGTERM and when another instance already holds the lock) is not
	// restarted, so launchd does not spin. False omits the key.
	KeepAlive bool
	// ThrottleSeconds sets ThrottleInterval (minimum seconds between launches)
	// when greater than zero; launchd's own default is 10.
	ThrottleSeconds int
}
    Options describes the LaunchAgent job.

type State string
    State is launchd's own description of a loaded job, or NotLoaded.

const (
	// NotLoaded means launchd does not know the job (`launchctl print` exited
	// 113 or said "Could not find service"). Status also returns it next to
	// an error, as the zero answer when the state could not be read.
	NotLoaded State = "not loaded"
	// Running means the job has a live process.
	Running State = "running"
	// Unknown means the job is loaded but `print` output had no state line.
	Unknown State = "unknown"
)
```

## learn

```text
package learn // import "github.com/zhuravel/magnum/internal/learn"

Package learn is the deterministic half of the retro (DECISIONS "Learning loop:
daily retro"): after a pull request magnum reviewed closes, Build turns what
other reviewers said about it into candidates for a classifier, after dropping
what cannot be a miss (the author's and magnum's own comments, short approvals,
comments on code magnum never saw, findings magnum already posted), and
ParseOutput reads the classifier's answer back, refusing anything off schema and
any lesson that retells the pull request instead of teaching (ScrubLesson).

The package is pure: it starts no process and touches no network or registry.
The engine reads GitHub and the registry and hands the data in; the one
comparison Build may need is a function of Input.

CONSTANTS

const (
	KindThread = store.MissSourceThread
	KindReview = store.MissSourceReview
)
    Candidate kinds (store.MissSourceThread, store.MissSourceReview).

const (
	CandidatesFile = "candidates.json" // what Build kept, for the classifier
	OutputFile     = "retro.json"      // the classifier's answer (ParseOutput)
	FilesDir       = "files"           // the commented files: files/<sha12>/<path>
)
    Files of a pull request's retro directory.

const (
	TitleMax   = 80  // runes
	MatchMax   = 3   // patterns per miss
	PatternMax = 120 // bytes per pattern
)
    Limits of the classifier's answer.

const (
	LessonIssueRef  = "issue_ref"  // a pull request or issue number (#123)
	LessonURL       = "url"        // a link
	LessonLogin     = "login"      // a login of the pull request, with or without "@"
	LessonNamesRepo = "names_repo" // the repository's owner or name, in a general lesson
)
    Why ScrubLesson dropped a lesson (the reason of a lesson_rejected event).

const NearLines = 3
    NearLines is how far, in lines, a comment may be from one of magnum's
    findings on the same path and still be about it.


FUNCTIONS

func FilePath(sha, p string) string
    FilePath is where the copy of path at sha lives under a retro directory's
    files: files/<sha12>/<path>, slash-separated.

func ParseOutput(data []byte, cands []Candidate) (map[string]Item, error)
    ParseOutput reads the classifier's answer for cands: one item per candidate,
    no other id, a known class; a miss also has a severity (P0 to P3), a title
    of at most TitleMax runes, a lesson, a scope (repo or general), lines [from,
    to] with 1 <= from <= to and one to MatchMax patterns of at most PatternMax
    bytes that compile as case-insensitive Go regular expressions. Only a miss
    keeps those fields: the other classes keep their id and class. The error
    names ids and fields, never the classifier's text, so it can go back into a
    prompt.

func ScrubLesson(lesson, scope string, people, own, repo []string) (string, string)
    ScrubLesson returns lesson when it teaches without retelling the pull
    request, else "" and why (Lesson*): a pull request or issue reference
    (#123), a URL, one of people (the author's and the reviewers' logins),
    with or without "@", or an @mention of one of own (magnum's logins,
    whose bare name is often the organization's); a "[bot]" suffix is ignored,
    and "@" before anything else is code (@property, @Transactional). A lesson
    of scope general may not name the repository's owner or name either (repo),
    backticks or not, since general lessons can reach the public judge skill;
    a repo lesson goes to that repository's own notes and may. Words match whole
    and case-insensitively.

func WriteFileAtomic(root *os.Root, name string, data []byte) error
    WriteFileAtomic writes data to name inside root: a temporary file next
    to it, then a rename, so a reader never sees half a file. name comes from
    GitHub (a commented path), and root confines it to the retro directory.


TYPES

type Candidate struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // KindThread | KindReview
	URL      string `json:"url"`
	Reviewer string `json:"reviewer"` // Account form
	// Path and the line range [StartLine, Line] are where an inline comment
	// sits in the file at ReviewedSHA (both 0 for a file comment, and for a
	// comment on deleted lines, Side "LEFT", whose lines are the old
	// file's); a review body has neither.
	Path      string `json:"path,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	Line      int    `json:"line,omitempty"`
	Side      string `json:"side,omitempty"` // the diff side: RIGHT, or LEFT for deleted lines
	// ReviewedSHA is the commit magnum reviewed that the comment applies
	// to: the comment's own commit, or a reviewed commit since which the
	// commented file did not change. CommentSHA is the commit the comment
	// was made on.
	ReviewedSHA string    `json:"reviewed_sha"`
	CommentSHA  string    `json:"comment_sha,omitempty"`
	DiffHunk    string    `json:"diff_hunk,omitempty"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
	// Raised is store.MissRaisedRejected when the judge raised a finding
	// near the comment and rejected it (FindingRef "<run id>/<finding id>",
	// ReasonCode its reason), else store.MissRaisedNone.
	Raised     string `json:"raised"`
	FindingRef string `json:"finding_ref,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	// File is the commented file at ReviewedSHA, relative to the candidates
	// file's directory; FileSkipped says why there is no copy. The engine
	// fills both.
	File        string `json:"file,omitempty"`
	FileSkipped string `json:"file_skipped,omitempty"`
}
    Candidate is one comment of another reviewer for the classifier: the root
    comment of a review thread ("t<comment id>") or the body of a review
    ("r<review id>").

func (c Candidate) Lines() []int
    Lines is the candidate's line range as [from, to] (nil without a line).

type Candidates struct {
	PR           string      `json:"pr"`            // the pull request's URL
	ReviewedSHAs []string    `json:"reviewed_shas"` // what magnum reviewed, oldest first
	FilesDir     string      `json:"files_dir"`     // relative to the file's directory
	Candidates   []Candidate `json:"candidates"`
}
    Candidates is the candidates file.

func ReadCandidates(p string) (Candidates, error)
    ReadCandidates reads a candidates file.

type Comparison struct {
	// Descendant: the later commit descends from the reviewed one (GitHub's
	// status "ahead" or "identical"). The file list is three-dot, so only
	// then is it the difference between the two: for an older commit or a
	// history a force push replaced it lists nothing that matters.
	Descendant bool
	// Paths are the files that differ (renames under both names); Complete
	// is false when GitHub cut the list.
	Paths    []string
	Complete bool
}
    Comparison is what Build needs of GitHub's comparison of two commits.

type Input struct {
	// Author is the pull request's author in Account form
	// (github.Account): its own comments are never candidates.
	Author string
	// Reviewed are the commits magnum reviewed; their order does not matter.
	Reviewed []Reviewed
	// Own are magnum's logins in Account form: every configured identity's,
	// and the ones the pull request's reviews were posted as.
	Own []string
	// Threads and Reviews are the pull request's review threads and
	// reviews as GitHub reports them.
	Threads []github.Thread
	Reviews []github.Review
	// Findings are the judge's findings of the pull request
	// (store.Store.FindingsByPR): a posted one near a comment means magnum
	// caught it; a rejected one, that magnum raised it and let it go.
	Findings []store.Finding
	// MinChars drops a comment whose text, without quotes and code blocks,
	// is shorter.
	MinChars int
	// IncludeBots counts comments of bot accounts other than magnum's.
	IncludeBots bool
	// Compare compares a reviewed commit with a later one (GitHub's
	// comparison). Build asks it only for an inline comment on a commit
	// magnum did not review; nil means no comparison, so such a comment is
	// outside.
	Compare func(from, to string) (Comparison, error)
}
    Input is everything Build reads about one pull request.

type Item struct {
	ID       string   `json:"id"`
	Class    string   `json:"class"`
	Severity string   `json:"severity,omitempty"`
	Title    string   `json:"title,omitempty"`
	Lesson   string   `json:"lesson,omitempty"`
	Scope    string   `json:"scope,omitempty"`
	Lines    []int    `json:"lines,omitempty"`
	Match    []string `json:"match,omitempty"`
}
    Item is the classifier's verdict on one candidate.

type Output struct {
	Items []Item `json:"items"`
}
    Output is the classifier's answer file.

type Result struct {
	// Candidates are the comments to classify, Outside the ones about code
	// magnum did not review, stored as such without a classifier.
	Candidates []Candidate
	Outside    []Candidate
	// Caught counts comments near a finding magnum posted, Dropped the
	// comments too short or only an approval. Neither is stored.
	Caught, Dropped int
}
    Result is what Build made of a pull request.

func Build(in Input) (Result, error)
    Build makes the candidates of a pull request: the root comment of every
    review thread and the body of every review by another reviewer (not the
    author, not one of magnum's logins, not a bot unless IncludeBots). Replies
    are never candidates.

    A comment shorter than MinChars without its quotes and code blocks, or that
    only approves, is dropped. One near a finding magnum posted on the same path
    is caught; one near a rejected finding is raised (a comment on deleted lines
    has no line, so neither). A comment made on a reviewed commit applies to it;
    an inline comment on another commit applies to the newest commit magnum
    reviewed before the comment when that commit descends from the reviewed
    one and the commented file did not change between the two (Compare), and is
    outside otherwise, as is a review body on a commit magnum did not review.
    Only a failing Compare is an error.

type Reviewed struct {
	SHA string
	At  time.Time // when the review was posted
}
    Reviewed is a commit magnum posted a review of.

```

## mysqlx

```text
package mysqlx // import "github.com/zhuravel/magnum/internal/mysqlx"

Package mysqlx inventories and drops the per-worktree MySQL databases that
Talkable's bin/worktree-setup creates (talkable_<env>[_<role>]__<slug>) on the
local DBngin server.

Reads are unrestricted; the one destructive operation, Drop, is gated by a
Guard that refuses unsuffixed base databases, system schemas and any slug
the caller has not explicitly allowed. SQL identifiers are validated against
^[A-Za-z0-9_]+$ and back-quoted before they reach the server.

mysqlx talks to MySQL over the wire (database/sql + go-sql-driver/mysql),
not through a subprocess, so it does not use execx; it never logs a DSN.

CONSTANTS

const DefaultDSN = "root:@tcp(127.0.0.1:3306)/?timeout=5s"
    DefaultDSN is DBngin's stock local server: root, empty password,
    127.0.0.1:3306. The timeout bounds only the TCP dial; per-call deadlines
    come from the context passed to each method.


VARIABLES

var DefaultPattern = likePattern("talkable_")
    DefaultPattern is the schemata LIKE pattern ListSuffixed uses: talkable_%__%
    with every literal underscore escaped by likeEscape. Queries pass it with
    an explicit ESCAPE clause, so it means the same thing under every sql_mode
    (NO_BACKSLASH_ESCAPES turns the backslash into a plain character in LIKE
    patterns).

var ErrGuard = errors.New("mysqlx: drop refused by guard")
    ErrGuard is wrapped by every refusal from Guard.Check, Drop and DropAll,
    so callers can tell "magnum declined to drop this" from "MySQL failed".

var ErrInvalidDSN = errors.New("mysqlx: invalid DSN")
    ErrInvalidDSN is wrapped by Open when the DSN cannot be parsed.
    The error never carries the driver's message or any part of the DSN:
    those can echo the password (the driver reads "root:secret/" as a network
    named "root:secret").

var ErrNotFound = errors.New("mysqlx: database or table not found")
    ErrNotFound is wrapped when MySQL reports an unknown database (1049) or a
    missing table (1146), so callers can treat "no schema loaded yet" apart from
    a connection or permission failure.


FUNCTIONS

func Slug(name string) (slug string, ok bool)
    Slug returns the part of a database name after its last "__". ok is false
    when the name has no "__", an empty base (name starts with "__") or an empty
    slug (name ends with "__"): such names are not per-worktree databases.


TYPES

type Client struct {
	// Has unexported fields.
}
    Client is a handle on the local MySQL server. It is safe for concurrent use.

func Open(dsn string) (*Client, error)
    Open prepares a client for dsn (go-sql-driver/mysql format); an empty
    dsn means DefaultDSN. It validates the DSN but does not connect,
    so the daemon can start while DBngin is down; use Ping to probe the server.
    multiStatements is refused: every statement magnum sends is a single
    statement.

func (c *Client) Close() error
    Close releases the client's connections.

func (c *Client) Drop(ctx context.Context, name string, g Guard) error
    Drop removes one database (DROP DATABASE IF EXISTS, so a retry after a crash
    is harmless) after g.Check approves its name. A refusal wraps ErrGuard and
    sends nothing to the server.

func (c *Client) DropAll(ctx context.Context, names []string, g Guard) []DropResult
    DropAll drops each name in order with Drop and returns one result per name,
    in input order. A failure (guard refusal or server error) does not stop the
    remaining names; a cancelled context fails the names it never reached.

func (c *Client) ListPrefixed(ctx context.Context, prefixes []string) ([]Database, error)
    ListPrefixed returns every schema named <prefix><slug> for one of prefixes,
    with its slug and on-disk size, ordered by name. Each prefix is a pool's
    database base followed by the separator ("talkable_development__");
    the slug is the non-empty rest of the name and must be what Slug reports,
    so a name like talkable_development__a__b (slug "b" to Slug and the guard)
    is not listed under that prefix. Prefixes are matched literally (LIKE
    wildcards in them are escaped) and case-sensitively, whatever the server's
    collation. Duplicate prefixes are ignored; no prefix at all lists nothing.
    A prefix that does not end in "__" or has an empty base is an error:
    no database under it could ever be dropped through Guard.

func (c *Client) ListSuffixed(ctx context.Context) ([]Database, error)
    ListSuffixed returns every schema named talkable_%__% (the per-worktree
    databases) with its slug and on-disk size, ordered by name. Schemas whose
    name ends in "__" (empty slug) are not worktree databases and are skipped.
    It only sees the talkable_ family; ListPrefixed lists any other prefix.

func (c *Client) Ping(ctx context.Context) error
    Ping verifies that the server is reachable and the credentials work.

func (c *Client) SchemaMigrationsMax(ctx context.Context, dbName string) (string, error)
    SchemaMigrationsMax returns MAX(version) from dbName's Rails
    schema_migrations table. Versions are compared as strings, which is correct
    for Rails' fixed-width timestamps. An empty table yields "". A missing
    database or table wraps ErrNotFound.

type Database struct {
	Name   string  // full schema name, e.g. talkable_development__review3
	Slug   string  // text after the last "__", e.g. review3
	SizeMB float64 // data_length + index_length of its tables, in MiB (0 when it has none)
}
    Database is one per-worktree schema found on the server.

type DropResult struct {
	Name string
	Err  error
}
    DropResult is the outcome for one name in DropAll; Err is nil on success.

type Guard struct {
	// AllowRegexp must match the whole slug (the text after the last "__"); it
	// is implicitly anchored, so an unanchored expression cannot match a
	// fragment. A nil AllowRegexp refuses every name.
	AllowRegexp *regexp.Regexp
	// Prefix, when non-empty, is a required name prefix (callers acting on the
	// Talkable pool set "talkable_" so a foreign schema with a "__" in its name
	// can never be dropped).
	Prefix string
}
    Guard decides which databases Drop may remove. The zero value refuses
    everything.

func (g Guard) Check(name string) error
    Check reports nil when the guard would allow dropping name, or an error
    wrapping ErrGuard saying why not. It is pure: it never touches the server.

    A name is refused unless all of these hold: it is a plain identifier
    ([A-Za-z0-9_]+); it has a non-empty base and a non-empty slug around a "__"
    (so unsuffixed base names such as "talkable_development" and system schemas
    are never droppable); it carries Prefix when one is set; and its slug fully
    matches AllowRegexp.

```

## notes

```text
package notes // import "github.com/zhuravel/magnum/internal/notes"

Package notes measures, versions and curates the repository notes (the file
every review role reads first and the judge rewrites, engine.NotesPath)
and their harness directory of QA scripts. It reads and writes files only:
the engine decides when (after a round, at startup, on a schedule) and the CLI
shows and applies what is here.

    <root>/<owner>/<repo>.md                    the notes
    <root>/<owner>/<repo>/                      the harness
    <root>/<owner>/<repo>.lock/                 the lock the judges take (agents.NotesLockLine)
    <root>/.curate/<owner>/<repo>/<id>/         one curation's scratch copy and proposal (curate.go)

Every version of the notes and the harness is kept in the registry
(store.RecordNotesVersion), not here. GitHub owners never start with a dot,
so ".curate" never collides with an owner's directory.

CONSTANTS

const (
	MissNoted   = "noted"
	MissSkipped = "skipped"
)
    The actions of a MissChange.

const (
	ActionKept    = "kept"
	ActionAdded   = "added"
	ActionMerged  = "merged"
	ActionRemoved = "removed" // a section
	ActionDeleted = "deleted" // a harness file
)
    The actions of a Change.

const (
	LimitBytes        = "max_bytes"
	LimitLine         = "max_line"
	LimitHarnessFiles = "max_harness_files"
	LimitHarnessBytes = "max_harness_bytes"
)
    The limits by their notes key, in the order Over reports them.

const (
	MaxFileBytes  = 8 << 20
	MaxStateBytes = 64 << 20
)
    Caps on what ReadState reads: a harness this large is not notes, and a
    version must fit the registry.

const CurateDirName = ".curate"
    CurateDirName is the curations' directory under the notes root.

const SnapshotFile = "notes-before.json"
    SnapshotFile is the snapshot a round takes of the notes before its judge is
    prompted, in the round's report directory.


VARIABLES

var ErrBusy = errors.New("notes busy: another writer holds the notes lock")
    ErrBusy is Lock's error when another holder kept the lock past the wait.

var ErrTooLarge = errors.New("notes: the harness is too large to record")
    ErrTooLarge is ReadState's error for a harness past MaxFileBytes or
    MaxStateBytes.


FUNCTIONS

func Fingerprint(notes []byte, files []File) string
    Fingerprint identifies a state of the notes and the harness: the notes'
    content (a missing file is empty notes, as a version records it) and every
    harness file's name and content.

func HarnessDelta(before, after []File) (added, removed, changed []string)
    HarnessDelta compares two listings: the names only in after (added), only in
    before (removed) and in both with another content (changed), each sorted.

func LineChanges(a, b string) (added, removed int)
    LineChanges counts the lines b adds to a and the lines it removes, with the
    same line matching as Unified (a changed line counts once in each).

func Lock(ctx context.Context, path string, wait time.Duration) (func(), error)
    Lock takes the notes lock at path (Repo.Lock) as the judges do, retrying
    until wait has passed (ErrBusy); it returns the release.

func Measure(r Repo, l Limits) (Size, []File, error)
    Measure measures r's notes file and harness. A missing notes file or harness
    measures zero; any other read error is returned.

func Printable(text string) string
    Printable replaces the control characters other than newline and tab in text
    written by an agent, so it cannot drive a terminal.

func SameListing(a, b []File) bool
    SameListing reports whether two listings name the same files with the same
    content.

func Sections(text []byte) []string
    Sections lists the "## " headings of notes text.

func TextSHA(text []byte) string
    TextSHA is the SHA-256 of notes text, hex.

func Unified(nameA, nameB, a, b string, ctx int) string
    Unified returns a unified diff of a and b, compared line by line,
    with ctx lines of context around each change: the header "--- <nameA>\n+++
    <nameB>\n", then hunks "@@ -<start>,<count> +<start>,<count> @@\n" followed
    by lines prefixed with ' ', '-' or '+', each ending in "\n". It returns ""
    when a == b.

    Start numbers are 1-based. A range of one line is written as its start
    alone ("@@ -3 +3 @@") and a range of no lines as the number of the line
    before it and a zero count ("-0,0" when a is empty), as GNU diff does.
    Changes whose context would overlap or touch (at most 2*ctx unchanged lines
    between them) share one hunk. Lines are split on "\n" only, so a "\r" stays
    part of its line, and a line without its "\n" (only the last one of a side
    can be) differs from the same text with it. Such a line is printed followed
    by "\n\\ No newline at end of file\n". A negative ctx counts as 0.

func Validate(p Proposal, c Check) []string
    Validate returns what is wrong with proposal p (nil = valid), in words fit
    for a nudge and the registry (names and rules, never notes text): the notes
    and changes.json must be there; every proposed section and harness file must
    be accounted for as kept or added with a one-line reason, and every current
    harness file with what became of it; every proposed harness file must be
    named in the notes; nothing may name a pull request, one of its branches or
    probe files, carry what looks like a secret or a home directory path; every
    miss given must be accounted for once, noted with a section of the proposal
    or skipped with a reason. Size is not checked: the limits trigger curations,
    they do not cap them. A proposal that changes nothing is invalid unless it
    was given misses and skips them all (the operator confirms the skips).

func WriteSnapshot(dir string, s Snapshot) error
    WriteSnapshot writes s to dir/SnapshotFile.

func WriteState(r Repo, s State) error
    WriteState makes r's notes and harness s: the notes file is replaced
    atomically (removed when s has none), and the harness by a complete new
    directory renamed into place, the old one removed after. The caller holds
    r's lock (Lock). A harness path that is not clean (cleanName) is refused
    before anything is written.

func WriteSuperseded(s Scratch, proposed State, changes []byte) error
    WriteSuperseded writes the stale proposal a curation follows up on into s's
    superseded/ directory, for the curator to read: its notes as notes.md, its
    harness under harness/ and its changes.json, with a reason for every section
    and file it kept, merged or removed. The notes changed since that proposal
    was made, so it was superseded rather than applied; its work is not lost.


TYPES

type Blob struct {
	Path   string // slash-separated, relative to the harness directory
	SHA256 string
	Body   []byte
}
    Blob is one harness file with its content.

type Change struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Into   string `json:"into,omitempty"` // merged: where it went
	Reason string `json:"reason"`
}
    Change is one item of Changes.

type Changes struct {
	Sections []Change     `json:"sections"`
	Files    []Change     `json:"files"`
	Misses   []MissChange `json:"misses,omitempty"`
}
    Changes is the curator's changes.json: what became of every notes section
    (by its "## " heading) and every harness file, each with a one-line reason,
    and of every miss the curation was given (misses.json).

type Check struct {
	Base State
	// PRNumbers are the repository's pull request numbers; Branches their
	// head branches. A harness file named after a number, or notes naming a
	// branch, carry one pull request's content.
	PRNumbers []int
	Branches  []string
	// Misses are the ids of the misses the curation was given: the proposal
	// accounts for each.
	Misses []int64
}
    Check is what Validate compares a proposal with: the state the curation
    started from and what names one pull request of the repository.

type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
    File is one harness file: its slash-separated path relative to the harness
    directory, its size and the SHA-256 of its content.

func List(dir string) ([]File, error)
    List lists the regular files under dir, recursively, sorted by name;
    symbolic links and other special files are left out (never followed).
    A missing dir lists nothing.

type Limits struct {
	MaxBytes        int64 `json:"max_bytes"`
	MaxLine         int   `json:"max_line"` // characters
	MaxHarnessFiles int   `json:"max_harness_files"`
	MaxHarnessBytes int64 `json:"max_harness_bytes"`
}
    Limits are the notes curation triggers: past any of them the repository is
    marked for curation. None of them blocks a review or a proposal.

type MergeConflict struct{ Start, End int }
    MergeConflict is a range of base lines both sides changed differently:
    lines [Start, End) of base, 0-based.

func Merge3(base, ours, theirs string) (merged string, conflicts []MergeConflict)
    Merge3 merges the changes ours and theirs made to base, line by line (diff3,
    as `git merge-file` does it). A region of base that only one side changed
    takes that side's lines, and one both changed the same way takes them once.
    Changes that overlap or touch (an insertion next to the other side's
    change included, as git treats changes on adjacent lines) are one region,
    and when the two sides' lines for it differ the region is a conflict:
    it is listed in conflicts and merged keeps base's lines there, so merged
    means something only when conflicts is empty.

type MissChange struct {
	ID      int64  `json:"id"`
	Action  string `json:"action"`
	Section string `json:"section,omitempty"`
	Reason  string `json:"reason,omitempty"`
}
    MissChange is what a curation did with one miss it was given: noted,
    with the "## " section of the proposal that covers it now, or skipped,
    with a one-line reason.

type Proposal struct {
	State       State
	Changes     Changes
	ChangesJSON []byte
}
    Proposal is what a curator wrote: the proposed notes and harness, and its
    changes.json (raw and parsed).

func ReadProposal(s Scratch) (Proposal, []string)
    ReadProposal reads what the curator wrote in s. Problems name what is
    missing or unreadable, in words fit for a nudge (paths, never content).

type Repo struct {
	Root, Owner, Name string
}
    Repo is one repository's notes under Root; Owner and Name are lower-case
    single path elements (engine.NotesPath checks them).

func RepoOf(notesPath string) (Repo, bool)
    RepoOf is the Repo whose notes file is notesPath (<root>/<owner>/<name>.md);
    false when the path does not have that shape.

func (r Repo) Curate() string
    Curate is the directory of the repository's curations.

func (r Repo) FullName() string
    FullName is "owner/name".

func (r Repo) Harness() string
    Harness is the harness directory next to the notes file.

func (r Repo) Lock() string
    Lock is the lock directory the judges take (agents.NotesFiles).

func (r Repo) Notes() string
    Notes is the notes file.

type Scratch struct{ Dir string }
    Scratch is one curation's directory.

func PrepareScratch(dir string, base State, usage []byte) (Scratch, error)
    PrepareScratch makes a new scratch directory dir holding base (the notes as
    current.md, the harness as current/ and as the harness/ the curator edits)
    and usage as usage.json.

func (s Scratch) Changes() string

func (s Scratch) Current() string

func (s Scratch) CurrentHarness() string

func (s Scratch) Harness() string

func (s Scratch) Misses() string

func (s Scratch) Proposal() string

func (s Scratch) Superseded() string

func (s Scratch) Usage() string

type Size struct {
	Exists       bool  `json:"exists"` // the notes file exists
	Bytes        int64 `json:"bytes"`
	Lines        int   `json:"lines"`
	LongLines    int   `json:"long_lines"`   // lines longer than Limits.MaxLine characters
	LongestLine  int   `json:"longest_line"` // characters
	HarnessFiles int   `json:"harness_files"`
	HarnessBytes int64 `json:"harness_bytes"`
}
    Size is what the notes and the harness hold.

func MeasureText(text []byte, maxLine int) Size
    MeasureText measures notes text: its bytes, lines and the lines longer than
    maxLine characters (0 = none counted).

func (s Size) Over(l Limits) []string
    Over names the limits s is past, in the order of the Limit* constants;
    a limit of 0 or less is off.

type Snapshot struct {
	Round    int       `json:"round"`
	At       time.Time `json:"at"`
	Exists   bool      `json:"exists"`
	NotesSHA string    `json:"notes_sha256"`
	Harness  []File    `json:"harness"`
}
    Snapshot is the notes and the harness as a round's judge found them:
    after the round, a state that differs from it is the round's change (or a
    concurrent writer's) and is recorded as a version of the judge.

func ReadSnapshot(dir string) (Snapshot, error)
    ReadSnapshot reads dir/SnapshotFile.

func Take(r Repo, round int, now time.Time) (Snapshot, error)
    Take snapshots r now for round.

func (s Snapshot) Fingerprint() string
    Fingerprint identifies the snapshot's state, comparable with
    State.Fingerprint.

type State struct {
	Exists bool // the notes file exists (false: Notes is empty)
	Notes  []byte
	Files  []Blob // sorted by Path
}
    State is the whole content of a repository's notes: the notes text and
    every harness file with its body. It is what a version in the registry holds
    (store.NotesVersion) and what an applied proposal writes.

func ReadState(r Repo) (State, error)
    ReadState reads r's notes and harness: regular files only, as List lists
    them. A missing notes file or harness reads empty.

func (s State) Fingerprint() string
    Fingerprint identifies s (Fingerprint of its notes and listing).

func (s State) Listing() []File
    Listing is s's harness as a listing (names, sizes, hashes).

func (s State) Same(o State) bool
    Same reports whether s and o hold the same notes and harness.

func (s State) Size(maxLine int) Size
    Size measures s against maxLine (Size.Over compares it with the limits).

type StateMerge struct {
	State          State           // the merged state; apply it only when Clean
	Clean          bool            // neither the notes text nor a harness file conflicts
	NotesConflicts []MergeConflict // in base's lines of the notes
	Kept           []string        // harness files the proposal deletes that changed since base: kept as live has them
	Conflicts      []string        // harness files both sides changed in ways that do not merge
	Merged         []string        // harness files both sides changed whose texts merged line by line
}
    StateMerge is the three-way merge of a stale curation proposal: base is the
    state the proposal started from, live the notes now, proposed the proposal's
    state.

func MergeStates(base, live, proposed State) StateMerge
    MergeStates merges proposed into live, both changes of base. The notes text
    merges line by line (Merge3). Each harness file, compared by its presence
    and its hash, goes by what each side did to it:

      - live left it as base had it: the proposal's version wins, deletion
        included;
      - the proposal left it as base had it, or both made it the same: live's
        version stays;
      - the proposal deletes it but live changed it: live's version stays
        and the path is in Kept, so a file written since the proposal started
        survives;
      - live deleted it and the proposal changed it, or both added it with
        different texts: a conflict;
      - both changed it: their texts merge line by line, and when they do not
        the file is a conflict.

    A conflicting file stays as live has it (absent when live deleted it), as
    the notes text keeps base's lines where its conflicts are; the merged state
    is to be applied only when Clean. Kept, Conflicts and Merged are in path
    order, nil when empty, and so are the state's files, which have SHA256 set.
    The inputs are not modified.

```

## notify

```text
package notify // import "github.com/zhuravel/magnum/internal/notify"

Package notify is magnum's user-facing status surface: toasts and herdr sidebar
tokens. The tab-bar file is the engine's (engine.WriteTabBarFile).

  - Toast shows a deduplicated message through herdr's notification.show and
    falls back to `osascript display notification` when herdr cannot show it
    (socket unreachable, timeout, or no terminal client attached).
  - ToastUrgent is Toast for alerts that must not be lost (identity leak,
    login required, usage paused, needs attention): it retries through herdr's
    rate limit with backoff and falls back to osascript after.
  - Batcher coalesces informational toasts (reviews posted, new repositories)
    into one summary per minute.
  - Sidebar publishes display-only tokens for a workspace under the source
    "magnum" with a 24 h TTL, so stale tokens disappear on their own.

Everything is best effort and never decides pipeline behavior: callers log
the returned errors and carry on. All subprocesses go through execx.Runner;
toast text and token values are passed through execx.Redact first.

CONSTANTS

const (
	DefaultBatchWindow  = 60 * time.Second
	DefaultBatchDedupe  = 10 * time.Minute
	DefaultBatchLines   = 5
	DefaultSummaryTitle = "magnum: %d reviews posted"
)
    Batcher defaults.

const (
	// SidebarSource is the metadata source id magnum reports under.
	SidebarSource = "magnum"
	// SidebarTTL is how long herdr keeps reported tokens without a refresh.
	SidebarTTL = 24 * time.Hour
	// MaxTitleRunes and MaxBodyRunes bound toast text (clipped with an
	// ellipsis) so a runaway error message cannot flood the screen.
	MaxTitleRunes = 80
	MaxBodyRunes  = 400
)
const (
	UrgentRetryFirst  = time.Second
	UrgentRetryBudget = 60 * time.Second
)
    Urgent toast retries: herdr's rate_limited (or busy) answer is retried after
    UrgentRetryFirst, doubling each time, until UrgentRetryBudget of waiting is
    spent; then the toast goes to osascript.


VARIABLES

var (
	KindReviewPosted = Kind{One: "review posted", Many: "reviews posted"}
	KindNewRepo      = Kind{One: "new repository", Many: "new repositories"}
)
    Kinds of informational toast.


TYPES

type Batcher struct {
	// Notifier delivers the toasts; required.
	Notifier *Notifier
	// Key namespaces the store dedupe keys; default "batch".
	Key string
	// Window is how long the oldest pending item waits for company before
	// FlushDue sends; default DefaultBatchWindow.
	Window time.Duration
	// Dedupe is the store window for a flushed batch's key; default
	// DefaultBatchDedupe.
	Dedupe time.Duration
	// SummaryTitle is the summary toast title, a format with one %d for the
	// item count; default DefaultSummaryTitle.
	SummaryTitle string
	// MaxLines caps the item lines in a summary body (the rest become
	// "+N more"); default DefaultBatchLines.
	MaxLines int
	// Now returns the current time; nil means time.Now. Tests replace it.
	Now func() time.Time

	// Has unexported fields.
}
    Batcher coalesces bursts of informational events into one toast:
    the engine Adds an Item per event and calls FlushDue every tick (and Flush
    on shutdown). One pending item flushes as its own toast; several flush as
    a single summary ("magnum: 3 reviews posted, 14 new repositories" with one
    line per item). Safe for concurrent use.

func (b *Batcher) Add(it Item)
    Add queues an item. The first pending item starts the coalescing window.

func (b *Batcher) Due() bool
    Due reports whether the oldest pending item has waited a full Window.

func (b *Batcher) Flush(ctx context.Context) (bool, error)
    Flush announces everything pending right now, regardless of age, and reports
    whether a toast was delivered. Items whose own Window is still reserved are
    dropped first. The batch is consumed when the toast is delivered, suppressed
    by the dedupe gate, or dropped because notifications are disabled.
    If delivery fails (or the store fails) the items stay queued (still due),
    their item reservations are released and the error is returned, so the next
    tick retries.

func (b *Batcher) FlushDue(ctx context.Context) (bool, error)
    FlushDue flushes when Due; otherwise it does nothing.

func (b *Batcher) Pending() int
    Pending reports how many items wait for a flush.

type DedupeStore interface {
	ShouldSend(ctx context.Context, key string, window time.Duration) (bool, error)
	ForgetSend(ctx context.Context, key string) error
}
    DedupeStore is the part of *store.Store this package uses. ShouldSend
    reserves key for the window; ForgetSend releases a reservation whose toast
    was never delivered.

type HerdrClient interface {
	NotificationShow(ctx context.Context, title, body string) (herdr.NotificationResult, error)
	WorkspaceReportMetadata(ctx context.Context, workspaceID, source string, tokens map[string]string, ttl time.Duration) error
}
    HerdrClient is the part of *herdr.Client this package uses.

type Item struct {
	// Key identifies the event ("owner/repo#123@sha"). A second Add with a
	// key that is already pending is ignored, and the keys of a flushed batch
	// form its dedupe key, so re-reporting the same events (crash recovery)
	// does not toast twice. Empty means "identify by Title and Body".
	Key string
	// Title and Body are the toast used when the item is announced alone.
	Title, Body string
	// Line is the one-line description used in a multi-item summary; it
	// defaults to Title.
	Line string
	// Kind counts the item in a summary title. When no pending item has a
	// Kind the title is SummaryTitle with the total count (the review-posted
	// batcher's original form).
	Kind Kind
	// Window, with a Key and a dedupe Store, announces the item at most once
	// per Window: the flush reserves Key itself in the store (so an item keeps
	// the dedupe key it had as a direct Toast), drops items whose key is
	// still reserved, and releases the reservations when delivery fails.
	Window time.Duration
}
    Item is one event to announce, for example one posted review.

type Kind struct{ One, Many string }
    Kind names what an item announces, in the singular and the plural,
    for the summary title of a mixed batch ("magnum: 3 reviews posted, 14 new
    repositories").

type Notifier struct {
	// Herdr shows toasts and reports sidebar tokens. Nil means herdr is not
	// available (Toast goes straight to osascript, Sidebar fails).
	Herdr HerdrClient
	// Store dedupes toasts by key. Nil disables deduplication.
	Store DedupeStore
	// Runner runs the osascript fallback. Nil disables the fallback.
	Runner execx.Runner
	// Enabled is the [herdr] notify switch. When false Toast (and every
	// Batcher flush) is a silent no-op. Sidebar is a display surface, not a
	// notification, and ignores it.
	Enabled bool
	// Log, when set, receives one redacted line per fallback or suppression.
	Log execx.Logger
	// Sleep waits between urgent retries; nil means a timer that stops early
	// when ctx is done (returning ctx.Err()). Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}
    Notifier delivers toasts and status tokens. Its methods are safe for
    concurrent use as long as the injected clients are.

func (n *Notifier) Sidebar(ctx context.Context, workspaceID string, tokens map[string]string) error
    Sidebar publishes tokens for the herdr workspace under source "magnum"
    with a 24 h TTL (see SidebarSource and SidebarTTL). Values are redacted
    and flattened to one line; the caller's map is not modified. Errors from
    herdr (for example workspace_not_found) are wrapped, so match them with
    herdr.IsCode.

func (n *Notifier) Toast(ctx context.Context, key, title, body string, window time.Duration) (bool, error)
    Toast shows title/body to the user and reports whether a toast was actually
    delivered.

    With a non-empty key, a positive window and a Store, Toast first asks
    Store.ShouldSend(key, window): a repeat inside the window is counted
    and dropped (false, nil). Otherwise it calls herdr's notification.show.
    If herdr is unreachable or times out, or has no terminal client attached
    ("no_foreground_client"), it falls back to osascript. If herdr itself chose
    not to show the toast (disabled, rate_limited, busy) that is respected:
    (false, nil). Any other herdr error is returned and nothing falls back.

    ShouldSend records the send before delivery, so it works as a reservation:
    when delivery fails with an error (every surface failed), Toast releases
    it with Store.ForgetSend and a retry of the same key inside the window is
    delivered. A herdr decision not to show the toast is not an error and keeps
    the reservation, so the window stays consumed.

func (n *Notifier) ToastUrgent(ctx context.Context, key, title, body string, window time.Duration) (bool, error)
    ToastUrgent is Toast for alerts that must reach the user: an identity leak,
    a login that needs the user, a paused agent kind, a PR that needs attention.
    Dedupe works as in Toast (the key is reserved once for all attempts,
    released when nothing was delivered, consumed when something was). Delivery
    differs: when herdr answers rate_limited or busy, the toast is retried after
    1s, 2s, 4s… until UrgentRetryBudget of waiting is spent and then shown with
    osascript; when herdr is unreachable, has no client or fails the request,
    osascript is used at once. Only herdr's "disabled" (the user turned toasts
    off in herdr) is respected as final.

    ToastUrgent can block for up to UrgentRetryBudget plus the osascript
    timeout; a caller on a latency-sensitive loop runs it in a goroutine.
    A cancelled ctx stops the waiting, releases the key and returns ctx's error.

```

## paths

```text
package paths // import "github.com/zhuravel/magnum/internal/paths"

Package paths defines magnum's on-disk layout. An installed magnum keeps its
files where XDG says: the user config in ~/.config/magnum, the registry, review
reports and notes in ~/.local/share/magnum, logs, locks and the identities'
gh config in ~/.local/state/magnum. A checkout layout (everything under
<checkout>/state, gitignored) remains for development (MAGNUM_HOME).

FUNCTIONS

func Expand(p string) string
    Expand replaces a leading ~ with the user's home directory.


TYPES

type Layout struct {
	// Home is the repository checkout magnum runs from (MAGNUM_HOME, or the
	// checkout holding the binary); "" for an installed binary. In the
	// checkout layout (DataDir and StateDir empty) everything lives under
	// Home/state.
	Home string
	// DataDir holds the registry, the review reports and the repository
	// notes ($XDG_DATA_HOME/magnum); StateDir the logs, locks, pidfile, the
	// identities' gh config, the tab-bar file and the skill copies
	// ($XDG_STATE_HOME/magnum). Both empty: the checkout layout.
	DataDir, StateDir string
	// Exe is the running binary as it was invoked ("" when unknown): what
	// launchd runs when no checkout binary exists (Binary).
	Exe string
	// UserConfig is the user's config file, layered over the built-in
	// defaults: $XDG_CONFIG_HOME/magnum/config.toml, else
	// ~/.config/magnum/config.toml (Resolve sets it). ""
	// means none (tests build layouts without it).
	UserConfig string
	// Scratch, when set, holds the registry, the review reports and the
	// repository notes instead of state/ (magnum eval: a replay never touches
	// the live registry, reports or notes). Logs, locks, the pidfile and the
	// identities' gh config stay under state/.
	Scratch string
}
    Layout resolves every path magnum reads or writes.

func Resolve() (Layout, error)
    Resolve picks the layout:

      - MAGNUM_HOME set: the checkout layout under it (development, tests).
        It is expanded (~), made absolute and symlink-free (see canonicalDir):
        magnum hands paths under it (GH_CONFIG_DIR, the report directory) to
        agents whose working directory is a PR checkout, where a relative or
        symlinked path would mean something else.
      - Else the XDG layout, with Home the checkout holding the binary, if any
        (it has config.toml, config.defaults.toml or magnum's own go.mod within
        four levels up, symlinks resolved).

func (l Layout) Binary() string
    Binary is what launchd runs: the checkout's bin/magnum when there is one,
    else the running binary (a Homebrew install's stable bin/ link rather than
    its versioned Cellar path, which an upgrade removes).

func (l Layout) CheckoutLayout() bool
    CheckoutLayout reports whether everything lives under Home/state (the
    development layout, MAGNUM_HOME).

func (l Layout) Config() string
    Config is the checkout's legacy full config (Home/config.toml); "" without a
    checkout.

func (l Layout) ConfigDir() string
    ConfigDir is the user config's directory (~/.config/magnum): eval.toml,
    App keys, prompt overrides; "" without one.

func (l Layout) DB() string

func (l Layout) DaemonLog() string

func (l Layout) Data() string
    Data is where the registry, reports and notes live (see data).

func (l Layout) EnsureDirs() error
    EnsureDirs creates the state tree and makes every directory in it private
    (0700). MkdirAll applies its mode only to directories it creates, so a
    state tree that already exists with looser permissions (an older install,
    a manual mkdir) is tightened explicitly: it holds the database, logs and the
    per-identity gh credentials.

func (l Layout) GhConfigDir(id string) string

func (l Layout) GhRoot() string

func (l Layout) Learn() string
    Learn is where the learning loop keeps its inputs and outputs: the retro's
    runs live under Learn()/retro/<run>/.

func (l Layout) Lock() string

func (l Layout) Logs() string

func (l Layout) Notes() string

func (l Layout) OpsLock() string

func (l Layout) Pid() string

func (l Layout) Plugin() string
    Plugin is the checkout's herdr plugin manifest; "" without a checkout.

func (l Layout) ReviewDir(owner, repo string, number int, sha string) string
    ReviewDir is where one review round's reports live.

func (l Layout) Reviews() string

func (l Layout) Skill() string
    Skill is the checkout's judge skill; "" without a checkout (the binary's
    embedded copy is used).

func (l Layout) State() string
    State is where logs, locks, the pidfile and the gh config dirs live:
    StateDir, else Home/state.

func (l Layout) TabBar() string

func (l Layout) Valid() bool
    Valid reports whether the layout names where magnum's files live (a zero
    Layout, as tests build, does not).

```

## pipeline

```text
package pipeline // import "github.com/zhuravel/magnum/internal/pipeline"

Package pipeline runs one review round for a PR inside its herdr workspace:
a readiness step in the checkout (the repository's prepare commands and ready
probes and a Ruby check, readiness.go; failures only inform the judge), then the
round's configured roles (config.Role: agent sessions on any configured kind,
or shell commands) in stages (config.Config.Stages: the roles of a stage in
parallel, then a check that they left HEAD and the tree as they found them,
restoring both when not: no role may edit the checkout), then the judge,
then verification on GitHub (the oracle for "review posted") and the optional
dismissal of the identity's own stale CHANGES_REQUESTED. RolesToRun says which
roles a round runs; with the built-in roles that is claude-review, codex-review
and claude-simplify (its first round, or when requested) in parallel, then
codex-judge.

RunRound blocks until the round ends and is meant to run in its own goroutine.
It records every transition on the round's run rows and appends audit events
(subject "pr:<owner>/<name>#<N>"), but never changes the PR's state: the engine
maps RoundResult.Outcome onto the PR state machine.

Completion of an agent turn is read from the store: the engine's per-tick
agents.Observe moves a run to ended after two idle ticks, and this package polls
the run rows (every PollInterval) until then, or until the role's timeout. The
judge's turn also ends when its result file has been present for ResultSettle.

CONSTANTS

const (
	KindInitial  = store.RunInitial  // first review of the PR
	KindRereview = store.RunRereview // new head after an earlier review by the same judge session
	KindContinue = store.RunContinue // a pause (usage limit) ended: the judge finishes its turn; the other roles do not run
	KindRecovery = store.RunRecovery // a fresh judge session after the old one was lost
)
    Round kinds (the run kinds of the round's run rows).

const (
	OutcomePosted         = "posted"          // review verified on GitHub (marker, login, commit)
	OutcomeDryRun         = "dry_run"         // dry run: the judge wrote planned_review, nothing posted
	OutcomeBlocked        = "blocked"         // the judge reported a blocker, or waits on a dialog
	OutcomeIdentityError  = "identity_error"  // the judge's identity check failed; nothing posted
	OutcomeIdentityLeak   = "identity_leak"   // a review carrying the run's marker was posted by another login
	OutcomeClosed         = "closed"          // the judge found the PR closed
	OutcomeStopped        = "stopped"         // the judge was told to stop, or ctx was cancelled
	OutcomeUsageLimit     = "usage_limit"     // judge stopped on a usage limit: Pause
	OutcomeLoginRequired  = "login_required"  // the judge's agent kind is logged out: Pause
	OutcomeOverloaded     = "overloaded"      // API overloaded/rate limited: Pause (backoff)
	OutcomeTimeout        = "timeout"         // judge_timeout passed without a result
	OutcomeNeedsAttention = "needs_attention" // nothing posted after one nudge, or an inconsistent result
	OutcomeError          = "error"           // the round could not run or be evaluated (RunRound's error is set)
)
    Round outcomes (RoundResult.Outcome and the judge run's outcome column).

const (
	ReportOK          = "ok"           // report written (non-empty)
	ReportMissing     = "missing"      // finished without writing the report
	ReportTimeout     = "timeout"      // the role's timeout passed; the agent or command was interrupted
	ReportFailed      = "failed"       // the prompt or command failed (a shell command's exit status outside ok_status)
	ReportLost        = "lost"         // the session's pane or agent disappeared
	ReportNoSession   = "no_session"   // the role has no live session
	ReportBusy        = "busy"         // a shell role's pane was not an idle shell
	ReportHumanActive = "human_active" // a human typed into the PR's panes recently
	ReportCancelled   = "cancelled"    // ctx was cancelled
	ReportHeadMoved   = "head_moved"   // a push cut the turn short; the round restarted on the new head (run outcome only)
)
    Role report statuses (RoleReport.Status and the non-judge runs' outcome
    column). Besides these, a status can be a health kind found in the pane:
    "login_required", "usage_limit", "overloaded", "stalled", "blocked",
    "trust_dialog".

const (
	DefaultPollInterval = 10 * time.Second
	// ResultSettle is how long the judge's result file must be present before
	// it ends the turn when the agent has not gone idle (status flicker).
	ResultSettle = 2 * time.Minute
	// InterruptWait bounds the wait for an interrupted agent (a timed-out
	// judge, a reviewer a push cut short) to be seen idle; the engine's
	// Observe records agent statuses every tick.
	InterruptWait = 60 * time.Second
	// TimeUpGrace is how long a reviewer whose time ran out has, once asked
	// for its report (round.timeUp), to write it and end its turn before it
	// is interrupted as timed out.
	TimeUpGrace = 5 * time.Minute
	// StopGrace bounds the wait for an interrupted reviewer, told to stop
	// the background tasks it left running (round.stopReviewer), until its
	// transcript shows none.
	StopGrace = 2 * time.Minute
)
    Timing.

const (
	// ReadinessFile is the JSON file the readiness step writes into the
	// round's report directory, next to the judge's result file.
	ReadinessFile = "readiness.json"
	// ReadinessShell runs the prepare commands, the ready probes and the
	// Ruby check as `zsh -lc <command>`: the login shell Codex and Claude
	// run their tool commands in, so the commands see the Ruby, Node and
	// database settings the agents see. The pool's reset_db commands do not
	// run in it: see Runner.Mise.
	ReadinessShell = "zsh"
	// RubyCheckCommand is the built-in Ruby check's command.
	RubyCheckCommand = "ruby -v"
)
const (
	ReplyFixed   = "fixed"
	ReplyNotABug = "not a bug"
	ReplyWontFix = "won't fix"
	ReplyOther   = "other"
)
    Reply classes (agents.ThreadReply.Class).

const (

	// ThreadsFile is the threads file in the round's report directory.
	ThreadsFile = "review-threads.json"
)
const DeltaCheckFile = "delta-check.json"
    DeltaCheckFile is the delta check's file list in the round's report
    directory.

const PostMergeEvent = "COMMENT"
    PostMergeEvent is the review event of a post-merge round
    (RoundInput.PostMerge), with or without findings.

const RelatedFile = "related.json"
    RelatedFile is the related PRs' file in the round's report directory.


VARIABLES

var ErrInvalid = errors.New("pipeline: invalid round")
    ErrInvalid marks a RoundInput or Runner that cannot run a round.


FUNCTIONS

func JudgeEvents(cfg *config.Config, fullName string, id *config.Identity, postMerge bool, reports ...agents.Report) (noFindings, blocking string)
    JudgeEvents are the review events the judge's prompt names for a round of
    repository fullName ("owner/name") posted as identity id: the repository's
    or the identity's (config.Config.VerdictsFor); COMMENT both ways in a
    post-merge round, where a verdict blocks nothing; and COMMENT for no
    findings when one of the round's reports is missing (an APPROVE once went
    out while claude-review had hit a usage limit: a review that did not hear
    every reviewer approves nothing).

func RolesToRun(ctx context.Context, st *store.Store, cfg *config.Config, pr store.PR, roles []config.Role, requested []string, kind string) ([]config.Role, error)
    RolesToRun returns the roles a round of kind runs for pr, in the order
    of roles (the round's candidates in [[role]] order, judge included;
    nil = cfg.RolesFor(nil)): for KindContinue only the judge; otherwise
    the judge plus every role whose Runs allows it (config.Role.ShouldRun),
    where ranBefore is store.RoleRanBefore and requested whether requested names
    the role (by name or alias, case ignored). Only the first judge of roles is
    kept. The engine uses it to lay out panes and start agents, RunRound to pick
    the roles it runs, so both agree.


TYPES

type Agents interface {
	NewRun(ctx context.Context, pr store.PR, role agents.Role, kind string, round int) (store.Run, error)
	Submit(ctx context.Context, run store.Run, text string) error
	RunShell(ctx context.Context, pr store.PR, role config.Role, paneID, line, marker string, timeout time.Duration) (int, error)
	ReadRecent(ctx context.Context, s store.Session, lines int) (string, error)
	PreflightRole(ctx context.Context, role config.Role) error
	RolePrompt(role config.Role, promptKind string, data any) (string, error)
	ShellLine(role config.Role, d agents.ShellData) (string, error)
	// Per-model limits (see modelFallback).
	NoteModelLimit(ctx context.Context, s store.Session, h agents.Health) (agents.ModelLimit, error)
	FallbackModel(ctx context.Context, s store.Session, tried []string) (string, bool)
	SwitchModel(ctx context.Context, s store.Session, model, reason string) error
	FallbackPrompt(d agents.FallbackData) (string, error)
	// A reviewer whose time ran out (see round.timeUp): TimeUp types the
	// last call into its agent within its run, and after an interrupt the
	// message to stop its background work (round.stopReviewer);
	// BackgroundTasks counts the work a claude agent started in the
	// background during a run and left running (ok false: unknown).
	TimeUp(ctx context.Context, run store.Run, text string) error
	BackgroundTasks(ctx context.Context, run store.Run) (int, bool)
}
    Agents is the subset of *agents.Manager a round uses. The caller must have
    started the sessions of the roles that run (RolesToRun: EnsureWorkspace,
    EnsurePane, StartAgent) before RunRound; a role without a live session is
    reported as no_session.

type DeltaCheck struct {
	Lines int         `json:"lines"`
	Files []DeltaFile `json:"files"`
}
    DeltaCheck is a delta check's measure of the commits since the judge's last
    review: their changed code lines (the re-review threshold's measure) and
    their files.

type DeltaFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Binary bool   `json:"binary,omitempty"`
}
    DeltaFile is a file of a delta check. Binary: modified without a patch (an
    image, a font), counted as 0 lines.

type Git interface {
	RevParse(ctx context.Context, dir, ref string) (string, error)
	SwitchDetach(ctx context.Context, dir, ref string) error
	Status(ctx context.Context, dir string) (gitx.Status, error)
	MergeBase(ctx context.Context, dir, a, b string) (string, error)
}
    Git is the subset of *gitx.Client the checkout check after each stage (and
    its restore) uses, and a restart's check that a head is not an older one
    (MergeBase).

type GitHub interface {
	ReviewsWithMarker(ctx context.Context, owner, repo string, number int, marker string) ([]github.Review, error)
	ReviewREST(ctx context.Context, owner, repo string, number int, id int64) (github.RESTReview, error)
	DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) error
	UpdateReviewBody(ctx context.Context, owner, repo string, number int, reviewID int64, body string) error
}
    GitHub is the subset of *github.Client a round uses. It must act as
    the round's Identity (Env from Identity.Env), because DismissReview and
    UpdateReviewBody write.

type Keys interface {
	AgentSendKeys(ctx context.Context, target string, keys ...string) error
	PaneSendKeys(ctx context.Context, paneID string, keys ...string) error
}
    Keys interrupts a timed-out role (esc to an agent, ctrl+c twice to the
    judge, ctrl+c to a shell role's pane). *herdr.Client satisfies it.

type Pause struct {
	Kind   string    // usage_limit | login_required | overloaded
	Tool   string    // the agent kind to pause (config.Role.AgentKind): codex, claude, droid, ...
	Until  time.Time // usage_limit reset when the text names one; zero = apply the fallback backoff
	Detail string    // the matching pane line (redacted)
}
    Pause asks the engine to pause an agent kind.

type PreviousReview struct {
	ID          int64
	Event       string // review state: APPROVED | COMMENTED | CHANGES_REQUESTED (REQUEST_CHANGES accepted)
	SHA         string // commit the review was on
	SubmittedAt time.Time
	// Login posted it ("" = unknown), for the record.
	Login string
	// Manual: the reviewer posted it by hand (magnum approve /
	// request-changes): a round never dismisses it as its own stale review.
	Manual bool
	// Former: a former login of the PR posted it (RoundInput.FormerLogins).
	// It is history the round reads, never a review it dismisses with its
	// own credentials (the engine dismisses it as that identity). The engine
	// decides it, because only it can tell a user "x" from the App "x[bot]".
	Former bool
}
    PreviousReview is the reviewer's earlier review of the PR.

type ReadinessPlan struct {
	// ResetDB are the pool's reset_db commands when the slot's databases
	// carry another schema than the checkout's (slots.CheckSchema): run
	// before everything else, in order (Mutates), so they carry the PR's
	// schema. Empty when they carry it already.
	ResetDB []string
	// Loaded is called once every ResetDB command passed: the engine
	// records the schema the databases now carry. nil = nothing to record.
	Loaded func(ctx context.Context)
	// SchemaNote is magnum's line about the schema for the readiness event
	// and file: why ResetDB runs, or why it does not ("schema unchanged
	// since abc1234: no reset").
	SchemaNote string
	// ResetDBTimeout is the budget of the ResetDB commands together, apart
	// from Timeout: the release's (slots.ResetDBTimeout, also when 0). A
	// command still running when it ends is stopped (timeout); the reset_db
	// commands after it are skipped, the others still run.
	ResetDBTimeout time.Duration
	Prepare        []string // run next, in order (Mutates)
	Ready          []string // probes, run after them (exit 0 = ready)
	// Timeout is the budget of the step's other commands (prepare, ready and
	// the Ruby check), which starts once the reset is done; 0 =
	// config.DefaultReadyTimeout. A command still running when it ends is
	// stopped (timeout); the ones after it are skipped.
	Timeout time.Duration
	// Env overlays the daemon's environment: the slot's pool env or the
	// per-PR worktree env, what the slot's own setup commands get.
	Env map[string]string
}
    ReadinessPlan is a round's readiness step: the repository's commands
    (config.Readiness) and the slot's environment they run with.

type RelatedPR struct {
	Number   int        `json:"number"`
	URL      string     `json:"url"`
	State    string     `json:"state"` // open | merged
	Draft    bool       `json:"draft,omitempty"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
	HeadSHA  string     `json:"head_sha"` // the head its paths were read at
	// Overlap counts the changed paths both PRs share (related_ignore's
	// left out); Paths are the first twenty of them, sorted.
	Overlap int      `json:"overlap"`
	Paths   []string `json:"paths"`
	// FilesTruncated: it changes more files than its list holds (100), so
	// the overlap may be larger.
	FilesTruncated bool `json:"files_truncated,omitempty"`
	// Reviewed: magnum reviewed it; ReviewURL and ReviewVerdict are its last
	// posted review's (store.ReviewSummary), when magnum has its result.
	Reviewed      bool   `json:"reviewed"`
	ReviewURL     string `json:"review_url,omitempty"`
	ReviewVerdict string `json:"review_verdict,omitempty"`
	// FindingsOnPaths counts the findings magnum posted on it, over all its
	// rounds, on the shared paths (all of them, not only Paths).
	FindingsOnPaths int `json:"findings_on_paths"`

	// Has unexported fields.
}
    RelatedPR is one related PR of RelatedPRs.

type RelatedPRs struct {
	PR      string `json:"pr"`       // owner/repo#N
	HeadSHA string `json:"head_sha"` // the head under review, whose paths were compared
	// FilesTruncated: this PR changes more files than its list holds (100),
	// so a PR sharing only a later one is missing.
	FilesTruncated bool        `json:"files_truncated,omitempty"`
	MergedSince    time.Time   `json:"merged_since"` // merged PRs from then on count (related_lookback)
	Related        []RelatedPR `json:"related"`
	More           int         `json:"more,omitempty"` // related PRs past the cap of ten
}
    RelatedPRs is related.json.

type ReviewCommentLister interface {
	ReviewComments(ctx context.Context, owner, repo string, number int, reviewID int64) ([]github.ReviewComment, error)
}
    ReviewCommentLister is implemented by a GitHub client that lists a review's
    inline comments (GET .../reviews/{id}/comments). Optional: without it only
    the review body is checked for local paths.

type ReviewDeleter interface {
	DeletePendingReview(ctx context.Context, owner, repo string, number int, reviewID int64) error
}
    ReviewDeleter is implemented by a GitHub client that can delete a pending
    (unsubmitted) review: DELETE /repos/{o}/{r}/pulls/{n}/reviews/{id}. GitHub
    refuses it for a submitted review. Optional: without it a duplicate draft is
    only reported.

type RoleReport struct {
	Role    string // the role's name
	Kind    string // the role's agent kind (config.Role.AgentKind), whose pause a health status asks for; "" = none
	Capture string // the role's capture (config.CaptureFile, CaptureStdout)
	RunID   string
	Status  string // Report* or a health kind
	Path    string // the report file when Status is ok
	Detail  string
	Health  *agents.Health // pane classification when it explains the status
}
    RoleReport is what one non-judge role produced.

type RoundInput struct {
	PR       store.PR
	Repo     store.Repo // zero = looked up by PR.RepoID
	SlotPath string     // absolute checkout path
	Round    int        // 0 = the PR's latest run round + 1; KindContinue: pass the paused round
	Kind     string     // KindInitial | KindRereview | KindContinue | KindRecovery

	TargetSHA string
	BaseRef   string // PR base branch ("master"); roles get origin/<BaseRef> (RoleData/ShellData.BaseRef)
	BaseSHA   string // merge base of TargetSHA and the base (RoleData.BaseSHA, judge context)

	// Roles are the round's candidate roles in [[role]] order, the judge
	// included (Config.RolesFor of the PR's watch); nil = Config.RolesFor(nil).
	// RolesToRun picks the ones that run.
	Roles []config.Role
	// Requested names roles (names or aliases) asked for this round
	// (`magnum review --role`, --simplify): a runs = "first" or "manual"
	// role runs when requested.
	Requested []string

	Previous *PreviousReview  // rereview/recovery: the last review by the reviewer login (or a former one)
	History  []PreviousReview // recovery: earlier reviews (Previous is used when empty)
	// FormerLogins are the logins (REST form) the PR's reviews were posted
	// as before its identity migrated to the round's: their reviews and
	// threads are the round's own history (the reply contract's threads, the
	// judge's earlier findings). Verification still accepts only the round's
	// own login, so a former login's review never counts as the round's.
	FormerLogins []string
	Since        time.Time // rereview: read every comment since then
	ForcePushed  bool
	// BaseMerged (rereview): the commits since the previous review have a
	// merge commit (the base branch merged in), so previous..TargetSHA
	// carries the base branch's commits too: the re-review prompts compare
	// the PR's own diff before and after them instead
	// (RoleData/JudgeData.BaseMerged). ForcePushed wins over it.
	BaseMerged bool
	MovedFrom  string // agents.Workspace.MovedFrom
	// DeltaCheck (rereview, or recovery for a judge in a fresh session): the
	// round is a delta check: Roles hold the judge alone, which reviews the
	// small delta since its last review (JudgeData.DeltaCheck) at its
	// rereview effort; nil = an ordinary round.
	DeltaCheck *DeltaCheck

	DryRun bool // the judge posts nothing; GitHub is not consulted
	// Blind (magnum eval, with DryRun): the round replays a pinned head to
	// measure what a review finds, so its roles must not read what was
	// written about the PR later (reviews, comments, later commits) and the
	// judge takes the local diff, not GitHub's (the PR may have moved on or
	// closed). Rendered as `blind: true` in the prompts.
	Blind bool
	// PostMerge: GitHub merged the PR before magnum reviewed it (the engine
	// dispatched a `magnum review` of a merged PR). The judge reviews the
	// commits magnum missed as usual but posts a COMMENT whatever the
	// identity's or repository's events (JudgeData.PostMerge, rendered as
	// `post_merge: true`), and the round dismisses no earlier review: a
	// verdict after the merge blocks nothing.
	PostMerge bool

	// NotesPath is the repository notes file the roles read and the judge
	// rewrites (RoleData/JudgeData.NotesPath); "" = none.
	NotesPath string

	// Readiness is what the round runs in the checkout before the reviewers
	// (readiness.go); the zero value still runs the built-in Ruby check
	// when the checkout pins a Ruby. A continued turn runs nothing.
	Readiness ReadinessPlan

	// ContinueRunID is the paused judge run whose marker the continued turn
	// may already have posted (KindContinue); "" uses the new run's id.
	ContinueRunID string

	// MaxRestarts bounds how often the round starts its reviewers over on a
	// head the poller recorded while they ran, before the judge is prompted
	// (daemon.max_round_restarts); 0 = never. Switch must be set too.
	MaxRestarts int
	// DispatchedHead is the PR head the poller had recorded when the round
	// was dispatched (the one its checkout asked for): a later PR head other
	// than TargetSHA is a push. "" = PR.HeadSHA.
	DispatchedHead string
	// Switch checks a newer head out in the round's slot (the caller's
	// checkout path) for a restart and reports what is checked out now.
	Switch func(ctx context.Context, sha string) (Switched, error)

	// OwnPass ([pipeline] judge_own_pass = "parallel" for the PR's watch):
	// the judge does its own pass while the reviewers work (ownpass.go) and
	// judges their reports once both ended; false prompts it once, after
	// the reviewers. A judge alone (a delta check, a continued turn, a
	// round without reviewers) gets one prompt either way.
	OwnPass bool
	// Related is the PR's watch's related_lookback and related_ignore
	// (config.Config.RelatedFor): every judge prompt names related.json, the
	// repository's other PRs that change the same paths (related.go).
	Related config.Related
}
    RoundInput describes one round. The slot is already checked out at TargetSHA
    (re-read the slot after slots.Checkout: CheckedOutSHA is the round's
    target).

type RoundResult struct {
	Outcome    string
	Round      int
	ReportDir  string
	JudgeRunID string // the run whose id the review's marker carries
	// TargetSHA is the commit the round reviewed: RoundInput.TargetSHA, or
	// the head of its last restart (Restarts > 0).
	TargetSHA string
	Restarts  int // how often the reviewers started over on a newer head

	ReviewID     int64
	ReviewURL    string
	ReviewCommit string
	Event        string         // APPROVED | COMMENTED | CHANGES_REQUESTED (dry run: the planned event)
	Findings     map[string]int // P0..P3 from the judge's result file
	// HarnessUsed are the repository notes' harness files the judge said it
	// ran or read (its result's harness_used), as written: names relative to
	// notes_dir or paths under it.
	HarnessUsed []string

	Pause   *Pause
	Reports map[agents.Role]RoleReport // by role name: every non-judge role the round ran (or skipped as logged out)
	// OwnPass is how the judge's own pass ended (RoundInput.OwnPass; Path
	// set when it wrote its file); nil when the round prompted none.
	OwnPass *RoleReport
	Nudged  bool

	DismissedReviewID int64 // the stale CHANGES_REQUESTED review dismissed after posting
	Warnings          []string
	Error             string
}
    RoundResult is the round's verdict. The engine decides the PR state from
    Outcome (and Pause).

type Runner struct {
	Agents Agents
	GitHub GitHub       // as Identity; unused under RoundInput.DryRun
	Git    Git          // the checkout check after each stage, and restarts (nil = no check; every new head counts as a push)
	Exec   execx.Runner // the readiness step's commands, and the restore of a checkout a stage left modified
	Keys   Keys         // optional: interrupt timed-out roles (nil = leave them running)
	Store  *store.Store

	Identity identity.Source // who posts: login, kind, GH_CONFIG_DIR
	// SelfLogin is the user's own login (the poll identity). A PR authored by
	// it or by the reviewer login is self_authored (the judge posts COMMENT).
	SelfLogin string

	Config *config.Config
	Layout paths.Layout

	Clock        func() time.Time                                 // nil = time.Now
	Sleep        func(ctx context.Context, d time.Duration) error // nil = a timer honouring ctx
	PollInterval time.Duration                                    // 0 = DefaultPollInterval
	Logger       execx.Logger                                     // optional
	// AgentTag is agents.Deps.Tag of the agents this runner prompts ("eval"
	// for magnum eval); it leads the shell roles' pane titles too.
	AgentTag string
	// Mise is the mise executable the readiness step runs the pool's reset_db
	// commands through (slots.MiseExecArgs, as the release does); "" means
	// "mise" on PATH. It is the slots manager's (slots.Deps.Mise).
	Mise string
}
    Runner runs review rounds. One Runner serves one identity; it holds no
    per-round state and is safe for concurrent RunRound calls.

func (r *Runner) AppendToReview(ctx context.Context, owner, repo string, number int, reviewID int64, text string) error
    AppendToReview adds text as the last paragraph of review reviewID, which
    the runner's identity posted (editReview), above the footer magnum appended
    (footerMarker), so the footer stays the last paragraph. A body that carries
    text there already is left alone.

func (r *Runner) RunRound(ctx context.Context, in RoundInput) (RoundResult, error)
    RunRound runs one round and blocks until it ends. The error is non-nil
    only when Outcome is error (the round could not run or be evaluated) or
    stopped because ctx was cancelled; every other outcome, including timeouts
    and pauses, returns a nil error. Run rows created but never prompted are
    abandoned; runs already sent stay submitted/working when ctx is cancelled
    (they are observed, never re-sent).

type Switched struct {
	TargetSHA   string // the commit now checked out (a fetch may find a newer one); "" = the one asked for
	BaseSHA     string // its merge base with the base ("" = unknown: the round keeps the old one)
	ForcePushed bool   // the previous review's commit is not an ancestor of TargetSHA
	BaseMerged  bool   // the commits since the previous review have a merge commit (RoundInput.BaseMerged)
}
    Switched is the checkout after RoundInput.Switch.

type ThreadLister interface {
	ReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, error)
}
    ThreadLister is implemented by a GitHub client that lists a pull request's
    review threads with their comments (github.Client.ReviewThreads). Optional:
    without it a re-review names no threads file and the judge reads the replies
    itself.

```

## postreview

```text
package postreview // import "github.com/zhuravel/magnum/internal/postreview"

Package postreview is `magnum post-review`, the judge's posting tool.
The judge writes its review (event, body, inline comments) to a file; Run checks
it (the fields, then every inline anchor against the pull request's diff),
posts it once as the judge's identity (never twice: a review carrying the
run's marker counts as posted) and reads it back. It works from its Options,
gh and git in the checkout alone: it loads no config, opens no registry,
never contacts the daemon and writes no file.

CONSTANTS

const (
	StatusPosted           = "posted"            // posted and read back
	StatusAlreadyPosted    = "already_posted"    // a review carrying the run's marker exists; nothing posted
	StatusDryRun           = "dry_run"           // checked; nothing posted (Outcome.PlannedReview)
	StatusInvalid          = "invalid"           // the review file is wrong (Outcome.Problems)
	StatusInvalidAnchors   = "invalid_anchors"   // inline comments off the diff (Outcome.InvalidAnchors)
	StatusRejected         = "rejected"          // GitHub refused the review and none was created
	StatusError            = "error"             // anything else; Outcome.Message says what
	StatusReadbackMismatch = "readback_mismatch" // posted, but the review reads back differently
)
    Outcome statuses (Outcome.Status).

const (
	EventComment        = "COMMENT"
	EventRequestChanges = "REQUEST_CHANGES"
	EventApprove        = "APPROVE"
)
    Review events the judge may post.

const (
	SideRight = "RIGHT" // the head's lines: added or context
	SideLeft  = "LEFT"  // the base's lines: deleted or context
)
    Sides of an inline comment.

const FooterMarker = "<!-- magnum:footer -->"
    FooterMarker starts the footer magnum appends to a verified review
    (pipeline's footerMarker); a judge never writes it.

const MaxBody = 65536
    MaxBody is GitHub's limit for a review body and for a comment body,
    in characters.


FUNCTIONS

func RunMarker(runID, head string) string
    RunMarker is the line every review of run carries, on head: `<!--
    magnum:run=<run id> head=<first 7 of head> -->`.


TYPES

type BadAnchor struct {
	Index     int    `json:"index"` // in the review file's comments
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"start_line,omitempty"`
	Side      string `json:"side"`
	Why       string `json:"why"`
	// Valid are the line ranges of the file's hunks on that side
	// ("12-20"); empty for a file the pull request does not change.
	Valid []string `json:"valid"`
}
    BadAnchor is an inline comment GitHub would refuse: its line (or its
    start_line) is not a line of the pull request's diff on its side.

type Deps struct {
	GitHub GitHub
	Git    Git
	Dir    string
}
    Deps are Run's GitHub, as the judge's identity, and git in Dir, the checkout
    ("." = the current directory).

type Git interface {
	RevParse(ctx context.Context, dir, ref string) (string, error)
	MergeBase(ctx context.Context, dir, a, b string) (string, error)
	FileDiff(ctx context.Context, dir, base, head, path string) (string, error)
}
    Git is what Run reads from the checkout (*gitx.Client).

type GitHub interface {
	PullSHAs(ctx context.Context, owner, repo string, number int) (base, head string, err error)
	PullFiles(ctx context.Context, owner, repo string, number int) ([]github.FileDelta, bool, error)
	CompareFiles(ctx context.Context, owner, repo, base, head string) ([]github.FileDelta, error)
	Reviews(ctx context.Context, owner, repo string, number int) ([]github.Review, error)
	SubmitReview(ctx context.Context, owner, repo string, number int, r github.ReviewRequest) (github.RESTReview, error)
	ReviewREST(ctx context.Context, owner, repo string, number int, id int64) (github.RESTReview, error)
	ReviewComments(ctx context.Context, owner, repo string, number int, reviewID int64) ([]github.ReviewComment, error)
}
    GitHub is what Run reads and writes on GitHub (*github.Client, as the
    judge's identity).

type Options struct {
	Owner, Repo string
	Number      int
	HeadSHA     string // the reviewed commit: the review's commit_id
	RunID       string // the round's marker id
	Login       string // reviewer_login, REST form ("talkable[bot]")
	// FormerLogins posted this PR's reviews before its identity migrated: a
	// review of theirs carrying the marker counts as posted too.
	FormerLogins []string
	DryRun       bool // check everything, post nothing
	// LocalBase (a blind replay, with DryRun): check the anchors against
	// `git diff LocalBase HeadSHA` in the checkout and read nothing from
	// GitHub, whose pull request may have moved on since HeadSHA.
	LocalBase string
}
    Options are the run's facts, from the judge's <magnum> block.

func (o Options) Check() error
    Check reports what is wrong with o.

type Outcome struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"` // what went wrong (error, rejected), or what was found
	// Problems are the review file's faults (invalid).
	Problems []string `json:"problems,omitempty"`
	// InvalidAnchors are the comments off the diff (invalid_anchors).
	InvalidAnchors []BadAnchor `json:"invalid_anchors,omitempty"`
	// Unchecked are the indexes of comments no diff could check (a binary
	// or too large file without its commits in the checkout): kept, GitHub
	// decides.
	Unchecked []int `json:"unchecked_anchors,omitempty"`
	// MarkerAppended: the body lacked the run marker; it was added as its
	// last line.
	MarkerAppended bool `json:"marker_appended,omitempty"`

	ReviewID  int64  `json:"review_id,omitempty"`
	ReviewURL string `json:"review_url,omitempty"`
	Event     string `json:"event,omitempty"` // the event posted (or, already posted, the review's)
	// EventDowngraded: GitHub refused a self-verdict, so the review went
	// out as COMMENT.
	EventDowngraded bool   `json:"event_downgraded,omitempty"`
	State           string `json:"state,omitempty"` // as read back
	CommitID        string `json:"commit_id,omitempty"`
	Comments        *int   `json:"comments,omitempty"` // inline comments read back (dry run: planned)
	// Mismatches say how the review read back differs (readback_mismatch).
	Mismatches []string `json:"mismatches,omitempty"`
	// PlannedReview is the request a dry run would have sent.
	PlannedReview *github.ReviewRequest `json:"planned_review,omitempty"`
}
    Outcome is what Run printed: a status and its details. ExitCode maps it to
    the command's exit status.

func Run(ctx context.Context, d Deps, o Options, data []byte) Outcome
    Run posts the judge's review file data for the run o names: check the file,
    check its inline anchors against the pull request's diff, look for a review
    already carrying the run's marker, then post (or, in a dry run, plan) the
    review once and read it back.

func (o Outcome) ExitCode() int
    ExitCode is 0 for posted, already posted and a dry run, 2 when the judge
    must fix its review file, 1 otherwise.

func (o Outcome) Summary() string
    Summary is the outcome in one human line (ids and counts, no PR text).

type Review struct {
	Event    string                `json:"event"`
	Body     string                `json:"body"`
	Comments []github.DraftComment `json:"comments"`
}
    Review is the file the judge writes: the review to post. Comments use
    GitHub's fields (path, line, side, start_line, start_side, body).

func Parse(data []byte, o Options) (r Review, appended bool, problems []string)
    Parse decodes and checks the judge's review file for the run o names.
    It returns the review to post, with each comment's side (RIGHT unless
    given) and a multi-line comment's start side filled in and the run's marker
    appended as the body's last line when the body lacks it (appended reports
    that), or the problems found, each naming its field.

```

## reveal

```text
package reveal // import "github.com/zhuravel/magnum/internal/reveal"

Package reveal brings the herdr client to the front of the user's terminal:
focus the existing client when there is one, otherwise open a new tab or window
running `herdr --session <name>`.

It is a Go port of the terminal layer of the Raycast herdr extension
(terminal.ts, terminal-focus.ts, process-lookup.ts, terminal-config.ts).
herdr clients are found through `ps -axo pid=,tty=,args=` (never pgrep,
see docs/spikes.md probe 8) and matched to a terminal tab by tty: iTerm2 and
Terminal.app through AppleScript, Ghostty through a temporary marker title
(`herdr terminal title set`), WezTerm through `wezterm cli`. A custom launcher
template and a generic `open -a <App>` cover everything else.

Every subprocess (ps, osascript, open, wezterm, herdr, the custom launcher)
runs through execx.Runner. Read-only lookups are not marked Mutates; anything
that touches the UI (focus, launch, title set, activate) is, so DryRun prints it
instead of running it. A DryRun therefore yields Unavailable focus results and
an "activated" outcome, never a false "focused".

The package never changes herdr state: it does not start servers, create
workspaces or panes, and only sets (then clears) the outer terminal title for
the Ghostty flow. Focusing a pane inside herdr (`herdr agent focus`) is the
caller's job and happens before Reveal.

TYPES

type Action string
    Action says what Reveal did.

const (
	ActionFocused   Action = "focused"   // focused an existing client
	ActionLaunched  Action = "launched"  // opened a new client tab/window
	ActionActivated Action = "activated" // could only bring the terminal app forward
)
type AutomationError struct {
	App string // the application the script controls: iTerm, Terminal or Ghostty
}
    AutomationError is a focus script macOS refused to run (osascript's error
    -1743, "Not authorized to send Apple events"): the app that runs magnum may
    not control App under Privacy & Security → Automation.

func (e *AutomationError) Error() string

type FocusResult string
    FocusResult is the outcome of looking for an existing herdr client.

const (
	// Focused: an existing client's tab/window was brought to the front.
	Focused FocusResult = "focused"
	// Missing: the terminal is scriptable and confirmed there is no client in it.
	Missing FocusResult = "missing"
	// Unavailable: the answer could not be determined (process list failed,
	// osascript denied or timed out, terminal cannot be scripted).
	Unavailable FocusResult = "unavailable"
)
type Kind string
    Kind is the family of terminal application magnum reveals herdr in.

const (
	KindTerminal Kind = "terminal" // macOS Terminal.app
	KindITerm    Kind = "iterm2"
	KindGhostty  Kind = "ghostty"
	KindWezTerm  Kind = "wezterm"
	KindCustom   Kind = "custom"  // user-supplied launcher template
	KindGeneric  Kind = "generic" // `open -a <App>` only
)
    The supported terminal kinds. Anything DetectKind does not recognise
    (including Muxy) is KindGeneric: the app can be activated but not scripted.

func DetectKind(app string) Kind
    DetectKind maps config.Terminal.App to a Kind. Matching is case-insensitive
    and tolerant of a ".app" suffix or a bundle id. An empty name means the
    built-in Terminal.app, as in the Raycast extension.

type Options struct {
	// NewWindow opens a new window instead of a tab in the current one.
	NewWindow bool
}
    Options tunes Launch and Reveal.

type Outcome struct {
	Action  Action `json:"action"`
	Kind    Kind   `json:"kind"`
	Session string `json:"session"`
	// Detail carries the reason when the result is a fallback (for example why
	// focusing was unavailable).
	Detail string `json:"detail,omitempty"`
}
    Outcome describes what Reveal did, for `magnum open` output and --json.

func Reveal(ctx context.Context, run execx.Runner, cfg config.Terminal, herdrBin string, opts Options) (Outcome, error)
    Reveal is New(run, cfg, herdrBin).Reveal(ctx, opts).

func (o Outcome) String() string
    String renders the outcome as one human-readable line.

type Revealer struct {
	// Has unexported fields.
}
    Revealer reveals herdr in one configured terminal. Build it with New.

func New(run execx.Runner, cfg config.Terminal, herdrBin string) *Revealer
    New builds a Revealer from config.Terminal. herdrBin is the herdr executable
    (typed into the new terminal tab and used for the Ghostty title calls);
    empty means "herdr" on PATH. An empty session means "default".

func (r *Revealer) FocusExisting(ctx context.Context) (FocusResult, error)
    FocusExisting looks for a herdr client of the session in the configured
    terminal and focuses it. The error accompanies Unavailable and says why.

func (r *Revealer) Launch(ctx context.Context, opts Options) error
    Launch opens a new herdr client for the session in the configured terminal:
    iTerm2 (new tab, or window with opts.NewWindow) and Terminal.app (do
    script), Ghostty (new surface configuration), WezTerm (`cli spawn`),
    a custom launcher template, or `open -a <App>` for generic terminals.
    A non-empty terminal.launcher wins over the app kind, as in the Raycast
    extension.

func (r *Revealer) Reveal(ctx context.Context, opts Options) (Outcome, error)
    Reveal focuses the existing herdr client when there is one. When the
    terminal confirms there is none it launches a new client (a tab, or a window
    with opts.NewWindow). When focus cannot be determined it only brings the
    terminal app forward (opening a second client could duplicate one it cannot
    see); a custom launcher has no app to raise, so it launches.

```

## slots

```text
package slots // import "github.com/zhuravel/magnum/internal/slots"

Package slots manages the checkouts magnum reviews in: the Talkable pool slots
(~/Projects/talkable.reviewN, provisioned once with bin/worktree-setup and
reused) and per-PR worktrees for small repositories, next to the repository's
main clone whatever it is named (~/Projects/<owner>-<name>__worktrees/pr-N).

Every operation is a sequence of steps.Step side effects, each idempotent or
prechecked, so a crash in the middle is repaired by calling the same operation
again. Slot state moves through compare-and-set store transitions; the slot row
is written before any side effect so the registry always knows about a directory
or database before it exists.

Heavy commands (setup, dependency installs, schema reloads, teardown) run
through `mise -C <slot> exec -- env <slot env> /bin/sh -c <cmd>` and are
serialized by the Manager, so at most one runs at a time.

CONSTANTS

const (
	HookPostCreate = "post-create"
	HookPostStart  = "post-start"
	HookPreRemove  = "pre-remove"
	// HookSetup and HookTeardown label [[repo]] commands.
	HookSetup    = "setup"
	HookTeardown = "teardown"
)
    Worktrunk hook types magnum runs (other tables in wt.toml are ignored).

const (
	SetupTimeout    = 45 * time.Minute
	DepsTimeout     = 30 * time.Minute
	ResetDBTimeout  = 30 * time.Minute
	TeardownTimeout = 30 * time.Minute
)
    Timeouts of the heavy commands.

const (
	HoldPinned            = "pinned"
	HoldForeignAgent      = "foreign_agent"
	HoldForegroundProcess = "foreground_process"
	HoldHeadDrift         = "head_drift"
	HoldUnpushed          = "unpushed_commits"
	HoldDirtyWorktree     = "dirty_worktree"
)
    Hold reasons returned in ErrHold. HoldHeadDrift, HoldUnpushed and
    HoldDirtyWorktree are also persisted as slots.hold_reason (the slot stays
    held until Unpin); the others are transient and only refuse the current
    operation.

const AgentPrefix = "mg-"
    AgentPrefix starts the name of every agent magnum starts; the guard treats
    any other agent as a human's.

const DefaultCloneRoot = "~/Projects"
    DefaultCloneRoot is used when a watch has no clone_root.

const MarkerFile = "tmp/.worktree-db-slug"
    MarkerFile is written by bin/worktree-setup with the slot's database slug.

const MiseLocal = ".mise.local.toml"
    MiseLocal is the per-checkout mise file magnum renders into every slot.

const SchemaFile = "db/schema.rb"
    SchemaFile is read for the `define(version: …)` the slot's dev database must
    match after a release.

const SlotLogMax = 2 << 20
    SlotLogMax caps each log transcript writes (slot-<name>.log,
    provision-<name>.log, perpr-<slug>.log): the write that would push one past
    it first moves it to <name>.1, replacing the previous one, as daemon.log
    rotates. A seed that prints its SQL fills several MB on every reset.

const WTConfigFile = ".config/wt.toml"
    WTConfigFile is worktrunk's project config, relative to the main clone.


VARIABLES

var (
	// ErrNoFreeSlot: the pool has no free, unpinned, unheld slot.
	ErrNoFreeSlot = errors.New("slots: no free slot")
	// ErrBroken: the slot was moved to broken (needs Repair).
	ErrBroken = errors.New("slots: slot is broken")
	// ErrLowDisk: provisioning refused below pool.min_free_disk_gb.
	ErrLowDisk = errors.New("slots: not enough free disk space")
	// ErrOriginMismatch: an existing clone's origin is not the expected repository.
	ErrOriginMismatch = errors.New("slots: clone origin does not match the repository")
	// ErrVerify: a post-condition (HEAD, marker file, databases) did not hold.
	ErrVerify = errors.New("slots: verification failed")
)
var (
	// ReleaseStates can be released: claimed and held start a release,
	// releasing and dirty_schema resume one.
	ReleaseStates = []string{store.SlotClaimed, store.SlotHeld, store.SlotReleasing, store.SlotDirtySchema}
	// RemoveStates are the pool slot states Remove starts from without force
	// (removing resumes; removed is a no-op).
	RemoveStates = []string{store.SlotFree, store.SlotBroken, store.SlotLost, store.SlotProvisioning}
	// ForceRemoveStates are the extra pool slot states Remove starts from
	// with force. Busy never is.
	ForceRemoveStates = []string{store.SlotClaimed, store.SlotHeld, store.SlotReleasing, store.SlotDirtySchema}
	// RemovePRStates are the per-PR worktree states RemovePRWorktree starts
	// from (removing resumes; busy never is).
	RemovePRStates = []string{store.SlotProvisioning, store.SlotClaimed, store.SlotHeld, store.SlotReleasing,
		store.SlotFree, store.SlotBroken, store.SlotLost}
)
    Slot states each operation accepts; cleanup plans with the same lists.
    Callers that extend one must slices.Clone it first.

var DefaultStripEnv = []string{"GITHUB_PERSONAL_ACCESS_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}
    DefaultStripEnv is always dropped from a checkout's rendered
    .mise.local.toml, a pool slot's (in addition to [pool] strip_env) as well
    as a per-PR worktree's (in addition to [[repo]] strip_env): a GitHub token
    there would override the reviewing identity's gh selection in the agents'
    panes, and the checkout runs PR-controlled code.

var ErrHook = errors.New("slots: per-PR hooks")
    ErrHook: a per-PR worktree's hook file or template cannot be used.

var ErrMiseLocal = errors.New("slots: cannot render .mise.local.toml")
    ErrMiseLocal: the main clone's .mise.local.toml cannot be rendered safely.

var LockFiles = []string{"Gemfile.lock", "pnpm-lock.yaml"}
    LockFiles are hashed into slots.lock_sha; a change reruns pool.PostCheckout.

var WorkspaceNameVars = []string{
	"CONDUCTOR_WORKSPACE_NAME", "EMDASH_TASK_NAME", "SUPERSET_WORKSPACE_NAME",
	"COMMANDER_CONTEXT_NAME", "CLAUDE_CODE_WORKTREE_NAME", "WM_HANDLE",
}
    WorkspaceNameVars are the workspace-name variables bin/worktree-setup and
    bin/worktree-archive read before WT_BRANCH; a per-PR worktree blanks them
    (the pool does the same through [pool.env]) so an orchestrator's value
    inherited from the daemon's environment cannot pick other databases.


FUNCTIONS

func CloneRoot(watch config.Watch) string
    CloneRoot is the watch's expanded clone_root (DefaultCloneRoot when unset).

func CopyIntoCheckout(src, checkout, rel string) error
    CopyIntoCheckout copies src (a file outside the checkout, e.g. in the main
    clone) to rel inside checkout through WriteCheckoutFile, keeping src's
    permission bits; a missing src is skipped (nil).

func DBListPrefixes(pools ...config.Pool) (prefixes, bad []string)
    DBListPrefixes returns the distinct name prefixes ("<base>__") of the pools'
    database templates, sorted: talkable_development__ and talkable_test__
    for talkable_development__{slug} and talkable_test__{slug}. bad lists the
    templates that do not end in "__{slug}" (config validation should refuse
    them); they are left out of prefixes.

func DBPrefix(pool config.Pool) string
    DBPrefix is the common start of the pool's database templates before "{"
    (talkable_), "" when there are none or they share nothing.

func DBSlug(name string) string
    DBSlug sanitizes a workspace name the way talkable's database.yml
    does (WORKTREE_NAME.gsub(/[^a-zA-Z0-9_]/, "_")[0, 30]): magnum-pr-7 →
    magnum_pr_7.

func DropGuard(pool config.Pool) mysqlx.Guard
    DropGuard is the one drop guard for a pool's own databases, shared by slots,
    inventory and cleanup: Prefix is the common start of the pool's database
    templates before "{" (talkable_ for talkable_development__{slug} and
    talkable_test__{slug}; a single template's text before "{"), and AllowRegexp
    is slot_name with {n} as [0-9]+ (^review[0-9]+$), so only magnum's own slot
    slugs are ever droppable. A pool without database templates, with no common
    prefix or with a slot_name lacking {n} gets the zero Guard, which refuses
    everything.

func LazySchema(pool config.Pool) bool
    LazySchema reports whether pool's slots reload their databases only when a
    round's checkout needs another schema (CheckSchema, before the reviewers)
    and keep them through the release: the pool names schema_paths and reset_db,
    and reset_db_on_schema_change is on. Otherwise the release loads the base
    schema again, as before.

func ListPoolDatabases(ctx context.Context, c DBLister, pools ...config.Pool) (dbs []mysqlx.Database, bad []string, err error)
    ListPoolDatabases lists the per-worktree databases of pools: every schema
    named <prefix><slug> for a prefix of DBListPrefixes, ordered by name.
    A client that is a PrefixLister lists exactly those; any other client's
    ListSuffixed is filtered to them, and a prefix ListSuffixed cannot see is an
    error instead of a silently empty answer. Templates without "__{slug}" are
    skipped and returned in bad for the caller to report. No pool template at
    all lists nothing (and is not an error).

func MiseExecArgs(dir string, env map[string]string, script string) []string
    MiseExecArgs builds `-C dir exec -- env K=V… /bin/sh -c script` (keys
    sorted; blank values blank the variable): the arguments of the mise
    executable that run a pool script as the release, the provisioning and
    `magnum open` do, with the real Ruby first on PATH (no shim that would
    re-apply the checkout's .mise.local.toml [env] over env).

func PRSlotName(repo string, number int) string
    PRSlotName is the slot name of a per-PR worktree: "owner/name#N".

func PRSlug(number int) string
    PRSlug is the workspace name of per-PR worktree number: magnum-pr-<N>.
    It is WT_BRANCH for the worktree's hooks and agents.

func PRWorktreePath(mainClone string, number int) string
    PRWorktreePath is the per-PR worktree of a main clone:
    <clone>__worktrees/pr-<N>, next to the clone whatever it is named
    (~/Projects/<owner>-<name>__worktrees/pr-7).

func PRWorktreePaths(watch config.Watch, repo string, number int) (mainClone, path string)
    PRWorktreePaths returns the default layout for repo "owner/name" when
    no clone exists yet: the main clone <clone_root>/<name> and its per-PR
    worktree. CreatePRWorktree finds an existing clone first (FindClone).

func PerPREnv(rc *config.Repo, number int, path, clone string) map[string]string
    PerPREnv is the environment of per-PR worktree number (path, main clone
    clone) for its hooks and its agents: WT_BRANCH=PRSlug(number), every
    WorkspaceNameVars entry blank, then the [[repo]] env (rc may be nil) on top.

func RenderMiseLocal(src []byte, strip []string, set map[string]string) ([]byte, error)
    RenderMiseLocal renders a checkout's .mise.local.toml from the main clone's
    file src. src is parsed as TOML: its `env` value must be absent or a table,
    however it is spelled ([env], [env.KEY] sub-tables, dotted keys,
    a root-level `env = { … }` inline table); every key in strip and every
    key in set is deleted from that table whatever its value (a string,
    an inline table, a sub-table such as { value = "…" }), then each key of set
    is set to its string value. The whole tree is encoded back after miseMarker
    with sorted keys, so the output is deterministic and rendering it again
    with the same arguments yields the same bytes. Comments of src are dropped,
    commented-out secrets with them, and so is the order of its keys. A src
    that does not parse, or whose `env` is not a table ([[env]], a string), is
    ErrMiseLocal. An empty src renders the marker and an [env] table with set.

func SlotDBSlug(sl store.Slot) string
    SlotDBSlug is the database slug of a slot: slots.db_slug, else the slot name
    sanitized like a workspace name (DBSlug: review3 stays review3, owner/name#7
    becomes owner_name_7). Slots, inventory and cleanup all use it.

func WriteCheckoutFile(checkout, rel string, data []byte, perm fs.FileMode) (err error)
    WriteCheckoutFile writes data to rel inside checkout atomically (temp file
    in the same directory, then rename), with permission perm, creating parent
    directories, through an os.Root on checkout: a rel that is absolute or
    escapes (.., or a symlink in a parent pointing outside) is an error and
    nothing is written outside checkout. A symlink at rel itself is replaced,
    not followed.

    The checkout holds PR-controlled files (tracked symlinks included), so every
    write magnum makes into one goes through here.


TYPES

type DBLister interface {
	ListSuffixed(ctx context.Context) ([]mysqlx.Database, error)
}
    DBLister is the listing half of *mysqlx.Client.

type Deps struct {
	Store *store.Store
	Run   execx.Runner
	// Git defaults to gitx.New(Run). Share one client with other packages so
	// fetches into the same clone stay serialized.
	Git   *gitx.Client
	MySQL MySQL // required for pool slots
	// Snapshot feeds the guard (e.g. (*herdr.Client).Snapshot). Nil skips the
	// herdr checks.
	Snapshot func(ctx context.Context) (herdr.Snapshot, error)
	// ProcessInfo (e.g. (*herdr.Client).PaneProcessInfo) lets the guard see a
	// non-shell foreground process in panes without an agent. Optional.
	ProcessInfo func(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	Layout      paths.Layout
	// Mise is the mise executable; "" means "mise" on PATH.
	Mise string
	// LookPath finds Mise for per-PR worktree hooks (nil: exec.LookPath);
	// without it they run through /bin/sh. Pool scripts always use mise.
	LookPath func(file string) (string, error)
	// Repos are the [[repo]] blocks (config.Config.Repos): setup, teardown,
	// copy_files, strip_env and env of per-PR worktrees.
	Repos []config.Repo
	// FreeDiskBytes returns the free bytes of the filesystem holding path;
	// nil uses statfs.
	FreeDiskBytes func(path string) (uint64, error)
	// Now defaults to time.Now.
	Now func() time.Time
	// Log receives one line per decision and dry-run plan (optional).
	Log execx.Logger
	// DryRun makes every mutating method log what it would do and return
	// without touching git, MySQL, files or the store. Claim returns the slot
	// it would claim.
	DryRun bool
}
    Deps are the Manager's collaborators.

type ErrHold struct {
	Reason string // one of the Hold* constants, or a manual hold_reason
	Detail string // what was seen (pane, process, shas)
}
    ErrHold is returned when a guard refuses to touch a slot because a human
    is (or may be) using it. Match it with errors.As(err, &slots.ErrHold{}) or
    AsHold. A slot.hold_reason set by hand is reported with that reason.

func AsHold(err error) (ErrHold, bool)
    AsHold reports whether err carries an ErrHold.

func (e ErrHold) Error() string

type Hook struct {
	Type    string // HookPostCreate, HookPostStart, HookPreRemove, HookSetup or HookTeardown
	Name    string // the table key ("setup"); the type for a bare string; "1", "2"… for [[repo]] commands
	Command string
}
    Hook is one command of a hook table.

type Manager struct {
	// Has unexported fields.
}
    Manager runs slot operations. Safe for concurrent use; heavy commands are
    serialized.

func New(d Deps) *Manager
    New returns a Manager.

func (m *Manager) Adopt(ctx context.Context, pool config.Pool, path string) (store.Slot, error)
    Adopt registers an existing checkout at path as a free pool slot:
    path must be pool.Path(n) for some n, a worktree of the main clone,
    its tmp/.worktree-db-slug must say pool.Slot(n), and all its databases must
    exist (ErrVerify otherwise). An already registered live slot is ErrConflict;
    a removed, lost or broken row with that name is revived (its PR and open
    assignment released with reason "adopted").

func (m *Manager) CheckSchema(ctx context.Context, slot store.Slot, pool config.Pool) (SchemaCheck, error)
    CheckSchema compares the schema a pool slot's checkout needs with the one
    its databases carry: a reload is needed when the fingerprints differ or none
    was recorded. A match is checked once more against the development database,
    whose MAX(schema_migrations.version) must be the version the checkout's
    db/schema.rb declares (a migration someone ran by hand, or an agent,
    moves it; a failed read counts as a mismatch). Read-only.

func (m *Manager) Checkout(ctx context.Context, slot store.Slot, pr store.PR, pool config.Pool, targetSHA string) error
    Checkout puts pr's head into a claimed or held slot that belongs to pr.
    Steps (subject "slot:<name>:pr:<N>:<sha7>", a new generation on every call;
    each step is idempotent): fetch (refs/pull/N/head → refs/magnum/pr/N,
    plus origin/<base> for pool slots; when the fetched head differs from
    targetSHA the fetched head is used and a slot.head_moved event is written),
    guard, switch (detached at refs/magnum/pr/N, discarding tracked changes the
    guard took for magnum's residue after a slot.discarded event names them;
    the target commit is written to the kv store first and checked_out_sha
    right after, so a retry after a failure in a later step recognizes
    HEAD as magnum's own switch, not a human's commit), and for pool
    slots render_mise, deps (pool.post_checkout through mise exec, 30m,
    when the Gemfile.lock/pnpm-lock.yaml hash differs from lock_sha) and schema
    (dirty_schema = 1 when the PR changes pool.schema_paths since its merge base
    with origin/<base>); finally verify (HEAD == fetched head → checked_out_sha,
    last_used_at). The slot state is left as it was: moving claimed → busy after
    the prompt is acked is the engine's job. Read the slot again for the sha
    actually checked out. pool is ignored for per-PR slots.

func (m *Manager) Claim(ctx context.Context, pr store.PR, pool config.Pool) (store.Slot, error)
    Claim hands a free pool slot to pr: store.FreeSlots (the slot that last
    held the PR first, then least recently used) and store.ClaimSlot for each
    candidate until one succeeds (PR → claiming, slot → claimed, open assignment
    with the pool's database names). A lost race moves on to the next slot;
    a PR that is not claimable is ErrConflict; no slot left is ErrNoFreeSlot.
    The returned row is the claimed slot.

func (m *Manager) ClearPin(ctx context.Context, slot store.Slot) error
    ClearPin removes the slot's pin and nothing else: a hold a guard persisted
    (hold_reason: a person's changes, unpushed commits, a moved HEAD) stays
    until Unpin, so the guard keeps refusing the slot. A review request uses
    it on the slot `magnum open` pinned: the person's work stays protected,
    the pin alone no longer holds the review.

func (m *Manager) CreatePRWorktree(ctx context.Context, watch config.Watch, repo string, pr store.PR, targetSHA string) (store.Slot, error)
    CreatePRWorktree checks pr out into its own detached worktree for a
    repo without a pool. The main clone is found with MainClonePath (an
    existing clone under any of the usual names, else the directory to
    clone into) and the worktree goes next to it (<clone>__worktrees/pr-N).
    The slot row (kind per_pr, state provisioning) is written first; then
    steps of subject "slot:owner/name#N": clone (only when no clone was found,
    see ensureClone; an existing directory's origin must be owner/name,
    else ErrOriginMismatch), fetch (refs/pull/N/head → refs/magnum/pr/N;
    a head different from targetSHA is used and reported as slot.head_moved),
    worktree_add (--detach at refs/magnum/pr/N, skipped when listed at the
    fetched head; a listed worktree at another commit, left by an earlier
    attempt, is switched to it after Guard), verify (HEAD == fetched head),
    render_mise (the main clone's .mise.local.toml rendered with the per-PR env,
    [[repo]] copy_files), setup (the [[repo]] setup commands or the main clone's
    .config/wt.toml [post-create]/[post-start] hooks as WT_BRANCH=magnum-pr-<N>,
    see hooks.go; a failure moves the slot to broken and returns ErrBroken) and
    claim (slot → claimed with pr_id, checked_out_sha and db_slug when setup
    ran, then its open assignment). The PR's state is not changed: move it to
    claiming before calling. Calling it again for a slot that already holds pr
    returns that slot (at its recorded paths), opening its assignment if a crash
    between the claim's two writes left none; a removed, broken or lost row for
    the same PR is reused (its old assignment closed) and its steps start over.

func (m *Manager) EnsureSchema(ctx context.Context, slot store.Slot, pool config.Pool) (string, error)
    EnsureSchema gives a pool slot handed to a person (magnum open) the
    databases of its checkout, as a round's readiness step does: when
    CheckSchema finds they carry another schema, the pool's reset_db runs
    through mise exec (ResetDBTimeout, the slot's log) and the new schema is
    recorded. It returns what it did for the person. A failed reload is an error
    and leaves the schema unknown. A pool that is not LazySchema, or a per-PR
    worktree, runs nothing: its released slots carry the base schema.

func (m *Manager) ForgetSchema(ctx context.Context, slot store.Slot) error
    ForgetSchema marks the schema of the slot's databases unknown, before a
    reload that may stop half-way: the next round reloads them again unless
    RecordSchema follows.

func (m *Manager) Guard(ctx context.Context, slot store.Slot) error
    Guard refuses to let magnum touch a slot a human may be using. It returns an
    ErrHold when:
      - the slot is pinned (HoldPinned) or has a hold_reason (that reason);
      - herdr shows an agent magnum did not start (name not starting with
        AgentPrefix), idle or not, in a pane whose cwd or foreground cwd
        is the slot or below it or in the herdr workspace of the slot's PR
        (HoldForeignAgent), or a pane in the slot without an agent whose
        foreground process is not its shell (HoldForegroundProcess). Panes of
        magnum's own live sessions are ignored, and so is the process of any
        pane in their workspaces;
      - the slot's HEAD is not the recorded checked_out_sha (HoldHeadDrift) or,
        with no recorded sha, HEAD (or, for a pool slot, its placeholder branch,
        which Release resets wherever HEAD is) carries commits on no remote or
        magnum ref (HoldUnpushed). A HEAD equal to the commit an interrupted
        Checkout was switching to is magnum's own and is recorded instead;
      - the working tree has changes to tracked files and a human was there
        (HoldDirtyWorktree): a foreign agent or process as above, or the PR's
        human_active_at after the slot's last checkout (last_used_at), which the
        agents package sets when someone types into the PR's magnum panes.

    Without such evidence changes are magnum's own residue (tests rewriting
    db/schema.rb, bundle install touching Gemfile.lock, copy_files the repo
    does not ignore): Guard passes, and the step that discards them records a
    slot.discarded event naming them (noteDiscard).

    Drift, unpushed commits and held changes are persisted as hold_reason,
    so the slot stays held until Unpin; live holds (agent, process) on a clean
    tree are transient. A herdr snapshot failure is returned as a plain error
    (not a hold): the caller retries later. Git checks are skipped when the slot
    directory is missing.

func (m *Manager) HumanEvidence(ctx context.Context, sl store.Slot) (string, error)
    HumanEvidence explains why changes in slot sl's tree would be a human's
    rather than magnum's residue: its PR's human_active_at is after the slot's
    last checkout. "" when nothing says so. Live evidence (a human's agent or
    process in the slot) is Guard's; read-only, for planners that want to show
    what a release or removal would hold on.

func (m *Manager) MainClonePath(ctx context.Context, watch config.Watch, repo string) (mainClone string, found bool, err error)
    MainClonePath is where repo "owner/name" lives under the watch's clone_root:
    the clone gitx FindClone discovers (<name>, <owner>-<name>, <owner>_<name>,
    else any directory whose origin is the repository) or, when there is
    none (gitx.ErrNoClone), the directory a clone goes into: <root>/<name>,
    or <root>/<owner>-<name> when a folder named <name> exists but is not a git
    repository. found reports an existing clone.

func (m *Manager) NextSlotNumber(ctx context.Context, pool config.Pool) (int, error)
    NextSlotNumber returns the lowest slot number whose name is not used by a
    non-removed slot and whose path does not exist (an unregistered directory,
    or a removed slot's path that is back, is a human's, not magnum's to take
    over). It does not check pool.max.

func (m *Manager) Pin(ctx context.Context, slot store.Slot) error
    Pin marks the slot pinned: no automatic claim, checkout, release or removal
    until Unpin.

func (m *Manager) ProvisionPool(ctx context.Context, pool config.Pool, n int) error
    ProvisionPool provisions pool slot number n (name pool.Slot(n),
    path pool.Path(n)). The slot row (state provisioning, kind pool,
    db_slug and placeholder_branch = name) is written first, then the
    steps of subject "slot:<name>" run: worktree_add (fetch origin/<base>,
    `git worktree add --no-track -b <name> <path> origin/<base>`, skipped when
    the path is already a worktree), render_mise, setup (each pool.setup command
    through mise exec, 45m, logged to logs/provision-<name>.log), verify_marker
    (tmp/.worktree-db-slug == name), verify_dbs (every pool database exists) and
    mark_free (provisioning → free, lock_sha, slot_databases).

    A slot that is already provisioned is left alone (nil). A provisioning
    slot resumes at the first step without an ok row; a removed or broken one
    starts over. A new or removed slot whose path already exists is refused
    (ErrConflict): that directory is not magnum's (adopt it instead). Failures
    leave the slot provisioning with last_error, except failed verifications,
    which move it to broken (ErrVerify). Below pool.min_free_disk_gb nothing is
    added (ErrLowDisk).

func (m *Manager) RecordSchema(ctx context.Context, slot store.Slot, c SchemaCheck) error
    RecordSchema stores c, CheckSchema's answer, as the schema the slot's
    databases carry, once its reload succeeded.

func (m *Manager) Release(ctx context.Context, slot store.Slot, pool config.Pool, reason string) error
    Release hands a claimed or held pool slot back to the pool. Steps (subject
    "slot:<name>:release"): guard (Guard: pins, holds, a human's agent or
    process, HEAD drift, tracked changes a human made), then the slot moves to
    releasing, fetch_base, reset (placeholder branch → origin/<base>, tracked
    changes discarded after a slot.discarded event, checked_out_sha cleared),
    delete_ref (refs/magnum/pr/N), render_mise, deps, schema_check (resetSchema:
    a LazySchema pool keeps the databases unless MAX(schema_migrations.version)
    of the development database differs from the version of the schema recorded
    for them; otherwise when dirty_schema is set or that version differs from
    db/schema.rb at HEAD: the slot moves to dirty_schema and pool.reset_db
    runs through mise exec, 30m; a second failure in a row moves the slot to
    broken and returns ErrBroken) and mark_free (store.ReleaseSlot: free,
    pr_id cleared, assignment closed with reason). A releasing or dirty_schema
    slot resumes after its last completed step, after Guard ran again:
    whatever a human did since the last attempt refuses the release before its
    next destructive step. Per-PR slots are removed instead (RemovePRWorktree
    semantics).

func (m *Manager) Remove(ctx context.Context, slot store.Slot, pool config.Pool, force bool) error
    Remove tears a pool slot down: steps of subject "slot:<name>:remove" are
    guard (skipped with force: Guard, untracked files counted as changes since
    the removal deletes them), teardown (pool.teardown through mise exec,
    skipped when the directory is gone), verify_dbs_gone (leftover pool
    databases dropped with the pool's drop guard, slot_databases marked
    dropped), worktree_remove (--force with force or over magnum's residue,
    see removeWorktree), branch_delete (placeholder, -D), worktree_prune and
    mark_removed. RemoveStates can be removed; with force also ForceRemoveStates
    (never busy). A removing slot resumes after the guard ran again; a removed
    one is a no-op. Per-PR slots are handed to RemovePRWorktree.

func (m *Manager) RemovePRWorktree(ctx context.Context, slot store.Slot, force bool) error
    RemovePRWorktree removes a per-PR worktree: steps of subject
    "slot:owner/name#N:remove" are guard (skipped with force: Guard, untracked
    files counted as changes), teardown (the [[repo]] teardown commands or the
    main clone's .config/wt.toml [pre-remove] hooks as WT_BRANCH=magnum-pr-<N>;
    skipped when the directory is gone, failures logged and tolerated),
    worktree_remove (--force with force or over magnum's residue,
    see removeWorktree), delete_ref (refs/magnum/pr/N), worktree_prune and
    mark_removed (pr_id cleared, assignment closed). RemovePRStates can be
    removed (a busy slot is ErrConflict); a removing one resumes after the guard
    ran again; a removed one is a no-op.

func (m *Manager) Repair(ctx context.Context, slot store.Slot, pool config.Pool) error
    Repair re-runs provisioning (render, setup, verification) for a free,
    broken, provisioning or lost slot; a missing worktree is added again.
    The slot ends free, its PR and open assignment released (reason "repaired").
    A pinned or held slot is refused (ErrHold): Unpin it first.

func (m *Manager) Reserve(ctx context.Context, pr store.PR, pool config.Pool) (store.Slot, error)
    Reserve hands a free pool slot to pr without touching the PR's state,
    for a human who wants the PR's checkout back (magnum open on a parked PR):
    like Claim but through store.AssignSlot (slot → claimed with pr_id,
    open assignment with the pool's database names). A claimed or held slot the
    PR already has is returned as is; no free slot left is ErrNoFreeSlot.

func (m *Manager) Unpin(ctx context.Context, slot store.Slot) error
    Unpin hands the slot back to automation: pinned and hold_reason are cleared.
    A head_drift or unpushed_commits hold is acknowledged by taking the current
    HEAD as the new checked_out_sha, so the next guard passes and the next
    release resets the slot. A dirty_worktree hold comes back at the next
    guard while the changes are there and a human still shows (their agent
    or process in the slot, or human_active_at after the checkout): commit,
    stash or discard them first (or remove the slot with force).

type MySQL interface {
	ListSuffixed(ctx context.Context) ([]mysqlx.Database, error)
	SchemaMigrationsMax(ctx context.Context, dbName string) (string, error)
	DropAll(ctx context.Context, names []string, g mysqlx.Guard) []mysqlx.DropResult
}
    MySQL is the part of *mysqlx.Client the slots package uses.

type PrefixLister interface {
	ListPrefixed(ctx context.Context, prefixes []string) ([]mysqlx.Database, error)
}
    PrefixLister is a MySQL client that lists the schemas whose names start with
    one of prefixes followed by a non-empty slug, ordered by name (requested
    from mysqlx as (*Client).ListPrefixed). ListPoolDatabases uses it when the
    client has it.

type SchemaCheck struct {
	// Need is true when the slot's databases must be reloaded (the pool's
	// reset_db) to carry the checkout's schema.
	Need bool
	// Why says why a reload is needed, or why not ("schema unchanged since
	// abc1234: no reset").
	Why string
	// FP, SHA and Version are the checkout's schema: the fingerprint of
	// the files schema_paths match at SHA (its HEAD) and the version its
	// db/schema.rb declares ("" = none). RecordSchema stores them once the
	// reload succeeded.
	FP, SHA, Version string
}
    SchemaCheck is CheckSchema's answer for a pool slot.

type WTHooks struct {
	PostCreate []Hook
	PostStart  []Hook
	PreRemove  []Hook
}
    WTHooks are the hook tables of a .config/wt.toml, each in file order.

func ParseWTHooks(data []byte) (WTHooks, error)
    ParseWTHooks reads the [post-create], [post-start] and [pre-remove] tables
    of a worktrunk project config (`post-start = "cmd"` is accepted as a
    one-command table named after the hook). Commands keep their {{ templates
    }}; every other key is ignored. A non-string command is an error.

```

## steps

```text
package steps // import "github.com/zhuravel/magnum/internal/steps"

Package steps makes multi-step side effects resumable after a crash.

Step writes an events row (kind "step", phase begin) before running a side
effect and an ok or fail row after it. When the daemon restarts and replays
the same sequence, steps that already have an ok row in the subject's current
generation are skipped, so the sequence picks up where it stopped. ResetSubject
starts a new generation (for example, a new review round on a new head) so every
step runs again; old rows stay as audit history.

Guarantee: a step whose ok row was written never runs again in that generation.
A crash after fn returned but before the ok row was written re-runs fn, so every
fn must be idempotent or prechecked (at-least-once).

Subjects are free-form strings such as "pr:123" or "slot:review3"; they are also
the events.subject that `magnum logs` filters on.

FUNCTIONS

func Done(ctx context.Context, st *store.Store, subject, name string) (bool, error)
    Done reports whether (subject, name) completed in the current generation.

func ResetSubject(ctx context.Context, st *store.Store, subject string) error
    ResetSubject starts a new step generation for subject: earlier ok rows are
    kept for history but no longer cause steps to be skipped.

func Step(ctx context.Context, st *store.Store, subject, name string, fn func(ctx context.Context) error) error
    Step runs fn once per subject generation. If (subject, name) already has
    an ok row in the current generation, fn is skipped and Step returns nil.
    Otherwise it writes a begin row, runs fn and writes an ok row, or a fail
    row (message redacted with execx.Redact) and returns fn's error. Rows are
    written even when ctx is cancelled during fn. A fail row caused by context
    cancellation (daemon shutdown) is logged at level "warn" as "interrupted"
    instead of level "error"; it is still a fail row, so the step is not done
    and runs again on resume.

func WithFailpoint(ctx context.Context, fp Failpoint) context.Context
    WithFailpoint returns a context whose Step calls consult fp.


TYPES

type Failpoint func(subject, name string, at Point) error
    Failpoint lets tests simulate a crash: a non-nil error aborts Step at that
    point with the error and without writing an ok or fail row.

type Point string
    Point identifies where in a step a failpoint fires.

const (
	// BeforeRun fires after the begin row is written, before fn runs.
	BeforeRun Point = "before"
	// AfterRun fires after fn succeeded, before the ok row is written.
	AfterRun Point = "after"
)
```

## store

```text
package store // import "github.com/zhuravel/magnum/internal/store"

Package store is magnum's SQLite registry: repos, PRs, slots, assignments,
slot databases, agent sessions, review runs, CLI requests, the audit event log,
small key/value state and notification dedup.

The daemon and the CLI open the same file (WAL mode, busy_timeout 5 s,
one connection per process, BEGIN IMMEDIATE transactions). The schema lives in
migrations/NNNN_*.sql and is applied in order, tracked by PRAGMA user_version;
a database newer than this binary is refused at Open, and SchemaChanged tells
a long-running process that another binary migrated the file after it opened.
Prune is the retention pass for the events and requests tables.

State changes are compare-and-set: TransitionPR, TransitionSlot,
TransitionSession and TransitionRun issue UPDATE … WHERE id=? AND state IN
(…) and return ErrConflict when another writer got there first. Timestamps are
stored as fixed-width RFC3339 UTC text with a 9-digit fraction (see TimeFormat
and FormatTime), so string comparison in SQL is chronological comparison.

CONSTANTS

const (
	FindingPosted   = "posted"
	FindingRejected = "rejected"
)
    Finding verdicts (findings.verdict).

const (
	KVDaemonStartedAt     = "daemon.started_at"     // store.FormatTime
	KVDaemonLastTick      = "daemon.last_tick"      // store.FormatTime
	KVDaemonLastPoll      = "daemon.last_poll"      // store.FormatTime
	KVDaemonLastReconcile = "daemon.last_reconcile" // store.FormatTime
	KVDaemonPid           = "daemon.pid"
	// KVDaemonPaused is "1" while automation is paused (magnum pause);
	// KVDaemonPausedReason and KVDaemonPausedUntil (optional) explain it.
	KVDaemonPaused       = "daemon.paused"
	KVDaemonPausedReason = "daemon.paused_reason"
	KVDaemonPausedUntil  = "daemon.paused_until"

	KVGHRemaining       = "gh.remaining"
	KVGHLimit           = "gh.limit"
	KVGHReset           = "gh.reset"
	KVGHPollPausedUntil = "gh.poll_paused_until"
	KVHerdrUp           = "herdr.up" // "1" or "0"
)
    kv keys shared by the daemon (which writes them) and the CLI (`magnum
    status`, `doctor`, `pause`, `resume`, `identities`), so neither side spells
    them as string literals. Values are never secrets.

const (
	RetroNothing      = "nothing" // no candidate: nothing to classify
	RetroClassified   = "classified"
	RetroUnclassified = "unclassified" // candidates stored without a classifier
	RetroFailed       = "failed"
)
    Retro outcomes per PR (retro_prs.status).

const (
	MissUnclassified = "unclassified"
	MissMiss         = "miss"
	MissNotIssue     = "not_issue"
	MissStyle        = "style"
	MissOutside      = "outside"
)
    Miss classes (misses.class).

const (
	MissSourceThread = "thread"
	MissSourceReview = "review"
)
    Miss sources (misses.source_kind): a review thread or a review body.

const (
	MissRaisedNone     = "none"
	MissRaisedRejected = "rejected"
)
    What magnum did with the point of a miss (misses.raised): it never raised
    it, or raised it and the judge rejected it.

const (
	MissNew       = "new"
	MissUsed      = "used"
	MissDismissed = "dismissed"
)
    Miss states (misses.state): new until a lesson was drawn from it (used) or
    someone dropped it (dismissed).

const (
	MissScopeRepo    = "repo"
	MissScopeGeneral = "general"
)
    Miss scopes (misses.scope): a lesson for the PR's repository or for every
    repository.

const (
	RepoModePool  = "pool"
	RepoModePerPR = "per_pr"
)
    Repo modes.

const (
	PRBaseline        = "baseline"
	PRIneligible      = "ineligible"
	PRQueued          = "queued"
	PRClaiming        = "claiming"
	PRReviewing       = "reviewing"
	PRVerifying       = "verifying"
	PRReviewed        = "reviewed"
	PRRereviewPending = "rereview_pending"
	PRPaused          = "paused"
	PRNeedsAttention  = "needs_attention"
	PRClosed          = "closed"
	PRReleasing       = "releasing"
	PRReleased        = "released"
)
    PR states (prs.state).

const (
	GHOpen    = "OPEN"
	GHClosed  = "CLOSED"
	GHMerged  = "MERGED"
	GHUnknown = "UNKNOWN"
)
    GitHub PR states (prs.gh_state).

const (
	SlotKindPool     = "pool"
	SlotKindPerPR    = "per_pr"
	SlotKindExternal = "external"
)
    Slot kinds.

const (
	SlotProvisioning = "provisioning"
	SlotFree         = "free"
	SlotClaimed      = "claimed"
	SlotBusy         = "busy"
	SlotHeld         = "held"
	SlotReleasing    = "releasing"
	SlotDirtySchema  = "dirty_schema"
	SlotBroken       = "broken"
	SlotRemoving     = "removing"
	SlotRemoved      = "removed"
	SlotLost         = "lost"
	SlotObserved     = "observed"
)
    Slot states (slots.state).

const (
	RoleJudge       = "codex-judge"
	RoleClaude      = "claude-review"
	RoleCodexReview = "codex-review"
	RoleSimplify    = "claude-simplify"
)
    Default role names (sessions.role, runs.role). Roles are free-form names
    defined in config; the store only requires them to be non-empty. These are
    the four built-in roles. Migration 0003 renamed their pre-v3 ids (judge,
    claude, codex_review, simplify) to these labels.

const (
	SessionStarting = "starting"
	SessionLive     = "live"
	SessionParked   = "parked"
	SessionLost     = "lost"
	SessionClosed   = "closed"
)
    Session states.

const (
	RunInitial  = "initial"
	RunRereview = "rereview"
	RunContinue = "continue"
	RunNudge    = "nudge"
	RunRecovery = "recovery"
	// RunOwnPass is the judge's own pass, prompted with the reviewers
	// (pipeline); its continuations on a fallback model keep the kind.
	RunOwnPass = "own_pass"
)
    Run kinds.

const (
	RunPending   = "pending"
	RunSubmitted = "submitted"
	RunWorking   = "working"
	RunEnded     = "ended"
	RunVerified  = "verified"
	RunFailed    = "failed"
	RunAbandoned = "abandoned"
)
    Run states.

const (
	RequestPending = "pending"
	RequestDone    = "done"
	RequestFailed  = "failed"
)
    Request states.

const (
	KindStep      = "step"       // one begin/ok/fail row per step attempt
	KindStepReset = "step.reset" // starts a new step generation for a subject
	PhaseBegin    = "begin"
	PhaseOK       = "ok"
	PhaseFail     = "fail"
)
    Event kinds and phases used by the steps helper.

const (
	CheckPassed  = "passed"
	CheckFailed  = "failed"
	CheckPending = "pending"
	CheckSkipped = "skipped"
)
    CheckResult.State values (github.Check's).

const (
	SinceFromReviewed = "reviewed" // prs.reviewed_sha: magnum's last verified review
	SinceFromReview   = "review"   // the commit of the PR identity's latest GitHub review (no reviewed_sha yet)
	SinceFromBase     = "base"     // the base branch tip: nothing reviewed yet, so the whole PR
)
    SinceReview.Source values: what Base is.

const (
	// NeedsMeApprove: GitHub still requires an approval that counts
	// (REVIEW_REQUIRED), which the operator's would give.
	NeedsMeApprove = "approve"
	// NeedsMeLift: the operator's own changes request is the only one
	// blocking the PR (CHANGES_REQUESTED): their approval, or a dismissal,
	// lifts it.
	NeedsMeLift = "lift"
)
    NeedsMe values: what a PR magnum approved waits for from the operator.

const (
	NotesFromJudge    = "judge"    // a round's judge changed them
	NotesFromCuration = "curation" // a curator proposed them (applied or not)
	NotesFromHuman    = "human"    // `magnum notes --edit`, or a restore the operator applied
	NotesFromImport   = "import"   // found on disk without a record of who wrote them
)
    Notes version sources (notes_versions.source).

const (
	ProposalCuration = "curation"
	ProposalRestore  = "restore"

	ProposalPending  = "pending"
	ProposalApplied  = "applied"
	ProposalRejected = "rejected"
	ProposalExpired  = "expired"
	ProposalInvalid  = "invalid"
	// ProposalSuperseded is a stale curation (the notes changed since it
	// was made) that a new curation of the notes now follows up on, reading
	// it as input (migration 0017).
	ProposalSuperseded = "superseded"
)
    Notes proposal kinds and states (notes_proposals.kind, .state).

const (
	MissNoted   = "noted"   // a note covers it now, in Section
	MissSkipped = "skipped" // no note helps a future review: Reason says why
)
    What a proposal did with a miss it was given
    (notes_proposal_misses.outcome).

const (
	RequiredFromConfig = "config" // the [[repo]] block's required_checks
	RequiredFromGitHub = "github" // the default branch's rulesets or branch protection
)
    RequiredChecks.Source values.

const (
	VerdictBlocking    = "blocking"     // at least one P0 or P1: request changes
	VerdictNonBlocking = "non_blocking" // only P2 and P3: comment
	VerdictClean       = "clean"        // nothing to fix: approve
)
    Verdicts: what a review concluded, whatever its repository lets it post
    (ReviewSummary.Verdict).

const MissDismissRejections = 2
    MissDismissRejections is how many rejected proposals a miss may be in before
    it is dismissed.

const NotesUnusedRounds = 20
    NotesUnusedRounds is how many judge rounds a harness file must have existed
    for, with no recorded use, to be a curation candidate.

const RetroMaxAttempts = 3
    RetroMaxAttempts is how many retros in a row may fail on a PR before the
    retro gives it up (RetroDue; `magnum retro --again` still takes it).

const SinceReviewVersion = 2
    SinceReviewVersion is the SinceReview.Version of a size measured with a
    merge of the base branch told apart (BaseMerged, Raw; 1), with a file GitHub
    lists without a patch or a line (an empty or binary file) compared by its
    blob and a raw size's commits the PR's own (2).

const TeamReviewerPrefix = "team:"
    TeamReviewerPrefix marks a team in requested_reviewers_json ("team:<slug>").

const TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"
    TimeFormat is RFC3339 with a fixed nine-digit fraction. Unlike
    time.RFC3339Nano it never trims zeros, so lexical order equals time order.


VARIABLES

var ClaimableStates = []string{PRQueued, PRRereviewPending}
    ClaimableStates are the PR states ClaimSlot accepts.

var DueStates = []string{PRQueued, PRRereviewPending, PRClaiming, PRReviewing, PRVerifying, PRPaused, PRNeedsAttention}
    DueStates are the automation states in which magnum means to review a PR:
    a round is due or running. Callers must not modify the slice.

var ErrConflict = errors.New("store: conflict")
    ErrConflict is returned (wrapped) when a compare-and-set update matched no
    row because the state moved on, or when an insert hits a uniqueness rule (a
    second open assignment, a second live session for a role, …).

var ErrNotFound = errors.New("store: not found")
    ErrNotFound is returned (wrapped) when a looked-up row does not exist.

var InFlightStates = []string{PRClaiming, PRReviewing, PRVerifying}
    InFlightStates are the PR states of a review round being prepared or
    running. The daemon owns a PR in them: CLI writes refuse, recovery
    re-evaluates them at startup. Callers must not modify the slice.


FUNCTIONS

func DayKey(t time.Time) string
    DayKey is the local calendar day of t ("2006-01-02"), the value stored in
    prs.rounds_day next to rounds_today.

func Deref[T any](p *T) T
    Deref returns *p, or the zero value when p is nil.

func FormatTime(t time.Time) string
    FormatTime renders t in UTC with TimeFormat. Every timestamp written to the
    database must go through it.

func IsFlagDismissed(ghState, prevState, headSHA, reviewedSHA string, muted, forced bool) bool
    IsFlagDismissed reports whether a PR is muted without being flagged,
    though it would be flagged unmuted: GitHub merged it in a state where magnum
    meant to review it, before its head was reviewed, and nothing forces a round
    of it. Muting such a PR is how the flag is dismissed; unmuting restores it.

func IsMergedUnreviewed(ghState, prevState, headSHA, reviewedSHA string, muted, forced bool) bool
    IsMergedUnreviewed reports whether GitHub merged a PR before magnum reviewed
    its last push: ghState is MERGED, prevState (the state the PR closed in)
    is one of DueStates, and headSHA is not reviewedSHA ("" = never reviewed).
    A muted PR waits for no round unless it was forced, so it is not flagged;
    baseline, reviewed, skipped and ignored PRs close outside DueStates and
    never are. The forced mark outlives the close, so a PR forced before it
    merged stays flagged after it is muted, until the daemon clears the mark on
    a mute of a PR GitHub no longer lists as open (IsFlagDismissed).

func KVIdentityCheck(name string) string
    KVIdentityCheck holds "pass" or "fail" from an identity's last Check.

func KVIdentityError(name string) string
    KVIdentityError holds why an identity's Check failed.

func KVIdentityTickError(name string) string
    KVIdentityTickError holds a tick-time token refresh failure.

func KVIdentityTokenExpiry(name string) string
    KVIdentityTokenExpiry holds when an App identity's token expires
    (store.FormatTime).

func KVPRDryRun(prID int64) string
    KVPRDryRun holds the PR state a dry-run round (review request with dry_run)
    returns the PR to; while set, the next round posts nothing.

func KVPRFresh(prID int64) string
    KVPRFresh is "1" when the PR's next round parks its sessions and starts new
    conversations (magnum review --fresh).

func KVPRGate(prID int64) string
    KVPRGate is why the daemon skipped the PR at its last dispatch (an agent
    still working, a human in its panes, an unhealthy identity, ...); cleared
    when a round starts. Shown by `magnum status` as "waiting: <reason>".

func KVPRRoles(prID int64) string
    KVPRRoles holds the role names (a JSON list) requested for the PR's next
    round (magnum review --role, --simplify); cleared once a review posted.

func KVPRSessionsIdentity(prID int64) string
    KVPRSessionsIdentity is the identity the PR's live agent sessions were
    created for (their panes carry that identity's gh config); a round for
    another identity parks them and starts fresh.

func KVPRSimplify(prID int64) string
    KVPRSimplify is "1" when the PR's next round runs /simplify (magnum review
    --simplify).

func KVRepoRequiredChecks(fullName string) string
    KVRepoRequiredChecks holds a repository's GitHubRequiredChecks (JSON),
    written by the poller.

func KVScreenWidths(screen string) string
    KVScreenWidths holds the column widths dragged with the mouse on a screen
    ("board", "dashboard") as a JSON object of column name to cells; written by
    the CLI's screens, absent until a column is dragged.

func KVToolBackoff(tool string) string
    KVToolBackoff holds the last fallback pause of a tool (a Go duration).

func KVToolPausedDetail(tool string) string
    KVToolPausedDetail holds the pane line that caused a tool pause.

func KVToolPausedReason(tool string) string
    KVToolPausedReason holds why a tool is paused (usage_limit |
    login_required).

func KVToolPausedUntil(tool string) string
    KVToolPausedUntil holds when a tool's pause ends ("codex", "claude";
    store.FormatTime).

func KVWatchPaused(owner string) string
    KVWatchPaused holds the reason a watch owner's automation was paused
    (identity leak); magnum resume --watch clears it.

func LatestSchemaVersion() int
    LatestSchemaVersion is the highest migration version embedded in this
    binary.

func NeedsMe(f NeedsMeFacts, mine func(login string) bool) string
    NeedsMe is what the operator's approval would fix on a PR magnum approved,
    "" when nothing: an open PR, not a draft and not authored by one of the
    operator's logins (mine), whose latest verified magnum review is on the
    current head and clean (an approval, or a clean comment from an identity
    that comments when clean), and which GitHub still blocks on a review:
    NeedsMeApprove for REVIEW_REQUIRED, NeedsMeLift for CHANGES_REQUESTED when
    every outstanding changes request is the operator's. Anyone else's changes
    request, an unread gate or a list of opinions GitHub cut leaves it alone.

func ParseReviewResult(data []byte, sum *ReviewSummary) bool
    ParseReviewResult fills sum from a judge result file (the skill's section
    8 JSON): findings by priority, simplifications suggested (the candidates'
    `suggested`), earlier findings, the posted event and the verdict. A result
    without a verdict (written before the skill had one) gets it from the
    counts: P0 or P1 blocks, other findings or still-open earlier ones comment,
    none is clean. It reports false when data is not a result.

func ParseTime(s string) (time.Time, error)
    ParseTime parses any RFC3339 timestamp (with or without a fraction) and
    returns it in UTC.

func Ptr[T any](v T) *T
    Ptr returns a pointer to v (for nullable fields).


TYPES

type Assignment struct {
	ID        int64      `json:"id"`
	PRID      int64      `json:"pr_id"`
	SlotID    int64      `json:"slot_id"`
	Path      string     `json:"path"`
	DBSlug    *string    `json:"db_slug"`
	DBNames   []string   `json:"db_names"` // db_names_json
	HeadSHA   *string    `json:"head_sha"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	EndReason *string    `json:"end_reason"`
}
    Assignment records that a PR's code (and databases) lived in a slot.

type BoardFilter struct {
	Repo string // "owner/name" or "name", case-insensitive; "" = every repository
	// States, when non-empty, lists exactly the PRs in these automation
	// states (prs.state) and IncludeClosed is ignored.
	States []string
	Limit  int // 0 = no limit
	// IncludeClosed also lists PRs in closed/releasing/released and PRs
	// GitHub reports CLOSED or MERGED.
	IncludeClosed bool
	// ClosedSince, when set, also lists the PRs GitHub merged or closed at
	// or after it (merged_at, else closed_at), without IncludeClosed: the
	// board's recently closed section ([board] recent_closed).
	ClosedSince time.Time
}
    BoardFilter selects the rows of Board. The zero value lists every open PR.

type BoardRow struct {
	PRID               int64          `json:"pr_id"`
	Ref                string         `json:"ref"` // owner/name#N
	Owner              string         `json:"owner"`
	Name               string         `json:"name"`
	Number             int            `json:"number"`
	Title              string         `json:"title"`
	Author             string         `json:"author"` // Account form: a bot's keeps "[bot]"
	URL                string         `json:"url"`
	Draft              bool           `json:"draft"`
	Labels             []string       `json:"labels"`
	Assignees          []string       `json:"assignees"`
	RequestedReviewers []string       `json:"requested_reviewers"` // teams as "team:<slug>"
	ReviewRequested    bool           `json:"review_requested"`    // the watch's own login is requested
	LatestReviews      []LatestReview `json:"latest_reviews"`
	SinceReview        *SinceReview   `json:"since_review"` // nil until the poller computed it
	State              string         `json:"state"`        // automation state (prs.state)
	SkipReason         string         `json:"skip_reason"`
	GHState            string         `json:"gh_state"`
	UpdatedAt          time.Time      `json:"updated_at"`  // GitHub's updatedAt (prs.gh_updated_at)
	ActivityAt         time.Time      `json:"activity_at"` // the last activity (PR.Activity): prs.activity_at, else UpdatedAt
	HeadSHA            string         `json:"head_sha"`
	ReviewedSHA        string         `json:"reviewed_sha"`
	LastReviewEvent    string         `json:"last_review_event"`
	LastReviewAt       time.Time      `json:"last_review_at"` // prs.reviewed_at
	LastReviewLogin    string         `json:"last_review_login"`
	Identity           string         `json:"identity"`
	Slot               string         `json:"slot"` // name of the slot holding the PR
	SlotPath           string         `json:"slot_path"`
	Pinned             bool           `json:"pinned"`
	Muted              bool           `json:"muted"`
	NextEligibleAt     time.Time      `json:"next_eligible_at"`
	LastError          string         `json:"last_error"`
	RoundsToday        int            `json:"rounds_today"` // 0 when the stored count is from an earlier day
	// CIState is the head's check rollup as last seen (prs.ci_state; "" =
	// none or not seen yet) and CI the last Details' checks (nil until
	// fetched). CI.SHA differs from HeadSHA until the Details of a new head
	// are fetched; CI.State trails CIState while a Details fetch fails.
	CIState string    `json:"ci_state"`
	CI      *CIStatus `json:"ci"`
	// ReviewRequests are the newest review requests of the PR (at most 10,
	// oldest first; empty until the next Details fetch).
	ReviewRequests []ReviewRequest `json:"review_requests"`
	// PrevState is the automation state the PR left when magnum confirmed
	// it closed ("" while open); MergedAt and ClosedAt are GitHub's (zero
	// while open; MergedAt stays zero for a PR closed unmerged).
	PrevState string    `json:"prev_state"`
	MergedAt  time.Time `json:"merged_at"`
	ClosedAt  time.Time `json:"closed_at"`
	// MergedUnreviewed: GitHub merged the PR before magnum reviewed its last
	// push (IsMergedUnreviewed). FlagDismissed: the PR was muted after that
	// merge, so it is not flagged but would be unmuted (IsFlagDismissed).
	MergedUnreviewed bool `json:"merged_unreviewed"`
	FlagDismissed    bool `json:"flag_dismissed"`
	// ReviewGate is what GitHub's merge gate said of the reviews at the last
	// Details fetch (prs.review_gate_json); nil until then.
	ReviewGate *ReviewGate `json:"review_gate"`
}
    BoardRow is one PR as the board shows it: the prs row flattened with its
    repository and its current slot. Empty strings, zero times and nil pointers
    mean "none".

func (b BoardRow) NeedsMeFacts() NeedsMeFacts
    NeedsMeFacts are b's facts for NeedsMe; the caller adds CommentWhenClean
    and, when it WantsVerdict, Verdict.

type CIStatus struct {
	SHA string `json:"sha"` // the commit the checks ran on
	// State is GitHub's rollup (SUCCESS | FAILURE | PENDING | ERROR |
	// EXPECTED; "" = no checks), which the radar compares; it counts
	// superseded runs too and calls a draft whose checks all skipped
	// SUCCESS. Read the counts and AllSkipped for what ran.
	State string `json:"state"`
	// Total is len(Checks) plus the checks GitHub did not return; the
	// counts below cover Checks.
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Pending int `json:"pending"`
	Skipped int `json:"skipped"`
	// AllSkipped is true when there are checks and every one skipped: the
	// CI did not really run (a draft).
	AllSkipped bool `json:"all_skipped"`
	// Complete is true when Checks lists every check (GitHub returns at
	// most 100).
	Complete bool `json:"complete"`
	// Checks holds the latest run of each (Workflow, Name). The same name
	// can still appear under several workflows (a check posted through the
	// API attaches to another workflow's suite): At tells the latest.
	Checks []CheckResult `json:"checks"`
}
    CIStatus is prs.ci_json: the checks of a PR's head commit. Its State can
    trail prs.ci_state, which the radar refreshes, until the next Details fetch.
    A required check absent from Checks has no run on SHA (a repository whose CI
    runs only when the PR is opened): it is missing on this head, not pending.

func (c *CIStatus) Tally()
    Tally sets Passed, Failed, Pending, Skipped and AllSkipped from Checks (and
    makes a nil Checks empty).

type CandidateParams struct {
	Now              time.Time
	QuietPeriod      time.Duration // pending_since + QuietPeriod <= Now
	MinInterval      time.Duration // last_round_started_at + MinInterval <= Now
	DraftMinInterval time.Duration // the same for drafts; 0 = MinInterval
	MaxRoundsPerDay  int           // rounds_today < cap when rounds_day == Day
	Day              string        // DayKey(Now) when empty
}
    CandidateParams are the throttle settings Candidates applies (mirroring
    config.Daemon). Zero durations and a zero cap disable that gate.

type CheckResult struct {
	Name     string `json:"name"`               // the check run's name or the status's context
	State    string `json:"state"`              // CheckPassed | CheckFailed | CheckPending | CheckSkipped
	Workflow string `json:"workflow,omitempty"` // the GitHub Actions workflow; "" for a status or another app's check
	// At is when the check finished, else started, or the status was
	// posted; zero for a check run not started yet (the newest run).
	At time.Time `json:"at,omitzero"`
}
    CheckResult is one check run or commit status of a CIStatus.

type Event struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"` // zero = now on append
	Level   string          `json:"level"`
	Subject *string         `json:"subject"`
	Kind    string          `json:"kind"`
	Step    *string         `json:"step"`
	Phase   *string         `json:"phase"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"` // data_json; nil = NULL
}
    Event is one audit-log row (`magnum logs`).

type FilesPR struct {
	ID          int64
	Number      int
	URL         string
	GHState     string // GHOpen or GHMerged
	IsDraft     bool
	MergedAt    *time.Time
	ReviewedSHA *string // the head magnum last reviewed; nil = never reviewed
	Files       PRFiles
}
    FilesPR is a PR with its stored file list (PRsWithFiles).

type Finding struct {
	ID         int64     `json:"id"`
	RunID      string    `json:"run_id"`
	PRID       int64     `json:"pr_id"`
	Round      int       `json:"round"`
	FindingID  string    `json:"finding_id"`
	Severity   string    `json:"severity,omitempty"` // P0..P3 as the judge wrote it
	Path       string    `json:"path,omitempty"`
	Line       int       `json:"line,omitempty"` // 0 = none (a finding in the review body)
	Sources    []string  `json:"sources"`
	Verdict    string    `json:"verdict"`               // FindingPosted | FindingRejected
	ReasonCode string    `json:"reason_code,omitempty"` // rejections: duplicate, not_reproducible, ...
	CreatedAt  time.Time `json:"created_at"`
	// Repo is the PR's repository (owner/name); FindingsSince fills it.
	Repo string `json:"repo,omitempty"`
}
    Finding is one finding the judge judged in a posted round, from the
    provenance list of its result file (skills/magnum-review/SKILL.md section
    8): the sources that raised it (reviewer roles, "judge" for the judge's own
    pass), whether it was posted and, for a rejection, the reason code.

type GitHubPR struct {
	RepoID      int64
	NodeID      string
	Number      int
	URL         string
	HeadSHA     string
	IsDraft     bool
	Title       *string
	AuthorLogin *string
	AuthorType  *string
	HeadRef     *string
	BaseRef     *string
	IsCrossRepo *bool
	// ReviewRequested is whether the watched user is a requested reviewer
	// (nil = keep the stored value).
	ReviewRequested *bool
	Labels          []string
	GHState         string
	GHUpdatedAt     *time.Time
	MergedAt        *time.Time
	ClosedAt        *time.Time

	// Board fields from Details; nil = keep the stored value.
	Assignees          []string
	RequestedReviewers []string // teams as "team:<slug>"
	LatestReviews      []LatestReview
	BaseSHA            *string
	// DetailsAt stamps prs.details_at (the Details were fetched now). It is
	// bookkeeping: on its own it neither counts as Changed nor forces a write
	// when the stored value is already set.
	DetailsAt *time.Time
	// AuthorAssociation is the Details' authorAssociation (nil = keep).
	AuthorAssociation *string
	// CIState is the head's check rollup, from the poll's CI read or the
	// Details, and CI the Details' checks (nil = keep). Like DetailsAt they
	// are not Changed: CI moving changes no eligibility and queues nothing.
	CIState *string
	CI      *CIStatus
	// ReviewRequests are the timeline's newest review requests, oldest first
	// (nil = keep). Not Changed either: a request moves no eligibility.
	ReviewRequests []ReviewRequest
	// Files are the Details' changed paths at Files.HeadSHA (nil = keep),
	// written only when the PR has no list or its list belongs to another
	// head (pr_files). Not Changed either: the list moves no eligibility
	// (skip_paths reads its own).
	Files *PRFiles
	// ActivityAt is the Details' activity time (nil = keep the stored one).
	// The stored prs.activity_at is the later of it and the PR's last head
	// move the poller saw (head_changed_at, but not its insertion); a head
	// move moves a stored one without Details too. Not Changed either: it
	// moves no eligibility.
	ActivityAt *time.Time
	// ReviewGate is the Details' review gate (nil = keep the stored one).
	// Not Changed either: it moves no eligibility.
	ReviewGate *ReviewGate

	// InitialState and Identity are used only when the PR is new.
	InitialState string
	Identity     string
}
    GitHubPR is what the poller knows about a PR. Pointer fields (and nil
    Labels, empty GHState) mean "not fetched this time": the stored value is
    kept. Number, URL, HeadSHA and IsDraft are always present in the radar.

type GitHubRequiredChecks struct {
	Branch string   `json:"branch"`
	Checks []string `json:"checks"` // contexts, i.e. check run names or status contexts
	// Known is false when GitHub would not tell (403 on a private repository
	// of a free plan, 404 without admin access to the branch protection).
	Known     bool      `json:"known"`
	FetchedAt time.Time `json:"fetched_at,omitzero"` // the last read GitHub answered
	CheckedAt time.Time `json:"checked_at"`          // the last attempt
	// Error is why the last attempt failed; the fields above are what the
	// reads before it found.
	Error string `json:"error,omitempty"`
}
    GitHubRequiredChecks is what the poller last read of the status checks a
    repository's default branch requires.

type LatestReview struct {
	Login       string     `json:"login"`                  // Account form (github.Account): a bot's keeps "[bot]"; "" for a ghost
	State       string     `json:"state"`                  // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	SubmittedAt *time.Time `json:"submitted_at,omitempty"` // nil for a PENDING review
	CommitSHA   string     `json:"commit_sha"`             // "" when the commit is gone
}
    LatestReview is one entry of prs.latest_reviews_json: GitHub's latest review
    of one reviewer.

type Miss struct {
	ID          int64     `json:"id"`
	PRID        int64     `json:"pr_id"`
	SourceURL   string    `json:"source_url"`  // GitHub's link to the comment; the key UpsertMiss stores by
	SourceKind  string    `json:"source_kind"` // MissSourceThread | MissSourceReview
	Reviewer    string    `json:"reviewer"`
	Path        string    `json:"path,omitempty"`
	Line        int       `json:"line,omitempty"` // 0 = none (a comment on the review body)
	ReviewedSHA string    `json:"reviewed_sha"`   // the commit magnum's review covered
	Class       string    `json:"class"`          // Miss* class
	Severity    string    `json:"severity,omitempty"`
	Raised      string    `json:"raised"`                // MissRaisedNone | MissRaisedRejected
	FindingRef  string    `json:"finding_ref,omitempty"` // the rejected finding: "<run id>/<finding id>" (findings)
	ReasonCode  string    `json:"reason_code,omitempty"` // why the judge rejected it
	Title       string    `json:"title,omitempty"`
	Lesson      string    `json:"lesson,omitempty"`
	Scope       string    `json:"scope,omitempty"` // MissScopeRepo | MissScopeGeneral
	Lines       []int     `json:"lines"`           // lines_json
	Match       []string  `json:"match"`           // match_json
	State       string    `json:"state"`           // MissNew | MissUsed | MissDismissed
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Repo ("owner/name") and Number name the miss's PR; Misses fills them.
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	// ProposalID and ProposalState name the latest notes proposal the miss
	// was given to (notes_proposal_misses) and where that one stands: the
	// proposal that used it once it is used; Misses fills them.
	ProposalID    *int64 `json:"proposal_id,omitempty"`
	ProposalState string `json:"proposal_state,omitempty"`
}
    Miss is a comment another reviewer made on a PR magnum reviewed, with what
    the retro made of it (misses).

type MissFilter struct {
	Classes []string // empty = every class
	States  []string // empty = every state
	Scopes  []string // empty = any scope, none included
	PRID    int64    // 0 = every PR
	RepoID  int64    // 0 = every repository
}
    MissFilter selects misses; zero values select everything.

type NeedsMeFacts struct {
	GHState string // prs.gh_state
	Draft   bool
	Author  string // the author's login ("" for a ghost)
	// HeadSHA is the PR's head and ReviewedSHA the commit magnum's latest
	// verified review stands for (prs.reviewed_sha).
	HeadSHA, ReviewedSHA string
	// LastReviewEvent is what magnum's latest review posted
	// (prs.last_review_event): APPROVED, COMMENTED, CHANGES_REQUESTED or
	// DISMISSED.
	LastReviewEvent string
	// CommentWhenClean: the PR's identity posts a comment, not an approval,
	// for a clean verdict on its repository ([[identity]] or [[repo]]
	// no_findings_event = "COMMENT"), so a clean comment is its approval.
	CommentWhenClean bool
	// Verdict is the verdict of magnum's latest posted round
	// (ReviewSummary.Verdict); only a comment reads it.
	Verdict string
	Gate    *ReviewGate // nil until the Details read it
}
    NeedsMeFacts are what NeedsMe decides from: the PR as the registry has it,
    and what the configuration and magnum's latest round say of its review.

func (f NeedsMeFacts) WantsVerdict() bool
    WantsVerdict reports whether NeedsMe reads f.Verdict: magnum's latest review
    is a comment from an identity that comments when clean.

type NeedsMePR struct {
	PR      PR
	Repo    string // owner/name
	NeedsMe string // NeedsMeApprove or NeedsMeLift
}
    NeedsMePR is an open PR that needs the operator.

type NotesBlob struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Body   []byte `json:"-"`
}
    NotesBlob is one harness file of a version: its path relative to the
    harness directory, its content's SHA-256 and size, and (when read with
    NotesVersionContent, or recorded) its body.

type NotesContent struct {
	Notes []byte
	Files []NotesBlob
}
    NotesContent is what a version holds: the notes text and every harness file
    with its body, sorted by path.

type NotesFileUse struct {
	File      string     `json:"file"`
	FirstSeen time.Time  `json:"first_seen"`
	Rounds    int        `json:"rounds"`
	Uses      int        `json:"uses"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}
    NotesFileUse is how a harness file of a repository was used: since when it
    exists, how many judge rounds it existed for and how many of them (or their
    reviewers) used it.

func (u NotesFileUse) Unused() bool
    Unused reports whether the file is a curation candidate: it existed for
    NotesUnusedRounds judge rounds or more and no round recorded a use.

type NotesProposal struct {
	ID               int64           `json:"id"`
	RepoID           int64           `json:"repo_id"`
	Kind             string          `json:"kind"`              // ProposalCuration | ProposalRestore
	Trigger          string          `json:"trigger,omitempty"` // why a curation ran: over_limit, weekly, request
	BaseVersionID    *int64          `json:"base_version_id,omitempty"`
	VersionID        *int64          `json:"version_id,omitempty"`         // the proposed state
	AppliedVersionID *int64          `json:"applied_version_id,omitempty"` // recorded when it was applied
	Changes          json.RawMessage `json:"changes,omitempty"`            // the curator's changes.json
	State            string          `json:"state"`
	Reason           string          `json:"reason,omitempty"` // rejected: the operator's; expired, invalid, superseded: magnum's
	Model            string          `json:"model,omitempty"`
	PromptSHA256     string          `json:"prompt_sha256,omitempty"`
	Scratch          string          `json:"scratch,omitempty"` // the curation's directory
	CreatedAt        time.Time       `json:"created_at"`
	DecidedAt        *time.Time      `json:"decided_at,omitempty"`
}
    NotesProposal is a proposal for a repository's notes: a curator's, or a
    restore of an earlier version.

type NotesProposalFilter struct {
	RepoID int64
	States []string
	Limit  int
}
    NotesProposalFilter selects proposals: of a repository (0 = any), in States
    (empty = any), newest first, at most Limit (0 = all).

type NotesProposalInput struct {
	RepoID        int64
	Kind          string // ProposalCuration | ProposalRestore
	Trigger       string
	BaseVersionID int64 // 0 = none
	// Proposed is a curation's proposed state, recorded as a version of
	// source curation tied to the proposal; nil for a restore (VersionID)
	// or an invalid proposal (no state kept).
	Proposed *NotesContent
	// VersionID is the version a restore proposes.
	VersionID int64
	Changes   []byte // changes.json (nil = none)
	State     string // ProposalPending, or ProposalInvalid with Reason
	Reason    string
	Model     string
	PromptSHA string
	Scratch   string
	At        time.Time // zero = now
	// Misses is what a curation did with each miss it was given
	// (notes_proposal_misses).
	Misses []ProposalMiss
}
    NotesProposalInput is a proposal to store (CreateNotesProposal).

type NotesVersion struct {
	ID         int64       `json:"id"`
	RepoID     int64       `json:"repo_id"`
	At         time.Time   `json:"at"`
	Source     string      `json:"source"`
	PRID       *int64      `json:"pr_id,omitempty"`
	PRNumber   int         `json:"pr_number,omitempty"` // the PR's number (read only)
	RunID      string      `json:"run_id,omitempty"`
	ProposalID *int64      `json:"proposal_id,omitempty"`
	Bytes      int64       `json:"bytes"`
	SHA256     string      `json:"sha256"`
	Files      []NotesBlob `json:"files"`
}
    NotesVersion is one recorded state of a repository's notes.

func (v NotesVersion) HarnessBytes() int64
    HarnessBytes is the total size of v's harness files.

type NotesVersionInput struct {
	RepoID     int64
	At         time.Time // zero = now
	Source     string    // NotesFrom*
	PRID       int64     // 0 = none
	RunID      string
	ProposalID int64 // 0 = none
	Content    NotesContent
	// Dedupe: when the latest version of the repository's history already
	// holds this state, nothing is recorded and that version is returned.
	Dedupe bool
}
    NotesVersionInput is a state to record (RecordNotesVersion).

type Options struct {
	// BeforeMigrate, when set, runs before pending migrations are applied,
	// with the file's schema version and this binary's. An error refuses the
	// migration: OpenWith fails with it and leaves the file untouched. The
	// CLI uses it so a newer build never migrates the registry under a
	// running older daemon (which exits when the schema changes under it).
	BeforeMigrate func(from, to int) error
}
    Options tune OpenWith.

type PR struct {
	ID                 int64      `json:"id"`
	RepoID             int64      `json:"repo_id"`
	NodeID             string     `json:"node_id"`
	Number             int        `json:"number"`
	URL                string     `json:"url"`
	Title              *string    `json:"title"`
	AuthorLogin        *string    `json:"author_login"`
	AuthorType         *string    `json:"author_type"`
	HeadRef            *string    `json:"head_ref"`
	BaseRef            *string    `json:"base_ref"`
	HeadSHA            string     `json:"head_sha"`
	HeadChangedAt      time.Time  `json:"head_changed_at"`
	IsDraft            bool       `json:"is_draft"`
	IsCrossRepo        bool       `json:"is_cross_repo"`
	ReviewRequested    bool       `json:"review_requested"` // the watched user is a requested reviewer
	Labels             []string   `json:"labels"`           // labels_json
	GHState            string     `json:"gh_state"`
	GHUpdatedAt        *time.Time `json:"gh_updated_at"`
	MergedAt           *time.Time `json:"merged_at"`
	ClosedAt           *time.Time `json:"closed_at"`
	MissingSince       *time.Time `json:"missing_since"`
	ConfirmCount       int        `json:"confirm_count"`
	State              string     `json:"state"`
	SkipReason         *string    `json:"skip_reason"`
	PrevState          *string    `json:"prev_state"`
	Forced             bool       `json:"forced"`
	Identity           string     `json:"identity"`
	ReviewedSHA        *string    `json:"reviewed_sha"`
	LastReviewID       *int64     `json:"last_review_id"`
	LastReviewEvent    *string    `json:"last_review_event"`
	ReviewedAt         *time.Time `json:"reviewed_at"`
	LastRoundStartedAt *time.Time `json:"last_round_started_at"`
	RoundsToday        int        `json:"rounds_today"`
	RoundsDay          *string    `json:"rounds_day"`
	NextEligibleAt     *time.Time `json:"next_eligible_at"`
	PendingSince       *time.Time `json:"pending_since"`
	ReleaseAfter       *time.Time `json:"release_after"`
	Attempts           int        `json:"attempts"`
	NextAttemptAt      *time.Time `json:"next_attempt_at"`
	LastError          *string    `json:"last_error"`
	Pinned             bool       `json:"pinned"`
	Muted              bool       `json:"muted"`
	SimplifyDone       bool       `json:"simplify_done"`
	HumanActiveAt      *time.Time `json:"human_active_at"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`

	// Board fields (migration 0002), written by the poller.
	Assignees          []string       `json:"assignees"`           // assignees_json
	RequestedReviewers []string       `json:"requested_reviewers"` // requested_reviewers_json; teams as "team:<slug>"
	LatestReviews      []LatestReview `json:"latest_reviews"`      // latest_reviews_json
	SinceReview        *SinceReview   `json:"since_review"`        // since_review_json; nil until computed
	LastReviewLogin    *string        `json:"last_review_login"`   // who posted reviewed_sha's review (Account form: an App's keeps "[bot]")
	BaseSHA            *string        `json:"base_sha"`            // base branch tip at the last Details fetch
	DetailsAt          *time.Time     `json:"details_at"`          // last Details fetch; nil = never
	// AuthorAssociation (migration 0006) is GitHub's authorAssociation of the
	// author with the repository; nil until the next Details fetch.
	AuthorAssociation *string `json:"author_association"`
	// CIState (migration 0007) is the head's check rollup as last seen
	// (SUCCESS | FAILURE | PENDING | ERROR | EXPECTED; "" = no checks); nil
	// until the poller saw it.
	CIState *string `json:"ci_state"`
	// CI (ci_json) is the head's checks as the last Details fetch saw them;
	// nil until then.
	CI *CIStatus `json:"ci"`
	// ReviewRequests (migration 0009, review_requests_json) are the newest
	// review requests of the PR's timeline, oldest first; empty until the
	// next Details fetch.
	ReviewRequests []ReviewRequest `json:"review_requests"`
	// ActivityAt (migration 0018) is the PR's last activity: the latest of
	// the Details' (github.PRDetails.ActivityAt) and the head moves the
	// poller saw, or its close or merge; nil until the next Details fetch.
	// The screens show Activity; radar change detection and the dispatch
	// order read GHUpdatedAt.
	ActivityAt *time.Time `json:"activity_at"`
	// ReviewGate (migration 0019, review_gate_json) is what GitHub's merge
	// gate said of the reviews at the last Details fetch; nil until then.
	ReviewGate *ReviewGate `json:"review_gate"`
}
    PR is one pull request and its automation state.

func (p PR) Activity() *time.Time
    Activity is the PR's last activity as the screens show it (the board's
    UPDATED): ActivityAt, else GitHub's updatedAt until the next Details fetch
    reads it; nil when neither is known.

func (p PR) FlagDismissed() bool
    FlagDismissed is IsFlagDismissed for p.

func (p PR) MergedUnreviewed() bool
    MergedUnreviewed is IsMergedUnreviewed for p.

func (p PR) NeedsMeFacts() NeedsMeFacts
    NeedsMeFacts are p's facts for NeedsMe; the caller adds CommentWhenClean
    and, when it WantsVerdict, Verdict.

type PRAgentTime struct {
	PRID   int64
	Repo   string // owner/name
	Number int
	Time   time.Duration // the sum of its runs' durations
	Rounds int           // the distinct rounds those runs belong to
}
    PRAgentTime is what one PR's runs cost over a window (AgentTimeSince).

type PRFiles struct {
	HeadSHA   string    // the head the paths belong to
	Paths     []string  // never nil once stored
	Truncated bool      // the PR changes more files than Paths lists
	FetchedAt time.Time // when the list was written (set by the store)
}
    PRFiles is the paths a PR changes at one head (pr_files): the first page of
    at most 100 that the poller's Details read (a rename lists its new path).

type PRFilter struct {
	RepoID int64
	States []string
	Limit  int
}
    PRFilter selects PRs for ListPRs. Zero values match everything.

type PRUpdate struct{ Update }
    PRUpdate is an Update on the prs table.

type PRUpsert struct {
	PR          PR
	New         bool // inserted now
	HeadChanged bool // existing PR whose head_sha changed (head_changed_at = now)
	Changed     bool // any GitHub-derived column changed (always true when New)
}
    PRUpsert reports what UpsertPRFromGitHub did.

type ProposalMiss struct {
	MissID  int64  `json:"miss_id"`
	Outcome string `json:"outcome"`           // MissNoted | MissSkipped
	Section string `json:"section,omitempty"` // noted: the "## " section of the proposed notes
	Reason  string `json:"reason,omitempty"`  // skipped: why; noted: optional
	// Miss is the miss as it is now (ProposalMisses fills it).
	Miss *Miss `json:"miss,omitempty"`
}
    ProposalMiss is what a proposal did with one miss it was given.

type PruneResult struct {
	Events   int64
	Requests int64
}
    PruneResult counts the rows Prune deleted.

type Repo struct {
	ID            int64      `json:"id"`
	NodeID        string     `json:"node_id"`
	Owner         string     `json:"owner"`
	Name          string     `json:"name"`
	WatchOwner    string     `json:"watch_owner"`
	ClonePath     *string    `json:"clone_path"`
	DefaultBranch string     `json:"default_branch"`
	Mode          string     `json:"mode"`
	FirstSyncedAt *time.Time `json:"first_synced_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
}
    Repo is a watched repository.

func (r Repo) FullName() string
    FullName is "owner/name".

type Request struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"` // payload_json
	State     string          `json:"state"`
	Result    *string         `json:"result"`
	CreatedAt time.Time       `json:"created_at"`
	HandledAt *time.Time      `json:"handled_at"`
}
    Request is a CLI → daemon work item.

type RequiredChecks struct {
	// Checks are check-name globs (path.Match, case-sensitive); entries from
	// the configuration may also be "workflow:<glob>", a whole GitHub
	// Actions workflow.
	Checks    []string  `json:"checks"`
	Source    string    `json:"source"`              // RequiredFromConfig | RequiredFromGitHub | "" (unknown)
	FetchedAt time.Time `json:"fetched_at,omitzero"` // when GitHub's list was read; zero for the configuration's
}
    RequiredChecks is the list of checks a repository's PRs must pass to be
    ready, and where it comes from.

type RetroPR struct {
	PRID       int64     `json:"pr_id"`
	RetroAt    time.Time `json:"retro_at"`
	Day        string    `json:"day"` // DayKey of the retro
	Status     string    `json:"status"`
	Candidates int       `json:"candidates"`
	Error      string    `json:"error,omitempty"`
	// Attempts counts the retros in a row that failed on the PR (0 once
	// one did not); RecordRetroPR keeps it.
	Attempts int `json:"attempts"`
}
    RetroPR is one PR's retro record (retro_prs).

type RetroQuery struct {
	Since time.Time // closed or merged at or after Since (ignored when PRIDs is set)
	// Until is the settle delay's bound: closed or merged at or before it
	// (zero = no bound; ignored when PRIDs is set).
	Until time.Time
	Again bool    // also PRs that already have a retro_prs row
	PRIDs []int64 // only these PRs, whenever they closed
}
    RetroQuery selects the PRs a retro looks at.

type ReviewGate struct {
	// Decision is GitHub's reviewDecision: APPROVED, CHANGES_REQUESTED or
	// REVIEW_REQUIRED; "" when the base branch requires no review.
	Decision string `json:"decision"`
	// Opinions are the latest approval or changes request (DISMISSED once
	// dismissed) of each reviewer with write access, which Decision counts
	// (GitHub's latestOpinionatedReviews, writersOnly); Login in Account
	// form, SubmittedAt not read.
	Opinions []LatestReview `json:"opinions"`
	// Complete is true when Opinions is every one (GitHub returns at most
	// 100).
	Complete bool `json:"complete"`
}
    ReviewGate is prs.review_gate_json (migration 0019): what GitHub's branch
    protection made of a PR's reviews at the last Details fetch.

type ReviewRequest struct {
	At time.Time `json:"at"`
	By string    `json:"by"` // who asked ("" for a deleted account); as GitHub's GraphQL names them, without "[bot]"
	To string    `json:"to"` // who was asked: Account form (github.Account), or "team:<slug>"
}
    ReviewRequest is one entry of prs.review_requests_json: a review asked of
    one reviewer.

type ReviewSummary struct {
	RunID           string    `json:"run_id"`
	SHA             string    `json:"sha"`   // the reviewed head
	Event           string    `json:"event"` // what was posted: APPROVE, REQUEST_CHANGES or COMMENT
	URL             string    `json:"url,omitempty"`
	At              time.Time `json:"at"`
	Counts          [4]int    `json:"counts"`          // P0..P3 posted this round
	Simplifications int       `json:"simplifications"` // optional suggestions posted this round
	Fixed           int       `json:"fixed"`           // earlier findings fixed (a re-review)
	Open            int       `json:"open"`            // earlier findings still open
	Answered        int       `json:"answered"`        // earlier findings answered with a reason
	Verdict         string    `json:"verdict"`         // VerdictBlocking, VerdictNonBlocking or VerdictClean
}
    ReviewSummary is what a PR's latest posted review round concluded,
    read from the judge's result file the round stored (runs.result_json):
    the findings it posted by priority, the simplifications it suggested,
    how the earlier findings stood, and its verdict. A repository whose policy
    only comments still has a verdict here: what the review would have decided.

func (s ReviewSummary) Findings() int
    Findings is the number of findings posted this round.

type RoundRun struct {
	Run
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}
    RoundRun is a run with its PR's repository (owner/name) and number.

type Run struct {
	ID              string     `json:"id"`
	PRID            int64      `json:"pr_id"`
	Round           int        `json:"round"`
	Role            string     `json:"role"`
	SessionID       *int64     `json:"session_id"`
	Kind            string     `json:"kind"`
	TargetSHA       string     `json:"target_sha"`
	PrevReviewedSHA *string    `json:"prev_reviewed_sha"`
	Identity        string     `json:"identity"`
	ReviewerLogin   string     `json:"reviewer_login"`
	State           string     `json:"state"`
	Outcome         *string    `json:"outcome"`
	ReportPath      *string    `json:"report_path"`
	ReviewID        *int64     `json:"review_id"`
	ReviewEvent     *string    `json:"review_event"`
	ReviewCommit    *string    `json:"review_commit"`
	ReviewURL       *string    `json:"review_url"`
	ResultJSON      *string    `json:"result_json"`
	PromptText      string     `json:"prompt_text"`
	CreatedAt       time.Time  `json:"created_at"`
	SubmittedAt     *time.Time `json:"submitted_at"`
	WorkingSeenAt   *time.Time `json:"working_seen_at"`
	EndedAt         *time.Time `json:"ended_at"`
	VerifiedAt      *time.Time `json:"verified_at"`
	Error           *string    `json:"error"`
}
    Run is one prompt/turn of one role in a review round.

type RunUpdate struct{ Update }
    RunUpdate is an Update on the runs table.

type Session struct {
	ID               int64             `json:"id"`
	PRID             int64             `json:"pr_id"`
	Role             string            `json:"role"`
	Generation       int               `json:"generation"`
	AgentName        *string           `json:"agent_name"`
	AgentKind        *string           `json:"agent_kind"`
	SessionID        *string           `json:"session_id"` // codex uuid / claude id
	ResumedFrom      *string           `json:"resumed_from"`
	HerdrWorkspaceID *string           `json:"herdr_workspace_id"`
	HerdrTabID       *string           `json:"herdr_tab_id"`
	HerdrPaneID      *string           `json:"herdr_pane_id"`
	Cwd              *string           `json:"cwd"`
	Env              map[string]string `json:"env"` // env_json
	State            string            `json:"state"`
	AgentStatus      *string           `json:"agent_status"`
	AgentStatusAt    *time.Time        `json:"agent_status_at"`
	IdleTicks        int               `json:"idle_ticks"`
	StartedAt        time.Time         `json:"started_at"`
	LastPromptAt     *time.Time        `json:"last_prompt_at"`
	ClosedAt         *time.Time        `json:"closed_at"`
}
    Session is one agent (judge, reviewer, simplifier) in a herdr pane.

type SessionUpdate struct{ Update }
    SessionUpdate is an Update on the sessions table.

type SinceReview struct {
	Source     string    `json:"source"` // SinceFromReviewed | SinceFromReview | SinceFromBase
	Base       string    `json:"base"`
	Head       string    `json:"head"`
	Commits    int       `json:"commits"`
	Files      int       `json:"files"`           // -1 when GitHub truncated the file list (300+ files)
	Additions  int       `json:"additions"`       // a lower bound when Files is -1
	Deletions  int       `json:"deletions"`       // likewise
	Error      string    `json:"error,omitempty"` // the comparison failed for good (e.g. Base is gone); counts are 0
	ComputedAt time.Time `json:"computed_at"`
	// BaseMerged: Base...Head merged the base branch BaseRef in (or was
	// rebased onto it), and the counts are the PR's own: its own commits,
	// the files whose own change differs and their own-change lines. Raw:
	// it did, but the PR's own diff could not be compared in full, so the
	// files and lines are Base...Head's, the base branch's changes
	// included; the commits are still the PR's own when both sides of its
	// own diff were read (they need only the commit lists).
	BaseMerged bool   `json:"base_merged,omitempty"`
	Raw        bool   `json:"raw,omitempty"`
	BaseRef    string `json:"base_ref,omitempty"`
	// Version is SinceReviewVersion for a size measured with the PR's own
	// diff in view; an older one of a reviewed base is measured again once.
	Version int `json:"version,omitempty"`
}
    SinceReview is prs.since_review_json: the size of Base...Head, i.e. what
    changed since the last review (or the whole PR when nothing was reviewed).

type Slot struct {
	ID                int64      `json:"id"`
	Name              string     `json:"name"`
	RepoID            *int64     `json:"repo_id"`
	RepoFullName      string     `json:"repo_full_name"`
	Kind              string     `json:"kind"`
	Path              string     `json:"path"`
	MainClone         string     `json:"main_clone"`
	PlaceholderBranch *string    `json:"placeholder_branch"`
	DBSlug            *string    `json:"db_slug"`
	State             string     `json:"state"`
	PRID              *int64     `json:"pr_id"`
	Pinned            bool       `json:"pinned"`
	DirtySchema       bool       `json:"dirty_schema"`
	CheckedOutSHA     *string    `json:"checked_out_sha"`
	HoldReason        *string    `json:"hold_reason"`
	LockSHA           *string    `json:"lock_sha"`
	LastUsedAt        *time.Time `json:"last_used_at"`
	LastError         *string    `json:"last_error"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	// SchemaFP, SchemaSHA and SchemaVersion say what a pool slot's databases
	// carry: the fingerprint of the files under the pool's schema_paths at
	// the commit they were last loaded from, that commit, and the version
	// db/schema.rb declared there. nil = unknown (the next round reloads).
	SchemaFP      *string `json:"schema_fp"`
	SchemaSHA     *string `json:"schema_sha"`
	SchemaVersion *string `json:"schema_version"`
}
    Slot is a checkout magnum reviews in (pool slot, per-PR worktree, or an
    observed external repoN checkout).

type SlotDatabase struct {
	ID          int64      `json:"id"`
	SlotID      *int64     `json:"slot_id"`
	DBName      string     `json:"db_name"`
	Slug        string     `json:"slug"`
	SizeMB      *float64   `json:"size_mb"`
	FirstSeenAt time.Time  `json:"first_seen_at"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	DroppedAt   *time.Time `json:"dropped_at"`
	DroppedBy   *string    `json:"dropped_by"`
}
    SlotDatabase is a MySQL schema seen by the inventory (SlotID nil = orphan).

type SlotFilter struct {
	RepoFullName string // case-insensitive
	Kind         string
	States       []string
}
    SlotFilter selects slots for ListSlots. Zero values match everything.

type SlotUpdate struct{ Update }
    SlotUpdate is an Update on the slots table.

type Store struct {
	// Clock returns the current time; nil means time.Now. Tests replace it.
	Clock func() time.Time

	// Has unexported fields.
}
    Store is the registry handle. It is safe for concurrent use.

func Open(path string) (*Store, error)
    Open creates the parent directory (0700), opens the database at path with
    WAL, busy_timeout=5000, foreign_keys=ON and synchronous=NORMAL, and applies
    pending migrations. It refuses a database whose schema version is newer than
    this binary knows. The database, its -wal and its -shm file are made 0600
    whatever the umask.

func OpenWith(path string, opts Options) (*Store, error)
    OpenWith is Open with options.

func (s *Store) ActiveRuns(ctx context.Context) ([]Run, error)
    ActiveRuns returns runs still in flight (pending, submitted, working,
    ended-but-unverified), oldest first.

func (s *Store) AgentTimeSince(ctx context.Context, since, now time.Time, prIDs ...int64) ([]PRAgentTime, error)
    AgentTimeSince sums, per PR, the durations of the runs created at or after
    since and counts the distinct rounds they belong to; a PR without such
    a run has no entry. A run lasts from its submission (its creation when
    it was never submitted) to its end; one still going (pending, submitted,
    working) lasts to now, and a finished run that never got an end stops where
    it was last seen (verified, else working), or lasts nothing. A duration is
    never negative. prIDs limits the PRs (none: every PR). The result is the PR
    with the most agent time first, ties by PR id.

    The durations are summed here, not in SQL: the runs' timestamps are text.

func (s *Store) AppendEvent(ctx context.Context, e Event) (int64, error)
    AppendEvent writes an audit row and returns its id. At defaults to now and
    Level to "info"; Kind and Message are required. Callers must redact secrets
    (execx.Redact) before putting command output into Message.

func (s *Store) AssignSlot(ctx context.Context, prID, slotID int64, dbNames ...string) (Assignment, error)
    AssignSlot is ClaimSlot without the PR transition: the slot moves from free
    (unpinned, no hold_reason) to claimed for prID and an open assignment is
    inserted, whatever state the PR is in (magnum open restores a parked PR's
    checkout for a human). Lost races are ErrConflict.

func (s *Store) AssignmentsByPR(ctx context.Context, prID int64) ([]Assignment, error)
    AssignmentsByPR returns the PR's assignment history, oldest first.

func (s *Store) Board(ctx context.Context, f BoardFilter) ([]BoardRow, error)
    Board returns the PRs the board shows, the latest activity first
    (BoardRow.ActivityAt; PRs never fetched last). It reads only the registry.

func (s *Store) Candidates(ctx context.Context, p CandidateParams) ([]PR, error)
    Candidates returns the PRs the dispatcher may start a round for, in dispatch
    order: forced first, then PRs where the watched user is a requested reviewer
    (review_requested), then oldest GitHub activity (gh_updated_at, falling back
    to created_at), then id.

    Every candidate is OPEN on GitHub, or MERGED and forced (a post-merge
    review: `magnum review` of a PR merged before magnum reviewed its
    last push), not muted (unless forced) and past its retry backoff
    (next_attempt_at). Within that:
      - queued PRs qualify when forced or next_eligible_at is unset or due;
      - rereview_pending PRs qualify when forced, or when next_eligible_at is
        unset or due AND the push quiet period, the (draft) minimum interval and
        the daily round cap all allow it.

func (s *Store) CheckoutSteps(ctx context.Context, prID int64, limit int) ([]Event, error)
    CheckoutSteps returns the step events (KindStep and KindStepReset) of
    the PR's checkouts, oldest first, at most limit of the newest (0 = all):
    the subjects "slot:<slot>:pr:<N>:<sha7>" of every slot the PR was assigned
    to and its per-PR worktree's own "slot:<owner>/<name>#<N>".

func (s *Store) ClaimSlot(ctx context.Context, prID, slotID int64, dbNames ...string) (Assignment, error)
    ClaimSlot atomically hands a free slot to a PR: the PR moves from
    a ClaimableStates state to claiming, the slot from free (unpinned,
    no hold_reason) to claimed with pr_id and last_used_at set, and an open
    assignment is inserted (path, db_slug from the slot; head_sha from the PR;
    dbNames recorded as db_names_json). Any lost race is ErrConflict and leaves
    every row untouched.

func (s *Store) Close() error
    Close closes the database.

func (s *Store) ClosedPastGrace(ctx context.Context, now time.Time) ([]PR, error)
    ClosedPastGrace returns closed PRs whose release_after has passed (an unset
    release_after counts as passed) and that have no active run, oldest deadline
    first.

func (s *Store) CompleteRequest(ctx context.Context, id int64, state, result string) error
    CompleteRequest marks a pending request done or failed with a result
    message. Completing a request twice is ErrConflict.

func (s *Store) CountMisses(ctx context.Context, f MissFilter) (int, error)
    CountMisses counts the misses f selects.

func (s *Store) CountNotesProposals(ctx context.Context, state string) (int, error)
    CountNotesProposals counts the proposals in state.

func (s *Store) CreateNotesProposal(ctx context.Context, in NotesProposalInput) (NotesProposal, error)
    CreateNotesProposal stores a proposal (and a curation's proposed state
    as a version of source curation, and what it did with its misses) in one
    transaction.

func (s *Store) CreateRun(ctx context.Context, r Run) (Run, error)
    CreateRun inserts a run and returns it. Role is any non-empty name. An empty
    ID is generated as "r-<UTC yyyymmddThhmmss>-<n>"; CreatedAt defaults to now.

func (s *Store) CreateSession(ctx context.Context, x Session) (Session, error)
    CreateSession inserts a session and returns it. Role is any non-empty name
    (config-defined; see RoleJudge for the defaults). Generation 0 means "next
    generation for (pr, role)"; StartedAt defaults to now; a nil Env stores {}.
    A second starting/live session for the same (pr, role) or agent name is
    ErrConflict.

func (s *Store) CreateSlot(ctx context.Context, sl Slot) (Slot, error)
    CreateSlot inserts a slot row (created_at/updated_at = now) and returns it.
    A duplicate name or path is ErrConflict.

func (s *Store) DB() *sql.DB
    DB exposes the underlying handle for ad hoc read-only queries (status views,
    tests). Writes should go through the typed methods.

func (s *Store) DecideNotesProposal(ctx context.Context, id int64, from []string, to, reason string, at time.Time, applied *NotesVersionInput) (NotesProposal, error)
    DecideNotesProposal moves proposal id from one of from to state to,
    with reason, compare-and-set (ErrConflict when it is in another state). With
    applied, the state the proposal leads to is recorded in the same transaction
    as a version of the repository (applied.RepoID and ProposalID are set here)
    and linked as the proposal's applied_version_id. The misses the proposal was
    given follow it (decideProposalMisses).

func (s *Store) DeleteKV(ctx context.Context, key string) error
    DeleteKV removes key (no error when absent).

func (s *Store) EnqueueRequest(ctx context.Context, kind string, payload any) (int64, error)
    EnqueueRequest inserts a pending request with payload marshalled as JSON
    (nil = {}) and returns its id.

func (s *Store) EventsBySubject(ctx context.Context, subject string, limit int) ([]Event, error)
    EventsBySubject returns a subject's events oldest first; limit > 0 keeps
    only the newest limit rows.

func (s *Store) EventsMatching(ctx context.Context, matches []SubjectMatch, after int64, limit int) ([]Event, error)
    EventsMatching returns the events with id > after whose subject satisfies
    any of matches, oldest first; limit > 0 keeps only the newest limit rows.
    Without any usable match it returns nil.

func (s *Store) EventsOfKindsSince(ctx context.Context, since time.Time, kinds ...string) ([]Event, error)
    EventsOfKindsSince returns the events of the given kinds at or after since,
    oldest first; no kinds returns nil.

func (s *Store) EvictableSlots(ctx context.Context, repoFullName string, now time.Time, minWarm time.Duration) ([]Slot, error)
    EvictableSlots returns the repository's held pool slots that may be taken
    back, least recently used first: not pinned, no hold_reason, unused for
    at least minWarm, PR not pinned and no human activity within minWarm,
    no active run, and no live session that is working, blocked or was prompted
    (or started) within minWarm.

func (s *Store) FindingsByPR(ctx context.Context, prID int64) ([]Finding, error)
    FindingsByPR returns the findings recorded for prID, oldest first
    (created_at, id); Repo is left empty.

func (s *Store) FindingsSince(ctx context.Context, since time.Time) ([]Finding, error)
    FindingsSince returns the findings recorded at or after since, oldest first,
    each with its PR's repository.

func (s *Store) ForgetSend(ctx context.Context, key string) error
    ForgetSend drops key's dedup record so the next ShouldSend for it sends at
    once (the condition the notification was about has cleared). Forgetting an
    unknown key is not an error.

func (s *Store) FreeSlots(ctx context.Context, repoFullName string, preferPRID int64) ([]Slot, error)
    FreeSlots returns the repository's claimable pool slots (state free,
    not pinned, no hold_reason): the slot that last held preferPRID first (0 =
    no preference), then least recently used.

func (s *Store) GetKV(ctx context.Context, key string) (string, bool, error)
    GetKV returns the value for key and whether it exists.

func (s *Store) GitHubRequiredChecks(ctx context.Context, fullName string) (GitHubRequiredChecks, bool, error)
    GitHubRequiredChecks returns the cached GitHub list of repository fullName
    ("owner/name") and whether there is one.

func (s *Store) LastReviewSummaries(ctx context.Context, prIDs []int64) (map[int64]ReviewSummary, error)
    LastReviewSummaries returns the ReviewSummary of each PR's latest posted
    round, for the PRs that have one. A result file that does not parse is
    skipped (the round still counts as posted elsewhere).

func (s *Store) LastRoleRunHead(ctx context.Context, prID int64, role string) (string, error)
    LastRoleRunHead is the head of the PR's latest completed run of role (ended
    or verified), "" when the role never completed one.

func (s *Store) LatestNotesVersion(ctx context.Context, repoID int64) (NotesVersion, error)
    LatestNotesVersion is the newest version of repoID's history (ErrNotFound
    when none was recorded).

func (s *Store) LatestRoundRuns(ctx context.Context, prIDs ...int64) (map[int64][]Run, error)
    LatestRoundRuns returns, per PR, the runs of its highest round, oldest
    first. prIDs limits the PRs; empty means every PR with a run. A PR without
    runs has no entry.

func (s *Store) ListPRs(ctx context.Context, f PRFilter) ([]PR, error)
    ListPRs returns PRs matching f ordered by repo and number.

func (s *Store) ListRepos(ctx context.Context) ([]Repo, error)
    ListRepos returns every repository ordered by owner/name.

func (s *Store) ListSlotDatabases(ctx context.Context, includeDropped bool) ([]SlotDatabase, error)
    ListSlotDatabases returns known databases ordered by name, dropped ones only
    when includeDropped.

func (s *Store) ListSlots(ctx context.Context, f SlotFilter) ([]Slot, error)
    ListSlots returns slots matching f ordered by id.

func (s *Store) LiveSessionByPRRole(ctx context.Context, prID int64, role string) (Session, error)
    LiveSessionByPRRole returns the starting/live session of a PR's role.

func (s *Store) LiveSessions(ctx context.Context) ([]Session, error)
    LiveSessions returns every starting/live session, oldest first.

func (s *Store) MarkSlotDatabaseDropped(ctx context.Context, dbName, by string) error
    MarkSlotDatabaseDropped records that dbName was dropped (by = who: a cleanup
    plan id, "cleanup", "reconcile", …).

func (s *Store) MissRejections(ctx context.Context, ids []int64) (map[int64][]string, error)
    MissRejections returns, per miss of ids, the operator's reasons of the
    rejected proposals it was in, oldest first (an empty reason is left out).

func (s *Store) Misses(ctx context.Context, f MissFilter) ([]Miss, error)
    Misses returns the misses f selects, newest first, each with its PR's
    repository and number and the latest proposal it was given to.

func (s *Store) NeedsMePRs(ctx context.Context, commentsWhenClean func(repo, identity string) bool, mine func(login string) bool) ([]NeedsMePR, error)
    NeedsMePRs lists the open PRs that need the operator (NeedsMe),
    by repository and number. commentsWhenClean tells whether an identity posts
    a comment for a clean verdict on a repository (owner/name); mine whether a
    login is one of the operator's. It reads magnum's latest round only for a PR
    whose last review is such a comment.

func (s *Store) NotesFileUses(ctx context.Context, repoID int64) ([]NotesFileUse, error)
    NotesFileUses lists repoID's harness files with their rounds and the uses
    recorded since each was first seen, by file name.

func (s *Store) NotesHistory(ctx context.Context, repoID int64, limit int) ([]NotesVersion, error)
    NotesHistory lists repoID's history, newest first, at most limit versions
    (limit <= 0 = all): every recorded version except the proposed states of
    curations, which appear as the version recorded when one was applied.

func (s *Store) NotesProposalByID(ctx context.Context, id int64) (NotesProposal, error)
    NotesProposalByID reads a proposal.

func (s *Store) NotesProposals(ctx context.Context, f NotesProposalFilter) ([]NotesProposal, error)
    NotesProposals lists the proposals f selects.

func (s *Store) NotesVersionByID(ctx context.Context, id int64) (NotesVersion, error)
    NotesVersionByID reads a version (its files without bodies).

func (s *Store) NotesVersionContent(ctx context.Context, id int64) (NotesContent, error)
    NotesVersionContent reads what version id holds: its notes text and its
    harness files with their bodies.

func (s *Store) OpenAssignment(ctx context.Context, a Assignment) (Assignment, error)
    OpenAssignment inserts an open assignment (for flows other than ClaimSlot,
    e.g. per-PR worktrees). StartedAt defaults to now. A second open assignment
    for the same slot or PR is ErrConflict.

func (s *Store) OpenAssignmentBySlot(ctx context.Context, slotID int64) (Assignment, error)
    OpenAssignmentBySlot returns the slot's open assignment.

func (s *Store) PRByID(ctx context.Context, id int64) (PR, error)
    PRByID looks up a PR by id.

func (s *Store) PRByRepoNumber(ctx context.Context, repoID int64, number int) (PR, error)
    PRByRepoNumber looks up PR #number of repository repoID.

func (s *Store) PRFilesOf(ctx context.Context, prID int64) (PRFiles, bool, error)
    PRFilesOf returns the PR's stored file list; ok is false when it has none.

func (s *Store) PRsWithFiles(ctx context.Context, repoID, except int64, mergedSince time.Time) ([]FilesPR, error)
    PRsWithFiles returns the PRs of the repository that have a file list and are
    open (drafts included) or were merged at or after mergedSince, except the PR
    except, by number.

func (s *Store) PendingRequests(ctx context.Context, limit int) ([]Request, error)
    PendingRequests returns up to limit pending requests, oldest first (limit <=
    0 = all), so a consumer can skip a request it already handed to a background
    worker.

func (s *Store) PostedFindingPaths(ctx context.Context, prIDs []int64) (map[int64]map[string]int, error)
    PostedFindingPaths returns, per PR of prIDs that has any, how many findings
    magnum posted on each path over all its rounds (findings without a path and
    rejected candidates excluded).

func (s *Store) ProposalMisses(ctx context.Context, id int64) ([]ProposalMiss, error)
    ProposalMisses lists what proposal id did with the misses it was given,
    by miss id, each with the miss as it is now.

func (s *Store) Prune(ctx context.Context, keepEvents, keepRequests time.Duration) (PruneResult, error)
    Prune deletes events older than keepEvents and handled requests (state done
    or failed, never pending) created more than keepRequests ago, both measured
    from the store's clock, in one transaction. A keep of zero or less disables
    pruning of that table. It is meant for the daemon's maintenance pass:
    nothing else ever removes these rows.

    One kind of old event is kept: the step rows of a subject's current
    generation (kind step, and the step.reset row that opened it), because
    StepDone reads them to resume a sequence, and only while the subject is in
    use, that is while it has an event of any kind inside the window. A subject
    quiet for the whole window is not mid-sequence (subjects carry the head's
    sha, so a finished checkout never comes back), and its step rows are pruned
    like any other; a sequence that did run again would repeat its steps, which
    package steps already requires to be idempotent. Superseded generations are
    pruned by age as before.

func (s *Store) RecordFindings(ctx context.Context, runID string, prID int64, round int, fs []Finding) error
    RecordFindings replaces the findings recorded for runID with fs in one
    transaction (each row gets runID, prID and round; CreatedAt defaults to
    now), so recording a round's result again is harmless. Every finding
    needs a FindingID unique within fs and a verdict of FindingPosted or
    FindingRejected.

func (s *Store) RecordNotesRound(ctx context.Context, repoID int64, files []string, at time.Time) error
    RecordNotesRound counts a judge round of repoID for the harness files
    present now: each counts one more round, a new one starts at one (first seen
    at), and the rows of files that are gone are dropped (a file that comes back
    starts over).

func (s *Store) RecordNotesUsage(ctx context.Context, repoID int64, runID string, files []string, at time.Time) error
    RecordNotesUsage records that run runID used files of repoID's harness (once
    per file and run).

func (s *Store) RecordNotesVersion(ctx context.Context, in NotesVersionInput) (NotesVersion, bool, error)
    RecordNotesVersion records in's state as a version of its repository:
    the notes text gzip-compressed (or NULL when an earlier version holds
    the same text), the harness files by path and each body once per content
    (harness_blobs). It reports whether a version was added (false: Dedupe found
    the latest version identical).

func (s *Store) RecordRetroPR(ctx context.Context, r RetroPR) error
    RecordRetroPR stores r as the retro record of r.PRID, replacing an
    earlier one. A zero RetroAt is now and an empty Day is DayKey(RetroAt);
    Status must be one of the Retro* values. An empty Error is stored as NULL.
    Attempts is not taken from r: a failed status adds one to the PR's count,
    any other sets it to 0.

func (s *Store) ReleaseSlot(ctx context.Context, slotID int64, from []string, to, reason string) error
    ReleaseSlot atomically moves slot slotID from one of from to state to,
    clears its pr_id and closes its open assignment (if any) with reason.

func (s *Store) RepoByFullName(ctx context.Context, fullName string) (Repo, error)
    RepoByFullName looks up "owner/name" case-insensitively.

func (s *Store) RepoByID(ctx context.Context, id int64) (Repo, error)
    RepoByID looks up a repository by id.

func (s *Store) RequestByID(ctx context.Context, id int64) (Request, error)
    RequestByID looks up a request by id.

func (s *Store) RequiredChecks(ctx context.Context, fullName string, configured []string) (RequiredChecks, error)
    RequiredChecks returns the checks PRs of repository fullName must pass:
    configured (config.Config.RequiredChecks) when it is not empty, else
    GitHub's as the poller last read them (Source "" and no checks when GitHub
    would not tell or was never asked). GitHub requiring nothing is Source
    RequiredFromGitHub with no checks.

func (s *Store) RetroDue(ctx context.Context, q RetroQuery) ([]PR, error)
    RetroDue lists the PRs due for a retro, newest closed first: merged or
    closed, closed (closed_at, else merged_at) at or after q.Since and,
    with q.Until, at or before it, unless q.PRIDs names the PRs, with at least
    one run that posted a review, and unless q.Again without a retro record,
    or with a failed one of fewer than RetroMaxAttempts attempts.

func (s *Store) RetroPRByID(ctx context.Context, prID int64) (RetroPR, error)
    RetroPRByID returns the retro record of prID, or an error matching
    ErrNotFound when the PR has none.

func (s *Store) RetroSettling(ctx context.Context, q RetroQuery) (int, error)
    RetroSettling counts the PRs that would be due for q's retro but closed
    after q.Until: they wait for the settle delay. 0 without Until or with
    PRIDs.

func (s *Store) RoleRanBefore(ctx context.Context, prID int64, role string) (bool, error)
    RoleRanBefore reports whether a PR already has an ended or verified run
    of role, the "runs = first" test: a role configured to run once per PR
    is skipped after its first completed run. Pending, in-flight, failed and
    abandoned runs do not count.

func (s *Store) RunByID(ctx context.Context, id string) (Run, error)
    RunByID looks up a run by id.

func (s *Store) RunsByPR(ctx context.Context, prID int64) ([]Run, error)
    RunsByPR returns every run of a PR, oldest first.

func (s *Store) RunsOfRoundsSince(ctx context.Context, since time.Time) ([]RoundRun, error)
    RunsOfRoundsSince returns every run of each round (pr_id, round) that has
    a run created at or after since, oldest first, with its PR's repository and
    number. A round that began before since and went on after it comes whole,
    so the caller can tell when it began.

func (s *Store) SchemaChanged(ctx context.Context) (bool, error)
    SchemaChanged reports whether PRAGMA user_version differs from the value
    this Store saw at Open, i.e. whether another binary migrated the database
    underneath it. A long-running process (the daemon) calls it once per tick
    and restarts when it returns true. It is one cheap read, no write lock.

func (s *Store) SchemaVersion(ctx context.Context) (int, error)
    SchemaVersion returns PRAGMA user_version.

func (s *Store) SessionByID(ctx context.Context, id int64) (Session, error)
    SessionByID looks up a session by id.

func (s *Store) SessionsByPR(ctx context.Context, prID int64) ([]Session, error)
    SessionsByPR returns every session of a PR, oldest first.

func (s *Store) SetGitHubRequiredChecks(ctx context.Context, fullName string, g GitHubRequiredChecks) error
    SetGitHubRequiredChecks caches g for repository fullName.

func (s *Store) SetKV(ctx context.Context, key, value string) error
    SetKV stores value under key. A key that holds value already is left alone,
    updated_at included: the daemon sets the same keys every tick, and nothing
    reads updated_at (a heartbeat such as daemon.last_tick carries its time in
    the value, so it is written every time). Never store secrets here.

func (s *Store) SetRepoClonePath(ctx context.Context, repoID int64, path string) error
    SetRepoClonePath records where the repository's main clone lives; "" clears
    a path that turned out not to be a clone (UpsertRepo can only keep or
    replace it).

func (s *Store) ShouldSend(ctx context.Context, key string, window time.Duration) (bool, error)
    ShouldSend is the notification dedup gate: it returns true (and records
    the send) when key was never sent or was last sent at least window ago;
    otherwise it counts the suppressed occurrence and returns false.
    notifications.count is the number of occurrences since the last send,
    including that send.

func (s *Store) SlotByID(ctx context.Context, id int64) (Slot, error)
    SlotByID looks up a slot by id.

func (s *Store) SlotByName(ctx context.Context, name string) (Slot, error)
    SlotByName looks up a slot by name ("review3").

func (s *Store) SlotByPR(ctx context.Context, prID int64) (Slot, error)
    SlotByPR returns the PR's current slot: the slot row (pool or per-PR,
    any state but removed) whose pr_id is prID, or ErrNotFound. The unique index
    slots_pr allows at most one row per PR, so there is never a choice to make.

func (s *Store) StepDone(ctx context.Context, subject, step string) (bool, error)
    StepDone reports whether a step event with phase ok exists for (subject,
    step) after the subject's latest KindStepReset row (its current step
    generation). See package steps.

func (s *Store) TransitionPR(ctx context.Context, id int64, from []string, to string, set func(*PRUpdate)) error
    TransitionPR moves PR id to state `to` only if its current state is one
    of from (nil from = any state; empty to = keep the state), applying set's
    assignments in the same UPDATE. It returns ErrConflict when the state or a
    condition added with Where did not match and ErrNotFound when the PR does
    not exist.

func (s *Store) TransitionRun(ctx context.Context, id string, from []string, to string, set func(*RunUpdate)) error
    TransitionRun is TransitionPR for runs.

func (s *Store) TransitionSession(ctx context.Context, id int64, from []string, to string, set func(*SessionUpdate)) error
    TransitionSession is TransitionPR for sessions.

func (s *Store) TransitionSlot(ctx context.Context, id int64, from []string, to string, set func(*SlotUpdate)) error
    TransitionSlot is TransitionPR for slots.

func (s *Store) UpdateAssignmentHead(ctx context.Context, assignmentID int64, sha string) error
    UpdateAssignmentHead records the commit an open assignment's checkout now
    holds (a later round checked out a newer head in the same slot). An unknown
    id is ErrNotFound; an ended assignment is ErrConflict.

func (s *Store) UpdatePR(ctx context.Context, id int64, set func(*PRUpdate)) error
    UpdatePR changes non-state columns of PR id.

func (s *Store) UpdateRun(ctx context.Context, id string, set func(*RunUpdate)) error
    UpdateRun changes non-state columns of run id.

func (s *Store) UpdateSession(ctx context.Context, id int64, set func(*SessionUpdate)) error
    UpdateSession changes non-state columns of session id.

func (s *Store) UpdateSlotFields(ctx context.Context, id int64, set func(*SlotUpdate)) error
    UpdateSlotFields changes non-state columns of slot id.

func (s *Store) UpsertMiss(ctx context.Context, m Miss) (Miss, error)
    UpsertMiss stores m by SourceURL and returns the stored row. A new miss is
    inserted (an empty State is MissNew); an existing one is updated in place
    and keeps its id, state and created_at, so running the retro again never
    brings back a dismissed or used miss. Every other column takes m's value,
    except that an unclassified m (a retro whose classifier failed) never
    replaces a class an earlier retro set, nor what came with it (severity,
    title, lesson, scope, lines, match). Empty Class is MissUnclassified,
    empty Raised is MissRaisedNone, nil Lines and Match are empty lists, and ""
    or 0 in an optional column is stored as NULL. PRID, SourceURL, SourceKind,
    Reviewer and ReviewedSHA are required.

func (s *Store) UpsertPRFromGitHub(ctx context.Context, in GitHubPR) (PRUpsert, error)
    UpsertPRFromGitHub inserts a PR (state InitialState, head_changed_at now)
    or updates only its GitHub-derived columns. It never touches the automation
    columns (state, throttles, missing_since, …): the engine decides transitions
    from the returned flags. A no-op refresh writes nothing.

func (s *Store) UpsertRepo(ctx context.Context, r Repo) (Repo, error)
    UpsertRepo inserts or refreshes a repository keyed by NodeID and returns the
    stored row. last_seen_at becomes now; first_synced_at is only ever set once;
    a nil ClonePath keeps the stored one; an empty DefaultBranch means "master"
    and an empty WatchOwner means Owner.

func (s *Store) UpsertSlotDatabase(ctx context.Context, d SlotDatabase) (SlotDatabase, error)
    UpsertSlotDatabase records that MySQL holds d.DBName: first_seen_at is set
    once, last_seen_at = now, a nil SlotID or SizeMB keeps the stored value,
    and a database seen again after a drop is no longer marked dropped.

type SubjectMatch struct {
	Exact  string
	Prefix string
}
    SubjectMatch selects events by subject for EventsMatching. A non-empty Exact
    matches a subject equal to it; a non-empty Prefix matches a subject starting
    with it. Both are case-sensitive and compare bytes, so "slot:review3:"
    never matches "slot:Review3:x", and no character in them is a wildcard.
    Set both to match either; an empty SubjectMatch matches nothing.

type Update struct {
	// Has unexported fields.
}
    Update collects column assignments for one row, and optional extra guards
    on the same UPDATE (Where). Column names are the schema's; id, state,
    created_at and updated_at cannot be set (state changes go through the
    Transition* `to` argument, updated_at is maintained by the store). The first
    invalid call is remembered and returned by the store method that applies the
    update.

    Accepted values: nil (NULL), string, bool, int, int64, float64, time.Time
    (the zero time stores NULL), pointers to those (nil pointer = NULL),
    []string, map[string]string, []LatestReview, []ReviewRequest, SinceReview
    and *SinceReview (stored as JSON; a nil *SinceReview stores NULL) and
    json.RawMessage.

func (u *Update) Copy(dst, src string)
    Copy sets column dst to the row's current value of src (read before the
    update, so Copy("prev_state", "state") records the state being left).

func (u *Update) Inc(col string, n int)
    Inc adds n to the integer column col.

func (u *Update) Set(col string, v any)
    Set assigns v to column col.

func (u *Update) Where(col string, v any)
    Where requires column col to equal v (a nil v requires NULL) for the UPDATE
    to match, in the same statement as the id and state guard. When the row
    exists but the condition fails, the store method returns ErrConflict,
    like a state mismatch, so a caller can require that the row still describes
    what it decided on (for example head_sha == the target sha). col may be any
    column of the table; conditions combine with AND.

```

## textx

```text
package textx // import "github.com/zhuravel/magnum/internal/textx"

Package textx holds the small text helpers several packages share: clipping to a
number of runes, the first line, a short commit SHA, plurals and login folding.
It imports only the standard library.

FUNCTIONS

func Clip(s string, n int) string
    Clip cuts s to at most n runes: a longer s becomes its first n-1 runes,
    spaces at their end dropped, and an ellipsis. n <= 0 means no limit.
    Normalizing s (trimming, collapsing spaces) is the caller's.

func Count(n int, one, many string) string
    Count is n and its noun: "1 file", "3 files".

func FirstLine(s string) string
    FirstLine is the first line of s that is not blank, trimmed.

func FoldLogin(s string) string
    FoldLogin folds a GitHub login for comparison: case, surrounding spaces,
    a leading "@" and a "[bot]" suffix do not matter, so "@Talkable[bot]" and
    "talkable" fold the same.

func MatchLogins(logins []string) func(login string) bool
    MatchLogins returns a test of whether a login is one of logins, as FoldLogin
    folds them; an empty login matches none.

func Plural(n int, one, many string) string
    Plural is one when n is 1, otherwise many.

func ShortSHA(sha string) string
    ShortSHA is a commit SHA cut to 7 characters; a shorter string is kept.

```

## tui

```text
package tui // import "github.com/zhuravel/magnum/internal/tui"

Package tui holds magnum's interactive terminal screens, built on Bubble Tea:
the status dashboard (RunDashboard), the PR board (RunPRBoard), the PR picker
(RunPicker), the cleanup plan review (RunCleanupPlan) and the read-only pane
mirror (RunWatch).

Every screen takes plain data structs and small interfaces defined here,
so the cli adapts its own types into them; the plain-text renderings stay
in the cli for output that is not a terminal (IsTerminal tells them apart).
Run* functions block until the user leaves the screen or ctx ends; an ended ctx
is not an error (the screen just closes). The dashboard and the PR board switch
to each other on tab: they return ErrSwitchToBoard or ErrSwitchToDashboard and
the caller runs the other screen.

Built on Charm v2 (charm.land/bubbletea/v2, bubbles/v2, lipgloss/v2).
Styles use the 16 ANSI colors only, in a light and a dark variant picked from
the background the terminal reports (tea.RequestBackgroundColor); every state
that color shows is also spelled out in text, so the screens read the same on a
monochrome terminal. No screen needs a mouse, but the board and the dashboard
take one (mouse.go): the wheel scrolls, a click selects, a double click opens,
a heading click sorts (the board), a heading gap drags a column wider and a
right click opens the row's actions, each through its key's path; m turns it off
and on.

CONSTANTS

const (
	RequestPending = "pending"
	RequestDone    = "done"
	RequestFailed  = "failed"
)
    The states of a Request.

const (
	NeedsMeApprove = "approve"
	NeedsMeLift    = "lift"
)
    PRBoardRow.NeedsMe values (store.NeedsMe's).


VARIABLES

var (
	ErrSwitchToBoard     = errors.New("tui: switch to the PR board")
	ErrSwitchToDashboard = errors.New("tui: switch to the status dashboard")
)
    The errors RunDashboard and RunPRBoard return when the user pressed tab to
    switch to the other screen: the caller runs that screen next (with the same
    source and actions it already holds). A caller that does not switch treats
    them as any other error, so it sees them only if it asked for both screens.


FUNCTIONS

func DeltaCheckPhrase(lines int) string
    DeltaCheckPhrase names a delta check of lines changed code lines: "delta
    check (4 lines)".

func HumanAgo(d time.Duration) string
    HumanAgo renders how long ago something happened: "12s ago". Zero or a
    negative duration means it never happened and renders as "never".

func HumanBytes(n int64) string
    HumanBytes renders a byte count the way the plain-text status does: "0",
    "812K", "34M", "1.3G".

func HumanDuration(d time.Duration) string
    HumanDuration renders a duration compactly: "42s", "7m", "3h12m", "2d4h".
    A negative duration renders as its absolute value.

func IsTerminal(f *os.File) bool
    IsTerminal reports whether f is a terminal (the termios ioctl answers), so
    a caller picks a screen of this package over plain-text output. /dev/null,
    pipes and regular files are not terminals.

func RenderPRBoard(rows []PRBoardRow, width int, opts PRBoardOptions) string
    RenderPRBoard renders the board once, for output that is not interactive and
    for tests: the title bar, the state summary, the column headings and every
    row of opts.DefaultView and opts.DefaultOwner (in opts.DefaultSort order,
    largest first), fitted to width (0 = no limit). It uses the dark palette and
    no cursor; ages count from opts.Now.

func RunDashboard(ctx context.Context, src DashboardSource, act DashboardActions, opts DashboardOptions) error
    RunDashboard shows the live status dashboard until the user quits or
    ctx ends. act may be nil (the action keys then say so). It returns
    ErrSwitchToBoard when the user pressed tab (or t) for the PR board.

func RunPRBoard(ctx context.Context, src PRBoardSource, act DashboardActions, opts PRBoardOptions) error
    RunPRBoard shows the live PR board until the user quits or ctx ends. act may
    be nil (the action keys then say so). It returns ErrSwitchToDashboard when
    the user pressed tab for the status dashboard.

func RunWatch(ctx context.Context, fetch WatchFetch, opts WatchOptions) error
    RunWatch mirrors fetch's frames full-screen, read-only, until the user
    quits (q, esc or ctrl+c), ctx ends (both return nil) or a fetch returns a
    StopWatch error (RunWatch returns the wrapped error).

func StageDuration(d time.Duration) string
    StageDuration renders a stage's length: "12s", "18m04s", "1h02m".

func StopWatch(err error) error
    StopWatch wraps err so that RunWatch ends and returns err (for "the pane
    closed" or "herdr is gone"). StopWatch(nil) is nil.

func TimingsText(t RoundTimings) string
    TimingsText is t on one line: "fetch/checkout 12s · claude-review 18m04s
    · … · total 34m10s", a running stage marked "(running)", a failed one
    "(failed)".


TYPES

type ActionLog struct {
	// Has unexported fields.
}
    ActionLog is the outcomes of the screens' last actions, newest last,
    with the requests among them the daemon has not answered yet. The board
    and the dashboard share one (tab keeps it), so a request queued on one
    screen flashes on the other when the answer comes. Safe for concurrent use;
    the zero value is ready.

func NewActionLog() *ActionLog
    NewActionLog returns an empty log.

type ActionResult struct {
	// Text is everything the action printed: the footer shows its last line,
	// the action log all of it.
	Text string
	// Requests are the daemon requests it queued, as last read.
	Requests []Request
}
    ActionResult is what a finished action reports.

type ActivityInfo struct {
	LastPoll, LastTick, LastReconcile time.Duration
}
    ActivityInfo is how long ago the daemon last polled GitHub, ticked and
    reconciled; zero (or negative) means never.

type AgentsInfo struct {
	CodexWorking, CodexMax, ClaudeWorking int
	Other                                 []KindCount // other agent kinds the roles use, shown after claude
	Error                                 string
}
    AgentsInfo counts working agent panes; a non-empty Error ("herdr
    unreachable") replaces the counts.

type AttentionRow struct {
	Subject, Kind, Message string
	Fix                    string // optional
}
    AttentionRow is something that needs the user.

type Badge struct {
	Label, Text string
	// Color is one of red, green, yellow, blue, magenta, cyan or gray ("" =
	// the terminal's; an emoji keeps its own colors).
	Color string
}
    Badge marks a PR that carries a GitHub label: Text (e.g. "🚩") shows before
    the title, Label names it on the card.

type CIInfo struct {
	// State is "passed" | "failed" | "pending" | "skipped" (every check
	// skipped: none ran) | "none" (no checks).
	State                                   string
	Total, Passed, Failed, Pending, Skipped int
	// Failing names the failed checks, "workflow / job" when the workflow is known.
	Failing []string
	// Workflows summarises the checks per GitHub Actions workflow ("" = checks outside a workflow).
	Workflows []WorkflowCI
	// Required are the repository's required checks with their state: Name
	// as GitHub or [[repo]] required_checks names it ("Completion",
	// "workflow:CI"), State "passed" | "failed" | "pending" | "skipped" |
	// "missing" (never ran on this head).
	Required []CheckState
	// RequiredSource says who requires them: "github" (the repository's
	// rulesets) or "config" ([[repo]] required_checks overrides them).
	RequiredSource string
	Stale          bool // describes an older commit than the PR's head
}
    CIInfo is the head commit's CI as the board shows it.

type CheckState struct {
	Name, Label, State string
	Done, Total        int
}
    CheckState is one required check and its state: Name as configured ("ci /
    *", "workflow:CI"), Label the short name the board's cell shows ("" = Name),
    Total the checks it matched on the head and Done those that finished (Total
    0: none, or the head's are not known yet).

func (c CheckState) Count() string
    Count is "done/total" for a passed or pending required check that matched
    several checks ("3/3", "1/3"); "" otherwise (one check, none, or a failure
    the card names).

type CleanupAction struct {
	ID      string // stable id the caller maps back to its own action
	Kind    string // release_slot, remove_slot, drop_db, reset_external, ...
	Subject string // owner/name#N, slot:<name>, slug:<slug>, path:<dir>
	Why     string
	Bytes   int64 // disk freed (0 = none or unknown)
	DBBytes int64 // MySQL space freed
	DBNames []string
	// NeedsTypedConfirm, when non-empty, is the exact text (for example a
	// slug) the user must type before the plan is applied.
	NeedsTypedConfirm string
}
    CleanupAction is one thing the plan would do.

type CleanupOutcome struct {
	Apply       bool     // true only after the user confirmed (and typed every required text)
	SelectedIDs []string // in plan order; empty when cancelled
}
    CleanupOutcome is what the user decided.

func RunCleanupPlan(ctx context.Context, plan CleanupPlan) (CleanupOutcome, error)
    RunCleanupPlan shows the plan full-screen so the user can pick actions,
    confirm them and type any required texts. It returns Apply false when the
    user cancelled or ctx ended.

type CleanupPlan struct {
	Actions []CleanupAction
	Skipped []CleanupSkip
	Totals  CleanupTotals // whole-plan estimates, shown for reference
}
    CleanupPlan is what the cleanup screen reviews: the actions the planner
    proposes, the things it left alone, and whole-plan estimates.

type CleanupSkip struct{ Subject, Reason string }
    CleanupSkip is something the planner left alone, with the reason.

type CleanupTotals struct{ Disk, MySQL int64 }
    CleanupTotals are whole-plan estimates, in bytes.

type CodexPace struct {
	Used int // percent
	Cap  float64
	At   time.Time
}
    CodexPace is the Codex budget used now and when, at the pace it has
    been spent since its window began, it reaches Cap ([usage] codex_soft,
    or codex_hard once past it), before the window resets.

type ColumnWidths interface {
	LoadWidths(ctx context.Context, screen string) (map[string]int, error)
	SaveWidths(ctx context.Context, screen string, widths map[string]int) error
}
    ColumnWidths keeps the column widths dragged with the mouse across runs,
    per screen ("board", "dashboard") and column name. LoadWidths returns nil
    when nothing is kept; SaveWidths with an empty map forgets them. The screens
    call both from commands, so a slow store never blocks a key.

type DaemonFacts struct {
	// SkewOld is the build the daemon runs when it is older than the CLI's
	// or the one on disk ("v1.4.0", "dev (8ad5bb2)"), SkewSince when that
	// daemon started and SkewNew what is built, as a phrase ("v1.5.0 built",
	// "new build 18:48"); SkewOld is "" when the daemon runs the newest
	// build.
	SkewOld, SkewNew string
	SkewSince        time.Time
	// Paused: `magnum pause` holds automation since PausedSince (zero when
	// unknown), holding Held review requests people made.
	Paused      bool
	PausedSince time.Time
	Held        int
	// Draining: `magnum daemon-restart --drain` holds new rounds; DrainerPID
	// is the draining command (0 when unknown).
	Draining   bool
	DrainerPID int
	// NeedsMe counts the open PRs magnum approved that GitHub still blocks
	// on the operator's approval (PRBoardRow.NeedsMe).
	NeedsMe int
	// Codex is the Codex budget's pace when it reaches a cap before the
	// window resets; nil otherwise.
	Codex *CodexPace
	// NotesProposals counts the curation proposals for repository notes
	// that wait for the operator (`magnum notes <repo> --review`).
	NotesProposals int
}
    DaemonFacts are what the titles say of the daemon; the zero value says
    nothing.

type DaemonInfo struct {
	Running bool
	PID     int
	Uptime  string // preformatted ("3h12m"); empty when unknown
	Launchd string // launchd job state ("running", "not loaded")
	// Skew says the daemon runs an older build than the CLI's or the one on
	// disk ("daemon runs v1 since …; v2 is built: `magnum daemon-restart`");
	// "" when it does not.
	Skew string
}
    DaemonInfo is the daemon process and its launchd job.

type DashboardActions interface {
	Open(ctx context.Context, ref string) (ActionResult, error)
	Review(ctx context.Context, ref string, opts ReviewOpts) (ActionResult, error)
	Pin(ctx context.Context, ref string) (ActionResult, error)
	Unpin(ctx context.Context, ref string) (ActionResult, error)
	Release(ctx context.Context, ref string) (ActionResult, error)
	Mute(ctx context.Context, ref string) (ActionResult, error)
	Unmute(ctx context.Context, ref string) (ActionResult, error)
	Abort(ctx context.Context, ref string) (ActionResult, error)  // kill the PR's running review
	Ignore(ctx context.Context, ref string) (ActionResult, error) // abort, mute and free the slot
	// Approve and RequestChanges post the reviewer's own verdict on the head
	// magnum reviewed (magnum approve / request-changes).
	Approve(ctx context.Context, ref string) (ActionResult, error)
	RequestChanges(ctx context.Context, ref string) (ActionResult, error)
	Attention(ctx context.Context) (ActionResult, error)
	OpenBrowser(ctx context.Context, url string) error
	// Requests re-reads the requests ids name; one the registry no longer
	// has is left out.
	Requests(ctx context.Context, ids []int64) ([]Request, error)
}
    DashboardActions run what the dashboard's and the board's keys ask for.
    ref is a row's PR reference (or a slot name, for pin/unpin/release of
    an empty slot). The result's text is shown in the footer (only its last
    non-empty line when it has several) and kept whole in the action log;
    an error is shown instead, until a key is pressed. A request the result
    names as pending marks its row and is re-read (Requests) on every refresh
    until the daemon answers. One action runs at a time; the data refreshes
    right after it returns.

type DashboardOptions struct {
	Refresh    time.Duration    // between Gather calls; default 2s, at least 200ms
	ShowManual bool             // start with the manual worktrees section shown
	Title      string           // default "magnum status"
	Now        func() time.Time // clock for "updated Xs ago"; default time.Now
	Judge      string           // the judge role's name in the help ("open the PR's <Judge> pane"); default "judge"
	Icons      IconMode         // the symbols: unicode (default), nerd (Nerd Font icons and emoji) or ascii
	// NoMouse starts with mouse support off ([terminal] mouse = false);
	// m turns it on and off either way.
	NoMouse bool
	// MouseToggled, when set, hears every m, so the next screen can start
	// the same way.
	MouseToggled func(on bool)
	// Widths keeps the column widths dragged with the mouse across runs;
	// nil keeps them for this run only.
	Widths ColumnWidths
	// Log keeps the actions' outcomes (! shows them) and the requests still
	// pending; the board shares it, so tab keeps both. nil = a log of this
	// screen's own.
	Log *ActionLog
}
    DashboardOptions tune the dashboard.

type DashboardSource interface {
	Gather(ctx context.Context) (StatusData, error)
}
    DashboardSource supplies the dashboard's data; Gather is called once per
    refresh, never concurrently with itself.

type DiskInfo struct {
	FreeGB float64
	MinGB  int
}
    DiskInfo is the free space where slots live; FreeGB <= 0 means unknown.

type FindingsInfo struct {
	Counts          [4]int // P0..P3 findings posted
	Simplifications int    // optional simplification suggestions posted
	Fixed, Open     int    // earlier findings fixed / still open (a re-review)
	Answered        int    // earlier findings answered with a reason
	// Verdict is the review's decision whatever its repository lets it
	// post: blocking (request changes), non_blocking (comment) or clean
	// (approve).
	Verdict string
	Posted  string // the event it posted: APPROVE, REQUEST_CHANGES or COMMENT
	SHA     string // the reviewed head
}
    FindingsInfo is what magnum's latest posted review of a PR concluded.

type GitHubInfo struct {
	Remaining, Limit int
	ResetIn          time.Duration
}
    GitHubInfo is the GraphQL rate budget; Limit 0 means unknown (no poll yet).

type IconMode string
    IconMode is the set of symbols the PR board and the status dashboard draw
    ([terminal] icons). A screen cannot tell which font the terminal uses,
    so the configuration decides; nothing is detected.

const (
	// IconsUnicode draws Unicode symbols (✔ ✗ 💬 📌) that any font has: the
	// default, also for an empty or unknown mode.
	IconsUnicode IconMode = "unicode"
	// IconsNerd draws Nerd Font icons and emoji: a colored marker before
	// every state, verdict and finding priority, an icon in every state
	// pill and before the section headings. Needs a Nerd Font.
	IconsNerd IconMode = "nerd"
	// IconsASCII draws plain ASCII ("+", "x", "pin").
	IconsASCII IconMode = "ascii"
)
type KindCount struct {
	Kind    string
	Working int
}
    KindCount is the number of working panes of one agent kind.

type ManualRow struct {
	Folder, Branch, PRRef, GitHub, DBs, Agents, Disk string
}
    ManualRow is a manual worktree (read-only, not selectable).

type PRBoardOptions struct {
	Refresh time.Duration    // between Rows calls; default 5s, at least 200ms
	Title   string           // default "magnum · pull requests"
	Now     func() time.Time // clock for ages and the refresh time; default time.Now
	// SelfLogins are the logins that count as "me": the user and magnum's
	// own reviewer (e.g. "zhuravel", "talkable[bot]"). Case, a leading "@"
	// and a "[bot]" suffix do not matter.
	SelfLogins  []string
	DefaultSort PRSort // default SortUpdated
	DefaultView PRView // the view the board opens in; default ViewAll
	// DefaultOwner is the owner scope the board opens in; "" (the default)
	// shows every owner, as does an owner without PRs.
	DefaultOwner string
	// OwnerChanged, when set, hears every change of the owner scope (O, or
	// its owner's last PR leaving), so the next board can open in it.
	OwnerChanged func(owner string)
	// ViewChanged, when set, hears every v, so the next board can open in
	// the same view.
	ViewChanged func(PRView)
	Repo        string // show only this repository ("name" or "owner/name"); empty shows all
	// DefaultRepo is daemon.default_repo ("owner/name"): its owner comes
	// first when O cycles the owners.
	DefaultRepo string
	Icons       IconMode // the symbols: unicode (default), nerd (Nerd Font icons and emoji) or ascii
	Judge       string   // the judge role's name in the help ("open the <Judge> pane"); default "judge"
	// NoMouse starts with mouse support off ([terminal] mouse = false);
	// m turns it on and off either way.
	NoMouse bool
	// NoShimmer keeps the state cells of the PRs that need the operator
	// still ([board] shimmer = false); by default their colors slide.
	NoShimmer bool
	// HideSkipped starts the board with the ignored and skipped PRs hidden
	// (h toggles it); HideToggled, when set, hears every h so the choice
	// can be kept.
	HideSkipped bool
	HideToggled func(hide bool)
	// MouseToggled, when set, hears every m, so the next screen can start
	// the same way.
	MouseToggled func(on bool)
	// Widths keeps the column widths dragged with the mouse across runs;
	// nil keeps them for this run only.
	Widths ColumnWidths
	// RecentClosed is [board] recent_closed, the window the heading of the
	// recently closed section names ("merged or closed in the last 24h");
	// the source marks the rows in it (PRBoardRow.Recent).
	RecentClosed time.Duration
	// Log keeps the actions' outcomes (! shows them) and the requests still
	// pending; the dashboard shares it, so tab keeps both. nil = a log of
	// this screen's own.
	Log *ActionLog
	// Facts, when set, reads what the title says of the daemon (an older
	// build, a pause, a drain, the Codex budget's pace) with every load.
	Facts func(ctx context.Context) DaemonFacts
}
    PRBoardOptions tune the PR board.

type PRBoardRow struct {
	Ref, Owner, Repo   string
	Number             int
	Title, Author, URL string
	// Issue is the first issue key in the title a [board] trackers template
	// knows ("PS-38553"), IssueURL its page; "" when the title names none.
	Issue, IssueURL   string
	Draft             bool
	Labels, Assignees []string
	// State is magnum's state: baseline, queued, reviewing, reviewed,
	// rereview_pending, needs_attention, paused, closed, released,
	// ineligible (the configuration skips it; the board says "skipped") or
	// ignored (magnum ignore).
	State string
	// SkipReason says why the configuration skips an ineligible PR ("bot
	// author", `author "x" is in skip_authors`, ...).
	SkipReason string
	// Badges are the PR's labels that [board] badges marks, in its order.
	Badges  []Badge
	GHState string // GitHub's state: OPEN, CLOSED, MERGED
	// ActivityAt is the PR's last activity, which the UPDATED column, the
	// updated sort and the card show: a push, a comment, a review, a label,
	// a review request, a draft change, a rename, a description edit, a base
	// change, a close, reopen or merge; GitHubUpdatedAt until magnum read it.
	ActivityAt time.Time
	// GitHubUpdatedAt is GitHub's updatedAt, which also moves for what a
	// reviewer never sees (a project field, a resolved thread); prs --json
	// only.
	GitHubUpdatedAt time.Time
	HeadSHA         string
	LastReview      *ReviewInfo // the latest review magnum knows of; nil when none
	// Findings is what magnum's latest posted review concluded (its findings
	// by priority, simplifications and verdict), also where it could only
	// comment; nil when magnum has not reviewed the PR.
	Findings *FindingsInfo
	// CI is the head commit's checks; nil when unknown.
	CI             *CIInfo
	Reviewers      []ReviewerInfo // everyone who reviewed or was asked to
	SinceReview    *ReviewDelta   // what changed since the last review; nil when unknown
	Slot           string         // folder of the review slot holding the PR, if any
	Pinned, Muted  bool
	Notes          bool      // the repository has reviewer notes (magnum notes)
	NextEligibleAt time.Time // earliest next automatic review; zero when not scheduled
	// LastError is the one-line explanation of the PR's stored error (for a
	// PR in needs_attention, why it needs you); ErrorFix the next step and
	// ErrorDetail the end of the failing command's output.
	LastError   string
	ErrorFix    string
	ErrorDetail []string
	RoundsToday int
	LastRound   *RoundTimings // the stages of the last review round; nil when none ran
	// RoundWhy says which roles the last round ran and why; nil when unknown.
	RoundWhy *RoundWhy
	// Progress is the round in flight: its start and its roles, which the
	// state cell turns into the stage and the time ("simplify · 17m") and
	// the card into a timeline; nil when no round runs or it is unknown.
	Progress *RoundProgress
	// Spend is the agent time the PR's runs took over the last 7 days and
	// how many rounds they ran in; nil when none ran.
	Spend *SpendInfo

	// Wait and WaitDetail say why a PR waiting for a round has none yet (the
	// daemon's account): the compact form the state cell shows ("re-review
	// · quiet → 14:09") and the sentence with the command that lifts it,
	// which the card shows. "" when the PR does not wait or no daemon said.
	Wait, WaitDetail string
	// DeltaCheck: the round the PR waits for is a delta check (the judge
	// alone on a small delta); the state cell says so, as it does for a
	// round in flight whose RoundWhy is one.
	DeltaCheck bool
	// Note is a one-line remark about the last review shown under LAST REVIEW
	// on the card (e.g. "comment-only push skipped (a7b3f8c → 602da9d)").
	Note string

	// RequestedToMe is the latest review request that asked one of the self
	// logins; LastRequest the latest one whoever it asked; Requests the
	// latest request to each reviewer, newest first. Nil or empty when the
	// registry knows none (only the newest ten requests of a PR are kept).
	RequestedToMe, LastRequest *RequestInfo
	Requests                   []RequestInfo

	// ClosedAt is when GitHub merged the PR, else closed it; zero while it
	// is open. Recent marks a PR GitHub merged or closed within [board]
	// recent_closed: the board lists it in a section after the open PRs.
	ClosedAt time.Time
	Recent   bool
	// MergedUnreviewed: GitHub merged the PR before magnum reviewed its last
	// push (store.IsMergedUnreviewed); LastReview.CommitSHA is the commit
	// magnum reviewed last, if any.
	MergedUnreviewed bool
	// FlagDismissed: the PR is muted without being flagged though it would
	// be flagged unmuted (store.IsFlagDismissed): a mute dismissed the
	// merged-unreviewed flag, and M restores it.
	FlagDismissed bool
	// NeedsMe: magnum approved the PR's head, but GitHub, which never counts
	// a GitHub App's approval, still blocks it on the operator
	// (store.NeedsMe): NeedsMeApprove while it requires an approval that
	// counts, NeedsMeLift while the operator's own changes request is the
	// only one blocking it; "" otherwise. The state cell says "✓ needs you"
	// or "✓ lift your ✗" and the updated sort lists it first.
	NeedsMe string
	// ReviewDecision is GitHub's reviewDecision: APPROVED,
	// CHANGES_REQUESTED or REVIEW_REQUIRED; "" when the base branch requires
	// no review or magnum has not read it yet. prs --json only.
	ReviewDecision string
}
    PRBoardRow is one pull request on the PR board. Ref is what actions receive;
    Owner, Repo and Number label the row (Ref is parsed when they are empty).

func FilterPRBoard(rows []PRBoardRow, v PRView, selfLogins []string) []PRBoardRow
    FilterPRBoard returns the rows of rows in view v; selfLogins are the logins
    that count as "me" (PRBoardOptions.SelfLogins).

func SortPRBoard(rows []PRBoardRow, by PRSort, desc bool) []PRBoardRow
    SortPRBoard returns a copy of rows in the given order; desc puts the
    largest key first (newest update, latest review, most recent verdict,
    newest request, most lines changed, most urgent state). Rows lacking the key
    (never reviewed, no request, no delta) come last either way; ties put the
    newest update first, then order by ref. An unknown sort means SortUpdated.
    The recently closed rows (Recent) come after all the others, newest closed
    first, whatever the sort: they are the board's own section. The updated
    sort, the board's default, lists the PRs that need the operator's approval
    (NeedsMe) first, either way, each part in its order.

type PRBoardSource interface {
	Rows(ctx context.Context) ([]PRBoardRow, error)
}
    PRBoardSource supplies the board's rows; Rows is called once per refresh,
    never concurrently with itself.

type PRBoardSourceFunc func(ctx context.Context) ([]PRBoardRow, error)
    PRBoardSourceFunc adapts a function to PRBoardSource.

func (f PRBoardSourceFunc) Rows(ctx context.Context) ([]PRBoardRow, error)
    Rows calls f.

type PRRow struct {
	Ref, Title, Author, State, Next, Age, URL string
	Review                                    *ReviewFacts // for the y/N question before a review; nil when unknown
	GHState                                   string       // GitHub's state: OPEN, CLOSED or MERGED; "" when unknown
	// MergedUnreviewed: GitHub merged the PR before magnum reviewed its last
	// push; FlagDismissed: the PR was muted after that, so it is not flagged
	// (store.IsMergedUnreviewed, store.IsFlagDismissed). M dismisses the one
	// and restores the other.
	MergedUnreviewed, FlagDismissed bool
}
    PRRow is one queued (or closing) PR; Ref is what actions receive.

type PRSort string
    PRSort orders the board.

const (
	SortUpdated          PRSort = "updated"           // newest update first
	SortLastReview       PRSort = "last-review"       // latest review first
	SortReviewerActivity PRSort = "reviewer-activity" // latest verdict by anyone first
	SortRequested        PRSort = "requested"         // latest review request first
	SortChanges          PRSort = "changes"           // most lines changed since the review first
	SortState            PRSort = "state"             // most urgent state first
)
    The board's sorts, in the order s cycles through them.

func PRSorts() []PRSort
    PRSorts lists the sorts in the order the s key cycles through them.

func ParsePRSort(s string) (PRSort, error)
    ParsePRSort reads a sort name such as "updated" or "last-review" (case,
    "_" and spaces do not matter); empty means SortUpdated.

type PRView string
    PRView is a preset subset of the board's rows.

const (
	ViewAll    PRView = "all"    // every row
	ViewMagnum PRView = "magnum" // rows magnum reviewed or is reviewing: state not baseline or ineligible
	ViewMine   PRView = "mine"   // assigned to one of the self logins, or their review is requested
	ViewReady  PRView = "ready"  // open, not a draft, approved on the head, nothing blocking, required checks passed
)
    The board's views, in the order v cycles through them.

func PRViews() []PRView
    PRViews lists the views in the order the v key cycles through them.

func ParsePRView(s string) (PRView, error)
    ParsePRView reads a view name ("all", "magnum", "mine", "ready"; case does
    not matter); empty means ViewAll.

type Pause struct {
	Key    string // scope: daemon, codex, claude, watch:<owner>, identity:<name>, github
	Reason string // why, with "until" already folded in when it applies
	Fix    string // command or step that lifts it; optional
}
    Pause is one reason automation (or part of it) is held back.

type PickAction int
    PickAction is what the user chose to do with the picked PR.

const (
	PickActionCancel    PickAction = iota
	PickActionReview               // enter
	PickActionFresh                // ctrl+f: review with new sessions
	PickActionOpen                 // ctrl+g: open the judge pane
	PickActionBrowser              // ctrl+o: open the PR URL
	PickActionTogglePin            // ctrl+p: pin, or unpin when Entry.Pinned
	PickActionRelease              // ctrl+x
)
    The picker's actions; PickActionCancel is the zero value. ctrl+r (and F5)
    refreshes the list, as on every screen.

func (a PickAction) String() string

type PickEntry struct {
	Ref    string // what actions receive: owner/name#N (repo#N labels work too)
	Title  string
	Author string
	State  string // magnum state, with flags ("reviewed,pinned")
	Age    string // since the last activity ("3h")
	URL    string
	Pinned bool         // ctrl+p means unpin when true
	Review *ReviewFacts // for the y/N question before a review; nil when unknown
	// GHState is GitHub's state of the PR: OPEN, CLOSED or MERGED; "" when
	// unknown. A MERGED one is asked about as a post-merge review.
	GHState string
}
    PickEntry is one PR the picker lists.

type PickOutcome struct {
	Action PickAction
	Entry  *PickEntry
	Query  string // the filter text as the user left it
}
    PickOutcome is the picker's result. Entry is nil when the action is Cancel,
    or when the query matched no entry but reads as a PR reference (URL,
    owner/repo#N, repo#N, #N or N): the caller resolves Query then.

func RunPicker(ctx context.Context, entries []PickEntry, opts PickerOptions) (PickOutcome, error)
    RunPicker lets the user filter entries and pick one with an action. It
    returns a Cancel outcome (and nil error) when the user cancels or ctx ends.

type PickerOptions struct {
	Query string           // initial filter; a PR URL or reference narrows to that PR
	Title string           // default "magnum pick"
	Now   func() time.Time // clock for "reviewed 2h ago" in the y/N question; default time.Now
	// Lookup resolves a typed reference the list lacks to a PR magnum knows,
	// e.g. a merged one, so the y/N question can say what it is; nil means none.
	Lookup func(query string) (PickEntry, bool)
	// Reload lists the PRs again for ctrl+r and F5 (the registry may have
	// changed since the picker opened); nil means the keys say so instead.
	Reload func(ctx context.Context) ([]PickEntry, error)
}
    PickerOptions tune the picker.

type Request struct {
	ID     int64
	Kind   string // review, pin, release, …
	State  string // RequestPending, RequestDone or RequestFailed
	Result string // the daemon's answer; "" while pending
}
    Request is a daemon request as the screens follow it.

type RequestInfo struct {
	To   string // the reviewer: a login (a bot's keeps "[bot]") or "team:<slug>"
	By   string // who asked; "" when unknown (a deleted account)
	At   time.Time
	Mine bool // To is one of the self logins
}
    RequestInfo is a review request: who was asked, by whom and when.

type ReviewDelta struct {
	Base                                 string
	BaseSHA                              string
	Commits, Files, Additions, Deletions int
	Truncated                            bool // the counts are lower bounds
	// MergedBase is the base branch the commits since merged in (or were
	// rebased onto) when the counts leave its changes out (the PR's own);
	// Raw: they merged it, but the counts include its changes (the PR's
	// own diff could not be compared in full). RawBase names it then.
	MergedBase, RawBase string
}
    ReviewDelta is what changed on a PR since Base: "reviewed" (the last
    reviewed head) or "base branch" (no review yet: the whole PR).

type ReviewFacts struct {
	HeadSHA     string
	ReviewedSHA string    // the last reviewed head; "" when never reviewed
	ReviewedAt  time.Time // zero when unknown
	ReviewedBy  string    // who posted that review; "" when unknown
	// SinceReview is what changed since ReviewedSHA (Base "reviewed");
	// nil when unknown, e.g. not counted yet for the current head.
	SinceReview *ReviewDelta
}
    ReviewFacts is what the y/N question before a review says about a PR on
    the dashboard and in the picker (the board reads its own rows): the head,
    the last reviewed head and the commits since it.

type ReviewInfo struct {
	Login, Event string // Event: APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED (any case)
	SubmittedAt  time.Time
	CommitSHA    string
	Stale        bool // the head moved since
	Mine         bool // Login is one of the self logins
}
    ReviewInfo is the latest review on a PR.

type ReviewOpts struct {
	Fresh    bool // new agent sessions instead of resuming
	Simplify bool // run the role aliased simplify this round (magnum review --simplify)
}
    ReviewOpts are the review variants the screens ask for. A forced round
    reviews the head even when it was reviewed already, so there is no "again"
    variant.

type ReviewerInfo struct {
	Login       string
	Verdict     string // approved | changes_requested | commented | dismissed | pending
	SubmittedAt time.Time
	CommitSHA   string
	Stale       bool // the head moved since the verdict
	Requested   bool // a review is requested from them
	Mine        bool
}
    ReviewerInfo is one reviewer's latest verdict on a PR.

type RoleProgress struct {
	Role, Label string
	Judge       bool
	// OwnPass: the judge's own pass, its runs of kind own_pass, listed as an
	// entry of its own before the judge's.
	OwnPass         bool
	Started, Ended  time.Time
	Working, Failed bool
}
    RoleProgress is one role of a round in flight. Started is when its run was
    prompted (zero: not started yet) and Ended when it ended (zero while it
    works); Working: its run is submitted or working; Failed: it failed or was
    abandoned. Label is the short name the state cell gives it while it works
    (the shortest of its name and aliases, see the cli's stageLabel); the judge
    is "judge" whatever its names.

type RoleRerun struct {
	Role  string
	Lines int
}
    RoleRerun is a role that ran again because Lines code lines changed since
    its last run.

type RoundProgress struct {
	StartedAt time.Time
	Roles     []RoleProgress
}
    RoundProgress is a round in flight: when it started (the PR's
    last_round_started_at) and its roles, the ones with a run in the order
    their runs were created, then the ones the round named that have none yet,
    the judge last and its own pass right before it.

type RoundTimings struct {
	Round   int
	Kind    string        // the round's kind: initial, rereview, continue, recovery
	Stages  []StageTiming // checkout, then each role in the order it started, then verify
	Total   time.Duration // the first stage's start to the last one's end (to now while running)
	Running bool          // the round is still in flight
}
    RoundTimings is how long each stage of a PR's last review round took,
    from the registry's runs and step events.

type RoundWhy struct {
	Kind      string // initial, rereview, continue, recovery, nudge
	PostMerge bool   // a post-merge review
	// DeltaCheck: a re-review of DeltaLines changed code lines by the judge
	// alone (a delta check).
	DeltaCheck bool
	DeltaLines int
	Roles      []string // the roles it ran, in the order the round named them
	Requested  []string // the roles asked for this round
	Reruns     []RoleRerun
	// Triaged: triage decided this round's roles; Skipped are the roles it
	// dropped and Reason its words (the model read the PR: PR content,
	// cleaned like any). EveryRole is why triage kept every role ("the diff
	// could not be read"); all empty when triage did not run.
	Triaged   bool
	Skipped   []string
	Reason    string
	EveryRole string
	// At is when the round started (its engine.round_start event).
	At time.Time
}
    RoundWhy says which roles a PR's last round ran and why: its kind (a
    continue runs the judge alone), the roles asked for it (`magnum review
    --role`, --simplify), the roles that ran again because their code changed
    (rerun_min_lines), and triage's decision.

type RoundsInfo struct {
	Active, Max int
	ActivePRs   []string
	Progress    []*RoundProgress
}
    RoundsInfo is the review rounds in progress. Progress[i] is the round
    of ActivePRs[i], which the rounds line follows with its stage and time
    ("talkable#1 · simplify · 17m"); nil, or missing, when unknown.

type SlotRow struct {
	Name, Folder, PRRef, PRState, SlotState, DBs, Disk string
	URL                                                string // PR URL for b; optional (looked up in Queue by PRRef)
	PRGHState                                          string // GitHub's state of the slot's PR: OPEN, CLOSED or MERGED; "" when unknown
	// PRMergedUnreviewed and PRFlagDismissed are PRRow's MergedUnreviewed and
	// FlagDismissed for the slot's PR; read with PRGHState.
	PRMergedUnreviewed, PRFlagDismissed bool
}
    SlotRow is one review slot. Actions on a slot row target PRRef, or the slot
    Name for pin/unpin/release when it holds no PR.

type SourceFunc func(ctx context.Context) (StatusData, error)
    SourceFunc adapts a function to DashboardSource.

func (f SourceFunc) Gather(ctx context.Context) (StatusData, error)
    Gather calls f.

type SpendInfo struct {
	Window    time.Duration // how far back it counts (7 days)
	AgentTime time.Duration
	Rounds    int
}
    SpendInfo is what a PR's reviews cost over a window: the agent time (the sum
    of its runs' durations, to now for a run still going) and how many rounds
    those runs belong to.

type StageTiming struct {
	Name     string        // "fetch/checkout", a role's name, the judge's own pass ("codex-judge own pass"), "verify"
	Duration time.Duration // to now while Running
	Running  bool
	Failed   bool
}
    StageTiming is one stage of a round.

type StatusData struct {
	Daemon    DaemonInfo
	Activity  ActivityInfo
	GitHub    GitHubInfo
	Rounds    RoundsInfo
	Agents    AgentsInfo
	Disk      DiskInfo
	Pauses    []Pause
	Slots     []SlotRow
	Queue     []PRRow // open PRs, then closed ones pending release
	Attention []AttentionRow
	Manual    []ManualRow // manual worktrees, shown when toggled on
	Warnings  []string    // sources that could not be read
	// Notes sums the repository notes up ("4 repos · 2 proposals to review
	// (...) · 1 over limit"); "" when no repository has notes.
	Notes string
	// Facts are what the title says of the daemon: an older build, a pause,
	// a drain, the Codex budget's pace.
	Facts       DaemonFacts
	GeneratedAt time.Time
}
    StatusData is one snapshot of what the dashboard shows. Table cells are
    display strings the caller formats (refs, folders, sizes); the header
    numbers are formatted by the dashboard.

type WatchFetch func(ctx context.Context) (WatchFrame, error)
    WatchFetch reads the current frame. It must honor ctx, which has a deadline
    (five intervals, at least 10s): a hung fetch then shows as an error instead
    of freezing the frame. An error shows in the header and the last good frame
    stays, unless it was wrapped with StopWatch, which ends the screen.

type WatchFrame struct {
	Title  string // e.g. "talkable#11920"
	Role   string // the session's role name (config.Role.Name), e.g. codex-judge
	Pane   string // herdr pane id
	Status string // agent status as the caller reports it ("working", "idle", ...)
	Text   string // pane contents
	ANSI   bool   // Text carries ANSI styling to pass through; false strips escape sequences
}
    WatchFrame is one snapshot of the pane being mirrored.

type WatchOptions struct {
	Interval time.Duration    // between fetches; default 2s, minimum 200ms
	Now      func() time.Time // clock for the header age; default time.Now
}
    WatchOptions tunes RunWatch.

type WorkflowCI struct {
	Name, State                    string
	Passed, Failed, Pending, Total int
}
    WorkflowCI is one workflow's checks on the head commit.

```

## usage

```text
package usage // import "github.com/zhuravel/magnum/internal/usage"

Package usage reads how much of an agent CLI's subscription budget is used.

Codex writes a token_count event into its session file
($CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl) after every turn; the event
carries the account's rate-limit windows as Codex last saw them:

    {"timestamp":"…","type":"event_msg","payload":{"type":"token_count",
     "rate_limits":{"primary":{"used_percent":7.0,"window_minutes":10080,
     "resets_at":1791710538},"secondary":null,"plan_type":"pro"}}}

Codex reads the newest of those snapshots without starting Codex or calling
any API. Decide turns a snapshot into a soft/hard verdict for the scheduler.
Everything here is read-only.

CONSTANTS

const (
	// MaxFiles is how many session files, newest modification first, are
	// searched for a snapshot.
	MaxFiles = 8
	// MaxTailBytes is how far from its end a session file is read.
	MaxTailBytes = 4 << 20
)
    Bounds of one Codex read.


VARIABLES

var ErrNoData = errors.New("usage: no Codex rate-limit snapshot found")
    ErrNoData means no rate-limit snapshot was found: no session directory,
    no session files, or none of the newest files carries one.


FUNCTIONS

func DefaultCodexHome() string
    DefaultCodexHome is $CODEX_HOME, or ~/.codex when it is unset.


TYPES

type Level int
    Level is a scheduling verdict on a snapshot.

const (
	// OK means below the soft threshold.
	OK Level = iota
	// Soft means at or above the soft threshold: defer optional work (first
	// reviews), keep what is already promised.
	Soft
	// Hard means at or above the hard threshold: start nothing that needs
	// this agent kind.
	Hard
)
func Decide(snap Snapshot, soft, hard float64) Level
    Decide compares snap.Used() with the thresholds (percentages). A threshold
    of zero or less is off.

func (l Level) String() string

type Pace struct {
	Used     float64   // percent of the budget used, 0–100
	Start    time.Time // when the window began: ResetsAt minus its length
	ResetsAt time.Time
	Now      time.Time
}
    Pace is how fast a rate-limit window's budget is being spent: the share used
    against the share of the window elapsed.

func PaceOf(used float64, windowMinutes int, resetsAt, now time.Time) (Pace, bool)
    PaceOf is the pace at now of a window windowMinutes long that resets at
    resetsAt, with used percent of it used. ok is false when the length or the
    reset is unknown (<= 0, zero) or now is not inside the window (before its
    start, or at/after the reset).

func (p Pace) Elapsed() float64
    Elapsed is the share of the window elapsed, 0–100.

func (p Pace) Ratio() float64
    Ratio is Used over Elapsed: 1 spends the whole budget exactly at the reset,
    2.7 runs out at 37% of the window. 0 when nothing elapsed.

func (p Pace) Reach(pct float64) (time.Time, bool)
    Reach is when the used share reaches pct at the current pace (the average
    since the window began: Start + (Now-Start)*pct/Used). ok is false when it
    is reached already (Used >= pct), nothing is used yet (Used <= 0), pct <= 0,
    nothing has elapsed to extrapolate from, or the time is not before ResetsAt.

type Snapshot struct {
	Window
	Secondary *Window
	// Plan is the account's plan_type ("pro", "plus", …); empty when absent.
	// Sessions under different logins can report different plans; the
	// newest snapshot wins whatever its plan.
	Plan string
	// At is the timestamp of the event the snapshot came from.
	At time.Time
	// Path is the session file it came from.
	Path string
}
    Snapshot is the newest rate-limit report found. The embedded Window is
    Codex's primary limit; Secondary is the other one when Codex reports two
    (older Codex versions report a 5-hour primary and a weekly secondary).

func Codex(ctx context.Context, home string, now time.Time) (Snapshot, error)
    Codex returns the newest rate-limit snapshot in home's session files (home
    is a CODEX_HOME; empty means DefaultCodexHome). It looks at the MaxFiles
    most recently modified rollout files, reads each from its end (at most
    MaxTailBytes), skips malformed lines and token_count events without rate
    limits, and returns the snapshot with the latest timestamp. Windows whose
    reset time is at or before now read 0% used. It returns ErrNoData when
    nothing is found.

func (s Snapshot) Used() float64
    Used is the higher used percentage of the primary and secondary windows:
    the budget that runs out first.

type Window struct {
	// UsedPercent is the share of the window's budget used, 0–100. Codex
	// reports it as of At; a window whose ResetsAt has passed reads 0.
	UsedPercent float64
	// WindowMinutes is the window's length (10080 for the weekly limit).
	WindowMinutes int
	// ResetsAt is when the window resets; zero when Codex did not say.
	ResetsAt time.Time
}
    Window is one rate-limit window as Codex reported it.

```
