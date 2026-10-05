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

func TestGraphQLCommandShape(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "reviews.json")},
	}}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/zhuravel"}, Dir: "/tmp/x"}
	if _, err := c.ReviewsWithMarker(context.Background(), "talkable", "talkable", 11973, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d", len(f.Calls))
	}
	cmd := f.Calls[0]
	if cmd.Name != "gh" || strings.Join(cmd.Args, " ") != "api graphql --hostname github.com --input -" {
		t.Errorf("argv = %s", cmd.String())
	}
	if cmd.Mutates {
		t.Error("reads must not be marked Mutates")
	}
	if cmd.Dir != "/tmp/x" {
		t.Errorf("dir = %q", cmd.Dir)
	}
	if cmd.Env["GH_CONFIG_DIR"] != "/state/gh/zhuravel" || cmd.Env["GH_PROMPT_DISABLED"] != "1" {
		t.Errorf("env = %v", cmd.Env)
	}
	if cmd.Label == "" {
		t.Error("label must be set for the transcript")
	}
	req := decodeReq(t, cmd)
	if req.Variables["owner"] != "talkable" || req.Variables["name"] != "talkable" || req.Variables["number"] != float64(11973) {
		t.Errorf("variables = %v", req.Variables)
	}
	if !strings.Contains(req.Query, "rateLimit { limit cost remaining used resetAt }") {
		t.Errorf("query lacks rateLimit: %s", req.Query)
	}
}

func TestClientEnvCanOverrideDefaults(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: fixture(t, "reviews.json")}}}}
	c := &Client{Run: f, Env: map[string]string{"GH_PROMPT_DISABLED": ""}}
	if _, err := c.ReviewsWithMarker(context.Background(), "talkable", "talkable", 11973, ""); err != nil {
		t.Fatal(err)
	}
	if v, ok := f.Calls[0].Env["GH_PROMPT_DISABLED"]; !ok || v != "" {
		t.Errorf("client env must win: %v", f.Calls[0].Env)
	}
}

func TestLastRateLimit(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: fixture(t, "reviews.json")}}}}
	c := &Client{Run: f}
	if got := c.LastRateLimit(); got != (RateLimit{}) {
		t.Fatalf("fresh client rate = %+v", got)
	}
	if _, err := c.ReviewsWithMarker(context.Background(), "talkable", "talkable", 11973, ""); err != nil {
		t.Fatal(err)
	}
	want := RateLimit{Limit: 5000, Cost: 1, Remaining: 4963, Used: 37, ResetAt: time.Date(2026, 10, 2, 21, 50, 10, 0, time.UTC)}
	if got := c.LastRateLimit(); got != want {
		t.Errorf("LastRateLimit = %+v, want %+v", got, want)
	}
}

func TestGraphQLValidationErrorFails(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "gql_syntax.json"), Stderr: fixture(t, "gql_syntax.stderr"), Code: 1},
	}}}
	c := &Client{Run: f}
	_, err := c.ReviewsWithMarker(context.Background(), "talkable", "talkable", 1, "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if len(apiErr.Errors) != 1 || !strings.Contains(apiErr.Errors[0].Message, "nosuchfield") {
		t.Errorf("errors = %+v", apiErr.Errors)
	}
	if got := strings.Join(apiErr.Errors[0].Path, "."); got != "query.rateLimit.nosuchfield" {
		t.Errorf("path = %q", got)
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrRateLimited) {
		t.Error("validation error misclassified")
	}
}

func TestUnauthorized(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "gql_401.json"), Stderr: fixture(t, "gql_401.stderr"), Code: 1},
	}}}
	c := &Client{Run: f}
	_, err := c.ReviewsWithMarker(context.Background(), "talkable", "talkable", 1, "")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || apiErr.Message != "Bad credentials" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestGraphQLRateLimited(t *testing.T) {
	// Synthetic: GitHub's documented primary rate-limit GraphQL error.
	body := compact(t, `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for user ID 1."}]}`)
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: body, Stderr: []byte("gh: API rate limit exceeded for user ID 1.\n"), Code: 1},
	}}}
	_, err := (&Client{Run: f}).ReviewsWithMarker(context.Background(), "talkable", "talkable", 1, "")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestRESTRateLimitedIsNotForbidden(t *testing.T) {
	// Synthetic: REST primary/secondary limits come back as 403 with this message.
	body := compact(t, `{"message":"API rate limit exceeded for user ID 1.","documentation_url":"https://docs.github.com/rest/overview/resources-in-the-rest-api#rate-limiting","status":"403"}`)
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api"},
		Result: execx.Result{Stdout: body, Stderr: []byte("gh: API rate limit exceeded for user ID 1. (HTTP 403)\n"), Code: 1},
	}}}
	_, err := (&Client{Run: f}).ReviewREST(context.Background(), "talkable", "talkable", 1, 2)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if errors.Is(err, ErrForbidden) {
		t.Error("rate limit must not read as forbidden")
	}
}

