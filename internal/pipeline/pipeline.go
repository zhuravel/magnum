// Package pipeline runs one review round for a PR inside its herdr workspace:
// a readiness step in the checkout (the repository's prepare commands and
// ready probes and a Ruby check, readiness.go; failures only inform the
// judge), then the round's configured roles (config.Role: agent sessions on any configured
// kind, or shell commands) in stages (config.Config.Stages: the roles of a
// stage in parallel, git-diff roles one at a time after them, each followed
// by a tree restore), then the judge, then verification on GitHub (the oracle
// for "review posted") and the optional dismissal of the identity's own stale
// CHANGES_REQUESTED. RolesToRun says which roles a round runs; with the
// built-in roles that is claude-review and codex-review in parallel, then
// claude-simplify (its first round, or when requested), then codex-judge.
//
// RunRound blocks until the round ends and is meant to run in its own
// goroutine. It records every transition on the round's run rows and appends
// audit events (subject "pr:<owner>/<name>#<N>"), but never changes the PR's
// state: the engine maps RoundResult.Outcome onto the PR state machine.
//
// Completion of an agent turn is read from the store: the engine's per-tick
// agents.Observe moves a run to ended after two idle ticks, and this package
// polls the run rows (every PollInterval) until then, or until the role's
// timeout. The judge's turn also ends when its result file has been present
// for ResultSettle.
package pipeline

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Round kinds (the run kinds of the round's run rows).
const (
	KindInitial  = store.RunInitial  // first review of the PR
	KindRereview = store.RunRereview // new head after an earlier review by the same judge session
	KindContinue = store.RunContinue // a pause (usage limit) ended: the judge finishes its turn; the other roles do not run
	KindRecovery = store.RunRecovery // a fresh judge session after the old one was lost
)

// Round outcomes (RoundResult.Outcome and the judge run's outcome column).
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

// Role report statuses (RoleReport.Status and the non-judge runs' outcome
// column). Besides these, a status can be a health kind found in the pane:
// "login_required", "usage_limit", "overloaded", "stalled", "blocked",
// "trust_dialog".
const (
	ReportOK          = "ok"           // report written (non-empty)
	ReportEmpty       = "empty"        // finished without content (a git-diff role: no changes)
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

// Timing.
const (
	DefaultPollInterval = 10 * time.Second
	// ResultSettle is how long the judge's result file must be present before
	// it ends the turn when the agent has not gone idle (status flicker).
	ResultSettle = 2 * time.Minute
	// InterruptWait bounds the wait for an interrupted agent (a timed-out
	// judge, a cancelled or timed-out git-diff role) to be seen idle; the
	// engine's Observe records agent statuses every tick.
	InterruptWait = 60 * time.Second
)

// ErrInvalid marks a RoundInput or Runner that cannot run a round.
var ErrInvalid = errors.New("pipeline: invalid round")

// Agents is the subset of *agents.Manager a round uses. The caller must have
// started the sessions of the roles that run (RolesToRun: EnsureWorkspace,
// EnsurePane, StartAgent) before RunRound; a role without a live session is
// reported as no_session.
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
}

// GitHub is the subset of *github.Client a round uses. It must act as the
// round's Identity (Env from Identity.Env), because DismissReview and
// UpdateReviewBody write.
type GitHub interface {
	ReviewsWithMarker(ctx context.Context, owner, repo string, number int, marker string) ([]github.Review, error)
	ReviewREST(ctx context.Context, owner, repo string, number int, id int64) (github.RESTReview, error)
	DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) error
	UpdateReviewBody(ctx context.Context, owner, repo string, number int, reviewID int64, body string) error
}

// Git is the subset of *gitx.Client the tree restore after a git-diff role
// uses, and a restart's check that a head is not an older one (MergeBase).
type Git interface {
	RevParse(ctx context.Context, dir, ref string) (string, error)
	SwitchDetach(ctx context.Context, dir, ref string) error
	Status(ctx context.Context, dir string) (gitx.Status, error)
	MergeBase(ctx context.Context, dir, a, b string) (string, error)
}

