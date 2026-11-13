package fixwork

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// The settle waits settle_quiet after the PR's last activity: a reviewer
// who commented a minute ago may still be at work.
func TestAutofixOpenWaitsForTheSettle(t *testing.T) {
	f := idleFacts()
	f.Items = []Item{personThread(4100)}
	f.PR.HeadChangedAt = openNow.Add(-10 * time.Minute)
	activity := openNow.Add(-time.Minute)
	f.PR.ActivityAt = &activity
	v, work, _ := DecideOpen(f)
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
			f.Items = []Item{personThread(4100)}
			f.Settle.Checks = []string{"pronto"}
			f.PR.CI = tc.ci
			f.PR.HeadChangedAt = openNow.Add(-10 * time.Minute)
			v, _, _ := DecideOpen(f)
			if v.Verdict != tc.want {
				t.Errorf("verdict = %s %q, want %s", v.Verdict, v.Why, tc.want)
			}
			if tc.want == OpenWaits && !v.Until.Equal(f.PR.HeadChangedAt.Add(20*time.Minute)) {
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
	f.Items = []Item{personThread(4100)}
	f.PR.HeadChangedAt = openNow.Add(-21 * time.Minute)
	activity := openNow.Add(-time.Minute)
	f.PR.ActivityAt = &activity
	f.Ledger.Spell = Spell{Start: f.PR.HeadChangedAt, Seen: openNow.Add(-30 * time.Second)}
	f.Settle.Checks = []string{"pronto"}
	f.PR.CI = &store.CIStatus{SHA: fixHead, Checks: []store.CheckResult{{Name: "pronto", State: store.CheckPending}}}
	v, work, _ := DecideOpen(f)
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
	f.Items = []Item{personThread(4100)}
	old := "0000000000000000000000000000000000000000"
	f.PR.ReviewedSHA, f.PR.State = &old, store.PRQueued
	f.PR.HeadChangedAt = openNow.Add(-3 * time.Hour)
	due := openNow.Add(-30 * time.Minute)
	f.PR.NextEligibleAt = &due
	v, work, _ := DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum reviews a1a1a1a")
	if len(work) != 0 {
		t.Errorf("work = %v while magnum reviews", work)
	}
	due = openNow.Add(-2*time.Hour - time.Minute)
	f.PR.NextEligibleAt = &due
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since")
	if len(v.Needs) == 0 || v.Needs[len(v.Needs)-1].Kind != "magnum_late" {
		t.Errorf("needs = %+v, want magnum_late", v.Needs)
	}
	f.Off, f.PR.State = OpenOffBaseline, store.PRBaseline
	v, _, _ = DecideOpen(f)
	if v.Verdict != OpenOff || v.Off != OpenOffBaseline {
		t.Errorf("a baseline PR: %+v", v)
	}
}

// A head the fixer did not push is the operator's: no loop starts on it
// until hands_off_after_push passed. A head the fixer pushed is never
// waited for.
func TestAutofixOpenSkipsAPRYouPushedInTheLast30Minutes(t *testing.T) {
	f := idleFacts()
	f.Items = []Item{personThread(4100)}
	f.PR.HeadChangedAt = openNow.Add(-10 * time.Minute)
	f.Ledger.FixerHeads = nil
	v, work, _ := DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "you pushed a1a1a1a at")
	if !v.Until.Equal(f.PR.HeadChangedAt.Add(30*time.Minute)) || len(work) != 0 {
		t.Errorf("until %s, work %v", v.Until, work)
	}
	f.Ledger.FixerHeads = []string{fixHead}
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop")
}

// Unknown is never idle: a read GitHub cut short needs you (it cannot get
// whole by waiting), a failed read and no read since the PR changed are
// unknown, and below the GitHub floor the read waits.
func TestTheOpenRowIsNeverIdleWhenAReadWasCutShort(t *testing.T) {
	cases := map[string]struct {
		edit func(*Facts)
		want string
	}{
		"cut short":         {func(f *Facts) { f.Cut = []string{"over 1000 review threads"} }, OpenNeedsYou},
		"failed":            {func(f *Facts) { f.Failed = true }, OpenUnknown},
		"not read":          {func(f *Facts) { f.Whole = false }, OpenUnknown},
		"not read, low":     {func(f *Facts) { f.Whole, f.Low = false, true }, OpenWaits},
		"failed, low":       {func(f *Facts) { f.Failed, f.Low = true, true }, OpenWaits},
		"low, but read now": {func(f *Facts) { f.Low = true }, OpenIdle},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := idleFacts()
			tc.edit(&f)
			if v, _, _ := DecideOpen(f); v.Verdict != tc.want {
				t.Errorf("verdict = %s %q, want %s", v.Verdict, v.Why, tc.want)
			}
		})
	}
}

