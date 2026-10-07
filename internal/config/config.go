// Package config loads and validates config.toml (kept in the repository).
package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
)

// Duration is a time.Duration that unmarshals from Go syntax ("30s", "5m",
// "2h") with whole days allowed in front ("30d", "1d12h").
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// ParseDuration is time.ParseDuration plus a leading whole-day count: "7d",
// "30d", "1d12h". A day is 24 hours.
func ParseDuration(s string) (time.Duration, error) {
	days, rest, ok := strings.Cut(s, "d")
	if !ok {
		return time.ParseDuration(s)
	}
	n, err := strconv.ParseUint(days, 10, 16)
	if err != nil || days == "" {
		return 0, fmt.Errorf("time: invalid duration %q", s)
	}
	d := time.Duration(n) * 24 * time.Hour
	if rest == "" {
		return d, nil
	}
	r, err := time.ParseDuration(rest)
	if err != nil || r < 0 || strings.HasPrefix(rest, "+") {
		return 0, fmt.Errorf("time: invalid duration %q", s)
	}
	return d + r, nil
}

// DatabaseSuffix ends every pool database template ([[pool]] databases):
// the slot's slug follows the last "__", which is how magnum lists and
// guards a slot's databases.
const DatabaseSuffix = "__{slug}"

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

	// snapshot is the prompt text the daemon loaded at startup
	// (SnapshotPrompts); nil = prompts are read from disk when resolved.
	// Set once, before the daemon starts its goroutines.
	snapshot *PromptSnapshot
}

type Daemon struct {
	PollInterval             Duration `toml:"poll_interval"`
	MaxConcurrentReviews     int      `toml:"max_concurrent_reviews"`
	MaxTotalWorkingCodex     int      `toml:"max_total_working_codex"`
	PushQuietPeriod          Duration `toml:"push_quiet_period"`
	MinRereviewInterval      Duration `toml:"min_rereview_interval"`
	DraftMinRereviewInterval Duration `toml:"draft_min_rereview_interval"`
	OwnMinRereviewInterval   Duration `toml:"own_min_rereview_interval"` // MinRereviewInterval for the operator's own PRs (SelfLogins; an own draft takes the longer of it and the draft one; 0 = MinRereviewInterval; Config.ThrottleFor)
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
	// ReplyDebounce is how long after the last reply on magnum's latest
	// review (the PR author's review or comment, or anyone's reply in one of
	// magnum's threads) a reviewed PR whose head has not moved waits before
	// its judge alone re-decides the threads; 0 = replies start no round.
	// ReplyMinInterval spaces such rounds of one PR and head (0 = no wait).
	// A push meanwhile wins: the re-review it gets reads the replies.
	ReplyDebounce    Duration `toml:"reply_debounce"`
	ReplyMinInterval Duration `toml:"reply_min_interval"`
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

// TrivialDeltaClasses are the values of skip_trivial_deltas: a push that
// only changes comment lines, only whitespace (blank lines, re-indented
// code where indentation carries no meaning), only documentation files, or
// only merges the base branch (or rebases onto it) and leaves the PR's own
// diff against its base as it was (base).
var TrivialDeltaClasses = []string{"comments", "whitespace", "docs", "base"}

// Usage is the [usage] section: subscription budgets the scheduler watches.
// Codex's budget is read from Codex's own session files (internal/usage).
type Usage struct {
	// CodexSoft: at or above this share (percent) of the Codex budget used,
	// automatic full re-reviews wait; first reviews, delta checks, re-reviews
	// of the same head, reply rounds and requested or forced reviews still
	// run. 0 = off.
	CodexSoft float64 `toml:"codex_soft"`
	// CodexHard: at or above this share every kind backed by Codex pauses
	// until the budget drops below it again. 0 = off.
	CodexHard float64 `toml:"codex_hard"`
	// CodexHome is the CODEX_HOME whose sessions are read; "" = $CODEX_HOME,
	// else ~/.codex.
	CodexHome string `toml:"codex_home"`
}

// GitHub tunes how magnum reaches the GitHub REST API itself (App JWT and
// installation-token calls; everything else already runs through gh).
type GitHub struct {
	// Transport is "gh" (default: requests run as `gh api --include`, so only
	// the signed gh binary talks to GitHub, which outbound firewalls such as
	// Little Snitch already allow) or "direct" (Go net/http).
	Transport string `toml:"transport"`
}

type Herdr struct {
	Socket           string `toml:"socket"`
	Notify           bool   `toml:"notify"`
	ToastEveryReview bool   `toml:"toast_every_review"`
}

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

// DismissStale reports whether the identity dismisses its own stale CHANGES_REQUESTED.
func (i Identity) DismissStale() bool {
	if i.DismissOwnStale != nil {
		return *i.DismissOwnStale
	}
	return i.Kind == "app"
}

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
	// OwnMinRereviewInterval overrides [daemon] own_min_rereview_interval
	// for this watch's PRs the operator authored (a zero duration keeps the
	// daemon's).
	OwnMinRereviewInterval Duration `toml:"own_min_rereview_interval"`
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
	// AutoApprove names the watch's repositories (names, any case, or "*"
	// for every one it covers) on whose PRs magnum posts an approval as
	// AutoApproveAs once its own review of the head found nothing that must
	// be fixed before merging (engine autoapprove.go); empty = never (the
	// default). AutoApproveAs is an [[identity]] of kind gh: the operator's
	// own account, whose approval GitHub counts (an App's does not).
	AutoApprove   []string `toml:"auto_approve"`
	AutoApproveAs string   `toml:"auto_approve_as"`
	// AutoApproveBody is the approval's one-line body, a template of
	// AutoApproveData (nil = DefaultAutoApproveBody); magnum appends its
	// marker.
	AutoApproveBody *string `toml:"auto_approve_body"`
	// ManualRepos names repositories of the watch (names, any case, no
	// owner or pattern) that are reviewed only when the operator asks
	// (`magnum review`, the board, the picker): their PRs are polled and
	// shown, but eligibility.Classify rejects them, so no push, new PR or
	// GitHub review request starts a round. See ManualRepo.
	ManualRepos []string `toml:"manual_repos"`
}

