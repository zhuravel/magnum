package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// countingRunner records the directories git was run in.
type countingRunner struct {
	inner execx.Runner
	mu    sync.Mutex
	dirs  []string
}

func (r *countingRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	r.mu.Lock()
	if len(c.Args) > 1 && c.Args[0] == "-C" {
		r.dirs = append(r.dirs, c.Args[1])
	}
	r.mu.Unlock()
	return r.inner.Run(ctx, c)
}

func (r *countingRunner) callsIn(dir string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, d := range r.dirs {
		if d == dir {
			n++
		}
	}
	return n
}

func (r *countingRunner) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dirs)
}

// cloneRoot builds a ~/Projects-like directory of git repositories (origin
// URL per directory; "" = a plain folder that is not a repository).
func cloneRoot(t *testing.T, repos map[string]string) (string, *countingRunner) {
	t.Helper()
	r := realEnv(t)
	if _, err := r.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"--version"}}); err != nil {
		t.Skipf("git not available: %v", err)
	}
	root := t.TempDir()
	for dir, origin := range repos {
		p := filepath.Join(root, dir)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if origin == "" {
			if err := os.WriteFile(filepath.Join(p, "README"), []byte("not a repo\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		for _, args := range [][]string{{"init", "--quiet"}, {"remote", "add", "origin", origin}} {
			if _, err := r.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", p}, args...)}); err != nil {
				t.Fatalf("git %v in %s: %v", args, p, err)
			}
		}
	}
	return root, &countingRunner{inner: r}
}

func TestFindCloneLayouts(t *testing.T) {
	root, rec := cloneRoot(t, map[string]string{
		"widgets":                         "", // a folder named like the repo, not a clone
		"zhuravel-widgets":                "git@github.com:zhuravel/widgets.git",
		"zhuravel":                        "https://github.com/zhuravel/zhuravel",
		"stalwart":                        "git@github.com:zhuravel/other.git", // same name, other repository
		"zhuravel_stalwart":               "https://github.com/zhuravel/stalwart.git",
		"infra":                           "ssh://git@github.com/ZhuraVEL/Infrastructure.git",
		"zhuravel-deployer__worktrees":    "git@github.com:zhuravel/deployer.git", // skipped by name
		"misc":                            "git@github.com:someone/else.git",
		"widget":                          "git@gitlab.com:zhuravel/widget.git", // not GitHub
		"zhuravel-widgets-old-fork-notes": "",
	})
	c := New(rec)
	ctx := context.Background()
	cases := []struct {
		owner, name, want string
	}{
		{"zhuravel", "widgets", "zhuravel-widgets"},
		{"zhuravel", "zhuravel", "zhuravel"},
		{"zhuravel", "stalwart", "zhuravel_stalwart"},
		{"zhuravel", "infrastructure", "infra"}, // found by the scan, case-insensitive
	}
	for _, tc := range cases {
		got, err := c.FindClone(ctx, root, tc.owner, tc.name)
		if err != nil || got != filepath.Join(root, tc.want) {
			t.Errorf("FindClone(%s/%s) = %q, %v; want %s", tc.owner, tc.name, got, err, tc.want)
		}
	}
	for _, name := range []string{"deployer", "widget", "missing"} {
		got, err := c.FindClone(ctx, root, "zhuravel", name)
		if !errors.Is(err, ErrNoClone) || got != "" {
			t.Errorf("FindClone(zhuravel/%s) = %q, %v; want ErrNoClone", name, got, err)
		}
	}
	// The plain folder never reaches git (no "not a git repository" noise).
	if n := rec.callsIn(filepath.Join(root, "widgets")); n != 0 {
		t.Errorf("git ran %d times in the non-repository folder", n)
	}
	if n := rec.callsIn(filepath.Join(root, "zhuravel-deployer__worktrees")); n != 0 {
		t.Errorf("git ran %d times in a __worktrees folder", n)
	}
}

