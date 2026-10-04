package store

import (
	"testing"
)

// A judge result says what the review concluded: findings by priority, the
// simplifications its candidates suggested, the earlier findings, and the
// verdict it wrote; a result from before the verdict field gets one from
// the counts.
func TestParseReviewResultReadsTheConclusion(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		want       ReviewSummary
	}{
		{"verdict written", `{"event":"COMMENT","verdict":"blocking","findings":{"P0":0,"P1":1,"P2":3,"P3":0},
			"previous_findings":{"fixed":2,"open":1,"answered":1},"candidates":{"claude-review":{"accepted":1},"claude-simplify":{"suggested":4}}}`,
			ReviewSummary{Event: "COMMENT", Counts: [4]int{0, 1, 3, 0}, Simplifications: 4, Fixed: 2, Open: 1, Answered: 1, Verdict: VerdictBlocking}},
		{"older result: P1 blocks", `{"event":"COMMENT","findings":{"P1":2}}`,
			ReviewSummary{Event: "COMMENT", Counts: [4]int{0, 2, 0, 0}, Verdict: VerdictBlocking}},
		{"older result: P2 comments", `{"event":"COMMENT","findings":{"P2":1}}`,
			ReviewSummary{Event: "COMMENT", Counts: [4]int{0, 0, 1, 0}, Verdict: VerdictNonBlocking}},
		{"older result: an open earlier finding comments", `{"event":"COMMENT","findings":{},"previous_findings":{"open":1}}`,
			ReviewSummary{Event: "COMMENT", Open: 1, Verdict: VerdictNonBlocking}},
		{"clean", `{"event":"APPROVE","findings":{"P0":0,"P1":0,"P2":0,"P3":0},"candidates":{"claude-simplify":{"suggested":2}}}`,
			ReviewSummary{Event: "APPROVE", Simplifications: 2, Verdict: VerdictClean}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got ReviewSummary
			if !ParseReviewResult([]byte(tc.json), &got) || got != tc.want {
				t.Fatalf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
	var s ReviewSummary
	if ParseReviewResult([]byte(`{"status":"blocked"}`), &s) || ParseReviewResult([]byte(`not json`), &s) {
		t.Fatal("a result without findings parsed")
	}
}
