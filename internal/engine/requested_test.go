package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// ---- helpers ----

var (
	askedPoll  = github.Reviewer{Type: "User", Login: "zhuravel"}
	askedBot   = github.Reviewer{Type: "Bot", Login: "talkable"}
	reqSubject = "pr:talkable/talkable#2"
)

// reqEvent is a ReviewRequestedEvent at asking actor's review of reviewer.
func reqEvent(at time.Time, actor string, reviewer github.Reviewer) github.ReviewRequestEvent {
	return github.ReviewRequestEvent{CreatedAt: at, Actor: actor, Reviewer: reviewer}
}

// reqAsk is a review request of reviewer by actor at the harness's now.
func reqAsk(h *harness, actor string, reviewer github.Reviewer) github.ReviewRequestEvent {
	return reqEvent(h.clock.Now(), actor, reviewer)
}

// reqPoll shows spec (next to the baseline #1) and runs one tick. Details
// are fetched only for a PR whose radar row changed: move the clock since
// the last poll that showed it (h.open stamps the PR's updated time with it).
func reqPoll(h *harness, spec prSpec) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"}, spec)
	h.tick()
}

// reqPollNoSettle is reqPoll for a test that holds a round in flight: the
// tick does not wait for rounds.
func reqPollNoSettle(h *harness, spec prSpec) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"}, spec)
	if err := h.e.Tick(h.ctx); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
}

// reqInPipeline waits until a round beyond the first n reached the gated
// pipeline: its PR is reviewing and the round's start is recorded.
func reqInPipeline(h *harness, n int) {
	h.t.Helper()
	for i := 0; len(h.rd.all()) <= n; i++ {
		if i > 500 {
			h.t.Fatal("the round never reached the pipeline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func reqWantRounds(t *testing.T, h *harness, n int) {
	t.Helper()
	if got := len(h.rd.all()); got != n {
		t.Fatalf("rounds = %d, want %d", got, n)
	}
}

// reqEvents lists PR #2's pr.review_requested events.
func reqEvents(t *testing.T, h *harness) []store.Event {
	t.Helper()
	return kindOf(subjectEvents(t, h, reqSubject), "pr.review_requested")
}

// reqWantRoundsToday fails unless PR #2 shows n automatic rounds today.
func reqWantRoundsToday(t *testing.T, h *harness, n int) {
	t.Helper()
	if got := h.pr(2).RoundsToday; got != n {
		t.Fatalf("rounds_today = %d, want %d", got, n)
	}
}

// reqSetFiles answers CompareFiles of key ("base...head").
func reqSetFiles(h *harness, key string, files []github.FileDelta) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if h.gh.files == nil {
		h.gh.files = map[string][]github.FileDelta{}
	}
	h.gh.files[key] = files
	if _, ok := h.gh.compare[key]; !ok {
		h.gh.compare[key] = github.CompareStats{Commits: 1} // a head ahead has a commit, or GitHub calls it behind
	}
}

// reqWatches applies mod to every watch of the harness's config.
func reqWatches(mod func(w *config.Watch)) func(h *harness) {
	return func(h *harness) {
		for i := range h.cfg.Watches {
			mod(&h.cfg.Watches[i])
		}
	}
}

// codeDelta is a Ruby file with n added code lines: n changed lines.
func codeDelta(n int) []github.FileDelta {
	var b strings.Builder
	fmt.Fprintf(&b, "@@ -1,2 +1,%d @@\n class Coupon\n", n+2)
	for i := range n {
		fmt.Fprintf(&b, "+  LIMIT_%d = %d\n", i, i)
	}
	b.WriteString(" end")
	return []github.FileDelta{{Path: "app/models/coupon.rb", Status: "modified", Patch: b.String()}}
}

// addedFile is a delta that adds one small file.
var addedFile = []github.FileDelta{{Path: "app/models/thing.rb", Status: "added", Patch: "@@ -0,0 +1,2 @@\n+class Thing\n+end"}}

// clockText is t as a screen shows it today ("15:04").
func clockText(t time.Time) string { return t.Local().Format("15:04") }

// ---- requests ----

// A request for the poll login on a reviewed PR whose head was already
// reviewed starts a round one debounce after the request, not an interval
// after the last round, and it does not use the daily cap.
func TestReviewRequestRunsAfterTheDebounceNotTheInterval(t *testing.T) {
	h := newHarness(t)
	before := h.reviewedPR(2, "b1") // round 1 started at 10:05
	h.advance(time.Minute)
	askedAt := h.clock.Now()
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqEvent(askedAt, "alice", askedPoll)}})

	pr := h.wantState(2, store.PRRereviewPending)
	if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(askedAt.Add(time.Minute)) {
		t.Fatalf("next_eligible_at = %v, want the request + 1m (%v)", pr.NextEligibleAt, askedAt.Add(time.Minute))
	}
	reqWantRounds(t, h, 1)
	if at, ok := h.e.kvTime(h.ctx, KVPRRequestAt(pr.ID)); !ok || !at.Equal(askedAt) {
		t.Fatalf("%s = %v (%v), want %v", KVPRRequestAt(pr.ID), at, ok, askedAt)
	}
	if by, _ := kvValue(h, KVPRRequestBy(pr.ID)); by != "alice" {
		t.Fatalf("%s = %q, want alice", KVPRRequestBy(pr.ID), by)
	}
	evs := reqEvents(t, h)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "review requested from zhuravel by alice") {
		t.Fatalf("pr.review_requested events: %+v", evs)
	}
	var data struct {
		By    string
		State string
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || data.By != "alice" || data.State != store.PRReviewed {
		t.Fatalf("event data %s: %+v (%v)", evs[0].Data, data, err)
	}

	h.advance(time.Minute) // the debounce ends: 10:07, far from the 10:35 interval
	h.tick()
	reqWantRounds(t, h, 2)
	if in := h.rd.all()[1]; in.Kind != pipeline.KindRereview || in.TargetSHA != "b1" {
		t.Fatalf("round 2 = %s of %s, want a re-review of b1", in.Kind, in.TargetSHA)
	}
	pr = h.wantState(2, store.PRReviewed)
	if pr.LastRoundStartedAt == nil || !pr.LastRoundStartedAt.Equal(askedAt.Add(time.Minute)) {
		t.Fatalf("last_round_started_at = %v, want %v", pr.LastRoundStartedAt, askedAt.Add(time.Minute))
	}
	if pr.RoundsToday != before.RoundsToday {
		t.Fatalf("rounds_today = %d after a requested round, want %d", pr.RoundsToday, before.RoundsToday)
	}
	if _, pending := h.e.pendingRequest(h.ctx, pr); pending {
		t.Fatal("the request is still pending after its round")
	}
}

