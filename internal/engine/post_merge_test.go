package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// Post-merge review: `magnum review` of a PR GitHub merged before magnum
// reviewed its last push runs a round on the merged head, posts a comment
// only and puts the PR back in closed, where the close grace releases it.

// withRepoVerdicts gives talkable/talkable a [[repo]] whose events are
// APPROVE and REQUEST_CHANGES: what a round on an open PR would post.
func withRepoVerdicts(h *harness) {
	h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", NoFindingsEvent: "APPROVE", BlockingEvent: "REQUEST_CHANGES"})
}

// mergedBeforeReReview is PR #2 reviewed at muReviewedHead, pushed to
// muMergedHead and merged before that re-review ran: closed and flagged
// merged unreviewed, its slot held for the close grace.
func (h *harness) mergedBeforeReReview() store.PR {
	h.t.Helper()
	h.reviewedThenPushed()
	h.advance(time.Minute)
	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})
	pr := h.pr(2)
	if !pr.MergedUnreviewed() || pr.GHState != store.GHMerged {
		h.t.Fatalf("PR #2 is not merged unreviewed: %+v", pr)
	}
	return pr
}

// requestPostMerge asks for a review of PR #n and returns the request after
// one tick.
func (h *harness) requestPostMerge(n int, p ReviewPayload) store.Request {
	h.t.Helper()
	p.PRTarget = PRTarget{Ref: itoa(int64(n))}
	id := h.enqueue(ReqReview, p)
	h.tick()
	return h.request(id)
}

// The whole path: a review request on a merged, rereview_pending PR runs a
// post-merge re-review from the reviewed commit to the merged head (the
// pipeline is told so, the panes' events say COMMENT although the
// repository says APPROVE and REQUEST_CHANGES), dismisses nothing, records
// the review on the merged head (which clears merged unreviewed) and leaves
// the PR closed, released after the close grace.
func TestPostMergeReviewOfAPRMergedBeforeItsReReview(t *testing.T) {
	h := newHarness(t, withRepoVerdicts, func(h *harness) {
		for i := range h.cfg.Identities {
			if h.cfg.Identities[i].Name == "zhuravel" {
				h.cfg.Identities[i].DismissOwnStale = store.Ptr(true)
			}
		}
	})
	pr := h.mergedBeforeReReview()
	// A former identity's change request, which a round on an open PR
	// dismisses (dismissFormer) once its review is posted.
	h.e.setKV(h.ctx, KVPRFormerIdentities(pr.ID), `["zhuravel"]`)
	h.gh.mu.Lock()
	h.gh.allReviews = map[int][]github.Review{2: {{DatabaseID: 41, State: "CHANGES_REQUESTED", AuthorLogin: "zhuravel", AuthorType: "User",
		CommitOid: muReviewedHead, Body: "**Verdict** changes requested"}}}
	h.gh.mu.Unlock()
	before := len(h.rd.all())

	r := h.requestPostMerge(2, ReviewPayload{})
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "post-merge review 1111111 → 2222222 (comment only)") {
		t.Fatalf("request: %s %q", r.State, deref(r.Result))
	}
	rounds := h.rd.all()[before:]
	if len(rounds) != 1 {
		t.Fatalf("rounds = %d, want 1", len(rounds))
	}
	in := rounds[0]
	if !in.PostMerge || in.Kind != pipeline.KindRereview || in.TargetSHA != muMergedHead || in.MaxRestarts != 0 {
		t.Fatalf("round input: post_merge %v kind %s target %s max_restarts %d", in.PostMerge, in.Kind, in.TargetSHA, in.MaxRestarts)
	}
	if in.Previous == nil || in.Previous.SHA != muReviewedHead {
		t.Fatalf("previous review = %+v, want the review of %s", in.Previous, muReviewedHead)
	}
	// The events the judge's <magnum> block names for this round input (the
	// skill reads them there): COMMENT both ways, though the repository says
	// APPROVE and REQUEST_CHANGES.
	id := h.cfg.IdentityByName(in.PR.Identity)
	if nf, be := pipeline.JudgeEvents(h.cfg, in.Repo.FullName(), id, in.PostMerge); nf != "COMMENT" || be != "COMMENT" {
		t.Fatalf("judge events %s/%s, want COMMENT/COMMENT", nf, be)
	}
	if nf, be := pipeline.JudgeEvents(h.cfg, in.Repo.FullName(), id, false); nf != "APPROVE" || be != "REQUEST_CHANGES" {
		t.Fatalf("control: an open PR's judge events %s/%s, want the repository's APPROVE/REQUEST_CHANGES", nf, be)
	}

	cur := h.wantState(2, store.PRClosed)
	if deref(cur.ReviewedSHA) != muMergedHead || cur.Forced || cur.GHState != store.GHMerged || deref(cur.PrevState) != store.PRRereviewPending {
		t.Fatalf("after the round: reviewed %s forced %v gh %s prev %s", deref(cur.ReviewedSHA), cur.Forced, cur.GHState, deref(cur.PrevState))
	}
	if cur.MergedUnreviewed() {
		t.Fatal("still merged unreviewed after the post-merge review")
	}
	if cur.ReleaseAfter == nil || !cur.ReleaseAfter.Equal(h.clock.Now().Add(10*time.Minute)) {
		t.Fatalf("release_after = %v, want a fresh close grace", cur.ReleaseAfter)
	}
	if n := h.gh.count("dismiss:"); n != 0 {
		t.Fatalf("dismissals = %d, want none: %v", n, callsWith(h.gh, "dismiss:"))
	}
	if !h.hasEvent("pr:talkable/talkable#2", "pr.post_merge_reviewed") {
		t.Fatal("no pr.post_merge_reviewed event")
	}
	if sl := h.slot("review1"); deref(sl.PRID) != pr.ID || sl.State != store.SlotHeld {
		t.Fatalf("slot during the grace: %s pr=%v", sl.State, sl.PRID)
	}

	h.advance(10*time.Minute + time.Second)
	h.tick()
	h.wantState(2, store.PRReleased)
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("slot after the grace: %s pr=%v", sl.State, sl.PRID)
	}
}

