package engine

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// lowBudget tells the meter that identity's GraphQL budget is at remaining
// of 5000 points until until, as a call answered it, and returns until.
func (h *harness) lowBudget(identity string, remaining int, until time.Time) time.Time {
	h.e.ghMeter.sawRate(identity, github.RateLimit{Limit: 5000, Remaining: remaining, ResetAt: until}, h.clock.Now())
	return until
}

// waitOf is PR #n's recorded wait (ok false: none).
func (h *harness) waitOf(n int) (Wait, bool) {
	h.t.Helper()
	v, ok := kvValue(h, KVPRWait(h.pr(n).ID))
	if !ok {
		return Wait{}, false
	}
	return ParseWait(v)
}

// wantGitHubWait fails unless PR #n waits for identity's GitHub budget
// until until, as the board and `magnum status` show it.
func (h *harness) wantGitHubWait(n int, identity string, remaining int, until time.Time) {
	h.t.Helper()
	w, ok := h.waitOf(n)
	clock := until.Local().Format("15:04")
	if !ok || w.Reason != WaitGitHub || w.Subject != identity || !w.Until.Equal(until) {
		h.t.Fatalf("PR #%d waits %+v, %v; want the GitHub budget of %s until %s", n, w, ok, identity, clock)
	}
	if got := w.Short(h.clock.Now()); !strings.HasSuffix(got, " · github budget → "+clock) {
		h.t.Errorf("Short = %q", got)
	}
	want := "waits: github budget: " + identity + " at " + strconv.Itoa(remaining) + " points until " + clock
	if got := w.Sentence("talkable#"+strconv.Itoa(n), h.clock.Now()); !strings.Contains(got, want) || strings.Contains(got, "runs it now") {
		h.t.Errorf("Sentence = %q, want %q and no override hint", got, want)
	}
}

// On 2026-10-20 the operator's gh login ran out of its GraphQL budget from
// 16:43 to 16:56 and the radar poll failed: rounds and jobs spent it to
// zero, since only the poll held back below a floor. Below [github]
// budget_floor (500 points by default) of the poll identity's budget, as
// the radar answered it, no round starts: the PR waits with the reason
// "github budget: zhuravel at 300 points until HH:MM", and starts after the
// reset.
func TestARoundWaitsBelowTheGitHubFloorAndStartsAfterTheReset(t *testing.T) {
	h := newHarness(t)
	until := h.clock.Now().Add(30 * time.Minute)
	h.gh.mu.Lock()
	h.gh.rate = github.RateLimit{Limit: 5000, Remaining: 300, Cost: 1, ResetAt: until}
	h.gh.mu.Unlock()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "head2"})
	h.tick() // #2 queued
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRQueued)
	if n := len(h.rd.all()); n != 0 {
		t.Fatalf("rounds = %d below the floor, want none", n)
	}
	h.wantGitHubWait(2, "zhuravel", 300, until)
	if gate, _ := kvValue(h, store.KVPRGate(h.pr(2).ID)); gate != "github budget: zhuravel at 300 points until "+until.Local().Format("15:04") {
		t.Errorf("gate = %q", gate)
	}
	if n := len(h.events("github.budget_low")); n != 1 {
		t.Errorf("github.budget_low events = %d, want 1", n)
	}

	h.gh.mu.Lock()
	h.gh.rate = github.RateLimit{} // no call tells the new window: the reset passing alone ends the hold
	h.gh.mu.Unlock()
	h.advance(31 * time.Minute) // past the reset
	h.tick()
	h.wantState(2, store.PRReviewed)
	if n := len(h.events("github.budget_recovered")); n != 1 {
		t.Errorf("github.budget_recovered events = %d, want 1", n)
	}
}

// The floor holds a round whose posting identity's budget is low too: the
// round posts and verifies as it.
func TestARoundWaitsForItsPostingIdentitysBudget(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "head2"})
	h.tick()
	until := h.lowBudget("talkable-app", 120, h.clock.Now().Add(20*time.Minute))
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRQueued)
	h.wantGitHubWait(2, "talkable-app", 120, until)
}

