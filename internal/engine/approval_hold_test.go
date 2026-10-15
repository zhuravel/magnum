package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// planVerdicts makes round n post the verdict plan[n-1] (the last one for
// later rounds), its judge run recorded as posted with its result, as the
// pipeline leaves it: a blocking one requests changes, a clean one
// approves, or comments when its input holds the approval, as its judge's
// events then say.
func planVerdicts(h *harness, plan ...string) {
	h.rd.mu.Lock()
	defer h.rd.mu.Unlock()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		n := len(h.rd.all())
		res, err := h.rd.posted(h.ctx, in, n)
		if err != nil {
			return res, err
		}
		verdict := plan[min(n, len(plan))-1]
		res.Event, res.Findings = "CHANGES_REQUESTED", map[string]int{"P1": 1}
		if verdict == store.VerdictClean {
			res.Event, res.Findings = "APPROVED", map[string]int{}
			if in.HoldApproval {
				res.Event = "COMMENTED"
			}
		}
		result, err := json.Marshal(map[string]any{"event": res.Event, "verdict": verdict, "findings": res.Findings})
		if err == nil {
			err = h.st.UpdateRun(h.ctx, res.JudgeRunID, func(u *store.RunUpdate) {
				u.Set("outcome", pipeline.OutcomePosted)
				u.Set("review_event", res.Event)
				u.Set("result_json", string(result))
			})
		}
		if err != nil {
			return failRound(h.t, err)
		}
		return res, nil
	}
}

// holdHarness is the full-pass harness with no scheduled full pass, whose
// rounds post plan (planVerdicts).
func holdHarness(t *testing.T, plan []string, mods ...func(*harness)) *harness {
	t.Helper()
	h := fullPassHarness(t, 0, mods...)
	planVerdicts(h, plan...)
	return h
}

// pushHeld pushes head to PR #n and runs its re-review once the push's
// quiet period is over, which the approval hold leaves waiting for its
// full pass on that head: forced, with a full pass asked for. It returns
// the round's input.
func pushHeld(t *testing.T, h *harness, n int, head string) pipeline.RoundInput {
	t.Helper()
	before := len(h.rd.all())
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != before+1 {
		t.Fatalf("rounds = %d after the push of %s, want %d", len(ins), head, before+1)
	}
	pr := h.wantState(n, store.PRRereviewPending)
	if deref(pr.ReviewedSHA) != head || deref(pr.LastReviewEvent) != "COMMENTED" || !pr.Forced {
		t.Fatalf("after the held re-review of %s: reviewed %q, event %q, forced %v: want its comment on %s and the full pass forced",
			head, deref(pr.ReviewedSHA), deref(pr.LastReviewEvent), pr.Forced, head)
	}
	if v, _ := kvValue(h, store.KVPRFresh(pr.ID)); v != store.FreshFullPass {
		t.Fatalf("%s = %q: want a full pass asked for", store.KVPRFresh(pr.ID), v)
	}
	return ins[before]
}

// wantHeld fails unless in is a re-review that holds its approval, and no
// full pass.
func wantHeld(t *testing.T, label string, in pipeline.RoundInput) {
	t.Helper()
	if !in.HoldApproval || in.FullPass {
		t.Fatalf("%s: hold %v, full pass %v: want a re-review that cannot approve", label, in.HoldApproval, in.FullPass)
	}
}

// wantApproves fails unless in may approve.
func wantApproves(t *testing.T, label string, in pipeline.RoundInput) {
	t.Helper()
	if in.HoldApproval {
		t.Fatalf("%s holds its approval: want it to approve on its own", label)
	}
}

// wantNoMoreRounds fails if a tick a minute later starts a round on PR n.
func wantNoMoreRounds(t *testing.T, h *harness, n int) {
	t.Helper()
	before := len(h.rd.all())
	h.advance(time.Minute)
	h.tick()
	if got := len(h.rd.all()); got != before {
		t.Fatalf("rounds = %d, want %d: nothing more to run", got, before)
	}
	if pr := h.wantState(n, store.PRReviewed); pr.Forced {
		t.Fatal("the PR stays forced")
	}
}

