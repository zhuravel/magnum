package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// postedJudgeRun records the judge's run of round as the pipeline leaves a
// posted review on target, its prompt submitted at promptAt (the fake
// pipeline records no judge run). A round script calls it too, so it
// returns its error instead of failing the test.
func postedJudgeRun(h *harness, prID int64, round int, target string, promptAt time.Time, reviewID int64) error {
	run, err := h.st.CreateRun(h.ctx, store.Run{PRID: prID, Round: round, Role: "codex-judge", Kind: store.RunRereview, TargetSHA: target,
		Identity: "talkable-app", ReviewerLogin: magnumApp, State: store.RunPending, PromptText: "judge", CreatedAt: promptAt.Add(-time.Second)})
	if err != nil {
		return err
	}
	return h.st.TransitionRun(h.ctx, run.ID, nil, store.RunVerified, func(u *store.RunUpdate) {
		u.Set("submitted_at", promptAt)
		u.Set("outcome", pipeline.OutcomePosted)
		u.Set("review_id", reviewID)
	})
}

// ciReviewedPR is a PR #2 magnum reviewed at b1, its judge prompted a
// minute before the review was recorded, with rounds that post as the
// pipeline does (their judge run prompted when the round runs).
func ciReviewedPR(h *harness) store.PR {
	h.t.Helper()
	pr := h.reviewedPR(2, "b1")
	if err := postedJudgeRun(h, pr.ID, 1, "b1", h.clock.Now().Add(-time.Minute), deref(pr.LastReviewID)); err != nil {
		h.t.Fatal(err)
	}
	h.rd.mu.Lock()
	defer h.rd.mu.Unlock()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		res, err := h.rd.posted(h.ctx, in, 99)
		if err == nil {
			if err := postedJudgeRun(h, in.PR.ID, res.Round, in.TargetSHA, h.clock.Now(), res.ReviewID); err != nil {
				return failRound(h.t, err)
			}
		}
		return res, err
	}
	return pr
}

// redCI makes the fake GitHub report #2's head red: the checks given, each
// failed a minute ago, beside a passing lint check.
func redCI(h *harness, failed ...github.Check) []github.Check {
	h.t.Helper()
	at := h.clock.Now().Add(-time.Minute).UTC()
	checks := []github.Check{{Name: "lint", State: github.CheckPassed, Workflow: "CI", At: at}}
	for _, c := range failed {
		c.State, c.At = github.CheckFailed, at
		checks = append(checks, c)
	}
	h.setCI(2, "FAILURE", checks...)
	return checks
}

// ciFollowupOfPR reads #2's CI follow-up record (ok false when none).
func ciFollowupOfPR(t *testing.T, h *harness) (ciFollowup, bool) {
	t.Helper()
	v, ok := kvValue(h, kvPRCIFollowup(h.pr(2).ID))
	if !ok {
		return ciFollowup{}, false
	}
	var rec ciFollowup
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		t.Fatalf("ci follow-up %q: %v", v, err)
	}
	return rec, true
}

var rspec = github.Check{Name: "rspec", Workflow: "RSpec"}

