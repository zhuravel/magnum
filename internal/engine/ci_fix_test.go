package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// ciBaseSHA is the base's commit the CI fix tests' base reads decide on.
var ciBaseSHA = strings.Repeat("c0", 20)

// completionRequired is GitHub's list of talkable's required checks.
var completionRequired = store.RequiredChecks{Checks: []string{"Completion"}, Source: store.RequiredFromGitHub}

// ciFailedFacts are idleFacts whose head failed the required check
// Completion (of ci.yml), and whose base ran it green at ciBaseSHA: a CI
// fix may start.
func ciFailedFacts() openFacts {
	f := idleFacts()
	f.pr.CI = &store.CIStatus{SHA: fixHead, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{
		{Name: "Completion", Workflow: "CI", WorkflowFile: "ci.yml", State: store.CheckFailed, At: openNow.Add(-40 * time.Minute)}}}
	f.ci = openCIOf(f.pr, completionRequired, false, nil)
	f.ciFix = ciFixFacts{on: true, base: ciBasePassed, baseSHA: ciBaseSHA, baseName: "main"}
	return f
}

// wantNeedsCI fails unless v needs you for the PR's failed CI alone (kind
// ci, the words a failed check's: " failed at ", never " did not run at ").
func wantNeedsCI(t *testing.T, v OpenVerdict, work []openItem, why string) {
	t.Helper()
	wantVerdict(t, v, OpenNeedsYou, why)
	if len(work) != 0 || len(v.Needs) != 1 || v.Needs[0].Kind != "ci" || !strings.Contains(v.Why, " failed at ") || strings.Contains(v.Why, " did not run at ") {
		t.Errorf("work %v, needs %+v, why %q: want the PR's failed CI alone", work, v.Needs, v.Why)
	}
}

// The operator's answer of 2026-11-08 (Q3): red required CI on an own PR
// whose base runs the same check green is the fixer's. With nothing else
// open, rule 9 starts a CI fix, the first of three.
func TestAFailedRequiredCheckGreenOnTheBaseStartsACIFix(t *testing.T) {
	v, work, _ := decideOpen(ciFailedFacts())
	wantVerdict(t, v, OpenFixing, "starts a CI fix of required check Completion at a1a1a1a (CI fix 1 of 3)")
	if len(work) != 1 || work[0].Kind != itemCI || work[0].Key != "ci:a1a1a1a" {
		t.Fatalf("work = %+v, want the CI item", work)
	}
	f := ciFailedFacts()
	f.ciFix.starts = 1
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenFixing, "(CI fix 2 of 3)")
}

// Findings first: a thread, or magnum's review of the head with work,
// makes the work that alone, and the CI waits for the push it brings.
func TestCIFixWaitsForFindingsFirst(t *testing.T) {
	f := ciFailedFacts()
	f.items = []openItem{personThread(4100)}
	_, work, _ := decideOpen(f)
	if len(work) != 1 || work[0].Kind != itemThread {
		t.Fatalf("work = %+v, want the thread alone", work)
	}
	f = ciFailedFacts()
	f.items = []openItem{{Key: reviewKey("r1"), Class: classMagnum, Kind: itemFindings, URL: prPage}}
	v, work, _ := decideOpen(f)
	if len(work) != 1 || work[0].Kind != itemFindings || strings.Contains(v.Why, "CI fix") {
		t.Fatalf("verdict %q, work = %+v, want magnum's review alone", v.Why, work)
	}
}

// A check that fails on the base too is not the PR's to fix, and one the
// base never ran gives magnum nothing to compare: both need you, with the
// base's word; one that still runs on the base waits, and one whose base
// was not read yet asks for the read.
func TestARequiredCheckRedOnTheBaseNeedsYou(t *testing.T) {
	f := ciFailedFacts()
	f.ciFix.base = ciBaseFailed
	v, work, _ := decideOpen(f)
	wantNeedsCI(t, v, work, "required check Completion failed at a1a1a1a, and on main at c0c0c0c too: magnum fixes only a check that passes on the base")
}