// Keys interrupts a timed-out role (esc to an agent, ctrl+c twice to the
// judge, ctrl+c to a shell role's pane). *herdr.Client satisfies it.
type Keys interface {
	AgentSendKeys(ctx context.Context, target string, keys ...string) error
	PaneSendKeys(ctx context.Context, paneID string, keys ...string) error
}

// Runner runs review rounds. One Runner serves one identity; it holds no
// per-round state and is safe for concurrent RunRound calls.
type Runner struct {
	Agents Agents
	GitHub GitHub       // as Identity; unused under RoundInput.DryRun
	Git    Git          // git-diff roles, and restarts (nil = every new head counts as a push)
	Exec   execx.Runner // git-diff roles only: git diff / checkout / clean in the slot
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
}

// PreviousReview is the reviewer's earlier review of the PR.
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

// RoundInput describes one round. The slot is already checked out at
// TargetSHA (re-read the slot after slots.Checkout: CheckedOutSHA is the
// round's target).
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
	// DeltaCheck (rereview): the round is a delta check: Roles hold the
	// judge alone, which reviews the small delta since its last review
	// (JudgeData.DeltaCheck); nil = an ordinary round.
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
}

// Switched is the checkout after RoundInput.Switch.
type Switched struct {
	TargetSHA   string // the commit now checked out (a fetch may find a newer one); "" = the one asked for
	BaseSHA     string // its merge base with the base ("" = unknown: the round keeps the old one)
	ForcePushed bool   // the previous review's commit is not an ancestor of TargetSHA
	BaseMerged  bool   // the commits since the previous review have a merge commit (RoundInput.BaseMerged)
}

// Pause asks the engine to pause an agent kind.
type Pause struct {
	Kind   string    // usage_limit | login_required | overloaded
	Tool   string    // the agent kind to pause (config.Role.AgentKind): codex, claude, droid, ...
	Until  time.Time // usage_limit reset when the text names one; zero = apply the fallback backoff
	Detail string    // the matching pane line (redacted)
}

// RoleReport is what one non-judge role produced.
type RoleReport struct {
	Role    string // the role's name
	Kind    string // the role's agent kind (config.Role.AgentKind), whose pause a health status asks for; "" = none
	Capture string // the role's capture (config.CaptureFile, CaptureStdout, CaptureGitDiff)
	RunID   string
	Status  string // Report* or a health kind
	Path    string // the report file (a patch for git-diff roles) when Status is ok
	Detail  string
	Health  *agents.Health // pane classification when it explains the status
}

// RoundResult is the round's verdict. The engine decides the PR state from
// Outcome (and Pause).
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

	Pause   *Pause
	Reports map[agents.Role]RoleReport // by role name: every non-judge role the round ran (or skipped as logged out)
	Nudged  bool

	DismissedReviewID int64 // the stale CHANGES_REQUESTED review dismissed after posting
	Warnings          []string
	Error             string
}

// RunRound runs one round and blocks until it ends. The error is non-nil only
// when Outcome is error (the round could not run or be evaluated) or stopped
// because ctx was cancelled; every other outcome, including timeouts and
// pauses, returns a nil error. Run rows created but never prompted are
// abandoned; runs already sent stay submitted/working when ctx is cancelled
// (they are observed, never re-sent).
func (r *Runner) RunRound(ctx context.Context, in RoundInput) (RoundResult, error) {
	rd, err := r.newRound(ctx, in)
	if err != nil {
		return RoundResult{Outcome: OutcomeError, Error: execx.Redact(err.Error())}, err
	}
	defer rd.abandonPending(ctx)
	return rd.run(ctx)
}

