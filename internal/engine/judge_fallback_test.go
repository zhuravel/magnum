package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// fallbackHarness is a harness with [pipeline] judge_fallback = "claude-judge".
func fallbackHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, func(h *harness) { h.cfg.Pipeline.JudgeFallback = "claude-judge" })
}

// refusedByClaudeJudge is a round whose fallback judge's own pass Claude
// refused.
func refusedByClaudeJudge(in pipeline.RoundInput) (pipeline.RoundResult, error) {
	ref := pipeline.Refusal{Role: "claude-judge", Kind: "claude", RunID: "r-own-2", Detail: "the request was refused by the safety policy"}
	return pipeline.RoundResult{Outcome: pipeline.OutcomeRefused, Round: 1, TargetSHA: in.TargetSHA, Refusal: &ref, Error: ref.Sentence()}, nil
}

// With judge_fallback set, a round Codex refuses flags the PR for Codex
// and switches it to the fallback judge: the PR goes back in line at
// once, its next round runs without any Codex role and with claude-judge
// as judge, its slot and agents are not given back, and `magnum review`
// is allowed.
func TestARefusedRoundSwitchesTheJudgeToTheFallback(t *testing.T) {
	h := fallbackHarness(t)
	h.queuedPR(2, "b1")
	h.rd.script = refusedRound
	h.advance(5 * time.Minute)
	h.tick()
	h.rd.script = nil
	pr := h.wantState(2, store.PRQueued) // never reviewed: its first round again
	f, ok := h.flagOf(2)
	if !ok || f.Kind != "codex" || f.Role != "codex-judge" || f.Fallback != "claude-judge" {
		t.Fatalf("flag = %+v, %v", f, ok)
	}
	if deref(pr.SkipReason) != "" || !strings.Contains(deref(pr.LastError), "Codex refused the review") {
		t.Errorf("PR = skip_reason %q, last_error %q", deref(pr.SkipReason), deref(pr.LastError))
	}
	if f.Short() != "Codex flagged · judge claude-judge" {
		t.Errorf("Short() = %q", f.Short())
	}
	if s := f.Sentence("talkable/talkable#2"); !strings.Contains(s, "without Codex from now on, with claude-judge as judge") {
		t.Errorf("Sentence() = %q", s)
	}
	h.wantToasts("judge claude-judge", 1)
	h.wantToasts(flagToast, 0)
	if !h.hasEvent("pr:talkable/talkable#2", "pr.codex_flagged") {
		t.Error("no pr.codex_flagged event")
	}

	// The next round: no Codex role, claude-judge as the judge.
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 2)
	names := roleNames(h.rd.all()[1].Roles)
	if !slices.Contains(names, "claude-judge") || slices.Contains(names, "codex-judge") || slices.Contains(names, "codex-review") ||
		!slices.Contains(names, "claude-review") {
		t.Fatalf("the fallback round's roles = %v", names)
	}
	h.wantState(2, store.PRReviewed)
	if h.hasEvent("pr:talkable/talkable#2", "pr.codex_flag_released") {
		t.Error("the PR gave back its slot and agents")
	}
	if got := h.e.judgeOf(h.ctx, h.pr(2).ID); got != "claude-judge" {
		t.Errorf("judgeOf = %q", got)
	}

	// A push is re-reviewed, and magnum review is allowed.
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2"})
	h.advance(30 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 3)
	h.wantState(2, store.PRReviewed)
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}})
	h.tick()
	if r := h.request(id); r.State == store.RequestFailed {
		t.Fatalf("magnum review = %s: %s", r.State, store.Deref(r.Result))
	}
}

// A reviewed PR whose re-review Codex refuses goes back to rereview_pending
// and its fallback round is a re-review.
func TestARefusedRereviewGoesBackInLineForTheFallback(t *testing.T) {
	h := fallbackHarness(t)
	h.reviewedPR(2, "b1")
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2"})
	h.rd.script = refusedRound
	h.advance(30 * time.Minute)
	h.tick()
	h.rd.script = nil
	h.wantState(2, store.PRRereviewPending)
	h.advance(35 * time.Minute) // past the minimum re-review interval, counted from the refused round
	h.tick()
	reqWantRounds(t, h, 3)
	in := h.rd.all()[2]
	if in.Kind != pipeline.KindRereview && in.Kind != pipeline.KindRecovery {
		t.Errorf("the fallback round's kind = %s", in.Kind)
	}
	h.wantState(2, store.PRReviewed)
}

// When the fallback judge's kind refuses too, the PR is held for good, as
// a flag without a fallback holds it.
func TestAFallbackJudgeRefusalHoldsThePRForGood(t *testing.T) {
	h := fallbackHarness(t)
	h.queuedPR(2, "b1")
	h.rd.script = refusedRound
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRQueued)
	h.rd.script = refusedByClaudeJudge
	h.advance(10 * time.Minute)
	h.tick()
	h.rd.script = nil
	pr := h.wantState(2, store.PRIneligible)
	f, ok := h.flagOf(2)
	if !ok || f.Kind != "claude" || f.Role != "claude-judge" || f.Fallback != "" {
		t.Fatalf("flag = %+v, %v", f, ok)
	}
	if !strings.Contains(deref(pr.SkipReason), "Claude flagged") || f.Short() != "Claude flagged · never reviewed again" {
		t.Errorf("skip_reason %q, Short() %q", deref(pr.SkipReason), f.Short())
	}
	h.advance(30 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 2)
}

// A flag set by hand attaches the fallback too: the PR keeps its state and
// its next round runs with the fallback judge; clearing the flag gives the
// watch's judge back.
func TestCodexFlagSetByHandAttachesTheFallback(t *testing.T) {
	h := fallbackHarness(t)
	h.reviewedPR(2, "b1")
	res := h.setFlag(2, "Codex warned about cyber abuse elsewhere")
	if !strings.Contains(res, "with claude-judge as judge") {
		t.Errorf("answer = %q", res)
	}
	f, ok := h.flagOf(2)
	if !ok || f.Kind != "codex" || f.Fallback != "claude-judge" {
		t.Fatalf("flag = %+v, %v", f, ok)
	}
	h.wantState(2, store.PRReviewed)
	if got := roleNames(h.e.rolesForPR(h.ctx, &h.cfg.Watches[0], h.pr(2).ID)); !slices.Contains(got, "claude-judge") || slices.Contains(got, "codex-review") {
		t.Errorf("rolesForPR = %v", got)
	}
	id := h.enqueue(ReqCodexFlag, CodexFlagPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}, Clear: true})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("clear = %s: %s", r.State, store.Deref(r.Result))
	}
	if got := roleNames(h.e.rolesForPR(h.ctx, &h.cfg.Watches[0], h.pr(2).ID)); slices.Contains(got, "claude-judge") || !slices.Contains(got, "codex-judge") {
		t.Errorf("rolesForPR after clear = %v", got)
	}
}

// A flag whose fallback is no longer a declared judge of another kind
// holds the PR as a flag without one does.
func TestAFlagWhoseFallbackIsGoneHoldsThePR(t *testing.T) {
	h := newHarness(t) // no judge_fallback
	h.queuedPR(2, "b1")
	if err := h.e.setCodexFlag(h.ctx, h.pr(2).ID, CodexFlag{Kind: "codex", Fallback: "nope", At: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	h.advance(5 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 0)
	h.wantState(2, store.PRIneligible)
	if got := roleNames(h.e.rolesForPR(h.ctx, &h.cfg.Watches[0], h.pr(2).ID)); slices.Contains(got, "claude-judge") {
		t.Errorf("rolesForPR = %v", got)
	}
}
