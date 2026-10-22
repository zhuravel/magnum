package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// hookSpec is one hook of a test hooks.json: its command and its timeout in
// seconds (0: none).
type hookSpec struct {
	command string
	timeout int
}

// codexHookHome writes a Codex home in a temp dir: hooks.json with one
// SessionStart group per hook (one hook each, so the nth hook's place is
// session_start:<n>:0) and, unless state is "-", config.toml with state, in
// which {hooks} stands for the hooks.json path (the key Codex gives each
// hook's [hooks.state] entry starts with it). {home} in a command stands for
// the Codex home. Commands never start with ~: the home a test reads is its
// own.
func codexHookHome(t *testing.T, hooks []hookSpec, state string) string {
	t.Helper()
	home := t.TempDir()
	var groups []map[string]any
	for _, h := range hooks {
		hook := map[string]any{"type": "command", "command": strings.ReplaceAll(h.command, "{home}", home)}
		if h.timeout > 0 {
			hook["timeout"] = h.timeout
		}
		groups = append(groups, map[string]any{"matcher": "", "hooks": []map[string]any{hook}})
	}
	raw, err := json.Marshal(map[string]any{"hooks": map[string]any{
		"SessionStart": groups,
		"Stop":         []map[string]any{{"hooks": []map[string]any{{"type": "command", "command": "bash herdr-agent-state.sh stop"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, "hooks.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if state != "-" {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(strings.ReplaceAll(state, "{hooks}", file)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// trustedAt is a config.toml that trusts the hooks at places, each given as
// "session_start:<group>:<index>".
func trustedAt(places ...string) string {
	var b strings.Builder
	b.WriteString("model = \"gpt-6.1-sol\"\n")
	for i, p := range places {
		fmt.Fprintf(&b, "[hooks.state.\"{hooks}:%s\"]\ntrusted_hash = \"sha256:%04x\"\n", p, i+1)
	}
	return b.String()
}

const (
	herdrHook    = "bash '/Users/x/.codex/herdr-agent-state.sh' session"
	otherHook    = "'/usr/local/bin/notify-hook' codex"
	notchBarHook = "'/Applications/Bartender 6.app/Contents/Helpers/Bartender NotchBar' codex-hook"
	moshiHook    = "/opt/homebrew/bin/moshi-hook codex-hook"
)

// doctorHookChecks runs doctor on the Codex home home and returns its checks
// whose names start with "codex session hook", by name.
func doctorHookChecks(t *testing.T, d doctorDeps, ag *fakeDoctorAgents, home string) map[string]doctorCheck {
	t.Helper()
	ag.codexHome = home
	out := map[string]doctorCheck{}
	for _, c := range doctorRun(context.Background(), d) {
		if strings.HasPrefix(c.Name, "codex session hook") {
			out[c.Name] = c
		}
	}
	if len(out) == 0 {
		t.Fatalf("no codex session hook check for %s", home)
	}
	return out
}

// herdr learns a Codex session's id from the SessionStart hook it installs in
// CODEX_HOME's hooks.json; a [hooks.state] entry with enabled = false turns it
// off, and then no judge resumes and no helper's refusal is seen. Doctor
// warns, naming the entry and what goes blind; a hooks.json without herdr's
// hook warns that it is missing, and a Codex home without hooks.json is not
// judged. It changes nothing.
func TestDoctorWarnsWhenHerdrsCodexSessionHookIsDisabled(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	disabled := `model = "gpt-6.1-sol"
[hooks.state."{hooks}:session_start:0:0"]
enabled = true
trusted_hash = "sha256:aaaa"
[hooks.state."{hooks}:session_start:1:0"]
trusted_hash = "sha256:47bf"
enabled = false
`
	home := codexHookHome(t, []hookSpec{{otherHook, 5}, {herdrHook, 10}}, disabled)
	before, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(home, "hooks.json") + ":session_start:1:0"
	checks := doctorHookChecks(t, d, ag, home)
	c := checks["codex session hook"]
	if c.Status != doctorWarn || !strings.Contains(c.Detail, key) || !strings.Contains(c.Detail, "enabled = false") ||
		!strings.Contains(c.Detail, agents.SessionIDBlind) || !strings.Contains(c.Detail, "codex-judge") || !strings.Contains(c.Fix, "enabled = true") {
		t.Fatalf("disabled hook = %s %q fix %q, want WARN naming %s, enabled = false and what goes blind", c.Status, c.Detail, c.Fix, key)
	}
	if c, ok := checks["codex session hook trust"]; ok {
		t.Errorf("a disabled hook got a trust check too: %s %q", c.Status, c.Detail)
	}
	if after, _ := os.ReadFile(filepath.Join(home, "config.toml")); string(after) != string(before) {
		t.Fatalf("doctor changed config.toml:\n%s", after)
	}

	c = doctorHookChecks(t, d, ag, codexHookHome(t, []hookSpec{{otherHook, 5}}, disabled))["codex session hook"]
	if c.Status != doctorWarn || !strings.Contains(c.Detail, "missing") || !strings.Contains(c.Detail, "herdr-agent-state.sh") ||
		!strings.Contains(c.Detail, agents.SessionIDBlind) || c.Fix == "" {
		t.Errorf("no herdr hook = %s %q fix %q, want WARN that it is missing", c.Status, c.Detail, c.Fix)
	}

	ag.codexHome = t.TempDir()
	if c := doctorByName(doctorRun(context.Background(), d))["codex session hook"]; c.Status != doctorSkip {
		t.Errorf("no hooks.json = %s %q, want SKIP (herdr not installed is another check)", c.Status, c.Detail)
	}
}

// On 2026-10-22 a rewrite of ~/.codex/hooks.json moved herdr's SessionStart
// hook from the first group to the third, behind two hooks without a
// timeout, and most Codex sessions got no id while doctor said the hook was
// on. Doctor now warns about the hook's place, naming the hooks before it by
// the last path element of their programs, cleaned for the terminal, and the
// herdr integration version its script's header names.
func TestDoctorWarnsWhenHerdrsHookRunsBehindHooksWithoutATimeout(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	home := codexHookHome(t, []hookSpec{{notchBarHook, 0}, {moshiHook, 0}, {"bash '{home}/herdr-agent-state.sh' session", 10}},
		trustedAt("session_start:0:0", "session_start:1:0", "session_start:2:0"))
	if err := os.WriteFile(filepath.Join(home, "herdr-agent-state.sh"),
		[]byte("#!/usr/bin/env bash\n# managed by herdr; do not edit\nHERDR_INTEGRATION_VERSION=10\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := doctorHookChecks(t, d, ag, home)
	c := checks["codex session hook"]
	for _, want := range []string{"runs after 2 SessionStart hooks without a timeout (Bartender NotchBar, moshi-hook)",
		filepath.Join(home, "hooks.json"), "get no id", "herdr integration 10", "codex-judge"} {
		if c.Status != doctorWarn || !strings.Contains(c.Detail, want) {
			t.Errorf("hook in third place = %s %q, want WARN with %q", c.Status, c.Detail, want)
		}
	}
	if !strings.Contains(c.Fix, "first") || !strings.Contains(c.Fix, "timeout") || !strings.Contains(c.Fix, "magnum never edits") {
		t.Errorf("fix %q, want: move herdr's entry first, or give those hooks a timeout", c.Fix)
	}
	if tc, ok := checks["codex session hook trust"]; ok {
		t.Errorf("a trusted hook got a trust check: %s %q", tc.Status, tc.Detail)
	}

	// A hook before herdr's with a timeout over 10 s warns too, and a name
	// with a terminal escape is cleaned.
	c = doctorHookChecks(t, d, ag, codexHookHome(t, []hookSpec{{"/usr/bin/slow\x1b[31m-hook run", 30}, {herdrHook, 10}},
		trustedAt("session_start:0:0", "session_start:1:0")))["codex session hook"]
	if c.Status != doctorWarn || !strings.Contains(c.Detail, "1 SessionStart hook with a timeout over 10 s (slow[31m-hook, 30 s)") ||
		strings.ContainsRune(c.Detail, '\x1b') {
		t.Errorf("hook behind a 30 s hook = %s %q, want WARN naming it cleaned", c.Status, c.Detail)
	}
}

// Herdr's hook first passes, and so does herdr's hook behind hooks with
// timeouts of 10 s or less; the OK line names its place.
func TestDoctorPassesHerdrsHookFirstOrBehindShortTimeouts(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	for name, hooks := range map[string][]hookSpec{
		"first":                 {{herdrHook, 10}, {notchBarHook, 0}, {moshiHook, 0}},
		"behind short timeouts": {{otherHook, 5}, {moshiHook, 10}, {herdrHook, 10}},
	} {
		home := codexHookHome(t, hooks, trustedAt("session_start:0:0", "session_start:1:0", "session_start:2:0"))
		checks := doctorHookChecks(t, d, ag, home)
		if c := checks["codex session hook"]; c.Status != doctorPass || !strings.Contains(c.Detail, "herdr-agent-state.sh") ||
			!strings.Contains(c.Detail, "session_start:") {
			t.Errorf("%s = %s %q fix %q, want PASS naming its place", name, c.Status, c.Detail, c.Fix)
		}
		if c, ok := checks["codex session hook trust"]; ok {
			t.Errorf("%s: trust = %s %q, want no such check", name, c.Status, c.Detail)
		}
	}
}

// Codex keys a hook's trust by its place: a hook moved to a place config.toml
// has no [hooks.state] entry for does not run until the operator reviews the
// hooks again. Doctor warns, naming the place, also when config.toml or its
// table is missing; it changes nothing.
func TestDoctorWarnsWhenHerdrsHookHasNoTrustEntryForItsPlace(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	for name, state := range map[string]string{
		"old places only": trustedAt("session_start:0:0", "session_start:1:0"),
		"entry, no hash":  "[hooks.state.\"{hooks}:session_start:2:0\"]\nenabled = true\n",
		"no table":        `model = "gpt-6.1-sol"`,
		"no config.toml":  "-",
		"another home's":  "[hooks.state.\"/elsewhere/hooks.json:session_start:2:0\"]\ntrusted_hash = \"sha256:aa\"\n",
		"another event's": "[hooks.state.\"{hooks}:stop:2:0\"]\ntrusted_hash = \"sha256:aa\"\n",
	} {
		home := codexHookHome(t, []hookSpec{{otherHook, 5}, {moshiHook, 5}, {herdrHook, 10}}, state)
		checks := doctorHookChecks(t, d, ag, home)
		c := checks["codex session hook trust"]
		if c.Status != doctorWarn || !strings.Contains(c.Detail, "no trust entry for its place session_start:2:0") ||
			!strings.Contains(c.Detail, "review the hooks") || !strings.Contains(c.Fix, "magnum never changes it") {
			t.Errorf("%s: trust = %s %q fix %q, want WARN naming session_start:2:0", name, c.Status, c.Detail, c.Fix)
		}
		if c := checks["codex session hook"]; c.Status == doctorPass {
			t.Errorf("%s: an untrusted hook kept the OK line %q", name, c.Detail)
		}
	}
}

// seedCodexSession adds a closed Codex session of role to pr, started at
// started, prompted a minute later unless prompted is false, with session
// id sid ("" = none).
func seedCodexSession(t *testing.T, st *store.Store, pr store.PR, role string, started time.Time, sid string, prompted bool) store.Session {
	t.Helper()
	s := store.Session{PRID: pr.ID, Role: role, AgentKind: new(agents.KindCodex), State: store.SessionClosed, StartedAt: started}
	if sid != "" {
		s.SessionID = &sid
	}
	if prompted {
		s.LastPromptAt = new(started.Add(time.Minute))
	}
	out, err := st.CreateSession(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Herdr's hook can look fine and still give no ids (the 2026-10-22 rewrite
// also replaced its script). Doctor reads the registry: when the newest two
// or more Codex sessions of the last day that ran an agent had no session id
// ten minutes after their start, it warns whatever the hook checks say,
// counting from the oldest session without one since the ids last came
// twice in a row. One old session without an id, a session started minutes
// ago, a resumed one (it takes the id it resumes) and one never prompted
// give no warning.
func TestDoctorWarnsWhenTheNewestCodexSessionsHaveNoSessionID(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	ag.codexHome = codexHookHome(t, []hookSpec{{herdrHook, 10}}, trustedAt("session_start:0:0"))
	st := d.Store
	_, pr1 := inspSeedPR(t, st, "talkable/talkable", 101, store.PRReviewed, nil)
	_, pr2 := inspSeedPR(t, st, "talkable/talkable", 102, store.PRReviewed, nil)
	now := time.Now()
	check := func() doctorCheck {
		t.Helper()
		c, ok := doctorByName(doctorRun(context.Background(), d))["codex session ids"]
		if !ok {
			t.Fatal("no codex session ids check")
		}
		return c
	}
	if c := check(); c.Status != doctorSkip {
		t.Errorf("no sessions = %s %q, want SKIP", c.Status, c.Detail)
	}

	seedCodexSession(t, st, pr1, "codex-judge", now.Add(-30*time.Hour), "", true) // older than a day
	seedCodexSession(t, st, pr1, "codex-judge", now.Add(-6*time.Hour), "019a-1", true)
	seedCodexSession(t, st, pr1, "codex-judge", now.Add(-5*time.Hour), "", true) // one old session without an id
	seedCodexSession(t, st, pr1, "codex-judge", now.Add(-4*time.Hour), "019a-2", true)
	recovered := seedCodexSession(t, st, pr2, "codex-judge", now.Add(-3*time.Hour), "019a-3", true)
	seedCodexSession(t, st, pr2, "codex-judge", now.Add(-2*time.Minute), "", true) // too new to judge
	if _, err := st.AppendEvent(context.Background(), store.Event{Level: "info", Kind: agents.EventSessionID, Message: "from its Codex rollout",
		Data: json.RawMessage(fmt.Sprintf(`{"session":%d,"source":%q}`, recovered.ID, agents.SessionIDFromRollout))}); err != nil {
		t.Fatal(err)
	}
	if c := check(); c.Status != doctorPass || !strings.Contains(c.Detail, "3 of the 4") ||
		!strings.Contains(c.Detail, "1 id came from its session's Codex rollout") {
		t.Errorf("one old session without an id = %s %q, want PASS counting 3 of the 4 with an id, 1 from its rollout", c.Status, c.Detail)
	}

	since := now.Add(-150 * time.Minute)
	seedCodexSession(t, st, pr1, "codex-judge", since, "", true)
	seedCodexSession(t, st, pr2, "codex-judge", now.Add(-2*time.Hour), "019a-4", true)
	seedCodexSession(t, st, pr1, "codex-judge", now.Add(-100*time.Minute), "", true)
	seedCodexSession(t, st, pr2, "codex-judge", now.Add(-90*time.Minute), "", false) // never prompted: no agent ran
	resumed := store.Session{PRID: pr2.ID, Role: "codex-judge", AgentKind: new(agents.KindCodex), State: store.SessionClosed,
		StartedAt: now.Add(-80 * time.Minute), SessionID: new("019a-0"), ResumedFrom: new("019a-0"), LastPromptAt: new(now.Add(-79 * time.Minute))}
	if _, err := st.CreateSession(context.Background(), resumed); err != nil {
		t.Fatal(err)
	}
	seedCodexSession(t, st, pr2, "codex-judge", now.Add(-time.Hour), "", true)
	c := check()
	want := fmt.Sprintf("3 of the 4 Codex sessions started since %s have no session id, the newest 2 in a row", since.Local().Format("15:04"))
	if c.Status != doctorWarn || !strings.Contains(c.Detail, want) || !strings.Contains(c.Fix, "codex session hook") {
		t.Fatalf("the newest two without an id = %s %q fix %q, want WARN %q", c.Status, c.Detail, c.Fix, want)
	}
}