// RolesToRun returns the roles a round of kind runs for pr, in the order of
// roles (the round's candidates in [[role]] order, judge included; nil =
// cfg.RolesFor(nil)): for KindContinue only the judge; otherwise the judge
// plus every role whose Runs allows it (config.Role.ShouldRun), where
// ranBefore is store.RoleRanBefore and requested whether requested names the
// role (by name or alias, case ignored). Only the first judge of roles is
// kept. The engine uses it to lay out panes and start agents, RunRound to
// pick the roles it runs, so both agree.
func RolesToRun(ctx context.Context, st *store.Store, cfg *config.Config, pr store.PR, roles []config.Role, requested []string, kind string) ([]config.Role, error) {
	if roles == nil {
		if cfg == nil {
			return nil, fmt.Errorf("%w: no roles and no config", ErrInvalid)
		}
		roles = cfg.RolesFor(nil)
	}
	var out []config.Role
	judged := false
	for _, r := range roles {
		if r.Judge {
			if !judged {
				out, judged = append(out, r), true
			}
			continue
		}
		if kind == KindContinue {
			continue
		}
		asked := slices.ContainsFunc(requested, r.Matches)
		ran := false
		if r.Runs == config.RunsFirst && !asked {
			if st == nil {
				return nil, fmt.Errorf("%w: role %s (runs first) needs the store", ErrInvalid, r.Name)
			}
			var err error
			if ran, err = st.RoleRanBefore(ctx, pr.ID, r.Name); err != nil {
				return nil, fmt.Errorf("pipeline: roles of pr %d: %w", pr.Number, err)
			}
		}
		if r.ShouldRun(ran, asked) {
			out = append(out, r)
		}
	}
	return out, nil
}

// round is the state of one RunRound call.
type round struct {
	r  *Runner
	in RoundInput
	pr store.PR // in.PR at TargetSHA, as the posting identity

	owner, name string
	subject     string
	dir         string
	login       string // reviewer login, REST form
	expectBot   bool
	idCfg       config.Identity
	ghDir       string

	judge  config.Role     // the round's judge
	order  []config.Role   // the candidate non-judge roles, in stage order
	stages [][]config.Role // the non-judge roles that run, by stage
	// unverified is the marker of an earlier judge run on the target whose
	// review magnum could not verify and did not find before the round
	// (unverified.go): the judge's review carries it, so the judge finds
	// that review before posting another. A restart on a newer head drops it.
	unverified string

	mu   sync.Mutex
	res  RoundResult
	runs []string // run ids this round created
	// cont is each role's latest continuation run on a fallback model
	// (modelFallback): the run a restart must settle in place of the one
	// the stage started with. Guarded by mu; reset by a restart.
	cont   map[string]store.Run
	start  time.Time
	health map[string]*config.HealthRegexps // compiled health patterns by agent kind (nil = the defaults)
	ready  agents.Readiness                 // the readiness step's outcome (guarded by mu; zero = none ran)

	// Restarts on a newer head (restart.go). restarts, restartedFrom, dir
	// and the target fields of in and pr change only between stages, when
	// no role runs.
	restarts      int                     // restarts so far
	restartedFrom string                  // the head the last restart left ("" = none)
	seenHead      string                  // the PR row's head as last seen (guarded by mu)
	stageCancel   context.CancelCauseFunc // cancels the running stages on a push (guarded by mu; nil = none)
}