func TestARequiredCheckTheBaseNeverRanNeedsYou(t *testing.T) {
	f := ciFailedFacts()
	f.ciFix.base, f.ciFix.baseSHA = ciBaseNever, ""
	v, work, _ := decideOpen(f)
	wantNeedsCI(t, v, work, "required check Completion failed at a1a1a1a; main's last 5 commits never ran it: magnum fixes only a check that passes on the base")
	f.ciFix.base, f.ciFix.baseSHA = ciBaseRunning, ciBaseSHA
	v, work, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "required check Completion failed at a1a1a1a; it still runs on main at c0c0c0c")
	if len(work) != 0 {
		t.Fatal(work)
	}
	f.ciFix.base, f.ciFix.baseDue = "", true
	v, work, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "required check Completion failed at a1a1a1a; reading main's CI")
	if !v.readCIBase || len(work) != 0 {
		t.Fatalf("readCIBase %v, work %v", v.readCIBase, work)
	}
	f.low = true
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "reading main's CI waits for the GitHub budget")
	if v.readCIBase {
		t.Error("the base is read below the GitHub floor")
	}
}

// A failed check beside one that still runs waits for it: the run may
// change the failed set.
func TestCIFixWaitsWhileARequiredCheckStillRuns(t *testing.T) {
	f := ciFailedFacts()
	f.pr.CI.Checks = append(f.pr.CI.Checks, store.CheckResult{Name: "Build", WorkflowFile: "build.yml", State: store.CheckPending})
	f.ci = openCIOf(f.pr, store.RequiredChecks{Checks: []string{"Completion", "Build"}, Source: store.RequiredFromGitHub}, false, nil)
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "required check Completion failed at a1a1a1a; a required check still runs")
	if len(work) != 0 || len(v.Needs) != 0 {
		t.Fatalf("work %v, needs %v", work, v.Needs)
	}
}

// After three CI fixes the PR's CI is the operator's.
func TestThreeCIFixesNeedYou(t *testing.T) {
	f := ciFailedFacts()
	f.ciFix.starts = ciFixMax
	v, work, _ := decideOpen(f)
	wantNeedsCI(t, v, work, "required check Completion failed at a1a1a1a after 3 CI fixes: the CI is yours")
}

// Rule 9's gates hold a CI fix as they hold any start: the operator's own
// push, the settle, `magnum pause`, quiet hours and the day's cap of loops,
// whose words keep the factory's "f starts another".
func TestCIFixKeepsHandsOffSettlePauseQuietHoursAndTheDailyCap(t *testing.T) {
	f := ciFailedFacts()
	f.ledger.FixerHeads = nil
	f.pr.HeadChangedAt = openNow.Add(-10 * time.Minute)
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "you pushed a1a1a1a at "+waitClock(f.pr.HeadChangedAt, openNow)+"; hands off until "+
		waitClock(f.pr.HeadChangedAt.Add(30*time.Minute), openNow))
	if len(work) != 0 {
		t.Fatal(work)
	}
	f = ciFailedFacts()
	f.pr.HeadChangedAt = openNow.Add(-10 * time.Minute)
	activity := openNow.Add(-time.Minute)
	f.pr.ActivityAt = &activity
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "settling: the CI fix of required check Completion waits until "+waitClock(activity.Add(3*time.Minute), openNow))
	f = ciFailedFacts()
	f.pause = "magnum paused"
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum paused: the CI fix of required check Completion waits for `magnum resume`")
	f = ciFailedFacts()
	f.settle.quietHours = "03:00-10:00"
	f.now = time.Date(2026, 11, 7, 4, 0, 0, 0, time.Local)
	f.pr.HeadChangedAt, f.ledger.Spell = f.now.Add(-time.Hour), openSpell{}
	quietActivity := f.now.Add(-30 * time.Minute)
	f.pr.ActivityAt = &quietActivity
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "quiet hours (03:00-10:00): the CI fix of required check Completion waits until 10:00")
	f = ciFailedFacts()
	f.ledger.Starts = []time.Time{openNow.Add(-time.Hour), openNow.Add(-2 * time.Hour), openNow.Add(-3 * time.Hour)}
	v, work, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "3 loops today: f starts another, c a CI fix")
	if len(work) != 0 || len(v.Needs) != 1 || v.Needs[0].Kind != "loop" {
		t.Errorf("work %v, needs %+v", work, v.Needs)
	}
}

