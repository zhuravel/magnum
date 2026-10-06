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

// A reply goes out as JSON on gh's stdin: no reply text reaches argv, and
// the call is marked Mutates so a dry run only plans it.
func TestReplyToReviewCommentSendsJSONOnStdin(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "-X", "POST"},
		Result: execx.Result{Stdout: []byte(`{"id":4321,"html_url":"https://github.com/talkable/talkable/pull/5#discussion_r4321","created_at":"2026-10-06T12:30:00Z","in_reply_to_id":1234}`)}}}}
	body := "Fixed in `abc1234`; $(rm -rf) stays text\n\n<!-- magnum:reply run=r-1 kind=ack -->"
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}
	got, err := c.ReplyToReviewComment(context.Background(), "talkable", "talkable", 5, 1234, body)
	if err != nil {
		t.Fatal(err)
	}
	want := ReviewCommentReply{ID: 4321, URL: "https://github.com/talkable/talkable/pull/5#discussion_r4321",
		CreatedAt: time.Date(2026, 10, 6, 12, 30, 0, 0, time.UTC)}
	if got != want {
		t.Errorf("reply = %+v, want %+v", got, want)
	}
	cmd := f.Calls[0]
	if wantArgs := []string{"api", "-X", "POST", "repos/talkable/talkable/pulls/5/comments/1234/replies", "--hostname", "github.com", "--input", "-"}; !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Errorf("args = %q", cmd.Args)
	}
	if !cmd.Mutates {
		t.Error("a reply must be marked Mutates")
	}
	if cmd.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
		t.Errorf("env = %v", cmd.Env)
	}
	for _, a := range cmd.Args {
		if strings.Contains(a, "rm -rf") {
			t.Errorf("the reply text reached argv: %q", cmd.Args)
		}
	}
	var sent map[string]any
	if err := json.Unmarshal(cmd.Stdin, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent["body"] != body {
		t.Errorf("stdin = %s", cmd.Stdin)
	}
}

// A malformed request never reaches gh.
func TestReplyToReviewCommentRefusesMalformedRequests(t *testing.T) {
	f := &execx.Fake{}
	c := &Client{Run: f}
	for _, bad := range []struct {
		name, owner, repo string
		number            int
		id                int64
		body              string
	}{
		{"no owner", "", "talkable", 5, 1, "x"},
		{"bad repo", "talkable", "../x", 5, 1, "x"},
		{"pr 0", "talkable", "talkable", 0, 1, "x"},
		{"comment 0", "talkable", "talkable", 5, 0, "x"},
		{"negative comment", "talkable", "talkable", 5, -3, "x"},
		{"blank body", "talkable", "talkable", 5, 1, " \n\t"},
	} {
		if _, err := c.ReplyToReviewComment(context.Background(), bad.owner, bad.repo, bad.number, bad.id, bad.body); err == nil {
			t.Errorf("%s: want an error", bad.name)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("gh ran for a malformed request: %+v", f.Calls)
	}
}

// A refusal keeps GitHub's status, so a caller can tell a missing comment
// from a failure; a dry run only plans the reply.
func TestReplyToReviewCommentReportsGitHubRefusals(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{
		Stdout: compact(t, `{"message":"Not Found","status":"404"}`),
		Stderr: []byte("gh: Not Found (HTTP 404)\n"), Code: 1}}}}
	_, err := (&Client{Run: f}).ReplyToReviewComment(context.Background(), "talkable", "talkable", 5, 1234, "x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	dry := &execx.DryRun{Inner: &execx.Fake{}}
	_, _ = (&Client{Run: dry}).ReplyToReviewComment(context.Background(), "talkable", "talkable", 5, 1234, "x")
	if len(dry.Planned) != 1 {
		t.Fatalf("dry run: planned %d", len(dry.Planned))
	}
}
