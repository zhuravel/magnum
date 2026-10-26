package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Learn is the [learn] section: the daily retro. After a PR closes, magnum
// collects what the other reviewers commented on it, drops what its own review
// already posted, and has an interactive agent classify the rest; the real
// misses go to the registry's misses table (`magnum misses` lists them).
// Enabled runs it once a day after DailyAt; `magnum retro` runs one whether
// or not it is enabled (internal/engine/retro.go, DECISIONS "Learning loop:
// daily retro").
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
	// FallbackModels are the models the classifier starts again on, in
	// order, when Model hits its own limit (Role.FallbackModels); empty =
	// the kind's fallback_models.
	FallbackModels []string `toml:"fallback_models"`

	// The self-improving loop (internal/engine/learn_edit.go, DECISIONS "The
	// editor proposes skill and prompt changes on a branch"). Editor turns
	// on the editor after a retro that found new general misses or
	// declines (`magnum learn --propose` runs it regardless): a pane agent
	// that drafts a change to the judge skill and the prompts on a branch
	// of the checkout. EditorKind, EditorModel, EditorEffort and EditorArgs
	// are its role's (EditorRole; editor_kind without editor_model gets
	// DefaultLearnModel of that kind), EditorPrompt its template,
	// EditorTimeout the bound of one turn and EditorFallbackModels the
	// models it starts again on when EditorModel hits its own limit (empty =
	// the kind's fallback_models).
	Editor               bool     `toml:"editor"`
	EditorKind           string   `toml:"editor_kind"`
	EditorModel          string   `toml:"editor_model"`
	EditorEffort         string   `toml:"editor_effort"`
	EditorArgs           []string `toml:"editor_args"`
	EditorPrompt         string   `toml:"editor_prompt"`
	EditorTimeout        Duration `toml:"editor_timeout"`
	EditorFallbackModels []string `toml:"editor_fallback_models"`
	// GateCases bounds the eval cases the gate of a proposal replays, its
	// targets and the newest others; GateGuards (0 to 10, 0 = none) adds
	// that many guards on top, cases whose newest replay found a defect,
	// which the candidate must find again (learn.SelectCases);
	// LandPerDay the proposals that land per local day; Restart is idle
	// (the daemon restarts on a landed change once nothing is in flight) or
	// manual (it asks for `magnum daemon-restart --when-idle`). The newest
	// landed version whose declined and argued share of its posted
	// findings grows by RegressionPoints over the previous version's, both
	// with at least RegressionMinFindings posted findings, gets one revert
	// proposal, which the gate holds for review (internal/engine
	// learn_revert.go).
	GateCases             int    `toml:"gate_cases"`
	GateGuards            int    `toml:"gate_guards"`
	LandPerDay            int    `toml:"land_per_day"`
	Restart               string `toml:"restart"`
	RegressionPoints      int    `toml:"regression_points"`
	RegressionMinFindings int    `toml:"regression_min_findings"`
}

// DefaultEditorPrompt is the skill editor's prompt file.
const DefaultEditorPrompt = "skill-edit.md"

// EditorRoleName is the name of the role the skill editor runs as
// (EditorRole).
const EditorRoleName = "skill"

// The values of [learn] restart.
const (
	LearnRestartIdle   = "idle"
	LearnRestartManual = "manual"
)

// DefaultLearnPrompt is the retro prompt file.
const DefaultLearnPrompt = "retro.md"

// LearnRoleName is the name of the role the retro's classifying agent runs as
// (LearnRole).
const LearnRoleName = "retro"

// DefaultLearnModel is the [learn] model of a kind the config names without
// choosing a model: sonnet for claude, the cheap model the retro was written
// for, and "" for every other kind, which then runs on its own default model
// (a Claude model name passed to another CLI fails every PR's retro).
func DefaultLearnModel(kind string) string {
	if kind == KindClaude {
		return "sonnet"
	}
	return ""
}

// learnModelFollowsKind makes the default model kind-aware: a layer (the base
// file, the user config) that sets learn.kind but not learn.model gets
// DefaultLearnModel of that kind instead of whatever an earlier layer left.
// A layer that sets model keeps it, whatever the kind.
func (c *Config) learnModelFollowsKind(md toml.MetaData) {
	if md.IsDefined("learn", "kind") && !md.IsDefined("learn", "model") {
		c.Learn.Model = DefaultLearnModel(c.Learn.Kind)
	}
	if md.IsDefined("learn", "editor_kind") && !md.IsDefined("learn", "editor_model") {
		c.Learn.EditorModel = DefaultLearnModel(c.Learn.EditorKind)
	}
}

