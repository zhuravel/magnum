package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Triage is the [triage] section: before a round starts its agents, a cheap
// model is asked which of the round's reviewers its diff needs, so a one-line
// fix does not wake every reviewer. Rounds with more than MaxLines changed
// lines run every role without asking; the judge always runs, the model can
// only remove roles that have a Summary, and any failure runs every role
// (internal/engine/triage.go, DECISIONS "Triage of small diffs").
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

// DefaultTriagePrompt is the triage prompt file.
const DefaultTriagePrompt = "triage.md"

// DefaultTriage returns the built-in [triage] values: off, Claude haiku with
// no tools and no session kept.
func DefaultTriage() Triage {
	return Triage{
		MaxLines: 120,
		Command:  []string{"claude", "-p", "--model", "haiku", "--tools", "", "--no-session-persistence"},
		Timeout:  Duration{2 * time.Minute},
		Prompt:   DefaultTriagePrompt,
	}
}

// validateTriage checks [triage]: the prompt names a file that resolves and
// parses always (the daemon loads it at startup either way); the limits and
// the command only when triage is enabled.
func (c *Config) validateTriage() []error {
	t := c.Triage
	var errs []error
	if t.Prompt == "" {
		errs = append(errs, errors.New("triage.prompt must name a prompt file"))
	} else if p, err := c.ResolvePrompt(t.Prompt); err != nil {
		errs = append(errs, fmt.Errorf("triage.prompt: %w", err))
	} else if err := parseTemplate(t.Prompt, p.Text); err != nil {
		errs = append(errs, fmt.Errorf("triage.prompt %s: %w", t.Prompt, err))
	}
	if t.Timeout.Duration < 0 {
		errs = append(errs, fmt.Errorf("triage.timeout must not be negative, got %s", t.Timeout.Duration))
	}
	if !t.Enabled {
		return errs
	}
	if t.MaxLines <= 0 {
		errs = append(errs, fmt.Errorf("triage.max_lines must be positive, got %d", t.MaxLines))
	}
	if len(t.Command) == 0 || strings.TrimSpace(t.Command[0]) == "" {
		errs = append(errs, errors.New("triage.command must name the model's CLI (an argument list)"))
	}
	if t.Timeout.Duration == 0 {
		errs = append(errs, fmt.Errorf("triage.timeout must be positive, got %s", t.Timeout.Duration))
	}
	return errs
}

// Removable is whether triage may drop the role from a round: not the judge,
// and it has a Summary to show the model.
func (r Role) Removable() bool { return !r.Judge && strings.TrimSpace(r.Summary) != "" }
