package eval

import (
	"fmt"
	"path"
	"strings"
)

// matchGlob reports whether name, a slash-separated path, matches pattern. Without "**" it is
// path.Match. A "**" path segment matches any number of directories, none included, so
// "app/**/oauth*.rb" matches "app/oauth.rb" and "app/a/b/oauth_x.rb". A "**" inside a longer segment
// is an ordinary "*".
func matchGlob(pattern, name string) bool {
	if !strings.Contains(pattern, "**") {
		ok, err := path.Match(pattern, name)
		return err == nil && ok
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for len(rest) > 0 && rest[0] == "**" {
				rest = rest[1:]
			}
			if len(rest) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// checkGlob reports why pattern can never be used: empty, an empty path segment (a leading,
// trailing or doubled slash) or a malformed segment.
func checkGlob(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("empty path pattern")
	}
	for seg := range strings.SplitSeq(pattern, "/") {
		if seg == "" {
			return fmt.Errorf("path pattern %q has an empty segment", pattern)
		}
		if _, err := path.Match(seg, "x"); err != nil {
			return fmt.Errorf("path pattern %q: %w", pattern, err)
		}
	}
	return nil
}