// Items are not known while the read is not whole: with no whole read, a
// failed one or one GitHub cut short, a person's handed thread and a bot's
// thread never handed count for nothing. The verdict holds no open item
// and no need, and the look gets no work and no needs (it would note the
// needs as waited on).
func TestNoItemCountsWhileTheReadIsNotWhole(t *testing.T) {
	person := personThread(4100)
	lint := Item{Key: "thread:4200@4200", Class: ClassBot, Kind: ItemThread, URL: prPage + "#discussion_r4200"}
	for name, edit := range map[string]func(*Facts){
		"failed":    func(f *Facts) { f.Failed = true },
		"cut short": func(f *Facts) { f.Cut = []string{"over 1000 review threads"} },
		"not read":  func(f *Facts) { f.Whole = false },
	} {
		t.Run(name, func(t *testing.T) {
			f := idleFacts()
			f.Items = []Item{person, lint}
			f.Ledger.Handed = map[string]time.Time{person.Key: openNow}
			edit(&f)
			v, work, needs := DecideOpen(f)
			if v.Open != 0 || len(v.Needs) != 0 || len(work) != 0 || len(needs) != 0 {
				t.Errorf("%s %q: open %d, verdict needs %+v, work %v, needs %v; want none of them", v.Verdict, v.Why, v.Open, v.Needs, itemKeys(work),
					itemKeys(needs))
			}
		})
	}
}

// Idle says nothing is open at the head and whether a person approved;
// every condition taken away gives something else.
func TestTheOpenRowIsIdleOnlyWhenEverythingHolds(t *testing.T) {
	v, work, needs := DecideOpen(idleFacts())
	wantVerdict(t, v, OpenIdle, "nothing open at a1a1a1a · approved, merge is yours")
	if len(work)+len(needs) != 0 || v.Open != 0 {
		t.Errorf("idle with work %v and needs %v", work, needs)
	}
	f := idleFacts()
	f.PR.ReviewGate = nil
	f.Approved, f.AfterCount = "c3c3c3c", 2
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenIdle, "nothing open at a1a1a1a · no approval yet · 2 commits after the approval")
	for name, edit := range map[string]func(*Facts){
		"a pending reply": func(f *Facts) { f.Pending = 1 },
		"a stalemate":     func(f *Facts) { f.Stalemate = []string{prPage + "#discussion_r1"} },
		"settle open": func(f *Facts) {
			a := openNow.Add(-time.Minute)
			f.PR.ActivityAt, f.PR.HeadChangedAt = &a, openNow.Add(-10*time.Minute)
		},
		"a pending check":     func(f *Facts) { f.CI = OpenCI{Pending: true} },
		"a red check":         func(f *Facts) { f.CI = OpenCI{Failed: "required check rspec"} },
		"a missing check":     func(f *Facts) { f.CI = OpenCI{Missing: []string{"Completion"}} },
		"a loop":              func(f *Facts) { f.Fix = &FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing} },
		"needs attention":     func(f *Facts) { f.Attention = "fix" },
		"an unreadable loop":  func(f *Facts) { f.FixBad = true },
		"a held fix":          func(f *Facts) { f.Held = HeldRestart },
		"magnum reviews":      func(f *Facts) { f.PR.State = store.PRQueued },
		"no review stands":    func(f *Facts) { f.Sum = nil },
		"an item":             func(f *Facts) { f.Items = []Item{personThread(4100)} },
		"magnum's review has": func(f *Facts) { f.Items = []Item{{Key: "review:r1", Class: ClassMagnum, Kind: ItemFindings}} },
	} {
		f := idleFacts()
		edit(&f)
		if v, _, _ := DecideOpen(f); v.Verdict == OpenIdle {
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
		f.PR.ReviewGate = c.gate
		v, _, _ := DecideOpen(f)
		wantVerdict(t, v, OpenIdle, c.why)
	}
}

