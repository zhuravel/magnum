package cli

import (
	"testing"

	"github.com/zhuravel/magnum/internal/config"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// The matcher the board and the auto-approval's CI gate share
// (store.RequiredRuns) reads a whole workflow by the prefix the
// configuration validates required_checks with.
func TestTheRequiredWorkflowPrefixIsTheConfigs(t *testing.T) {
	if store.RequiredWorkflowPrefix != config.RequiredWorkflowPrefix {
		t.Fatalf("store.RequiredWorkflowPrefix = %q, config.RequiredWorkflowPrefix = %q", store.RequiredWorkflowPrefix, config.RequiredWorkflowPrefix)
	}
}

// Badges match labels whatever the case and leading emoji or symbols.
func TestPRsBadges(t *testing.T) {
	badges := map[string]config.BadgeSpec{"Flagged": {Text: "🚩"}, "schema migration": {Text: "\uf1c0", Color: "yellow"}}
	got := prsBadges([]string{"Bug", "⚠️ Schema Migration", "🚩 Flagged"}, badges)
	if len(got) != 2 || got[0] != (tui.Badge{Label: "🚩 Flagged", Text: "🚩"}) || got[1] != (tui.Badge{Label: "⚠️ Schema Migration", Text: "\uf1c0", Color: "yellow"}) {
		t.Fatalf("badges %+v", got)
	}
	if prsBadges([]string{"Bug"}, badges) != nil || prsBadges([]string{"🚩 Flagged"}, nil) != nil {
		t.Fatal("badges without a match")
	}
}

// The printed CI cell names required checks by their labels, with the
// finished count of a glob that matched several ("ci:passed 3/3"), never the
// raw pattern.
func TestPRsCICellRequired(t *testing.T) {
	ci := &tui.CIInfo{State: "passed", Required: []tui.CheckState{
		{Name: "ci / *", Label: "ci", State: "passed", Done: 3, Total: 3},
		{Name: "Completion", Label: "Completion", State: "missing"},
		{Name: "workflow:Lint", Label: "Lint", State: "pending", Done: 1, Total: 2},
	}}
	if got, want := prsCICell(ci), "ci:passed 3/3, Completion:missing, Lint:pending 1/2"; got != want {
		t.Fatalf("cell = %q, want %q", got, want)
	}
}
