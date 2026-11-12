package gitx

import (
	"bytes"
	"context"
	sha1hash "crypto/sha1" //nolint:gosec // git names its objects by SHA-1: VerifyObjects checks a name, it keeps no secret
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// gitDirPrefix marks a dir that names a bare git directory of gitx's own
// (isolate): gitCommand runs git there with --git-dir, never -C, since git
// finds a bare repository by discovery only where the global config's
// safe.bareRepository allows it (explicit refuses it, and git 3.0 makes
// explicit the default). No real directory's path starts with it.
const gitDirPrefix = "gitdir:"

// shallowMax caps the shallow file copyShallow copies: 41 bytes a line,
// so about 100,000 cut commits.
const shallowMax = 4 << 20

// Isolated runs fn with iso, a bare repository of its own whose objects
// are dir's (isolate) and that has none of dir's config, attributes,
// hooks, refs or index: a read there runs no merge driver, diff driver or
// attribute of the clone's, which an agent's pane can write. Every Client
// method takes iso as its dir; only a full commit id names a commit there.
// The repository goes when fn returns.
func (c *Client) Isolated(ctx context.Context, dir string, fn func(iso string) error) error {
	iso, done, err := c.isolate(ctx, dir)
	if err != nil {
		return fmt.Errorf("gitx: %w", err)
	}
	defer done()
	return fn(iso)
}

// isolate makes a bare repository of its own in a new temporary directory,
// whose objects/info/alternates names dir's common objects directory, with
// a shallow clone's list of cut commits copied (copyShallow), so git there
// still finds where the clone's history ends. It returns the repository as
// a dir for Client's methods (gitDirPrefix) and the func that removes it.
func (c *Client) isolate(ctx context.Context, dir string) (string, func(), error) {
	res, err := c.git(ctx, dir, call{label: "rev-parse --git-common-dir"}, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", nil, fmt.Errorf("the clone's git directory: %w", err)
	}
	common := res.Out()
	tmp, err := os.MkdirTemp("", "magnum-git-")
	if err != nil {
		return "", nil, err
	}
	done := func() { _ = os.RemoveAll(tmp) }
	fail := func(what string, err error) (string, func(), error) {
		done()
		return "", nil, fmt.Errorf("a repository of its own: %s: %w", what, err)
	}
	if _, err := c.git(ctx, tmp, call{label: "init --bare", mutates: true}, "init", "--quiet", "--bare", "--template=", tmp); err != nil {
		return fail("init", err)
	}
	info := filepath.Join(tmp, "objects", "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		return fail("objects", err)
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.Join(common, "objects")+"\n"), 0o600); err != nil {
		return fail("alternates", err)
	}
	if err := copyShallow(common, tmp); err != nil {
		return fail("the clone's shallow list", err)
	}
	return gitDirPrefix + tmp, done, nil
}

// copyShallow copies the shallow file of the git directory common (the
// commits whose parents a shallow clone lacks) to the git directory to,
// when there is one: without it git in to would look for those parents
// and fail. It opens the file with no link followed and without waiting
// (a FIFO's open would wait for a writer), and copies a regular file of at
// most shallowMax bytes alone. A wrong list can only make git send less,
// which the remote refuses.
func copyShallow(common, to string) error {
	from := filepath.Join(common, "shallow")
	src, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	switch {
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", from)
	case info.Size() > shallowMax:
		return fmt.Errorf("%s has more than %d bytes", from, shallowMax)
	}
	data, err := io.ReadAll(io.LimitReader(src, shallowMax+1))
	switch {
	case err != nil:
		return err
	case len(data) > shallowMax:
		return fmt.Errorf("%s has more than %d bytes", from, shallowMax)
	}
	return os.WriteFile(filepath.Join(to, "shallow"), data, 0o600)
}

// ErrObjectMismatch is VerifyObjects' answer for an object whose content
// is not what its name says.
var ErrObjectMismatch = errors.New("gitx: the content does not match the name")

// VerifyObjects hashes again, in Go, every object a read of path (a
// directory, "/" between its parts, from the top) at each of commits (full
// ids) goes through: the commit, its root tree, the tree of each part of
// path, and every tree and blob under it. git reads an object as its store
// has it and never hashes it again (a loose object rewritten, a pack whose
// index names another object's place), while GitHub has the real one, so a
// diff git shows of a forged object is not what a push of it gives GitHub.
// An object whose content is not its name is ErrObjectMismatch, named; a
// missing one is an error too. A commit that has no path has nothing more
// to check, and a gitlink under it (a submodule's commit) is no object
// here.
func (c *Client) VerifyObjects(ctx context.Context, dir, path string, commits ...string) error {
	for _, rev := range commits {
		if !isOID(rev) {
			return fmt.Errorf("gitx: verify objects: %q is not a full commit id", rev)
		}
	}
	objs, err := c.catObjects(ctx, dir, commits)
	if err != nil {
		return err
	}
	var trees []string
	for _, rev := range commits {
		o := objs[rev]
		tree, ok := commitTree(o)
		if !ok {
			return fmt.Errorf("gitx: verify objects: %s is a %s with no tree", rev, o.typ)
		}
		trees = append(trees, tree)
	}
	for part := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		if part == "" {
			continue
		}
		if trees, err = c.subtrees(ctx, dir, trees, func(e treeEntry) bool { return e.name == part }); err != nil {
			return err
		}
	}
	for len(trees) > 0 {
		var blobs []string
		if trees, err = c.subtrees(ctx, dir, trees, func(e treeEntry) bool {
			if e.mode != "40000" && e.mode != "160000" {
				blobs = append(blobs, e.oid)
			}
			return true
		}); err != nil {
			return err
		}
		got, err := c.catObjects(ctx, dir, blobs)
		if err != nil {
			return err
		}
		for _, oid := range blobs {
			if got[oid].typ != "blob" {
				return fmt.Errorf("gitx: verify objects: %s is a %s, not a blob", oid, got[oid].typ)
			}
		}
	}
	return nil
}

