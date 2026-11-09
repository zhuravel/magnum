package staging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// The timeouts of a deploy's git commands. A merge in the treeless mirror
// fetches the trees and blobs it needs from origin (a lazy fetch), and so
// may a push (the objects origin lacks).
const (
	mergeTimeout = 10 * time.Minute
	pushTimeout  = 10 * time.Minute
)

// stagingBranchRe is what a deploy's branch must match: staging-<name>,
// config.StagingBranchPattern's rule (the package does not import config).
var stagingBranchRe = regexp.MustCompile(`^staging-[a-z0-9-]+$`)

// pushSpecRe is the one refspec a deploy pushes: a full sha onto a staging
// branch, never forced (no +), never a delete (an empty source).
var pushSpecRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64}):refs/heads/(staging-[a-z0-9-]+)$`)

// The refs that hold an effect's merge in the mirror for its life, so no
// gc or prune takes it before the push: a resolver's merge, once its
// objects are written into the mirror (handOver), is kept at
// refs/magnum/resolved/<effect>, a clean merge at refs/magnum/deploy/<effect>.
// Both go when the effect ends.
const (
	resolvedPrefix = "refs/magnum/resolved/"
	mergePrefix    = "refs/magnum/deploy/"
)

// blobModes are the modes a resolved path may have in the resolver's merge:
// a file, an executable file, a symlink.
var blobModes = []string{"100644", "100755", "120000"}

// errObjectTooLarge is srcObject's error for an object over execx's
// MaxOutput, which no git command's output can carry whole.
var errObjectTooLarge = fmt.Errorf("the object is over %d MiB", execx.MaxOutput>>20)

// errGitTooOld is srcEntries' error when git's cat-file lacks -Z (git 2.42)
// or the %(objectmode) atom (git 2.51): no try with this git can pass.
var errGitTooOld = errors.New(oldGit(""))

// oldGit is the why of a git too old for the hand-off, version its `git
// version` ("" when it could not be read).
func oldGit(version string) string {
	this := "this git is older"
	if v, ok := strings.CutPrefix(version, "git version "); ok {
		this = "this git is " + oneLine(v)
	}
	return "the hand-off needs git 2.51 or later (cat-file's %(objectmode)); " + this
}

// versionTimeout bounds `git version`.
const versionTimeout = 30 * time.Second

// gitVersion is `git version`'s answer, read once per Git with the
// mirror's scrubbed env (base, beside mirror) and versionTimeout; "" when
// it cannot be read, which the next call tries again.
func (g *execGit) gitVersion(ctx context.Context, mirror string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.version != "" {
		return g.version
	}
	parent := filepath.Dir(mirror)
	c := g.base(parent, filepath.Dir(parent), "version", versionTimeout, false, false, nil)
	c.Args = []string{"version"}
	res, err := g.run.Run(ctx, c)
	if v := strings.TrimSpace(res.Out()); err == nil && strings.HasPrefix(v, "git version ") && !strings.Contains(v, "\n") {
		g.version = v
	}
	return g.version
}

// gitAtLeast compares version, as `git version` prints it ("git version
// 2.54.0 (Apple Git-154)", "git version 2.45.1.windows.1"), with
// major.minor: 1 when it is that or later, -1 when earlier, 0 when its
// numbers cannot be read.
func gitAtLeast(version string, major, minor int) int {
	v, ok := strings.CutPrefix(version, "git version ")
	f := strings.Fields(v)
	if !ok || len(f) == 0 {
		return 0
	}
	parts := strings.SplitN(f[0], ".", 3)
	if len(parts) < 2 {
		return 0
	}
	ma, errA := strconv.Atoi(parts[0])
	mi, errB := strconv.Atoi(parts[1])
	switch {
	case errA != nil || errB != nil:
		return 0
	case ma > major || ma == major && mi >= minor:
		return 1
	}
	return -1
}

// treeEntry is a path's entry in a commit's tree: its mode, type and id;
// an empty Type is a path the tree does not have.
type treeEntry struct {
	Mode, Type, OID, Path string
}

// change is a path a tree changes against another, with git's status letter
// (A added, D deleted, M modified, T type changed).
type change struct {
	Status byte
	Path   string
}

