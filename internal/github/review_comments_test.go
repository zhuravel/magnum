package github

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

func TestReviewComments(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "repos/talkable/talkable/pulls/5/reviews/77/comments?per_page=100"},
		Result: execx.Result{Stdout: []byte(`[{"id":1,"path":"app/a.rb","line":12,"body":"**[P2] x**","html_url":"https://github.com/talkable/talkable/pull/5#discussion_r1"},` +
			`{"id":2,"path":"app/b.rb","line":null,"body":"outdated"}]`)},
	}}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}
	got, err := c.ReviewComments(context.Background(), "talkable", "talkable", 5, 77)
	if err != nil {
		t.Fatal(err)
	}
	want := []ReviewComment{
		{ID: 1, Path: "app/a.rb", Line: 12, Body: "**[P2] x**", HTMLURL: "https://github.com/talkable/talkable/pull/5#discussion_r1"},
		{ID: 2, Path: "app/b.rb", Body: "outdated"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("comments = %+v", got)
	}
	if cmd := f.Calls[0]; cmd.Mutates || cmd.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
		t.Errorf("call = %+v", cmd)
	}
	if _, err := c.ReviewComments(context.Background(), "talkable", "talkable", 5, 0); err == nil {
		t.Fatal("review 0: want error")
	}
}

func TestDeletePendingReview(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "-X", "DELETE"},
		Result: execx.Result{Stdout: []byte(`{"id":77,"state":"PENDING"}`)},
	}}}
	c := &Client{Run: f}
	if err := c.DeletePendingReview(context.Background(), "talkable", "talkable", 5, 77); err != nil {
		t.Fatal(err)
	}
	cmd := f.Calls[0]
	if want := []string{"api", "-X", "DELETE", "repos/talkable/talkable/pulls/5/reviews/77", "--hostname", "github.com"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %q", cmd.Args)
	}
	if !cmd.Mutates {
		t.Error("a delete must be marked Mutates")
	}

	// Synthetic: GitHub refuses a submitted review.
	refused := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api"},
		Result: execx.Result{
			Stdout: compact(t, `{"message":"Unprocessable Entity","errors":["Can not delete a non-pending pull request review"],"status":"422"}`),
			Stderr: []byte("gh: Unprocessable Entity (HTTP 422)\n"),
			Code:   1,
		},
	}}}
	err := (&Client{Run: refused}).DeletePendingReview(context.Background(), "talkable", "talkable", 5, 77)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Fatalf("err = %v, want a 422 APIError", err)
	}
	dry := &execx.DryRun{Inner: &execx.Fake{}}
	if err := (&Client{Run: dry}).DeletePendingReview(context.Background(), "talkable", "talkable", 5, 77); err != nil || len(dry.Planned) != 1 {
		t.Fatalf("dry run: %v, planned %d", err, len(dry.Planned))
	}
}
