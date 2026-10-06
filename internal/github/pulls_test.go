package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// PullFiles reads every page, keeps each file's patch and marks a file
// GitHub sent none for.
func TestPullFilesReadsPatchesOverPages(t *testing.T) {
	page1 := make([]map[string]any, 100)
	for i := range page1 {
		page1[i] = map[string]any{"filename": fmt.Sprintf("app/f%d.rb", i), "status": "modified", "patch": "@@ -1 +1 @@\n-a\n+b"}
	}
	page1[0] = map[string]any{"filename": "db/structure.sql", "status": "modified"} // too large: no patch
	page2 := []map[string]any{{"filename": "app/new.rb", "previous_filename": "app/old.rb", "status": "renamed", "patch": "@@ -3 +3 @@\n-x\n+y"}}
	f := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"gh", "api", "repos/talkable/talkable/pulls/5/files?per_page=100&page=1"}, Fn: func(execx.Cmd) (execx.Result, error) { return okJSON(page1) }},
		{Prefix: []string{"gh", "api", "repos/talkable/talkable/pulls/5/files?per_page=100&page=2"}, Fn: func(execx.Cmd) (execx.Result, error) { return okJSON(page2) }},
	}}
	files, complete, err := (&Client{Run: f}).PullFiles(context.Background(), "talkable", "talkable", 5)
	if err != nil || !complete || len(files) != 101 {
		t.Fatalf("files %d complete %v err %v", len(files), complete, err)
	}
	if d := files[0]; !d.Truncated || d.Patch != "" {
		t.Errorf("patchless file = %+v", d)
	}
	if d := files[1]; d.Truncated || !strings.HasPrefix(d.Patch, "@@ -1 +1 @@") {
		t.Errorf("file with a patch = %+v", d)
	}
	if d := files[100]; d.Path != "app/new.rb" || d.PreviousPath != "app/old.rb" || d.Status != "renamed" {
		t.Errorf("renamed file = %+v", d)
	}
}

// PullFiles stops at GitHub's 3000-file cap and says the list may be short.
func TestPullFilesCapIsIncomplete(t *testing.T) {
	full := make([]map[string]any, 100)
	for i := range full {
		full[i] = map[string]any{"filename": fmt.Sprintf("f%d", i), "patch": "@@ -1 +1 @@"}
	}
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(execx.Cmd) (execx.Result, error) { return okJSON(full) }}}}
	files, complete, err := (&Client{Run: f}).PullFiles(context.Background(), "talkable", "talkable", 5)
	if err != nil || complete || len(files) != 3000 || len(f.Calls) != PullFilesMaxPages {
		t.Fatalf("files %d complete %v calls %d err %v", len(files), complete, len(f.Calls), err)
	}
}

func TestPullSHAs(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "repos/talkable/talkable/pulls/5"},
		Result: execx.Result{Stdout: []byte(`{"base":{"sha":"aaa","ref":"master"},"head":{"sha":"bbb"}}`)}}}}
	base, head, err := (&Client{Run: f}).PullSHAs(context.Background(), "talkable", "talkable", 5)
	if err != nil || base != "aaa" || head != "bbb" {
		t.Fatalf("%q %q %v", base, head, err)
	}
}

