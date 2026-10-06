package cli

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// threadsJSON is GraphQL's answer for one review thread: the judge's
// comment 11 (an App, "talkable[bot]") and the author's reply 12.
const threadsJSON = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
{"id":"PRRT_1","isResolved":false,"isOutdated":false,"path":"app/x.rb","line":12,"originalLine":12,"diffSide":"RIGHT",
"comments":{"nodes":[
{"databaseId":11,"body":"**[P2] wrong total**","url":"https://github.com/talkable/talkable/pull/5#discussion_r11","createdAt":"2026-10-06T12:00:00Z","author":{"login":"talkable","__typename":"Bot"},"pullRequestReview":{"databaseId":77}},
{"databaseId":12,"body":"It is intended.","url":"https://github.com/talkable/talkable/pull/5#discussion_r12","createdAt":"2026-10-06T12:10:00Z","author":{"login":"alice","__typename":"User"},"pullRequestReview":null}]},
"root":{"nodes":[{"originalCommit":{"oid":"d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3"},"diffHunk":"@@ -1 +1 @@"}]}}]}}}}}`

// fakePostReplies routes post-review's gh calls to a fake GitHub with one
// thread of the judge's; a reply POST creates comment 4321.
func fakePostReplies(t *testing.T) *execx.Fake {
	t.Helper()
	f := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: []byte(threadsJSON)}},
		{Prefix: []string{"gh", "api", "-X", "POST", "repos/talkable/talkable/pulls/5/comments/11/replies"}, Result: execx.Result{
			Stdout: []byte(`{"id":4321,"html_url":"https://github.com/talkable/talkable/pull/5#discussion_r4321","created_at":"2026-10-06T12:30:00Z"}`)}},
	}}
	prev := postReviewRunner
	postReviewRunner = func() execx.Runner { return f }
	t.Cleanup(func() { postReviewRunner = prev })
	return f
}

// --replies answers in the threads as --gh-config-dir's identity: the reply
// goes out as JSON on gh's stdin (its text and the run's reply marker, none
// in argv), stdout is one JSON object listing it, stderr one summary line,
// and the config and registry stay unopened, as for a review.
func TestPostReviewRepliesPostsInTheThread(t *testing.T) {
	f := fakePostReplies(t)
	c, out, errb := bareContext(t)
	c.layoutErr = os.ErrPermission // no magnum layout: the command needs none
	file := writeReviewFile(t, `{"replies":[{"comment_id":11,"kind":"ack","body":"Thanks, fixed in a1b2c3d."}]}`)
	code := execute(c, postRepliesArgs(file, "--gh-config-dir", "/state/gh/talkable-app"))
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	var got struct {
		Status  string `json:"status"`
		Replies []struct {
			CommentID int64  `json:"comment_id"`
			ID        int64  `json:"id"`
			URL       string `json:"url"`
			Kind      string `json:"kind"`
			Already   bool   `json:"already"`
		} `json:"replies"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
	}
	if got.Status != "replied" || len(got.Replies) != 1 || got.Replies[0].CommentID != 11 || got.Replies[0].ID != 4321 ||
		got.Replies[0].Kind != "ack" || got.Replies[0].Already || !strings.HasSuffix(got.Replies[0].URL, "#discussion_r4321") {
		t.Errorf("stdout = %s", out)
	}
	if lines := strings.Split(strings.TrimSpace(errb.String()), "\n"); len(lines) != 1 || lines[0] != "magnum post-review: posted 1 reply (1 ack)" {
		t.Errorf("stderr = %q", errb)
	}
	posts := f.CallsWithPrefix("gh", "api", "-X", "POST")
	if len(posts) != 1 {
		t.Fatalf("calls = %+v", f.Calls)
	}
	post := posts[0]
	if !post.Mutates || post.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" || !slices.Contains(post.Args, "--input") {
		t.Errorf("POST = %+v", post)
	}
	for _, a := range post.Args {
		if strings.Contains(a, "Thanks") || strings.Contains(a, "magnum:reply") {
			t.Errorf("reply text in argv: %q", post.Args)
		}
	}
	var sent struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(post.Stdin, &sent); err != nil {
		t.Fatal(err)
	}
	if want := "Thanks, fixed in a1b2c3d.\n\n<!-- magnum:reply run=r-20261006T120000-7 kind=ack -->"; sent.Body != want {
		t.Errorf("body = %q, want %q", sent.Body, want)
	}
	if c.Config != nil {
		t.Error("post-review loaded the config")
	}
	if ents, _ := os.ReadDir(c.Layout.Home); len(ents) != 0 {
		t.Errorf("post-review wrote into the magnum home: %v", ents)
	}
}

// --dry-run with --replies plans the replies and posts nothing.
func TestPostReviewRepliesDryRunPostsNothing(t *testing.T) {
	f := fakePostReplies(t)
	c, out, errb := bareContext(t)
	file := writeReviewFile(t, `{"replies":[{"comment_id":11,"kind":"rebuttal","body":"The spec covers tax only."}]}`)
	if code := execute(c, postRepliesArgs(file, "--dry-run")); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if !strings.Contains(out.String(), `"status": "dry_run"`) || !strings.Contains(out.String(), `"kind": "rebuttal"`) {
		t.Errorf("stdout = %s", out)
	}
	if !strings.Contains(errb.String(), "dry run: nothing posted; 1 reply planned (1 rebuttal)") {
		t.Errorf("stderr = %q", errb)
	}
	if n := len(f.CallsWithPrefix("gh", "api", "-X", "POST")); n != 0 {
		t.Errorf("a dry run posted %d times", n)
	}
}

// A reply to a comment that does not start a thread of --login is the
// judge's file to fix: exit 2, the problem named, nothing posted.
func TestPostReviewRepliesRefusesAThreadOfSomeoneElse(t *testing.T) {
	f := fakePostReplies(t)
	c, out, errb := bareContext(t)
	file := writeReviewFile(t, `{"replies":[{"comment_id":12,"kind":"ack","body":"ok"}]}`)
	if code := execute(c, postRepliesArgs(file)); code != 2 {
		t.Fatalf("exit %d, want 2\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if !strings.Contains(out.String(), `"status": "invalid"`) || !strings.Contains(out.String(), "replies[0]: comment 12 does not start a thread of talkable[bot]") {
		t.Errorf("stdout = %s", out)
	}
	if n := len(f.CallsWithPrefix("gh", "api", "-X", "POST")); n != 0 {
		t.Errorf("posted %d times", n)
	}
}

// The help says what --replies is for: the file's shape, the marker, the
// checks, running again and the new status.
func TestPostReviewHelpDescribesReplies(t *testing.T) {
	c, out, errb := bareContext(t)
	if code := execute(c, []string{"post-review", "--help"}); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, errb)
	}
	help := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"--replies FILE", `{"replies":[{"comment_id":123,"kind":"ack|rebuttal|answer","body":"…"}]}`,
		"<!-- magnum:reply run=<run id> kind=<kind> -->", "rebutted twice", "status: posted, already_posted, replied,", "exactly one of --review and --replies"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q:\n%s", want, out)
		}
	}
}