// The live case (talkable#11483, 2026-10-13): magnum's re-review of
// e2777d0 posted at 08:11 said "CI is red: Dependency Audit failed,
// unrelated"; the RSpec run on that head started at 08:12 and finished red
// at 08:31 with two failures the PR causes, and nothing in magnum looked at
// the head's CI again: a colleague's review reported both an hour later. A
// check of the reviewed head that fails after the judge of that review was
// prompted now queues a judge check of the same head (rereview_pending,
// pr.ci_red, the follow-up record), which dispatch starts at once as the
// judge alone, flagged CIRed for its prompt, uncounted; once it posted, the
// same failure starts nothing more.
func TestCIThatTurnsRedAfterTheReviewStartsAJudgeCheck(t *testing.T) {
	h := newHarness(t)
	reviewed := ciReviewedPR(h)
	h.advance(10 * time.Minute)
	checks := redCI(h, rspec)
	h.tick()

	queued := approvalEvents(t, h, 2, "pr.rereview_pending")
	if len(queued) == 0 || queued[len(queued)-1].Message != "reviewed → rereview_pending (CI red after the review)" {
		t.Fatalf("queue events: %+v", queued)
	}
	red := approvalEvents(t, h, 2, "pr.ci_red")
	want := "CI finished red on b1 after the review (1 failed check, at " + checks[1].At.Local().Format("15:04") + "); a judge check follows"
	if len(red) != 1 || red[0].Level != "info" || red[0].Message != want {
		t.Fatalf("pr.ci_red events: %+v, want %q", red, want)
	}
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the judge check", len(ins))
	}
	in := ins[1]
	if in.Kind != pipeline.KindRereview || !in.SameHead || !in.CIRed || in.Replies != 0 || in.TargetSHA != "b1" ||
		!slices.Equal(roleNames(in.Roles), []string{"codex-judge"}) {
		t.Fatalf("judge check input: kind %s same head %v CI red %v replies %d target %s roles %v",
			in.Kind, in.SameHead, in.CIRed, in.Replies, in.TargetSHA, roleNames(in.Roles))
	}
	starts := approvalEvents(t, h, 2, "engine.round_start")
	if got := starts[len(starts)-1]; got.Message != "rereview round (CI red after the review) in review1 at b1: codex-judge" ||
		!strings.Contains(string(got.Data), `"ci_red":true`) {
		t.Fatalf("round start = %q %s", got.Message, got.Data)
	}
	rec, ok := ciFollowupOfPR(t, h)
	if !ok || rec.Head != "b1" || len(rec.Checks) != 1 || rec.Checks[0] != (ciFailure{Workflow: "RSpec", Name: "rspec", At: checks[1].At}) ||
		!rec.Queued.IsZero() || rec.Started.IsZero() {
		t.Fatalf("follow-up record = %+v (%v)", rec, ok)
	}
	pr := h.wantState(2, store.PRReviewed)
	if pr.RoundsToday != reviewed.RoundsToday {
		t.Fatalf("rounds today = %d, want %d: the judge check counts against no cap", pr.RoundsToday, reviewed.RoundsToday)
	}
	for _, ev := range subjectEvents(t, h, "pr:talkable/talkable#2") {
		if strings.Contains(ev.Message+string(ev.Data), "rspec") || strings.Contains(ev.Message+string(ev.Data), "RSpec") {
			t.Errorf("event %s names a check: %s %s", ev.Kind, ev.Message, ev.Data)
		}
	}

	h.advance(time.Minute)
	h.tick()
	h.advance(time.Hour)
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d: the failure the judge check covered started another", n)
	}
	if n := len(approvalEvents(t, h, 2, "pr.ci_red")); n != 1 {
		t.Fatalf("pr.ci_red events = %d", n)
	}
}

