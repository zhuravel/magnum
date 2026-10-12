package agents

import (
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// The login stamp follows the kind's login file (codex: auth.json under its
// home, the pane env's CODEX_HOME first): absent, then a value that changes
// when a login rewrites the file; claude has no login file, nor has a codex
// kind whose login_file is cleared.
func TestLoginStampFollowsTheCodexAuthFile(t *testing.T) {
	e := newEnv(t)
	home := filepath.Dir(e.codexConfig)
	if got := e.m.LoginStamp(KindCodex); got != "absent" {
		t.Fatalf("no auth.json: stamp = %q, want absent", got)
	}
	writeFile(t, filepath.Join(home, "auth.json"), `{"tokens":1}`, 0o600)
	first := e.m.LoginStamp(KindCodex)
	if first == "" || first == "absent" {
		t.Fatalf("stamp = %q", first)
	}
	writeFile(t, filepath.Join(home, "auth.json"), `{"tokens":12}`, 0o600)
	if got := e.m.LoginStamp(KindCodex); got == first || got == "" || got == "absent" {
		t.Fatalf("rewritten file: stamp = %q, first %q", got, first)
	}
	if got := e.m.LoginStamp(KindClaude); got != "" {
		t.Fatalf("claude: stamp = %q, want none", got)
	}
	other := t.TempDir()
	e.setKind(KindCodex, func(k *config.Kind) { k.Env = map[string]string{"CODEX_HOME": other} })
	if got := e.m.LoginStamp(KindCodex); got != "absent" {
		t.Fatalf("pane env CODEX_HOME: stamp = %q, want absent", got)
	}
	e.setKind(KindCodex, func(k *config.Kind) { k.Env, k.LoginFile = nil, "" })
	if got := e.m.LoginStamp(KindCodex); got != "" {
		t.Fatalf("no login_file: stamp = %q, want none", got)
	}
}
