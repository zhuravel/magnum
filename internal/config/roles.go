package config

import (
	"slices"
	"strings"
	"time"
)

// Role is a [[role]] block: one agent (or shell command) of a review round.
// After Load (or Normalize) every defaulted field below holds its resolved
// value, so consumers read fields directly.
//
// A round runs the watch's roles (Config.RolesFor) in Stages: non-judge
// roles in parallel unless After orders them, the judge last. Each role
// writes ReportFile in the round's report directory
// (paths.Layout.ReviewDir); the judge reads the others' reports and posts
// the review.
//
// A [[role]] in config.toml named like a built-in role (DefaultRoles)
// inherits that role's fields for every key it does not set. Declaring any
// [[role]] in the base (config.defaults.toml, or a --config file) replaces the built-in list; the user config
// [[role]] blocks merge into it by name (or are appended).
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
	// judge-recovery.md, judge-nudge.md and judge-stop.md; other session
	// roles use <name>.md, <name>-rereview.md (only when it exists, else
	// Prompt) and <name>-restart.md (only when it exists, else none). A
	// shell role may name a full-line shell template (codex-review.sh) in
	// Prompt instead of setting Command.
	Prompt   string `toml:"prompt"`
	Rereview string `toml:"rereview"`
	// Restart: a session reviewer whose turn a push cut short (the round
	// restarted on the new head before the judge was prompted); "" = the
	// role gets the prompt it ran (Prompt or Rereview) on the new head.
	Restart        string `toml:"restart"`
	ContinuePrompt string `toml:"continue_prompt"`
	Recovery       string `toml:"recovery"`
	Nudge          string `toml:"nudge"`
	Stop           string `toml:"stop"`

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
	// <name>.json for a judge, <name>.patch for capture = "git-diff", else
	// <name>.md.
	Output string `toml:"output"`
	// Capture: "file", "stdout" or "git-diff" (see CaptureFile). Default
	// "stdout" for shell roles, else "file". "stdout" is for shell roles
	// only; a judge uses "file".
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

// IsShell reports whether the role runs a shell command (kind "shell").
func (r Role) IsShell() bool { return r.Kind == KindShell || r.Mode == ModeShell }

// IsAgent reports whether the role runs an interactive agent session.
func (r Role) IsAgent() bool { return !r.IsShell() }

// AgentKind is the agent CLI whose login preflight, pauses and health
// patterns apply to the role: Kind for session roles, Tool for shell roles
// ("" when the command uses none).
func (r Role) AgentKind() string {
	if r.IsShell() {
		return r.Tool
	}
	return r.Kind
}

// StatusOK reports whether a shell command's exit status counts as a
// finished report: only 0 when OKStatus is empty, else any status in it.
func (r Role) StatusOK(status int) bool {
	if len(r.OKStatus) == 0 {
		return status == 0
	}
	return slices.Contains(r.OKStatus, status)
}

// ReportFile is the file the role writes in the round's report directory.
func (r Role) ReportFile() string {
	if r.Output != "" {
		return r.Output
	}
	return defaultOutput(r)
}

// PromptFile returns the prompt file name for a prompt kind (PromptInitial,
// PromptRereview, PromptRestart, PromptContinue, PromptRecovery,
// PromptNudge, PromptStop); "" when the role has none (shell roles driven
// by Command, non-judge roles for continue/recovery/nudge/stop unless set,
// a role without a restart prompt, unknown kinds). Rereview falls back to
// the initial prompt.
func (r Role) PromptFile(kind string) string {
	switch kind {
	case PromptInitial:
		return r.Prompt
	case PromptRereview:
		if r.Rereview != "" {
			return r.Rereview
		}
		return r.Prompt
	case PromptRestart:
		return r.Restart
	case PromptContinue:
		return r.ContinuePrompt
	case PromptRecovery:
		return r.Recovery
	case PromptNudge:
		return r.Nudge
	case PromptStop:
		return r.Stop
	}
	return ""
}

// EffortFor is the role's effort for a round: RereviewEffort (when set) for
// a re-review round, else Effort.
func (r Role) EffortFor(rereview bool) string {
	if rereview && r.RereviewEffort != "" {
		return r.RereviewEffort
	}
	return r.Effort
}

// Matches reports whether s (case and surrounding space ignored) is the
// role's name or one of its aliases.
func (r Role) Matches(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return false
	}
	if s == r.Name {
		return true
	}
	return slices.Contains(r.Aliases, s)
}

// ShouldRun applies Runs: ranBefore is whether the role already completed
// for the PR, requested whether the user asked for it this round.
func (r Role) ShouldRun(ranBefore, requested bool) bool {
	switch r.Runs {
	case RunsNever:
		return false
	case RunsManual:
		return requested
	case RunsFirst:
		return requested || !ranBefore
	}
	return true
}

