package eligibility

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

// schedDaemon is the shipped throttle settings plus the burst rule, a 1m
// request debounce and the re-review threshold (30 lines, 2h max wait).
func schedDaemon() config.Daemon {
	d := burstDaemon()
	d.RequestDebounce = dur(time.Minute)
	d.RereviewMinLines = 30
	d.RereviewMaxWait = dur(2 * time.Hour)
	return d
}

// reviewedFacts are the facts of a PR whose re-review no timing rule holds
// back: old head, old last round, no rounds today.
func reviewedFacts() PRFacts {
	return PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Hour)}
}

// wantDecision checks a Throttle verdict: ready (NextEligibleAt == now) or held
// until next with a reason starting with prefix.
func wantDecision(t *testing.T, got ThrottleDecision, ready bool, next time.Time, prefix string) {
	t.Helper()
	if got.Ready != ready {
		t.Fatalf("Ready = %v (%+v), want %v", got.Ready, got, ready)
	}
	if ready {
		if !got.NextEligibleAt.Equal(now) || got.Reason != "" {
			t.Fatalf("ready decision = %+v, want NextEligibleAt = now and no reason", got)
		}
		return
	}
	if !got.NextEligibleAt.Equal(next) {
		t.Fatalf("NextEligibleAt = %v, want %v (%+v)", got.NextEligibleAt, next, got)
	}
	if !strings.HasPrefix(got.Reason, prefix) {
		t.Fatalf("Reason = %q, want prefix %q", got.Reason, prefix)
	}
}

func TestThrottleRequestSkipsEveryOtherRule(t *testing.T) {
	requestedAt := ago(3 * time.Minute) // past the 1m debounce, inside every other rule's wait
	burstPushes := []time.Time{ago(24 * time.Minute), ago(14 * time.Minute), ago(4 * time.Minute)}
	for _, tc := range []struct {
		name   string
		d      func(*config.Daemon)
		f      PRFacts
		reason string // what holds the PR without the request
	}{
		{
			name:   "push quiet period",
			f:      PRFacts{HeadChangedAt: ago(4 * time.Minute)},
			reason: reasonQuiet,
		},
		{
			name:   "quiet period of a re-review counted from pending_since",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), PendingSince: ago(4 * time.Minute)},
			reason: reasonQuiet,
		},
		{
			name:   "burst quiet period",
			f:      PRFacts{HeadChangedAt: ago(4 * time.Minute), PushTimes: burstPushes},
			reason: "waiting for burst quiet period",
		},
		{
			name:   "min re-review interval",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(10 * time.Minute)},
			reason: reasonInterval,
		},
		{
			name:   "draft re-review interval",
			f:      PRFacts{ReviewedSHA: reviewed, IsDraft: true, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(40 * time.Minute)},
			reason: reasonDraft,
		},
		{
			name:   "daily cap",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(2 * time.Hour), RoundsToday: 6},
			reason: reasonCap,
		},
		{
			name:   "daily cap far above the limit",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(2 * time.Hour), RoundsToday: 99},
			reason: reasonCap,
		},
		{
			name:   "small delta",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(2 * time.Hour), DeltaKnown: true, DeltaLines: 3, DeltaSince: ago(10 * time.Minute)},
			reason: ReasonSmallDelta,
		},
		{
			name: "every rule at once",
			f: PRFacts{
				ReviewedSHA: reviewed, IsDraft: true, HeadChangedAt: ago(4 * time.Minute), PendingSince: ago(4 * time.Minute), PushTimes: burstPushes,
				LastRoundStartedAt: ago(time.Minute), RoundsToday: 50, DeltaKnown: true, DeltaLines: 1, DeltaSince: ago(time.Minute),
			},
			reason: reasonCap,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := schedDaemon()
			if tc.d != nil {
				tc.d(&d)
			}
			// without the request the rule holds the PR
			held := Throttle(d, tc.f, now)
			if held.Ready || !strings.HasPrefix(held.Reason, tc.reason) {
				t.Fatalf("baseline without a request = %+v, want held with reason %q", held, tc.reason)
			}
			// with the request every one of them is skipped
			tc.f.RequestedAt = requestedAt
			wantDecision(t, Throttle(d, tc.f, now), true, time.Time{}, "")
		})
	}
}