// A post-merge review GitHub verified as something other than a comment
// (the judge ignored post_merge) is kept as posted, with a warning naming
// the event: nothing is undone.
func TestPostMergeReviewPostedAsAVerdictIsFlagged(t *testing.T) {
	h := newHarness(t)
	h.mergedBeforeReReview()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 2, ReviewID: 777, Event: "APPROVED", ReviewCommit: in.TargetSHA}, nil
	}
	if r := h.requestPostMerge(2, ReviewPayload{}); r.State != store.RequestDone {
		t.Fatalf("request: %s %q", r.State, deref(r.Result))
	}
	cur := h.wantState(2, store.PRClosed)
	if deref(cur.ReviewedSHA) != muMergedHead || deref(cur.LastReviewEvent) != "APPROVED" {
		t.Fatalf("reviewed %s event %s, want the posted review kept", deref(cur.ReviewedSHA), deref(cur.LastReviewEvent))
	}
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(evs, func(e store.Event) bool { return e.Kind == "pr.post_merge_event" })
	if i < 0 || evs[i].Level != "warn" || !strings.Contains(evs[i].Message, "APPROVED") {
		t.Fatalf("want a pr.post_merge_event warning naming APPROVED: %+v", evs)
	}

	// A comment raises none.
	h2 := newHarness(t)
	h2.mergedBeforeReReview()
	if r := h2.requestPostMerge(2, ReviewPayload{}); r.State != store.RequestDone {
		t.Fatalf("request: %s %q", r.State, deref(r.Result))
	}
	if h2.hasEvent("pr:talkable/talkable#2", "pr.post_merge_event") {
		t.Fatal("a COMMENTED post-merge review raised pr.post_merge_event")
	}
}

// A merged PR already released claims a pool slot again for its post-merge
// round, and the release follows the round's close grace again.
func TestPostMergeReviewOfAReleasedPRClaimsASlotAndReleasesIt(t *testing.T) {
	h := newHarness(t)
	pr := h.mergedBeforeReReview()
	h.advance(10*time.Minute + time.Second)
	h.tick()
	h.wantState(2, store.PRReleased)

	r := h.requestPostMerge(2, ReviewPayload{})
	if r.State != store.RequestDone {
		t.Fatalf("request: %s %q", r.State, deref(r.Result))
	}
	if claims := slices.DeleteFunc(h.sl.all(), func(c string) bool { return c != "claim:"+itoa(pr.ID) }); len(claims) != 2 {
		t.Fatalf("slot calls %v: want a second claim for the post-merge round", h.sl.all())
	}
	cur := h.wantState(2, store.PRClosed)
	if deref(cur.ReviewedSHA) != muMergedHead || cur.MergedUnreviewed() {
		t.Fatalf("reviewed %s, merged unreviewed %v", deref(cur.ReviewedSHA), cur.MergedUnreviewed())
	}
	h.advance(10*time.Minute + time.Second)
	h.tick()
	h.wantState(2, store.PRReleased)
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("slot: %s pr=%v", sl.State, sl.PRID)
	}
}

