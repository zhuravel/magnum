package config

import (
	"cmp"
	"encoding"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// The review pipeline is configuration, "a Procfile of agents": [kinds.<name>]
// says how to drive an agent CLI, [[role]] says what each agent of a round
// does, [[watch]] roles picks the roles a watched repository runs and
// [pipeline] says where the prompt files live. Without any of it the built-in
// defaults reproduce the four classic roles (DefaultRoles) on the codex and
// claude CLIs (DefaultKinds).

// Pipeline is the [pipeline] section.
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
	// two PRs related and that the changed files' history.json and docs.json
	// leave out (default DefaultRelatedIgnore, the lockfiles; [] = none). A
	// [[watch]] may override it (Watch.RelatedIgnore).
	RelatedIgnore []string `toml:"related_ignore"`
	// JudgeFreshAfter: a judge whose last turn on the PR ended longer ago
	// than this starts the PR's next round in a fresh session (the recovery
	// path, which reads the earlier reviews and threads from GitHub) instead
	// of resuming its conversation, whose prompt cache has gone cold by
	// then (it lasts about 1.5 hours), so its first turn would re-read the
	// whole conversation uncached. Default 90m; 0 = always resume. Only the
	// judge; a continue of a paused turn keeps its conversation.
	JudgeFreshAfter Duration `toml:"judge_fresh_after"`
	// FullPassEvery: the Nth automatic re-review of a PR, counted since its
	// first review or its last full pass, runs as a scheduled full pass:
	// every role starts in a fresh session, the reviewers review the whole
	// PR with their first-review prompts and effort, and the judge reviews
	// the full diff again through the recovery prompt (engine fullpass.go).
	// A re-review of the same head, a reply round, a CI check and a round
	// someone asked for never become one; a delta check does, when one is
	// due. Default DefaultFullPassEvery; 0 = this rule off.
	FullPassEvery int `toml:"full_pass_every"`
	// FullPassLines: an automatic re-review is also a full pass once the
	// lines the PR's re-reviews changed since its baseline (its last full
	// pass, else its first review), added plus deleted, lockfiles and
	// related_ignore's paths left out, reach the larger of this and a
	// quarter of the PR's lines at the baseline (engine fullpass.go).
	// Default DefaultFullPassLines; 0 = this rule off.
	FullPassLines int `toml:"full_pass_lines"`
	// FullPassFiles: likewise once the files those re-reviews touched
	// reach the larger of this and a quarter of the PR's files at the
	// baseline. Default DefaultFullPassFiles; 0 = this rule off.
	FullPassFiles int `toml:"full_pass_files"`
	// FullPassAfter: an automatic re-review of a large PR (50 files or 2,000
	// lines at its baseline) is also a full pass once this long passed
	// since the baseline. Default DefaultFullPassAfter; 0 = this rule off.
	FullPassAfter Duration `toml:"full_pass_after"`
	// AuditEvery: the Nth automatic re-review of a PR, counted since its
	// first review, its last full pass or its last audit pass, runs as an
	// audit pass while the PR's evidence map holds unknown or stale areas:
	// every session starts fresh, the reviewers re-review the delta, and the
	// judge first probes at most three of those areas, the riskiest first,
	// and one earlier probe again (engine audit.go). A full pass due on the
	// same round wins. Default DefaultAuditEvery; 0 = never.
	AuditEvery int `toml:"audit_every"`
	// ApproveAfterFullPass: a re-review (a delta check, a recovery, an audit
	// pass or a round someone asked for alike) of a PR that has this many
	// re-reviews or more since its baseline (its last full pass, else its
	// first review) cannot approve: its clean verdict posts as a comment,
	// and a full pass of the whole PR on the same head follows at once,
	// whose clean verdict approves (engine approval_hold.go). A full pass, a
	// post-merge review, a reply round, a CI check and a dry run never hold.
	// Default DefaultApproveAfterFullPass; 0 = off.
	ApproveAfterFullPass int `toml:"approve_after_full_pass"`
	// JudgeFallback names a judge role of another kind that takes over a
	// PR's rounds after the judge's kind refused one of them (Codex's
	// cybersecurity classifier; engine codex_flag.go): the PR is flagged
	// for that kind, its roles of that kind are dropped and this role
	// judges from then on. "" = a refused PR is never reviewed again. A
	// [[watch]] may override it (Watch.JudgeFallback); see
	// Config.JudgeFallbackFor. The role never runs otherwise: RolesFor gives
	// a watch without roles its first judge only.
	JudgeFallback string `toml:"judge_fallback"`
	// FixRole names the fix role that fixes magnum's findings on the
	// operator's own PRs (the fix job; `magnum fix`): a [[role]] with
	// fix = true. "" = no fix job. A [[watch]] may override it
	// (Watch.FixRole); see Config.FixRoleFor.
	FixRole string `toml:"fix_role"`
	// FixCycles caps the fix runs of one fix loop (a fix run, the re-review
	// it forces, and so on) before the PR needs the operator; at least 1,
	// default DefaultFixCycles. A [[watch]] may override it; see
	// Config.FixCyclesFor.
	FixCycles int `toml:"fix_cycles"`
	// Autofix: AutofixNever (default) starts a fix loop only when asked
	// (`magnum fix`, the board); AutofixOwn starts it by itself after a
	// review with findings on a PR whose author is one of the operator's
	// logins. A [[watch]] may override it; see Config.AutofixFor.
	Autofix string `toml:"autofix"`
	// Whiteboard switches the whiteboard job on: after every review magnum
	// posts on a new head, the whiteboard role writes a whiteboard of the
	// PR (an interactive document in the dev.fast Whiteboard app) in the
	// PR's slot. Default false. A [[watch]] may override it
	// (Watch.Whiteboard); see Config.WhiteboardFor.
	Whiteboard bool `toml:"whiteboard"`
	// WhiteboardRole names the whiteboard role that writes it: a [[role]]
	// with whiteboard = true, RoleClaudeWhiteboard by default. "" = no
	// whiteboard job at all, automatic or by hand. A [[watch]] may
	// override it (Watch.WhiteboardRole); see Config.WhiteboardRoleFor.
	WhiteboardRole string `toml:"whiteboard_role"`
	// Spend is the spend level, how hard and on which models the roles
	// think: SpendMedium (default; "" counts as it) keeps the effort and
	// rereview_effort config.defaults.toml writes out, SpendHigh makes every
	// one xhigh, SpendLow takes each one notch down, to medium at the lowest
	// (see spendEffort); Load applies that notch mapping to the values a
	// built-in role (DefaultRoles) inherits, never to a key a [[role]] block
	// of a config file sets, nor to a role that is not built in. Any other
	// level is the operator's own, declared by a [spend.<level>] table
	// (Config.SpendLevels), and starts from SpendMedium. The active level's
	// table then sets the keys it names on its roles over everything else
	// (applySpend). A kind's own level (Kind.Spend) takes its place for the
	// roles of that kind (Config.SpendLevel).
	Spend string `toml:"spend"`
	// StatusComment switches the review status comment on: one comment on
	// the conversation of each PR magnum means to review, posted as the PR's
	// posting identity and edited in place as the review is queued and runs,
	// collapsed to one line once it posts, and ended by a last line when the
	// PR is closed, merged or turned into a draft before that (engine,
	// status_comment.go). Default false. A [[watch]] may override it
	// (Watch.StatusComment); see Config.StatusCommentFor.
	StatusComment bool `toml:"status_comment"`
	// StatusCommentNote is a line the operator writes under the status
	// comment while a review is queued, under way or held, not under the
	// posted line nor a last line ("" = none).
	StatusCommentNote string `toml:"status_comment_note"`
}