// budget_floor = 0 turns the floor off: the round starts however low the
// budget.
func TestABudgetFloorOfZeroLetsTheRoundStart(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.GitHub.BudgetFloor = 0 })
	h.gh.mu.Lock()
	h.gh.rate = github.RateLimit{Limit: 5000, Remaining: 300, Cost: 1, ResetAt: h.clock.Now().Add(30 * time.Minute)}
	h.gh.mu.Unlock()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "head2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
}

// A round that runs when the budget drops below the floor finishes: its
// settle and posting reads are the round's, and the floor is checked only
// where work starts. The PR queued meanwhile waits.
func TestARunningRoundFinishesBelowTheFloor(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "head2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before) // #2's round runs, held in its pipeline
	until := h.lowBudget("zhuravel", 200, h.clock.Now().Add(40*time.Minute))
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "head2"}, prSpec{n: 3, head: "head3"})
	if err := h.e.Tick(h.ctx); err != nil { // #3 queued
		t.Fatal(err)
	}
	h.advance(5 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil { // #3 due, held by the floor
		t.Fatal(err)
	}
	close(h.rd.gate)
	h.settle()
	h.rd.mu.Lock()
	h.rd.gate = nil
	h.rd.mu.Unlock()
	h.wantState(2, store.PRReviewed)
	h.wantState(3, store.PRQueued)
	h.wantGitHubWait(3, "zhuravel", 200, until)
}

// `magnum review` forces the round, which still waits for the budget, and
// the answer says why.
func TestAForcedReviewWaitsForTheGitHubBudgetAndSaysSo(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "head2")
	until := h.lowBudget("zhuravel", 310, h.clock.Now().Add(25*time.Minute))
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	want := "; waiting: github budget: zhuravel at 310 points until " + until.Local().Format("15:04")
	if r := h.request(id); r.State != store.RequestDone || !strings.HasSuffix(deref(r.Result), want) {
		t.Fatalf("magnum review = %s %q, want an answer ending %q", r.State, deref(r.Result), want)
	}
	before := len(h.rd.all())
	h.tick()
	if n := len(h.rd.all()); n != before {
		t.Fatalf("a forced round started below the floor (%d rounds, %d before)", n, before)
	}
	h.wantGitHubWait(2, "zhuravel", 310, until)
}

// `magnum fix` below the floor of the poll identity the fix reads as is held,
// not refused, and answered so; nothing starts while the budget is low, a
// restart keeps the held fix (the new daemon knows the budget from the
// meter's rows), and the tick that finds the budget recovered starts it on
// the review that still stands.
func TestAFixAskedBelowTheFloorStartsOnceTheBudgetRecovers(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	pushingFixer(h, git, fixPushed) // a fixer started too early ends at once
	until := h.lowBudget("zhuravel", 300, h.clock.Now().Add(30*time.Minute))
	id := h.fix(FixPayload{})
	h.handle()
	want := "held; the fix of talkable/talkable#2 starts once the GitHub budget of zhuravel recovers if the review on a1a1a1a still stands " +
		"(github budget: zhuravel at 300 points until " + until.Local().Format("15:04") + ")"
	if res := h.wantDone(id); res != want {
		t.Fatalf("magnum fix below the floor = %q, want %q", res, want)
	}
	raw, ok := kvValue(h, KVPRFixHeld(pr.ID))
	if !ok || !strings.Contains(raw, heldBudget) || HeldFixStarts(raw) != "when the GitHub budget recovers" {
		t.Fatalf("held fix = %q, %v", raw, ok)
	}
	if ev, ok := h.fixEvent("fix.held"); !ok || !strings.Contains(ev.Message, "once the GitHub budget of zhuravel recovers") {
		t.Errorf("fix.held = %+v, %v", ev, ok)
	}
	h.tick()
	h.e = New(h.d) // a restart while the budget is still low: the new engine reads the meter's rows
	h.startup()
	h.tick()
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Fatalf("fix runs = %d below the floor, want none", n)
	}
	if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); !ok {
		t.Fatal("the held fix was dropped while the budget was low")
	}

	h.advance(31 * time.Minute) // past the reset
	h.tick()
	if runs := h.fixRuns(pr.ID); len(runs) != 1 || runs[0].TargetSHA != fixHead {
		t.Fatalf("fix runs after the recovery = %+v, want cycle 1 on %s", runs, fixHead)
	}
	if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); ok {
		t.Error("the held fix outlived its start")
	}
	if ev, ok := h.fixEvent("fix.dropped"); ok {
		t.Errorf("fix.dropped recorded: %s", ev.Message)
	}
}

