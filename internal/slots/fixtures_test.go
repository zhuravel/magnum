package slots

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitGuard bounds every real git command the tests run (directly or through
// the code under test). A git on these tiny repositories takes milliseconds;
// one that hangs fails its test quickly instead of the whole package hitting
// the go test timeout.
const gitGuard = 30 * time.Second

// hermeticGitEnv pins git's configuration and identity for the whole test
// binary, the code under test included. Without an identity every git that
// writes a reflog (worktree add, switch, branch, fetch into refs) derives a
// default e-mail from the host name, and that lookup can stall for seconds
// on mDNS.
var hermeticGitEnv = map[string]string{
	"GIT_CONFIG_GLOBAL":   "/dev/null",
	"GIT_CONFIG_NOSYSTEM": "1",
	"GIT_AUTHOR_NAME":     "t",
	"GIT_AUTHOR_EMAIL":    "t@example.com",
	"GIT_COMMITTER_NAME":  "t",
	"GIT_COMMITTER_EMAIL": "t@example.com",
	"GIT_TERMINAL_PROMPT": "0",
}

// leakedGitEnv would make git use another identity source (EMAIL) or
// redirect it away from the test repositories (set when the tests run from a
// git hook, for example).
var leakedGitEnv = []string{"EMAIL", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR"}

// testHome is HOME (and XDG_CONFIG_HOME's parent) for the whole test binary:
// git, paths.Expand and os.UserHomeDir never see the real home directory.
var testHome string

func TestMain(m *testing.M) {
	for _, k := range leakedGitEnv {
		os.Unsetenv(k)
	}
	for k, v := range hermeticGitEnv {
		os.Setenv(k, v)
	}
	home, err := os.MkdirTemp("", "magnum-slots-home-")
	if err == nil {
		home, err = filepath.EvalSymlinks(home)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "slots: test home:", err)
		os.Exit(1)
	}
	testHome = home
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	code := m.Run()
	removeFixtures()
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintln(os.Stderr, "slots: test home:", err)
	}
	os.Exit(code)
}

// repoFixture is one fixture repository family: a bare origin with master,
// refs/pull/7/head (changes db/schema.rb and Gemfile.lock) and
// refs/pull/8/head (README only), the scratch clone that pushed them, and a
// template main clone of the origin.
type repoFixture struct {
	origin string // shared and read-only: tests may fetch and clone, never push
	work   string // template scratch clone (origin remote = origin)
	main   string // template main clone (origin remote = origin)

	master, pr7, pr8 string
}

// fixtureSet holds the repositories every test starts from. They are built
// once per test binary (about twenty git processes each) and copied into
// each test's temp dir, which costs no git process at all.
type fixtureSet struct {
	root     string
	talkable repoFixture // main carries a .mise.local.toml for CopyFiles
	widget   repoFixture // the per-PR repository
}

var (
	fixtureOnce sync.Once
	fixtureVal  *fixtureSet
	fixtureErr  error
)

// fixtures returns the shared fixture repositories, building them on first use.
func fixtures(t testing.TB) *fixtureSet {
	t.Helper()
	fixtureOnce.Do(func() { fixtureVal, fixtureErr = buildFixtures() })
	if fixtureErr != nil {
		t.Fatalf("build fixture repositories: %v", fixtureErr)
	}
	return fixtureVal
}

const miseLocalFixture = "[tools]\nnode = \"22\"\n\n[env]\nFAKE_AWS = \"1\"\nGITHUB_PERSONAL_ACCESS_TOKEN = \"ghp_abcdefghijklmnopqrstuvwxyz0123456789\"\n"

func buildFixtures() (_ *fixtureSet, err error) {
	root, err := os.MkdirTemp("", "magnum-slots-fixtures-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(root)
		}
	}()
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, err
	}
	set := &fixtureSet{root: root}
	if set.talkable, err = buildRepoFixture(filepath.Join(root, "talkable"), "remotes/talkable/talkable.git"); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(set.talkable.main, ".mise.local.toml"), []byte(miseLocalFixture), 0o644); err != nil {
		return nil, err
	}
	if set.widget, err = buildRepoFixture(filepath.Join(root, "widget"), "remotes/zhuravel/widget.git"); err != nil {
		return nil, err
	}
	// Everything under root is read-only from here on: a test that tries to
	// push to a shared origin fails instead of leaking into later tests.
	return set, chmodTree(root, false)
}

