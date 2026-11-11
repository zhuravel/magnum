package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agenthooks"
)

// hookCommands are the commands of the handlers of event in the settings
// file at path (a Claude settings.json or a Codex hooks.json).
func (f *hooksFixture) hookCommands(path, event string) []string {
	f.t.Helper()
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(f.read(path)), &doc); err != nil {
		f.t.Fatalf("%s: %v", path, err)
	}
	var out []string
	for _, g := range doc.Hooks[event] {
		for _, h := range g.Hooks {
			out = append(out, h.Command)
		}
	}
	return out
}

// Claude's hook config holds UserPromptSubmit beside the other events: the
// turn's start, which no other hook tells. Claude has no per-hook trust, so
// the install always adds it.
func TestHooksGiveClaudeAUserPromptSubmitEntry(t *testing.T) {
	f := newHooksFixture(t)
	if code := f.run("hooks", "install", "claude"); code != 0 {
		t.Fatalf("install = %d\n%s%s", code, f.Out, f.Err)
	}
	want := hookCommand(f.claudeScript(), agenthooks.AgentClaude, agenthooks.NameUserPromptSubmit)
	if got := f.hookCommands(f.claudeSettings(), "UserPromptSubmit"); len(got) != 1 || got[0] != want {
		t.Fatalf("UserPromptSubmit = %q, want %q", got, want)
	}
	if !strings.Contains(f.Out.String(), "SessionEnd, UserPromptSubmit") {
		t.Fatalf("install output:\n%s", f.Out)
	}
	if code := f.run("hooks", "status", "claude"); code != 0 {
		t.Fatalf("status = %d %s", code, f.Out)
	}
}

// Codex gets UserPromptSubmit only with the install's --prompt-submit: a
// new Codex hook asks for trust in every Codex home, and until then the
// daemon holds new Codex sessions, so the install names the trust step.
// Once there it is magnum's like the others: status counts its trust and a
// later install without the flag keeps it; without it, status does not miss
// it.
func TestCodexGetsUserPromptSubmitOnlyWithTheInstallFlag(t *testing.T) {
	f := newHooksFixture(t)
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatalf("install = %d\n%s%s", code, f.Out, f.Err)
	}
	if got := f.hookCommands(f.codexHooks(), "UserPromptSubmit"); len(got) != 0 {
		t.Fatalf("UserPromptSubmit without the flag: %q", got)
	}
	f.trustPlaces("sha256:1", f.magnumPlaces()...)
	if code := f.run("hooks", "status", "codex"); code != 0 {
		t.Fatalf("status without UserPromptSubmit = %d %s", code, f.Out)
	}
	trusted := f.magnumPlaces()

	if code := f.run("hooks", "install", "codex", "--prompt-submit", "--dry-run"); code != 0 ||
		!strings.Contains(f.Out.String(), "would add to "+f.codexHooks()+": UserPromptSubmit") ||
		!strings.Contains(f.Out.String(), codexTrustStep) || len(f.hookCommands(f.codexHooks(), "UserPromptSubmit")) != 0 {
		t.Fatalf("dry run = %d\n%s%s", code, f.Out, f.Err)
	}
	if code := f.run("hooks", "install", "codex", "--prompt-submit"); code != 0 {
		t.Fatalf("install --prompt-submit = %d\n%s%s", code, f.Out, f.Err)
	}
	out := f.Out.String()
	for _, want := range []string{"added UserPromptSubmit", "codex: untrusted", "user_prompt_submit:0:0", codexTrustStep} {
		if !strings.Contains(out, want) {
			t.Errorf("install output lacks %q:\n%s", want, out)
		}
	}
	want := hookCommand(f.codexScript(), agenthooks.AgentCodex, agenthooks.NameUserPromptSubmit)
	if got := f.hookCommands(f.codexHooks(), "UserPromptSubmit"); len(got) != 1 || got[0] != want {
		t.Fatalf("UserPromptSubmit = %q, want %q", got, want)
	}

	f.trustPlaces("sha256:1", append(trusted, "user_prompt_submit:0:0")...)
	if code := f.run("hooks", "status", "codex"); code != 0 {
		t.Fatalf("status once trusted = %d %s", code, f.Out)
	}
	before := f.read(f.codexHooks())
	if code := f.run("hooks", "install", "codex"); code != 0 || f.read(f.codexHooks()) != before || !strings.Contains(f.Out.String(), "nothing to change") {
		t.Fatalf("an install without the flag = %d\n%s%s", code, f.Out, f.Err)
	}

	// A drifted UserPromptSubmit is magnum's entry like any other.
	at := strings.Index(before, "codex user_prompt_submit")
	if at < 0 {
		t.Fatalf("no UserPromptSubmit entry:\n%s", before)
	}
	f.write(f.codexHooks(), before[:at]+strings.Replace(before[at:], `"timeout": 5`, `"timeout": 9`, 1))
	if code := f.run("hooks", "status", "codex"); code != 1 || !strings.Contains(f.Out.String(), "UserPromptSubmit") {
		t.Fatalf("status of a drifted entry = %d %s", code, f.Out)
	}
	if code := f.run("hooks", "install", "codex"); code != 0 || !strings.Contains(f.Out.String(), "rewrote UserPromptSubmit") {
		t.Fatalf("an install without the flag over a drifted entry = %d\n%s%s", code, f.Out, f.Err)
	}
}
