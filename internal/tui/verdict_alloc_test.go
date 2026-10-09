package tui

import "testing"

// normVerdict runs twice per comparison in the reviewers' sort: a verdict
// already in its normal form costs no allocation.
func TestNormVerdictOfANormalVerdictAllocatesNothing(t *testing.T) {
	for _, v := range []string{"approved", "changes_requested", "pending"} {
		if n := testing.AllocsPerRun(100, func() { _ = normVerdict(v) }); n != 0 {
			t.Errorf("normVerdict(%q): %v allocations", v, n)
		}
	}
	if got := normVerdict(" Request-Changes "); got != "changes_requested" {
		t.Errorf("normVerdict = %q", got)
	}
}
