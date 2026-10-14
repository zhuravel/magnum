package github

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

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