// A handed item still open after its loop needs you, a person's and a bot's
// thread alike: a person's item never closes on the fixer's word alone. A
// bot's handed receipt or note on the conversation is seen and drops out.
func TestALeftoverPersonsItemOrBotThreadNeedsYouAndABotsReceiptIsSeen(t *testing.T) {
	person, botThread := personThread(4100), Item{Key: "thread:4200@4200", Class: ClassBot, Kind: ItemThread, URL: prPage + "#discussion_r4200"}
	receipt := Item{Key: "remark:701", Class: ClassBot, Kind: ItemComment, URL: prPage + "#issuecomment-701"}
	f := idleFacts()
	f.Items = []Item{person, botThread, receipt}
	f.Ledger.Handed = map[string]time.Time{person.Key: openNow, botThread.Key: openNow, receipt.Key: openNow}
	v, work, needs := DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "2 items need you: 1 thread of a person and 1 thread of a bot")
	if len(work) != 0 || len(needs) != 2 || len(v.Needs) != 2 || v.Needs[0].URL != person.URL || v.Needs[1].Kind != ItemThread {
		t.Errorf("work %v, needs %v, verdict needs %+v", work, needs, v.Needs)
	}
	f.Items = []Item{receipt}
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenIdle, "nothing open")
}

// magnum's rebuttal alone, on a review that leaves the fixer no work of
// its own, needs your answer and never starts a loop (prepareFix would
// refuse it as nothing to fix). With review work it starts one.
func TestOnlyAMagnumRebuttalWithNoReviewWorkNeedsYou(t *testing.T) {
	f := idleFacts()
	rebuttal := Item{Key: "mthread:5000@5002", Class: ClassMagnum, Kind: ItemMagnum, URL: prPage + "#discussion_r5000"}
	f.Items = []Item{rebuttal}
	v, work, _ := DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum's reply waits for your answer: "+rebuttal.URL)
	if len(work) != 0 {
		t.Errorf("work = %v, want no start", work)
	}
	if !slices.ContainsFunc(v.Needs, func(n OpenNeed) bool { return n.URL == rebuttal.URL }) {
		t.Errorf("verdict needs = %+v, want the reply's link", v.Needs)
	}
	f.Sum.Counts[2] = 1
	f.Ledger.Reviews = []string{"review:r1"}
	v, work, _ = DecideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop on 1 reply of magnum's")
	if len(work) != 1 {
		t.Errorf("work = %v", work)
	}
}

// The off verdict names why the look leaves the PR alone.
func TestTheOffVerdictNamesItsReason(t *testing.T) {
	for _, off := range []string{OpenOffDraft, OpenOffFork, OpenOffFixOff, OpenOffIncludeOwn, OpenOffBaseline, OpenOffIneligible} {
		f := idleFacts()
		f.Off = off
		v, work, needs := DecideOpen(f)
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
	f.Items = []Item{th}
	v, work, _ := DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "1 item needs you")
	if len(work) != 0 {
		t.Errorf("work = %v", work)
	}
}