// The reviewers magnum answers to: the poll login, the App's login in its
// GraphQL form (type Bot, no suffix) and its REST form ("[bot]"); not other
// people or bots.
func TestReviewRequestCountsOnlyForOurLogins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reviewer github.Reviewer
		counts   bool
	}{
		{"the poll login", askedPoll, true},
		{"the poll login in another case", github.Reviewer{Type: "User", Login: "Zhuravel"}, true},
		{"the App as GraphQL names it", askedBot, true},
		{"the App as REST names it", github.Reviewer{Type: "Bot", Login: "talkable[bot]"}, true},
		{"the App's REST login as a user", github.Reviewer{Type: "User", Login: "talkable[bot]"}, true},
		{"another user", github.Reviewer{Type: "User", Login: "bob"}, false},
		{"another bot", github.Reviewer{Type: "Bot", Login: "other-app"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reviewedPR(2, "b1")
			h.advance(time.Minute)
			reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", tc.reviewer)}})
			h.advance(time.Minute) // the debounce passed, the 30m interval did not
			h.tick()
			id := h.pr(2).ID
			if !tc.counts {
				reqWantRounds(t, h, 1)
				if v, ok := kvValue(h, KVPRRequestAt(id)); ok {
					t.Fatalf("%s = %q for a request that is not for us", KVPRRequestAt(id), v)
				}
				if evs := reqEvents(t, h); len(evs) != 0 {
					t.Fatalf("pr.review_requested events: %+v", evs)
				}
				h.wantState(2, store.PRReviewed)
				return
			}
			reqWantRounds(t, h, 2)
			if by, _ := kvValue(h, KVPRRequestBy(id)); by != "alice" {
				t.Fatalf("%s = %q, want alice", KVPRRequestBy(id), by)
			}
		})
	}
}

// A team counts only when the watch lists its slug in request_teams.
func TestReviewRequestForATeamNeedsRequestTeams(t *testing.T) {
	listed := func(teams ...string) func(*harness) {
		return reqWatches(func(w *config.Watch) { w.RequestTeams = teams })
	}
	for _, tc := range []struct {
		name   string
		mod    func(*harness)
		team   string
		counts bool
	}{
		{"a listed team", listed("reviewers"), "reviewers", true},
		{"a listed team in another case, with spaces", listed(" Reviewers "), "reviewers", true},
		{"a team the watch does not list", listed("reviewers"), "others", false},
		{"no request_teams", listed(), "reviewers", false},
		{"a team named like the poll login is not the user", listed(), "zhuravel", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mod)
			h.reviewedPR(2, "b1")
			h.advance(time.Minute)
			ev := reqAsk(h, "alice", github.Reviewer{Type: "Team", Login: tc.team})
			reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}})
			h.advance(time.Minute)
			h.tick()
			if tc.counts {
				reqWantRounds(t, h, 2)
				if evs := reqEvents(t, h); len(evs) != 1 || !strings.Contains(evs[0].Message, "team "+tc.team) {
					t.Fatalf("pr.review_requested events: %+v", evs)
				}
				return
			}
			reqWantRounds(t, h, 1)
			if evs := reqEvents(t, h); len(evs) != 0 {
				t.Fatalf("pr.review_requested events: %+v", evs)
			}
		})
	}
}

// ---- edge triggering ----

// The timeline keeps a request after it was served: every poll that
// fetches the PR's details sees it again, and it must not restart the
// debounce or run a second round.
func TestSameReviewRequestSeenTwiceRunsOneRound(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.advance(time.Minute)
	ev := reqAsk(h, "alice", askedPoll)
	spec := prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}}
	reqPoll(h, spec) // 10:06
	due := h.pr(2).NextEligibleAt
	if due == nil || !due.Equal(ev.CreatedAt.Add(time.Minute)) {
		t.Fatalf("next_eligible_at = %v", due)
	}

	h.advance(30 * time.Second)
	reqPoll(h, spec) // the same request again, before its round ran
	if cur := h.pr(2).NextEligibleAt; cur == nil || !cur.Equal(*due) {
		t.Fatalf("a second sighting moved next_eligible_at to %v, want %v", cur, *due)
	}
	reqWantRounds(t, h, 1)

	h.advance(30 * time.Second)
	h.tick()
	reqWantRounds(t, h, 2)

	h.advance(5 * time.Minute)
	reqPoll(h, spec) // and again after the round
	h.advance(time.Hour)
	reqPoll(h, spec)
	reqWantRounds(t, h, 2)
	if evs := reqEvents(t, h); len(evs) != 1 {
		t.Fatalf("pr.review_requested events: %d, want 1", len(evs))
	}
}

// Edge-triggered by the request's time, not once per PR: a newer request
// runs another round.
func TestEachNewReviewRequestRunsOneRound(t *testing.T) {
	h := newHarness(t)
	before := h.reviewedPR(2, "b1")
	h.advance(time.Minute)
	first := reqAsk(h, "alice", askedPoll)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{first}})
	h.advance(time.Minute)
	h.tick()
	reqWantRounds(t, h, 2)

	h.advance(10 * time.Minute)
	second := reqAsk(h, "bob", askedBot)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{first, second}})
	if by, _ := kvValue(h, KVPRRequestBy(before.ID)); by != "bob" {
		t.Fatalf("%s = %q, want the newer request's actor bob", KVPRRequestBy(before.ID), by)
	}
	reqWantRounds(t, h, 2)
	h.advance(time.Minute)
	h.tick()
	reqWantRounds(t, h, 3)
	reqWantRoundsToday(t, h, before.RoundsToday)
	if evs := reqEvents(t, h); len(evs) != 2 {
		t.Fatalf("pr.review_requested events: %d, want 2", len(evs))
	}
}

