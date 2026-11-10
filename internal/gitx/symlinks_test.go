package gitx

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// Symlinks reads every symbolic link of a commit's tree from the object
// store, whatever the work tree holds: its path from the top and its text,
// at any depth, in one ls-tree and one cat-file of the links' blobs (none
// for a tree without links). A tree listing cut at execx.MaxOutput is
// ErrTreeTooLarge, never a list that may leave a link out; a commit the
// clone lacks fails.
func TestSymlinksReadsEveryLinkOfACommitsTree(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(execx.Rule{Prefix: []string{"git", "-C", slot, "ls-tree"}, Result: execx.Result{Truncated: true,
		Stdout: []byte("120000 blob 1111111111111111111111111111111111111111\tCLAUDE.md\x00")}})
	if _, err := c.Symlinks(ctx, slot, sha1); !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("a cut listing: %v, want ErrTreeTooLarge", err)
	}
	wantCall(t, f, 0, false, "git", "-C", slot, "ls-tree", "-r", "-z", "--full-tree", sha1)
	for _, bad := range []string{"", "a..b", sha1 + ":CLAUDE.md", "-x"} {
		if _, err := c.Symlinks(ctx, slot, bad); err == nil {
			t.Errorf("Symlinks(%q) must be refused", bad)
		}
	}

	fx := newFixture(t)
	dir := fx.origin
	plain := fx.commit(dir, "docs/guide.md", "the team's\n", "Add a guide")
	if got, err := fx.c.Symlinks(ctx, dir, plain); err != nil || len(got) != 0 {
		t.Fatalf("a tree without links: %q, %v", got, err)
	}
	links := map[string]string{
		"CLAUDE.md":                "docs/guide.md",
		"app/CLAUDE.md":            "../docs/app.md",
		".claude/skills":           "../.agents/skills",
		"lib/odd name\twith a tab": "/outside/x",
	}
	for name, text := range links {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(text, p); err != nil {
			t.Fatal(err)
		}
		fx.git(dir, "add", "--", name)
	}
	fx.git(dir, "commit", "--quiet", "-m", "Add links")
	head := fx.git(dir, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	got, err := fx.c.Symlinks(ctx, dir, head)
	if err != nil || !maps.Equal(got, links) {
		t.Fatalf("Symlinks = %q, %v; want %q", got, err, links)
	}
	if _, err := fx.c.Symlinks(ctx, dir, "89abcdef0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("a commit the clone lacks must fail")
	}
}

// TrackedSymlinks lists the links of a checkout's index that the paths
// name, in any directory for an AnyDir path, from one ls-files: an
// instruction file below the root that links elsewhere, a link under a
// .claude/ below it; a tracked file that is no link, and an untracked
// link, are left out.
func TestTrackedSymlinksListsTheIndexsLinksThePathsName(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule("100644 1111111111111111111111111111111111111111 0\tCLAUDE.md\x00"+
		"120000 2222222222222222222222222222222222222222 0\tapp/CLAUDE.md\x00", "git", "-C", slot, "ls-files"))
	got, err := c.TrackedSymlinks(ctx, slot, AnyDir+"CLAUDE.md", AnyDir+".claude"+AnyDirTree)
	if err != nil || !slices.Equal(got, []string{"app/CLAUDE.md"}) {
		t.Fatalf("TrackedSymlinks = %q, %v", got, err)
	}
	wantCall(t, f, 0, false, "git", "-C", slot, "ls-files", "-s", "-z", "--", ":(top,icase,glob)**/CLAUDE.md", ":(top,icase,glob)**/.claude/**")
	if _, err := c.TrackedSymlinks(ctx, slot); err == nil {
		t.Error("TrackedSymlinks without a path must be refused")
	}

	fx := newFixture(t)
	dir := fx.origin
	fx.commit(dir, "CLAUDE.md", "the team's\n", "Add the instructions")
	for name, text := range map[string]string{"app/CLAUDE.md": "../docs/app.md", "packages/web/.claude/skills": "../../../shared/skills",
		"lib/x": "../y"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(text, p); err != nil {
			t.Fatal(err)
		}
		fx.git(dir, "add", "--", name)
	}
	fx.git(dir, "commit", "--quiet", "-m", "Add links")
	if err := os.Symlink("../z", filepath.Join(dir, "lib", "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	got, err = fx.c.TrackedSymlinks(ctx, dir, "CLAUDE.md", AnyDir+"CLAUDE.md", AnyDir+".claude", AnyDir+".claude"+AnyDirTree)
	if want := []string{"app/CLAUDE.md", "packages/web/.claude/skills"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("TrackedSymlinks = %q, %v; want %q", got, err, want)
	}
}