// At most autofix_max_loops loops start on one PR in 24 hours; then the PR
// needs you, and f starts another.
func TestThreeAutofixLoopsADayNeedYou(t *testing.T) {
	f := idleFacts()
	f.Items = []Item{personThread(4100)}
	f.Ledger.Starts = []time.Time{openNow.Add(-25 * time.Hour), openNow.Add(-5 * time.Hour), openNow.Add(-3 * time.Hour)}
	v, _, _ := DecideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop")
	f.Ledger.Starts = append(f.Ledger.Starts, openNow.Add(-time.Hour))
	v, work, _ := DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "3 loops today: f starts another")
	if len(work) != 0 || len(v.Needs) == 0 || v.Needs[len(v.Needs)-1].Kind != "loop" {
		t.Errorf("work %v, needs %+v", work, v.Needs)
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
	if got, ok := ParseOpenVerdict(string(raw)); !ok || !got.SameAs(v) {
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
	f.Items = []Item{personThread(4100)}
	f.PollStale = true
	v, work, _ := DecideOpen(f)
	wantVerdict(t, v, OpenUnknown, "the poll of GitHub is behind")
	if len(work) != 0 {
		t.Errorf("work = %v on a stale poll", work)
	}
	f.Low = true
	v, _, _ = DecideOpen(f)
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
	f.Items = []Item{it}
	v, work, needs := DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "1 item needs you")
	if len(work) != 0 || len(needs) != 1 || !v.Leftovers {
		t.Errorf("work %+v, needs %+v, leftovers %v: want the thread among the leftovers", work, needs, v.Leftovers)
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
	f.Items = []Item{personThread(4100)}
	f.PR.HeadChangedAt = openNow.Add(-48 * time.Hour)
	activity := openNow.Add(-30 * time.Second)
	f.PR.ActivityAt = &activity
	f.Ledger.Spell = Spell{Start: openNow.Add(-2 * time.Minute), Seen: openNow.Add(-30 * time.Second)}
	v, work, _ := DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "settling")
	if !v.Until.Equal(activity.Add(3*time.Minute)) || len(work) != 0 {
		t.Errorf("until %s, work %v; want %s and no start", v.Until, work, activity.Add(3*time.Minute))
	}
	f.Ledger.Spell.Start = openNow.Add(-21 * time.Minute)
	v, work, _ = DecideOpen(f)
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
	s := Spell{}.Next(pr, quiet, gap, openNow, false)
	if !s.Start.Equal(*pr.ActivityAt) || !s.Seen.Equal(openNow) {
		t.Errorf("a new spell = %+v, want it to begin at the activity, seen now", s)
	}
	pr.ActivityAt = at(time.Minute)
	if next := s.Next(pr, quiet, gap, openNow.Add(90*time.Second), false); !next.Start.Equal(s.Start) {
		t.Errorf("a spell that goes on = %+v, want its start kept", next)
	}
	pr.ActivityAt = at(4 * time.Minute)
	if next := s.Next(pr, quiet, 20*time.Minute, openNow.Add(5*time.Minute), false); !next.Start.Equal(s.Start) {
		t.Errorf("a spell that goes on across looks 5 minutes apart = %+v, want its start kept", next)
	}
	pr.ActivityAt = at(time.Minute)
	if next := s.Next(pr, quiet, gap, openNow.Add(5*time.Minute), false); !next.Start.IsZero() {
		t.Errorf("a quiet PR's spell = %+v, want none", next)
	}
	if next := s.Next(pr, quiet, gap, openNow.Add(90*time.Second), true); !next.Start.IsZero() {
		t.Errorf("a spell under a loop = %+v, want none", next)
	}
	pr.ActivityAt = at(10 * time.Hour)
	if next := s.Next(pr, quiet, gap, openNow.Add(10*time.Hour+30*time.Second), false); !next.Start.Equal(*pr.ActivityAt) {
		t.Errorf("a spell after a night the look did not see = %+v, want a new one at %s", next, *pr.ActivityAt)
	}
}

// A look at the base that failed leaves the merge unknown, and unknown is
// never idle: an approved PR whose merge with its base could not be
// checked does not say "merge is yours". Work still starts a loop, whose
// job checks the base itself.
func TestAnUncheckedMergeIsNeverIdle(t *testing.T) {
	f := idleFacts()
	f.MergeUnchecked = "master"
	v, _, _ := DecideOpen(f)
	wantVerdict(t, v, OpenUnknown, "the merge with master could not be checked")
	f.Items = []Item{personThread(4100)}
	v, work, _ := DecideOpen(f)
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
	f.Items = []Item{personThread(4100)}
	f.Pause = "daemon paused lunch"
	v, work, needs := DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "daemon paused lunch: 1 thread of a person waits for `magnum resume`")
	if len(work) != 0 || len(needs) != 0 || len(v.Needs) != 0 {
		t.Errorf("work %v, needs %v, verdict needs %+v; want nothing started and nothing for you", work, needs, v.Needs)
	}
	f.PauseUntil = openNow.Add(2 * time.Hour)
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "waits until")
	if !v.Until.Equal(f.PauseUntil) {
		t.Errorf("until = %s, want the pause's end %s", v.Until, f.PauseUntil)
	}
	f.Pause, f.PauseUntil = "", time.Time{}
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop")
}

// [daemon] quiet_hours hold the look's new starts as they hold a full
// round: work waits until the window ends, the verdict says so, and once it
// ended the same work starts.
func TestQuietHoursHoldTheLooksStartUntilTheyEnd(t *testing.T) {
	f := idleFacts()
	f.Items = []Item{personThread(4100)}
	f.Settle.QuietHours = "03:00-10:00"
	f.Now = time.Date(2026, 11, 7, 4, 0, 0, 0, time.Local)
	f.PR.HeadChangedAt, f.Ledger.Spell = f.Now.Add(-time.Hour), Spell{}
	activity := f.Now.Add(-30 * time.Minute)
	f.PR.ActivityAt = &activity
	end := time.Date(2026, 11, 7, 10, 0, 0, 0, time.Local)
	v, work, needs := DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "quiet hours (03:00-10:00): 1 thread of a person waits until 10:00")
	if !v.Until.Equal(end) || len(work) != 0 || len(needs) != 0 {
		t.Errorf("until %s, work %v, needs %v; want %s and nothing started", v.Until, work, needs, end)
	}
	f.Now = end
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop")
}

