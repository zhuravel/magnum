package gitx

import (
	"context"
	"strings"
)

// Status summarizes `git status --porcelain` of a working tree. Ignored files
// (.mise.local.toml, tmp/) are not counted.
type Status struct {
	Tracked   int // entries with changes to tracked files (modified, added, deleted, renamed, conflicted)
	Untracked int // untracked files or directories
}

// Dirty reports whether anything differs from HEAD, untracked files included.
func (s Status) Dirty() bool { return s.Tracked+s.Untracked > 0 }

// UntrackedOnly reports whether the only dirt is untracked files.
func (s Status) UntrackedOnly() bool { return s.Untracked > 0 && s.Tracked == 0 }

// Status inspects the working tree at dir without taking the index lock.
func (c *Client) Status(ctx context.Context, dir string) (Status, error) {
	res, err := c.git(ctx, dir, call{label: "status"}, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return Status{}, err
	}
	var st Status
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "!! "):
		case strings.HasPrefix(line, "?? "):
			st.Untracked++
		default:
			st.Tracked++
		}
	}
	return st, nil
}

// StatusEntry is one record of `git status --porcelain=v1 -z`.
type StatusEntry struct {
	// X and Y are the two status letters: X for the index, Y for the work
	// tree (' ' unchanged, 'M', 'A', 'D', 'R', 'C', 'U' unmerged, '?' for an
	// untracked file, which has both letters '?').
	X, Y byte
	// Path is relative to the top level of the work tree, not to the directory
	// StatusPaths was given. In -z mode it is raw: spaces, newlines and
	// non-ASCII bytes are not quoted.
	Path string
	// OrigPath is the source of a rename or copy; empty for every other entry.
	OrigPath string
}

// Untracked reports whether the entry is an untracked file or directory.
func (e StatusEntry) Untracked() bool { return e.X == '?' && e.Y == '?' }

// Ignored reports whether the entry is an ignored file. StatusPaths never
// asks for ignored files, so it never returns such an entry.
func (e StatusEntry) Ignored() bool { return e.X == '!' && e.Y == '!' }

// StatusPaths lists the changes in the working tree at dir: tracked changes
// and untracked files (`--untracked-files=normal`, so a wholly untracked
// directory is one entry). Ignored files (.mise.local.toml, tmp/) are not
// requested. It is read-only and does not take the index lock.
func (c *Client) StatusPaths(ctx context.Context, dir string) ([]StatusEntry, error) {
	res, err := c.git(ctx, dir, call{label: "status paths"}, "status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		return nil, err
	}
	return parseStatusZ(res.Stdout), nil
}

// parseStatusZ parses `git status --porcelain=v1 -z`: NUL-terminated "XY path"
// records, where a rename or copy (R or C in either column) is followed by a
// second NUL-terminated field with its source path.
func parseStatusZ(out []byte) []StatusEntry {
	var entries []StatusEntry
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 || f[2] != ' ' {
			continue
		}
		e := StatusEntry{X: f[0], Y: f[1], Path: f[3:]}
		if (e.X == 'R' || e.X == 'C' || e.Y == 'R' || e.Y == 'C') && i+1 < len(fields) {
			i++
			e.OrigPath = fields[i]
		}
		entries = append(entries, e)
	}
	return entries
}
