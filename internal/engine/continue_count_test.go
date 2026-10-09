package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// nextRound is the round number a new round of the PR gets: one past the
// latest of its runs. Round scripts call it, so it returns its error.
func nextRound(h *harness, prID int64) (int, error) {
	runs, err := h.st.RunsByPR(h.ctx, prID)
	if err != nil {
		return 0, err
	}
	round := 1
	for _, r := range runs {
		round = max(round, r.Round+1)
	}
	return round, nil
}

// sentJudge records the judge run of round the way the pipeline leaves a
// turn in flight: prompted and submitted.
func sentJudge(h *harness, in pipeline.RoundInput, round int) error {
	sent := h.clock.Now()
	_, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: round, Role: store.RoleJudge, Kind: in.Kind,
		TargetSHA: in.TargetSHA, Identity: in.PR.Identity, ReviewerLogin: "talkable[bot]", State: store.RunSubmitted,
		PromptText: "judge", SubmittedAt: &sent})
	return err
}

// shutDown cancels the PR's round as a daemon shutdown does: its context
// ends, and no abort asked for it.
func shutDown(h *harness, prID int64) error {
	h.e.mu.Lock()
	rh := h.e.rounds[prID]
	h.e.mu.Unlock()
	if rh == nil {
		return errors.New("no round to shut down")
	}
	rh.cancel()
	return nil
}

// A round a shutdown stopped once its judge was prompted is not over: the
// next daemon pauses it and continues the judge's turn, which posts. It is
// one round, counted once at its start: the stop is not refunded, and the
// continue counts nothing again, also when it became a full round because
// the judge's session was lost.
func TestARoundStoppedDuringTheJudgesTurnCountsOnce(t *testing.T) {
	t.Run("continued", func(t *testing.T) { stoppedDuringTheJudgesTurn(t, false) })
	t.Run("the judge's session lost", func(t *testing.T) { stoppedDuringTheJudgesTurn(t, true) })
}

func stoppedDuringTheJudgesTurn(t *testing.T, sessionLost bool) {
	h := newHarness(t)
	before := h.reviewedPR(2, "b1")
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		round, err := nextRound(h, in.PR.ID)
		if err == nil {
			err = sentJudge(h, in, round)
		}
		if err == nil {
			err = shutDown(h, in.PR.ID)
		}
		if err != nil {
			return failRound(t, err)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeStopped, Round: round, Error: "pipeline: round cancelled: context canceled"}, nil
	}
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(5 * time.Minute)
	started := h.clock.Now()
	h.tick()
	stopped := h.pr(2)
	if stopped.RoundsToday != before.RoundsToday+1 || stopped.LastRoundStartedAt == nil || !stopped.LastRoundStartedAt.Equal(started) {
		t.Fatalf("after the stop: rounds_today %d (want %d), last_round_started_at %v (want %v)",
			stopped.RoundsToday, before.RoundsToday+1, stopped.LastRoundStartedAt, started)
	}
	if evs := refundEvents(t, h); len(evs) != 0 {
		t.Fatalf("a round whose judge turn continues was refunded: %+v", evs)
	}

	// The next daemon continues the turn, and it posts.
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: in.Round, ReviewID: 202, Event: "COMMENTED", ReviewCommit: "b2"}, nil
	}
	h.e = New(h.d)
	h.startup()
	if p := h.wantState(2, store.PRPaused); !strings.Contains(deref(p.LastError), "during the judge's turn") {
		t.Fatalf("recovered as %s: %q", p.State, deref(p.LastError))
	}
	if sessionLost {
		if err := h.ag.Park(h.ctx, h.pr(2)); err != nil {
			t.Fatal(err)
		}
	}
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	ins := h.rd.all()
	if kind := ins[len(ins)-1].Kind; (kind == pipeline.KindContinue) == sessionLost {
		t.Fatalf("the round after the restart is a %s round", kind)
	}
	after := h.wantState(2, store.PRReviewed)
	if after.RoundsToday != before.RoundsToday+1 {
		t.Fatalf("after the continue: rounds_today %d, want %d", after.RoundsToday, before.RoundsToday+1)
	}
	if !sessionLost && !after.LastRoundStartedAt.Equal(started) {
		t.Fatalf("after the continue: last_round_started_at %v, want %v", after.LastRoundStartedAt, started)
	}
	if evs := refundEvents(t, h); len(evs) != 0 {
		t.Fatalf("refunded: %+v", evs)
	}
}