// DefaultRoles returns the built-in roles in display order (fresh copies,
// before normalization; Normalize fills Mode, Runs, Output, Capture,
// Timeout and the judge's prompt names):
//
//   - codex-judge: codex, judge, effort xhigh, rereview_effort high, skill
//     DefaultSkill, prompts judge-*.md, timeout daemon.judge_timeout;
//     aliases judge.
//   - claude-review: claude, prompt claude-review.md, rereview
//     claude-rereview.md, restart claude-restart.md, effort high; aliases
//     claude.
//   - codex-review: shell, tool codex, command "command codex review --base
//     {{if .BaseSHA}}{{.BaseSHA}}{{else}}{{.BaseRef}}{{end}}" (the merge base,
//     else the base ref), ok_status [0], capture stdout; aliases codex,
//     codex_review.
//   - claude-simplify: claude, runs first, prompt claude-simplify.md,
//     capture git-diff, output claude-simplify.patch, after claude-review and
//     codex-review; aliases simplify.
//
// Each non-judge role carries a Summary, which makes it a candidate for
// triage ([triage]).
func DefaultRoles() []Role {
	return []Role{
		{Name: RoleCodexJudge, Kind: KindCodex, Judge: true, Effort: "xhigh", RereviewEffort: "high", Skill: DefaultSkill,
			Prompt: "judge-initial.md", Rereview: "judge-rereview.md", ContinuePrompt: "judge-continue.md",
			Recovery: "judge-recovery.md", Nudge: "judge-nudge.md", Stop: "judge-stop.md",
			Aliases: []string{"judge"}},
		{Name: RoleClaudeReview, Kind: KindClaude, Effort: "high", Summary: "deep review for bugs, security and correctness",
			Prompt: "claude-review.md", Rereview: "claude-rereview.md", Restart: "claude-restart.md", Aliases: []string{"claude"}},
		{Name: RoleCodexReview, Kind: KindShell, Tool: KindCodex, Command: defaultCodexReviewCommand, Summary: "Codex's own static review of the diff",
			OKStatus: []int{0}, Capture: CaptureStdout, Aliases: []string{"codex", "codex_review"}},
		{Name: RoleClaudeSimplify, Kind: KindClaude, Runs: RunsFirst, RerunMinLines: DefaultSimplifyRerunLines, Prompt: "claude-simplify.md",
			Summary: "simplifications and refactors of the changed code", Capture: CaptureGitDiff, Output: "claude-simplify.patch",
			After: []string{RoleClaudeReview, RoleCodexReview}, Aliases: []string{"simplify"}},
	}
}

// defaultCodexReviewCommand is the codex-review role's command: `codex
// review` against the PR's merge base, or its base ref when the merge base
// is unknown. The merge base does not move: with --base origin/master a
// review of a PR whose base branch gained commits flagged files the PR does
// not touch.
const defaultCodexReviewCommand = "command codex review --base {{if .BaseSHA}}{{.BaseSHA}}{{else}}{{.BaseRef}}{{end}}"

func defaultOutput(r Role) string {
	switch {
	case r.Judge:
		return r.Name + ".json"
	case r.Capture == CaptureGitDiff:
		return r.Name + ".patch"
	}
	return r.Name + ".md"
}

func (c *Config) roles() []Role {
	if c.Roles != nil {
		return c.Roles
	}
	return c.normalizedRoles(DefaultRoles())
}