// DefaultLearn returns the built-in [learn] values: off, a week of lookback,
// a day to settle, Claude sonnet classifying; the editor off, Claude sonnet
// editing for at most 45 minutes a turn, a gate of 6 cases, one landing a
// day, an idle restart, a regression of 10 points over 20 findings.
func DefaultLearn() Learn {
	return Learn{
		DailyAt:               "07:00",
		Lookback:              Duration{7 * 24 * time.Hour},
		Settle:                Duration{24 * time.Hour},
		MaxPRs:                20,
		MinCommentChars:       20,
		Kind:                  KindClaude,
		Model:                 DefaultLearnModel(KindClaude),
		Prompt:                DefaultLearnPrompt,
		Timeout:               Duration{20 * time.Minute},
		EditorKind:            KindClaude,
		EditorModel:           DefaultLearnModel(KindClaude),
		EditorPrompt:          DefaultEditorPrompt,
		EditorTimeout:         Duration{45 * time.Minute},
		GateCases:             6,
		GateGuards:            2,
		LandPerDay:            1,
		Restart:               LearnRestartIdle,
		RegressionPoints:      10,
		RegressionMinFindings: 20,
	}
}

// DailyTime is the daily_at time on the local calendar day of day (in day's
// location). daily_at must be "HH:MM", 00:00 to 23:59, two digits each.
func (l Learn) DailyTime(day time.Time) (time.Time, error) {
	h, m, ok := parseDailyAt(l.DailyAt)
	if !ok {
		return time.Time{}, fmt.Errorf("learn.daily_at %q: want HH:MM (00:00 to 23:59)", l.DailyAt)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, day.Location()), nil
}

// parseDailyAt reads "HH:MM" strictly: two digits, a colon, two digits, a valid
// time of day (parseClock, for quiet hours, takes "H:MM" too).
func parseDailyAt(s string) (hour, minute int, ok bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, 0, false
		}
	}
	hour, minute = int(s[0]-'0')*10+int(s[1]-'0'), int(s[3]-'0')*10+int(s[4]-'0')
	return hour, minute, hour < 24 && minute < 60
}

// LearnRole is the role the retro's classifying agent runs as: LearnRoleName,
// the [learn] kind in session mode with its model, effort, args, prompt,
// timeout and fallback models, writing retro.json, normalized like a
// configured role.
func (c *Config) LearnRole() Role {
	l := c.Learn
	return c.sessionRole(Role{Name: LearnRoleName, Kind: l.Kind, Model: l.Model, Effort: l.Effort, Args: l.Args,
		Prompt: l.Prompt, Output: LearnRoleName + ".json", Timeout: l.Timeout, FallbackModels: l.FallbackModels})
}

// EditorRole is the role the skill editor runs as: EditorRoleName, the
// [learn] editor_kind in session mode with its editor_model, editor_effort,
// editor_args, editor_prompt, editor_timeout and editor_fallback_models,
// writing changes.json, normalized like a configured role.
func (c *Config) EditorRole() Role {
	l := c.Learn
	return c.sessionRole(Role{Name: EditorRoleName, Kind: l.EditorKind, Model: l.EditorModel, Effort: l.EditorEffort, Args: l.EditorArgs,
		Prompt: l.EditorPrompt, Output: "changes.json", Timeout: l.EditorTimeout, FallbackModels: l.EditorFallbackModels})
}

// sessionRole is r, a role an agent runs outside the review rounds (the
// retro's classifier, LearnRole; the notes curator, NotesRole), in session
// mode with its output written as a file, normalized like a configured role.
func (c *Config) sessionRole(r Role) Role {
	r.Mode, r.Capture = ModeSession, CaptureFile
	return c.normalizedRoles([]Role{r})[0]
}

// validateSessionRole runs a sessionRole through validateRole, the checks a
// [[role]] gets, with its messages worded for its section ("learn: kind
// droid has no model args ..."). The prompt, the timeout and the fallback
// models are left out: the section's own checks cover them, with their own
// key names (fallbackModelErrs), so none is reported twice. The kind is
// declared (the caller checked).
func (c *Config) validateSessionRole(section string, r Role) []error {
	r.Prompt, r.Rereview, r.Timeout, r.FallbackModels = "", "", Duration{time.Minute}, nil
	var errs []error
	for _, err := range c.validateRole(r, c.kinds(), nil) {
		errs = append(errs, errors.New(section+": "+strings.TrimPrefix(err.Error(), "role "+r.Name+": ")))
	}
	return errs
}

