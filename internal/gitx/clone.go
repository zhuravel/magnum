package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrNoClone is returned by FindClone when no directory under the clone root
// is a git clone of the repository.
var ErrNoClone = errors.New("gitx: no clone of the repository")

// WorktreesSuffix is appended to a main clone's path to name the directory
// holding magnum's per-PR worktrees (<clone>__worktrees/pr-N). FindClone
// skips such directories.
const WorktreesSuffix = "__worktrees"

// fileStamp identifies one version of a small file by modification time and
// size.
type fileStamp struct {
	mtime int64 // UnixNano
	size  int64
}

func stampOf(st fs.FileInfo) fileStamp { return fileStamp{st.ModTime().UnixNano(), st.Size()} }

// originEntry caches the origin URL of one directory, valid while the files
// that decide it keep their stamps: the repository's config and, for a linked
// checkout, also the .git file pointing at it (see originStamps).
type originEntry struct {
	stamps [2]fileStamp
	url    string
	ok     bool // false: git answered, but there is no usable origin
}

// FindClone returns the main clone of github.com/<owner>/<name> under
// cloneRoot (an absolute, already expanded directory). Candidates are tried
// in order: <root>/<name>, <root>/<owner>-<name>, <root>/<owner>_<name>; the
// first that is a git repository whose `git remote get-url origin` is a
// GitHub URL of <owner>/<name> wins (see originMatches; owner and name are
// compared case-insensitively). Otherwise every directory directly under the
// root (not recursively; names containing "__worktrees" are skipped) is
// checked the same way, in name order. A directory is asked once however many
// names reach it (symlinks, case-insensitive filesystems). A directory
// without a .git entry is never handed to git, so a same-named folder that is
// not a repository costs no subprocess and no warning.
//
// Origins are cached per directory until its git config changes, so
// repeated lookups cost a directory listing and a few stats. ErrNoClone
// (wrapped) means nothing matched; any other error is an unreadable root.
func (c *Client) FindClone(ctx context.Context, cloneRoot, owner, name string) (string, error) {
	if badPathSegment(owner) || badPathSegment(name) {
		return "", fmt.Errorf("gitx: find clone: invalid repository %q/%q", owner, name)
	}
	if cloneRoot == "" {
		return "", fmt.Errorf("gitx: find clone of %s/%s: empty clone root", owner, name)
	}
	root := filepath.Clean(cloneRoot)
	var asked []fs.FileInfo // identities of the directories already tried
	try := func(dir string) (bool, error) {
		st, err := os.Stat(dir) // follows a symlinked clone
		if err != nil || !st.IsDir() || slices.ContainsFunc(asked, func(o fs.FileInfo) bool { return os.SameFile(o, st) }) {
			return false, nil
		}
		asked = append(asked, st)
		if c.originIs(ctx, dir, owner, name) {
			return true, nil
		}
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("gitx: find clone of %s/%s: %w", owner, name, err)
		}
		return false, nil
	}
	for _, cand := range []string{name, owner + "-" + name, owner + "_" + name} {
		dir := filepath.Join(root, cand)
		if found, err := try(dir); err != nil {
			return "", err
		} else if found {
			return dir, nil
		}
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s/%s (clone root %s does not exist)", ErrNoClone, owner, name, root)
	}
	if err != nil {
		return "", fmt.Errorf("gitx: find clone of %s/%s: %w", owner, name, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), WorktreesSuffix) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if found, err := try(dir); err != nil {
			return "", err
		} else if found {
			return dir, nil
		}
	}
	return "", fmt.Errorf("%w: %s/%s under %s", ErrNoClone, owner, name, root)
}

// badPathSegment reports whether s cannot be one directory name under the
// clone root (empty, a dot entry, or containing a separator).
func badPathSegment(s string) bool {
	return s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`)
}

// originMatches reports whether remoteURL is a GitHub remote of owner/name:
// https://github.com/o/r, git@github.com:o/r, ssh://git@github.com/o/r and the
// other forms ParseRemote reads, with or without .git or a trailing slash. The
// host must be exactly github.com, so github.com in a path or a longer host
// name (evilgithub.com, notgithub.com) does not match; owner and name are
// compared case-insensitively, as GitHub does.
func originMatches(remoteURL, owner, name string) bool {
	r, ok := ParseRemote(remoteURL)
	return ok && r.Host == "github.com" && strings.EqualFold(r.Owner, owner) && strings.EqualFold(r.Repo, name)
}

// IsRepo reports whether dir has a .git entry (a directory for a clone, a
// file for a linked worktree or submodule). It never runs git.
func IsRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// originIs reports whether dir is a git repository whose origin is a GitHub
// URL of owner/name.
func (c *Client) originIs(ctx context.Context, dir, owner, name string) bool {
	url, ok := c.cachedOrigin(ctx, dir)
	return ok && originMatches(url, owner, name)
}

// originStamps returns the stamps that decide dir's origin, from its .git
// entry (st is its FileInfo). For a clone (.git is a directory) that is the
// config file. For a linked checkout or submodule (.git is a file) `git
// remote add` or `set-url` rewrites the config of the common directory the
// file leads to, not the file, so it is that config plus the .git file. ok is
// false when the layout cannot be resolved; such a directory is not cached.
func originStamps(gitPath string, st fs.FileInfo) (stamps [2]fileStamp, ok bool) {
	if st.IsDir() {
		cfg, err := os.Stat(filepath.Join(gitPath, "config"))
		if err != nil {
			return [2]fileStamp{stampOf(st)}, true // no config yet: key on the directory
		}
		return [2]fileStamp{stampOf(cfg)}, true
	}
	gitdir, err := gitFileTarget(gitPath)
	if err != nil {
		return stamps, false
	}
	common := gitdir
	if b, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil { // linked worktree
		common = strings.TrimSpace(string(b))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
	}
	cfg, err := os.Stat(filepath.Join(common, "config"))
	if err != nil {
		return stamps, false
	}
	return [2]fileStamp{stampOf(cfg), stampOf(st)}, true
}

// gitFileTarget reads the "gitdir: <path>" line of a .git file and returns the
// path, made absolute against the file's directory.
func gitFileTarget(gitPath string) (string, error) {
	b, err := os.ReadFile(gitPath)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	target = strings.TrimSpace(target)
	if !ok || target == "" {
		return "", fmt.Errorf("gitx: %s: not a gitdir file", gitPath)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(gitPath), target)
	}
	return target, nil
}

// cachedOrigin returns dir's origin URL, consulting git only when the
// directory is a repository and its config changed since the last lookup.
func (c *Client) cachedOrigin(ctx context.Context, dir string) (string, bool) {
	gitPath := filepath.Join(dir, ".git")
	st, err := os.Stat(gitPath)
	if err != nil {
		return "", false
	}
	stamps, cacheable := originStamps(gitPath, st)
	if cacheable {
		c.mu.Lock()
		ent, hit := c.origins[gitPath]
		c.mu.Unlock()
		if hit && ent.stamps == stamps {
			return ent.url, ent.ok
		}
	}
	url, err := c.RemoteURL(ctx, dir)
	if err != nil {
		if _, isExit := exitCode(err); !isExit {
			return "", false // not git's answer (cancelled, timeout): do not cache
		}
		url = ""
	}
	ent := originEntry{stamps: stamps, url: url, ok: err == nil && url != ""}
	if cacheable {
		c.mu.Lock()
		c.origins[gitPath] = ent
		c.mu.Unlock()
	}
	return ent.url, ent.ok
}