func TestThrottleRequestDebounce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		d      func(*config.Daemon)
		f      PRFacts
		ready  bool
		next   time.Time
		reason string
	}{
		{
			name:   "a fresh request waits the whole debounce",
			f:      PRFacts{RequestedAt: now, HeadChangedAt: ago(time.Hour)},
			next:   now.Add(time.Minute),
			reason: ReasonRequested,
		},
		{
			name:   "the debounce counts from the request when it is the later one",
			f:      PRFacts{RequestedAt: ago(20 * time.Second), HeadChangedAt: ago(time.Hour)},
			next:   now.Add(40 * time.Second),
			reason: ReasonRequested,
		},
		{
			name:   "the debounce counts from the last push when it is the later one",
			f:      PRFacts{RequestedAt: ago(10 * time.Minute), HeadChangedAt: ago(20 * time.Second)},
			next:   now.Add(40 * time.Second),
			reason: ReasonRequested,
		},
		{
			name:   "a push right now restarts the debounce of an old request",
			f:      PRFacts{RequestedAt: ago(time.Hour), HeadChangedAt: now},
			next:   now.Add(time.Minute),
			reason: ReasonRequested,
		},
		{
			name:  "the debounce boundary is inclusive",
			f:     PRFacts{RequestedAt: ago(time.Minute), HeadChangedAt: ago(time.Hour)},
			ready: true,
		},
		{
			name:   "one second short of the debounce waits",
			f:      PRFacts{RequestedAt: ago(time.Minute - time.Second), HeadChangedAt: ago(time.Hour)},
			next:   now.Add(time.Second),
			reason: ReasonRequested,
		},
		{
			name:  "request and push both older than the debounce",
			f:     PRFacts{RequestedAt: ago(5 * time.Minute), HeadChangedAt: ago(10 * time.Minute)},
			ready: true,
		},
		{
			name:   "unknown push time: the request alone anchors the debounce",
			f:      PRFacts{RequestedAt: ago(30 * time.Second)},
			next:   now.Add(30 * time.Second),
			reason: ReasonRequested,
		},
		{
			name:  "zero debounce is ready at once",
			d:     func(d *config.Daemon) { d.RequestDebounce = dur(0) },
			f:     PRFacts{RequestedAt: now, HeadChangedAt: now},
			ready: true,
		},
		{
			name:  "a negative debounce never blocks",
			d:     func(d *config.Daemon) { d.RequestDebounce = dur(-time.Minute) },
			f:     PRFacts{RequestedAt: now, HeadChangedAt: now},
			ready: true,
		},
		{
			name:   "a longer configured debounce",
			d:      func(d *config.Daemon) { d.RequestDebounce = dur(10 * time.Minute) },
			f:      PRFacts{RequestedAt: ago(4 * time.Minute), HeadChangedAt: ago(time.Hour)},
			next:   now.Add(6 * time.Minute),
			reason: ReasonRequested,
		},
		{
			name:  "forced ignores the debounce",
			f:     PRFacts{Forced: true, RequestedAt: now, HeadChangedAt: now},
			ready: true,
		},
		{
			name:   "first review: the debounce replaces the 5m quiet period",
			f:      PRFacts{RequestedAt: ago(2 * time.Minute), HeadChangedAt: ago(30 * time.Second)},
			next:   now.Add(30 * time.Second),
			reason: ReasonRequested,
		},
		{
			name:  "first review: ready after the debounce although the quiet period has not passed",
			f:     PRFacts{RequestedAt: ago(2 * time.Minute), HeadChangedAt: ago(90 * time.Second)},
			ready: true,
		},
		{
			name:   "re-review: the debounce replaces the interval, cap and quiet period",
			f:      PRFacts{ReviewedSHA: reviewed, RequestedAt: ago(10 * time.Second), HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Minute), RoundsToday: 9},
			next:   now.Add(50 * time.Second),
			reason: ReasonRequested,
		},
		{
			name:  "pending_since does not move the debounce anchor",
			f:     PRFacts{ReviewedSHA: reviewed, RequestedAt: ago(2 * time.Minute), HeadChangedAt: ago(time.Hour), PendingSince: ago(10 * time.Second)},
			ready: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := schedDaemon()
			if tc.d != nil {
				tc.d(&d)
			}
			wantDecision(t, Throttle(d, tc.f, now), tc.ready, tc.next, tc.reason)
		})
	}
}

func TestThrottleRequestReasonNamesTheDebounce(t *testing.T) {
	got := Throttle(schedDaemon(), PRFacts{RequestedAt: now}, now)
	if got.Ready || !strings.HasPrefix(got.Reason, ReasonRequested) || !strings.Contains(got.Reason, "1m0s") {
		t.Fatalf("got %+v, want a %q reason naming the 1m0s debounce", got, ReasonRequested)
	}
}

