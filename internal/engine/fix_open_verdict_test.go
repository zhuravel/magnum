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
		now: openNow,
		pr: store.PR{ID: 2, Number: 2, URL: prPage, HeadSHA: head, ReviewedSHA: &head, State: store.PRReviewed, HeadChangedAt: openNow.Add(-time.Hour),
			ActivityAt: &activity, ReviewGate: &store.ReviewGate{Decision: "APPROVED"}},
		sum:    &store.ReviewSummary{RunID: "r1", ReviewID: 101, SHA: head, URL: prPage + "#pullrequestreview-101", At: openNow.Add(-50 * time.Minute)},
		whole:  true,
		settle: openSettle{quiet: 3 * time.Minute, max: 20 * time.Minute, late: 2 * time.Hour, handsOff: 30 * time.Minute, maxLoops: 3},
		ledger: openLedger{FixerHeads: []string{head}},
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

// The settle waits settle_quiet after the PR's last activity: a reviewer
// who commented a minute ago may still be at work.
func TestAutofixOpenWaitsForTheSettle(t *testing.T) {
	f := idleFacts()
	f.items = []openItem{personThread(4100)}
	f.pr.HeadChangedAt = openNow.Add(-10 * time.Minute)
	activity := openNow.Add(-time.Minute)
	f.pr.ActivityAt = &activity
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "settling")
	if !v.Until.Equal(activity.Add(3*time.Minute)) || len(work) != 0 {
		t.Errorf("until %s, work %v; want %s and no start", v.Until, work, activity.Add(3*time.Minute))
	}
}

// A settle_checks check that runs at the head is waited for, and so is one
// whose checks are stored for another head (not known yet); a listed check
// the head does not have is not.
func TestAutofixOpenWaitsForProntoAtTheHead(t *testing.T) {
	cases := map[string]struct {
		ci   *store.CIStatus
		want string
	}{
		"pending at the head": {&store.CIStatus{SHA: fixHead, Checks: []store.CheckResult{{Name: "Pronto", State: store.CheckPending}}}, OpenWaits},
		"at another head":     {&store.CIStatus{SHA: fixPushed, Checks: []store.CheckResult{{Name: "pronto", State: store.CheckPassed}}}, OpenWaits},
		"by workflow name":    {&store.CIStatus{SHA: fixHead, Checks: []store.CheckResult{{Name: "lint", Workflow: "pronto", State: store.CheckPending}}}, OpenWaits},
		"absent at the head":  {&store.CIStatus{SHA: fixHead, Checks: []store.CheckResult{{Name: "rspec", State: store.CheckPending}}}, OpenFixing},
		"done at the head":    {&store.CIStatus{SHA: fixHead, Checks: []store.CheckResult{{Name: "pronto", State: store.CheckFailed}}}, OpenFixing},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := idleFacts()
			f.items = []openItem{personThread(4100)}
			f.settle.checks = []string{"pronto"}
			f.pr.CI = tc.ci
			f.pr.HeadChangedAt = openNow.Add(-10 * time.Minute)
			v, _, _ := decideOpen(f)
			if v.Verdict != tc.want {
				t.Errorf("verdict = %s %q, want %s", v.Verdict, v.Why, tc.want)
			}
			if tc.want == OpenWaits && !v.Until.Equal(f.pr.HeadChangedAt.Add(20*time.Minute)) {
				t.Errorf("until = %s, want settle_max after the head moved", v.Until)
			}
		})
	}
}

// settle_max caps the wait for quiet and checks, counted from the head's
// move: 21 minutes after it, with the PR active since, a loop starts
// despite a comment a minute ago and a check still running.
func TestAutofixOpenStartsAfterSettleMaxDespiteActivity(t *testing.T) {
	f := idleFacts()
	f.items = []openItem{personThread(4100)}
	f.pr.HeadChangedAt = openNow.Add(-21 * time.Minute)
	activity := openNow.Add(-time.Minute)
	f.pr.ActivityAt = &activity
	f.ledger.Spell = openSpell{Start: f.pr.HeadChangedAt, Seen: openNow.Add(-30 * time.Second)}
	f.settle.checks = []string{"pronto"}
	f.pr.CI = &store.CIStatus{SHA: fixHead, Checks: []store.CheckResult{{Name: "pronto", State: store.CheckPending}}}
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop on 1 thread of a person")
	if len(work) != 1 {
		t.Errorf("work = %v, want the thread", work)
	}
}

