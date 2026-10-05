package eligibility

import (
	"testing"
	"time"
)

// Every rule that holds a PR back names itself as a code next to its words,
// so a caller never sorts the words: a rewording of a reason cannot turn the
// wait into an unknown one.
func TestThrottleNamesTheRuleThatHoldsThePR(t *testing.T) {
	burst := []time.Time{ago(26 * time.Minute), ago(16 * time.Minute), ago(6 * time.Minute)}
	draft := reviewedFacts()
	draft.IsDraft, draft.LastRoundStartedAt = true, ago(time.Hour)
	for _, tc := range []struct {
		name string
		f    PRFacts
		want Rule
	}{
		{"nothing holds it", reviewedFacts(), ""},
		{"a request's debounce", PRFacts{ReviewedSHA: reviewed, RequestedAt: ago(10 * time.Second), HeadChangedAt: ago(time.Hour)}, RuleRequested},
		{"the push quiet period", PRFacts{HeadChangedAt: ago(time.Minute)}, RuleQuiet},
		{"the burst quiet period", PRFacts{HeadChangedAt: ago(6 * time.Minute), PushTimes: burst}, RuleBurst},
		{"the re-review interval", PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(10 * time.Minute)}, RuleInterval},
		{"the draft interval", draft, RuleDraftInterval},
		{"the daily cap", PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), RoundsToday: 6}, RuleCap},
		{"a small delta", PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), DeltaKnown: true, DeltaLines: 8, DeltaSince: ago(time.Hour)}, RuleSmallDelta},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Throttle(schedDaemon(), tc.f, now)
			if got.Rule != tc.want || (tc.want == "") != got.Ready {
				t.Fatalf("Throttle = %+v, want rule %q", got, tc.want)
			}
		})
	}
}
