package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeSettings points the env's Manager at a temp settings file holding
// content ("" = no file) and makes the fake Claude save every /model choice
// there as Claude Code does (JSON.stringify of the parsed object: key order
// kept, two-space indent). It returns the file's path.
func (e *env) claudeSettings(content string) string {
	e.t.Helper()
	path := filepath.Join(e.t.TempDir(), "claude", "settings.json")
	if content != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			e.t.Fatal(err)
		}
	}
	e.m.d.ClaudeSettings = path
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	screen := e.h.onRun
	e.h.onRun = func(f *fakeHerdr, pane, cmd string) {
		if screen != nil {
			screen(f, pane, cmd)
		}
		model, ok := strings.CutPrefix(cmd, "/model ")
		if !ok {
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			b = []byte("{}")
		}
		ms, err := parseObject(b)
		if err != nil {
			e.t.Errorf("fake claude: %v", err)
			return
		}
		raw := json.RawMessage(jsonString(model))
		if i := ms.index("model"); i >= 0 {
			ms[i].val = raw
		} else {
			ms = append(ms, member{"model", raw})
		}
		out, _ := formatLike([]byte("{\n  \"x\": 1\n}\n"), ms.encode())
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, out, 0o600)
	}
	return path
}

const userSettings = `{
  "theme": "dark",
  "model": "fable",
  "permissions": {
    "allow": [
      "Bash(ls:*)"
    ]
  },
  "statusLine": {
    "type": "command",
    "command": "~/bin/status"
  }
}
`

// /model opus in a review session must not become the user's default for
// new sessions: the file is byte for byte what it was.
func TestSwitchModelRestoresTheUsersDefaultModel(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.claudeScreen(false, "")
	path := e.claudeSettings(userSettings)
	s := e.session(RoleClaude)
	if err := e.m.SwitchModel(e.ctx, s, "opus", SwitchLimitHit); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != userSettings {
		t.Fatalf("settings after the switch:\n%s\nwant unchanged:\n%s", got, userSettings)
	}
	evs := e.eventsOf(EventDefaultModelRestored)
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"model":"fable"`) || !strings.Contains(string(evs[0].Data), `"found":"opus"`) {
		t.Fatalf("events = %+v", evs)
	}
	if v, _ := e.kv(KVSessionModel(s.ID)); v != "opus" {
		t.Fatalf("the session itself still switched: model %q", v)
	}
}

// A user without a default model keeps having none; the other keys stay.
func TestSwitchModelKeepsAnAbsentDefaultModelAbsent(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.claudeScreen(false, "")
	const noModel = "{\n  \"theme\": \"dark\",\n  \"env\": {\n    \"A\": \"1\"\n  }\n}\n"
	path := e.claudeSettings(noModel)
	if err := e.m.SwitchModel(e.ctx, e.session(RoleClaude), "opus", SwitchLimitHit); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != noModel {
		t.Fatalf("settings:\n%s\nwant:\n%s", got, noModel)
	}
	if evs := e.eventsOf(EventDefaultModelRestored); len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"model":null`) {
		t.Fatalf("events = %+v", evs)
	}

	// No settings file at all: Claude creates one; the key goes again.
	e2 := newEnv(t)
	e2.started()
	e2.claudeScreen(false, "")
	path = e2.claudeSettings("")
	if err := e2.m.SwitchModel(e2.ctx, e2.session(RoleClaude), "opus", SwitchLimitHit); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &doc); err != nil || len(doc) != 0 {
		t.Fatalf("settings = %v (%v), want an empty object", doc, err)
	}
}