// A request made before the PR's last round started was answered by that
// round: it is not pending and runs nothing, even long after the interval.
func TestReviewRequestOlderThanTheLastRoundStartRunsNothing(t *testing.T) {
	h := newHarness(t)
	before := h.reviewedPR(2, "b1") // round 1 started at 10:05
	old := reqEvent(before.LastRoundStartedAt.Add(-time.Minute), "alice", askedPoll)
	h.advance(40 * time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{old}})
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	pr := h.wantState(2, store.PRReviewed)
	if _, pending := h.e.pendingRequest(h.ctx, pr); pending {
		t.Fatal("a request older than the last round start is pending")
	}
}

// Requests made before the repository's first sync (the PR was open then),
// or at that very moment, are history.
func TestReviewRequestBeforeTheFirstSyncIsIgnored(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Duration // relative to the first sync at 10:00
	}{{"before", -30 * time.Minute}, {"at the sync", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			synced := h.clock.Now()
			spec := prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqEvent(synced.Add(tc.at), "alice", askedPoll)}}
			h.open(prSpec{n: 1, head: "base1"}, spec)
			h.startup()
			h.tick() // first sync: both PRs are a baseline
			h.wantState(2, store.PRBaseline)
			h.advance(time.Minute)
			reqPoll(h, spec) // details again, with the old request in them
			h.advance(10 * time.Minute)
			h.tick()
			reqWantRounds(t, h, 0)
			h.wantState(2, store.PRBaseline)
			if v, ok := kvValue(h, KVPRRequestAt(h.pr(2).ID)); ok {
				t.Fatalf("%s = %q for a request from before the first sync", KVPRRequestAt(h.pr(2).ID), v)
			}
		})
	}
}

// requests_since is when the daemon started tracking requests (set once, at
// startup): an older request never counts, so an upgrade does not turn every
// request ever made into a round.
func TestReviewRequestBeforeTheDaemonTrackedRequestsIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	started := h.clock.Now().Add(-5 * time.Minute) // startup ran at 10:00
	if since, ok := h.e.kvTime(h.ctx, kvRequestsSince); !ok || !since.Equal(started) {
		t.Fatalf("%s = %v (%v), want the startup time %v", kvRequestsSince, since, ok, started)
	}
	h.advance(time.Hour)
	h.e.noteRequestsSince(h.ctx) // already set: not moved
	if since, _ := h.e.kvTime(h.ctx, kvRequestsSince); !since.Equal(started) {
		t.Fatalf("%s moved to %v", kvRequestsSince, since)
	}

	// An upgrade: tracking starts now, so an hour-old request is history and
	// one from after it counts.
	trackedFrom := h.clock.Now()
	h.e.setKV(h.ctx, kvRequestsSince, store.FormatTime(trackedFrom))
	older := reqEvent(trackedFrom.Add(-10*time.Minute), "alice", askedPoll)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{older}})
	h.advance(5 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)

	h.advance(time.Minute)
	newer := reqAsk(h, "alice", askedPoll)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{older, newer}})
	h.advance(time.Minute)
	h.tick()
	reqWantRounds(t, h, 2)
}

// ---- which PRs a request queues ----

func TestReviewRequestQueuesPRsThatWaitForNothing(t *testing.T) {
	t.Run("baseline PR", func(t *testing.T) {
		h := newHarness(t)
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
		h.startup()
		h.tick() // first sync: baseline
		h.wantState(2, store.PRBaseline)
		h.advance(time.Minute)
		ev := reqAsk(h, "alice", askedPoll)
		reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}})
		pr := h.wantState(2, store.PRQueued)
		if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(ev.CreatedAt.Add(time.Minute)) {
			t.Fatalf("next_eligible_at = %v", pr.NextEligibleAt)
		}
		h.advance(time.Minute)
		h.tick()
		reqWantRounds(t, h, 1)
		if in := h.rd.all()[0]; in.Kind != pipeline.KindInitial || in.TargetSHA != "b1" {
			t.Fatalf("round = %s of %s, want the first review of b1", in.Kind, in.TargetSHA)
		}
		h.wantState(2, store.PRReviewed)
		reqWantRoundsToday(t, h, 0)
	})
	t.Run("needs attention PR", func(t *testing.T) {
		h := newHarness(t)
		h.open(prSpec{n: 1, head: "base1"})
		h.startup()
		h.tick()
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
		h.tick()
		h.advance(5 * time.Minute)
		h.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeNeedsAttention, Round: 1, Error: "the judge could not decide"}, nil
		}
		h.tick()
		h.wantState(2, store.PRNeedsAttention)
		h.rd.script = nil
		h.advance(time.Minute)
		ev := reqAsk(h, "alice", askedPoll)
		reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}})
		h.wantState(2, store.PRQueued)
		h.advance(time.Minute)
		h.tick()
		reqWantRounds(t, h, 2)
		h.wantState(2, store.PRReviewed)
	})
	t.Run("muted PR stays muted", func(t *testing.T) {
		h := newHarness(t)
		pr := h.reviewedPR(2, "b1")
		if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("muted", true) }); err != nil {
			t.Fatal(err)
		}
		h.advance(time.Minute)
		reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
		h.advance(10 * time.Minute)
		h.tick()
		reqWantRounds(t, h, 1)
		h.wantState(2, store.PRReviewed)
	})
}

