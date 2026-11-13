package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// openNow is the look's time in the verdict tests.
var openNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// idleFacts are the facts of an own PR on which everything holds: magnum
// reviewed its head an hour ago and found nothing, a colleague approved,
// the read is whole, nothing is open and the settle passed. decideOpen
// says idle; each test takes one thing away.
func idleFacts() openFacts {
	head := fixHead
	activity := openNow.Add(-30 * time.Minute)
	return openFacts{
		Now: openNow,
		PR: store.PR{ID: 2, Number: 2, URL: prPage, HeadSHA: head, ReviewedSHA: &head, State: store.PRReviewed, HeadChangedAt: openNow.Add(-time.Hour),
			ActivityAt: &activity, ReviewGate: &store.ReviewGate{Decision: "APPROVED"}},
		Sum:    &store.ReviewSummary{RunID: "r1", ReviewID: 101, SHA: head, URL: prPage + "#pullrequestreview-101", At: openNow.Add(-50 * time.Minute)},
		Whole:  true,
		Settle: openSettle{Quiet: 3 * time.Minute, Max: 20 * time.Minute, Late: 2 * time.Hour, HandsOff: 30 * time.Minute, MaxLoops: 3},
		Ledger: openLedger{FixerHeads: []string{head}},
	}
}

// personThread is a colleague's unanswered thread item.
func personThread(id int64) openItem {
	return openItem{Key: threadKey(id, id), Class: classPerson, Kind: itemThread, URL: prPage + "#discussion_r" + itoa(id)}
}

// wantVerdict fails unless v is verdict and its why holds why.
func wantVerdict(t *testing.T, v OpenVerdict, verdict, why string) {
	t.Helper()
	if v.Verdict != verdict || !strings.Contains(v.Why, why) {
		t.Errorf("verdict = %s %q, want %s %q", v.Verdict, v.Why, verdict, why)
	}
}

// Red required CI the look gathered no CI fix facts for needs you, as does
// the rollup's failure (a repository without a required list), which no CI
// fix covers: ci_fix_test.go has the CI fix's own verdicts.
func TestRedRequiredCIWithoutACIFixNeedsYou(t *testing.T) {
	pr := idleFacts().PR
	pr.CI = &store.CIStatus{SHA: fixHead, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{{Name: "rspec", State: store.CheckFailed}}}
	ci := openCIOf(pr, store.RequiredChecks{Checks: []string{"rspec"}, Source: store.RequiredFromGitHub}, false, nil)
	f := idleFacts()
	f.CI = ci
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "required check rspec failed at a1a1a1a")
	if len(v.Needs) != 1 || v.Needs[0].Kind != "ci" { // a check that ran and failed, never ci_missing
		t.Errorf("needs = %+v, want the PR's failed CI (ci)", v.Needs)
	}
	failure := "FAILURE"
	pr.CI, pr.CIState = nil, &failure // the stored checks cannot say: the rollup decides
	f.CI = openCIOf(pr, store.RequiredChecks{}, false, nil)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "CI failed at a1a1a1a")
}

// Pending required CI waits, and so does a required check the head has not
// started.
func TestPendingRequiredCIWaits(t *testing.T) {
	pr := idleFacts().PR
	pr.CI = &store.CIStatus{SHA: fixHead, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{{Name: "rspec", State: store.CheckPending}}}
	f := idleFacts()
	f.CI = openCIOf(pr, store.RequiredChecks{Checks: []string{"rspec"}, Source: store.RequiredFromConfig}, false, nil)
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a")
	pr.CI = nil
	f.CI = openCIOf(pr, store.RequiredChecks{Checks: []string{"rspec"}, Source: store.RequiredFromConfig}, false, nil)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a")
}

// ghRequired is GitHub's list of the checks main, the default branch,
// requires.
func ghRequired(checks ...string) store.GitHubRequiredChecks {
	return store.GitHubRequiredChecks{Branch: "main", Checks: checks, Known: true}
}

// othersPassedPR is idleFacts' PR into main, past settle_max, at whose head
// only other apps' checks ran, and passed: the rollup is SUCCESS.
func othersPassedPR() store.PR {
	success, main := "SUCCESS", "main"
	pr := idleFacts().PR // its head moved an hour ago, past settle_max
	pr.BaseRef, pr.CIState = &main, &success
	pr.CI = &store.CIStatus{SHA: fixHead, State: success, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{
		{Name: "pronto", Workflow: "Pronto", WorkflowFile: "pronto.yml", State: store.CheckPassed},
		{Name: "Devin Review", State: store.CheckPassed}}}
	return pr
}

