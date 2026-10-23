package agents

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeHomeHooks writes a Codex home in a temp dir: hooks.json as given and,
// unless state is "-", config.toml with state, in which {hooks} stands for
// the hooks.json path (the key of a hook's [hooks.state] entry starts with
// it). It returns the home.
func writeHomeHooks(t *testing.T, hooks, state string) string {
	t.Helper()
	home := t.TempDir()
	file := filepath.Join(home, "hooks.json")
	if err := os.WriteFile(file, []byte(hooks), 0o600); err != nil {
		t.Fatal(err)
	}
	if state != "-" {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(strings.ReplaceAll(state, "{hooks}", file)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// A hooks.json of 2026-10-23: herdr's hooks, the Bartender NotchBar's and
// moshi's on several events, and one event Codex does not run.
const fiveHooksJSON = `{"hooks":{
  "SessionStart":[
    {"matcher":"","hooks":[{"type":"command","command":"bash '/Users/x/.codex/herdr-agent-state.sh' session","timeout":10}]},
    {"hooks":[{"type":"command","command":"/opt/homebrew/bin/moshi-hook codex-hook"},{"type":"command","command":"'/usr/local/bin/notify-hook' codex"}]}],
  "PreToolUse":[{"hooks":[{"type":"command","command":"'/Applications/Bartender 6.app/Contents/Helpers/Bartender NotchBar' codex-hook"}]}],
  "Stop":[{"hooks":[{"type":"command","command":"bash '/Users/x/.codex/herdr-agent-state.sh' stop"}]}],
  "Notification":[{"hooks":[{"type":"command","command":"/opt/homebrew/bin/moshi-hook notify"}]}]}}`

// places are the places of hooks.
func places(hooks []CodexHookAt) []string {
	out := make([]string, len(hooks))
	for i, h := range hooks {
		out[i] = h.Place
	}
	return out
}

// Codex shows its hooks review for any hook of hooks.json whose place has no
// [hooks.state] entry with a trusted_hash, whatever its event (spec 122
// checked only herdr's SessionStart hook, and on 2026-10-23 a second review
// wave came from the others). ReadCodexHooksTrust reads every hook of every
// event Codex runs, group and index, keyed as Codex keys its trust
// ("<hooks.json>:<event>:<group>:<index>", the path as written or with its
// symlinks resolved): an entry without a hash, another home's entry and
// another event's entry trust nothing; an event Codex does not run is not
// read. A missing config.toml trusts nothing, a missing hooks.json has
// nothing to judge, and a file that is not JSON is an error that does not
// quote it.
func TestReadCodexHooksTrustNamesEveryHookWithoutATrustEntry(t *testing.T) {
	state := `model = "gpt-6.1-sol"
[hooks.state."{hooks}:session_start:0:0"]
trusted_hash = "sha256:0001"
[hooks.state."{hooks}:session_start:1:1"]
trusted_hash = "sha256:0002"
enabled = true
[hooks.state."{hooks}:pre_tool_use:0:0"]
enabled = true
[hooks.state."/elsewhere/hooks.json:session_start:1:0"]
trusted_hash = "sha256:0003"
[hooks.state."{hooks}:stop:1:0"]
trusted_hash = "sha256:0004"
`
	home := writeHomeHooks(t, fiveHooksJSON, state)
	got, ok, err := ReadCodexHooksTrust(home)
	if err != nil || !ok {
		t.Fatalf("ReadCodexHooksTrust = %v, %v", ok, err)
	}
	file := filepath.Join(home, "hooks.json")
	if got.HooksFile != file || got.ConfigFile != filepath.Join(home, "config.toml") || got.Hooks != 5 {
		t.Fatalf("files %q %q, hooks %d; want 5 hooks", got.HooksFile, got.ConfigFile, got.Hooks)
	}
	want := []string{"pre_tool_use:0:0", "session_start:1:0", "stop:0:0"}
	if p := places(got.Untrusted); !slices.Equal(p, want) {
		t.Fatalf("untrusted = %q, want %q (Codex's event order, then group and index)", p, want)
	}
	if names := []string{got.Untrusted[0].Name, got.Untrusted[1].Name, got.Untrusted[2].Name}; !slices.Equal(names,
		[]string{"Bartender NotchBar", "moshi-hook", "herdr-agent-state.sh"}) {
		t.Errorf("names = %q", names)
	}
	raw, _ := os.ReadFile(file)
	if got.Fingerprint == "" || got.Fingerprint != codexHooksFingerprint(file, raw) {
		t.Errorf("fingerprint = %q, want hooks.json's", got.Fingerprint)
	}
	if s := got.UntrustedSentence("~/.codex/hooks.json"); s != "3 hooks in ~/.codex/hooks.json have no trust entry" {
		t.Errorf("sentence = %q", s)
	}
	if s := got.UntrustedPlaces(); s != "Bartender NotchBar at pre_tool_use:0:0, moshi-hook at session_start:1:0, herdr-agent-state.sh at stop:0:0" {
		t.Errorf("places = %q", s)
	}

	t.Run("trusted through the resolved path", func(t *testing.T) {
		target := writeHomeHooks(t, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x"}]}]}}`, "-")
		link := filepath.Join(t.TempDir(), "codex")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(target, "hooks.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "config.toml"), []byte("[hooks.state.\""+resolved+":stop:0:0\"]\ntrusted_hash = \"sha256:aa\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok, err := ReadCodexHooksTrust(link)
		if err != nil || !ok || got.Hooks != 1 || len(got.Untrusted) != 0 {
			t.Fatalf("got %+v, %v, %v; want the one hook trusted", got, ok, err)
		}
		if s := got.UntrustedSentence("~/.codex/hooks.json"); s != "" {
			t.Errorf("sentence = %q, want none", s)
		}
	})
	t.Run("one hook, no config.toml", func(t *testing.T) {
		home := writeHomeHooks(t, `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/opt/homebrew/bin/moshi-hook codex-hook"}]}]}}`, "-")
		got, ok, err := ReadCodexHooksTrust(home)
		if err != nil || !ok || !slices.Equal(places(got.Untrusted), []string{"user_prompt_submit:0:0"}) {
			t.Fatalf("got %+v, %v, %v", got, ok, err)
		}
		if s := got.UntrustedSentence("$CODEX_HOME/hooks.json"); s != "1 hook in $CODEX_HOME/hooks.json has no trust entry" {
			t.Errorf("sentence = %q", s)
		}
	})
	t.Run("no hooks.json", func(t *testing.T) {
		if _, ok, err := ReadCodexHooksTrust(t.TempDir()); ok || err != nil {
			t.Fatalf("ok %v, err %v; want nothing to judge", ok, err)
		}
		if _, ok, err := ReadCodexHooksTrust(""); ok || err != nil {
			t.Fatalf("unknown home: ok %v, err %v", ok, err)
		}
	})
	t.Run("not JSON", func(t *testing.T) {
		home := writeHomeHooks(t, `{"hooks": secret-token-123`, "-")
		_, _, err := ReadCodexHooksTrust(home)
		if err == nil || !strings.Contains(err.Error(), "is not valid JSON") || strings.Contains(err.Error(), "secret-token-123") {
			t.Fatalf("err = %v", err)
		}
	})
}
