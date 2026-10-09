package cli

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// A required check is matched by name across workflows with the latest run
// winning (a manually posted Completion lands under another workflow, next
// to an older skipped one); "workflow:<glob>" takes a whole workflow; a
// required check that never ran on the head is missing; a skipped one stays
// skipped (GitHub would accept it, the board does not call it passed).
func TestPRsCIRequiredChecks(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	ci := &store.CIStatus{SHA: "h1", Total: 5, Passed: 3, Skipped: 1, Failed: 1, Checks: []store.CheckResult{
		{Name: "Completion", State: store.CheckSkipped, Workflow: "ci", At: t0},
		{Name: "Completion", State: store.CheckPassed, Workflow: "Pronto", At: t0.Add(time.Hour)},
		{Name: "rspec (3)", State: store.CheckFailed, Workflow: "ci", At: t0},
		{Name: "jest", State: store.CheckPassed, Workflow: "ci", At: t0},
		{Name: "pronto", State: store.CheckPassed, Workflow: "Pronto", At: t0},
	}}
	req := store.RequiredChecks{Checks: []string{"Completion", "workflow:ci", "build", "rspec*"}, Source: store.RequiredFromGitHub}
	got := prsCI(ci, "h1", req)
	want := []tui.CheckState{{Name: "Completion", Label: "Completion", State: "passed", Done: 1, Total: 1},
		{Name: "workflow:ci", Label: "ci", State: "failed", Done: 3, Total: 3},
		{Name: "build", Label: "build", State: "missing"}, {Name: "rspec*", Label: "rspec", State: "failed", Done: 1, Total: 1}}
	if len(got.Required) != len(want) {
		t.Fatalf("required %+v", got.Required)
	}
	for i := range want {
		if got.Required[i] != want[i] {
			t.Errorf("required[%d] = %+v, want %+v", i, got.Required[i], want[i])
		}
	}
	if got.State != "failed" || got.RequiredSource != "github" || len(got.Failing) != 1 || got.Failing[0] != "ci / rspec (3)" {
		t.Fatalf("ci %+v", got)
	}
	if len(got.Workflows) != 2 || got.Workflows[0].Name != "Pronto" || got.Workflows[0].State != "passed" || got.Workflows[1].State != "failed" {
		t.Fatalf("workflows %+v", got.Workflows)
	}

	skipped := &store.CIStatus{SHA: "h1", Total: 1, Skipped: 1, Checks: []store.CheckResult{{Name: "Completion", State: store.CheckSkipped, At: t0}}}
	if s := prsCI(skipped, "h1", store.RequiredChecks{Checks: []string{"Completion"}}).Required[0].State; s != "skipped" {
		t.Fatalf("skipped required = %q", s)
	}
	allSkipped := &store.CIStatus{SHA: "h1", Total: 2, Skipped: 2, AllSkipped: true,
		Checks: []store.CheckResult{{Name: "a", State: store.CheckSkipped}, {Name: "b", State: store.CheckSkipped}}}
	if s := prsCI(allSkipped, "h1", store.RequiredChecks{}).State; s != "skipped" {
		t.Fatalf("a draft whose checks all skipped = %q", s)
	}
	stale := prsCI(&store.CIStatus{SHA: "old", Total: 1, Passed: 1, Checks: []store.CheckResult{{Name: "build", State: store.CheckPassed}}},
		"new", store.RequiredChecks{Checks: []string{"build"}})
	if !stale.Stale || stale.Required[0].State != "pending" || stale.Required[0].Total != 0 {
		t.Fatalf("an older commit's checks: %+v", stale)
	}
	if prsCI(nil, "h1", req) != nil {
		t.Fatal("unknown CI is not nil")
	}
}

// A glob's label drops its trailing wildcards and the separator before them,
// so the board reads "ci 3/3" rather than "ci / *"; a workflow pattern drops
// its prefix.
func TestCheckLabel(t *testing.T) {
	for pattern, want := range map[string]string{"ci / *": "ci", "rspec*": "rspec", "Workers Builds: *": "Workers Builds",
		"workflow:CI": "CI", "workflow:ci-*": "ci", "Completion": "Completion", "build_": "build_", "*": "*", "workflow:*": "workflow:*"} {
		if got := checkLabel(pattern); got != want {
			t.Errorf("checkLabel(%q) = %q, want %q", pattern, got, want)
		}
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