// A PR opened after the first sync with a request for us starts one
// debounce later instead of after the push quiet period.
func TestReviewRequestOnANewPRSkipsTheQuietPeriod(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync at 10:00
	h.advance(time.Minute)
	opened := h.clock.Now() // 10:01
	h.open(prSpec{n: 1, head: "base1"},
		prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqEvent(opened, "alice", askedPoll)}},
		prSpec{n: 3, head: "c1"}) // a PR nobody asked about, for contrast
	h.tick()
	two := h.wantState(2, store.PRQueued)
	if two.NextEligibleAt == nil || !two.NextEligibleAt.Equal(opened.Add(time.Minute)) {
		t.Fatalf("#2 next_eligible_at = %v, want the request + 1m", two.NextEligibleAt)
	}
	reqWantRounds(t, h, 0)

	h.advance(time.Minute) // 10:02
	h.tick()
	ins := h.rd.all()
	if len(ins) != 1 || ins[0].PR.Number != 2 || ins[0].Kind != pipeline.KindInitial {
		t.Fatalf("rounds at 10:02 = %d, want only the first review of #2", len(ins))
	}
	reqWantRoundsToday(t, h, 0)
	// #3, which nobody asked about, still waits out the quiet period.
	if three := h.wantState(3, store.PRQueued); three.NextEligibleAt == nil || !three.NextEligibleAt.Equal(opened.Add(5*time.Minute)) {
		t.Fatalf("#3 next_eligible_at = %v, want the quiet period's end %v", three.NextEligibleAt, opened.Add(5*time.Minute))
	}
}

// A PR already waiting out its quiet period gets a shorter wait when the
// request arrives, and the wait shows it.
func TestReviewRequestShortensTheWaitOfAQueuedPR(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync at 10:00
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b1"}) // new at 10:01: due at 10:06
	opened := h.clock.Now()
	pr := h.wantState(2, store.PRQueued)
	if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(opened.Add(5*time.Minute)) {
		t.Fatalf("next_eligible_at = %v, want the quiet period's end", pr.NextEligibleAt)
	}
	if w := waitOf(t, h, 2); w.Reason != WaitQuiet {
		t.Fatalf("wait = %+v", w)
	}

	h.advance(2 * time.Minute) // 10:03
	ev := reqAsk(h, "alice", askedPoll)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}})
	due := ev.CreatedAt.Add(time.Minute)
	pr = h.wantState(2, store.PRQueued)
	if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(due) {
		t.Fatalf("next_eligible_at = %v, want the request + 1m (%v)", pr.NextEligibleAt, due)
	}
	w := waitOf(t, h, 2)
	if w.Reason != WaitRequested || w.Rereview || w.Short(h.clock.Now()) != "review · requested by alice → "+clockText(due) {
		t.Fatalf("wait = %+v (%q)", w, w.Short(h.clock.Now()))
	}
	reqWantRounds(t, h, 0)
	h.advance(time.Minute) // 10:04, not 10:06
	h.tick()
	reqWantRounds(t, h, 1)
	reqWantRoundsToday(t, h, 0)
}

// ---- a request skips every other timing rule ----

func TestReviewRequestSkipsTheTimingRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mod    func(*harness)
		files  []github.FileDelta // the delta b1...b2
		draft  bool
		pushes []string // pushed from 10:45 on, a minute apart
		hold   string   // why the automatic re-review waits before the request
	}{
		{name: "daily cap", mod: func(h *harness) { h.cfg.Daemon.MaxRoundsPerPRPerDay = 1 }, pushes: []string{"b2"}, hold: WaitCap},
		{name: "small delta", files: rubyMixed, pushes: []string{"b2"}, hold: WaitDelta},
		{name: "burst quiet period", pushes: []string{"b2", "b3", "b4"}, hold: WaitBurst},
		{name: "draft interval", draft: true, pushes: []string{"b2"}, hold: WaitDraftInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mods []func(*harness)
			if tc.mod != nil {
				mods = append(mods, tc.mod)
			}
			h := newHarness(t, mods...)
			before := h.reviewedPR(2, "b1") // round 1 at 10:05
			if tc.files != nil {
				reqSetFiles(h, "b1...b2", tc.files)
			}
			h.advance(40 * time.Minute) // past the 30m interval
			head := ""
			for i, p := range tc.pushes {
				if i > 0 {
					h.advance(time.Minute)
				}
				head = p
				reqPoll(h, prSpec{n: 2, head: head, draft: tc.draft})
			}
			h.wantState(2, store.PRRereviewPending)
			if w := waitOf(t, h, 2); w.Reason != tc.hold {
				t.Fatalf("wait before the request = %+v, want %s", w, tc.hold)
			}
			reqWantRounds(t, h, 1)

			h.advance(time.Minute)
			ev := reqAsk(h, "alice", askedPoll)
			reqPoll(h, prSpec{n: 2, head: head, draft: tc.draft, requests: []github.ReviewRequestEvent{ev}})
			if w := waitOf(t, h, 2); w.Reason != WaitRequested || !w.Until.Equal(ev.CreatedAt.Add(time.Minute)) {
				t.Fatalf("wait after the request = %+v", w)
			}
			reqWantRounds(t, h, 1)

			h.advance(time.Minute) // the debounce ends
			h.tick()
			ins := h.rd.all()
			if len(ins) != 2 || ins[1].TargetSHA != head {
				t.Fatalf("rounds = %d (last %q), want the re-review of %s", len(ins), ins[len(ins)-1].TargetSHA, head)
			}
			pr := h.wantState(2, store.PRReviewed)
			if pr.RoundsToday != before.RoundsToday {
				t.Fatalf("rounds_today = %d, want %d", pr.RoundsToday, before.RoundsToday)
			}
		})
	}
}

