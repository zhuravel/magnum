package gitx

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// prepassFixture commits a base and a PR head on branch feature in the
// fixture's origin: the head renames a method, deletes a file, adds two,
// edits a lockfile, a SQL file whose removed line starts with "--", a
// file whose added line starts with "++" and a file in two places, adds a
// binary file and a file whose name git quotes. It returns the base and
// the head.
func prepassFixture(t *testing.T, fx *fixture) (base, head string) {
	t.Helper()
	dir := fx.origin
	for rel, content := range map[string]string{
		"app/models/order.rb":   "class Order\n  def total_price\n    items.sum(&:price)\n  end\nend\n",
		"app/models/user.rb":    "class User\nend\n",
		"lib/report.rb":         "module Report\n  def self.summarize(rows)\n  end\nend\n",
		"Gemfile.lock":          "GEM\n  rails (7.1)\n",
		"db/notes.sql":          "-- keep\n-- note\nSELECT 1;\n",
		"app/counter.txt":       "+x\n",
		"app/services/check.rb": "order.total_price\n",
		"app/models/cache.rb":   "total_price_cache = 1\n",
		"app/jobs/sync.rb":      "class Sync\n  def a\n    1\n  end\n\n  def b\n    2\n  end\nend\n",
	} {
		fx.write(dir, rel, content)
	}
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "Base")
	base = fx.git(dir, "rev-parse", "HEAD")

	fx.git(dir, "checkout", "--quiet", "-b", "feature")
	fx.write(dir, "app/models/order.rb", "class Order\n  def total_amount\n    items.sum(&:price)\n  end\nend\n")
	fx.git(dir, "rm", "--quiet", "lib/report.rb")
	fx.write(dir, "app/models/order_item.rb", "class OrderItem\nend\n")
	fx.write(dir, "Gemfile.lock", "GEM\n  rails (7.2)\n")
	fx.write(dir, "db/notes.sql", "-- keep\nSELECT 1;\n")
	fx.write(dir, "app/counter.txt", "+x\n++y\n")
	fx.write(dir, "app/jobs/sync.rb", "class Sync\n  def a\n    10\n  end\n\n  def b\n    20\n  end\nend\n")
	fx.write(dir, "app/logo.png", "\x89PNG\x00 total_price\n")
	fx.write(dir, `app/we"ird.rb`, "def odd_name\nend\n")
	fx.write(dir, "app/views/orders/show.html.erb", "<%= @order.total_price %>\n<%= total_price_cache %>\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "The PR")
	head = fx.git(dir, "rev-parse", "HEAD")
	// An uncommitted edit of the checkout is never read.
	fx.write(dir, "app/models/user.rb", "class User\n  total_price\nend\n")
	return base, head
}

// The pre-pass reads a PR's diff with no context, file by file, as git has
// it on the two commits: each file's status, the lines it removes and adds,
// a removed "-- note" and an added "++y" as content, not headers, and where
// each hunk's lines are at the head (a hunk that only removes lines sits
// after the line it follows, 0 at the top); a binary file without lines or
// hunks; a quoted name unquoted; and the paths it is told to skip (a
// lockfile) left out. DiffStat counts each file's lines.
func TestRealDiffLinesReadsThePRsDiffFileByFile(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	base, head := prepassFixture(t, fx)

	stats, err := fx.c.DiffStat(ctx, fx.origin, base, head)
	if err != nil {
		t.Fatalf("DiffStat: %v", err)
	}
	wantStats := []FileStat{
		{Path: "Gemfile.lock", Added: 1, Removed: 1},
		{Path: "app/counter.txt", Added: 1},
		{Path: "app/jobs/sync.rb", Added: 2, Removed: 2},
		{Path: "app/logo.png"},
		{Path: "app/models/order.rb", Added: 1, Removed: 1},
		{Path: "app/models/order_item.rb", Added: 2},
		{Path: "app/views/orders/show.html.erb", Added: 2},
		{Path: `app/we"ird.rb`, Added: 2},
		{Path: "db/notes.sql", Removed: 1},
		{Path: "lib/report.rb", Removed: 4},
	}
	if !reflect.DeepEqual(stats, wantStats) {
		t.Fatalf("DiffStat = %+v\nwant %+v", stats, wantStats)
	}

	files, err := fx.c.DiffLines(ctx, fx.origin, base, head, "Gemfile.lock")
	if err != nil {
		t.Fatalf("DiffLines: %v", err)
	}
	want := []FileLines{
		{Path: "app/counter.txt", Status: 'M', Added: []string{"++y"}, Hunks: []Hunk{{Start: 2, Count: 1}}},
		{Path: "app/jobs/sync.rb", Status: 'M', Removed: []string{"    1", "    2"}, Added: []string{"    10", "    20"},
			Hunks: []Hunk{{Start: 3, Count: 1}, {Start: 7, Count: 1}}},
		{Path: "app/logo.png", Status: 'A'},
		{Path: "app/models/order.rb", Status: 'M', Removed: []string{"  def total_price"}, Added: []string{"  def total_amount"},
			Hunks: []Hunk{{Start: 2, Count: 1}}},
		{Path: "app/models/order_item.rb", Status: 'A', Added: []string{"class OrderItem", "end"}, Hunks: []Hunk{{Start: 1, Count: 2}}},
		{Path: "app/views/orders/show.html.erb", Status: 'A', Added: []string{"<%= @order.total_price %>", "<%= total_price_cache %>"},
			Hunks: []Hunk{{Start: 1, Count: 2}}},
		{Path: `app/we"ird.rb`, Status: 'A', Added: []string{"def odd_name", "end"}, Hunks: []Hunk{{Start: 1, Count: 2}}},
		{Path: "db/notes.sql", Status: 'M', Removed: []string{"-- note"}, Hunks: []Hunk{{Start: 1}}},
		{Path: "lib/report.rb", Status: 'D', Removed: []string{"module Report", "  def self.summarize(rows)", "  end", "end"},
			Hunks: []Hunk{{}}},
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("DiffLines = %+v\nwant %+v", files, want)
	}

	if got, err := fx.c.DiffLines(ctx, fx.origin, head, head); err != nil || got != nil {
		t.Fatalf("DiffLines of no change = %v, %v; want nil", got, err)
	}
	for _, revs := range [][2]string{{"-x", head}, {base, "a..b"}, {base, ""}} {
		if _, err := fx.c.DiffLines(ctx, fx.origin, revs[0], revs[1]); err == nil {
			t.Errorf("DiffLines(%q, %q) succeeded, want an error", revs[0], revs[1])
		}
		if _, err := fx.c.DiffStat(ctx, fx.origin, revs[0], revs[1]); err == nil {
			t.Errorf("DiffStat(%q, %q) succeeded, want an error", revs[0], revs[1])
		}
	}
}

