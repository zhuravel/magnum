package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// ---- helpers ----

// ownInterval sets [daemon] own_min_rereview_interval.
func ownInterval(d time.Duration) func(*harness) {
	return func(h *harness) { h.cfg.Daemon.OwnMinRereviewInterval = config.Duration{Duration: d} }
}

// authoredPR runs the standard flow to a reviewed PR #n at head, authored
// by author ("zhuravel" is the operator, one of the harness's own logins).
func authoredPR(h *harness, n int, head, author string) store.PR {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head, author: author})
	h.tick() // #n queued
	h.advance(5 * time.Minute)
	h.tick() // dispatched and reviewed
	return h.wantState(n, store.PRReviewed)
}

// authoredPush lets d pass and pushes head to PR #n by author (one poll).
func authoredPush(h *harness, d time.Duration, spec prSpec) {
	h.t.Helper()
	h.advance(d)
	h.open(prSpec{n: 1, head: "base1"}, spec)
	h.tick()
}

// snooze asks the daemon to snooze PR #n until until (one tick) and returns
// the request as it ended.
func snooze(h *harness, n int, until time.Time) store.Request {
	h.t.Helper()
	id := h.enqueue(ReqSnooze, SnoozePayload{PRTarget: PRTarget{Ref: itoa(int64(n))}, Until: until, By: "magnum snooze"})
	h.tick()
	return h.request(id)
}

// unsnooze asks the daemon to lift PR #n's snooze (one tick).
func unsnooze(h *harness, n int) store.Request {
	h.t.Helper()
	id := h.enqueue(ReqSnooze, SnoozePayload{PRTarget: PRTarget{Ref: itoa(int64(n))}, Off: true, By: "the board"})
	h.tick()
	return h.request(id)
}

// wantRequestDone fails unless req completed with a result containing want.
func wantRequestDone(t *testing.T, req store.Request, want string) {
	t.Helper()
	if req.State != store.RequestDone || !strings.Contains(deref(req.Result), want) {
		t.Fatalf("request %s %s: %q, want done with %q", req.Kind, req.State, deref(req.Result), want)
	}
}

// ---- own PR interval ----

// The live case: the operator develops his own PR in another session that
// pushes often, and every burst got a re-review 30 minutes after the last
// round. With own_min_rereview_interval = "2h" his PR waits 2h after its
// last round (the wait names the rule), while someone else's PR keeps the
// normal 30m.
func TestOwnPRReReviewWaitsTheOwnInterval(t *testing.T) {
	t.Run("own", func(t *testing.T) {
		h := newHarness(t, ownInterval(2*time.Hour))
		start := *authoredPR(h, 2, "b1", "zhuravel").LastRoundStartedAt
		authoredPush(h, 40*time.Minute, prSpec{n: 2, head: "b2", author: "zhuravel"})
		due := start.Add(2 * time.Hour)
		pr := h.wantState(2, store.PRRereviewPending)
		if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(due) {
			t.Fatalf("next_eligible_at = %v, want the last round + 2h (%v)", pr.NextEligibleAt, due)
		}
		w := waitOf(t, h, 2)
		if w.Reason != WaitOwnInterval || !w.Until.Equal(due) {
			t.Fatalf("wait = %+v, want the own PR interval until %v", w, due)
		}
		if got, want := w.Short(h.clock.Now()), "re-review · own PR interval → "+clockText(due); got != want {
			t.Errorf("short = %q, want %q", got, want)
		}
		if got := w.Sentence("talkable#2", h.clock.Now()); !strings.Contains(got, "waits for the own PR interval (2h since the last round) until "+clockText(due)) ||
			!strings.HasSuffix(got, "; `magnum review talkable#2` runs it now") {
			t.Errorf("sentence = %q", got)
		}
		h.advance(due.Sub(h.clock.Now()) - time.Minute)
		h.tick()
		reqWantRounds(t, h, 1)
		h.advance(time.Minute)
		h.tick()
		reqWantRounds(t, h, 2)
		h.wantState(2, store.PRReviewed)
	})
	t.Run("someone else's", func(t *testing.T) {
		h := newHarness(t, ownInterval(2*time.Hour))
		authoredPR(h, 2, "b1", "alice")
		authoredPush(h, 40*time.Minute, prSpec{n: 2, head: "b2", author: "alice"})
		if w := waitOf(t, h, 2); w.Reason != WaitQuiet {
			t.Fatalf("wait = %+v, want only the push quiet period", w)
		}
		h.advance(5 * time.Minute)
		h.tick()
		reqWantRounds(t, h, 2)
	})
}