// ManualRepo reports whether manual_repos names the repository name
// (without owner; any case). A blank name is never manual.
func (w Watch) ManualRepo(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && slices.ContainsFunc(w.ManualRepos, func(n string) bool { return strings.EqualFold(strings.TrimSpace(n), name) })
}

// TrivialDeltas is the skip_trivial_deltas that applies to w's PRs: the
// watch's when it sets one, else the daemon's. Empty = every push is
// re-reviewed.
func (c *Config) TrivialDeltas(w *Watch) []string {
	if w != nil && w.SkipTrivialDeltas != nil {
		return w.SkipTrivialDeltas
	}
	return c.Daemon.SkipTrivialDeltas
}

// ThrottleFor is the [daemon] section with w's burst, re-review delta,
// delta check and own PR interval overrides applied: the throttle settings
// (eligibility.Throttle) of w's PRs. A nil w is the daemon's.
func (c *Config) ThrottleFor(w *Watch) Daemon {
	d := c.Daemon
	if w == nil {
		return d
	}
	if w.BurstQuietPeriod.Duration > 0 {
		d.BurstQuietPeriod = w.BurstQuietPeriod
	}
	if w.BurstPushes != nil {
		d.BurstPushes = *w.BurstPushes
	}
	if w.BurstWindow.Duration > 0 {
		d.BurstWindow = w.BurstWindow
	}
	if w.RereviewMinLines != nil {
		d.RereviewMinLines = *w.RereviewMinLines
	}
	if w.RereviewMaxWait.Duration > 0 {
		d.RereviewMaxWait = w.RereviewMaxWait
	}
	if w.DeltaCheck != nil {
		d.DeltaCheck = *w.DeltaCheck
	}
	if w.OwnMinRereviewInterval.Duration > 0 {
		d.OwnMinRereviewInterval = w.OwnMinRereviewInterval
	}
	return d
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// DraftsIncluded is include_drafts (default true). False skips drafts until
// they are ready for review, except that a review request for the poll
// login, a posting identity or a team of request_teams makes a draft
// eligible for the round the request starts (eligibility.PRFacts.Requested).
func (w Watch) DraftsIncluded() bool   { return boolOr(w.IncludeDrafts, true) }
func (w Watch) OwnIncluded() bool      { return boolOr(w.IncludeOwn, true) }
func (w Watch) BotsSkipped() bool      { return boolOr(w.SkipBotAuthors, true) }
func (w Watch) CrossRepoSkipped() bool { return boolOr(w.SkipCrossRepository, true) }

// DepartedAuthorsSkipped is SkipDepartedAuthors with its default (true).
func (w Watch) DepartedAuthorsSkipped() bool { return boolOr(w.SkipDepartedAuthors, true) }

// Matches reports whether owner/name is covered by this watch: the owner
// equals w.Owner, no Exclude glob matches the name and an Include glob does
// (path.Match globs; case is ignored throughout).
func (w Watch) Matches(owner, name string) bool {
	if !strings.EqualFold(owner, w.Owner) {
		return false
	}
	// GitHub names are case-insensitive, like owners (above), RepoFor and
	// PoolFor: match with both sides lower-cased.
	name = strings.ToLower(name)
	for _, g := range w.Exclude {
		if ok, _ := path.Match(strings.ToLower(g), name); ok {
			return false
		}
	}
	for _, g := range w.Include {
		if ok, _ := path.Match(strings.ToLower(g), name); ok {
			return true
		}
	}
	return false
}

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

// ResetsDBOnSchemaChange is ResetDBOnSchemaChange with its default (true).
func (p Pool) ResetsDBOnSchemaChange() bool { return boolOr(p.ResetDBOnSchemaChange, true) }

// Slot renders the name of slot n.
func (p Pool) Slot(n int) string { return strings.ReplaceAll(p.SlotName, "{n}", fmt.Sprint(n)) }

// Path renders the checkout path of slot n.
func (p Pool) Path(n int) string {
	return paths.Expand(strings.ReplaceAll(p.SlotPath, "{n}", fmt.Sprint(n)))
}

// DBNames renders the database names for a slug.
func (p Pool) DBNames(slug string) []string {
	out := make([]string, 0, len(p.Databases))
	for _, d := range p.Databases {
		out = append(out, strings.ReplaceAll(d, "{slug}", slug))
	}
	return out
}

// SlotEnv renders [pool.env] for a slot (blank values stay blank on purpose).
func (p Pool) SlotEnv(slot string) map[string]string {
	out := make(map[string]string, len(p.Env))
	for k, v := range p.Env {
		out[k] = strings.ReplaceAll(v, "{slot}", slot)
	}
	return out
}

// Repo is a [[repo]] block: the repository's verdicts (any watched
// repository, pooled or not) and setup for the per-PR worktrees of a
// repository without a pool. Without a block, a per-PR worktree runs the main clone's
// .config/wt.toml hooks (worktrunk); setup or teardown commands here replace
// those hooks (both of them), and wt_hooks = false ignores them without a
// replacement. Commands run in the worktree with WT_BRANCH=magnum-pr-<N> and
// the rendered env; they are not templated (use $WT_BRANCH).
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

// RequiredWorkflowPrefix marks a required_checks entry naming a whole
// GitHub Actions workflow ("workflow:CI").
const RequiredWorkflowPrefix = "workflow:"

// worktreeKeys reports whether the block sets any per-PR worktree key
// (everything but repo, the verdicts, readiness, keep_approvals,
// required_checks and trackers).
func (r Repo) worktreeKeys() bool {
	return len(r.Setup) > 0 || len(r.Teardown) > 0 || r.WTHooks != nil || len(r.CopyFiles) > 0 || len(r.StripEnv) > 0 || len(r.Env) > 0
}

// WTHooksEnabled reports whether .config/wt.toml hooks may run (wt_hooks,
// default true).
func (r Repo) WTHooksEnabled() bool { return boolOr(r.WTHooks, true) }

// HasCommands reports whether the block supplies its own setup or teardown.
func (r Repo) HasCommands() bool { return len(r.Setup) > 0 || len(r.Teardown) > 0 }

// RenderEnv renders [repo.env] for a per-PR worktree: {slug} is the
// workspace name (magnum-pr-<N>), {path} the worktree and {clone} the main
// clone. Blank values stay blank on purpose.
func (r Repo) RenderEnv(slug, path, clone string) map[string]string {
	out := make(map[string]string, len(r.Env))
	rep := strings.NewReplacer("{slug}", slug, "{path}", path, "{clone}", clone)
	for k, v := range r.Env {
		out[k] = rep.Replace(v)
	}
	return out
}

// LoadOptions tunes LoadWithOptions.
type LoadOptions struct {
	// NoOverlay skips the user layer (the user config) even when it exists,
	// so the result depends on the base alone.
	NoOverlay bool
}

// BuiltinDefaults names the embedded base in messages and Sources.
const BuiltinDefaults = "built-in defaults (config.defaults.toml)"

// Load reads the configuration (see LoadWithOptions) with the zero
// LoadOptions.
func Load(layout paths.Layout, file string) (*Config, error) {
	return LoadWithOptions(layout, file, LoadOptions{})
}

// LoadWithOptions reads the configuration in two layers and validates it.
//
// The base is file (or $MAGNUM_CONFIG): a complete configuration replacing
// the built-in defaults; else a config.toml in the layout's home (a checkout
// from before the defaults were built in); else the built-in defaults, the
// repository's config.defaults.toml embedded in the binary.
//
// The user layer goes over it unless opts.NoOverlay: the layout's
// UserConfig (~/.config/magnum/config.toml) when it exists. It appends
// [[identity]], [[watch]], [[pool]] and [[repo]], and overrides keys (see
// applyOverlay). cfg.Sources lists what was read.
func LoadWithOptions(layout paths.Layout, file string, opts LoadOptions) (*Config, error) {
	if file == "" {
		file = os.Getenv("MAGNUM_CONFIG")
	}
	if file == "" && layout.Home != "" {
		if legacy := layout.Config(); fsx.Exists(legacy) {
			file = legacy
		}
	}
	cfg := Defaults()
	cfg.Layout = layout
	// Kinds and roles are merged key by key below; decoding into the
	// defaults would reuse their slice elements and replace whole map values.
	cfg.Kinds, cfg.Roles = nil, nil
	name := file
	var md toml.MetaData
	var data []byte // nil: read file
	var err error
	if file != "" {
		md, err = toml.DecodeFile(file, cfg)
	} else {
		name, data = BuiltinDefaults, magnum.DefaultConfig
		md, err = toml.Decode(string(data), cfg)
	}
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", name, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("config %s: unknown keys: %v", name, undecoded)
	}
	cfg.learnModelFollowsKind(md)
	cfg.notesModelFollowsKind(md)
	base, err := readLayerData(name, data, md, cfg.Kinds, cfg.Roles)
	if err != nil {
		return nil, err
	}
	cfg.Sources = []string{name}
	var over *layer
	if user := layout.UserConfig; user != "" && fsx.Exists(user) && !opts.NoOverlay {
		if over, err = cfg.applyOverlay(user); err != nil {
			return nil, err
		}
		cfg.Sources = append(cfg.Sources, user)
	}
	cfg.buildPipeline(base, over)
	cfg.expand()
	cfg.Normalize()
	cfg.expandPipeline()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", strings.Join(cfg.Sources, " + "), err)
	}
	return cfg, nil
}