func (r *Runner) newRound(ctx context.Context, in RoundInput) (*round, error) {
	switch {
	case r.Agents == nil || r.Store == nil || r.Config == nil || r.Identity == nil:
		return nil, fmt.Errorf("%w: runner needs Agents, Store, Config and Identity", ErrInvalid)
	case in.Kind != KindInitial && in.Kind != KindRereview && in.Kind != KindContinue && in.Kind != KindRecovery:
		return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalid, in.Kind)
	case in.TargetSHA == "" || in.SlotPath == "" || in.PR.ID == 0 || in.PR.URL == "":
		return nil, fmt.Errorf("%w: PR (with URL), TargetSHA and SlotPath are required", ErrInvalid)
	case !in.DryRun && r.GitHub == nil:
		return nil, fmt.Errorf("%w: GitHub is required unless DryRun", ErrInvalid)
	}
	if in.Roles == nil {
		in.Roles = r.Config.RolesFor(nil)
	}
	toRun, err := RolesToRun(ctx, r.Store, r.Config, in.PR, in.Roles, in.Requested, in.Kind)
	if err != nil {
		return nil, err
	}
	var judge config.Role
	for _, x := range toRun {
		if x.Judge {
			judge = x
		}
		if x.Capture == config.CaptureGitDiff && (r.Git == nil || r.Exec == nil) {
			return nil, fmt.Errorf("%w: role %s (capture git-diff) needs Git and Exec", ErrInvalid, x.Name)
		}
	}
	if judge.Name == "" {
		return nil, fmt.Errorf("%w: no judge among the round's roles", ErrInvalid)
	}
	// The daemon's judges read the copy of their skill it took at startup
	// (config.Config.SnapshotPrompts), not the file as it is now.
	judge.Skill = r.Config.SkillFile(config.SkillPath(judge.Skill, r.Layout))
	repo := in.Repo
	if repo.Owner == "" || repo.Name == "" {
		if repo, err = r.Store.RepoByID(ctx, in.PR.RepoID); err != nil {
			return nil, fmt.Errorf("pipeline: repo of pr %d: %w", in.PR.Number, err)
		}
		in.Repo = repo
	}
	if in.Round <= 0 {
		runs, err := r.Store.RunsByPR(ctx, in.PR.ID)
		if err != nil {
			return nil, fmt.Errorf("pipeline: rounds of pr %d: %w", in.PR.Number, err)
		}
		n := 0
		for _, x := range runs {
			n = max(n, x.Round)
		}
		in.Round = n + 1
	}
	pr := in.PR
	pr.HeadSHA = in.TargetSHA
	pr.Identity = r.Identity.Name()

	idCfg := r.identityConfig()
	login := r.Identity.Login()
	rd := &round{
		r: r, in: in, pr: pr,
		owner: repo.Owner, name: repo.Name,
		subject:   fmt.Sprintf("pr:%s/%s#%d", repo.Owner, repo.Name, in.PR.Number),
		dir:       r.Layout.ReviewDir(repo.Owner, repo.Name, in.PR.Number, in.TargetSHA),
		login:     login,
		expectBot: r.Identity.Kind() == "app" || strings.HasSuffix(strings.ToLower(login), "[bot]"),
		idCfg:     idCfg,
		judge:     judge,
		start:     r.now(),
		health:    map[string]*config.HealthRegexps{},
		seenHead:  cmp.Or(in.DispatchedHead, in.PR.HeadSHA),
	}
	rd.plan(toRun)
	rd.res = RoundResult{Round: in.Round, ReportDir: rd.dir, TargetSHA: in.TargetSHA, Reports: map[agents.Role]RoleReport{}}
	return rd, nil
}

// plan orders the candidate non-judge roles by stage (config.Config.Stages
// over in.Roles, so After between candidates holds even when one of them
// does not run this round) and keeps, per stage, the ones in toRun.
func (rd *round) plan(toRun []config.Role) {
	var cands []config.Role
	for _, x := range rd.in.Roles {
		if !x.Judge {
			cands = append(cands, x)
		}
	}
	if len(cands) == 0 {
		return
	}
	runs := map[string]bool{}
	for _, x := range toRun {
		runs[x.Name] = true
	}
	// A Config holding only the candidates stages exactly them.
	for _, stage := range (&config.Config{Roles: cands}).Stages(nil) {
		var keep []config.Role
		for _, x := range stage {
			rd.order = append(rd.order, x)
			if runs[x.Name] {
				keep = append(keep, x)
			}
		}
		if len(keep) > 0 {
			rd.stages = append(rd.stages, keep)
		}
	}
}

// running lists the non-judge roles that run, in stage order.
func (rd *round) running() []config.Role {
	var out []config.Role
	for _, stage := range rd.stages {
		out = append(out, stage...)
	}
	return out
}

