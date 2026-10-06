package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// magnumApp is the talkable watch's posting login (Account form).
const magnumApp = "talkable[bot]"

// threadReply is a reply by by (a user) in one of magnum's threads now.
func threadReply(h *harness, by string) github.Remark {
	return github.Remark{Review: true, At: h.clock.Now(), Author: by, Answers: []string{magnumApp}, AnswersKnown: true}
}

// repliedRounds makes a reply round (pipeline.RoundInput.Replies) end
// replied, prompted at its start, with one rebuttal; any other round posts.
func repliedRounds(h *harness, stops ...pipeline.StopThread) {
	h.rd.mu.Lock()
	defer h.rd.mu.Unlock()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.Replies == 0 {
			return h.rd.posted(h.ctx, in, 99)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeReplied, Round: 2, TargetSHA: in.TargetSHA, JudgePromptedAt: h.clock.Now(),
			Replies:     []pipeline.PostedReply{{CommentID: 101, ID: 800, Kind: "rebuttal"}},
			ThreadsRead: true, Stops: stops}, nil
	}
}

// The live cases (2026-10-06): three author replies waited 16.5 hours for
// the next push, and an operator forced a 17-minute round to have a
// declined finding re-decided. A reply in one of magnum's threads on the
// head magnum reviewed starts a round of the judge alone on that head once
// reply_debounce (3m) passed since the last reply, with no other timing
// rule holding it; a later reply moves the wait.
func TestRepliesOnTheReviewedHeadStartAReplyRoundAfterTheDebounce(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.advance(10 * time.Minute)
	first := threadReply(h, "alice")
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{first}})
	pr := h.wantState(2, store.PRRereviewPending)
	if pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(first.At.Add(3*time.Minute)) {
		t.Fatalf("next eligible = %v, want 3m after the reply (%v)", pr.NextEligibleAt, first.At)
	}
	if n := len(pr.PendingReplies()); n != 1 {
		t.Fatalf("pending replies = %d", n)
	}
	queued := approvalEvents(t, h, 2, "pr.rereview_pending")
	if len(queued) == 0 || !strings.Contains(queued[len(queued)-1].Message, "reviewed → rereview_pending (1 reply to the review)") {
		t.Fatalf("queue events: %+v", queued)
	}

	h.advance(2 * time.Minute)
	second := threadReply(h, "alice")
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{first, second}})
	if pr := h.pr(2); !pr.NextEligibleAt.Equal(second.At.Add(3 * time.Minute)) {
		t.Fatalf("after a second reply: next eligible = %v, want 3m after it", pr.NextEligibleAt)
	}
	h.advance(2 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d inside the debounce", n)
	}
	repliedRounds(h)
	h.advance(time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the reply round", len(ins))
	}
	in := ins[1]
	if in.Kind != pipeline.KindRereview || !in.SameHead || in.Replies != 2 || in.TargetSHA != "b1" ||
		!slices.Equal(roleNames(in.Roles), []string{"codex-judge"}) {
		t.Fatalf("reply round input: kind %s same head %v replies %d target %s roles %v", in.Kind, in.SameHead, in.Replies, in.TargetSHA, roleNames(in.Roles))
	}
	starts := approvalEvents(t, h, 2, "engine.round_start")
	if got := starts[len(starts)-1].Message; got != "reply round (2 replies) in review1 at b1: codex-judge" {
		t.Fatalf("round start = %q", got)
	}
	if rr := h.e.lastReplyRound(h.ctx, h.pr(2)); rr.Head != "b1" || rr.Replies != 2 || rr.At.IsZero() {
		t.Fatalf("reply round record = %+v", rr)
	}
}

