package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// commentKey is one RSA key for every test of the binary: the fixer App's
// private key, so the App's token is minted the way magnum mints it.
var commentKey = sync.OnceValue(func() []byte {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
})

// commentMint is the gh call that mints the fixer App's installation token
// (installation 8) through the gh transport, and commentApp the one that
// reads the App (GET /app) with its JWT.
var (
	commentMint = []string{"gh", "api", "--method", "POST", "--include", "--hostname", "github.com", "app/installations/8/access_tokens"}
	commentApp  = []string{"gh", "api", "--method", "GET", "--include", "--hostname", "github.com", "app"}
)

// commentHarness runs `magnum comment` against a fake GitHub: pull request
// talkable/talkable#5 by alice, with rev-ann's thread (comments 11 and
// 12), a thread magnum's review App started (31), one a former
// reviewer's App started (41) and one you started as zhuravel (51), and
// no reviews; the conversation and the review requests
// keep what the command writes, as GitHub would. The config has your gh
// identity zhuravel, the App magnum reviews as and the watch's fixer App
// (fix_identity fixer-app, login fixer-app[bot]); the registry is a file.
// GET /app names the App's slug (slug, default fixer-app), and GitHub
// shows each comment posted by poster (GraphQL login and typename,
// default the fixer App's).
type commentHarness struct {
	t          *testing.T
	c          *Context
	out, errb  *bytes.Buffer
	run        *execx.Fake
	layout     paths.Layout
	st         *store.Store
	mintFail   bool
	appFail    bool
	slug       string
	poster     [2]string
	title      string
	requested  []string
	comments   []map[string]any // the conversation, as GraphQL lists it
	replies    int
	requests   [][]string
	titleSends int
}

func newCommentHarness(t *testing.T, edit func(*config.Config)) *commentHarness {
	t.Helper()
	storetest.Serial(t)
	t.Setenv("MAGNUM_CONFIG", "")
	for _, k := range []string{"MAGNUM_REPORT_DIR", "MAGNUM_PR_URL", "MAGNUM_STAGING_RESOLVE", "MAGNUM_FACTORY_TASK"} {
		t.Setenv(k, "")
	}
	home := t.TempDir()
	layout := paths.Layout{Home: home}
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "fixer-app.pem")
	if err := os.WriteFile(keyFile, commentKey(), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Layout = layout
	cfg.Identities = []config.Identity{
		{Name: "zhuravel", Kind: "gh", Login: "zhuravel"},
		{Name: "review-app", Kind: "app", Login: "review-app[bot]", AppID: 1, InstallationID: 2, ClientID: "Iv1.review", PrivateKeyFile: keyFile},
		{Name: "fixer-app", Kind: "app", Login: "fixer-app[bot]", AppID: 7, InstallationID: 8, ClientID: "Iv1.fix", PrivateKeyFile: keyFile,
			TokenRepositories: []string{"talkable"}, TokenPermissions: map[string]string{"contents": "write", "pull_requests": "write"}, UserID: 4242},
	}
	cfg.Watches = []config.Watch{{Owner: "talkable", Include: []string{"talkable"}, Identity: "review-app", PollIdentity: "review-app",
		FixIdentity: "fixer-app", CloneRoot: home}}
	if edit != nil {
		edit(cfg)
	}
	h := &commentHarness{t: t, layout: layout, title: "Fix the total", slug: "fixer-app", poster: [2]string{"fixer-app", "Bot"},
		out: &bytes.Buffer{}, errb: &bytes.Buffer{}}
	h.st = storetest.OpenFile(t, layout.DB())
	h.c = &Context{Version: "test", Layout: layout, Config: cfg, Stdout: h.out, Stderr: h.errb}
	h.run = &execx.Fake{Rules: []execx.Rule{
		{Prefix: commentMint, Fn: h.mint},
		{Prefix: commentApp, Fn: h.app},
		{Prefix: []string{"gh", "api", "graphql"}, Fn: h.graphql},
		{Prefix: []string{"gh", "api", "-X", "POST", "repos/talkable/talkable/pulls/5/comments/11/replies"}, Fn: func(execx.Cmd) (execx.Result, error) {
			h.replies++
			return jsonResult(map[string]any{"id": 4321, "html_url": "https://github.com/talkable/talkable/pull/5#discussion_r4321",
				"user": map[string]any{"login": "fixer-app[bot]", "type": "Bot"}})
		}},
		{Prefix: []string{"gh", "api", "-X", "POST", "repos/talkable/talkable/issues/5/comments"}, Fn: h.postComment},
		{Prefix: []string{"gh", "api", "-X", "PATCH", "repos/talkable/talkable/pulls/5"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			var req struct{ Title string }
			_ = json.Unmarshal(c.Stdin, &req)
			h.title, h.titleSends = req.Title, h.titleSends+1
			return jsonResult(h.pull())
		}},
		{Prefix: []string{"gh", "api", "-X", "POST", "repos/talkable/talkable/pulls/5/requested_reviewers"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			var req struct{ Reviewers []string }
			_ = json.Unmarshal(c.Stdin, &req)
			h.requests = append(h.requests, req.Reviewers)
			h.requested = append(h.requested, req.Reviewers...)
			return jsonResult(h.pull())
		}},
		{Prefix: []string{"gh", "api", "repos/talkable/talkable/pulls/5"}, Fn: func(execx.Cmd) (execx.Result, error) { return jsonResult(h.pull()) }},
	}}
	prevRun, prevIn, prevTTY := commentRunner, commentStdin, commentStdinTTY
	commentRunner = func() execx.Runner { return h.run }
	commentStdinTTY = func() bool { return false }
	t.Cleanup(func() { commentRunner, commentStdin, commentStdinTTY = prevRun, prevIn, prevTTY })
	return h
}

