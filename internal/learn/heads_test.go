package learn

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// twoHeads is a pull request magnum reviewed at shaA (run r-1) and then at
// shaC (run r-2), with rev-ann's comment on a.rb line 45 of shaC, an hour
// after the first review, next to finding f of run r-1 three lines above
// it. changed says whether a.rb changed between the two heads (GitHub's
// comparison of shaA with shaC); asked counts the comparisons.
func twoHeads(f store.Finding, changed bool, asked *int) Input {
	in := input([]github.Thread{thread(1, "a.rb", 45, shaC, longish)}, nil)
	in.Reviewed = []Reviewed{{SHA: shaA, At: t0}, {SHA: shaC, At: t0.Add(30 * time.Minute)}}
	in.Targets = map[string]string{"r-1": shaA, "r-2": shaC}
	in.Findings = []store.Finding{f}
	in.Compare = func(from, to string) (Comparison, error) {
		*asked++
		if from != shaA || to != shaC {
			return Comparison{}, nil
		}
		cmp := Comparison{Descendant: true, Complete: true, Paths: []string{"b.rb"}}
		if changed {
			cmp.Paths = append(cmp.Paths, "a.rb")
		}
		return cmp, nil
	}
	return in
}

// TestACommentLinksToAnEarlierHeadsRejectedFindingOnlyWhenItsFileDidNotChange:
// a finding the judge rejected on line 42 of the first head and a comment on
// line 45 of the second are three lines apart by number, but when the file
// changed between the heads the numbers name other code (the 2026-10-23 run
// found a comment that the OAuth callback posts secrets to any host linked
// to "OAuth host gate blocks legitimate callbacks"), so the comment raised
// nothing; when the file did not change, the lines are the same code and
// the comment links to the finding. The comparison is asked once.
func TestACommentLinksToAnEarlierHeadsRejectedFindingOnlyWhenItsFileDidNotChange(t *testing.T) {
	rejected := store.Finding{RunID: "r-1", FindingID: "F1", Title: "Host gate blocks callbacks", Path: "a.rb", Line: 42,
		Verdict: store.FindingRejected, ReasonCode: "not_reproducible"}
	for name, tc := range map[string]struct {
		changed bool
		raised  string
		ref     string
	}{
		"the file changed":        {true, store.MissRaisedNone, ""},
		"the file did not change": {false, store.MissRaisedRejected, "r-1/F1"},
	} {
		asked := 0
		res, err := Build(twoHeads(rejected, tc.changed, &asked))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ids(res.Candidates), []string{"t1"}) || res.Caught != 0 {
			t.Fatalf("%s: candidates %v, caught %d", name, ids(res.Candidates), res.Caught)
		}
		if c := res.Candidates[0]; c.ReviewedSHA != shaC || c.Raised != tc.raised || c.FindingRef != tc.ref {
			t.Errorf("%s: candidate = %+v, want raised %q by %q", name, c, tc.raised, tc.ref)
		}
		if asked != 1 {
			t.Errorf("%s: comparisons = %d, want 1", name, asked)
		}
	}
}

// TestACommentIsCaughtByAnEarlierHeadsPostedFindingEvenWhenItsFileChanged:
// a finding magnum posted stays open as a thread on the later heads, so a
// comment on a later head that repeats it by place repeats magnum's own
// finding and is caught, whether or not the file changed between the
// heads; no comparison is asked for it.
func TestACommentIsCaughtByAnEarlierHeadsPostedFindingEvenWhenItsFileChanged(t *testing.T) {
	posted := store.Finding{RunID: "r-1", FindingID: "F1", Path: "a.rb", Line: 42, Verdict: store.FindingPosted}
	for name, changed := range map[string]bool{"the file changed": true, "the file did not change": false} {
		asked := 0
		res, err := Build(twoHeads(posted, changed, &asked))
		if err != nil {
			t.Fatal(err)
		}
		if res.Caught != 1 || len(res.Candidates) != 0 || len(res.Outside) != 0 {
			t.Errorf("%s: caught %d, candidates %v, outside %v; want caught", name, res.Caught, ids(res.Candidates), ids(res.Outside))
		}
		if asked != 0 {
			t.Errorf("%s: comparisons = %d, want none", name, asked)
		}
	}
}

