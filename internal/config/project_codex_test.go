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
	if !slices.Equal(codex.ProjectUntrust, []string{"-c", "projects={projects}"}) || codex.ProjectMCP != ProjectMCPOff {
		t.Fatalf("codex project_untrust = %q, project_mcp = %q", codex.ProjectUntrust, codex.ProjectMCP)
	}
	paths := []string{"/Users/x/Projects/talkable.review1", `/private/tmp/a "b"\c`}
	got := codex.Argv(LaunchArgs{MCPServers: []string{"docs"}, Untrusted: paths})
	table := `{"/Users/x/Projects/talkable.review1"={trust_level="untrusted"},"/private/tmp/a \"b\"\\c"={trust_level="untrusted"}}`
	want := []string{"-c", "tool_output_token_limit=65536", "-c", "features.apps=false", "-c", "skills.include_instructions=false", "-c", "mcp_servers.docs.enabled=false", "-c", "projects=" + table, "--dangerously-bypass-approvals-and-sandbox"}
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
	omp, _ := Defaults().KindSpec(KindOMP)
	if got := omp.UntrustArgs(paths); got != nil {
		t.Fatalf("omp has no project_untrust: %q", got)
	}
	none := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nproject_untrust = []\n" + minimalConfig})
	k, _ := none.KindSpec(KindCodex)
	if got := k.UntrustArgs(paths); got != nil {
		t.Fatalf("project_untrust = []: %q", got)
	}
}

// When the PR leaves .codex/ alone the checkout's project servers are the
// team's: project_mcp "off" (codex's default) turns them off like the
// user's, by name, once even when both declare one, but for those in
// mcp_allow, and also with mcp_off = false; "allow" keeps them.
func TestProjectMCPOffTurnsTheCheckoutsServersOffButTheAllowedOnes(t *testing.T) {
	user, project := []string{"browser", "docs"}, []ProjectServer{{Name: "docs"}, {Name: "sentry"}, {Name: "tracker"}}
	codex, _ := Defaults().KindSpec(KindCodex)
	if got, want := codex.ConfigOffArgs(user, nil, project, nil, false), []string{"-c", "features.apps=false", "-c", "skills.include_instructions=false", "-c", "mcp_servers.browser.enabled=false",
		"-c", "mcp_servers.docs.enabled=false", "-c", "mcp_servers.sentry.enabled=false", "-c", "mcp_servers.tracker.enabled=false"}; !slices.Equal(got, want) {
		t.Fatalf("project_mcp off by default: %q\nwant %q", got, want)
	}
	allow := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nproject_mcp = \"allow\"\n" + minimalConfig})
	ka, _ := allow.KindSpec(KindCodex)
	if got := ka.ConfigOffArgs(user, nil, project, nil, false); !slices.Equal(got, []string{"-c", "features.apps=false", "-c", "skills.include_instructions=false", "-c", "mcp_servers.browser.enabled=false", "-c", "mcp_servers.docs.enabled=false"}) {
		t.Fatalf("project_mcp allow: %q, want the user's servers only", got)
	}
	off := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nproject_mcp = \"OFF\"\nmcp_allow = [\"tracker\"]\n" + minimalConfig})
	k, _ := off.KindSpec(KindCodex)
	want := []string{"-c", "features.apps=false", "-c", "skills.include_instructions=false", "-c", "mcp_servers.browser.enabled=false", "-c", "mcp_servers.docs.enabled=false", "-c", "mcp_servers.sentry.enabled=false"}
	if got := k.ConfigOffArgs(user, nil, project, nil, false); !slices.Equal(got, want) {
		t.Fatalf("project_mcp off: %q\nwant %q", got, want)
	}
	if got := k.Argv(LaunchArgs{MCPServers: user, ProjectServers: project, Wrapper: true}); !slices.Equal(got, slices.Concat(codexToolOutput, want)) {
		t.Fatalf("argv: %q\nwant %q", got, want)
	}
	userOn := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nproject_mcp = \"off\"\nmcp_off = false\n" + minimalConfig})
	k, _ = userOn.KindSpec(KindCodex)
	want = []string{"-c", "mcp_servers.docs.enabled=false", "-c", "mcp_servers.sentry.enabled=false", "-c", "mcp_servers.tracker.enabled=false"}
	if got := k.ConfigOffArgs(user, nil, project, nil, false); !slices.Equal(got, want) {
		t.Fatalf("mcp_off false, project_mcp off: %q\nwant %q", got, want)
	}
}

