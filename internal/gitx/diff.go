package gitx

// The reads of a round's pre-pass (internal/pipeline/prepass.go): a PR's
// diff with its line counts, its lines and its hunks, whole-word uses of a
// name in a revision's tree, the files of a revision's directories and a
// file as a revision has it. Each reads the object store only, never the
// work tree, whose files a role may have changed.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
)

// checkRange checks the two revisions of a base...head diff: each a
// revision, neither a range.
func checkRange(base, head string) error {
	for _, rev := range []string{base, head} {
		if err := checkRev("revision", rev); err != nil {
			return err
		}
		if strings.Contains(rev, "..") {
			return fmt.Errorf("gitx: revision %q must not be a range", rev)
		}
	}
	return nil
}

// rangeErr is err of a base...head diff: ErrNoMergeBase when git found no
// merge base, else err.
func rangeErr(err error, base, head string) error {
	var ee *execx.ExitError
	if errors.As(err, &ee) && strings.Contains(ee.Stderr, "no merge base") {
		return fmt.Errorf("gitx: diff %s...%s: %w", base, head, ErrNoMergeBase)
	}
	return err
}

// checkCommit checks rev names a commit: a revision, no range and no
// "<rev>:<path>".
func checkCommit(rev string) error {
	if err := checkRev("revision", rev); err != nil {
		return err
	}
	if strings.Contains(rev, "..") || strings.Contains(rev, ":") {
		return fmt.Errorf("gitx: revision %q must name a commit", rev)
	}
	return nil
}

// FileStat is one file of DiffStat: the lines its diff adds and removes.
type FileStat struct {
	Path           string
	Added, Removed int // both 0 for a binary file
}

// DiffStat counts the lines of each file head changed relative to its merge
// base with base (git diff --numstat base...head; a rename as a deletion
// plus an addition, no external diff or textconv), in git's order; nil when
// head changes nothing.
func (c *Client) DiffStat(ctx context.Context, dir, base, head string) ([]FileStat, error) {
	if err := checkRange(base, head); err != nil {
		return nil, err
	}
	res, err := c.git(ctx, dir, call{label: fmt.Sprintf("diff --numstat %s...%s", base, head)},
		"diff", "--numstat", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", base+"..."+head, "--")
	if err != nil {
		return nil, rangeErr(err, base, head)
	}
	// Each file is "<added> TAB <removed> TAB <path>" NUL; "-" counts a
	// binary file's lines.
	var out []FileStat
	for rec := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\t", 3)
		if len(f) != 3 || f[2] == "" {
			return nil, fmt.Errorf("gitx: diff --numstat %s...%s: unexpected record", base, head)
		}
		s := FileStat{Path: f[2]}
		if f[0] != "-" || f[1] != "-" {
			a, errA := strconv.Atoi(f[0])
			r, errR := strconv.Atoi(f[1])
			if errA != nil || errR != nil {
				return nil, fmt.Errorf("gitx: diff --numstat %s...%s: unexpected counts", base, head)
			}
			s.Added, s.Removed = a, r
		}
		out = append(out, s)
	}
	return out, nil
}

// FileLines is one file of DiffLines.
type FileLines struct {
	Path string
	// Status is 'A' for a file head adds, 'D' for one it deletes, 'M' for
	// any other change (its content, mode or type).
	Status byte
	// Removed and Added are the lines the diff removes and adds, in order,
	// without their "-" or "+"; none for a binary file or a submodule.
	Removed, Added []string
	// Hunks are where each hunk's lines are in head's file, in order; none
	// for a binary file or a submodule.
	Hunks []Hunk
}

// Hunk is the head's side of a hunk without context lines (the "+<start>,
// <count>" of its header): the Count lines from line Start (1-based). A
// hunk that only removes lines has Count 0 and sits after line Start (0:
// at the top of the file).
type Hunk struct {
	Start, Count int
}

// DiffLines is the diff head made relative to its merge base with base,
// file by file in git's order, with its hunks' places in head's files and
// without context lines (git diff -U0
// base...head; a rename as a deletion plus an addition; none of the user's
// settings that change it: an external diff, textconv, path prefixes,
// quoted non-ASCII names), leaving out the paths skip names (relative to
// the top, taken literally). nil when nothing is left.
func (c *Client) DiffLines(ctx context.Context, dir, base, head string, skip ...string) ([]FileLines, error) {
	if err := checkRange(base, head); err != nil {
		return nil, err
	}
	args := []string{"-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--unified=0",
		"--no-renames", "--src-prefix=a/", "--dst-prefix=b/", base + "..." + head, "--"}
	for _, p := range skip {
		if p != "" {
			args = append(args, ":(top,literal,exclude)"+p)
		}
	}
	res, err := c.git(ctx, dir, call{label: fmt.Sprintf("diff -U0 %s...%s", base, head)}, args...)
	if err != nil {
		return nil, rangeErr(err, base, head)
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: diff %s...%s: more than %d bytes", base, head, execx.MaxOutput)
	}
	files, err := parseDiffLines(string(res.Stdout))
	if err != nil {
		return nil, fmt.Errorf("gitx: diff %s...%s: %w", base, head, err)
	}
	return files, nil
}