// RolesFor returns the roles a watch runs, in [[role]] order: every role
// when w is nil or w.Roles is empty, else the ones w.Roles names (names or
// aliases).
func (c *Config) RolesFor(w *Watch) []Role {
	all := c.roles()
	if w == nil || len(w.Roles) == 0 {
		return slices.Clone(all)
	}
	var out []Role
	for _, r := range all {
		for _, s := range w.Roles {
			if r.Matches(s) {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// JudgeFor returns the watch's judge (validation guarantees exactly one);
// the zero Role when there is none.
func (c *Config) JudgeFor(w *Watch) Role {
	for _, r := range c.RolesFor(w) {
		if r.Judge {
			return r
		}
	}
	return Role{}
}

// RoleByNameOrAlias finds one of the watch's roles by name or alias (case
// and surrounding space ignored, so stored ids such as codex_review match
// too); "" is the watch's judge.
func (c *Config) RoleByNameOrAlias(w *Watch, s string) (Role, bool) {
	if strings.TrimSpace(s) == "" {
		j := c.JudgeFor(w)
		return j, j.Name != ""
	}
	for _, r := range c.RolesFor(w) {
		if r.Matches(s) {
			return r, true
		}
	}
	return Role{}, false
}

// Stages orders the watch's roles for a round: non-judge roles layered by
// After (a stage holds the roles whose dependencies all ran in earlier
// stages; dependencies outside the watch's set are ignored), then the judge
// alone. Validation guarantees After is acyclic.
func (c *Config) Stages(w *Watch) [][]Role {
	roles := c.RolesFor(w)
	in := map[string]bool{}
	for _, r := range roles {
		in[r.Name] = true
	}
	level := map[string]int{}
	var depth func(r Role, seen map[string]bool) int
	byName := map[string]Role{}
	for _, r := range roles {
		byName[r.Name] = r
	}
	depth = func(r Role, seen map[string]bool) int {
		if d, ok := level[r.Name]; ok {
			return d
		}
		if seen[r.Name] {
			return 0 // a cycle; Validate reports it
		}
		seen[r.Name] = true
		d := 0
		for _, a := range r.After {
			if dep, ok := byName[a]; ok && in[a] && !dep.Judge {
				d = max(d, depth(dep, seen)+1)
			}
		}
		level[r.Name] = d
		return d
	}
	var stages [][]Role
	var judges []Role
	for _, r := range roles {
		if r.Judge {
			judges = append(judges, r)
			continue
		}
		d := depth(r, map[string]bool{})
		for len(stages) <= d {
			stages = append(stages, nil)
		}
		stages[d] = append(stages[d], r)
	}
	if len(judges) > 0 {
		stages = append(stages, judges)
	}
	return stages
}

// RoleEnv is the pane environment of a role: its kind's env (the Tool's for
// shell roles) overlaid by the role's env.
func (c *Config) RoleEnv(r Role) map[string]string {
	out := map[string]string{}
	if k, ok := c.KindSpec(r.AgentKind()); ok {
		for key, v := range k.Env {
			out[key] = v
		}
	}
	for key, v := range r.Env {
		out[key] = v
	}
	return out
}

func (c *Config) normalizedRoles(in []Role) []Role {
	roles := slices.Clone(in)
	names := map[string]string{} // name or alias -> name
	for _, r := range roles {
		for _, a := range r.Aliases {
			names[strings.ToLower(a)] = r.Name
		}
	}
	for _, r := range roles {
		names[r.Name] = r.Name
	}
	for i := range roles {
		r := &roles[i]
		r.Aliases, r.After, r.Args = cloneOrNil(r.Aliases), cloneOrNil(r.After), cloneOrNil(r.Args)
		r.OKStatus = cloneOrNil(r.OKStatus)
		if len(r.Env) == 0 {
			r.Env = nil
		}
		for j, a := range r.Aliases {
			r.Aliases[j] = strings.ToLower(strings.TrimSpace(a))
		}
		if r.Mode == "" {
			r.Mode = ModeSession
			if r.Kind == KindShell {
				r.Mode = ModeShell
			}
		}
		if r.Runs == "" {
			r.Runs = RunsAlways
		}
		if r.Capture == "" {
			r.Capture = CaptureFile
			if r.IsShell() {
				r.Capture = CaptureStdout
			}
		}
		if r.Output == "" {
			r.Output = defaultOutput(*r)
		}
		if r.Timeout.Duration == 0 {
			r.Timeout = c.Daemon.ReviewerTimeout
			if r.Judge {
				r.Timeout = c.Daemon.JudgeTimeout
			}
			if r.Timeout.Duration == 0 {
				r.Timeout = Duration{40 * time.Minute}
				if r.Judge {
					r.Timeout = Duration{90 * time.Minute}
				}
			}
		}
		switch {
		case r.Judge:
			fill := func(p *string, def string) {
				if *p == "" {
					*p = def
				}
			}
			fill(&r.Prompt, "judge-initial.md")
			fill(&r.Rereview, "judge-rereview.md")
			fill(&r.ContinuePrompt, "judge-continue.md")
			fill(&r.Recovery, "judge-recovery.md")
			fill(&r.Nudge, "judge-nudge.md")
			fill(&r.Stop, "judge-stop.md")
			if r.Skill == "" {
				r.Skill = DefaultSkill
			}
		case !r.IsShell():
			if r.Prompt == "" {
				r.Prompt = r.Name + ".md"
			}
			if r.Rereview == "" {
				r.Rereview = r.Prompt
				if c.promptExists(r.Name + "-rereview.md") {
					r.Rereview = r.Name + "-rereview.md"
				}
			}
			if r.Restart == "" && c.promptExists(r.Name+"-restart.md") {
				r.Restart = r.Name + "-restart.md"
			}
		}
		for j, a := range r.After {
			if n, ok := names[strings.ToLower(strings.TrimSpace(a))]; ok {
				r.After[j] = n
			}
		}
	}
	return roles
}

// DefaultSimplifyRerunLines is claude-simplify's rerun_min_lines: about
// two new functions' worth of code since it last looked.
const DefaultSimplifyRerunLines = 150