// A judge whose model and every fallback were limited pauses the PR, and
// the continue finishes that turn: the round is not refunded either.
func TestAJudgeTurnPausedOnModelLimitsIsNotRefunded(t *testing.T) {
	h := newHarness(t)
	before, after := secondRound(t, h, func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		round, err := nextRound(h, in.PR.ID)
		if err != nil {
			return failRound(t, err)
		}
		sent := h.clock.Now()
		if _, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: round, Role: store.RoleJudge, Kind: in.Kind,
			TargetSHA: in.TargetSHA, Identity: in.PR.Identity, ReviewerLogin: "talkable[bot]", State: store.RunFailed,
			Outcome: new(pipeline.OutcomeUsageLimit), PromptText: "judge", SubmittedAt: &sent}); err != nil {
			return failRound(t, err)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeUsageLimit, Round: round, Error: "judge pane",
			Pause: &pipeline.Pause{Kind: "usage_limit", Tool: "claude", Detail: "You've reached your Fable limit · resets 6pm"}}, nil
	})
	if after.State != store.PRPaused || after.RoundsToday != before.RoundsToday+1 {
		t.Fatalf("state %s rounds_today %d, want paused and %d", after.State, after.RoundsToday, before.RoundsToday+1)
	}
	if evs := refundEvents(t, h); len(evs) != 0 {
		t.Fatalf("refunded: %+v", evs)
	}
}

// An abort ends the round for good: stopped after its judge was prompted,
// it is refunded as before.
func TestAnAbortedRoundIsRefundedAfterTheJudgeWasPrompted(t *testing.T) {
	h := newHarness(t)
	before := h.reviewedPR(2, "b1")
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		round, err := nextRound(h, in.PR.ID)
		if err == nil {
			err = sentJudge(h, in, round)
		}
		if err != nil {
			return failRound(t, err)
		}
		h.e.mu.Lock()
		rh := h.e.rounds[in.PR.ID]
		rh.stop.reqs = append(rh.stop.reqs, stopOrder{id: 0})
		h.e.mu.Unlock()
		rh.cancel()
		return pipeline.RoundResult{Outcome: pipeline.OutcomeStopped, Round: round, Error: "pipeline: round cancelled: context canceled"}, nil
	}
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(5 * time.Minute)
	h.tick()
	evs := refundEvents(t, h)
	if len(evs) != 1 {
		t.Fatalf("round.refunded events: %+v", evs)
	}
	var data struct{ Reason string }
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || data.Reason != RefundStopped {
		t.Fatalf("refund %s (%v)", evs[0].Data, err)
	}
	if p := h.pr(2); p.RoundsToday != before.RoundsToday {
		t.Fatalf("rounds_today %d, want %d", p.RoundsToday, before.RoundsToday)
	}
}

// Recovery says where the round was when the daemon stopped: before it
// started (claiming), before it prompted any agent, or while its reviewers
// ran; each goes back in line.
func TestRecoveryNamesWhereTheRoundWas(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"}, prSpec{n: 3, head: "c1"}, prSpec{n: 4, head: "d1"})
	h.advance(time.Minute)
	h.tick()
	h.advance(time.Minute)
	start := h.clock.Now()
	for n, state := range map[int]string{2: store.PRReviewing, 3: store.PRClaiming, 4: store.PRReviewing} {
		if err := h.st.TransitionPR(h.ctx, h.pr(n).ID, nil, state, func(u *store.PRUpdate) {
			u.Set("last_round_started_at", start)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// #2's reviewers were prompted; #4's round had prompted nothing yet.
	sent := start.Add(time.Second)
	h.advance(time.Second)
	if _, err := h.st.CreateRun(h.ctx, store.Run{PRID: h.pr(2).ID, Round: 2, Role: "claude-review", Kind: store.RunRereview,
		TargetSHA: "b2", Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: store.RunWorking, PromptText: "review",
		SubmittedAt: &sent}); err != nil {
		t.Fatal(err)
	}
	h.e = New(h.d)
	h.e.recoverRows(h.ctx)
	for n, want := range map[int]string{
		2: "daemon restarted while the reviewers ran; the round starts again",
		3: "daemon restarted before the round started",
		4: "daemon restarted before the round prompted its agents; the round starts again",
	} {
		if got := deref(h.pr(n).LastError); got != want {
			t.Errorf("#%d: last_error %q, want %q", n, got, want)
		}
	}
}