// A dry run of `magnum fix` below the floor says it would hold the fix.
func TestADryRunFixBelowTheFloorSaysItWouldHoldIt(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	h.lowBudget("zhuravel", 300, h.clock.Now().Add(30*time.Minute))
	id := h.fix(FixPayload{DryRun: true})
	h.handle()
	if res := h.wantDone(id); !strings.HasPrefix(res, "dry run: would hold the fix until the GitHub budget recovers (github budget: zhuravel at 300 points") {
		t.Fatalf("dry run = %q", res)
	}
	if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); ok {
		t.Error("a dry run held the fix")
	}
}

// A loop whose re-review calls for a next cycle while the budget is below
// the floor holds that cycle, as a drain does, and says so (fix.waiting);
// the tick that finds the budget recovered starts it.
func TestANextFixCycleWaitsForTheGitHubBudget(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"), fixPushed2)
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	before := len(h.rd.all())
	reqPollNoSettle(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	h.awaitRound(before) // the re-review runs
	h.lowBudget("zhuravel", 250, h.clock.Now().Add(30*time.Minute))
	close(h.rd.gate)
	h.settle()
	h.rd.mu.Lock()
	h.rd.gate = nil
	h.rd.mu.Unlock()

	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d, want 1: the next cycle started below the floor", n)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 || !st.Held {
		t.Fatalf("fix state = %+v, %v; want the loop held before its next cycle", st, ok)
	}
	if ev, ok := h.fixEvent("fix.waiting"); !ok || !strings.Contains(ev.Message, "github budget: zhuravel at 250 points") {
		t.Errorf("fix.waiting = %+v, %v", ev, ok)
	}
	h.tick()
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d while the budget is low, want 1", n)
	}

	h.advance(31 * time.Minute)
	h.tick()
	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 || runs[1].TargetSHA != fixPushed {
		t.Fatalf("fix runs after the recovery = %+v, want cycle 2 on %s", runs, fixPushed)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Cycle != 2 {
		t.Errorf("fix state after the recovery = %+v, %v", st, ok)
	}
}

// An autofix below the floor is held as `magnum fix` would be, and starts
// once the budget recovers.
func TestAnAutofixBelowTheFloorStartsOnceTheBudgetRecovers(t *testing.T) {
	h, git := autofixHarness(t, "zhuravel", findings(map[string]int{"P2": 2}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.lowBudget("zhuravel", 300, h.clock.Now().Add(30*time.Minute))
	close(h.rd.gate)
	h.settle()
	h.rd.mu.Lock()
	h.rd.gate = nil
	h.rd.mu.Unlock()
	pr := h.wantState(2, store.PRReviewed)
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Fatalf("fix runs = %d, want the autofix held", n)
	}
	raw, ok := kvValue(h, KVPRFixHeld(pr.ID))
	if !ok || !strings.Contains(raw, `"by":"autofix"`) {
		t.Fatalf("held fix = %q, %v; want the autofix held", raw, ok)
	}

	h.advance(31 * time.Minute)
	h.tick()
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs after the recovery = %d, want 1", n)
	}
	if by := h.fixStartBy(); by != "autofix" {
		t.Errorf("the loop was started by %q, want autofix", by)
	}
}

