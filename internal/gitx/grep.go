package gitx

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
)

// Mentions returns where the files of rev name each of words: for each word,
// the files that pathspecs match (pathspec magic allowed) whose text
// contains it, with the number of its occurrences in each. It is one `git
// grep -o -F` on rev's tree, which reads the object store only: a file the
// work tree or the index has in another version, or only there, is never
// read. Words are taken literally and in their case, one line each; where
// two overlap, an occurrence counts for the one that starts first, then for
// the longer (git grep -o). Binary files are left out. A word no file
// names is absent; nil when none is named or words is empty. rev must name
// a commit (no range, no "<rev>:<path>").
func (c *Client) Mentions(ctx context.Context, dir, rev string, words []string, pathspecs ...string) (map[string]map[string]int, error) {
	if err := checkRev("revision", rev); err != nil {
		return nil, err
	}
	if strings.Contains(rev, "..") || strings.Contains(rev, ":") {
		return nil, fmt.Errorf("gitx: revision %q must name a commit", rev)
	}
	if len(words) == 0 {
		return nil, nil
	}
	args := []string{"grep", "--no-recurse-submodules", "--no-line-number", "--no-column", "--no-color", "-I", "-o", "-z", "-F"}
	for _, w := range words {
		if w == "" || strings.ContainsAny(w, "\n\x00") {
			return nil, errors.New("gitx: grep: a word must be one non-empty line")
		}
		args = append(args, "-e", w)
	}
	args = append(append(args, rev, "--"), pathspecs...)
	res, err := c.git(ctx, dir, call{label: "grep " + rev, noExit: 1}, args...)
	if err != nil {
		if code, ok := exitCode(err); ok && code == 1 {
			return nil, nil // no file names any word
		}
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: grep %s: more than %d bytes of matches", rev, execx.MaxOutput)
	}
	// Each occurrence is "<rev>:<file> NUL <word> LF" (-o -z): a file name
	// has no NUL, and a word no LF.
	var out map[string]map[string]int
	prefix := rev + ":"
	for rest := string(res.Stdout); rest != ""; {
		name, after, ok := strings.Cut(rest, "\x00")
		word, tail, ok2 := strings.Cut(after, "\n")
		file, ok3 := strings.CutPrefix(name, prefix)
		if !ok || !ok2 || !ok3 || file == "" || !slices.Contains(words, word) {
			return nil, fmt.Errorf("gitx: grep %s: unexpected output", rev)
		}
		if out == nil {
			out = map[string]map[string]int{}
		}
		if out[word] == nil {
			out[word] = map[string]int{}
		}
		out[word][file]++
		rest = tail
	}
	return out, nil
}