// WordUses finds the files of a revision that name a word as a whole word,
// each with its first such line, from the object store only: a longer word
// that contains it, a binary file and an uncommitted edit of the checkout
// never count.
func TestRealWordUsesReadsTheRevisionsWholeWords(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	_, head := prepassFixture(t, fx)

	got, err := fx.c.WordUses(ctx, fx.origin, head, "total_price")
	want := []Use{{Path: "app/services/check.rb", Line: 1}, {Path: "app/views/orders/show.html.erb", Line: 1}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("WordUses = %+v, %v\nwant %+v", got, err, want)
	}
	if got, err := fx.c.WordUses(ctx, fx.origin, head, "summarize"); err != nil || got != nil {
		t.Fatalf("WordUses of a word no file names = %+v, %v; want nil", got, err)
	}
	for _, tc := range []struct{ rev, word string }{{"a..b", "x"}, {head + ":app", "x"}, {head, ""}, {head, "two\nlines"}} {
		if _, err := fx.c.WordUses(ctx, fx.origin, tc.rev, tc.word); err == nil {
			t.Errorf("WordUses(%q, %q) succeeded, want an error", tc.rev, tc.word)
		}
	}
}

// The pre-pass reads a changed file as a revision has it, from the object
// store: an uncommitted edit of the checkout never counts. A path the
// revision lacks fails, as do a range, a path not from the top and a path
// of two lines; a file past execx.MaxOutput is ErrFileTooLarge.
func TestRealFileAtReadsARevisionsFile(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	base, head := prepassFixture(t, fx)

	for _, tc := range []struct{ rev, path, want string }{
		{head, "app/jobs/sync.rb", "class Sync\n  def a\n    10\n  end\n\n  def b\n    20\n  end\nend\n"},
		{base, "app/jobs/sync.rb", "class Sync\n  def a\n    1\n  end\n\n  def b\n    2\n  end\nend\n"},
		{head, "app/models/user.rb", "class User\nend\n"},
		{head, `app/we"ird.rb`, "def odd_name\nend\n"},
	} {
		if got, err := fx.c.FileAt(ctx, fx.origin, tc.rev, tc.path); err != nil || got != tc.want {
			t.Errorf("FileAt(%s) = %q, %v; want %q", tc.path, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ rev, path string }{
		{head, "lib/report.rb"}, {"a..b", "app/jobs/sync.rb"}, {head + ":app", "jobs/sync.rb"}, {head, ""}, {head, "/etc/hosts"},
		{head, "../x.rb"}, {head, "./app/jobs/sync.rb"}, {head, "app//jobs/sync.rb"}, {head, "two\nlines.rb"},
	} {
		if _, err := fx.c.FileAt(ctx, fx.origin, tc.rev, tc.path); err == nil {
			t.Errorf("FileAt(%q, %q) succeeded, want an error", tc.rev, tc.path)
		}
	}

	c, _ := newFake(execx.Rule{Prefix: []string{"git", "-C", slot, "cat-file"}, Result: execx.Result{Stdout: []byte("x"), Truncated: true}})
	if _, err := c.FileAt(ctx, slot, sha1, "app/assets/bundle.js"); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("FileAt of a truncated file = %v, want ErrFileTooLarge", err)
	}
}

// DirFiles lists the files directly in each directory at a revision, the
// top as ".", never a subdirectory or a file of one, nor an uncommitted
// file of the checkout.
func TestRealDirFilesListsADirectorysFiles(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	_, head := prepassFixture(t, fx)
	fx.write(fx.origin, "app/models/untracked.rb", "class Untracked\nend\n")

	got, err := fx.c.DirFiles(ctx, fx.origin, head, "app/models", ".", "app/missing")
	want := []string{"Gemfile.lock", "README.md", "app/models/cache.rb", "app/models/order.rb", "app/models/order_item.rb", "app/models/user.rb"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("DirFiles = %q, %v\nwant %q", got, err, want)
	}
	if got, err := fx.c.DirFiles(ctx, fx.origin, head, "app/services"); err != nil || !slices.Equal(got, []string{"app/services/check.rb"}) {
		t.Fatalf("DirFiles(app/services) = %q, %v", got, err)
	}
	if got, err := fx.c.DirFiles(ctx, fx.origin, head); err != nil || got != nil {
		t.Fatalf("DirFiles of no directory = %q, %v; want nil", got, err)
	}
	if _, err := fx.c.DirFiles(ctx, fx.origin, "a..b", "app"); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Errorf("DirFiles of a range = %v, want an error", err)
	}
}
