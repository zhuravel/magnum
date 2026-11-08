package staging

import (
	"context"
	"errors"
	"fmt"
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

// resolvedPrefix is where a resolver's merge is fetched into the mirror,
// refs/magnum/resolved/<effect>, so the mirror holds it through verify and
// the push; the ref goes when the effect ends.
const resolvedPrefix = "refs/magnum/resolved/"

// change is a path a tree changes against another, with git's status letter
// (A added, D deleted, M modified, T type changed).
type change struct {
	Status byte
	Path   string
}

// deployGit is the git a deploy runs beyond Git's reads, always in the
// mirror (execGit; Git implementations without it cannot deploy). Every
// revision is a full sha.
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
	// hasCommit reports whether the mirror holds the commit, never fetching
	// it.
	hasCommit(ctx context.Context, dir, sha string) (bool, error)
	// isAncestor reports whether a is b or an ancestor of b.
	isAncestor(ctx context.Context, dir, a, b string) (bool, error)
	// fetchResolved fetches the commit sha from the local repository at src
	// (a git dir checkResolverDir passed) into ref.
	fetchResolved(ctx context.Context, dir, src, sha, ref string) error
	// deleteRef deletes one of the mirror's resolved refs; an absent one is
	// no error.
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
	var tree string
	var parents []string
	for line := range strings.Lines(string(res.Stdout)) {
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
	if !isOID(tree) {
		return "", nil, fmt.Errorf("commit %s names no tree", short(sha))
	}
	return tree, parents, nil
}

func (g *execGit) hasCommit(ctx context.Context, dir, sha string) (bool, error) {
	if err := checkRevs(sha); err != nil {
		return false, err
	}
	_, err := g.run.Run(ctx, g.cmd(dir, "has commit", readTimeout, false, false, nil, "cat-file", "-e", sha+"^{commit}"))
	switch {
	case err == nil:
		return true, nil
	case exitCode(err) > 0 && ctx.Err() == nil:
		return false, nil
	}
	return false, err
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

func (g *execGit) fetchResolved(ctx context.Context, dir, src, sha, ref string) error {
	if !isOID(sha) {
		return fmt.Errorf("%q is no sha", sha)
	}
	if id, ok := strings.CutPrefix(ref, resolvedPrefix); !ok || !validID(EffectID(id)) {
		return fmt.Errorf("ref %q is none of the resolved refs", ref)
	}
	if !filepath.IsAbs(src) || strings.HasPrefix(src, "-") {
		return fmt.Errorf("the resolver's repository %q is no absolute path", src)
	}
	// A local path only: protocol.file.allow for this call alone, no hooks
	// (gitOptions), no submodules; the mirror never lazily fetches here.
	c := g.cmd(dir, "fetch resolved", fetchTimeout, true, false, nil)
	c.Args = slices.Concat([]string{"--git-dir=" + dir}, gitOptions, []string{"-c", "protocol.file.allow=always",
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--", src, sha + ":" + ref})
	_, err := g.run.Run(ctx, c)
	return err
}

func (g *execGit) deleteRef(ctx context.Context, dir, ref string) error {
	if id, ok := strings.CutPrefix(ref, resolvedPrefix); !ok || !validID(EffectID(id)) {
		return fmt.Errorf("ref %q is none of the resolved refs", ref)
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
		c := g.base(filepath.Dir(gd), filepath.Dir(filepath.Dir(mirror)), "resolver repository", readTimeout, false, false, nil)
		c.Args = slices.Concat([]string{"--git-dir=" + gd}, gitOptions, []string{"rev-parse", "--git-dir"})
		c.Env["GIT_DIR"] = gd
		if _, err := g.run.Run(ctx, c); err == nil {
			return gd, nil
		}
	}
	return "", fmt.Errorf("the resolver's repository %q is no git repository", dir)
}
