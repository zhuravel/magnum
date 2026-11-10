package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ErrTreeTooLarge is Symlinks' error for a tree, and ChangedNames' for a
// list of names, whose listing passes execx.MaxOutput: a link or a path it
// would leave out may be one that matters.
var ErrTreeTooLarge = errors.New("gitx: tree too large to list")

// symlinkMode is git's mode of a symbolic link in a tree.
const symlinkMode = "120000"

// Symlinks are the symbolic links of rev's tree, from the object store
// (never the work tree): each link's path from the top, with its text,
// the path it names as git stores it (relative to the link's directory,
// or absolute). One `git ls-tree -r` of the whole tree and, when it has
// links, one `git cat-file --batch` of their blobs; an empty map when it
// has none. A listing past execx.MaxOutput is ErrTreeTooLarge, and a
// commit the clone lacks fails. rev must name a commit.
func (c *Client) Symlinks(ctx context.Context, dir, rev string) (map[string]string, error) {
	if err := checkCommit(rev); err != nil {
		return nil, err
	}
	res, err := c.git(ctx, dir, call{label: "ls-tree -r " + rev}, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: ls-tree -r %s: %w", rev, ErrTreeTooLarge)
	}
	// Each entry is "<mode> SP <type> SP <oid> TAB <path>" NUL.
	var paths, oids []string
	for rec := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if rec == "" {
			continue
		}
		meta, file, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || file == "" {
			return nil, fmt.Errorf("gitx: ls-tree -r %s: unexpected entry", rev)
		}
		if f[0] == symlinkMode {
			paths, oids = append(paths, file), append(oids, f[2])
		}
	}
	out := make(map[string]string, len(paths))
	if len(oids) == 0 {
		return out, nil
	}
	k := call{label: "cat-file --batch " + rev}
	cmd := gitCommand(dir, k, []string{"cat-file", "--batch"})
	cmd.Stdin = []byte(strings.Join(oids, "\n") + "\n")
	blobs, err := c.run.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("gitx: %s: %w", k.label, err)
	}
	if blobs.Truncated {
		return nil, fmt.Errorf("gitx: %s: %w", k.label, ErrTreeTooLarge)
	}
	// Each object is "<oid> SP blob SP <size> LF <content> LF", in the
	// order asked.
	data := blobs.Stdout
	for i, oid := range oids {
		header, rest, ok := bytes.Cut(data, []byte("\n"))
		f := strings.Fields(string(header))
		if !ok || len(f) != 3 || f[0] != oid || f[1] != "blob" {
			return nil, fmt.Errorf("gitx: %s: unexpected object for %s", k.label, oid)
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size < 0 || size >= len(rest) || rest[size] != '\n' {
			return nil, fmt.Errorf("gitx: %s: unexpected size of %s", k.label, oid)
		}
		out[paths[i]], data = string(rest[:size]), rest[size+1:]
	}
	return out, nil
}

// TrackedSymlinks are the symbolic links of dir's index that paths name
// (pathspecs: from the top, in any case, a path taken literally or one of
// AnyDir), as the checkout has them tracked: `git ls-files -s` of those
// paths, each link's path from the top, in git's order; nil when there
// are none. A session's checkout compares what such a link leads to, which
// may lie anywhere in it (app/CLAUDE.md -> ../docs/app.md).
func (c *Client) TrackedSymlinks(ctx context.Context, dir string, paths ...string) ([]string, error) {
	specs, err := pathspecs(paths)
	if err != nil {
		return nil, err
	}
	res, err := c.git(ctx, dir, call{label: "ls-files -s"}, append([]string{"ls-files", "-s", "-z", "--"}, specs...)...)
	if err != nil {
		return nil, err
	}
	// Each entry is "<mode> SP <oid> SP <stage> TAB <path>" NUL.
	var out []string
	for rec := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if rec == "" {
			continue
		}
		meta, file, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || file == "" {
			return nil, errors.New("gitx: ls-files -s: unexpected entry")
		}
		if f[0] == symlinkMode && !slices.Contains(out, file) {
			out = append(out, file)
		}
	}
	return out, nil
}
