package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
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

// TestBudgetSoftCapDefersOnlyFirstReviews: at the soft cap a new PR's first
// review waits with a reason; a forced review still runs, and the PR runs
// once the budget is below the cap.
func TestBudgetSoftCapDefersOnlyFirstReviews(t *testing.T) {
	h, u := newBudgetHarness(t)
	u.set(85, h.clock.Now().Add(72*time.Hour))
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRQueued)
	if gate, _ := h.e.getKV(h.ctx, kvPRGate(pr.ID)); !strings.Contains(gate, "soft cap 80%") || !strings.Contains(gate, "first reviews wait") {
		t.Fatalf("gate = %q", gate)
	}
	if why := h.e.budgetGate("rereview", false, 2); why != "" {
		t.Fatalf("a re-review waits: %q", why)
	}
	if why := h.e.budgetGate("initial", true, 2); why != "" {
		t.Fatalf("a forced first review waits: %q", why)
	}
	if why := h.e.budgetGate("initial", false, 0); why != "" {
		t.Fatalf("a round without Codex waits: %q", why)
	}

	u.set(50, h.clock.Now().Add(72*time.Hour))
	h.advance(time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
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
