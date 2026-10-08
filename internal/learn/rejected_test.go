package learn

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// webhookCase is a person's comment on the controller line no example
// covers, next to two findings the judge rejected: the test gap it raised
// on the spec file (F2, speculative) and a pre-existing problem 2 lines
// from the comment (F3).
func webhookCase() Input {
	in := input([]github.Thread{thread(1, "app/controllers/hooks/webhook_controller.rb", 56, shaA, longish)}, nil)
	in.Findings = []store.Finding{
		{RunID: "r-1", FindingID: "F1", Title: "Platform secret accepted for Lite shops", Severity: "P2",
			Path: "app/controllers/hooks/webhook_controller.rb", Line: 30, Verdict: store.FindingPosted, CreatedAt: t0},
		{RunID: "r-1", FindingID: "F2", Title: "No example signs a known shop's webhook with the new secret", Severity: "P3",
			Path: "spec/controllers/hooks/webhook_controller_spec.rb", Line: 120, Verdict: store.FindingRejected, ReasonCode: "speculative", CreatedAt: t0},
		{RunID: "r-1", FindingID: "F3", Title: "An array hmac raises TypeError", Severity: "P3",
			Path: "app/controllers/hooks/webhook_controller.rb", Line: 58, Verdict: store.FindingRejected, ReasonCode: "pre_existing", CreatedAt: t0},
	}
	return in
}

// TestBuildListsTheRejectedFindingsForTheClassifier: the classifier gets
// the findings the judge rejected, each with its reference, title, path,
// reason and priority, so it can match a comment by meaning; a posted
// finding is not listed.
func TestBuildListsTheRejectedFindingsForTheClassifier(t *testing.T) {
	res, err := Build(webhookCase())
	if err != nil {
		t.Fatal(err)
	}
	want := []Rejected{
		{ID: "r-1/F2", Title: "No example signs a known shop's webhook with the new secret", Path: "spec/controllers/hooks/webhook_controller_spec.rb",
			Reason: "speculative", Priority: "P3"},
		{ID: "r-1/F3", Title: "An array hmac raises TypeError", Path: "app/controllers/hooks/webhook_controller.rb", Reason: "pre_existing", Priority: "P3"},
	}
	if !slices.Equal(res.Rejected, want) {
		t.Fatalf("rejected = %+v\nwant %+v", res.Rejected, want)
	}
}

// TestBuildListsOnlyTheNewestRejectedFindings: the list is bounded to the
// RejectedMax newest rejected findings, oldest first, and leaves out a row
// recorded before findings had titles (nothing to match by meaning).
func TestBuildListsOnlyTheNewestRejectedFindings(t *testing.T) {
	in := input([]github.Thread{thread(1, "a.rb", 10, shaA, longish)}, nil)
	n := RejectedMax + 3
	for i := range n {
		f := store.Finding{RunID: fmt.Sprintf("r-%d", i/10), FindingID: fmt.Sprintf("F%d", i%10+1), Title: fmt.Sprintf("Finding %d", i),
			Path: "a.rb", Line: 100 + i, Verdict: store.FindingRejected, ReasonCode: "speculative", CreatedAt: t0.Add(time.Duration(i) * time.Minute)}
		if i == n-1 {
			f.Title = "" // recorded before titles
		}
		in.Findings = append(in.Findings, f)
	}
	// FindingsByPR's order, oldest first, is not Build's to trust.
	slices.Reverse(in.Findings)
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rejected) != RejectedMax {
		t.Fatalf("rejected %d rows, want %d", len(res.Rejected), RejectedMax)
	}
	if first, last := res.Rejected[0], res.Rejected[RejectedMax-1]; first.Title != "Finding 2" || last.Title != fmt.Sprintf("Finding %d", n-2) {
		t.Fatalf("rows run from %q to %q, want the newest titled ones, oldest first", first.Title, last.Title)
	}
}

// TestLinkRejectedTakesTheFindingTheClassifierNamed: a comment on the code
// whose test gap the judge raised on the spec file and rejected as
// speculative is linked to that finding when the classifier names it, not
// to the pre-existing one that sits 2 lines from the comment.
func TestLinkRejectedTakesTheFindingTheClassifierNamed(t *testing.T) {
	res, err := Build(webhookCase())
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Candidates[0]; c.FindingRef != "r-1/F3" {
		t.Fatalf("by position the comment is F3's: %+v", c)
	}
	items := map[string]Item{"t1": {ID: "t1", Class: store.MissMiss, Rejected: "r-1/F2"}}
	got := LinkRejected(res.Candidates, items, res.Rejected)
	if c := got[0]; c.Raised != store.MissRaisedRejected || c.FindingRef != "r-1/F2" || c.ReasonCode != "speculative" {
		t.Fatalf("linked candidate = %+v, want rejected speculative r-1/F2", c)
	}
	if res.Candidates[0].FindingRef != "r-1/F3" {
		t.Fatal("LinkRejected changed the candidates it was given")
	}
}

// TestLinkRejectedLeavesItToNearbyWhenNoFindingIsNamed: an item that
// names no rejected finding keeps what Build found by path and line, and
// a candidate with nothing near stays not raised.
func TestLinkRejectedLeavesItToNearbyWhenNoFindingIsNamed(t *testing.T) {
	in := webhookCase()
	in.Threads = append(in.Threads, thread(2, "app/models/shop.rb", 10, shaA, longish))
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	items := map[string]Item{"t1": {ID: "t1", Class: store.MissMiss}, "t2": {ID: "t2", Class: store.MissNotIssue}}
	for _, it := range []map[string]Item{items, nil} { // classified, or left unclassified
		got := LinkRejected(res.Candidates, it, res.Rejected)
		if c := got[0]; c.Raised != store.MissRaisedRejected || c.FindingRef != "r-1/F3" || c.ReasonCode != "pre_existing" {
			t.Fatalf("t1 = %+v, want what is near it", c)
		}
		if c := got[1]; c.Raised != store.MissRaisedNone || c.FindingRef != "" || c.ReasonCode != "" {
			t.Fatalf("t2 = %+v, want not raised", c)
		}
	}
}
