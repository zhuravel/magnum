package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// After `magnum hooks install` rewrote magnum's Codex entry in its place,
// the entry's old trusted_hash is still in config.toml, and Codex will show
// the hooks review: the hold, which reads agents.ReadCodexHooksTrust, holds
// new Codex sessions until the operator's Trust all gives the place a new
// hash. (The engine is unchanged: the reader counts the recorded stale hash
// as no trust.)
func TestARewrittenCodexEntrysOldTrustHoldsNewCodexSessions(t *testing.T) {
	storetest.Serial(t) // swaps the stale trust records' directory
	records := filepath.Join(t.TempDir(), "codex-stale-trust")
	agents.SetStaleCodexTrustDir(records)
	t.Cleanup(func() { agents.SetStaleCodexTrustDir("") })

	home := t.TempDir()
	file := filepath.Join(home, "hooks.json")
	hooks := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"bash '/Users/x/.codex/herdr-agent-state.sh' session","timeout":10}]}],` +
		`"Stop":[{"hooks":[{"type":"command","command":"sh '/c/magnum-agent-hook.sh' codex stop","async":true,"timeout":5}]}]}}`
	trust := func(stop string) {
		t.Helper()
		state := "[hooks.state.\"" + file + ":session_start:0:0\"]\ntrusted_hash = \"sha256:aa\"\n" +
			"[hooks.state.\"" + file + ":stop:0:0\"]\ntrusted_hash = \"" + stop + "\"\n"
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, []byte(hooks), 0o600); err != nil {
		t.Fatal(err)
	}
	trust("sha256:old")
	if err := agents.RecordStaleCodexTrust(home, []string{"stop:0:0"}); err != nil {
		t.Fatal(err)
	}
	h := hooksHarness(t, home)
	h.advance(5 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 0)
	const stale = "1 hook in $CODEX_HOME/hooks.json has a trust entry Codex no longer accepts " +
		"(if `codex` asks nothing, run `magnum hooks install codex --forget-stale`)"
	wantHooksHold(t, h, stale)
	if toasts := toastsWith(h, "Codex hooks need review"); len(toasts) != 1 || !strings.Contains(toasts[0], "magnum hooks install codex --forget-stale") {
		t.Fatalf("toasts = %q, want the way out named", toasts)
	}

	trust("sha256:new") // the operator ran codex and chose Trust all
	h.advance(time.Minute)
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatalf("a new trust kept the pause %+v", p)
	}
	reqWantRounds(t, h, 1)

	// A rewrite back to the hook Codex already trusts: Codex asks nothing
	// and writes no new hash, and `magnum hooks install codex
	// --forget-stale` ends the hold.
	if err := agents.RecordStaleCodexTrust(home, []string{"stop:0:0"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(hooks+"\n"), 0o600); err != nil { // the rewrite: a new hooks.json
		t.Fatal(err)
	}
	h.advance(time.Minute)
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); !ok || p.Reason != HooksReviewPauseReason {
		t.Fatalf("the stuck record: pause %+v, %v; want the hold", p, ok)
	}
	if n, err := agents.ForgetStaleCodexTrust(home); err != nil || n != 1 {
		t.Fatalf("forget = %d %v", n, err)
	}
	h.advance(time.Minute)
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatalf("--forget-stale kept the pause %+v", p)
	}
}