// An own draft waits the longer of the draft (2h) and own intervals.
func TestOwnDraftWaitsTheLongerOfTheDraftAndOwnIntervals(t *testing.T) {
	h := newHarness(t, ownInterval(3*time.Hour))
	start := *authoredPR(h, 2, "b1", "zhuravel").LastRoundStartedAt
	authoredPush(h, 40*time.Minute, prSpec{n: 2, head: "b2", author: "zhuravel", draft: true})
	if w := waitOf(t, h, 2); w.Reason != WaitOwnInterval || !w.Until.Equal(start.Add(3*time.Hour)) {
		t.Fatalf("own 3h: wait = %+v, want the own interval until %v", w, start.Add(3*time.Hour))
	}

	h = newHarness(t, ownInterval(time.Hour))
	start = *authoredPR(h, 2, "b1", "zhuravel").LastRoundStartedAt
	authoredPush(h, 40*time.Minute, prSpec{n: 2, head: "b2", author: "zhuravel", draft: true})
	if w := waitOf(t, h, 2); w.Reason != WaitDraftInterval || !w.Until.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("own 1h: wait = %+v, want the draft interval until %v", w, start.Add(2*time.Hour))
	}
}

// A review request on the operator's own PR skips the own interval: the
// round runs after the request debounce.
func TestReviewRequestSkipsTheOwnInterval(t *testing.T) {
	h := newHarness(t, ownInterval(2*time.Hour))
	authoredPR(h, 2, "b1", "zhuravel")
	authoredPush(h, 10*time.Minute, prSpec{n: 2, head: "b2", author: "zhuravel"})
	h.advance(time.Minute)
	askedAt := h.clock.Now()
	reqPoll(h, prSpec{n: 2, head: "b2", author: "zhuravel", requests: []github.ReviewRequestEvent{reqEvent(askedAt, "alice", askedPoll)}})
	if pr := h.pr(2); pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(askedAt.Add(time.Minute)) {
		t.Fatalf("next_eligible_at = %v, want the request + 1m", pr.NextEligibleAt)
	}
	h.advance(time.Minute)
	h.tick()
	reqWantRounds(t, h, 2)
}

// A [[watch]] own_min_rereview_interval overrides the daemon's for its PRs.
func TestWatchOverridesTheOwnInterval(t *testing.T) {
	h := newHarness(t, ownInterval(2*time.Hour), reqWatches(func(w *config.Watch) {
		if w.Owner == "talkable" {
			w.OwnMinRereviewInterval = config.Duration{Duration: 4 * time.Hour}
		}
	}))
	start := *authoredPR(h, 2, "b1", "zhuravel").LastRoundStartedAt
	authoredPush(h, 40*time.Minute, prSpec{n: 2, head: "b2", author: "zhuravel"})
	if w := waitOf(t, h, 2); w.Reason != WaitOwnInterval || !w.Until.Equal(start.Add(4*time.Hour)) || !strings.Contains(w.Detail, "(4h since the last round)") {
		t.Fatalf("wait = %+v, want the watch's 4h", w)
	}
}

// An own interval shorter than min_rereview_interval is not held back by
// the dispatcher's backstop, which knows only the daemon's intervals.
func TestAShorterOwnIntervalIsNotHeldByTheBackstop(t *testing.T) {
	h := newHarness(t, ownInterval(10*time.Minute))
	authoredPR(h, 2, "b1", "zhuravel")                                            // round 1 at 10:05
	authoredPush(h, 10*time.Minute, prSpec{n: 2, head: "b2", author: "zhuravel"}) // 10:15
	h.advance(5 * time.Minute)                                                    // 10:20: quiet and 15m since the round
	h.tick()
	reqWantRounds(t, h, 2)
}

// ---- snooze ----

// A snooze holds a push's re-review past the quiet period and the interval
// until it ends; the wait says so, and the round runs once it ends with no
// clean-up.
func TestSnoozeHoldsAReReviewUntilItEnds(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1") // 10:05
	until := h.clock.Now().Add(3 * time.Hour)
	req := snooze(h, 2, until)
	wantRequestDone(t, req, "snoozed talkable/talkable#2 until "+clockText(until))
	evs := approvalEvents(t, h, 2, "pr.snoozed")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "by magnum snooze") {
		t.Fatalf("pr.snoozed events: %+v", evs)
	}
	s, ok := h.e.snoozeOf(h.ctx, h.pr(2).ID, h.clock.Now())
	if !ok || !s.Until.Equal(until) || s.By != "magnum snooze" || !s.At.Equal(h.clock.Now()) {
		t.Fatalf("snooze = %+v (%v)", s, ok)
	}

	pollPR(h, 40*time.Minute, 2, "b2")
	pr := h.wantState(2, store.PRRereviewPending)
	if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(until) {
		t.Fatalf("next_eligible_at = %v, want the snooze's end %v", pr.NextEligibleAt, until)
	}
	w := waitOf(t, h, 2)
	if w.Reason != WaitSnoozed || !w.Until.Equal(until) {
		t.Fatalf("wait = %+v, want the snooze until %v", w, until)
	}
	if got, want := w.Short(h.clock.Now()), "re-review · snoozed → "+clockText(until); got != want {
		t.Errorf("short = %q, want %q", got, want)
	}
	if got := w.Sentence("talkable#2", h.clock.Now()); !strings.Contains(got, "re-review waits for the end of its snooze until "+clockText(until)) ||
		!strings.Contains(got, "`magnum snooze talkable#2 --off` lifts it") || !strings.Contains(got, "`magnum review talkable#2` runs it now") {
		t.Errorf("sentence = %q", got)
	}
	h.advance(time.Hour)
	h.tick()
	reqWantRounds(t, h, 1)
	h.advance(until.Sub(h.clock.Now()))
	h.tick()
	reqWantRounds(t, h, 2)
	h.wantState(2, store.PRReviewed)
}