// The poller may never have seen the last push before the merge: the round
// reviews the merged head its checkout of refs/pull/N/head found, and the
// registry's head follows it, so the flag clears on that head too.
func TestPostMergeReviewRecordsTheMergedHeadTheCheckoutFound(t *testing.T) {
	const fetched = "3333333cccccccc"
	h := newHarness(t)
	h.mergedBeforeReReview()
	h.sl.moveHead = fetched
	before := len(h.rd.all())
	if r := h.requestPostMerge(2, ReviewPayload{}); r.State != store.RequestDone {
		t.Fatalf("request: %s %q", r.State, deref(r.Result))
	}
	if rounds := h.rd.all()[before:]; len(rounds) != 1 || rounds[0].TargetSHA != fetched {
		t.Fatalf("rounds: %+v", rounds)
	}
	cur := h.wantState(2, store.PRClosed)
	if cur.HeadSHA != fetched || deref(cur.ReviewedSHA) != fetched || cur.MergedUnreviewed() {
		t.Fatalf("head %s reviewed %s merged unreviewed %v, want both %s", cur.HeadSHA, deref(cur.ReviewedSHA), cur.MergedUnreviewed(), fetched)
	}
}

// A PR merged before its first review gets a first review of the whole PR.
func TestPostMergeReviewOfANeverReviewedPRIsAFirstReview(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, muMergedHead)
	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})
	before := len(h.rd.all())

	if r := h.requestPostMerge(2, ReviewPayload{}); r.State != store.RequestDone {
		t.Fatalf("request: %s %q", r.State, deref(r.Result))
	}
	rounds := h.rd.all()[before:]
	if len(rounds) != 1 || !rounds[0].PostMerge || rounds[0].Kind != pipeline.KindInitial || rounds[0].Previous != nil {
		t.Fatalf("rounds: %+v", rounds)
	}
	cur := h.wantState(2, store.PRClosed)
	if deref(cur.ReviewedSHA) != muMergedHead || cur.MergedUnreviewed() {
		t.Fatalf("reviewed %s, merged unreviewed %v", deref(cur.ReviewedSHA), cur.MergedUnreviewed())
	}
}

// What a post-merge review refuses: a PR closed without merging, a merged
// PR whose merged head magnum reviewed, and one whose slot is being
// released. Nothing runs and the PR stays as it was.
func TestPostMergeReviewRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness)
		state string
		want  string
	}{
		{"closed without merging", func(h *harness) {
			h.reviewedThenPushed()
			h.advance(time.Minute)
			h.closeOnGitHub(2, "CLOSED", prSpec{n: 1, head: "base1"})
		}, store.PRClosed, "talkable/talkable#2 was closed without merging (closed, GitHub CLOSED): only open or merged PRs are reviewed"},
		{"merged head already reviewed", func(h *harness) {
			h.reviewedPR(2, muReviewedHead)
			h.advance(time.Minute)
			h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})
		}, store.PRClosed, "talkable/talkable#2: its merged head 1111111 was already reviewed"},
		{"being released", func(h *harness) {
			pr := h.mergedBeforeReReview()
			if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRClosed}, store.PRReleasing, nil); err != nil {
				h.t.Fatal(err)
			}
		}, store.PRReleased, // the release the request gave way to went on in the same tick
			"talkable/talkable#2: its checkout is being released; run `magnum review` again in a minute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)
			before := len(h.rd.all())
			r := h.requestPostMerge(2, ReviewPayload{})
			if r.State != store.RequestFailed || deref(r.Result) != tc.want {
				t.Fatalf("request: %s %q, want failed %q", r.State, deref(r.Result), tc.want)
			}
			if n := len(h.rd.all()) - before; n != 0 {
				t.Fatalf("%d rounds ran", n)
			}
			if cur := h.wantState(2, tc.state); cur.Forced {
				t.Fatal("refused PR was forced")
			}
		})
	}
}

