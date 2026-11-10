package gitx

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// Tree lists, with a commit's links, the paths of its regular files the
// caller keeps, from the same ls-tree; Blobs reads files of the commit by
// path in one cat-file of "<rev>:<path>", leaving out a path the commit
// lacks or that names a directory (a link gives its text), and refuses a
// path the batch cannot ask; a cut output is ErrTreeTooLarge.
func TestTreeAndBlobsReadACommitsFiles(t *testing.T) {
	ctx := context.Background()
	c, _ := newFake(execx.Rule{Prefix: []string{"git", "-C", slot, "cat-file"}, Result: execx.Result{Truncated: true}})
	if _, err := c.Blobs(ctx, slot, sha1, []string{"CLAUDE.md"}); !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("a cut output: %v, want ErrTreeTooLarge", err)
	}
	if _, err := c.Blobs(ctx, slot, sha1, []string{"a\nb"}); err == nil {
		t.Fatal("a path with a line feed must be refused")
	}

	fx := newFixture(t)
	dir := fx.origin
	fx.write(dir, "docs/guide.md", "@notes.md\n")
	fx.write(dir, "docs/odd name.md", "odd\n")
	fx.write(dir, "app/x.rb", "x\n")
	if err := os.Symlink("docs/guide.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "Add docs")
	head := fx.git(dir, "rev-parse", "HEAD")
	tree, err := fx.c.Tree(ctx, dir, head, func(p string) bool { return strings.HasSuffix(p, ".md") })
	if want := []string{"README.md", "docs/guide.md", "docs/odd name.md"}; err != nil || !slices.Equal(tree.Files, want) ||
		!maps.Equal(tree.Links, map[string]string{"CLAUDE.md": "docs/guide.md"}) {
		t.Fatalf("Tree = %+v, %v; want the files %q and the link", tree, err, want)
	}
	got, err := fx.c.Blobs(ctx, dir, head, []string{"docs/guide.md", "docs/odd name.md", "docs", "missing.md", "CLAUDE.md"})
	want := map[string]string{"docs/guide.md": "@notes.md\n", "docs/odd name.md": "odd\n", "CLAUDE.md": "docs/guide.md"}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("Blobs = %q, %v; want %q", got, err, want)
	}
}

// TrackedFiles gives, with the index's links the paths name, its regular
// files there, each once.
func TestTrackedFilesListsTheIndexsFilesAndLinks(t *testing.T) {
	ctx := context.Background()
	c, _ := newFake(outRule("100644 1111111111111111111111111111111111111111 0\tCLAUDE.md\x00"+
		"120000 2222222222222222222222222222222222222222 0\tapp/CLAUDE.md\x00"+
		"100755 3333333333333333333333333333333333333333 0\t.claude/rules/x.md\x00", "git", "-C", slot, "ls-files"))
	links, files, err := c.TrackedFiles(ctx, slot, AnyDir+"CLAUDE.md", AnyDir+".claude"+AnyDirTree)
	if err != nil || !slices.Equal(links, []string{"app/CLAUDE.md"}) || !slices.Equal(files, []string{"CLAUDE.md", ".claude/rules/x.md"}) {
		t.Fatalf("TrackedFiles = %q, %q, %v", links, files, err)
	}
}