// JudgeFallbackFor is the judge role that takes over a PR of watch w after
// its judge's kind refused a round: the watch's judge_fallback, else
// [pipeline] judge_fallback, found among every declared role (not only the
// watch's). ok is false when none is set or the name matches no judge
// role; Validate reports the latter.
func (c *Config) JudgeFallbackFor(w *Watch) (Role, bool) {
	name := c.Pipeline.JudgeFallback
	if w != nil && w.JudgeFallback != "" {
		name = w.JudgeFallback
	}
	if strings.TrimSpace(name) == "" {
		return Role{}, false
	}
	r, ok := c.roleNamed(name)
	if !ok || !r.Judge {
		return Role{}, false
	}
	return r, true
}

// FixRoleFor is the fix role of watch w: the watch's fix_role when it sets
// one (an empty one switches the fix job off for the watch), else [pipeline]
// fix_role, found among every declared role (not only the watch's). ok is
// false when none is set or the name matches no fix role; Validate reports
// the latter. A fix role that names no identity acts as w's poll identity
// (its identity when it has no poll_identity, as Normalize sets it): the
// operator's own gh login in a `magnum init` config, which polls as it.
// Without a watch the identity stays empty.
func (c *Config) FixRoleFor(w *Watch) (Role, bool) {
	name := c.FixRoleName(w)
	if strings.TrimSpace(name) == "" {
		return Role{}, false
	}
	r, ok := c.roleNamed(name)
	if !ok || !r.Fix {
		return Role{}, false
	}
	if r.Identity == "" && w != nil {
		r.Identity = cmp.Or(w.PollIdentity, w.Identity)
	}
	return r, true
}