// The debounce counts from the later of the request and the last push: a
// push right after the request restarts it, so the author's follow-up
// commits are reviewed together.
func TestReviewRequestDebounceRestartsOnAPush(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.advance(time.Minute)
	ev := reqAsk(h, "alice", askedPoll)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}}) // 10:06
	h.advance(30 * time.Second)
	reqPoll(h, prSpec{n: 2, head: "b2", requests: []github.ReviewRequestEvent{ev}}) // pushed at 10:06:30
	pushedAt := h.clock.Now()
	pr := h.wantState(2, store.PRRereviewPending)
	if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(pushedAt.Add(time.Minute)) {
		t.Fatalf("next_eligible_at = %v, want the push + 1m (%v)", pr.NextEligibleAt, pushedAt.Add(time.Minute))
	}
	h.advance(30 * time.Second) // 10:07: one minute after the request, 30s after the push
	h.tick()
	reqWantRounds(t, h, 1)
	h.advance(30 * time.Second)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].TargetSHA != "b2" {
		t.Fatalf("rounds = %+v, want the re-review of b2 one minute after the push", ins)
	}
	reqWantRoundsToday(t, h, 1)
}

// ---- a request during a round ----

// The round that is running reviews the head the request was made for: it
// answers the request, no second round follows.
func TestReviewRequestDuringARoundThatCoversTheHeadIsAnswered(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	if err := h.e.Tick(h.ctx); err != nil { // 10:05: the round starts and waits at the gate
		t.Fatal(err)
	}
	reqInPipeline(h, 0)
	h.wantState(2, store.PRReviewing)

	h.advance(time.Minute)
	ev := reqAsk(h, "alice", askedPoll)
	reqPollNoSettle(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}})
	h.wantState(2, store.PRReviewing) // a PR in a round keeps it
	id := h.pr(2).ID
	if at, ok := h.e.kvTime(h.ctx, KVPRRequestAt(id)); !ok || !at.Equal(ev.CreatedAt) {
		t.Fatalf("%s = %v (%v), want the request recorded during the round", KVPRRequestAt(id), at, ok)
	}

	h.advance(time.Minute)
	close(h.rd.gate)
	h.settle()
	pr := h.wantState(2, store.PRReviewed)
	if _, pending := h.e.pendingRequest(h.ctx, pr); pending {
		t.Fatal("the review of the head did not answer the request")
	}
	h.advance(time.Hour)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}})
	h.advance(time.Hour)
	h.tick()
	reqWantRounds(t, h, 1)
	reqWantRoundsToday(t, h, 1)
}

// The poller may see the request only after the round ended: the review,
// recorded after the request was made, answers it all the same.
func TestReviewRequestMadeDuringARoundButSeenAfterItIsAnswered(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	if err := h.e.Tick(h.ctx); err != nil { // 10:05
		t.Fatal(err)
	}
	reqInPipeline(h, 0)
	h.advance(time.Minute)
	ev := reqAsk(h, "alice", askedPoll) // 10:06: made, not polled yet
	h.advance(time.Minute)
	close(h.rd.gate)
	h.settle() // the review is recorded at 10:07
	h.wantState(2, store.PRReviewed)

	h.advance(30 * time.Second)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}})
	pr := h.wantState(2, store.PRReviewed)
	if _, pending := h.e.pendingRequest(h.ctx, pr); pending {
		t.Fatal("a request made before the review was recorded is pending")
	}
	h.advance(time.Hour)
	h.tick()
	reqWantRounds(t, h, 1)
}

// The round reviews an older head than the one the author pushed while it
// ran: the re-review it leaves is the requested one.
func TestReviewRequestDuringARoundThatMissedTheHeadRunsAsRequested(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	if err := h.e.Tick(h.ctx); err != nil { // 10:05
		t.Fatal(err)
	}
	reqInPipeline(h, 0)
	h.advance(time.Minute)
	ev := reqAsk(h, "alice", askedPoll)
	reqPollNoSettle(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}}) // 10:06
	h.advance(30 * time.Second)
	reqPollNoSettle(h, prSpec{n: 2, head: "b2", requests: []github.ReviewRequestEvent{ev}}) // pushed during the round
	pushedAt := h.clock.Now()
	h.advance(30 * time.Second)
	close(h.rd.gate)
	h.settle()
	pr := h.wantState(2, store.PRRereviewPending)
	if deref(pr.ReviewedSHA) != "b1" {
		t.Fatalf("reviewed_sha = %q, want b1", deref(pr.ReviewedSHA))
	}
	if req, pending := h.e.pendingRequest(h.ctx, pr); !pending || req.By != "alice" {
		t.Fatalf("pending request = %+v (%v), want alice's", req, pending)
	}
	if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(pushedAt.Add(time.Minute)) {
		t.Fatalf("next_eligible_at = %v, want the push + 1m (%v), not the quiet period", pr.NextEligibleAt, pushedAt.Add(time.Minute))
	}
	reqWantRounds(t, h, 1)

	h.advance(30 * time.Second) // 10:07:30: the debounce after the push ends
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].TargetSHA != "b2" {
		t.Fatalf("rounds = %+v, want the re-review of b2", ins)
	}
	h.wantState(2, store.PRReviewed)
	reqWantRoundsToday(t, h, 1) // the first round counted, the requested one did not
}

// ---- draft -> ready for review ----