// A head whose round falls inside [daemon] quiet_hours is reviewed at their
// end, so magnum_late_after counts from there: a push at 02:50 due at 03:05
// waits through the night and is late only at 12:00, two hours after the
// window's end, never at 05:05.
func TestMagnumLateCountsFromTheEndOfQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 7, h, m, 0, 0, time.Local) }
	f := idleFacts()
	f.Items = []Item{personThread(4100)}
	f.Settle.QuietHours = "03:00-10:00"
	old := "0000000000000000000000000000000000000000"
	f.PR.ReviewedSHA, f.PR.State = &old, store.PRRereviewPending
	f.PR.HeadChangedAt = at(2, 50)
	due := at(3, 5)
	f.PR.NextEligibleAt = &due
	f.Now = at(5, 10)
	v, _, _ := DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum reviews a1a1a1a")
	if !v.Until.Equal(at(10, 0)) {
		t.Errorf("until = %s, want the end of quiet hours", v.Until)
	}
	f.Now = at(11, 59)
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum reviews a1a1a1a")
	f.Now = at(12, 0)
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since 10:00")
}

// A review that was already late when quiet hours began is not late
// inside them: no needs you, so no toast, at night; at their end, still
// late, it needs you, counted from its own due time.
func TestMagnumIsNeverLateInsideQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 7, h, m, 0, 0, time.Local) }
	f := idleFacts()
	f.Settle.QuietHours = "03:00-10:00"
	old := "0000000000000000000000000000000000000000"
	f.PR.ReviewedSHA, f.PR.State = &old, store.PRRereviewPending
	f.PR.HeadChangedAt = at(0, 20)
	due := at(0, 30) // late at 02:30, before the window
	f.PR.NextEligibleAt = &due
	f.Now = at(4, 0)
	v, _, _ := DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum reviews a1a1a1a")
	if v.Late || !v.Until.Equal(at(10, 0)) {
		t.Errorf("late %v, until %s; want no late review inside quiet hours, a wait until their end", v.Late, v.Until)
	}
	f.Now = at(10, 0)
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since 00:30")
	if !v.Late {
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
		f.Settle.QuietHours = "03:00-10:00"
		old := "0000000000000000000000000000000000000000"
		f.PR.ReviewedSHA, f.PR.State = &old, store.PRRereviewPending
		f.PR.HeadChangedAt, f.PR.NextEligibleAt = tc.head, tc.due
		for _, now := range []time.Time{at(4, 30), at(10, 0), at(11, 59)} {
			f.Now = now
			if v, _, _ := DecideOpen(f); v.Verdict != OpenWaits || v.Late {
				t.Errorf("%s, at %s: verdict %s %q (late %v), want a wait for magnum's review", tc.name, now.Format("15:04"), v.Verdict, v.Why, v.Late)
			}
		}
		f.Now = at(12, 0)
		v, _, _ := DecideOpen(f)
		if v.Verdict != OpenNeedsYou || !v.Late || !strings.Contains(v.Why, "magnum has not reviewed a1a1a1a since 10:00") {
			t.Errorf("%s, at 12:00: verdict %s %q (late %v), want late since the window's end", tc.name, v.Verdict, v.Why, v.Late)
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
	f.PR.ReviewedSHA, f.PR.State = &old, store.PRRereviewPending
	f.PR.HeadChangedAt = at(14, 10)
	f.Pause, f.PauseUntil = "daemon paused", at(18, 0)
	f.Now = at(16, 15)
	v, _, _ := DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "daemon paused: magnum's review of a1a1a1a waits until 18:00")
	if v.Late || len(v.Needs) != 0 || !v.Until.Equal(at(18, 0)) {
		t.Errorf("late %v, needs %+v, until %s; want a wait until the pause's end", v.Late, v.Needs, v.Until)
	}
	f.PauseUntil = time.Time{}
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "daemon paused: magnum's review of a1a1a1a waits for `magnum resume`")
	f.Pause, f.Pauses = "", []Hold{{From: at(14, 0), To: at(18, 0)}} // resumed at 18:00
	f.Now = at(19, 59)
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "magnum reviews a1a1a1a")
	f.Now = at(20, 0)
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since 18:00")
	f.PR.HeadChangedAt, f.Now = at(11, 0), at(18, 0) // late at 13:00, before the pause
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since 11:00")
	f.PR.HeadChangedAt, f.PR.Forced, f.Now = at(14, 10), true, at(16, 15)
	f.Pause, f.Pauses = "daemon paused", nil
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "magnum has not reviewed a1a1a1a since 14:10")
}

