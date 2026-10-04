package config

import (
	"encoding"
	"fmt"
	"reflect"
	"slices"
	"strings"

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

	CaptureFile    = "file"     // the agent (or command) writes ReportFile itself
	CaptureStdout  = "stdout"   // shell roles: magnum tees the command's stdout into ReportFile (stderr stays in the pane)
	CaptureGitDiff = "git-diff" // magnum saves `git diff` of the checkout into ReportFile, then reverts the tree

	WrapperAuto  = "auto"  // probe `zsh -ic 'whence -w <kind>'` once: a function or alias is a wrapper
	WrapperTrue  = "true"  // the command is a wrapper that supplies its own flags
	WrapperFalse = "false" // a plain binary: Kind.Args are appended too

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
	PromptStop     = "stop"     // Role.Stop: the PR closed or the round was cancelled (judge)
)

// PromptKinds lists every prompt kind a role may name (Role.PromptFile).
var PromptKinds = []string{PromptInitial, PromptRereview, PromptRestart, PromptContinue, PromptRecovery, PromptNudge, PromptStop}

// Built-in role names (DefaultRoles) and kinds (DefaultKinds).
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

// Placeholders substituted by Kind.Argv and in Kind.Rename.
const (
	PlaceholderSession = "{session}"
	PlaceholderTitle   = "{title}"
	PlaceholderModel   = "{model}"
	PlaceholderEffort  = "{effort}"
)

// DefaultSkill is the judge's default skill path ({{repo}} = magnum's home).
const DefaultSkill = "{{repo}}/skills/magnum-review/SKILL.md"

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

// expandPipeline resolves {{repo}} and ~ in the judges' skills (Load only;
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
	return &layer{md: md, kinds: kinds, roles: roles, raw: raw.Role}, nil
}

// buildPipeline computes Kinds and Roles from the built-in defaults, the
// legacy [codex]/[claude]/[daemon] keys (fallbacks), the config file's
// blocks and the overlay's blocks, in that order.
func (c *Config) buildPipeline(base, over *layer) {
	kinds := DefaultKinds()
	applyLegacyKinds(kinds, c.Codex, c.Claude)
	builtins := DefaultRoles()
	c.legacyErrs = nil
	if err := applyLegacyRoles(builtins, c.Codex, c.Claude); err != nil {
		c.legacyErrs = append(c.legacyErrs, err)
	}
	roles := builtins
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
			roles = append(roles, mergeRole(builtinRole(builtins, r.Name), r, base.raw, i))
		}
	}
	if over != nil {
		for i, r := range over.roles {
			if j := slices.IndexFunc(roles, func(x Role) bool { return x.Name == r.Name }); j >= 0 {
				roles[j] = mergeRole(roles[j], r, over.raw, i)
				continue
			}
			roles = append(roles, mergeRole(builtinRole(builtins, r.Name), r, over.raw, i))
		}
	}
	c.Kinds, c.Roles = kinds, roles
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
		tag := strings.Split(f.Tag.Get("toml"), ",")[0]
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

// applyLegacyKinds maps [codex]/[claude] wrapper_mode and args onto the codex
// and claude kinds.
func applyLegacyKinds(kinds map[string]Kind, codex Codex, claude Claude) {
	set := func(name, wrapper string, args []string) {
		k := kinds[name]
		if wrapper != "" {
			k.Wrapper = wrapper
		}
		if len(args) > 0 {
			k.Args = slices.Clone(args)
		}
		kinds[name] = k
	}
	set(KindCodex, codex.WrapperMode, codex.Args)
	set(KindClaude, claude.WrapperMode, claude.Args)
}

// applyLegacyRoles maps [codex] skill_path and review_args and [claude]
// effort and simplify onto the built-in roles. It returns an error for a
// [claude] simplify value other than "", first, always or never (which then
// maps to nothing).
func applyLegacyRoles(roles []Role, codex Codex, claude Claude) error {
	var err error
	switch claude.Simplify {
	case "", "first", "always", "never":
	default:
		err = fmt.Errorf("claude.simplify must be first, always or never, got %q", claude.Simplify)
	}
	for i := range roles {
		r := &roles[i]
		switch r.Name {
		case RoleCodexJudge:
			if codex.SkillPath != "" {
				r.Skill = codex.SkillPath
			}
		case RoleCodexReview:
			if len(codex.ReviewArgs) > 0 {
				r.Args = slices.Clone(codex.ReviewArgs)
			}
		case RoleClaudeReview:
			if claude.Effort != "" {
				r.Effort = claude.Effort
			}
		case RoleClaudeSimplify:
			switch claude.Simplify {
			case "always":
				r.Runs = RunsAlways
			case "never":
				r.Runs = RunsManual // [claude] simplify = never still honoured `magnum review --simplify`
			case "first":
				r.Runs = RunsFirst
			}
		}
	}
	return err
}