// A review request that wins the race against a release cleanup planned
// for the closed PR (past its grace) stops that release before any side
// effect: the request's closed → rereview_pending and the release's closed →
// releasing are both compare-and-set, and the release parks nothing before
// its own. The post-merge round then runs in the PR's own slot.
func TestPostMergeReviewRequestWinsTheRaceAgainstARelease(t *testing.T) {
	h := newHarness(t)
	pr := h.mergedBeforeReReview()
	h.advance(10*time.Minute + time.Second) // past the grace: the default cleanup releases it
	plan, err := h.d.Cleanup.Plan(h.ctx, cleanup.Options{})
	if err != nil || !slices.ContainsFunc(plan.Actions, func(a cleanup.Action) bool { return a.PRID == pr.ID }) {
		t.Fatalf("plan %+v (%v): want the release of PR #2", plan.Actions, err)
	}
	if _, err := h.e.requestReview(h.ctx, ReviewPayload{PRTarget: PRTarget{Ref: "2"}}); err != nil {
		t.Fatal(err)
	}
	parks := h.ag.count(fmt.Sprintf("park:%d", pr.ID))
	rep, err := h.d.Cleanup.Apply(h.ctx, plan, false)
	if !errors.Is(err, store.ErrConflict) || rep.Done != 0 {
		t.Fatalf("apply = %+v, %v: want the release stopped", rep, err)
	}
	if n := h.ag.count(fmt.Sprintf("park:%d", pr.ID)); n != parks {
		t.Fatalf("the stopped release parked the sessions (%d parks, %d before)", n, parks)
	}
	if sl := h.slot("review1"); deref(sl.PRID) != pr.ID || sl.State != store.SlotHeld {
		t.Fatalf("slot after the stopped release: %s pr=%v", sl.State, sl.PRID)
	}
	before := len(h.rd.all())
	h.tick()
	if rounds := h.rd.all()[before:]; len(rounds) != 1 || !rounds[0].PostMerge {
		t.Fatalf("rounds after the request: %+v", rounds)
	}
	if cur := h.wantState(2, store.PRClosed); deref(cur.ReviewedSHA) != muMergedHead {
		t.Fatalf("reviewed %s", deref(cur.ReviewedSHA))
	}
}

// A daemon restart in the middle of a post-merge round: recovery puts the
// merged PR back in line (before the judge was prompted) or pauses it
// (during the judge's turn), the new daemon dispatches it again as a
// post-merge round (a continue of the judge's turn in the second case), and
// it ends closed with the merged head reviewed.
func TestPostMergeRoundSurvivesADaemonRestart(t *testing.T) {
	for _, prompted := range []bool{false, true} {
		h := newHarness(t)
		pr := h.mergedBeforeReReview()
		now := h.clock.Now()
		if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRClosed}, store.PRReviewing, func(u *store.PRUpdate) {
			u.Set("forced", true)
			u.Set("last_round_started_at", now)
		}); err != nil {
			t.Fatal(err)
		}
		var run store.Run
		if prompted {
			run = h.judgeRun(h.pr(2), pipeline.KindRereview, muMergedHead, store.RunSubmitted, "", "")
		}

		h.e = New(h.d) // a new daemon process
		h.startup()
		want := store.PRRereviewPending
		if prompted {
			want = store.PRPaused
		}
		if cur := h.wantState(2, want); !cur.Forced {
			t.Fatalf("prompted %v: recovery lost forced", prompted)
		}
		before := len(h.rd.all())
		h.tick()
		rounds := h.rd.all()[before:]
		if len(rounds) != 1 || !rounds[0].PostMerge {
			t.Fatalf("prompted %v: rounds after the restart %+v", prompted, rounds)
		}
		if prompted && (rounds[0].Kind != pipeline.KindContinue || rounds[0].ContinueRunID != run.ID) {
			t.Fatalf("the judge's turn did not continue: kind %s run %q", rounds[0].Kind, rounds[0].ContinueRunID)
		}
		if cur := h.wantState(2, store.PRClosed); deref(cur.ReviewedSHA) != muMergedHead || cur.Forced {
			t.Fatalf("prompted %v: reviewed %s forced %v", prompted, deref(cur.ReviewedSHA), cur.Forced)
		}
	}
}

