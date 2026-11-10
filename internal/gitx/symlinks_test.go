package gitx

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
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