func TestThrottleRequestWithoutDaemonSettingsNeverBlocks(t *testing.T) {
	f := PRFacts{ReviewedSHA: reviewed, RequestedAt: now, HeadChangedAt: now, LastRoundStartedAt: now, RoundsToday: 100}
	wantDecision(t, Throttle(config.Daemon{}, f, now), true, time.Time{}, "")
}

func TestThrottleSmallDelta(t *testing.T) {
	smallFacts := func(mod func(*PRFacts)) PRFacts {
		f := reviewedFacts()
		f.DeltaKnown, f.DeltaLines, f.DeltaSince = true, 8, ago(10*time.Minute)
		if mod != nil {
			mod(&f)
		}
		return f
	}
	for _, tc := range []struct {
		name   string
		d      func(*config.Daemon)
		f      PRFacts
		ready  bool
		next   time.Time
		reason string
	}{
		{
			name:   "8 of 30 lines holds the re-review until delta_since + max wait",
			f:      smallFacts(nil),
			next:   ago(10 * time.Minute).Add(2 * time.Hour),
			reason: ReasonSmallDelta,
		},
		{
			name:   "zero changed lines is still under the threshold",
			f:      smallFacts(func(f *PRFacts) { f.DeltaLines = 0 }),
			next:   ago(10 * time.Minute).Add(2 * time.Hour),
			reason: ReasonSmallDelta,
		},
		{
			name:   "one line short of the threshold holds",
			f:      smallFacts(func(f *PRFacts) { f.DeltaLines = 29 }),
			next:   ago(10 * time.Minute).Add(2 * time.Hour),
			reason: ReasonSmallDelta,
		},
		{
			name:  "exactly at the threshold is not small",
			f:     smallFacts(func(f *PRFacts) { f.DeltaLines = 30 }),
			ready: true,
		},
		{
			name:  "over the threshold is not small",
			f:     smallFacts(func(f *PRFacts) { f.DeltaLines = 31 }),
			ready: true,
		},
		{
			name:  "an added file makes any delta reviewable",
			f:     smallFacts(func(f *PRFacts) { f.DeltaAddedFiles = 1; f.DeltaLines = 2 }),
			ready: true,
		},
		{
			name:  "an unknown delta never holds",
			f:     smallFacts(func(f *PRFacts) { f.DeltaKnown = false }),
			ready: true,
		},
		{
			name:  "rereview_min_lines 0 turns the rule off",
			d:     func(d *config.Daemon) { d.RereviewMinLines = 0 },
			f:     smallFacts(nil),
			ready: true,
		},
		{
			name:  "a negative threshold turns the rule off",
			d:     func(d *config.Daemon) { d.RereviewMinLines = -5 },
			f:     smallFacts(nil),
			ready: true,
		},
		{
			name:  "a first review is never held by the delta",
			f:     smallFacts(func(f *PRFacts) { f.ReviewedSHA = "" }),
			ready: true,
		},
		{
			name:  "no delta_since: nothing to count the wait from",
			f:     smallFacts(func(f *PRFacts) { f.DeltaSince = time.Time{} }),
			ready: true,
		},
		{
			name:  "max wait elapsed (boundary inclusive)",
			f:     smallFacts(func(f *PRFacts) { f.DeltaSince = ago(2 * time.Hour) }),
			ready: true,
		},
		{
			name:   "one second short of the max wait holds",
			f:      smallFacts(func(f *PRFacts) { f.DeltaSince = ago(2*time.Hour - time.Second) }),
			next:   now.Add(time.Second),
			reason: ReasonSmallDelta,
		},
		{
			name:  "max wait long elapsed",
			f:     smallFacts(func(f *PRFacts) { f.DeltaSince = ago(9 * time.Hour) }),
			ready: true,
		},
		{
			name:  "zero max wait never holds",
			d:     func(d *config.Daemon) { d.RereviewMaxWait = dur(0) },
			f:     smallFacts(nil),
			ready: true,
		},
		{
			name:   "a shorter max wait from the config",
			d:      func(d *config.Daemon) { d.RereviewMaxWait = dur(30 * time.Minute) },
			f:      smallFacts(nil),
			next:   now.Add(20 * time.Minute),
			reason: ReasonSmallDelta,
		},
		{
			name:  "forced bypasses the small delta",
			f:     smallFacts(func(f *PRFacts) { f.Forced = true }),
			ready: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := schedDaemon()
			if tc.d != nil {
				tc.d(&d)
			}
			wantDecision(t, Throttle(d, tc.f, now), tc.ready, tc.next, tc.reason)
		})
	}
}