// An optional call the fixer left the operator holds nothing, but the
// operator sees it at the merge: the idle verdict ends with "1 optional
// call: <link>" (counted, each linked), approved or not, and carries the
// links for the factory screen's merge move.
func TestTheIdleVerdictNamesTheOptionalCalls(t *testing.T) {
	const link = prPage + "#discussion_r4302866759"
	f := idleFacts()
	f.Optional = []OptionalCall{{Thread: 4302866759, URL: link}}
	v, work, needs := DecideOpen(f)
	wantVerdict(t, v, OpenIdle, "nothing open at a1a1a1a · approved, merge is yours · 1 optional call: "+link)
	if len(work)+len(needs) != 0 || len(v.Optional) != 1 || v.Optional[0] != link {
		t.Errorf("work %v, needs %v, optional %q; want only the link", work, needs, v.Optional)
	}
	f.PR.ReviewGate = nil
	f.Optional = append(f.Optional, OptionalCall{Thread: 4302866800, URL: prPage + "#discussion_r4302866800"})
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenIdle, "nothing open at a1a1a1a · no approval yet · 2 optional calls: "+link+", "+prPage+"#discussion_r4302866800")
	if got := OptionalCallsWords(nil); got != "" {
		t.Errorf("OptionalCallsWords(nil) = %q", got)
	}
}

// A colleague's change request on an own PR waited for the author, handed
// to an earlier loop, while a lint bot's thread on the merge's code sat
// beside it, never handed: rule 8 said needs you before rule 9 looked at the
// work, so the small fix waited on the person. Work never handed now starts
// a loop while the items that wait are all a person's: they stay listed as
// needs in the verdict and its words. Anything else handed and still open
// (magnum's finding skipped without a word, a bot's thread, a conflict)
// still needs you first, and so do a person's items with no work beside
// them.
func TestTheLookStartsOnNeverHandedWorkWhileAPersonsItemsWait(t *testing.T) {
	person := personThread(4100)
	lint := Item{Key: "thread:4200@4200", Class: ClassBot, Kind: ItemThread, URL: prPage + "#discussion_r4200"}
	f := idleFacts()
	f.Items = []Item{person, lint}
	f.Ledger.Handed = map[string]time.Time{person.Key: openNow}
	v, work, needs := DecideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop on 1 thread of a bot; 1 item waits for you: 1 thread of a person")
	if len(work) != 1 || work[0].Key != lint.Key || len(needs) != 1 || needs[0].Key != person.Key || len(v.Needs) != 1 || v.Needs[0].URL != person.URL {
		t.Errorf("work %v, needs %v, verdict needs %+v; want the bot's thread as work and the person's listed", work, needs, v.Needs)
	}
	if v.Leftovers {
		t.Error("the person's item is raised while the loop starts")
	}
	skipped := Item{Key: "rthread:5000", Class: ClassMagnum, Kind: ItemFinding, URL: prPage + "#discussion_r5000"}
	f.Items = []Item{person, skipped, lint}
	v, work, _ = DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "2 items need you")
	if len(work) != 0 {
		t.Errorf("work = %v beside magnum's skipped finding", work)
	}
	f.Items = []Item{person}
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "1 item needs you: 1 thread of a person")
}