// A replied round leaves the review standing: the PR is reviewed again with
// its review fields as they were, the replies read up to the judge's prompt
// (no new round for them), an event names what was answered. A reply after
// the prompt starts the next reply round, no sooner than reply_min_interval
// (2h) after the last one on that head.
func TestARepliedRoundLeavesTheReviewAndSpacesTheNextOneByTheInterval(t *testing.T) {
	h := newHarness(t)
	reviewed := h.reviewedPR(2, "b1")
	repliedRounds(h)
	h.advance(10 * time.Minute)
	first := threadReply(h, "alice")
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{first}})
	h.advance(3 * time.Minute)
	h.tick()
	start := h.clock.Now()
	pr := h.wantState(2, store.PRReviewed)
	if deref(pr.LastReviewID) != deref(reviewed.LastReviewID) || deref(pr.LastReviewEvent) != deref(reviewed.LastReviewEvent) ||
		!pr.ReviewedAt.Equal(*reviewed.ReviewedAt) || deref(pr.ReviewedSHA) != "b1" {
		t.Fatalf("review fields moved: %v %v %v %v", pr.LastReviewID, pr.LastReviewEvent, pr.ReviewedAt, pr.ReviewedSHA)
	}
	if pr.RepliesReadAt == nil || !pr.RepliesReadAt.Equal(start) || len(pr.PendingReplies()) != 0 {
		t.Fatalf("replies read at %v (want %v), pending %d", pr.RepliesReadAt, start, len(pr.PendingReplies()))
	}
	evs := approvalEvents(t, h, 2, "pr.replied")
	if len(evs) != 1 || evs[0].Message != "replied in 1 thread on b1; no new review: review 101 stands" {
		t.Fatalf("pr.replied events: %+v", evs)
	}
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d: the replies it read started another", n)
	}

	h.advance(10 * time.Minute)
	later := threadReply(h, "alice")
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{first, later}})
	pr = h.wantState(2, store.PRRereviewPending)
	if !pr.NextEligibleAt.Equal(start.Add(2 * time.Hour)) {
		t.Fatalf("next eligible = %v, want 2h after the last reply round (%v)", pr.NextEligibleAt, start)
	}
	if w, ok := ParseWait(mustKV(t, h, KVPRWait(pr.ID))); !ok || w.Reason != WaitReplyInterval || w.Short(h.clock.Now()) != "re-decision · reply interval → "+waitClock(start.Add(2*time.Hour), h.clock.Now()) {
		t.Fatalf("wait = %+v", w)
	}
	h.advance(2*time.Hour - 10*time.Minute)
	h.tick()
	if ins := h.rd.all(); len(ins) != 3 || ins[2].Replies != 1 {
		t.Fatalf("rounds = %d, want the next reply round with 1 reply", len(ins))
	}
}

// A push while the reply round waits wins: the PR waits for an ordinary
// re-review of the new head, whose judge reads the replies with the rest.
func TestAPushWinsOverAReplyRound(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.advance(10 * time.Minute)
	reply := threadReply(h, "alice")
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{reply}})
	h.wantState(2, store.PRRereviewPending)
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2", remarks: []github.Remark{reply}})
	h.advance(10 * time.Minute)
	h.tick()
	h.advance(30 * time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the re-review of b2", len(ins))
	}
	if in := ins[1]; in.TargetSHA != "b2" || in.SameHead || in.Replies != 0 {
		t.Fatalf("round after the push: target %s same head %v replies %d", in.TargetSHA, in.SameHead, in.Replies)
	}
}

// Only replies that may answer magnum count: the PR author's own reviews
// and comments ("(Claude)" replies an author's agent posts are the
// author's), and anyone's reply in one of magnum's threads, a bot's too.
// magnum's own logins, a teammate's review elsewhere and a bot's top-level
// comment never start a round.
func TestOnlyRepliesThatMayAnswerMagnumStartARound(t *testing.T) {
	for name, c := range map[string]struct {
		remark func(h *harness) github.Remark
		starts bool
	}{
		"the author's comment":   {func(h *harness) github.Remark { return github.Remark{At: h.clock.Now(), Author: "alice"} }, true},
		"a teammate in a thread": {func(h *harness) github.Remark { return threadReply(h, "bob-rev") }, true},
		"a bot in a thread": {func(h *harness) github.Remark {
			r := threadReply(h, "coder[bot]")
			r.Bot = true
			return r
		}, true},
		"magnum's own reply": {func(h *harness) github.Remark {
			r := threadReply(h, magnumApp)
			r.Bot = true
			return r
		}, false},
		"a teammate's review elsewhere": {func(h *harness) github.Remark {
			return github.Remark{Review: true, At: h.clock.Now(), Author: "bob-rev", Answers: []string{"alice"}, AnswersKnown: true}
		}, false},
		"a bot's comment": {func(h *harness) github.Remark {
			return github.Remark{At: h.clock.Now(), Author: "coder[bot]", Bot: true}
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.reviewedPR(2, "b1")
			h.advance(10 * time.Minute)
			reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{c.remark(h)}})
			want := store.PRReviewed
			if c.starts {
				want = store.PRRereviewPending
			}
			h.wantState(2, want)
		})
	}
}