func TestFindCloneCachesOrigins(t *testing.T) {
	root, rec := cloneRoot(t, map[string]string{
		"a": "git@github.com:o/a.git",
		"b": "git@github.com:o/b.git",
	})
	c := New(rec)
	ctx := context.Background()
	if got, err := c.FindClone(ctx, root, "o", "b"); err != nil || got != filepath.Join(root, "b") {
		t.Fatalf("FindClone = %q, %v", got, err)
	}
	if _, err := c.FindClone(ctx, root, "o", "missing"); !errors.Is(err, ErrNoClone) {
		t.Fatalf("err = %v", err)
	}
	first := rec.total() // every repository was asked once
	if _, err := c.FindClone(ctx, root, "o", "missing"); !errors.Is(err, ErrNoClone) {
		t.Fatalf("err = %v", err)
	}
	if got, err := c.FindClone(ctx, root, "o", "b"); err != nil || got != filepath.Join(root, "b") {
		t.Fatalf("FindClone again = %q, %v", got, err)
	}
	if rec.total() != first {
		t.Fatalf("unchanged repositories were asked again: %d git calls, want %d", rec.total(), first)
	}
	// Changing a remote invalidates that directory's entry.
	r := realEnv(t)
	if _, err := r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", filepath.Join(root, "a"), "remote", "set-url", "origin", "https://github.com/o/renamed-repository.git"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.FindClone(ctx, root, "o", "renamed-repository"); err != nil || got != filepath.Join(root, "a") {
		t.Fatalf("FindClone after set-url = %q, %v", got, err)
	}
}

func TestOriginMatches(t *testing.T) {
	match := []string{
		"https://github.com/o/r",
		"https://github.com/o/r.git",
		"https://github.com/o/r/",
		"https://github.com/o/r.git/",
		"http://github.com/o/r.git",
		"git@github.com:o/r",
		"git@github.com:o/r.git",
		"ssh://git@github.com/o/r.git",
		"ssh://git@github.com:22/o/r",
		"https://user:tok@github.com/o/r.git",
		"HTTPS://GitHub.COM/O/R.git", // mixed-case host, owner and name
		"git@github.com:O/r.git",
		"https://github.com/o/R",
	}
	for _, u := range match {
		if !originMatches(u, "o", "r") {
			t.Errorf("originMatches(%q, o, r) = false", u)
		}
	}
	reject := []string{
		"https://evilgithub.com/o/r.git",
		"https://example.com/github.com/o/r",
		"https://example.com/github.com/o/r.git",
		"git@notgithub.com:o/r.git",
		"git@evil.com:github.com/o/r.git",
		"ssh://git@evil.com/github.com/o/r.git",
		"https://github.com.evil.com/o/r",
		"https://github.com@evil.com/o/r",
		"https://gitlab.com/o/r",
		"https://github.com/o/r/tree/main", // a page, not a remote
		"https://github.com/o/r2",
		"https://github.com/xo/r",
		"https://github.com/o",
		"/local/github.com/o/r",
		"",
	}
	for _, u := range reject {
		if originMatches(u, "o", "r") {
			t.Errorf("originMatches(%q, o, r) = true", u)
		}
	}
}

func TestFindCloneRejectsForeignHosts(t *testing.T) {
	root, rec := cloneRoot(t, map[string]string{
		"r":      "https://evilgithub.com/o/r.git",
		"x":      "https://example.com/github.com/o/r",
		"y":      "git@notgithub.com:o/r.git",
		"z":      "ssh://git@evil.com/github.com/o/r.git",
		"mirror": "https://gitlab.com/o/r.git",
	})
	c := New(rec)
	if got, err := c.FindClone(context.Background(), root, "o", "r"); !errors.Is(err, ErrNoClone) || got != "" {
		t.Fatalf("FindClone = %q, %v; want ErrNoClone", got, err)
	}
	// The real one is still found next to the impostors, in any spelling.
	for dir, origin := range map[string]string{
		"https": "https://github.com/O/R.git",
		"scp":   "git@GitHub.com:o/r.git",
		"ssh":   "ssh://git@github.com/o/r/",
	} {
		root, rec := cloneRoot(t, map[string]string{"other": "https://evilgithub.com/o/r", dir: origin})
		if got, err := New(rec).FindClone(context.Background(), root, "o", "r"); err != nil || got != filepath.Join(root, dir) {
			t.Errorf("FindClone with origin %q = %q, %v", origin, got, err)
		}
	}
}

