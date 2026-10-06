package config

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Kind is a [kinds.<name>] block: how magnum drives one agent CLI that herdr
// knows by that name (`herdr agent start --kind <name>`). User blocks are
// merged key by key onto DefaultKinds (a key that is not set keeps the
// default); a name without a default starts from DefaultHealthPatterns,
// wrapper "auto" and session_source "herdr". KindSpec returns the result.
//
// herdr types the command into the pane's shell, so the CLI may be a zsh
// wrapper function or alias that adds its own flags. The launch argv is
// built by Argv in this order: Resume (resumed sessions only), Name (when a
// title is known), Model (the role's model, else DefaultModel), Effort
// (when the role sets it), Subagents or NoSubagents (when the role sets
// max_subagents), MCPDisable per MCP server to turn off and ProjectUntrust
// for a checkout whose .codex/ the PR changes (ConfigOffArgs), Start, Args
// (only without a wrapper), then the role's args.
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
	// Subagents: args that cap the subagents a session may have open at
	// once to {subagents} (Role.MaxSubagents, 1 or more), e.g. codex's
	// ["-c", "agents.max_concurrent_threads_per_session={subagents}"];
	// NoSubagents: args that turn subagents off (max_subagents = 0). Empty =
	// the kind cannot cap them, and the role key is ignored.
	Subagents   []string `toml:"subagents"`
	NoSubagents []string `toml:"no_subagents"`
	// MCPOff: turn off each MCP server a session would load from the Codex
	// config ([mcp_servers.<name>] tables not set enabled = false), except
	// those in MCPAllow, through MCPDisable: Codex merges -c tables into
	// its config, so servers go off only one by one, by name (see
	// MCPOffArgs; codex: true). A kind without mcp_disable args ignores it.
	MCPOff   bool     `toml:"mcp_off"`
	MCPAllow []string `toml:"mcp_allow"`
	// MCPDisable: args that turn off MCP server {server}, passed once per
	// server, e.g. codex's ["-c", "mcp_servers.{server}.enabled=false"].
	MCPDisable []string `toml:"mcp_disable"`
	// ProjectMCP: the MCP servers the checkout's own .codex/config.toml
	// declares when the PR leaves .codex/ as its merge base has it (the
	// team's servers): "allow" (default) keeps them, "off" turns each off
	// through MCPDisable like the user's, except those in MCPAllow (see
	// ConfigOffArgs). A PR that changes .codex/ loads none of them
	// (ProjectUntrust).
	ProjectMCP string `toml:"project_mcp"`
	// ProjectUntrust: args that make one session treat the checkout as an
	// untrusted folder, so it loads nothing from the checkout's .codex/
	// (config, MCP servers, hooks, rules), passed when the PR changes
	// .codex/ against its merge base; {projects} is a TOML inline table
	// marking each of the checkout's paths untrusted (UntrustArgs), e.g.
	// codex's ["-c", "projects={projects}"]. Empty = such a session loads
	// the PR's .codex/ like any trusted project's.
	ProjectUntrust []string `toml:"project_untrust"`
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

// RoleModel is the model a role's sessions are configured to run: the
// role's own model, else its kind's default_model ("" = the CLI's default;
// a shell role: its own model only).
func (c *Config) RoleModel(r Role) string {
	if r.Model != "" || r.IsShell() {
		return r.Model
	}
	k, _ := c.KindSpec(r.Kind)
	return k.DefaultModel
}

// SwitchModelCommand is SwitchModel with {model} replaced ("" when the kind
// cannot switch or model is empty).
func (k Kind) SwitchModelCommand(model string) string {
	if k.SwitchModel == "" || model == "" {
		return ""
	}
	return strings.ReplaceAll(k.SwitchModel, PlaceholderModel, model)
}

// DefaultAfterDenyPrompt is every built-in and declared kind's
// after_deny_prompt.
const DefaultAfterDenyPrompt = "magnum denied that command: review roles never run approval-gated or destructive commands. " +
	"Continue the task without it and finish as instructed."

// Kind.OnPermissionPrompt values.
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

