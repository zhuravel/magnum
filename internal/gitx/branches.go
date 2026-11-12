package gitx

// The branch work of the skill editor and its landing (internal/engine
// learn_edit.go): a commit of named paths on a worktree's branch, paths
// restored as an earlier commit had them (a revert), a fast-forward of the
// checkout's branch, a rebase that aborts itself on a conflict, a commit's
// patch id, which of some paths are dirty, the branches under a prefix, the
// branch checked out and the operation a repository is in the middle of.
// And the fixer's `magnum fix-sync`: the commits a branch gained, a head's
// own diff against its base as one patch id, and a replay of a checkout's
// own commits onto a branch's new tip that aborts itself on a conflict.
// And the factory's: a task's branch started in its copy (SwitchCreate) and
// checked out again in the copy a later claim gives it (Switch), which,
// with its fast-forward (FastForward), never write over a file git does not
// track, an ignored one included (--no-overwrite-ignore; git's default
// writes over an ignored file): the copy's files are yours.
// Like every gitx command they run none of the clone's hooks (hooksOff).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

var (
	// ErrNotFastForward is MergeFFOnly refusing a branch that does not
	// descend from the checkout's HEAD.
	ErrNotFastForward = errors.New("gitx: not a fast-forward")
	// ErrRebaseConflict is RebaseOnto stopping on a conflict; the rebase
	// was aborted and the branch is as it was.
	ErrRebaseConflict = errors.New("gitx: the rebase conflicts")
	// ErrNotOnBranch is MergeFFOnly finding another branch (or a detached
	// HEAD) checked out than the one it is to fast-forward, right before
	// its merge, or that branch not at the commit it merged right after;
	// FastForward and ResetKeep finding a detached HEAD.
	ErrNotOnBranch = errors.New("gitx: the checkout is not on the branch to fast-forward")
	// ErrBranchExists is SwitchCreate finding the branch it is to create.
	ErrBranchExists = errors.New("gitx: the branch exists")
)

// SwitchCreate creates branch at start in the work tree at dir and checks
// it out (`git switch --no-track -c <branch> <start>`): a factory task's
// branch, from origin/<base>, tracking nothing. A branch of that name, in
// any worktree of the repository, is ErrBranchExists, and nothing moves.
// Local changes carry over as git carries them; one the switch would
// overwrite makes git refuse, and so does a file git does not track at a
// path the switch writes, an ignored one included (--no-overwrite-ignore).
func (c *Client) SwitchCreate(ctx context.Context, dir, branch, start string) error {
	if err := errors.Join(checkBranch("branch", branch), checkRev("start", start)); err != nil {
		return err
	}
	switch _, err := c.RevParse(ctx, dir, "refs/heads/"+branch); {
	case err == nil:
		return fmt.Errorf("gitx: switch -c %s: %w", branch, ErrBranchExists)
	case !errors.Is(err, ErrNoSuchRef):
		return err
	}
	_, err := c.git(ctx, dir, call{label: "switch -c " + branch + " " + start, mutates: true, timeout: worktreeTimeout},
		"switch", "--quiet", "--no-track", "--no-overwrite-ignore", "-c", branch, start)
	if ee, ok := errors.AsType[*execx.ExitError](err); ok && strings.Contains(ee.Stderr, "already exists") {
		return fmt.Errorf("gitx: switch -c %s: %w", branch, ErrBranchExists) // created since the check
	}
	return err
}

// Switch checks out branch, a local branch that exists, in the work tree
// at dir (`git switch --no-guess <branch>`): never a new branch that tracks
// a remote one of the name, never a discarded change (one the switch would
// overwrite makes git refuse, and so do a file git does not track at a path
// the switch writes, an ignored one included, --no-overwrite-ignore, and a
// branch another worktree has checked out).
func (c *Client) Switch(ctx context.Context, dir, branch string) error {
	if err := checkBranch("branch", branch); err != nil {
		return err
	}
	_, err := c.git(ctx, dir, call{label: "switch " + branch, mutates: true, timeout: worktreeTimeout},
		"switch", "--quiet", "--no-guess", "--no-overwrite-ignore", branch)
	return err
}

// ResetKeep moves the branch checked out in the work tree at dir, and HEAD,
// to rev (`git reset --keep <rev>`): the work tree's changes the move does
// not touch stay, and one it would overwrite makes git refuse, which is an
// error, and nothing moves. The commit the branch was at stays in the
// repository (its reflog). A detached HEAD is ErrNotOnBranch: it moves no
// branch.
func (c *Client) ResetKeep(ctx context.Context, dir, rev string) error {
	if err := checkRev("rev", rev); err != nil {
		return err
	}
	switch on, err := c.headRef(ctx, dir); {
	case err != nil:
		return err
	case on == "":
		return fmt.Errorf("gitx: reset --keep %s: HEAD is detached: %w", rev, ErrNotOnBranch)
	}
	_, err := c.git(ctx, dir, call{label: "reset --keep " + rev, mutates: true, timeout: worktreeTimeout},
		"reset", "--quiet", "--keep", rev)
	return err
}