// parseDiffLines reads a patch without context lines (git diff -U0
// --no-renames, prefixes a/ and b/). Before a file's first hunk its lines
// are headers; in its hunks every line starts with "-", "+", "\" (no newline
// at the end) or "@@" until the next "diff --git" header, so a removed "--
// note" (shown as "--- note") is content, never a header.
func parseDiffLines(patch string) ([]FileLines, error) {
	var out []FileLines
	var cur *FileLines
	inHunk := false
	for line := range strings.SplitSeq(strings.TrimSuffix(patch, "\n"), "\n") {
		if rest, ok := strings.CutPrefix(line, "diff --git "); ok {
			path, err := diffHeaderPath(rest)
			if err != nil {
				return nil, err
			}
			out = append(out, FileLines{Path: path, Status: 'M'})
			cur, inHunk = &out[len(out)-1], false
			continue
		}
		if cur == nil {
			if line == "" {
				continue
			}
			return nil, errors.New("unexpected output before the first file")
		}
		switch {
		case strings.HasPrefix(line, "@@"):
			h, err := hunkHeader(line)
			if err != nil {
				return nil, err
			}
			cur.Hunks = append(cur.Hunks, h)
			inHunk = true
		case !inHunk:
			if strings.HasPrefix(line, "new file mode ") {
				cur.Status = 'A'
			} else if strings.HasPrefix(line, "deleted file mode ") {
				cur.Status = 'D'
			}
		case strings.HasPrefix(line, "-"):
			cur.Removed = append(cur.Removed, line[1:])
		case strings.HasPrefix(line, "+"):
			cur.Added = append(cur.Added, line[1:])
		case strings.HasPrefix(line, `\`):
		default:
			return nil, errors.New("unexpected line in a hunk")
		}
	}
	return out, nil
}

// hunkHeader reads the head's side of a hunk header, "@@ -<a>[,<b>]
// +<start>[,<count>] @@[ <section>]" (a count of 1 is left out).
func hunkHeader(line string) (Hunk, error) {
	f := strings.Fields(line)
	if len(f) < 4 || f[0] != "@@" || f[3] != "@@" {
		return Hunk{}, errors.New("unexpected hunk header")
	}
	start, count, hasCount := strings.Cut(strings.TrimPrefix(f[2], "+"), ",")
	h := Hunk{Count: 1}
	var err error
	if h.Start, err = strconv.Atoi(start); err != nil || !strings.HasPrefix(f[2], "+") || h.Start < 0 {
		return Hunk{}, errors.New("unexpected hunk header")
	}
	if hasCount {
		if h.Count, err = strconv.Atoi(count); err != nil || h.Count < 0 {
			return Hunk{}, errors.New("unexpected hunk header")
		}
	}
	return h, nil
}

// diffHeaderPath is the path of a "diff --git a/<path> b/<path>" header
// without renames, rest being what follows "diff --git ": both names are
// the same path, each quoted C-style when it holds a quote, a backslash or
// a control character (core.quotePath=false).
func diffHeaderPath(rest string) (string, error) {
	if strings.HasPrefix(rest, `"`) {
		end := 1
		for end < len(rest) && rest[end] != '"' {
			if rest[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(rest) {
			return "", errors.New("unterminated quoted name")
		}
		name, err := strconv.Unquote(rest[:end+1])
		if err != nil {
			return "", fmt.Errorf("quoted name: %w", err)
		}
		if p, ok := strings.CutPrefix(name, "a/"); ok && p != "" {
			return p, nil
		}
		return "", errors.New("unexpected file header")
	}
	n := (len(rest) - len("a/ b/")) / 2
	if n <= 0 || rest[:2] != "a/" || rest[2+n:] != " b/"+rest[2:2+n] {
		return "", errors.New("unexpected file header")
	}
	return rest[2 : 2+n], nil
}

// Use is a file that names a word, and the first line that does (WordUses).
type Use struct {
	Path string
	Line int
}

// WordUses lists the files of rev that name word as a whole word, each
// with its first line that does, in git's order: one `git grep -w -F` on
// rev's tree, which reads the object store only (an uncommitted edit of the
// work tree never counts), binary files left out. word is taken literally,
// one line; nil when no file names it. rev must name a commit.
func (c *Client) WordUses(ctx context.Context, dir, rev, word string) ([]Use, error) {
	if err := checkCommit(rev); err != nil {
		return nil, err
	}
	if word == "" || strings.ContainsAny(word, "\n\x00") {
		return nil, errors.New("gitx: grep: a word must be one non-empty line")
	}
	res, err := c.git(ctx, dir, call{label: "grep -w " + rev, noExit: 1},
		"grep", "--no-recurse-submodules", "--no-color", "--line-number", "--no-column", "-I", "-o", "-z", "-w", "-F",
		"--max-count=1", "-e", word, rev, "--")
	if err != nil {
		if code, ok := exitCode(err); ok && code == 1 {
			return nil, nil // no file names it
		}
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: grep -w %s: more than %d bytes of matches", rev, execx.MaxOutput)
	}
	// Each match is "<rev>:<file> NUL <line> NUL <word> LF" (-n -o -z): -o
	// prints a line's every match, so a file can come more than once.
	var out []Use
	prefix := rev + ":"
	for rest := string(res.Stdout); rest != ""; {
		name, after, ok := strings.Cut(rest, "\x00")
		num, after, ok2 := strings.Cut(after, "\x00")
		_, tail, ok3 := strings.Cut(after, "\n")
		file, ok4 := strings.CutPrefix(name, prefix)
		n, err := strconv.Atoi(num)
		if !ok || !ok2 || !ok3 || !ok4 || file == "" || err != nil {
			return nil, fmt.Errorf("gitx: grep -w %s: unexpected output", rev)
		}
		if len(out) == 0 || out[len(out)-1].Path != file {
			out = append(out, Use{Path: file, Line: n})
		}
		rest = tail
	}
	return out, nil
}

// DirFiles lists the files directly in each of dirs (relative to the top,
// "." for the top itself) at rev, never a directory, a submodule or a file
// of a subdirectory: one `git ls-tree` of rev's tree, each file as its path
// from the top, in git's order. nil when there are none or dirs is empty.
// rev must name a commit.
func (c *Client) DirFiles(ctx context.Context, dir, rev string, dirs ...string) ([]string, error) {
	if err := checkCommit(rev); err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, nil
	}
	args := []string{"ls-tree", "-z", "--full-tree", rev, "--"}
	want := map[string]bool{}
	for _, d := range dirs {
		d = path.Clean(d)
		want[d] = true
		args = append(args, d+"/")
	}
	res, err := c.git(ctx, dir, call{label: "ls-tree " + rev}, args...)
	if err != nil {
		return nil, err
	}
	// Each entry is "<mode> SP <type> SP <oid> TAB <path>" NUL. With the
	// top among dirs, ls-tree also lists the entries of a directory it
	// walks into for another of them: only those of dirs count.
	var out []string
	for rec := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if rec == "" {
			continue
		}
		meta, file, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || file == "" {
			return nil, fmt.Errorf("gitx: ls-tree %s: unexpected entry", rev)
		}
		if f[1] == "blob" && want[path.Dir(file)] {
			out = append(out, file)
		}
	}
	return out, nil
}

// ErrFileTooLarge is FileAt's error for a file past execx.MaxOutput.
var ErrFileTooLarge = errors.New("gitx: file too large")

// FileAt is the content of file (a clean path from the top) as rev has it:
// `git cat-file blob <rev>:<file>`, the object store's bytes without
// textconv or filters, never the work tree's. A file past execx.MaxOutput
// is ErrFileTooLarge; a path rev lacks fails. rev must name a commit.
func (c *Client) FileAt(ctx context.Context, dir, rev, file string) (string, error) {
	if err := checkCommit(rev); err != nil {
		return "", err
	}
	if file == "" || path.Clean(file) != file || path.IsAbs(file) || file == ".." || strings.HasPrefix(file, "../") ||
		strings.ContainsAny(file, "\n\x00") {
		return "", errors.New("gitx: cat-file: the file must be a clean path from the top, one line")
	}
	res, err := c.git(ctx, dir, call{label: "cat-file blob " + rev}, "cat-file", "blob", rev+":"+file)
	if err != nil {
		return "", err
	}
	if res.Truncated {
		return "", fmt.Errorf("gitx: cat-file blob %s: %w", rev, ErrFileTooLarge)
	}
	return string(res.Stdout), nil
}
