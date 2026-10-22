package config

import (
	"slices"
	"testing"
)

// codexToolOutput is the arg pair the built-in codex kind passes.
var codexToolOutput = []string{"-c", "tool_output_token_limit=65536"}

// carries reports whether argv holds want as consecutive elements.
func carries(argv, want []string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// Codex 0.162.1 keeps at most 10,000 tokens of one tool call's output by
// default and drops the middle, so a judge reading the 97 KB notes whole
// lost their middle. Every Codex launch and resume carries the raised
// ceiling, tool_output_token_limit=65536 (Codex's policy over every tool
// output a session keeps), after the effort and before the args that keep
// configuration out; claude has no such key, and a kind set to 0 keeps the
// CLI's own.
func TestEveryCodexLaunchAndResumeCarriesTheToolOutputCeiling(t *testing.T) {
	kinds := DefaultKinds()
	codex := kinds[KindCodex]
	if codex.ToolOutputLimit != DefaultCodexToolOutputLimit || DefaultCodexToolOutputLimit != 65536 {
		t.Fatalf("codex tool_output_limit = %d, want %d", codex.ToolOutputLimit, 65536)
	}
	if got := codex.ToolOutputArgs(); !slices.Equal(got, codexToolOutput) {
		t.Fatalf("codex ToolOutputArgs = %q, want %q", got, codexToolOutput)
	}
	for what, a := range map[string]LaunchArgs{
		"launch": {Wrapper: true, Effort: "high"},
		"resume": {Session: "u-1", Wrapper: true, Effort: "high"},
	} {
		argv := codex.Argv(a)
		if !carries(argv, codexToolOutput) {
			t.Errorf("codex %s argv = %q, want %q", what, argv, codexToolOutput)
		}
		effort := slices.Index(argv, "model_reasoning_effort=high")
		limit := slices.Index(argv, "tool_output_token_limit=65536")
		strict := slices.Index(argv, "features.apps=false")
		if effort < 0 || strict < 0 || effort >= limit || limit >= strict {
			t.Errorf("codex %s argv = %q: want the ceiling after the effort and before the MCP args", what, argv)
		}
	}
	for _, name := range []string{KindClaude, KindDroid, KindOMP} {
		k := kinds[name]
		if k.ToolOutputLimit != 0 || len(k.ToolOutput) != 0 || k.ToolOutputArgs() != nil {
			t.Errorf("%s tool output = %d %q, want none", name, k.ToolOutputLimit, k.ToolOutput)
		}
		for _, a := range k.Argv(LaunchArgs{Session: "u-1", Wrapper: true}) {
			if a == "tool_output_token_limit=65536" {
				t.Errorf("%s argv carries the codex ceiling", name)
			}
		}
	}

	off := mustLoad(t, map[string]string{"config.toml": minimalConfig + "[kinds.codex]\ntool_output_limit = 0\n"})
	k, _ := off.KindSpec(KindCodex)
	if got := k.ToolOutputArgs(); got != nil {
		t.Errorf("tool_output_limit = 0: ToolOutputArgs = %q, want none", got)
	}
	for _, a := range k.Argv(LaunchArgs{Session: "u-1", Wrapper: true}) {
		if a == "tool_output_token_limit=65536" || a == "tool_output_token_limit=0" {
			t.Errorf("tool_output_limit = 0: argv carries %q", a)
		}
	}
	if k.ToolOutput == nil {
		t.Errorf("tool_output_limit = 0 dropped the kind's tool_output args")
	}

	set := mustLoad(t, map[string]string{"config.toml": minimalConfig + "[kinds.codex]\ntool_output_limit = 131072\n"})
	k, _ = set.KindSpec(KindCodex)
	if got, want := k.ToolOutputArgs(), []string{"-c", "tool_output_token_limit=131072"}; !slices.Equal(got, want) {
		t.Errorf("tool_output_limit = 131072: %q, want %q", got, want)
	}
	// A kind of the operator's sets both keys.
	pi := mustLoad(t, map[string]string{"config.toml": minimalConfig + "[kinds.pi]\ntool_output_limit = 4000\ntool_output = [\"--max-output\", \"{limit}\"]\n"})
	k, _ = pi.KindSpec("pi")
	if got, want := k.Argv(LaunchArgs{Wrapper: true}), []string{"--max-output", "4000"}; !slices.Equal(got, want) {
		t.Errorf("pi argv = %q, want %q", got, want)
	}
}

// tool_output_limit is a count of tokens (0 or more), and tool_output must
// name {limit}, as the other args with a placeholder must name theirs.
func TestToolOutputKeysAreValidated(t *testing.T) {
	for key, block := range map[string]string{
		"tool_output_limit": "[kinds.codex]\ntool_output_limit = -1\n",
		"tool_output":       "[kinds.codex]\ntool_output = [\"-c\", \"tool_output_token_limit=9\"]\n",
	} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + block})
		wantError(t, err, key)
	}
}
