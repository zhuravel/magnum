package config

import (
	"slices"
	"strings"
	"testing"
)

// The judge started 53 subagent threads in 36 sessions on its own, 27% of
// the Codex spend (two of them 1.4M and 1.3M tokens on one PR). A role's
// max_subagents caps the subagents its agent may have open at once, through
// its kind's subagents args (Codex: agents.max_concurrent_threads_per_session),
// and 0 turns them off (no_subagents: agents.enabled = false). The judge
// defaults to 2.
func TestTheJudgeCapsItsSubagents(t *testing.T) {
	def := Defaults()
	j := def.JudgeFor(nil)
	if j.MaxSubagents == nil || *j.MaxSubagents != 2 {
		t.Fatalf("judge max_subagents = %v, want 2", j.MaxSubagents)
	}
	for _, r := range def.RolesFor(nil) {
		if !r.Judge && r.MaxSubagents != nil {
			t.Errorf("%s max_subagents = %d, want unset", r.Name, *r.MaxSubagents)
		}
	}
	codex, _ := def.KindSpec(KindCodex)
	two, zero := 2, 0
	for _, tc := range []struct {
		name string
		n    *int
		want []string
	}{
		{"a cap", &two, []string{"-c", "model_reasoning_effort=xhigh", "-c", "agents.max_concurrent_threads_per_session=2", "-c", "features.apps=false"}},
		{"none at all", &zero, []string{"-c", "model_reasoning_effort=xhigh", "-c", "agents.enabled=false", "-c", "features.apps=false"}},
		{"unset", nil, []string{"-c", "model_reasoning_effort=xhigh", "-c", "features.apps=false"}},
	} {
		if got := codex.Argv(LaunchArgs{Effort: "xhigh", Subagents: tc.n, Wrapper: true}); !slices.Equal(got, tc.want) {
			t.Errorf("%s: codex argv = %q, want %q", tc.name, got, tc.want)
		}
	}
	// A kind without subagents args cannot cap them: the key is ignored,
	// as effort is for a kind without effort args.
	claude, _ := def.KindSpec(KindClaude)
	if got := claude.Argv(LaunchArgs{Subagents: &two, Wrapper: true}); !slices.Equal(got, claude.MCPOffArgs(nil)) {
		t.Errorf("claude argv = %q, want its MCP args alone", got)
	}

	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + `
[[role]]
name = "codex-judge"
max_subagents = 0
`})
	if j := cfg.JudgeFor(nil); j.MaxSubagents == nil || *j.MaxSubagents != 0 || j.Effort != "xhigh" {
		t.Fatalf("a user's max_subagents = 0: judge %+v", j)
	}
}

func TestMaxSubagentsRefusesANegativeCapAndArgsWithoutTheirPlaceholder(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"negative": {minimalConfig + "\n[[role]]\nname = \"codex-judge\"\nmax_subagents = -1\n", "max_subagents"},
		"no placeholder": {"[kinds.codex]\nsubagents = [\"-c\", \"agents.max_threads=2\"]\n" + minimalConfig,
			"subagents"},
	} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": tc.body})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %s", name, err, tc.want)
		}
	}
}
