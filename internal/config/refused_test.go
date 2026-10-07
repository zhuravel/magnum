package config

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every kind starts with the refused patterns, which match Codex's
// cybersecurity refusal, and a config sets its own list in their place
// without touching the other lists.
func TestRefusedPatternsAreDataEveryKindStartsWith(t *testing.T) {
	refusal := "■ This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request."
	h, err := DefaultHealthPatterns().Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !slices.ContainsFunc(h.Refused, func(re *regexp.Regexp) bool { return re.MatchString(refusal) }) {
		t.Fatalf("no default refused pattern matches %q", refusal)
	}
	for name, k := range DefaultKinds() {
		if !slices.Equal(k.HealthPatterns.Refused, DefaultHealthPatterns().Refused) {
			t.Errorf("kind %s refused = %q", name, k.HealthPatterns.Refused)
		}
	}

	cfg := mustLoad(t, map[string]string{
		"config.toml": minimalConfig + "[kinds.codex.health_patterns]\nrefused = [\"custom flag\"]\n",
	})
	codex, _ := cfg.KindSpec("codex")
	if !slices.Equal(codex.HealthPatterns.Refused, []string{"custom flag"}) ||
		!slices.Equal(codex.HealthPatterns.UsageLimit, DefaultHealthPatterns().UsageLimit) {
		t.Fatalf("codex health = %+v", codex.HealthPatterns)
	}
	rx, err := codex.HealthPatterns.Compile()
	if err != nil || len(rx.Refused) != 1 || !rx.Refused[0].MatchString("a CUSTOM FLAG here") {
		t.Fatalf("compile: %v %+v", err, rx.Refused)
	}

	bad := validPipelineConfig()
	k := bad.Kinds["codex"]
	k.HealthPatterns.Refused = []string{"("}
	bad.Kinds["codex"] = k
	bad.Normalize()
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "health_patterns.refused") {
		t.Fatalf("Validate() = %v, want the refused pattern named", err)
	}
}
