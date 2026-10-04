package cli

import (
	"github.com/zhuravel/magnum/internal/config"
	"testing"
	"time"

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
	want := []tui.CheckState{{Name: "Completion", State: "passed"}, {Name: "workflow:ci", State: "failed"},
		{Name: "build", State: "missing"}, {Name: "rspec*", State: "failed"}}
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
	if !stale.Stale || stale.Required[0].State != "pending" {
		t.Fatalf("an older commit's checks: %+v", stale)
	}
	if prsCI(nil, "h1", req) != nil {
		t.Fatal("unknown CI is not nil")
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
