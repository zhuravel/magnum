package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stagingConflict is the fixture's clone with a staging branch whose tip
// and PR 7's head both change app/a.rb's line (a conflict), the tip also
// README.md and PR 7 also db/schema.rb (no conflict): it returns the tip
// and the head.
func (fx *fixture) stagingConflict() (tip, head string) {
	fx.t.Helper()
	fx.git(fx.origin, "checkout", "--quiet", "-b", "staging-void", fx.base)
	fx.write(fx.origin, "app/a.rb", "a = 2\n")
	fx.write(fx.origin, "README.md", "hello staging\n")
	fx.git(fx.origin, "add", ".")
	fx.git(fx.origin, "commit", "--quiet", "-m", "staging moves")
	tip = fx.git(fx.origin, "rev-parse", "HEAD")
	fx.git(fx.origin, "checkout", "--quiet", "main")
	fx.fetchPR7()
	if err := fx.c.FetchBranch(context.Background(), fx.clone, "staging-void"); err != nil {
		fx.t.Fatal(err)
	}
	fx.git(fx.clone, "switch", "--quiet", "--detach", tip)
	return tip, fx.pr
}

var resolveWho = Signature{Name: "fixer-app[bot]", Email: "7+fixer-app[bot]@users.noreply.github.com"}

// A staging merge's resolution: MergeNoCommit leaves the merge in the work
// tree with HEAD on the tip and names its conflicts, MergeTreeWrite gives
// the same conflicts with merge-tree's own tree, and WriteTreeFrom makes
// that tree with the resolved path as the work tree holds it, whatever
// the work tree's index says and without touching it; WorkTreeDiff names
// what else the work tree changed, CommitTree makes the merge on that tree,
// Parents reads its parents in order and UpdateRef keeps it at a magnum ref.
func TestAStagingMergeIsResolvedInTheWorkTreeAndWrittenApartFromItsIndex(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	tip, head := fx.stagingConflict()

	conflicts, err := fx.c.MergeNoCommit(ctx, fx.clone, head, resolveWho)
	if err != nil || !slices.Equal(conflicts, []string{"app/a.rb"}) {
		t.Fatalf("MergeNoCommit = %v, %v; want the conflict in app/a.rb", conflicts, err)
	}
	if got := fx.git(fx.clone, "rev-parse", "HEAD"); got != tip {
		t.Errorf("HEAD = %s, want the tip %s: --no-commit commits nothing", got, tip)
	}
	if got := fx.git(fx.clone, "rev-parse", "MERGE_HEAD"); got != head {
		t.Errorf("MERGE_HEAD = %s, want the head %s", got, head)
	}
	mt, mtConflicts, err := fx.c.MergeTreeWrite(ctx, fx.clone, tip, head)
	if err != nil || !isOID(mt) || !slices.Equal(mtConflicts, conflicts) {
		t.Fatalf("MergeTreeWrite = %q, %v, %v; want a tree and app/a.rb", mt, mtConflicts, err)
	}
	if got := fx.git(fx.clone, "show", mt+":app/a.rb"); !strings.Contains(got, "<<<<<<<") {
		t.Errorf("merge-tree's app/a.rb = %q, want its conflict markers", got)
	}

	fx.write(fx.clone, "app/a.rb", "a = 2\nb = 1\n") // the resolution
	fx.write(fx.clone, "README.md", "residue\n")     // something else the work tree changed
	tree, err := fx.c.WriteTreeFrom(ctx, fx.clone, mt, []string{"app/a.rb"})
	if err != nil || !isOID(tree) {
		t.Fatalf("WriteTreeFrom = %q, %v", tree, err)
	}
	if got := fx.git(fx.clone, "show", tree+":app/a.rb"); got != "a = 2\nb = 1" {
		t.Errorf("the tree's app/a.rb = %q, want the resolution", got)
	}
	if got := fx.git(fx.clone, "show", tree+":README.md"); got != "hello staging" {
		t.Errorf("the tree's README.md = %q, want merge-tree's, not the work tree's", got)
	}
	if got := fx.git(fx.clone, "diff", "--name-only", "--diff-filter=U"); got != "app/a.rb" {
		t.Errorf("the work tree's index lists %q unmerged, want app/a.rb still: WriteTreeFrom must not touch it", got)
	}
	if got, err := fx.c.ChangedBetween(ctx, fx.clone, mt, tree); err != nil || !slices.Equal(got, []string{"app/a.rb"}) {
		t.Errorf("the tree differs from merge-tree's in %v, %v; want app/a.rb alone", got, err)
	}
	if got, err := fx.c.WorkTreeDiff(ctx, fx.clone, tree); err != nil || !slices.Equal(got, []string{"README.md"}) {
		t.Errorf("WorkTreeDiff = %v, %v; want README.md, which the tree does not hold", got, err)
	}

	merge, err := fx.c.CommitTree(ctx, fx.clone, tree, []string{tip, head}, "Merge branch 'pr7' into staging-void", resolveWho)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fx.c.Parents(ctx, fx.clone, merge); err != nil || !slices.Equal(got, []string{tip, head}) {
		t.Errorf("Parents = %v, %v; want [tip, head]", got, err)
	}
	if got, err := fx.c.Parents(ctx, fx.clone, tip); err != nil || len(got) != 1 || got[0] != fx.base {
		t.Errorf("the tip's Parents = %v, %v; want the base", got, err)
	}
	ref := "refs/magnum/staging-resolved/void-7-1"
	if err := fx.c.UpdateRef(ctx, fx.clone, ref, merge); err != nil {
		t.Fatal(err)
	}
	if got := fx.git(fx.clone, "rev-parse", ref); got != merge {
		t.Errorf("%s = %s, want the merge", ref, got)
	}
	for _, bad := range []string{"refs/heads/staging-void", "refs/magnum/", "HEAD"} {
		if err := fx.c.UpdateRef(ctx, fx.clone, bad, merge); err == nil {
			t.Errorf("UpdateRef(%s) was let through", bad)
		}
	}
	if err := fx.c.UpdateRef(ctx, fx.clone, ref, "main"); err == nil {
		t.Error("UpdateRef took a branch name for a commit id")
	}
}

