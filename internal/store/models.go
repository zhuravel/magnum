package store

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Repo modes.
const (
	RepoModePool  = "pool"
	RepoModePerPR = "per_pr"
)

// PR states (prs.state).
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

// InFlightStates are the PR states of a review round being prepared or
// running. The daemon owns a PR in them: CLI writes refuse, recovery
// re-evaluates them at startup. Callers must not modify the slice.
var InFlightStates = []string{PRClaiming, PRReviewing, PRVerifying}

// GitHub PR states (prs.gh_state).
const (
	GHOpen    = "OPEN"
	GHClosed  = "CLOSED"
	GHMerged  = "MERGED"
	GHUnknown = "UNKNOWN"
)

// Slot kinds.
const (
	SlotKindPool     = "pool"
	SlotKindPerPR    = "per_pr"
	SlotKindExternal = "external"
)

// Slot states (slots.state).
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

// Default role names (sessions.role, runs.role). Roles are free-form names
// defined in config; the store only requires them to be non-empty. These are
// the four built-in roles. Migration 0003 renamed their pre-v3 ids (judge,
// claude, codex_review, simplify) to these labels.
const (
	RoleJudge       = "codex-judge"
	RoleClaude      = "claude-review"
	RoleCodexReview = "codex-review"
	RoleSimplify    = "claude-simplify"
)

// Session states.
const (
	SessionStarting = "starting"
	SessionLive     = "live"
	SessionParked   = "parked"
	SessionLost     = "lost"
	SessionClosed   = "closed"
)

// Run kinds.
const (
	RunInitial  = "initial"
	RunRereview = "rereview"
	RunContinue = "continue"
	RunNudge    = "nudge"
	RunRecovery = "recovery"
)

// Run states.
const (
	RunPending   = "pending"
	RunSubmitted = "submitted"
	RunWorking   = "working"
	RunEnded     = "ended"
	RunVerified  = "verified"
	RunFailed    = "failed"
	RunAbandoned = "abandoned"
)

// Request states.
const (
	RequestPending = "pending"
	RequestDone    = "done"
	RequestFailed  = "failed"
)

// Event kinds and phases used by the steps helper.
const (
	KindStep      = "step"       // one begin/ok/fail row per step attempt
	KindStepReset = "step.reset" // starts a new step generation for a subject
	PhaseBegin    = "begin"
	PhaseOK       = "ok"
	PhaseFail     = "fail"
)

// Live session states and active run states, as used by the queries below.
var (
	liveSessionStates = []string{SessionStarting, SessionLive}
	activeRunStates   = []string{RunPending, RunSubmitted, RunWorking, RunEnded}
)

// ClaimableStates are the PR states ClaimSlot accepts.
var ClaimableStates = []string{PRQueued, PRRereviewPending}

// Repo is a watched repository.
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

// FullName is "owner/name".
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// PR is one pull request and its automation state.
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
}

// CIStatus is prs.ci_json: the checks of a PR's head commit. Its State can
// trail prs.ci_state, which the radar refreshes, until the next Details
// fetch. A required check absent from Checks has no run on SHA (a
// repository whose CI runs only when the PR is opened): it is missing on
// this head, not pending.
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

// CheckResult is one check run or commit status of a CIStatus.
type CheckResult struct {
	Name     string `json:"name"`               // the check run's name or the status's context
	State    string `json:"state"`              // CheckPassed | CheckFailed | CheckPending | CheckSkipped
	Workflow string `json:"workflow,omitempty"` // the GitHub Actions workflow; "" for a status or another app's check
	// At is when the check finished, else started, or the status was
	// posted; zero for a check run not started yet (the newest run).
	At time.Time `json:"at,omitzero"`
}

// CheckResult.State values (github.Check's).
const (
	CheckPassed  = "passed"
	CheckFailed  = "failed"
	CheckPending = "pending"
	CheckSkipped = "skipped"
)

// Tally sets Passed, Failed, Pending, Skipped and AllSkipped from Checks
// (and makes a nil Checks empty).
func (c *CIStatus) Tally() {
	if c.Checks == nil {
		c.Checks = []CheckResult{}
	}
	c.Passed, c.Failed, c.Pending, c.Skipped = 0, 0, 0, 0
	for _, ch := range c.Checks {
		switch ch.State {
		case CheckPassed:
			c.Passed++
		case CheckFailed:
			c.Failed++
		case CheckPending:
			c.Pending++
		case CheckSkipped:
			c.Skipped++
		}
	}
	c.AllSkipped = len(c.Checks) > 0 && c.Skipped == len(c.Checks)
}