// preflight checks the login of every agent kind the round uses, the
// judge's first, once per kind and environment (Agents.PreflightRole: a role
// may point its CLI at another account): without the judge the round cannot
// post, so nothing is prompted (stop ends the round); a logged-out kind of
// other roles only skips the roles checked in that environment, which are
// returned by name with a login_required report. A shell command without a
// tool is not checked.
func (rd *round) preflight(ctx context.Context) (loggedOut map[string]bool, stop func() (RoundResult, error)) {
	loggedOut = map[string]bool{}
	checked := map[string]error{} // kind + env -> PreflightRole's error
	for _, x := range append([]config.Role{rd.judge}, rd.running()...) {
		kind := x.AgentKind()
		if kind == "" || kind == config.KindShell {
			continue
		}
		key := kind + "\x00" + envKey(rd.r.Config.RoleEnv(x))
		err, seen := checked[key]
		if !seen {
			err = rd.r.Agents.PreflightRole(ctx, x)
			checked[key] = err
			switch {
			case err == nil:
			case ctx.Err() != nil:
				return nil, func() (RoundResult, error) { return rd.stopped(ctx) }
			case !errors.Is(err, agents.ErrLoginRequired):
				rd.warn(ctx, "%s preflight could not run: %v", kind, err)
			}
		}
		if !errors.Is(err, agents.ErrLoginRequired) {
			continue
		}
		if x.Judge {
			rd.res.Pause = &Pause{Kind: string(agents.HealthLoginRequired), Tool: kind, Detail: execx.Redact(err.Error())}
			return nil, func() (RoundResult, error) { return rd.done(ctx, OutcomeLoginRequired, err) }
		}
		loggedOut[x.Name] = true
		h := agents.Health{Kind: agents.HealthLoginRequired, Detail: execx.Redact(err.Error())}
		rep := rd.newReport(x, "")
		rep.Status, rep.Detail, rep.Health = string(h.Kind), h.Detail, &h
		rd.setReport(x, rep)
		rd.warn(ctx, "%s is logged out: %s skipped", kind, x.Name)
	}
	return loggedOut, nil
}

// envKey is a stable rendering of env, to tell environments apart.
func envKey(env map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(env)) {
		b.WriteString(k + "=" + env[k] + "\x00")
	}
	return b.String()
}

// run is the round body.
func (rd *round) run(ctx context.Context) (RoundResult, error) {
	if end := rd.adoptUnverified(ctx); end != nil {
		return end()
	}
	in := rd.in
	names := []string{}
	for _, x := range rd.running() {
		names = append(names, x.Name)
	}
	names = append(names, rd.judge.Name)
	rd.event(ctx, "info", "round.start", fmt.Sprintf("round %d (%s) of %s on %s in %s: %s", in.Round, in.Kind, rd.subject, textx.ShortSHA(in.TargetSHA), in.SlotPath, strings.Join(names, ", ")),
		map[string]any{"round": in.Round, "kind": in.Kind, "target_sha": in.TargetSHA, "slot": in.SlotPath, "dry_run": in.DryRun, "roles": names})

	env, err := rd.r.Identity.Env(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return rd.stopped(ctx)
		}
		return rd.done(ctx, OutcomeError, fmt.Errorf("pipeline: identity %s env: %w", rd.r.Identity.Name(), err))
	}
	rd.ghDir = env["GH_CONFIG_DIR"]
	if err := os.MkdirAll(rd.dir, 0o700); err != nil {
		return rd.done(ctx, OutcomeError, fmt.Errorf("pipeline: report dir: %w", err))
	}

	loggedOut, stop := rd.preflight(ctx)
	if stop != nil {
		return stop()
	}
	if err := rd.setAsideStale(); err != nil {
		return rd.done(ctx, OutcomeError, err)
	}

	// Run rows first, one per role, so the round is visible (and Park waits)
	// before anything is prompted.
	runs := map[string]*store.Run{}
	for _, x := range rd.running() {
		if loggedOut[x.Name] {
			continue
		}
		if runs[x.Name], err = rd.newRun(ctx, x, in.Kind); err != nil {
			return rd.failed(ctx, err)
		}
	}
	judgeRun, err := rd.newRun(ctx, rd.judge, in.Kind)
	if err != nil {
		return rd.failed(ctx, err)
	}

	if in.Kind == KindContinue {
		rd.existingReports()
	}
	if err := rd.readiness(ctx); err != nil {
		return rd.stopped(ctx)
	}
	if judgeRun, err = rd.reviewers(ctx, runs, judgeRun); err != nil {
		if ctx.Err() != nil {
			return rd.stopped(ctx)
		}
		return rd.done(ctx, OutcomeError, err)
	}
	return rd.runJudge(ctx, *judgeRun)
}

