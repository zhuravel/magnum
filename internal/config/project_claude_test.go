package config

import (
	"slices"
	"strings"
	"testing"
)

// A review session needs none of the operator's own MCP servers, and a
// Claude session with them kept them in the pane's foreground after its
// agent was asked to exit, so the quit before a checkout failed. The claude
// kind turns them all off at once (Claude Code has no flag per server):
// mcp_off with mcp_strict, --strict-mcp-config without --mcp-config, at
// every launch and resume, a wrapper's too; mcp_off = false keeps them;
// "--mcp-config", "<file>" added to mcp_strict keeps that file's servers.
// mcp_allow names servers only a kind with mcp_disable turns off one by
// one, so a claude kind setting it is refused.
func TestClaudeStartsWithoutTheUsersMCPServers(t *testing.T) {
	claude, _ := Defaults().KindSpec(KindClaude)
	if !claude.MCPOff || !slices.Equal(claude.MCPStrict, []string{"--strict-mcp-config"}) {
		t.Fatalf("claude mcp_off = %v, mcp_strict = %q", claude.MCPOff, claude.MCPStrict)
	}
	for _, a := range []LaunchArgs{{Effort: "high"}, {Session: "s-1", Effort: "high"}, {Effort: "high", Wrapper: true}} {
		if got := claude.Argv(a); !slices.Contains(got, "--strict-mcp-config") || slices.Index(got, "--strict-mcp-config") < slices.Index(got, "high") {
			t.Fatalf("%+v: argv = %q, want --strict-mcp-config after the effort", a, got)
		}
	}
	if codex, _ := Defaults().KindSpec(KindCodex); len(codex.MCPStrict) > 0 || slices.Contains(codex.Argv(LaunchArgs{}), "--strict-mcp-config") {
		t.Fatalf("codex mcp_strict = %q", codex.MCPStrict)
	}

	on := mustLoad(t, map[string]string{"config.toml": "[kinds.claude]\nmcp_off = false\n" + minimalConfig})
	if k, _ := on.KindSpec(KindClaude); slices.Contains(k.Argv(LaunchArgs{Effort: "high"}), "--strict-mcp-config") {
		t.Fatalf("mcp_off = false: argv = %q", k.Argv(LaunchArgs{Effort: "high"}))
	}
	file := "/Users/x/.config/magnum/claude-mcp.json"
	allowed := mustLoad(t, map[string]string{"config.toml": "[kinds.claude]\nmcp_strict = [\"--strict-mcp-config\", \"--mcp-config\", \"" + file + "\"]\n" + minimalConfig})
	k, _ := allowed.KindSpec(KindClaude)
	if got := k.Argv(LaunchArgs{Wrapper: true}); !slices.Equal(got, []string{"--strict-mcp-config", "--mcp-config", file}) {
		t.Fatalf("mcp_strict with a server file: argv = %q", got)
	}

	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[kinds.claude]\nmcp_allow = [\"docs\"]\n" + minimalConfig})
	if err == nil || !strings.Contains(err.Error(), "kinds.claude: mcp_allow") || !strings.Contains(err.Error(), "--mcp-config") {
		t.Fatalf("mcp_allow on claude: err = %v", err)
	}
}

// A PR controls its checkout's .claude/ and .mcp.json, which Claude Code
// loads as the project and local setting sources (settings with hooks,
// MCP servers, skills, commands, agents, CLAUDE.md). A session started for
// a PR that changes them gets the claude kind's project_untrust, which
// names no path: --setting-sources user, after the effort and before the
// kind's own args, on a launch and a resume alike.
func TestClaudeLoadsOnlyTheUserSettingsForAChangedCheckout(t *testing.T) {
	claude, _ := Defaults().KindSpec(KindClaude)
	if !slices.Equal(claude.ProjectUntrust, []string{"--setting-sources", "user"}) {
		t.Fatalf("claude project_untrust = %q", claude.ProjectUntrust)
	}
	paths := []string{"/Users/x/Projects/talkable.review1", "/private/x/talkable.review1"}
	got := claude.Argv(LaunchArgs{Session: "s-1", Title: "t", Effort: "high", Untrusted: paths})
	want := []string{"--resume", "s-1", "--name", "t", "--effort", "high", "--strict-mcp-config", "--setting-sources", "user",
		"--dangerously-skip-permissions"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}
	if got := claude.Argv(LaunchArgs{Effort: "high", Wrapper: true}); !slices.Equal(got, []string{"--effort", "high", "--strict-mcp-config"}) {
		t.Fatalf("an unchanged checkout: argv = %q", got)
	}
	// {projects} is optional: a kind's args may keep the project out
	// without naming its paths, so such a key loads and validates.
	cfg := mustLoad(t, map[string]string{"config.toml": "[kinds.claude]\nproject_untrust = [\"--setting-sources\", \"user\", \"--strict-mcp-config\"]\n" + minimalConfig})
	k, _ := cfg.KindSpec(KindClaude)
	if got := k.UntrustArgs(paths); !slices.Equal(got, []string{"--setting-sources", "user", "--strict-mcp-config"}) {
		t.Fatalf("UntrustArgs = %q", got)
	}
	if got := k.UntrustArgs(nil); got != nil {
		t.Fatalf("an unchanged checkout: UntrustArgs = %q", got)
	}
	none := mustLoad(t, map[string]string{"config.toml": "[kinds.claude]\nproject_untrust = []\n" + minimalConfig})
	if k, _ := none.KindSpec(KindClaude); k.UntrustArgs(paths) != nil {
		t.Fatalf("project_untrust = []: %q", k.UntrustArgs(paths))
	}
}
