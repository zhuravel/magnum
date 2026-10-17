package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
)

// codexHookHome writes a Codex home in a temp dir: hooks.json with one
// SessionStart group per command (one hook each) and, unless state is "-",
// config.toml with state, in which {hooks} stands for the hooks.json path
// (the key Codex gives each hook's [hooks.state] entry starts with it).
func codexHookHome(t *testing.T, commands []string, state string) string {
	t.Helper()
	home := t.TempDir()
	var groups []map[string]any
	for _, c := range commands {
		groups = append(groups, map[string]any{"matcher": "", "hooks": []map[string]any{{"type": "command", "command": c}}})
	}
	raw, err := json.Marshal(map[string]any{"hooks": map[string]any{
		"SessionStart": groups,
		"Stop":         []map[string]any{{"hooks": []map[string]any{{"type": "command", "command": "bash herdr-agent-state.sh stop"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(home, "hooks.json")
	if err := os.WriteFile(hooks, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if state != "-" {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(strings.ReplaceAll(state, "{hooks}", hooks)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// herdr learns a Codex session's id from the SessionStart hook it installs in
// CODEX_HOME's hooks.json; a [hooks.state] entry with enabled = false turns it
// off, and then no judge resumes and no helper's refusal is seen. Doctor
// warns, naming the entry and what goes blind; an enabled hook or one without
// an entry passes, a hooks.json without herdr's hook warns that it is
// missing, and a Codex home without hooks.json is not judged. It changes
// nothing.
func TestDoctorWarnsWhenHerdrsCodexSessionHookIsDisabled(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	const herdrHook = "bash '/Users/x/.codex/herdr-agent-state.sh' session"
	const otherHook = "'/usr/local/bin/notify-hook' codex"
	check := func(home string) doctorCheck {
		t.Helper()
		ag.codexHome = home
		c, ok := doctorByName(doctorRun(context.Background(), d))["codex session hook"]
		if !ok {
			t.Fatalf("no codex session hook check for %s", home)
		}
		return c
	}

	disabled := `model = "gpt-6.1-sol"
[hooks.state."{hooks}:session_start:0:0"]
enabled = true
trusted_hash = "sha256:aaaa"
[hooks.state."{hooks}:session_start:1:0"]
trusted_hash = "sha256:47bf"
enabled = false
`
	home := codexHookHome(t, []string{otherHook, herdrHook}, disabled)
	before, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(home, "hooks.json") + ":session_start:1:0"
	c := check(home)
	if c.Status != doctorWarn || !strings.Contains(c.Detail, key) || !strings.Contains(c.Detail, "enabled = false") ||
		!strings.Contains(c.Detail, agents.SessionIDBlind) || !strings.Contains(c.Detail, "codex-judge") || !strings.Contains(c.Fix, "enabled = true") {
		t.Fatalf("disabled hook = %s %q fix %q, want WARN naming %s, enabled = false and what goes blind", c.Status, c.Detail, c.Fix, key)
	}
	if after, _ := os.ReadFile(filepath.Join(home, "config.toml")); string(after) != string(before) {
		t.Fatalf("doctor changed config.toml:\n%s", after)
	}

	for name, state := range map[string]string{
		"enabled": `[hooks.state."{hooks}:session_start:1:0"]
enabled = true
trusted_hash = "sha256:47bf"
`,
		"no entry": `[hooks.state."{hooks}:session_start:0:0"]
enabled = false
`,
		"no table":       `model = "gpt-6.1-sol"`,
		"no config.toml": "-",
	} {
		if c := check(codexHookHome(t, []string{otherHook, herdrHook}, state)); c.Status != doctorPass || !strings.Contains(c.Detail, "herdr-agent-state.sh") {
			t.Errorf("%s = %s %q fix %q, want PASS", name, c.Status, c.Detail, c.Fix)
		}
	}

	c = check(codexHookHome(t, []string{otherHook}, disabled))
	if c.Status != doctorWarn || !strings.Contains(c.Detail, "missing") || !strings.Contains(c.Detail, "herdr-agent-state.sh") ||
		!strings.Contains(c.Detail, agents.SessionIDBlind) || c.Fix == "" {
		t.Errorf("no herdr hook = %s %q fix %q, want WARN that it is missing", c.Status, c.Detail, c.Fix)
	}

	if c := check(t.TempDir()); c.Status != doctorSkip {
		t.Errorf("no hooks.json = %s %q, want SKIP (herdr not installed is another check)", c.Status, c.Detail)
	}
}