// A post-merge round whose charged retries run out (three failed rounds on
// the merged head) ends where an open PR would need attention: back in
// closed, through postMergeFailed, and the release follows.
func TestPostMergeRoundOutOfRetriesReturnsThePRToClosed(t *testing.T) {
	h := newHarness(t)
	h.mergedBeforeReReview()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeTimeout, Round: 2, Error: "no result within judge_timeout"}, nil
	}
	before := len(h.rd.all())
	if r := h.requestPostMerge(2, ReviewPayload{}); r.State != store.RequestDone {
		t.Fatalf("request: %s %q", r.State, deref(r.Result))
	}
	for attempt := 1; attempt < maxAttempts; attempt++ {
		cur := h.wantState(2, store.PRRereviewPending)
		if !cur.Forced || cur.Attempts != attempt || cur.NextAttemptAt == nil {
			t.Fatalf("after attempt %d: forced %v attempts %d next %v", attempt, cur.Forced, cur.Attempts, cur.NextAttemptAt)
		}
		h.advance(cur.NextAttemptAt.Sub(h.clock.Now()) + time.Second)
		h.tick()
	}
	rounds := h.rd.all()[before:]
	if len(rounds) != maxAttempts || slices.ContainsFunc(rounds, func(in pipeline.RoundInput) bool { return !in.PostMerge }) {
		t.Fatalf("rounds = %d (want %d, all post-merge)", len(rounds), maxAttempts)
	}
	cur := h.wantState(2, store.PRClosed)
	if cur.Forced || cur.Attempts != maxAttempts || !strings.Contains(deref(cur.LastError), "3 attempts") {
		t.Fatalf("forced %v attempts %d last_error %q", cur.Forced, cur.Attempts, deref(cur.LastError))
	}
	if !h.hasEvent("pr:talkable/talkable#2", "pr.post_merge_failed") || h.hasEvent("pr:talkable/talkable#2", "pr.needs_attention") {
		t.Fatal("want pr.post_merge_failed and no pr.needs_attention")
	}
	h.advance(10*time.Minute + time.Second)
	h.tick()
	h.wantState(2, store.PRReleased)
}

// magnum abort of a running post-merge round, end to end: the round stops,
// its pool slot is handed back, the merged PR goes back to closed (not
// reviewed or baseline, which a merged PR would never leave) and the
// slot-less release follows.
func TestAbortOfARunningPostMergeRound(t *testing.T) {
	h := newHarness(t)
	h.mergedBeforeReReview()
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	before := len(h.rd.all())
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	if r := h.request(review); r.State != store.RequestDone {
		t.Fatalf("review request: %s %q", r.State, deref(r.Result))
	}
	pr := h.wantState(2, store.PRReviewing)

	abort := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	r := h.request(abort)
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "aborted talkable/talkable#2") || !strings.Contains(deref(r.Result), "PR closed") {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	cur := h.wantState(2, store.PRClosed)
	if cur.Forced || deref(cur.ReviewedSHA) != muReviewedHead || !cur.MergedUnreviewed() {
		t.Fatalf("after the abort: forced %v reviewed %s", cur.Forced, deref(cur.ReviewedSHA))
	}
	if h.e.roundActive(pr.ID) {
		t.Fatal("still reserved")
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("slot not handed back: %s pr=%v", sl.State, sl.PRID)
	}
	h.advance(10*time.Minute + time.Second)
	h.tick()
	h.wantState(2, store.PRReleased)
}

// A post-merge round paused by an overloaded model continues once its
// backoff passed, like any paused round, and ends closed.
func TestPausedPostMergeRoundContinuesAndEndsClosed(t *testing.T) {
	h := newHarness(t)
	h.mergedBeforeReReview()
	calls := 0
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		calls++
		if calls == 1 {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeOverloaded, Round: 2, Error: "API overloaded"}, nil
		}
		return h.rd.posted(context.Background(), in, calls)
	}
	before := len(h.rd.all())
	if r := h.requestPostMerge(2, ReviewPayload{}); r.State != store.RequestDone {
		t.Fatalf("request: %s %q", r.State, deref(r.Result))
	}
	cur := h.wantState(2, store.PRPaused)
	if !cur.Forced || cur.NextAttemptAt == nil {
		t.Fatalf("paused: forced %v next %v", cur.Forced, cur.NextAttemptAt)
	}
	h.advance(cur.NextAttemptAt.Sub(h.clock.Now()) + time.Second)
	h.tick()
	rounds := h.rd.all()[before:]
	if len(rounds) != 2 || !rounds[1].PostMerge {
		t.Fatalf("rounds after the pause: %+v", rounds)
	}
	if cur := h.wantState(2, store.PRClosed); deref(cur.ReviewedSHA) != muMergedHead || cur.MergedUnreviewed() {
		t.Fatalf("reviewed %s", deref(cur.ReviewedSHA))
	}
}