func jsonResult(v any) (execx.Result, error) {
	b, err := json.Marshal(v)
	return execx.Result{Stdout: b}, err
}

func (h *commentHarness) mint(execx.Cmd) (execx.Result, error) {
	if h.mintFail {
		return execx.Result{Stdout: []byte("HTTP/2.0 500 Internal Server Error\nContent-Type: application/json\r\n\r\n{\"message\":\"boom\"}"), Code: 1},
			&execx.ExitError{Code: 1}
	}
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	return execx.Result{Stdout: []byte("HTTP/2.0 201 Created\nContent-Type: application/json\r\n\r\n" +
		`{"token":"ghs_commenttesttoken","expires_at":"` + expires + `"}`)}, nil
}

// app answers GET /app: the App's slug, or a failure with appFail.
func (h *commentHarness) app(execx.Cmd) (execx.Result, error) {
	if h.appFail {
		return execx.Result{Stdout: []byte("HTTP/2.0 502 Bad Gateway\nContent-Type: application/json\r\n\r\n{\"message\":\"boom\"}"), Code: 1},
			&execx.ExitError{Code: 1}
	}
	return execx.Result{Stdout: []byte("HTTP/2.0 200 OK\nContent-Type: application/json\r\n\r\n" +
		`{"id":7,"slug":"` + h.slug + `","owner":{"login":"talkable","type":"Organization"}}`)}, nil
}

func (h *commentHarness) pull() map[string]any {
	var requested []map[string]any
	for _, l := range h.requested {
		requested = append(requested, map[string]any{"login": l, "type": "User"})
	}
	return map[string]any{"number": 5, "title": h.title, "state": "open", "html_url": "https://github.com/talkable/talkable/pull/5",
		"user": map[string]any{"login": "alice", "type": "User"}, "requested_reviewers": requested}
}

// threadNode is one review thread of the fake's list: its comments by
// (id, GraphQL login, typename).
func threadNode(id string, comments ...[3]string) map[string]any {
	var nodes []map[string]any
	for _, c := range comments {
		nodes = append(nodes, map[string]any{"databaseId": json.Number(c[0]), "body": "a comment",
			"url": "https://github.com/talkable/talkable/pull/5#discussion_r" + c[0], "createdAt": "2026-10-06T12:00:00Z",
			"authorAssociation": "MEMBER", "author": map[string]any{"login": c[1], "__typename": c[2]}})
	}
	return map[string]any{"id": id, "isResolved": false, "isOutdated": false, "path": "app/x.rb", "line": 12, "originalLine": 12,
		"diffSide": "RIGHT", "comments": map[string]any{"totalCount": len(nodes), "nodes": nodes}, "root": map[string]any{"nodes": []any{}}}
}