// runStage runs one stage: its roles in parallel, except git-diff roles,
// which edit the checkout and so run one at a time after the others, each
// followed by a tree restore. The error is ctx's, or a tree that could not
// be restored (nothing may run on a modified checkout).
func (rd *round) runStage(ctx context.Context, stage []config.Role, runs map[string]*store.Run) error {
	var wg sync.WaitGroup
	var patches []config.Role
	for _, role := range stage {
		run := runs[role.Name]
		switch {
		case run == nil: // skipped: logged out
		case role.Capture == config.CaptureGitDiff:
			patches = append(patches, role)
		default:
			wg.Go(func() { rd.setReport(role, rd.runRole(ctx, role, *run)) })
		}
	}
	wg.Wait()
	for _, role := range patches {
		if err := ctx.Err(); err != nil {
			return err
		}
		rep, err := rd.runPatchRole(ctx, role, *runs[role.Name])
		rd.setReport(role, rep)
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

// newRun inserts a pending run for role in this round.
func (rd *round) newRun(ctx context.Context, role config.Role, kind string) (*store.Run, error) {
	run, err := rd.r.Agents.NewRun(ctx, rd.pr, agents.Role(role.Name), kind, rd.in.Round)
	if err != nil {
		return nil, fmt.Errorf("pipeline: new %s run: %w", role.Name, err)
	}
	rd.mu.Lock()
	rd.runs = append(rd.runs, run.ID)
	rd.mu.Unlock()
	return &run, nil
}

// setAsideStale renames report files a previous attempt on the same head left
// behind to <name>.prev, so they are never read as this round's output. A
// continued round keeps the other roles' reports. A file it cannot move (or
// even stat) fails the round before anything is prompted: a role that then
// wrote nothing would pass the old file off as its report.
func (rd *round) setAsideStale() error {
	roles := []config.Role{rd.judge}
	if rd.in.Kind != KindContinue {
		roles = append(roles, rd.order...)
	}
	for _, role := range roles {
		p := filepath.Join(rd.dir, role.ReportFile())
		_, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			err = os.Rename(p, p+".prev")
		}
		if err != nil {
			return fmt.Errorf("pipeline: set aside the stale %s: %w", role.ReportFile(), err)
		}
	}
	return nil
}

// existingReports fills Reports for a continued round from the files on
// disk: a role that runs on every round is listed as missing when its file
// is absent, others (first, manual) only when they left a report.
func (rd *round) existingReports() {
	for _, role := range rd.order {
		p := filepath.Join(rd.dir, role.ReportFile())
		st, err := os.Stat(p)
		rep := rd.newReport(role, "")
		switch {
		case err == nil && st.Size() > 0:
			rep.Status, rep.Path = ReportOK, p
		case role.Runs == config.RunsAlways:
			rep.Status, rep.Detail = ReportMissing, "not available"
		default:
			continue
		}
		rd.setReport(role, rep)
	}
}

// newReport is an empty report of role's run.
func (rd *round) newReport(role config.Role, runID string) RoleReport {
	return RoleReport{Role: role.Name, Kind: role.AgentKind(), Capture: role.Capture, RunID: runID}
}

func (rd *round) setReport(role config.Role, rep RoleReport) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	rep.Role, rep.Kind, rep.Capture = role.Name, role.AgentKind(), role.Capture
	rd.res.Reports[agents.Role(role.Name)] = rep
}

func (rd *round) report(role config.Role) (RoleReport, bool) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	rep, ok := rd.res.Reports[agents.Role(role.Name)]
	return rep, ok
}

// judgeReports lists the candidate reports for the judge prompt: every
// non-judge role of the round in stage order, missing (with its status) when
// it produced nothing usable.
func (rd *round) judgeReports() []agents.Report {
	var out []agents.Report
	for _, role := range rd.order {
		rep, ok := rd.report(role)
		if !ok {
			continue
		}
		if rep.Status == ReportOK {
			out = append(out, agents.Report{Role: role.Name, Path: rep.Path, Status: rep.Status})
			continue
		}
		detail := rep.Status
		if role.Capture == config.CaptureGitDiff && rep.Status == ReportEmpty {
			detail = "no changes"
		}
		out = append(out, agents.Report{Role: role.Name, Status: rep.Status, Missing: true, Detail: detail})
	}
	return out
}