// Codex 0.166 reads the tables of the session's -c flags before it merges the
// checkout's, so -c mcp_servers.<name>.enabled=false alone makes a table with
// no transport for a server only the checkout declares, and Codex stops at
// its start ("failed to load workspace requirements: invalid data"). The
// codex kind's mcp_disable_url and mcp_disable_command give such a server a
// transport of its table's kind with a fixed value, once, before
// mcp_disable. A server the user's config declares too (the agents package
// leaves its transport empty) keeps mcp_disable alone, as does a server the
// user's pass already turned off, one in mcp_allow, a kind without the
// templates (claude, a custom kind) and a transport the kind has no
// template for. None of it reads a value of the checkout's file.
func TestProjectMCPOffGivesAServerOnlyTheCheckoutDeclaresATransportOfItsKind(t *testing.T) {
	codex, _ := Defaults().KindSpec(KindCodex)
	if want := []string{"-c", `mcp_servers.{server}.url="https://mcp.invalid"`}; !slices.Equal(codex.MCPDisableURL, want) {
		t.Fatalf("codex mcp_disable_url = %q, want %q", codex.MCPDisableURL, want)
	}
	if want := []string{"-c", `mcp_servers.{server}.command="true"`}; !slices.Equal(codex.MCPDisableCommand, want) {
		t.Fatalf("codex mcp_disable_command = %q, want %q", codex.MCPDisableCommand, want)
	}
	strict := []string{"-c", "features.apps=false", "-c", "skills.include_instructions=false"}
	user := []string{"browser", "docs"}
	project := []ProjectServer{{Name: "build", Transport: MCPTransportCommand}, {Name: "docs", Transport: MCPTransportURL},
		{Name: "parked"}, {Name: "sentry", Transport: MCPTransportURL}, {Name: "socket", Transport: "websocket"}, {Name: "tracker", Transport: MCPTransportURL}}
	want := slices.Concat(strict, []string{
		"-c", "mcp_servers.browser.enabled=false", "-c", "mcp_servers.docs.enabled=false",
		"-c", `mcp_servers.build.command="true"`, "-c", "mcp_servers.build.enabled=false",
		"-c", "mcp_servers.parked.enabled=false",
		"-c", `mcp_servers.sentry.url="https://mcp.invalid"`, "-c", "mcp_servers.sentry.enabled=false",
		"-c", "mcp_servers.socket.enabled=false",
		"-c", `mcp_servers.tracker.url="https://mcp.invalid"`, "-c", "mcp_servers.tracker.enabled=false",
	})
	if got := codex.ConfigOffArgs(user, nil, project, nil, false); !slices.Equal(got, want) {
		t.Fatalf("args = %q\nwant %q", got, want)
	}
	if got := codex.Argv(LaunchArgs{MCPServers: user, ProjectServers: project, Wrapper: true}); !slices.Equal(got, slices.Concat(codexToolOutput, want)) {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}

	// mcp_allow keeps a server on, transport or not.
	allow := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nmcp_allow = [\"build\", \"tracker\"]\n" + minimalConfig})
	k, _ := allow.KindSpec(KindCodex)
	if got, want := k.ConfigOffArgs(user, nil, project, nil, false), slices.Concat(strict, []string{"-c", "mcp_servers.browser.enabled=false", "-c", "mcp_servers.docs.enabled=false",
		"-c", "mcp_servers.parked.enabled=false", "-c", `mcp_servers.sentry.url="https://mcp.invalid"`, "-c", "mcp_servers.sentry.enabled=false",
		"-c", "mcp_servers.socket.enabled=false"}); !slices.Equal(got, want) {
		t.Fatalf("mcp_allow: %q\nwant %q", got, want)
	}

	// A kind without the templates turns a server off by name alone, as before.
	bare := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nmcp_disable_url = []\nmcp_disable_command = []\n" + minimalConfig})
	k, _ = bare.KindSpec(KindCodex)
	if len(k.MCPDisableURL) != 0 || len(k.MCPDisableCommand) != 0 {
		t.Fatalf("[] left the templates: %q %q", k.MCPDisableURL, k.MCPDisableCommand)
	}
	if got, want := k.ConfigOffArgs(nil, nil, project, nil, false), slices.Concat(strict, []string{"-c", "mcp_servers.build.enabled=false", "-c", "mcp_servers.docs.enabled=false",
		"-c", "mcp_servers.parked.enabled=false", "-c", "mcp_servers.sentry.enabled=false", "-c", "mcp_servers.socket.enabled=false", "-c", "mcp_servers.tracker.enabled=false"}); !slices.Equal(got, want) {
		t.Fatalf("no templates: %q\nwant %q", got, want)
	}
	claude, _ := Defaults().KindSpec(KindClaude)
	if claude.MCPDisableURL != nil || claude.MCPDisableCommand != nil {
		t.Fatalf("claude has transport templates: %q %q", claude.MCPDisableURL, claude.MCPDisableCommand)
	}
	if got := claude.ConfigOffArgs(nil, nil, project, nil, false); !slices.Equal(got, claude.MCPStrict) {
		t.Fatalf("claude: %q, want mcp_strict alone", got)
	}
}