// A snooze holds a new PR's first review too.
func TestSnoozeHoldsAFirstReview(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick() // #2 queued for the quiet period
	until := h.clock.Now().Add(time.Hour)
	snooze(h, 2, until)
	if w := waitOf(t, h, 2); w.Reason != WaitSnoozed || w.Rereview || w.Short(h.clock.Now()) != "review · snoozed → "+clockText(until) {
		t.Fatalf("wait = %+v (%q)", w, w.Short(h.clock.Now()))
	}
	h.advance(30 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 0)
	h.advance(30 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
}

// A snooze holds a delta check and a reply round, which otherwise skip
// some of the timing rules.
func TestSnoozeHoldsADeltaCheckAndAReplyRound(t *testing.T) {
	t.Run("delta check", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		until := h.clock.Now().Add(2 * time.Hour)
		snooze(h, 2, until)
		reqSetFiles(h, "b1...b2", rubyMixed)
		h.gh.compare["b1...b2"] = github.CompareStats{Commits: 1}
		pollPR(h, 40*time.Minute, 2, "b2")
		w := waitOf(t, h, 2)
		if !w.DeltaCheck || w.Reason != WaitSnoozed || w.Short(h.clock.Now()) != "delta check · snoozed → "+clockText(until) {
			t.Fatalf("wait = %+v (%q), want the delta check held by the snooze", w, w.Short(h.clock.Now()))
		}
		h.advance(30 * time.Minute)
		h.tick()
		reqWantRounds(t, h, 1)
		h.advance(until.Sub(h.clock.Now()))
		h.tick()
		reqWantRounds(t, h, 2)
	})
	t.Run("reply round", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		until := h.clock.Now().Add(2 * time.Hour)
		snooze(h, 2, until)
		h.advance(10 * time.Minute)
		reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{threadReply(h, "alice")}})
		pr := h.wantState(2, store.PRRereviewPending)
		if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(until) {
			t.Fatalf("next_eligible_at = %v, want the snooze's end %v", pr.NextEligibleAt, until)
		}
		if w := waitOf(t, h, 2); w.Reason != WaitSnoozed || w.Replies != 1 || w.Short(h.clock.Now()) != "re-decision · snoozed → "+clockText(until) {
			t.Fatalf("wait = %+v (%q)", w, w.Short(h.clock.Now()))
		}
		repliedRounds(h)
		h.advance(10 * time.Minute) // past the reply debounce
		h.tick()
		reqWantRounds(t, h, 1)
		h.advance(until.Sub(h.clock.Now()))
		h.tick()
		if ins := h.rd.all(); len(ins) != 2 || ins[1].Replies != 1 {
			t.Fatalf("rounds = %d, want the reply round after the snooze", len(ins))
		}
	})
}

// A review request and `magnum review` pass a snooze, which stays for the
// automatic rounds after them.
func TestSnoozeLetsRequestsAndForcedReviewsThrough(t *testing.T) {
	t.Run("review request", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		snooze(h, 2, h.clock.Now().Add(3*time.Hour))
		h.advance(time.Minute)
		askedAt := h.clock.Now()
		reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqEvent(askedAt, "alice", askedPoll)}})
		if pr := h.pr(2); pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(askedAt.Add(time.Minute)) {
			t.Fatalf("next_eligible_at = %v, want the request debounce", pr.NextEligibleAt)
		}
		h.advance(time.Minute)
		h.tick()
		reqWantRounds(t, h, 2)
	})
	t.Run("magnum review", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		until := h.clock.Now().Add(3 * time.Hour)
		snooze(h, 2, until)
		h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
		h.tick()
		reqWantRounds(t, h, 2)
		if _, ok := h.e.snoozeOf(h.ctx, h.pr(2).ID, h.clock.Now()); !ok {
			t.Fatal("the forced round lifted the snooze")
		}
		pollPR(h, 40*time.Minute, 2, "b2")
		if w := waitOf(t, h, 2); w.Reason != WaitSnoozed || !w.Until.Equal(until) {
			t.Fatalf("after the forced round a push waits %+v, want the snooze", w)
		}
	})
}

