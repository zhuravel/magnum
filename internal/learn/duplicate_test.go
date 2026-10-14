package learn

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// duplicateCase is one comment by rev-ann on a.rb line 42, made at
// commentAt, next to a finding the judge rejected on the same line for
// reason, in review 501 (run r-1) that magnum posted at reviewAt.
func duplicateCase(commentAt, reviewAt time.Time, reason string) Input {
	th := thread(1, "a.rb", 42, shaA, longish)
	th.Comments[0].CreatedAt = commentAt
	posted := github.Review{DatabaseID: 501, State: "COMMENTED", Body: "Two findings.", CommitOid: shaA, AuthorLogin: "talkable",
		AuthorType: "Bot", SubmittedAt: reviewAt, URL: "https://github.com/talkable/widgets/pull/7#pullrequestreview-501"}
	in := input([]github.Thread{th}, []github.Review{posted})
	in.Reviewed = []Reviewed{{SHA: shaA, At: reviewAt}}
	in.ReviewRuns = map[int64]string{501: "r-1"}
	in.Findings = []store.Finding{{RunID: "r-1", FindingID: "F1", Title: "Coupon scope dropped", Severity: "P1", Path: "a.rb", Line: 42,
		Verdict: store.FindingRejected, ReasonCode: reason, CreatedAt: reviewAt}}
	return in
}

// TestACommentTheJudgeReadAndRejectedAsADuplicateIsCaught: the judge rejects
// its own finding as a duplicate when a thread already covers the problem;
// a comment made before that review and next to the finding is that thread,
// so magnum caught it as it does when it posts the finding, and the retro
// makes no candidate of it.
func TestACommentTheJudgeReadAndRejectedAsADuplicateIsCaught(t *testing.T) {
	res, err := Build(duplicateCase(t0.Add(time.Hour), t0.Add(2*time.Hour), "duplicate"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Caught != 1 || len(res.Candidates) != 0 || len(res.Outside) != 0 || res.Dropped != 0 {
		t.Fatalf("caught %d, candidates %v, outside %v, dropped %d; want 1 caught and nothing else",
			res.Caught, ids(res.Candidates), ids(res.Outside), res.Dropped)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].ID != "r-1/F1" {
		t.Fatalf("the classifier still gets the rejected finding: %+v", res.Rejected)
	}
}

// TestACommentAfterTheReviewNearADuplicateRejectionStaysRaised: a comment
// made after the review, or in the very second it was posted, was never
// read by the judge that rejected the finding, so it keeps today's
// reading: a candidate that magnum raised and let go, with the reason.
func TestACommentAfterTheReviewNearADuplicateRejectionStaysRaised(t *testing.T) {
	review := t0.Add(2 * time.Hour)
	for name, commentAt := range map[string]time.Time{"after": review.Add(time.Minute), "in the same second": review} {
		res, err := Build(duplicateCase(commentAt, review, "duplicate"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Caught != 0 || !slices.Equal(ids(res.Candidates), []string{"t1"}) {
			t.Fatalf("%s: caught %d, candidates %v; want a candidate", name, res.Caught, ids(res.Candidates))
		}
		if c := res.Candidates[0]; c.Raised != store.MissRaisedRejected || c.FindingRef != "r-1/F1" || c.ReasonCode != "duplicate" {
			t.Fatalf("%s: candidate = %+v", name, c)
		}
	}
}

// TestADuplicateIsTheOnlyRejectionReasonThatCountsAsCaught: a finding
// rejected as not reproducible, speculative or pre-existing says the judge
// read the comment's problem and disagreed (or dismissed it), so a comment
// made before the review stays a candidate, raised and rejected.
func TestADuplicateIsTheOnlyRejectionReasonThatCountsAsCaught(t *testing.T) {
	for _, reason := range []string{"not_reproducible", "speculative", "pre_existing", "outside_diff", "style_only", "environment", ""} {
		res, err := Build(duplicateCase(t0.Add(time.Hour), t0.Add(2*time.Hour), reason))
		if err != nil {
			t.Fatal(err)
		}
		if res.Caught != 0 || !slices.Equal(ids(res.Candidates), []string{"t1"}) {
			t.Fatalf("reason %q: caught %d, candidates %v; want a candidate", reason, res.Caught, ids(res.Candidates))
		}
		if c := res.Candidates[0]; c.Raised != store.MissRaisedRejected || c.ReasonCode != reason {
			t.Fatalf("reason %q: candidate = %+v", reason, c)
		}
	}
}

// TestADuplicateRejectionCountsOnlyWhenTheReviewIsKnown: the review's time
// is that of the posted review of the finding's run (ReviewRuns, Reviews).
// With no review for the run, a review GitHub does not list, one with no
// time (pending) or a finding of another run, nothing proves the judge read
// the comment, so the comment stays a candidate as before.
func TestADuplicateRejectionCountsOnlyWhenTheReviewIsKnown(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"no run bound to the review": func(in *Input) { in.ReviewRuns = nil },
		"the review is not listed":   func(in *Input) { in.Reviews = nil },
		"the review has no time":     func(in *Input) { in.Reviews[0].SubmittedAt = time.Time{} },
		"a finding of another run":   func(in *Input) { in.Findings[0].RunID = "r-9" },
	} {
		in := duplicateCase(t0.Add(time.Hour), t0.Add(2*time.Hour), "duplicate")
		mutate(&in)
		res, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		if res.Caught != 0 || !slices.Equal(ids(res.Candidates), []string{"t1"}) || res.Candidates[0].Raised != store.MissRaisedRejected {
			t.Errorf("%s: caught %d, candidates %+v; want a raised candidate", name, res.Caught, res.Candidates)
		}
	}
}

// TestTheNearestRejectedFindingDecidesWhetherADuplicateCatchesTheComment: a
// duplicate rejection counts only for a comment within NearLines of it on
// its path, and when another rejected finding sits nearer the comment, that
// one is the comment's finding.
func TestTheNearestRejectedFindingDecidesWhetherADuplicateCatchesTheComment(t *testing.T) {
	farther := func(in *Input) { in.Findings[0].Line = 45 } // the duplicate, 3 lines below the comment
	for name, tc := range map[string]struct {
		extra  store.Finding
		caught bool
	}{
		"a duplicate and a rejection on another path": {
			store.Finding{RunID: "r-1", FindingID: "F2", Path: "b.rb", Line: 42, Verdict: store.FindingRejected, ReasonCode: "speculative"}, true},
		"a nearer rejection of another reason": {
			store.Finding{RunID: "r-1", FindingID: "F2", Path: "a.rb", Line: 44, Verdict: store.FindingRejected, ReasonCode: "speculative"}, false},
	} {
		in := duplicateCase(t0.Add(time.Hour), t0.Add(2*time.Hour), "duplicate")
		farther(&in)
		in.Findings = append(in.Findings, tc.extra)
		res, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Caught == 1; got != tc.caught {
			t.Errorf("%s: caught %d, candidates %+v; want caught = %v", name, res.Caught, res.Candidates, tc.caught)
		}
	}
	in := duplicateCase(t0.Add(time.Hour), t0.Add(2*time.Hour), "duplicate")
	in.Findings[0].Line = 46 // 4 lines below the comment: not near
	if res, err := Build(in); err != nil || res.Caught != 0 || len(res.Candidates) != 1 || res.Candidates[0].Raised != store.MissRaisedNone {
		t.Fatalf("a duplicate rejection 4 lines away: %+v, %v", res, err)
	}
}