// buildRepoFixture creates <dir>/<rel> (bare), <dir>/work and <dir>/main.
func buildRepoFixture(dir, rel string) (f repoFixture, err error) {
	f = repoFixture{origin: filepath.Join(dir, rel), work: filepath.Join(dir, "work"), main: filepath.Join(dir, "main")}
	git := func(repo string, args ...string) string {
		if err != nil {
			return ""
		}
		var out string
		out, err = gitRun(repo, args...)
		return out
	}
	write := func(rel, content string) {
		if err != nil {
			return
		}
		path := filepath.Join(f.work, rel)
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			err = os.WriteFile(path, []byte(content), 0o644)
		}
	}
	for _, d := range []string{f.origin, f.work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return f, err
		}
	}
	// --template= skips the sample hooks: fewer files to copy per test.
	git(f.origin, "init", "--quiet", "--bare", "--template=", "-b", "master")
	git(f.work, "init", "--quiet", "--template=", "-b", "master")
	write("README", "hello\n")
	write(".gitignore", ".mise.local.toml\ntmp/\n")
	write("Gemfile.lock", "GEM v1\n")
	write("pnpm-lock.yaml", "lock v1\n")
	write(filepath.Join("db", "schema.rb"), schemaRB(schemaV1))
	git(f.work, "add", "-A")
	git(f.work, "commit", "--quiet", "-m", "init")
	f.master = git(f.work, "rev-parse", "HEAD")
	git(f.work, "remote", "add", "origin", f.origin)
	git(f.work, "push", "--quiet", "origin", "master")

	git(f.work, "switch", "--quiet", "-c", "pr7")
	write(filepath.Join("db", "schema.rb"), schemaRB(schemaV2))
	write("Gemfile.lock", "GEM v2\n")
	git(f.work, "commit", "--quiet", "-am", "pr7")
	f.pr7 = git(f.work, "rev-parse", "HEAD")
	git(f.work, "push", "--quiet", "origin", "HEAD:refs/pull/7/head")

	git(f.work, "switch", "--quiet", "-C", "pr8", f.master)
	write("README", "hello pr8\n")
	git(f.work, "commit", "--quiet", "-am", "pr8")
	f.pr8 = git(f.work, "rev-parse", "HEAD")
	git(f.work, "push", "--quiet", "origin", "HEAD:refs/pull/8/head")

	git(dir, "clone", "--quiet", "--template=", f.origin, f.main)
	return f, err
}

// gitRun runs git in dir under the hermetic environment and gitGuard.
func gitRun(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitGuard)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (exceeded the %s test guard)", err, gitGuard)
		}
		return "", fmt.Errorf("git -C %s %s: %w\n%s", dir, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// copyRepo gives a test its own copy of a fixture repository at dst (which
// must not exist yet). Absolute paths inside the copy still name the shared
// fixtures (the main clones' origin remote), which is what tests that only
// fetch want; repointOrigin rewires a copy to a private origin.
func copyRepo(t testing.TB, src, dst string) {
	t.Helper()
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
}

// copyTree copies src to dst in-process (no cp, no git). Copies are owner
// writable even though the fixtures are read-only.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		return fmt.Errorf("copy fixture %s: %w", src, err)
	}
	return nil
}

// repointOrigin rewrites the origin remote of the non-bare repository at repo
// from one local path to another, without running git.
func repointOrigin(t testing.TB, repo, from, to string) {
	t.Helper()
	cfg := filepath.Join(repo, ".git", "config")
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	old := []byte("url = " + from + "\n")
	if !bytes.Contains(b, old) {
		t.Fatalf("%s: no remote url %s", cfg, from)
	}
	if err := os.WriteFile(cfg, bytes.ReplaceAll(b, old, []byte("url = "+to+"\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

// chmodTree makes every directory and file under root writable (by the
// owner) or read-only.
func chmodTree(root string, writable bool) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if writable {
			mode |= 0o200
		} else {
			mode &^= 0o222
		}
		return os.Chmod(path, mode)
	})
}

func removeFixtures() {
	if fixtureVal == nil {
		return
	}
	if err := chmodTree(fixtureVal.root, true); err != nil {
		fmt.Fprintln(os.Stderr, "slots: fixtures:", err)
	}
	if err := os.RemoveAll(fixtureVal.root); err != nil {
		fmt.Fprintln(os.Stderr, "slots: fixtures:", err)
	}
}