// applyOverlay merges the user config: [[identity]], [[watch]], [[pool]]
// and [[repo]] entries are appended; keys present under [daemon], [herdr], [terminal],
// [github], [pipeline], [usage], [triage], [learn] and [notes] override the committed values key by key.
// Its [kinds.<name>] keys and [[role]] blocks are returned for buildPipeline:
// kind keys override key by key, a [[role]] named like an existing role
// overrides the keys it sets, any other [[role]] is appended.
func (c *Config) applyOverlay(path string) (*layer, error) {
	var o Config
	md, err := toml.DecodeFile(path, &o)
	if err != nil {
		return nil, fmt.Errorf("config overlay %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("config overlay %s: unknown keys: %v", path, undecoded)
	}
	c.Identities = append(c.Identities, o.Identities...)
	c.Watches = append(c.Watches, o.Watches...)
	c.Pools = append(c.Pools, o.Pools...)
	c.Repos = append(c.Repos, o.Repos...)
	overlaySection(md, "daemon", &c.Daemon, &o.Daemon)
	overlaySection(md, "herdr", &c.Herdr, &o.Herdr)
	overlaySection(md, "terminal", &c.Terminal, &o.Terminal)
	overlaySection(md, "github", &c.GitHub, &o.GitHub)
	overlaySection(md, "pipeline", &c.Pipeline, &o.Pipeline)
	overlaySection(md, "usage", &c.Usage, &o.Usage)
	overlaySection(md, "triage", &c.Triage, &o.Triage)
	overlaySection(md, "learn", &c.Learn, &o.Learn)
	c.learnModelFollowsKind(md)
	overlaySection(md, "notes", &c.Notes, &o.Notes)
	c.notesModelFollowsKind(md)
	overlaySection(md, "board", &c.Board, &o.Board)
	l, err := readLayer(path, md, o.Kinds, o.Roles)
	if err != nil {
		return nil, fmt.Errorf("config overlay %s: %w", path, err)
	}
	return l, nil
}

// overlaySection copies every field of src whose toml key is defined in the
// overlay into dst.
func overlaySection(md toml.MetaData, section string, dst, src any) {
	dv := reflect.ValueOf(dst).Elem()
	sv := reflect.ValueOf(src).Elem()
	t := dv.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("toml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		if md.IsDefined(section, tag) {
			dv.Field(i).Set(sv.Field(i))
		}
	}
}