func TestDraftMarkedReadyIsARequest(t *testing.T) {
	t.Run("a new head", func(t *testing.T) {
		h := newHarness(t)
		before := h.reviewedPR(2, "b1")
		h.advance(40 * time.Minute)
		reqPoll(h, prSpec{n: 2, head: "b2", draft: true}) // 10:45: waits for the 2h draft interval
		h.wantState(2, store.PRRereviewPending)
		if w := waitOf(t, h, 2); w.Reason != WaitDraftInterval {
			t.Fatalf("wait = %+v", w)
		}
		h.advance(time.Minute)
		readyAt := h.clock.Now()
		reqPoll(h, prSpec{n: 2, head: "b2"}) // marked ready for review
		id := h.pr(2).ID
		if by, _ := kvValue(h, KVPRRequestBy(id)); by != RequestReadyForReview {
			t.Fatalf("%s = %q, want %q", KVPRRequestBy(id), by, RequestReadyForReview)
		}
		w := waitOf(t, h, 2)
		if w.Reason != WaitRequested || w.Subject != "ready for review" || !w.Until.Equal(readyAt.Add(time.Minute)) ||
			w.Short(h.clock.Now()) != "re-review · ready for review → "+clockText(readyAt.Add(time.Minute)) {
			t.Fatalf("wait = %+v (%q)", w, w.Short(h.clock.Now()))
		}
		if evs := reqEvents(t, h); len(evs) != 1 || !strings.Contains(evs[0].Message, "draft marked ready for review") {
			t.Fatalf("pr.review_requested events: %+v", evs)
		}
		reqWantRounds(t, h, 1)
		h.advance(time.Minute)
		h.tick()
		ins := h.rd.all()
		if len(ins) != 2 || ins[1].TargetSHA != "b2" {
			t.Fatalf("rounds = %+v, want the re-review of b2", ins)
		}
		reqWantRoundsToday(t, h, before.RoundsToday)
	})
	t.Run("the head is already reviewed", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		h.advance(time.Minute)
		reqPoll(h, prSpec{n: 2, head: "b1", draft: true})
		h.advance(time.Minute)
		reqPoll(h, prSpec{n: 2, head: "b1"}) // ready again: nothing new to review
		h.advance(time.Hour)
		h.tick()
		reqWantRounds(t, h, 1)
		if v, ok := kvValue(h, KVPRRequestAt(h.pr(2).ID)); ok {
			t.Fatalf("%s = %q for a draft whose head was reviewed", KVPRRequestAt(h.pr(2).ID), v)
		}
		if evs := reqEvents(t, h); len(evs) != 0 {
			t.Fatalf("pr.review_requested events: %+v", evs)
		}
	})
}

// ---- the wait shown for a request ----

func TestRequestedWaitShowsWhoAskedAndWhen(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.advance(time.Minute)
	ev := reqAsk(h, "alice", askedPoll)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}})
	dueAt := ev.CreatedAt.Add(time.Minute)

	w := waitOf(t, h, 2)
	if w.Reason != WaitRequested || !w.Rereview || w.Subject != "requested by alice" || !w.Until.Equal(dueAt) {
		t.Fatalf("wait = %+v", w)
	}
	now := h.clock.Now()
	if got, want := w.Short(now), "re-review · requested by alice → "+clockText(dueAt); got != want {
		t.Errorf("short = %q, want %q", got, want)
	}
	wantSentence := "re-review requested by alice waits for the request debounce (1m after the request or the last push) until " +
		clockText(dueAt) + "; `magnum review talkable#2` runs it now"
	if got := w.Sentence("talkable#2", now); got != wantSentence {
		t.Errorf("sentence = %q, want %q", got, wantSentence)
	}
}

// Once the debounce is over and the dispatcher has not started the round
// (and recorded no gate), the wait says it starts at the next dispatch.
func TestRequestedWaitIsNowOnceDueAndNotStarted(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
	h.advance(2 * time.Minute) // due, no dispatch yet
	now := h.clock.Now()
	w := h.e.waitFor(h.ctx, h.pr(2), nil, now)
	if w.Reason != WaitRequested || w.Subject != "requested by alice" || !w.Until.IsZero() {
		t.Fatalf("wait = %+v", w)
	}
	if got, want := w.Short(now), "re-review · requested by alice → now"; got != want {
		t.Errorf("short = %q, want %q", got, want)
	}
	if got, want := w.Sentence("talkable#2", now), "re-review requested by alice starts at the next dispatch"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
}

// A request that is due but held by a dispatch gate (here
// max_concurrent_reviews) shows the gate: the dispatcher's own reason wins
// over the request's "→ now" (waitFor reads the gate before the request).
// The request stays pending and its round runs once capacity frees.
func TestRequestedWaitNamesTheGateThatHoldsIt(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Daemon.MaxConcurrentReviews = 1 })
	// A per-PR repository next to the pool's: its round holds the capacity.
	app := func(prs ...prSpec) {
		for i := range prs {
			prs[i].updated = h.clock.Now()
		}
		h.gh.set("zhuravel/app", prs...)
	}
	app(prSpec{n: 1, head: "x1"})
	before := h.reviewedPR(2, "b1") // 10:05
	app(prSpec{n: 1, head: "x1"}, prSpec{n: 2, head: "y1"})
	h.tick() // zhuravel/app#2 is new: due at 10:10
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	ev := reqAsk(h, "alice", askedPoll)
	reqPollNoSettle(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}}) // 10:10
	reqInPipeline(h, 1)                                                                     // the other repository's round takes the capacity
	h.advance(time.Minute)                                                                  // 10:11: the debounce is over, the capacity is not free
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	w := waitOf(t, h, 2)
	if w.Reason != WaitCapacity || !w.Rereview || !strings.HasPrefix(w.Detail, gateCapacity) ||
		w.Short(h.clock.Now()) != "re-review · capacity" {
		t.Fatalf("wait = %+v (%q)", w, w.Short(h.clock.Now()))
	}
	if _, pending := h.e.pendingRequest(h.ctx, h.pr(2)); !pending {
		t.Fatal("the request is not pending while the PR waits for capacity")
	}
	reqWantRounds(t, h, 2)
	close(h.rd.gate)
	h.settle()
	h.tick() // the capacity is free: the requested round starts
	ins := h.rd.all()
	if len(ins) != 3 || ins[2].PR.Number != 2 || ins[2].TargetSHA != "b1" {
		t.Fatalf("rounds = %d, want #2's requested re-review last", len(ins))
	}
	if pr := h.wantState(2, store.PRReviewed); pr.RoundsToday != before.RoundsToday {
		t.Fatalf("rounds_today = %d, want %d", pr.RoundsToday, before.RoundsToday)
	}
}

// ---- the small-delta threshold ----