func (c CIStatus) equal(o CIStatus) bool {
	return c.SHA == o.SHA && c.State == o.State && c.Total == o.Total && c.Passed == o.Passed && c.Failed == o.Failed &&
		c.Pending == o.Pending && c.Skipped == o.Skipped && c.AllSkipped == o.AllSkipped && c.Complete == o.Complete &&
		slices.EqualFunc(c.Checks, o.Checks, func(a, b CheckResult) bool {
			return a.Name == b.Name && a.State == b.State && a.Workflow == b.Workflow && a.At.Equal(b.At)
		})
}

// TeamReviewerPrefix marks a team in requested_reviewers_json ("team:<slug>").
const TeamReviewerPrefix = "team:"

// LatestReview is one entry of prs.latest_reviews_json: GitHub's latest
// review of one reviewer.
type LatestReview struct {
	Login       string     `json:"login"`                  // Account form (github.Account): a bot's keeps "[bot]"; "" for a ghost
	State       string     `json:"state"`                  // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	SubmittedAt *time.Time `json:"submitted_at,omitempty"` // nil for a PENDING review
	CommitSHA   string     `json:"commit_sha"`             // "" when the commit is gone
}

func (a LatestReview) equal(b LatestReview) bool {
	if a.Login != b.Login || a.State != b.State || a.CommitSHA != b.CommitSHA || (a.SubmittedAt == nil) != (b.SubmittedAt == nil) {
		return false
	}
	return a.SubmittedAt == nil || a.SubmittedAt.Equal(*b.SubmittedAt)
}

// SinceReview.Source values: what Base is.
const (
	SinceFromReviewed = "reviewed" // prs.reviewed_sha: magnum's last verified review
	SinceFromReview   = "review"   // the commit of the PR identity's latest GitHub review (no reviewed_sha yet)
	SinceFromBase     = "base"     // the base branch tip: nothing reviewed yet, so the whole PR
)

// SinceReview is prs.since_review_json: the size of Base...Head, i.e. what
// changed since the last review (or the whole PR when nothing was reviewed).
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
}

// Slot is a checkout magnum reviews in (pool slot, per-PR worktree, or an
// observed external repoN checkout).
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
}

// Assignment records that a PR's code (and databases) lived in a slot.
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

// SlotDatabase is a MySQL schema seen by the inventory (SlotID nil = orphan).
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

// Session is one agent (judge, reviewer, simplifier) in a herdr pane.
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

// Run is one prompt/turn of one role in a review round.
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

// Request is a CLI → daemon work item.
type Request struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"` // payload_json
	State     string          `json:"state"`
	Result    *string         `json:"result"`
	CreatedAt time.Time       `json:"created_at"`
	HandledAt *time.Time      `json:"handled_at"`
}

// Event is one audit-log row (`magnum logs`).
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

// Column lists, in schema order; scan functions below must match them.
var (
	repoColumns = []string{"id", "node_id", "owner", "name", "watch_owner", "clone_path", "default_branch", "mode",
		"first_synced_at", "last_seen_at"}
	prColumns = []string{"id", "repo_id", "node_id", "number", "url", "title", "author_login", "author_type",
		"head_ref", "base_ref", "head_sha", "head_changed_at", "is_draft", "is_cross_repo", "review_requested",
		"labels_json", "gh_state", "gh_updated_at", "merged_at", "closed_at", "missing_since", "confirm_count", "state", "skip_reason",
		"prev_state", "forced", "identity", "reviewed_sha", "last_review_id", "last_review_event", "reviewed_at",
		"last_round_started_at", "rounds_today", "rounds_day", "next_eligible_at", "pending_since", "release_after",
		"attempts", "next_attempt_at", "last_error", "pinned", "muted", "simplify_done", "human_active_at",
		"created_at", "updated_at",
		// 0002_board
		"assignees_json", "requested_reviewers_json", "latest_reviews_json", "since_review_json",
		"last_review_login", "base_sha", "details_at",
		// 0006_author_association
		"author_association",
		// 0007_ci
		"ci_state", "ci_json"}
	slotColumns = []string{"id", "name", "repo_id", "repo_full_name", "kind", "path", "main_clone",
		"placeholder_branch", "db_slug", "state", "pr_id", "pinned", "dirty_schema", "checked_out_sha",
		"hold_reason", "lock_sha", "last_used_at", "last_error", "created_at", "updated_at"}
	assignmentColumns = []string{"id", "pr_id", "slot_id", "path", "db_slug", "db_names_json", "head_sha",
		"started_at", "ended_at", "end_reason"}
	slotDatabaseColumns = []string{"id", "slot_id", "db_name", "slug", "size_mb", "first_seen_at", "last_seen_at",
		"dropped_at", "dropped_by"}
	sessionColumns = []string{"id", "pr_id", "role", "generation", "agent_name", "agent_kind", "session_id",
		"resumed_from", "herdr_workspace_id", "herdr_tab_id", "herdr_pane_id", "cwd", "env_json", "state",
		"agent_status", "agent_status_at", "idle_ticks", "started_at", "last_prompt_at", "closed_at"}
	runColumns = []string{"id", "pr_id", "round", "role", "session_id", "kind", "target_sha", "prev_reviewed_sha",
		"identity", "reviewer_login", "state", "outcome", "report_path", "review_id", "review_event",
		"review_commit", "review_url", "result_json", "prompt_text", "created_at", "submitted_at",
		"working_seen_at", "ended_at", "verified_at", "error"}
	requestColumns = []string{"id", "kind", "payload_json", "state", "result", "created_at", "handled_at"}
	eventColumns   = []string{"id", "at", "level", "subject", "kind", "step", "phase", "message", "data_json"}
)