// deployGit is the git a deploy runs beyond Git's reads, in the mirror but
// for the src reads of a resolver's repository, which never write there
// (execGit; Git implementations without it cannot deploy). Every revision
// is a full sha.
type deployGit interface {
	// mergeTree is `git merge-tree --write-tree` of tip and head: the
	// merged tree (with conflict markers in the conflicted files) and the
	// conflicted paths, nil when it merges cleanly. env is the fetch's: the
	// treeless mirror fetches the trees and blobs it needs.
	mergeTree(ctx context.Context, dir string, env map[string]string, tip, head string) (tree string, conflicts []string, err error)
	// treeDiff lists what tree b changes against tree a, recursively,
	// without rename detection.
	treeDiff(ctx context.Context, dir string, env map[string]string, a, b string) ([]change, error)
	// commitTree makes the commit of tree with parents and message, its
	// author and committer name and email.
	commitTree(ctx context.Context, dir, tree string, parents []string, message, name, email string) (string, error)
	// commitInfo reads a commit's tree and parents, in order.
	commitInfo(ctx context.Context, dir, sha string) (tree string, parents []string, err error)
	// objectType is the type of the object sha in the mirror (commit, tag,
	// tree, blob), "" when the mirror lacks it; it never fetches it.
	objectType(ctx context.Context, dir, sha string) (string, error)
	// keepMerge points the effect's merge ref at sha.
	keepMerge(ctx context.Context, dir, ref, sha string) error
	// isAncestor reports whether a is b or an ancestor of b.
	isAncestor(ctx context.Context, dir, a, b string) (bool, error)
	// srcType is the type of the object sha in the resolver's repository at
	// src (a git dir checkResolverDir passed, beside mirror), "" when it
	// lacks it.
	srcType(ctx context.Context, src, mirror, sha string) (string, error)
	// srcObject is the content of the object sha of type typ (commit, blob)
	// in src, as git stores it; errObjectTooLarge past MaxOutput.
	srcObject(ctx context.Context, src, mirror, typ, sha string) ([]byte, error)
	// srcEntries is the entry of each of paths in commit sha's tree in src,
	// in paths' order; a path the tree lacks has no Type. errGitTooOld when
	// git cannot answer.
	srcEntries(ctx context.Context, src, mirror, sha string, paths []string) ([]treeEntry, error)
	// srcTree lists the tree tree in src (its entries, Path a name), from
	// the root whatever src's config says of its work tree.
	srcTree(ctx context.Context, src, mirror, tree string) ([]treeEntry, error)
	// gitVersion is `git version`'s answer, read once (beside mirror); ""
	// when it cannot be read.
	gitVersion(ctx context.Context, mirror string) string
	// lsTree lists the tree tree in the mirror (its entries, Path a name),
	// never fetching it.
	lsTree(ctx context.Context, dir, tree string) ([]treeEntry, error)
	// writeObject writes data into the mirror as an object of type typ
	// (blob, commit) and returns its id.
	writeObject(ctx context.Context, dir, typ string, data []byte) (string, error)
	// buildTree writes the tree of base with each of entries put in at its
	// path, or removed when it has no Type, and returns its id. It builds
	// it in the temporary index at index, in the mirror, which goes before
	// (a killed run's) and after. Nothing is fetched: base is merge-tree's
	// tree, every subtree of which the mirror holds.
	buildTree(ctx context.Context, dir, index, base string, entries []treeEntry) (string, error)
	// deleteRef deletes one of the effect refs (resolved or merge); an
	// absent one is no error.
	deleteRef(ctx context.Context, dir, ref string) error
	// push pushes sha to origin's branch with env (PushEnv's), the argv
	// pushArgv builds and checkPushArgv passes; allowed says whether the
	// branch passes the deploy's allowlist.
	push(ctx context.Context, dir string, env map[string]string, sha, branch string, allowed func(branch string) bool) error
	// checkResolverDir is the git dir of the resolver's repository at dir,
	// or why it is refused.
	checkResolverDir(ctx context.Context, dir, mirror string) (string, error)
}

var _ deployGit = (*execGit)(nil)

// exitCode is err's exit code when git ran and exited non-zero, else -1.
func exitCode(err error) int {
	if ee, ok := errors.AsType[*execx.ExitError](err); ok {
		return ee.Code
	}
	return -1
}

