package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// flagToast is the toast every refused round sends.
const flagToast = "Codex flagged · never reviewed again"

// refusedRound is a round whose judge's own pass Codex refused.
func refusedRound(in pipeline.RoundInput) (pipeline.RoundResult, error) {
	ref := pipeline.Refusal{Role: "codex-judge", Kind: "codex", RunID: "r-own-1",
		Detail: "■ This content was flagged for possible cybersecurity risk."}
	return pipeline.RoundResult{Outcome: pipeline.OutcomeRefused, Round: 1, TargetSHA: in.TargetSHA, Refusal: &ref, Error: ref.Sentence()}, nil
}

// flagOf is PR #n's Codex flag (ok false: none).
func (h *harness) flagOf(n int) (CodexFlag, bool) {
	h.t.Helper()
	v, ok, err := h.st.GetKV(h.ctx, KVPRCodexFlag(h.pr(n).ID))
	if err != nil {
		h.t.Fatal(err)
	}
	if !ok {
		return CodexFlag{}, false
	}
	return ParseCodexFlag(v)
}

// refusedPR is PR #2 at b1 after a round Codex refused.
func (h *harness) refusedPR() store.PR {
	h.t.Helper()
	h.queuedPR(2, "b1")
	h.rd.script = refusedRound
	h.advance(5 * time.Minute)
	h.tick()
	h.rd.script = nil
	return h.wantState(2, store.PRIneligible)
}

// The live case (talkable#11990): Codex refused the round. The PR is
// flagged for good (the refused head, role, run and time), its last error
// says who was refused in which run, one toast goes out, and nothing about
// it runs again by itself: not the next push, not a review request on
// GitHub, not a forced `magnum review`, which refuses with the reason.
func TestARefusedRoundFlagsThePRForGood(t *testing.T) {
	h := newHarness(t)
	pr := h.refusedPR()
	want := "Codex refused the review: content flagged as a cybersecurity risk (codex-judge, run r-own-1)"
	if deref(pr.LastError) != want || !strings.Contains(deref(pr.SkipReason), "Codex flagged") || pr.Forced {
		t.Fatalf("PR = last_error %q, skip_reason %q, forced %v", deref(pr.LastError), deref(pr.SkipReason), pr.Forced)
	}
	f, ok := h.flagOf(2)
	if !ok || f.Kind != "codex" || f.Role != "codex-judge" || f.Run != "r-own-1" || f.Head != "b1" || f.At.IsZero() ||
		!strings.Contains(f.Detail, "cybersecurity") {
		t.Fatalf("flag = %+v, %v", f, ok)
	}
	if f.Short() != flagToast {
		t.Errorf("Short() = %q", f.Short())
	}
	h.wantToasts(flagToast, 1)
	if !h.hasEvent("pr:talkable/talkable#2", "pr.codex_flagged") {
		t.Error("no pr.codex_flagged event")
	}

	// A push.
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2"})
	h.advance(30 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	if pr := h.wantState(2, store.PRIneligible); !strings.Contains(deref(pr.SkipReason), "Codex flagged") {
		t.Errorf("skip_reason after the push = %q", deref(pr.SkipReason))
	}
	// A review request on GitHub.
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
	h.advance(30 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	h.wantState(2, store.PRIneligible)
	// magnum review.
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}})
	h.tick()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(store.Deref(r.Result), "Codex flagged") ||
		!strings.Contains(store.Deref(r.Result), "magnum codex-flag clear") {
		t.Fatalf("magnum review = %s: %s", r.State, store.Deref(r.Result))
	}
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	h.wantToasts(flagToast, 1)
}

// What waits for a round when the flag is set (a forced review queued
// before, a paused turn) never starts: dispatch takes it out of line.
func TestDispatchHoldsAFlaggedPRWhateverWaits(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, "b1")
	pr := h.pr(2)
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRQueued}, store.PRQueued, func(u *store.PRUpdate) {
		u.Set("forced", true)
	}); err != nil {
		t.Fatal(err)
	}
	h.rawFlag(2)
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 0)
	if pr := h.wantState(2, store.PRIneligible); pr.Forced {
		t.Error("the forced mark stayed")
	}

	h2 := newHarness(t)
	h2.queuedPR(2, "b1")
	if err := h2.st.TransitionPR(h2.ctx, h2.pr(2).ID, []string{store.PRQueued}, store.PRPaused, nil); err != nil {
		t.Fatal(err)
	}
	h2.rawFlag(2)
	h2.advance(10 * time.Minute)
	h2.tick()
	reqWantRounds(t, h2, 0)
	h2.wantState(2, store.PRIneligible)
}

// rawFlag writes PR #n's flag record and nothing else: the dispatch guard
// is what keeps the round off.
func (h *harness) rawFlag(n int) {
	h.t.Helper()
	v, err := CodexFlag{Kind: "codex", Role: "codex-judge", Run: "r-1", At: h.clock.Now()}.marshal()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, KVPRCodexFlag(h.pr(n).ID), v); err != nil {
		h.t.Fatal(err)
	}
}

// setFlag flags PR #n as `magnum codex-flag set` does.
func (h *harness) setFlag(n int, reason string) string {
	h.t.Helper()
	id := h.enqueue(ReqCodexFlag, CodexFlagPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: n}, Reason: reason})
	h.handle()
	r := h.request(id)
	if r.State != store.RequestDone {
		h.t.Fatalf("codex-flag set = %s: %s", r.State, store.Deref(r.Result))
	}
	return store.Deref(r.Result)
}

// `magnum codex-flag set` flags a PR found before (its reason kept, by
// whom), and `clear` lifts the flag: the PR is judged by the watch again,
// so an unreviewed head is queued and reviewed.
func TestCodexFlagSetAndClear(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, "b1")
	res := h.setFlag(2, "Codex warned about cyber abuse on 10-06")
	if !strings.Contains(res, "flagged talkable/talkable#2") {
		t.Errorf("answer = %q", res)
	}
	f, ok := h.flagOf(2)
	if !ok || f.Kind != "codex" || f.Detail != "Codex warned about cyber abuse on 10-06" || f.By != "magnum codex-flag" || f.Head != "b1" {
		t.Fatalf("flag = %+v, %v", f, ok)
	}
	h.wantState(2, store.PRIneligible)
	h.wantToasts(flagToast, 0) // the operator set it: no toast

	id := h.enqueue(ReqCodexFlag, CodexFlagPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}, Clear: true})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(store.Deref(r.Result), "cleared") {
		t.Fatalf("clear = %s: %s", r.State, store.Deref(r.Result))
	}
	if _, ok := h.flagOf(2); ok {
		t.Fatal("the flag stayed")
	}
	h.wantState(2, store.PRQueued)
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	h.wantState(2, store.PRReviewed)

	// Clearing a PR with no flag says so.
	id = h.enqueue(ReqCodexFlag, CodexFlagPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}, Clear: true})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(store.Deref(r.Result), "not flagged") {
		t.Fatalf("clear again = %s: %s", r.State, store.Deref(r.Result))
	}
}