// A CI fix whose fixer could not fix the failure ends quietly, and its
// class word says why: the head then needs you, and nothing starts again
// there. A CI fix that failed or was lost needs you too.
func TestTheFixerSaidCannotNeedsYouAndStartsNothingAgainAtTheHead(t *testing.T) {
	for class, words := range map[string]string{
		"flaky": "a flaky test, not the PR's code", "infra": "an infrastructure failure", "secret": "a missing secret",
		"config": "a CI config change it may not make", "base": "a failure on the base", "unfixed": "a failure it could not fix",
	} {
		f := ciFailedFacts()
		f.ciFix.starts = 1
		f.ciFix.atHead = ciFixStart{Head: fixHead, Outcome: ciOutcomeCannot, Why: class}
		v, work, _ := decideOpen(f)
		wantNeedsCI(t, v, work, "required check Completion failed at a1a1a1a: the CI fix found "+words)
	}
	for _, outcome := range []string{ciOutcomeFailed, ciOutcomeLost} {
		f := ciFailedFacts()
		f.ciFix.starts = 1
		f.ciFix.atHead = ciFixStart{Head: fixHead, Outcome: outcome}
		v, work, _ := decideOpen(f)
		wantNeedsCI(t, v, work, "required check Completion failed at a1a1a1a: the CI fix at a1a1a1a failed; it is yours")
	}
}

// The PR's own files decide before anything is read: one that changes its
// CI config (a workflow, an action) gets no CI fix, nor one whose file
// list GitHub cut short; one not listed at the head waits for the list.
func TestAPRThatChangesItsCIConfigGetsNoCIFix(t *testing.T) {
	runs := ciFailedFacts().ci.failedRuns
	for _, files := range [][]string{{".github/workflows/ci.yml"}, {".github/actions/setup/action.yml"}, {"tools/action.yaml"}} {
		refused, wait := ciFixRefusal(store.PRFiles{HeadSHA: fixHead, Paths: append([]string{"app/a.rb"}, files...)}, true, fixHead, runs)
		if refused != "the PR changes its own workflows or actions, so its CI is yours" || wait {
			t.Errorf("files %q: %q, wait %v", files, refused, wait)
		}
	}
	if refused, _ := ciFixRefusal(store.PRFiles{HeadSHA: fixHead, Paths: []string{"app/a.rb"}, Truncated: true}, true, fixHead, runs); !strings.Contains(refused, "did not list every file") {
		t.Errorf("a cut list: %q", refused)
	}
	if refused, wait := ciFixRefusal(store.PRFiles{HeadSHA: "e0e0", Paths: []string{"app/a.rb"}}, true, fixHead, runs); refused != "" || !wait {
		t.Errorf("another head's list: %q, wait %v", refused, wait)
	}
	if refused, wait := ciFixRefusal(store.PRFiles{HeadSHA: fixHead, Paths: []string{"app/a.rb"}}, true, fixHead, runs); refused != "" || wait {
		t.Errorf("the PR's own code: %q, wait %v", refused, wait)
	}
	f := ciFailedFacts()
	f.ciFix.refused = "the PR changes its own workflows or actions, so its CI is yours"
	v, work, _ := decideOpen(f)
	wantNeedsCI(t, v, work, "required check Completion failed at a1a1a1a: the PR changes its own workflows or actions, so its CI is yours")
}

