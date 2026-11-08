package prompts

import (
	"slices"
	"testing"
)

// defaults lists the embedded default prompts.
var defaults = []string{
	"claude-rereview.md", "claude-restart.md", "claude-review-local.md", "claude-review.md", "claude-simplify.md", "codex-review.sh",
	"factory-amend.md", "factory-cause.md", "factory-critic.md", "factory-history.md", "factory-judge.md", "factory-liaison.md", "factory-points.md", "factory-pr-body.md", "factory-qa-fix.md", "factory-qa.md", "factory-repro.md", "factory-review-fix.md",
	"factory-scope.md", "factory-writer.md", "fix.md", "judge-continue.md", "judge-initial.md", "judge-nudge.md", "judge-own-pass.md", "judge-recovery.md", "judge-rereview.md",
	"model-fallback.md", "notes-critic.md", "notes-curate.md", "retro.md", "skill-edit.md", "triage.md", "whiteboard.md",
}

func TestNames(t *testing.T) {
	if got := Names(); !slices.Equal(got, defaults) {
		t.Fatalf("Names() = %v, want %v", got, defaults)
	}
	if _, ok := Read("README.md"); ok {
		t.Fatal("README.md must not be embedded")
	}
}

// The judge prompts' and the factory prompts' partials files are embedded
// with them, but neither is a prompt a role can name, so Names leaves both
// out.
func TestThePartialsFilesAreEmbeddedButAreNoPrompts(t *testing.T) {
	for _, name := range []string{Shared, FactoryShared} {
		if text, ok := Read(name); !ok || text == "" {
			t.Fatalf("Read(%s) = %q, %v: the partials file must be embedded", name, text, ok)
		}
		if slices.Contains(Names(), name) {
			t.Fatalf("Names() = %v lists the partials file %s", Names(), name)
		}
	}
}
