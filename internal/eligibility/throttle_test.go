package eligibility

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

func dur(d time.Duration) config.Duration { return config.Duration{Duration: d} }

// daemon mirrors the shipped config.toml throttle settings.
func daemon() config.Daemon {
	return config.Daemon{
		PushQuietPeriod:          dur(5 * time.Minute),
		MinRereviewInterval:      dur(30 * time.Minute),
		DraftMinRereviewInterval: dur(2 * time.Hour),
		MaxRoundsPerPRPerDay:     6,
	}
}

var now = time.Date(2026, 1, 14, 12, 0, 0, 0, time.UTC)

const (
	reasonQuiet    = "waiting for push quiet period (5m0s)"
	reasonInterval = "waiting for min re-review interval (30m0s)"
	reasonDraft    = "waiting for draft re-review interval (2h0m0s)"
	reasonCap      = "daily round cap reached (6 per day)"
	reviewed       = "abc123"
)

var wantMidnight = time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return now.Add(-d) }

// throttleCase is one table row. When ready, NextEligibleAt must equal now;
// otherwise it must equal next.
type throttleCase struct {
	name   string
	f      PRFacts
	ready  bool
	next   time.Time
	reason string
}

func runThrottle(t *testing.T, tests []throttleCase) {
	t.Helper()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Throttle(daemon(), tc.f, now)
			if got.Ready != tc.ready {
				t.Fatalf("Ready = %v (%+v), want %v", got.Ready, got, tc.ready)
			}
			wantNext := tc.next
			if tc.ready {
				wantNext = now
			}
			if !got.NextEligibleAt.Equal(wantNext) {
				t.Fatalf("NextEligibleAt = %v, want %v", got.NextEligibleAt, wantNext)
			}
			if got.Reason != tc.reason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.reason)
			}
		})
	}
}

func TestThrottleFirstReview(t *testing.T) {
	runThrottle(t, []throttleCase{
		{
			name:  "quiet for longer than the period",
			f:     PRFacts{HeadChangedAt: ago(10 * time.Minute)},
			ready: true,
		},
		{
			name:  "quiet for exactly the period is ready",
			f:     PRFacts{HeadChangedAt: ago(5 * time.Minute)},
			ready: true,
		},
		{
			name:   "one second short of the period waits",
			f:      PRFacts{HeadChangedAt: ago(5*time.Minute - time.Second)},
			next:   now.Add(time.Second),
			reason: reasonQuiet,
		},
		{
			name:   "fresh push waits the whole period",
			f:      PRFacts{HeadChangedAt: now},
			next:   now.Add(5 * time.Minute),
			reason: reasonQuiet,
		},
		{
			name:  "unknown push time does not block",
			f:     PRFacts{},
			ready: true,
		},
		{
			name:  "first review ignores the re-review interval",
			f:     PRFacts{HeadChangedAt: ago(10 * time.Minute), LastRoundStartedAt: ago(time.Minute)},
			ready: true,
		},
		{
			name:  "first review ignores the daily cap",
			f:     PRFacts{HeadChangedAt: ago(10 * time.Minute), RoundsToday: 99},
			ready: true,
		},
		{
			name:  "first review of a draft uses the quiet period, not the draft interval",
			f:     PRFacts{HeadChangedAt: ago(6 * time.Minute), IsDraft: true, LastRoundStartedAt: ago(time.Minute)},
			ready: true,
		},
		{
			name:  "first review ignores a recent PendingSince",
			f:     PRFacts{HeadChangedAt: ago(10 * time.Minute), PendingSince: ago(time.Minute)},
			ready: true,
		},
		{
			name:  "forced first review ignores a fresh push",
			f:     PRFacts{HeadChangedAt: now, Forced: true},
			ready: true,
		},
	})
}

