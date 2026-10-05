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
}

// DefaultLearn returns the built-in [learn] values: off, a week of lookback,
// Claude sonnet classifying.
func DefaultLearn() Learn {
	return Learn{
		DailyAt:         "07:00",
		Lookback:        Duration{7 * 24 * time.Hour},
		MaxPRs:          20,
		MinCommentChars: 20,
		Kind:            KindClaude,
		Model:           DefaultLearnModel(KindClaude),
		Prompt:          DefaultLearnPrompt,
		Timeout:         Duration{20 * time.Minute},
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
// the [learn] kind in session mode with its model, effort, args, prompt and
// timeout, writing retro.json, normalized like a configured role.
func (c *Config) LearnRole() Role {
	l := c.Learn
	r := Role{
		Name: LearnRoleName, Kind: l.Kind, Mode: ModeSession,
		Model: l.Model, Effort: l.Effort, Args: l.Args,
		Prompt: l.Prompt, Capture: CaptureFile, Output: LearnRoleName + ".json",
		Timeout: l.Timeout,
	}
	return c.normalizedRoles([]Role{r})[0]
}

// validateLearnRole runs LearnRole through validateRole, the checks a [[role]]
// gets, with its messages worded for [learn] ("learn: kind droid has no model
// args ..."). The prompt and the timeout are left out: validateLearn checks
// learn.prompt and learn.timeout itself, with their own wording, so neither is
// reported twice. The kind is declared (the caller checked).
func (c *Config) validateLearnRole() []error {
	r := c.LearnRole()
	r.Prompt, r.Rereview, r.Timeout = "", "", Duration{time.Minute}
	var errs []error
	for _, err := range c.validateRole(r, c.kinds(), nil) {
		errs = append(errs, errors.New("learn: "+strings.TrimPrefix(err.Error(), "role "+LearnRoleName+": ")))
	}
	return errs
}

// validateLearn checks [learn] always (a forced `magnum retro` runs whether or
// not the schedule is enabled): daily_at parses, the kind is a declared agent
// kind ("shell" is not one), the role it builds (LearnRole) passes the checks
// of a [[role]] (a kind without model args cannot be handed a model), the
// prompt names a file that resolves and parses, and the limits are positive.
func (c *Config) validateLearn() []error {
	l := c.Learn
	var errs []error
	if _, err := l.DailyTime(time.Time{}); err != nil {
		errs = append(errs, err)
	}
	if _, ok := c.KindSpec(l.Kind); !ok {
		errs = append(errs, fmt.Errorf("learn.kind %q is not a declared agent kind (declared: %s)", l.Kind, strings.Join(c.KindNames(), ", ")))
	} else {
		errs = append(errs, c.validateLearnRole()...)
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
	if l.Timeout.Duration <= 0 {
		errs = append(errs, fmt.Errorf("learn.timeout must be positive, got %s", l.Timeout.Duration))
	}
	if l.MaxPRs <= 0 {
		errs = append(errs, fmt.Errorf("learn.max_prs must be positive, got %d", l.MaxPRs))
	}
	if l.MinCommentChars <= 0 {
		errs = append(errs, fmt.Errorf("learn.min_comment_chars must be positive, got %d", l.MinCommentChars))
	}
	return errs
}
