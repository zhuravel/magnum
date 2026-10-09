package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// fixPollEventCount counts the events of kind on subject.
func fixPollEventCount(t *testing.T, h *harness, subject, kind string) int {
	t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, subject, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// A draft that becomes ready for review is a review request: its re-review
// waits only for request_debounce, not for the re-review interval, and does
// not count against the daily cap.
func TestDraftReadyRunsAsRequested(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	first := h.pr(2)
	if first.LastRoundStartedAt == nil {
		t.Fatal("reviewed PR has no last_round_started_at")
	}
	lastRound := *first.LastRoundStartedAt

	// Pushed as a draft: the draft interval (2h) wins over the quiet period.
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", draft: true})
	h.tick()
	pr := h.wantState(2, store.PRRereviewPending)
	if !pr.IsDraft || pr.HeadSHA != "b2" {
		t.Fatalf("PR after the draft push: draft=%v head=%s", pr.IsDraft, pr.HeadSHA)
	}
	if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(lastRound.Add(2*time.Hour)) {
		t.Fatalf("next_eligible_at = %v, want last_round_started_at+2h = %v", pr.NextEligibleAt, lastRound.Add(2*time.Hour))
	}
	if pr.PendingSince == nil {
		t.Fatal("pending_since not set by the push")
	}
	pending := *pr.PendingSince

	// Ready for review: same head, a new updated timestamp.
	h.advance(time.Minute)
	ready := h.clock.Now()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	pr = h.wantState(2, store.PRRereviewPending)
	if pr.IsDraft {
		t.Fatal("PR still a draft")
	}
	if pr.PendingSince == nil || !pr.PendingSince.Equal(pending) {
		t.Fatalf("pending_since = %v, want %v (unchanged by the draft toggle)", pr.PendingSince, pending)
	}
	want := ready.Add(time.Minute) // request_debounce after the transition
	if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(want) {
		t.Fatalf("next_eligible_at = %v, want the transition + 1m = %v", pr.NextEligibleAt, want)
	}
	if w := waitOf(t, h, 2); w.Reason != WaitRequested || w.Short(h.clock.Now()) != "re-review · ready for review → "+want.Format("15:04") {
		t.Fatalf("wait = %+v (%q)", w, w.Short(h.clock.Now()))
	}
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d before the debounce ended, want 1", n)
	}

	// Due after the debounce, long before the interval: the second round
	// reviews b2 and is not counted against the cap.
	h.advance(want.Sub(h.clock.Now()))
	h.tick()
	pr = h.wantState(2, store.PRReviewed)
	if deref(pr.ReviewedSHA) != "b2" {
		t.Fatalf("reviewed_sha = %q, want b2", deref(pr.ReviewedSHA))
	}
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want 2", n)
	}
	if pr.RoundsToday != first.RoundsToday {
		t.Fatalf("rounds_today = %d, want %d (a requested round does not count)", pr.RoundsToday, first.RoundsToday)
	}
}

// The gap between the two CLOSED/MERGED answers is counted from the first
// successful one: a confirmation outage neither starts the clock nor shortens
// the gap, and a repeated outage is reported once.
func TestCloseConfirmationGapFromFirstSuccess(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.open(prSpec{n: 1, head: "base1"}) // #2 left the radar
	h.gh.closed[2] = "MERGED"
	h.gh.fail(nil, errors.New("confirm down"))

	for i := 0; i < 4; i++ {
		h.advance(time.Minute)
		h.tick()
		pr := h.wantState(2, store.PRReviewed)
		if pr.MissingSince != nil || pr.ConfirmCount != 0 {
			t.Fatalf("tick %d: missing_since=%v confirm_count=%d during the outage", i, pr.MissingSince, pr.ConfirmCount)
		}
	}
	if n := fixPollEventCount(t, h, "repo:talkable/talkable", "poll.confirm_error"); n != 1 {
		t.Fatalf("poll.confirm_error events = %d, want 1 (repeats are deduplicated)", n)
	}
	if h.gh.count("confirm:talkable/talkable") < 4 {
		t.Fatalf("confirm calls = %d, want one per tick", h.gh.count("confirm:talkable/talkable"))
	}

	// The outage ends: the first success starts the clock.
	h.gh.fail(nil, nil)
	h.advance(time.Minute)
	h.tick()
	firstAt := h.clock.Now()
	pr := h.wantState(2, store.PRReviewed)
	if pr.ConfirmCount != 1 || pr.MissingSince == nil || !pr.MissingSince.Equal(firstAt) {
		t.Fatalf("after the first answer: confirm_count=%d missing_since=%v, want 1 and %v", pr.ConfirmCount, pr.MissingSince, firstAt)
	}

	// A second answer only 10s later is too soon.
	h.advance(10 * time.Second)
	h.tick()
	pr = h.wantState(2, store.PRReviewed)
	if pr.MissingSince == nil || !pr.MissingSince.Equal(firstAt) {
		t.Fatalf("missing_since = %v, want %v (still the first answer)", pr.MissingSince, firstAt)
	}

	// 60s after the first answer it counts as closed.
	h.advance(60 * time.Second)
	h.tick()
	pr = h.wantState(2, store.PRClosed)
	if pr.GHState != store.GHMerged {
		t.Fatalf("gh_state = %s, want %s", pr.GHState, store.GHMerged)
	}
}

