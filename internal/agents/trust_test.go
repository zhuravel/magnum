package agents

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
)

const (
	trustSlot = "/Users/x/Projects/sandbox.pr1"
	trustRoot = "/Users/x/Projects/sandbox"
)

// gitCommonDir makes the fake git report dir as a linked worktree of root.
func gitCommonDir(e *env, dir, root string) {
	e.run.Rules = append([]execx.Rule{{Prefix: []string{"git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir"},
		Result: execx.Result{Stdout: []byte(root + "/.git\n")}}}, e.run.Rules...)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestEnsureTrustCodexAppendsRootAndDirOnce(t *testing.T) {
	e := newEnv(t)
	gitCommonDir(e, trustSlot, trustRoot)
	// No trailing newline: the appended tables must still start on their own line.
	orig := "model = \"gpt-5.5\"\n\n[projects.\"/Users/x/Projects/other\"]\ntrust_level = \"trusted\"\n\n[mcp_servers.github]\ncommand = \"gh\""
	writeFile(t, e.codexConfig, orig, 0o640)

	if err := e.m.EnsureTrust(e.ctx, KindCodex, trustSlot); err != nil {
		t.Fatal(err)
	}
	want := orig + "\n\n[projects.\"" + trustRoot + "\"]\ntrust_level = \"trusted\"\n\n[projects.\"" + trustSlot + "\"]\ntrust_level = \"trusted\"\n"
	if got := readFile(t, e.codexConfig); got != want {
		t.Fatalf("config =\n%s\nwant\n%s", got, want)
	}
	if m := fileMode(t, e.codexConfig); m != 0o640 {
		t.Fatalf("mode = %o, want 640", m)
	}
	if logs := e.logs.all(); len(logs) != 1 || !strings.Contains(logs[0], trustRoot+", "+trustSlot) {
		t.Fatalf("logs = %q, want one line naming both paths", logs)
	}
	calls := e.run.CallsWithPrefix("git")
	if len(calls) == 0 {
		t.Fatal("no git probe ran")
	}
	for _, c := range calls {
		if c.Mutates || c.Timeout == 0 {
			t.Fatalf("git probe must be read-only with a timeout: %+v", c)
		}
		if !slices.Equal(c.Unset, gitx.ScrubbedEnv()) {
			t.Fatalf("git probe Unset = %q, want gitx.ScrubbedEnv() %q", c.Unset, gitx.ScrubbedEnv())
		}
	}

	// Idempotent: nothing to add, nothing written or logged.
	if err := e.m.EnsureTrust(e.ctx, KindCodex, trustSlot); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, e.codexConfig); got != want {
		t.Fatalf("second call changed the config:\n%s", got)
	}
	if n := len(e.logs.all()); n != 1 {
		t.Fatalf("log lines = %d after an idempotent call, want 1", n)
	}
	entries, _ := os.ReadDir(filepath.Dir(e.codexConfig))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestEnsureTrustCodexExistingTables(t *testing.T) {
	e := newEnv(t)
	gitCommonDir(e, trustSlot, trustRoot)
	// The root's table lacks trust_level (it gets the line under its header);
	// the slot was explicitly marked untrusted (kept).
	orig := "[projects.\"" + trustRoot + "\"] # added by hand\nnote = \"x\"\n\n[projects.\"" + trustSlot + "\"]\ntrust_level = \"untrusted\"\n"
	writeFile(t, e.codexConfig, orig, 0o600)
	if err := e.m.EnsureTrust(e.ctx, KindCodex, trustSlot); err != nil {
		t.Fatal(err)
	}
	want := "[projects.\"" + trustRoot + "\"] # added by hand\ntrust_level = \"trusted\"\nnote = \"x\"\n\n[projects.\"" + trustSlot + "\"]\ntrust_level = \"untrusted\"\n"
	if got := readFile(t, e.codexConfig); got != want {
		t.Fatalf("config =\n%s\nwant\n%s", got, want)
	}
}

func TestEnsureTrustCodexCreatesMissingConfig(t *testing.T) {
	e := newEnv(t) // git has no rule: the root falls back to the dir itself
	if err := e.m.EnsureTrust(e.ctx, KindCodex, trustSlot+"/"); err != nil {
		t.Fatal(err)
	}
	want := "[projects.\"" + trustSlot + "\"]\ntrust_level = \"trusted\"\n"
	if got := readFile(t, e.codexConfig); got != want {
		t.Fatalf("config =\n%s\nwant\n%s", got, want)
	}
	if m := fileMode(t, e.codexConfig); m != 0o600 {
		t.Fatalf("mode = %o, want 600", m)
	}
}

func TestEnsureTrustCodexRefusesUnparsableConfig(t *testing.T) {
	e := newEnv(t)
	orig := "model = \"gpt\n[broken"
	writeFile(t, e.codexConfig, orig, 0o600)
	if err := e.m.EnsureTrust(e.ctx, KindCodex, trustSlot); err == nil {
		t.Fatal("unparsable config: want error")
	}
	if got := readFile(t, e.codexConfig); got != orig {
		t.Fatalf("unparsable config was rewritten:\n%s", got)
	}
	// An inline projects table cannot take a [projects."…"] header: refuse
	// instead of writing a file Codex could not load.
	orig = "projects = { \"/a\" = { trust_level = \"trusted\" } }\n"
	writeFile(t, e.codexConfig, orig, 0o600)
	if err := e.m.EnsureTrust(e.ctx, KindCodex, trustSlot); err == nil {
		t.Fatal("inline projects table: want error")
	}
	if got := readFile(t, e.codexConfig); got != orig {
		t.Fatalf("config was rewritten:\n%s", got)
	}
}

func TestEnsureTrustWritesThroughSymlink(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(t.TempDir(), "dotfiles", "codex.toml")
	writeFile(t, target, "model = \"gpt\"\n", 0o644)
	if err := os.MkdirAll(filepath.Dir(e.codexConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.codexConfig); err != nil {
		t.Fatal(err)
	}
	if err := e.m.EnsureTrust(e.ctx, KindCodex, trustSlot); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(e.codexConfig); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the config symlink was replaced: %v %v", st, err)
	}
	if got := readFile(t, target); !strings.Contains(got, "[projects.\""+trustSlot+"\"]") || fileMode(t, target) != 0o644 {
		t.Fatalf("target = %q (mode %o)", got, fileMode(t, target))
	}
}

const claudeIndented = `{
  "numStartups": 42,
  "theme": "dark",
  "projects": {
    "/Users/x/Projects/other": {
      "allowedTools": [],
      "hasTrustDialogAccepted": true
    },
    "` + trustSlot + `": {
      "allowedTools": [
        "Bash(git status)"
      ],
      "mcpServers": {},
      "hasTrustDialogAccepted": false,
      "lastCost": 1.25
    }
  },
  "oauthAccount": {
    "emailAddress": "a<b>@example.com"
  },
  "zzz": null
}`

func TestEnsureTrustClaudeMergesIndented(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.claudeConfig, claudeIndented, 0o600)
	if err := e.m.EnsureTrust(e.ctx, KindClaude, trustSlot); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(claudeIndented, `"hasTrustDialogAccepted": false`, `"hasTrustDialogAccepted": true`, 1)
	if got := readFile(t, e.claudeConfig); got != want {
		t.Fatalf("config =\n%s\nwant\n%s", got, want)
	}
	if n := len(e.run.Calls); n != 0 {
		t.Fatalf("claude trust ran commands: %v", e.run.Calls)
	}
	if logs := e.logs.all(); len(logs) != 1 || !strings.Contains(logs[0], "claude") {
		t.Fatalf("logs = %q", logs)
	}
	// Idempotent.
	if err := e.m.EnsureTrust(e.ctx, KindClaude, trustSlot); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, e.claudeConfig); got != want || len(e.logs.all()) != 1 {
		t.Fatalf("second call changed the config or logged:\n%s", got)
	}
}