// magnum's own review of the head is waited for without a cap: three hours
// after the head moved, with the round due 30 minutes ago, it still waits.
// magnum_late_after past the due time it needs you, as magnum_late. A
// baseline PR is off and never alerts.
func TestAutofixOpenNeverCapsTheWaitForMagnum(t *testing.T) {
	f := idleFacts()
	f.items = []openItem{personThread(4100)}
	old := "0000000000000000000000000000000000000000"
	f.pr.ReviewedSHA, f.pr.State = &old, store.PRQueued
	f.pr.HeadChangedAt = openNow.Add(-3 * time.Hour)
	due := openNow.Add(-30 * time.Minute)
	f.pr.NextEligibleAt = &due
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum reviews a1a1a1a")
	if len(work) != 0 {
		t.Errorf("work = %v while magnum reviews", work)
	}
	due = openNow.Add(-2*time.Hour - time.Minute)
	f.pr.NextEligibleAt = &due
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since")
	if len(v.Needs) == 0 || v.Needs[len(v.Needs)-1].Kind != "magnum_late" {
		t.Errorf("needs = %+v, want magnum_late", v.Needs)
	}
	f.off, f.pr.State = OpenOffBaseline, store.PRBaseline
	v, _, _ = decideOpen(f)
	if v.Verdict != OpenOff || v.Off != OpenOffBaseline {
		t.Errorf("a baseline PR: %+v", v)
	}
}

// A head the fixer did not push is the operator's: no loop starts on it
// until hands_off_after_push passed. A head the fixer pushed is never
// waited for.
func TestAutofixOpenSkipsAPRYouPushedInTheLast30Minutes(t *testing.T) {
	f := idleFacts()
	f.items = []openItem{personThread(4100)}
	f.pr.HeadChangedAt = openNow.Add(-10 * time.Minute)
	f.ledger.FixerHeads = nil
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "you pushed a1a1a1a at")
	if !v.Until.Equal(f.pr.HeadChangedAt.Add(30*time.Minute)) || len(work) != 0 {
		t.Errorf("until %s, work %v", v.Until, work)
	}
	f.ledger.FixerHeads = []string{fixHead}
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop")
}

// Unknown is never idle: a read GitHub cut short needs you (it cannot get
// whole by waiting), a failed read and no read since the PR changed are
// unknown, and below the GitHub floor the read waits.
func TestTheOpenRowIsNeverIdleWhenAReadWasCutShort(t *testing.T) {
	cases := map[string]struct {
		edit func(*openFacts)
		want string
	}{
		"cut short":         {func(f *openFacts) { f.cut = []string{"over 1000 review threads"} }, OpenNeedsYou},
		"failed":            {func(f *openFacts) { f.failed = true }, OpenUnknown},
		"not read":          {func(f *openFacts) { f.whole = false }, OpenUnknown},
		"not read, low":     {func(f *openFacts) { f.whole, f.low = false, true }, OpenWaits},
		"failed, low":       {func(f *openFacts) { f.failed, f.low = true, true }, OpenWaits},
		"low, but read now": {func(f *openFacts) { f.low = true }, OpenIdle},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := idleFacts()
			tc.edit(&f)
			if v, _, _ := decideOpen(f); v.Verdict != tc.want {
				t.Errorf("verdict = %s %q, want %s", v.Verdict, v.Why, tc.want)
			}
		})
	}
}

// Idle says nothing is open at the head and whether a person approved;
// every condition taken away gives something else.
func TestTheOpenRowIsIdleOnlyWhenEverythingHolds(t *testing.T) {
	v, work, needs := decideOpen(idleFacts())
	wantVerdict(t, v, OpenIdle, "nothing open at a1a1a1a · approved, merge is yours")
	if len(work)+len(needs) != 0 || v.Open != 0 {
		t.Errorf("idle with work %v and needs %v", work, needs)
	}
	f := idleFacts()
	f.pr.ReviewGate = nil
	f.approved, f.afterCount = "c3c3c3c", 2
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenIdle, "nothing open at a1a1a1a · no approval yet · 2 commits after the approval")
	for name, edit := range map[string]func(*openFacts){
		"a pending reply": func(f *openFacts) { f.pending = 1 },
		"a stalemate":     func(f *openFacts) { f.stalemate = []string{prPage + "#discussion_r1"} },
		"settle open": func(f *openFacts) {
			a := openNow.Add(-time.Minute)
			f.pr.ActivityAt, f.pr.HeadChangedAt = &a, openNow.Add(-10*time.Minute)
		},
		"a pending check":     func(f *openFacts) { f.ci = openCI{pending: true} },
		"a red check":         func(f *openFacts) { f.ci = openCI{failed: "required check rspec"} },
		"a missing check":     func(f *openFacts) { f.ci = openCI{missing: []string{"Completion"}} },
		"a loop":              func(f *openFacts) { f.fix = &FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing} },
		"needs attention":     func(f *openFacts) { f.attention = "fix" },
		"an unreadable loop":  func(f *openFacts) { f.fixBad = true },
		"a held fix":          func(f *openFacts) { f.held = heldRestart },
		"magnum reviews":      func(f *openFacts) { f.pr.State = store.PRQueued },
		"no review stands":    func(f *openFacts) { f.sum = nil },
		"an item":             func(f *openFacts) { f.items = []openItem{personThread(4100)} },
		"magnum's review has": func(f *openFacts) { f.items = []openItem{{Key: "review:r1", Class: classMagnum, Kind: itemFindings}} },
	} {
		f := idleFacts()
		edit(&f)
		if v, _, _ := decideOpen(f); v.Verdict == OpenIdle {
			t.Errorf("%s: idle %q", name, v.Why)
		}
	}
}