func TestThrottleSmallDeltaReasonShowsLinesAndThreshold(t *testing.T) {
	f := reviewedFacts()
	f.DeltaKnown, f.DeltaLines, f.DeltaSince = true, 8, ago(10*time.Minute)
	got := Throttle(schedDaemon(), f, now)
	if got.Ready || !strings.HasPrefix(got.Reason, ReasonSmallDelta) || !strings.Contains(got.Reason, "8/30 lines") || !strings.Contains(got.Reason, "2h0m0s") {
		t.Fatalf("Reason = %q, want %q ... 8/30 lines ... 2h0m0s", got.Reason, ReasonSmallDelta)
	}
}

func TestThrottleLatestRuleWins(t *testing.T) {
	const intervalUntil = 29 * time.Minute // LastRoundStartedAt ago(1m) + 30m
	for _, tc := range []struct {
		name   string
		d      func(*config.Daemon)
		f      PRFacts
		next   time.Time
		reason string
	}{
		{
			name:   "min interval later than the small delta",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Minute), DeltaKnown: true, DeltaLines: 5, DeltaSince: ago(110 * time.Minute)},
			next:   now.Add(intervalUntil),
			reason: reasonInterval,
		},
		{
			name:   "small delta later than the min interval",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Minute), DeltaKnown: true, DeltaLines: 5, DeltaSince: ago(10 * time.Minute)},
			next:   ago(10 * time.Minute).Add(2 * time.Hour),
			reason: ReasonSmallDelta,
		},
		{
			name:   "draft interval later than the small delta",
			f:      PRFacts{ReviewedSHA: reviewed, IsDraft: true, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Minute), DeltaKnown: true, DeltaLines: 5, DeltaSince: ago(30 * time.Minute)},
			next:   ago(time.Minute).Add(2 * time.Hour),
			reason: reasonDraft,
		},
		{
			name:   "quiet period later than the small delta",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Minute), DeltaKnown: true, DeltaLines: 5, DeltaSince: ago(118 * time.Minute)},
			next:   now.Add(4 * time.Minute),
			reason: reasonQuiet,
		},
		{
			name:   "daily cap later than the small delta",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Hour), RoundsToday: 6, DeltaKnown: true, DeltaLines: 5, DeltaSince: ago(10 * time.Minute)},
			next:   wantMidnight,
			reason: reasonCap,
		},
		{
			name:   "small delta later than the daily cap",
			d:      func(d *config.Daemon) { d.RereviewMaxWait = dur(20 * time.Hour) },
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Hour), RoundsToday: 6, DeltaKnown: true, DeltaLines: 5, DeltaSince: ago(10 * time.Minute)},
			next:   ago(10 * time.Minute).Add(20 * time.Hour),
			reason: ReasonSmallDelta,
		},
		{
			name:   "tie: quiet period beats the small delta",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Minute), DeltaKnown: true, DeltaLines: 5, DeltaSince: now.Add(4 * time.Minute).Add(-2 * time.Hour)},
			next:   now.Add(4 * time.Minute),
			reason: reasonQuiet,
		},
		{
			name:   "tie: min interval beats the small delta",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Minute), DeltaKnown: true, DeltaLines: 5, DeltaSince: now.Add(intervalUntil).Add(-2 * time.Hour)},
			next:   now.Add(intervalUntil),
			reason: reasonInterval,
		},
		{
			name:   "tie: daily cap beats the small delta",
			f:      PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(time.Hour), RoundsToday: 6, DeltaKnown: true, DeltaLines: 5, DeltaSince: wantMidnight.Add(-2 * time.Hour)},
			next:   wantMidnight,
			reason: reasonCap,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := schedDaemon()
			if tc.d != nil {
				tc.d(&d)
			}
			got := Throttle(d, tc.f, now)
			wantDecision(t, got, false, tc.next, "")
			if !strings.HasPrefix(got.Reason, tc.reason) {
				t.Fatalf("Reason = %q, want prefix %q", got.Reason, tc.reason)
			}
		})
	}
}

// The first review counts nothing of the delta, interval or cap: only the
// quiet period (or a request's debounce) can hold it.
func TestThrottleFirstReviewWithDeltaAndRequestFacts(t *testing.T) {
	f := PRFacts{HeadChangedAt: ago(10 * time.Minute), DeltaKnown: true, DeltaLines: 1, DeltaSince: ago(time.Minute), RoundsToday: 99, LastRoundStartedAt: ago(time.Minute)}
	wantDecision(t, Throttle(schedDaemon(), f, now), true, time.Time{}, "")

	f.RequestedAt = ago(30 * time.Second)
	wantDecision(t, Throttle(schedDaemon(), f, now), false, now.Add(30*time.Second), ReasonRequested)
}