// A failed required check that is no GitHub Actions job (another app's
// check, a commit status) leaves magnum no log to read.
func TestAFailureOfNoActionsJobNeedsYou(t *testing.T) {
	refused, _ := ciFixRefusal(store.PRFiles{HeadSHA: fixHead, Paths: []string{"app/a.rb"}}, true, fixHead,
		[]ciFailedCheck{{Entry: "pronto/eslint_npm", Name: "pronto/eslint_npm"}})
	if refused != "no GitHub Actions job, so magnum cannot read why" {
		t.Errorf("refused = %q", refused)
	}
}

// talkable's Completion needs the test jobs without always(): GitHub skips
// it when one fails, and a skipped required check read as green. Beside a
// failed job of its workflow file it is a failed required check, whose
// base comparison is that job's.
func TestACompletionSkippedBesideAFailedJobOfItsWorkflowIsAFailedRequiredCheck(t *testing.T) {
	pr := idleFacts().pr
	pr.CI = &store.CIStatus{SHA: fixHead, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{
		{Name: "Capybara", Workflow: "CI", WorkflowFile: "ci.yml", State: store.CheckFailed},
		{Name: "RSpec", Workflow: "CI", WorkflowFile: "ci.yml", State: store.CheckPassed},
		{Name: "Completion", Workflow: "CI", WorkflowFile: "ci.yml", State: store.CheckSkipped},
		{Name: "Dependency Audit", Workflow: "Audit", WorkflowFile: "audit.yml", State: store.CheckFailed}}}
	ci := openCIOf(pr, completionRequired, false, nil)
	if ci.failed != "required check Completion" || len(ci.failedRuns) != 1 {
		t.Fatalf("ci = %+v", ci)
	}
	if r := ci.failedRuns[0]; r.Entry != "Completion" || r.Name != "Completion" || r.WorkflowFile != "ci.yml" || strings.Join(r.Compare, ",") != "Capybara" {
		t.Errorf("failed run = %+v, want Completion compared by its failed job", r)
	}
	pr.CI.Checks[0].State = store.CheckPassed // a skip with no failure beside it (a path filter) is no failure
	if ci := openCIOf(pr, completionRequired, false, nil); ci.failed != "" {
		t.Errorf("a skip beside no failure of its workflow: %+v", ci)
	}
}

// talkable's dispatched run (ci_dispatch) is outside the PR's rollup: its
// Completion is posted only on success. The run ending failure after its
// jobs started is a failed required check, kind ci, no longer one that did
// not run; a run that ended success or cancelled without it stays missing.
func TestARunMagnumStartedThatEndedFailureIsAFailedCheckNotCIMissing(t *testing.T) {
	f := idleFacts()
	f.pr.CI = &store.CIStatus{SHA: fixHead, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{{Name: "Pronto", State: store.CheckPassed}}}
	f.ci = openCIOf(f.pr, completionRequired, false, []string{"Completion"})
	if len(f.ci.missing) != 1 {
		t.Fatalf("ci = %+v", f.ci)
	}
	f.ciWorkflow = "ci.yml"
	f.ciStart = ciStart{Head: fixHead, Workflow: "ci.yml", At: openNow.Add(-50 * time.Minute), Run: &ciRun{ID: 77, Status: "completed", Conclusion: "failure"}}
	f.ci = ciShape3(f.ci, f.ciStart, f.ciRerun)
	f.ciFix = ciFixFacts{on: true, base: ciBasePassed, baseSHA: ciBaseSHA, baseName: "main", starts: ciFixMax}
	v, work, _ := decideOpen(f)
	wantNeedsCI(t, v, work, "required check Completion failed at a1a1a1a (magnum's run of ci.yml ended failure) after 3 CI fixes")
	if r := f.ci.failedRuns; len(r) != 1 || r[0].Run != 77 || r[0].WorkflowFile != "ci.yml" || r[0].Entry != "Completion" {
		t.Errorf("failed runs = %+v", r)
	}
	for _, end := range []string{"success", "cancelled"} {
		s := f.ciStart
		s.Run = &ciRun{ID: 77, Status: "completed", Conclusion: end}
		ci := ciShape3(openCIOf(f.pr, completionRequired, false, []string{"Completion"}), s, ciRerun{})
		if ci.failed != "" || len(ci.missing) != 1 {
			t.Errorf("a run that ended %s: %+v, want the check missing", end, ci)
		}
	}
}

