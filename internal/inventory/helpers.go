package inventory

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/slots"
)

// under reports whether p is root or inside it (talkable.review1 does not
// contain talkable.review10).
func under(p, root string) bool {
	if p == "" || root == "" {
		return false
	}
	p, root = filepath.Clean(p), filepath.Clean(root)
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

// dirSlug infers a slug from a worktree directory: "talkable.repo6" next to
// main clone "talkable" gives "repo6"; any other basename is used whole.
func dirSlug(dir, mainClone string) string {
	base := filepath.Base(filepath.Clean(dir))
	if mb := filepath.Base(filepath.Clean(mainClone)); mb != "" && mb != "." {
		base = strings.TrimPrefix(base, mb+".")
	}
	return slots.DBSlug(base)
}

// hasSlug reports whether database d belongs to slug (slugs may themselves
// contain "__", so the name suffix is checked too).
func hasSlug(d mysqlx.Database, slug string) bool {
	return slug != "" && (d.Slug == slug || strings.HasSuffix(d.Name, "__"+slug))
}

var branchPR = regexp.MustCompile(`PR-(\d+)`)

// branchPRNumber is the fallback PR number from a branch name ("zhuravel-PR-27206-x").
// In the Talkable repo PR-N is usually a Jira key, so callers treat it as a guess.
func branchPRNumber(branch string) (int, bool) {
	m := branchPR.FindStringSubmatch(branch)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

var perPRName = regexp.MustCompile(`^pr-[0-9]+$`)

// templateGlob turns a slot template into a filepath.Glob pattern.
func templateGlob(tmpl string) string {
	parts := strings.Split(tmpl, "{n}")
	for i, p := range parts {
		parts[i] = globEscape(p)
	}
	return strings.Join(parts, "*")
}

func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// parseDU reads the KiB total from `du -sk` output.
func parseDU(out []byte) (int64, bool) {
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// NaturalLess orders strings with embedded numbers numerically
// (review2 < review10), for sorting slots and paths.
func NaturalLess(a, b string) bool { return naturalCmp(a, b) < 0 }

func naturalCmp(a, b string) int {
	for a != "" && b != "" {
		da, db := isDigit(a[0]), isDigit(b[0])
		if da && db {
			na, ra := splitDigits(a)
			nb, rb := splitDigits(b)
			ta, tb := strings.TrimLeft(na, "0"), strings.TrimLeft(nb, "0")
			if len(ta) != len(tb) {
				return cmpInt(len(ta), len(tb))
			}
			if c := strings.Compare(ta, tb); c != 0 {
				return c
			}
			if len(na) != len(nb) {
				return cmpInt(len(na), len(nb))
			}
			a, b = ra, rb
			continue
		}
		if a[0] != b[0] {
			return cmpInt(int(a[0]), int(b[0]))
		}
		a, b = a[1:], b[1:]
	}
	return cmpInt(len(a), len(b))
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func splitDigits(s string) (digits, rest string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