// Defaults returns the built-in defaults (Kinds and Roles normalized,
// {{repo}} paths unexpanded); Load overlays the file on top.
func Defaults() *Config {
	c := &Config{
		Daemon: Daemon{
			PollInterval:             Duration{30 * time.Second},
			MaxConcurrentReviews:     3,
			MaxTotalWorkingCodex:     5,
			PushQuietPeriod:          Duration{5 * time.Minute},
			MinRereviewInterval:      Duration{30 * time.Minute},
			DraftMinRereviewInterval: Duration{2 * time.Hour},
			MaxRoundsPerPRPerDay:     12,
			CloseGrace:               Duration{10 * time.Minute},
			ReviewerTimeout:          Duration{40 * time.Minute},
			JudgeTimeout:             Duration{90 * time.Minute},
			AgentStartStagger:        Duration{15 * time.Second},
			MinWarm:                  Duration{30 * time.Minute},
			HumanCooldown:            Duration{15 * time.Minute},
			ReconcileInterval:        Duration{10 * time.Minute},
			MinFreeDiskGB:            15,
			KeepEvents:               Duration{30 * 24 * time.Hour},
			KeepRequests:             Duration{7 * 24 * time.Hour},
			MaxRoundRestarts:         2,
			BurstQuietPeriod:         Duration{15 * time.Minute},
			BurstPushes:              3,
			BurstWindow:              Duration{30 * time.Minute},
			ModelLimitCooldown:       Duration{5 * time.Hour},
			ParkIdleAfter:            Duration{2 * time.Hour},
			SkipTrivialDeltas:        slices.Clone(TrivialDeltaClasses),
			RequestDebounce:          Duration{time.Minute},
			ReplyDebounce:            Duration{3 * time.Minute},
			ReplyMinInterval:         Duration{2 * time.Hour},
			RereviewMinLines:         30,
			RereviewMaxWait:          Duration{2 * time.Hour},
			DeltaCheck:               true,
		},
		Herdr:    Herdr{Socket: "~/.config/herdr/herdr.sock", Notify: true},
		Terminal: Terminal{App: "Terminal", Session: "default", Mouse: true, Icons: "unicode"},
		GitHub:   GitHub{Transport: "gh"},
		Pipeline: Pipeline{PromptsDir: "{{repo}}/prompts", JudgeOwnPass: OwnPassParallel, RelatedLookback: Duration{14 * 24 * time.Hour}, RelatedIgnore: DefaultRelatedIgnore(), JudgeFreshAfter: Duration{90 * time.Minute}},
		Usage:    Usage{CodexSoft: 80, CodexHard: 95},
		Triage:   DefaultTriage(),
		Learn:    DefaultLearn(),
		Notes:    DefaultNotes(),
		Board:    Board{RecentClosed: Duration{24 * time.Hour}, Shimmer: true},
	}
	c.Normalize()
	return c
}