func (h *commentHarness) graphql(c execx.Cmd) (execx.Result, error) {
	if strings.Contains(string(c.Stdin), "reviewThreads") {
		threads := []any{
			threadNode("PRRT_1", [3]string{"11", "rev-ann", "User"}, [3]string{"12", "alice", "User"}),
			threadNode("PRRT_3", [3]string{"31", "review-app", "Bot"}),
			threadNode("PRRT_4", [3]string{"41", "old-app", "Bot"}),
			threadNode("PRRT_5", [3]string{"51", "zhuravel", "User"}),
		}
		return jsonResult(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": threads}}}}})
	}
	if strings.Contains(string(c.Stdin), "reviews(first:") {
		return jsonResult(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviews": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": []any{}}}}}})
	}
	return jsonResult(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"comments": map[string]any{"pageInfo": map[string]any{"hasPreviousPage": false, "startCursor": ""}, "nodes": append([]map[string]any{}, h.comments...)}}}}})
}

func (h *commentHarness) postComment(c execx.Cmd) (execx.Result, error) {
	var req struct{ Body string }
	_ = json.Unmarshal(c.Stdin, &req)
	id := 7001 + len(h.comments)
	url := fmt.Sprintf("https://github.com/talkable/talkable/pull/5#issuecomment-%d", id)
	h.comments = append(h.comments, map[string]any{"databaseId": id, "url": url, "body": req.Body, "createdAt": "2026-10-06T12:30:00Z",
		"author": map[string]any{"login": h.poster[0], "__typename": h.poster[1]}, "authorAssociation": "NONE"})
	login, typ := h.poster[0], "User"
	if h.poster[1] == "Bot" {
		login, typ = login+"[bot]", "Bot"
	}
	return jsonResult(map[string]any{"id": id, "html_url": url, "user": map[string]any{"login": login, "type": typ}})
}

// exec runs `magnum comment` with args and text on stdin, from fresh
// buffers, and returns the exit code and the JSON on stdout.
func (h *commentHarness) exec(text string, args ...string) (int, map[string]any) {
	h.t.Helper()
	h.out.Reset()
	h.errb.Reset()
	commentStdin = func() io.Reader { return strings.NewReader(text) }
	code := execute(h.c, append([]string{"comment"}, args...))
	var got map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		h.t.Fatalf("stdout is not one JSON object: %v\n%s\nstderr: %s", err, h.out, h.errb)
	}
	return code, got
}

// githubCalls are the gh calls other than the App's own with its JWT (the
// mint, GET /app).
func (h *commentHarness) githubCalls() []execx.Cmd {
	return slices.DeleteFunc(slices.Clone(h.run.Calls), func(c execx.Cmd) bool {
		return slices.Contains(c.Args, "app/installations/8/access_tokens") || slices.Contains(c.Args, "app")
	})
}

