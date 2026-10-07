package slots

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/store"
)

// The one definition of a pool's databases: names, listing and drop guard.

// dbTemplateSuffix ends every listable pool database template: the slug
// follows the last "__", which is what mysqlx.Slug and mysqlx.Guard expect.
const dbTemplateSuffix = "__{slug}"

// DBLister is the listing half of *mysqlx.Client: the schemas whose names
// start with one of prefixes followed by a non-empty slug, ordered by name.
type DBLister interface {
	ListPrefixed(ctx context.Context, prefixes []string) ([]mysqlx.Database, error)
}

// DBListPrefixes returns the distinct name prefixes ("<base>__") of the
// pools' database templates, sorted: talkable_development__ and
// talkable_test__ for talkable_development__{slug} and talkable_test__{slug}.
// bad lists the templates that do not end in "__{slug}" (config validation
// should refuse them); they are left out of prefixes.
func DBListPrefixes(pools ...config.Pool) (prefixes, bad []string) {
	for _, p := range pools {
		for _, tmpl := range p.Databases {
			base, ok := strings.CutSuffix(tmpl, dbTemplateSuffix)
			if !ok || base == "" || strings.ContainsAny(base, "{}") {
				bad = append(bad, tmpl)
				continue
			}
			prefixes = append(prefixes, base+"__")
		}
	}
	slices.Sort(prefixes)
	slices.Sort(bad)
	return slices.Compact(prefixes), slices.Compact(bad)
}

// ListPoolDatabases lists the per-worktree databases of pools: every schema
// named <prefix><slug> for a prefix of DBListPrefixes, ordered by name (the
// client's answer is checked again: a LIKE scan is only a coarse filter).
// Templates without "__{slug}" are skipped and returned in bad for the
// caller to report. No pool template at all lists nothing (and is not an
// error).
func ListPoolDatabases(ctx context.Context, c DBLister, pools ...config.Pool) (dbs []mysqlx.Database, bad []string, err error) {
	prefixes, bad := DBListPrefixes(pools...)
	if len(prefixes) == 0 {
		return nil, bad, nil
	}
	dbs, err = c.ListPrefixed(ctx, prefixes)
	if err != nil {
		return nil, bad, err
	}
	return slices.DeleteFunc(dbs, func(d mysqlx.Database) bool { return !hasDBPrefix(d.Name, prefixes) }), bad, nil
}

// hasDBPrefix reports whether name is one of prefixes followed by a non-empty slug.
func hasDBPrefix(name string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return len(name) > len(p) && strings.HasPrefix(name, p) })
}

// SlotDBSlug is the database slug of a slot: slots.db_slug, else the slot
// name sanitized like a workspace name (DBSlug: review3 stays review3,
// owner/name#7 becomes owner_name_7). Slots, inventory and cleanup all use it.
func SlotDBSlug(sl store.Slot) string {
	if s := strings.TrimSpace(store.Deref(sl.DBSlug)); s != "" {
		return s
	}
	return DBSlug(sl.Name)
}

// DropGuard is the one drop guard for a pool's own databases, shared by
// slots, inventory and cleanup: Prefix is the common start of the pool's
// database templates before "{" (talkable_ for talkable_development__{slug}
// and talkable_test__{slug}; a single template's text before "{"), and
// AllowRegexp is slot_name with {n} as [0-9]+ (^review[0-9]+$), so only
// magnum's own slot slugs are ever droppable. A pool without database
// templates, with no common prefix or with a slot_name lacking {n} gets the
// zero Guard, which refuses everything.
func DropGuard(pool config.Pool) mysqlx.Guard {
	prefix := DBPrefix(pool)
	if prefix == "" || !strings.Contains(pool.SlotName, "{n}") {
		return mysqlx.Guard{}
	}
	return mysqlx.Guard{Prefix: prefix, AllowRegexp: TemplateRegexp(pool.SlotName)}
}

// DBPrefix is the common start of the pool's database templates before "{"
// (talkable_), "" when there are none or they share nothing.
func DBPrefix(pool config.Pool) string {
	prefix := ""
	for i, d := range pool.Databases {
		head, _, _ := strings.Cut(d, "{")
		if i == 0 {
			prefix = head
			continue
		}
		prefix = commonPrefix(prefix, head)
	}
	return prefix
}

func commonPrefix(a, b string) string {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return a[:i]
		}
	}
	return a[:n]
}

// TemplateRegexp matches what a slot template renders to (slot_name
// review{n}, an expanded slot_path): the template anchored, its text quoted,
// every {n} as [0-9]+ (^review[0-9]+$).
func TemplateRegexp(tmpl string) *regexp.Regexp {
	parts := strings.Split(tmpl, "{n}")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile("^" + strings.Join(parts, "[0-9]+") + "$")
}

// devDatabase is the database whose schema_migrations is compared with
// db/schema.rb: the first configured name containing "development", else the
// first one.
func devDatabase(pool config.Pool, slug string) string {
	names := pool.DBNames(slug)
	for _, n := range names {
		if strings.Contains(n, "development") {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}