// FixRoleName is the fix_role that applies to w: the watch's when it sets
// one, else [pipeline]'s ("" = no fix job). FixRoleFor resolves it.
func (c *Config) FixRoleName(w *Watch) string {
	if w != nil && w.FixRole != nil {
		return *w.FixRole
	}
	return c.Pipeline.FixRole
}

// WhiteboardFor reports whether the whiteboard job is on for watch w: the
// watch's whiteboard when it sets one, else [pipeline] whiteboard. A nil w
// is [pipeline]'s. The job still needs a role: WhiteboardRoleFor.
func (c *Config) WhiteboardFor(w *Watch) bool {
	if w != nil && w.Whiteboard != nil {
		return *w.Whiteboard
	}
	return c.Pipeline.Whiteboard
}

// StatusCommentFor reports whether the review status comment is on for
// watch w: the watch's status_comment when it sets one, else [pipeline]
// status_comment. A nil w is [pipeline]'s.
func (c *Config) StatusCommentFor(w *Watch) bool {
	if w != nil && w.StatusComment != nil {
		return *w.StatusComment
	}
	return c.Pipeline.StatusComment
}

// WhiteboardRoleFor is the whiteboard role of watch w: the watch's
// whiteboard_role when it sets one (an empty one switches the whiteboard
// job off for the watch, automatic or by hand), else [pipeline]
// whiteboard_role, found among every declared role (not only the watch's).
// ok is false when none is set or the name matches no whiteboard role;
// Validate reports the latter. A whiteboard role that names no identity
// acts as w's poll identity (its identity when it has no poll_identity, as
// Normalize sets it): the operator's own gh login in a `magnum init`
// config, which polls as it. Without a watch the identity stays empty.
func (c *Config) WhiteboardRoleFor(w *Watch) (Role, bool) {
	name := c.WhiteboardRoleName(w)
	if strings.TrimSpace(name) == "" {
		return Role{}, false
	}
	r, ok := c.roleNamed(name)
	if !ok || !r.Whiteboard {
		return Role{}, false
	}
	if r.Identity == "" && w != nil {
		r.Identity = cmp.Or(w.PollIdentity, w.Identity)
	}
	return r, true
}