// `magnum retro` below the floor is queued until the budget recovers, and
// the daily retro waits too: the retro reads the PRs of every watch.
func TestARetroWaitsForTheGitHubBudget(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	until := h.lowBudget("zhuravel", 300, h.clock.Now().Add(30*time.Minute))
	req := h.requestRetro(RetroPayload{Lookback: "3d"})
	want := "queued: starts when the GitHub budget of zhuravel recovers (github budget: zhuravel at 300 points until " + until.Local().Format("15:04") +
		"); `magnum logs` follows it"
	if req.State != store.RequestDone || deref(req.Result) != want {
		t.Fatalf("magnum retro below the floor = %s %q, want %q", req.State, deref(req.Result), want)
	}
	h.tick()
	if n := len(h.events("retro.start")); n != 0 {
		t.Fatalf("retro.start events = %d below the floor", n)
	}
	h.advance(31 * time.Minute)
	h.tick()
	if n := len(h.events("retro.start")); n != 1 {
		t.Fatalf("retro.start events after the recovery = %d, want 1", n)
	}
}

// The day's retro does not start below the floor.
func TestTheDailyRetroWaitsForTheGitHubBudget(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc), func(h *harness) {
		h.cfg.Learn.Enabled = true
		h.cfg.Learn.DailyAt = "00:00"
	})
	h.lowBudget("zhuravel", 300, h.clock.Now().Add(30*time.Minute))
	h.e.maybeRetro(h.ctx)
	h.settle()
	if n := len(h.events("retro.start")); n != 0 {
		t.Fatalf("the daily retro started below the floor (%d)", n)
	}
	h.advance(31 * time.Minute)
	h.e.maybeRetro(h.ctx)
	h.settle()
	if n := len(h.events("retro.start")); n != 1 {
		t.Fatalf("retro.start events after the recovery = %d, want 1", n)
	}
}

// A notes curation asked below the floor is queued until the budget
// recovers, then runs.
func TestACurationAskedBelowTheFloorWaitsForTheBudget(t *testing.T) {
	h, _, _ := curationHarness(t, goodProposal, func(h *harness) { h.cfg.Notes.Curate = config.CurateTriggers{} })
	h.lowBudget("zhuravel", 300, h.clock.Now().Add(30*time.Minute))
	msg, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"})
	if err != nil || !strings.Contains(msg, "queued: starts when the GitHub budget recovers") {
		t.Fatalf("requestCurate = %q, %v", msg, err)
	}
	if q := ReadCurateQueue(h.ctx, h.st); len(q) != 1 || q[0].Why != QueuedGitHub {
		t.Fatalf("queue = %+v", q)
	}
	h.tick()
	if n := len(proposals(t, h)); n != 0 {
		t.Fatalf("a curation ran below the floor: %d proposals", n)
	}
	h.advance(31 * time.Minute)
	h.tick()
	if n := len(proposals(t, h)); n != 1 {
		t.Fatalf("proposals after the recovery = %d, want 1", n)
	}
}

// A posted review's whiteboard below the floor is skipped and remembered,
// as a drain's is, and made once the budget recovers; `magnum whiteboard`
// is refused with the budget and its reset.
func TestAWhiteboardBelowTheFloorIsMadeOnceTheBudgetRecovers(t *testing.T) {
	w := wbHarness(t, true)
	wbWriter(w.harness, wbDone(wbSession))
	w.open(prSpec{n: 1, head: "base1"})
	w.startup()
	w.tick()
	w.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead})
	w.tick()
	w.advance(5 * time.Minute)
	w.rd.mu.Lock()
	w.rd.gate = make(chan struct{})
	w.rd.mu.Unlock()
	before := len(w.rd.all())
	if err := w.e.Tick(w.ctx); err != nil {
		t.Fatal(err)
	}
	w.awaitRound(before)
	w.lowBudget("zhuravel", 300, w.clock.Now().Add(30*time.Minute))
	close(w.rd.gate)
	w.settle()
	w.rd.mu.Lock()
	w.rd.gate = nil
	w.rd.mu.Unlock()
	pr := w.wantState(2, store.PRReviewed)
	if runs := w.wbRuns(pr.ID); len(runs) != 0 {
		t.Fatalf("whiteboard runs below the floor = %+v", runs)
	}
	if ev, ok := w.fixEvent("whiteboard.skipped"); !ok || !strings.Contains(ev.Message, "github budget: zhuravel at 300 points") ||
		!strings.Contains(ev.Message, "it is made once the budget recovers") {
		t.Errorf("whiteboard.skipped = %+v, %v", ev, ok)
	}
	if p, ok := w.wbPending(pr.ID); !ok || p.Head != fixHead {
		t.Fatalf("pending whiteboard = %+v, %v", p, ok)
	}
	id := w.wbRequest()
	w.wbAnswered(id)
	w.wbFailed(id, "no whiteboard now: github budget: zhuravel at 300 points")

	w.advance(31 * time.Minute)
	w.tick()
	w.settle()
	if runs := w.wbRuns(pr.ID); len(runs) != 1 || runs[0].TargetSHA != fixHead {
		t.Fatalf("whiteboard runs after the recovery = %+v, want one for %s", runs, fixHead)
	}
	if p, ok := w.wbPending(pr.ID); ok {
		t.Errorf("the pending whiteboard stays: %+v", p)
	}
}