// GitHub's required Completion with no run at the head goes before the
// rollup's failure of a check nobody requires (talkable#12016's shape:
// Dependency Audit failed): the check did not run, and the CI fix is no
// fix of Dependency Audit.
func TestTheEnforcedMissingCheckGoesBeforeTheRollupsFailureOfAnother(t *testing.T) {
	failure := "FAILURE"
	pr := idleFacts().pr
	pr.CIState = &failure
	pr.CI = &store.CIStatus{SHA: fixHead, State: failure, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{
		{Name: "Dependency Audit", Workflow: "Audit", WorkflowFile: "dependency-audit.yml", State: store.CheckFailed}}}
	ci := openCIOf(pr, completionRequired, false, []string{"Completion"})
	if ci.failed != "" || len(ci.missing) != 1 || ci.missing[0] != "Completion" {
		t.Errorf("ci = %+v, want Completion missing", ci)
	}
}

// A check that failed and passed on a re-run is no failure: the stored
// checks hold its newest run.
func TestAReRunThatPassedIsNoFailure(t *testing.T) {
	f := ciFailedFacts()
	f.pr.CI.Checks = append(f.pr.CI.Checks, store.CheckResult{Name: "Completion", Workflow: "CI", WorkflowFile: "ci-dispatch.yml",
		State: store.CheckPassed, At: openNow.Add(-10 * time.Minute)})
	f.ci = openCIOf(f.pr, completionRequired, false, nil)
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenIdle, "nothing open at a1a1a1a")
	if len(work) != 0 {
		t.Fatal(work)
	}
}

// The verdict through a CI fix: while it runs, while its push is
// re-reviewed, while CI runs again on the push, and once that passed.
func TestTheVerdictWhileTheCIFixRunsWhileItsPushIsReReviewedWhileCIRerunsAndAfterSuccess(t *testing.T) {
	f := ciFailedFacts()
	f.ciFix.starts = 1
	f.fix = &FixState{Cycle: 1, Cap: 5, Phase: FixPhaseFixing, Head: fixHead, Mode: FixModeCI, CIChecks: []string{"Completion"}}
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenFixing, "fixing CI: required check Completion at a1a1a1a, cycle 1 of 5 (CI fix 1 of 3)")
	pushed := strings.Repeat("b2", 20)
	f.fix.Phase, f.fix.Head = FixPhaseReviewing, pushed
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenFixing, "fixing, re-review of b2b2b2b (the CI fix's push)")

	after := idleFacts()
	after.pr.HeadSHA, after.pr.ReviewedSHA = pushed, &pushed
	after.sum.SHA = pushed
	after.ledger.FixerHeads = []string{pushed}
	after.pr.CI = &store.CIStatus{SHA: pushed, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{
		{Name: "Completion", Workflow: "CI", WorkflowFile: "ci.yml", State: store.CheckPending}}}
	after.ci = openCIOf(after.pr, completionRequired, false, nil)
	after.ciFix = ciFixFacts{starts: 1, last: ciFixStart{Head: fixHead, Entries: []string{"Completion"}, Outcome: ciOutcomePushed, Pushed: pushed}, lastN: 1}
	v, _, _ = decideOpen(after)
	wantVerdict(t, v, OpenWaits, "CI at b2b2b2b after CI fix 1 of 3")
	after.pr.CI.Checks[0].State = store.CheckPassed
	after.ci = openCIOf(after.pr, completionRequired, false, nil)
	v, _, _ = decideOpen(after)
	wantVerdict(t, v, OpenIdle, "nothing open at b2b2b2b · approved, merge is yours · CI fix 1 of 3 made required check Completion pass")
}
