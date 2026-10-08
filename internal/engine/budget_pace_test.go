package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/usage"
)

const paceWeek = 7 * 24 * time.Hour

// paceResets is the reset of a weekly window that is share percent elapsed at
// now.
func paceResets(now time.Time, share float64) time.Time {
	return now.Add(time.Duration((100 - share) / 100 * float64(paceWeek)))
}

// paceEvents counts the events of a kind about the Codex kind.
func paceEvents(h *harness, kind string) int {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, "tool:"+agents.KindCodex, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// paceToasts runs the tick after the batcher's window, which flushes what
// the checks queued, and returns the toasts about the Codex budget.
func paceToasts(h *harness) []string {
	h.t.Helper()
	h.advance(2 * time.Minute)
	h.tick()
	return h.nh.titles("Codex budget")
}

// paceBody is the body of the last toast about the Codex budget.
func paceBody(h *harness) string {
	var body string
	for _, toast := range h.nh.all() {
		if strings.Contains(toast, "Codex budget") {
			_, body, _ = strings.Cut(toast, " | ")
		}
	}
	return body
}

// TestBudgetPaceWarnsOnceWhenSoftCapComesBeforeTheReset: half the weekly
// budget used with 18% of the window elapsed (2.8x the sustainable rate)
// reaches codex_soft (80%) about two days into the window, long before the
// reset; the operator is told once, in one toast and one event.
func TestBudgetPaceWarnsOnceWhenSoftCapComesBeforeTheReset(t *testing.T) {
	h, u := newBudgetHarness(t)
	now := h.clock.Now()
	resets := paceResets(now, 18)
	u.set(50, resets)
	h.startup()
	h.tick()

	pace, _ := usage.PaceOf(50, 10080, resets, now)
	reach, ok := pace.Reach(80)
	if !ok {
		t.Fatal("test setup: 80% is not reached before the reset")
	}
	if got := paceToasts(h); len(got) != 1 || got[0] != "magnum: Codex budget at 50%" {
		t.Fatalf("toasts = %v", got)
	}
	body := paceBody(h)
	for _, want := range []string{
		"reaches 80% (codex_soft) " + reach.Local().Format("Mon 15:04"),
		"before the reset " + resets.Local().Format("Mon 15:04"),
		"full re-reviews will wait",
		"Pace 2.8x",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("toast body lacks %q:\n%s", want, body)
		}
	}
	if n := paceEvents(h, "usage.pace"); n != 1 {
		t.Fatalf("usage.pace events = %d", n)
	}
	// Nothing is paused or held: this is a warning.
	if _, paused := h.e.toolPause(h.ctx, agents.KindCodex); paused {
		t.Fatal("codex paused by a pace warning")
	}
}

// TestBudgetPaceDoesNotRepeatInTheSameWindow: later checks of the same
// window stay quiet, and so does a daemon that restarted in between (the
// dedupe lives in the registry, not in memory).
func TestBudgetPaceDoesNotRepeatInTheSameWindow(t *testing.T) {
	h, u := newBudgetHarness(t)
	u.set(50, paceResets(h.clock.Now(), 18))
	h.startup()
	h.tick()
	if got := paceToasts(h); len(got) != 1 {
		t.Fatalf("first toasts = %v", got)
	}
	for range 3 {
		if got := paceToasts(h); len(got) != 1 {
			t.Fatalf("toasts after another check = %v", got)
		}
	}

	h.e = New(h.d) // the daemon restarts on the same registry
	h.startup()
	h.tick()
	if got := paceToasts(h); len(got) != 1 {
		t.Fatalf("toasts after a restart = %v", got)
	}
	if n := paceEvents(h, "usage.pace"); n != 1 {
		t.Fatalf("usage.pace events = %d", n)
	}
}