// A base that requires no review (GitHub's review decision is empty) leaves
// the merge to you as an approval does; only a gate magnum has not read,
// or one that still wants a review, is no approval yet.
func TestIdleOnABaseThatRequiresNoReviewLeavesTheMergeToYou(t *testing.T) {
	for _, c := range []struct {
		gate *store.ReviewGate
		why  string
	}{
		{&store.ReviewGate{}, "nothing open at a1a1a1a · no review required, merge is yours"},
		{&store.ReviewGate{Decision: "APPROVED"}, "nothing open at a1a1a1a · approved, merge is yours"},
		{&store.ReviewGate{Decision: "REVIEW_REQUIRED"}, "nothing open at a1a1a1a · no approval yet"},
		{&store.ReviewGate{Decision: "CHANGES_REQUESTED"}, "nothing open at a1a1a1a · no approval yet"},
		{nil, "nothing open at a1a1a1a · no approval yet"},
	} {
		f := idleFacts()
		f.pr.ReviewGate = c.gate
		v, _, _ := decideOpen(f)
		wantVerdict(t, v, OpenIdle, c.why)
	}
}

// Red required CI needs you, until a later milestone lets the fixer at it.
func TestRedRequiredCINeedsYou(t *testing.T) {
	pr := idleFacts().pr
	pr.CI = &store.CIStatus{SHA: fixHead, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{{Name: "rspec", State: store.CheckFailed}}}
	ci := openCIOf(pr, store.RequiredChecks{Checks: []string{"rspec"}, Source: store.RequiredFromGitHub}, false, nil)
	f := idleFacts()
	f.ci = ci
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "required check rspec failed at a1a1a1a")
	if len(v.Needs) != 1 || v.Needs[0].Kind != "ci" { // a check that ran and failed, never ci_missing
		t.Errorf("needs = %+v, want the PR's failed CI (ci)", v.Needs)
	}
	failure := "FAILURE"
	pr.CI, pr.CIState = nil, &failure // the stored checks cannot say: the rollup decides
	f.ci = openCIOf(pr, store.RequiredChecks{}, false, nil)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "CI failed at a1a1a1a")
}

// Pending required CI waits, and so does a required check the head has not
// started.
func TestPendingRequiredCIWaits(t *testing.T) {
	pr := idleFacts().pr
	pr.CI = &store.CIStatus{SHA: fixHead, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{{Name: "rspec", State: store.CheckPending}}}
	f := idleFacts()
	f.ci = openCIOf(pr, store.RequiredChecks{Checks: []string{"rspec"}, Source: store.RequiredFromConfig}, false, nil)
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a")
	pr.CI = nil
	f.ci = openCIOf(pr, store.RequiredChecks{Checks: []string{"rspec"}, Source: store.RequiredFromConfig}, false, nil)
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
	pr := idleFacts().pr // its head moved an hour ago, past settle_max
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
			f.pr = pr
			f.ci = openCIOf(pr, tc.required, tc.strict, tc.enforced)
			v, _, _ := decideOpen(f)
			wantVerdict(t, v, OpenNeedsYou, tc.what+" did not run at a1a1a1a: run its workflow on the PR's branch")
			if len(v.Needs) != 1 || v.Needs[0].Kind != "ci_missing" || v.Needs[0].URL != prPage {
				t.Errorf("needs = %+v, want the PR's CI that did not run (ci_missing)", v.Needs)
			}
			f.pr.HeadChangedAt = openNow.Add(-5 * time.Minute) // pushed five minutes ago: its run may not have started
			v, _, _ = decideOpen(f)
			wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: "+tc.what+" has not started")
			if want := f.pr.HeadChangedAt.Add(20 * time.Minute); !v.Until.Equal(want) {
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
			f.pr = tc.pr
			f.ci = openCIOf(tc.pr, tc.required, false, tc.enforced)
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
	f.pr, f.ciWorkflow = pr, "ci.yml"
	f.ci = openCIOf(pr, store.RequiredChecks{Checks: []string{"Completion"}, Source: store.RequiredFromGitHub}, false,
		enforcedMissing(pr, ghRequired("Completion")))
	if !f.ci.running || len(f.ci.missing) != 1 {
		t.Fatalf("ci = %+v, want Completion missing while the head's checks run", f.ci)
	}
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check Completion has not started")
	if v.dispatch || !v.Until.IsZero() {
		t.Errorf("dispatch %v, until %s while the head's checks run", v.dispatch, v.Until)
	}
	f.ciStart = ciStart{Head: fixHead, Workflow: "ci.yml", At: openNow.Add(-time.Hour)} // older than settle_max
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	f.ci.running = false
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "and no run came")
}