// Kind.OnHooksReview values.
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

// Kind.ProjectMCP values.
const (
	// ProjectMCPAllow: the MCP servers of an unchanged .codex/config.toml
	// in the checkout (the base branch's, so the team's) load.
	ProjectMCPAllow = "allow"
	// ProjectMCPOff: they are turned off like the user's (MCPDisable),
	// except those in MCPAllow.
	ProjectMCPOff = "off"
)

// HealthPatterns are regular expressions (RE2, matched case-insensitively)
// that classify an agent pane's recent output. Setting one list replaces
// that list only.
//
// ModelLimit patterns name a cap on one model while the account still has
// usage left ("You've reached your Fable limit"). They are checked before
// UsageLimit; a named group "model" captures the model, else the session's
// current model is the limited one.
type HealthPatterns struct {
	LoginRequired []string `toml:"login_required"` // pause the kind until the human logs in
	ModelLimit    []string `toml:"model_limit"`    // switch the session to a fallback model (see Kind.FallbackModels)
	UsageLimit    []string `toml:"usage_limit"`    // pause until the reset time the text names
	Overloaded    []string `toml:"overloaded"`     // retry with backoff
}

// HealthRegexps are compiled HealthPatterns.
type HealthRegexps struct {
	LoginRequired, ModelLimit, UsageLimit, Overloaded []*regexp.Regexp
}

// Compile compiles every pattern with the (?i) flag.
func (h HealthPatterns) Compile() (HealthRegexps, error) {
	var out HealthRegexps
	var errs []error
	compile := func(list string, exprs []string) []*regexp.Regexp {
		res := make([]*regexp.Regexp, 0, len(exprs))
		for _, e := range exprs {
			re, err := regexp.Compile(`(?i)` + e)
			if err != nil {
				errs = append(errs, fmt.Errorf("health_patterns.%s %q: %w", list, e, err))
				continue
			}
			res = append(res, re)
		}
		return res
	}
	out.LoginRequired = compile("login_required", h.LoginRequired)
	out.ModelLimit = compile("model_limit", h.ModelLimit)
	out.UsageLimit = compile("usage_limit", h.UsageLimit)
	out.Overloaded = compile("overloaded", h.Overloaded)
	return out, errors.Join(errs...)
}

// DefaultHealthPatterns returns the classifier patterns magnum has always
// used for Codex and Claude (a fresh copy); every kind starts from them.
func DefaultHealthPatterns() HealthPatterns {
	return HealthPatterns{
		LoginRequired: []string{
			`\bnot logged in\b`,
			`please run /login`,
			`\bcodex login\b`,
			`\bclaude (?:auth )?login\b`,
			`invalid api key`,
			`oauth token (?:has )?expired`,
			`authentication_error`,
			`api error:? 401\b`,
			`\b401 unauthori[sz]ed`,
			`\bstatus:? 401\b`,
			`access token could not be refreshed`,
			`refresh token (?:was already used|has expired|is invalid|expired)`,
			`\bsign in again\b`,
			`sign in with chatgpt`,
			`select login method`,
			`\btoken_expired\b`,
		},
		// A model's own cap: Claude Code's "You've reached your Fable
		// limit. /model to switch models." and "You've hit your Opus limit ·
		// resets 3:45pm" (the account windows still have room).
		ModelLimit: []string{
			`you(?:'|’|‘)?ve (?:reached|hit) your (?P<model>fable|mythos|opus|sonnet|haiku)\b[\w .-]* limit`,
			`(?P<model>fable|mythos|opus|sonnet|haiku)\b[\w .-]* limit reached`,
			`/model to switch models`,
		},
		// Account-wide limits only: the optional word is never a model name
		// (those are model_limit above).
		UsageLimit: []string{
			`you(?:'|’|‘)?ve hit your (?:(?:usage|session|weekly|daily|monthly|5-hour|account|plan) )?limit`,
			`you(?:'|’|‘)?ve reached your (?:(?:usage|session|weekly|daily|monthly|5-hour|account|plan) )?limit`,
			`\busage limit reached`,
			`\b(?:5-hour|weekly|session|daily) limit reached`,
			`you(?:'|’|‘)?re out of (?:extra )?usage`,
			`\bquota exceeded\b`,
			`\binsufficient_quota\b`,
		},
		Overloaded: []string{
			`overloaded_error`,
			`api error:? (?:5\d\d|429)\b`,
			`api error \((?:request timed out|connection error)`,
			`\bstatus:? (?:5\d\d|429)\b`,
			`\b429 too many requests\b`,
			`\brate limit exceeded\b`,
			`\brate_limit_error\b`,
			`stream disconnected before completion`,
			`\breconnecting\b`,
			`retrying in \d+(?:\.\d+)?\s*(?:s|sec|secs|seconds?)\b`,
			`\bhigh demand\b`,
			`\b(?:service unavailable|internal server error|bad gateway|gateway timeout)\b`,
			`\b(?:server|servers|api|model|service) (?:is |are )?(?:currently )?overloaded\b`,
			`(?m)^[^\w\n]*overloaded\b`,
			`\brequest timed out\b`,
		},
	}
}