// TestBudgetPaceWarnsAgainInANewWindow: the next window (a new reset time) is
// judged on its own.
func TestBudgetPaceWarnsAgainInANewWindow(t *testing.T) {
	h, u := newBudgetHarness(t)
	first := paceResets(h.clock.Now(), 18)
	u.set(50, first)
	h.startup()
	h.tick()
	if got := paceToasts(h); len(got) != 1 {
		t.Fatalf("first window toasts = %v", got)
	}

	h.clock.Add(first.Sub(h.clock.Now()) + time.Duration(0.18*float64(paceWeek)))
	u.set(50, first.Add(paceWeek))
	h.tick()
	if got := paceToasts(h); len(got) != 2 {
		t.Fatalf("toasts after the window reset = %v", got)
	}
	if n := paceEvents(h, "usage.pace"); n != 2 {
		t.Fatalf("usage.pace events = %d", n)
	}
}

// TestBudgetPaceStaysQuietWhenNothingIsAhead covers what is no warning: a
// pace that reaches codex_soft only after the reset, a budget already at or
// above codex_soft (the soft gate speaks then), the first tenth of a window
// (one burst is no pace), a window with no known length and nothing used.
func TestBudgetPaceStaysQuietWhenNothingIsAhead(t *testing.T) {
	tests := []struct {
		name        string
		used, share float64
		unknownLen  bool
	}{
		{name: "pace reaches the soft cap after the reset", used: 20, share: 50},
		{name: "already above the soft cap", used: 85, share: 18},
		{name: "exactly at the soft cap", used: 80, share: 18},
		{name: "first tenth of the window", used: 8, share: 5},
		{name: "nothing used yet", used: 0, share: 30},
		{name: "unknown window length", used: 50, share: 18, unknownLen: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, u := newBudgetHarness(t)
			u.set(tt.used, paceResets(h.clock.Now(), tt.share))
			if tt.unknownLen {
				u.snap.WindowMinutes = 0
			}
			h.startup()
			h.tick()
			if got := paceToasts(h); len(got) != 0 {
				t.Fatalf("toasts = %v", got)
			}
			if n := paceEvents(h, "usage.pace"); n != 0 {
				t.Fatalf("usage.pace events = %d", n)
			}
		})
	}
}

// TestBudgetPaceIsOffWithoutASoftCap: [usage] codex_soft = 0 turns the soft
// cap off, so there is nothing to project.
func TestBudgetPaceIsOffWithoutASoftCap(t *testing.T) {
	h, u := newBudgetHarness(t)
	h.cfg.Usage.CodexSoft = 0
	u.set(50, paceResets(h.clock.Now(), 18))
	h.startup()
	h.tick()
	if got := paceToasts(h); len(got) != 0 {
		t.Fatalf("toasts = %v", got)
	}
	if n := paceEvents(h, "usage.pace"); n != 0 {
		t.Fatalf("usage.pace events = %d", n)
	}
}

// TestBudgetPaceDryRunOnlyRecords: a dry run shows no toast and writes no
// usage.pace event; it records what it would have done.
func TestBudgetPaceDryRunOnlyRecords(t *testing.T) {
	u := &fakeUsage{}
	h := newHarness(t, func(h *harness) { h.d.Usage = u.read; h.d.DryRun = true })
	u.set(50, paceResets(h.clock.Now(), 18))
	h.e.checkBudget(h.ctx)
	h.advance(2 * time.Minute)
	h.e.checkBudget(h.ctx)
	h.settle()
	if got := h.nh.all(); len(got) != 0 {
		t.Fatalf("toasts in a dry run: %v", got)
	}
	if n := paceEvents(h, "usage.pace"); n != 0 {
		t.Fatalf("usage.pace events in a dry run = %d", n)
	}
	if n := paceEvents(h, "dryrun.pace"); n != 1 {
		t.Fatalf("dryrun.pace records = %d", n)
	}
}