func TestEnsureTrustClaudeCompactAddsProject(t *testing.T) {
	e := newEnv(t)
	orig := `{"theme":"dark","projects":{"/a":{"allowedTools":[]}},"tipsHistory":{"x":1}}` + "\n"
	writeFile(t, e.claudeConfig, orig, 0o644)
	if err := e.m.EnsureTrust(e.ctx, KindClaude, trustSlot); err != nil {
		t.Fatal(err)
	}
	want := `{"theme":"dark","projects":{"/a":{"allowedTools":[]},"` + trustSlot + `":{"allowedTools":[],"hasTrustDialogAccepted":true}},"tipsHistory":{"x":1}}` + "\n"
	if got := readFile(t, e.claudeConfig); got != want {
		t.Fatalf("config =\n%s\nwant\n%s", got, want)
	}
	if m := fileMode(t, e.claudeConfig); m != 0o644 {
		t.Fatalf("mode = %o, want 644", m)
	}

	// No projects key at all: one is appended.
	writeFile(t, e.claudeConfig, `{"theme":"dark"}`, 0o600)
	if err := e.m.EnsureTrust(e.ctx, KindClaude, trustSlot); err != nil {
		t.Fatal(err)
	}
	want = `{"theme":"dark","projects":{"` + trustSlot + `":{"allowedTools":[],"hasTrustDialogAccepted":true}}}`
	if got := readFile(t, e.claudeConfig); got != want {
		t.Fatalf("config =\n%s\nwant\n%s", got, want)
	}
}