func (g *execGit) mergeTree(ctx context.Context, dir string, env map[string]string, tip, head string) (string, []string, error) {
	if err := checkRevs(tip, head); err != nil {
		return "", nil, err
	}
	res, err := g.run.Run(ctx, g.cmd(dir, "merge-tree", mergeTimeout, true, true, env,
		"merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", "--end-of-options", tip, head))
	if err != nil && exitCode(err) != 1 {
		return "", nil, err
	}
	fields := strings.Split(strings.TrimSuffix(string(res.Stdout), "\x00"), "\x00")
	tree := fields[0]
	if !isOID(tree) {
		return "", nil, fmt.Errorf("merge-tree printed no tree (exit %d)", res.Code)
	}
	if err == nil {
		return tree, nil, nil
	}
	var conflicts []string
	for _, p := range fields[1:] {
		if p != "" && !slices.Contains(conflicts, p) {
			conflicts = append(conflicts, p)
		}
	}
	if len(conflicts) == 0 {
		return "", nil, errors.New("merge-tree reported a conflict without a conflicted path")
	}
	return tree, conflicts, nil
}

func (g *execGit) treeDiff(ctx context.Context, dir string, env map[string]string, a, b string) ([]change, error) {
	if err := checkRevs(a, b); err != nil {
		return nil, err
	}
	res, err := g.run.Run(ctx, g.cmd(dir, "diff-tree", mergeTimeout, false, true, env,
		"diff-tree", "-r", "--no-renames", "--name-status", "-z", "--end-of-options", a, b))
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, errors.New("diff-tree's output was cut")
	}
	fields := strings.Split(strings.TrimSuffix(string(res.Stdout), "\x00"), "\x00")
	var out []change
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "" {
			continue
		}
		out = append(out, change{Status: fields[i][0], Path: fields[i+1]})
	}
	return out, nil
}

func (g *execGit) commitTree(ctx context.Context, dir, tree string, parents []string, message, name, email string) (string, error) {
	if err := checkRevs(append([]string{tree}, parents...)...); err != nil {
		return "", err
	}
	if err := checkIdentity(name, email); err != nil {
		return "", err
	}
	args := []string{"commit-tree", "--no-gpg-sign"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-F", "-", tree)
	c := g.cmd(dir, "commit-tree", readTimeout, true, false, map[string]string{
		"GIT_AUTHOR_NAME": name, "GIT_AUTHOR_EMAIL": email,
		"GIT_COMMITTER_NAME": name, "GIT_COMMITTER_EMAIL": email,
	}, args...)
	c.Unset = append(c.Unset, "GIT_AUTHOR_DATE", "GIT_COMMITTER_DATE")
	c.Stdin = []byte(message + "\n")
	res, err := g.run.Run(ctx, c)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(res.Out())
	if !isOID(sha) {
		return "", fmt.Errorf("commit-tree printed %q, no sha", short(sha))
	}
	return sha, nil
}

func (g *execGit) commitInfo(ctx context.Context, dir, sha string) (string, []string, error) {
	if err := checkRevs(sha); err != nil {
		return "", nil, err
	}
	res, err := g.run.Run(ctx, g.cmd(dir, "commit info", readTimeout, false, false, nil, "cat-file", "commit", sha))
	if err != nil {
		return "", nil, err
	}
	tree, parents := commitFields(res.Stdout)
	if !isOID(tree) {
		return "", nil, fmt.Errorf("commit %s names no tree", short(sha))
	}
	return tree, parents, nil
}

// commitFields reads a commit object's tree and parents, in order, from its
// header.
func commitFields(raw []byte) (tree string, parents []string) {
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			break // the end of the header: the message follows
		}
		switch k, v, _ := strings.Cut(line, " "); k {
		case "tree":
			tree = v
		case "parent":
			parents = append(parents, v)
		}
	}
	return tree, parents
}

func (g *execGit) objectType(ctx context.Context, dir, sha string) (string, error) {
	if err := checkRevs(sha); err != nil {
		return "", err
	}
	res, err := g.run.Run(ctx, g.cmd(dir, "object type", readTimeout, false, false, nil, "cat-file", "-t", sha))
	switch {
	case err == nil:
		return strings.TrimSpace(res.Out()), nil
	case exitCode(err) > 0 && ctx.Err() == nil:
		return "", nil
	}
	return "", err
}