// WhiteboardRoleName is the whiteboard_role that applies to w: the watch's
// when it sets one, else [pipeline]'s ("" = no whiteboard job).
// WhiteboardRoleFor resolves it.
func (c *Config) WhiteboardRoleName(w *Watch) string {
	if w != nil && w.WhiteboardRole != nil {
		return *w.WhiteboardRole
	}
	return c.Pipeline.WhiteboardRole
}

// Values of [pipeline] autofix.
const (
	AutofixNever = "never" // a fix loop starts only on request
	AutofixOwn   = "own"   // and after a review with findings on the operator's own PRs
)

// DefaultFixCycles is [pipeline] fix_cycles' default: three fix runs, each
// followed by a re-review, before the PR is left to the operator.
const DefaultFixCycles = 3

// DefaultFullPassEvery is [pipeline] full_pass_every's default: every tenth
// automatic re-review of a PR reviews the whole PR again.
const DefaultFullPassEvery = 10

// DefaultFullPassLines and DefaultFullPassFiles are [pipeline]
// full_pass_lines' and full_pass_files' defaults: the churn since a PR's
// baseline that makes its next automatic re-review a full pass, unless a
// quarter of the PR's size at the baseline is more.
const (
	DefaultFullPassLines = 500
	DefaultFullPassFiles = 10
)

// DefaultFullPassAfter is [pipeline] full_pass_after's default: a large
// PR's next automatic re-review a week after its baseline is a full pass.
const DefaultFullPassAfter = 7 * 24 * time.Hour

// DefaultAuditEvery is [pipeline] audit_every's default: every fifth
// automatic re-review of a PR probes what no round probed.
const DefaultAuditEvery = 5

// DefaultApproveAfterFullPass is [pipeline] approve_after_full_pass'
// default: the first two re-reviews after a PR's baseline approve on their
// delta, so one round of fixes after the first review approves at once.
const DefaultApproveAfterFullPass = 2

// FixCyclesFor is the fix_cycles that apply to w's PRs: the watch's when it
// sets one, else [pipeline]'s, else DefaultFixCycles. A nil w is
// [pipeline]'s.
func (c *Config) FixCyclesFor(w *Watch) int {
	n := c.Pipeline.FixCycles
	if w != nil && w.FixCycles != nil {
		n = *w.FixCycles
	}
	if n < 1 {
		return DefaultFixCycles
	}
	return n
}

// AutofixFor is the autofix that applies to w's PRs: the watch's when it
// sets one, else [pipeline]'s, else AutofixNever. A nil w is [pipeline]'s.
func (c *Config) AutofixFor(w *Watch) string {
	if w != nil && w.Autofix != "" {
		return w.Autofix
	}
	if c.Pipeline.Autofix != "" {
		return c.Pipeline.Autofix
	}
	return AutofixNever
}

// DefaultRelatedIgnore is [pipeline] related_ignore's default: lockfiles,
// which most dependency changes touch whatever else they do.
func DefaultRelatedIgnore() []string {
	return []string{"**/Gemfile.lock", "**/package-lock.json", "**/pnpm-lock.yaml", "**/yarn.lock", "**/bun.lock", "**/go.sum",
		"**/poetry.lock", "**/uv.lock", "**/Pipfile.lock", "**/Cargo.lock", "**/composer.lock"}
}

// Related is what decides the related PRs of a watch's PR (Config.RelatedFor).
type Related struct {
	Lookback time.Duration // a merged PR within it is related
	Ignore   []string      // path globs that never relate two PRs
}

// RelatedFor is the related_lookback and related_ignore that apply to w's
// PRs: the watch's when it sets them (any lookback, "0s" included: open PRs
// only; any list, [] included), else [pipeline]'s. A nil w is [pipeline]'s.
func (c *Config) RelatedFor(w *Watch) Related {
	r := Related{Lookback: c.Pipeline.RelatedLookback.Duration, Ignore: c.Pipeline.RelatedIgnore}
	if w != nil && w.RelatedLookback != nil {
		r.Lookback = w.RelatedLookback.Duration
	}
	if w != nil && w.RelatedIgnore != nil {
		r.Ignore = w.RelatedIgnore
	}
	if r.Ignore == nil {
		r.Ignore = []string{}
	}
	return r
}

