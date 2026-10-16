package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// reReviewHeldByADrain runs the forced re-review of fixPushed that
// loopToReReview left PR #2's loop waiting for while `magnum daemon-restart
// --drain` holds the daemon (a drain an older CLI started, naming no pid,
// which only the restart ends): the review posts what the loop's rounds
// post, and the next cycle it calls for waits for the restart.
func reReviewHeldByADrain(t *testing.T, h *harness) {
	t.Helper()
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	before := len(h.rd.all())
	reqPollNoSettle(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	h.awaitRound(before)
	h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
	close(h.rd.gate)
	h.settle()
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 || !st.Held {
		t.Fatalf("fix state after the re-review = %+v, %v; want cycle 1 waiting, its next cycle held for the restart", st, ok)
	}
}

// replyAfterTheReReview has alice, a member of the organization, answer in
// one of magnum's threads a minute after the re-review of fixPushed posted
// (the operator's own comments are magnum's here: a watch posts as their
// login), which queues PR #2 for a reply round once reply_debounce passed.
func replyAfterTheReReview(t *testing.T, h *harness) []github.Remark {
	t.Helper()
	h.advance(time.Minute)
	reply := threadReply(h, "alice")
	reply.Association = "MEMBER"
	remarks := []github.Remark{reply}
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel", remarks: remarks})
	if pr := h.wantState(2, store.PRRereviewPending); len(pr.PendingReplies()) != 1 || pr.Forced {
		t.Fatalf("after the reply: %d pending replies, forced %v", len(pr.PendingReplies()), pr.Forced)
	}
	return remarks
}

// The live case (2026-10-16): a fix loop's forced re-review posted with one
// optional P3 left while `magnum daemon-restart --drain` held the daemon, so
// its next cycle waited for the restart. A reply came, a reply round of the
// judge alone on the same head ended replied with nothing to answer, and the
// loop's hook took that round for its re-review: "fix loop ended: the
// re-review did not post (replied)", the PR in needs attention. The loop
// acts only on the re-review it forced. Here the reply round starts as the
// drain's command ends after the tick looked at the held loops and before
// its dispatch, and it ends replied under a second drain: the loop stays
// held, fix.round_ignored names the round, nothing fails or needs
// attention, and after the restart the held cycle fixes the re-review.
func TestAReplyRoundNeverEndsTheFixLoop(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P3": 1}, "COMMENTED"), fixPushed2)
	reReviewHeldByADrain(t, h)
	replyAfterTheReReview(t, h)

	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) { // round 3: replied, nothing to answer
		if in.Replies == 0 {
			h.t.Errorf("a round that is no reply round ran on %s", in.TargetSHA)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeReplied, Round: 3, TargetSHA: in.TargetSHA, JudgePromptedAt: h.clock.Now(), ThreadsRead: true}, nil
	}
	h.rd.mu.Unlock()
	h.advance(3 * time.Minute) // reply_debounce
	before := len(h.rd.all())
	if err := h.st.DeleteKV(h.ctx, KVDaemonDraining); err != nil { // the drain's command ends (its endDrain)
		t.Fatal(err)
	}
	h.e.dispatch(h.ctx, h.e.observe(h.ctx)) // the tick's dispatch, after its health looked at the held loop
	h.awaitRound(before)
	h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now())) // a second drain while the reply round runs
	close(h.rd.gate)
	h.settle()

	ins := h.rd.all()
	if in := ins[len(ins)-1]; in.Replies != 1 || !in.SameHead || in.TargetSHA != fixPushed {
		t.Fatalf("the last round: %d replies, same head %v, on %s; want the reply round on %s", in.Replies, in.SameHead, in.TargetSHA, fixPushed)
	}
	ev, ok := h.fixEvent("fix.round_ignored")
	if want := "round 3 ended replied: a reply round, not the re-review the fix loop forced on b2b2b2b"; !ok || ev.Level != "info" || ev.Message != want {
		t.Fatalf("fix.round_ignored = %+v, %v; want %q", ev, ok, want)
	}
	var data struct {
		Round   int    `json:"round"`
		Outcome string `json:"outcome"`
		Why     string `json:"why"`
		Cycle   int    `json:"cycle"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil || data.Round != 3 || data.Outcome != pipeline.OutcomeReplied || data.Why != "a reply round" || data.Cycle != 1 {
		t.Errorf("fix.round_ignored data = %s (%v)", ev.Data, err)
	}
	if ev, ok := h.fixEvent("fix.failed"); ok {
		t.Errorf("fix.failed recorded: %s", ev.Message)
	}
	if cur := h.wantState(2, store.PRReviewed); cur.LastError != nil {
		t.Errorf("last_error = %q", deref(cur.LastError))
	}
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 || !st.Held || st.Head != fixPushed {
		t.Fatalf("fix state after the reply round = %+v, %v; want cycle 1 still held", st, ok)
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d, want 1: a fixer started under the drain", n)
	}
	h.flush()
	h.wantToasts("needs attention", 0)

	h.startup() // the restart lifts the hold
	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 || runs[1].TargetSHA != fixPushed {
		t.Fatalf("fix runs after the restart = %+v, want cycle 2 on %s", runs, fixPushed)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Cycle != 2 || st.Phase != FixPhaseReviewing || st.Head != fixPushed2 || st.Held || st.ForcedAt.IsZero() {
		t.Errorf("fix state after the restart = %+v, %v", st, ok)
	}
	if ev, ok := h.fixEvent("fix.failed"); ok {
		t.Errorf("fix.failed recorded: %s", ev.Message)
	}
}

// The restart of 2026-10-16 ended the drain that held the loop's next cycle
// while a reply to the re-review waited for its reply round, and its
// recovery read the PR as waiting for the loop's re-review: it kept the
// loop for the reply round, whose end then failed the loop. The row now
// says the cycle is held, so the restart starts it at once, on the
// re-review that posted: no round runs first, nothing fails or is ignored,
// and the loop's next re-review is the forced round of the fixer's push, not
// a reply round.
func TestAHeldNextCycleStartsAfterTheRestart(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P3": 1}, "COMMENTED"), fixPushed2)
	reReviewHeldByADrain(t, h)
	remarks := replyAfterTheReReview(t, h)
	rounds := len(h.rd.all())

	h.startup()
	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 || runs[1].TargetSHA != fixPushed {
		t.Fatalf("fix runs after the restart = %+v, want cycle 2 on %s", runs, fixPushed)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Cycle != 2 || st.Phase != FixPhaseReviewing || st.Head != fixPushed2 || st.Held {
		t.Fatalf("fix state after the restart = %+v, %v", st, ok)
	}
	if n := len(h.rd.all()); n != rounds {
		t.Errorf("rounds = %d, want %d: a round ran before the held cycle", n, rounds)
	}
	for _, kind := range []string{"fix.failed", "fix.round_ignored", "fix.ended"} {
		if ev, ok := h.fixEvent(kind); ok {
			t.Errorf("%s recorded: %s", kind, ev.Message)
		}
	}

	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed2, author: "zhuravel", remarks: remarks})
	ins := h.rd.all()
	if len(ins) != rounds+1 {
		t.Fatalf("rounds = %d, want the re-review of the fixer's push", len(ins))
	}
	if in := ins[len(ins)-1]; in.Replies != 0 || in.TargetSHA != fixPushed2 {
		t.Errorf("the next round: %d replies on %s; want the loop's re-review of %s", in.Replies, in.TargetSHA, fixPushed2)
	}
	if ev, ok := h.fixEvent("fix.failed"); ok {
		t.Errorf("fix.failed recorded: %s", ev.Message)
	}
}

// The loop's own re-review decides it as before: the row records when the
// loop forced it, and the forced round of the fixer's push, once it posts,
// ends the loop clean, starts the next cycle or ends it at the cap, with
// nothing ignored.
func TestTheFixLoopActsOnItsOwnReReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cycles int
		found  map[string]int
		check  func(t *testing.T, h *harness, pr store.PR)
	}{
		{name: "clean", cycles: 3, found: map[string]int{}, check: func(t *testing.T, h *harness, _ store.PR) {
			t.Helper()
			if ev, ok := h.fixEvent("fix.done"); !ok || ev.Message != "the fix loop ends clean after 1 fix cycle: the re-review of b2b2b2b found nothing" {
				t.Errorf("fix.done = %+v, %v", ev, ok)
			}
			if _, ok := h.fixStateOf(2); ok {
				t.Error("the loop goes on")
			}
		}},
		{name: "next cycle", cycles: 3, found: map[string]int{"P2": 1}, check: func(t *testing.T, h *harness, pr store.PR) {
			t.Helper()
			if runs := h.fixRuns(pr.ID); len(runs) != 2 || runs[1].TargetSHA != fixPushed {
				t.Errorf("fix runs = %+v, want cycle 2 on %s", runs, fixPushed)
			}
			if st, ok := h.fixStateOf(2); !ok || st.Cycle != 2 || st.Phase != FixPhaseReviewing {
				t.Errorf("fix state = %+v, %v", st, ok)
			}
		}},
		{name: "capped", cycles: 1, found: map[string]int{"P2": 1}, check: func(t *testing.T, h *harness, _ store.PR) {
			t.Helper()
			if _, ok := h.fixEvent("fix.capped"); !ok {
				t.Error("no fix.capped")
			}
			h.wantState(2, store.PRNeedsAttention)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t, func(h *harness) { h.cfg.Pipeline.FixCycles = tc.cycles })
			pr := loopToReReview(t, h, git, findings(tc.found, "COMMENTED"), fixPushed2)
			if st, ok := h.fixStateOf(2); !ok || st.ForcedAt.IsZero() || st.ForcedAt.After(h.clock.Now().Add(-time.Minute)) || st.Held {
				t.Fatalf("fix state waiting for the re-review = %+v, %v; want when the loop forced it", st, ok)
			}
			reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
			tc.check(t, h, pr)
			if ev, ok := h.fixEvent("fix.round_ignored"); ok {
				t.Errorf("the loop's own re-review was ignored: %s", ev.Message)
			}
			if ev, ok := h.fixEvent("fix.failed"); ok {
				t.Errorf("fix.failed recorded: %s", ev.Message)
			}
		})
	}
}

// Rounds the loop did not force can end while it waits for its re-review:
// a CI check, a full pass the scheduler started, an audit pass, a reply
// round, a post-merge review, an automatic re-review, or a forced round
// dispatched before the loop forced its own. A clean review one of them
// posted ended the loop as done. Each is ignored with fix.round_ignored,
// which names it, the loop's row stays as it was, and the loop's own forced
// re-review then decides it.
func TestASchedulerRoundWhileTheLoopWaitsIsIgnored(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"), fixPushed2)
	st, raw := h.fixRowOf(2)
	forced := h.pr(2)
	if !forced.Forced {
		t.Fatal("the loop's re-review is not forced")
	}
	unforced := forced
	unforced.Forced = false // as the scheduler dispatches its rounds
	now := h.clock.Now()
	clean := pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 7, Event: "COMMENTED", Findings: map[string]int{}, TargetSHA: fixPushed}
	for _, tc := range []struct {
		job *roundJob
		why string
	}{
		{&roundJob{pr: unforced, kind: pipeline.KindRereview, sameHead: true, ciRed: true, dispatchedAt: now}, "a CI check"},
		{&roundJob{pr: unforced, kind: pipeline.KindRereview, fullPass: "re-review 10 since the first review (full_pass_every 10)",
			fullPassRule: "full_pass_every", dispatchedAt: now}, "a full pass (full_pass_every)"},
		{&roundJob{pr: unforced, kind: pipeline.KindRereview, audit: "audit_every 5", dispatchedAt: now}, "an audit pass"},
		{&roundJob{pr: unforced, kind: pipeline.KindRereview, sameHead: true, replies: 2, dispatchedAt: now}, "a reply round"},
		{&roundJob{pr: unforced, kind: pipeline.KindRereview, postMerge: true, dispatchedAt: now}, "a post-merge review"},
		{&roundJob{pr: unforced, kind: pipeline.KindRereview, dispatchedAt: now}, "a round the loop did not force"},
		{&roundJob{pr: forced, kind: pipeline.KindRereview, dispatchedAt: st.ForcedAt.Add(-time.Second)}, "a forced round dispatched before the loop forced its re-review"},
	} {
		h.e.fixAfterRound(h.ctx, tc.job, clean)
		h.settle()
		ev, ok := h.fixEvent("fix.round_ignored")
		if want := "round 7 ended posted: " + tc.why + ", not the re-review the fix loop forced on b2b2b2b"; !ok || ev.Message != want {
			t.Errorf("fix.round_ignored = %+v, %v; want %q", ev, ok, want)
		}
		if _, cur := h.fixRowOf(2); cur != raw {
			t.Fatalf("%s changed the loop's row: %s, was %s", tc.why, cur, raw)
		}
	}
	for _, kind := range []string{"fix.done", "fix.failed", "fix.ended"} {
		if ev, ok := h.fixEvent(kind); ok {
			t.Errorf("%s recorded: %s", kind, ev.Message)
		}
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d, want 1", n)
	}

	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // the loop's forced re-review
	if runs := h.fixRuns(pr.ID); len(runs) != 2 || runs[1].TargetSHA != fixPushed {
		t.Errorf("fix runs after the loop's re-review = %+v, want cycle 2 on %s", runs, fixPushed)
	}
}