// A branch GitHub says requires no checks leaves CI out of idle, even with
// a red rollup.
func TestAKnownEmptyRequiredListLeavesCIOut(t *testing.T) {
	pr := idleFacts().pr
	failure := "FAILURE"
	pr.CIState = &failure
	if ci := openCIOf(pr, store.RequiredChecks{Checks: []string{}, Source: store.RequiredFromGitHub}, false, nil); ci.failed != "" || ci.pending || len(ci.missing) > 0 {
		t.Errorf("ci = %+v, want nothing", ci)
	}
}

// A handed item still open after its loop needs you, a person's and a bot's
// thread alike: a person's item never closes on the fixer's word alone. A
// bot's handed receipt or note on the conversation is seen and drops out.
func TestALeftoverPersonsItemOrBotThreadNeedsYouAndABotsReceiptIsSeen(t *testing.T) {
	person, botThread := personThread(4100), openItem{Key: "thread:4200@4200", Class: classBot, Kind: itemThread, URL: prPage + "#discussion_r4200"}
	receipt := openItem{Key: "remark:701", Class: classBot, Kind: itemComment, URL: prPage + "#issuecomment-701"}
	f := idleFacts()
	f.items = []openItem{person, botThread, receipt}
	f.ledger.Handed = map[string]time.Time{person.Key: openNow, botThread.Key: openNow, receipt.Key: openNow}
	v, work, needs := decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "2 items need you: 1 thread of a person and 1 thread of a bot")
	if len(work) != 0 || len(needs) != 2 || len(v.Needs) != 2 || v.Needs[0].URL != person.URL || v.Needs[1].Kind != itemThread {
		t.Errorf("work %v, needs %v, verdict needs %+v", work, needs, v.Needs)
	}
	f.items = []openItem{receipt}
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenIdle, "nothing open")
}

// magnum's rebuttal alone, on a review that leaves the fixer no work of
// its own, needs your answer and never starts a loop (prepareFix would
// refuse it as nothing to fix). With review work it starts one.
func TestOnlyAMagnumRebuttalWithNoReviewWorkNeedsYou(t *testing.T) {
	f := idleFacts()
	rebuttal := openItem{Key: "mthread:5000@5002", Class: classMagnum, Kind: itemMagnum, URL: prPage + "#discussion_r5000"}
	f.items = []openItem{rebuttal}
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum's reply waits for your answer: "+rebuttal.URL)
	if len(work) != 0 {
		t.Errorf("work = %v, want no start", work)
	}
	f.sum.Counts[2] = 1
	f.ledger.Reviews = []string{"review:r1"}
	v, work, _ = decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop on 1 reply of magnum's")
	if len(work) != 1 {
		t.Errorf("work = %v", work)
	}
}

// The off verdict names why the look leaves the PR alone.
func TestTheOffVerdictNamesItsReason(t *testing.T) {
	for _, off := range []string{OpenOffDraft, OpenOffFork, OpenOffFixOff, OpenOffIncludeOwn, OpenOffBaseline, OpenOffIneligible} {
		f := idleFacts()
		f.off = off
		v, work, needs := decideOpen(f)
		if v.Verdict != OpenOff || v.Off != off || v.Why == "" || len(work)+len(needs) != 0 {
			t.Errorf("%s: %+v", off, v)
		}
	}
}

