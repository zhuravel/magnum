package agents

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// codexMCPConfig declares two MCP servers a session would load (one with
// an env table), one already off and one whose name is no TOML bare key.
const codexMCPConfig = `model = "gpt-test"

[mcp_servers.docs]
command = "docs-mcp"

[mcp_servers.browser]
url = "http://127.0.0.1:9/mcp"

[mcp_servers.browser.env]
BROWSER_PROFILE = "review"

[mcp_servers.parked]
command = "parked-mcp"
enabled = false

[mcp_servers."my.server"]
command = "dotted-mcp"
`

var mcpOffArgs = []string{"-c", "features.apps=false", "-c", "skills.include_instructions=false", "-c", "mcp_servers.browser.enabled=false", "-c", "mcp_servers.docs.enabled=false"}

// Magnum's Codex sessions loaded every MCP server of the operator's Codex
// config. A judge launched or resumed passes -c mcp_servers.<name>.enabled=false
// for each server its Codex config declares and does not turn off itself,
// after its effort and subagent args; a name that is no TOML bare key is
// skipped with a log line.
func TestJudgeStartsWithoutTheOperatorsMCPServers(t *testing.T) {
	for _, resume := range []string{"", "01a0-uuid"} {
		e := newEnv(t)
		writeFile(t, e.codexConfig, codexMCPConfig, 0o600)
		ws := e.workspace()
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], resume); err != nil {
			t.Fatal(err)
		}
		want := slices.Concat([]string{"-c", "model_reasoning_effort=xhigh", "-c", "agents.max_concurrent_threads_per_session=2"}, mcpOffArgs)
		if resume != "" {
			want = slices.Concat([]string{"resume", resume}, want)
		}
		if got := e.h.starts[0].Args; !slices.Equal(got, want) {
			t.Fatalf("resume %q: args = %q\nwant %q", resume, got, want)
		}
		if logs := mcpLogs(e); len(logs) != 1 || !strings.Contains(logs[0], `"my.server"`) || strings.Contains(logs[0], e.codexConfig) {
			t.Fatalf("resume %q: logs = %q, want the skipped name and no path", resume, logs)
		}
	}
}

// mcpLogs are the log lines about MCP servers (EnsureTrust logs the
// trust it adds with the config's path).
func mcpLogs(e *env) []string {
	var out []string
	for _, l := range e.logs.all() {
		if strings.Contains(l, "MCP server") {
			out = append(out, l)
		}
	}
	return out
}

func TestJudgeKeepsTheMCPServersAllowedOrWhenMCPOffIsFalse(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(k *config.Kind)
		want []string
	}{
		"allowed":       {func(k *config.Kind) { k.MCPAllow = []string{"docs"} }, []string{"-c", "features.apps=false", "-c", "skills.include_instructions=false", "-c", "mcp_servers.browser.enabled=false"}},
		"all allowed":   {func(k *config.Kind) { k.MCPAllow = []string{"docs", "browser"} }, []string{"-c", "features.apps=false", "-c", "skills.include_instructions=false"}},
		"mcp_off false": {func(k *config.Kind) { k.MCPOff = false }, nil},
	} {
		e := newEnv(t)
		writeFile(t, e.codexConfig, codexMCPConfig, 0o600)
		e.setKind(KindCodex, tc.edit)
		ws := e.workspace()
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
			t.Fatal(err)
		}
		want := slices.Concat([]string{"-c", "model_reasoning_effort=xhigh", "-c", "agents.max_concurrent_threads_per_session=2"}, tc.want)
		if got := e.h.starts[0].Args; !slices.Equal(got, want) {
			t.Errorf("%s: args = %q\nwant %q", name, got, want)
		}
	}
}

// The servers are those of the Codex config the session reads: the
// config.toml in the CODEX_HOME the pane env gives the kind, which may
// not exist (no servers, nothing logged).
func TestJudgeReadsTheMCPServersOfItsPaneEnvsCodexHome(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.codexConfig, codexMCPConfig, 0o600)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "config.toml"), "[mcp_servers.tracker]\ncommand = \"tracker-mcp\"\n", 0o600)
	e.setKind(KindCodex, func(k *config.Kind) { k.Env = map[string]string{"CODEX_HOME": home} })
	if got := e.m.mcpServers(e.spec(RoleJudge)); !slices.Equal(got, []string{"tracker"}) {
		t.Fatalf("pane env CODEX_HOME: servers = %q, want tracker", got)
	}
	e.setKind(KindCodex, func(k *config.Kind) { k.Env = map[string]string{"CODEX_HOME": filepath.Join(home, "missing")} })
	if got := e.m.mcpServers(e.spec(RoleJudge)); got != nil {
		t.Fatalf("missing config: servers = %q, want none", got)
	}
	if logs := e.logs.all(); len(logs) != 0 {
		t.Fatalf("a missing config logged %q", logs)
	}
	e.setKind(KindCodex, func(k *config.Kind) { k.Env = nil })
	writeFile(t, e.codexConfig, "[mcp_servers.docs\n", 0o600)
	if got := e.m.mcpServers(e.spec(RoleJudge)); got != nil {
		t.Fatalf("unreadable config: servers = %q, want none", got)
	}
	if logs := mcpLogs(e); len(logs) != 1 || strings.Contains(logs[0], e.codexConfig) {
		t.Fatalf("unreadable config: logs = %q, want one line without the path", logs)
	}
}

