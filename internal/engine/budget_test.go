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

// fakeUsage is a Codex usage reader whose snapshots a test sets: the newest
// (read) and the one from a past time (readAt).
type fakeUsage struct {
	mu      sync.Mutex
	snap    *usage.Snapshot
	past    *usage.Snapshot
	reads   int
	pastAts []time.Time // the times readAt was asked about
}

func (f *fakeUsage) set(pct float64, resets time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = &usage.Snapshot{Window: usage.Window{UsedPercent: pct, WindowMinutes: 10080, ResetsAt: resets}, Plan: "pro", At: resets.Add(-time.Hour)}
}

// setAt is set with the snapshot stamped at (Snapshot.At).
func (f *fakeUsage) setAt(pct float64, resets, at time.Time) {
	f.set(pct, resets)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap.At = at
}

// setPast sets what readAt finds: pct of the weekly window resetting at
// resets; nothing (usage.ErrNoData) when pct is negative.
func (f *fakeUsage) setPast(pct float64, resets time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.past = nil
	if pct >= 0 {
		f.past = &usage.Snapshot{Window: usage.Window{UsedPercent: pct, WindowMinutes: 10080, ResetsAt: resets}, Plan: "pro"}
	}
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

func (f *fakeUsage) readAt(_ context.Context, _ string, at time.Time) (usage.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pastAts = append(f.pastAts, at)
	if f.past == nil {
		return usage.Snapshot{}, usage.ErrNoData
	}
	return *f.past, nil
}

func newBudgetHarness(t *testing.T) (*harness, *fakeUsage) {
	u := &fakeUsage{}
	h := newHarness(t, func(h *harness) { h.d.Usage, h.d.UsageAt = u.read, u.readAt })
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
	if gate, _ := h.e.getKV(h.ctx, store.KVPRGate(pr.ID)); !strings.Contains(gate, "soft cap 80%") || !strings.Contains(gate, "full re-reviews wait") {
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
		j := &roundJob{kind: pipeline.KindRereview, pr: store.PR{ReviewedSHA: new("b1"), HeadSHA: "b2"}}
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

// TestAResumeOverrulesTheBudgetCapUntilCodexReportsAgain: Codex's newest
// snapshot is the last session's, so after a switch to another Codex account
// it still says 97% while the pause keeps the session that would refresh it
// from running (2026-10-15: three re-reviews waited at the cap behind a
// reading the operator knew was stale, and `magnum resume` lifted nothing).
// `magnum resume --tool codex` lifts the pause and overrules the reading: the
// same snapshot read again pauses nothing and holds nothing, a later one at
// the cap pauses again, with its toast, and one below it ends the overrule.
// The CLI's offline resume leaves the same mark for the daemon's start.
func TestAResumeOverrulesTheBudgetCapUntilCodexReportsAgain(t *testing.T) {
	h, u := newBudgetHarness(t)
	resets := h.clock.Now().Add(72 * time.Hour)
	u.setAt(97, resets, h.clock.Now().Add(-time.Hour))
	h.startup()
	h.tick()
	if _, ok := h.e.toolPause(h.ctx, agents.KindCodex); !ok {
		t.Fatal("not paused at 97%")
	}
	h.advance(time.Minute)
	h.tick()
	if got := h.nh.titles("Codex budget"); len(got) != 1 {
		t.Fatalf("toasts = %v", got)
	}
	resumed := h.clock.Now()
	res, err := h.e.requestPause(h.ctx, PausePayload{Tool: agents.KindCodex}, false)
	if err != nil || res != "resumed codex (the Codex budget caps wait for Codex's next reading)" {
		t.Fatalf("resume = %q, %v", res, err)
	}
	if v, _ := h.e.getKV(h.ctx, KVUsageCodexOverruledAt); v != store.FormatTime(resumed) {
		t.Fatalf("overruled_at = %q", v)
	}
	if evs := h.events("tool.resumed"); len(evs) != 1 || evs[0].Message != "codex pause (budget_cap) lifted by magnum resume: "+BudgetOverruledNote {
		t.Fatalf("tool.resumed events = %+v", evs)
	}
	// The same snapshot, read again a minute later, pauses nothing and holds
	// no round.
	h.advance(time.Minute)
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatalf("paused again from the overruled snapshot: %+v", p)
	}
	if level, _ := h.e.budgetLevel(); level != usage.OK {
		t.Fatalf("level under the overrule = %v", level)
	}
	// Codex reports again, still at the cap: the pause is back and toasts.
	u.setAt(97, resets, h.clock.Now())
	h.advance(time.Minute)
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); !ok || p.Reason != BudgetPauseReason {
		t.Fatalf("not paused from the new snapshot: %+v %v", p, ok)
	}
	if _, ok := h.e.getKV(h.ctx, KVUsageCodexOverruledAt); ok {
		t.Fatal("overrule kept after Codex reported again")
	}
	h.advance(time.Minute)
	h.tick()
	if got := h.nh.titles("Codex budget"); len(got) != 2 {
		t.Fatalf("toasts after the new reading = %v", got)
	}
	// Lifted again, then a reading below the cap ends the overrule quietly.
	if _, err := h.e.requestPause(h.ctx, PausePayload{Tool: agents.KindCodex}, false); err != nil {
		t.Fatal(err)
	}
	u.setAt(10, resets, h.clock.Now().Add(time.Minute))
	h.advance(time.Minute)
	h.tick()
	if _, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatal("paused at 10%")
	}
	if _, ok := h.e.getKV(h.ctx, KVUsageCodexOverruledAt); ok {
		t.Fatal("overrule kept after a reading below the cap")
	}

	t.Run("the CLI's offline resume", func(t *testing.T) {
		h, u := newBudgetHarness(t)
		resets := h.clock.Now().Add(72 * time.Hour)
		u.setAt(97, resets, h.clock.Now().Add(-time.Hour))
		h.e.setKV(h.ctx, KVUsageCodexOverruledAt, store.FormatTime(h.clock.Now().Add(-time.Minute)))
		h.startup()
		h.tick()
		if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
			t.Fatalf("paused despite the CLI's mark: %+v", p)
		}
	})
}