func TestRunnerErrorIsWrapped(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Err: context.DeadlineExceeded}}}
	_, err := (&Client{Run: f}).ReviewsWithMarker(context.Background(), "talkable", "talkable", 1, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want wrapped DeadlineExceeded", err)
	}
}

func TestExitWithoutJSONKeepsStderr(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh"},
		Result: execx.Result{Stderr: []byte("error connecting to api.github.com\n"), Code: 1},
	}}}
	_, err := (&Client{Run: f}).ReviewsWithMarker(context.Background(), "talkable", "talkable", 1, "")
	if err == nil || !strings.Contains(err.Error(), "error connecting to api.github.com") {
		t.Fatalf("err = %v", err)
	}
	var exitErr *execx.ExitError
	if !errors.As(err, &exitErr) {
		t.Error("transport failures should wrap the execx.ExitError")
	}
}

func TestErrorsAreRedacted(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh"},
		Result: execx.Result{
			Stdout: compact(t, `{"message":"bad token ghs_abcdefghijklmnopqrstuvwxyz0123456789","status":"401"}`),
			Stderr: []byte("gh: bad token ghs_abcdefghijklmnopqrstuvwxyz0123456789 (HTTP 401)\n"),
			Code:   1,
		},
	}}}
	_, err := (&Client{Run: f}).ReviewREST(context.Background(), "talkable", "talkable", 1, 2)
	if err == nil || strings.Contains(err.Error(), "ghs_abc") {
		t.Fatalf("token leaked or no error: %v", err)
	}
}

func TestNilRunner(t *testing.T) {
	if _, err := (&Client{}).ReviewsWithMarker(context.Background(), "talkable", "talkable", 1, ""); err == nil {
		t.Fatal("want error for nil Run")
	}
}

func TestRejectsBadRepoNames(t *testing.T) {
	f := &execx.Fake{}
	c := &Client{Run: f}
	ctx := context.Background()
	if _, err := c.ReviewREST(ctx, "talkable", "../../user", 1, 2); err == nil {
		t.Error("ReviewREST accepted a path-traversal repo")
	}
	if err := c.DismissReview(ctx, "tal kable", "talkable", 1, 2, "m"); err == nil {
		t.Error("DismissReview accepted a bad owner")
	}
	if _, _, err := c.Radar(ctx, ""); err == nil {
		t.Error("Radar accepted an empty org")
	}
	if _, _, err := c.Details(ctx, "talkable", "", []int{1}); err == nil {
		t.Error("Details accepted an empty repo")
	}
	if len(f.Calls) != 0 {
		t.Errorf("gh ran for invalid input: %d calls", len(f.Calls))
	}
}

// hostPinned reports whether argv carries --hostname github.com.
func hostPinned(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--hostname" && args[i+1] == "github.com" {
			return true
		}
	}
	return false
}

func TestEveryCommandPinsHostname(t *testing.T) {
	// An inherited GH_HOST must not redirect any call: each command names
	// github.com itself. The answers do not matter, only the recorded argv.
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: []byte("{}")}}}}
	c := &Client{Run: f, Env: map[string]string{"GH_HOST": "enterprise.example"}}
	ctx := context.Background()
	_, _, _ = c.Radar(ctx, "talkable")
	_, _, _ = c.Details(ctx, "talkable", "talkable", []int{1})
	_, _, _ = c.ConfirmStates(ctx, "talkable", "talkable", []int{1})
	_, _ = c.ReviewsWithMarker(ctx, "talkable", "talkable", 1, "")
	_, _ = c.Reviews(ctx, "talkable", "talkable", 1)
	_, _ = c.ReviewThreads(ctx, "talkable", "talkable", 1)
	_, _ = c.ReviewREST(ctx, "talkable", "talkable", 1, 2)
	_, _ = c.Compare(ctx, "talkable", "talkable", cmpBase, cmpHead)
	_, _ = c.FileAt(ctx, "talkable", "talkable", "app/a.rb", cmpHead)
	_ = c.DismissReview(ctx, "talkable", "talkable", 1, 2, "m")
	if len(f.Calls) != 10 {
		t.Fatalf("calls = %d, want 10", len(f.Calls))
	}
	for _, cmd := range f.Calls {
		if !hostPinned(cmd.Args) {
			t.Errorf("%s does not pin --hostname github.com", cmd.String())
		}
	}
}

