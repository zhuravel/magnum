package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The notes critic ([notes] critic*; internal/engine/notes_critic.go, DECISIONS
// "The notes critic decides curation proposals"): an agent that reviews each
// curation proposal as the operator does with `magnum notes <repo> --review`
// and applies, rejects or re-curates it. Off by default, so the operator
// decides every proposal.

// DefaultCriticPrompt is the critic's prompt file.
const DefaultCriticPrompt = "notes-critic.md"

// CriticRoleName is the name of the role the critic runs as (CriticRole).
const CriticRoleName = "notes-critic"

// Built-in critic values.
const (
	DefaultCriticDeclines = 2
	DefaultCriticPerDay   = 4
)

// DefaultCriticModel is the critic's model of a kind the config names
// without choosing a model: fable, the strongest Claude model (a second
// opinion on the curator's work), for claude, and "" for every other kind,
// whose CLI would not know a Claude model name.
func DefaultCriticModel(kind string) string {
	if kind == KindClaude {
		return "fable"
	}
	return ""
}

// defaultCritic sets the built-in critic values of n.
func defaultCritic(n *Notes) {
	n.CriticKind = KindClaude
	n.CriticModel = DefaultCriticModel(KindClaude)
	n.CriticEffort = "xhigh"
	n.CriticPrompt = DefaultCriticPrompt
	n.CriticTimeout = Duration{30 * time.Minute}
	n.CriticFallbackModels = []string{"opus"}
	n.CriticDeclines = DefaultCriticDeclines
	n.CriticPerDay = DefaultCriticPerDay
}

// CriticRole is the role the critic runs as: CriticRoleName, the [notes]
// critic_kind in session mode with its model, effort, args, prompt, timeout
// and fallback models, normalized like a configured role.
func (c *Config) CriticRole() Role {
	n := c.Notes
	return c.sessionRole(Role{Name: CriticRoleName, Kind: n.CriticKind, Model: n.CriticModel, Effort: n.CriticEffort, Args: n.CriticArgs,
		Prompt: n.CriticPrompt, Output: "decision.json", Timeout: n.CriticTimeout, FallbackModels: n.CriticFallbackModels})
}

// validateCritic checks the critic's keys always (as the curator's and the
// skill editor's are checked whether or not they run): the kind is a
// declared agent kind whose role (CriticRole) passes a [[role]]'s checks,
// the prompt resolves and parses, each fallback model is one word listed
// once, the timeout, the declines and the daily cap are positive.
func (c *Config) validateCritic() []error {
	n := c.Notes
	var errs []error
	if _, ok := c.KindSpec(n.CriticKind); !ok {
		errs = append(errs, fmt.Errorf("notes.critic_kind %q is not a declared agent kind (declared: %s)", n.CriticKind, strings.Join(c.KindNames(), ", ")))
	} else {
		errs = append(errs, c.validateSessionRole("notes critic", c.CriticRole())...)
	}
	if n.CriticPrompt == "" {
		errs = append(errs, errors.New("notes.critic_prompt must name a prompt file"))
	} else if p, err := c.ResolvePrompt(n.CriticPrompt); err != nil {
		errs = append(errs, fmt.Errorf("notes.critic_prompt: %w", err))
	} else if err := parseTemplate(n.CriticPrompt, p.Text); err != nil {
		errs = append(errs, fmt.Errorf("notes.critic_prompt %s: %w", n.CriticPrompt, err))
	}
	errs = append(errs, fallbackModelErrs("notes.critic_fallback_models", n.CriticFallbackModels)...)
	if n.CriticTimeout.Duration <= 0 {
		errs = append(errs, fmt.Errorf("notes.critic_timeout must be positive, got %s", n.CriticTimeout.Duration))
	}
	for _, v := range []struct {
		key string
		n   int
	}{{"critic_declines", n.CriticDeclines}, {"critic_per_day", n.CriticPerDay}} {
		if v.n <= 0 {
			errs = append(errs, fmt.Errorf("notes.%s must be positive, got %d", v.key, v.n))
		}
	}
	return errs
}
