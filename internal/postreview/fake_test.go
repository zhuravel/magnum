package postreview

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
)

// The tests drive Run through the real github and gitx clients over a
// scripted execx.Fake: gh and git are faked, never run.

const (
	head     = "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3"
	base     = "0123456789abcdef0123456789abcdef01234567"
	mergeSHA = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	runID    = "r-20261006T120000-7"
	login    = "talkable[bot]"
	reviews  = "repos/talkable/talkable/pulls/5/reviews"
)

func opts() Options {
	return Options{Owner: "talkable", Repo: "talkable", Number: 5, HeadSHA: head, RunID: runID, Login: login}
}

// prFile is one entry of GET pulls/{n}/files; a nil patch is a file GitHub
// sends no patch for.
type prFile struct {
	Filename string  `json:"filename"`
	Status   string  `json:"status"`
	Patch    *string `json:"patch"`
}

func patch(s string) *string { return &s }

// appPatch: a hunk of app/x.rb with RIGHT lines 10-16 and LEFT lines
// 10-15, then a second hunk with RIGHT 40-45 and LEFT 39-43.
const appPatch = "@@ -10,6 +10,7 @@ class X\n ctx\n ctx\n ctx\n+added\n ctx\n-gone\n+new\n ctx\n" +
	"@@ -39,5 +40,6 @@ def y\n ctx\n ctx\n+added\n ctx\n ctx\n ctx"

// node is a review as the GraphQL list reports it.
type node struct {
	ID     int64
	State  string
	Body   string
	Author string // GraphQL login, no [bot]
	Bot    bool
}

// world is a fake GitHub and checkout.
type world struct {
	t *testing.T

	prBase, prHead string
	files          []prFile
	compareFiles   []prFile // the comparison used when the PR moved on
	nodes          []node
	listErr        error
	// posts answer the POSTs in order; the default creates review 900.
	posts []func(c execx.Cmd) (execx.Result, error)
	// readback overrides the review GET returns (user, commit, state, body).
	readUser, readCommit, readState, readBody string
	readComments                              int // -1: as many as were sent

	gitDiffs map[string]string // path → git diff output
	missing  map[string]bool   // commits git does not have

	fake *execx.Fake
	sent []github.ReviewRequest
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, prBase: base, prHead: head, readComments: -1,
		files:    []prFile{{Filename: "app/x.rb", Status: "modified", Patch: patch(appPatch)}},
		gitDiffs: map[string]string{}, missing: map[string]bool{}}
	w.fake = &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"gh"}, Fn: w.gh},
		{Prefix: []string{"git"}, Fn: w.git},
	}}
	return w
}

func (w *world) deps() Deps {
	return Deps{GitHub: &github.Client{Run: w.fake, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}, Git: gitx.New(w.fake), Dir: "."}
}

func (w *world) run(o Options, file string) Outcome {
	return Run(context.Background(), w.deps(), o, []byte(file))
}

func ok(v any) (execx.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return execx.Result{Stdout: b}, nil
}

// apiFail mimics gh exiting 1 on an HTTP error with GitHub's JSON body.
func apiFail(c execx.Cmd, status int, message string, details ...string) (execx.Result, error) {
	body, _ := json.Marshal(map[string]any{"message": message, "errors": details, "status": fmt.Sprint(status)})
	stderr := fmt.Sprintf("gh: %s (HTTP %d)\n", message, status)
	return execx.Result{Stdout: body, Stderr: []byte(stderr), Code: 1}, &execx.ExitError{Cmd: c, Code: 1, Stderr: stderr}
}