// repoPath expands ~ and {{repo}} (the checkout) in a configured path; ""
// when it names the checkout and there is none (an installed binary): the
// embedded prompts and skill stand in.
func (c *Config) repoPath(p string) string {
	if strings.Contains(p, "{{repo}}") {
		if c.Layout.Home == "" {
			return ""
		}
		p = strings.ReplaceAll(p, "{{repo}}", c.Layout.Home)
	}
	return paths.Expand(p)
}

// expand resolves ~ and {{repo}} in paths and fills per-watch and per-pool
// defaults. It runs before Normalize, which reads prompts from prompts_dir.
func (c *Config) expand() {
	c.Herdr.Socket = paths.Expand(c.Herdr.Socket)
	c.Usage.CodexHome = paths.Expand(c.Usage.CodexHome)
	c.Pipeline.PromptsDir = c.repoPath(c.Pipeline.PromptsDir)
	if d := c.Pipeline.PromptsDir; d != "" && !filepath.IsAbs(d) && c.Layout.Home != "" {
		c.Pipeline.PromptsDir = filepath.Join(c.Layout.Home, d)
	}
	for i := range c.Identities {
		if f := paths.Expand(c.Identities[i].PrivateKeyFile); f != "" && !filepath.IsAbs(f) && c.Layout.ConfigDir() != "" {
			c.Identities[i].PrivateKeyFile = filepath.Join(c.Layout.ConfigDir(), f)
		} else {
			c.Identities[i].PrivateKeyFile = f
		}
	}
	for i := range c.Watches {
		c.Watches[i].CloneRoot = paths.Expand(c.Watches[i].CloneRoot)
		if c.Watches[i].CloneRoot == "" {
			c.Watches[i].CloneRoot = paths.Expand("~/Projects")
		}
		if c.Watches[i].PollIdentity == "" {
			c.Watches[i].PollIdentity = c.Watches[i].Identity
		}
	}
	for i := range c.Pools {
		c.Pools[i].MainClone = paths.Expand(c.Pools[i].MainClone)
		if c.Pools[i].Base == "" {
			c.Pools[i].Base = "master"
		}
		if c.Pools[i].MinFreeDiskGB == 0 {
			c.Pools[i].MinFreeDiskGB = c.Daemon.MinFreeDiskGB
		}
		if c.Pools[i].IdleRemoveAfter.Duration == 0 {
			c.Pools[i].IdleRemoveAfter.Duration = DefaultIdleRemoveAfter
		}
	}
}

