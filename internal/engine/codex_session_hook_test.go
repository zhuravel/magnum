package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// codexSessionAt adds a closed Codex judge session to the PR, started at
// started and prompted a minute later, with session id sid ("" = none).
func (h *harness) codexSessionAt(prID int64, started time.Time, sid string) store.Session {
	h.t.Helper()
	s := store.Session{PRID: prID, Role: "codex-judge", AgentKind: new(agents.KindCodex), State: store.SessionClosed,
		StartedAt: started, LastPromptAt: new(started.Add(time.Minute))}
	if sid != "" {
		s.SessionID = &sid
	}
	out, err := h.st.CreateSession(h.ctx, s)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// On 2026-10-22 most Codex sessions got no session id for hours after a
// rewrite of ~/.codex/hooks.json, and the daemon said nothing but one log
// line per session. Now two Codex sessions in a row, across PRs, without an
// id a few minutes after their start give one codex.session_ids_missing
// warning and one toast; a third says nothing more, and a later session
// with an id ends the run with one info event.
func TestTwoCodexSessionsInARowWithoutAnIDWarnOnceUntilOneHasAnID(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	first := h.pr(1)
	h.codexSessionAt(first.ID, h.clock.Now().Add(-3*time.Hour), "019a-old")
	since := h.clock.Now()
	h.codexSessionAt(first.ID, since, "")
	h.advance(time.Minute)
	h.codexSessionAt(pr.ID, h.clock.Now(), "")
	h.tick()
	if evs := h.events(EventCodexSessionIDsMissing); len(evs) != 0 {
		t.Fatalf("warned before the sessions were minutes old: %+v", evs)
	}

	h.advance(codexSessionIDWait)
	h.tick()
	evs := h.events(EventCodexSessionIDsMissing)
	want := "Codex sessions get no session id since " + since.Local().Format("15:04") + " (2 sessions)"
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, want) ||
		!strings.Contains(evs[0].Message, "magnum doctor") {
		t.Fatalf("events = %+v, want one warning %q", evs, want)
	}
	if got := toastsWith(h, "no session id"); len(got) != 1 {
		t.Fatalf("toasts = %q, want one", h.nh.shown())
	}

	h.codexSessionAt(pr.ID, h.clock.Now(), "")
	h.advance(codexSessionIDWait + time.Minute)
	h.tick()
	if evs := h.events(EventCodexSessionIDsMissing); len(evs) != 1 {
		t.Fatalf("a third session without an id warned again: %+v", evs)
	}
	if got := toastsWith(h, "no session id"); len(got) != 1 {
		t.Fatalf("toasts after a third session = %q, want still one", h.nh.shown())
	}

	h.codexSessionAt(first.ID, h.clock.Now(), "019a-new")
	h.advance(codexSessionIDWait + time.Minute)
	h.tick()
	h.advance(time.Minute)
	h.tick()
	back := h.events(EventCodexSessionIDsBack)
	if len(back) != 1 || back[0].Level != "info" || !strings.Contains(back[0].Message, "session id") {
		t.Fatalf("back events = %+v, want one info event", back)
	}
	if _, ok := kvValue(h, KVCodexSessionIDsMissing); ok {
		t.Fatal("the warning's mark stayed after a session got an id")
	}
	if evs := h.events(EventCodexSessionIDsMissing); len(evs) != 1 {
		t.Fatalf("missing events = %d after the run ended, want still one", len(evs))
	}
}

// writeHooks writes home's hooks.json with herdr's SessionStart hook at
// place (0: first, 2: behind two hooks without a timeout) and a config.toml
// that trusts every place but session_start:2:0, then sets the file's
// modification time to at.
func writeHooks(t *testing.T, home string, place int, at time.Time) {
	t.Helper()
	herdr := `{"matcher":"","hooks":[{"type":"command","command":"bash '/Users/x/.codex/herdr-agent-state.sh' session","timeout":10}]}`
	groups := []string{herdr}
	if place == 2 {
		groups = []string{
			`{"hooks":[{"type":"command","command":"'/Applications/Bartender 6.app/Contents/Helpers/Bartender NotchBar' codex-hook"}]}`,
			`{"hooks":[{"type":"command","command":"/opt/homebrew/bin/moshi-hook codex-hook"}]}`, herdr}
	}
	file := filepath.Join(home, "hooks.json")
	if err := os.WriteFile(file, []byte(`{"hooks":{"SessionStart":[`+strings.Join(groups, ",")+`]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state := "[hooks.state.\"" + file + ":session_start:0:0\"]\ntrusted_hash = \"sha256:aa\"\n[hooks.state.\"" + file + ":session_start:1:0\"]\ntrusted_hash = \"sha256:bb\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, at, at); err != nil {
		t.Fatal(err)
	}
}

// The daemon watches herdr's Codex session hook: once a minute, with the
// usage read, it compares hooks.json's modification time, and a change that
// leaves the hook failing doctor's place or trust check gives one
// codex.hooks_changed warning and one toast naming the file and the failed
// checks. An unchanged file, the first reading and a change that passes give
// none.
func TestAChangedHooksJSONThatFailsTheChecksWarnsOncePerChange(t *testing.T) {
	home := t.TempDir()
	h := newHarness(t, func(h *harness) { h.d.CodexHome = func(config.Role) string { return home } })
	base := time.Date(2026, 10, 22, 15, 0, 0, 0, time.Local)
	writeHooks(t, home, 0, base)
	h.tick()
	h.advance(time.Minute)
	h.tick()
	if evs := h.events(EventCodexHooksChanged); len(evs) != 0 {
		t.Fatalf("an unchanged hooks.json warned: %+v", evs)
	}

	changed := base.Add(67 * time.Minute)
	writeHooks(t, home, 2, changed)
	h.advance(time.Minute)
	h.tick()
	evs := h.events(EventCodexHooksChanged)
	if len(evs) != 1 || evs[0].Level != "warn" {
		t.Fatalf("events = %+v, want one warning", evs)
	}
	for _, want := range []string{"$CODEX_HOME/hooks.json changed at " + changed.Local().Format("15:04"),
		"runs after 2 SessionStart hooks without a timeout (Bartender NotchBar, moshi-hook)",
		"; it has no trust entry for its place session_start:2:0", "magnum doctor"} {
		if !strings.Contains(evs[0].Message, want) {
			t.Errorf("message %q lacks %q", evs[0].Message, want)
		}
	}
	if strings.Contains(evs[0].Message, home) {
		t.Errorf("message %q names the Codex home's path (a pane env value)", evs[0].Message)
	}
	if got := toastsWith(h, "Codex session hook"); len(got) != 1 {
		t.Fatalf("toasts = %q, want one", h.nh.shown())
	}

	h.advance(time.Minute)
	h.tick()
	if evs := h.events(EventCodexHooksChanged); len(evs) != 1 {
		t.Fatalf("the same change warned again: %+v", evs)
	}
	writeHooks(t, home, 0, changed.Add(2*time.Hour)) // restored: passes
	h.advance(time.Minute)
	h.tick()
	if evs := h.events(EventCodexHooksChanged); len(evs) != 1 {
		t.Fatalf("a change that passes warned: %+v", evs)
	}
	if got := toastsWith(h, "Codex session hook"); len(got) != 1 {
		t.Fatalf("toasts = %q, want still one", h.nh.shown())
	}
}
