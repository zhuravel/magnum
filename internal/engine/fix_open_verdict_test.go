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

// Red required CI needs you, until a later milestone lets the fixer at it.
func TestRedRequiredCINeedsYou(t *testing.T) {
	pr := idleFacts().pr
	pr.CI = &store.CIStatus{SHA: fixHead, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{{Name: "rspec", State: store.CheckFailed}}}
	ci := openCIOf(pr, store.RequiredChecks{Checks: []string{"rspec"}, Source: store.RequiredFromGitHub})
	f := idleFacts()
	f.ci = ci
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "required check rspec failed at a1a1a1a")
	failure := "FAILURE"
	pr.CI, pr.CIState = nil, &failure // the stored checks cannot say: the rollup decides
	f.ci = openCIOf(pr, store.RequiredChecks{})
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "CI failed at a1a1a1a")
}

// Pending required CI waits, and so does a required check the head has not
// started.
func TestPendingRequiredCIWaits(t *testing.T) {
	pr := idleFacts().pr
	pr.CI = &store.CIStatus{SHA: fixHead, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{{Name: "rspec", State: store.CheckPending}}}
	f := idleFacts()
	f.ci = openCIOf(pr, store.RequiredChecks{Checks: []string{"rspec"}, Source: store.RequiredFromConfig})
	v, _, _ := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a")
	pr.CI = nil
	f.ci = openCIOf(pr, store.RequiredChecks{Checks: []string{"rspec"}, Source: store.RequiredFromConfig})
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a")
}

// A branch GitHub says requires no checks leaves CI out of idle, even with
// a red rollup.
func TestAKnownEmptyRequiredListLeavesCIOut(t *testing.T) {
	pr := idleFacts().pr
	failure := "FAILURE"
	pr.CIState = &failure
	if ci := openCIOf(pr, store.RequiredChecks{Checks: []string{}, Source: store.RequiredFromGitHub}); ci != (openCI{}) {
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