// Both templates carry {server}, like mcp_disable, or the key is refused.
func TestTransportTemplatesNeedTheirServerPlaceholder(t *testing.T) {
	for _, key := range []string{"mcp_disable_url", "mcp_disable_command"} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[kinds.codex]\n" + key + " = [\"-c\", \"mcp_servers.x.url=\\\"u\\\"\"]\n" + minimalConfig})
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v, want one naming the key", key, err)
		}
	}
}

// A PR that changes only the AGENTS.md Codex loads keeps the team's
// .codex/: the session gets project_docs_off (Codex reads no project
// AGENTS.md at project_doc_max_bytes=0) where project_untrust would go,
// after the servers project_mcp "off" turns off, and the checkout stays
// trusted. Claude has none (--setting-sources user covers its CLAUDE.md),
// and project_docs_off = [] leaves the flag out.
func TestCodexKeepsOnlyTheInstructionFilesOutWithProjectDocsOff(t *testing.T) {
	codex, _ := Defaults().KindSpec(KindCodex)
	if !slices.Equal(codex.ProjectDocsOff, []string{"-c", "project_doc_max_bytes=0"}) {
		t.Fatalf("codex project_docs_off = %q", codex.ProjectDocsOff)
	}
	got := codex.Argv(LaunchArgs{MCPServers: []string{"docs"}, DocsOff: true, Wrapper: true})
	if want := []string{"-c", "tool_output_token_limit=65536", "-c", "features.apps=false", "-c", "skills.include_instructions=false", "-c", "mcp_servers.docs.enabled=false", "-c", "project_doc_max_bytes=0"}; !slices.Equal(got, want) {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}
	off := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nproject_mcp = \"off\"\n" + minimalConfig})
	k, _ := off.KindSpec(KindCodex)
	if got, want := k.ConfigOffArgs(nil, nil, []ProjectServer{{Name: "sentry"}}, nil, true), []string{"-c", "features.apps=false", "-c", "skills.include_instructions=false", "-c", "mcp_servers.sentry.enabled=false", "-c", "project_doc_max_bytes=0"}; !slices.Equal(got, want) {
		t.Fatalf("project_mcp off: %q\nwant %q", got, want)
	}
	if claude, _ := Defaults().KindSpec(KindClaude); claude.ProjectDocsOff != nil {
		t.Fatalf("claude project_docs_off = %q", claude.ProjectDocsOff)
	}
	none := mustLoad(t, map[string]string{"config.toml": "[kinds.codex]\nproject_docs_off = []\n" + minimalConfig})
	k, _ = none.KindSpec(KindCodex)
	if got := k.ConfigOffArgs(nil, nil, nil, nil, true); !slices.Equal(got, []string{"-c", "features.apps=false", "-c", "skills.include_instructions=false"}) {
		t.Fatalf("project_docs_off = []: %q", got)
	}
}

// project_mcp takes allow or off; project_untrust needs no {projects}
// (claude's names no path), see TestClaudeLoadsOnlyTheUserSettingsForAChangedCheckout.
func TestProjectKeysAreValidated(t *testing.T) {
	for key, cfg := range map[string]string{
		"project_mcp": "[kinds.codex]\nproject_mcp = \"sometimes\"\n",
	} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": cfg + minimalConfig})
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v, want one naming the key", key, err)
		}
	}
}