// DefaultKinds returns the built-in kinds (fresh copies):
//
//   - codex (0.160): args ["--dangerously-bypass-approvals-and-sandbox"],
//     resume ["resume","{session}"], model ["--model","{model}"],
//     effort ["-c","model_reasoning_effort={effort}"], rename "/rename {title}",
//     login_check "codex login status" + login_ok "text:Logged in",
//     mcp_off true with mcp_disable ["-c","mcp_servers.{server}.enabled=false"],
//     project_untrust ["-c","projects={projects}"].
//   - claude: args ["--dangerously-skip-permissions"],
//     resume ["--resume","{session}"], model ["--model","{model}"],
//     name ["--name","{title}"], login_check "claude auth status" + login_ok
//     "json:loggedIn", switch_model "/model {model}", fallback_models
//     ["opus","sonnet"], reset_model "default"; no effort flag
//     (claude-review passes its effort to /code-review in the prompt).
//   - droid (0.232): resume ["--resume","{session}"]; no model, effort or
//     name flag in interactive mode, no login check.
//   - omp (18.4): resume ["--resume={session}"], model ["--model={model}"],
//     effort ["--thinking={effort}"]; no login check.
//
// All use wrapper "auto", session_source "herdr", on_permission_prompt
// "deny", on_hooks_review "trust_own", project_mcp "allow",
// DefaultAfterDenyPrompt and DefaultHealthPatterns. The codex and
// claude args make a plain binary run without approval prompts and (codex)
// without its sandbox, as the user's zsh wrappers do: review agents run
// tests and `gh`, and magnum answers every approval prompt No. Args apply
// only without a wrapper, so a wrapper's own flags are never doubled.
func DefaultKinds() map[string]Kind {
	base := func(k Kind) Kind {
		k.Wrapper, k.SessionSource, k.HealthPatterns = WrapperAuto, SessionHerdr, DefaultHealthPatterns()
		k.OnPermissionPrompt, k.AfterDenyPrompt, k.OnHooksReview = PermissionDeny, DefaultAfterDenyPrompt, HooksTrustOwn
		k.ProjectMCP = ProjectMCPAllow
		return k
	}
	return map[string]Kind{
		KindCodex: base(Kind{
			Args:       []string{"--dangerously-bypass-approvals-and-sandbox"},
			Resume:     []string{"resume", PlaceholderSession},
			Model:      []string{"--model", PlaceholderModel},
			Effort:     []string{"-c", "model_reasoning_effort=" + PlaceholderEffort},
			Rename:     "/rename " + PlaceholderTitle,
			LoginCheck: "codex login status", LoginOK: "text:Logged in",
			// The spawned-agent threads open at once, the primary excluded
			// (at least 1); agents.enabled = false removes the tools.
			Subagents: []string{"-c", "agents.max_concurrent_threads_per_session=" + PlaceholderSubagents}, NoSubagents: []string{"-c", "agents.enabled=false"},
			MCPOff: true, MCPDisable: []string{"-c", "mcp_servers." + PlaceholderServer + ".enabled=false"},
			// Codex 0.160 merges a -c table into the user's config
			// (config/src/overrides.rs, merge.rs) and decides a folder's
			// trust from those session flags before the project layers load
			// (config/src/loader/mod.rs), so the table overrides the trust
			// magnum recorded for one session only.
			ProjectUntrust: []string{"-c", "projects=" + PlaceholderProjects},
		}),
		KindClaude: base(Kind{
			Args:       []string{"--dangerously-skip-permissions"},
			Resume:     []string{"--resume", PlaceholderSession},
			Model:      []string{"--model", PlaceholderModel},
			Effort:     []string{"--effort", PlaceholderEffort},
			Name:       []string{"--name", PlaceholderTitle},
			LoginCheck: "claude auth status", LoginOK: "json:loggedIn",
			SwitchModel: "/model " + PlaceholderModel, FallbackModels: []string{"opus", "sonnet"}, ResetModel: "default",
		}),
		KindDroid: base(Kind{Resume: []string{"--resume", PlaceholderSession}}),
		KindOMP: base(Kind{
			Resume: []string{"--resume=" + PlaceholderSession},
			Model:  []string{"--model=" + PlaceholderModel},
			Effort: []string{"--thinking=" + PlaceholderEffort},
		}),
	}
}

