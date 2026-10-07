package gitx

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// mergeFixture adds to newFixture's origin a branch "ours" that rewrites
// README.md and changes app/a.rb's neighbour, a branch "clean" that adds
// another file, and a branch "clash" that rewrites README.md another way.
func mergeFixture(t *testing.T) (fx *fixture, ours, clean, clash string) {
	t.Helper()
	fx = newFixture(t)
	fx.git(fx.origin, "checkout", "--quiet", "-b", "ours", fx.base)
	ours = fx.commit(fx.origin, "README.md", "hello from ours\n", "ours")
	fx.git(fx.origin, "checkout", "--quiet", "-b", "clean", fx.base)
	clean = fx.commit(fx.origin, "lib/b.rb", "b = 1\n", "clean")
	fx.git(fx.origin, "checkout", "--quiet", "-b", "clash", fx.base)
	clash = fx.commit(fx.origin, "README.md", "hello from clash\n", "clash")
	fx.git(fx.origin, "checkout", "--quiet", "main")
	fx.git(fx.clone, "fetch", "--quiet", "origin", "ours", "clean", "clash")
	return fx, ours, clean, clash
}

func TestRealMergeTreeCommitsTheMergeWithoutTouchingTheWorkTree(t *testing.T) {
	fx, ours, clean, _ := mergeFixture(t)
	ctx := context.Background()
	headBefore := fx.git(fx.clone, "rev-parse", "HEAD")

	tree, conflicts, err := fx.c.MergeTree(ctx, fx.clone, ours, clean)
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("MergeTree = %q, %v, %v; want a clean merge", tree, conflicts, err)
	}
	commit, err := fx.c.CommitTree(ctx, fx.clone, tree, "magnum merge-check: #7 onto "+ours[:7], ours, clean)
	if err != nil {
		t.Fatal(err)
	}
	if got := fx.git(fx.clone, "rev-parse", commit+"^{tree}"); got != tree {
		t.Fatalf("the commit's tree is %s, want %s", got, tree)
	}
	if got := fx.git(fx.clone, "rev-list", "--parents", "-n", "1", commit); got != commit+" "+ours+" "+clean {
		t.Fatalf("parents: %q", got)
	}
	// Both sides' files are in the merged tree.
	if got := fx.git(fx.clone, "show", commit+":README.md"); got != "hello from ours" {
		t.Fatalf("README.md = %q", got)
	}
	if got := fx.git(fx.clone, "show", commit+":lib/b.rb"); got != "b = 1" {
		t.Fatalf("lib/b.rb = %q", got)
	}
	if got := fx.git(fx.clone, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD moved to %s", got)
	}
	if st := fx.status(fx.clone); st.Dirty() {
		t.Fatalf("the work tree changed: %+v", st)
	}

	ref := MergeCheckRefPrefix + "review1/tree"
	if err := fx.c.UpdateRef(ctx, fx.clone, ref, commit); err != nil {
		t.Fatal(err)
	}
	if got, err := fx.c.RevParse(ctx, fx.clone, ref); err != nil || got != commit {
		t.Fatalf("%s = %q, %v", ref, got, err)
	}
	// Under refs/magnum/, the merge commit is no unpushed work.
	fx.git(fx.clone, "switch", "--quiet", "--detach", commit)
	if n, err := fx.c.Unpushed(ctx, fx.clone); err != nil || n != 0 {
		t.Fatalf("Unpushed at the merge commit = %d, %v", n, err)
	}
	if err := fx.c.UpdateRefDelete(ctx, fx.clone, ref); err != nil {
		t.Fatal(err)
	}
}

func TestRealMergeTreeReportsAConflictAsAnAnswer(t *testing.T) {
	fx, ours, _, clash := mergeFixture(t)
	tree, conflicts, err := fx.c.MergeTree(context.Background(), fx.clone, ours, clash)
	if err != nil {
		t.Fatalf("a conflict is not an error: %v", err)
	}
	if !slices.Equal(conflicts, []string{"README.md"}) {
		t.Fatalf("conflicts = %q, want README.md", conflicts)
	}
	if tree == "" {
		t.Fatal("no tree id")
	}
}

func TestRealFetchIntoFetchesAPRHeadAndACommitByID(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	ref := MergeCheckRefPrefix + "review1/head"
	if got, err := fx.c.FetchInto(ctx, fx.clone, "refs/pull/7/head", ref); err != nil || got != fx.pr {
		t.Fatalf("FetchInto(PR 7) = %q, %v; want %s", got, err, fx.pr)
	}
	// The daemon's ref for the PR stays untouched.
	if _, err := fx.c.RevParse(ctx, fx.clone, PRRef(7)); err == nil {
		t.Fatalf("%s was written", PRRef(7))
	}
	fx.git(fx.origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	extra := fx.commit(fx.origin, "lib/c.rb", "c\n", "on main, not fetched yet")
	merged := MergeCheckRefPrefix + "review1/merged"
	if got, err := fx.c.FetchInto(ctx, fx.clone, extra, merged); err != nil || got != extra {
		t.Fatalf("FetchInto(%s) = %q, %v", extra[:7], got, err)
	}
}

func TestMergeCheckRefsStayInTheirNamespace(t *testing.T) {
	t.Parallel()
	c := New(nil) // every call below is refused before git runs
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	for _, ref := range []string{"refs/heads/main", PRRef(7), "refs/magnum/eval/x", MergeCheckRefPrefix, "refs/magnum/merge-check/../heads/x"} {
		if err := c.UpdateRef(ctx, "/repo", ref, sha); err == nil {
			t.Errorf("UpdateRef(%q) was allowed", ref)
		}
		if _, err := c.FetchInto(ctx, "/repo", "refs/pull/7/head", ref); err == nil {
			t.Errorf("FetchInto(→ %q) was allowed", ref)
		}
	}
	for _, src := range []string{"refs/pull/7/head:refs/heads/main", "+refs/pull/7/head", "main", "-x", "abc1234"} {
		if _, err := c.FetchInto(ctx, "/repo", src, MergeCheckRefPrefix+"review1/head"); err == nil {
			t.Errorf("FetchInto(%q) was allowed", src)
		}
	}
	if _, err := c.CommitTree(ctx, "/repo", "HEAD", "m", sha); err == nil {
		t.Error("CommitTree took a tree that is not an id")
	}
	if _, err := c.CommitTree(ctx, "/repo", sha, "m", "main"); err == nil {
		t.Error("CommitTree took a parent that is not an id")
	}
}