// TestAResumeAtTheSoftCapOverrulesTheReading: at the soft cap nothing is
// paused, so `magnum resume --tool codex` overruled nothing, and the held
// full re-reviews waited behind a reading the operator knew was stale until
// some Codex turn refreshed it, which the soft cap prevents while every
// pending round is a full re-review (2026-10-21: seven re-reviews waited at
// 97% of a 95/98 config after a switch of Codex account). Now the soft-cap
// wait names that resume, which overrules the reading at either cap, a
// pause or not: its answer and a tool.resumed event say the caps wait for
// Codex's next reading, the held re-review dispatches at the next tick, and
// a snapshot stamped after the resume, still at the soft cap, holds the
// next push's re-review again. `--tool all` does the same.
func TestAResumeAtTheSoftCapOverrulesTheReading(t *testing.T) {
	for _, tool := range []string{agents.KindCodex, "all"} {
		t.Run(tool, func(t *testing.T) {
			h, u := newBudgetHarness(t)
			h.cfg.Usage.CodexSoft, h.cfg.Usage.CodexHard = 95, 98
			resets := h.clock.Now().Add(72 * time.Hour)
			u.setAt(97, resets, h.clock.Now().Add(-time.Hour))
			h.reviewedPR(2, "b1") // the first review runs at the soft cap
			pollPR(h, 40*time.Minute, 2, "b2")
			h.advance(time.Hour)
			h.tick()
			reqWantRounds(t, h, 1)
			if _, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
				t.Fatal("codex paused at the soft cap")
			}
			w := waitOf(t, h, 2)
			if w.Reason != WaitBudget {
				t.Fatalf("wait = %+v, want the soft cap", w)
			}
			if got, want := w.Short(h.clock.Now()), "re-review · Codex soft cap"; got != want {
				t.Errorf("short = %q, want %q", got, want)
			}
			const hint = "`magnum resume --tool codex` overrules a reading made stale by a switch of Codex account"
			if got := w.Sentence("talkable#2", h.clock.Now()); !strings.Contains(got, hint) {
				t.Errorf("sentence = %q, want it to name %q", got, hint)
			}

			resumed := h.clock.Now()
			res, err := h.e.requestPause(h.ctx, PausePayload{Tool: tool}, false)
			want := "resumed codex (" + BudgetOverruledNote + ")"
			if tool == "all" {
				want = "resumed " + strings.Join(h.e.kinds(), ", ") + " (" + BudgetOverruledNote + ")"
			}
			if err != nil || res != want {
				t.Fatalf("resume = %q, %v; want %q", res, err, want)
			}
			if v, _ := h.e.getKV(h.ctx, KVUsageCodexOverruledAt); v != store.FormatTime(resumed) {
				t.Fatalf("overruled_at = %q", v)
			}
			evs := h.events("tool.resumed")
			if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "soft cap") ||
				!strings.HasSuffix(evs[0].Message, ": "+BudgetOverruledNote) {
				t.Fatalf("tool.resumed events = %+v", evs)
			}

			// The same snapshot, read again, holds nothing: the re-review starts.
			h.advance(time.Minute)
			h.tick()
			if ins := h.rd.all(); len(ins) != 2 || ins[1].TargetSHA != "b2" || ins[1].Kind != pipeline.KindRereview {
				t.Fatalf("rounds = %d, want the held re-review of b2 under the overrule", len(ins))
			}

			// Codex reports again, still at the soft cap: the next push waits.
			u.setAt(97, resets, h.clock.Now())
			pollPR(h, time.Minute, 2, "b3")
			h.advance(time.Hour)
			h.tick()
			reqWantRounds(t, h, 2)
			if w := waitOf(t, h, 2); w.Reason != WaitBudget {
				t.Fatalf("wait = %+v, want the soft cap from the new reading", w)
			}
			if _, ok := h.e.getKV(h.ctx, KVUsageCodexOverruledAt); ok {
				t.Fatal("overrule kept after Codex reported again")
			}
		})
	}
}

