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
// first page has a resolved multi-line thread with a reply and an outdated
// one on deleted lines (diffSide LEFT, no startLine, a comment whose
// originalCommit is null) with a ghost reply; the second a single-line thread without startLine keys at all and an
// orphan comment without a review. "root" is the alias that carries the first
// comment's originalCommit and diffHunk.
const threadsPage1 = `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4990,"used":10,"resetAt":"2026-10-04T12:00:00Z"},
"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"Y3Vyc29yOjE="},"nodes":[
 {"id":"PRRT_1","isResolved":true,"isOutdated":false,"path":"app/models/order.rb","line":42,"originalLine":40,"startLine":41,"originalStartLine":39,"diffSide":"RIGHT",
  "root":{"nodes":[{"originalCommit":{"oid":"1111111111111111111111111111111111111111"},"diffHunk":"@@ -38,3 +38,5 @@ def retry\n   ctx\n+  deliver\n+  deliver"}]},
  "comments":{"nodes":[
   {"databaseId":101,"body":"**[P2] Retry sends the email twice**\n\nDetails.","url":"https://github.com/talkable/talkable/pull/5#discussion_r101","createdAt":"2026-10-03T09:00:00Z","author":{"login":"talkable","__typename":"Bot"},"pullRequestReview":{"databaseId":9001}},
   {"databaseId":102,"body":"(Claude) Fixed in 1a2b3c4.","url":"https://github.com/talkable/talkable/pull/5#discussion_r102","createdAt":"2026-10-03T10:00:00Z","author":{"login":"octocat","__typename":"User"},"pullRequestReview":{"databaseId":9002}}]}},
 {"id":"PRRT_2","isResolved":false,"isOutdated":true,"path":"app/x.rb","line":null,"originalLine":7,"startLine":null,"originalStartLine":null,"diffSide":"LEFT",
  "root":{"nodes":[{"originalCommit":null,"diffHunk":"@@ -7 +7 @@\n-old\n+new"}]},
  "comments":{"nodes":[
   {"databaseId":103,"body":"**[P3] Wrong field name**","url":"u103","createdAt":"2026-10-03T09:00:00Z","author":{"login":"talkable","__typename":"Bot"},"pullRequestReview":{"databaseId":9001}},
   {"databaseId":104,"body":"by design","url":"u104","createdAt":"2026-10-03T11:00:00Z","author":null,"pullRequestReview":{"databaseId":9003}}]}}]}}}}}`

const threadsPage2 = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":"Y3Vyc29yOjI="},"nodes":[
 {"id":"PRRT_3","isResolved":false,"isOutdated":false,"path":"lib/y.go","line":3,"originalLine":3,
  "root":{"nodes":[{"originalCommit":{"oid":"2222222222222222222222222222222222222222"},"diffHunk":"@@ -3 +3 @@\n-a\n+b"}]},
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
		{ID: "PRRT_1", Path: "app/models/order.rb", Line: 42, OriginalLine: 40, StartLine: 41, OriginalStartLine: 39, DiffSide: "RIGHT", Resolved: true, Comments: []ThreadComment{
			{ID: 101, AuthorLogin: "talkable", AuthorType: "Bot", Body: "**[P2] Retry sends the email twice**\n\nDetails.",
				URL: "https://github.com/talkable/talkable/pull/5#discussion_r101", CreatedAt: at(9), ReviewID: 9001,
				OriginalCommitOid: "1111111111111111111111111111111111111111", DiffHunk: "@@ -38,3 +38,5 @@ def retry\n   ctx\n+  deliver\n+  deliver"},
			{ID: 102, AuthorLogin: "octocat", AuthorType: "User", Body: "(Claude) Fixed in 1a2b3c4.",
				URL: "https://github.com/talkable/talkable/pull/5#discussion_r102", CreatedAt: at(10), ReviewID: 9002},
		}},
		{ID: "PRRT_2", Path: "app/x.rb", OriginalLine: 7, DiffSide: "LEFT", Outdated: true, Comments: []ThreadComment{
			{ID: 103, AuthorLogin: "talkable", AuthorType: "Bot", Body: "**[P3] Wrong field name**", URL: "u103", CreatedAt: at(9), ReviewID: 9001,
				DiffHunk: "@@ -7 +7 @@\n-old\n+new"},
			{ID: 104, Body: "by design", URL: "u104", CreatedAt: at(11), ReviewID: 9003},
		}},
		{ID: "PRRT_3", Path: "lib/y.go", Line: 3, OriginalLine: 3, Comments: []ThreadComment{
			{ID: 105, AuthorLogin: "octocat", AuthorType: "User", Body: "a question", URL: "u105", CreatedAt: at(12),
				OriginalCommitOid: "2222222222222222222222222222222222222222", DiffHunk: "@@ -3 +3 @@\n-a\n+b"},
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
	for _, want := range []string{"reviewThreads(first: 100, after: $cursor)", "id isResolved isOutdated path line originalLine startLine originalStartLine",
		"comments(first: 50)", "author { login __typename } pullRequestReview { databaseId }",
		"root: comments(first: 1) { nodes { originalCommit { oid } diffHunk } }"} {
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

func TestReviewThreadsCopiesRootFieldsOntoTheFirstCommentOnly(t *testing.T) {
	// A thread whose "root" alias is missing or empty keeps plain comments;
	// replies never get the hunk of the first comment.
	body := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[
	 {"id":"PRRT_a","isResolved":false,"isOutdated":false,"path":"a.go","line":1,"originalLine":1,
	  "comments":{"nodes":[{"databaseId":1,"body":"x","url":"u1","createdAt":"2026-10-03T09:00:00Z","author":null,"pullRequestReview":null}]}},
	 {"id":"PRRT_b","isResolved":false,"isOutdated":false,"path":"b.go","line":2,"originalLine":2,
	  "root":{"nodes":[{"originalCommit":{"oid":"3333333333333333333333333333333333333333"},"diffHunk":"@@ -2 +2 @@\n-a\n+b"}]},
	  "comments":{"nodes":[
	   {"databaseId":2,"body":"first","url":"u2","createdAt":"2026-10-03T09:00:00Z","author":null,"pullRequestReview":null},
	   {"databaseId":3,"body":"reply","url":"u3","createdAt":"2026-10-03T10:00:00Z","author":null,"pullRequestReview":null}]}},
	 {"id":"PRRT_c","isResolved":false,"isOutdated":false,"path":"c.go","line":3,"originalLine":3,
	  "root":{"nodes":[]},"comments":{"nodes":[]}}]}}}}}`
	c := &Client{Run: &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: compact(t, body)}}}}}
	got, err := c.ReviewThreads(context.Background(), "talkable", "talkable", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("threads = %+v", got)
	}
	if a := got[0].Comments[0]; a.OriginalCommitOid != "" || a.DiffHunk != "" || got[0].StartLine != 0 || got[0].OriginalStartLine != 0 {
		t.Errorf("thread without root or startLine = %+v", got[0])
	}
	b := got[1].Comments
	if b[0].OriginalCommitOid != "3333333333333333333333333333333333333333" || b[0].DiffHunk != "@@ -2 +2 @@\n-a\n+b" {
		t.Errorf("first comment = %+v", b[0])
	}
	if b[1].OriginalCommitOid != "" || b[1].DiffHunk != "" {
		t.Errorf("reply must not carry the root fields: %+v", b[1])
	}
	if len(got[2].Comments) != 0 {
		t.Errorf("thread without comments = %+v", got[2])
	}
}