// cols renders a column list, optionally qualified with a table alias.
func cols(alias string, names []string) string {
	if alias == "" {
		return strings.Join(names, ", ")
	}
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = alias + "." + n
	}
	return strings.Join(q, ", ")
}

type scanner interface{ Scan(dest ...any) error }

func scanRepo(sc scanner) (Repo, error) {
	var r Repo
	err := sc.Scan(&r.ID, &r.NodeID, &r.Owner, &r.Name, &r.WatchOwner, &r.ClonePath, &r.DefaultBranch, &r.Mode,
		nullTime(&r.FirstSyncedAt), timeCol(&r.LastSeenAt))
	return r, err
}

func scanPR(sc scanner) (PR, error) {
	var p PR
	err := sc.Scan(&p.ID, &p.RepoID, &p.NodeID, &p.Number, &p.URL, &p.Title, &p.AuthorLogin, &p.AuthorType,
		&p.HeadRef, &p.BaseRef, &p.HeadSHA, timeCol(&p.HeadChangedAt), &p.IsDraft, &p.IsCrossRepo,
		&p.ReviewRequested, jsonCol(&p.Labels), &p.GHState, nullTime(&p.GHUpdatedAt), nullTime(&p.MergedAt), nullTime(&p.ClosedAt),
		nullTime(&p.MissingSince), &p.ConfirmCount, &p.State, &p.SkipReason, &p.PrevState, &p.Forced,
		&p.Identity, &p.ReviewedSHA, &p.LastReviewID, &p.LastReviewEvent, nullTime(&p.ReviewedAt),
		nullTime(&p.LastRoundStartedAt), &p.RoundsToday, &p.RoundsDay, nullTime(&p.NextEligibleAt),
		nullTime(&p.PendingSince), nullTime(&p.ReleaseAfter), &p.Attempts, nullTime(&p.NextAttemptAt),
		&p.LastError, &p.Pinned, &p.Muted, &p.SimplifyDone, nullTime(&p.HumanActiveAt),
		timeCol(&p.CreatedAt), timeCol(&p.UpdatedAt),
		jsonCol(&p.Assignees), jsonCol(&p.RequestedReviewers), jsonCol(&p.LatestReviews), jsonCol(&p.SinceReview),
		&p.LastReviewLogin, &p.BaseSHA, nullTime(&p.DetailsAt), &p.AuthorAssociation, &p.CIState, jsonCol(&p.CI))
	return p, err
}

func scanSlot(sc scanner) (Slot, error) {
	var s Slot
	err := sc.Scan(&s.ID, &s.Name, &s.RepoID, &s.RepoFullName, &s.Kind, &s.Path, &s.MainClone,
		&s.PlaceholderBranch, &s.DBSlug, &s.State, &s.PRID, &s.Pinned, &s.DirtySchema, &s.CheckedOutSHA,
		&s.HoldReason, &s.LockSHA, nullTime(&s.LastUsedAt), &s.LastError, timeCol(&s.CreatedAt),
		timeCol(&s.UpdatedAt))
	return s, err
}

func scanAssignment(sc scanner) (Assignment, error) {
	var a Assignment
	err := sc.Scan(&a.ID, &a.PRID, &a.SlotID, &a.Path, &a.DBSlug, jsonCol(&a.DBNames), &a.HeadSHA,
		timeCol(&a.StartedAt), nullTime(&a.EndedAt), &a.EndReason)
	return a, err
}

func scanSlotDatabase(sc scanner) (SlotDatabase, error) {
	var d SlotDatabase
	err := sc.Scan(&d.ID, &d.SlotID, &d.DBName, &d.Slug, &d.SizeMB, timeCol(&d.FirstSeenAt),
		timeCol(&d.LastSeenAt), nullTime(&d.DroppedAt), &d.DroppedBy)
	return d, err
}