// A snooze is in the registry: a restarted daemon keeps holding the PR.
func TestSnoozeSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	until := h.clock.Now().Add(2 * time.Hour)
	snooze(h, 2, until)
	h.e = New(h.d) // the daemon restarts on the same registry
	h.startup()
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	if w := waitOf(t, h, 2); w.Reason != WaitSnoozed {
		t.Fatalf("wait = %+v, want the snooze", w)
	}
}

// --off lifts the snooze: the waiting PR gets its time again and runs
// after the rules that hold it without the snooze.
func TestSnoozeOffLiftsIt(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	snooze(h, 2, h.clock.Now().Add(3*time.Hour))
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)

	wantRequestDone(t, unsnooze(h, 2), "lifted the snooze of talkable/talkable#2")
	reqWantRounds(t, h, 2)
	if _, ok := kvValue(h, KVPRSnooze(h.pr(2).ID)); ok {
		t.Fatal("the snooze is still recorded")
	}
	evs := approvalEvents(t, h, 2, "pr.unsnoozed")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "by the board") {
		t.Fatalf("pr.unsnoozed events: %+v", evs)
	}
	wantRequestDone(t, unsnooze(h, 2), "talkable/talkable#2 is not snoozed")
}

// The dispatcher checks the snooze itself: a waiting PR whose time was set
// before the snooze (a round's settle, a retry's backoff) starts nothing.
func TestSnoozeHoldsAPRWhoseTimeWasSetBeforeIt(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	pollPR(h, 40*time.Minute, 2, "b2")
	until := h.clock.Now().Add(time.Hour)
	b, err := (Snooze{Until: until, At: h.clock.Now(), By: "magnum snooze"}).marshal()
	if err != nil {
		t.Fatal(err)
	}
	h.e.setKV(h.ctx, KVPRSnooze(h.pr(2).ID), b) // next_eligible_at stays the quiet period's
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	h.advance(time.Hour)
	h.tick()
	reqWantRounds(t, h, 2)
}

// A paused round whose retry time passed waits for the snooze too: its
// continuation is an automatic round.
func TestSnoozeHoldsAPausedRound(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	pollPR(h, 40*time.Minute, 2, "b2")
	if err := h.st.TransitionPR(h.ctx, h.pr(2).ID, nil, store.PRPaused, func(u *store.PRUpdate) {
		u.Set("next_attempt_at", h.clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	snooze(h, 2, h.clock.Now().Add(time.Hour))
	reqWantRounds(t, h, 1)
	h.wantState(2, store.PRPaused)
	h.advance(time.Hour)
	h.tick()
	reqWantRounds(t, h, 2)
}

// Only an open PR is snoozed, and only until a time ahead.
func TestSnoozeRefusesAClosedPRAndATimePassed(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	if req := snooze(h, 2, h.clock.Now().Add(-time.Minute)); req.State != store.RequestDone || !strings.Contains(deref(req.Result), "nothing to do") {
		t.Fatalf("a snooze ending in the past: %s %q", req.State, deref(req.Result))
	}
	if _, ok := h.e.snoozeOf(h.ctx, h.pr(2).ID, h.clock.Now()); ok {
		t.Fatal("a snooze ending in the past was recorded")
	}
	if err := h.st.UpdatePR(h.ctx, h.pr(2).ID, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) }); err != nil {
		t.Fatal(err)
	}
	if req := snooze(h, 2, h.clock.Now().Add(time.Hour)); req.State != store.RequestFailed || !strings.Contains(deref(req.Result), "nothing to snooze") {
		t.Fatalf("a merged PR: %s %q", req.State, deref(req.Result))
	}
}

// The new rules map onto their waits.
func TestThrottleWaitOfTheOwnIntervalAndTheSnooze(t *testing.T) {
	h := newHarness(t)
	pr := pushedAfter(t, h, 40*time.Minute)
	w := *h.cfg.WatchFor("talkable/talkable")
	until := h.clock.Now().Add(time.Hour)
	for rule, want := range map[eligibility.Rule]string{
		eligibility.RuleOwnInterval: WaitOwnInterval,
		eligibility.RuleSnoozed:     WaitSnoozed,
	} {
		td := eligibility.ThrottleDecision{Rule: rule, Reason: "a rule worded anew", NextEligibleAt: until}
		if got := h.e.throttleWait(h.ctx, pr, w, eligibility.PRFacts{}, td, h.clock.Now()); got.Reason != want || !got.Until.Equal(until) {
			t.Errorf("rule %q: wait %+v, want %s", rule, got, want)
		}
	}
}