// A colleague's thread the fixer answered twice, whose reviewer answered
// again, needs you: it is never work a third time.
func TestAThirdReplyInAnExternalThreadNeedsYou(t *testing.T) {
	f := idleFacts()
	th := personThread(4100)
	th.Key, th.FixerReplies = "thread:4100@4104", 2
	f.items = []openItem{th}
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "1 item needs you")
	if len(work) != 0 {
		t.Errorf("work = %v", work)
	}
}

// At most autofix_max_loops loops start on one PR in 24 hours; then the PR
// needs you, and f starts another.
func TestThreeAutofixLoopsADayNeedYou(t *testing.T) {
	f := idleFacts()
	f.items = []openItem{personThread(4100)}
	f.ledger.Starts = []time.Time{openNow.Add(-25 * time.Hour), openNow.Add(-5 * time.Hour), openNow.Add(-3 * time.Hour)}
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop")
	f.ledger.Starts = append(f.ledger.Starts, openNow.Add(-time.Hour))
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "3 loops today: f starts another")
	if len(work) != 0 || len(v.Needs) == 0 || v.Needs[len(v.Needs)-1].Kind != "loop" {
		t.Errorf("work %v, needs %+v", work, v.Needs)
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
		f.items = openItemsOf(itemsInput{read: read, sum: f.sum, head: fixHead})
		if handed {
			f.ledger.Handed = map[string]time.Time{}
			for _, it := range f.items {
				f.ledger.Handed[it.Key] = openNow
			}
		}
		v, _, _ := decideOpen(f)
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), secret) || len(f.items) != 2 {
			t.Errorf("handed %v: verdict %s (items %v)", handed, b, f.items)
		}
	}
}

// A reader takes the verdict as stale, and shows the PR as looked at, when
// the daemon stopped ticking, the head moved or the PR's row changed after
// the verdict. A long tick is no stop: the next tick starts a whole poll
// interval after it ends, so a tick of 45 seconds puts 75 seconds between
// two starts.
func TestOpenVerdictStaleness(t *testing.T) {
	v := OpenVerdict{Verdict: OpenIdle, Head: fixHead, At: openNow}
	poll := 30 * time.Second
	if v.Stale(fixHead, openNow.Add(-time.Minute), openNow.Add(-10*time.Second), openNow, poll) {
		t.Error("a fresh verdict is stale")
	}
	if v.Stale(fixHead, openNow.Add(-time.Minute), openNow.Add(-75*time.Second), openNow, poll) {
		t.Error("a verdict is stale near the end of a long tick")
	}
	for name, stale := range map[string]bool{
		"head moved":  v.Stale(fixPushed, openNow.Add(-time.Minute), openNow, openNow, poll),
		"row changed": v.Stale(fixHead, openNow.Add(time.Second), openNow, openNow, poll),
		"no tick":     v.Stale(fixHead, openNow.Add(-time.Minute), openNow.Add(-3*time.Minute), openNow, poll),
		"never":       v.Stale(fixHead, openNow.Add(-time.Minute), time.Time{}, openNow, poll),
	} {
		if !stale {
			t.Errorf("%s: not stale", name)
		}
	}
	raw, _ := json.Marshal(v)
	if got, ok := ParseOpenVerdict(string(raw)); !ok || !got.sameAs(v) {
		t.Errorf("ParseOpenVerdict = %+v, %v", got, ok)
	}
	if _, ok := ParseOpenVerdict("not json"); ok {
		t.Error("a value that is no verdict parses")
	}
}

// A poll that is behind makes the verdict unknown, never idle, and starts
// nothing; below the GitHub floor it waits for the budget.
func TestAPollThatIsBehindIsUnknown(t *testing.T) {
	f := idleFacts()
	f.items = []openItem{personThread(4100)}
	f.pollStale = true
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenUnknown, "the poll of GitHub is behind")
	if len(work) != 0 {
		t.Errorf("work = %v on a stale poll", work)
	}
	f.low = true
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "GitHub budget")
}

// A thread a fixer resolved with no answer is never work: the fixer's file
// leaves resolved threads out, so no loop can be handed it, and prepareFix
// refused such a start as nothing to fix. Never handed, it needs you at
// once, as a leftover the look raises.
func TestAThreadTheFixerResolvedUnansweredNeedsYouNotALoop(t *testing.T) {
	f := idleFacts()
	it := personThread(4100)
	it.Resolved = true
	f.items = []openItem{it}
	v, work, needs := decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "1 item needs you")
	if len(work) != 0 || len(needs) != 1 || !v.leftovers {
		t.Errorf("work %+v, needs %+v, leftovers %v: want the thread among the leftovers", work, needs, v.leftovers)
	}
}