// A rerun of a check the judge check covered that fails the same way is
// covered by the review that stands: one judge check per failing check and
// head. A check that had not failed yet fails later: it gets its own.
func TestARerunThatFailsAgainStartsNoJudgeCheckButAnotherCheckDoes(t *testing.T) {
	h := newHarness(t)
	ciReviewedPR(h)
	h.advance(10 * time.Minute)
	redCI(h, rspec)
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the judge check", n)
	}

	h.advance(10 * time.Minute)
	h.setCI(2, "PENDING", github.Check{Name: "rspec", Workflow: "RSpec", State: github.CheckPending, At: h.clock.Now().UTC()})
	h.tick()
	h.advance(10 * time.Minute)
	redCI(h, rspec)
	h.tick()
	h.wantState(2, store.PRReviewed)
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d: a rerun that failed the same way started a judge check", n)
	}

	// The rollup has to change for the poll to read the checks again
	// (fetchDetails): jest runs, then fails.
	h.advance(10 * time.Minute)
	h.setCI(2, "PENDING", github.Check{Name: "jest", Workflow: "CI", State: github.CheckPending, At: h.clock.Now().UTC()})
	h.tick()
	h.advance(10 * time.Minute)
	redCI(h, rspec, github.Check{Name: "jest", Workflow: "CI"})
	h.tick()
	ins := h.rd.all()
	if len(ins) != 3 || !ins[2].CIRed || !ins[2].SameHead {
		t.Fatalf("rounds = %d, want a judge check of the new failure", len(ins))
	}
	if n := len(approvalEvents(t, h, 2, "pr.ci_red")); n != 2 {
		t.Fatalf("pr.ci_red events = %d, want 2", n)
	}
	if rec, _ := ciFollowupOfPR(t, h); len(rec.Checks) != 2 {
		t.Fatalf("follow-up record = %+v, want both checks", rec)
	}
}

// A check that failed before the judge of the review was prompted was in
// its failing-checks.json: the review's Checks already answer it, so it
// starts nothing (TestCIThatFailedBeforeTheJudgePromptIsFetchedOnceAndQueuesNothing
// checks the poll's side), and a check without a time cannot be placed.
func TestCIRedBeforeThePromptOrWithoutATimeStartsNoJudgeCheck(t *testing.T) {
	h := newHarness(t)
	reviewed := ciReviewedPR(h)
	h.setCI(2, "FAILURE", github.Check{Name: "rspec", Workflow: "RSpec", State: github.CheckFailed, At: reviewed.ReviewedAt.Add(-2 * time.Minute).UTC()},
		github.Check{Name: "status", State: github.CheckFailed})
	for range 3 {
		h.advance(time.Minute)
		h.tick()
	}
	h.wantState(2, store.PRReviewed)
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d", n)
	}
	if _, ok := ciFollowupOfPR(t, h); ok {
		t.Fatal("a follow-up record for checks the judge saw")
	}
}

// A judge check that waits (here for a snooze) is dropped when CI no longer
// shows its checks failed: a rerun in progress turns them pending, a rerun
// that passes clears them. The PR goes back to reviewed with
// pr.ci_red_dropped, and a rerun that fails again queues it again, since
// none ran.
func TestAWaitingJudgeCheckDropsWhenItsChecksNoLongerFail(t *testing.T) {
	h := newHarness(t)
	ciReviewedPR(h)
	until := h.clock.Now().Add(3 * time.Hour)
	snooze(h, 2, until)
	h.advance(10 * time.Minute)
	redCI(h, rspec)
	h.tick()
	h.wantState(2, store.PRRereviewPending)
	w := waitOf(t, h, 2)
	if w.Reason != WaitSnoozed || !w.CIRed || w.Short(h.clock.Now()) != "CI check · snoozed → "+waitClock(until, h.clock.Now()) {
		t.Fatalf("wait = %+v (%q)", w, w.Short(h.clock.Now()))
	}
	// Held by nothing (the snooze set aside), it says what comes.
	snoozed := mustKV(t, h, KVPRSnooze(h.pr(2).ID))
	h.e.delKV(h.ctx, KVPRSnooze(h.pr(2).ID))
	w = h.e.waitFor(h.ctx, h.pr(2), nil, h.clock.Now())
	if w.Reason != WaitCIRed || w.Short(h.clock.Now()) != "CI check · CI red" ||
		w.Sentence("talkable#2", h.clock.Now()) != "CI check starts at the next dispatch: CI turned red after the review; judge check pending" {
		t.Fatalf("wait held by nothing = %+v (%q, %q)", w, w.Short(h.clock.Now()), w.Sentence("talkable#2", h.clock.Now()))
	}
	h.e.setKV(h.ctx, KVPRSnooze(h.pr(2).ID), snoozed)

	h.advance(time.Minute)
	h.setCI(2, "PENDING", github.Check{Name: "rspec", Workflow: "RSpec", State: github.CheckPending, At: h.clock.Now().UTC()})
	h.tick()
	h.wantState(2, store.PRReviewed)
	dropped := approvalEvents(t, h, 2, "pr.ci_red_dropped")
	if len(dropped) != 1 || dropped[0].Message != "rereview_pending → reviewed: CI no longer shows the checks that queued the judge check of b1 failed (a rerun runs or passed); the review stands" {
		t.Fatalf("pr.ci_red_dropped events: %+v", dropped)
	}
	if _, ok := ciFollowupOfPR(t, h); ok {
		t.Fatal("the dropped follow-up is still recorded")
	}

	h.advance(10 * time.Minute)
	redCI(h, rspec)
	h.tick()
	h.wantState(2, store.PRRereviewPending)
	if n := len(approvalEvents(t, h, 2, "pr.ci_red")); n != 2 {
		t.Fatalf("pr.ci_red events = %d: the rerun's failure did not queue the check again", n)
	}
	h.advance(time.Minute)
	h.setCI(2, "SUCCESS", github.Check{Name: "rspec", Workflow: "RSpec", State: github.CheckPassed, At: h.clock.Now().UTC()})
	h.tick()
	h.wantState(2, store.PRReviewed)
	if n := len(approvalEvents(t, h, 2, "pr.ci_red_dropped")); n != 2 {
		t.Fatalf("pr.ci_red_dropped events = %d", n)
	}
	unsnooze(h, 2)
	h.advance(time.Hour)
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want none after the drops", n)
	}
}