func TestThrottleRereview(t *testing.T) {
	runThrottle(t, []throttleCase{
		{
			name:  "all constraints satisfied",
			f:     PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(10 * time.Minute), LastRoundStartedAt: ago(time.Hour), RoundsToday: 1},
			ready: true,
		},
		{
			name:  "never started a round before: no interval constraint",
			f:     PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(10 * time.Minute)},
			ready: true,
		},

		// quiet period
		{
			name:  "quiet period boundary is inclusive",
			f:     PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(5 * time.Minute), LastRoundStartedAt: ago(time.Hour)},
			ready: true,
		},
		{
			name:   "quiet period not yet over",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(4 * time.Minute), LastRoundStartedAt: ago(time.Hour)},
			next:   now.Add(time.Minute),
			reason: reasonQuiet,
		},
		{
			name:  "unknown push time does not block a re-review",
			f:     PRFacts{ReviewedSHA: reviewed, LastRoundStartedAt: ago(time.Hour)},
			ready: true,
		},
		{
			name:   "a later PendingSince restarts the quiet period",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), PendingSince: ago(time.Minute), LastRoundStartedAt: ago(2 * time.Hour)},
			next:   now.Add(4 * time.Minute),
			reason: reasonQuiet,
		},
		{
			name:   "an older PendingSince does not shorten the quiet period",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Minute), PendingSince: ago(time.Hour), LastRoundStartedAt: ago(2 * time.Hour)},
			next:   now.Add(4 * time.Minute),
			reason: reasonQuiet,
		},
		{
			name:  "PendingSince old enough is ready",
			f:     PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), PendingSince: ago(5 * time.Minute), LastRoundStartedAt: ago(2 * time.Hour)},
			ready: true,
		},

		// min re-review interval
		{
			name:  "interval boundary is inclusive",
			f:     PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(30 * time.Minute)},
			ready: true,
		},
		{
			name:   "interval not yet over",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(30*time.Minute - time.Second)},
			next:   now.Add(time.Second),
			reason: reasonInterval,
		},
		{
			name:   "interval counts from the round start",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Minute)},
			next:   now.Add(29 * time.Minute),
			reason: reasonInterval,
		},

		// drafts use the draft interval
		{
			name:  "same facts, non-draft is ready after 1h",
			f:     PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(2 * time.Hour), LastRoundStartedAt: ago(time.Hour)},
			ready: true,
		},
		{
			name:   "draft waits for the 2h interval",
			f:      PRFacts{ReviewedSHA: reviewed, IsDraft: true, HeadChangedAt: ago(2 * time.Hour), LastRoundStartedAt: ago(time.Hour)},
			next:   now.Add(time.Hour),
			reason: reasonDraft,
		},
		{
			name:  "draft interval boundary is inclusive",
			f:     PRFacts{ReviewedSHA: reviewed, IsDraft: true, HeadChangedAt: ago(3 * time.Hour), LastRoundStartedAt: ago(2 * time.Hour)},
			ready: true,
		},

		// daily cap
		{
			name:  "one round below the cap is ready",
			f:     PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Hour), RoundsToday: 5},
			ready: true,
		},
		{
			name:   "at the cap waits for local midnight",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Hour), RoundsToday: 6},
			next:   wantMidnight,
			reason: reasonCap,
		},
		{
			name:   "above the cap waits for local midnight",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Hour), RoundsToday: 7},
			next:   wantMidnight,
			reason: reasonCap,
		},

		// forced bypasses interval, cap and quiet period
		{
			name:  "forced bypasses quiet period, interval and cap together",
			f:     PRFacts{ReviewedSHA: reviewed, Forced: true, HeadChangedAt: now, LastRoundStartedAt: now, RoundsToday: 50},
			ready: true,
		},
		{
			name:  "forced draft bypasses the draft interval",
			f:     PRFacts{ReviewedSHA: reviewed, Forced: true, IsDraft: true, LastRoundStartedAt: ago(time.Minute)},
			ready: true,
		},

		// several blockers: the latest one is binding
		{
			name:   "interval later than quiet period is binding",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Minute), LastRoundStartedAt: ago(10 * time.Minute)},
			next:   now.Add(20 * time.Minute),
			reason: reasonInterval,
		},
		{
			name:   "quiet period later than interval is binding",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Minute), LastRoundStartedAt: ago(29 * time.Minute)},
			next:   now.Add(4 * time.Minute),
			reason: reasonQuiet,
		},
		{
			name:   "cap beats an earlier interval",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(10 * time.Minute), RoundsToday: 6},
			next:   wantMidnight,
			reason: reasonCap,
		},
		{
			name:   "equal times report the quiet period first",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Minute), LastRoundStartedAt: ago(26 * time.Minute)},
			next:   now.Add(4 * time.Minute),
			reason: reasonQuiet,
		},
	})
}

