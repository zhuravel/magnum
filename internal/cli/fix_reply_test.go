package cli

import (
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/postreview"
)

// fakeFixReply routes fix-reply's gh calls to a fake GitHub with
// threadsJSON's threads (a reply POST to comment 11 creates comment 4321)
// and an empty conversation (a comment POST creates comment 7001), and
// gives the command text on stdin; it returns the fake for its calls.
func fakeFixReply(t *testing.T, text string) *execx.Fake {
	t.Helper()
	f := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"gh", "api", "graphql"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			if strings.Contains(string(c.Stdin), "reviewThreads") {
				return execx.Result{Stdout: []byte(threadsJSON)}, nil
			}
			return execx.Result{Stdout: []byte(`{"data":{"repository":{"pullRequest":{"comments":{"pageInfo":{"hasPreviousPage":false,"startCursor":""},"nodes":[]}}}}}`)}, nil
		}},
		{Prefix: []string{"gh", "api", "-X", "POST", "repos/talkable/talkable/pulls/5/comments/11/replies"}, Result: execx.Result{
			Stdout: []byte(`{"id":4321,"html_url":"https://github.com/talkable/talkable/pull/5#discussion_r4321","created_at":"2026-10-06T12:30:00Z"}`)}},
		{Prefix: []string{"gh", "api", "-X", "POST", "repos/talkable/talkable/issues/5/comments"}, Result: execx.Result{
			Stdout: []byte(`{"id":7001,"html_url":"https://github.com/talkable/talkable/pull/5#issuecomment-7001"}`)}},
	}}
	prevRun, prevIn := fixReplyRunner, fixReplyStdin
	fixReplyRunner = func() execx.Runner { return f }
	fixReplyStdin = func() io.Reader { return strings.NewReader(text) }
	t.Cleanup(func() { fixReplyRunner, fixReplyStdin = prevRun, prevIn })
	return f
}

// The fixer's pane carries MAGNUM_PR_URL and MAGNUM_REPORT_DIR, which keep
// the operator's commands out of it; fix-reply is the fixer's own and runs
// there: it reads the PR from MAGNUM_PR_URL and the reply from stdin, posts
// it signed with the fix marker as the pane's gh login (no gh config of
// its own), prints one JSON object and one line, and opens neither the
// config nor the registry.
func TestFixReplyPostsFromTheFixersPane(t *testing.T) {
	t.Setenv("MAGNUM_PR_URL", "https://github.com/talkable/talkable/pull/5")
	t.Setenv("MAGNUM_REPORT_DIR", "/x/reviews/talkable/talkable/5/d4e5f6a")
	for _, tc := range []struct {
		args     []string
		post     string
		id       float64
		stderr   string
		wantBody string
	}{
		{[]string{"fix-reply", "--comment", "12"}, "repos/talkable/talkable/pulls/5/comments/11/replies", 4321,
			"magnum fix-reply: posted the reply in the thread of comment 11: https://github.com/talkable/talkable/pull/5#discussion_r4321",
			"Fixed in a1b2c3d: it's checked.\n\n" + postreview.FixReplyMarker},
		{[]string{"fix-reply", "--issue"}, "repos/talkable/talkable/issues/5/comments", 7001,
			"magnum fix-reply: posted the reply as a comment on the pull request: https://github.com/talkable/talkable/pull/5#issuecomment-7001",
			"Fixed in a1b2c3d: it's checked.\n\n" + postreview.FixReplyMarker},
	} {
		t.Run(tc.args[1], func(t *testing.T) {
			f := fakeFixReply(t, "Fixed in a1b2c3d: it's checked.\n")
			c, out, errb := bareContext(t)
			c.layoutErr = os.ErrPermission // no magnum layout: the command needs none
			if code := execute(c, tc.args); code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
			}
			if got["status"] != "replied" || got["id"] != tc.id {
				t.Errorf("stdout = %s", out)
			}
			if lines := strings.Split(strings.TrimSpace(errb.String()), "\n"); len(lines) != 1 || lines[0] != tc.stderr {
				t.Errorf("stderr = %q, want %q", errb, tc.stderr)
			}
			posts := f.CallsWithPrefix("gh", "api", "-X", "POST", tc.post)
			if len(posts) != 1 {
				t.Fatalf("calls = %+v", f.Calls)
			}
			var sent struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(posts[0].Stdin, &sent); err != nil || sent.Body != tc.wantBody {
				t.Errorf("body = %q, %v; want %q", sent.Body, err, tc.wantBody)
			}
			if _, set := posts[0].Env["GH_CONFIG_DIR"]; set || !slices.Contains(posts[0].Args, "--input") {
				t.Errorf("POST = %+v: want the pane's own gh login and the body on stdin", posts[0])
			}
			if c.Config != nil {
				t.Error("fix-reply loaded the config")
			}
		})
	}
}

// Without MAGNUM_PR_URL (not the fixer's pane), with a URL that names no
// pull request, or with both or neither of --comment and --issue,
// fix-reply posts nothing and exits 1; a refused reply exits 2.
func TestFixReplyRefusesWhatIsNotAFixersReply(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		args      []string
		code      int
		stderr    string
	}{
		{"no pane", "", []string{"--issue"}, 1, "MAGNUM_PR_URL is not set: fix-reply runs in the fixer's pane, which magnum starts with the PR's URL"},
		{"no PR", "https://github.com/talkable/talkable", []string{"--issue"}, 1, "MAGNUM_PR_URL"},
		{"both", "https://github.com/talkable/talkable/pull/5", []string{"--issue", "--comment", "11"}, 1, "give exactly one of --comment <id> and --issue"},
		{"neither", "https://github.com/talkable/talkable/pull/5", nil, 1, "give exactly one of --comment <id> and --issue"},
		{"a marker", "https://github.com/talkable/talkable/pull/5", []string{"--issue"}, 2, "the reply has 1 problem; nothing posted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAGNUM_PR_URL", tc.url)
			f := fakeFixReply(t, "Fixed.\n\n"+postreview.FixReplyMarker)
			c, out, errb := bareContext(t)
			if code := execute(c, append([]string{"fix-reply"}, tc.args...)); code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, out, errb)
			}
			if !strings.Contains(errb.String(), tc.stderr) {
				t.Errorf("stderr = %q, want %q in it", errb, tc.stderr)
			}
			if len(f.Calls) != 0 {
				t.Errorf("calls = %+v", f.Calls)
			}
		})
	}
}