// A person's handed items wait beside work only for the loop the work
// starts. While the work itself waits (a pause, quiet hours, the settle,
// the hands off after your push) the verdict still names the person's
// items, which the look raises once the loop ran; raising them now would
// hold the PR in needs attention, and no loop would start on the work. At
// the daily cap no loop starts today: rule 8 then raises them, as before.
func TestAPersonsWaitingItemsStayNamedWhileTheWorkCannotStart(t *testing.T) {
	person := personThread(4100)
	lint := Item{Key: "thread:4200@4200", Class: ClassBot, Kind: ItemThread, URL: prPage + "#discussion_r4200"}
	facts := func() Facts {
		f := idleFacts()
		f.Items = []Item{person, lint}
		f.Ledger.Handed = map[string]time.Time{person.Key: openNow}
		return f
	}
	f := facts()
	f.Pause = "fixes paused"
	v, _, needs := DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "fixes paused: 1 thread of a bot waits for `magnum resume`; 1 item waits for you: 1 thread of a person")
	if v.Leftovers || len(needs) != 1 || len(v.Needs) != 1 {
		t.Errorf("paused: leftovers %v, needs %v, verdict needs %+v; want the person's item listed, not raised", v.Leftovers, needs, v.Needs)
	}
	f = facts()
	act := openNow.Add(-time.Minute)
	f.PR.HeadChangedAt, f.PR.ActivityAt = openNow.Add(-10*time.Minute), &act
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "settling: 1 thread of a bot waits until "+WaitClock(act.Add(3*time.Minute), openNow)+
		"; 1 item waits for you: 1 thread of a person")
	if v.Leftovers {
		t.Error("settling: the person's item is raised")
	}
	f = facts()
	f.Ledger.Starts = []time.Time{openNow.Add(-5 * time.Hour), openNow.Add(-3 * time.Hour), openNow.Add(-time.Hour)}
	v, work, _ := DecideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "1 item needs you: 1 thread of a person")
	if !v.Leftovers || len(work) != 0 {
		t.Errorf("at the cap: leftovers %v, work %v; want the person's item raised as rule 8 does", v.Leftovers, work)
	}
}

// The re-run waits for the end of `magnum pause` and for the GitHub budget
// of the identity it re-runs as (the poll identity's, f.low, and
// ci_dispatch_as's, f.ciLow), as a start does, and so does the read of
// the head's runs for the budget; without either, the verdict asks the
// look to read, then to re-run.
func TestTheReRunWaitsForThePauseAndTheBudget(t *testing.T) {
	f := idleFacts()
	f.CI = OpenCI{Missing: []string{"build"}}
	v, _, _ := DecideOpen(f)
	if !v.ReadRuns || v.Rerun {
		t.Fatalf("verdict = %+v, want the read of the head's runs asked for", v)
	}
	f.Low = true
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check build did not run; magnum reads the head's runs once the GitHub budget allows")
	if v.ReadRuns {
		t.Errorf("read asked for below the floor")
	}
	f.Low = false
	f.CIRerun = CIRerun{Head: fixHead, ReadAt: openNow.Add(-time.Minute),
		Run: &CINoJob{ID: liveRunID, Workflow: "ci.yml", Attempt: 1, Conclusion: "failure"}}
	v, _, _ = DecideOpen(f)
	if !v.Rerun || v.ReadRuns || v.Verdict != OpenNeedsYou {
		t.Fatalf("verdict = %+v, want the re-run asked for", v)
	}
	wantVerdict(t, v, OpenNeedsYou, "required check build did not run at a1a1a1a: magnum re-runs run 42673098082 of ci.yml")
	f.Pause, f.PauseUntil = "daemon paused", openNow.Add(time.Hour)
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check build did not run; magnum re-runs run 42673098082 of ci.yml once the pause ends")
	if v.Rerun || !v.Until.Equal(f.PauseUntil) {
		t.Errorf("rerun %v, until %s under the pause", v.Rerun, v.Until)
	}
	f.Pause, f.PauseUntil, f.CILow = "", time.Time{}, true
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check build did not run; magnum re-runs run 42673098082 of ci.yml once the GitHub budget allows")
	if v.Rerun {
		t.Errorf("re-run asked for below the floor")
	}
}

// The start waits for the end of `magnum pause` and for the GitHub budget
// of the identity it starts as; without either, the verdict asks the look
// to start it.
func TestTheStartOfTheCIWorkflowWaitsForThePauseAndTheBudget(t *testing.T) {
	f := idleFacts()
	f.CI = OpenCI{Missing: []string{"Completion"}}
	f.CIWorkflow = "ci.yml"
	v, _, _ := DecideOpen(f)
	if !v.Dispatch || v.Verdict != OpenNeedsYou {
		t.Fatalf("verdict = %+v, want the start asked for", v)
	}
	f.Pause, f.PauseUntil = "daemon paused", openNow.Add(time.Hour)
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check Completion did not run; magnum starts ci.yml once the pause ends")
	if v.Dispatch || !v.Until.Equal(f.PauseUntil) {
		t.Errorf("dispatch %v, until %s under the pause", v.Dispatch, v.Until)
	}
	f.Pause, f.PauseUntil, f.Low = "", time.Time{}, true
	v, _, _ = DecideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check Completion did not run; magnum starts ci.yml once the GitHub budget allows")
	if v.Dispatch {
		t.Error("asked to start it below the GitHub floor")
	}
}

