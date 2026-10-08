package gitx

import (
	"context"
	"maps"
	"reflect"
	"testing"
)

// Mentions reads the pages of a revision from the object store only: a page
// the checkout edits, a page it adds (untracked or committed on the PR's
// head) and an uncommitted edit are never read, and the base's version of a
// page the PR edits is the one that counts. Only the files the pathspec
// names count, binary ones never, and where two words overlap an occurrence
// counts for the longer.
func TestRealMentionsReadsTheRevisionOnly(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	dir := fx.origin
	const path = "app/models/view_screenshot.rb"
	for rel, content := range map[string]string{
		".docs/wiki/view-testing.md":   "See `" + path + "`.\nThe fence lives in " + path + ".\n",
		"docs/adr/0001-screenshots.md": path + " holds it; lib/tool.rb too.\n",
		"docs/unrelated.md":            "Nothing here.\n",
		"engines/e/README.md":          "engines/e/" + path + "\n",
		"docs/blob.md":                 "\x00" + path + "\n",
		"notes.txt":                    path + "\n",
		path:                           "# " + path + "\n",
	} {
		fx.write(dir, rel, content)
	}
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "Pages")
	base := fx.git(dir, "rev-parse", "HEAD")

	// The PR's head drops the mention from one page and adds one to
	// another; the checkout then edits a third and adds an untracked page.
	fx.git(dir, "checkout", "--quiet", "-b", "feature")
	fx.write(dir, ".docs/wiki/view-testing.md", "Rewritten.\n")
	fx.write(dir, "docs/unrelated.md", "Now about "+path+".\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "The PR edits the pages")
	fx.write(dir, "docs/adr/0001-screenshots.md", "Edited in the checkout.\n")
	fx.write(dir, "docs/new.md", path+"\n")

	want := map[string]map[string]int{
		path:          {".docs/wiki/view-testing.md": 2, "docs/adr/0001-screenshots.md": 1, "engines/e/README.md": 1},
		"lib/tool.rb": {"docs/adr/0001-screenshots.md": 1},
	}
	for _, rev := range []string{"main", base} {
		got, err := fx.c.Mentions(ctx, dir, rev, []string{path, "lib/tool.rb", "lib/missing.rb"}, ":(top)*.md")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Mentions at %s = %v, %v\nwant %v", rev, got, err, want)
		}
	}

	longer := "engines/e/" + path
	got, err := fx.c.Mentions(ctx, dir, "main", []string{path, longer}, ":(top)*.md")
	if err != nil || !maps.Equal(got[longer], map[string]int{"engines/e/README.md": 1}) || got[path]["engines/e/README.md"] != 0 {
		t.Fatalf("overlapping words = %v, %v; want engines/e/README.md counted for %s only", got, err, longer)
	}

	if got, err := fx.c.Mentions(ctx, dir, "main", []string{"lib/missing.rb"}, ":(top)*.md"); err != nil || got != nil {
		t.Fatalf("Mentions of a word no page has = %v, %v; want nil", got, err)
	}
	if got, err := fx.c.Mentions(ctx, dir, "main", nil, ":(top)*.md"); err != nil || got != nil {
		t.Fatalf("Mentions of no word = %v, %v; want nil", got, err)
	}
	for _, tc := range []struct {
		rev   string
		words []string
	}{
		{"main..feature", []string{path}},
		{"main:docs", []string{path}},
		{"-main", []string{path}},
		{"main", []string{"two\nlines"}},
		{"main", []string{""}},
	} {
		if got, err := fx.c.Mentions(ctx, dir, tc.rev, tc.words, ":(top)*.md"); err == nil {
			t.Errorf("Mentions(%q, %q) = %v, want an error", tc.rev, tc.words, got)
		}
	}
}
