package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// approves makes every round post an approval of its target.
func approves(h *harness) {
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 1, ReviewID: 300, Event: "APPROVED",
			ReviewCommit: in.TargetSHA, TargetSHA: in.TargetSHA}, nil
	}
}

var (
	gateRequired = &github.ReviewGate{Decision: "REVIEW_REQUIRED", Opinions: []github.LatestReview{}, Complete: true}
	gateMine     = &github.ReviewGate{Decision: "CHANGES_REQUESTED", Complete: true, Opinions: []github.LatestReview{
		{State: "CHANGES_REQUESTED", AuthorLogin: "zhuravel", AuthorType: "User", CommitOid: "old"}}}
	gateOthers = &github.ReviewGate{Decision: "CHANGES_REQUESTED", Complete: true, Opinions: []github.LatestReview{
		{State: "CHANGES_REQUESTED", AuthorLogin: "zhuravel", AuthorType: "User", CommitOid: "old"},
		{State: "CHANGES_REQUESTED", AuthorLogin: "bob-rev", AuthorType: "User", CommitOid: "old"}}}
)

// The poller stores what GitHub's merge gate says of a PR's reviews, the
// logins of its opinions in Account form (an App's keeps "[bot]").
func TestThePollStoresTheReviewGate(t *testing.T) {
	h := newHarness(t)
	gate := &github.ReviewGate{Decision: "CHANGES_REQUESTED", Complete: true, Opinions: []github.LatestReview{
		{State: "CHANGES_REQUESTED", AuthorLogin: "zhuravel", AuthorType: "User", CommitOid: "old"},
		{State: "APPROVED", AuthorLogin: "helper", AuthorType: "Bot", CommitOid: "b1"}}}
	h.open(prSpec{n: 2, head: "b1", gate: gate})
	h.startup()
	h.tick()
	g := h.pr(2).ReviewGate
	if g == nil || g.Decision != "CHANGES_REQUESTED" || !g.Complete || len(g.Opinions) != 2 {
		t.Fatalf("stored gate = %+v", g)
	}
	if o := g.Opinions[0]; o.Login != "zhuravel" || o.State != "CHANGES_REQUESTED" || o.CommitSHA != "old" {
		t.Errorf("first opinion = %+v", o)
	}
	if o := g.Opinions[1]; o.Login != "helper[bot]" || o.State != "APPROVED" {
		t.Errorf("a bot's opinion = %+v", o)
	}
}

// A PR magnum approved that GitHub still blocks on the operator toasts once
// per PR and head, with its link: "needs your approval" while a review that
// counts is required, "lift your changes request" while the operator's own
// is the only one blocking it. A PR with someone else's changes request
// stays quiet, a restarted daemon does not toast the same head again, and a
// new head magnum approves toasts anew.
func TestAPRThatNeedsTheOperatorToastsOncePerHead(t *testing.T) {
	h := newHarness(t, noEveryReview, approves)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	specs := []prSpec{{n: 1, head: "base1"}, {n: 2, head: "b1", gate: gateRequired}, {n: 3, head: "c1", gate: gateMine},
		{n: 4, head: "d1", gate: gateOthers}}
	h.open(specs...)
	h.tick() // queued
	reviewed := func() bool {
		for _, n := range []int{2, 3, 4} {
			if h.pr(n).State != store.PRReviewed {
				return false
			}
		}
		return true
	}
	for i := 0; i < 20 && !reviewed(); i++ {
		h.advance(5 * time.Minute)
		h.tick() // one round at a time (one slot)
	}
	for _, n := range []int{2, 3, 4} {
		h.wantState(n, store.PRReviewed)
	}
	h.flush()
	if n := len(h.nh.shown()); n != 2 {
		t.Fatalf("toasts = %q, want #2's and #3's", h.nh.shown())
	}
	got := strings.Join(h.nh.shown(), "\n")
	for _, want := range []string{"magnum: talkable#2 needs your approval | https://github.com/talkable/talkable/pull/2\nmagnum approved b1;",
		"magnum: talkable#3: lift your changes request | https://github.com/talkable/talkable/pull/3\nmagnum approved c1;"} {
		if !strings.Contains(got, want) {
			t.Errorf("toasts lack %q: %q", want, got)
		}
	}
	if strings.Contains(got, "#4") {
		t.Errorf("someone else's changes request toasted: %q", got)
	}

	h.flush()
	h.flush()
	if n := len(h.nh.shown()); n != 2 {
		t.Fatalf("toasted again: %q", h.nh.shown())
	}

	// A restarted daemon has no memory of what it toasted: the registry's
	// dedupe keeps the same heads quiet.
	h.e = New(h.d)
	h.startup()
	h.tick()
	h.flush()
	if n := len(h.nh.shown()); n != 2 {
		t.Fatalf("the restart toasted again: %q", h.nh.shown())
	}

	// A push magnum approves again toasts the new head.
	h.advance(time.Minute)
	specs[1] = prSpec{n: 2, head: "b2", gate: gateRequired}
	h.open(specs...)
	h.tick()
	for i := 0; i < 20 && deref(h.pr(2).ReviewedSHA) != "b2"; i++ {
		h.advance(5 * time.Minute)
		h.tick()
	}
	if pr := h.wantState(2, store.PRReviewed); pr.HeadSHA != "b2" || deref(pr.ReviewedSHA) != "b2" {
		t.Fatalf("#2 after the push: head %s reviewed %s", pr.HeadSHA, deref(pr.ReviewedSHA))
	}
	h.flush() // the tick that saw the approval queued the toast
	h.flush()
	again := toastsWith(h, "talkable#2 needs your approval")
	if len(again) != 2 || !strings.Contains(again[1], "b2") || !strings.Contains(again[1], "https://github.com/talkable/talkable/pull/2") {
		t.Fatalf("new head toasts = %q", again)
	}
}