// subtrees reads trees (verified, catObjects) and returns the trees their
// entries that keep says yes to name, a gitlink never.
func (c *Client) subtrees(ctx context.Context, dir string, trees []string, keep func(treeEntry) bool) ([]string, error) {
	objs, err := c.catObjects(ctx, dir, trees)
	if err != nil {
		return nil, err
	}
	var next []string
	for _, oid := range slices.Compact(slices.Sorted(slices.Values(trees))) {
		o := objs[oid]
		if o.typ != "tree" {
			return nil, fmt.Errorf("gitx: verify objects: %s is a %s, not a tree", oid, o.typ)
		}
		entries, err := parseTree(o.body, len(oid)/2)
		if err != nil {
			return nil, fmt.Errorf("gitx: verify objects: tree %s: %w", oid, err)
		}
		for _, e := range entries {
			if keep(e) && e.mode == "40000" {
				next = append(next, e.oid)
			}
		}
	}
	return next, nil
}

// object is an object as git's store has it: its type and its content.
type object struct {
	typ  string
	body []byte
}

// catObjects reads oids with `git cat-file --batch` and hashes each again:
// an object whose content is not its name is ErrObjectMismatch.
func (c *Client) catObjects(ctx context.Context, dir string, oids []string) (map[string]object, error) {
	oids = slices.Compact(slices.Sorted(slices.Values(oids)))
	if len(oids) == 0 {
		return nil, nil
	}
	in := []byte(strings.Join(oids, "\n") + "\n")
	res, err := c.git(ctx, dir, call{label: "cat-file --batch", stdin: in}, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, errors.New("gitx: cat-file --batch: the objects are too long to check")
	}
	out, objs := res.Stdout, make(map[string]object, len(oids))
	for _, oid := range oids {
		line, rest, ok := bytes.Cut(out, []byte("\n"))
		fields := strings.Fields(string(line))
		if !ok || len(fields) != 3 || fields[0] != oid {
			return nil, fmt.Errorf("gitx: cat-file --batch: no object %s (%.80q)", oid, line)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 || size+1 > len(rest) || rest[size] != '\n' {
			return nil, fmt.Errorf("gitx: cat-file --batch: an unreadable answer for %s", oid)
		}
		o := object{typ: fields[1], body: rest[:size]}
		if got := objectName(o, len(oid)); got != oid {
			return nil, fmt.Errorf("gitx: %s %s: its content hashes to %s: %w", o.typ, oid, got, ErrObjectMismatch)
		}
		objs[oid], out = o, rest[size+1:]
	}
	return objs, nil
}

// objectName is the name git gives o: the SHA-1 (a 40-hex name) or
// SHA-256 of its header and content.
func objectName(o object, hexLen int) string {
	h := sha256.New()
	if hexLen == 40 {
		h = sha1hash.New() //nolint:gosec // git's object name, see the import
	}
	fmt.Fprintf(h, "%s %d\x00", o.typ, len(o.body))
	h.Write(o.body)
	return hex.EncodeToString(h.Sum(nil))
}

// commitTree is the tree a commit object names on its first line.
func commitTree(o object) (string, bool) {
	line, _, _ := bytes.Cut(o.body, []byte("\n"))
	tree, ok := strings.CutPrefix(string(line), "tree ")
	return tree, o.typ == "commit" && ok && isOID(tree)
}

// treeEntry is one entry of a tree object.
type treeEntry struct {
	mode, name, oid string
}

// parseTree reads a tree object's entries: "<mode> <name>\0" and the
// entry's raw id of size bytes.
func parseTree(body []byte, size int) ([]treeEntry, error) {
	var out []treeEntry
	for len(body) > 0 {
		head, rest, ok := bytes.Cut(body, []byte{0})
		mode, name, okName := strings.Cut(string(head), " ")
		if !ok || !okName || len(rest) < size {
			return nil, errors.New("an unreadable entry")
		}
		out = append(out, treeEntry{mode: mode, name: name, oid: hex.EncodeToString(rest[:size])})
		body = rest[size:]
	}
	return out, nil
}
