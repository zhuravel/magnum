package config

import (
	"slices"
	"strings"
	"testing"
)

// Magnum's Codex sessions loaded every MCP server of the operator's Codex
// config. Codex merges -c tables into that config, so a server can be
// turned off only by name: the codex kind's mcp_off (default true) passes
// mcp_disable once per server the session would load, after the effort and
// subagent args, except the servers in mcp_allow. Its built-in apps
// connector (codex_apps) is no such table: mcp_strict turns the apps
// feature off once, servers or none, whatever mcp_allow names.
func TestCodexTurnsOffTheMCPServersItWouldLoadButTheAllowedOnes(t *testing.T) {
	codex, _ := Defaults().KindSpec(KindCodex)
	if !codex.MCPOff || len(codex.MCPAllow) != 0 {
		t.Fatalf("codex mcp_off = %v, mcp_allow = %q; want true and none", codex.MCPOff, codex.MCPAllow)
	}
	appsOff := []string{"-c", "features.apps=false"}
	servers := []string{"browser", "docs"}
	got := codex.Argv(LaunchArgs{Effort: "xhigh", MCPServers: servers, Wrapper: true})
	want := []string{"-c", "model_reasoning_effort=xhigh", "-c", "features.apps=false", "-c", "mcp_servers.browser.enabled=false", "-c", "mcp_servers.docs.enabled=false"}
	if !slices.Equal(got, want) {
		t.Fatalf("codex argv = %q, want %q", got, want)
	}
	if got := codex.MCPOffArgs(nil); !slices.Equal(got, appsOff) {
		t.Fatalf("no servers: %q, want the apps connector off alone", got)
	}

	allowed := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nmcp_allow = [\"docs\"]\n" + minimalConfig})
	k, _ := allowed.KindSpec(KindCodex)
	if got := k.MCPOffArgs(servers); !slices.Equal(got, slices.Concat(appsOff, []string{"-c", "mcp_servers.browser.enabled=false"})) {
		t.Fatalf("mcp_allow docs: %q", got)
	}
	on := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nmcp_off = false\n" + minimalConfig})
	k, _ = on.KindSpec(KindCodex)
	if got := k.Argv(LaunchArgs{MCPServers: servers, Wrapper: true}); len(got) != 0 {
		t.Fatalf("mcp_off = false: %q, want none", got)
	}
	// A kind without mcp_disable args cannot turn a server off by name: the
	// claude kind's mcp_strict turns them all off at once, whatever the
	// servers; without it too, mcp_off is ignored, as max_subagents is
	// without subagents args.
	claude := mustLoad(t, map[string]string{"config.toml": "[kinds.claude]\nmcp_off = true\n" + minimalConfig})
	k, _ = claude.KindSpec(KindClaude)
	if got := k.MCPOffArgs(servers); !slices.Equal(got, []string{"--strict-mcp-config"}) {
		t.Fatalf("claude: %q, want --strict-mcp-config alone", got)
	}
	k.MCPStrict = nil
	if got := k.MCPOffArgs(servers); got != nil {
		t.Fatalf("claude without mcp_strict: %q, want none", got)
	}
}

func TestMCPDisableNeedsItsServerPlaceholder(t *testing.T) {
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[kinds.codex]\nmcp_disable = [\"-c\", \"mcp_servers.enabled=false\"]\n" + minimalConfig})
	if err == nil || !strings.Contains(err.Error(), "mcp_disable") {
		t.Fatalf("err = %v, want one naming mcp_disable", err)
	}
}