// A PR whose Details are unknown cannot be filtered, so it is never
// dispatched; once the Details arrive the watch's filters apply.
func TestDetailsFailureKeepsUnknownPRsFromDispatch(t *testing.T) {
	h := newHarness(t)
	h.gh.fail(errors.New("details down"), nil)
	h.open(prSpec{n: 1, head: "a1", author: "dependabot"})
	h.startup()
	h.tick()
	h.tick()

	pr := h.wantState(1, store.PRBaseline)
	if pr.DetailsAt != nil || pr.AuthorLogin != nil {
		t.Fatalf("PR recorded details despite the failure: details_at=%v author=%v", pr.DetailsAt, pr.AuthorLogin)
	}
	if n := fixPollEventCount(t, h, "repo:talkable/talkable", "poll.details_error"); n != 1 {
		t.Fatalf("poll.details_error events = %d, want 1", n)
	}

	// A push of a PR we know nothing about: queued, but held back.
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "a2", author: "dependabot"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	pr = h.pr(1)
	if len(h.rd.all()) != 0 {
		t.Fatalf("a round ran for a PR without details: %d", len(h.rd.all()))
	}
	if gate, _ := h.e.getKV(h.ctx, store.KVPRGate(pr.ID)); gate != "waiting for the PR's details from GitHub" {
		t.Fatalf("gate = %q (state %s)", gate, pr.State)
	}

	// The Details arrive: the author is on skip_authors.
	h.gh.fail(nil, nil)
	h.advance(time.Minute)
	h.tick()
	pr = h.wantState(1, store.PRIneligible)
	if !strings.Contains(deref(pr.SkipReason), "skip_authors") {
		t.Fatalf("skip_reason = %q, want the skip_authors rule", deref(pr.SkipReason))
	}
	if pr.DetailsAt == nil || deref(pr.AuthorLogin) != "dependabot" {
		t.Fatalf("details not stored: details_at=%v author=%q", pr.DetailsAt, deref(pr.AuthorLogin))
	}
	h.advance(10 * time.Minute)
	h.tick()
	if len(h.rd.all()) != 0 {
		t.Fatalf("a round ran for a skipped author: %d", len(h.rd.all()))
	}
	h.wantState(1, store.PRIneligible)
}

// The watch config can change while a PR waits: dispatch applies the current
// filters, not the ones in force when the poller queued the PR.
func TestDispatchAppliesTheCurrentWatchFilters(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	pr := h.wantState(2, store.PRQueued)
	if deref(pr.AuthorLogin) != "alice" {
		t.Fatalf("author = %q, want alice", deref(pr.AuthorLogin))
	}

	// alice is now skipped (config edited in place, as a reload would).
	if h.cfg.Watches[0].Owner != "talkable" {
		t.Fatalf("watch 0 = %q, want talkable", h.cfg.Watches[0].Owner)
	}
	h.cfg.Watches[0].SkipAuthors = append(h.cfg.Watches[0].SkipAuthors, "alice")

	h.advance(5 * time.Minute)
	h.tick()
	pr = h.wantState(2, store.PRIneligible)
	if !strings.Contains(deref(pr.SkipReason), "skip_authors") {
		t.Fatalf("skip_reason = %q, want the skip_authors rule", deref(pr.SkipReason))
	}
	if len(h.rd.all()) != 0 {
		t.Fatalf("a round ran for a skipped author: %d", len(h.rd.all()))
	}
	if !slices.ContainsFunc(fixPollEvents(t, h, "pr:talkable/talkable#2"), func(ev store.Event) bool { return ev.Kind == "pr.ineligible" }) {
		t.Fatal("no pr.ineligible event")
	}
}