// An approval rests on a whole read of the PR. A first review, two
// re-reviews with findings, then a third re-review whose judge finds
// nothing: approve_after_full_pass (2) re-reviews since the first review
// hold its approval, so its clean verdict posts as a comment, and the next
// dispatch runs a full pass of the same head with every role fresh, its
// reason the approval, whose clean verdict approves. After it, a push and a
// clean re-review approve on their delta: the count starts at the full pass.
func TestAnApprovalAfterTwoReReviewsWaitsForAFullPass(t *testing.T) {
	h := holdHarness(t, []string{store.VerdictBlocking, store.VerdictBlocking, store.VerdictBlocking, store.VerdictClean})
	h.reviewedPR(2, "b1")
	wantApproves(t, "re-review 1", pushAndRereview(t, h, "b2"))
	wantApproves(t, "re-review 2", pushAndRereview(t, h, "b3"))

	held := pushHeld(t, h, 2, "b4")
	wantHeld(t, "re-review 3", held)
	if held.Kind != pipeline.KindRereview {
		t.Fatalf("the held re-review ran as %s, want a re-review in the judge's conversation", held.Kind)
	}
	pr := h.pr(2)
	start := approvalEvents(t, h, 2, "engine.round_start")
	if d := eventData(t, start[len(start)-1]); d["approval_hold"] != true {
		t.Fatalf("round start %s: want approval_hold", start[len(start)-1].Data)
	}
	why := approvalEvents(t, h, 2, "round.approval_hold")
	if len(why) != 1 || why[0].Message != "approval held: 2 re-reviews since the first review (approve_after_full_pass 2): "+
		"a clean verdict posts as a comment, and a full pass of the whole PR follows before the approval" {
		t.Fatalf("round.approval_hold events: %+v", why)
	}
	queued := approvalEvents(t, h, 2, "pr.approval_full_pass")
	if len(queued) != 1 || !strings.Contains(queued[0].Message, "the clean re-review of b4 posted as a comment; a full pass of the whole PR on b4 follows before approval") {
		t.Fatalf("pr.approval_full_pass events: %+v", queued)
	}

	parks, starts := h.ag.count(fmt.Sprintf("park:%d", pr.ID)), len(agentStarts(h))
	h.advance(time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 5 {
		t.Fatalf("rounds = %d, want the full pass after the held re-review", len(ins))
	}
	full := ins[4]
	wantFullPass(t, "the full pass before the approval", full)
	wantApproves(t, "the full pass", full)
	if full.TargetSHA != "b4" {
		t.Fatalf("the full pass reviewed %s, want the held head b4", full.TargetSHA)
	}
	if h.ag.count(fmt.Sprintf("park:%d", pr.ID)) == parks {
		t.Fatalf("calls %v: want the sessions parked before the full pass", h.ag.all())
	}
	for _, s := range agentStarts(h)[starts:] {
		if !strings.HasSuffix(s, ":") {
			t.Errorf("start %s resumed a conversation: the full pass starts every role fresh", s)
		}
	}
	reason, rule, startReason := fullPassWhyOf(t, h, 2)
	if rule != fullPassApproval || !strings.HasPrefix(reason, "before approval: ") || startReason != reason {
		t.Fatalf("full pass reason %q, rule %q, start %q: want the approval", reason, rule, startReason)
	}
	start = approvalEvents(t, h, 2, "engine.round_start")
	if d := eventData(t, start[len(start)-1]); d["before_approval"] != true || d["approval_hold"] == true {
		t.Fatalf("round start %s: want before_approval and no hold", start[len(start)-1].Data)
	}
	pr = h.wantState(2, store.PRReviewed)
	if deref(pr.LastReviewEvent) != "APPROVED" || deref(pr.ReviewedSHA) != "b4" {
		t.Fatalf("after the full pass: event %q on %q, want APPROVED on b4", deref(pr.LastReviewEvent), deref(pr.ReviewedSHA))
	}
	if rec := lastFullPassOf(t, h, 2); rec.Round != 5 || rec.Target != "b4" {
		t.Fatalf("full pass record %+v: want round 5 on b4", rec)
	}
	if _, ok := kvValue(h, kvPRApprovalHold(pr.ID)); ok {
		t.Error("the held review's record outlived its full pass")
	}
	wantNoMoreRounds(t, h, 2)

	wantApproves(t, "the re-review after the full pass", pushAndRereview(t, h, "b5"))
	if pr := h.pr(2); deref(pr.LastReviewEvent) != "APPROVED" {
		t.Fatalf("the clean re-review after the full pass posted %q, want APPROVED", deref(pr.LastReviewEvent))
	}
}

// With the default 2, the common path, one round of fixes after the first
// review, approves on its delta: the first re-review is not held and no
// full pass follows it.
func TestOneReReviewApprovesOnItsDelta(t *testing.T) {
	h := holdHarness(t, []string{store.VerdictBlocking, store.VerdictClean})
	if n := h.cfg.Pipeline.ApproveAfterFullPass; n != 2 {
		t.Fatalf("approve_after_full_pass = %d, want the default 2", n)
	}
	h.reviewedPR(2, "b1")
	wantApproves(t, "the first re-review", pushAndRereview(t, h, "b2"))
	pr := h.pr(2)
	if deref(pr.LastReviewEvent) != "APPROVED" {
		t.Fatalf("event %q, want APPROVED", deref(pr.LastReviewEvent))
	}
	if v, ok := kvValue(h, store.KVPRFresh(pr.ID)); ok {
		t.Fatalf("%s = %q: no full pass follows", store.KVPRFresh(pr.ID), v)
	}
	wantNoMoreRounds(t, h, 2)
	if evs := approvalEvents(t, h, 2, "round.approval_hold"); len(evs) != 0 {
		t.Fatalf("round.approval_hold events: %+v", evs)
	}
}

// A full pass that follows a held review and finds problems posts as
// usual, and nothing more follows: the PR's next re-review counts from the
// full pass, so it may approve on its delta.
func TestAFullPassThatFindsProblemsBlocks(t *testing.T) {
	h := holdHarness(t, []string{store.VerdictBlocking, store.VerdictBlocking, store.VerdictBlocking, store.VerdictClean,
		store.VerdictBlocking, store.VerdictClean})
	h.reviewedPR(2, "b1")
	pushAndRereview(t, h, "b2")
	pushAndRereview(t, h, "b3")
	wantHeld(t, "re-review 3", pushHeld(t, h, 2, "b4"))
	h.advance(time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 5 {
		t.Fatalf("rounds = %d, want the full pass", len(ins))
	}
	wantFullPass(t, "the full pass", ins[4])
	pr := h.wantState(2, store.PRReviewed)
	if deref(pr.LastReviewEvent) != "CHANGES_REQUESTED" {
		t.Fatalf("the full pass that found a problem posted %q, want CHANGES_REQUESTED", deref(pr.LastReviewEvent))
	}
	if _, ok := kvValue(h, kvPRApprovalHold(pr.ID)); ok {
		t.Error("the held review's record outlived its full pass")
	}
	if evs := approvalEvents(t, h, 2, "pr.approval_full_pass"); len(evs) != 1 {
		t.Fatalf("pr.approval_full_pass events: %+v: want the one of the held review only", evs)
	}
	wantNoMoreRounds(t, h, 2)
	wantApproves(t, "the re-review of the fix", pushAndRereview(t, h, "b5"))
	if pr := h.pr(2); deref(pr.LastReviewEvent) != "APPROVED" {
		t.Fatalf("the clean re-review of the fix posted %q, want APPROVED", deref(pr.LastReviewEvent))
	}
}

// approve_after_full_pass = 0 turns the hold off: the twentieth re-review
// of a PR approves on its delta, while with 2 it is held.
func TestApproveAfterFullPassOffByZero(t *testing.T) {
	for _, tc := range []struct {
		after int
		held  bool
	}{{0, false}, {2, true}} {
		t.Run(fmt.Sprintf("approve_after_full_pass %d", tc.after), func(t *testing.T) {
			h := holdHarness(t, []string{store.VerdictBlocking, store.VerdictClean}, func(h *harness) {
				h.cfg.Pipeline.ApproveAfterFullPass = tc.after
			})
			h.reviewedPR(2, "b1")
			seedRereviews(t, h, 19)
			if tc.held {
				wantHeld(t, "the twentieth re-review", pushHeld(t, h, 2, "b2"))
				return
			}
			wantApproves(t, "the twentieth re-review", pushAndRereview(t, h, "b2"))
			if pr := h.pr(2); deref(pr.LastReviewEvent) != "APPROVED" {
				t.Fatalf("event %q, want APPROVED", deref(pr.LastReviewEvent))
			}
			wantNoMoreRounds(t, h, 2)
		})
	}
}

// The status comment's posted line of a held review says that a full pass
// follows before the approval; the full pass under way shows as one, as
// magnum's own full pass before the approval, not a review someone asked
// for, and its posted review's line says nothing more.
func TestAHeldReviewSaysSoInTheStatusComment(t *testing.T) {
	h := holdHarness(t, []string{store.VerdictBlocking, store.VerdictBlocking, store.VerdictClean}, statusOn, func(h *harness) {
		h.cfg.Pipeline.ApproveAfterFullPass = 1
	})
	h.statusReviewed()
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusPush})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick() // re-review 1 finds a problem
	h.wantState(7, store.PRReviewed)
	wantHeld(t, "re-review 2", pushHeld(t, h, 7, statusThird))
	pr := h.pr(7)
	h.advance(statusEvery)
	h.e.noteStatusComments(h.ctx) // before the next dispatch starts the full pass
	link := reviewURL(7, deref(pr.LastReviewID))
	if c := h.statusComment(7); !strings.HasSuffix(c.body, "\nReview of `c3d4e5f` posted "+utcClock(*pr.ReviewedAt)+": "+link+
		". A full pass follows before approval.") {
		t.Fatalf("the held review:\n%s", c.body)
	}

	h.holdRounds()
	before := len(h.rd.all())
	h.advance(time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.advance(statusEvery)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	pr = h.wantState(7, store.PRReviewing)
	if c := h.statusComment(7); !strings.Contains(c.body, "\n- [x] Full pass before approval of `c3d4e5f`\n- [x] In progress (full pass) since "+
		utcClock(*pr.LastRoundStartedAt)+"\n") {
		t.Fatalf("the full pass under way:\n%s", c.body)
	}
	h.releaseRound()
	h.settle()
	pr = h.wantState(7, store.PRReviewed)
	if deref(pr.LastReviewEvent) != "APPROVED" {
		t.Fatalf("the full pass posted %q, want APPROVED", deref(pr.LastReviewEvent))
	}
	h.advance(statusEvery)
	h.tick()
	c := h.statusComment(7)
	if !strings.HasSuffix(c.body, "\nReview of `c3d4e5f` posted "+utcClock(*pr.ReviewedAt)+": "+reviewURL(7, deref(pr.LastReviewID))) {
		t.Fatalf("after the full pass:\n%s", c.body)
	}
}

// Auto-approval as the operator waits for the full pass of a held review:
// the gates refuse that review, as a wait that withdraws no standing
// approval, and let a later review of the PR through.
func TestAutoApprovalWaitsForAHeldReviewsFullPass(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.e.setKV(h.ctx, kvPRApprovalHold(pr.ID), `{"review_id":501,"round":4,"target":"b1"}`)
	c := store.RepoPR{PR: pr, Repo: "talkable/talkable"}
	held := store.ReviewSummary{ReviewID: 501, SHA: "b1", Verdict: store.VerdictClean}
	if why, waits := h.e.autoGateRefusal(h.ctx, c, held); why != approvalHoldRefusal || !waits {
		t.Fatalf("the held review: refusal %q (waits %v), want %q as a wait", why, waits, approvalHoldRefusal)
	}
	full := store.ReviewSummary{ReviewID: 502, SHA: "b1", Verdict: store.VerdictClean}
	if why, _ := h.e.autoGateRefusal(h.ctx, c, full); why == approvalHoldRefusal {
		t.Fatal("the full pass's review is refused as the held one")
	}
}
