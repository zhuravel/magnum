package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// Synthetic pages in the shape of GitHub's reviewThreads connection: the
// first page has a resolved thread with a reply and an outdated one with a
// ghost reply; the second an orphan comment without a review.
const threadsPage1 = `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4990,"used":10,"resetAt":"2026-10-04T12:00:00Z"},
"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"Y3Vyc29yOjE="},"nodes":[
 {"id":"PRRT_1","isResolved":true,"isOutdated":false,"path":"app/models/order.rb","line":42,"originalLine":40,
  "comments":{"nodes":[
   {"databaseId":101,"body":"**[P2] Retry sends the email twice**\n\nDetails.","url":"https://github.com/talkable/talkable/pull/5#discussion_r101","createdAt":"2026-10-03T09:00:00Z","author":{"login":"talkable","__typename":"Bot"},"pullRequestReview":{"databaseId":9001}},
   {"databaseId":102,"body":"(Claude) Fixed in 1a2b3c4.","url":"https://github.com/talkable/talkable/pull/5#discussion_r102","createdAt":"2026-10-03T10:00:00Z","author":{"login":"octocat","__typename":"User"},"pullRequestReview":{"databaseId":9002}}]}},
 {"id":"PRRT_2","isResolved":false,"isOutdated":true,"path":"app/x.rb","line":null,"originalLine":7,
  "comments":{"nodes":[
   {"databaseId":103,"body":"**[P3] Wrong field name**","url":"u103","createdAt":"2026-10-03T09:00:00Z","author":{"login":"talkable","__typename":"Bot"},"pullRequestReview":{"databaseId":9001}},
   {"databaseId":104,"body":"by design","url":"u104","createdAt":"2026-10-03T11:00:00Z","author":null,"pullRequestReview":{"databaseId":9003}}]}}]}}}}}`

const threadsPage2 = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":"Y3Vyc29yOjI="},"nodes":[
 {"id":"PRRT_3","isResolved":false,"isOutdated":false,"path":"lib/y.go","line":3,"originalLine":3,
  "comments":{"nodes":[{"databaseId":105,"body":"a question","url":"u105","createdAt":"2026-10-03T12:00:00Z","author":{"login":"octocat","__typename":"User"},"pullRequestReview":null}]}}]}}}}}`

func TestReviewThreads(t *testing.T) {
	var cursors []any
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		cursors = append(cursors, req.Variables["cursor"])
		if req.Variables["cursor"] == nil {
			return okResult(compact(t, threadsPage1))
		}
		return okResult(compact(t, threadsPage2))
	})}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}
	got, err := c.ReviewThreads(context.Background(), "talkable", "talkable", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cursors, []any{nil, "Y3Vyc29yOjE="}) {
		t.Fatalf("cursors = %v", cursors)
	}
	at := func(h int) time.Time { return time.Date(2026, 10, 3, h, 0, 0, 0, time.UTC) }
	want := []Thread{
		{ID: "PRRT_1", Path: "app/models/order.rb", Line: 42, OriginalLine: 40, Resolved: true, Comments: []ThreadComment{
			{ID: 101, AuthorLogin: "talkable", AuthorType: "Bot", Body: "**[P2] Retry sends the email twice**\n\nDetails.",
				URL: "https://github.com/talkable/talkable/pull/5#discussion_r101", CreatedAt: at(9), ReviewID: 9001},
			{ID: 102, AuthorLogin: "octocat", AuthorType: "User", Body: "(Claude) Fixed in 1a2b3c4.",
				URL: "https://github.com/talkable/talkable/pull/5#discussion_r102", CreatedAt: at(10), ReviewID: 9002},
		}},
		{ID: "PRRT_2", Path: "app/x.rb", OriginalLine: 7, Outdated: true, Comments: []ThreadComment{
			{ID: 103, AuthorLogin: "talkable", AuthorType: "Bot", Body: "**[P3] Wrong field name**", URL: "u103", CreatedAt: at(9), ReviewID: 9001},
			{ID: 104, Body: "by design", URL: "u104", CreatedAt: at(11), ReviewID: 9003},
		}},
		{ID: "PRRT_3", Path: "lib/y.go", Line: 3, OriginalLine: 3, Comments: []ThreadComment{
			{ID: 105, AuthorLogin: "octocat", AuthorType: "User", Body: "a question", URL: "u105", CreatedAt: at(12)},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("threads = %+v\nwant %+v", got, want)
	}
	call := f.Calls[0]
	if call.Mutates || call.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
		t.Errorf("call = %+v", call)
	}
	q := oneLine(decodeReq(t, call).Query)
	for _, want := range []string{"reviewThreads(first: 100, after: $cursor)", "id isResolved isOutdated path line originalLine",
		"comments(first: 50)", "author { login __typename } pullRequestReview { databaseId }"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q: %s", want, q)
		}
	}
}

func TestReviewThreadsEmptyAndErrors(t *testing.T) {
	empty := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: compact(t,
		`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}`)}}}}
	got, err := (&Client{Run: empty}).ReviewThreads(context.Background(), "talkable", "talkable", 5)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no threads = %v, %v (want an empty, non-nil list)", got, err)
	}

	missing := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "reviews_notfound.json"), Stderr: fixture(t, "reviews_notfound.stderr"), Code: 1},
	}}}
	if _, err := (&Client{Run: missing}).ReviewThreads(context.Background(), "talkable", "talkable", 99999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing PR = %v, want ErrNotFound", err)
	}
	if _, err := (&Client{Run: empty}).ReviewThreads(context.Background(), "talkable", "talkable", 0); err == nil {
		t.Fatal("PR 0: want an error")
	}
	if _, err := (&Client{Run: empty}).ReviewThreads(context.Background(), "", "talkable", 5); err == nil {
		t.Fatal("no owner: want an error")
	}
}
