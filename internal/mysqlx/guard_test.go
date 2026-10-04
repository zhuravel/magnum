package mysqlx

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	cases := []struct {
		name string
		slug string
		ok   bool
	}{
		{"talkable_development__review3", "review3", true},
		{"talkable_development_shard_1001__repo12", "repo12", true},
		{"talkable_test__pr27087fix", "pr27087fix", true},
		{"talkable_test__my_branch", "my_branch", true}, // single underscores stay in the slug
		{"talkable_x__a__b", "b", true},                 // slug is what follows the LAST "__"
		{"talkable_x___slug", "slug", true},             // triple underscore: last pair wins
		{"talkable_development", "", false},             // unsuffixed base name
		{"talkable_x__", "", false},                     // empty slug
		{"__review1", "", false},                        // empty base
		{"", "", false},                                 // nothing
		{"talkable_x_", "", false},                      // single trailing underscore
	}
	for _, c := range cases {
		slug, ok := Slug(c.name)
		if slug != c.slug || ok != c.ok {
			t.Errorf("Slug(%q) = (%q, %v), want (%q, %v)", c.name, slug, ok, c.slug, c.ok)
		}
	}
}

func TestGuardCheck(t *testing.T) {
	review := regexp.MustCompile(`^review\d+$`)
	loose := regexp.MustCompile(`review\d+`) // not anchored on purpose: Check must still demand a full match
	alt := regexp.MustCompile(`review|review\d+`)

	cases := []struct {
		label   string
		guard   Guard
		name    string
		refused bool
	}{
		{"matching slug", Guard{AllowRegexp: review}, "talkable_development__review3", false},
		{"matching shard db", Guard{AllowRegexp: review}, "talkable_test_shard_1001__review12", false},
		{"slug mismatch", Guard{AllowRegexp: review}, "talkable_development__repo3", true},
		{"slug mismatch pr slug", Guard{AllowRegexp: review}, "talkable_test__pr27087fix", true},
		{"unsuffixed base name", Guard{AllowRegexp: review}, "talkable_development", true},
		{"unsuffixed even with permissive regexp", Guard{AllowRegexp: regexp.MustCompile(`.*`)}, "talkable_development", true},
		{"system schema", Guard{AllowRegexp: regexp.MustCompile(`.*`)}, "mysql", true},
		{"information_schema", Guard{AllowRegexp: regexp.MustCompile(`.*`)}, "information_schema", true},
		{"empty slug", Guard{AllowRegexp: regexp.MustCompile(`.*`)}, "talkable_x__", true},
		{"empty base", Guard{AllowRegexp: regexp.MustCompile(`.*`)}, "__review1", true},
		{"empty name", Guard{AllowRegexp: regexp.MustCompile(`.*`)}, "", true},
		{"nil regexp fails closed", Guard{}, "talkable_development__review3", true},
		{"unanchored regexp must match whole slug (prefix junk)", Guard{AllowRegexp: loose}, "talkable_development__xreview3", true},
		{"unanchored regexp must match whole slug (suffix junk)", Guard{AllowRegexp: loose}, "talkable_development__review3x", true},
		{"unanchored regexp full match ok", Guard{AllowRegexp: loose}, "talkable_development__review3", false},
		{"leftmost-first alternation still demands full match", Guard{AllowRegexp: alt}, "talkable_development__review12", false},
		{"alternation partial match refused", Guard{AllowRegexp: alt}, "talkable_development__review12x", true},
		{"regexp is applied to the slug, not the full name", Guard{AllowRegexp: regexp.MustCompile(`^talkable_.*`)}, "talkable_development__review3", true},
		{"backtick injection", Guard{AllowRegexp: regexp.MustCompile(`.*`)}, "talkable_x__a`; DROP DATABASE mysql; --", true},
		{"space in name", Guard{AllowRegexp: regexp.MustCompile(`.*`)}, "talkable_x__a b", true},
		{"dash in name", Guard{AllowRegexp: regexp.MustCompile(`.*`)}, "talkable_x__a-b", true},
		{"newline in name", Guard{AllowRegexp: regexp.MustCompile(`.*`)}, "talkable_x__a\n", true},
		{"prefix satisfied", Guard{AllowRegexp: review, Prefix: "talkable_"}, "talkable_test__review1", false},
		{"prefix violated", Guard{AllowRegexp: review, Prefix: "talkable_"}, "mysql__review1", true},
	}
	for _, c := range cases {
		err := c.guard.Check(c.name)
		if c.refused {
			if err == nil || !errors.Is(err, ErrGuard) {
				t.Errorf("%s: Check(%q) = %v, want ErrGuard", c.label, c.name, err)
			}
		} else if err != nil {
			t.Errorf("%s: Check(%q) = %v, want nil", c.label, c.name, err)
		}
	}
}

func TestValidIdent(t *testing.T) {
	for _, ok := range []string{"talkable_test__review1", "a", "A9_z", "magnum_test_ab12__x"} {
		if err := validIdent(ok); err != nil {
			t.Errorf("validIdent(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a-b", "a b", "a`b", "a.b", "a;b", "a\n", "ünï", "a'b"} {
		if err := validIdent(bad); err == nil {
			t.Errorf("validIdent(%q) accepted", bad)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	got, err := quoteIdent("talkable_test__review1")
	if err != nil || got != "`talkable_test__review1`" {
		t.Fatalf("quoteIdent = %q, %v", got, err)
	}
	if _, err := quoteIdent("x`y"); err == nil {
		t.Fatal("quoteIdent accepted a backtick")
	}
}

func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		"talkable_": `talkable|_`,
		"__":        `|_|_`,
		"100%":      `100|%`,
		`a|b`:       `a||b`,
		`a\b`:       `a\b`, // the backslash is an ordinary character now
		"plain":     "plain",
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLikePattern(t *testing.T) {
	if got, want := likePattern("talkable_"), `talkable|_%|_|_%`; got != want {
		t.Fatalf("likePattern = %q, want %q", got, want)
	}
	if DefaultPattern != `talkable|_%|_|_%` {
		t.Fatalf("DefaultPattern = %q", DefaultPattern)
	}
	if strings.Contains(DefaultPattern, `\`) {
		t.Fatalf("DefaultPattern uses backslash escapes, which NO_BACKSLASH_ESCAPES breaks: %q", DefaultPattern)
	}
}

// Check anchors a copy of the regexp once per distinct pattern, and the copy
// keeps full-match semantics.
func TestAnchoredIsMemoized(t *testing.T) {
	re := regexp.MustCompile(`review\d+`)
	a, err := anchored(re)
	if err != nil {
		t.Fatal(err)
	}
	b, err := anchored(regexp.MustCompile(`review\d+`)) // a different *Regexp, the same pattern
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("anchored recompiled an already anchored pattern")
	}
	if !a.MatchString("review3") || a.MatchString("xreview3") || a.MatchString("review3x") {
		t.Fatal("anchored regexp is not an exact match")
	}
	if re.String() != `review\d+` {
		t.Fatal("anchored modified its input")
	}
}

func TestAnchoredCacheIsBounded(t *testing.T) {
	for i := range 3 * anchoredCacheMax {
		if _, err := anchored(regexp.MustCompile(fmt.Sprintf("^slug%d$", i))); err != nil {
			t.Fatal(err)
		}
	}
	anchoredCache.Lock()
	n := len(anchoredCache.m)
	anchoredCache.Unlock()
	if n > anchoredCacheMax {
		t.Fatalf("cache holds %d entries, cap is %d", n, anchoredCacheMax)
	}
}
