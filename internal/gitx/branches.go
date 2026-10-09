package gitx

// The branch work of the skill editor and its landing (internal/engine
// learn_edit.go): a commit of named paths on a worktree's branch, paths
// restored as an earlier commit had them (a revert), a fast-forward of the
// checkout's branch, a rebase that aborts itself on a conflict, a commit's
// patch id, which of some paths are dirty, the branch checked out and the
// operation a repository is in the middle of. Like every gitx command they
// run none of the clone's hooks (hooksOff).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var (
	// ErrNotFastForward is MergeFFOnly refusing a branch that does not
	// descend from the checkout's HEAD.
	ErrNotFastForward = errors.New("gitx: not a fast-forward")
	// ErrRebaseConflict is RebaseOnto stopping on a conflict; the rebase
	// was aborted and the branch is as it was.
	ErrRebaseConflict = errors.New("gitx: the rebase conflicts")
)

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

// MergeFFOnly fast-forwards the branch checked out in repo to branch:
// ErrNotFastForward, with nothing changed, when branch does not descend
// from HEAD. Local changes the move would overwrite make git refuse, which
// is an error too.
func (c *Client) MergeFFOnly(ctx context.Context, repo, branch string) error {
	if err := checkBranch("branch", branch); err != nil {
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
	_, err = c.git(ctx, repo, call{label: "merge --ff-only " + branch, mutates: true, timeout: commitTimeout},
		"merge", "--ff-only", "--quiet", "--no-verify", to)
	return err
}

// RebaseOnto replays the commits of the branch checked out at dir that
// follow upstream onto onto, unsigned (`git rebase --onto onto upstream`):
// only those, so a commit onto no longer has (one the operator dropped
// from master after the branch was cut on it) does not come back with
// them. A rebase that stops (a conflict) is aborted, so the branch and its
// work tree are as they were, and the error is ErrRebaseConflict.
func (c *Client) RebaseOnto(ctx context.Context, dir, onto, upstream string) error {
	if err := checkRev("onto", onto); err != nil {
		return err
	}
	if err := checkRev("upstream", upstream); err != nil {
		return err
	}
	_, err := c.git(ctx, dir, call{label: "rebase --onto " + onto + " " + upstream, mutates: true, timeout: commitTimeout},
		"-c", "commit.gpgsign=false", "rebase", "--quiet", "--no-autostash", "--onto", onto, upstream)
	if err == nil {
		return nil
	}
	if p, perr := c.InProgress(ctx, dir); perr != nil || p != "rebase" {
		return errors.Join(err, perr)
	}
	if _, aerr := c.git(ctx, dir, call{label: "rebase --abort", mutates: true, timeout: commitTimeout}, "rebase", "--abort"); aerr != nil {
		return errors.Join(fmt.Errorf("gitx: rebase %s: %w", onto, ErrRebaseConflict), aerr)
	}
	return fmt.Errorf("gitx: rebase %s: %w", onto, ErrRebaseConflict)
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
	diff, err := c.git(ctx, dir, call{label: "diff-tree --patch " + rev},
		"diff-tree", "--patch", "--unified=0", "--no-color", "--no-ext-diff", "--no-textconv", rev)
	if err != nil {
		return "", err
	}
	if diff.Truncated {
		return "", fmt.Errorf("gitx: patch-id %s: the diff is too large", rev)
	}
	out, err := c.runStdin(ctx, dir, call{label: "patch-id"}, diff.Stdout, "patch-id", "--stable")
	if err != nil {
		return "", err
	}
	id, _, _ := strings.Cut(out, " ")
	if !isOID(id) {
		return "", fmt.Errorf("gitx: patch-id %s: no change to hash", rev)
	}
	return id, nil
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
// untracked files included, then forgets every registration whose
// directory is gone (WorktreePrune): a worktree someone removed already,
// or a second discard, is fine.
func (c *Client) WorktreeDiscard(ctx context.Context, repo, dir string) error {
	if err := checkAbsPath(dir); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		if err := c.WorktreeRemove(ctx, repo, dir, true); err != nil {
			return err
		}
	}
	return c.WorktreePrune(ctx, repo)
}