// A chat session's comment goes out as the watch's fixer App, never as
// your login: magnum mints the App's token once, and every gh call names
// the App's gh config with the inherited token and host overridden, even
// with your GH_TOKEN and GH_CONFIG_DIR in the environment. It prints one
// JSON object and one line, and no token.
func TestCommentPostsAsTheWatchsFixerApp(t *testing.T) {
	h := newCommentHarness(t, nil)
	t.Setenv("GH_TOKEN", "gho_operatortoken")
	t.Setenv("GH_CONFIG_DIR", filepath.Join(t.TempDir(), "operator-gh"))
	code, got := h.exec("Renamed to `order_total` in a1b2c3d.\n", "talkable/talkable#5", "--reply-to", "12")
	if code != 0 || got["status"] != "posted" || got["as"] != "fixer-app[bot]" || got["identity"] != "fixer-app" || got["pr"] != "talkable/talkable#5" {
		t.Fatalf("exit %d, stdout %v\nstderr: %s", code, got, h.errb)
	}
	if c, _ := got["comment"].(map[string]any); c["kind"] != "reply" || c["thread"] != float64(11) || c["id"] != float64(4321) {
		t.Errorf("comment = %v", got["comment"])
	}
	if mints := h.run.CallsWithPrefix(commentMint...); len(mints) != 1 {
		t.Errorf("minted %d times, want once", len(mints))
	}
	calls := h.githubCalls()
	if len(calls) == 0 || h.replies != 1 {
		t.Fatalf("calls = %+v, replies %d", h.run.Calls, h.replies)
	}
	for _, c := range calls {
		if c.Env["GH_CONFIG_DIR"] != h.layout.GhConfigDir("fixer-app") || c.Env["GH_HOST"] != "github.com" {
			t.Errorf("call %q env = %v: want the fixer App's gh config and github.com", c.Args, c.Env)
		}
		for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
			if v, set := c.Env[k]; !set || v != "" {
				t.Errorf("call %q: %s = %q (set %v), want it blanked", c.Args, k, v, set)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(h.layout.GhConfigDir("fixer-app"), "hosts.yml")); err != nil {
		t.Errorf("the App's gh config: %v", err)
	}
	want := "magnum comment: as fixer-app[bot]: posted a reply in the thread of comment 11: https://github.com/talkable/talkable/pull/5#discussion_r4321"
	if lines := strings.Split(strings.TrimSpace(h.errb.String()), "\n"); len(lines) != 1 || lines[0] != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
	if strings.Contains(h.out.String()+h.errb.String(), "ghs_") {
		t.Error("the output carries the App's token")
	}
}

// Without a fix_identity, or with one that is your gh identity or the App
// magnum reviews as, magnum comment refuses (exit 2) and says which key to
// set: it never posts as your login or as whatever gh is logged in as, and
// mints nothing.
func TestCommentNeverFallsBackToYourLogin(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*config.Config)
		want string
	}{
		"no fix_identity":    {func(c *config.Config) { c.Watches[0].FixIdentity = "" }, "sets no fix_identity"},
		"your gh identity":   {func(c *config.Config) { c.Watches[0].FixIdentity = "zhuravel" }, `is kind = \"gh\", your own login`},
		"the review App":     {func(c *config.Config) { c.Watches[0].FixIdentity = "review-app" }, "is the App magnum reviews as"},
		"outside its tokens": {func(c *config.Config) { c.Identities[2].TokenRepositories = []string{"other"} }, "add \\\"talkable\\\" to token_repositories"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCommentHarness(t, tc.edit)
			code, got := h.exec("Renamed.", "talkable/talkable#5")
			if code != 2 || got["status"] != "invalid" || !strings.Contains(h.out.String(), tc.want) {
				t.Errorf("exit %d, stdout %s, want invalid with %q", code, h.out, tc.want)
			}
			if len(h.run.Calls) != 0 {
				t.Errorf("gh ran: %+v", h.run.Calls)
			}
		})
	}
}

// A repository no watch covers is refused, and so is a reference that
// leaves the repository to a default: a chat session names the PR in full.
func TestCommentRefusesARepositoryMagnumDoesNotWatch(t *testing.T) {
	for ref, want := range map[string]string{
		"example/other#3":                         "magnum watches no repository example/other",
		"https://github.com/example/other/pull/3": "magnum watches no repository example/other",
		"talkable#5":                              "owner/repo#N", "#5": "owner/repo#N", "5": "owner/repo#N",
	} {
		h := newCommentHarness(t, nil)
		code, got := h.exec("Renamed.", ref)
		if code != 2 || got["status"] != "invalid" || !strings.Contains(h.out.String(), want) {
			t.Errorf("%s: exit %d, stdout %s, want invalid with %q", ref, code, h.out, want)
		}
		if len(h.run.Calls) != 0 {
			t.Errorf("%s: gh ran: %+v", ref, h.run.Calls)
		}
	}
}

