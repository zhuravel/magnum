package gitx

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
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
	for line := range strings.SplitSeq(string(res.Stdout), "\n") {
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

// NotTracked lists what git tracks none of in the work tree whose top is
// dir (`git ls-files --others --directory`): untracked and ignored files
// alike, and a directory git tracks nothing in as one entry ending in "/",
// relative to the top, in git's order. A switch that writes one of their
// paths loses it, which the factory reads before it switches a copy. It is
// read-only and does not take the index lock.
func (c *Client) NotTracked(ctx context.Context, dir string) ([]string, error) {
	res, err := c.git(ctx, dir, call{label: "ls-files --others --directory"}, "ls-files", "-z", "--others", "--directory")
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: ls-files --others in %s: more than %d bytes", dir, execx.MaxOutput)
	}
	var out []string
	for p := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// UntrackedFiles lists the untracked files of the work tree at dir that git
// does not ignore (`git ls-files --others --exclude-standard`), those at or
// under paths (each taken literally from the top; none: the whole tree), one
// by one: a wholly untracked directory, which StatusPaths names once, as each
// of its files (a repository inside the work tree stays one entry, ending in
// /). Relative to the top, sorted. It is read-only and does not take the
// index lock.
func (c *Client) UntrackedFiles(ctx context.Context, dir string, paths ...string) ([]string, error) {
	var specs []string
	if len(paths) > 0 {
		var err error
		if specs, err = literal(paths); err != nil {
			return nil, fmt.Errorf("gitx: ls-files --others: %w", err)
		}
	}
	res, err := c.git(ctx, dir, call{label: "ls-files --others --exclude-standard"},
		append([]string{"ls-files", "-z", "--full-name", "--others", "--exclude-standard", "--"}, specs...)...)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: ls-files --others in %s: more than %d bytes", dir, execx.MaxOutput)
	}
	var out []string
	for p := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// DiffHead is the binary patch of paths of the work tree at dir against
// HEAD (`git diff --binary HEAD`, each path taken literally from the top):
// each path as the work tree has it, which `git apply` puts back in a work
// tree as HEAD has them. A version staged and then changed in the work tree
// is not in it. Its prefixes are a/ and b/ whatever the
// config says, and it runs no external diff or textconv. A patch over
// execx.MaxOutput is an error, never part of one. It is read-only and does
// not take the index lock.
func (c *Client) DiffHead(ctx context.Context, dir string, paths ...string) ([]byte, error) {
	specs, err := literal(paths)
	if err != nil {
		return nil, fmt.Errorf("gitx: diff HEAD: %w", err)
	}
	res, err := c.git(ctx, dir, call{label: "diff --binary HEAD"},
		append([]string{"diff", "--binary", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/",
			"HEAD", "--"}, specs...)...)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: diff HEAD in %s: more than %d bytes", dir, execx.MaxOutput)
	}
	return res.Stdout, nil
}

// DiffHeadTo writes the binary patch of the work tree at dir against HEAD
// to file, an absolute path git creates or truncates, as DiffHead makes it
// but with no rename pairs (`git diff --binary --no-renames HEAD`): a
// deletion and an addition each patch their own path, so the patch of one
// path is that path's alone. paths are taken literally from the top; none
// is every tracked change. Nothing bounds it but the disk: the caller reads
// its size. It reads the work tree only and does not take the index lock.
func (c *Client) DiffHeadTo(ctx context.Context, dir, file string, paths ...string) error {
	if err := checkAbsPath(file); err != nil {
		return fmt.Errorf("gitx: diff HEAD: %w", err)
	}
	var specs []string
	if len(paths) > 0 {
		var err error
		if specs, err = literal(paths); err != nil {
			return fmt.Errorf("gitx: diff HEAD: %w", err)
		}
	}
	_, err := c.git(ctx, dir, call{label: "diff --binary HEAD --output", timeout: worktreeTimeout},
		append([]string{"diff", "--binary", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/",
			"--dst-prefix=b/", "--output=" + file, "HEAD", "--"}, specs...)...)
	return err
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