// An accepted item waits for no one where it would wait for you (handed to
// a loop, a thread a fixer resolved): no work and no need. One never handed
// is work as before: a reviewer's comment and a separate approval still ask
// for the fix.
func TestAnAcceptedItemWaitsForNoOneButWorkStaysWork(t *testing.T) {
	handed, fresh, resolved := personThread(4100), personThread(4200), personThread(4300)
	handed.Accepted, fresh.Accepted = true, true
	resolved.Resolved, resolved.Accepted = true, true
	f := idleFacts()
	f.Items = []Item{handed, fresh, resolved}
	f.Ledger.Handed = map[string]time.Time{handed.Key: openNow}
	v, work, needs := DecideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop on 1 thread of a person")
	if len(work) != 1 || work[0].Key != fresh.Key || len(needs) != 0 {
		t.Errorf("work %v, needs %v; want the never-handed thread as work alone", work, needs)
	}
	if got := waitsOf(acceptedWaits(f.Items, f.Ledger)); !slices.Equal(got, []string{handed.Key}) {
		t.Errorf("waits = %v, want the handed accepted thread", got)
	}
}

// A reviewer's pushback counts from the earliest hand-off of one of their
// person's items to a loop (Handed), unless an earlier time stands: not
// from a bot's, nor from an item never handed. A CI fix by hand seeds it
// the same way from the look's last read (lastReadWaits), and leaves the
// ledger it read as it was.
func TestPushbackCountsFromTheLoopsHandOff(t *testing.T) {
	ann1, ann2, fresh, alices := personThread(4100), personThread(4200), personThread(4300), personThread(4400)
	ann1.By, ann2.By, fresh.By, alices.By = "rev-ann", "Rev-Ann", "carol", "alice"
	bot := Item{Key: ThreadKey(4500, 4500), Class: ClassBot, Kind: ItemThread, By: "lint-bot[bot]"}
	l := Ledger{
		Handed: map[string]time.Time{ann1.Key: openNow.Add(-time.Hour), ann2.Key: openNow.Add(-2 * time.Hour), alices.Key: openNow.Add(-3 * time.Hour),
			bot.Key: openNow.Add(-4 * time.Hour)},
		Waited: map[string]time.Time{"alice": openNow.Add(-5 * time.Hour)},
	}
	items := []Item{ann1, ann2, fresh, alices, bot}
	l.Read.Others = items
	waited := maps.Clone(l.Waited)
	keys := l.LastReadWaits()
	if want := "changes:rev-ann@" + strconv.FormatInt(openNow.Add(-2*time.Hour).UnixNano(), 10); !slices.Contains(keys, want) {
		t.Errorf("a CI fix by hand's waits = %v, want %s", keys, want)
	}
	if !maps.Equal(l.Waited, waited) {
		t.Errorf("lastReadWaits wrote the ledger it read: waited %v", l.Waited)
	}
	l.SeedWaited(items)
	if want := map[string]time.Time{"rev-ann": openNow.Add(-2 * time.Hour), "alice": openNow.Add(-5 * time.Hour)}; !maps.Equal(l.Waited, want) {
		t.Errorf("waited = %v, want %v", l.Waited, want)
	}
}

// While the settle confirms the push, the loop says so: the board's state
// cell, the PR's status line (the look's verdict) and the dispatch wait.
func TestTheLoopSaysItConfirmsThePushWhileGitHubDoesNotAnswer(t *testing.T) {
	const head = "e6c1f37" + "000000000000000000000000000000000"
	st := FixState{Cycle: 1, Cap: 5, Phase: FixPhaseFixing, Role: "claude-fix", Confirm: &FixConfirm{Head: head}}
	if got, want := st.Short(), "fixing 1/5 · confirming e6c1f37"; got != want {
		t.Errorf("Short = %q, want %q", got, want)
	}
	if got, want := st.Confirming(), "confirming the push of e6c1f37 (GitHub does not answer)"; got != want {
		t.Errorf("Confirming = %q, want %q", got, want)
	}
	if got := (FixState{Cycle: 1, Cap: 5, Phase: FixPhaseFixing, Role: "claude-fix"}).Confirming(); got != "" {
		t.Errorf("Confirming of a loop at work = %q, want none", got)
	}
	f := Facts{Fix: &st, PR: store.PR{HeadSHA: fixHead}}
	if v, _, _ := DecideOpen(f); v.Verdict != OpenFixing || v.Why != "fixing, confirming the push of e6c1f37 (GitHub does not answer)" {
		t.Errorf("the verdict = %s %q", v.Verdict, v.Why)
	}
}