func scanSession(sc scanner) (Session, error) {
	var s Session
	err := sc.Scan(&s.ID, &s.PRID, &s.Role, &s.Generation, &s.AgentName, &s.AgentKind, &s.SessionID,
		&s.ResumedFrom, &s.HerdrWorkspaceID, &s.HerdrTabID, &s.HerdrPaneID, &s.Cwd, jsonCol(&s.Env), &s.State,
		&s.AgentStatus, nullTime(&s.AgentStatusAt), &s.IdleTicks, timeCol(&s.StartedAt),
		nullTime(&s.LastPromptAt), nullTime(&s.ClosedAt))
	return s, err
}

func scanRun(sc scanner) (Run, error) {
	var r Run
	err := sc.Scan(&r.ID, &r.PRID, &r.Round, &r.Role, &r.SessionID, &r.Kind, &r.TargetSHA, &r.PrevReviewedSHA,
		&r.Identity, &r.ReviewerLogin, &r.State, &r.Outcome, &r.ReportPath, &r.ReviewID, &r.ReviewEvent,
		&r.ReviewCommit, &r.ReviewURL, &r.ResultJSON, &r.PromptText, timeCol(&r.CreatedAt),
		nullTime(&r.SubmittedAt), nullTime(&r.WorkingSeenAt), nullTime(&r.EndedAt), nullTime(&r.VerifiedAt),
		&r.Error)
	return r, err
}

func scanRequest(sc scanner) (Request, error) {
	var r Request
	err := sc.Scan(&r.ID, &r.Kind, rawCol(&r.Payload), &r.State, &r.Result, timeCol(&r.CreatedAt),
		nullTime(&r.HandledAt))
	return r, err
}

func scanEvent(sc scanner) (Event, error) {
	var e Event
	err := sc.Scan(&e.ID, timeCol(&e.At), &e.Level, &e.Subject, &e.Kind, &e.Step, &e.Phase, &e.Message,
		rawCol(&e.Data))
	return e, err
}

// textOf extracts TEXT column content; ok is false for NULL.
func textOf(v any) (s string, ok bool, err error) {
	switch x := v.(type) {
	case nil:
		return "", false, nil
	case string:
		return x, true, nil
	case []byte:
		return string(x), true, nil
	case time.Time:
		return FormatTime(x), true, nil
	default:
		return "", false, fmt.Errorf("store: expected text, got %T", v)
	}
}

// timeScanner scans a NOT NULL timestamp column.
type timeScanner struct{ p *time.Time }

func timeCol(p *time.Time) timeScanner { return timeScanner{p} }

func (t timeScanner) Scan(v any) error {
	s, ok, err := textOf(v)
	if err != nil {
		return err
	}
	if !ok {
		*t.p = time.Time{}
		return nil
	}
	parsed, err := ParseTime(s)
	if err != nil {
		return err
	}
	*t.p = parsed
	return nil
}

// nullTimeScanner scans a nullable timestamp column into a *time.Time.
type nullTimeScanner struct{ p **time.Time }

func nullTime(p **time.Time) nullTimeScanner { return nullTimeScanner{p} }

func (t nullTimeScanner) Scan(v any) error {
	s, ok, err := textOf(v)
	if err != nil {
		return err
	}
	if !ok || s == "" {
		*t.p = nil
		return nil
	}
	parsed, err := ParseTime(s)
	if err != nil {
		return err
	}
	*t.p = &parsed
	return nil
}

// jsonScanner decodes a JSON text column into p (NULL/empty leaves it zero).
type jsonScanner struct{ p any }

func jsonCol(p any) jsonScanner { return jsonScanner{p} }

func (j jsonScanner) Scan(v any) error {
	s, ok, err := textOf(v)
	if err != nil || !ok || s == "" {
		return err
	}
	if err := json.Unmarshal([]byte(s), j.p); err != nil {
		return fmt.Errorf("store: decode json column: %w", err)
	}
	return nil
}

// rawScanner copies a JSON text column verbatim (NULL = nil).
type rawScanner struct{ p *json.RawMessage }

func rawCol(p *json.RawMessage) rawScanner { return rawScanner{p} }

func (r rawScanner) Scan(v any) error {
	s, ok, err := textOf(v)
	if err != nil {
		return err
	}
	if !ok {
		*r.p = nil
		return nil
	}
	*r.p = json.RawMessage(s)
	return nil
}

// collect reads every row of rows with scan and closes rows.
func collect[T any](rows interface {
	Next() bool
	Err() error
	Close() error
	Scan(dest ...any) error
}, scan func(scanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