func (g *execGit) keepMerge(ctx context.Context, dir, ref, sha string) error {
	if !effectRef(ref) || !isOID(sha) {
		return fmt.Errorf("ref %q or sha %q refused", ref, sha)
	}
	_, err := g.run.Run(ctx, g.cmd(dir, "keep merge", readTimeout, true, false, nil, "update-ref", ref, sha))
	return err
}

func (g *execGit) isAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	if err := checkRevs(a, b); err != nil {
		return false, err
	}
	_, err := g.run.Run(ctx, g.cmd(dir, "is ancestor", readTimeout, false, false, nil, "merge-base", "--is-ancestor", "--end-of-options", a, b))
	switch {
	case err == nil:
		return true, nil
	case exitCode(err) == 1:
		return false, nil
	}
	return false, err
}

// resolvedRef is the mirror's ref of effect id's resolved merge.
func resolvedRef(id EffectID) string { return resolvedPrefix + string(id) }

// mergeRef is the mirror's ref of effect id's clean merge.
func mergeRef(id EffectID) string { return mergePrefix + string(id) }

// effectRef reports whether ref is one of an effect's refs.
func effectRef(ref string) bool {
	for _, p := range []string{resolvedPrefix, mergePrefix} {
		if id, ok := strings.CutPrefix(ref, p); ok && validID(EffectID(id)) {
			return true
		}
	}
	return false
}

// resolvedIndex is the temporary index of effect id's hand-off, in the
// mirror's own directory (dir): never the operator's, never the resolver's.
func resolvedIndex(dir string, id EffectID) string {
	return filepath.Join(dir, resolvedIndexPrefix+string(id)+".index")
}

// resolvedIndexPrefix starts the name of every hand-off's index.
const resolvedIndexPrefix = "magnum-resolve-"