func TestEnsureTrustClaudeMissingOrBrokenConfig(t *testing.T) {
	e := newEnv(t)
	if err := e.m.EnsureTrust(e.ctx, KindClaude, trustSlot); err != nil {
		t.Fatalf("missing config: %v", err)
	}
	if _, err := os.Stat(e.claudeConfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing ~/.claude.json must not be created: %v", err)
	}
	for _, orig := range []string{`{"projects": [1]}`, `{"projects": {}} trailing`, `[1]`, ``} {
		writeFile(t, e.claudeConfig, orig, 0o600)
		if err := e.m.EnsureTrust(e.ctx, KindClaude, trustSlot); err == nil {
			t.Fatalf("%q: want error", orig)
		}
		if got := readFile(t, e.claudeConfig); got != orig {
			t.Fatalf("%q was rewritten to %q", orig, got)
		}
	}
}

func TestEnsureTrustArguments(t *testing.T) {
	e := newEnv(t)
	if err := e.m.EnsureTrust(e.ctx, KindShell, trustSlot); err != nil {
		t.Fatalf("shell: %v", err)
	}
	// Other kinds (droid, omp, user-declared) have no known trust store: a no-op.
	for _, kind := range []string{KindDroid, KindOMP, "gemini", ""} {
		if err := e.m.EnsureTrust(e.ctx, kind, "relative/dir"); err != nil {
			t.Fatalf("%q: %v", kind, err)
		}
	}
	if logs := e.logs.all(); len(logs) != 0 {
		t.Fatalf("logs = %q", logs)
	}
	if err := e.m.EnsureTrust(e.ctx, KindCodex, "relative/dir"); err == nil {
		t.Fatal("relative dir: want error")
	}
	// Without configured paths a test binary never touches the real configs.
	m := New(Deps{Herdr: e.h, Store: e.st, Runner: e.run, Config: e.cfg})
	for _, kind := range []string{KindCodex, KindClaude} {
		if err := m.EnsureTrust(e.ctx, kind, trustSlot); err != nil {
			t.Fatalf("%s default paths: %v", kind, err)
		}
	}
	if m.codexConfigPath() != "" || m.claudeConfigPath() != "" {
		t.Fatal("default config paths resolved inside a test binary")
	}
	if n := len(e.run.CallsWithPrefix("git")); n != 0 {
		t.Fatalf("git ran %d times without a config to edit", n)
	}
}

func TestStartAgentTrustsCwdBeforeStarting(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.claudeConfig, "{\n  \"projects\": {}\n}\n", 0o600)
	e.started()
	cwd := "/Users/x/Projects/talkable.review1"
	if got := readFile(t, e.codexConfig); got != "[projects.\""+cwd+"\"]\ntrust_level = \"trusted\"\n" {
		t.Fatalf("codex config = %q", got)
	}
	want := "{\n  \"projects\": {\n    \"" + cwd + "\": {\n      \"allowedTools\": [],\n      \"hasTrustDialogAccepted\": true\n    }\n  }\n}\n"
	if got := readFile(t, e.claudeConfig); got != want {
		t.Fatalf("claude config =\n%s\nwant\n%s", got, want)
	}
	if len(e.run.CallsWithPrefix("git", "-C", cwd)) != 1 {
		t.Fatalf("git root probe calls = %v", e.run.Calls)
	}

	// EnsureTrust runs before agent.start: the entry exists even when the
	// start then fails.
	e1 := newEnv(t)
	ws := e1.workspace()
	e1.h.errs["AgentStart"] = &herdr.Error{Method: "agent.start", Code: herdr.CodeInvalidRequest, Message: "boom"}
	if err := e1.m.StartAgent(e1.ctx, e1.pr, e1.spec(RoleJudge), ws.Panes[RoleJudge], ""); err == nil {
		t.Fatal("want the start error")
	}
	if got := readFile(t, e1.codexConfig); !strings.Contains(got, cwd) {
		t.Fatalf("codex config after a failed start = %q", got)
	}

	// A config EnsureTrust cannot edit does not stop the start.
	e2 := newEnv(t)
	writeFile(t, e2.codexConfig, "[broken", 0o600)
	e2.started()
	if s := e2.session(RoleJudge); s.State != "live" {
		t.Fatalf("judge session = %+v", s)
	}
	if logs := strings.Join(e2.logs.all(), "\n"); !strings.Contains(logs, "trust dialog fallback still applies") {
		t.Fatalf("logs = %s", logs)
	}
}