func TestThrottleCapEdgeCases(t *testing.T) {
	re := func(f PRFacts, at time.Time) PRFacts {
		f.ReviewedSHA = reviewed
		f.HeadChangedAt = at.Add(-time.Hour)
		return f
	}

	t.Run("zero cap disables the limit", func(t *testing.T) {
		d := daemon()
		d.MaxRoundsPerPRPerDay = 0
		if got := Throttle(d, re(PRFacts{RoundsToday: 100}, now), now); !got.Ready {
			t.Fatalf("got %+v, want ready", got)
		}
	})
	t.Run("negative cap disables the limit", func(t *testing.T) {
		d := daemon()
		d.MaxRoundsPerPRPerDay = -1
		if got := Throttle(d, re(PRFacts{RoundsToday: 100}, now), now); !got.Ready {
			t.Fatalf("got %+v, want ready", got)
		}
	})
	t.Run("midnight is computed in the location of now", func(t *testing.T) {
		loc := time.FixedZone("UTC+3", 3*3600)
		at := time.Date(2026, 1, 14, 23, 30, 0, 0, loc)
		got := Throttle(daemon(), re(PRFacts{RoundsToday: 6}, at), at)
		want := time.Date(2026, 1, 15, 0, 0, 0, 0, loc)
		if got.Ready || !got.NextEligibleAt.Equal(want) || got.Reason != reasonCap {
			t.Fatalf("got %+v, want next=%v reason=%q", got, want, reasonCap)
		}
	})
	t.Run("midnight rolls over month and year ends", func(t *testing.T) {
		at := time.Date(2026, 12, 31, 18, 0, 0, 0, time.UTC)
		got := Throttle(daemon(), re(PRFacts{RoundsToday: 6}, at), at)
		want := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		if got.Ready || !got.NextEligibleAt.Equal(want) {
			t.Fatalf("got %+v, want next=%v", got, want)
		}
	})
	t.Run("exactly at midnight the next reset is a full day away", func(t *testing.T) {
		at := time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC)
		got := Throttle(daemon(), re(PRFacts{RoundsToday: 6}, at), at)
		want := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
		if got.Ready || !got.NextEligibleAt.Equal(want) {
			t.Fatalf("got %+v, want next=%v", got, want)
		}
	})
	t.Run("a binding interval past midnight wins over the cap", func(t *testing.T) {
		at := time.Date(2026, 1, 14, 23, 30, 0, 0, time.UTC)
		started := at.Add(-time.Minute)
		got := Throttle(daemon(), re(PRFacts{RoundsToday: 6, IsDraft: true, LastRoundStartedAt: started}, at), at)
		want := started.Add(2 * time.Hour)
		if got.Ready || !got.NextEligibleAt.Equal(want) || got.Reason != reasonDraft {
			t.Fatalf("got %+v, want next=%v reason=%q", got, want, reasonDraft)
		}
	})
	t.Run("cap comes from the config value", func(t *testing.T) {
		d := daemon()
		d.MaxRoundsPerPRPerDay = 10
		if got := Throttle(d, re(PRFacts{RoundsToday: 9}, now), now); !got.Ready {
			t.Fatalf("got %+v, want ready", got)
		}
		if got := Throttle(d, re(PRFacts{RoundsToday: 10}, now), now); got.Ready || got.Reason != "daily round cap reached (10 per day)" {
			t.Fatalf("got %+v, want capped", got)
		}
	})
}