// DefaultIdleRemoveAfter is a pool's idle_remove_after when it sets none (or
// 0): a free slot above pool.min is removed once it has been idle this long.
// Without it every reconcile removed the surplus at once, and the next
// provision wrote about 1 GB again.
const DefaultIdleRemoveAfter = 168 * time.Hour

// IdentityByName returns the identity or nil.
func (c *Config) IdentityByName(name string) *Identity {
	for i := range c.Identities {
		if c.Identities[i].Name == name {
			return &c.Identities[i]
		}
	}
	return nil
}

// WatchFor returns the watch that covers owner/name, or nil.
func (c *Config) WatchFor(fullName string) *Watch {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return nil
	}
	for i := range c.Watches {
		if c.Watches[i].Matches(owner, name) {
			return &c.Watches[i]
		}
	}
	return nil
}

// RepoFor returns the [[repo]] block for owner/name, or nil.
func (c *Config) RepoFor(fullName string) *Repo {
	for i := range c.Repos {
		if strings.EqualFold(c.Repos[i].Repo, fullName) {
			return &c.Repos[i]
		}
	}
	return nil
}

// VerdictsFor returns the review events the judge posts for repository
// fullName ("owner/name") as identity id: noFindings when it finds nothing
// (APPROVE | COMMENT), blocking when it finds a blocking issue
// (REQUEST_CHANGES | COMMENT). Each comes from the repository's [[repo]]
// block when it sets it, else from the identity, else the default:
// APPROVE for a gh identity and COMMENT for an app or an unknown one (a bot
// approval must not unlock a merge by accident), and REQUEST_CHANGES.
func (c *Config) VerdictsFor(fullName string, id *Identity) (noFindings, blocking string) {
	var r *Repo
	if c != nil {
		r = c.RepoFor(fullName)
	}
	pick := func(repo, ident, def string) string {
		switch {
		case r != nil && repo != "":
			return repo
		case ident != "":
			return ident
		}
		return def
	}
	var idNF, idBE string
	def := "COMMENT"
	if id != nil {
		idNF, idBE = id.NoFindingsEvent, id.BlockingEvent
		if id.Kind == "gh" {
			def = "APPROVE"
		}
	}
	var repoNF, repoBE string
	if r != nil {
		repoNF, repoBE = r.NoFindingsEvent, r.BlockingEvent
	}
	return pick(repoNF, idNF, def), pick(repoBE, idBE, "REQUEST_CHANGES")
}

