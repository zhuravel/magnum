package eligibility

import "testing"

// Quiet hours hold every round but a re-review of the judge alone (a delta
// check, the same head, a reply round), and nothing outside them.
func TestQuietHoursHoldAllButTheJudgeAlone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spec       string
		hour       int
		judgeAlone bool
		want       bool
	}{
		{"inside, a full round", "03:00-12:00", 5, false, true},
		{"inside, the judge alone", "03:00-12:00", 5, true, false},
		{"outside, a full round", "03:00-12:00", 13, false, false},
		{"outside, the judge alone", "03:00-12:00", 13, true, false},
		{"no quiet hours", "", 5, false, false},
	} {
		if got := QuietHoursHold(tc.spec, at(tc.hour, 0, 0), tc.judgeAlone); got != tc.want {
			t.Errorf("%s: QuietHoursHold = %v, want %v", tc.name, got, tc.want)
		}
	}
}