// pushedWithDelta reviews #2 at b1 and, 40 minutes later (past the
// re-review interval), pushes b2 whose delta from b1 is files.
func pushedWithDelta(t *testing.T, h *harness, files []github.FileDelta) store.PR {
	t.Helper()
	h.reviewedPR(2, "b1")
	reqSetFiles(h, "b1...b2", files)
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 1}
	pollPR(h, 40*time.Minute, 2, "b2")
	return h.wantState(2, store.PRRereviewPending)
}

func TestSmallDeltaHoldsTheReReviewUntilTheMaxWait(t *testing.T) {
	h := newHarness(t)
	pr := pushedWithDelta(t, h, rubyMixed)
	pushedAt := h.clock.Now() // 10:45
	due := pushedAt.Add(2 * time.Hour)

	var rec DeltaRecord
	v, ok := kvValue(h, KVPRDelta(pr.ID))
	if err := json.Unmarshal([]byte(v), &rec); !ok || err != nil {
		t.Fatalf("%s = %q (%v)", KVPRDelta(pr.ID), v, err)
	}
	if rec.From != "b1" || rec.To != "b2" || rec.Lines != 2 || rec.AddedFiles != 0 || !rec.Complete || !rec.Since.Equal(pushedAt) {
		t.Fatalf("delta record = %+v, want 2 lines b1 → b2 since the push", rec)
	}
	if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(due) {
		t.Fatalf("next_eligible_at = %v, want the push + 2h (%v)", pr.NextEligibleAt, due)
	}
	w := waitOf(t, h, 2)
	if w.Reason != WaitDelta || w.Count != 2 || w.Max != 30 || !w.Until.Equal(due) || !w.Rereview {
		t.Fatalf("wait = %+v", w)
	}
	if got, want := w.Short(h.clock.Now()), "re-review · small delta 2/30 lines → "+clockText(due); got != want {
		t.Errorf("short = %q, want %q", got, want)
	}
	if got := w.Sentence("talkable#2", h.clock.Now()); !strings.Contains(got, "waits for a larger delta (2 of 30 changed lines since the review) or 2h after its first push until "+clockText(due)) {
		t.Errorf("sentence = %q", got)
	}

	h.advance(time.Hour) // 11:45: long past the quiet period and the interval
	h.tick()
	reqWantRounds(t, h, 1)
	h.advance(59 * time.Minute) // 12:44
	h.tick()
	reqWantRounds(t, h, 1)
	h.advance(time.Minute) // 12:45: two hours after the first unreviewed push
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].TargetSHA != "b2" {
		t.Fatalf("rounds = %d, want the re-review of b2 after the max wait", len(ins))
	}
	reqWantRoundsToday(t, h, 2) // an automatic round counts
}

// A later push that brings the unreviewed delta (counted from the reviewed
// commit) to the threshold, or adds a file, is reviewed after the quiet
// period; one line short of the threshold it keeps waiting.
func TestLargerUnreviewedDeltaIsReviewedAfterTheQuietPeriod(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  []github.FileDelta // the delta b1...b3
		review bool
	}{
		{"30 changed lines", codeDelta(30), true},
		{"an added file", addedFile, true},
		{"29 changed lines", codeDelta(29), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			pushedWithDelta(t, h, rubyMixed) // b2 at 10:45: 2 lines
			firstPush := h.clock.Now()
			if got := MeasureDelta(tc.files); got.Lines < 29 && got.AddedFiles == 0 {
				t.Fatalf("helper delta measures %+v", got)
			}
			reqSetFiles(h, "b1...b3", tc.files)
			h.gh.compare["b1...b3"] = github.CompareStats{Commits: 2}
			pollPR(h, time.Minute, 2, "b3") // 10:46
			pushedAt := h.clock.Now()
			pr := h.wantState(2, store.PRRereviewPending)
			if !tc.review {
				w := waitOf(t, h, 2)
				if w.Reason != WaitDelta || w.Count != 29 || !w.Until.Equal(firstPush.Add(2*time.Hour)) {
					t.Fatalf("wait = %+v, want the small delta wait counted from the first push", w)
				}
				h.advance(30 * time.Minute)
				h.tick()
				reqWantRounds(t, h, 1)
				return
			}
			if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(pushedAt.Add(5*time.Minute)) {
				t.Fatalf("next_eligible_at = %v, want the push + 5m (%v)", pr.NextEligibleAt, pushedAt.Add(5*time.Minute))
			}
			if w := waitOf(t, h, 2); w.Reason != WaitQuiet {
				t.Fatalf("wait = %+v, want the quiet period", w)
			}
			h.advance(4 * time.Minute)
			h.tick()
			reqWantRounds(t, h, 1)
			h.advance(time.Minute)
			h.tick()
			ins := h.rd.all()
			if len(ins) != 2 || ins[1].TargetSHA != "b3" {
				t.Fatalf("rounds = %d, want the re-review of b3 after the quiet period", len(ins))
			}
		})
	}
}

// rereview_min_lines = 0 (daemon or watch) turns the threshold off; a
// watch's own value turns it on again.
func TestSmallDeltaThresholdCanBeSetPerWatchOrTurnedOff(t *testing.T) {
	zero, thirty := 0, 30
	for _, tc := range []struct {
		name string
		mod  func(*harness)
		held bool
	}{
		{"on by default", func(*harness) {}, true},
		{"daemon 0", func(h *harness) { h.cfg.Daemon.RereviewMinLines = 0 }, false},
		{"watch 0", reqWatches(func(w *config.Watch) { w.RereviewMinLines = &zero }), false},
		{"watch 30 over daemon 0", func(h *harness) {
			h.cfg.Daemon.RereviewMinLines = 0
			reqWatches(func(w *config.Watch) { w.RereviewMinLines = &thirty })(h)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mod)
			pushedWithDelta(t, h, rubyMixed) // pushed at 10:45
			h.advance(5 * time.Minute)       // 10:50: the quiet period ends
			h.tick()
			if tc.held {
				reqWantRounds(t, h, 1)
				if w := waitOf(t, h, 2); w.Reason != WaitDelta {
					t.Fatalf("wait = %+v, want the small delta wait", w)
				}
				return
			}
			ins := h.rd.all()
			if len(ins) != 2 || ins[1].TargetSHA != "b2" {
				t.Fatalf("rounds = %d, want the re-review of b2 after the quiet period", len(ins))
			}
		})
	}
}