// On a head older than settle_max the settle still waits for quiet: the
// cap counts from the start of the PR's current spell of activity, as the
// look saw it, not from the head's move. A colleague who posts three
// comments a minute apart on a head two days old gets one loop after the
// third, not one after the first and another for the rest. A spell longer
// than settle_max starts the loop anyway.
func TestTheSettleWaitsForQuietOnAnOldHead(t *testing.T) {
	f := idleFacts()
	f.items = []openItem{personThread(4100)}
	f.pr.HeadChangedAt = openNow.Add(-48 * time.Hour)
	activity := openNow.Add(-30 * time.Second)
	f.pr.ActivityAt = &activity
	f.ledger.Spell = openSpell{Start: openNow.Add(-2 * time.Minute), Seen: openNow.Add(-30 * time.Second)}
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "settling")
	if !v.Until.Equal(activity.Add(3*time.Minute)) || len(work) != 0 {
		t.Errorf("until %s, work %v; want %s and no start", v.Until, work, activity.Add(3*time.Minute))
	}
	f.ledger.Spell.Start = openNow.Add(-21 * time.Minute)
	v, work, _ = decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop")
	if len(work) != 1 {
		t.Errorf("work = %v after a spell of 21 minutes, want the thread", work)
	}
}

// The look's spell of activity on a PR: a new one begins at the activity
// the look first sees within settle_quiet, goes on while the activity
// does, also across looks further apart than settle_quiet (a long poll
// interval), ends once the PR is quiet or while a loop runs (its fixer's
// activity is the loop's), and begins anew when the look did not see it go
// on for the gap (the daemon slept): yesterday's spell does not cap this
// morning's comment.
func TestTheSpellOfActivityBeginsGoesOnAndEnds(t *testing.T) {
	quiet, gap := 3*time.Minute, 3*time.Minute
	pr := store.PR{HeadChangedAt: openNow.Add(-48 * time.Hour)}
	at := func(d time.Duration) *time.Time { t := openNow.Add(d); return &t }
	pr.ActivityAt = at(-30 * time.Second)
	s := openSpell{}.next(pr, quiet, gap, openNow, false)
	if !s.Start.Equal(*pr.ActivityAt) || !s.Seen.Equal(openNow) {
		t.Errorf("a new spell = %+v, want it to begin at the activity, seen now", s)
	}
	pr.ActivityAt = at(time.Minute)
	if next := s.next(pr, quiet, gap, openNow.Add(90*time.Second), false); !next.Start.Equal(s.Start) {
		t.Errorf("a spell that goes on = %+v, want its start kept", next)
	}
	pr.ActivityAt = at(4 * time.Minute)
	if next := s.next(pr, quiet, 20*time.Minute, openNow.Add(5*time.Minute), false); !next.Start.Equal(s.Start) {
		t.Errorf("a spell that goes on across looks 5 minutes apart = %+v, want its start kept", next)
	}
	pr.ActivityAt = at(time.Minute)
	if next := s.next(pr, quiet, gap, openNow.Add(5*time.Minute), false); !next.Start.IsZero() {
		t.Errorf("a quiet PR's spell = %+v, want none", next)
	}
	if next := s.next(pr, quiet, gap, openNow.Add(90*time.Second), true); !next.Start.IsZero() {
		t.Errorf("a spell under a loop = %+v, want none", next)
	}
	pr.ActivityAt = at(10 * time.Hour)
	if next := s.next(pr, quiet, gap, openNow.Add(10*time.Hour+30*time.Second), false); !next.Start.Equal(*pr.ActivityAt) {
		t.Errorf("a spell after a night the look did not see = %+v, want a new one at %s", next, *pr.ActivityAt)
	}
}

// A look at the base that failed leaves the merge unknown, and unknown is
// never idle: an approved PR whose merge with its base could not be
// checked does not say "merge is yours". Work still starts a loop, whose
// job checks the base itself.
func TestAnUncheckedMergeIsNeverIdle(t *testing.T) {
	f := idleFacts()
	f.mergeUnchecked = "master"
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenUnknown, "the merge with master could not be checked")
	f.items = []openItem{personThread(4100)}
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop")
	if len(work) != 1 {
		t.Errorf("work = %v, want the thread", work)
	}
}

