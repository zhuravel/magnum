package store

import (
	"reflect"
	"testing"
	"time"
)

// seenChecks are a head's checks on talkable/talkable on 2026-10-26: a
// third-party app's check that fails on every PR, beside the PR's own.
func seenChecks(rspec string) []CheckResult {
	at := time.Date(2026, 10, 26, 13, 35, 0, 0, time.UTC)
	return []CheckResult{
		{Name: "Vendor Gates", State: CheckFailed, At: at},
		{Name: "rspec", State: rspec, Workflow: "CI", WorkflowFile: "ci.yml", At: at},
		{Name: "lint", State: CheckSkipped, Workflow: "CI", WorkflowFile: "ci.yml", At: at},
	}
}

func tallied(ci CIStatus) *CIStatus {
	ci.Tally()
	return &ci
}

// SeenCI is the one definition of the checks magnum sees: the checks
// ignore_checks names leave the list, the counts and Total, and GitHub's
// rollup, which an ignored check that failed holds at FAILURE, is derived
// again from the rest: failed when one of them failed, even while another
// still runs (a pending state would let the judge's APPROVE through on a
// head whose RSpec failed), pending when one runs and none failed, success
// when they passed or skipped, none when nothing is left. Checks of
// another head, or a list GitHub cut short, keep the rollup: magnum cannot
// tell what the rest did. A list that ignores none of the checks, or none
// at all, returns the stored CI itself.
func TestSeenCILeavesOutTheIgnoredChecksAndDerivesTheStateFromTheRest(t *testing.T) {
	ignore := []string{"Vendor *"}
	for _, tc := range []struct {
		name      string
		rspec     string
		lint      string
		state     string
		wantState string
	}{
		{"the rest passed", CheckPassed, CheckSkipped, "FAILURE", "SUCCESS"},
		{"another check failed", CheckFailed, CheckSkipped, "FAILURE", "FAILURE"},
		{"another check runs", CheckPending, CheckSkipped, "FAILURE", "PENDING"},
		{"another check failed while a third runs", CheckFailed, CheckPending, "FAILURE", "FAILURE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := func() []CheckResult {
				c := seenChecks(tc.rspec)
				c[2].State = tc.lint
				return c
			}
			ci := tallied(CIStatus{SHA: "h1", State: tc.state, Total: 3, Complete: true, ByWorkflowFile: true,
				Checks: checks(), Rollup: CheckCounts{Failed: 2, Passed: 1, Total: 3}})
			before := *ci
			got := SeenCI(ci, "h1", ignore)
			want := tallied(CIStatus{SHA: "h1", State: tc.wantState, Total: 2, Complete: true, ByWorkflowFile: true,
				Checks: checks()[1:], Rollup: CheckCounts{Failed: 2, Passed: 1, Total: 3}})
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("SeenCI = %+v\nwant %+v", got, want)
			}
			if !reflect.DeepEqual(*ci, before) {
				t.Fatalf("SeenCI changed the stored CI: %+v", ci)
			}
		})
	}

	all := tallied(CIStatus{SHA: "h1", State: "FAILURE", Total: 1, Complete: true, Checks: seenChecks(CheckPassed)[:1]})
	if got := SeenCI(all, "h1", ignore); got.State != "" || got.Total != 0 || len(got.Checks) != 0 || got.Failed != 0 || got.AllSkipped {
		t.Fatalf("every check ignored: %+v, want no checks", got)
	}

	// The rollup stays where magnum cannot tell what the rest did; the list
	// and the counts still leave the ignored check out.
	for name, ci := range map[string]*CIStatus{
		"another head's checks": tallied(CIStatus{SHA: "h0", State: "FAILURE", Total: 3, Complete: true, Checks: seenChecks(CheckPassed)}),
		"a list GitHub cut":     tallied(CIStatus{SHA: "h1", State: "FAILURE", Total: 140, Complete: false, Checks: seenChecks(CheckPassed)}),
	} {
		got := SeenCI(ci, "h1", ignore)
		if got.State != "FAILURE" || got.Failed != 0 || got.Passed != 1 || len(got.Checks) != 2 || got.Total != ci.Total-1 {
			t.Errorf("%s: SeenCI = %+v, want the rollup kept and the ignored check left out", name, got)
		}
	}

	ci := tallied(CIStatus{SHA: "h1", State: "FAILURE", Total: 3, Complete: true, Checks: seenChecks(CheckPassed)})
	for name, ignore := range map[string][]string{"no list": nil, "a list that names none": {"Other", "workflow:Vendor*"}} {
		if got := SeenCI(ci, "h1", ignore); got != ci {
			t.Errorf("%s: SeenCI = %+v, want the stored CI itself", name, got)
		}
	}
	if SeenCI(nil, "h1", ignore) != nil {
		t.Fatal("SeenCI(nil) is not nil")
	}

	// A workflow entry names its workflow's checks by file or declared
	// name, as required_checks does (RequiredNames).
	if got := SeenCI(ci, "h1", []string{"workflow:ci.yml"}); len(got.Checks) != 1 || got.Checks[0].Name != "Vendor Gates" || got.State != "FAILURE" {
		t.Fatalf("workflow:ci.yml: %+v", got)
	}
}

// A PR as magnum sees it carries the derived state as its rollup too
// (prs.ci_state, which the approval's CI gate reads), unless the rollup
// moved since its checks were read (a Details fetch that failed: the
// stored checks trail it), the checks are not the head's or not every one;
// a PR whose rollup the registry never read stays that way.
func TestAPRIgnoringChecksTakesTheDerivedStateAsItsRollup(t *testing.T) {
	ignore := []string{"Vendor Gates"}
	pr := func(head, rollup string, ci CIStatus) PR {
		p := PR{HeadSHA: head, CI: tallied(ci)}
		if rollup != "-" {
			p.CIState = &rollup
		}
		return p
	}
	head := CIStatus{SHA: "h1", State: "FAILURE", Total: 3, Complete: true, Checks: seenChecks(CheckPassed)}
	for _, tc := range []struct {
		name string
		pr   PR
		want string // "-" = nil
	}{
		{"the head's checks", pr("h1", "FAILURE", head), "SUCCESS"},
		{"a rollup that moved since", pr("h1", "PENDING", head), "PENDING"},
		{"another head's checks", pr("h2", "FAILURE", head), "FAILURE"},
		{"a rollup never read", pr("h1", "-", head), "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.pr.IgnoringChecks(ignore)
			state := "-"
			if got.CIState != nil {
				state = *got.CIState
			}
			if state != tc.want {
				t.Fatalf("CIState = %q, want %q", state, tc.want)
			}
			if len(got.CI.Checks) != 2 || got.CI.Failed != 0 {
				t.Fatalf("CI = %+v, want the ignored check left out", got.CI)
			}
			if len(tc.pr.CI.Checks) != 3 || (tc.pr.CIState != nil && *tc.pr.CIState == "SUCCESS") {
				t.Fatalf("IgnoringChecks changed the PR it was given: %+v %v", tc.pr.CI, tc.pr.CIState)
			}
		})
	}
	p := pr("h1", "FAILURE", head)
	if got := p.IgnoringChecks(nil); got.CI != p.CI || got.CIState != p.CIState {
		t.Fatal("no list changed the PR")
	}
}
