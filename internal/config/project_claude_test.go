package config

import (
	"slices"
	"testing"
)

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
	want := []string{"--resume", "s-1", "--name", "t", "--effort", "high", "--setting-sources", "user", "--dangerously-skip-permissions"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}
	if got := claude.Argv(LaunchArgs{Effort: "high", Wrapper: true}); !slices.Equal(got, []string{"--effort", "high"}) {
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
