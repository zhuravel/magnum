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