// abandonPending marks runs this round created but never prompted abandoned.
func (rd *round) abandonPending(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	rd.mu.Lock()
	ids := append([]string(nil), rd.runs...)
	rd.mu.Unlock()
	for _, id := range ids {
		err := rd.r.Store.TransitionRun(ctx, id, []string{store.RunPending}, store.RunAbandoned, func(u *store.RunUpdate) {
			u.Set("error", "round ended before this run was prompted")
			u.Set("ended_at", rd.r.now())
		})
		if err != nil && !errors.Is(err, store.ErrConflict) {
			rd.logf("pipeline: abandon run %s: %v", id, err)
		}
	}
}

// done records the outcome and returns the result. err is returned only for
// OutcomeError; for other outcomes it only fills Error.
func (rd *round) done(ctx context.Context, outcome string, err error) (RoundResult, error) {
	rd.mu.Lock()
	rd.res.Outcome = outcome
	if err != nil && rd.res.Error == "" {
		rd.res.Error = execx.Redact(err.Error())
	}
	res := rd.res
	res.Warnings = append([]string(nil), rd.res.Warnings...)
	rd.mu.Unlock()

	level := "info"
	switch outcome {
	case OutcomePosted, OutcomeDryRun:
	case OutcomeError, OutcomeIdentityLeak, OutcomeNeedsAttention:
		level = "error"
	default:
		level = "warn"
	}
	msg := fmt.Sprintf("round %d ended: %s", rd.in.Round, outcome)
	if res.ReviewURL != "" {
		msg += " " + res.ReviewURL
	}
	if res.Error != "" {
		msg += ": " + res.Error
	}
	rd.event(ctx, level, "round.end", msg, map[string]any{"outcome": outcome, "review_id": res.ReviewID,
		"event": res.Event, "judge_run": res.JudgeRunID, "nudged": res.Nudged})
	if outcome == OutcomeError {
		if err == nil {
			err = errors.New("pipeline: round failed")
		}
		return res, err
	}
	return res, nil
}

// failed is done(OutcomeError) for an internal error, or stopped when ctx ended.
func (rd *round) failed(ctx context.Context, err error) (RoundResult, error) {
	if ctx.Err() != nil {
		return rd.stopped(ctx)
	}
	return rd.done(ctx, OutcomeError, err)
}

// stopped ends the round because ctx was cancelled.
func (rd *round) stopped(ctx context.Context) (RoundResult, error) {
	cause := ctx.Err()
	res, _ := rd.done(context.WithoutCancel(ctx), OutcomeStopped, fmt.Errorf("pipeline: round cancelled: %w", cause))
	return res, fmt.Errorf("pipeline: round %d of %s: %w", rd.in.Round, rd.subject, cause)
}

func (rd *round) warn(ctx context.Context, format string, args ...any) {
	msg := execx.Redact(fmt.Sprintf(format, args...))
	rd.mu.Lock()
	rd.res.Warnings = append(rd.res.Warnings, msg)
	rd.mu.Unlock()
	rd.event(ctx, "warn", "round.warning", msg, nil)
}

// event appends an audit row (never fails the round).
func (rd *round) event(ctx context.Context, level, kind, msg string, data map[string]any) {
	msg = execx.Redact(msg)
	rd.logf("pipeline: %s %s: %s", rd.subject, kind, msg)
	e := store.Event{Level: level, Subject: &rd.subject, Kind: kind, Message: msg}
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			e.Data = json.RawMessage(execx.Redact(string(b)))
		}
	}
	if _, err := rd.r.Store.AppendEvent(context.WithoutCancel(ctx), e); err != nil {
		rd.logf("pipeline: event %s %s: %v", rd.subject, kind, err)
	}
}

func (rd *round) logf(format string, args ...any) {
	if rd.r.Logger != nil {
		rd.r.Logger.Printf("%s", execx.Redact(fmt.Sprintf(format, args...)))
	}
}

func (r *Runner) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Runner) poll() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return DefaultPollInterval
}

func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
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