// A CLI that left the file alone: nothing is written and nothing recorded.
// A switch that was never confirmed is restored too (the choice may have
// been saved anyway).
func TestSwitchModelRestoresOnlyWhatChanged(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.claudeScreen(false, "")
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(userSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	e.m.d.ClaudeSettings = path
	before, _ := os.Stat(path)
	if err := e.m.SwitchModel(e.ctx, e.session(RoleClaude), "opus", SwitchLimitHit); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) || readFile(t, path) != userSettings {
		t.Fatal("an unchanged settings file was rewritten")
	}
	if evs := e.eventsOf(EventDefaultModelRestored); len(evs) != 0 {
		t.Fatalf("events = %+v, want none", evs)
	}

	e2 := newEnv(t)
	e2.started()
	e2.claudeScreen(true, "no") // the switch times out
	path = e2.claudeSettings(userSettings)
	if err := e2.m.SwitchModel(e2.ctx, e2.session(RoleClaude), "opus", SwitchLimitHit); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if got := readFile(t, path); got != userSettings {
		t.Fatalf("settings after a failed switch:\n%s", got)
	}
}

// Two Claude sessions switching at once share the settings file: the second
// waits for the first to restore it, so it never snapshots the first one's
// choice. Waiting ends with ctx.
func TestHoldDefaultModelSerializesSwitches(t *testing.T) {
	e := newEnv(t)
	e.started()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"model":"fable"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.m.d.ClaudeSettings = path
	s := e.session(RoleClaude)
	restore, err := e.m.holdDefaultModel(e.ctx, s, KindClaude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"model":"opus"}`), 0o600); err != nil { // the first switch saved opus
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := e.m.holdDefaultModel(ctx, s, KindClaude); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second hold while the first is held: %v, want the deadline", err)
	}
	restore()
	if got := readFile(t, path); got != `{"model":"fable"}` {
		t.Fatalf("settings = %s", got)
	}
	restore2, err := e.m.holdDefaultModel(e.ctx, s, KindClaude)
	if err != nil {
		t.Fatalf("hold after the release: %v", err)
	}
	restore2()

	// Another kind, or no settings path (a test binary's default), holds nothing.
	for _, c := range []struct {
		kind, path string
	}{{KindCodex, path}, {KindClaude, ""}} {
		e.m.d.ClaudeSettings = c.path
		r1, err1 := e.m.holdDefaultModel(e.ctx, s, c.kind)
		r2, err2 := e.m.holdDefaultModel(e.ctx, s, c.kind) // would block if it took the lock
		if err1 != nil || err2 != nil {
			t.Fatalf("%s %q: %v %v", c.kind, c.path, err1, err2)
		}
		r1()
		r2()
	}
}

func TestRestoreSettingsKeyKeepsLayoutAndOrder(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fable := settingsKey{present: true, raw: json.RawMessage(`"fable"`)}
	for _, c := range []struct {
		name, in string
		want     settingsKey
		out      string
		changed  bool
	}{
		{"compact set", `{"a":1,"model":"opus","b":[1,2]}`, fable, `{"a":1,"model":"fable","b":[1,2]}`, true},
		{"compact add", `{"a":1}`, fable, `{"a":1,"model":"fable"}`, true},
		{"indented remove", "{\n    \"a\": 1,\n    \"model\": \"opus\"\n}\n", settingsKey{}, "{\n    \"a\": 1\n}\n", true},
		{"duplicate keys removed", `{"model":"x","a":1,"model":"opus"}`, settingsKey{}, `{"a":1}`, true},
		{"already equal", `{"model": "fable"}`, fable, `{"model": "fable"}`, false},
		{"already absent", `{"a":1}`, settingsKey{}, `{"a":1}`, false},
	} {
		p := write("s.json", c.in)
		_, changed, err := restoreSettingsKey(p, "model", c.want)
		if err != nil || changed != c.changed {
			t.Errorf("%s: changed %v, err %v; want changed %v", c.name, changed, err, c.changed)
		}
		if got := readFile(t, p); got != c.out {
			t.Errorf("%s:\n%s\nwant:\n%s", c.name, got, c.out)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
			t.Errorf("%s: mode %v, want the file's own 0640", c.name, fi.Mode().Perm())
		}
	}
	// A file that is not a JSON object is left alone.
	p := write("bad.json", "not json")
	if _, _, err := restoreSettingsKey(p, "model", fable); err == nil || readFile(t, p) != "not json" {
		t.Fatalf("bad file: err %v, content %q", err, readFile(t, p))
	}
	if _, err := readSettingsKey(p, "model"); err == nil {
		t.Fatal("readSettingsKey of a bad file: no error")
	}
}
