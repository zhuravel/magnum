package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// withoutDrafts sets include_drafts = false on every watch.
var withoutDrafts = reqWatches(func(w *config.Watch) { w.IncludeDrafts = new(false) })

// draftSynced is a harness past the first sync (#1 the baseline).
func draftSynced(t *testing.T, mods ...func(*harness)) *harness {
	t.Helper()
	h := newHarness(t, mods...)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	return h
}

// A review request for magnum on a draft the watch skips (include_drafts =
// false) makes it eligible for the one round the request starts; the wait
// says why. GitHub keeps the request on the timeline and in the PR's
// requested reviewers after magnum posts, so a later push is skipped again;
// only a newer request runs another round.
func TestReviewRequestOnASkippedDraftRunsOneRound(t *testing.T) {
	h := draftSynced(t, withoutDrafts)
	reqPoll(h, prSpec{n: 2, head: "d1", draft: true})
	h.wantState(2, store.PRIneligible)
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 0)

	h.advance(time.Minute)
	ask := reqAsk(h, "alice", askedPoll)
	asked := prSpec{n: 2, head: "d1", draft: true, request: true, requests: []github.ReviewRequestEvent{ask}}
	reqPoll(h, asked)
	h.wantState(2, store.PRQueued)
	dueAt := ask.CreatedAt.Add(time.Minute)
	w := waitOf(t, h, 2)
	if w.Reason != WaitRequested || w.Subject != "requested on a draft by alice" || !w.Until.Equal(dueAt) {
		t.Fatalf("wait = %+v", w)
	}
	now := h.clock.Now()
	if got, want := w.Short(now), "review · requested on a draft by alice → "+clockText(dueAt); got != want {
		t.Errorf("short = %q, want %q", got, want)
	}
	wantSentence := "review requested on a draft by alice waits for the request debounce (1m after the request or the last push) until " +
		clockText(dueAt) + "; `magnum review talkable#2` runs it now"
	if got := w.Sentence("talkable#2", now); got != wantSentence {
		t.Errorf("sentence = %q, want %q", got, wantSentence)
	}
	if evs := reqEvents(t, h); len(evs) != 1 || !strings.Contains(evs[0].Message, "review requested from zhuravel by alice on a draft") {
		t.Fatalf("pr.review_requested events: %+v", evs)
	}

	h.advance(time.Minute) // the debounce ends
	h.tick()
	reqWantRounds(t, h, 1)
	if in := h.rd.all()[0]; in.Kind != pipeline.KindInitial || in.TargetSHA != "d1" {
		t.Fatalf("round 1 = %s of %s, want the first review of d1", in.Kind, in.TargetSHA)
	}
	h.wantState(2, store.PRReviewed)

	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "d2", draft: true, request: true, requests: []github.ReviewRequestEvent{ask}})
	if pr := h.wantState(2, store.PRIneligible); deref(pr.SkipReason) != "draft PR (include_drafts = false)" {
		t.Fatalf("skip_reason = %q", deref(pr.SkipReason))
	}
	h.advance(3 * time.Hour)
	reqPoll(h, prSpec{n: 2, head: "d2", draft: true, request: true, requests: []github.ReviewRequestEvent{ask}})
	h.wantState(2, store.PRIneligible)
	reqWantRounds(t, h, 1)

	h.advance(time.Minute)
	again := reqAsk(h, "bob-rev", askedPoll)
	reqPoll(h, prSpec{n: 2, head: "d2", draft: true, request: true, requests: []github.ReviewRequestEvent{ask, again}})
	h.wantState(2, store.PRRereviewPending)
	if w := waitOf(t, h, 2); w.Subject != "requested on a draft by bob-rev" {
		t.Fatalf("wait after the newer request = %+v", w)
	}
	h.advance(time.Minute)
	h.tick()
	reqWantRounds(t, h, 2)
	if in := h.rd.all()[1]; in.Kind != pipeline.KindRereview || in.TargetSHA != "d2" {
		t.Fatalf("round 2 = %s of %s, want the re-review of d2", in.Kind, in.TargetSHA)
	}
	h.wantState(2, store.PRReviewed)
}

// A draft opened with magnum already requested is queued by the poll that
// first sees it.
func TestNewSkippedDraftWithAReviewRequestIsQueued(t *testing.T) {
	h := draftSynced(t, withoutDrafts)
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "d1", draft: true, request: true,
		requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedBot)}})
	if pr := h.wantState(2, store.PRQueued); pr.SkipReason != nil {
		t.Fatalf("skip_reason = %q on a queued PR", *pr.SkipReason)
	}
	h.advance(time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	h.wantState(2, store.PRReviewed)
}

// A request for someone else, or a team the watch does not list, leaves a
// skipped draft skipped.
func TestReviewRequestForSomeoneElseLeavesASkippedDraftSkipped(t *testing.T) {
	for _, reviewer := range []github.Reviewer{{Type: "User", Login: "rev-ann"}, {Type: "Team", Login: "reviewers"}} {
		t.Run(reviewer.Login, func(t *testing.T) {
			h := draftSynced(t, withoutDrafts)
			reqPoll(h, prSpec{n: 2, head: "d1", draft: true})
			h.advance(time.Minute)
			reqPoll(h, prSpec{n: 2, head: "d1", draft: true, requests: []github.ReviewRequestEvent{reqAsk(h, "alice", reviewer)}})
			h.wantState(2, store.PRIneligible)
			h.advance(time.Hour)
			h.tick()
			reqWantRounds(t, h, 0)
		})
	}
}

// include_drafts = true (the default) is unchanged: a draft is reviewed
// without a request, and a push to it waits for the draft re-review
// interval, not for a request.
func TestDraftsIncludedNeedNoRequest(t *testing.T) {
	h := draftSynced(t)
	reqPoll(h, prSpec{n: 2, head: "d1", draft: true})
	h.wantState(2, store.PRQueued)
	h.advance(5 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	h.wantState(2, store.PRReviewed)

	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "d2", draft: true})
	h.wantState(2, store.PRRereviewPending)
	if w := waitOf(t, h, 2); w.Reason != WaitDraftInterval {
		t.Fatalf("wait = %+v, want the draft interval", w)
	}
}