// commitTimeout bounds a commit, a rebase and a merge of a checkout.
const commitTimeout = 2 * time.Minute

// literal turns paths, relative to the top of the work tree, into literal
// pathspecs: no glob, no magic a path could carry.
func literal(paths []string) ([]string, error) {
	if len(paths) == 0 || slices.Contains(paths, "") {
		return nil, errors.New("gitx: no path")
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = ":(top,literal)" + p
	}
	return out, nil
}

// CommitAll commits paths of the work tree at dir (their changes,
// additions and deletions; nothing else, whatever else is dirty) on its
// current branch with message, unsigned (commit.gpgsign=false whatever the
// repository says), and returns the new commit's id. Nothing to commit is
// an error.
func (c *Client) CommitAll(ctx context.Context, dir, message string, paths ...string) (string, error) {
	specs, err := literal(paths)
	if err != nil {
		return "", fmt.Errorf("gitx: commit: %w", err)
	}
	if _, err := c.git(ctx, dir, call{label: "add", mutates: true, timeout: commitTimeout}, append([]string{"add", "-A", "--"}, specs...)...); err != nil {
		return "", err
	}
	cmd := []string{"-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify", "-F", "-", "--"}
	if _, err := c.runStdin(ctx, dir, call{label: "commit", mutates: true, timeout: commitTimeout}, []byte(message), append(cmd, specs...)...); err != nil {
		return "", err
	}
	return c.RevParse(ctx, dir, "HEAD")
}

// RestoreFrom sets paths (relative to the top of the work tree at dir) in
// the work tree to their content at rev, and removes a path rev does not
// have (`git restore --source=<rev> --worktree`; the index stays, so
// CommitAll of the same paths commits the change): a revert of a skill
// version restores its files as the commit before it had them. At least
// one path; nothing else in the work tree changes.
func (c *Client) RestoreFrom(ctx context.Context, dir, rev string, paths ...string) error {
	if err := checkRev("source", rev); err != nil {
		return err
	}
	specs, err := literal(paths)
	if err != nil {
		return fmt.Errorf("gitx: restore: %w", err)
	}
	_, err = c.git(ctx, dir, call{label: "restore --source " + rev, mutates: true, timeout: commitTimeout},
		append([]string{"restore", "--source=" + rev, "--worktree", "--"}, specs...)...)
	return err
}

// RestoreTracked puts paths (relative to the top of the work tree at dir)
// back as HEAD has them, in the index and the work tree (`git restore
// --source=HEAD --staged --worktree`): a change of a factory command to a
// tracked file undone, a path HEAD lacks removed from both. At least one
// path; nothing else changes, untracked files included.
func (c *Client) RestoreTracked(ctx context.Context, dir string, paths ...string) error {
	specs, err := literal(paths)
	if err != nil {
		return fmt.Errorf("gitx: restore: %w", err)
	}
	_, err = c.git(ctx, dir, call{label: "restore --source HEAD --staged --worktree", mutates: true, timeout: commitTimeout},
		append([]string{"restore", "--source=HEAD", "--staged", "--worktree", "--"}, specs...)...)
	return err
}

// RestoreIndex puts paths back in the index of the work tree at dir as HEAD
// has them (`git restore --source=HEAD --staged`), and leaves their files in
// the work tree as they are: a file the index got only since HEAD (`git
// add`, `git add -N`) is untracked again, with its content.
func (c *Client) RestoreIndex(ctx context.Context, dir string, paths ...string) error {
	specs, err := literal(paths)
	if err != nil {
		return fmt.Errorf("gitx: restore --staged: %w", err)
	}
	_, err = c.git(ctx, dir, call{label: "restore --source HEAD --staged", mutates: true, timeout: commitTimeout},
		append([]string{"restore", "--source=HEAD", "--staged", "--"}, specs...)...)
	return err
}