// LaunchArgs are the per-start values Kind.Argv substitutes.
type LaunchArgs struct {
	Session string   // session id to resume; "" = a fresh session (Resume is skipped)
	Title   string   // the pane title; "" skips Name
	Model   string   // Role.Model; "" = the kind's DefaultModel, and Model is skipped when that is "" too
	Effort  string   // Role.Effort; "" skips Effort
	Wrapper bool     // the command is a wrapper function (Args are skipped)
	Extra   []string // Role.Args, appended last as given

	// Subagents is Role.MaxSubagents: nil skips Subagents and NoSubagents,
	// 0 takes NoSubagents, more takes Subagents.
	Subagents *int
	// MCPServers are the MCP servers the session would load (the Codex
	// config's); MCPOffArgs turns them off.
	MCPServers []string
	// ProjectServers are the MCP servers the checkout's unchanged
	// .codex/config.toml declares, turned off under project_mcp "off";
	// Untrusted are the checkout's paths whose project config the session
	// must not load (the PR changes .codex/). See ConfigOffArgs.
	ProjectServers, Untrusted []string
}

// Argv builds the args after the command name (herdr shell-quotes each one):
// Resume, Name, Model (a.Model, else DefaultModel), Effort, Subagents (or
// NoSubagents for 0), ConfigOffArgs(a.MCPServers, a.ProjectServers,
// a.Untrusted), Start, Args (no wrapper only), then a.Extra. A group whose value is empty is skipped;
// placeholders are replaced in every element.
func (k Kind) Argv(a LaunchArgs) []string {
	var out []string
	add := func(group []string, placeholder, value string) {
		if value == "" {
			return
		}
		for _, s := range group {
			out = append(out, strings.ReplaceAll(s, placeholder, value))
		}
	}
	add(k.Resume, PlaceholderSession, a.Session)
	add(k.Name, PlaceholderTitle, a.Title)
	add(k.Model, PlaceholderModel, cmp.Or(a.Model, k.DefaultModel))
	add(k.Effort, PlaceholderEffort, a.Effort)
	switch {
	case a.Subagents == nil:
	case *a.Subagents == 0:
		out = append(out, k.NoSubagents...)
	default:
		add(k.Subagents, PlaceholderSubagents, strconv.Itoa(*a.Subagents))
	}
	out = append(out, k.ConfigOffArgs(a.MCPServers, a.ProjectServers, a.Untrusted)...)
	out = append(out, k.Start...)
	if !a.Wrapper {
		out = append(out, k.Args...)
	}
	return append(out, a.Extra...)
}

