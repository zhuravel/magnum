package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// A PR controls its checkout's .codex/: a session started for one that
// changes it gets project_untrust with every path of the checkout in ONE
// TOML table (a second -c projects=... would replace the first), after the
// MCP args and before the kind's own args, and the table parses as the
// [projects] entries Codex reads.
func TestCodexMarksAChangedCheckoutUntrustedInOneTable(t *testing.T) {
	codex, _ := Defaults().KindSpec(KindCodex)
	if !slices.Equal(codex.ProjectUntrust, []string{"-c", "projects={projects}"}) || codex.ProjectMCP != ProjectMCPAllow {
		t.Fatalf("codex project_untrust = %q, project_mcp = %q", codex.ProjectUntrust, codex.ProjectMCP)
	}
	paths := []string{"/Users/x/Projects/talkable.review1", `/private/tmp/a "b"\c`}
	got := codex.Argv(LaunchArgs{MCPServers: []string{"docs"}, Untrusted: paths})
	table := `{"/Users/x/Projects/talkable.review1"={trust_level="untrusted"},"/private/tmp/a \"b\"\\c"={trust_level="untrusted"}}`
	want := []string{"-c", "mcp_servers.docs.enabled=false", "-c", "projects=" + table, "--dangerously-bypass-approvals-and-sandbox"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}
	var doc struct {
		X map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"_x_"`
	}
	if _, err := toml.Decode("_x_ = "+table, &doc); err != nil {
		t.Fatalf("the table is no TOML value: %v", err)
	}
	for _, p := range paths {
		if doc.X[p].TrustLevel != "untrusted" {
			t.Fatalf("parsed %+v, want %q untrusted", doc.X, p)
		}
	}
	if got := codex.UntrustArgs(nil); got != nil {
		t.Fatalf("no paths: %q", got)
	}
	claude, _ := Defaults().KindSpec(KindClaude)
	if got := claude.UntrustArgs(paths); got != nil {
		t.Fatalf("claude has no project_untrust: %q", got)
	}
	none := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nproject_untrust = []\n" + minimalConfig})
	k, _ := none.KindSpec(KindCodex)
	if got := k.UntrustArgs(paths); got != nil {
		t.Fatalf("project_untrust = []: %q", got)
	}
}

// When the PR leaves .codex/ alone the checkout's project servers are the
// team's: project_mcp "allow" (the default) keeps them; "off" turns them off
// like the user's, by name, once even when both declare one, but for those
// in mcp_allow, and also with mcp_off = false.
func TestProjectMCPOffTurnsTheCheckoutsServersOffButTheAllowedOnes(t *testing.T) {
	user, project := []string{"browser", "docs"}, []string{"docs", "sentry", "tracker"}
	codex, _ := Defaults().KindSpec(KindCodex)
	if got := codex.ConfigOffArgs(user, project, nil); !slices.Equal(got, []string{"-c", "mcp_servers.browser.enabled=false", "-c", "mcp_servers.docs.enabled=false"}) {
		t.Fatalf("project_mcp allow: %q, want the user's servers only", got)
	}
	off := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nproject_mcp = \"OFF\"\nmcp_allow = [\"tracker\"]\n" + minimalConfig})
	k, _ := off.KindSpec(KindCodex)
	want := []string{"-c", "mcp_servers.browser.enabled=false", "-c", "mcp_servers.docs.enabled=false", "-c", "mcp_servers.sentry.enabled=false"}
	if got := k.ConfigOffArgs(user, project, nil); !slices.Equal(got, want) {
		t.Fatalf("project_mcp off: %q\nwant %q", got, want)
	}
	if got := k.Argv(LaunchArgs{MCPServers: user, ProjectServers: project, Wrapper: true}); !slices.Equal(got, want) {
		t.Fatalf("argv: %q\nwant %q", got, want)
	}
	userOn := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nproject_mcp = \"off\"\nmcp_off = false\n" + minimalConfig})
	k, _ = userOn.KindSpec(KindCodex)
	want = []string{"-c", "mcp_servers.docs.enabled=false", "-c", "mcp_servers.sentry.enabled=false", "-c", "mcp_servers.tracker.enabled=false"}
	if got := k.ConfigOffArgs(user, project, nil); !slices.Equal(got, want) {
		t.Fatalf("mcp_off false, project_mcp off: %q\nwant %q", got, want)
	}
}

func TestProjectKeysAreValidated(t *testing.T) {
	for key, cfg := range map[string]string{
		"project_mcp":     "[kinds.codex]\nproject_mcp = \"sometimes\"\n",
		"project_untrust": "[kinds.codex]\nproject_untrust = [\"-c\", \"projects={}\"]\n",
	} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": cfg + minimalConfig})
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v, want one naming the key", key, err)
		}
	}
}
