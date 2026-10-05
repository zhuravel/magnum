package usage

import (
	"math"
	"testing"
	"time"
)

const weekMinutes = 7 * 24 * 60

// weeklyAt is a weekly window that resets at resets, observed when share
// percent of it (0–100) has elapsed.
func weeklyAt(resets time.Time, share float64) time.Time {
	start := resets.Add(-weekMinutes * time.Minute)
	return start.Add(time.Duration(share / 100 * float64(weekMinutes) * float64(time.Minute)))
}

func TestPaceOfRefusesWhatItCannotPlace(t *testing.T) {
	resets := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	start := resets.Add(-weekMinutes * time.Minute)
	tests := []struct {
		name    string
		minutes int
		resets  time.Time
		now     time.Time
		ok      bool
	}{
		{"inside the window", weekMinutes, resets, resets.Add(-24 * time.Hour), true},
		{"at the start", weekMinutes, resets, start, true},
		{"before the start", weekMinutes, resets, start.Add(-time.Second), false},
		{"at the reset", weekMinutes, resets, resets, false},
		{"after the reset", weekMinutes, resets, resets.Add(time.Hour), false},
		{"length unknown", 0, resets, resets.Add(-time.Hour), false},
		{"negative length", -5, resets, resets.Add(-time.Hour), false},
		{"reset unknown", weekMinutes, time.Time{}, resets.Add(-time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := PaceOf(50, tt.minutes, tt.resets, tt.now)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (%+v)", ok, tt.ok, p)
			}
			if ok && (!p.Start.Equal(start) || !p.ResetsAt.Equal(resets) || !p.Now.Equal(tt.now) || p.Used != 50) {
				t.Fatalf("pace = %+v", p)
			}
		})
	}
}

// TestPaceReachProjectsSoftCapBeforeReset: a weekly window 18% elapsed with
// half its budget used (2.78x the sustainable rate) reaches 80% at 28.8% of
// the window, 2 days and 23 minutes after it began, 4 days before the reset.
func TestPaceReachProjectsSoftCapBeforeReset(t *testing.T) {
	resets := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	now := weeklyAt(resets, 18)
	p, ok := PaceOf(50, weekMinutes, resets, now)
	if !ok {
		t.Fatal("no pace")
	}
	at, ok := p.Reach(80)
	if !ok {
		t.Fatal("80% not projected before the reset")
	}
	want := p.Start.Add(48*time.Hour + 23*time.Minute + 2*time.Second + 400*time.Millisecond)
	if d := at.Sub(want).Abs(); d > time.Second {
		t.Fatalf("reach = %v, want %v (off by %v)", at, want, d)
	}
	if got := at.Sub(p.Start).Hours() / (7 * 24); math.Abs(got-0.288) > 1e-6 {
		t.Fatalf("reached at %.4f of the window, want 0.288", got)
	}
	if !at.After(now) || !at.Before(resets) {
		t.Fatalf("reach %v is not between now %v and the reset %v", at, now, resets)
	}
}

func TestPaceReachFalseWhenNotBeforeReset(t *testing.T) {
	resets := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		used  float64
		share float64
		pct   float64
		ok    bool
	}{
		{"a pace that reaches 80% at 64% of the window", 50, 40, 80, true},
		{"slower than sustainable never reaches 80%", 20, 50, 80, false},
		{"the sustainable rate reaches 100% exactly at the reset", 50, 50, 100, false},
		{"the sustainable rate reaches 99% just before it", 50, 50, 99, true},
		{"half the sustainable rate reaches 49% at 98% of the window", 25, 50, 49, true},
		{"half the sustainable rate reaches 50% at the reset", 25, 50, 50, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := PaceOf(tt.used, weekMinutes, resets, weeklyAt(resets, tt.share))
			if !ok {
				t.Fatal("no pace")
			}
			_, ok = p.Reach(tt.pct)
			if ok != tt.ok {
				t.Fatalf("Reach(%g) ok = %v, want %v", tt.pct, ok, tt.ok)
			}
		})
	}
}

func TestPaceReachFalseWhenThereIsNothingToProject(t *testing.T) {
	resets := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	now := weeklyAt(resets, 18)
	tests := []struct {
		name string
		used float64
		pct  float64
		now  time.Time
	}{
		{"already reached", 80, 80, now},
		{"already passed", 91, 80, now},
		{"nothing used yet", 0, 80, now},
		{"negative use", -1, 80, now},
		{"no threshold", 50, 0, now},
		{"negative threshold", 50, -80, now},
		{"nothing elapsed", 50, 80, resets.Add(-weekMinutes * time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := PaceOf(tt.used, weekMinutes, resets, tt.now)
			if !ok {
				t.Fatal("no pace")
			}
			if at, ok := p.Reach(tt.pct); ok {
				t.Fatalf("Reach = %v, want none", at)
			}
		})
	}
}

func TestPaceElapsedAndRatio(t *testing.T) {
	resets := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		used, share  float64
		elapsed, rat float64
	}{
		{"sustainable", 50, 50, 50, 1},
		{"2.7 times too fast", 54, 20, 20, 2.7},
		{"a quarter of the rate", 10, 40, 40, 0.25},
		{"nothing elapsed", 5, 0, 0, 0},
		{"nothing used", 0, 30, 30, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := PaceOf(tt.used, weekMinutes, resets, weeklyAt(resets, tt.share))
			if !ok {
				t.Fatal("no pace")
			}
			if got := p.Elapsed(); math.Abs(got-tt.elapsed) > 1e-9 {
				t.Errorf("Elapsed = %v, want %v", got, tt.elapsed)
			}
			if got := p.Ratio(); math.Abs(got-tt.rat) > 1e-9 {
				t.Errorf("Ratio = %v, want %v", got, tt.rat)
			}
		})
	}
}