// A clone whose directory name differs in case from the repository name is
// found with its real path, on case-sensitive and case-insensitive
// filesystems alike, and is asked about once.
func TestFindCloneMixedCaseDirectory(t *testing.T) {
	root, rec := cloneRoot(t, map[string]string{"Widget": "https://github.com/o/widget.git"})
	c := New(rec)
	got, err := c.FindClone(context.Background(), root, "o", "widget")
	if err != nil {
		t.Fatalf("FindClone: %v", err)
	}
	want, err := os.Stat(filepath.Join(root, "Widget"))
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(got); err != nil || !os.SameFile(st, want) {
		t.Fatalf("FindClone = %q, not the Widget directory (%v)", got, err)
	}

	// A mismatch costs one git call, not one per spelling.
	root, rec = cloneRoot(t, map[string]string{"Widget": "https://github.com/o/other.git"})
	if _, err := New(rec).FindClone(context.Background(), root, "o", "widget"); !errors.Is(err, ErrNoClone) {
		t.Fatalf("err = %v", err)
	}
	if n := rec.total(); n != 1 {
		t.Fatalf("git ran %d times for one directory", n)
	}
}

func TestFindCloneAsksEachDirectoryOnce(t *testing.T) {
	root, rec := cloneRoot(t, map[string]string{"widget": "https://github.com/o/other.git"})
	if err := os.Symlink("widget", filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := New(rec).FindClone(context.Background(), root, "o", "widget"); !errors.Is(err, ErrNoClone) {
		t.Fatalf("err = %v", err)
	}
	if n := rec.total(); n != 1 {
		t.Fatalf("git ran %d times for a directory and its symlink", n)
	}
}

// Changing the remote through a linked worktree rewrites the shared config of
// the main repository; the worktree's own .git file does not change.
func TestFindCloneLinkedWorktreeOriginChange(t *testing.T) {
	r := realEnv(t)
	ctx := context.Background()
	if _, err := r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"--version"}}); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := func(dir string, args ...string) {
		t.Helper()
		if _, err := r.Run(ctx, execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)}); err != nil {
			t.Fatalf("git %v in %s: %v", args, dir, err)
		}
	}
	main := filepath.Join(t.TempDir(), "main") // outside the clone root
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	git(main, "init", "--quiet", "-b", "main")
	git(main, "commit", "--quiet", "--allow-empty", "-m", "c1")
	git(main, "remote", "add", "origin", "https://github.com/o/a.git")
	root := t.TempDir()
	linked := filepath.Join(root, "wt")
	git(main, "worktree", "add", "--quiet", "--detach", linked)
	if fi, err := os.Lstat(filepath.Join(linked, ".git")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("linked checkout must have a .git file: %v", err)
	}

	rec := &countingRunner{inner: r}
	c := New(rec)
	if got, err := c.FindClone(ctx, root, "o", "a"); err != nil || got != linked {
		t.Fatalf("FindClone(a) = %q, %v; want %s", got, err, linked)
	}
	first := rec.total()
	if got, err := c.FindClone(ctx, root, "o", "a"); err != nil || got != linked {
		t.Fatalf("FindClone(a) again = %q, %v", got, err)
	}
	if rec.total() != first {
		t.Fatalf("an unchanged linked checkout was asked again: %d git calls, want %d", rec.total(), first)
	}

	git(linked, "remote", "set-url", "origin", "https://github.com/o/renamed-repository.git")
	if got, err := c.FindClone(ctx, root, "o", "renamed-repository"); err != nil || got != linked {
		t.Fatalf("FindClone after set-url = %q, %v; want %s", got, err, linked)
	}
	if _, err := c.FindClone(ctx, root, "o", "a"); !errors.Is(err, ErrNoClone) {
		t.Fatalf("the old origin must be forgotten, got %v", err)
	}
}

