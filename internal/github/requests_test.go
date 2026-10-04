package github

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// requestsBody answers a two-PR details query: #5 has five ReviewRequestedEvent
// nodes (User, Bot, Team, a deleted reviewer, a ghost actor), #6 has no
// timelineItems key at all (an older answer).
const requestsBody = `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4990,"used":10,"resetAt":"2026-10-02T21:50:10Z"},
"repository":{
"p5":{"id":"PR_5","number":5,"title":"t","url":"u","author":{"login":"alice","__typename":"User"},"labels":{"nodes":[]},
"headRefName":"h","baseRefName":"master","isCrossRepository":false,"state":"OPEN","merged":false,"mergedAt":null,
"closedAt":null,"updatedAt":"2026-10-02T09:04:36Z","isDraft":false,"headRefOid":"abc",
"reviewRequests":{"nodes":[]},"latestReviews":{"nodes":[]},
"timelineItems":{"nodes":[
{"createdAt":"2026-10-02T08:00:00Z","actor":{"login":"alice"},"requestedReviewer":{"__typename":"User","login":"bob"}},
{"createdAt":"2026-10-02T08:05:00Z","actor":{"login":"alice"},"requestedReviewer":{"__typename":"Bot","login":"talkable"}},
{"createdAt":"2026-10-02T08:10:00Z","actor":{"login":"bob"},"requestedReviewer":{"__typename":"Team","slug":"reviewers"}},
{"createdAt":"2026-10-02T08:15:00Z","actor":{"login":"alice"},"requestedReviewer":null},
{"createdAt":"2026-10-02T08:20:00Z","actor":null,"requestedReviewer":{"__typename":"User","login":"alice"}}
]}},
"p6":{"id":"PR_6","number":6,"title":"t","url":"u","author":{"login":"alice","__typename":"User"},"labels":{"nodes":[]},
"headRefName":"h","baseRefName":"master","isCrossRepository":false,"state":"OPEN","merged":false,"mergedAt":null,
"closedAt":null,"updatedAt":"2026-10-02T09:04:36Z","isDraft":false,"headRefOid":"def",
"reviewRequests":{"nodes":[]},"latestReviews":{"nodes":[]}}
}}}`

func TestDetailsParsesReviewRequestEvents(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: compact(t, requestsBody)}}}}
	got, missing, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{5, 6})
	if err != nil || len(missing) != 0 {
		t.Fatalf("err = %v missing = %v", err, missing)
	}

	at := func(h, m int) time.Time { return time.Date(2026, 10, 2, h, m, 0, 0, time.UTC) }
	want := []ReviewRequestEvent{
		{CreatedAt: at(8, 0), Actor: "alice", Reviewer: Reviewer{Type: "User", Login: "bob"}},
		{CreatedAt: at(8, 5), Actor: "alice", Reviewer: Reviewer{Type: "Bot", Login: "talkable"}},
		{CreatedAt: at(8, 10), Actor: "bob", Reviewer: Reviewer{Type: "Team", Login: "reviewers"}},
		// the node with a null requestedReviewer (08:15) is dropped; a null actor is kept with Actor ""
		{CreatedAt: at(8, 20), Actor: "", Reviewer: Reviewer{Type: "User", Login: "alice"}},
	}
	if !reflect.DeepEqual(got[5].ReviewRequestEvents, want) {
		t.Errorf("events =\n%+v\nwant\n%+v", got[5].ReviewRequestEvents, want)
	}
	if ev := got[5].ReviewRequestEvents[1]; !SameLogin(ev.Reviewer.Login, "talkable[bot]") {
		t.Errorf("GraphQL bot login %q must match the REST [bot] login", ev.Reviewer.Login)
	}
	// the pending reviewRequests list is read from its own connection, unaffected
	if len(got[5].ReviewRequests) != 0 {
		t.Errorf("review requests = %+v", got[5].ReviewRequests)
	}
}

func TestDetailsWithoutTimelineItemsHasEmptyNonNilEvents(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: compact(t, requestsBody)}}}}
	got, _, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{5, 6})
	if err != nil {
		t.Fatal(err)
	}
	if ev := got[6].ReviewRequestEvents; ev == nil || len(ev) != 0 {
		t.Errorf("a PR without timelineItems must have an empty, non-nil slice: %#v", ev)
	}

	// an old fixture captured before the timeline was queried decodes the same way
	old := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "details.json"), Stderr: fixture(t, "details.stderr"), Code: 1},
	}}}
	oldGot, _, err := (&Client{Run: old}).Details(context.Background(), "talkable", "talkable", []int{11975})
	if err != nil {
		t.Fatal(err)
	}
	if ev := oldGot[11975].ReviewRequestEvents; ev == nil || len(ev) != 0 {
		t.Errorf("old fixture events = %#v, want empty non-nil", ev)
	}
}

func TestDetailsEmptyTimelineNodesHasEmptyNonNilEvents(t *testing.T) {
	body := compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4990,"used":10,"resetAt":"2026-10-02T21:50:10Z"},
	"repository":{"p5":{"id":"PR_5","number":5,"state":"OPEN","headRefOid":"abc","labels":{"nodes":[]},
	"reviewRequests":{"nodes":[]},"latestReviews":{"nodes":[]},"timelineItems":{"nodes":[]}}}}}`)
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: body}}}}
	got, _, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{5})
	if err != nil {
		t.Fatal(err)
	}
	if ev := got[5].ReviewRequestEvents; ev == nil || len(ev) != 0 {
		t.Errorf("events = %#v, want empty non-nil", ev)
	}
}

func TestDetailsDropsEventsWithoutTime(t *testing.T) {
	body := compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4990,"used":10,"resetAt":"2026-10-02T21:50:10Z"},
	"repository":{"p5":{"id":"PR_5","number":5,"state":"OPEN","headRefOid":"abc","labels":{"nodes":[]},
	"reviewRequests":{"nodes":[]},"latestReviews":{"nodes":[]},"timelineItems":{"nodes":[
	{"createdAt":null,"actor":{"login":"alice"},"requestedReviewer":{"__typename":"User","login":"bob"}},
	{"actor":{"login":"alice"},"requestedReviewer":{"__typename":"User","login":"bob"}},
	{"createdAt":"2026-10-02T08:00:00Z","actor":{"login":"alice"},"requestedReviewer":{"__typename":"Mannequin","login":"ghost-bob"}}
	]}}}}}`)
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: body}}}}
	got, _, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{5})
	if err != nil {
		t.Fatal(err)
	}
	want := []ReviewRequestEvent{{
		CreatedAt: time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC),
		Actor:     "alice",
		Reviewer:  Reviewer{Type: "Mannequin", Login: "ghost-bob"},
	}}
	if !reflect.DeepEqual(got[5].ReviewRequestEvents, want) {
		t.Errorf("events = %+v, want only the node with a time", got[5].ReviewRequestEvents)
	}
}

func TestDetailsQueryReadsReviewRequestedEvents(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: compact(t, requestsBody)}}}}
	if _, _, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{5, 6}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want one batched query", len(f.Calls))
	}
	q := oneLine(decodeReq(t, f.Calls[0]).Query)
	for _, want := range []string{
		"REVIEW_REQUESTED_EVENT",
		"timelineItems(itemTypes: [REVIEW_REQUESTED_EVENT], last: 10)",
		"... on ReviewRequestedEvent { createdAt actor { login } requestedReviewer {",
		"... on Team { slug }",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q: %s", want, q)
		}
	}
}
