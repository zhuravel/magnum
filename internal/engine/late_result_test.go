package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// judgeGivesUp is a round script whose judge ends its turn and its nudge's
// without a result, as the pipeline records it: the judge's runs failed with
// agents.JudgeNoResult, and the round needing attention. It records the
// round's marker (the first run) in marker.
func judgeGivesUp(h *harness, marker *string) func(pipeline.RoundInput) (pipeline.RoundResult, error) {
	return func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		round, msg := max(in.Round, 1), agents.JudgeNoResult+", also after a nudge"
		for _, kind := range []string{store.RunRereview, store.RunNudge} {
			now := h.clock.Now()
			run, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: round, Role: "codex-judge", Kind: kind, TargetSHA: in.TargetSHA,
				Identity: "talkable-app", ReviewerLogin: magnumApp, State: store.RunSubmitted, PromptText: "go", CreatedAt: now, SubmittedAt: &now})
			if err != nil {
				return failRound(h.t, err)
			}
			if err := h.st.TransitionRun(h.ctx, run.ID, nil, store.RunFailed, func(u *store.RunUpdate) {
				u.Set("ended_at", now)
				u.Set("outcome", pipeline.OutcomeNeedsAttention)
				u.Set("error", msg)
			}); err != nil {
				return failRound(h.t, err)
			}
			if *marker == "" {
				*marker = run.ID
			}
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeNeedsAttention, Round: round, TargetSHA: in.TargetSHA, JudgeRunID: *marker, Error: msg}, nil
	}
}

// judgeWritesLate writes the judge's result file for PR #n at head as the
// judge's skill does once it posted review id, for run marker.
func judgeWritesLate(h *harness, n int, head, marker string, id int64) {
	h.t.Helper()
	dir := h.e.d.Layout.ReviewDir("talkable", "talkable", n, head)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	body := fmt.Sprintf(`{"status":"posted","run_id":%q,"review_id":%d,"event":"COMMENT","findings":{}}`, marker, id)
	if err := os.WriteFile(filepath.Join(dir, "codex-judge.json"), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// On 2026-11-05 a re-review of the operator's own PR ended needing attention
// when the judge's turn and its nudge's ended without its result; the judge
// worked on and posted its review 15 minutes later, but the PR stayed parked
// as failed (and its own PR interval of 2 hours would have held a retry). Once
// the judge's result file holds a posted review for that round, within the
// judge's timeout, the PR goes back in line at once, past the re-review
// interval, its attention cleared, with one round.late_result info event;
// the round that adopts the review is the next one (the pipeline's
// findUnverified), and the PR is reviewed.
func TestAJudgesLateReviewPutsThePRBackInLineForTheRoundThatAdoptsIt(t *testing.T) {
	h := newHarness(t, ownInterval(2*time.Hour))
	authoredPR(h, 2, "b1", "zhuravel")
	var marker string
	h.rd.script = judgeGivesUp(h, &marker)
	authoredPush(h, 2*time.Hour+time.Minute, prSpec{n: 2, head: "b2", author: "zhuravel"})
	h.advance(20 * time.Minute) // past the push quiet period
	h.tick()
	reqWantRounds(t, h, 2)
	h.wantState(2, store.PRNeedsAttention)

	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) { return posted(in.TargetSHA, 0), nil }
	h.advance(5 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 2) // no late result yet: the PR stays parked
	h.wantState(2, store.PRNeedsAttention)

	judgeWritesLate(h, 2, "b2", marker, 810)
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 3)
	h.wantState(2, store.PRReviewed)
	if why, ok := h.e.getKV(h.ctx, KVPRAttention(h.pr(2).ID)); ok {
		t.Fatalf("the PR keeps its attention %q", why)
	}
	evs := h.events(eventLateResult)
	if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "review 810") {
		t.Fatalf("round.late_result events %+v, want one naming review 810", evs)
	}
}

// A result file the judge writes after its timeout has run out since the
// round gave up is no late result of that round: the PR stays parked.
func TestAJudgesResultAfterItsTimeoutLeavesThePRParked(t *testing.T) {
	h := newHarness(t)
	authoredPR(h, 2, "b1", "alice")
	var marker string
	h.rd.script = judgeGivesUp(h, &marker)
	authoredPush(h, time.Hour, prSpec{n: 2, head: "b2", author: "alice"})
	h.advance(20 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 2)
	h.wantState(2, store.PRNeedsAttention)

	judge, _ := h.cfg.RoleByNameOrAlias(nil, "codex-judge")
	h.advance(judge.Timeout.Duration)
	judgeWritesLate(h, 2, "b2", marker, 811)
	h.tick()
	reqWantRounds(t, h, 2)
	h.wantState(2, store.PRNeedsAttention)
}
