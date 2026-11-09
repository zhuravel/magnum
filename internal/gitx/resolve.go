package gitx

// The git of a staging merge's resolution (the review daemon's staging
// resolver): the merge left in a work tree for an agent to resolve, the
// tree of what it resolved, written apart from the work tree's index, the
// merge commit's parents and the ref that keeps it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
)

// MergeNoCommit merges rev into the work tree at dir and stops before it
// commits (`git merge --no-commit --no-ff`): HEAD stays where it is, the
// merge is in progress (MERGE_HEAD), and the conflicted paths, sorted, are
// what it returns (nil when the merge applied cleanly). who stands in for
// the committer git asks for; nothing is committed. Rerere is off, so no
// recorded resolution resolves a path in the agent's place.
func (c *Client) MergeNoCommit(ctx context.Context, dir, rev string, who Signature) ([]string, error) {
	if err := checkRev("revision", rev); err != nil {
		return nil, err
	}
	if err := checkSignature(who); err != nil {
		return nil, err
	}
	env := map[string]string{"GIT_AUTHOR_NAME": who.Name, "GIT_AUTHOR_EMAIL": who.Email, "GIT_COMMITTER_NAME": who.Name, "GIT_COMMITTER_EMAIL": who.Email}
	_, err := c.git(ctx, dir, call{label: "merge --no-commit " + rev, mutates: true, timeout: commitTimeout, noExit: 1, env: env},
		"-c", "rerere.enabled=false", "-c", "merge.autoStash=false", "merge", "--no-commit", "--no-ff", "--no-edit", rev)
	if err == nil {
		return nil, nil
	}
	if code, ok := exitCode(err); !ok || code != 1 {
		return nil, err
	}
	paths, uerr := c.unmergedPaths(ctx, dir)
	switch {
	case uerr != nil:
		return nil, uerr
	case len(paths) == 0:
		return nil, fmt.Errorf("gitx: merge --no-commit %s: it failed without a conflicted path: %w", rev, err)
	}
	return paths, nil
}

// MergeTreeWrite is `git merge-tree --write-tree a b` with both its
// answers: the merged tree (conflict markers in its conflicted files) and
// the conflicted paths, sorted, nil for a clean merge. It writes objects
// alone: no ref, index or work tree.
func (c *Client) MergeTreeWrite(ctx context.Context, dir, a, b string) (tree string, conflicts []string, err error) {
	if err := errors.Join(checkRev("revision", a), checkRev("revision", b)); err != nil {
		return "", nil, err
	}
	what := "merge-tree " + a + " " + b
	if tree, conflicts, err = c.mergeTree(ctx, dir, what, a, b); err != nil {
		return "", nil, err
	}
	if !isOID(tree) {
		return "", nil, fmt.Errorf("gitx: %s: unreadable tree %.40q", what, tree)
	}
	return tree, conflicts, nil
}

// The kinds of conflict merge-tree's messages name (MergeMessage.Kind) that a
// staging merge's resolver refuses to resolve: git's stable names for them.
const (
	ConflictBinary       = "CONFLICT (binary)"
	ConflictRenameRename = "CONFLICT (rename/rename)"
)

// MergeMessage is one of the informational messages of `git merge-tree
// --write-tree`: its kind, git's stable name for it ("Auto-merging",
// "CONFLICT (contents)", ConflictBinary, ConflictRenameRename,
// "CONFLICT (modify/delete)" and so on), and the paths it is about, in
// git's order. Its text, for people, is not kept.
type MergeMessage struct {
	Kind  string
	Paths []string
}

// MergeTreeMessages lists the messages of merging b into a, in git's order
// (`git merge-tree --write-tree -z --name-only a b`, read from its
// informational messages section); none for a clean merge. It writes
// objects alone: no ref, index or work tree.
func (c *Client) MergeTreeMessages(ctx context.Context, dir, a, b string) ([]MergeMessage, error) {
	if err := errors.Join(checkRev("revision", a), checkRev("revision", b)); err != nil {
		return nil, err
	}
	what := "merge-tree " + a + " " + b
	res, err := c.git(ctx, dir, call{label: what, noExit: 1, timeout: commitTimeout}, "merge-tree", "--write-tree", "-z", "--name-only", a, b)
	if err == nil {
		return nil, nil
	}
	if code, ok := exitCode(err); !ok || code != 1 {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("gitx: %s: more than %d bytes", what, execx.MaxOutput)
	}
	// "<tree>\0<path>\0...\0\0" (the conflicted paths, ended by an empty
	// field), then each message: "<n>\0<path 1>\0...<path n>\0<kind>\0<text>\0".
	fields := strings.Split(string(res.Stdout), "\x00")
	bad := func() error { return fmt.Errorf("gitx: %s: unreadable messages %.80q", what, res.Stdout) }
	if !isOID(fields[0]) {
		return nil, bad()
	}
	i := 1
	for i < len(fields) && fields[i] != "" {
		i++
	}
	i++ // the empty field that ends the paths
	var out []MergeMessage
	for i < len(fields) && fields[i] != "" {
		n, err := strconv.Atoi(fields[i])
		if err != nil || n < 0 || i+n+2 >= len(fields) {
			return nil, bad()
		}
		out = append(out, MergeMessage{Kind: fields[i+n+1], Paths: slices.Clone(fields[i+1 : i+1+n])})
		i += n + 3
	}
	return out, nil
}

