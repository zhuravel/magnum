package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// pr.merged_unreviewed named no cause (10 events on 2026-10-21 to 23: 3
// merged inside the quiet period, 5 in the middle of a round, 2 waited on a
// paused kind, told apart by hand, spec 07 of the 2026-10-23 run). Its data
// now carries gate, what held the PR from its round at the merge (a wait
// reason of wait.go: quiet, interval, kind, ...; round_in_flight; paused;
// needs_attention), and eligible_at, when that gate ends, where it is known.

// mergedGate is the gate, gate subject and eligible_at of PR #2's one
// pr.merged_unreviewed event.
func mergedGate(t *testing.T, h *harness) (gate, subject string, eligible time.Time, hasEligible bool) {
	t.Helper()
	evs := h.mergedUnreviewedEvents(2)
	if len(evs) != 1 {
		t.Fatalf("pr.merged_unreviewed events = %d, want 1", len(evs))
	}
	var data struct {
		Gate        string    `json:"gate"`
		GateSubject string    `json:"gate_subject"`
		EligibleAt  time.Time `json:"eligible_at"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatalf("event data %s: %v", evs[0].Data, err)
	}
	return data.Gate, data.GateSubject, data.EligibleAt, !data.EligibleAt.IsZero()
}

// A PR that merged inside the push quiet period says so, and when the quiet
// period would have ended.
func TestMergedInsideTheQuietPeriodSaysSo(t *testing.T) {
	h := newHarness(t)
	pushedAfter(t, h, 40*time.Minute)
	until := h.pr(2).HeadChangedAt.Add(5 * time.Minute)
	h.advance(time.Minute)
	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})
	gate, _, eligible, has := mergedGate(t, h)
	if gate != WaitQuiet || !has || !eligible.Equal(until) {
		t.Fatalf("gate %q eligible_at %v (%v), want %s until %v", gate, eligible, has, WaitQuiet, until)
	}
}

// A PR that merged while its round ran says so; it was eligible already, and
// the event gives no time.
func TestMergedInTheMiddleOfARoundSaysSo(t *testing.T) {
	h := newHarness(t)
	pr := h.runningRound(2, "h2", false)
	h.confirmCloseMidRound(2, "MERGED")
	for deadline := time.Now().Add(10 * time.Second); h.e.roundActive(pr.ID); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the round still runs on a merged PR")
		}
	}
	h.settle()
	gate, _, _, has := mergedGate(t, h)
	if gate != "round_in_flight" || has {
		t.Fatalf("gate %q, has eligible_at %v, want round_in_flight and none", gate, has)
	}
}

// A PR that waited on a paused kind says so, with the kind and the end of
// the pause.
func TestMergedWhileItsKindWasPausedSaysSo(t *testing.T) {
	h := newHarness(t)
	pushedAfter(t, h, 40*time.Minute)
	h.advance(10 * time.Minute)
	until := h.clock.Now().Add(time.Hour)
	h.e.setKV(h.ctx, store.KVToolPausedUntil("codex"), store.FormatTime(until))
	h.e.setKV(h.ctx, store.KVToolPausedReason("codex"), "usage_limit")
	h.tick()
	h.advance(time.Minute)
	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})
	gate, subject, eligible, has := mergedGate(t, h)
	if gate != WaitKind || subject != "codex" || !has || !eligible.Equal(until) {
		t.Fatalf("gate %q subject %q eligible_at %v (%v), want %s codex until %v", gate, subject, eligible, has, WaitKind, until)
	}
}

// A PR whose round was paused (a usage limit) when it merged says so, with
// the time the round would have continued.
func TestMergedWhilePausedSaysSo(t *testing.T) {
	h := newHarness(t)
	h.reviewedThenPushed()
	retry := h.clock.Now().Add(2 * time.Hour)
	pr := h.pr(2)
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRRereviewPending}, store.PRPaused, func(u *store.PRUpdate) {
		u.Set("next_attempt_at", retry)
	}); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})
	gate, _, eligible, has := mergedGate(t, h)
	if gate != store.PRPaused || !has || !eligible.Equal(retry) {
		t.Fatalf("gate %q eligible_at %v (%v), want paused until %v", gate, eligible, has, retry)
	}
}
