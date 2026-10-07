package engine

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

const manualReason = "manual repository (manual_repos)"

// wantManual fails unless PR #n is ineligible as a PR of a manual
// repository.
func wantManual(t *testing.T, h *harness, n int) {
	t.Helper()
	if pr := h.wantState(n, store.PRIneligible); deref(pr.SkipReason) != manualReason {
		t.Fatalf("PR #%d skip_reason = %q, want %q", n, deref(pr.SkipReason), manualReason)
	}
}

// A repository in manual_repos is polled and shown, but no round starts on
// its own: not for a new PR, a push or a review request on GitHub. A
// review the operator asks for runs.
func TestManualRepositoryReviewsOnlyOnRequest(t *testing.T) {
	h := newHarness(t)
	h.cfg.Watches[0].ManualRepos = []string{"talkable"}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	wantManual(t, h, 2) // a new PR

	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	wantManual(t, h, 2) // a push

	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
	wantManual(t, h, 2) // a review request on GitHub
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 0)

	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	h.advance(time.Minute)
	h.tick()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("review request: %s %q", r.State, deref(r.Result))
	}
	reqWantRounds(t, h, 1)
	if in := h.rd.all()[0]; in.TargetSHA != "b2" {
		t.Fatalf("the forced round reviewed %s, want b2", in.TargetSHA)
	}
	h.wantState(2, store.PRReviewed)

	// The next push is the operator's call again.
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b3"})
	h.tick()
	wantManual(t, h, 2)
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
}

// A new PR of a manual repository goes in ineligible on the poll that first
// sees it: the classification of a new PR knows its repository, so it is
// neither inserted queued nor logged as a PR of no repository.
func TestNewPRInAManualRepositoryGoesInIneligible(t *testing.T) {
	var logs syncBuffer
	h := newHarness(t, func(h *harness) {
		h.d.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	h.cfg.Watches[0].ManualRepos = []string{"talkable"}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	wantManual(t, h, 2)
	skipped := kindOf(subjectEvents(t, h, "pr:talkable/talkable#2"), "pr.ineligible")
	if len(skipped) != 1 || skipped[0].Message != "new PR skipped: "+manualReason {
		t.Fatalf("pr.ineligible events: %+v, want the new PR skipped", skipped)
	}
	if strings.Contains(logs.String(), "eligibility: repository") {
		t.Fatalf("a new PR's classification read no repository:\n%s", logs.String())
	}
}

// A PR already waiting when its repository joins manual_repos becomes
// ineligible at dispatch instead of starting a round.
func TestManualRepositoryTakesBackAWaitingPR(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.wantState(2, store.PRQueued)

	h.cfg.Watches[0].ManualRepos = []string{"Talkable"} // edited in place, as a reload would
	h.advance(5 * time.Minute)
	h.tick()
	wantManual(t, h, 2)
	reqWantRounds(t, h, 0)
}

// A waiting PR a changed configuration now rejects is taken back when the
// daemon starts, not only at dispatch: dispatch may be held (the Codex
// soft cap holds full re-reviews) for days, and meanwhile the board said
// "queued" for a PR that will never be reviewed on its own.
func TestStartupTakesBackAWaitingPRTheConfigNowRejects(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.wantState(2, store.PRQueued)

	h.cfg.Watches[0].ManualRepos = []string{"Talkable"} // the operator's edit, then a restart
	h.e.reclassifyIneligible(h.ctx)
	wantManual(t, h, 2)
}