// A post-merge round that cannot post (the judge is blocked, or reports the
// PR closed) puts the PR back in closed instead of needs_attention, which
// a merged PR would never leave: forced cleared, the reason kept, a warning
// and a toast, and the release follows.
func TestPostMergeRoundThatFailsReturnsThePRToClosed(t *testing.T) {
	for _, outcome := range []string{pipeline.OutcomeBlocked, pipeline.OutcomeClosed} {
		t.Run(outcome, func(t *testing.T) {
			h := newHarness(t)
			h.mergedBeforeReReview()
			h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
				return pipeline.RoundResult{Outcome: outcome, Round: 2, Error: "the judge stopped"}, nil
			}
			if r := h.requestPostMerge(2, ReviewPayload{}); r.State != store.RequestDone {
				t.Fatalf("request: %s %q", r.State, deref(r.Result))
			}
			cur := h.wantState(2, store.PRClosed)
			if cur.Forced || !strings.Contains(deref(cur.LastError), "the judge stopped") || deref(cur.ReviewedSHA) != muReviewedHead {
				t.Fatalf("after the failure: forced %v last_error %q reviewed %s", cur.Forced, deref(cur.LastError), deref(cur.ReviewedSHA))
			}
			if !cur.MergedUnreviewed() {
				t.Fatal("the failed round cleared merged unreviewed")
			}
			if !h.hasEvent("pr:talkable/talkable#2", "pr.post_merge_failed") || h.hasEvent("pr:talkable/talkable#2", "pr.needs_attention") {
				t.Fatal("want a pr.post_merge_failed event and no pr.needs_attention")
			}
			if toasts := h.nh.titles("post-merge review failed"); len(toasts) != 1 {
				t.Fatalf("toasts = %q, want one", h.nh.all())
			}
			h.advance(10*time.Minute + time.Second)
			h.tick()
			h.wantState(2, store.PRReleased)
		})
	}
}

// A dry-run post-merge round (--no-post) posts nothing and puts the PR back
// in closed with its review where it was.
func TestPostMergeDryRunReturnsThePRToClosed(t *testing.T) {
	h := newHarness(t)
	h.mergedBeforeReReview()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if !in.DryRun || !in.PostMerge {
			t.Errorf("round input: dry run %v post-merge %v", in.DryRun, in.PostMerge)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun, Round: 2, Event: "COMMENT"}, nil
	}
	if r := h.requestPostMerge(2, ReviewPayload{DryRun: true}); r.State != store.RequestDone {
		t.Fatalf("request: %s %q", r.State, deref(r.Result))
	}
	cur := h.wantState(2, store.PRClosed)
	if cur.Forced || deref(cur.ReviewedSHA) != muReviewedHead || !cur.MergedUnreviewed() {
		t.Fatalf("after the dry run: forced %v reviewed %s", cur.Forced, deref(cur.ReviewedSHA))
	}
}

// A post-merge round paused by a tool limit continues like any paused round
// (the dispatcher's paused PRs keep a forced merged PR), and one nobody
// forced is left alone.
func TestContinueCandidatesKeepAPausedPostMergeRound(t *testing.T) {
	h := newHarness(t)
	pr := h.mergedBeforeReReview()
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRClosed}, store.PRPaused, func(u *store.PRUpdate) { u.Set("forced", true) }); err != nil {
		t.Fatal(err)
	}
	ids := func() []int64 {
		cands, err := h.e.continueCandidates(h.ctx, h.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, c := range cands {
			out = append(out, c.ID)
		}
		return out
	}
	if got := ids(); !slices.Equal(got, []int64{pr.ID}) {
		t.Fatalf("paused candidates = %v, want the post-merge PR %d", got, pr.ID)
	}
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("forced", false) }); err != nil {
		t.Fatal(err)
	}
	if got := ids(); len(got) != 0 {
		t.Fatalf("paused candidates = %v, want none for a merged PR nobody forced", got)
	}
}