// Values of [pipeline] spend.
const (
	SpendLow    = "low"    // each built-in effort one notch down, medium the floor
	SpendMedium = "medium" // the built-in efforts as config.defaults.toml writes them
	SpendHigh   = "high"   // xhigh, the strongest effort both Codex and Claude take, everywhere
)

// SpendRole is a role's entry in a [spend.<level>] table: the keys the level
// sets on the role while [pipeline] spend names it, over the role's own
// [[role]] keys and the notch mapping alike; nil = the role's value stays.
type SpendRole struct {
	Model          *string `toml:"model"`
	Effort         *string `toml:"effort"`
	RereviewEffort *string `toml:"rereview_effort"`
	MaxSubagents   *int    `toml:"max_subagents"`
}

// spendLevelName is what a [spend.<level>] table's level name must match.
var spendLevelName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,15}$`)

// isBuiltinSpend reports whether level is one of the three built-in levels
// (or "", which counts as SpendMedium).
func isBuiltinSpend(level string) bool {
	return level == "" || level == SpendLow || level == SpendMedium || level == SpendHigh
}

// mergeSpendLevels merges over's [spend.<level>] tables into base's, level by
// level, role by role and key by key, a key over sets winning, and returns
// the result (base, filled in place).
func mergeSpendLevels(base, over map[string]map[string]SpendRole) map[string]map[string]SpendRole {
	for level, roles := range over {
		if base == nil {
			base = map[string]map[string]SpendRole{}
		}
		if base[level] == nil {
			base[level] = map[string]SpendRole{}
		}
		for name, o := range roles {
			b := base[level][name]
			b.Model, b.Effort, b.RereviewEffort = cmp.Or(o.Model, b.Model), cmp.Or(o.Effort, b.Effort), cmp.Or(o.RereviewEffort, b.RereviewEffort)
			b.MaxSubagents = cmp.Or(o.MaxSubagents, b.MaxSubagents)
			base[level][name] = b
		}
	}
	return base
}

// spendEffort is a built-in role's effort (or rereview_effort) under the
// spend level's notch mapping: xhigh for SpendHigh, empty or not; for
// SpendLow xhigh becomes high and high medium, and anything else (medium,
// "") stays; for SpendMedium, "" and an operator's own level the effort as
// it is.
func spendEffort(spend, effort string) string {
	switch spend {
	case SpendHigh:
		return "xhigh"
	case SpendLow:
		switch effort {
		case "xhigh":
			return "high"
		case "high":
			return "medium"
		}
	}
	return effort
}

// Values of [pipeline] judge_own_pass.
const (
	OwnPassParallel = "parallel" // the judge's own pass runs with the reviewers
	OwnPassAfter    = "after"    // one judge prompt after the reviewers
)

// JudgeOwnPassFor is the judge_own_pass that applies to w's PRs: the
// watch's when it sets one, else [pipeline]'s, else OwnPassParallel.
func (c *Config) JudgeOwnPassFor(w *Watch) string {
	if w != nil && w.JudgeOwnPass != "" {
		return w.JudgeOwnPass
	}
	if c.Pipeline.JudgeOwnPass != "" {
		return c.Pipeline.JudgeOwnPass
	}
	return OwnPassParallel
}

// Role kinds, modes and option values.
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
	// WrapperBypass: the pane's shell function or alias named like the kind
	// is removed before every launch and resume (`unset -f`, `unalias`), so
	// the binary runs with magnum's args alone; Kind.Args are appended.
	WrapperBypass = "bypass"

	SessionHerdr = "herdr" // herdr reports the CLI's session id (pane agent_session); resume uses it
	SessionNone  = "none"  // no session id: the role always starts fresh
)

// Prompt kinds accepted by Role.PromptFile.
const (
	PromptInitial  = "initial"  // Role.Prompt: first review of a PR (shell roles: the full command line)
	PromptRereview = "rereview" // Role.Rereview: a new head after an earlier review
	PromptRestart  = "restart"  // Role.Restart: a push cut the reviewer's turn short; the round restarted on the new head
	PromptContinue = "continue" // Role.ContinuePrompt: a pause ended mid-turn (judge)
	PromptRecovery = "recovery" // Role.Recovery: a fresh session after the old one was lost (judge)
	PromptNudge    = "nudge"    // Role.Nudge: the agent stopped without a result (judge)
	PromptOwnPass  = "own_pass" // Role.OwnPass: the judge's own pass, prompted with the reviewers (judge)
)

// PromptKinds lists every prompt kind a role may name (Role.PromptFile).
var PromptKinds = []string{PromptInitial, PromptRereview, PromptRestart, PromptContinue, PromptRecovery, PromptNudge, PromptOwnPass}

// Built-in role names (DefaultRoles) and kinds (DefaultKinds).
const (
	RoleCodexJudge     = "codex-judge"
	RoleClaudeReview   = "claude-review"
	RoleCodexReview    = "codex-review"
	RoleClaudeSimplify = "claude-simplify"
	// RoleClaudeJudge is the built-in second judge, held in reserve for
	// [pipeline] judge_fallback (a watch runs its first judge unless its
	// roles list names this one).
	RoleClaudeJudge = "claude-judge"
	// RoleClaudeFix is the built-in fix role, the fixer of the fix job
	// (Role.Fix); [pipeline] fix_role names it by default.
	RoleClaudeFix = "claude-fix"
	// RoleClaudeWhiteboard is the built-in whiteboard role, the writer of
	// the whiteboard job (Role.Whiteboard); [pipeline] whiteboard_role
	// names it by default.
	RoleClaudeWhiteboard = "claude-whiteboard"

	KindCodex  = "codex"
	KindClaude = "claude"
	KindDroid  = "droid"
	KindOMP    = "omp"
)

// Placeholders substituted by Kind.Argv and in Kind.Rename.
const (
	PlaceholderSession = "{session}"
	PlaceholderTitle   = "{title}"
	PlaceholderModel   = "{model}"
	PlaceholderEffort  = "{effort}"
	// PlaceholderSubagents is Role.MaxSubagents in Kind.Subagents.
	PlaceholderSubagents = "{subagents}"
	// PlaceholderServer is an MCP server's name in Kind.MCPDisable.
	PlaceholderServer = "{server}"
	// PlaceholderPlugin is a plugin's name (name@marketplace) in
	// Kind.PluginDisable.
	PlaceholderPlugin = "{plugin}"
	// PlaceholderProjects is the TOML table of untrusted paths in
	// Kind.ProjectUntrust (Kind.UntrustArgs), optional there.
	PlaceholderProjects = "{projects}"
)

// DefaultSkill is the judge's default skill path ({{repo}} = magnum's home).
const DefaultSkill = "{{repo}}/skills/magnum-review/SKILL.md"

// DefaultFixSkill is a fix role's default skill path, as DefaultSkill is the
// judge's; DefaultFixPrompt is its default prompt.
const (
	DefaultFixSkill  = "{{repo}}/skills/magnum-fix/SKILL.md"
	DefaultFixPrompt = "fix.md"
)

// DefaultWhiteboardPrompt is a whiteboard role's default prompt (it has no
// skill: the Whiteboard app's own instructions guide it).
const DefaultWhiteboardPrompt = "whiteboard.md"

// Normalize fills the defaulted fields of Kinds and Roles in place (Load
// and Defaults call it; call it again after editing either in code). It is
// idempotent and never overrides a value that is set.
func (c *Config) Normalize() {
	if c.Kinds == nil {
		c.Kinds = DefaultKinds()
	}
	normalizeKinds(c.Kinds)
	if c.Roles == nil {
		c.Roles = DefaultRoles()
	}
	c.Roles = c.normalizedRoles(c.Roles)
}

// cloneOrNil copies s; an empty list (`args = []`) becomes nil, so a block
// written out explicitly equals the built-in default.
func cloneOrNil[E any](s []E) []E {
	if len(s) == 0 {
		return nil
	}
	return slices.Clone(s)
}

// expandPipeline resolves {{repo}} and ~ in the judges' and fix roles' skills (Load only;
// Defaults keeps them unexpanded). prompts_dir is expanded earlier, by
// expand, because Normalize looks prompts up in it.
func (c *Config) expandPipeline() {
	for i := range c.Roles {
		if s := c.Roles[i].Skill; s != "" {
			c.Roles[i].Skill = c.repoPath(s)
		}
	}
}

// layer is one decoded file's pipeline blocks plus what is needed to tell
// which keys it set.
type layer struct {
	md    toml.MetaData
	kinds map[string]Kind
	roles []Role
	raw   []map[string]any // the [[role]] tables as written, for their key sets
	// builtin: the layer is the embedded config.defaults.toml, whose role
	// keys are the built-in defaults written out, not the operator's.
	builtin bool
}

func readLayer(file string, md toml.MetaData, kinds map[string]Kind, roles []Role) (*layer, error) {
	return readLayerData(file, nil, md, kinds, roles)
}

// readLayerData is readLayer for a layer given as data (the built-in
// defaults); nil data reads file.
func readLayerData(file string, data []byte, md toml.MetaData, kinds map[string]Kind, roles []Role) (*layer, error) {
	var raw struct {
		Role []map[string]any `toml:"role"`
	}
	var err error
	if data != nil {
		_, err = toml.Decode(string(data), &raw)
	} else {
		_, err = toml.DecodeFile(file, &raw)
	}
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", file, err)
	}
	return &layer{md: md, kinds: kinds, roles: roles, raw: raw.Role, builtin: data != nil}, nil
}

// buildPipeline computes Kinds and Roles from the built-in defaults, the
// config file's blocks and the overlay's blocks, in that order, then applies
// each role's spend level (its kind's spend, else [pipeline] spend:
// SpendLevel): its notch mapping to the efforts of the built-in roles that
// no block of the operator's set, and its [spend.<level>] table over them
// (applySpend).
func (c *Config) buildPipeline(base, over *layer) {
	kinds := DefaultKinds()
	builtins := DefaultRoles()
	roles := builtins
	set := map[string]map[string]bool{} // role name -> the keys a [[role]] block of the operator's sets
	mark := func(l *layer, name string, i int) {
		if l.builtin || i >= len(l.raw) {
			return
		}
		if set[name] == nil {
			set[name] = map[string]bool{}
		}
		for k := range l.raw[i] {
			set[name][k] = true
		}
	}
	for _, l := range []*layer{base, over} {
		if l == nil {
			continue
		}
		for name, k := range l.kinds {
			dst, ok := kinds[name]
			if !ok {
				dst = Kind{HealthPatterns: DefaultHealthPatterns(), AfterDenyPrompt: DefaultAfterDenyPrompt}
			}
			mergeDefined(reflect.ValueOf(&dst).Elem(), reflect.ValueOf(k), nil, func(p []string) bool {
				return l.md.IsDefined(append([]string{"kinds", name}, p...)...)
			})
			kinds[name] = dst
		}
	}
	if base != nil && len(base.roles) > 0 {
		roles = nil
		for i, r := range base.roles {
			mark(base, r.Name, i)
			roles = append(roles, mergeRole(builtinRole(builtins, r.Name), r, base.raw, i))
		}
	}
	if over != nil {
		for i, r := range over.roles {
			mark(over, r.Name, i)
			if j := slices.IndexFunc(roles, func(x Role) bool { return x.Name == r.Name }); j >= 0 {
				roles[j] = mergeRole(roles[j], r, over.raw, i)
				continue
			}
			roles = append(roles, mergeRole(builtinRole(builtins, r.Name), r, over.raw, i))
		}
	}
	c.Kinds = kinds
	c.Roles = applySpend(c.SpendLevel, c.SpendLevels, roles, builtins, set)
}

// SpendLevel is the spend level of the roles whose agent kind
// (Role.AgentKind) is kind: the kind's own spend ([kinds.<name>] spend),
// else [pipeline] spend, SpendMedium when neither names one. A shell role
// without a tool (kind "") and an undeclared kind take [pipeline] spend.
func (c *Config) SpendLevel(kind string) string {
	if k, ok := c.kinds()[kind]; ok && k.Spend != "" {
		return k.Spend
	}
	return cmp.Or(c.Pipeline.Spend, SpendMedium)
}

// applySpend applies each role's spend level (level, by the role's agent
// kind: Config.SpendLevel) to roles, in two steps. First the notch mapping:
// the effort and rereview_effort of each built-in role (one named like a
// role of builtins) go through spendEffort, unless a [[role]] block of the
// operator's set that key (set, by role name): an explicit key wins over
// the mapping, in the user config and in a base configuration of their own
// alike, while the embedded config.defaults.toml only writes the built-in
// values out; a role that is not built in keeps what it has. Then the
// level's table (levels[level]): each role it names, built in or not,
// takes the keys the table sets, over its own [[role]] keys and the
// mapping alike, the table being the more specific intent. A name no role
// has is skipped (Validate reports it).
func applySpend(level func(kind string) string, levels map[string]map[string]SpendRole, roles, builtins []Role, set map[string]map[string]bool) []Role {
	for i := range roles {
		r := &roles[i]
		spend := level(r.AgentKind())
		if slices.ContainsFunc(builtins, func(b Role) bool { return b.Name == r.Name }) {
			if !set[r.Name]["effort"] {
				r.Effort = spendEffort(spend, r.Effort)
			}
			if !set[r.Name]["rereview_effort"] {
				r.RereviewEffort = spendEffort(spend, r.RereviewEffort)
			}
		}
		t, ok := levels[spend][r.Name]
		if !ok {
			continue
		}
		if t.Model != nil {
			r.Model = *t.Model
		}
		if t.Effort != nil {
			r.Effort = *t.Effort
		}
		if t.RereviewEffort != nil {
			r.RereviewEffort = *t.RereviewEffort
		}
		if t.MaxSubagents != nil {
			r.MaxSubagents = new(*t.MaxSubagents)
		}
	}
	return roles
}

func builtinRole(builtins []Role, name string) Role {
	for _, b := range builtins {
		if b.Name == name {
			return b
		}
	}
	return Role{}
}

// mergeRole copies the keys the i-th raw [[role]] table sets from r onto base.
func mergeRole(base, r Role, raw []map[string]any, i int) Role {
	var keys map[string]any
	if i < len(raw) {
		keys = raw[i]
	}
	mergeDefined(reflect.ValueOf(&base).Elem(), reflect.ValueOf(r), nil, func(p []string) bool {
		_, ok := keys[p[0]]
		return ok
	})
	return base
}

var textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()

// mergeDefined copies every toml-tagged field of src into dst whose key path
// is defined; plain struct fields (not text-unmarshalled ones such as
// Duration) recurse with the longer path.
func mergeDefined(dst, src reflect.Value, path []string, defined func([]string) bool) {
	t := dst.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		p := append(slices.Clone(path), tag)
		if f.Type.Kind() == reflect.Struct && !reflect.PointerTo(f.Type).Implements(textUnmarshalerType) {
			mergeDefined(dst.Field(i), src.Field(i), p, defined)
			continue
		}
		if defined(p) {
			dst.Field(i).Set(src.Field(i))
		}
	}
}
