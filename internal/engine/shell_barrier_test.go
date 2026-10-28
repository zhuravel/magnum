package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
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

// holdShells makes the slot's shell pane busy with a command magnum did
// not start (foreign) or with its own that it cannot stop.
func holdShells(h *harness, foreign bool) {
	h.ag.mu.Lock()
	defer h.ag.mu.Unlock()
	h.ag.clearShell = &agents.ShellHoldError{Role: "codex-review", Pane: "w1:p3", Checkout: "/slots/review1", Foreign: foreign,
		Cause: "1 processes of its job still run after SIGKILL (sandbox-exec)"}
}

func freeShells(h *harness) {
	h.ag.mu.Lock()
	defer h.ag.mu.Unlock()
	h.ag.clearShell = nil
}

// countToasts counts the toasts shown whose title contains sub.
func countToasts(h *harness, sub string) int { return len(h.nh.titles(sub)) }

// checkouts counts the slot checkouts the fake slots saw.
func checkouts(h *harness) int {
	return len(slices.DeleteFunc(h.sl.all(), func(c string) bool { return !strings.HasPrefix(c, "checkout:") }))
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
// before its checkout. While a command keeps one busy the slot is not
// switched: the hold costs no attempt and no round of the daily cap, never
// marks a human at work, says so once (pr.shell_hold and one toast), and
// looks again after 1, 2 and 5 minutes. Once the panes are free the hold ends (pr.shell_hold_ended) and
// the round checks out and runs.
func TestARetryAfterAFailedStopHoldsBeforeItsCheckout(t *testing.T) {
	h := newHarness(t)
	before, _ := secondRound(t, h, barrierRound(t, h))
	h.rd.script = nil
	holdShells(h, false)
	start := checkouts(h)
	var waits []time.Duration
	for range 3 {
		pr := h.pr(2)
		at := deref(pr.NextAttemptAt)
		h.clock.Set(at.Add(time.Second))
		h.tick()
		if n := len(h.rd.all()); n != 2 {
			t.Fatalf("rounds = %d, want none while the slot's shell pane runs a command", n)
		}
		if next := h.pr(2).NextAttemptAt; next != nil {
			waits = append(waits, next.Sub(h.clock.Now()).Round(time.Minute))
		}
	}
	if !slices.Equal(waits, []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}) {
		t.Errorf("the hold's waits = %v, want 1m, 2m and 5m", waits)
	}
	if got := checkouts(h); got != start {
		t.Errorf("checkouts = %d after the hold, want %d: the slot switched under the command", got, start)
	}
	pr := h.pr(2)
	if pr.Attempts != before.Attempts || pr.HumanActiveAt != nil {
		t.Errorf("attempts %d (want %d), human_active_at %v (want none): the hold is no failure and no human", pr.Attempts, before.Attempts, pr.HumanActiveAt)
	}
	if pr.RoundsToday != before.RoundsToday {
		t.Errorf("rounds_today = %d, want %d: a held round is no round of the daily cap", pr.RoundsToday, before.RoundsToday)
	}
	if evs := kindOf(subjectEvents(t, h, "pr:talkable/talkable#2"), eventShellHold); len(evs) != 1 {
		t.Errorf("hold events = %+v, want one", evs)
	}
	if n := countToasts(h, "waits for a shell pane"); n != 1 {
		t.Errorf("hold toasts = %d, want one", n)
	}

	freeShells(h)
	h.clock.Set(deref(h.pr(2).NextAttemptAt).Add(time.Second))
	h.tick()
	if n := len(h.rd.all()); n != 3 {
		t.Fatalf("rounds = %d, want the retry once the panes are free", n)
	}
	if got := checkouts(h); got == start {
		t.Error("the retry did not check out")
	}
	if evs := kindOf(subjectEvents(t, h, "pr:talkable/talkable#2"), eventShellHoldEnded); len(evs) != 1 {
		t.Errorf("hold end events = %+v, want one", evs)
	}
	h.wantState(2, store.PRReviewed)
}