func fixPollEvents(t *testing.T, h *harness, subject string) []store.Event {
	t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, subject, 0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// A review request names a PR of a repository no [[watch]] covers any more:
// refused, and the PR is left alone.
func TestReviewRequestRefusesAnUnwatchedRepository(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // #1 baseline
	h.wantState(1, store.PRBaseline)

	h.cfg.Watches = h.cfg.Watches[1:] // the talkable watch is gone
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 1}})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "is not watched") {
		t.Fatalf("review request: state=%s result=%q", r.State, deref(r.Result))
	}
	pr := h.wantState(1, store.PRBaseline)
	if pr.Forced {
		t.Fatal("PR forced by a refused request")
	}
	if len(h.rd.all()) != 0 {
		t.Fatalf("rounds = %d, want 0", len(h.rd.all()))
	}
}

// `magnum review --as X` re-checks X when it is marked unhealthy, not the
// PR's previous identity.
func TestReviewAsRechecksTheRequestedIdentity(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	if pr.Identity != "talkable-app" {
		t.Fatalf("PR identity = %q, want talkable-app", pr.Identity)
	}
	h.e.setKV(h.ctx, store.KVIdentityCheck("zhuravel"), "fail")
	h.e.setKV(h.ctx, store.KVIdentityError("zhuravel"), "token expired")
	if ok, _ := h.e.identityHealthy(h.ctx, "zhuravel"); ok {
		t.Fatal("zhuravel should be unhealthy before the request")
	}
	src := h.ids["zhuravel"]
	src.mu.Lock()
	before := src.checks
	src.mu.Unlock()

	if _, err := h.e.requestReview(h.ctx, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, As: "zhuravel"}); err != nil {
		t.Fatalf("requestReview: %v", err)
	}
	src.mu.Lock()
	after := src.checks
	src.mu.Unlock()
	if after <= before {
		t.Fatalf("zhuravel checks = %d, want more than %d (the requested identity is re-checked)", after, before)
	}
	if got := h.pr(2).Identity; got != "zhuravel" {
		t.Fatalf("PR identity = %q, want zhuravel", got)
	}
	if ok, why := h.e.identityHealthy(h.ctx, "zhuravel"); !ok {
		t.Fatalf("zhuravel still unhealthy: %s", why)
	}
}

// `magnum resume --watch X` needs a watch X or a recorded pause for it (a
// leak pause outlives a removed [[watch]]).
func TestResumeWatchRequiresAKnownWatch(t *testing.T) {
	h := newHarness(t)
	if res, err := h.e.requestPause(h.ctx, PausePayload{Watch: "example"}, false); err == nil {
		t.Fatalf("resume of an unknown watch succeeded: %q", res)
	}
	res, err := h.e.requestPause(h.ctx, PausePayload{Watch: "talkable"}, false)
	if err != nil || res != "resumed watch talkable" {
		t.Fatalf("resume of a configured watch: %q, %v", res, err)
	}

	h.e.setKV(h.ctx, store.KVWatchPaused("example"), "identity leak")
	res, err = h.e.requestPause(h.ctx, PausePayload{Watch: "example"}, false)
	if err != nil || res != "resumed watch example" {
		t.Fatalf("resume of a recorded pause: %q, %v", res, err)
	}
	if v, ok := h.e.getKV(h.ctx, store.KVWatchPaused("example")); ok {
		t.Fatalf("pause record still there: %q", v)
	}
}

// A judge session row stuck in "starting" (a pane without an agent) is not a
// live session: `magnum open` restarts the agent instead of answering
// "nothing to restore".
func TestOpenRequestRestartsAStartingSession(t *testing.T) {
	h := newHarness(t)
	pr := h.parkedPR()
	if _, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: store.RoleJudge,
		HerdrPaneID: store.Ptr("p-old"), State: store.SessionStarting}); err != nil {
		t.Fatal(err)
	}
	earlier := len(h.ag.all())
	id := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()

	r := h.request(id)
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "restored") || strings.Contains(deref(r.Result), "nothing to restore") {
		t.Fatalf("open request: state=%s result=%q", r.State, deref(r.Result))
	}
	if !slices.Contains(h.ag.all()[earlier:], "start:"+itoa(pr.ID)+":codex-judge:") {
		t.Fatalf("no judge start after the request: %v", h.ag.all()[earlier:])
	}
}

