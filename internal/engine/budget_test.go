package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/usage"
)

// fakeUsage is a Codex usage reader whose snapshot a test sets.
type fakeUsage struct {
	mu    sync.Mutex
	snap  *usage.Snapshot
	reads int
}

func (f *fakeUsage) set(pct float64, resets time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = &usage.Snapshot{Window: usage.Window{UsedPercent: pct, WindowMinutes: 10080, ResetsAt: resets}, Plan: "pro", At: resets.Add(-time.Hour)}
}

func (f *fakeUsage) read(context.Context, string, time.Time) (usage.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.snap == nil {
		return usage.Snapshot{}, usage.ErrNoData
	}
	return *f.snap, nil
}

func newBudgetHarness(t *testing.T) (*harness, *fakeUsage) {
	u := &fakeUsage{}
	h := newHarness(t, func(h *harness) { h.d.Usage = u.read })
	return h, u
}

// TestBudgetIsReadOnceAMinuteAndRecorded: the health tick reads Codex's
// snapshot at most once a minute and writes the gauge to kv; status and the
// tab bar show it.
func TestBudgetIsReadOnceAMinuteAndRecorded(t *testing.T) {
	h, u := newBudgetHarness(t)
	resets := h.clock.Now().Add(72 * time.Hour)
	u.set(42, resets)
	h.startup()
	h.tick()
	h.tick()
	if u.reads != 1 {
		t.Fatalf("reads within a minute = %d", u.reads)
	}
	h.advance(time.Minute)
	h.tick()
	if u.reads != 2 {
		t.Fatalf("reads after a minute = %d", u.reads)
	}
	for key, want := range map[string]string{KVUsageCodexPercent: "42", KVUsageCodexPlan: "pro", KVUsageCodexWindow: "10080",
		KVUsageCodexResetsAt: store.FormatTime(resets)} {
		if v, _ := h.e.getKV(h.ctx, key); v != want {
			t.Errorf("kv %s = %q, want %q", key, v, want)
		}
	}
	if bar := h.e.tabBar(h.ctx); !strings.HasSuffix(bar, " · codex 42%") {
		t.Fatalf("tab bar = %q", bar)
	}
}

// TestBudgetSoftCapHoldsFullReReviews: at the soft cap a new PR's first
// review starts, a push's full re-review waits with a short reason the
// board shows ("re-review · Codex soft cap"), a review request runs it at
// once, and an automatic one runs once the budget is below the cap.
func TestBudgetSoftCapHoldsFullReReviews(t *testing.T) {
	h, u := newBudgetHarness(t)
	u.set(85, h.clock.Now().Add(72*time.Hour))
	h.reviewedPR(2, "b1") // the first review runs at the soft cap
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(time.Hour) // far past the quiet period and the interval
	h.tick()
	reqWantRounds(t, h, 1)
	pr := h.wantState(2, store.PRRereviewPending)
	if gate, _ := h.e.getKV(h.ctx, kvPRGate(pr.ID)); !strings.Contains(gate, "soft cap 80%") || !strings.Contains(gate, "full re-reviews wait") {
		t.Fatalf("gate = %q", gate)
	}
	w := waitOf(t, h, 2)
	if w.Reason != WaitBudget || !w.Rereview || w.DeltaCheck {
		t.Fatalf("wait = %+v, want the soft cap on a full re-review", w)
	}
	if got, want := w.Short(h.clock.Now()), "re-review · Codex soft cap"; got != want {
		t.Errorf("short = %q, want %q", got, want)
	}
	if got := w.Sentence("talkable#2", h.clock.Now()); !strings.Contains(got, "full re-reviews wait") ||
		!strings.HasSuffix(got, "`magnum review talkable#2` runs it now") {
		t.Errorf("sentence = %q", got)
	}

	// A review request runs the held re-review.
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
	h.advance(time.Minute)
	h.tick()
	if ins := h.rd.all(); len(ins) != 2 || ins[1].TargetSHA != "b2" || ins[1].Kind != pipeline.KindRereview {
		t.Fatalf("rounds = %d, want the requested re-review of b2", len(ins))
	}

	// The next push is automatic again: held until the budget is below the cap.
	pollPR(h, time.Minute, 2, "b3")
	h.advance(time.Hour)
	h.tick()
	reqWantRounds(t, h, 2)
	if w := waitOf(t, h, 2); w.Reason != WaitBudget {
		t.Fatalf("wait = %+v, want the soft cap", w)
	}
	u.set(50, h.clock.Now().Add(72*time.Hour))
	h.advance(time.Minute)
	h.tick()
	if ins := h.rd.all(); len(ins) != 3 || ins[2].TargetSHA != "b3" {
		t.Fatalf("rounds = %d, want the re-review of b3 below the cap", len(ins))
	}
}

// TestBudgetSoftCapLetsADeltaCheckRun: at the soft cap a small delta's
// check by the judge alone starts after the quiet period.
func TestBudgetSoftCapLetsADeltaCheckRun(t *testing.T) {
	h, u := newBudgetHarness(t)
	u.set(85, h.clock.Now().Add(72*time.Hour))
	pushedWithDelta(t, h, rubyMixed) // b2 at 10:45: 2 lines
	if w := waitOf(t, h, 2); !w.DeltaCheck || w.Reason != WaitQuiet {
		t.Fatalf("wait = %+v, want the delta check after the quiet period", w)
	}
	h.advance(5 * time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].DeltaCheck == nil || ins[1].TargetSHA != "b2" {
		t.Fatalf("rounds = %d, want the delta check of b2 at the soft cap", len(ins))
	}
}