// dropIndex removes the hand-off index at index and its lock, which a
// killed run may have left; absent ones are no error.
func dropIndex(index string) error {
	var errs []error
	for _, f := range []string{index + ".lock", index} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// srcCmd is `git --git-dir=<src> <gitOptions> args...` in the resolver's
// repository, which only reads it: in src's parent with no search for a
// repository above it, git's HOME the mirror root's (gitHome), no hook, no
// lazy fetch and no optional lock.
func (g *execGit) srcCmd(src, mirror, label string, args ...string) execx.Cmd {
	c := g.base(filepath.Dir(src), filepath.Dir(filepath.Dir(mirror)), label, readTimeout, false, false, nil)
	c.Args = slices.Concat([]string{"--git-dir=" + src}, gitOptions, args)
	c.Env["GIT_DIR"] = src
	return c
}

// checkSrc refuses a src that is no absolute path (checkResolverDir's git
// dir) and a sha that is no full sha.
func checkSrc(src, sha string) error {
	if !filepath.IsAbs(src) || strings.HasPrefix(src, "-") {
		return fmt.Errorf("the resolver's repository %q is no absolute path", src)
	}
	return checkRevs(sha)
}

func (g *execGit) srcType(ctx context.Context, src, mirror, sha string) (string, error) {
	if err := checkSrc(src, sha); err != nil {
		return "", err
	}
	c := g.srcCmd(src, mirror, "resolver object type", "cat-file", "-t", sha)
	c.Probe = true
	res, err := g.run.Run(ctx, c)
	switch {
	case err == nil:
		return strings.TrimSpace(res.Out()), nil
	case exitCode(err) > 0 && ctx.Err() == nil:
		return "", nil
	}
	return "", err
}

func (g *execGit) srcObject(ctx context.Context, src, mirror, typ, sha string) ([]byte, error) {
	if err := checkSrc(src, sha); err != nil {
		return nil, err
	}
	if typ != "commit" && typ != "blob" {
		return nil, fmt.Errorf("object type %q refused", typ)
	}
	res, err := g.run.Run(ctx, g.srcCmd(src, mirror, "resolver "+typ, "cat-file", typ, sha))
	switch {
	case err != nil:
		return nil, err
	case res.Truncated:
		return nil, errObjectTooLarge
	}
	return res.Stdout, nil
}

// srcEntries asks cat-file for <sha>:<path> of each path on stdin, NUL
// separated: no path reaches argv or a pathspec, and any byte a path holds
// but NUL is its own. cat-file answers "missing" both for a path the tree
// lacks and for an entry whose object src lacks; either is taken as
// removed, and a removal the resolver did not make only ever ends in the
// rebuilt tree's mismatch, which names the path.
func (g *execGit) srcEntries(ctx context.Context, src, mirror, sha string, paths []string) ([]treeEntry, error) {
	if err := checkSrc(src, sha); err != nil || len(paths) == 0 {
		return nil, err
	}
	var in bytes.Buffer
	for _, p := range paths {
		if p == "" || strings.ContainsRune(p, 0) {
			return nil, fmt.Errorf("path %q refused", p)
		}
		in.WriteString(sha + ":" + p + "\x00")
	}
	c := g.srcCmd(src, mirror, "resolver entries", "cat-file", "-Z", "--batch-check=%(objectmode) %(objecttype) %(objectname)")
	c.Stdin = in.Bytes()
	res, err := g.run.Run(ctx, c)
	// The atom's and the switch's names, which every git echoes in every
	// locale: "unknown format element: objectmode" (git 2.42 to 2.44), "bad
	// cat-file format: %(objectmode)" (2.45 to 2.50), "unknown switch `Z'".
	// gitVersion decides first; this is for a git whose version is unread.
	ee, exited := errors.AsType[*execx.ExitError](err)
	switch {
	case exited && ee.Code == 128 && strings.Contains(ee.Stderr, "objectmode"), exited && strings.Contains(ee.Stderr, "`Z'"):
		return nil, errGitTooOld
	case err != nil:
		return nil, err
	case res.Truncated:
		return nil, errors.New("cat-file's output was cut")
	}
	recs := strings.Split(strings.TrimSuffix(string(res.Stdout), "\x00"), "\x00")
	if len(recs) != len(paths) {
		return nil, fmt.Errorf("cat-file answered %d of %d paths", len(recs), len(paths))
	}
	out := make([]treeEntry, len(paths))
	for i, rec := range recs {
		out[i].Path = paths[i]
		if rec == sha+":"+paths[i]+" missing" {
			continue
		}
		switch f := strings.Fields(rec); {
		case len(f) == 2 && f[1] == "submodule" && isOID(f[0]): // a gitlink, whose commit src need not hold
			out[i].Mode, out[i].Type, out[i].OID = "160000", "commit", f[0]
		case len(f) == 3 && isOID(f[2]):
			out[i].Mode, out[i].Type, out[i].OID = f[0], f[1], f[2]
		default:
			return nil, fmt.Errorf("cat-file's answer for path %d is no entry", i+1)
		}
	}
	return out, nil
}

func (g *execGit) srcTree(ctx context.Context, src, mirror, tree string) ([]treeEntry, error) {
	if err := checkSrc(src, tree); err != nil {
		return nil, err
	}
	// --full-tree: a core.worktree above src's directory would otherwise
	// make ls-tree list the subdirectory that directory is of the work tree.
	res, err := g.run.Run(ctx, g.srcCmd(src, mirror, "resolver tree", "ls-tree", "-z", "--full-tree", "--end-of-options", tree))
	if err != nil {
		return nil, err
	}
	return parseTree(res)
}

func (g *execGit) lsTree(ctx context.Context, dir, tree string) ([]treeEntry, error) {
	if err := checkRevs(tree); err != nil {
		return nil, err
	}
	res, err := g.run.Run(ctx, g.cmd(dir, "tree", readTimeout, false, false, nil, "ls-tree", "-z", "--end-of-options", tree))
	if err != nil {
		return nil, err
	}
	return parseTree(res)
}

// parseTree reads `ls-tree -z`'s entries: <mode> <type> <id> TAB <name>,
// NUL separated.
func parseTree(res execx.Result) ([]treeEntry, error) {
	if res.Truncated {
		return nil, errors.New("ls-tree's output was cut")
	}
	var out []treeEntry
	for rec := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if rec == "" {
			continue
		}
		meta, name, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || !isOID(f[2]) || name == "" {
			return nil, errors.New("ls-tree printed an entry it should not")
		}
		out = append(out, treeEntry{Mode: f[0], Type: f[1], OID: f[2], Path: name})
	}
	return out, nil
}

