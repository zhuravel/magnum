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

// writeHookHome writes a Codex home's hooks.json from its SessionStart
// groups (raw JSON) and config.toml from state, in which {hooks} stands for
// hooksPath (the hooks.json path the [hooks.state] keys start with).
func writeHookHome(t *testing.T, home, groups, state, hooksPath string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "hooks.json"), []byte(`{"hooks":{"SessionStart":[`+groups+`]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(strings.ReplaceAll(state, "{hooks}", hooksPath)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Codex keys a hook's trust by its place (session_start:<group>:<index>)
// and waits up to 600 s for a hook without a timeout. ReadCodexSessionHook
// says where herdr's hook is, which hooks run before it (a disabled one does
// not), whether its place has a trust entry (keyed by the hooks.json path as
// written or with its symlinks resolved), and the herdr integration version
// its script's header names; Problems names each failed check without the
// files' paths.
func TestReadCodexSessionHookFindsThePlaceTheHooksBeforeItAndTheTrust(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	script := filepath.Join(target, HerdrSessionHook)
	groups := `{"hooks":[{"type":"command","command":"'/Applications/Bartender 6.app/Contents/Helpers/Bartender NotchBar' codex-hook"},
{"type":"command","command":"/usr/bin/quiet-hook","timeout":5}]},
{"hooks":[{"type":"command","command":"OFF=1 /usr/bin/off-hook"}]},
{"matcher":"startup","hooks":[{"type":"command","command":"FOO=1 /opt/homebrew/bin/moshi-hook codex-hook","timeout":30},
{"type":"command","command":"bash '` + script + `' session","timeout":10}]}`
	state := `[hooks.state."{hooks}:session_start:1:0"]
enabled = false
[hooks.state."{hooks}:session_start:2:1"]
trusted_hash = "sha256:47bf"
`
	// The trust entries name the resolved path: the Codex home is a symlink.
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	writeHookHome(t, target, groups, state, filepath.Join(resolved, "hooks.json"))
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\n# managed by herdr\nHERDR_INTEGRATION_VERSION=\"10\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.Symlink(target, home); err != nil {
		t.Fatal(err)
	}
	h, ok, err := ReadCodexSessionHook(home)
	if err != nil || !ok || !h.Found {
		t.Fatalf("read = %+v %v %v", h, ok, err)
	}
	if h.Group != 2 || h.Index != 1 || h.Place != "session_start:2:1" || !h.Trusted || h.Version != "10" || len(h.Disabled) != 0 {
		t.Fatalf("hook = %+v, want trusted at session_start:2:1, version 10", h)
	}
	want := []CodexHook{{Name: "Bartender NotchBar"}, {Name: "quiet-hook", Timeout: 5}, {Name: "moshi-hook", Timeout: 30}}
	if len(h.Before) != len(want) {
		t.Fatalf("before = %+v, want %+v (the disabled hook does not run)", h.Before, want)
	}
	for i := range want {
		if h.Before[i] != want[i] {
			t.Errorf("before[%d] = %+v, want %+v", i, h.Before[i], want[i])
		}
	}
	problems := h.Problems("$CODEX_HOME/hooks.json", "$CODEX_HOME/config.toml")
	if len(problems) != 1 || problems[0].Check != CodexHookPlace || problems[0].Text != "herdr's Codex session hook runs after 1 SessionStart hook without a timeout (Bartender NotchBar) "+
		"and 1 with a timeout over 10 s (moshi-hook, 30 s) in $CODEX_HOME/hooks.json" {
		t.Fatalf("problems = %q", problems)
	}

	writeHookHome(t, target, `{"hooks":[{"type":"command","command":"bash '`+script+`' session","timeout":10}]}`, "", filepath.Join(resolved, "hooks.json"))
	h, _, _ = ReadCodexSessionHook(home)
	if problems := h.Problems("$CODEX_HOME/hooks.json", "$CODEX_HOME/config.toml"); len(problems) != 1 ||
		problems[0].Text != "herdr's Codex session hook has no trust entry for its place session_start:0:0 in $CODEX_HOME/config.toml" {
		t.Fatalf("problems of an untrusted first hook = %q", problems)
	}
}

// A Codex session that ends its first turn without an id names the check
// herdr's hook fails, as doctor does, without the Codex home's path.
func TestASessionWithoutAnIDNamesTheHooksFailedPlace(t *testing.T) {
	e := newEnv(t)
	home := filepath.Dir(e.codexConfig)
	hooks := filepath.Join(home, "hooks.json")
	writeHookHome(t, home, `{"hooks":[{"type":"command","command":"/opt/homebrew/bin/moshi-hook codex-hook"}]},
{"hooks":[{"type":"command","command":"bash '/Users/x/.codex/herdr-agent-state.sh' session","timeout":10}]}`,
		"[hooks.state.\"{hooks}:session_start:0:0\"]\ntrusted_hash = \"sha256:aa\"\n", hooks)
	e.started()
	const judge = "mg-11920-codex-judge-5d01cf"
	e.clock.Add(3 * time.Minute)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
		t.Fatal(err)
	}
	for _, status := range []herdr.Status{herdr.StatusWorking, herdr.StatusIdle, herdr.StatusIdle} {
		e.h.setAgentStatus(judge, status)
		e.clock.Add(30 * time.Second)
		if _, err := e.m.Observe(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	evs := e.sessionIDEvents(KindCodex)
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one", evs)
	}
	for _, want := range []string{"runs after 1 SessionStart hook without a timeout (moshi-hook) in $CODEX_HOME/hooks.json",
		"; it has no trust entry for its place session_start:1:0", SessionIDBlind} {
		if !strings.Contains(evs[0].Message, want) {
			t.Errorf("message %q lacks %q", evs[0].Message, want)
		}
	}
	if strings.Contains(evs[0].Message, home) {
		t.Errorf("message %q names the Codex home's path", evs[0].Message)
	}
}
