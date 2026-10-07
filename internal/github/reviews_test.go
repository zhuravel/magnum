package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// UpdateReviewBody sends the body, which quotes the PR, as JSON on gh's
// stdin: argv (which a failing call logs) holds none of it.
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
	want := []string{"api", "-X", "PUT", "repos/talkable/talkable/pulls/5/reviews/77", "--hostname", "github.com", "--input", "-"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %q", cmd.Args)
	}
	var sent map[string]any
	if err := json.Unmarshal(cmd.Stdin, &sent); err != nil || len(sent) != 1 || sent["body"] != body {
		t.Errorf("stdin = %s, %v", cmd.Stdin, err)
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

// Synthetic pages of the paged reviews connection: the first has a review by a
// ghost (null author), the second one whose commit is gone (null commit).
const reviewsPage1 = `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4990,"used":10,"resetAt":"2026-10-04T12:00:00Z"},
"repository":{"pullRequest":{"reviews":{"pageInfo":{"hasNextPage":true,"endCursor":"Y3Vyc29yOjE="},"nodes":[
 {"databaseId":1001,"state":"COMMENTED","body":"first","url":"https://github.com/talkable/talkable/pull/5#pullrequestreview-1001","submittedAt":"2026-10-03T09:00:00Z","commit":{"oid":"1111111111111111111111111111111111111111"},"author":{"login":"rev-ann","__typename":"User"}},
 {"databaseId":1002,"state":"DISMISSED","body":"from a deleted account","url":"u1002","submittedAt":"2026-10-03T10:00:00Z","commit":{"oid":"1111111111111111111111111111111111111111"},"author":null}]}}}}}`

const reviewsPage2 = `{"data":{"repository":{"pullRequest":{"reviews":{"pageInfo":{"hasNextPage":false,"endCursor":"Y3Vyc29yOjI="},"nodes":[
 {"databaseId":1003,"state":"APPROVED","body":"","url":"u1003","submittedAt":"2026-10-03T11:00:00Z","commit":null,"author":{"login":"talkable","__typename":"Bot"}}]}}}}}`

func TestReviewsPagesInOrder(t *testing.T) {
	var cursors []any
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		cursors = append(cursors, req.Variables["cursor"])
		if req.Variables["cursor"] == nil {
			return okResult(compact(t, reviewsPage1))
		}
		return okResult(compact(t, reviewsPage2))
	})}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}
	got, err := c.Reviews(context.Background(), "talkable", "talkable", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cursors, []any{nil, "Y3Vyc29yOjE="}) {
		t.Fatalf("cursors = %v", cursors)
	}
	at := func(h int) time.Time { return time.Date(2026, 10, 3, h, 0, 0, 0, time.UTC) }
	want := []Review{
		{DatabaseID: 1001, State: "COMMENTED", Body: "first", URL: "https://github.com/talkable/talkable/pull/5#pullrequestreview-1001",
			SubmittedAt: at(9), CommitOid: "1111111111111111111111111111111111111111", AuthorLogin: "rev-ann", AuthorType: "User"},
		// A ghost: no author.
		{DatabaseID: 1002, State: "DISMISSED", Body: "from a deleted account", URL: "u1002", SubmittedAt: at(10), CommitOid: "1111111111111111111111111111111111111111"},
		// No commit: CommitOid stays empty.
		{DatabaseID: 1003, State: "APPROVED", URL: "u1003", SubmittedAt: at(11), AuthorLogin: "talkable", AuthorType: "Bot"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reviews = %+v\nwant %+v", got, want)
	}
	call := f.Calls[0]
	if call.Mutates || call.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
		t.Errorf("call = %+v", call)
	}
	q := oneLine(decodeReq(t, call).Query)
	for _, want := range []string{"reviews(first: 100, after: $cursor)", "pageInfo { hasNextPage endCursor }",
		"nodes { databaseId state body url submittedAt commit { oid } author { login __typename } }"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q: %s", want, q)
		}
	}
	if strings.Contains(q, "last:") {
		t.Errorf("Reviews must page from the start, not read the last reviews: %s", q)
	}
}