// reauthFake answers 401 for the first failures calls, then the reviews fixture
// (graphql) or a dismissal (REST).
func reauthFake(t *testing.T, failures int) *execx.Fake {
	n := 0
	return &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api"},
		Fn: func(c execx.Cmd) (execx.Result, error) {
			n++
			if n <= failures {
				return failResult(c, fixture(t, "gql_401.json"), fixture(t, "gql_401.stderr"))
			}
			if c.Mutates {
				return okResult([]byte(`{"id":77,"state":"DISMISSED"}`))
			}
			return okResult(fixture(t, "reviews.json"))
		},
	}}}
}

func TestReauthRetriesGraphQLOnce(t *testing.T) {
	f := reauthFake(t, 1)
	reauths := 0
	c := &Client{Run: f, Reauth: func(context.Context) error { reauths++; return nil }}
	got, err := c.ReviewsWithMarker(context.Background(), "talkable", "talkable", 11973, "")
	if err != nil || len(got) != 10 {
		t.Fatalf("got %d reviews, err %v", len(got), err)
	}
	if reauths != 1 || len(f.Calls) != 2 {
		t.Errorf("reauths = %d, gh calls = %d, want 1 and 2", reauths, len(f.Calls))
	}
	if !reflect.DeepEqual(f.Calls[0].Args, f.Calls[1].Args) || string(f.Calls[0].Stdin) != string(f.Calls[1].Stdin) {
		t.Error("the retry must repeat the same request")
	}
}

func TestReauthRetriesMutationOnce(t *testing.T) {
	f := reauthFake(t, 1)
	reauths := 0
	c := &Client{Run: f, Reauth: func(context.Context) error { reauths++; return nil }}
	if err := c.DismissReview(context.Background(), "talkable", "talkable", 5, 77, "Superseded"); err != nil {
		t.Fatal(err)
	}
	if reauths != 1 || len(f.Calls) != 2 || !f.Calls[1].Mutates {
		t.Errorf("reauths = %d, gh calls = %d", reauths, len(f.Calls))
	}
}

func TestReauthRetriesOnlyOnce(t *testing.T) {
	f := reauthFake(t, 5)
	reauths := 0
	c := &Client{Run: f, Reauth: func(context.Context) error { reauths++; return nil }}
	_, err := c.ReviewsWithMarker(context.Background(), "talkable", "talkable", 1, "")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if reauths != 1 || len(f.Calls) != 2 {
		t.Errorf("reauths = %d, gh calls = %d, want 1 and 2", reauths, len(f.Calls))
	}
}

func TestReauthFailureKeepsUnauthorized(t *testing.T) {
	f := reauthFake(t, 5)
	boom := errors.New("cannot mint a token")
	c := &Client{Run: f, Reauth: func(context.Context) error { return boom }}
	err := c.DismissReview(context.Background(), "talkable", "talkable", 5, 77, "m")
	if !errors.Is(err, ErrUnauthorized) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want both ErrUnauthorized and the reauth error", err)
	}
	if len(f.Calls) != 1 {
		t.Errorf("gh calls = %d; a failed reauth must not retry", len(f.Calls))
	}
}

func TestReauthSkippedForOtherErrors(t *testing.T) {
	body := compact(t, `{"message":"Resource not accessible by integration","status":"403"}`)
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"},
		Result: execx.Result{Stdout: body, Stderr: []byte("gh: Resource not accessible by integration (HTTP 403)\n"), Code: 1}}}}
	c := &Client{Run: f, Reauth: func(context.Context) error { t.Error("Reauth called for a 403"); return nil }}
	if err := c.DismissReview(context.Background(), "talkable", "talkable", 5, 77, "m"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
	if len(f.Calls) != 1 {
		t.Errorf("gh calls = %d", len(f.Calls))
	}
}