func (g *execGit) writeObject(ctx context.Context, dir, typ string, data []byte) (string, error) {
	if typ != "commit" && typ != "blob" {
		return "", fmt.Errorf("object type %q refused", typ)
	}
	c := g.cmd(dir, "write "+typ, readTimeout, true, false, nil, "hash-object", "-w", "--no-filters", "-t", typ, "--stdin")
	c.Stdin = data
	res, err := g.run.Run(ctx, c)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(res.Out())
	if !isOID(id) {
		return "", fmt.Errorf("hash-object printed %q, no id", short(id))
	}
	return id, nil
}

func (g *execGit) buildTree(ctx context.Context, dir, index, base string, entries []treeEntry) (string, error) {
	if err := checkRevs(base); err != nil {
		return "", err
	}
	if filepath.Dir(index) != dir || !strings.HasPrefix(filepath.Base(index), resolvedIndexPrefix) {
		return "", fmt.Errorf("index %q is not the mirror's", index)
	}
	// A killed run's index, or its lock, is this effect's alone: the
	// mirror's flock serializes every git in it.
	if err := dropIndex(index); err != nil {
		return "", err
	}
	defer func() { _ = dropIndex(index) }()
	// Never a lazy fetch: the mirror holds every subtree of merge-tree's
	// tree. merge-ort takes a subtree that is the same on all sides without
	// reading it, but each lazy fetch of the base's, the tip's or the head's
	// root tree is `fetch --filter=blob:none`, which brings that tree's whole
	// closure, and every other tree in base merge-tree wrote itself.
	run := func(label string, stdin []byte, args ...string) (execx.Result, error) {
		c := g.cmd(dir, label, readTimeout, true, false, nil, args...)
		c.Env["GIT_INDEX_FILE"] = index
		c.Stdin = stdin
		return g.run.Run(ctx, c)
	}
	if _, err := run("resolved tree", nil, "read-tree", base); err != nil {
		return "", err
	}
	var in bytes.Buffer
	for _, e := range entries {
		switch {
		case strings.ContainsRune(e.Path, 0):
			return "", fmt.Errorf("path %q refused", e.Path)
		case e.Type == "":
			in.WriteString("0 " + strings.Repeat("0", len(base)) + "\t" + e.Path + "\x00") // mode 0 removes it
		case e.Type != "blob" || !slices.Contains(blobModes, e.Mode) || !isOID(e.OID):
			return "", fmt.Errorf("entry %s %s refused", e.Mode, e.Type)
		default:
			in.WriteString(e.Mode + " " + e.OID + "\t" + e.Path + "\x00")
		}
	}
	if _, err := run("resolved entries", in.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	// --missing-ok: the mirror holds few blobs but the resolved ones.
	res, err := run("resolved write-tree", nil, "write-tree", "--missing-ok")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(res.Out())
	if !isOID(tree) {
		return "", fmt.Errorf("write-tree printed %q, no tree", short(tree))
	}
	return tree, nil
}

func (g *execGit) deleteRef(ctx context.Context, dir, ref string) error {
	if !effectRef(ref) {
		return fmt.Errorf("ref %q is none of the effect refs", ref)
	}
	_, err := g.run.Run(ctx, g.cmd(dir, "delete ref", readTimeout, true, false, nil, "update-ref", "-d", ref))
	return err
}

func (g *execGit) push(ctx context.Context, dir string, env map[string]string, sha, branch string, allowed func(string) bool) error {
	args, err := pushArgv(dir, sha, branch, allowed)
	if err != nil {
		return err
	}
	c := g.cmd(dir, "push "+branch, pushTimeout, true, true, env)
	c.Args = args
	if err := checkPushArgv(c.Args, dir, allowed); err != nil {
		return err
	}
	_, err = g.run.Run(ctx, c)
	return err
}

// pushArgv is the one builder of a deploy's push: exactly `git
// --git-dir=<mirror> <gitOptions> push origin <sha>:refs/heads/<branch>`,
// which checkPushArgv must pass.
func pushArgv(dir, sha, branch string, allowed func(string) bool) ([]string, error) {
	args := slices.Concat([]string{"--git-dir=" + dir}, gitOptions, []string{"push", "origin", sha + ":refs/heads/" + branch})
	if err := checkPushArgv(args, dir, allowed); err != nil {
		return nil, err
	}
	return args, nil
}

// forbiddenPush are the push options and forms a deploy never passes, in
// any position: a force in any spelling, a mirror, a delete, every branch
// or tag, and pruning.
var forbiddenPush = []string{"-f", "--force", "--force-with-lease", "--force-if-includes", "--mirror", "--delete", "-d",
	"--all", "--branches", "--tags", "--prune", "--follow-tags", "--set-upstream", "-u", "--receive-pack", "--exec", "--repo"}

// checkPushArgv refuses every argv but a deploy's exact push: the mirror's
// --git-dir and gitOptions, then push, origin and one refspec of a full sha
// onto refs/heads/staging-<name> whose branch allowed passes. Any +, any
// force, mirror or delete option, a remote other than origin, a refspec of
// another form (a delete's empty source, a ref name, a glob, two of them)
// or an extra argument is refused.
func checkPushArgv(args []string, dir string, allowed func(string) bool) error {
	for _, a := range args {
		if strings.HasPrefix(a, "+") {
			return fmt.Errorf("push refused: a forced refspec %q", a)
		}
		for _, f := range forbiddenPush {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Errorf("push refused: %s", f)
			}
		}
	}
	prefix := slices.Concat([]string{"--git-dir=" + dir}, gitOptions)
	if !filepath.IsAbs(dir) || len(args) != len(prefix)+3 || !slices.Equal(args[:len(prefix)], prefix) {
		return fmt.Errorf("push refused: not the mirror's push (%d arguments)", len(args))
	}
	tail := args[len(prefix):]
	switch {
	case tail[0] != "push":
		return fmt.Errorf("push refused: %q is no push", tail[0])
	case tail[1] != "origin":
		return fmt.Errorf("push refused: the remote %q is not origin", tail[1])
	}
	m := pushSpecRe.FindStringSubmatch(tail[2])
	switch {
	case m == nil:
		return fmt.Errorf("push refused: the refspec %q is not <sha>:refs/heads/staging-<name>", tail[2])
	case allowed == nil || !allowed(m[2]):
		return fmt.Errorf("push refused: %s is not an allowed staging branch", m[2])
	}
	return nil
}

