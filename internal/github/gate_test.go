package github

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// details_gate.json is synthetic, in the shape GitHub answers the fragment
// with: what GitHub's merge gate says of each PR's reviews. #401 waits for a
// review that counts (an App's approval is in latestReviews, never in
// latestOpinionatedReviews), #402 has the operator's changes request on an
// older commit and someone's approval, #403 someone else's changes request
// next to the operator's dismissed one, #404 needs no review (null), #405 is
// approved, #406's opinions run past the page of 100. An answer without the
// fields (an older fixture) has no gate.
func TestDetailsReadsTheReviewGate(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: fixture(t, "details_gate.json")}}}}
	got, missing, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{401, 402, 403, 404, 405, 406})
	if err != nil || len(missing) != 0 || len(got) != 6 {
		t.Fatalf("got %d, missing %v, err %v", len(got), missing, err)
	}
	q := oneLine(decodeReq(t, f.Calls[0]).Query)
	want := "reviewDecision latestOpinionatedReviews(first: 100, writersOnly: true) { totalCount pageInfo { hasNextPage } " +
		"nodes { state author { login __typename } commit { oid } } }"
	if !strings.Contains(q, want) {
		t.Errorf("query lacks %q: %s", want, q)
	}
	cases := map[int]ReviewGate{
		401: {Decision: "REVIEW_REQUIRED", Opinions: []LatestReview{}, Complete: true},
		402: {Decision: "CHANGES_REQUESTED", Complete: true, Opinions: []LatestReview{
			{State: "CHANGES_REQUESTED", AuthorLogin: "zhuravel", AuthorType: "User", CommitOid: "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"},
			{State: "APPROVED", AuthorLogin: "rev-ann", AuthorType: "User", CommitOid: "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"}}},
		403: {Decision: "CHANGES_REQUESTED", Complete: true, Opinions: []LatestReview{
			{State: "CHANGES_REQUESTED", AuthorLogin: "bob-rev", AuthorType: "User", CommitOid: "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3"},
			{State: "DISMISSED", AuthorLogin: "zhuravel", AuthorType: "User"}}},
		404: {Decision: "", Opinions: []LatestReview{}, Complete: true},
		405: {Decision: "APPROVED", Complete: true, Opinions: []LatestReview{
			{State: "APPROVED", AuthorLogin: "rev-ann", AuthorType: "User", CommitOid: "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5"}}},
		406: {Decision: "CHANGES_REQUESTED", Complete: false, Opinions: []LatestReview{
			{State: "CHANGES_REQUESTED", AuthorLogin: "zhuravel", AuthorType: "User", CommitOid: "e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6"}}},
	}
	for n, want := range cases {
		d := got[n]
		if d.ReviewGate == nil {
			t.Errorf("#%d: no review gate", n)
			continue
		}
		if !reflect.DeepEqual(*d.ReviewGate, want) {
			t.Errorf("#%d gate = %+v, want %+v", n, *d.ReviewGate, want)
		}
	}

	old := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "details.json"), Stderr: fixture(t, "details.stderr"), Code: 1}}}}
	prev, _, err := (&Client{Run: old}).Details(context.Background(), "talkable", "talkable", []int{11975})
	if err != nil {
		t.Fatal(err)
	}
	if g := prev[11975].ReviewGate; g != nil {
		t.Errorf("an answer without the fields has gate %+v", *g)
	}
}
