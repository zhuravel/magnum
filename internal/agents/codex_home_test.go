package agents

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// judgeServers and judgePlugins are what a launch of the judge turns off of
// the Codex home (codexOff reads the config once for both).
func judgeServers(e *env) []string {
	servers, _ := e.m.codexOff(e.spec(RoleJudge))
	return servers
}

func judgePlugins(e *env) []string {
	_, plugins := e.m.codexOff(e.spec(RoleJudge))
	return plugins
}

// The Codex home is looked up one way: the login stamp (per kind), the MCP
// servers and the plugins a launch turns off all read under the CODEX_HOME
// of the kind's env, and under the directory of the Codex config when the
// env sets none.
func TestLoginStampAndMCPReadTheSameCodexHome(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "auth.json"), `{"tokens":1}`, 0o600)
	writeFile(t, filepath.Join(home, "config.toml"), "[mcp_servers.tracker]\ncommand = \"tracker-mcp\"\n", 0o600)
	if err := os.MkdirAll(filepath.Join(home, "plugins", "cache", "acme", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.setKind(KindCodex, func(k *config.Kind) { k.Env = map[string]string{"CODEX_HOME": home} })
	if got := e.m.LoginStamp(KindCodex); got == "" || got == "absent" {
		t.Fatalf("kind env CODEX_HOME: stamp = %q, want the auth.json under it", got)
	}
	if got := judgeServers(e); !slices.Equal(got, []string{"tracker"}) {
		t.Fatalf("kind env CODEX_HOME: servers = %q", got)
	}
	if got := judgePlugins(e); !slices.Equal(got, []string{"docs@acme"}) {
		t.Fatalf("kind env CODEX_HOME: plugins = %q", got)
	}
	k, _ := e.m.kindSpec(KindCodex)
	if h, c := e.m.codexHomeFor(k.Env); h != home || c != filepath.Join(home, "config.toml") {
		t.Fatalf("codexHomeFor = %q, %q", h, c)
	}

	// Without CODEX_HOME in the env, all three read the Codex config's directory.
	e.setKind(KindCodex, func(k *config.Kind) { k.Env = nil })
	other := filepath.Dir(e.codexConfig)
	writeFile(t, e.codexConfig, "[mcp_servers.docs]\ncommand = \"docs-mcp\"\n", 0o600)
	if got := e.m.LoginStamp(KindCodex); got != "absent" {
		t.Fatalf("no env: stamp = %q, want absent (no auth.json beside the config)", got)
	}
	writeFile(t, filepath.Join(other, "auth.json"), `{"tokens":2}`, 0o600)
	if got := e.m.LoginStamp(KindCodex); got == "" || got == "absent" {
		t.Fatalf("no env: stamp = %q, want the auth.json beside the config", got)
	}
	if got := judgeServers(e); !slices.Equal(got, []string{"docs"}) {
		t.Fatalf("no env: servers = %q", got)
	}
	if got := judgePlugins(e); got != nil {
		t.Fatalf("no env: plugins = %q, want none (the first home's cache is not read)", got)
	}
	if h, c := e.m.codexHomeFor(nil); h != other || c != e.codexConfig {
		t.Fatalf("codexHomeFor(nil) = %q, %q", h, c)
	}
}

// A Codex config that cannot be decoded is one fact of one launch: it is
// logged once for the servers and the plugins together (it was a line per
// reader), and both stay on.
func TestACodexConfigThatCannotBeReadIsLoggedOncePerLaunch(t *testing.T) {
	e := newEnv(t)
	home := filepath.Dir(e.codexConfig)
	if err := os.MkdirAll(filepath.Join(home, "plugins", "cache", "acme", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.codexConfig, "[mcp_servers.docs\n", 0o600)
	ws := e.workspace()
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, judgeLaunchArgs) {
		t.Fatalf("args = %q, want no server or plugin turned off", got)
	}
	var logs []string
	for _, l := range e.logs.all() {
		if strings.Contains(l, "cannot read the Codex config") {
			logs = append(logs, l)
		}
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "MCP servers and plugins stay on") || strings.Contains(logs[0], home) {
		t.Fatalf("logs = %q, want one line for both, without the path", logs)
	}
}
