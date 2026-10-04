package config

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// MatchPath reports whether the "/"-separated name matches glob. A "**"
// segment matches zero or more whole segments; every other segment is matched
// with path.Match, so "*" and "?" never cross a "/". Matching is
// case-sensitive (git paths are). A malformed glob matches nothing; see
// ValidatePathGlob.
func MatchPath(glob, name string) bool {
	if glob == "" || name == "" {
		return false
	}
	pat := strings.Split(glob, "/")
	segs := strings.Split(name, "/")
	// Greedy wildcard matching: "**" is the only token that can consume a
	// variable number of segments, so remembering the last one is enough.
	p, s := 0, 0
	star, mark := -1, 0
	for s < len(segs) {
		switch {
		case p < len(pat) && pat[p] == "**":
			star, mark = p, s
			p++
		case p < len(pat) && segMatch(pat[p], segs[s]):
			p++
			s++
		case star >= 0:
			mark++
			p, s = star+1, mark
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == "**" {
		p++
	}
	return p == len(pat)
}

func segMatch(pattern, seg string) bool {
	ok, err := path.Match(pattern, seg)
	return err == nil && ok
}

// ValidatePathGlob checks a skip_paths entry: it must not be blank, no
// segment may be empty (a leading, trailing or doubled "/" would silently
// match nothing), every segment must be a well-formed path.Match pattern, and
// "**" is only allowed as a whole segment ("a**b" is rejected).
func ValidatePathGlob(glob string) error {
	if strings.TrimSpace(glob) == "" {
		return errors.New("empty pattern")
	}
	for seg := range strings.SplitSeq(glob, "/") {
		switch {
		case seg == "":
			return errors.New("empty path segment (leading, trailing or doubled \"/\")")
		case seg == "**":
		case strings.Contains(seg, "**"):
			return fmt.Errorf("segment %q: ** must be a whole path segment", seg)
		default:
			// Match reports ErrBadPattern even for an empty name.
			if _, err := path.Match(seg, ""); err != nil {
				return fmt.Errorf("segment %q: %w", seg, err)
			}
		}
	}
	return nil
}

// PathsSkipped reports whether every file of a pull request matches at least
// one skip_paths glob. It is false without globs and without files: a PR whose
// changes are unknown is never skipped.
func (w Watch) PathsSkipped(files []string) bool {
	if len(w.SkipPaths) == 0 || len(files) == 0 {
		return false
	}
	for _, f := range files {
		matched := false
		for _, g := range w.SkipPaths {
			if MatchPath(g, f) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