// The review agents', the staging resolver's and the factory's panes read
// a PR's or a ticket's text, which may steer them: magnum comment refuses
// to post from them (the fixer has fix-reply, the judge post-review).
func TestCommentRefusesInAReviewAgentsPane(t *testing.T) {
	for _, k := range []string{"MAGNUM_PR_URL", "MAGNUM_REPORT_DIR", "MAGNUM_STAGING_RESOLVE", "MAGNUM_FACTORY_TASK"} {
		t.Run(k, func(t *testing.T) {
			h := newCommentHarness(t, nil)
			t.Setenv(k, "x")
			code, got := h.exec("Renamed.", "talkable/talkable#5")
			if code != 2 || got["status"] != "invalid" || !strings.Contains(h.out.String(), k+" is set") {
				t.Errorf("exit %d, stdout %s", code, h.out)
			}
			if len(h.run.Calls) != 0 {
				t.Errorf("gh ran: %+v", h.run.Calls)
			}
		})
	}
}

// When the App's token cannot be minted nothing is posted and no other
// login is tried: exit 1, run the same line again.
func TestCommentFailsWhenTheTokenCannotBeMinted(t *testing.T) {
	h := newCommentHarness(t, nil)
	h.mintFail = true
	code, got := h.exec("Renamed.", "talkable/talkable#5")
	if code != 1 || got["status"] != "error" || !strings.Contains(h.out.String(), "fixer-app") {
		t.Errorf("exit %d, stdout %s", code, h.out)
	}
	if calls := h.githubCalls(); len(calls) != 0 {
		t.Errorf("gh ran past the mint: %+v", calls)
	}
}