// Replies older than reply tracking (an upgrade) start nothing, though the
// board still counts them; `magnum review --replies` re-decides them, and a
// plain `magnum review` of the same head stays a review (no reply round).
func TestOldRepliesWaitForTheOperatorWhoMayAskForTheReDecision(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.advance(10 * time.Minute)
	reply := threadReply(h, "alice")
	h.e.setKV(h.ctx, kvRepliesSince, store.FormatTime(h.clock.Now().Add(time.Minute)))
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{reply}})
	pr := h.wantState(2, store.PRReviewed)
	if len(pr.PendingReplies()) != 1 {
		t.Fatalf("pending = %d, want the old reply", len(pr.PendingReplies()))
	}

	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if ins := h.rd.all(); len(ins) != 2 || !ins[1].SameHead || ins[1].Replies != 0 {
		t.Fatalf("magnum review: rounds %d, same head %v replies %d", len(ins), ins[len(ins)-1].SameHead, ins[len(ins)-1].Replies)
	}
	repliedRounds(h)
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{reply, threadReply(h, "alice")}})
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Replies: true})
	h.tick()
	ins := h.rd.all()
	if len(ins) != 3 || !ins[2].SameHead || ins[2].Replies != 1 {
		t.Fatalf("magnum review --replies: rounds %d, same head %v replies %d", len(ins), ins[len(ins)-1].SameHead, ins[len(ins)-1].Replies)
	}
	if pr := h.wantState(2, store.PRReviewed); pr.Forced {
		t.Fatal("the forced mark outlived the re-decision")
	}
	if _, ok := kvValue(h, kvPRRedecide(pr.ID)); ok {
		t.Fatal("the re-decision mark outlived its round")
	}
}

// After two rebuttals and an answer in a thread, the board flags the PR for
// the operator (KVPRStalemate, with an event); the operator acting on the PR
// (a review they ask for) clears the flag, and the same answer does not
// raise it again; a newer answer there does.
func TestAThreadMagnumStoppedArguingInFlagsThePRUntilTheOperatorActs(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	stop := pipeline.StopThread{ID: "PRRT_A", URL: "https://github.com/talkable/talkable/pull/2#discussion_r1", LastReply: 206}
	repliedRounds(h, stop)
	h.advance(10 * time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{threadReply(h, "alice")}})
	h.advance(3 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRReviewed)
	st, ok := ParseStalemate(mustKV(t, h, KVPRStalemate(pr.ID)))
	if !ok || len(st.Threads) != 1 || st.Threads[0].URL != stop.URL {
		t.Fatalf("stalemate = %+v", st)
	}
	evs := approvalEvents(t, h, 2, "pr.stalemate")
	if len(evs) != 1 || evs[0].Message != "magnum stopped arguing in 1 thread after two rebuttals and an answer; the operator decides" {
		t.Fatalf("stalemate events: %+v", evs)
	}

	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Replies: true})
	h.tick() // the request, then its round, which finds the same answer
	if _, ok := kvValue(h, KVPRStalemate(pr.ID)); ok {
		t.Fatal("the flag outlived the operator's request, or the same answer raised it again")
	}
	if seen := mustKV(t, h, kvPRStalemateSeen(pr.ID)); seen != `{"PRRT_A":206}` {
		t.Fatalf("seen = %s", seen)
	}

	repliedRounds(h, pipeline.StopThread{ID: stop.ID, URL: stop.URL, LastReply: 207})
	h.advance(3 * time.Hour)
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{threadReply(h, "alice")}})
	h.advance(3 * time.Minute)
	h.tick()
	if st, ok := ParseStalemate(mustKV(t, h, KVPRStalemate(pr.ID))); !ok || st.Threads[0].LastReply != 207 {
		t.Fatalf("a newer answer: stalemate %+v", st)
	}
}

// reply_debounce = 0 turns reply rounds off: the replies are kept and
// counted, nothing starts.
func TestReplyDebounceZeroStartsNoReplyRound(t *testing.T) {
	h := newHarness(t)
	h.cfg.Daemon.ReplyDebounce.Duration = 0
	h.reviewedPR(2, "b1")
	h.advance(10 * time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{threadReply(h, "alice")}})
	if pr := h.wantState(2, store.PRReviewed); len(pr.PendingReplies()) != 1 {
		t.Fatalf("pending = %d", len(pr.PendingReplies()))
	}
}

// A continued turn of a paused reply round may still end replied; one of
// another round may not.
func TestAContinuedReplyRoundKeepsItsReplies(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	start := h.clock.Now()
	h.e.noteReplyRound(h.ctx, pr.ID, "b1", 2, start)
	pr.LastRoundStartedAt = &start
	if n := h.e.continuedReplies(h.ctx, pr, "b1"); n != 2 {
		t.Fatalf("continued replies = %d, want 2", n)
	}
	later := start.Add(time.Hour)
	pr.LastRoundStartedAt = &later
	if n := h.e.continuedReplies(h.ctx, pr, "b1"); n != 0 {
		t.Fatalf("another round's continue: replies = %d", n)
	}
	if n := h.e.continuedReplies(h.ctx, pr, "b2"); n != 0 {
		t.Fatalf("another head: replies = %d", n)
	}
}

// mustKV reads a kv value that must be set.
func mustKV(t *testing.T, h *harness, key string) string {
	t.Helper()
	v, ok := kvValue(h, key)
	if !ok {
		t.Fatalf("kv %s is not set", key)
	}
	return v
}
