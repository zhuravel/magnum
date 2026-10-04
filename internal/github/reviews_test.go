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

func reviewsFake(t *testing.T) *execx.Fake {
	return &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "reviews.json")},
	}}}
}

func TestReviewsWithMarker(t *testing.T) {
	f := reviewsFake(t)
	c := &Client{Run: f}
	got, err := c.ReviewsWithMarker(context.Background(), "talkable", "talkable", 11973, "### 💡 Codex Review")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("reviews = %d, want 1", len(got))
	}
	r := got[0]
	if r.DatabaseID != 5379752632 || r.State != "COMMENTED" || r.AuthorLogin != "chatgpt-codex-connector" || r.AuthorType != "Bot" ||
		r.CommitOid != "61c18504b757fa03e6596add55a20c709f5ae47a" ||
		r.URL != "https://github.com/talkable/talkable/pull/11973#pullrequestreview-5379752632" ||
		!r.SubmittedAt.Equal(time.Date(2026, 10, 1, 13, 17, 3, 0, time.UTC)) ||
		!strings.Contains(r.Body, "Codex Review") {
		t.Errorf("review = %+v", r)
	}
	q := oneLine(decodeReq(t, f.Calls[0]).Query)
	if !strings.Contains(q, "reviews(last: 30)") || !strings.Contains(q, "databaseId state body url submittedAt commit { oid } author { login __typename }") {
		t.Errorf("query = %s", q)
	}
}

func TestReviewsWithMarkerEmptyReturnsAllInOrder(t *testing.T) {
	got, err := (&Client{Run: reviewsFake(t)}).ReviewsWithMarker(context.Background(), "talkable", "talkable", 11973, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("reviews = %d, want 10", len(got))
	}
	if got[0].DatabaseID != 5379658495 || got[9].DatabaseID != 5382071632 {
		t.Errorf("order = %d..%d", got[0].DatabaseID, got[9].DatabaseID)
	}
	var zh []int64
	for _, r := range got {
		if SameLogin(r.AuthorLogin, "zhuravel") {
			zh = append(zh, r.DatabaseID)
		}
	}
	if !reflect.DeepEqual(zh, []int64{5380286730, 5382071632}) {
		t.Errorf("zhuravel reviews = %v", zh)
	}
}

func TestReviewsWithMarkerNoMatch(t *testing.T) {
	got, err := (&Client{Run: reviewsFake(t)}).ReviewsWithMarker(context.Background(), "talkable", "talkable", 11973, "<!-- magnum:run=r-1 -->")
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestReviewsWithMarkerPRNotFound(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "reviews_notfound.json"), Stderr: fixture(t, "reviews_notfound.stderr"), Code: 1},
	}}}
	_, err := (&Client{Run: f}).ReviewsWithMarker(context.Background(), "talkable", "talkable", 99999999, "x")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestReviewREST(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "repos/talkable/talkable/pulls/11973/reviews/5379752632"},
		Result: execx.Result{Stdout: fixture(t, "review_rest_bot.json")},
	}}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}
	r, err := c.ReviewREST(context.Background(), "talkable", "talkable", 11973, 5379752632)
	if err != nil {
		t.Fatal(err)
	}
	if r.UserLogin != "chatgpt-codex-connector[bot]" || r.UserType != "Bot" {
		t.Errorf("user = %q/%q", r.UserLogin, r.UserType)
	}
	if !SameLogin(r.UserLogin, "chatgpt-codex-connector") || !IsBot(r.UserType, r.UserLogin) {
		t.Error("REST bot login must normalize to the GraphQL login")
	}
	if r.ID != 5379752632 || r.State != "COMMENTED" || r.CommitID != "61c18504b757fa03e6596add55a20c709f5ae47a" ||
		r.HTMLURL != "https://github.com/talkable/talkable/pull/11973#pullrequestreview-5379752632" ||
		!r.SubmittedAt.Equal(time.Date(2026, 10, 1, 13, 17, 3, 0, time.UTC)) || r.Body == "" {
		t.Errorf("review = %+v", r)
	}
	cmd := f.Calls[0]
	if strings.Join(cmd.Args, " ") != "api repos/talkable/talkable/pulls/11973/reviews/5379752632 --hostname github.com" || cmd.Mutates {
		t.Errorf("cmd = %s mutates=%v", cmd.String(), cmd.Mutates)
	}
	if cmd.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
		t.Errorf("env = %v", cmd.Env)
	}
}