func (w *world) gh(c execx.Cmd) (execx.Result, error) {
	args := c.Args
	if len(args) < 2 || args[0] != "api" {
		w.t.Fatalf("unexpected gh call %q", args)
	}
	if args[1] == "graphql" {
		if w.listErr != nil {
			return execx.Result{}, w.listErr
		}
		var nodes []map[string]any
		for _, n := range w.nodes {
			typ := "User"
			if n.Bot {
				typ = "Bot"
			}
			nodes = append(nodes, map[string]any{"databaseId": n.ID, "state": cmpOr(n.State, "COMMENTED"), "body": n.Body,
				"url":         fmt.Sprintf("https://github.com/talkable/talkable/pull/5#pullrequestreview-%d", n.ID),
				"submittedAt": "2026-10-06T12:00:00Z", "commit": map[string]any{"oid": head},
				"author": map[string]any{"login": n.Author, "__typename": typ}})
		}
		return ok(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviews": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": nodes}}}}})
	}
	if args[1] == "-X" && args[2] == "POST" && args[3] == reviews {
		var req github.ReviewRequest
		if err := json.Unmarshal(c.Stdin, &req); err != nil {
			w.t.Fatalf("POST without a JSON body on stdin: %v", err)
		}
		w.sent = append(w.sent, req)
		if i := len(w.sent) - 1; i < len(w.posts) {
			return w.posts[i](c)
		}
		return ok(map[string]any{"id": 900, "html_url": "https://github.com/talkable/talkable/pull/5#pullrequestreview-900",
			"state": reviewState(req.Event), "commit_id": req.CommitID, "user": map[string]any{"login": login, "type": "Bot"}})
	}
	path := args[1]
	switch {
	case path == "repos/talkable/talkable/pulls/5":
		return ok(map[string]any{"base": map[string]any{"sha": w.prBase}, "head": map[string]any{"sha": w.prHead}})
	case strings.HasPrefix(path, "repos/talkable/talkable/pulls/5/files?per_page=100&page="):
		if strings.HasSuffix(path, "&page=1") {
			return ok(w.files)
		}
		return ok([]prFile{})
	case strings.HasPrefix(path, "repos/talkable/talkable/compare/"):
		return ok(map[string]any{"status": "ahead", "files": w.compareFiles})
	case strings.HasPrefix(path, reviews+"/"):
		rest := strings.TrimPrefix(path, reviews+"/")
		id, tail, _ := strings.Cut(rest, "/")
		if tail != "" {
			n := w.readComments
			if n < 0 {
				n = 0
				if len(w.sent) > 0 {
					n = len(w.sent[len(w.sent)-1].Comments)
				}
			}
			comments := []map[string]any{}
			for i := range n {
				comments = append(comments, map[string]any{"id": i + 1, "path": "app/x.rb", "line": 12, "body": "x"})
			}
			return ok(comments)
		}
		state, body := w.readState, w.readBody
		if len(w.sent) > 0 {
			last := w.sent[len(w.sent)-1]
			state, body = cmpOr(state, reviewState(last.Event)), cmpOr(body, last.Body)
		}
		for _, n := range w.nodes {
			if fmt.Sprint(n.ID) == id {
				state, body = cmpOr(w.readState, cmpOr(n.State, "COMMENTED")), cmpOr(w.readBody, n.Body)
			}
		}
		return ok(map[string]any{"id": json.Number(id), "html_url": "https://github.com/talkable/talkable/pull/5#pullrequestreview-" + id,
			"state": state, "body": body, "commit_id": cmpOr(w.readCommit, head),
			"user": map[string]any{"login": cmpOr(w.readUser, login), "type": "Bot"}})
	}
	w.t.Fatalf("unexpected gh call %q", args)
	return execx.Result{}, nil
}

func (w *world) git(c execx.Cmd) (execx.Result, error) {
	args := c.Args
	if len(args) < 3 || args[0] != "-C" || args[1] != "." {
		w.t.Fatalf("git outside the current directory: %q", args)
	}
	switch args[2] {
	case "rev-parse":
		sha := strings.TrimSuffix(args[len(args)-1], "^{commit}")
		if w.missing[sha] {
			return execx.Result{Code: 1}, &execx.ExitError{Cmd: c, Code: 1}
		}
		return execx.Result{Stdout: []byte(sha + "\n")}, nil
	case "merge-base":
		return execx.Result{Stdout: []byte(mergeSHA + "\n")}, nil
	case "diff":
		path := strings.TrimPrefix(args[len(args)-1], ":(top,literal)")
		return execx.Result{Stdout: []byte(w.gitDiffs[path])}, nil
	}
	w.t.Fatalf("unexpected git call %q", args)
	return execx.Result{}, nil
}

// calls counts the recorded commands whose argv starts with prefix.
func (w *world) calls(prefix ...string) int { return len(w.fake.CallsWithPrefix(prefix...)) }

func (w *world) postCount() int { return w.calls("gh", "api", "-X", "POST") }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// review renders a review file.
func review(event, body string, comments ...string) string {
	return fmt.Sprintf(`{"event":%q,"body":%q,"comments":[%s]}`, event, body, strings.Join(comments, ","))
}

func comment(path string, line int, extra string) string {
	s := fmt.Sprintf(`{"path":%q,"line":%d,"body":"**[P2] wrong total**"`, path, line)
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}