// Each post is an event comment.posted of the PR in magnum's registry: the
// PR, the App, the kind of post and its link, never the comment's or the
// title's text. A retry that finds the comment there writes its event at
// most once; a dry run writes none.
func TestCommentRecordsEachPostInTheRegistry(t *testing.T) {
	h := newCommentHarness(t, nil)
	repo, err := h.st.UpsertRepo(t.Context(), store.Repo{NodeID: "R_tt", Owner: "talkable", Name: "talkable", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.UpsertPRFromGitHub(t.Context(), store.GitHubPR{RepoID: repo.ID, NodeID: "PR_5", Number: 5, URL: "https://github.com/talkable/talkable/pull/5",
		HeadSHA: "abc1234def5678", Title: new("Fix the total"), AuthorLogin: new("alice"), GHState: store.GHOpen, InitialState: store.PRReviewed,
		Identity: "review-app"}); err != nil {
		t.Fatal(err)
	}
	args := []string{"https://github.com/Talkable/Talkable/pull/5", "--title", "Fix the order total", "--request-review", "rev-ann"}
	if code, got := h.exec("Secret plan: rename it.", args...); code != 0 || got["status"] != "posted" || got["event"] != nil {
		t.Fatalf("exit %d, stdout %v\nstderr: %s", code, got, h.errb)
	}
	events := func() []store.Event {
		evs, err := h.st.EventsBySubject(t.Context(), "pr:talkable/talkable#5", 0)
		if err != nil {
			t.Fatal(err)
		}
		return slices.DeleteFunc(evs, func(e store.Event) bool { return e.Kind != "comment.posted" })
	}
	evs := events()
	var posts []string
	for _, e := range evs {
		var d map[string]any
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		posts = append(posts, fmt.Sprint(d["post"]))
		if d["identity"] != "fixer-app" || d["login"] != "fixer-app[bot]" || d["repo"] != "talkable/talkable" || d["number"] != float64(5) || d["url"] == "" {
			t.Errorf("event %s data = %v", e.Kind, d)
		}
		if d["post"] == postreview.PostConversation && (d["comment_id"] != float64(7001) || d["url"] != "https://github.com/talkable/talkable/pull/5#issuecomment-7001") {
			t.Errorf("the comment's event data = %v", d)
		}
		if d["post"] == postreview.PostReviewRequest && fmt.Sprint(d["reviewers"]) != "[rev-ann]" {
			t.Errorf("the review request's event data = %v", d)
		}
		if strings.Contains(e.Message+string(e.Data), "Secret plan") || strings.Contains(e.Message+string(e.Data), "order total") {
			t.Errorf("event %q carries the text: %s", e.Message, e.Data)
		}
	}
	if !slices.Equal(posts, []string{postreview.PostConversation, postreview.PostTitle, postreview.PostReviewRequest}) {
		t.Fatalf("posts = %v", posts)
	}

	if code, got := h.exec("Secret plan: rename it.", args...); code != 0 || got["status"] != "already_posted" {
		t.Fatalf("retry: exit %d, stdout %v", code, got)
	}
	if n := len(events()); n != 3 || len(h.comments) != 1 || h.titleSends != 1 || len(h.requests) != 1 {
		t.Errorf("after the retry: %d events, %d comments, %d titles, %d requests", n, len(h.comments), h.titleSends, len(h.requests))
	}
	if code, got := h.exec("Another plan.", append(args, "--dry-run")...); code != 0 || got["status"] != "dry_run" || len(events()) != 3 || len(h.comments) != 1 {
		t.Errorf("dry run: exit %d, stdout %v, %d events", code, got, len(events()))
	}
	if !strings.Contains(h.errb.String(), "as fixer-app[bot] (identity fixer-app)") {
		t.Errorf("dry run stderr = %q", h.errb)
	}

	// A record that cannot be written leaves the post posted (exit 0) and
	// says so.
	prev := commentOpenStore
	commentOpenStore = func(c *Context) (*store.Store, error) {
		return store.OpenWith(c.Layout.DB(), store.Options{QueryOnly: true})
	}
	t.Cleanup(func() { commentOpenStore = prev })
	code, got := h.exec("A third note.", "talkable/talkable#5")
	if note, _ := got["event"].(string); code != 0 || got["status"] != "posted" || !strings.HasPrefix(note, "not recorded: ") ||
		!strings.Contains(h.errb.String(), "run it again to record it") {
		t.Errorf("an unwritable registry: exit %d, stdout %v, stderr %q", code, got, h.errb)
	}
}

// A reply in a thread the PR's last reviewer started is refused even when
// that login is no identity of the config any more: the registry's
// last_review_login counts among magnum's.
func TestCommentRefusesAThreadOfThePRsLastReviewer(t *testing.T) {
	h := newCommentHarness(t, nil)
	repo, err := h.st.UpsertRepo(t.Context(), store.Repo{NodeID: "R_tt", Owner: "talkable", Name: "talkable", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.st.UpsertPRFromGitHub(t.Context(), store.GitHubPR{RepoID: repo.ID, NodeID: "PR_5", Number: 5, URL: "https://github.com/talkable/talkable/pull/5",
		HeadSHA: "abc1234def5678", AuthorLogin: new("alice"), GHState: store.GHOpen, InitialState: store.PRReviewed, Identity: "review-app"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpdatePR(t.Context(), res.PR.ID, func(u *store.PRUpdate) { u.Set("last_review_login", "old-app[bot]") }); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"31", "41"} {
		code, got := h.exec("It is right.", "talkable/talkable#5", "--reply-to", id)
		if code != 2 || got["status"] != "invalid" || !strings.Contains(h.out.String(), "a thread magnum's review started") {
			t.Errorf("comment %s: exit %d, stdout %s", id, code, h.out)
		}
	}
	if h.replies != 0 {
		t.Errorf("replied %d times", h.replies)
	}
}

// A reply in a thread the PR's own identity or one of its former ones
// started is refused, since the poll and the threads file take those
// threads for magnum's: a PR pinned to your gh identity (magnum review
// --as), or one whose watch moved from your gh identity to an App
// (pr.N.former_identities). With neither, a thread you started by hand is
// one the App may answer.
func TestCommentRefusesAThreadOfThePRsOwnOrFormerIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		identity, former string
		refused          bool
	}{
		"pinned to your gh identity": {identity: "zhuravel", refused: true},
		"a former gh identity":       {identity: "review-app", former: `["zhuravel"]`, refused: true},
		"neither":                    {identity: "review-app"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCommentHarness(t, nil)
			repo, err := h.st.UpsertRepo(t.Context(), store.Repo{NodeID: "R_tt", Owner: "talkable", Name: "talkable", Mode: store.RepoModePerPR})
			if err != nil {
				t.Fatal(err)
			}
			res, err := h.st.UpsertPRFromGitHub(t.Context(), store.GitHubPR{RepoID: repo.ID, NodeID: "PR_5", Number: 5, URL: "https://github.com/talkable/talkable/pull/5",
				HeadSHA: "abc1234def5678", AuthorLogin: new("alice"), GHState: store.GHOpen, InitialState: store.PRReviewed, Identity: tc.identity})
			if err != nil {
				t.Fatal(err)
			}
			if tc.former != "" {
				if err := h.st.SetKV(t.Context(), engine.KVPRFormerIdentities(res.PR.ID), tc.former); err != nil {
					t.Fatal(err)
				}
			}
			code, got := h.exec("It is right.", "talkable/talkable#5", "--reply-to", "51", "--dry-run")
			switch {
			case tc.refused && (code != 2 || got["status"] != "invalid" || !strings.Contains(h.out.String(), "a thread magnum's review started (zhuravel)")):
				t.Errorf("exit %d, stdout %s: want it refused", code, h.out)
			case !tc.refused && (code != 0 || got["status"] != "dry_run"):
				t.Errorf("exit %d, stdout %s: want it planned", code, h.out)
			}
		})
	}
}

// A login of the identity that is not the one GitHub shows the App's posts
// by (a typo, a block copied from another App) is refused before the first
// write: GET /app names the App's slug, and its "[bot]" login is what the
// config must say. Exit 2, the login to set, and nothing posted, however
// often the same line runs. GET /app failing is exit 1, nothing posted.
func TestCommentRefusesALoginTheAppDoesNotPostAs(t *testing.T) {
	h := newCommentHarness(t, func(c *config.Config) { c.Identities[2].Login = "fixer-bot[bot]" })
	args := []string{"talkable/talkable#5", "--title", "Fix the order total", "--request-review", "rev-ann"}
	for run := range 2 {
		code, got := h.exec("Renamed.", args...)
		if code != 2 || got["status"] != "invalid" || !strings.Contains(h.out.String(), `set login = \"fixer-app[bot]\" for identity fixer-app`) {
			t.Errorf("run %d: exit %d, stdout %s", run+1, code, h.out)
		}
		if len(h.comments) != 0 || h.titleSends != 0 || len(h.requests) != 0 {
			t.Fatalf("run %d: %d comments, %d titles, %d requests: want nothing posted", run+1, len(h.comments), h.titleSends, len(h.requests))
		}
	}
	if code, got := h.exec("Renamed.", append(args, "--dry-run")...); code != 2 || got["status"] != "invalid" {
		t.Errorf("a dry run: exit %d, stdout %s", code, h.out)
	}

	h = newCommentHarness(t, nil)
	h.appFail = true
	if code, got := h.exec("Renamed.", args...); code != 1 || got["status"] != "error" || !strings.Contains(h.out.String(), "nothing posted") ||
		len(h.comments) != 0 || h.titleSends != 0 {
		t.Errorf("GET /app failing: exit %d, stdout %s, %d comments", code, h.out, len(h.comments))
	}
}

// GitHub showing the comment by a person, not the App (it went out under
// someone's login), is exit 2, never 1, which would post it again, and
// nothing more goes out. The post is still recorded, as a warning that
// names the author GitHub showed.
func TestCommentRecordsAPostGitHubShowsByAPerson(t *testing.T) {
	h := newCommentHarness(t, nil)
	h.poster = [2]string{"zhuravel", "User"}
	if _, err := h.st.UpsertRepo(t.Context(), store.Repo{NodeID: "R_tt", Owner: "talkable", Name: "talkable", Mode: store.RepoModePerPR}); err != nil {
		t.Fatal(err)
	}
	args := []string{"talkable/talkable#5", "--title", "Fix the order total"}
	code, got := h.exec("Renamed.", args...)
	if code != 2 || got["status"] != "invalid" || !strings.Contains(h.out.String(), "it went out under a person's login") ||
		len(h.comments) != 1 || h.titleSends != 0 {
		t.Fatalf("exit %d, stdout %s, %d comments, %d titles", code, h.out, len(h.comments), h.titleSends)
	}
	evs, err := h.st.EventsBySubject(t.Context(), "pr:talkable/talkable#5", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs = slices.DeleteFunc(evs, func(e store.Event) bool { return e.Kind != "comment.posted" })
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want the comment's", evs)
	}
	var d map[string]any
	if err := json.Unmarshal(evs[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if evs[0].Level != "warn" || d["author"] != "zhuravel" || d["login"] != "fixer-app[bot]" || d["comment_id"] != float64(7001) ||
		!strings.Contains(evs[0].Message, "GitHub shows it by zhuravel") {
		t.Errorf("event %s %q, data %v", evs[0].Level, evs[0].Message, d)
	}
}

// A line the caller got wrong exits 2 with the JSON's problems: a flag it
// does not have, a comment id that is no number, no argument or two, and
// no comment on stdin where one is needed.
func TestCommentFlagErrorsExit2(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		text string
		tty  bool
		want string
	}{
		"an unknown flag":       {[]string{"talkable/talkable#5", "--issue"}, "x", false, "unknown flag: --issue"},
		"a thread's node id":    {[]string{"talkable/talkable#5", "--reply-to", "PRRT_kwDOAbCd"}, "x", false, "--reply-to takes the number id of a review comment"},
		"no argument":           {nil, "x", false, "give one pull request"},
		"two arguments":         {[]string{"talkable/talkable#5", "talkable/talkable#6"}, "x", false, "give one pull request"},
		"no comment":            {[]string{"talkable/talkable#5"}, "", false, "the comment is empty"},
		"a terminal for stdin":  {[]string{"talkable/talkable#5"}, "", true, "pass the comment on standard input (a pipe or a heredoc)"},
		"a reply with no input": {[]string{"talkable/talkable#5", "--reply-to", "12", "--title", "x"}, "", true, "pass the comment on standard input"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCommentHarness(t, nil)
			commentStdinTTY = func() bool { return tc.tty }
			code, got := h.exec(tc.text, tc.args...)
			if code != 2 || got["status"] != "invalid" || !strings.Contains(h.out.String()+h.errb.String(), tc.want) {
				t.Errorf("exit %d, stdout %s, stderr %s; want invalid with %q", code, h.out, h.errb, tc.want)
			}
			if h.replies != 0 || len(h.comments) != 0 {
				t.Error("posted for a wrong line")
			}
		})
	}
	// With only a title, a terminal on stdin is no comment and nothing is read.
	h := newCommentHarness(t, nil)
	commentStdinTTY = func() bool { return true }
	if code, got := h.exec("never read", "talkable/talkable#5", "--title", "Fix the order total"); code != 0 || got["status"] != "posted" || len(h.comments) != 0 {
		t.Errorf("a title alone: exit %d, stdout %v", code, got)
	}
}

// The help says plainly what the command does, as whom, and what it
// refuses: the operator's rule tells chat sessions to use it.
func TestCommentHelpSaysWhatAsWhomAndWhatItRefuses(t *testing.T) {
	c, out, _ := bareContext(t)
	if code := execute(c, []string{"comment", "--help"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	help := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"fix_identity", "never as your own login", "<!-- magnum:comment -->", "--dry-run", "token_repositories",
		"--reply-to", "--request-review", "--title", "standard input", "a review agent's pane", "comment.posted",
		// Where the token goes: the App's own gh config, on a dry run too.
		"writes it only to the App's own gh config", "even on --dry-run",
		// The exact form that answers a review body or a comment.
		"the remark's own link alone on the next line", "#pullrequestreview-N", "#issuecomment-N"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(help, "never prints, logs or stores") {
		t.Errorf("help says the token is never stored:\n%s", out)
	}
}
