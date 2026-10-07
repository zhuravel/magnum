package slots

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/store"
)

func TestDBListPrefixes(t *testing.T) {
	a := config.Pool{Databases: []string{"talkable_test__{slug}", "talkable_development__{slug}", "app_{slug}"}}
	b := config.Pool{Databases: []string{"talkable_development__{slug}", "example_dev__{slug}", "x__{slug}_suffix", "__{slug}"}}
	prefixes, bad := DBListPrefixes(a, b)
	if want := []string{"example_dev__", "talkable_development__", "talkable_test__"}; !slices.Equal(prefixes, want) {
		t.Fatalf("prefixes = %v, want %v", prefixes, want)
	}
	if want := []string{"__{slug}", "app_{slug}", "x__{slug}_suffix"}; !slices.Equal(bad, want) {
		t.Fatalf("bad = %v, want %v", bad, want)
	}
}

// prefixLister is mysqlx's ListPrefixed, with the coarse LIKE scan's
// extra hits (a "__" inside the slug).
type prefixLister struct {
	names []string
	asked [][]string
}

func (l *prefixLister) ListPrefixed(_ context.Context, prefixes []string) ([]mysqlx.Database, error) {
	l.asked = append(l.asked, prefixes)
	var out []mysqlx.Database
	for _, n := range l.names {
		if slug, ok := mysqlx.Slug(n); ok && slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(n, p) }) {
			out = append(out, mysqlx.Database{Name: n, Slug: slug})
		}
	}
	return out, nil
}

func TestListPoolDatabases(t *testing.T) {
	ctx := context.Background()
	names := []string{"talkable_development__review1", "talkable_staging__review1", "talkable_development__",
		"example_dev__review2", "other__review3"}
	talkable := config.Pool{Databases: []string{"talkable_development__{slug}"}}
	example := config.Pool{Databases: []string{"example_dev__{slug}", "example_{slug}"}}

	t.Run("lists every pool's prefixes", func(t *testing.T) {
		l := &prefixLister{names: names}
		dbs, bad, err := ListPoolDatabases(ctx, l, talkable, example)
		if err != nil {
			t.Fatal(err)
		}
		if got := dbNamesOf(dbs); !slices.Equal(got, []string{"talkable_development__review1", "example_dev__review2"}) {
			t.Fatalf("dbs = %v", got)
		}
		if len(l.asked) != 1 || !slices.Equal(l.asked[0], []string{"example_dev__", "talkable_development__"}) {
			t.Fatalf("asked = %v", l.asked)
		}
		if !slices.Equal(bad, []string{"example_{slug}"}) {
			t.Fatalf("bad = %v", bad)
		}
	})
	t.Run("no template lists nothing", func(t *testing.T) {
		l := &prefixLister{names: names}
		dbs, _, err := ListPoolDatabases(ctx, l, config.Pool{})
		if err != nil || dbs != nil || len(l.asked) != 0 {
			t.Fatalf("dbs = %v, err = %v, asked = %v", dbs, err, l.asked)
		}
	})
}

func dbNamesOf(dbs []mysqlx.Database) []string {
	var out []string
	for _, d := range dbs {
		out = append(out, d.Name)
	}
	return out
}

func TestSlotDBSlug(t *testing.T) {
	cases := []struct {
		sl   store.Slot
		want string
	}{
		{store.Slot{Name: "review3", DBSlug: store.Ptr("review3")}, "review3"},
		{store.Slot{Name: "review3"}, "review3"},
		{store.Slot{Name: "zhuravel/widget#7"}, "zhuravel_widget_7"},
		{store.Slot{Name: "zhuravel/widget#7", DBSlug: store.Ptr(" magnum_pr_7 ")}, "magnum_pr_7"},
	}
	for _, c := range cases {
		if got := SlotDBSlug(c.sl); got != c.want {
			t.Errorf("SlotDBSlug(%+v) = %q, want %q", c.sl, got, c.want)
		}
	}
}

func TestNonTalkablePoolDatabases(t *testing.T) {
	h := newHarness(t)
	h.pool.Databases = []string{"example_development__{slug}", "example_test__{slug}"}
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); err != nil {
		t.Fatalf("ProvisionPool: %v", err)
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree {
		t.Fatalf("state = %s", sl.State)
	}
	h.my.add(schemaV1, "example_development__review9") // another slot's: never touched
	if err := h.m.Remove(h.ctx, h.slot("review1"), h.pool, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, n := range h.pool.DBNames("review1") {
		if h.my.has(n) {
			t.Fatalf("%s left behind", n)
		}
	}
	if !h.my.has("example_development__review9") {
		t.Fatal("another slot's database was dropped")
	}
}
