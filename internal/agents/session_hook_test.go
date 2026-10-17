package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// writeCodexHooks writes a Codex home's hooks.json, herdr's SessionStart
// hook its first group, and config.toml with that hook's [hooks.state]
// entry set enabled = enabled.
func writeCodexHooks(t *testing.T, home string, enabled bool) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(home, "hooks.json")
	if err := os.WriteFile(hooks, []byte(`{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"bash '/Users/x/.codex/herdr-agent-state.sh' session"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	on := "false"
	if enabled {
		on = "true"
	}
	state := "[hooks.state.\"" + hooks + ":session_start:0:0\"]\ntrusted_hash = \"sha256:47bf\"\nenabled = " + on + "\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sessionIDEvents are the agents.session_id_missing events of kind.
func (e *env) sessionIDEvents(kind string) []store.Event {
	e.t.Helper()
	evs, err := e.st.EventsBySubject(e.ctx, "tool:"+kind, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range evs {
		if ev.Kind == EventSessionIDMissing {
			out = append(out, ev)
		}
	}
	return out
}

// herdr learns a Codex session's id from its SessionStart hook; with the hook
// off no session has one, judges never resume and a helper's refusal is not
// seen. When a session's first turn (magnum prompted it, the observer saw it
// working, then idle) ends without an id, the daemon says so once per kind:
// later turns say nothing, a daemon start says it again, and a session herdr
// reports an id for says nothing.
func TestACodexAgentWithoutASessionIDWarnsOncePerStart(t *testing.T) {
	e := newEnv(t)
	home := filepath.Dir(e.codexConfig)
	writeCodexHooks(t, home, false)
	e.started()
	const judge = "mg-11920-codex-judge-5d01cf"
	turn := func(m *Manager) {
		t.Helper()
		e.clock.Add(3 * time.Minute)
		if _, err := m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
			t.Fatal(err)
		}
		for _, status := range []herdr.Status{herdr.StatusWorking, herdr.StatusIdle, herdr.StatusIdle} {
			e.h.setAgentStatus(judge, status)
			e.clock.Add(30 * time.Second)
			if _, err := m.Observe(e.ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	turn(e.m)
	evs := e.sessionIDEvents(KindCodex)
	if len(evs) != 1 {
		t.Fatalf("events after the first turn = %+v, want one %s", evs, EventSessionIDMissing)
	}
	ev := evs[0]
	for _, want := range []string{"codex-judge", "herdr-agent-state.sh", "session_start:0:0", "enabled = false", SessionIDBlind} {
		if !strings.Contains(ev.Message, want) {
			t.Errorf("message %q lacks %q", ev.Message, want)
		}
	}
	if ev.Level != "warn" || strings.Contains(ev.Message, home) {
		t.Errorf("event = %s %q, want a warning that names no Codex home path (a pane env value)", ev.Level, ev.Message)
	}

	turn(e.m)
	if evs := e.sessionIDEvents(KindCodex); len(evs) != 1 {
		t.Fatalf("events after the second turn = %d, want still one", len(evs))
	}
	if evs := e.sessionIDEvents(KindClaude); len(evs) != 0 {
		t.Fatalf("claude events = %+v, want none (its session had no turn)", evs)
	}

	restarted := New(e.m.d)
	restarted.sleep = e.m.sleep
	turn(restarted)
	if evs := e.sessionIDEvents(KindCodex); len(evs) != 2 {
		t.Fatalf("events after a daemon start's turn = %d, want two", len(evs))
	}

	withID := New(e.m.d)
	withID.sleep = e.m.sleep
	e.h.setAgentSession(judge, codexSID)
	turn(withID)
	if evs := e.sessionIDEvents(KindCodex); len(evs) != 2 {
		t.Fatalf("events after a turn with a session id = %d, want still two", len(evs))
	}
}
