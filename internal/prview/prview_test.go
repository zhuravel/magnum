package prview

import (
	"slices"
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
func TestCIMatchesRequiredChecksAcrossWorkflows(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	ci := &store.CIStatus{SHA: "h1", Total: 5, Passed: 3, Skipped: 1, Failed: 1, Checks: []store.CheckResult{
		{Name: "Completion", State: store.CheckSkipped, Workflow: "ci", At: t0},
		{Name: "Completion", State: store.CheckPassed, Workflow: "Pronto", At: t0.Add(time.Hour)},
		{Name: "rspec (3)", State: store.CheckFailed, Workflow: "ci", At: t0},
		{Name: "jest", State: store.CheckPassed, Workflow: "ci", At: t0},
		{Name: "pronto", State: store.CheckPassed, Workflow: "Pronto", At: t0},
	}}
	req := store.RequiredChecks{Checks: []string{"Completion", "workflow:ci", "build", "rspec*"}, Source: store.RequiredFromGitHub}
	got := CI(ci, "h1", req, Merge{})
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
	if s := CI(skipped, "h1", store.RequiredChecks{Checks: []string{"Completion"}}, Merge{}).Required[0].State; s != "skipped" {
		t.Fatalf("skipped required = %q", s)
	}
	allSkipped := &store.CIStatus{SHA: "h1", Total: 2, Skipped: 2, AllSkipped: true,
		Checks: []store.CheckResult{{Name: "a", State: store.CheckSkipped}, {Name: "b", State: store.CheckSkipped}}}
	if s := CI(allSkipped, "h1", store.RequiredChecks{}, Merge{}).State; s != "skipped" {
		t.Fatalf("a draft whose checks all skipped = %q", s)
	}
	stale := CI(&store.CIStatus{SHA: "old", Total: 1, Passed: 1, Checks: []store.CheckResult{{Name: "build", State: store.CheckPassed}}},
		"new", store.RequiredChecks{Checks: []string{"build"}}, Merge{})
	if !stale.Stale || stale.Required[0].State != "pending" || stale.Required[0].Total != 0 {
		t.Fatalf("an older commit's checks: %+v", stale)
	}
	if CI(nil, "h1", req, Merge{}) != nil {
		t.Fatal("unknown CI is not nil")
	}
}

// A glob's label drops its trailing wildcards and the separator before them,
// so the screens read "ci 3/3" rather than "ci / *"; a workflow pattern drops
// its prefix.
func TestCheckLabelDropsTheGlob(t *testing.T) {
	for pattern, want := range map[string]string{"ci / *": "ci", "rspec*": "rspec", "Workers Builds: *": "Workers Builds",
		"workflow:CI": "CI", "workflow:ci-*": "ci", "Completion": "Completion", "build_": "build_", "*": "*", "workflow:*": "workflow:*"} {
		if got := CheckLabel(pattern); got != want {
			t.Errorf("CheckLabel(%q) = %q, want %q", pattern, got, want)
		}
	}
}

// The failed checks keep their pages, named as Failing names them, for the
// card's CI line to link each failing job: a failed check that posted no
// page has none, and a passed one's page is not a failing job's.
func TestCILinksEachFailedCheckToItsPage(t *testing.T) {
	job := "https://github.com/example/widget/actions/runs/7001/job/9001"
	ci := &store.CIStatus{SHA: "h1", Total: 4, Failed: 3, Passed: 1, Checks: []store.CheckResult{
		{Name: "rspec (3)", State: store.CheckFailed, Workflow: "CI", URL: job},
		{Name: "coverage", State: store.CheckFailed, Status: true, URL: "https://ci.example.com/coverage/42"},
		{Name: "jest", State: store.CheckFailed, Workflow: "CI"},
		{Name: "lint", State: store.CheckPassed, Workflow: "CI", URL: job + "0"},
	}}
	got := CI(ci, "h1", store.RequiredChecks{}, Merge{})
	want := []tui.CheckLink{{Name: "CI / rspec (3)", URL: job}, {Name: "coverage", URL: "https://ci.example.com/coverage/42"}}
	if !slices.Equal(got.Links, want) {
		t.Errorf("links = %+v, want %+v", got.Links, want)
	}
}

// Why GitHub blocks the merge (its mergeStateStatus, read with the head's
// checks) says what magnum knows of it: the required checks that failed,
// did not run or have not finished, a draft, the reviews GitHub's gate
// still wants; a block none of those explains is a rule magnum does not
// read; a conflict or a branch behind its base is said as GitHub says it.
// GitHub's word on an older commit, a merge it does not block and one it
// has not computed say nothing, and GitHub's CLEAN never makes a required
// check that did not run green.
func TestCISaysWhyGitHubBlocksTheMerge(t *testing.T) {
	req := store.RequiredChecks{Checks: []string{"Completion", "build"}, Source: store.RequiredFromGitHub}
	checks := func(completion, build string) []store.CheckResult {
		var out []store.CheckResult
		for _, c := range [][2]string{{"Completion", completion}, {"build", build}} {
			if c[1] != "" {
				out = append(out, store.CheckResult{Name: c[0], State: c[1], Workflow: "CI"})
			}
		}
		return out
	}
	passed := checks(store.CheckPassed, store.CheckPassed)
	cases := []struct {
		name, merge string
		checks      []store.CheckResult
		sha         string
		m           Merge
		want        string
	}{
		{"a required check failed", "BLOCKED", checks(store.CheckFailed, store.CheckPassed), "h1", Merge{Decision: "APPROVED"},
			"GitHub blocks the merge: Completion failed"},
		{"a required check did not run", "BLOCKED", checks(store.CheckPassed, ""), "h1", Merge{Decision: "APPROVED"},
			"GitHub blocks the merge: build did not run"},
		{"required checks still run", "BLOCKED", checks(store.CheckPending, store.CheckPending), "h1", Merge{},
			"GitHub blocks the merge: Completion and build have not finished"},
		{"an approval is missing", "BLOCKED", passed, "h1", Merge{Decision: "REVIEW_REQUIRED"},
			"GitHub blocks the merge: a required approval is missing"},
		{"changes are requested", "BLOCKED", passed, "h1", Merge{Decision: "CHANGES_REQUESTED"},
			"GitHub blocks the merge: changes are requested"},
		{"a check and the reviews", "BLOCKED", checks(store.CheckFailed, ""), "h1", Merge{Decision: "REVIEW_REQUIRED"},
			"GitHub blocks the merge: Completion failed, build did not run, a required approval is missing"},
		{"a draft", "BLOCKED", passed, "h1", Merge{Draft: true}, "GitHub blocks the merge: a draft"},
		{"GitHub's own draft word", "DRAFT", passed, "h1", Merge{}, "GitHub blocks the merge: a draft"},
		{"nothing magnum knows explains it", "BLOCKED", passed, "h1", Merge{Decision: "APPROVED"},
			"GitHub blocks the merge: a rule magnum does not read"},
		{"a conflict", "DIRTY", passed, "h1", Merge{Decision: "APPROVED"}, "GitHub blocks the merge: it conflicts with its base"},
		{"a branch behind its base", "BEHIND", passed, "h1", Merge{Decision: "APPROVED"},
			"GitHub blocks the merge: its branch is behind its base"},
		{"an older commit's word", "BLOCKED", checks(store.CheckFailed, ""), "h0", Merge{}, ""},
		{"clean", "CLEAN", passed, "h1", Merge{}, ""},
		{"a failing check GitHub does not require", "UNSTABLE", passed, "h1", Merge{}, ""},
		{"hooks", "HAS_HOOKS", passed, "h1", Merge{}, ""},
		{"not computed yet", "UNKNOWN", checks(store.CheckFailed, ""), "h1", Merge{}, ""},
		{"an earlier build's read", "", checks(store.CheckFailed, ""), "h1", Merge{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ci := &store.CIStatus{SHA: tc.sha, Merge: tc.merge, Total: len(tc.checks), Checks: tc.checks}
			ci.Tally()
			if got := CI(ci, "h1", req, tc.m).Blocked; got != tc.want {
				t.Errorf("Blocked = %q, want %q", got, tc.want)
			}
		})
	}
	clean := &store.CIStatus{SHA: "h1", Merge: "CLEAN", Total: 1, Checks: checks(store.CheckPassed, "")}
	clean.Tally()
	if got := CI(clean, "h1", req, Merge{Decision: "APPROVED"}); got.Required[1].State != "missing" || got.Blocked != "" {
		t.Errorf("GitHub's CLEAN with a required check that did not run: %+v, Blocked %q", got.Required, got.Blocked)
	}
}