func TestThrottleHonoursConfiguredDurations(t *testing.T) {
	d := config.Daemon{
		PushQuietPeriod:          dur(90 * time.Second),
		MinRereviewInterval:      dur(10 * time.Minute),
		DraftMinRereviewInterval: dur(20 * time.Minute),
	}
	f := PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Minute), LastRoundStartedAt: ago(time.Minute)}
	got := Throttle(d, f, now)
	if got.Ready || !got.NextEligibleAt.Equal(now.Add(9*time.Minute)) || got.Reason != "waiting for min re-review interval (10m0s)" {
		t.Fatalf("non-draft: %+v", got)
	}
	f.IsDraft = true
	got = Throttle(d, f, now)
	if got.Ready || !got.NextEligibleAt.Equal(now.Add(19*time.Minute)) || got.Reason != "waiting for draft re-review interval (20m0s)" {
		t.Fatalf("draft: %+v", got)
	}
	f = PRFacts{HeadChangedAt: ago(time.Minute)}
	got = Throttle(d, f, now)
	if got.Ready || !got.NextEligibleAt.Equal(now.Add(30*time.Second)) || got.Reason != "waiting for push quiet period (1m30s)" {
		t.Fatalf("first review: %+v", got)
	}
}

func TestThrottleDraftZeroIntervalFallsBackToMinInterval(t *testing.T) {
	// Mirrors store.Candidates: DraftMinInterval == 0 means MinInterval.
	d := daemon()
	d.DraftMinRereviewInterval = dur(0)
	started := ago(10 * time.Minute)
	f := PRFacts{ReviewedSHA: reviewed, IsDraft: true, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: started}
	got := Throttle(d, f, now)
	if got.Ready || !got.NextEligibleAt.Equal(started.Add(30*time.Minute)) || got.Reason != reasonInterval {
		t.Fatalf("got %+v, want next=%v reason=%q", got, started.Add(30*time.Minute), reasonInterval)
	}
	f.LastRoundStartedAt = ago(30 * time.Minute)
	if got := Throttle(d, f, now); !got.Ready {
		t.Fatalf("draft after MinRereviewInterval: got %+v, want ready", got)
	}
}

func TestThrottleZeroConfigNeverBlocks(t *testing.T) {
	f := PRFacts{ReviewedSHA: reviewed, HeadChangedAt: now, LastRoundStartedAt: now, RoundsToday: 100}
	if got := Throttle(config.Daemon{}, f, now); !got.Ready {
		t.Fatalf("got %+v, want ready", got)
	}
}

func TestThrottleIgnoresUnrelatedFacts(t *testing.T) {
	f := PRFacts{
		ReviewedSHA: reviewed, HeadSHA: "def456", HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Hour),
		Muted: true, Pinned: true, State: "rereview_pending", AuthorLogin: "x", AuthorIsBot: true,
	}
	if got := Throttle(daemon(), f, now); !got.Ready {
		t.Fatalf("timing must not depend on eligibility facts, got %+v", got)
	}
}

// burstDaemon is daemon() with the shipped burst settings (15m after 3
// pushes within 30m).
func burstDaemon() config.Daemon {
	d := daemon()
	d.BurstQuietPeriod, d.BurstPushes, d.BurstWindow = dur(15*time.Minute), 3, dur(30*time.Minute)
	return d
}