// A push while the judge check waits supersedes it: the push's re-review
// runs, no CI follow-up, and pr.ci_red_dropped says why.
func TestAPushSupersedesAWaitingJudgeCheck(t *testing.T) {
	h := newHarness(t)
	ciReviewedPR(h)
	snooze(h, 2, h.clock.Now().Add(3*time.Hour))
	h.advance(10 * time.Minute)
	redCI(h, rspec)
	h.tick()
	h.wantState(2, store.PRRereviewPending)

	pollPR(h, time.Minute, 2, "b2")
	dropped := approvalEvents(t, h, 2, "pr.ci_red_dropped")
	if len(dropped) != 1 || dropped[0].Message != "the push of b2 supersedes the judge check of b1's red CI" {
		t.Fatalf("pr.ci_red_dropped events: %+v", dropped)
	}
	if _, ok := ciFollowupOfPR(t, h); ok {
		t.Fatal("the superseded follow-up is still recorded")
	}
	unsnooze(h, 2)
	h.advance(time.Hour)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].TargetSHA != "b2" || ins[1].CIRed || ins[1].SameHead {
		t.Fatalf("rounds = %d, want the push's re-review (%+v)", len(ins), ins[len(ins)-1].TargetSHA)
	}
}

// Quiet hours let the judge check run, and so do the daily cap and the
// re-review interval: no code changed, and it counts against no cap.
func TestTheJudgeCheckRunsInQuietHoursPastTheCapAndTheInterval(t *testing.T) {
	h := newHarness(t)
	reviewed := ciReviewedPR(h)
	h.cfg.Daemon.QuietHours = operatorQuietHours
	h.cfg.Daemon.MaxRoundsPerPRPerDay = reviewed.RoundsToday
	h.advance(5 * time.Minute) // well inside min_rereview_interval (30m)
	redCI(h, rspec)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || !ins[1].CIRed || ins[1].TargetSHA != "b1" {
		t.Fatalf("rounds = %d, want the judge check inside quiet hours, the cap and the interval", len(ins))
	}
	if pr := h.wantState(2, store.PRReviewed); pr.RoundsToday != reviewed.RoundsToday {
		t.Fatalf("rounds today = %d, want %d", pr.RoundsToday, reviewed.RoundsToday)
	}
}
