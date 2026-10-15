package gitx

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The evidence map marks what a round examined stale when its file changed
// since: the files whose content differs between the two heads, whatever
// their history. A rewritten branch that drops an earlier head's change to a
// file changes that file too, which a diff from the merge base
// (ChangedPaths) does not name.
func TestRealChangedBetweenNamesTheFilesThatDifferBetweenTwoCommits(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	dir := fx.origin
	fx.write(dir, "app/a.rb", "a\n")
	fx.write(dir, "app/b.rb", "b\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "Base")
	base := fx.git(dir, "rev-parse", "HEAD")

	fx.git(dir, "checkout", "--quiet", "-b", "feature")
	fx.write(dir, "app/a.rb", "a2\n")
	fx.git(dir, "commit", "--quiet", "-am", "First head")
	first := fx.git(dir, "rev-parse", "HEAD")

	fx.git(dir, "checkout", "--quiet", "-b", "rewritten", base)
	fx.write(dir, "app/b.rb", "b2\n")
	fx.write(dir, "app/c.rb", "c\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "Rewritten head")
	second := fx.git(dir, "rev-parse", "HEAD")

	got, err := fx.c.ChangedBetween(ctx, dir, first, second)
	if want := []string{"app/a.rb", "app/b.rb", "app/c.rb"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("ChangedBetween = %q, %v; want %q", got, err, want)
	}
	if got, err := fx.c.ChangedPaths(ctx, dir, first, second); err != nil || slices.Contains(got, "app/a.rb") {
		t.Fatalf("ChangedPaths = %q, %v: the merge-base diff was expected to miss the dropped change", got, err)
	}
	if got, err := fx.c.ChangedBetween(ctx, dir, second, second); err != nil || got != nil {
		t.Fatalf("ChangedBetween of one commit = %q, %v; want nil", got, err)
	}
	if _, err := fx.c.ChangedBetween(ctx, dir, "a..b", second); err == nil || !strings.Contains(err.Error(), "range") {
		t.Errorf("ChangedBetween of a range = %v, want an error", err)
	}
}
