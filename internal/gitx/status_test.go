package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// DiffHead reads the work tree's changes of the paths against HEAD as a
// binary patch, with a/ and b/ prefixes whatever the config says and no
// external diff or textconv; a cut output is an error, never part of a
// patch, and no path is refused.
func TestDiffHeadArgv(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule("diff --git a/Gemfile.lock b/Gemfile.lock\n", "git", "-C", slot, "diff"))
	got, err := c.DiffHead(ctx, slot, "Gemfile.lock", "a b.rb")
	if err != nil || string(got) != "diff --git a/Gemfile.lock b/Gemfile.lock\n" {
		t.Fatalf("DiffHead = %q, %v", got, err)
	}
	cmd := wantCall(t, f, 0, false, "git", "-C", slot, "diff", "--binary", "--no-color", "--no-ext-diff", "--no-textconv",
		"--src-prefix=a/", "--dst-prefix=b/", "HEAD", "--", ":(top,literal)Gemfile.lock", ":(top,literal)a b.rb")
	if cmd.Env["GIT_OPTIONAL_LOCKS"] != "0" {
		t.Errorf("a diff must not take the index lock, env=%v", cmd.Env)
	}
	c, _ = newFake(execx.Rule{Prefix: []string{"git"}, Result: execx.Result{Stdout: []byte("diff --git"), Truncated: true}})
	if _, err := c.DiffHead(ctx, slot, "Gemfile.lock"); err == nil {
		t.Error("a cut patch was taken")
	}
	if _, err := c.DiffHead(ctx, slot); err == nil {
		t.Error("a diff of no path was taken")
	}
}

// The patch DiffHead reads puts the changes back: staged and unstaged
// changes, a deletion and a binary file, applied with `git apply` once
// the paths are back as HEAD has them, are the same changes again.
func TestRealDiffHeadIsAPatchThatPutsTheChangesBack(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.commit(fx.clone, "Gemfile.lock", "GEM\n  specs:\n", "lock")
	fx.commit(fx.clone, "logo.bin", "\x00\x01\x02", "logo")
	fx.write(fx.clone, "Gemfile.lock", "GEM\n  specs:\n    rake (13.3.0)\n")
	fx.write(fx.clone, "logo.bin", "\x00\x01\x02\x03\xff")
	fx.git(fx.clone, "add", "--", "logo.bin")
	fx.git(fx.clone, "rm", "--quiet", "README.md")
	paths := []string{"Gemfile.lock", "README.md", "logo.bin"}
	patch, err := fx.c.DiffHead(ctx, fx.clone, paths...)
	if err != nil || !strings.Contains(string(patch), "GIT binary patch") {
		t.Fatalf("DiffHead = %q, %v", patch, err)
	}
	if err := fx.c.RestoreTracked(ctx, fx.clone, paths...); err != nil {
		t.Fatal(err)
	}
	if st := fx.git(fx.clone, "status", "--porcelain"); st != "" {
		t.Fatalf("status after the restore:\n%s", st)
	}
	file := filepath.Join(t.TempDir(), "left.patch")
	if err := os.WriteFile(file, patch, 0o600); err != nil {
		t.Fatal(err)
	}
	fx.git(fx.clone, "apply", file)
	if again, err := fx.c.DiffHead(ctx, fx.clone, paths...); err != nil || string(again) != string(patch) {
		t.Fatalf("the changes after the patch:\n%s\nwant:\n%s (%v)", again, patch, err)
	}
	if b, _ := os.ReadFile(filepath.Join(fx.clone, "logo.bin")); string(b) != "\x00\x01\x02\x03\xff" {
		t.Fatalf("logo.bin = %q", b)
	}
}

// UntrackedFiles lists the untracked files git does not ignore under each
// path, or in the whole tree, one by one: a wholly untracked directory,
// which git status names once, as each of its files, and an ignored file in
// it not at all.
func TestUntrackedFilesListsANewDirectoryFileByFile(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.commit(fx.clone, ".gitignore", "*.log\n", "ignore logs")
	fx.write(fx.clone, "probe/a_spec.rb", "a\n")
	fx.write(fx.clone, "probe/deep/b spec.rb", "b\n")
	fx.write(fx.clone, "probe/run.log", "ignored\n")
	fx.write(fx.clone, "c.txt", "c\n")
	fx.write(fx.clone, "other.txt", "not asked for\n")
	got, err := fx.c.UntrackedFiles(ctx, fx.clone, "probe/", "c.txt")
	if want := []string{"c.txt", "probe/a_spec.rb", "probe/deep/b spec.rb"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("UntrackedFiles = %q, %v; want %q", got, err, want)
	}
	if got, err := fx.c.UntrackedFiles(ctx, fx.clone, "README.md"); err != nil || len(got) != 0 {
		t.Fatalf("UntrackedFiles of a tracked file = %q, %v; want none", got, err)
	}
	got, err = fx.c.UntrackedFiles(ctx, fx.clone)
	if want := []string{"c.txt", "other.txt", "probe/a_spec.rb", "probe/deep/b spec.rb"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("UntrackedFiles of the whole tree = %q, %v; want %q", got, err, want)
	}
	if _, err := fx.c.UntrackedFiles(ctx, fx.clone, ""); err == nil {
		t.Fatal("an empty path was taken")
	}
}