// A clean review's fix reads the other reviewers' threads and comments to
// know whether anything waits (spec 96): below the floor it reads none of
// them and takes the request as held, since the read would spend the very
// budget the floor keeps, and a read that GitHub refuses at exhaustion
// would drop the request. A restart below the floor still reads nothing;
// the read comes when the held fix starts, after the recovery.
func TestACleanReviewsFixBelowTheFloorReadsNothingUntilTheBudgetRecovers(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{})
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {
		extThread(4322458360, "lib/sandbox.rb", 185, h.clock.Now().Add(-time.Hour), "MINOR: `run` drops stderr", "rev-ann"),
	}}
	h.gh.mu.Unlock()
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	until := h.lowBudget("zhuravel", 200, h.clock.Now().Add(30*time.Minute))
	reads := func() int {
		return h.gh.count("issue_comments:talkable/talkable#2") + h.gh.count("threads:talkable/talkable#2")
	}
	before := reads()
	id := h.fix(FixPayload{})
	h.handle()
	want := "held; the fix of talkable/talkable#2 starts once the GitHub budget of zhuravel recovers if the review on a1a1a1a still stands " +
		"(github budget: zhuravel at 200 points until " + until.Local().Format("15:04") + ")"
	if res := h.wantDone(id); res != want {
		t.Fatalf("magnum fix of a clean review below the floor = %q, want %q", res, want)
	}
	if n := reads() - before; n != 0 {
		t.Fatalf("the held request read the other reviewers %d times below the floor", n)
	}
	if ev, ok := h.fixEvent("fix.held"); !ok || !strings.Contains(ev.Message, "read once the GitHub budget recovers") {
		t.Errorf("fix.held = %+v, %v", ev, ok)
	}
	h.tick()
	h.e = New(h.d) // a restart below the floor
	h.startup()
	h.tick()
	if n := reads() - before; n != 0 {
		t.Fatalf("the restart read the other reviewers %d times below the floor", n)
	}
	if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); !ok {
		t.Fatal("the restart dropped the held fix")
	}

	h.advance(31 * time.Minute)
	h.tick()
	if runs := h.fixRuns(pr.ID); len(runs) != 1 {
		t.Fatalf("fix runs after the recovery = %+v, want the fixer's", runs)
	}
	if n := reads() - before; n == 0 {
		t.Error("the fix that started never read the other reviewers")
	}
	if ev, ok := h.fixEvent("fix.dropped"); ok {
		t.Errorf("fix.dropped recorded: %s", ev.Message)
	}
}