// A required check with no run at a head whose checks magnum read whole is
// never green where something makes it required there: GitHub's rulesets
// for the PR's base (whatever list decides the rest), or a [[repo]]
// required_checks list of a repository that sets ci_dispatch (strict). The
// repository's CI runs only when a PR opens, so a later push, the fixer's
// included, gets no run of Completion: GitHub blocked the merge while the
// rollup of the other checks said SUCCESS, and the look called the PR
// idle. While the head is younger than settle_max the run may not have
// started yet, and it waits; after that the check did not run, and the
// verdict needs you, says which check at which head and how to start it. A
// required workflow is named as one, and the others are counted.
func TestARequiredCheckThatDidNotRunAtTheHeadIsNeverGreen(t *testing.T) {
	pr := othersPassedPR()
	for _, tc := range []struct {
		name     string
		required store.RequiredChecks
		strict   bool
		enforced []string
		what     string
	}{
		{"GitHub's rulesets", store.RequiredChecks{Checks: []string{"Completion"}, Source: store.RequiredFromGitHub}, false,
			enforcedMissing(pr, ghRequired("Completion")), "required check Completion"},
		{"GitHub's beside required_checks", store.RequiredChecks{Checks: []string{"pronto"}, Source: store.RequiredFromConfig}, false,
			enforcedMissing(pr, ghRequired("Completion", "pronto")), "required check Completion"},
		{"required_checks", store.RequiredChecks{Checks: []string{"Completion"}, Source: store.RequiredFromConfig}, true, nil, "required check Completion"},
		{"a workflow", store.RequiredChecks{Checks: []string{"workflow:CI"}, Source: store.RequiredFromConfig}, true, nil, "required workflow CI"},
		{"two", store.RequiredChecks{Checks: []string{"Completion", "pronto", "Lint"}, Source: store.RequiredFromConfig}, true,
			enforcedMissing(pr, ghRequired("Completion")), "required check Completion and 1 more"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := idleFacts()
			f.PR = pr
			f.CI = openCIOf(pr, tc.required, tc.strict, tc.enforced)
			v, _, _ := decideOpen(f)
			wantVerdict(t, v, OpenNeedsYou, tc.what+" did not run at a1a1a1a: run its workflow on the PR's branch")
			if len(v.Needs) != 1 || v.Needs[0].Kind != "ci_missing" || v.Needs[0].URL != prPage {
				t.Errorf("needs = %+v, want the PR's CI that did not run (ci_missing)", v.Needs)
			}
			f.PR.HeadChangedAt = openNow.Add(-5 * time.Minute) // pushed five minutes ago: its run may not have started
			v, _, _ = decideOpen(f)
			wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: "+tc.what+" has not started")
			if want := f.PR.HeadChangedAt.Add(20 * time.Minute); !v.Until.Equal(want) {
				t.Errorf("until = %s, want settle_max after the push (%s)", v.Until, want)
			}
		})
	}
}