// TestAResumeBelowTheSoftCapOverrulesNothing: a resume of the codex kind
// with Codex's reading below the soft cap leaves the caps judging it, and
// its answer is the plain one; so is a resume of a kind Codex does not back
// at the soft cap, and a second resume while an overrule is pending, which
// moves the overrule's mark not at all.
func TestAResumeBelowTheSoftCapOverrulesNothing(t *testing.T) {
	h, u := newBudgetHarness(t)
	h.cfg.Usage.CodexSoft, h.cfg.Usage.CodexHard = 95, 98
	resets := h.clock.Now().Add(72 * time.Hour)
	u.setAt(94, resets, h.clock.Now().Add(-time.Hour))
	h.startup()
	h.tick()
	if res, err := h.e.requestPause(h.ctx, PausePayload{Tool: agents.KindCodex}, false); err != nil || res != "resumed codex" {
		t.Fatalf("resume below the soft cap = %q, %v", res, err)
	}
	if v, ok := h.e.getKV(h.ctx, KVUsageCodexOverruledAt); ok {
		t.Fatalf("overruled below the soft cap at %q", v)
	}

	u.setAt(97, resets, h.clock.Now())
	h.advance(time.Minute)
	h.tick()
	if res, err := h.e.requestPause(h.ctx, PausePayload{Tool: agents.KindClaude}, false); err != nil || res != "resumed claude" {
		t.Fatalf("resume of claude at the soft cap = %q, %v", res, err)
	}
	if _, ok := h.e.getKV(h.ctx, KVUsageCodexOverruledAt); ok {
		t.Fatal("a resume of claude overruled Codex's reading")
	}
	if evs := h.events("tool.resumed"); len(evs) != 0 {
		t.Fatalf("tool.resumed events = %+v", evs)
	}

	if _, err := h.e.requestPause(h.ctx, PausePayload{Tool: agents.KindCodex}, false); err != nil {
		t.Fatal(err)
	}
	first, _ := h.e.getKV(h.ctx, KVUsageCodexOverruledAt)
	h.advance(10 * time.Second)
	if res, err := h.e.requestPause(h.ctx, PausePayload{Tool: agents.KindCodex}, false); err != nil || res != "resumed codex" {
		t.Fatalf("a second resume under the overrule = %q, %v", res, err)
	}
	if v, _ := h.e.getKV(h.ctx, KVUsageCodexOverruledAt); first == "" || v != first {
		t.Fatalf("overruled_at moved from %q to %q", first, v)
	}
	if n := len(h.events("tool.resumed")); n != 1 {
		t.Fatalf("tool.resumed events = %d, want the first resume's", n)
	}
}