// TestAFindingOfALaterHeadOrOfNoKnownRunIsNearNoComment: what magnum found
// at a head reviewed after the one a comment applies to, even with the file
// unchanged, came after the review the comment measures, and a finding
// whose run names no head belongs to none: neither catches nor links the
// comment, and no comparison is asked for them. A finding of the comment's
// own head still does.
func TestAFindingOfALaterHeadOrOfNoKnownRunIsNearNoComment(t *testing.T) {
	for name, tc := range map[string]struct {
		run, verdict string
		caught       int
		raised       string
	}{
		"a later head's posted finding":            {"r-2", store.FindingPosted, 0, store.MissRaisedNone},
		"a later head's rejected finding":          {"r-2", store.FindingRejected, 0, store.MissRaisedNone},
		"a posted finding of an unknown run":       {"r-9", store.FindingPosted, 0, store.MissRaisedNone},
		"a rejected finding of an unknown run":     {"r-9", store.FindingRejected, 0, store.MissRaisedNone},
		"a posted finding of the comment's head":   {"r-1", store.FindingPosted, 1, ""},
		"a rejected finding of the comment's head": {"r-1", store.FindingRejected, 0, store.MissRaisedRejected},
	} {
		in := input([]github.Thread{thread(1, "a.rb", 42, shaA, longish)}, nil)
		in.Reviewed = []Reviewed{{SHA: shaA, At: t0}, {SHA: shaC, At: t0.Add(30 * time.Minute)}}
		in.Targets = map[string]string{"r-1": shaA, "r-2": shaC}
		in.Findings = []store.Finding{{RunID: tc.run, FindingID: "F1", Path: "a.rb", Line: 42, Verdict: tc.verdict, ReasonCode: "speculative"}}
		in.Compare = func(string, string) (Comparison, error) { t.Fatalf("%s: compared", name); return Comparison{}, nil }
		res, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		if res.Caught != tc.caught || len(res.Candidates)+res.Caught != 1 || (tc.caught == 0 && res.Candidates[0].Raised != tc.raised) {
			t.Errorf("%s: caught %d, candidates %+v", name, res.Caught, res.Candidates)
		}
	}
}

// TestAnOutsideCommentIsCaughtByAPostedFindingButLinksNoRejectedOne: a
// comment on code magnum did not review has lines no reviewed head shares,
// so a finding the judge rejected beside it by number links nothing, and it
// is stored outside, having raised nothing. A finding magnum posted at the
// review before it is still open as a thread, so a comment that repeats it
// is caught, as on a reviewed head; one made before magnum's first review
// came before every posted finding and is outside.
func TestAnOutsideCommentIsCaughtByAPostedFindingButLinksNoRejectedOne(t *testing.T) {
	early := thread(1, "a.rb", 42, shaB, longish)
	early.Comments[0].CreatedAt = t0.Add(-time.Hour) // before magnum's first review
	moved := thread(2, "a.rb", 60, shaB, longish)    // after it, on a push that changed a.rb
	repeat := thread(3, "a.rb", 44, shaB, longish)   // the same push, beside magnum's posted finding
	in := input([]github.Thread{early, moved, repeat}, nil)
	in.Targets = map[string]string{"r-1": shaA}
	in.Findings = []store.Finding{
		{RunID: "r-1", FindingID: "F1", Path: "a.rb", Line: 42, Verdict: store.FindingPosted},
		{RunID: "r-1", FindingID: "F2", Path: "a.rb", Line: 61, Verdict: store.FindingRejected, ReasonCode: "speculative"},
	}
	in.Compare = func(string, string) (Comparison, error) {
		return Comparison{Descendant: true, Complete: true, Paths: []string{"a.rb"}}, nil
	}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Caught != 1 || len(res.Candidates) != 0 || !slices.Equal(ids(res.Outside), []string{"t1", "t2"}) {
		t.Fatalf("caught %d, candidates %v, outside %v; want t3 caught, t1 and t2 outside", res.Caught, ids(res.Candidates), ids(res.Outside))
	}
	for _, o := range res.Outside {
		if o.Raised != store.MissRaisedNone || o.FindingRef != "" {
			t.Errorf("%s: outside = %+v, want it to raise nothing", o.ID, o)
		}
	}
}