// A hold that lasts 15 minutes puts the PR under needs_attention with the
// hold's plain reason, and still with that one toast; the reconcile looks
// again every 10 minutes and puts the PR back in line once the pane is
// free.
func TestAHoldOfFifteenMinutesNeedsAttentionWithItsReason(t *testing.T) {
	h := newHarness(t)
	secondRound(t, h, barrierRound(t, h))
	h.rd.script = nil
	holdShells(h, true)
	for i := 0; i < 10 && h.pr(2).State != store.PRNeedsAttention; i++ {
		h.clock.Set(deref(h.pr(2).NextAttemptAt).Add(time.Second))
		h.tick()
	}
	pr := h.wantState(2, store.PRNeedsAttention)
	if want := "a command magnum did not start keeps the shell pane of review1 busy"; deref(pr.LastError) != want {
		t.Errorf("last_error = %q, want %q", deref(pr.LastError), want)
	}
	if kind, _ := h.e.getKV(h.ctx, KVPRAttention(pr.ID)); kind != attention.KindShellHold {
		t.Errorf("attention kind = %q", kind)
	}
	if n := countToasts(h, "waits for a shell pane") + countToasts(h, "needs attention"); n != 1 {
		t.Errorf("toasts = %d, want the hold's one", n)
	}
	if hold, _ := h.e.shellHoldOf(h.ctx, pr.ID); h.clock.Now().Sub(hold.Since) < shellHoldAttention {
		t.Errorf("needs attention after %s of the hold, want %s", h.clock.Now().Sub(hold.Since), shellHoldAttention)
	}

	h.advance(11 * time.Minute)
	_ = h.e.reconcile(h.ctx) // still held: it waits
	h.wantState(2, store.PRNeedsAttention)
	freeShells(h)
	h.advance(11 * time.Minute)
	_ = h.e.reconcile(h.ctx)
	if pr := h.pr(2); pr.State == store.PRNeedsAttention {
		t.Fatalf("state = %s after the pane was free, want the PR back in line", pr.State)
	}
	h.tick()
	h.wantState(2, store.PRReviewed)
}

// The runs a crashed daemon left in flight are closed before the hold
// looks at the slot's shell panes: magnum's own leftover there then counts
// as over, so the cleanup stops it rather than hold for it.
func TestStaleRunsAreClosedBeforeTheHoldLooks(t *testing.T) {
	h := newHarness(t)
	before := h.reviewedPR(2, "b1")
	stale, err := h.st.CreateRun(h.ctx, store.Run{PRID: before.ID, Round: 1, Role: "codex-review", Kind: pipeline.KindInitial,
		TargetSHA: "b1", Identity: before.Identity, ReviewerLogin: "x", State: store.RunSubmitted, PromptText: "p"})
	if err != nil {
		t.Fatal(err)
	}
	var stateAtLook string
	h.ag.mu.Lock()
	h.ag.onClearShells = func() {
		if r, err := h.st.RunByID(h.ctx, stale.ID); err == nil {
			stateAtLook = r.State
		}
	}
	h.ag.mu.Unlock()
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(5 * time.Minute)
	h.tick()
	if stateAtLook == "" || runInFlightState(stateAtLook) {
		t.Errorf("the stale run was %q when the hold looked, want it closed", stateAtLook)
	}
}

func runInFlightState(s string) bool {
	return s == store.RunPending || s == store.RunSubmitted || s == store.RunWorking
}

// A release resets the slot's checkout: while a command keeps a shell pane
// of the slot's PR busy it waits, as a round's setup does, and the slot is
// not reset.
func TestAReleaseWaitsForTheShellHold(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	slot, has, err := h.e.slotOf(h.ctx, h.pr(2).ID)
	if err != nil || !has {
		t.Fatalf("slot = %v, %v", has, err)
	}
	holdShells(h, true)
	pool := h.cfg.Pools[0]
	err = h.e.releaseSlot(h.ctx, slot, pool, "evicted")
	if err == nil || !strings.Contains(err.Error(), "a command magnum did not start keeps the shell pane of "+slot.Name+" busy") {
		t.Fatalf("releaseSlot = %v, want the hold", err)
	}
	if slices.ContainsFunc(h.sl.all(), func(c string) bool { return strings.HasPrefix(c, "release:") }) {
		t.Errorf("slot calls %v: the slot was released under the command", h.sl.all())
	}
}

// The fixer works in the checkout and its settle moves it: its guard sees
// the same hold, and no fixer starts.
func TestTheFixerSeesTheShellHold(t *testing.T) {
	h, _ := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	holdShells(h, false)
	h.fix(FixPayload{})
	h.tick()
	cur := h.pr(2)
	if le := deref(cur.LastError); !strings.Contains(le, "magnum cannot stop its own command in review1") {
		t.Errorf("last_error = %q, want the hold's reason", le)
	}
	if h.ag.count("submit:") != 0 {
		t.Errorf("the fixer started: %v", h.ag.all())
	}
}
