package eligibility

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

// A re-review gets a delta check when delta_check and the threshold are on
// and the measured delta is readable, adds no file and has more than 0 and
// fewer than rereview_min_lines changed code lines.
func TestDeltaCheck(t *testing.T) {
	on := schedDaemon()
	on.DeltaCheck = true
	small := func(mod func(*PRFacts)) PRFacts {
		f := reviewedFacts()
		f.DeltaReadable, f.DeltaKnown, f.DeltaLines = true, true, 4
		if mod != nil {
			mod(&f)
		}
		return f
	}
	for _, tc := range []struct {
		name string
		d    func(*config.Daemon)
		f    PRFacts
		want bool
	}{
		{"4 lines", nil, small(nil), true},
		{"4 lines and modified binary files", nil, small(func(f *PRFacts) { f.DeltaKnown = false }), true},
		{"29 lines", nil, small(func(f *PRFacts) { f.DeltaLines = 29 }), true},
		{"30 lines: the threshold", nil, small(func(f *PRFacts) { f.DeltaLines = 30 }), false},
		{"no code line", nil, small(func(f *PRFacts) { f.DeltaLines = 0 }), false},
		{"an added file", nil, small(func(f *PRFacts) { f.DeltaAddedFiles = 1 }), false},
		{"a file it cannot read", nil, small(func(f *PRFacts) { f.DeltaReadable, f.DeltaKnown = false, false }), false},
		{"a first review", nil, small(func(f *PRFacts) { f.ReviewedSHA = "" }), false},
		{"delta_check off", func(d *config.Daemon) { d.DeltaCheck = false }, small(nil), false},
		{"no threshold", func(d *config.Daemon) { d.RereviewMinLines = 0 }, small(nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := on
			if tc.d != nil {
				tc.d(&d)
			}
			if got := DeltaCheck(d, tc.f); got != tc.want {
				t.Fatalf("DeltaCheck = %v, want %v", got, tc.want)
			}
		})
	}
}

// A delta check does not wait for rereview_max_wait: only the quiet period
// (and the other timing rules) hold it.
func TestThrottleDoesNotHoldADeltaCheckForTheMaxWait(t *testing.T) {
	d := schedDaemon()
	f := reviewedFacts()
	f.DeltaReadable, f.DeltaKnown, f.DeltaLines, f.DeltaSince = true, true, 4, ago(10*time.Minute)
	wantDecision(t, Throttle(d, f, now), false, ago(10*time.Minute).Add(2*time.Hour), ReasonSmallDelta) // delta_check off
	d.DeltaCheck = true
	wantDecision(t, Throttle(d, f, now), true, time.Time{}, "")
	f.HeadChangedAt = ago(time.Minute)
	if got := Throttle(d, f, now); got.Ready || got.Rule != RuleQuiet {
		t.Fatalf("decision = %+v, want the quiet period", got)
	}
}

// SmallDeltaHold is the predicate Throttle holds a re-review by for the
// threshold, which the dispatcher asks to know that a round it starts waited
// for a small delta: a delta that is known, adds no file and stays under
// rereview_min_lines, gets no delta check (an empty push, with no code line,
// gets none) and has its first push to count the wait from.
func TestSmallDeltaHoldIsWhatTheThresholdHoldsBy(t *testing.T) {
	on := schedDaemon()
	on.DeltaCheck = true
	small := func(mod func(*PRFacts)) PRFacts {
		f := reviewedFacts()
		f.DeltaReadable, f.DeltaKnown, f.DeltaLines, f.DeltaSince = true, true, 8, ago(10*time.Minute)
		if mod != nil {
			mod(&f)
		}
		return f
	}
	noCheck := func(d *config.Daemon) { d.DeltaCheck = false }
	for _, tc := range []struct {
		name string
		d    func(*config.Daemon)
		f    PRFacts
		want bool
	}{
		{"8 lines, delta_check off", noCheck, small(nil), true},
		{"8 lines get a delta check", nil, small(nil), false},
		{"an empty push (no code line) gets none", nil, small(func(f *PRFacts) { f.DeltaLines = 0 }), true},
		{"30 lines: the threshold", nil, small(func(f *PRFacts) { f.DeltaLines = 30 }), false},
		{"an added file", noCheck, small(func(f *PRFacts) { f.DeltaAddedFiles = 1 }), false},
		{"a delta that was not measured", noCheck, small(func(f *PRFacts) { f.DeltaKnown = false }), false},
		{"no first push to count from", noCheck, small(func(f *PRFacts) { f.DeltaSince = time.Time{} }), false},
		{"no threshold", func(d *config.Daemon) { d.RereviewMinLines = 0 }, small(nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := on
			if tc.d != nil {
				tc.d(&d)
			}
			if got := SmallDeltaHold(d, tc.f); got != tc.want {
				t.Fatalf("SmallDeltaHold = %v, want %v", got, tc.want)
			}
			if held := Throttle(d, tc.f, now).Rule == RuleSmallDelta; held != tc.want {
				t.Fatalf("Throttle holds by the threshold = %v, want %v: the two must agree", held, tc.want)
			}
		})
	}
}