// validateLearn checks [learn] always (a forced `magnum retro` runs whether or
// not the schedule is enabled): daily_at parses, the kind is a declared agent
// kind ("shell" is not one), the role it builds (LearnRole) passes the checks
// of a [[role]] (a kind without model args cannot be handed a model), the
// prompt names a file that resolves and parses, the limits are positive and
// settle is 0 or more and shorter than the lookback (else no PR is ever due).
func (c *Config) validateLearn() []error {
	l := c.Learn
	var errs []error
	if _, err := l.DailyTime(time.Time{}); err != nil {
		errs = append(errs, err)
	}
	if _, ok := c.KindSpec(l.Kind); !ok {
		errs = append(errs, fmt.Errorf("learn.kind %q is not a declared agent kind (declared: %s)", l.Kind, strings.Join(c.KindNames(), ", ")))
	} else {
		errs = append(errs, c.validateSessionRole("learn", c.LearnRole())...)
	}
	if l.Prompt == "" {
		errs = append(errs, errors.New("learn.prompt must name a prompt file"))
	} else if p, err := c.ResolvePrompt(l.Prompt); err != nil {
		errs = append(errs, fmt.Errorf("learn.prompt: %w", err))
	} else if err := parseTemplate(l.Prompt, p.Text); err != nil {
		errs = append(errs, fmt.Errorf("learn.prompt %s: %w", l.Prompt, err))
	}
	if l.Lookback.Duration <= 0 {
		errs = append(errs, fmt.Errorf("learn.lookback must be positive, got %s", l.Lookback.Duration))
	}
	switch {
	case l.Settle.Duration < 0:
		errs = append(errs, fmt.Errorf("learn.settle must not be negative, got %s", l.Settle.Duration))
	case l.Lookback.Duration > 0 && l.Settle.Duration >= l.Lookback.Duration:
		errs = append(errs, fmt.Errorf("learn.settle must be shorter than learn.lookback (%s), got %s", l.Lookback.Duration, l.Settle.Duration))
	}
	if l.Timeout.Duration <= 0 {
		errs = append(errs, fmt.Errorf("learn.timeout must be positive, got %s", l.Timeout.Duration))
	}
	if l.MaxPRs <= 0 {
		errs = append(errs, fmt.Errorf("learn.max_prs must be positive, got %d", l.MaxPRs))
	}
	if l.MinCommentChars <= 0 {
		errs = append(errs, fmt.Errorf("learn.min_comment_chars must be positive, got %d", l.MinCommentChars))
	}
	errs = append(errs, fallbackModelErrs("learn.fallback_models", l.FallbackModels)...)
	return append(errs, c.validateEditor()...)
}

// validateEditor checks the skill editor's keys of [learn] always (`magnum
// learn --propose` runs it whether or not editor is on): editor_kind is a
// declared agent kind whose role (EditorRole) passes a [[role]]'s checks,
// editor_prompt resolves and parses, the timeout and the gate's numbers are
// positive (gate_guards 0 to 10), restart is idle or manual and
// regression_points is 1 to 100.
func (c *Config) validateEditor() []error {
	l := c.Learn
	var errs []error
	if _, ok := c.KindSpec(l.EditorKind); !ok {
		errs = append(errs, fmt.Errorf("learn.editor_kind %q is not a declared agent kind (declared: %s)", l.EditorKind, strings.Join(c.KindNames(), ", ")))
	} else {
		errs = append(errs, c.validateSessionRole("learn editor", c.EditorRole())...)
	}
	if l.EditorPrompt == "" {
		errs = append(errs, errors.New("learn.editor_prompt must name a prompt file"))
	} else if p, err := c.ResolvePrompt(l.EditorPrompt); err != nil {
		errs = append(errs, fmt.Errorf("learn.editor_prompt: %w", err))
	} else if err := parseTemplate(l.EditorPrompt, p.Text); err != nil {
		errs = append(errs, fmt.Errorf("learn.editor_prompt %s: %w", l.EditorPrompt, err))
	}
	errs = append(errs, fallbackModelErrs("learn.editor_fallback_models", l.EditorFallbackModels)...)
	if l.EditorTimeout.Duration <= 0 {
		errs = append(errs, fmt.Errorf("learn.editor_timeout must be positive, got %s", l.EditorTimeout.Duration))
	}
	for _, n := range []struct {
		key string
		v   int
	}{{"gate_cases", l.GateCases}, {"land_per_day", l.LandPerDay}, {"regression_min_findings", l.RegressionMinFindings}} {
		if n.v <= 0 {
			errs = append(errs, fmt.Errorf("learn.%s must be positive, got %d", n.key, n.v))
		}
	}
	if l.GateGuards < 0 || l.GateGuards > 10 {
		errs = append(errs, fmt.Errorf("learn.gate_guards must be 0 to 10, got %d", l.GateGuards))
	}
	if l.Restart != LearnRestartIdle && l.Restart != LearnRestartManual {
		errs = append(errs, fmt.Errorf("learn.restart %q: want %s or %s", l.Restart, LearnRestartIdle, LearnRestartManual))
	}
	if l.RegressionPoints < 1 || l.RegressionPoints > 100 {
		errs = append(errs, fmt.Errorf("learn.regression_points must be 1 to 100, got %d", l.RegressionPoints))
	}
	return errs
}
