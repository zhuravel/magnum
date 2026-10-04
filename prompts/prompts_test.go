package prompts

import (
	"slices"
	"testing"
)

// defaults lists the embedded default prompts.
var defaults = []string{
	"claude-rereview.md", "claude-restart.md", "claude-review.md", "claude-simplify.md", "codex-review.sh",
	"judge-continue.md", "judge-initial.md", "judge-nudge.md", "judge-recovery.md", "judge-rereview.md", "judge-stop.md",
	"model-fallback.md",
}

func TestNames(t *testing.T) {
	if got := Names(); !slices.Equal(got, defaults) {
		t.Fatalf("Names() = %v, want %v", got, defaults)
	}
	if _, ok := Read("README.md"); ok {
		t.Fatal("README.md must not be embedded")
	}
}