// WriteTreeFrom writes the tree that is base with each of paths as the work
// tree at dir holds it (added or changed; a path the work tree lacks is
// left out of it) and returns its id. It goes through an index of its own
// (GIT_INDEX_FILE in a temporary directory), so the work tree's index,
// with whatever a merge in progress or an agent left in it, is neither
// read nor changed.
func (c *Client) WriteTreeFrom(ctx context.Context, dir, base string, paths []string) (string, error) {
	if err := checkRev("tree", base); err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", errors.New("gitx: write-tree: no path")
	}
	var list strings.Builder
	for _, p := range paths {
		if p == "" || strings.ContainsRune(p, 0) || filepath.IsAbs(p) {
			return "", fmt.Errorf("gitx: write-tree: %q is no path of the repository", p)
		}
		list.WriteString(p)
		list.WriteByte(0)
	}
	tmp, err := os.MkdirTemp("", "magnum-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := map[string]string{"GIT_INDEX_FILE": filepath.Join(tmp, "index")}
	if _, err := c.git(ctx, dir, call{label: "read-tree " + base, mutates: true, timeout: commitTimeout, env: env}, "read-tree", base); err != nil {
		return "", err
	}
	if _, err := c.runStdin(ctx, dir, call{label: "update-index", mutates: true, timeout: commitTimeout, env: env}, []byte(list.String()),
		"update-index", "--add", "--remove", "-z", "--stdin"); err != nil {
		return "", err
	}
	res, err := c.git(ctx, dir, call{label: "write-tree", mutates: true, timeout: commitTimeout, env: env}, "write-tree")
	if err != nil {
		return "", err
	}
	if tree := res.Out(); isOID(tree) {
		return tree, nil
	}
	return "", fmt.Errorf("gitx: write-tree: unreadable tree %.40q", res.Out())
}

// WorkTreeDiff lists the tracked paths whose content in the work tree at dir
// is not rev's (a commit or a tree), sorted: `git diff --name-only rev`,
// without rename detection. Untracked files are not listed.
func (c *Client) WorkTreeDiff(ctx context.Context, dir, rev string) ([]string, error) {
	if err := checkRev("revision", rev); err != nil {
		return nil, err
	}
	if strings.Contains(rev, "..") {
		return nil, fmt.Errorf("gitx: revision %q must not be a range", rev)
	}
	res, err := c.git(ctx, dir, call{label: "diff --name-only " + rev},
		"diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", rev, "--")
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

// Parents lists the parents of commit rev, in order.
func (c *Client) Parents(ctx context.Context, dir, rev string) ([]string, error) {
	if err := checkRev("revision", rev); err != nil {
		return nil, err
	}
	res, err := c.git(ctx, dir, call{label: "rev-list --parents " + rev}, "rev-list", "--parents", "-n", "1", rev+"^{commit}", "--")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(res.Out())
	if len(fields) == 0 || !slices.ContainsFunc(fields, isOID) || slices.ContainsFunc(fields, func(f string) bool { return !isOID(f) }) {
		return nil, fmt.Errorf("gitx: rev-list --parents %s: unreadable answer %.80q", rev, res.Out())
	}
	return fields[1:], nil
}

// UpdateRef points ref, under refs/magnum/, at the commit sha (a full id):
// `git update-ref --no-deref`. Any other ref is refused.
func (c *Client) UpdateRef(ctx context.Context, dir, ref, sha string) error {
	if err := checkBranch("ref", ref); err != nil {
		return err
	}
	if !strings.HasPrefix(ref, magnumRefs) || strings.HasSuffix(ref, "/") {
		return fmt.Errorf("gitx: refusing to write %q: only refs/magnum/* may be written", ref)
	}
	if !isOID(sha) {
		return fmt.Errorf("gitx: update-ref %s: %q is no full commit id", ref, sha)
	}
	_, err := c.git(ctx, dir, call{label: "update-ref " + ref, mutates: true}, "update-ref", "--no-deref", ref, sha)
	return err
}

// checkSignature refuses a name and email git would not take as one: an
// empty, multi-line or bracketed part.
func checkSignature(who Signature) error {
	for _, part := range []string{who.Name, who.Email} {
		if part == "" || strings.ContainsAny(part, "<>\n\r\x00") {
			return fmt.Errorf("gitx: signature %q is no name and email", who.Name+" <"+who.Email+">")
		}
	}
	return nil
}
