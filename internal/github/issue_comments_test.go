package github

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// A comment on a PR's conversation and its edit go out as JSON on gh's
// stdin, marked Mutates so a dry run only plans them, and answer the
// comment's id and link.
func TestIssueCommentsSendJSONOnStdin(t *testing.T) {
	answer := `{"id":987,"html_url":"https://github.com/talkable/talkable/pull/5#issuecomment-987"}`
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: []byte(answer)}}}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}
	body := "<!-- magnum:status -->\n$(rm -rf) stays text"
	want := IssueComment{ID: 987, URL: "https://github.com/talkable/talkable/pull/5#issuecomment-987"}

	got, err := c.CreateIssueComment(context.Background(), "talkable", "talkable", 5, body)
	if err != nil || got != want {
		t.Fatalf("create = %+v, %v; want %+v", got, err, want)
	}
	got, err = c.EditIssueComment(context.Background(), "talkable", "talkable", 987, body)
	if err != nil || got != want {
		t.Fatalf("edit = %+v, %v; want %+v", got, err, want)
	}
	wantArgs := [][]string{
		{"api", "-X", "POST", "repos/talkable/talkable/issues/5/comments", "--hostname", "github.com", "--input", "-"},
		{"api", "-X", "PATCH", "repos/talkable/talkable/issues/comments/987", "--hostname", "github.com", "--input", "-"},
	}
	if len(f.Calls) != len(wantArgs) {
		t.Fatalf("calls = %+v", f.Calls)
	}
	for i, cmd := range f.Calls {
		if !reflect.DeepEqual(cmd.Args, wantArgs[i]) {
			t.Errorf("call %d args = %q, want %q", i, cmd.Args, wantArgs[i])
		}
		if !cmd.Mutates {
			t.Errorf("call %d is not marked Mutates", i)
		}
		if cmd.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
			t.Errorf("call %d env = %v", i, cmd.Env)
		}
		for _, a := range cmd.Args {
			if strings.Contains(a, "rm -rf") {
				t.Errorf("call %d: the body reached argv: %q", i, cmd.Args)
			}
		}
		var sent map[string]any
		if err := json.Unmarshal(cmd.Stdin, &sent); err != nil {
			t.Fatal(err)
		}
		if len(sent) != 1 || sent["body"] != body {
			t.Errorf("call %d stdin = %s", i, cmd.Stdin)
		}
	}
}

// A malformed request never reaches gh.
func TestIssueCommentsRefuseMalformedRequests(t *testing.T) {
	f := &execx.Fake{}
	c := &Client{Run: f}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"create: no owner":   func() error { _, err := c.CreateIssueComment(ctx, "", "talkable", 5, "x"); return err },
		"create: bad repo":   func() error { _, err := c.CreateIssueComment(ctx, "talkable", "../x", 5, "x"); return err },
		"create: pr 0":       func() error { _, err := c.CreateIssueComment(ctx, "talkable", "talkable", 0, "x"); return err },
		"create: blank body": func() error { _, err := c.CreateIssueComment(ctx, "talkable", "talkable", 5, " \n\t"); return err },
		"edit: bad repo":     func() error { _, err := c.EditIssueComment(ctx, "talkable", "..", 1, "x"); return err },
		"edit: comment 0":    func() error { _, err := c.EditIssueComment(ctx, "talkable", "talkable", 0, "x"); return err },
		"edit: blank body":   func() error { _, err := c.EditIssueComment(ctx, "talkable", "talkable", 1, " "); return err },
	} {
		if call() == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("gh ran for a malformed request: %+v", f.Calls)
	}
}

// An edit of a comment someone deleted keeps GitHub's 404, so the caller
// can tell a gone comment from a failure; a dry run only plans both calls.
func TestEditIssueCommentReportsAGoneComment(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{
		Stdout: compact(t, `{"message":"Not Found","status":"404"}`),
		Stderr: []byte("gh: Not Found (HTTP 404)\n"), Code: 1}}}}
	_, err := (&Client{Run: f}).EditIssueComment(context.Background(), "talkable", "talkable", 987, "x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	dry := &execx.DryRun{Inner: &execx.Fake{}}
	c := &Client{Run: dry}
	_, _ = c.CreateIssueComment(context.Background(), "talkable", "talkable", 5, "x")
	_, _ = c.EditIssueComment(context.Background(), "talkable", "talkable", 987, "x")
	if len(dry.Planned) != 2 {
		t.Fatalf("dry run: planned %d, want 2", len(dry.Planned))
	}
}