// A held fix starts at the tick that finds the budget recovered before
// dispatch gives the PR to another round: a forced review asked while the
// fix was held waits for the fixer instead of taking the PR and having the
// held fix dropped as "a round in flight".
func TestAHeldFixStartsBeforeARoundThatWouldTakeThePR(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	pushingFixer(h, git, fixPushed)
	h.lowBudget("zhuravel", 300, h.clock.Now().Add(30*time.Minute))
	fix := h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(fix); !strings.HasPrefix(res, "held; ") {
		t.Fatalf("magnum fix below the floor = %q", res)
	}
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(review); r.State != store.RequestDone {
		t.Fatalf("magnum review = %s %q", r.State, deref(r.Result))
	}
	rounds := len(h.rd.all())

	h.advance(31 * time.Minute)
	h.tick()
	if runs := h.fixRuns(pr.ID); len(runs) != 1 || runs[0].TargetSHA != fixHead {
		t.Fatalf("fix runs after the recovery = %+v, want the held fix's cycle 1", runs)
	}
	if ev, ok := h.fixEvent("fix.dropped"); ok {
		t.Fatalf("the held fix was dropped: %s", ev.Message)
	}
	if n := len(h.rd.all()); n != rounds {
		// the fixer pushed, so its loop's own forced re-review may run: but not before the fixer
		if st, ok := h.fixStateOf(2); !ok || st.Cycle != 1 {
			t.Errorf("a round ran before the fixer (%d rounds, %d before; fix state %+v, %v)", n, rounds, st, ok)
		}
	}
}

// An autofix the floor held starts no more once the PR was switched out of
// autofix (`magnum fix --off`) meanwhile: the held start runs autofix's own
// checks again and drops it, with a fix.dropped event and no toast.
func TestAHeldAutofixIsDroppedOnceFixIsSwitchedOff(t *testing.T) {
	h, git := autofixHarness(t, "zhuravel", findings(map[string]int{"P2": 2}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.lowBudget("zhuravel", 300, h.clock.Now().Add(30*time.Minute))
	close(h.rd.gate)
	h.settle()
	h.rd.mu.Lock()
	h.rd.gate = nil
	h.rd.mu.Unlock()
	pr := h.wantState(2, store.PRReviewed)
	if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); !ok {
		t.Fatal("the autofix was not held")
	}
	off := h.fix(FixPayload{Off: true})
	h.handle()
	h.wantDone(off)

	h.advance(31 * time.Minute)
	h.tick()
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Fatalf("fix runs = %d: the held autofix started after magnum fix --off", n)
	}
	if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); ok {
		t.Error("the held autofix stays")
	}
	if ev, ok := h.fixEvent("fix.dropped"); !ok || !strings.Contains(ev.Message, "fix is off for the PR") {
		t.Errorf("fix.dropped = %+v, %v", ev, ok)
	}
	h.flush()
	if got := h.nh.titles("fix dropped"); len(got) != 0 {
		t.Errorf("toasts = %v: nobody asked for the autofix", got)
	}
}

// A paused round's continuation finishes the judge turn of a round that ran:
// below the floor it continues all the same, while a new PR waits for the
// budget.
func TestAPausedRoundContinuesBelowTheFloor(t *testing.T) {
	h := newHarness(t)
	resetAt := h.clock.Now().Add(2 * time.Hour)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.Kind == pipeline.KindContinue || in.PR.Number != 2 {
			return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: max(in.Round, 1), ReviewID: 7, Event: "COMMENTED"}, nil
		}
		run, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: 1, Role: store.RoleJudge, Kind: in.Kind,
			TargetSHA: in.TargetSHA, Identity: in.PR.Identity, ReviewerLogin: "talkable[bot]", State: store.RunPending, PromptText: "judge"})
		if err != nil {
			return failRound(t, err)
		}
		if err := h.st.TransitionRun(h.ctx, run.ID, nil, store.RunFailed, func(u *store.RunUpdate) {
			u.Set("submitted_at", h.clock.Now())
			u.Set("outcome", pipeline.OutcomeUsageLimit)
			u.Set("error", "judge pane: usage limit")
		}); err != nil {
			return failRound(t, err)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeUsageLimit, Round: 1, JudgeRunID: run.ID,
			Pause: &pipeline.Pause{Kind: string(agents.HealthUsageLimit), Tool: agents.KindCodex, Until: resetAt}}, nil
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRPaused)

	until := h.lowBudget("zhuravel", 300, h.clock.Now().Add(3*time.Hour))
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick()
	h.advance(2 * time.Hour) // the Codex pause ends; the GitHub budget is still low
	h.tick()
	var continued bool
	for _, in := range h.rd.all() {
		if in.PR.Number == 2 && in.Kind == pipeline.KindContinue {
			continued = true
		}
	}
	if !continued {
		t.Fatalf("the paused round did not continue below the floor: %+v", h.rd.all())
	}
	h.wantState(3, store.PRQueued)
	h.wantGitHubWait(3, "zhuravel", 300, until)
}