// A `magnum open` that fails after reserving a free slot hands it back: the
// sessions are parked and the slot released.
func TestFailedOpenReleasesTheReservedSlot(t *testing.T) {
	h := newHarness(t)
	pr := h.parkedPR()
	h.sl.holdErr = errors.New("checkout failed")
	agentsBefore := len(h.ag.all())
	slotsBefore := len(h.sl.all())
	id := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()

	r := h.request(id)
	if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "checkout failed") {
		t.Fatalf("open request: state=%s result=%q", r.State, deref(r.Result))
	}
	sl := h.slot("review1")
	if sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("slot after the failed open: state=%s pr=%v", sl.State, sl.PRID)
	}
	if !slices.Contains(h.sl.all()[slotsBefore:], "release:review1:open failed") {
		t.Fatalf("slot calls %v lack the release", h.sl.all()[slotsBefore:])
	}
	if !slices.Contains(h.ag.all()[agentsBefore:], "park:"+itoa(pr.ID)) {
		t.Fatalf("agent calls %v lack the park", h.ag.all()[agentsBefore:])
	}
	if n := fixPollEventCount(t, h, "slot:review1", "slot.released"); n < 1 {
		t.Fatal("no slot.released event")
	}
}

// The radar reporting a head a round already reviewed (the round's checkout
// fetched it first) settles the PR as reviewed instead of queueing it.
func TestRadarCatchingUpToAReviewedHeadQueuesNothing(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.wantState(2, store.PRQueued)

	h.sl.moveHead = "b9" // the checkout fetches a newer head than the store knows
	h.advance(5 * time.Minute)
	h.tick()
	if rounds := h.rd.all(); len(rounds) != 1 || rounds[0].TargetSHA != "b9" {
		t.Fatalf("rounds = %+v, want one at b9", rounds)
	}
	pr := h.wantState(2, store.PRRereviewPending)
	if deref(pr.ReviewedSHA) != "b9" || pr.HeadSHA != "b1" {
		t.Fatalf("reviewed_sha=%q head=%q, want b9 and b1", deref(pr.ReviewedSHA), pr.HeadSHA)
	}

	// The radar catches up.
	h.sl.moveHead = ""
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b9"})
	h.tick()
	pr = h.wantState(2, store.PRReviewed)
	if deref(pr.ReviewedSHA) != "b9" || pr.HeadSHA != "b9" {
		t.Fatalf("reviewed_sha=%q head=%q, want b9 both", deref(pr.ReviewedSHA), pr.HeadSHA)
	}
	if !slices.ContainsFunc(fixPollEvents(t, h, "pr:talkable/talkable#2"), func(ev store.Event) bool {
		return ev.Kind == "pr.reviewed" && strings.Contains(ev.Message, "already reviewed")
	}) {
		t.Fatal("no pr.reviewed event saying the head was already reviewed")
	}

	h.advance(time.Hour)
	h.tick()
	h.wantState(2, store.PRReviewed)
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want 1", n)
	}
}

// A round whose judge found the PR closed does not mark a head reviewed that
// it never reviewed: the PR waits for a recheck in line.
func TestJudgeFoundClosedRequeuesAMovedHead(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeClosed, Round: 2}, nil
	}

	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.wantState(2, store.PRRereviewPending)
	h.advance(40 * time.Minute)
	h.tick()

	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want 2", n)
	}
	pr := h.wantState(2, store.PRRereviewPending)
	if deref(pr.ReviewedSHA) != "b1" || pr.HeadSHA != "b2" {
		t.Fatalf("reviewed_sha=%q head=%q, want b1 and b2", deref(pr.ReviewedSHA), pr.HeadSHA)
	}
	if deref(pr.LastError) != "the judge found the PR closed" {
		t.Fatalf("last_error = %q", deref(pr.LastError))
	}
	if want := h.clock.Now().Add(5 * time.Minute); pr.NextAttemptAt == nil || !pr.NextAttemptAt.Equal(want) {
		t.Fatalf("next_attempt_at = %v, want %v", pr.NextAttemptAt, want)
	}
}
