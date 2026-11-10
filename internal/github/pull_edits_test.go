package github

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// A review request goes out as JSON on gh's stdin, marked Mutates so a dry
// run only plans it, with the logins as GitHub takes them; a login that is
// no user's (an App's, a team's, one with an "@") never reaches gh.
func TestRequestReviewersSendsTheLoginsOnStdin(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "-X", "POST"},
		Result: execx.Result{Stdout: []byte(`{"number":5,"requested_reviewers":[{"login":"alice"},{"login":"rev-ann"}]}`)}}}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/fixer-app"}}
	if err := c.RequestReviewers(context.Background(), "talkable", "talkable", 5, []string{"alice", "rev-ann"}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %+v", f.Calls)
	}
	cmd := f.Calls[0]
	if want := []string{"api", "-X", "POST", "repos/talkable/talkable/pulls/5/requested_reviewers", "--hostname", "github.com", "--input", "-"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	if !cmd.Mutates || cmd.Env["GH_CONFIG_DIR"] != "/state/gh/fixer-app" {
		t.Errorf("call = %+v: want a write as the client's identity", cmd)
	}
	var sent struct {
		Reviewers []string `json:"reviewers"`
	}
	if err := json.Unmarshal(cmd.Stdin, &sent); err != nil || !slices.Equal(sent.Reviewers, []string{"alice", "rev-ann"}) {
		t.Errorf("stdin = %s (%v)", cmd.Stdin, err)
	}
	for _, bad := range [][]string{nil, {"fixer-app[bot]"}, {"@alice"}, {"talkable/reviewers"}, {""}} {
		if err := c.RequestReviewers(context.Background(), "talkable", "talkable", 5, bad); err == nil {
			t.Errorf("RequestReviewers(%q) = nil, want an error", bad)
		}
	}
	if err := c.RequestReviewers(context.Background(), "talkable", "..", 5, []string{"alice"}); err == nil {
		t.Error("a bad repository: want an error")
	}
	if len(f.Calls) != 1 {
		t.Errorf("gh ran for a malformed request: %+v", f.Calls[1:])
	}
}

// A new title goes out as JSON on gh's stdin, never in argv (it is the
// caller's text), marked Mutates, and the answer reads back the PR as
// GitHub has it now.
func TestUpdatePullTitleSendsTheTitleOnStdinNotArgv(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "-X", "PATCH"},
		Result: execx.Result{Stdout: []byte(`{"number":5,"title":"Fix the total; $(rm -rf) stays text","state":"open",` +
			`"html_url":"https://github.com/talkable/talkable/pull/5","user":{"login":"alice","type":"User"}}`)}}}}
	c := &Client{Run: f}
	title := "Fix the total; $(rm -rf) stays text"
	got, err := c.UpdatePullTitle(context.Background(), "talkable", "talkable", 5, title)
	if err != nil || got.Title != title || got.URL != "https://github.com/talkable/talkable/pull/5" {
		t.Fatalf("UpdatePullTitle = %+v, %v", got, err)
	}
	cmd := f.Calls[0]
	if want := []string{"api", "-X", "PATCH", "repos/talkable/talkable/pulls/5", "--hostname", "github.com", "--input", "-"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	if !cmd.Mutates || slices.ContainsFunc(cmd.Args, func(a string) bool { return strings.Contains(a, "rm -rf") }) {
		t.Errorf("call = %+v: want a write with the title off argv", cmd)
	}
	var sent map[string]any
	if err := json.Unmarshal(cmd.Stdin, &sent); err != nil || len(sent) != 1 || sent["title"] != title {
		t.Errorf("stdin = %s (%v)", cmd.Stdin, err)
	}
	for _, bad := range []string{"", " \t", "two\nlines"} {
		if _, err := c.UpdatePullTitle(context.Background(), "talkable", "talkable", 5, bad); err == nil {
			t.Errorf("UpdatePullTitle(%q) = nil error", bad)
		}
	}
	if len(f.Calls) != 1 {
		t.Errorf("gh ran for a malformed title: %+v", f.Calls[1:])
	}
}

// Pull reads what a comment checks before it writes: the title, the state,
// the author in Account form and the users asked for a review now.
func TestPullReadsTitleAuthorAndRequestedReviewers(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "repos/talkable/talkable/pulls/5"},
		Result: execx.Result{Stdout: []byte(`{"number":5,"title":"Fix the total","state":"open","html_url":"https://github.com/talkable/talkable/pull/5",` +
			`"user":{"login":"alice","type":"User"},"requested_reviewers":[{"login":"rev-ann","type":"User"},{"login":"other-app[bot]","type":"Bot"}],` +
			`"requested_teams":[{"slug":"reviewers"}]}`)}}}}
	got, err := (&Client{Run: f}).Pull(context.Background(), "talkable", "talkable", 5)
	want := Pull{Number: 5, Title: "Fix the total", State: "open", URL: "https://github.com/talkable/talkable/pull/5",
		Author: "alice", RequestedReviewers: []string{"rev-ann", "other-app[bot]"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Pull = %+v, %v; want %+v", got, err, want)
	}
	if f.Calls[0].Mutates {
		t.Error("a read is marked Mutates")
	}
	if _, err := (&Client{Run: f}).Pull(context.Background(), "talkable", "talkable", 0); err == nil {
		t.Error("PR 0: want an error")
	}
}

// A reply and a conversation comment read back who wrote them, in Account
// form: magnum comment checks that GitHub shows the App it posted as.
func TestRepliesAndCommentsReadTheirAuthor(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"gh", "api", "-X", "POST", "repos/talkable/talkable/pulls/5/comments/11/replies"}, Result: execx.Result{
			Stdout: []byte(`{"id":4321,"html_url":"https://github.com/talkable/talkable/pull/5#discussion_r4321","user":{"login":"fixer-app[bot]","type":"Bot"}}`)}},
		{Prefix: []string{"gh", "api", "-X", "POST", "repos/talkable/talkable/issues/5/comments"}, Result: execx.Result{
			Stdout: []byte(`{"id":7001,"html_url":"https://github.com/talkable/talkable/pull/5#issuecomment-7001","user":{"login":"alice","type":"User"}}`)}},
	}}
	c := &Client{Run: f}
	reply, err := c.ReplyToReviewComment(context.Background(), "talkable", "talkable", 5, 11, "Thanks.")
	if err != nil || reply.AuthorLogin != "fixer-app[bot]" || reply.AuthorType != "Bot" || reply.Author() != "fixer-app[bot]" {
		t.Errorf("reply = %+v, %v", reply, err)
	}
	comment, err := c.CreateIssueComment(context.Background(), "talkable", "talkable", 5, "Thanks.")
	if err != nil || comment.AuthorLogin != "alice" || comment.AuthorType != "User" || comment.Author() != "alice" {
		t.Errorf("comment = %+v, %v", comment, err)
	}
}
