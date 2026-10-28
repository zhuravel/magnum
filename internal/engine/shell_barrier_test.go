package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// barrierRound is a re-review that a restart ended because codex-review's
// cut command still ran in the checkout (pipeline.ErrShellRunning), after
// its reviewers were prompted.
func barrierRound(t *testing.T, h *harness) func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
	t.Helper()
	return func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if err := promptedRun(h.ctx, h.st, in, 2); err != nil {
			return failRound(t, err)
		}
		err := fmt.Errorf("pipeline: restart on b3: codex-review still runs in the checkout after magnum tried to stop it, so the checkout stays on b2: %w",
			pipeline.ErrShellRunning)
		return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Round: 2, Error: err.Error()}, err
	}
}

// A round the restart's barrier ended (a cut command still ran in the
// checkout) is not the PR's doing: it costs no attempt and no round of the
// daily cap, and the PR is tried again a minute later.
func TestARestartThatKeptTheCheckoutCostsNoAttemptOrCapRound(t *testing.T) {
	h := newHarness(t)
	before, after := secondRound(t, h, barrierRound(t, h))
	if after.Attempts != before.Attempts {
		t.Errorf("attempts = %d, want %d: the barrier is no failed attempt", after.Attempts, before.Attempts)
	}
	if after.RoundsToday != before.RoundsToday {
		t.Errorf("rounds_today = %d, want %d: the barrier's round is refunded", after.RoundsToday, before.RoundsToday)
	}
	if evs := refundEvents(t, h); len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"reason":"`+RefundShell+`"`) {
		t.Errorf("round.refunded events = %+v, want one for the shell command", evs)
	}
	if after.NextAttemptAt == nil || after.NextAttemptAt.Before(h.clock.Now().Add(busyRetry-time.Second)) {
		t.Errorf("next_attempt_at = %v, want about %s from now", after.NextAttemptAt, busyRetry)
	}
}

// The retry of a round the barrier ended looks at the slot's shell panes
// before its checkout: while a command still keeps one busy (the agents
// manager holds the round under the PR's cooldown, ErrHumanActive), the
// slot is not switched and no attempt is counted; once the panes are clear,
// the round checks out and runs.
func TestARetryAfterAFailedStopHoldsBeforeItsCheckout(t *testing.T) {
	h := newHarness(t)
	before, _ := secondRound(t, h, barrierRound(t, h))
	h.rd.script = nil
	h.ag.mu.Lock()
	h.ag.clearShell = fmt.Errorf("agents: codex-review pane w1:p3 still runs a command in /slots/review1: %w", agents.ErrHumanActive)
	h.ag.mu.Unlock()
	checkouts := func() int {
		return len(slices.DeleteFunc(h.sl.all(), func(c string) bool { return !strings.HasPrefix(c, "checkout:") }))
	}
	start := checkouts()
	h.advance(2 * time.Minute) // past the barrier's retry
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want none while the slot's shell pane runs a command", n)
	}
	if got := checkouts(); got != start {
		t.Errorf("checkouts = %d after the hold, want %d: the slot switched under the command", got, start)
	}
	if h.ag.count("clear_shells:") == 0 {
		t.Fatal("the retry never looked at the slot's shell panes")
	}
	if pr := h.pr(2); pr.Attempts != before.Attempts {
		t.Errorf("attempts = %d, want %d: the hold is no failed attempt", pr.Attempts, before.Attempts)
	}

	h.ag.mu.Lock()
	h.ag.clearShell = nil
	h.ag.mu.Unlock()
	h.advance(h.cfg.Daemon.HumanCooldown.Duration + time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 3 {
		t.Fatalf("rounds = %d, want the retry once the panes are clear", n)
	}
	if got := checkouts(); got == start {
		t.Error("the retry did not check out")
	}
	h.wantState(2, store.PRReviewed)
}
