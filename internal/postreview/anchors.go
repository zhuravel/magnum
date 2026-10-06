package postreview

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/github"
)

// BadAnchor is an inline comment GitHub would refuse: its line (or its
// start_line) is not a line of the pull request's diff on its side.
type BadAnchor struct {
	Index     int    `json:"index"` // in the review file's comments
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"start_line,omitempty"`
	Side      string `json:"side"`
	Why       string `json:"why"`
	// Valid are the line ranges of the file's hunks on that side
	// ("12-20"); empty for a file the pull request does not change.
	Valid []string `json:"valid"`
}

// span is one hunk's lines on one side, first to last.
type span struct{ first, last int }

func (s span) String() string {
	if s.first == s.last {
		return strconv.Itoa(s.first)
	}
	return fmt.Sprintf("%d-%d", s.first, s.last)
}

// fileDiff is one file's hunks on both sides; changed is false for a file
// the pull request does not change.
type fileDiff struct {
	changed     bool
	right, left []span
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parsePatch reads the hunk headers of a unified diff: a hunk covers its
// new range on RIGHT (added and context lines) and its old range on LEFT
// (deleted and context lines). ok is false when patch has no hunk (a binary
// file, or no diff at all).
func parsePatch(patch string) (fd fileDiff, ok bool) {
	fd.changed = true
	for line := range strings.SplitSeq(patch, "\n") {
		m := hunkRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ok = true
		if s, n := hunkRange(m[1], m[2]); n > 0 {
			fd.left = append(fd.left, span{s, s + n - 1})
		}
		if s, n := hunkRange(m[3], m[4]); n > 0 {
			fd.right = append(fd.right, span{s, s + n - 1})
		}
	}
	return fd, ok
}

// hunkRange reads "start[,count]"; the count defaults to 1.
func hunkRange(start, count string) (int, int) {
	s, _ := strconv.Atoi(start)
	n := 1
	if count != "" {
		n, _ = strconv.Atoi(count)
	}
	return s, n
}

// check returns why comment c (its sides filled in) is not on fd's lines,
// or "".
func (fd fileDiff) check(c github.DraftComment) string {
	if !fd.changed {
		return fmt.Sprintf("the pull request does not change %s", c.Path)
	}
	spans := fd.spans(c.Side)
	at := func(line int) int {
		for i, s := range spans {
			if line >= s.first && line <= s.last {
				return i
			}
		}
		return -1
	}
	end := at(c.Line)
	if end < 0 {
		return fmt.Sprintf("line %d is not in the diff of %s on %s", c.Line, c.Path, c.Side)
	}
	if c.StartLine > 0 {
		start := at(c.StartLine)
		switch {
		case start < 0:
			return fmt.Sprintf("start_line %d is not in the diff of %s on %s", c.StartLine, c.Path, c.Side)
		case start != end:
			return fmt.Sprintf("start_line %d and line %d are in different hunks: a comment's lines must be in one hunk", c.StartLine, c.Line)
		}
	}
	return ""
}

func (fd fileDiff) spans(side string) []span {
	if side == SideLeft {
		return fd.left
	}
	return fd.right
}

// valid lists fd's hunks on side as "first-last".
func (fd fileDiff) valid(side string) []string {
	out := []string{}
	for _, s := range fd.spans(side) {
		out = append(out, s.String())
	}
	return out
}