func TestOriginStamps(t *testing.T) {
	base := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stampsOf := func(dir string) ([2]fileStamp, bool) {
		t.Helper()
		gitPath := filepath.Join(dir, ".git")
		st, err := os.Stat(gitPath)
		if err != nil {
			t.Fatal(err)
		}
		return originStamps(gitPath, st)
	}

	// A clone is keyed on .git/config.
	clone := filepath.Join(base, "clone")
	write(filepath.Join(clone, ".git", "config"), "[remote \"origin\"]\n")
	a, ok := stampsOf(clone)
	if !ok {
		t.Fatal("a clone is cacheable")
	}
	write(filepath.Join(clone, ".git", "config"), "[remote \"origin\"]\n\turl = x\n")
	if b, _ := stampsOf(clone); a == b {
		t.Error("a changed config must change the stamps")
	}

	// A linked checkout is keyed on the common directory's config, found
	// through the gitdir file (absolute or relative) and its commondir file.
	common := filepath.Join(base, "common")
	write(filepath.Join(common, "config"), "[core]\n")
	write(filepath.Join(common, "worktrees", "wt", "commondir"), "../..\n")
	for name, target := range map[string]string{
		"abs": filepath.Join(common, "worktrees", "wt"),
		"rel": "../common/worktrees/wt",
	} {
		co := filepath.Join(base, "co-"+name)
		write(filepath.Join(co, ".git"), "gitdir: "+target+"\n")
		before, ok := stampsOf(co)
		if !ok {
			t.Fatalf("%s: a linked checkout is cacheable", name)
		}
		write(filepath.Join(common, "config"), "[core]\n\tbare = false\n"+name)
		if after, _ := stampsOf(co); before == after {
			t.Errorf("%s: a change of the shared config must change the stamps", name)
		}
	}

	// A submodule-style gitdir (no commondir) is its own common directory.
	write(filepath.Join(base, "mod", "config"), "[core]\n")
	write(filepath.Join(base, "sub", ".git"), "gitdir: ../mod\n")
	if _, ok := stampsOf(filepath.Join(base, "sub")); !ok {
		t.Error("a submodule checkout is cacheable")
	}

	// Anything unresolvable is not cacheable: git is asked every time.
	write(filepath.Join(base, "broken", ".git"), "gitdir: ../nowhere\n")
	write(filepath.Join(base, "junk", ".git"), "not a gitdir file\n")
	for _, d := range []string{"broken", "junk"} {
		if _, ok := stampsOf(filepath.Join(base, d)); ok {
			t.Errorf("%s: an unresolvable .git file must not be cached", d)
		}
	}
	c, f := newFake(outRule("git@github.com:o/r.git\n", "git"))
	for i := 0; i < 2; i++ {
		if got, err := c.FindClone(context.Background(), base, "o", "nothing"); !errors.Is(err, ErrNoClone) {
			t.Fatalf("FindClone = %q, %v; want ErrNoClone", got, err)
		}
	}
	asked := func(d string) int { return len(f.CallsWithPrefix("git", "-C", filepath.Join(base, d))) }
	if asked("clone") != 1 || asked("broken") != 2 || asked("junk") != 2 {
		t.Errorf("git asked clone=%d broken=%d junk=%d times over two scans, want 1, 2, 2", asked("clone"), asked("broken"), asked("junk"))
	}
}

func TestFindCloneArguments(t *testing.T) {
	c, f := newFake()
	ctx := context.Background()
	for _, tc := range [][3]string{{"/r", "", "x"}, {"/r", "o", ""}, {"/r", "o/x", "y"}, {"", "o", "x"}, {"/r", "o", ".."}, {"/r", ".", "x"}, {"/r", "o", `a\b`}} {
		if _, err := c.FindClone(ctx, tc[0], tc[1], tc[2]); err == nil || errors.Is(err, ErrNoClone) {
			t.Errorf("FindClone(%q, %q, %q) = %v, want an argument error", tc[0], tc[1], tc[2], err)
		}
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := c.FindClone(ctx, missing, "o", "x"); !errors.Is(err, ErrNoClone) || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing root: %v", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("git ran %d times", len(f.Calls))
	}
}

func TestFindCloneHonorsContext(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "r", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := newFake(execx.Rule{Prefix: []string{"git"}, Err: ctx.Err()})
	got, err := c.FindClone(ctx, root, "o", "r")
	if !errors.Is(err, context.Canceled) || got != "" {
		t.Fatalf("FindClone = %q, %v; want context.Canceled and no path", got, err)
	}
}

func TestIsRepo(t *testing.T) {
	dir := t.TempDir()
	if IsRepo(dir) {
		t.Fatal("empty dir is not a repo")
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsRepo(dir) {
		t.Fatal("a .git file (linked worktree) counts")
	}
}