func TestReviewREST404(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api"},
		Result: execx.Result{Stdout: fixture(t, "review_rest_404.json"), Stderr: fixture(t, "review_rest_404.stderr"), Code: 1},
	}}}
	_, err := (&Client{Run: f}).ReviewREST(context.Background(), "talkable", "talkable", 11973, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Message != "Not Found" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestDismissReview(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "-X", "PUT"},
		Result: execx.Result{Stdout: []byte(`{"id":77,"state":"DISMISSED"}`)},
	}}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}
	if err := c.DismissReview(context.Background(), "talkable", "talkable", 5, 77, "Superseded by a newer review"); err != nil {
		t.Fatal(err)
	}
	cmd := f.Calls[0]
	want := []string{"api", "-X", "PUT", "repos/talkable/talkable/pulls/5/reviews/77/dismissals", "--hostname", "github.com",
		"-f", "message=Superseded by a newer review", "-f", "event=DISMISS"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %q", cmd.Args)
	}
	if !cmd.Mutates {
		t.Error("dismissal must be marked Mutates")
	}
	if cmd.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
		t.Errorf("env = %v", cmd.Env)
	}
}

func TestDismissReviewForbidden(t *testing.T) {
	// Synthetic: what gh prints when the App lacks pull_requests: write.
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api"},
		Result: execx.Result{
			Stdout: compact(t, `{"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com/rest/pulls/reviews#dismiss-a-review-for-a-pull-request","status":"403"}`),
			Stderr: []byte("gh: Resource not accessible by integration (HTTP 403)\n"),
			Code:   1,
		},
	}}}
	err := (&Client{Run: f}).DismissReview(context.Background(), "talkable", "talkable", 5, 77, "m")
	if !errors.Is(err, ErrForbidden) || errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestDismissReviewDryRun(t *testing.T) {
	inner := &execx.Fake{} // any real call would fail: no rules
	dry := &execx.DryRun{Inner: inner}
	if err := (&Client{Run: dry}).DismissReview(context.Background(), "talkable", "talkable", 5, 77, "m"); err != nil {
		t.Fatal(err)
	}
	if len(dry.Planned) != 1 || len(inner.Calls) != 0 {
		t.Errorf("planned = %d, executed = %d", len(dry.Planned), len(inner.Calls))
	}
}

func TestUpdateReviewBody(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "-X", "PUT"},
		Result: execx.Result{Stdout: []byte(`{"id":77,"body":"x"}`)},
	}}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}
	body := "**Verdict**\n<!-- magnum:run=r-1 head=abc1234 -->\n\n_Reviewed abc1234; 2 commits arrived during the review, re-review follows._"
	if err := c.UpdateReviewBody(context.Background(), "talkable", "talkable", 5, 77, body); err != nil {
		t.Fatal(err)
	}
	cmd := f.Calls[0]
	want := []string{"api", "-X", "PUT", "repos/talkable/talkable/pulls/5/reviews/77", "--hostname", "github.com", "-f", "body=" + body}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %q", cmd.Args)
	}
	if !cmd.Mutates {
		t.Error("an update must be marked Mutates")
	}
	for _, bad := range []struct {
		number int
		id     int64
		body   string
	}{{0, 77, "x"}, {5, 0, "x"}, {5, 77, " "}} {
		if err := c.UpdateReviewBody(context.Background(), "talkable", "talkable", bad.number, bad.id, bad.body); err == nil {
			t.Errorf("UpdateReviewBody(%d, %d, %q) = nil, want an error", bad.number, bad.id, bad.body)
		}
	}
	if len(f.Calls) != 1 {
		t.Errorf("invalid input reached gh: %d calls", len(f.Calls))
	}
}

func TestUpdateReviewBodyForbidden(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api"},
		Result: execx.Result{
			Stdout: compact(t, `{"message":"Resource not accessible by integration","status":"403"}`),
			Stderr: []byte("gh: Resource not accessible by integration (HTTP 403)\n"),
			Code:   1,
		},
	}}}
	err := (&Client{Run: f}).UpdateReviewBody(context.Background(), "talkable", "talkable", 5, 77, "body")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}