// SubmitReview sends the whole review, inline comments included, as JSON on
// gh's stdin: no review text reaches argv.
func TestSubmitReviewSendsJSONOnStdin(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "-X", "POST"},
		Result: execx.Result{Stdout: []byte(`{"id":900,"html_url":"https://github.com/talkable/talkable/pull/5#pullrequestreview-900","state":"COMMENTED","commit_id":"` + strings.Repeat("a", 40) + `","user":{"login":"talkable[bot]","type":"Bot"}}`)}}}}
	req := ReviewRequest{CommitID: strings.Repeat("a", 40), Event: "COMMENT", Body: "Fix 1 problem; `rm -rf` $(x)",
		Comments: []DraftComment{{Path: "app/x.rb", Line: 12, Side: "RIGHT", StartLine: 10, StartSide: "RIGHT", Body: "**[P2] wrong**"}}}
	got, err := (&Client{Run: f}).SubmitReview(context.Background(), "talkable", "talkable", 5, req)
	if err != nil || got.ID != 900 || got.UserLogin != "talkable[bot]" {
		t.Fatalf("%+v %v", got, err)
	}
	cmd := f.Calls[0]
	if want := []string{"api", "-X", "POST", "repos/talkable/talkable/pulls/5/reviews", "--hostname", "github.com", "--input", "-"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %q", cmd.Args)
	}
	if !cmd.Mutates {
		t.Error("a review must be marked Mutates")
	}
	var sent map[string]any
	if err := json.Unmarshal(cmd.Stdin, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["body"] != req.Body || sent["event"] != "COMMENT" || sent["commit_id"] != req.CommitID {
		t.Errorf("stdin = %s", cmd.Stdin)
	}
	c := sent["comments"].([]any)[0].(map[string]any)
	if c["path"] != "app/x.rb" || c["line"] != 12.0 || c["start_line"] != 10.0 || c["side"] != "RIGHT" || c["start_side"] != "RIGHT" {
		t.Errorf("comment = %v", c)
	}
	// No comments: an empty list, as GitHub's schema has it.
	req.Comments = nil
	if _, err := (&Client{Run: f}).SubmitReview(context.Background(), "talkable", "talkable", 5, req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.Calls[1].Stdin), `"comments":[]`) {
		t.Errorf("stdin = %s", f.Calls[1].Stdin)
	}
	if _, err := (&Client{Run: f}).SubmitReview(context.Background(), "talkable", "talkable", 5, ReviewRequest{CommitID: "abc", Event: "COMMENT"}); err == nil {
		t.Error("a short commit id: want error")
	}
}

// A REST error body's "errors" entries reach the APIError, so a caller can
// tell GitHub's reasons apart (a self-verdict, a line off the diff).
func TestAPIErrorKeepsRESTErrorDetails(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{
		Stdout: compact(t, `{"message":"Unprocessable Entity","errors":["Can not approve your own pull request",
			{"resource":"PullRequestReviewComment","code":"custom","field":"pull_request_review_thread.line","message":"could not be resolved"},
			{"resource":"PullRequestReview","code":"missing_field"}],"status":"422"}`),
		Stderr: []byte("gh: Unprocessable Entity (HTTP 422)\n"), Code: 1}}}}
	_, err := (&Client{Run: f}).SubmitReview(context.Background(), "talkable", "talkable", 5, ReviewRequest{CommitID: strings.Repeat("a", 40), Event: "APPROVE"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Fatalf("err = %v", err)
	}
	want := []string{"Can not approve your own pull request", "pull_request_review_thread.line could not be resolved", "missing_field"}
	if !slices.Equal(apiErr.Details, want) {
		t.Errorf("details = %q", apiErr.Details)
	}
	if !strings.Contains(err.Error(), "Unprocessable Entity; Can not approve your own pull request") {
		t.Errorf("error = %v", err)
	}
}

// ReviewComments reads past the first page of 100.
func TestReviewCommentsReadsEveryPage(t *testing.T) {
	page := func(n, from int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"id": from + i, "path": "app/x.rb", "line": 1, "body": "x"}
		}
		return out
	}
	f := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"gh", "api", "repos/talkable/talkable/pulls/5/reviews/77/comments?per_page=100"}, Fn: func(execx.Cmd) (execx.Result, error) { return okJSON(page(100, 1)) }},
		{Prefix: []string{"gh", "api", "repos/talkable/talkable/pulls/5/reviews/77/comments?per_page=100&page=2"}, Fn: func(execx.Cmd) (execx.Result, error) { return okJSON(page(3, 101)) }},
	}}
	got, err := (&Client{Run: f}).ReviewComments(context.Background(), "talkable", "talkable", 5, 77)
	if err != nil || len(got) != 103 || got[102].ID != 103 || len(f.Calls) != 2 {
		t.Fatalf("%d comments, %d calls, %v", len(got), len(f.Calls), err)
	}
}

func okJSON(v any) (execx.Result, error) {
	b, err := json.Marshal(v)
	return execx.Result{Stdout: b}, err
}