// BranchesUnder lists the local branches of repo under prefix, one or more
// path segments ending in "/" ("learn/"), by their short names, sorted: the
// skill editor's orphan cleanup finds the branches no proposal holds.
func (c *Client) BranchesUnder(ctx context.Context, repo, prefix string) ([]string, error) {
	dir, ok := strings.CutSuffix(prefix, "/")
	if !ok || dir == "" {
		return nil, fmt.Errorf("gitx: branches under %q: the prefix must be a path ending in /", prefix)
	}
	if err := checkBranch("prefix", dir); err != nil {
		return nil, err
	}
	res, err := c.git(ctx, repo, call{label: "for-each-ref refs/heads/" + prefix}, "for-each-ref", "--format=%(refname)", "refs/heads/"+prefix)
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(res.Out(), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "refs/heads/"); ok && strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// runStdin is git with stdin.
func (c *Client) runStdin(ctx context.Context, dir string, k call, stdin []byte, args ...string) (string, error) {
	if err := checkDir(dir); err != nil {
		return "", fmt.Errorf("gitx: %s: %w", k.label, err)
	}
	cmd := gitCommand(dir, k, args)
	cmd.Stdin = stdin
	res, err := c.run.Run(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("gitx: %s: %w", k.label, err)
	}
	return res.Out(), nil
}

// MergeFFOnly fast-forwards base, the branch checked out in repo, to
// branch: ErrNotFastForward, with nothing changed, when branch does not
// descend from HEAD. git merges into whatever HEAD is, so HEAD must be
// refs/heads/<base> right before the merge (symbolic-ref; ErrNotOnBranch,
// with nothing changed, for another branch or a detached HEAD), and
// refs/heads/<base> must be branch's commit right after it (ErrNotOnBranch
// too: a checkout that switched branch in between had the other branch
// moved). Local changes the move would overwrite make git refuse, which
// is an error too; the others stay as they are, staged ones staged: the
// merge never stashes them (--no-autostash, whatever merge.autoStash says,
// as RebaseOnto).
func (c *Client) MergeFFOnly(ctx context.Context, repo, base, branch string) error {
	if err := errors.Join(checkBranch("base", base), checkBranch("branch", branch)); err != nil {
		return err
	}
	to, err := c.RevParse(ctx, repo, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	head, err := c.RevParse(ctx, repo, "HEAD")
	if err != nil {
		return err
	}
	if _, err := c.git(ctx, repo, call{label: "merge-base --is-ancestor", probe: true}, "merge-base", "--is-ancestor", head, to); err != nil {
		if code, ok := exitCode(err); ok && code == 1 {
			return fmt.Errorf("gitx: merge --ff-only %s: %w", branch, ErrNotFastForward)
		}
		return err
	}
	switch on, err := c.headRef(ctx, repo); {
	case err != nil:
		return err
	case on != "refs/heads/"+base:
		return fmt.Errorf("gitx: merge --ff-only %s: HEAD is %s, not refs/heads/%s: %w", branch, cmp.Or(on, "detached"), base, ErrNotOnBranch)
	}
	if _, err := c.git(ctx, repo, call{label: "merge --ff-only " + branch, mutates: true, timeout: commitTimeout},
		"merge", "--ff-only", "--quiet", "--no-verify", "--no-autostash", to); err != nil {
		return err
	}
	switch got, err := c.RevParse(ctx, repo, "refs/heads/"+base); {
	case err != nil:
		return err
	case got != to:
		return fmt.Errorf("gitx: merge --ff-only %s: %s is at %s after the merge, not at %s (the checkout switched branch meanwhile): %w",
			branch, base, got[:min(len(got), 12)], to[:min(len(to), 12)], ErrNotOnBranch)
	}
	return nil
}

// FastForward moves the branch checked out in the work tree at dir to to,
// a commit ahead of it (`git merge --ff-only --no-autostash <to>`): a factory
// task's branch with no commit of its own, to a fresh origin/<base>. The
// work tree's changes the move does not touch are carried, staged ones
// staged; one it would overwrite makes git refuse, which is an error, and
// nothing moves, as does a file git does not track at a path the move
// writes, an ignored one included (--no-overwrite-ignore; MergeFFOnly, the
// skill editor's, keeps git's default). A to that HEAD is not an ancestor
// of is ErrNotFastForward
// and a detached HEAD ErrNotOnBranch, nothing moved; to at HEAD is a no-op.
func (c *Client) FastForward(ctx context.Context, dir, to string) error {
	if err := checkRev("target", to); err != nil {
		return err
	}
	switch on, err := c.headRef(ctx, dir); {
	case err != nil:
		return err
	case on == "":
		return fmt.Errorf("gitx: merge --ff-only %s: HEAD is detached: %w", to, ErrNotOnBranch)
	}
	head, err := c.RevParse(ctx, dir, "HEAD")
	if err != nil {
		return err
	}
	target, err := c.RevParse(ctx, dir, to)
	if err != nil {
		return err
	}
	if head == target {
		return nil
	}
	switch ahead, err := c.IsAncestor(ctx, dir, head, target); {
	case err != nil:
		return err
	case !ahead:
		return fmt.Errorf("gitx: merge --ff-only %s: %w", to, ErrNotFastForward)
	}
	_, err = c.git(ctx, dir, call{label: "merge --ff-only " + to, mutates: true, timeout: commitTimeout},
		"merge", "--ff-only", "--quiet", "--no-verify", "--no-autostash", "--no-overwrite-ignore", target)
	return err
}

// TreeOf is the id of rev's tree (`git rev-parse --verify <rev>^{tree}`); a
// rev that does not resolve is ErrNoSuchRef.
func (c *Client) TreeOf(ctx context.Context, dir, rev string) (string, error) {
	if err := checkRev("revision", rev); err != nil {
		return "", err
	}
	res, err := c.git(ctx, dir, call{label: "rev-parse " + rev + "^{tree}", probe: true}, "rev-parse", "--verify", "--quiet", rev+"^{tree}")
	if err != nil {
		if code, ok := exitCode(err); ok && code == 1 {
			return "", fmt.Errorf("gitx: rev-parse %s^{tree}: %w", rev, ErrNoSuchRef)
		}
		return "", err
	}
	tree := res.Out()
	if !isOID(tree) {
		return "", fmt.Errorf("gitx: rev-parse %s^{tree}: unexpected output %q", rev, tree)
	}
	return tree, nil
}

// headRef is the ref HEAD points at in repo ("refs/heads/main"), "" when it
// is detached.
func (c *Client) headRef(ctx context.Context, repo string) (string, error) {
	res, err := c.git(ctx, repo, call{label: "symbolic-ref HEAD", noExit: 1}, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		if code, ok := exitCode(err); ok && code == 1 {
			return "", nil
		}
		return "", err
	}
	return res.Out(), nil
}

// RebaseOnto replays the commits of the branch checked out at dir that
// follow upstream onto onto, unsigned (`git rebase --onto onto upstream`):
// only those, so a commit onto no longer has (one the operator dropped
// from master after the branch was cut on it) does not come back with
// them. A rebase that stops (a conflict) is aborted, so the branch and its
// work tree are as they were, and the error is ErrRebaseConflict.
func (c *Client) RebaseOnto(ctx context.Context, dir, onto, upstream string) error {
	_, err := c.rebase(ctx, dir, onto, upstream, []string{"-c", "commit.gpgsign=false"}, nil)
	return err
}

// ReplayOnto replays the commits from..HEAD of the checkout at dir (a
// detached HEAD, or a branch, which then moves) onto onto (`git rebase
// --onto onto from`): only those, as RebaseOnto, and no other branch moves
// with them (--no-update-refs). Unlike RebaseOnto's, the commits are signed
// as the repository's config says, with config laid over it ("key=value"
// settings passed as `git -c`, which no file keeps): the fixer, whose
// commits these are, made them with that config (`magnum fix-sync`, which
// passes a fixer App pane's commit.gpgsign=false). A replay that stops (a
// conflict) is aborted, so HEAD, the index and the work tree are as they
// were; the error is then ErrRebaseConflict, with the paths that conflicted
// in the commit it stopped at, sorted.
func (c *Client) ReplayOnto(ctx context.Context, dir, onto, from string, config ...string) ([]string, error) {
	var args []string
	for _, kv := range config {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" || strings.HasPrefix(kv, "-") || strings.ContainsAny(kv, "\n\r\x00") {
			return nil, fmt.Errorf("gitx: replay: %q is no key=value setting", kv)
		}
		args = append(args, "-c", kv)
	}
	return c.rebase(ctx, dir, onto, from, args, []string{"--no-update-refs"})
}

// rebase is `git <config> rebase --quiet --no-autostash <opts> --onto onto
// upstream` in dir: a rebase that stops is aborted, and the error is
// ErrRebaseConflict with the paths unmerged when it stopped.
func (c *Client) rebase(ctx context.Context, dir, onto, upstream string, config, opts []string) ([]string, error) {
	if err := errors.Join(checkRev("onto", onto), checkRev("upstream", upstream)); err != nil {
		return nil, err
	}
	args := append(slices.Clone(config), "rebase", "--quiet", "--no-autostash")
	args = append(append(args, opts...), "--onto", onto, upstream)
	_, err := c.git(ctx, dir, call{label: "rebase --onto " + onto + " " + upstream, mutates: true, timeout: commitTimeout}, args...)
	if err == nil {
		return nil, nil
	}
	// The look at what is in progress and the abort run whatever stopped the
	// rebase, a cancelled ctx included: a rebase left in progress would hold
	// the checkout.
	actx := context.WithoutCancel(ctx)
	if p, perr := c.InProgress(actx, dir); perr != nil || p != "rebase" {
		return nil, errors.Join(err, perr)
	}
	paths, _ := c.unmergedPaths(actx, dir)
	if _, aerr := c.git(actx, dir, call{label: "rebase --abort", mutates: true, timeout: commitTimeout}, "rebase", "--abort"); aerr != nil {
		return paths, errors.Join(fmt.Errorf("gitx: rebase %s: %w", onto, ErrRebaseConflict), aerr)
	}
	return paths, fmt.Errorf("gitx: rebase %s: %w", onto, ErrRebaseConflict)
}

// unmergedPaths are the paths of dir's index with a conflict (`git diff
// --name-only --diff-filter=U`), sorted, nil for none.
func (c *Client) unmergedPaths(ctx context.Context, dir string) ([]string, error) {
	res, err := c.git(ctx, dir, call{label: "unmerged paths"}, "diff", "--name-only", "--diff-filter=U", "-z", "--no-color")
	if err != nil {
		return nil, err
	}
	var paths []string
	for p := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// AbortOperation ends op, the operation dir is in the middle of as
// InProgress names it ("rebase", "merge", "cherry-pick" or "revert"), with
// `git <op> --abort`: HEAD, the index and the work tree go back to where it
// started. Callers ask InProgress first; with nothing in progress git
// fails, and any other op is an error before git runs.
func (c *Client) AbortOperation(ctx context.Context, dir, op string) error {
	if !slices.ContainsFunc(inProgress, func(p struct{ name, file string }) bool { return p.name == op }) {
		return fmt.Errorf("gitx: abort: %q is no operation git keeps in progress", op)
	}
	_, err := c.git(ctx, dir, call{label: op + " --abort", mutates: true, timeout: commitTimeout}, op, "--abort")
	return err
}

// ReplayConflicts says where the change commit rev makes from its first
// parent would conflict if replayed onto onto, as RebaseOnto would replay
// it, without replaying it: the conflicting paths, sorted, nil when it
// applies. It is `git merge-tree --write-tree --merge-base=<rev>^ onto rev`
// (git 2.40 or later), which writes the merge's objects and no ref, index or
// work tree, so dir, any work tree of the repository, is left exactly as it
// was and a conflict leaves nothing to abort. An error for a commit with no
// parent, for revisions git does not know and for an answer it cannot read.
func (c *Client) ReplayConflicts(ctx context.Context, dir, onto, rev string) ([]string, error) {
	if err := errors.Join(checkRev("onto", onto), checkRev("rev", rev)); err != nil {
		return nil, err
	}
	return c.mergeTreeConflicts(ctx, dir, "merge-tree "+onto+" "+rev, "--merge-base="+rev+"^", onto, rev)
}

// MergeConflicts says where merging base into head would conflict, as
// GitHub's CONFLICTING does, without merging: the conflicting paths of `git
// merge-tree --write-tree head base` (git 2.40 or later), sorted, nil for a
// clean merge. As ReplayConflicts, it writes the merge's objects and no ref,
// index or work tree, so dir, any work tree of the repository that has both
// commits, is left exactly as it was. An error for revisions git does not
// know, histories without a common commit and an answer it cannot read.
func (c *Client) MergeConflicts(ctx context.Context, dir, head, base string) ([]string, error) {
	if err := errors.Join(checkRev("head", head), checkRev("base", base)); err != nil {
		return nil, err
	}
	return c.mergeTreeConflicts(ctx, dir, "merge-tree "+head+" "+base, head, base)
}

// MergeTree is MergeConflicts with the merge itself: the id of the tree
// `git merge-tree --write-tree head base` writes for a clean merge (no
// conflicts), else no tree and the conflicting paths, sorted. It writes the
// merge's objects and no ref, index or work tree, so CommitTree can make
// the merge commit of that tree with nothing checked out.
func (c *Client) MergeTree(ctx context.Context, dir, head, base string) (tree string, conflicts []string, err error) {
	if err := errors.Join(checkRev("head", head), checkRev("base", base)); err != nil {
		return "", nil, err
	}
	what := "merge-tree " + head + " " + base
	tree, conflicts, err = c.mergeTree(ctx, dir, what, head, base)
	switch {
	case err != nil || conflicts != nil:
		return "", conflicts, err
	case !isOID(tree):
		return "", nil, fmt.Errorf("gitx: %s: unreadable tree %.40q", what, tree)
	}
	return tree, nil, nil
}

// mergeTreeConflicts runs `git merge-tree --write-tree` with args (its
// options, then the two revisions) in dir and returns the conflicting paths,
// sorted, nil for a clean merge; what names the call in its errors.
func (c *Client) mergeTreeConflicts(ctx context.Context, dir, what string, args ...string) ([]string, error) {
	_, paths, err := c.mergeTree(ctx, dir, what, args...)
	return paths, err
}

// mergeTree is mergeTreeConflicts with the first field of git's answer, the
// merged tree's id (unchecked for a clean merge: "" or what git printed),
// with conflict markers in its conflicted files when there are any.
func (c *Client) mergeTree(ctx context.Context, dir, what string, args ...string) (string, []string, error) {
	res, err := c.git(ctx, dir, call{label: what, noExit: 1, timeout: commitTimeout},
		append([]string{"merge-tree", "--write-tree", "--name-only", "-z", "--no-messages"}, args...)...)
	if err == nil {
		tree, _, _ := strings.Cut(strings.TrimSpace(string(res.Stdout)), "\x00")
		return tree, nil, nil
	}
	if code, ok := exitCode(err); !ok || code != 1 {
		return "", nil, err
	}
	// "<tree>\0<path>\0<path>\0": the merged tree, then each conflicted path once.
	fields := strings.Split(string(res.Stdout), "\x00")
	if !isOID(fields[0]) {
		return "", nil, fmt.Errorf("gitx: %s: unreadable answer %.40q", what, res.Stdout)
	}
	var paths []string
	for _, f := range fields[1:] {
		if f != "" {
			paths = append(paths, f)
		}
	}
	if len(paths) == 0 {
		return "", nil, fmt.Errorf("gitx: %s: conflicts, but names no path", what)
	}
	slices.Sort(paths)
	return fields[0], slices.Compact(paths), nil
}

// Signature is who a commit is by: the name and email of its author, who
// is its committer too.
type Signature struct{ Name, Email string }

// CommitTree writes the commit of tree with parents (the first is the
// branch it goes on: a merge's are the PR's head, then the base's tip) and
// message, by who as author and committer whatever the environment or the
// repository says, unsigned (commit.gpgsign=false), and returns its id:
// `git commit-tree <tree> -p <parent>... -F -`, the message on stdin. No
// ref, index or work tree changes. A tree or parent that is no revision, no
// parent, or a signature with an empty, multi-line or bracketed part is
// refused before git runs.
func (c *Client) CommitTree(ctx context.Context, dir, tree string, parents []string, message string, who Signature) (string, error) {
	errs := []error{checkRev("tree", tree)}
	if len(parents) == 0 {
		errs = append(errs, errors.New("gitx: commit-tree: no parent"))
	}
	for _, p := range parents {
		errs = append(errs, checkRev("parent", p))
	}
	for _, part := range []string{who.Name, who.Email} {
		if part == "" || strings.ContainsAny(part, "<>\n\r\x00") {
			errs = append(errs, fmt.Errorf("gitx: commit-tree: signature %q is no name and email", who.Name+" <"+who.Email+">"))
			break
		}
	}
	if err := errors.Join(errs...); err != nil {
		return "", err
	}
	args := []string{"-c", "commit.gpgsign=false", "commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	env := map[string]string{"GIT_AUTHOR_NAME": who.Name, "GIT_AUTHOR_EMAIL": who.Email, "GIT_COMMITTER_NAME": who.Name, "GIT_COMMITTER_EMAIL": who.Email}
	out, err := c.runStdin(ctx, dir, call{label: "commit-tree " + tree, mutates: true, timeout: commitTimeout, env: env}, []byte(message), append(args, "-F", "-")...)
	if err != nil {
		return "", err
	}
	if sha := strings.TrimSpace(out); isOID(sha) {
		return sha, nil
	}
	return "", fmt.Errorf("gitx: commit-tree %s: unreadable answer %.40q", tree, out)
}

// PatchID is the stable patch id of commit rev's change from its first
// parent (`git diff-tree --patch --unified=0 rev | git patch-id --stable`):
// with no context lines, a commit rebased onto a branch that changed lines
// near its own keeps the id it had, and a commit that changes anything else
// has another. An error for a commit with no change to hash.
func (c *Client) PatchID(ctx context.Context, dir, rev string) (string, error) {
	if err := checkRev("rev", rev); err != nil {
		return "", err
	}
	id, err := c.patchID(ctx, dir, patchIDStable, rev, rev)
	if err == nil && id == "" {
		err = fmt.Errorf("gitx: patch-id %s: no change to hash", rev)
	}
	return id, err
}

// OwnDiffID is the verbatim patch id of head's own diff against base: the
// change from their merge base to head (`git diff-tree --patch --unified=0
// --full-index <merge base> head | git patch-id --verbatim`), the PR as
// GitHub's base...head shows it. With no context lines, a merge of the base
// into head that changes other lines, near the PR's own or not, leaves the
// id as it was, as line numbers and the order of files do not count; one
// that changes a line of the PR's own (a conflict resolved, a change slipped
// into the merge, its indentation or the spaces inside it, which --stable
// would not see and YAML, Haml, Python or a Makefile read) or brings a
// branch the base lacks changes it. A binary file counts by its blob ids.
// "" when head changes nothing against base; ErrNoMergeBase for histories
// without a common commit.
func (c *Client) OwnDiffID(ctx context.Context, dir, base, head string) (string, error) {
	if err := errors.Join(checkRev("base", base), checkRev("head", head)); err != nil {
		return "", err
	}
	mb, err := c.MergeBase(ctx, dir, base, head)
	if err != nil {
		return "", err
	}
	return c.patchID(ctx, dir, patchIDVerbatim, base+"..."+head, "--full-index", mb, head)
}

// The modes of `git patch-id` patchID runs: both ignore line numbers and
// the order of files; --stable also ignores whitespace, --verbatim keeps it.
const (
	patchIDStable   = "--stable"
	patchIDVerbatim = "--verbatim"
)

// patchID is the patch id, in mode (patchIDStable or patchIDVerbatim), of
// `git diff-tree --patch --unified=0 <args>` (what names it in the
// errors), "" for a diff with no change.
func (c *Client) patchID(ctx context.Context, dir, mode, what string, args ...string) (string, error) {
	diff, err := c.git(ctx, dir, call{label: "diff-tree --patch " + what},
		append([]string{"diff-tree", "--patch", "--unified=0", "--no-color", "--no-ext-diff", "--no-textconv"}, args...)...)
	if err != nil {
		return "", err
	}
	if diff.Truncated {
		return "", fmt.Errorf("gitx: patch-id %s: the diff is too large", what)
	}
	out, err := c.runStdin(ctx, dir, call{label: "patch-id"}, diff.Stdout, "patch-id", mode)
	if err != nil || out == "" {
		return "", err
	}
	id, _, _ := strings.Cut(out, " ")
	if !isOID(id) {
		return "", fmt.Errorf("gitx: patch-id %s: unreadable answer %.60q", what, out)
	}
	return id, nil
}

// ListedCommit is one commit CommitsAfter lists: its id and its parents'.
type ListedCommit struct {
	SHA     string
	Parents []string
}

// Merge reports whether the commit has more than one parent.
func (l ListedCommit) Merge() bool { return len(l.Parents) > 1 }

// CommitsAfter lists the commits of to that from lacks (`git rev-list
// --parents from..to`), newest first, each with its parents. With
// firstParent, only to's first-parent chain (--first-parent): a merge is
// then one commit, without the commits it brought in, as GitHub's "Update
// branch" adds one merge to a PR's branch. Nothing when from has every
// commit of to.
func (c *Client) CommitsAfter(ctx context.Context, dir, from, to string, firstParent bool) ([]ListedCommit, error) {
	if err := errors.Join(checkRev("from", from), checkRev("to", to)); err != nil {
		return nil, err
	}
	args := []string{"rev-list", "--parents"}
	if firstParent {
		args = append(args, "--first-parent")
	}
	res, err := c.git(ctx, dir, call{label: "rev-list " + from + ".." + to}, append(args, "^"+from, to, "--")...)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: rev-list %s..%s: too many commits", from, to)
	}
	var out []ListedCommit
	for line := range strings.Lines(res.Out()) {
		ids := strings.Fields(line)
		if len(ids) == 0 {
			continue
		}
		for _, id := range ids {
			if !isOID(id) {
				return nil, fmt.Errorf("gitx: rev-list %s..%s: unreadable line %.100q", from, to, line)
			}
		}
		out = append(out, ListedCommit{SHA: ids[0], Parents: ids[1:]})
	}
	return out, nil
}

// DirtyPaths returns the paths among paths (relative to the top of the
// work tree at dir) that differ from HEAD or are untracked, sorted, nil
// when none; ignored files are not listed. At least one path.
func (c *Client) DirtyPaths(ctx context.Context, dir string, paths ...string) ([]string, error) {
	specs, err := literal(paths)
	if err != nil {
		return nil, fmt.Errorf("gitx: status: %w", err)
	}
	res, err := c.git(ctx, dir, call{label: "status of paths"},
		append([]string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--"}, specs...)...)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range parseStatusZ(res.Stdout) {
		out = append(out, e.Path)
		if e.OrigPath != "" && slices.Contains(paths, e.OrigPath) {
			out = append(out, e.OrigPath)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// CurrentBranch is the branch checked out in repo, "" when its HEAD is
// detached (a rebase in progress included).
func (c *Client) CurrentBranch(ctx context.Context, repo string) (string, error) {
	res, err := c.git(ctx, repo, call{label: "symbolic-ref HEAD", noExit: 1}, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if code, ok := exitCode(err); ok && code == 1 {
			return "", nil
		}
		return "", err
	}
	return res.Out(), nil
}

// inProgress are the files git keeps while an operation waits for its
// user, by the name InProgress gives it.
var inProgress = []struct{ name, file string }{
	{"rebase", "rebase-merge"}, {"rebase", "rebase-apply"}, {"merge", "MERGE_HEAD"},
	{"cherry-pick", "CHERRY_PICK_HEAD"}, {"revert", "REVERT_HEAD"},
}

// InProgress names the operation repo is in the middle of: "rebase",
// "merge", "cherry-pick", "revert", or "" for none.
func (c *Client) InProgress(ctx context.Context, repo string) (string, error) {
	args := []string{"rev-parse", "--path-format=absolute"}
	for _, p := range inProgress {
		args = append(args, "--git-path", p.file)
	}
	res, err := c.git(ctx, repo, call{label: "rev-parse --git-path"}, args...)
	if err != nil {
		return "", err
	}
	got := strings.Split(res.Out(), "\n")
	if len(got) != len(inProgress) {
		return "", fmt.Errorf("gitx: rev-parse --git-path: %d paths for %d names", len(got), len(inProgress))
	}
	for i, p := range inProgress {
		if _, err := os.Lstat(filepath.Clean(got[i])); err == nil {
			return p.name, nil
		}
	}
	return "", nil
}

// WorktreeDiscard removes the worktree at dir of repo, modified and
// untracked files included, then forgets the stale registrations below
// root, the directory of worktrees dir is in (the worktree list's prunable
// entries there, whose directory is gone, dir's included; a locked one
// stays), and no other: never WorktreePrune, which would forget every stale
// worktree of repo, one the operator keeps on a volume that is not mounted
// now too. A worktree someone removed already, or a second discard, is
// fine; a dir outside root is an error.
func (c *Client) WorktreeDiscard(ctx context.Context, repo, root, dir string) error {
	if err := errors.Join(checkAbsPath(root), checkAbsPath(dir)); err != nil {
		return err
	}
	if !below(root, dir) {
		return fmt.Errorf("gitx: worktree %s is not below %s", dir, root)
	}
	if _, err := os.Lstat(dir); err == nil {
		if err := c.WorktreeRemove(ctx, repo, dir, true); err != nil {
			return err
		}
	}
	list, err := c.WorktreeList(ctx, repo)
	if err != nil {
		return err
	}
	for _, w := range list {
		if !w.Prunable || w.Locked || !below(root, w.Path) {
			continue
		}
		// git removes a registration whose directory is gone (it deletes only
		// its admin directory then).
		if err := c.WorktreeRemove(ctx, repo, w.Path, true); err != nil {
			return err
		}
	}
	return nil
}

// below reports whether path is below root (not root itself), both cleaned
// and symlink-resolved as FindWorktree compares them.
func below(root, path string) bool {
	rel, err := filepath.Rel(canonPath(root), canonPath(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CommitIdentity is one commit FirstParentIdentities lists: its id, its
// parents' and the emails of its author and committer as the commit
// records them.
type CommitIdentity struct {
	SHA                         string
	Parents                     []string
	AuthorEmail, CommitterEmail string
}

// FirstParentIdentities lists the commits of to that from lacks on to's
// first-parent chain (`git log --first-parent ^from to`), newest first,
// each with its parents and its author's and committer's emails (%ae and
// %ce, which ignore .mailmap: the commit's own words). A merge is one
// entry, without the commits it brought in. Nothing when from has every
// commit of to. `magnum push` checks that each is the fixer's.
func (c *Client) FirstParentIdentities(ctx context.Context, dir, from, to string) ([]CommitIdentity, error) {
	if err := errors.Join(checkRev("from", from), checkRev("to", to)); err != nil {
		return nil, err
	}
	res, err := c.git(ctx, dir, call{label: "log --first-parent " + from + ".." + to},
		"log", "--first-parent", "--no-show-signature", "--no-color", "--format=%H%x00%P%x00%ae%x00%ce", "^"+from, to, "--")
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: log %s..%s: too many commits", from, to)
	}
	var out []CommitIdentity
	for line := range strings.Lines(res.Out()) {
		line = strings.TrimRight(line, "\n")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x00")
		if len(f) != 4 || !isOID(f[0]) {
			return nil, fmt.Errorf("gitx: log %s..%s: unreadable line %.100q", from, to, line)
		}
		parents := strings.Fields(f[1])
		if slices.ContainsFunc(parents, func(p string) bool { return !isOID(p) }) {
			return nil, fmt.Errorf("gitx: log %s..%s: unreadable parents %.100q", from, to, f[1])
		}
		out = append(out, CommitIdentity{SHA: f[0], Parents: parents, AuthorEmail: f[2], CommitterEmail: f[3]})
	}
	return out, nil
}