// `magnum pause` holds the look's new starts: work waits with the pause's
// words and its end when it has one, starts nothing, spends none of
// autofix_max_loops and needs nobody.
func TestAPauseHoldsTheLooksStart(t *testing.T) {
	f := idleFacts()
	f.items = []openItem{personThread(4100)}
	f.pause = "daemon paused lunch"
	v, work, needs := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "daemon paused lunch: 1 thread of a person waits for `magnum resume`")
	if len(work) != 0 || len(needs) != 0 || len(v.Needs) != 0 {
		t.Errorf("work %v, needs %v, verdict needs %+v; want nothing started and nothing for you", work, needs, v.Needs)
	}
	f.pauseUntil = openNow.Add(2 * time.Hour)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "waits until")
	if !v.Until.Equal(f.pauseUntil) {
		t.Errorf("until = %s, want the pause's end %s", v.Until, f.pauseUntil)
	}
	f.pause, f.pauseUntil = "", time.Time{}
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop")
}

// [daemon] quiet_hours hold the look's new starts as they hold a full
// round: work waits until the window ends, the verdict says so, and once it
// ended the same work starts.
func TestQuietHoursHoldTheLooksStartUntilTheyEnd(t *testing.T) {
	f := idleFacts()
	f.items = []openItem{personThread(4100)}
	f.settle.quietHours = "03:00-10:00"
	f.now = time.Date(2026, 11, 7, 4, 0, 0, 0, time.Local)
	f.pr.HeadChangedAt, f.ledger.Spell = f.now.Add(-time.Hour), openSpell{}
	activity := f.now.Add(-30 * time.Minute)
	f.pr.ActivityAt = &activity
	end := time.Date(2026, 11, 7, 10, 0, 0, 0, time.Local)
	v, work, needs := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "quiet hours (03:00-10:00): 1 thread of a person waits until 10:00")
	if !v.Until.Equal(end) || len(work) != 0 || len(needs) != 0 {
		t.Errorf("until %s, work %v, needs %v; want %s and nothing started", v.Until, work, needs, end)
	}
	f.now = end
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop")
}

// A head whose round falls inside [daemon] quiet_hours is reviewed at their
// end, so magnum_late_after counts from there: a push at 02:50 due at 03:05
// waits through the night and is late only at 12:00, two hours after the
// window's end, never at 05:05.
func TestMagnumLateCountsFromTheEndOfQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 7, h, m, 0, 0, time.Local) }
	f := idleFacts()
	f.items = []openItem{personThread(4100)}
	f.settle.quietHours = "03:00-10:00"
	old := "0000000000000000000000000000000000000000"
	f.pr.ReviewedSHA, f.pr.State = &old, store.PRRereviewPending
	f.pr.HeadChangedAt = at(2, 50)
	due := at(3, 5)
	f.pr.NextEligibleAt = &due
	f.now = at(5, 10)
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum reviews a1a1a1a")
	if !v.Until.Equal(at(10, 0)) {
		t.Errorf("until = %s, want the end of quiet hours", v.Until)
	}
	f.now = at(11, 59)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum reviews a1a1a1a")
	f.now = at(12, 0)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since 10:00")
}

// A review that was already late when quiet hours began is not late
// inside them: no needs you, so no toast, at night; at their end, still
// late, it needs you, counted from its own due time.
func TestMagnumIsNeverLateInsideQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 7, h, m, 0, 0, time.Local) }
	f := idleFacts()
	f.settle.quietHours = "03:00-10:00"
	old := "0000000000000000000000000000000000000000"
	f.pr.ReviewedSHA, f.pr.State = &old, store.PRRereviewPending
	f.pr.HeadChangedAt = at(0, 20)
	due := at(0, 30) // late at 02:30, before the window
	f.pr.NextEligibleAt = &due
	f.now = at(4, 0)
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum reviews a1a1a1a")
	if v.late || !v.Until.Equal(at(10, 0)) {
		t.Errorf("late %v, until %s; want no late review inside quiet hours, a wait until their end", v.late, v.Until)
	}
	f.now = at(10, 0)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since 00:30")
	if !v.late {
		t.Error("not late at the end of quiet hours")
	}
}