// MCPOffArgs are the args that turn off the given MCP servers: MCPDisable
// with {server} replaced, once per server not in MCPAllow, in order; nil
// when MCPOff is false or the kind has no MCPDisable args.
func (k Kind) MCPOffArgs(servers []string) []string {
	if !k.MCPOff || len(k.MCPDisable) == 0 {
		return nil
	}
	var out []string
	for _, s := range servers {
		if slices.Contains(k.MCPAllow, s) {
			continue
		}
		for _, a := range k.MCPDisable {
			out = append(out, strings.ReplaceAll(a, PlaceholderServer, s))
		}
	}
	return out
}

// ConfigOffArgs are the args that keep configuration out of a session:
// MCPDisable once per server to turn off (servers under MCPOff, then
// under ProjectMCP "off" each project server not among them; none in
// MCPAllow, nothing without MCPDisable), then UntrustArgs(untrusted).
func (k Kind) ConfigOffArgs(servers, project, untrusted []string) []string {
	out := k.MCPOffArgs(servers)
	if k.ProjectMCP == ProjectMCPOff && len(k.MCPDisable) > 0 {
		for _, s := range project {
			if slices.Contains(k.MCPAllow, s) || k.MCPOff && slices.Contains(servers, s) {
				continue
			}
			for _, a := range k.MCPDisable {
				out = append(out, strings.ReplaceAll(a, PlaceholderServer, s))
			}
		}
	}
	return append(out, k.UntrustArgs(untrusted)...)
}

// UntrustArgs are ProjectUntrust with {projects} replaced by one TOML
// inline table marking each path untrusted, e.g.
// {"/p/x"={trust_level="untrusted"}} (a second -c of the same key would
// replace the first); nil without paths or ProjectUntrust.
func (k Kind) UntrustArgs(paths []string) []string {
	if len(paths) == 0 || len(k.ProjectUntrust) == 0 {
		return nil
	}
	entries := make([]string, len(paths))
	for i, p := range paths {
		entries[i] = TOMLString(p) + `={trust_level="untrusted"}`
	}
	table := "{" + strings.Join(entries, ",") + "}"
	out := make([]string, len(k.ProjectUntrust))
	for i, a := range k.ProjectUntrust {
		out[i] = strings.ReplaceAll(a, PlaceholderProjects, table)
	}
	return out
}

// TOMLString renders s as a TOML basic string.
func TOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// RenameCommand is Rename with {title} replaced ("" when the kind has none).
func (k Kind) RenameCommand(title string) string {
	if k.Rename == "" {
		return ""
	}
	return strings.ReplaceAll(k.Rename, PlaceholderTitle, title)
}

// LoginArgv splits LoginCheck on spaces: the command name and its args
// (nil when the kind has no login check).
func (k Kind) LoginArgv() []string { return strings.Fields(k.LoginCheck) }

// LoggedIn reads LoginCheck's output per LoginOK. exitOK is whether the
// command exited 0. readable is false when the output could not be read
// (json: no JSON object in stdout, or the path is not a boolean); the caller
// then decides between "not logged in" and "the check could not run".
//
//	text:<s>   loggedIn = stdout+stderr contains s (case-sensitive)
//	regex:<re> loggedIn = stdout+stderr matches re
//	json:<p>   loggedIn = the boolean at dotted path p of stdout's first JSON object
//	""         loggedIn = exitOK
func (k Kind) LoggedIn(stdout, stderr string, exitOK bool) (loggedIn, readable bool) {
	mode, value, err := parseLoginOK(k.LoginOK)
	if err != nil {
		return false, false
	}
	both := stdout + "\n" + stderr
	switch mode {
	case "text":
		return strings.Contains(both, value), true
	case "regex":
		re, err := regexp.Compile(value)
		if err != nil {
			return false, false
		}
		return re.MatchString(both), true
	case "json":
		// Decode only the first object: whatever follows it (another object,
		// a stray brace in a trailing log line) is not part of the answer.
		i := strings.IndexByte(stdout, '{')
		if i < 0 {
			return false, false
		}
		var v any
		if json.NewDecoder(strings.NewReader(stdout[i:])).Decode(&v) != nil {
			return false, false
		}
		for _, key := range strings.Split(value, ".") {
			m, ok := v.(map[string]any)
			if !ok {
				return false, false
			}
			v = m[key]
		}
		b, ok := v.(bool)
		return b, ok
	}
	return exitOK, true
}