// A resolution that deletes the conflicted path leaves it out of the tree.
func TestWriteTreeFromLeavesOutAPathTheWorkTreeDeleted(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	tip, head := fx.stagingConflict()
	if _, err := fx.c.MergeNoCommit(ctx, fx.clone, head, resolveWho); err != nil {
		t.Fatal(err)
	}
	mt, _, err := fx.c.MergeTreeWrite(ctx, fx.clone, tip, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fx.clone, "app/a.rb")); err != nil {
		t.Fatal(err)
	}
	tree, err := fx.c.WriteTreeFrom(ctx, fx.clone, mt, []string{"app/a.rb"})
	if err != nil {
		t.Fatal(err)
	}
	if got := fx.git(fx.clone, "ls-tree", "-r", "--name-only", tree); strings.Contains(got, "app/a.rb") || !strings.Contains(got, "db/schema.rb") {
		t.Errorf("the tree lists %q; want db/schema.rb and no app/a.rb", got)
	}
}

// A merge that applies cleanly names no conflict, and one of a revision
// git cannot merge is an error, never a clean merge.
func TestMergeNoCommitOfACleanMergeAndOfNoRevision(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetchPR7()
	if conflicts, err := fx.c.MergeNoCommit(ctx, fx.clone, fx.pr, resolveWho); err != nil || conflicts != nil {
		t.Errorf("a clean MergeNoCommit = %v, %v; want no conflict", conflicts, err)
	}
	fx.git(fx.clone, "merge", "--abort")
	if _, err := fx.c.MergeNoCommit(ctx, fx.clone, strings.Repeat("ab", 20), resolveWho); err == nil {
		t.Error("a merge of a commit the clone lacks answered no error")
	}
	if _, err := fx.c.MergeNoCommit(ctx, fx.clone, "--abort", resolveWho); err == nil {
		t.Error("an option passed as the revision was let through")
	}
}