// Server names reach argv only as TOML bare keys (letters, digits, _ and
// -), which Codex's -c key path carries as written.
func TestMCPServerNamesAreBareKeys(t *testing.T) {
	names, skipped, err := declaredMCPServers(`
[mcp_servers.docs]
[mcp_servers.browser-2]
[mcp_servers.x_Y9]
[mcp_servers."my.server"]
[mcp_servers."a b"]
[mcp_servers."ü"]
[mcp_servers."$(touch x)"]
[mcp_servers.off]
enabled = false
`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"browser-2", "docs", "x_Y9"}) {
		t.Fatalf("names = %q", names)
	}
	if !slices.Equal(skipped, []string{"$(touch x)", "a b", "my.server", "ü"}) {
		t.Fatalf("skipped = %q", skipped)
	}
	if names, skipped, err := declaredMCPServers("model = \"x\"\n"); names != nil || skipped != nil || err != nil {
		t.Fatalf("no servers: %q %q %v", names, skipped, err)
	}
}

// Codex's built-in apps connector (codex_apps, ChatGPT's connectors) is
// no [mcp_servers] table, so turning servers off by name misses it: a
// codex review with every server off still searched the web through it.
// The codex kind's mcp_off also turns the apps feature off, once, on a
// launch, a resume and codex-review's line alike, servers or none;
// mcp_off = false leaves it on.
func TestCodexRunsWithoutTheAppsConnector(t *testing.T) {
	appsOff := []string{"-c", "features.apps=false", "-c", "skills.include_instructions=false"}
	for _, resume := range []string{"", "01a0-uuid"} {
		e := newEnv(t)
		ws := e.workspace()
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], resume); err != nil {
			t.Fatal(err)
		}
		want := slices.Concat([]string{"-c", "model_reasoning_effort=xhigh", "-c", "agents.max_concurrent_threads_per_session=2"}, appsOff)
		if resume != "" {
			want = slices.Concat([]string{"resume", resume}, want)
		}
		if got := e.h.starts[0].Args; !slices.Equal(got, want) {
			t.Fatalf("resume %q: args = %q\nwant %q", resume, got, want)
		}
	}
	e := newEnv(t)
	d := ShellData{BaseRef: "origin/master", ReportPath: "/r/codex.md", Marker: DoneMarker("r-1")}
	role := e.spec(RoleCodexReview)
	if got, err := e.m.ShellLine(e.ctx, e.pr.ID, role, d); err != nil || !strings.Contains(got, "review -c model_reasoning_effort=high -c features.apps=false -c skills.include_instructions=false --base") {
		t.Fatalf("codex-review: line = %q, %v", got, err)
	}
	e.setKind(KindCodex, func(k *config.Kind) { k.MCPOff = false })
	if got, err := e.m.ShellLine(e.ctx, e.pr.ID, role, d); err != nil || strings.Contains(got, "features.apps") {
		t.Fatalf("mcp_off false: line = %q, %v", got, err)
	}
}

// codex review is a Codex session too: codex-review's line (its command
// and the full-line codex-review.sh) passes the same -c per server, after
// its effort and the apps connector's, none when they are allowed, and
// neither when mcp_off is false.
func TestCodexReviewRunsWithoutTheOperatorsMCPServers(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.codexConfig, codexMCPConfig, 0o600)
	d := ShellData{BaseRef: "origin/master", ReportPath: "/r/codex.md", Marker: DoneMarker("r-1")}
	role := e.spec(RoleCodexReview)
	sh := role
	sh.Command, sh.Prompt = "", "codex-review.sh"
	want := "command codex review -c model_reasoning_effort=high " + strings.Join(mcpOffArgs, " ") + " --base origin/master; } |"
	for name, r := range map[string]config.Role{"command": role, "codex-review.sh": sh} {
		got, err := e.m.ShellLine(e.ctx, e.pr.ID, r, d)
		if err != nil || !strings.Contains(got, want) {
			t.Fatalf("%s: line = %q, %v\nwant it to contain %q", name, got, err, want)
		}
	}
	e.setKind(KindCodex, func(k *config.Kind) { k.MCPAllow = []string{"browser", "docs"} })
	if got, err := e.m.ShellLine(e.ctx, e.pr.ID, role, d); err != nil || !strings.Contains(got, "review -c model_reasoning_effort=high -c features.apps=false -c skills.include_instructions=false --base") {
		t.Fatalf("all allowed: line = %q, %v", got, err)
	}
	e.setKind(KindCodex, func(k *config.Kind) { k.MCPAllow, k.MCPOff = nil, false })
	if got, err := e.m.ShellLine(e.ctx, e.pr.ID, role, d); err != nil || !strings.Contains(got, "review -c model_reasoning_effort=high --base") {
		t.Fatalf("mcp_off false: line = %q, %v", got, err)
	}
}
