package store

import (
	"reflect"
	"testing"
)

// A re-review's result counts only the findings it posts as new; the
// earlier findings still open come by priority in previous_findings.open,
// so a reader knows the PR is still blocked without the verdict line (a
// judge posted "Blocking: 1 problem…" with every count at 0). A result
// written before the split gives only their number, and no priorities.
func TestParseReviewResultReadsTheOpenFindingsByPriority(t *testing.T) {
	var got ReviewSummary
	if !ParseReviewResult([]byte(`{"event":"REQUEST_CHANGES","verdict":"blocking","findings":{"P0":0,"P1":0,"P2":1,"P3":0},
		"previous_findings":{"fixed":1,"open":{"P0":0,"P1":2,"P2":0,"P3":0},"answered":0,"rebutted":0}}`), &got) {
		t.Fatal("the result did not parse")
	}
	want := ReviewSummary{Event: "REQUEST_CHANGES", Counts: [4]int{0, 0, 1, 0}, Fixed: 1, Open: 2, OpenCounts: &[4]int{0, 2, 0, 0}, Verdict: VerdictBlocking}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v (open %v)\nwant %+v (open %v)", got, got.OpenCounts, want, want.OpenCounts)
	}

	// Without a verdict, an open P1 blocks; a missing priority counts 0.
	got = ReviewSummary{}
	if !ParseReviewResult([]byte(`{"event":"COMMENT","findings":{},"previous_findings":{"open":{"P1":1,"P3":1}}}`), &got) ||
		got.Open != 2 || got.OpenCounts == nil || *got.OpenCounts != [4]int{0, 1, 0, 1} || got.Verdict != VerdictBlocking {
		t.Fatalf("no verdict: %+v (open %v)", got, got.OpenCounts)
	}

	// An older result: the number of open findings, no priorities.
	got = ReviewSummary{}
	want = ReviewSummary{Event: "COMMENT", Counts: [4]int{0, 0, 1, 0}, Open: 3, Answered: 1, Verdict: VerdictNonBlocking}
	if !ParseReviewResult([]byte(`{"event":"COMMENT","verdict":"non_blocking","findings":{"P2":1},"previous_findings":{"fixed":0,"open":3,"answered":1}}`), &got) ||
		!reflect.DeepEqual(got, want) {
		t.Fatalf("older result:\ngot %+v\nwant %+v", got, want)
	}
}

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
			if !ParseReviewResult([]byte(tc.json), &got) || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
	var s ReviewSummary
	if ParseReviewResult([]byte(`{"status":"blocked"}`), &s) || ParseReviewResult([]byte(`not json`), &s) {
		t.Fatal("a result without findings parsed")
	}
}
