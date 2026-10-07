package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// [daemon] reviewer_timeout and judge_timeout are only the fallbacks of a
// role that sets no timeout of its own: with the built-in defaults, whose
// four [[role]] blocks each set one, an operator who sets them changed
// nothing and was not told. A role the user config adds without a timeout
// takes reviewer_timeout; the built-in roles keep theirs; and the keys that
// cover no role warn once, naming each key and the roles that set their own.
func TestDaemonTimeoutsAreTheFallbackOfARoleWithoutItsOwn(t *testing.T) {
	keys := `
[daemon]
reviewer_timeout = "50m"
judge_timeout = "2h"
`
	extra := `
[[role]]
name = "claude-second"
kind = "claude"
prompt = "claude-review.md"
output = "claude-second.md"
`
	timeoutWarnings := func(cfg *Config) []string {
		var out []string
		for _, w := range cfg.Warnings() {
			if strings.Contains(w, "_timeout") {
				out = append(out, w)
			}
		}
		return out
	}

	cfg, err := loadCommittedWithLocal(t, testLocalConfig+keys+extra)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]time.Duration{"claude-second": 50 * time.Minute, "claude-review": 40 * time.Minute, "codex-judge": 90 * time.Minute} {
		if r, ok := cfg.RoleByNameOrAlias(nil, name); !ok || r.Timeout.Duration != want {
			t.Errorf("%s timeout = %v (found %v), want %v", name, r.Timeout.Duration, ok, want)
		}
	}
	w := timeoutWarnings(cfg)
	if len(w) != 1 || !strings.Contains(w[0], "judge_timeout") || strings.Contains(w[0], "reviewer_timeout") ||
		!strings.Contains(w[0], "codex-judge 1h30m") || !strings.Contains(w[0], "[[role]]") {
		t.Fatalf("with a role that takes reviewer_timeout: warnings = %q, want one naming judge_timeout and codex-judge's own", w)
	}

	cfg, err = loadCommittedWithLocal(t, testLocalConfig+keys)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := cfg.RoleByNameOrAlias(nil, "claude-review"); r.Timeout.Duration != 40*time.Minute {
		t.Errorf("claude-review timeout = %v, want its own 40m", r.Timeout.Duration)
	}
	w = timeoutWarnings(cfg)
	if len(w) != 1 || !strings.Contains(w[0], "reviewer_timeout") || !strings.Contains(w[0], "judge_timeout") ||
		!strings.Contains(w[0], "claude-review 40m") {
		t.Fatalf("with every role setting its own: warnings = %q, want one naming both keys", w)
	}

	cfg, err = loadCommittedWithLocal(t, testLocalConfig)
	if err != nil {
		t.Fatal(err)
	}
	if w := timeoutWarnings(cfg); len(w) != 0 {
		t.Fatalf("without the keys: warnings = %q, want none", w)
	}

	// A base config whose roles set no timeout (the built-in roles of code)
	// gives the keys something to do: no warning.
	cfg = mustLoad(t, map[string]string{"config.toml": minimalConfig + keys})
	if w := timeoutWarnings(cfg); len(w) != 0 {
		t.Fatalf("roles without their own timeout: warnings = %q, want none", w)
	}
	if got := roleNames(cfg.Roles); !slices.Contains(got, "codex-judge") {
		t.Fatalf("roles = %v", got)
	}
}