// The comments of a PR's conversation are read over GraphQL from the newest
// back, a page of 100 at a time, until a page holds a comment the caller's
// stop accepts (the author's last word, past more than a page of others'),
// and come back oldest first, each with its REST id, link, body, time,
// author (a ghost's empty) and the author's association with the
// repository; the read mutates nothing.
func TestIssueCommentsReadBackToTheCallersStop(t *testing.T) {
	newest := `{"data":{"repository":{"pullRequest":{"comments":{"pageInfo":{"hasPreviousPage":true,"startCursor":"c1"},"nodes":[
	 {"databaseId":702,"url":"https://github.com/talkable/talkable/pull/5#issuecomment-702","body":"CI passed","createdAt":"2026-10-20T07:30:00Z","author":{"login":"bob-rev","__typename":"Bot"}},
	 {"databaseId":703,"url":"u703","body":"gone","createdAt":"2026-10-20T07:40:00Z","author":null}]}}}}}`
	older := `{"data":{"repository":{"pullRequest":{"comments":{"pageInfo":{"hasPreviousPage":true,"startCursor":"c2"},"nodes":[
	 {"databaseId":700,"url":"u700","body":"addressing it","createdAt":"2026-10-20T07:20:00Z","author":{"login":"zhuravel","__typename":"User"},"authorAssociation":"OWNER"},
	 {"databaseId":701,"url":"https://github.com/talkable/talkable/pull/5#issuecomment-701","body":"Please also log stderr.","createdAt":"2026-10-20T07:24:00Z","author":{"login":"rev-ann","__typename":"User"},"authorAssociation":"MEMBER"}]}}}}}`
	var cursors []any
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(_ execx.Cmd, req gqlReq) (execx.Result, error) {
		cursors = append(cursors, req.Variables["cursor"])
		if req.Variables["cursor"] == nil {
			return okResult(compact(t, newest))
		}
		return okResult(compact(t, older))
	})}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}
	got, complete, err := c.IssueComments(context.Background(), "talkable", "talkable", 5, func(pc PRComment) bool { return pc.AuthorLogin == "zhuravel" })
	if err != nil || !complete {
		t.Fatal(complete, err)
	}
	if !reflect.DeepEqual(cursors, []any{nil, "c1"}) {
		t.Fatalf("cursors = %v, want the newest page and the one before it", cursors)
	}
	at := func(m int) time.Time { return time.Date(2026, 10, 20, 7, m, 0, 0, time.UTC) }
	want := []PRComment{
		{ID: 700, URL: "u700", Body: "addressing it", CreatedAt: at(20), AuthorLogin: "zhuravel", AuthorType: "User", AuthorAssociation: "OWNER"},
		{ID: 701, URL: "https://github.com/talkable/talkable/pull/5#issuecomment-701", Body: "Please also log stderr.", CreatedAt: at(24), AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"},
		{ID: 702, URL: "https://github.com/talkable/talkable/pull/5#issuecomment-702", Body: "CI passed", CreatedAt: at(30), AuthorLogin: "bob-rev", AuthorType: "Bot"},
		{ID: 703, URL: "u703", Body: "gone", CreatedAt: at(40)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("comments = %+v\nwant %+v", got, want)
	}
	if call := f.Calls[0]; call.Mutates || call.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
		t.Errorf("call = %+v", call)
	}
	if q := oneLine(decodeReq(t, f.Calls[0]).Query); !strings.Contains(q, "comments(last: 100, before: $cursor) { pageInfo { hasPreviousPage startCursor } "+
		"nodes { databaseId url body createdAt author { login __typename } authorAssociation } }") {
		t.Errorf("query = %s", q)
	}

	// Without a stop it reads to the first comment, and at the page limit
	// it says the list is not complete.
	endless := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: compact(t, older)}}}}
	got, complete, err = (&Client{Run: endless}).IssueComments(context.Background(), "talkable", "talkable", 5, nil)
	if err != nil || complete || len(got) != 2*issueCommentsMaxPages || len(endless.Calls) != issueCommentsMaxPages {
		t.Errorf("at the page limit: %d comments in %d calls, complete %v, %v", len(got), len(endless.Calls), complete, err)
	}

	empty := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: compact(t,
		`{"data":{"repository":{"pullRequest":{"comments":{"pageInfo":{"hasPreviousPage":false,"startCursor":null},"nodes":[]}}}}}`)}}}}
	if got, complete, err := (&Client{Run: empty}).IssueComments(context.Background(), "talkable", "talkable", 5, nil); err != nil || !complete || got == nil || len(got) != 0 {
		t.Fatalf("no comments = %v, %v, %v (want an empty, non-nil, complete list)", got, complete, err)
	}
	missing := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "reviews_notfound.json"), Stderr: fixture(t, "reviews_notfound.stderr"), Code: 1},
	}}}
	if _, _, err := (&Client{Run: missing}).IssueComments(context.Background(), "talkable", "talkable", 99999999, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing PR = %v, want ErrNotFound", err)
	}
	if _, _, err := (&Client{Run: empty}).IssueComments(context.Background(), "talkable", "talkable", 0, nil); err == nil {
		t.Fatal("PR 0: want an error")
	}
}