// TestBudgetSoftCapHoldsOnlyAutomaticFullReReviews: the soft cap's gate for
// each kind of round. A full re-review whose roles run Codex waits; a first
// review, a delta check, a re-review of the same head, a reply round, a
// continue, a forced or requested round and a round without Codex start,
// and nothing waits for it below the soft cap or at the hard cap (the kind
// pause holds rounds there).
func TestBudgetSoftCapHoldsOnlyAutomaticFullReReviews(t *testing.T) {
	h, u := newBudgetHarness(t)
	u.set(85, h.clock.Now().Add(72*time.Hour))
	h.startup()
	h.tick()
	rereview := func(mod func(j *roundJob)) *roundJob {
		j := &roundJob{kind: pipeline.KindRereview, pr: store.PR{ReviewedSHA: store.Ptr("b1"), HeadSHA: "b2"}}
		if mod != nil {
			mod(j)
		}
		return j
	}
	for _, tc := range []struct {
		name  string
		job   *roundJob
		codex int
		waits bool
	}{
		{"a full re-review", rereview(nil), 2, true},
		{"a first review", &roundJob{kind: pipeline.KindInitial}, 2, false},
		{"a delta check", rereview(func(j *roundJob) { j.deltaCheck = true }), 1, false},
		{"a re-review of the same head", rereview(func(j *roundJob) { j.sameHead = true }), 1, false},
		{"a reply round", rereview(func(j *roundJob) { j.sameHead, j.replies = true, 2 }), 1, false},
		{"a continued turn", rereview(func(j *roundJob) { j.kind, j.continued = kindContinue, true }), 2, false},
		{"a continue whose checkout is gone", rereview(func(j *roundJob) { j.continued = true }), 2, false},
		{"a forced re-review", rereview(func(j *roundJob) { j.pr.Forced = true }), 2, false},
		{"a requested re-review", rereview(func(j *roundJob) { j.requested = true }), 2, false},
		{"a re-review without Codex", rereview(nil), 0, false},
	} {
		if why := h.e.budgetGate(tc.job, tc.codex); (why != "") != tc.waits {
			t.Errorf("%s: gate = %q, want waiting %v", tc.name, why, tc.waits)
		}
	}
	for _, pct := range []float64{50, 96} {
		u.set(pct, h.clock.Now().Add(72*time.Hour))
		h.advance(time.Minute)
		h.tick()
		if why := h.e.budgetGate(rereview(nil), 2); why != "" {
			t.Errorf("at %.0f%%: a full re-review waits for the soft cap: %q", pct, why)
		}
	}
}

// TestBudgetHardCapPausesCodexUntilBelow: at the hard cap the codex kind
// pauses through the tool-pause path with one urgent toast; the pause is
// not lifted by its own expiry but by the budget dropping below the cap.
func TestBudgetHardCapPausesCodexUntilBelow(t *testing.T) {
	h, u := newBudgetHarness(t)
	resets := h.clock.Now().Add(2 * time.Hour)
	u.set(96, resets)
	h.startup()
	h.tick()
	p, ok := h.e.toolPause(h.ctx, agents.KindCodex)
	if !ok || p.Reason != BudgetPauseReason || !p.Until.Equal(resets) || !strings.Contains(p.Detail, "96% used") {
		t.Fatalf("codex pause = %+v %v", p, ok)
	}
	if why := h.e.kindPauseReason(h.ctx, []string{agents.KindCodex}); !strings.Contains(why, BudgetPauseReason) {
		t.Fatalf("kindPauseReason = %q", why)
	}
	if _, ok := h.e.toolPause(h.ctx, agents.KindClaude); ok {
		t.Fatal("claude paused by the Codex budget")
	}
	h.advance(time.Minute)
	h.tick()
	if got := h.nh.titles("Codex budget"); len(got) != 1 || got[0] != "magnum: Codex budget at 96%" {
		t.Fatalf("toasts = %v", got)
	}

	u.set(90, resets)
	h.advance(time.Minute)
	h.tick()
	if _, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatal("codex still paused below the hard cap")
	}
	// The next time the cap is hit it toasts again at once.
	if ok, err := h.st.ShouldSend(h.ctx, budgetToastKey, time.Hour); err != nil || !ok {
		t.Fatalf("budget toast key kept: %v %v", ok, err)
	}
}

// TestBudgetWindowResetEndsTheHardCap: once the window's reset passes the
// cached snapshot reads 0% and the pause ends, even without a new read.
func TestBudgetWindowResetEndsTheHardCap(t *testing.T) {
	h, u := newBudgetHarness(t)
	resets := h.clock.Now().Add(30 * time.Minute)
	u.set(99, resets)
	h.startup()
	h.tick()
	if _, ok := h.e.toolPause(h.ctx, agents.KindCodex); !ok {
		t.Fatal("not paused at 99%")
	}
	u.mu.Lock()
	u.snap = nil // Codex wrote nothing new
	u.mu.Unlock()
	h.advance(31 * time.Minute)
	h.tick()
	if _, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatal("still paused after the window reset")
	}
	if v, _ := h.e.getKV(h.ctx, KVUsageCodexPercent); v != "0" {
		t.Fatalf("recorded percent after the reset = %q", v)
	}
}