// A review due before quiet hours began, and not late yet when they began,
// waits for their end with the round they hold: magnum_late_after counts
// from that end, so it is not late at 10:00, when the round can start at
// last, but two hours later. A round due at 02:00 that waited for a busy
// pool, and the operator's push at 02:43 that push_quiet_period set due at
// 02:48, both went into the night that way.
func TestMagnumLateCountsFromTheEndOfQuietHoursThatBeganBeforeItWasLate(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 7, h, m, 0, 0, time.Local) }
	two := at(2, 0)
	for _, tc := range []struct {
		name string
		head time.Time
		due  *time.Time
	}{
		{"due at 02:00", at(1, 50), &two},
		{"pushed at 02:43", at(2, 43), nil},
	} {
		f := idleFacts()
		f.settle.quietHours = "03:00-10:00"
		old := "0000000000000000000000000000000000000000"
		f.pr.ReviewedSHA, f.pr.State = &old, store.PRRereviewPending
		f.pr.HeadChangedAt, f.pr.NextEligibleAt = tc.head, tc.due
		for _, now := range []time.Time{at(4, 30), at(10, 0), at(11, 59)} {
			f.now = now
			if v, _, _ := decideOpen(f); v.Verdict != OpenWaits || v.late {
				t.Errorf("%s, at %s: verdict %s %q (late %v), want a wait for magnum's review", tc.name, now.Format("15:04"), v.Verdict, v.Why, v.late)
			}
		}
		f.now = at(12, 0)
		v, _, _ := decideOpen(f)
		if v.Verdict != OpenNeedsYou || !v.late || !strings.Contains(v.Why, "magnum has not reviewed a1a1a1a since 10:00") {
			t.Errorf("%s, at 12:00: verdict %s %q (late %v), want late since the window's end", tc.name, v.Verdict, v.Why, v.late)
		}
	}
}

// `magnum pause` holds every round nobody forced, so a review it holds is
// not late while it lasts: the wait names the pause and its end, nothing
// needs you, no toast. After the pause the late clock counts from its end,
// for a review that was not late when it began; one late before it began
// is late again at its end. A round you forced runs under the pause, so
// its clock does not stop.
func TestMagnumIsNeverLateUnderThePauseAndCountsFromItsEnd(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 7, h, m, 0, 0, time.Local) }
	f := idleFacts()
	old := "0000000000000000000000000000000000000000"
	f.pr.ReviewedSHA, f.pr.State = &old, store.PRRereviewPending
	f.pr.HeadChangedAt = at(14, 10)
	f.pause, f.pauseUntil = "daemon paused", at(18, 0)
	f.now = at(16, 15)
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "daemon paused: magnum's review of a1a1a1a waits until 18:00")
	if v.late || len(v.Needs) != 0 || !v.Until.Equal(at(18, 0)) {
		t.Errorf("late %v, needs %+v, until %s; want a wait until the pause's end", v.late, v.Needs, v.Until)
	}
	f.pauseUntil = time.Time{}
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "daemon paused: magnum's review of a1a1a1a waits for `magnum resume`")
	f.pause, f.pauses = "", []openHold{{from: at(14, 0), to: at(18, 0)}} // resumed at 18:00
	f.now = at(19, 59)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum reviews a1a1a1a")
	f.now = at(20, 0)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since 18:00")
	f.pr.HeadChangedAt, f.now = at(11, 0), at(18, 0) // late at 13:00, before the pause
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since 11:00")
	f.pr.HeadChangedAt, f.pr.Forced, f.now = at(14, 10), true, at(16, 15)
	f.pause, f.pauses = "daemon paused", nil
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since 14:10")
}

// An optional call the fixer left the operator holds nothing, but the
// operator sees it at the merge: the idle verdict ends with "1 optional
// call: <link>" (counted, each linked), approved or not, and carries the
// links for the factory screen's merge move.
func TestTheIdleVerdictNamesTheOptionalCalls(t *testing.T) {
	const link = prPage + "#discussion_r4302866759"
	f := idleFacts()
	f.optional = []optionalCall{{Thread: 4302866759, URL: link}}
	v, work, needs := decideOpen(f)
	wantVerdict(t, v, OpenIdle, "nothing open at a1a1a1a · approved, merge is yours · 1 optional call: "+link)
	if len(work)+len(needs) != 0 || len(v.Optional) != 1 || v.Optional[0] != link {
		t.Errorf("work %v, needs %v, optional %q; want only the link", work, needs, v.Optional)
	}
	f.pr.ReviewGate = nil
	f.optional = append(f.optional, optionalCall{Thread: 4302866800, URL: prPage + "#discussion_r4302866800"})
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenIdle, "nothing open at a1a1a1a · no approval yet · 2 optional calls: "+link+", "+prPage+"#discussion_r4302866800")
	if got := OptionalCallsWords(nil); got != "" {
		t.Errorf("OptionalCallsWords(nil) = %q", got)
	}
}
