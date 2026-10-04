package mysqlx

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// ErrGuard is wrapped by every refusal from Guard.Check, Drop and DropAll, so
// callers can tell "magnum declined to drop this" from "MySQL failed".
var ErrGuard = errors.New("mysqlx: drop refused by guard")

// sep separates a database's base name from the per-worktree slug:
// talkable_development__review3 is base "talkable_development", slug "review3".
const sep = "__"

// identRe is the only shape of identifier magnum will interpolate into SQL.
var identRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// Guard decides which databases Drop may remove. The zero value refuses
// everything.
type Guard struct {
	// AllowRegexp must match the whole slug (the text after the last "__"); it
	// is implicitly anchored, so an unanchored expression cannot match a
	// fragment. A nil AllowRegexp refuses every name.
	AllowRegexp *regexp.Regexp
	// Prefix, when non-empty, is a required name prefix (callers acting on the
	// Talkable pool set "talkable_" so a foreign schema with a "__" in its name
	// can never be dropped).
	Prefix string
}

// Check reports nil when the guard would allow dropping name, or an error
// wrapping ErrGuard saying why not. It is pure: it never touches the server.
//
// A name is refused unless all of these hold: it is a plain identifier
// ([A-Za-z0-9_]+); it has a non-empty base and a non-empty slug around a "__"
// (so unsuffixed base names such as "talkable_development" and system schemas
// are never droppable); it carries Prefix when one is set; and its slug fully
// matches AllowRegexp.
func (g Guard) Check(name string) error {
	if err := validIdent(name); err != nil {
		return fmt.Errorf("%w: %w", ErrGuard, err)
	}
	slug, ok := Slug(name)
	if !ok {
		return fmt.Errorf("%w: %q has no <base>__<slug> suffix", ErrGuard, name)
	}
	if g.Prefix != "" && !strings.HasPrefix(name, g.Prefix) {
		return fmt.Errorf("%w: %q does not start with %q", ErrGuard, name, g.Prefix)
	}
	if g.AllowRegexp == nil {
		return fmt.Errorf("%w: guard has no AllowRegexp", ErrGuard)
	}
	full, err := anchored(g.AllowRegexp)
	if err != nil {
		return fmt.Errorf("%w: cannot anchor AllowRegexp %q: %w", ErrGuard, g.AllowRegexp.String(), err)
	}
	if !full.MatchString(slug) {
		return fmt.Errorf("%w: slug %q of %q does not match %s", ErrGuard, slug, name, g.AllowRegexp.String())
	}
	return nil
}

// anchoredCache memoizes anchored by pattern text: inventory and cleanup call
// Guard.Check for every database against every pool guard on each scan, and
// the few distinct patterns would otherwise be recompiled each time. Guard is
// a plain value built by struct literal throughout the module, so the cache
// lives here instead of in a constructor. It is cleared when it passes
// anchoredCacheMax entries (manual cleanup guards embed per-slug patterns).
var anchoredCache = struct {
	sync.Mutex
	m map[string]*regexp.Regexp
}{m: map[string]*regexp.Regexp{}}

const anchoredCacheMax = 256

// anchored returns a copy of re that must match the whole input. A bare
// `review\d+` must not be satisfied by "xreview3x", and leftmost-first
// alternation must not hide a full match, so the guard asks the engine for an
// exact match instead of inspecting FindStringIndex.
func anchored(re *regexp.Regexp) (*regexp.Regexp, error) {
	src := re.String()
	anchoredCache.Lock()
	full, ok := anchoredCache.m[src]
	anchoredCache.Unlock()
	if ok {
		return full, nil
	}
	full, err := regexp.Compile(`^(?:` + src + `)$`)
	if err != nil {
		return nil, err
	}
	anchoredCache.Lock()
	if len(anchoredCache.m) >= anchoredCacheMax {
		clear(anchoredCache.m)
	}
	anchoredCache.m[src] = full
	anchoredCache.Unlock()
	return full, nil
}

// Slug returns the part of a database name after its last "__". ok is false
// when the name has no "__", an empty base (name starts with "__") or an empty
// slug (name ends with "__"): such names are not per-worktree databases.
func Slug(name string) (slug string, ok bool) {
	i := strings.LastIndex(name, sep)
	if i <= 0 || i+len(sep) >= len(name) {
		return "", false
	}
	return name[i+len(sep):], true
}

// validIdent rejects anything that is not [A-Za-z0-9_]+.
func validIdent(name string) error {
	if !identRe.MatchString(name) {
		return fmt.Errorf("invalid database identifier %q (want [A-Za-z0-9_]+)", name)
	}
	return nil
}

// quoteIdent validates name and wraps it in backticks for use in DDL, where
// placeholders are not allowed.
func quoteIdent(name string) (string, error) {
	if err := validIdent(name); err != nil {
		return "", err
	}
	return "`" + name + "`", nil
}

// likeEscape is the LIKE escape character every pattern magnum builds uses,
// always passed as an explicit ESCAPE clause. The default escape is the
// backslash, but NO_BACKSLASH_ESCAPES turns that into a plain character and
// would silently change what a pattern matches; "|" is not special in any
// sql_mode, and database names (identifiers) never contain it.
const likeEscape = "|"

// escapeLike escapes the LIKE metacharacters (likeEscape % _) in s so it
// matches literally when the query says ESCAPE '|'.
func escapeLike(s string) string {
	return strings.NewReplacer(likeEscape, likeEscape+likeEscape, "%", likeEscape+"%", "_", likeEscape+"_").Replace(s)
}

// likePattern builds the schemata filter for databases that start with prefix
// and contain "__": prefix, anything, a literal "__", anything.
func likePattern(prefix string) string {
	return escapeLike(prefix) + "%" + escapeLike(sep) + "%"
}
