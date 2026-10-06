package github

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// dismissals.json is synthetic, in the shape GitHub answers the query with:
// the operator dismissed review 7001 by hand, GitHub dismissed 7002 as stale
// when alice pushed c1c1…, a dismissal of a deleted review by a deleted
// account names nothing, and an App dismissed 7004 (its login in Account
// form).
func TestReviewDismissalsReadsWhoDismissedEachReview(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: fixture(t, "dismissals.json")}}}}
	got, err := (&Client{Run: f}).ReviewDismissals(context.Background(), "talkable", "talkable", 12001)
	if err != nil {
		t.Fatal(err)
	}
	q := oneLine(decodeReq(t, f.Calls[0]).Query)
	for _, want := range []string{"timelineItems(last: 100, itemTypes: [REVIEW_DISMISSED_EVENT])",
		"... on ReviewDismissedEvent { createdAt actor { login __typename } review { databaseId } pullRequestCommit { commit { oid } } }"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q: %s", want, q)
		}
	}
	at := func(h int) time.Time { return time.Date(2026, 10, 6, h, 0, 0, 0, time.UTC) }
	want := []ReviewDismissal{
		{ReviewID: 7001, Actor: "zhuravel", At: at(10)},
		{ReviewID: 7002, Actor: "alice", ByPush: true, Commit: "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1", At: at(11)},
		{At: at(12)},
		{ReviewID: 7004, Actor: "helper[bot]", At: at(13)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dismissals = %+v\nwant %+v", got, want)
	}
	if _, err := (&Client{Run: f}).ReviewDismissals(context.Background(), "talkable", "talkable", 0); err == nil {
		t.Error("PR 0 was asked for")
	}
}