func parseLoginOK(s string) (mode, value string, err error) {
	if s == "" {
		return "", "", nil
	}
	mode, value, ok := strings.Cut(s, ":")
	if !ok || value == "" {
		return "", "", fmt.Errorf("login_ok %q: want text:<substring>, regex:<expr> or json:<path>", s)
	}
	switch mode {
	case "text", "json":
	case "regex":
		if _, err := regexp.Compile(value); err != nil {
			return "", "", fmt.Errorf("login_ok %q: %w", s, err)
		}
	default:
		return "", "", fmt.Errorf("login_ok %q: want text:<substring>, regex:<expr> or json:<path>", s)
	}
	return mode, value, nil
}

// KindSpec returns the merged [kinds.<name>] spec (user keys over
// DefaultKinds); false for "shell" and undeclared names.
func (c *Config) KindSpec(name string) (Kind, bool) {
	k, ok := c.kinds()[name]
	return k, ok
}

// KindNames lists the declared kinds, sorted.
func (c *Config) KindNames() []string {
	out := make([]string, 0, len(c.kinds()))
	for n := range c.kinds() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (c *Config) kinds() map[string]Kind {
	if c.Kinds != nil {
		return c.Kinds
	}
	k := DefaultKinds()
	normalizeKinds(k)
	return k
}

func normalizeKinds(kinds map[string]Kind) {
	for name, k := range kinds {
		k.Start, k.Args, k.Resume, k.Model = cloneOrNil(k.Start), cloneOrNil(k.Args), cloneOrNil(k.Resume), cloneOrNil(k.Model)
		k.Effort, k.Name, k.FallbackModels = cloneOrNil(k.Effort), cloneOrNil(k.Name), cloneOrNil(k.FallbackModels)
		k.Subagents, k.NoSubagents = cloneOrNil(k.Subagents), cloneOrNil(k.NoSubagents)
		k.MCPAllow, k.MCPDisable, k.ProjectUntrust = cloneOrNil(k.MCPAllow), cloneOrNil(k.MCPDisable), cloneOrNil(k.ProjectUntrust)
		k.SwitchModel, k.DefaultModel = strings.TrimSpace(k.SwitchModel), strings.TrimSpace(k.DefaultModel)
		k.ResetModel = strings.TrimSpace(k.ResetModel)
		h := &k.HealthPatterns
		h.LoginRequired, h.ModelLimit = cloneOrNil(h.LoginRequired), cloneOrNil(h.ModelLimit)
		h.UsageLimit, h.Overloaded = cloneOrNil(h.UsageLimit), cloneOrNil(h.Overloaded)
		if len(k.Env) == 0 {
			k.Env = nil
		}
		k.Wrapper = strings.ToLower(strings.TrimSpace(k.Wrapper))
		if k.Wrapper == "" {
			k.Wrapper = WrapperAuto
		}
		if k.SessionSource == "" {
			k.SessionSource = SessionHerdr
		}
		k.OnPermissionPrompt = strings.ToLower(strings.TrimSpace(k.OnPermissionPrompt))
		if k.OnPermissionPrompt == "" {
			k.OnPermissionPrompt = PermissionDeny
		}
		k.OnHooksReview = strings.ToLower(strings.TrimSpace(k.OnHooksReview))
		if k.OnHooksReview == "" {
			k.OnHooksReview = HooksTrustOwn
		}
		k.ProjectMCP = strings.ToLower(strings.TrimSpace(k.ProjectMCP))
		if k.ProjectMCP == "" {
			k.ProjectMCP = ProjectMCPAllow
		}
		k.AfterDenyPrompt = strings.TrimSpace(k.AfterDenyPrompt)
		kinds[name] = k
	}
}