func TestReviewsStopsAtTheMaxPages(t *testing.T) {
	// A pull request with more reviews than reviewsMaxPages pages hold: the
	// later ones are left out, and the client stops asking.
	calls := 0
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		calls++
		return okResult(compact(t, fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"reviews":{"pageInfo":{"hasNextPage":true,"endCursor":"c%d"},"nodes":[
		 {"databaseId":%d,"state":"COMMENTED","body":"","url":"u","submittedAt":"2026-10-03T09:00:00Z","commit":null,"author":null}]}}}}}`, calls, calls)))
	})}}
	got, err := (&Client{Run: f}).Reviews(context.Background(), "talkable", "talkable", 5)
	if err != nil {
		t.Fatal(err)
	}
	if calls != reviewsMaxPages || len(got) != reviewsMaxPages {
		t.Errorf("calls = %d, reviews = %d, want %d of each", calls, len(got), reviewsMaxPages)
	}
	if got[0].DatabaseID != 1 || got[len(got)-1].DatabaseID != int64(reviewsMaxPages) {
		t.Errorf("order = %d..%d", got[0].DatabaseID, got[len(got)-1].DatabaseID)
	}
}

// AllReviews says whether it read every review: a list cut at
// reviewsMaxPages pages is incomplete, one whose last page says so is
// complete, however many pages it took.
func TestAllReviewsSaysWhetherTheListIsComplete(t *testing.T) {
	pages := func(last int) *execx.Fake {
		calls := 0
		return &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
			calls++
			return okResult(compact(t, fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"reviews":{"pageInfo":{"hasNextPage":%v,"endCursor":"c%d"},"nodes":[
			 {"databaseId":%d,"state":"COMMENTED","body":"","url":"u","submittedAt":"2026-10-03T09:00:00Z","commit":null,"author":null}]}}}}}`, calls < last, calls, calls)))
		})}}
	}
	for _, tc := range []struct {
		pages    int
		complete bool
	}{{1, true}, {reviewsMaxPages, true}, {reviewsMaxPages + 1, false}} {
		f := pages(tc.pages)
		got, complete, err := (&Client{Run: f}).AllReviews(context.Background(), "talkable", "talkable", 5)
		if err != nil {
			t.Fatal(err)
		}
		if complete != tc.complete || len(got) != min(tc.pages, reviewsMaxPages) || len(f.Calls) != min(tc.pages, reviewsMaxPages) {
			t.Errorf("%d pages: %d reviews in %d calls, complete %v, want %v", tc.pages, len(got), len(f.Calls), complete, tc.complete)
		}
	}
}

func TestReviewsEmptyAndErrors(t *testing.T) {
	empty := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: compact(t,
		`{"data":{"repository":{"pullRequest":{"reviews":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}`)}}}}
	got, err := (&Client{Run: empty}).Reviews(context.Background(), "talkable", "talkable", 5)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no reviews = %v, %v (want an empty, non-nil list)", got, err)
	}

	missing := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "reviews_notfound.json"), Stderr: fixture(t, "reviews_notfound.stderr"), Code: 1},
	}}}
	if _, err := (&Client{Run: missing}).Reviews(context.Background(), "talkable", "talkable", 99999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing PR = %v, want ErrNotFound", err)
	}
	// A null pullRequest without a GraphQL error is a missing one too.
	null := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: []byte(`{"data":{"repository":{"pullRequest":null}}}`)}}}}
	if _, err := (&Client{Run: null}).Reviews(context.Background(), "talkable", "talkable", 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("null pullRequest = %v, want ErrNotFound", err)
	}
	for _, tc := range []struct {
		owner, repo string
		number      int
	}{{"talkable", "talkable", 0}, {"", "talkable", 5}, {"talkable", "tal/kable", 5}} {
		f := &execx.Fake{}
		if _, err := (&Client{Run: f}).Reviews(context.Background(), tc.owner, tc.repo, tc.number); err == nil || len(f.Calls) != 0 {
			t.Errorf("%+v: err = %v, calls = %d, want an error before any call", tc, err, len(f.Calls))
		}
	}
}