// A pending whiteboard waits only for the identity its job reads as: the
// tick that finds that identity recovered makes it, though another watch's
// poll identity is still low.
func TestAPendingWhiteboardWaitsOnlyForTheIdentityItReadsAs(t *testing.T) {
	w := wbHarness(t, true, func(h *harness) {
		h.cfg.Watches[1].PollIdentity = "talkable-app" // the other watch polls as another identity
		h.d.GitHub = func(id string) GitHub {
			if id == "zhuravel" || id == "talkable-app" {
				return h.gh
			}
			return nil
		}
	})
	wbWriter(w.harness, wbDone(wbSession))
	w.open(prSpec{n: 1, head: "base1"})
	w.startup()
	w.tick()
	w.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead})
	w.tick()
	w.advance(5 * time.Minute)
	w.rd.mu.Lock()
	w.rd.gate = make(chan struct{})
	w.rd.mu.Unlock()
	before := len(w.rd.all())
	if err := w.e.Tick(w.ctx); err != nil {
		t.Fatal(err)
	}
	w.awaitRound(before)
	w.lowBudget("zhuravel", 300, w.clock.Now().Add(30*time.Minute))
	w.lowBudget("talkable-app", 100, w.clock.Now().Add(3*time.Hour))
	close(w.rd.gate)
	w.settle()
	w.rd.mu.Lock()
	w.rd.gate = nil
	w.rd.mu.Unlock()
	pr := w.wantState(2, store.PRReviewed)
	if p, ok := w.wbPending(pr.ID); !ok || p.Head != fixHead {
		t.Fatalf("pending whiteboard = %+v, %v", p, ok)
	}

	w.advance(31 * time.Minute) // zhuravel's reset; talkable-app's budget stays low
	w.tick()
	w.settle()
	if runs := w.wbRuns(pr.ID); len(runs) != 1 || runs[0].TargetSHA != fixHead {
		t.Fatalf("whiteboard runs once zhuravel recovered = %+v, want one for %s", runs, fixHead)
	}
	if p, ok := w.wbPending(pr.ID); ok {
		t.Errorf("the pending whiteboard stays: %+v", p)
	}
}

// A held fix whose start finds GitHub refusing its read for the rate limit
// (the meter thought the budget recovered: its reset passed, but other
// tools on the login spent the new window) stays held for the budget, not
// dropped, and starts once the read goes through.
func TestAHeldFixWhoseReadHitsTheRateLimitStaysHeld(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{})
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {
		extThread(4322458360, "lib/sandbox.rb", 185, h.clock.Now().Add(-time.Hour), "MINOR: `run` drops stderr", "rev-ann"),
	}}
	h.gh.mu.Unlock()
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	h.lowBudget("zhuravel", 200, h.clock.Now().Add(30*time.Minute))
	id := h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(id); !strings.HasPrefix(res, "held; ") {
		t.Fatalf("magnum fix below the floor = %q", res)
	}
	h.gh.mu.Lock()
	h.gh.threadsErr = &github.APIError{Op: "review threads talkable/talkable#2", Status: 403, Message: "API rate limit exceeded for user ID 1"}
	h.gh.mu.Unlock()
	h.advance(31 * time.Minute) // past the reset the meter knew
	h.tick()
	if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); !ok {
		t.Fatal("the held fix was dropped when its read hit the rate limit")
	}
	if ev, ok := h.fixEvent("fix.dropped"); ok {
		t.Fatalf("fix.dropped recorded: %s", ev.Message)
	}
	h.gh.mu.Lock()
	h.gh.threadsErr = nil
	h.gh.mu.Unlock()
	h.tick()
	if runs := h.fixRuns(pr.ID); len(runs) != 1 {
		t.Fatalf("fix runs once the read goes through = %+v, want the fixer's", runs)
	}
}