// magnum abort (and ignore) of a post-merge round puts the merged PR back
// in closed, never in reviewed or baseline, which a merged PR would never
// leave.
func TestStoppedPostMergeRoundReturnsThePRToClosed(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		h := newHarness(t)
		pr := h.mergedBeforeReReview()
		if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRClosed}, store.PRReviewing, func(u *store.PRUpdate) { u.Set("forced", true) }); err != nil {
			t.Fatal(err)
		}
		to, err := h.e.settleStopped(h.ctx, h.pr(2), ignore)
		if err != nil || to != store.PRClosed {
			t.Fatalf("ignore %v: settleStopped = %q, %v; want closed", ignore, to, err)
		}
		if cur := h.wantState(2, store.PRClosed); cur.Forced || cur.Muted != ignore {
			t.Fatalf("ignore %v: forced %v muted %v", ignore, cur.Forced, cur.Muted)
		}
	}
}

// A post-merge PR's wait says so in the cell and the sentence.
func TestWaitOfAPostMergeReview(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.Local)
	w := Wait{Reason: WaitCapacity, Rereview: true, Forced: true, PostMerge: true, Detail: "review capacity: 3 rounds running"}
	if got, want := w.Short(now), "post-merge review · capacity"; got != want {
		t.Errorf("Short = %q, want %q", got, want)
	}
	if got, want := w.Sentence("talkable#2", now), "forced post-merge review waits: review capacity: 3 rounds running"; got != want {
		t.Errorf("Sentence = %q, want %q", got, want)
	}
	b, err := json.Marshal(Wait{Reason: WaitNext})
	if err != nil || strings.Contains(string(b), "post_merge") {
		t.Errorf("an open PR's wait names post_merge: %s %v", b, err)
	}
}

// noteWaits records the post-merge kind for a merged PR waiting for its round.
func TestNoteWaitsMarksAPostMergePR(t *testing.T) {
	h := newHarness(t)
	pr := h.mergedBeforeReReview()
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRClosed}, store.PRRereviewPending, func(u *store.PRUpdate) { u.Set("forced", true) }); err != nil {
		t.Fatal(err)
	}
	w := h.e.waitFor(h.ctx, h.pr(2), nil, h.clock.Now())
	if !w.PostMerge || !strings.HasPrefix(w.Short(h.clock.Now()), "post-merge review · ") {
		t.Fatalf("wait = %+v (%q)", w, w.Short(h.clock.Now()))
	}
}

// ---- the base a post-merge round reviews from ----

// graphGit is a fake git over a commit graph: parents by commit (local
// ones, and the remote's that FetchCommit brings in), refs by name.
// MergeBase walks the graph; RevParse resolves a commit, a ref or
// "<commit>^1".
type graphGit struct {
	fakeGit
	mu      sync.Mutex
	parents map[string][]string // the clone's commits
	remote  map[string][]string // commits only the remote has until fetched
	refs    map[string]string
	fetched []string // FetchCommit calls: "<clone>:<sha>"
}

func (g *graphGit) resolve(ref string) (string, bool) {
	if sha, ok := g.refs[ref]; ok {
		ref = sha
	}
	_, ok := g.parents[ref]
	return ref, ok
}

func (g *graphGit) RevParse(_ context.Context, _, ref string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	base, parent := strings.CutSuffix(ref, "^1")
	sha, ok := g.resolve(base)
	switch {
	case !ok:
		return "", fmt.Errorf("rev-parse %s: %w", ref, gitx.ErrNoSuchRef)
	case parent:
		if ps := g.parents[sha]; len(ps) > 0 {
			return ps[0], nil
		}
		return "", fmt.Errorf("rev-parse %s: %w", ref, gitx.ErrNoSuchRef)
	}
	return sha, nil
}

func (g *graphGit) MergeBase(_ context.Context, _, a, b string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, okA := g.resolve(a)
	b, okB := g.resolve(b)
	if !okA || !okB {
		return "", fmt.Errorf("merge-base %s %s: unknown commit", a, b)
	}
	ancestors := map[string]bool{}
	for queue := []string{a}; len(queue) > 0; queue = queue[1:] {
		if !ancestors[queue[0]] {
			ancestors[queue[0]] = true
			queue = append(queue, g.parents[queue[0]]...)
		}
	}
	seen := map[string]bool{}
	for queue := []string{b}; len(queue) > 0; queue = queue[1:] {
		c := queue[0]
		if ancestors[c] {
			return c, nil
		}
		if !seen[c] {
			seen[c] = true
			queue = append(queue, g.parents[c]...)
		}
	}
	return "", fmt.Errorf("merge-base %s %s: %w", a, b, gitx.ErrNoMergeBase)
}

