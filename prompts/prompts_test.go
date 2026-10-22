package prompts

import (
	"slices"
	"testing"
)

// defaults lists the embedded default prompts.
var defaults = []string{
	"claude-rereview.md", "claude-restart.md", "claude-review.md", "claude-simplify.md", "codex-review.sh",
	"fix.md", "judge-continue.md", "judge-initial.md", "judge-nudge.md", "judge-own-pass.md", "judge-recovery.md", "judge-rereview.md",
	"model-fallback.md", "notes-curate.md", "retro.md", "skill-edit.md", "triage.md", "whiteboard.md",
}

func TestNames(t *testing.T) {
	if got := Names(); !slices.Equal(got, defaults) {
		t.Fatalf("Names() = %v, want %v", got, defaults)
	}
	if _, ok := Read("README.md"); ok {
		t.Fatal("README.md must not be embedded")
	}
}

// The judge prompts' partials file is embedded with them, but it is no
// prompt a role can name, so Names leaves it out.
func TestThePartialsFileIsEmbeddedButIsNoPrompt(t *testing.T) {
	if text, ok := Read(Shared); !ok || text == "" {
		t.Fatalf("Read(%s) = %q, %v: the partials file must be embedded", Shared, text, ok)
	}
	if slices.Contains(Names(), Shared) {
		t.Fatalf("Names() = %v lists the partials file", Names())
	}
}
