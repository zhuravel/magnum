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
	// in paths' order; a path the tree lacks has no Type.
	srcEntries(ctx context.Context, src, mirror, sha string, paths []string) ([]treeEntry, error)
	// writeObject writes data into the mirror as an object of type typ
	// (blob, commit) and returns its id.
	writeObject(ctx context.Context, dir, typ string, data []byte) (string, error)
	// buildTree writes the tree of base with each of entries put in at its
	// path, or removed when it has no Type, and returns its id. It builds
	// it in the temporary index at index, in the mirror, which goes before
	// (a killed run's) and after. env is the fetch's: reading base may
	// fetch a tree the mirror lacks.
	buildTree(ctx context.Context, dir string, env map[string]string, index, base string, entries []treeEntry) (string, error)
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
// but NUL is its own.
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
	switch {
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

func (g *execGit) buildTree(ctx context.Context, dir string, env map[string]string, index, base string, entries []treeEntry) (string, error) {
	if err := checkRevs(base); err != nil {
		return "", err
	}
	if filepath.Dir(index) != dir || !strings.HasPrefix(filepath.Base(index), resolvedIndexPrefix) {
		return "", fmt.Errorf("index %q is not the mirror's", index)
	}
	// A killed run's index, or its lock, is this effect's alone: the
	// mirror's flock serializes every git in it.
	drop := func() error {
		var errs []error
		for _, f := range []string{index + ".lock", index} {
			if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	if err := drop(); err != nil {
		return "", err
	}
	defer func() { _ = drop() }()
	run := func(label string, timeout time.Duration, fetchEnv map[string]string, stdin []byte, args ...string) (execx.Result, error) {
		c := g.cmd(dir, label, timeout, true, fetchEnv != nil, fetchEnv, args...)
		c.Env["GIT_INDEX_FILE"] = index
		c.Stdin = stdin
		return g.run.Run(ctx, c)
	}
	// merge-tree read these trees a moment ago, so the mirror holds them;
	// one it lacks is fetched from origin, as merge-tree would.
	if _, err := run("resolved tree", mergeTimeout, env, nil, "read-tree", base); err != nil {
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
	if _, err := run("resolved entries", readTimeout, nil, in.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	// --missing-ok: the mirror holds few blobs but the resolved ones.
	res, err := run("resolved write-tree", readTimeout, nil, nil, "write-tree", "--missing-ok")
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
