package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// promptedRun records a run of round that sent its prompt, as the pipeline
// would before it failed.
func promptedRun(ctx context.Context, st *store.Store, in pipeline.RoundInput, round int) error {
	sent := in.PR.UpdatedAt
	_, err := st.CreateRun(ctx, store.Run{PRID: in.PR.ID, Round: round, Role: "codex-judge", Kind: in.Kind, TargetSHA: in.TargetSHA,
		Identity: in.PR.Identity, ReviewerLogin: "x", State: store.RunFailed, PromptText: "p", SubmittedAt: &sent})
	return err
}

// secondRound reviews PR #2 at b1, then pushes b2 and lets its re-review end
// as script says; it returns the PR before and after that round.
func secondRound(t *testing.T, h *harness, script func(in pipeline.RoundInput) (pipeline.RoundResult, error)) (before, after store.PR) {
	t.Helper()
	before = h.reviewedPR(2, "b1")
	h.rd.script = script
	pollPR(h, 40*time.Minute, 2, "b2") // past the quiet period and the re-review interval
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the re-review", n)
	}
	return before, h.pr(2)
}

func refundEvents(t *testing.T, h *harness) []store.Event {
	t.Helper()
	return kindOf(subjectEvents(t, h, "pr:talkable/talkable#2"), "round.refunded")
}

func TestRoundsMagnumCutShortAreRefunded(t *testing.T) {
	renderErr := "pipeline: judge prompt: agents: render judge-rereview.md: template: judge-rereview.md:3:9: executing \"judge-rereview.md\" at <.ThreadsFile>: can't evaluate field ThreadsFile"
	for _, tc := range []struct {
		name   string
		result pipeline.RoundResult
		runs   bool // a run of the round was prompted
		reason string
	}{
		{"stopped", pipeline.RoundResult{Outcome: pipeline.OutcomeStopped, Round: 2, Error: "pipeline: round cancelled: context canceled"}, true, RefundStopped},
		{"render error after the reviewers ran", pipeline.RoundResult{Outcome: pipeline.OutcomeError, Round: 2, Error: renderErr}, true, RefundRender},
		{"error before any prompt", pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: "pipeline: report dir: permission denied"}, false, RefundUnprompted},
		{"every fallback model limited", pipeline.RoundResult{Outcome: pipeline.OutcomeUsageLimit, Round: 2, Error: "judge pane",
			Pause: &pipeline.Pause{Kind: "usage_limit", Tool: "claude", Detail: "You've reached your Fable limit · resets 6pm"}}, true, RefundModelLimits},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			before, after := secondRound(t, h, func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
				if tc.runs {
					if err := promptedRun(h.ctx, h.st, in, 2); err != nil {
						return failRound(t, err)
					}
				}
				return tc.result, nil
			})
			if after.RoundsToday != before.RoundsToday {
				t.Errorf("rounds_today = %d after a refunded round, want %d", after.RoundsToday, before.RoundsToday)
			}
			if after.LastRoundStartedAt == nil || !after.LastRoundStartedAt.Equal(*before.LastRoundStartedAt) {
				t.Errorf("last_round_started_at = %v, want the first round's %v", after.LastRoundStartedAt, before.LastRoundStartedAt)
			}
			evs := refundEvents(t, h)
			if len(evs) != 1 {
				t.Fatalf("round.refunded events: %+v", evs)
			}
			var data struct {
				Reason      string
				RoundsToday int `json:"rounds_today"`
			}
			if err := json.Unmarshal(evs[0].Data, &data); err != nil || data.Reason != tc.reason || data.RoundsToday != before.RoundsToday {
				t.Fatalf("event %q data %s: %+v (%v)", evs[0].Message, evs[0].Data, data, err)
			}
		})
	}
}

func TestRoundsThatDidTheirWorkCount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result pipeline.RoundResult
	}{
		{"posted", pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 2, ReviewID: 202, Event: "COMMENTED", ReviewCommit: "b2"}},
		{"timeout", pipeline.RoundResult{Outcome: pipeline.OutcomeTimeout, Round: 2, Error: "judge timed out"}},
		{"needs attention", pipeline.RoundResult{Outcome: pipeline.OutcomeNeedsAttention, Round: 2, Error: "nothing posted"}},
		{"blocked", pipeline.RoundResult{Outcome: pipeline.OutcomeBlocked, Round: 2, Error: "HEAD mismatch"}},
		{"error after a prompt", pipeline.RoundResult{Outcome: pipeline.OutcomeError, Round: 2, Error: "pipeline: judge session lost"}},
		{"account usage limit", pipeline.RoundResult{Outcome: pipeline.OutcomeUsageLimit, Round: 2, Error: "judge pane",
			Pause: &pipeline.Pause{Kind: "usage_limit", Tool: "codex", Detail: "You've hit your usage limit. Try again at 6:00 PM."}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			before, after := secondRound(t, h, func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
				if err := promptedRun(h.ctx, h.st, in, 2); err != nil {
					return failRound(t, err)
				}
				return tc.result, nil
			})
			if after.RoundsToday != before.RoundsToday+1 {
				t.Errorf("rounds_today = %d, want %d", after.RoundsToday, before.RoundsToday+1)
			}
			if evs := refundEvents(t, h); len(evs) != 0 {
				t.Errorf("refunded: %+v", evs)
			}
		})
	}
}

// The live case: the cap was reached with rounds magnum cut short, and the
// PR sat for hours. With the refunds the next push is still reviewed.
func TestRefundedRoundsDoNotExhaustTheDailyCap(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Daemon.MaxRoundsPerPRPerDay = 2 })
	h.reviewedPR(2, "b1") // round 1 counts
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeStopped, Error: "pipeline: round cancelled"}, nil
	}
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(5 * time.Minute)
	h.tick() // round 2 stopped by a restart: refunded
	h.rd.script = nil
	h.advance(10 * time.Minute) // past the retry delay after a stop
	h.tick()
	ins := h.rd.all()
	if len(ins) != 3 || ins[2].TargetSHA != "b2" {
		t.Fatalf("rounds = %d, want the push reviewed after the refunded round", len(ins))
	}
	if pr := h.wantState(2, store.PRReviewed); pr.RoundsToday != 2 {
		t.Fatalf("rounds_today = %d, want 2 (the stopped round refunded)", pr.RoundsToday)
	}
}