// PoolFor returns the pool for owner/name, or nil (per-PR worktree repos).
func (c *Config) PoolFor(fullName string) *Pool {
	for i := range c.Pools {
		if strings.EqualFold(c.Pools[i].Repo, fullName) {
			return &c.Pools[i]
		}
	}
	return nil
}

// DefaultReadyTimeout bounds a round's whole readiness step when neither the
// [[repo]] nor the [[pool]] sets ready_timeout.
const DefaultReadyTimeout = 5 * time.Minute

// Readiness is a repository's verification readiness step: the commands a
// round runs in the checkout before the reviewers (see Pool.Prepare).
type Readiness struct {
	Prepare []string
	Ready   []string
	Timeout time.Duration // the budget of all of them together
}

// ReadinessFor returns the readiness step of repository fullName
// ("owner/name"): each of prepare, ready and ready_timeout from its [[repo]]
// block when the block sets it, else from its [[pool]]; the timeout defaults
// to DefaultReadyTimeout.
func (c *Config) ReadinessFor(fullName string) Readiness {
	var out Readiness
	if p := c.PoolFor(fullName); p != nil {
		out = Readiness{Prepare: p.Prepare, Ready: p.Ready, Timeout: p.ReadyTimeout.Duration}
	}
	if r := c.RepoFor(fullName); r != nil {
		if len(r.Prepare) > 0 {
			out.Prepare = r.Prepare
		}
		if len(r.Ready) > 0 {
			out.Ready = r.Ready
		}
		if r.ReadyTimeout.Duration > 0 {
			out.Timeout = r.ReadyTimeout.Duration
		}
	}
	if out.Timeout <= 0 {
		out.Timeout = DefaultReadyTimeout
	}
	return out
}

// KeepApprovals reports whether an App identity's approval on a PR of
// repository fullName stays when the PR gets new commits: the [[repo]]
// block's keep_approvals when set, else the covering watch's (default
// false: magnum dismisses it until the re-review posts).
func (c *Config) KeepApprovals(fullName string) bool {
	if r := c.RepoFor(fullName); r != nil && r.KeepApprovals != nil {
		return *r.KeepApprovals
	}
	if w := c.WatchFor(fullName); w != nil {
		return w.KeepApprovals
	}
	return false
}

// TrackersFor returns the issue trackers of repository fullName's PRs: the
// [[repo]] block's, then the covering [[watch]]'s, then [board]'s, each
// issue key prefix taken from the first of them that names it. So one
// owner's "PR-" may lead to Linear while every other owner's leads to
// Jira. Templates that do not parse are left out (Validate reports them).
func (c *Config) TrackersFor(fullName string) []Tracker {
	var levels [][]string
	if r := c.RepoFor(fullName); r != nil {
		levels = append(levels, r.Trackers)
	}
	if w := c.WatchFor(fullName); w != nil {
		levels = append(levels, w.Trackers)
	}
	levels = append(levels, c.Board.Trackers)
	var out []Tracker
	for _, level := range levels {
		for _, t := range parseTrackers(level) {
			if !slices.ContainsFunc(out, func(o Tracker) bool { return o.Prefix == t.Prefix }) {
				out = append(out, t)
			}
		}
	}
	return out
}

// RequiredChecks returns the [[repo]] block's required_checks for
// repository fullName (nil when none): check-name globs and
// "workflow:<glob>" entries that replace GitHub's list
// (store.Store.RequiredChecks).
func (c *Config) RequiredChecks(fullName string) []string {
	if r := c.RepoFor(fullName); r != nil {
		return r.RequiredChecks
	}
	return nil
}
