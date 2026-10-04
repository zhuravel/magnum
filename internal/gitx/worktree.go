package gitx

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Worktree is one entry of `git worktree list --porcelain`.
type Worktree struct {
	Path       string // as reported by git (symlinks resolved); compare with FindWorktree
	Head       string // full commit sha; empty for a bare repository
	Branch     string // short branch name (refs/heads/ stripped); empty when detached or bare
	Detached   bool
	Bare       bool
	Locked     bool
	LockReason string
	Prunable   bool // administrative files are stale (for example the directory was deleted)
}

// WorktreeAdd creates a worktree at the absolute path, checking out ref.
// With detach it is a detached HEAD at ref; with branch it creates that new
// branch at ref without upstream tracking; with neither, git decides (a branch
// name checks the branch out, anything else detaches). Callers that must be
// idempotent check WorktreeList first, since git refuses an existing path.
func (c *Client) WorktreeAdd(ctx context.Context, mainClone, path, ref string, detach bool, branch string) error {
	if err := checkAbsPath(path); err != nil {
		return err
	}
	if err := checkRev("ref", ref); err != nil {
		return err
	}
	if detach && branch != "" {
		return errors.New("gitx: worktree add: detach and branch are mutually exclusive")
	}
	args := []string{"worktree", "add", "--quiet"}
	switch {
	case detach:
		args = append(args, "--detach")
	case branch != "":
		if err := checkBranch("branch", branch); err != nil {
			return err
		}
		args = append(args, "--no-track", "-b", branch)
	}
	args = append(args, path, ref)
	_, err := c.git(ctx, mainClone, call{label: fmt.Sprintf("worktree add %s %s", path, ref), mutates: true, timeout: worktreeTimeout}, args...)
	return err
}

// WorktreeRemove removes the worktree at path. force also removes a worktree
// with modified or untracked files (git refuses without it); a locked worktree
// is never removed.
func (c *Client) WorktreeRemove(ctx context.Context, mainClone, path string, force bool) error {
	if err := checkAbsPath(path); err != nil {
		return err
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, err := c.git(ctx, mainClone, call{label: "worktree remove " + path, mutates: true, timeout: worktreeTimeout}, args...)
	return err
}

// WorktreePrune drops administrative entries of worktrees whose directory is
// gone. Callers need it after a slot directory vanished: git refuses to add a
// worktree at that path or delete the branch "used by" it until the stale
// registration is forgotten. `git worktree prune` takes no path, so it also
// forgets other worktrees of the clone whose directory is missing (for
// example one on an unmounted volume). That is accepted instead of editing
// .git/worktrees/<id> by hand; a worktree locked with `git worktree lock` is
// never pruned.
func (c *Client) WorktreePrune(ctx context.Context, mainClone string) error {
	_, err := c.git(ctx, mainClone, call{label: "worktree prune", mutates: true}, "worktree", "prune")
	return err
}

// WorktreeList returns every worktree of the repository, the main one first.
// A path containing a newline is not supported.
func (c *Client) WorktreeList(ctx context.Context, mainClone string) ([]Worktree, error) {
	res, err := c.git(ctx, mainClone, call{label: "worktree list"}, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(string(res.Stdout)), nil
}

// parseWorktrees parses the porcelain format: records of "key value" lines
// separated by blank lines, each starting with a "worktree <path>" line.
func parseWorktrees(out string) []Worktree {
	var list []Worktree
	var cur *Worktree
	flush := func() {
		if cur != nil {
			list = append(list, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		if key == "worktree" {
			flush()
			cur = &Worktree{Path: val}
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "HEAD":
			cur.Head = val
		case "branch":
			cur.Branch = strings.TrimPrefix(val, "refs/heads/")
		case "detached":
			cur.Detached = true
		case "bare":
			cur.Bare = true
		case "locked":
			cur.Locked = true
			cur.LockReason = val
		case "prunable":
			cur.Prunable = true
		}
	}
	flush()
	return list
}

// FindWorktree returns the entry for path. Both sides are cleaned and, where
// they exist, symlink-resolved, so /var/... and /private/var/... spellings (and
// a trailing slash) match.
func FindWorktree(list []Worktree, path string) (Worktree, bool) {
	if path == "" {
		return Worktree{}, false
	}
	want := canonPath(path)
	for _, w := range list {
		if canonPath(w.Path) == want {
			return w, true
		}
	}
	return Worktree{}, false
}

// canonPath cleans p and resolves symlinks. For a directory that no longer
// exists it resolves the parent so stale entries still compare equal.
func canonPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(r, filepath.Base(p))
	}
	return p
}

func checkAbsPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("gitx: worktree path %q must be absolute", path)
	}
	return nil
}