// checkIdentity refuses a commit identity that is no single line: empty, a
// control character (a newline, NUL), < or > (git's ident delimiters), or
// over maxIdent bytes. The name and the email are data (CommitIdentity's).
func checkIdentity(name, email string) error {
	for _, f := range []struct{ what, v string }{{"name", name}, {"email", email}} {
		switch {
		case strings.TrimSpace(f.v) == "":
			return fmt.Errorf("the commit identity's %s is empty", f.what)
		case len(f.v) > maxIdent:
			return fmt.Errorf("the commit identity's %s is over %d bytes", f.what, maxIdent)
		case strings.ContainsFunc(f.v, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '<' || r == '>' }):
			return fmt.Errorf("the commit identity's %s holds a control character, < or >", f.what)
		}
	}
	return nil
}

// checkResolverDir is the git dir of the resolver's repository at dir,
// which must be a local absolute path (never a URL or a path that starts
// with a dash), clean, an existing directory, a git repository to git
// named by --git-dir (dir itself, or its .git), and not the mirror.
func (g *execGit) checkResolverDir(ctx context.Context, dir, mirror string) (string, error) {
	switch {
	case dir == "":
		return "", errors.New("the resolver named no repository")
	case strings.Contains(dir, "://") || !filepath.IsAbs(dir) || strings.HasPrefix(dir, "-"):
		return "", fmt.Errorf("the resolver's repository %q is no local absolute path", dir)
	case filepath.Clean(dir) != dir || strings.ContainsFunc(dir, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", fmt.Errorf("the resolver's repository %q is no clean path", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("the resolver's repository %q is no directory", dir)
	}
	same := func(a, b string) bool {
		ra, errA := filepath.EvalSymlinks(a)
		rb, errB := filepath.EvalSymlinks(b)
		return errA == nil && errB == nil && ra == rb
	}
	for _, gd := range []string{dir, filepath.Join(dir, ".git")} {
		if _, err := os.Stat(gd); err != nil {
			continue
		}
		if same(gd, mirror) {
			return "", errors.New("the resolver's repository is the mirror itself")
		}
		if _, err := g.run.Run(ctx, g.srcCmd(gd, mirror, "resolver repository", "rev-parse", "--git-dir")); err == nil {
			return gd, nil
		}
	}
	return "", fmt.Errorf("the resolver's repository %q is no git repository", dir)
}