// ---- the daily cap ----

func TestDailyCapCountsAutomaticRoundsOnly(t *testing.T) {
	t.Run("an automatic re-review counts", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		pollPR(h, 40*time.Minute, 2, "b2")
		h.advance(5 * time.Minute)
		h.tick()
		reqWantRounds(t, h, 2)
		reqWantRoundsToday(t, h, 2)
	})
	t.Run("a requested review does not", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		h.advance(time.Minute)
		reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
		h.advance(time.Minute)
		h.tick()
		reqWantRounds(t, h, 2)
		reqWantRoundsToday(t, h, 1)
		if pr := h.pr(2); pr.LastRoundStartedAt == nil || !pr.LastRoundStartedAt.Equal(h.clock.Now()) {
			t.Fatalf("last_round_started_at = %v, want the requested round's start %v", pr.LastRoundStartedAt, h.clock.Now())
		}
	})
	t.Run("a forced review does not", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		h.advance(time.Minute)
		id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Again: true})
		h.tick()
		if r := h.request(id); r.State != store.RequestDone {
			t.Fatalf("magnum review: %+v %q", r, deref(r.Result))
		}
		reqWantRounds(t, h, 2)
		reqWantRoundsToday(t, h, 1)
		if pr := h.pr(2); pr.LastRoundStartedAt == nil || !pr.LastRoundStartedAt.Equal(h.clock.Now()) {
			t.Fatalf("last_round_started_at = %v, want the forced round's start %v", pr.LastRoundStartedAt, h.clock.Now())
		}
	})
}

// With one automatic round allowed per day, the automatic re-review waits
// for midnight while a request still runs, and the request did not use up
// anything.
func TestDailyCapHoldsAutomaticReReviewsButNotRequests(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Daemon.MaxRoundsPerPRPerDay = 1 })
	h.reviewedPR(2, "b1") // the day's one automatic round
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(time.Hour) // far past the quiet period and the interval
	h.tick()
	reqWantRounds(t, h, 1)
	midnight := time.Date(2026, 10, 6, 0, 0, 0, 0, time.Local)
	if w := waitOf(t, h, 2); w.Reason != WaitCap || w.Count != 1 || w.Max != 1 || !w.Until.Equal(midnight) {
		t.Fatalf("wait = %+v", w)
	}

	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
	h.advance(time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].TargetSHA != "b2" {
		t.Fatalf("rounds = %d, want the requested re-review of b2", len(ins))
	}
	reqWantRoundsToday(t, h, 1)

	// The next push is automatic again: still capped.
	pollPR(h, time.Minute, 2, "b3")
	h.advance(time.Hour)
	h.tick()
	reqWantRounds(t, h, 2)
	if w := waitOf(t, h, 2); w.Reason != WaitCap || w.Count != 1 {
		t.Fatalf("wait = %+v, want the daily cap", w)
	}
}

// ---- a refunded requested round ----

// A requested round that magnum itself cut short (a shutdown or abort
// stopped it) goes back: the daily cap is untouched, the round start goes
// back to the round before and the request is pending again, so the retry
// runs as requested.
func TestStoppedRequestedRoundLeavesTheRequestPending(t *testing.T) {
	h := newHarness(t)
	before := h.reviewedPR(2, "b1") // round 1 started at 10:05
	h.advance(time.Minute)
	ev := reqAsk(h, "alice", askedPoll)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{ev}})

	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeStopped, Error: "pipeline: round cancelled"}, nil
	}
	h.advance(time.Minute) // 10:07: the requested round starts and is stopped
	h.tick()
	reqWantRounds(t, h, 2)
	pr := h.wantState(2, store.PRRereviewPending)
	if pr.RoundsToday != before.RoundsToday {
		t.Fatalf("rounds_today = %d after a stopped requested round, want %d", pr.RoundsToday, before.RoundsToday)
	}
	if pr.LastRoundStartedAt == nil || !pr.LastRoundStartedAt.Equal(*before.LastRoundStartedAt) {
		t.Fatalf("last_round_started_at = %v, want the first round's %v", pr.LastRoundStartedAt, before.LastRoundStartedAt)
	}
	if req, pending := h.e.pendingRequest(h.ctx, pr); !pending || req.By != "alice" || !req.At.Equal(ev.CreatedAt) {
		t.Fatalf("pending request = %+v (%v), want alice's request still pending", req, pending)
	}
	evs := refundEvents(t, h)
	if len(evs) != 1 {
		t.Fatalf("round.refunded events: %+v", evs)
	}
	var data struct {
		Reason      string
		Counted     bool
		RoundsToday int `json:"rounds_today"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || data.Reason != RefundStopped || data.Counted || data.RoundsToday != before.RoundsToday {
		t.Fatalf("event %q data %s: %+v (%v)", evs[0].Message, evs[0].Data, data, err)
	}
	if w := h.e.waitFor(h.ctx, pr, nil, h.clock.Now()); w.Reason != WaitRetry {
		t.Fatalf("wait = %+v, want the retry after the stop", w)
	}

	h.rd.script = nil
	h.advance(2 * time.Minute) // past the retry delay after a stop
	h.tick()
	ins := h.rd.all()
	if len(ins) != 3 || ins[2].TargetSHA != "b1" {
		t.Fatalf("rounds = %d, want the requested re-review run again", len(ins))
	}
	pr = h.wantState(2, store.PRReviewed)
	if pr.RoundsToday != before.RoundsToday {
		t.Fatalf("rounds_today = %d after the retry, want %d (still a requested round)", pr.RoundsToday, before.RoundsToday)
	}
	if _, pending := h.e.pendingRequest(h.ctx, pr); pending {
		t.Fatal("the request is still pending after its round ran")
	}
}
