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

// Tree is what Tree reads of a commit's tree.
type Tree struct {
	// Links are its symbolic links: each link's path from the top, with
	// its text (the path it names as git stores it: relative to the link's
	// directory, or absolute).
	Links map[string]string
	// Files are the paths of its regular files (a blob of mode 100644 or
	// 100755) that the caller's keep accepted, in git's order.
	Files []string
}

// Symlinks are the symbolic links of rev's tree (Tree's Links), from the
// object store (never the work tree): an empty map when it has none.
func (c *Client) Symlinks(ctx context.Context, dir, rev string) (map[string]string, error) {
	t, err := c.Tree(ctx, dir, rev, nil)
	return t.Links, err
}

// Tree reads rev's tree from the object store (never the work tree): its
// symbolic links with their texts, and the paths of its regular files keep
// accepts (nil keeps none). One `git ls-tree -r` of the whole tree and,
// when it has links, one `git cat-file --batch` of their blobs. A listing
// past execx.MaxOutput is ErrTreeTooLarge, and a commit the clone lacks
// fails. rev must name a commit.
func (c *Client) Tree(ctx context.Context, dir, rev string, keep func(path string) bool) (Tree, error) {
	if err := checkCommit(rev); err != nil {
		return Tree{}, err
	}
	res, err := c.git(ctx, dir, call{label: "ls-tree -r " + rev}, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return Tree{}, err
	}
	if res.Truncated {
		return Tree{}, fmt.Errorf("gitx: ls-tree -r %s: %w", rev, ErrTreeTooLarge)
	}
	// Each entry is "<mode> SP <type> SP <oid> TAB <path>" NUL.
	var t Tree
	var paths, oids []string
	for rec := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if rec == "" {
			continue
		}
		meta, file, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || file == "" {
			return Tree{}, fmt.Errorf("gitx: ls-tree -r %s: unexpected entry", rev)
		}
		switch {
		case f[0] == symlinkMode:
			paths, oids = append(paths, file), append(oids, f[2])
		case (f[0] == "100644" || f[0] == "100755") && keep != nil && keep(file):
			t.Files = append(t.Files, file)
		}
	}
	t.Links, err = c.linkTexts(ctx, dir, rev, paths, oids)
	return t, err
}

// linkTexts reads the blobs oids of the links paths of rev's tree (one
// `git cat-file --batch`): each path with its text; an empty map for none.
func (c *Client) linkTexts(ctx context.Context, dir, rev string, paths, oids []string) (map[string]string, error) {
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

// Blobs reads the files paths name in rev's tree (each from the top, as
// git stores it) from the object store, in one `git cat-file --batch` of
// "<rev>:<path>": each regular file or link found with its content (a
// link's is its text); a path rev lacks, or names a directory, is left out.
// A path with a line feed cannot be asked and fails, and so does an output
// past execx.MaxOutput (ErrTreeTooLarge). rev must name a commit.
func (c *Client) Blobs(ctx context.Context, dir, rev string, paths []string) (map[string]string, error) {
	if err := checkCommit(rev); err != nil {
		return nil, err
	}
	out := map[string]string{}
	if len(paths) == 0 {
		return out, nil
	}
	names := make([]string, len(paths))
	for i, p := range paths {
		if p == "" || strings.ContainsAny(p, "\n\r\x00") {
			return nil, fmt.Errorf("gitx: cat-file %s: a path git's batch cannot ask", rev)
		}
		names[i] = rev + ":" + p
	}
	k := call{label: "cat-file --batch " + rev}
	cmd := gitCommand(dir, k, []string{"cat-file", "--batch"})
	cmd.Stdin = []byte(strings.Join(names, "\n") + "\n")
	res, err := c.run.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("gitx: %s: %w", k.label, err)
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: %s: %w", k.label, ErrTreeTooLarge)
	}
	// Each object is "<oid> SP <type> SP <size> LF <content> LF", or
	// "<name> SP missing LF", in the order asked.
	data := res.Stdout
	for i, name := range names {
		header, rest, ok := bytes.Cut(data, []byte("\n"))
		if !ok {
			return nil, fmt.Errorf("gitx: %s: unexpected end", k.label)
		}
		if string(header) == name+" missing" {
			data = rest
			continue
		}
		f := strings.Fields(string(header))
		if len(f) != 3 {
			return nil, fmt.Errorf("gitx: %s: unexpected header for %s", k.label, paths[i])
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size < 0 || size >= len(rest) || rest[size] != '\n' {
			return nil, fmt.Errorf("gitx: %s: unexpected size of %s", k.label, paths[i])
		}
		if f[1] == "blob" {
			out[paths[i]] = string(rest[:size])
		}
		data = rest[size+1:]
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
	links, _, err := c.TrackedFiles(ctx, dir, paths...)
	return links, err
}

// TrackedFiles is TrackedSymlinks that also gives the regular files of
// dir's index that paths name (a blob of mode 100644 or 100755), each once,
// in git's order: the instruction files and project files a session reads
// for the files they import.
func (c *Client) TrackedFiles(ctx context.Context, dir string, paths ...string) (links, files []string, err error) {
	specs, err := pathspecs(paths)
	if err != nil {
		return nil, nil, err
	}
	res, err := c.git(ctx, dir, call{label: "ls-files -s"}, append([]string{"ls-files", "-s", "-z", "--"}, specs...)...)
	if err != nil {
		return nil, nil, err
	}
	// Each entry is "<mode> SP <oid> SP <stage> TAB <path>" NUL.
	for rec := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if rec == "" {
			continue
		}
		meta, file, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || file == "" {
			return nil, nil, errors.New("gitx: ls-files -s: unexpected entry")
		}
		switch {
		case f[0] == symlinkMode && !slices.Contains(links, file):
			links = append(links, file)
		case (f[0] == "100644" || f[0] == "100755") && !slices.Contains(files, file):
			files = append(files, file)
		}
	}
	return links, files, nil
}