func (g *graphGit) FetchCommit(_ context.Context, clone, sha string, _ int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fetched = append(g.fetched, clone+":"+sha)
	ps, ok := g.remote[sha]
	if !ok {
		return fmt.Errorf("fetch %s: not on the remote", sha)
	}
	g.parents[sha] = ps
	return nil
}

// The commits of the merge-base tests: the base branch m1 ← m2, the PR
// branched from m1 (muReviewedHead ← muMergedHead), merged in one of
// GitHub's three ways after m2.
const (
	pmBase1    = "m1m1m1m1"                                 // the PR's fork point: the base its diff starts from
	pmBase2    = "m2m2m2m2"                                 // the base branch's tip when the PR merged
	pmMerge    = "4444444444444444444444444444444444444444" // a merge commit: m2 + the head
	pmSquash   = "5555555555555555555555555555555555555555" // a squash commit on m2
	pmRebase1  = "6666666666666666666666666666666666666661" // the rebased commits on m2
	pmRebase2  = "6666666666666666666666666666666666666662"
	pmFallback = fakeBaseOid // the base tip the last Details fetch recorded (base_sha)
)

// pmGraph is the base branch and the PR's commits, the base-tip the harness'
// Details report, and the merge commits of the three merge methods (local,
// or remote only).
func pmGraph(local bool) *graphGit {
	g := &graphGit{
		parents: map[string][]string{
			pmBase1: nil, pmBase2: {pmBase1}, pmFallback: {pmBase1},
			muReviewedHead: {pmBase1}, muMergedHead: {muReviewedHead},
		},
		remote: map[string][]string{
			pmMerge: {pmBase2, muMergedHead}, pmSquash: {pmBase2}, pmRebase1: {pmBase2}, pmRebase2: {pmRebase1},
		},
		refs: map[string]string{},
	}
	if local {
		maps.Copy(g.parents, g.remote)
	}
	return g
}

// A post-merge round reviews from the PR's fork point whatever the merge
// method: the base branch before the merge is the first parent of GitHub's
// merge commit (fetched when the clone lacks it). After a merge-commit merge
// origin/<base> holds the head, and its merge base with the head is the
// head itself: every diff would be empty. Without a merge commit, the base
// tip the last Details fetch recorded stands in.
func TestPostMergeRoundReviewsFromTheBaseBeforeTheMerge(t *testing.T) {
	cases := []struct {
		name, mergeCommit, origin string
		local                     bool
	}{
		{"merge commit", pmMerge, pmMerge, true},
		{"squash", pmSquash, pmSquash, true},
		{"rebase", pmRebase2, pmRebase2, true},
		{"merge commit not fetched yet", pmMerge, pmBase2, false},
		{"no merge commit from GitHub", "", pmMerge, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := pmGraph(tc.local)
			g.refs["origin/master"] = tc.origin
			h := newHarness(t, func(h *harness) { h.d.Git = g })
			h.mergedBeforeReReview()
			if tc.mergeCommit != "" {
				h.gh.mu.Lock()
				h.gh.mergeCommits = map[int]string{2: tc.mergeCommit}
				h.gh.mu.Unlock()
			}
			before := len(h.rd.all())
			if r := h.requestPostMerge(2, ReviewPayload{}); r.State != store.RequestDone {
				t.Fatalf("request: %s %q", r.State, deref(r.Result))
			}
			rounds := h.rd.all()[before:]
			if len(rounds) != 1 || rounds[0].BaseSHA != pmBase1 {
				t.Fatalf("base %q, want the fork point %s (rounds %d)", rounds[0].BaseSHA, pmBase1, len(rounds))
			}
			g.mu.Lock()
			fetched := slices.Clone(g.fetched)
			g.mu.Unlock()
			if want := !tc.local && tc.mergeCommit != ""; want != (len(fetched) == 1) {
				t.Fatalf("fetches %v (want one: %v)", fetched, want)
			}
			h.wantState(2, store.PRClosed)
		})
	}
}
