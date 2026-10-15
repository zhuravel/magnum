package config

import (
	"slices"
	"testing"
)

// Codex 0.162 resumes a session from a directory other than the one the
// session recorded only after a "Working directory · resume" dialog, which
// magnum does not answer; a parked session restored into another slot would
// wait on it. The built-in codex kind resumes with tui.resume_cwd=current:
// the slot's directory, never the recorded one. A fresh session needs no
// such choice.
func TestACodexResumeTakesTheCurrentDirectory(t *testing.T) {
	codex := DefaultKinds()[KindCodex]
	resumed := codex.Argv(LaunchArgs{Session: "u-1", Wrapper: true})
	if want := []string{"resume", "u-1", "-c", "tui.resume_cwd=current"}; len(resumed) < len(want) || !slices.Equal(resumed[:len(want)], want) {
		t.Errorf("resume argv = %q, want it to start with %q", resumed, want)
	}
	if fresh := codex.Argv(LaunchArgs{Wrapper: true}); slices.Contains(fresh, "tui.resume_cwd=current") {
		t.Errorf("fresh argv = %q carries the resume setting", fresh)
	}
}

// An agent that stays in its pane's foreground after two ctrl+c is asked to
// exit by its kind's exit_keys before its pane is closed: claude types /exit
// and enter, one key each (herdr's send_keys takes key combos, never a
// word); codex has none, so its quit goes straight to the close. A user
// block sets them key by key like any other kind key, an explicit empty
// list turns them off, and an empty key is refused.
func TestExitKeysDefaultToSlashExitForClaudeOnly(t *testing.T) {
	kinds := DefaultKinds()
	if got, want := kinds[KindClaude].ExitKeys, []string{"/", "e", "x", "i", "t", "enter"}; !slices.Equal(got, want) {
		t.Errorf("claude exit_keys = %q, want %q", got, want)
	}
	for _, name := range []string{KindCodex, KindDroid, KindOMP} {
		if got := kinds[name].ExitKeys; len(got) != 0 {
			t.Errorf("%s exit_keys = %q, want none", name, got)
		}
	}
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + "[kinds.codex]\nexit_keys = [\"ctrl+d\"]\n"})
	if codex, _ := cfg.KindSpec(KindCodex); !slices.Equal(codex.ExitKeys, []string{"ctrl+d"}) {
		t.Errorf("codex exit_keys = %q, want [ctrl+d]", codex.ExitKeys)
	}
	if claude, _ := cfg.KindSpec(KindClaude); len(claude.ExitKeys) != 6 {
		t.Errorf("claude exit_keys = %q, want the default kept", claude.ExitKeys)
	}
	cfg = mustLoad(t, map[string]string{"config.toml": minimalConfig + "[kinds.claude]\nexit_keys = []\n"})
	if claude, _ := cfg.KindSpec(KindClaude); len(claude.ExitKeys) != 0 {
		t.Errorf("claude exit_keys = %q, want none", claude.ExitKeys)
	}
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + "[kinds.claude]\nexit_keys = [\"/\", \" \"]\n"})
	wantError(t, err, "exit_keys")
}
