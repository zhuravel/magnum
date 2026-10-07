package gitx

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// MergeCheckRefPrefix holds the refs `magnum merge-check` keeps the commits
// it tests in (refs/magnum/merge-check/<slot>/…), apart from
// refs/magnum/pr/* (the daemon's PR heads), which it never touches. Being
// under refs/magnum/, they also keep a merged tree's commit from counting as
// unpushed work in the slot.
const MergeCheckRefPrefix = "refs/magnum/merge-check/"

// checkMergeCheckRef refuses a ref outside MergeCheckRefPrefix.
func checkMergeCheckRef(ref string) error {
	if err := checkBranch("ref", ref); err != nil {
		return err
	}
	if !strings.HasPrefix(ref, MergeCheckRefPrefix) || strings.HasSuffix(ref, "/") {
		return fmt.Errorf("gitx: refusing ref %q: only %s* may be written", ref, MergeCheckRefPrefix)
	}
	return nil
}

// FetchInto fetches src from origin into ref, a ref under
// MergeCheckRefPrefix, forcing the update, and returns the commit ref then
// points to. src is a ref the server has (refs/pull/N/head) or a full commit
// id (GitHub serves reachable commits by id).
func (c *Client) FetchInto(ctx context.Context, mainClone, src, ref string) (string, error) {
	if err := checkMergeCheckRef(ref); err != nil {
		return "", err
	}
	if err := checkRev("source", src); err != nil {
		return "", err
	}
	if strings.ContainsAny(src, ":+") || !(strings.HasPrefix(src, "refs/") || isOID(src)) {
		return "", fmt.Errorf("gitx: fetch %q: neither a ref nor a full commit id", src)
	}
	if err := checkDir(mainClone); err != nil {
		return "", fmt.Errorf("gitx: fetch %s: %w", src, err)
	}
	if err := c.fetch(ctx, mainClone, "fetch "+src, "+"+src+":"+ref); err != nil {
		return "", err
	}
	return c.RevParse(ctx, mainClone, ref)
}

// MergeTree merges commit theirs into commit ours as `git merge` would (the
// ort strategy), without a work tree, an index or a ref: `git merge-tree
// --write-tree`. It returns the merged tree's id, or the paths that
// conflict (each once, sorted) when the merge does not apply cleanly, which
// is an answer, not an error.
func (c *Client) MergeTree(ctx context.Context, dir, ours, theirs string) (tree string, conflicts []string, err error) {
	for _, rev := range []string{ours, theirs} {
		if err := checkRev("commit", rev); err != nil {
			return "", nil, err
		}
	}
	label := fmt.Sprintf("merge-tree %s %s", short(ours), short(theirs))
	res, err := c.git(ctx, dir, call{label: label, mutates: true, timeout: worktreeTimeout, probe: true},
		"merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", ours, theirs)
	conflicted := false
	if err != nil {
		if code, ok := exitCode(err); !ok || code != 1 {
			return "", nil, err
		}
		conflicted = true // exit 1: the merge has conflicts
	}
	// -z: "<tree>" NUL, then with conflicts one "<path>" NUL per path.
	fields := strings.Split(string(res.Stdout), "\x00")
	tree = strings.TrimSpace(fields[0])
	if !isOID(tree) {
		return "", nil, fmt.Errorf("gitx: %s: unexpected output %q", label, textHead(res.Stdout))
	}
	if !conflicted {
		return tree, nil, nil
	}
	for _, f := range fields[1:] {
		if f = strings.TrimSpace(f); f != "" {
			conflicts = append(conflicts, f)
		}
	}
	slices.Sort(conflicts)
	conflicts = slices.Compact(conflicts)
	if len(conflicts) == 0 {
		return "", nil, fmt.Errorf("gitx: %s: exit 1 without conflicted paths", label)
	}
	return tree, conflicts, nil
}

// CommitTree writes a commit object for tree with parents and message,
// moving no ref and touching no work tree (`git commit-tree`, unsigned, as
// "magnum" unless the environment names someone). It returns its id.
func (c *Client) CommitTree(ctx context.Context, dir, tree, message string, parents ...string) (string, error) {
	if !isOID(tree) {
		return "", fmt.Errorf("gitx: commit-tree: %q is not a tree id", tree)
	}
	args := []string{"-c", "user.name=magnum", "-c", "user.email=magnum@localhost", "commit-tree", "--no-gpg-sign"}
	for _, p := range parents {
		if !isOID(p) {
			return "", fmt.Errorf("gitx: commit-tree: parent %q is not a commit id", p)
		}
		args = append(args, "-p", p)
	}
	args = append(args, "-m", message, tree)
	res, err := c.git(ctx, dir, call{label: "commit-tree " + short(tree), mutates: true}, args...)
	if err != nil {
		return "", err
	}
	sha := res.Out()
	if !isOID(sha) {
		return "", fmt.Errorf("gitx: commit-tree %s: unexpected output %q", short(tree), sha)
	}
	return sha, nil
}

// UpdateRef points ref, a ref under MergeCheckRefPrefix, at commit sha
// (never through a symbolic ref). UpdateRefDelete removes it.
func (c *Client) UpdateRef(ctx context.Context, dir, ref, sha string) error {
	if err := checkMergeCheckRef(ref); err != nil {
		return err
	}
	if !isOID(sha) {
		return fmt.Errorf("gitx: update-ref %s: %q is not a commit id", ref, sha)
	}
	_, err := c.git(ctx, dir, call{label: "update-ref " + ref, mutates: true}, "update-ref", "--no-deref", ref, sha)
	return err
}

// short is the first 7 characters of a revision, for labels.
func short(rev string) string {
	if len(rev) > 7 && isOID(rev) {
		return rev[:7]
	}
	return rev
}

// textHead is the start of b, for an error message.
func textHead(b []byte) string {
	const n = 80
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