func TestThrottleBurstQuietPeriod(t *testing.T) {
	const reasonBurst = "waiting for burst quiet period after 3 pushes within 30m0s (15m0s)"
	tests := []struct {
		name   string
		d      config.Daemon
		f      PRFacts
		ready  bool
		next   time.Time
		reason string
	}{
		{
			name:   "three pushes within the window: the burst quiet period applies",
			d:      burstDaemon(),
			f:      PRFacts{HeadChangedAt: ago(6 * time.Minute), PushTimes: []time.Time{ago(26 * time.Minute), ago(16 * time.Minute), ago(6 * time.Minute)}},
			next:   ago(6 * time.Minute).Add(15 * time.Minute),
			reason: reasonBurst,
		},
		{
			name:  "the burst quiet period has passed",
			d:     burstDaemon(),
			f:     PRFacts{HeadChangedAt: ago(16 * time.Minute), PushTimes: []time.Time{ago(36 * time.Minute), ago(26 * time.Minute), ago(16 * time.Minute)}},
			ready: true,
		},
		{
			name:  "two pushes are no burst: the push quiet period applies",
			d:     burstDaemon(),
			f:     PRFacts{HeadChangedAt: ago(6 * time.Minute), PushTimes: []time.Time{ago(16 * time.Minute), ago(6 * time.Minute)}},
			ready: true,
		},
		{
			name:   "the window is counted back from the last push, not from now",
			d:      burstDaemon(),
			f:      PRFacts{HeadChangedAt: ago(10 * time.Minute), PushTimes: []time.Time{ago(10 * time.Minute), ago(40 * time.Minute), ago(25 * time.Minute)}},
			next:   ago(10 * time.Minute).Add(15 * time.Minute),
			reason: reasonBurst,
		},
		{
			name:  "a push outside the window does not count",
			d:     burstDaemon(),
			f:     PRFacts{HeadChangedAt: ago(6 * time.Minute), PushTimes: []time.Time{ago(50 * time.Minute), ago(16 * time.Minute), ago(6 * time.Minute)}},
			ready: true,
		},
		{
			name:   "re-review: counted from pending_since like the push quiet period",
			d:      burstDaemon(),
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(20 * time.Minute), PendingSince: ago(4 * time.Minute), PushTimes: []time.Time{ago(30 * time.Minute), ago(25 * time.Minute), ago(20 * time.Minute)}},
			next:   ago(4 * time.Minute).Add(15 * time.Minute),
			reason: reasonBurst,
		},
		{
			name: "burst_pushes 0 turns the rule off",
			d: func() config.Daemon {
				d := burstDaemon()
				d.BurstPushes = 0
				return d
			}(),
			f:     PRFacts{HeadChangedAt: ago(6 * time.Minute), PushTimes: []time.Time{ago(26 * time.Minute), ago(16 * time.Minute), ago(6 * time.Minute)}},
			ready: true,
		},
		{
			name: "a burst quiet period shorter than the push quiet period never shortens it",
			d: func() config.Daemon {
				d := burstDaemon()
				d.BurstQuietPeriod = dur(time.Minute)
				return d
			}(),
			f:      PRFacts{HeadChangedAt: ago(2 * time.Minute), PushTimes: []time.Time{ago(4 * time.Minute), ago(3 * time.Minute), ago(2 * time.Minute)}},
			next:   ago(2 * time.Minute).Add(5 * time.Minute),
			reason: reasonQuiet,
		},
		{
			name:  "forced bypasses the burst quiet period",
			d:     burstDaemon(),
			f:     PRFacts{Forced: true, HeadChangedAt: ago(time.Minute), PushTimes: []time.Time{ago(3 * time.Minute), ago(2 * time.Minute), ago(time.Minute)}},
			ready: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Throttle(tc.d, tc.f, now)
			if got.Ready != tc.ready {
				t.Fatalf("Ready = %v (%+v), want %v", got.Ready, got, tc.ready)
			}
			want := tc.next
			if tc.ready {
				want = now
			}
			if !got.NextEligibleAt.Equal(want) || got.Reason != tc.reason {
				t.Fatalf("got %v %q, want %v %q", got.NextEligibleAt, got.Reason, want, tc.reason)
			}
		})
	}
}