// TestBudgetPaceNamesTheLastDayNextToTheWindowAverage: the window's average
// (2.8x) counts what was spent days ago; Codex's reading from a day ago says
// 10 of the 50 points went in the last 24h, a seventh of the window (0.7x).
// The toast names both, and status gets the last day's pace from kv.
func TestBudgetPaceNamesTheLastDayNextToTheWindowAverage(t *testing.T) {
	h, u := newBudgetHarness(t)
	now := h.clock.Now()
	resets := paceResets(now, 18)
	u.set(50, resets)
	u.setPast(40, resets.Add(time.Second)) // the reset jitters between readings
	h.startup()
	h.tick()

	if len(u.pastAts) != 1 || !u.pastAts[0].Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("readings asked for = %v, want one at %v", u.pastAts, now.Add(-24*time.Hour))
	}
	if got := paceToasts(h); len(got) != 1 {
		t.Fatalf("toasts = %v", got)
	}
	if body := paceBody(h); !strings.HasSuffix(body, "full re-reviews will wait. Pace 2.8x since the reset, 0.7x in the last 24h.") {
		t.Fatalf("toast body = %q", body)
	}
	if v, _ := h.e.getKV(h.ctx, KVUsageCodexPace24h); v != "0.70" {
		t.Fatalf("kv %s = %q, want 0.70", KVUsageCodexPace24h, v)
	}
}

// TestBudgetPaceSaysOnlySinceTheResetWithoutTheLastDay: with no reading
// from a day ago the toast names the window's average alone, and a last-day
// pace a previous daemon recorded is deleted.
func TestBudgetPaceSaysOnlySinceTheResetWithoutTheLastDay(t *testing.T) {
	h, u := newBudgetHarness(t)
	u.set(50, paceResets(h.clock.Now(), 18))
	h.e.setKV(h.ctx, KVUsageCodexPace24h, "1.10")
	h.startup()
	h.tick()

	if got := paceToasts(h); len(got) != 1 {
		t.Fatalf("toasts = %v", got)
	}
	if body := paceBody(h); !strings.HasSuffix(body, "full re-reviews will wait. Pace 2.8x since the reset.") {
		t.Fatalf("toast body = %q", body)
	}
	if v, ok := h.e.getKV(h.ctx, KVUsageCodexPace24h); ok {
		t.Fatalf("kv %s = %q, want none", KVUsageCodexPace24h, v)
	}
}

// TestBudgetLastDayPaceIsReadEveryQuarterHour: the reading from a day ago is
// read at most every 15 minutes; one that finds nothing keeps the previous
// reading, and one of the window before a reset (not comparable) deletes the
// recorded pace.
func TestBudgetLastDayPaceIsReadEveryQuarterHour(t *testing.T) {
	h, u := newBudgetHarness(t)
	resets := paceResets(h.clock.Now(), 50)
	u.set(50, resets)
	u.setPast(45, resets)
	h.startup()
	h.tick()
	h.advance(10 * time.Minute)
	h.tick()
	if len(u.pastAts) != 1 {
		t.Fatalf("readings within 15 minutes = %d", len(u.pastAts))
	}
	if v, _ := h.e.getKV(h.ctx, KVUsageCodexPace24h); v != "0.35" {
		t.Fatalf("kv %s = %q, want 0.35", KVUsageCodexPace24h, v)
	}

	u.setPast(-1, time.Time{})
	h.advance(5 * time.Minute)
	h.tick()
	if len(u.pastAts) != 2 {
		t.Fatalf("readings after 15 minutes = %d", len(u.pastAts))
	}
	if v, _ := h.e.getKV(h.ctx, KVUsageCodexPace24h); v != "0.35" {
		t.Fatalf("kv %s = %q after a read that found nothing, want 0.35 kept", KVUsageCodexPace24h, v)
	}

	u.setPast(70, resets.Add(-paceWeek))
	h.advance(15 * time.Minute)
	h.tick()
	if v, ok := h.e.getKV(h.ctx, KVUsageCodexPace24h); ok {
		t.Fatalf("kv %s = %q across a reset, want none", KVUsageCodexPace24h, v)
	}
}