// A required check with no run at the head that nothing makes required
// there leaves rule 11 to the rollup, as before, and the PR is idle once
// the rest passed: GitHub's list for a PR stacked on another branch (the
// list is the default branch's), a list GitHub would not give, a [[repo]]
// list without ci_dispatch (its workflow's path filters may skip the
// head), and a check GitHub names as a context a glob would not match.
func TestARequiredCheckWithNoRunThatNothingEnforcesLeavesTheLookToTheRollup(t *testing.T) {
	stacked := othersPassedPR()
	feature := "feature-a"
	stacked.BaseRef = &feature
	bracketed := othersPassedPR()
	bracketed.CI.Checks = append(bracketed.CI.Checks, store.CheckResult{Name: "lint [py]", State: store.CheckPassed})
	unknown := ghRequired("Completion")
	unknown.Known = false
	for _, tc := range []struct {
		name     string
		pr       store.PR
		required store.RequiredChecks
		enforced []string
	}{
		{"a stacked PR", stacked, store.RequiredChecks{Checks: []string{"Completion"}, Source: store.RequiredFromGitHub},
			enforcedMissing(stacked, ghRequired("Completion"))},
		{"a list GitHub would not give", othersPassedPR(), store.RequiredChecks{}, enforcedMissing(othersPassedPR(), unknown)},
		{"required_checks without ci_dispatch", othersPassedPR(), store.RequiredChecks{Checks: []string{"workflow:CI"}, Source: store.RequiredFromConfig}, nil},
		{"a context with brackets", bracketed, store.RequiredChecks{Checks: []string{"lint [py]"}, Source: store.RequiredFromGitHub},
			enforcedMissing(bracketed, ghRequired("lint [py]"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := idleFacts()
			f.PR = tc.pr
			f.CI = openCIOf(tc.pr, tc.required, false, tc.enforced)
			v, _, _ := decideOpen(f)
			wantVerdict(t, v, OpenIdle, "nothing open at a1a1a1a")
		})
	}
}

// A required check its workflow creates only once the jobs it needs end
// (Completion `needs:` the rest of CI) has no run while they run, however
// long: rule 11 waits for it while a check of the head has not finished,
// past settle_max too, and never asks for a start; after a start of its
// own it keeps waiting while the head's checks run, past settle_max after
// the start, and says that no run came only once nothing runs.
func TestAMissingRequiredCheckWaitsWhileTheHeadsChecksRun(t *testing.T) {
	pending := "PENDING"
	pr := othersPassedPR()
	pr.CIState = &pending
	pr.CI.Checks = []store.CheckResult{
		{Name: "RSpec (3)", Workflow: "CI", WorkflowFile: "ci.yml", State: store.CheckPending},
		{Name: "Checks", Workflow: "CI", WorkflowFile: "ci.yml", State: store.CheckPassed}}
	f := idleFacts()
	f.PR, f.CIWorkflow = pr, "ci.yml"
	f.CI = openCIOf(pr, store.RequiredChecks{Checks: []string{"Completion"}, Source: store.RequiredFromGitHub}, false,
		enforcedMissing(pr, ghRequired("Completion")))
	if !f.CI.Running || len(f.CI.Missing) != 1 {
		t.Fatalf("ci = %+v, want Completion missing while the head's checks run", f.CI)
	}
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check Completion has not started")
	if v.Dispatch || !v.Until.IsZero() {
		t.Errorf("dispatch %v, until %s while the head's checks run", v.Dispatch, v.Until)
	}
	f.CIStart = ciStart{Head: fixHead, Workflow: "ci.yml", At: openNow.Add(-time.Hour)} // older than settle_max
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	f.CI.Running = false
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "and no run came")
}

// A branch GitHub says requires no checks leaves CI out of idle, even with
// a red rollup.
func TestAKnownEmptyRequiredListLeavesCIOut(t *testing.T) {
	pr := idleFacts().PR
	failure := "FAILURE"
	pr.CIState = &failure
	if ci := openCIOf(pr, store.RequiredChecks{Checks: []string{}, Source: store.RequiredFromGitHub}, false, nil); ci.Failed != "" || ci.Pending || len(ci.Missing) > 0 {
		t.Errorf("ci = %+v, want nothing", ci)
	}
}

// The verdict says kinds, counts, links, short SHAs and times: no body of a
// thread, a review or a comment reaches it.
func TestTheVerdictCarriesNoPRText(t *testing.T) {
	const secret = "SENTINEL-PR-TEXT"
	th := extThread(4100, "app/a.rb", 3, openNow.Add(-time.Hour), secret+" rename this", "rev-ann")
	comments := []github.PRComment{{ID: 701, URL: prPage + "#issuecomment-701", Body: secret + " a note", CreatedAt: openNow.Add(-time.Hour),
		AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"}}
	read := readFor([]github.Thread{th}, nil, comments, nil, nil)
	for _, handed := range []bool{false, true} {
		f := idleFacts()
		f.Items = openItemsOf(itemsInput{read: read, sum: f.Sum, head: fixHead})
		if handed {
			f.Ledger.Handed = map[string]time.Time{}
			for _, it := range f.Items {
				f.Ledger.Handed[it.Key] = openNow
			}
		}
		v, _, _ := decideOpen(f)
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), secret) || len(f.Items) != 2 {
			t.Errorf("handed %v: verdict %s (items %v)", handed, b, f.Items)
		}
	}
}
