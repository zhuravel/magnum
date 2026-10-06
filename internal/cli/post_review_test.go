package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

// The line a judge prompt renders (agents.PostReviewLine) is a command this
// build runs: its flags are post-review's, every value arrives intact.
func TestPostReviewRunsTheJudgesLine(t *testing.T) {
	f := fakePostReview(t)
	dir := filepath.Join(t.TempDir(), "report dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	d := agents.JudgeData{Magnum: "/opt/magnum/bin/magnum", Owner: "talkable", Repo: "talkable", Number: 5, HeadSHA: postReviewHead,
		RunID: "r-20261006T120000-7", ReviewerLogin: "talkable[bot]", FormerLogins: []string{"zhuravel"},
		GhConfigDir: "/state/gh/talkable app", DryRun: true, ResultFile: filepath.Join(dir, "codex-judge.json")}
	line, err := agents.RenderPrompt(config.Prompt{Name: "line", Text: "{{.PostReviewCommand}}"}, d)
	if err != nil {
		t.Fatal(err)
	}
	argv := shellWords(t, line)
	if argv[0] != "/opt/magnum/bin/magnum" {
		t.Fatalf("argv = %q", argv)
	}
	if err := os.WriteFile(filepath.Join(dir, "review.json"), []byte(`{"event":"COMMENT","body":"No problems found.","comments":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, out, errb := bareContext(t)
	if code := execute(c, argv[1:]); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if !strings.Contains(out.String(), `"status": "dry_run"`) || len(f.Calls) != 1 || f.Calls[0].Env["GH_CONFIG_DIR"] != "/state/gh/talkable app" {
		t.Errorf("stdout = %s, calls = %+v", out, f.Calls)
	}
}

// shellWords splits a line the way a POSIX shell does for the quoting
// agents.shellQuote produces: plain words and single-quoted runs.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord, quoted := false, false
	for _, r := range line {
		switch {
		case quoted && r == '\'':
			quoted = false
		case quoted:
			cur.WriteRune(r)
		case r == '\'':
			quoted, inWord = true, true
		case r == '\\':
			t.Fatalf("unexpected backslash outside quotes in %q", line)
		case r == ' ':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quoted {
		t.Fatalf("unterminated quote in %q", line)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

const postReviewHead = "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3"

// fakePostReview routes post-review's gh calls to a fake GitHub with no
// reviews; it returns the fake for its recorded calls.
func fakePostReview(t *testing.T) *execx.Fake {
	t.Helper()
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{
		Stdout: []byte(`{"data":{"repository":{"pullRequest":{"reviews":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`)}}}}
	prev := postReviewRunner
	postReviewRunner = func() execx.Runner { return f }
	t.Cleanup(func() { postReviewRunner = prev })
	return f
}

// postBaseArgs are post-review's flags but the file.
func postBaseArgs(extra ...string) []string {
	return append([]string{"post-review", "--repo", "talkable/talkable", "--pr", "5", "--head", postReviewHead,
		"--run-id", "r-20261006T120000-7", "--login", "talkable[bot]"}, extra...)
}

func postReviewArgs(file string, extra ...string) []string {
	return postBaseArgs(append([]string{"--review", file}, extra...)...)
}

func postRepliesArgs(file string, extra ...string) []string {
	return postBaseArgs(append([]string{"--replies", file}, extra...)...)
}

func writeReviewFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// post-review runs from the judge's pane with its flags alone: it reads no
// config (an unreadable one does not matter) and opens no registry (the
// layout's directory stays empty), and posts as --gh-config-dir.
func TestPostReviewOpensNeitherConfigNorRegistry(t *testing.T) {
	f := fakePostReview(t)
	c, out, errb := bareContext(t)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("not toml ["), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGNUM_CONFIG", cfg)
	c.cfgPath = cfg
	c.layoutErr = os.ErrPermission // no magnum layout either
	file := writeReviewFile(t, `{"event":"COMMENT","body":"No problems found. LGTM :shipit:","comments":[]}`)
	code := execute(c, postReviewArgs(file, "--dry-run", "--gh-config-dir", "/state/gh/talkable-app"))
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
	}
	if got["status"] != "dry_run" || got["planned_review"] == nil {
		t.Errorf("stdout = %s", out)
	}
	if lines := strings.Split(strings.TrimSpace(errb.String()), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "magnum post-review: dry run") {
		t.Errorf("stderr = %q", errb)
	}
	if c.Config != nil {
		t.Error("post-review loaded the config")
	}
	if ents, _ := os.ReadDir(c.Layout.Home); len(ents) != 0 {
		t.Errorf("post-review wrote into the magnum home: %v", ents)
	}
	if len(f.Calls) != 1 || f.Calls[0].Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
		t.Errorf("calls = %+v", f.Calls)
	}
}

// The exit status tells the judge what to do: 2 to fix its file (here: no
// file at all), 1 for anything else, a bad flag included (magnum's line,
// not the judge's file); stdout carries one JSON object either way.
func TestPostReviewExitCodes(t *testing.T) {
	fakePostReview(t)
	cases := []struct {
		name   string
		args   []string
		code   int
		status string
	}{
		{"no review file", postReviewArgs(filepath.Join(t.TempDir(), "review.json")), 2, "invalid"},
		{"invalid review", postReviewArgs(writeReviewFile(t, `{"event":"LGTM","body":"x"}`)), 2, "invalid"},
		{"unknown flag", postReviewArgs(writeReviewFile(t, `{}`), "--force"), 1, "error"},
		{"short head", append(postReviewArgs(writeReviewFile(t, `{}`)), "--head", "d4e5f6a"), 1, "error"},
		{"repo without owner", append(postReviewArgs(writeReviewFile(t, `{}`)), "--repo", "talkable"), 1, "error"},
		{"local base without dry run", postReviewArgs(writeReviewFile(t, `{}`), "--local-base", postReviewHead), 1, "error"},
		{"neither review nor replies", postBaseArgs(), 1, "error"},
		{"review and replies", postReviewArgs(writeReviewFile(t, `{}`), "--replies", writeReviewFile(t, `{"replies":[]}`)), 1, "error"},
		{"replies with a local base", append(postRepliesArgs(writeReviewFile(t, `{"replies":[]}`)), "--dry-run", "--local-base", postReviewHead), 1, "error"},
		{"no replies file", postRepliesArgs(filepath.Join(t.TempDir(), "replies.json")), 2, "invalid"},
		{"invalid replies", postRepliesArgs(writeReviewFile(t, `{"replies":[{"comment_id":1,"kind":"agree","body":"x"}]}`)), 2, "invalid"},
		{"replies that are a review", postRepliesArgs(writeReviewFile(t, `{"event":"COMMENT","body":"x","comments":[]}`)), 2, "invalid"},
		{"empty replies", postRepliesArgs(writeReviewFile(t, `{"replies":[]}`)), 0, "replied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errb := bareContext(t)
			if code := execute(c, tc.args); code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, out, errb)
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil || got["status"] != tc.status {
				t.Errorf("stdout = %s (%v), want status %s", out, err, tc.status)
			}
			if strings.Contains(errb.String(), "usage:") {
				t.Errorf("stderr prints usage: %s", errb)
			}
		})
	}
}
