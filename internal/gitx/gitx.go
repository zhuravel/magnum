// Package gitx holds every git operation magnum needs: fetching PR heads into
// refs/magnum/pr/N, detached switches and placeholder resets in review slots,
// worktree management, dirty/unpushed guards and the diff queries behind
// "did this PR touch db/".
//
// All commands go through an execx.Runner (never os/exec), run as
// `git -C <dir> ...`, and are marked Mutates when they change repository state
// so --dry-run only prints them. Inputs that end up as git arguments (refs,
// branch names, worktree paths) are validated so a hostile branch name can
// never be parsed as an option. The variables that redirect git to another
// repository (GIT_DIR and friends, see ScrubbedEnv) are removed from the
// environment of every command.
package gitx

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/execx"
)

// remote is the only remote magnum talks to.
const remote = "origin"

// Timeouts for the operations that can legitimately run long on a big repo.
const (
	fetchTimeout    = 5 * time.Minute
	worktreeTimeout = 5 * time.Minute
	cloneTimeout    = 30 * time.Minute
)

var (
	// ErrNoSuchRef is returned when a ref or revision does not resolve.
	ErrNoSuchRef = errors.New("gitx: no such ref")
	// ErrNoMergeBase is returned when two revisions share no history.
	ErrNoMergeBase = errors.New("gitx: no merge base")
)

// PRRef is the local ref a PR head is fetched into.
func PRRef(number int) string { return fmt.Sprintf("refs/magnum/pr/%d", number) }

// Client runs git through an execx.Runner. It is safe for concurrent use;
// fetches into the same clone are serialized because concurrent fetches of the
// same ref (two PRs sharing a base branch) fail with "cannot lock ref". The
// lock only covers this process; a fetch that still loses the race to another
// process (the CLI next to the daemon) is retried once.
type Client struct {
	run execx.Runner

	// HTTPSFetch makes fetches from a clone whose origin is a github.com SSH
	// URL go over HTTPS with gh as the credential helper (see fetchArgs).
	// The app turns it on; the zero value keeps fetching from origin.
	HTTPSFetch bool

	mu       sync.Mutex
	fetching map[string]chan struct{} // per clone, a 1-slot semaphore
	origins  map[string]originEntry   // FindClone's origin cache, by .git path
}

// scrubbedEnv lists the variables that point git at a repository, work tree,
// index, object store or ref namespace other than the one -C names. An
// inherited GIT_DIR would make `git -C <slot> switch --discard-changes` move
// another repository's HEAD.
var scrubbedEnv = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
}

// ScrubbedEnv returns the environment variables gitx removes from every git
// command it runs. Code that runs git outside gitx should put the same list
// in execx.Cmd.Unset.
func ScrubbedEnv() []string { return slices.Clone(scrubbedEnv) }

// New returns a Client that executes commands with r.
func New(r execx.Runner) *Client {
	return &Client{run: r, fetching: make(map[string]chan struct{}), origins: make(map[string]originEntry)}
}

// call describes how one git invocation is run.
type call struct {
	label   string // short human name; also the error context
	mutates bool
	timeout time.Duration
	probe   bool // a non-zero exit is an expected answer, logged at Debug
}

// git runs `git -C dir args...`. Errors are wrapped with the label and keep the
// underlying *execx.ExitError reachable through errors.As.
func (c *Client) git(ctx context.Context, dir string, k call, args ...string) (execx.Result, error) {
	if err := checkDir(dir); err != nil {
		return execx.Result{}, fmt.Errorf("gitx: %s: %w", k.label, err)
	}
	cmd := execx.Cmd{
		Name:    "git",
		Args:    append([]string{"-C", dir}, args...),
		Env:     gitEnv(k.mutates),
		Unset:   ScrubbedEnv(),
		Timeout: k.timeout,
		Mutates: k.mutates,
		Label:   "git " + k.label,
		Probe:   k.probe,
	}
	// -C is resolved against the process cwd, so only an absolute dir may also
	// become Dir (a relative one would be applied twice).
	if filepath.IsAbs(dir) {
		cmd.Dir = dir
	}
	res, err := c.run.Run(ctx, cmd)
	if err != nil {
		return res, fmt.Errorf("gitx: %s: %w", k.label, err)
	}
	return res, nil
}

// gitEnv never lets git prompt for credentials (the daemon has no tty) and
// keeps read-only commands from taking the index lock, which would otherwise
// race with a human running git in the same worktree.
func gitEnv(mutates bool) map[string]string {
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	if !mutates {
		env["GIT_OPTIONAL_LOCKS"] = "0"
	}
	return env
}

// exitCode returns the process exit code carried by err, if any.
func exitCode(err error) (int, bool) {
	var ee *execx.ExitError
	if errors.As(err, &ee) {
		return ee.Code, true
	}
	return 0, false
}

func checkDir(dir string) error {
	if dir == "" {
		return errors.New("empty directory")
	}
	return nil
}

// checkRev rejects anything that git could mistake for an option or that is
// not a single word. It accepts the usual revision syntax (HEAD~1, x^{commit}).
func checkRev(what, s string) error {
	if s == "" {
		return fmt.Errorf("gitx: empty %s", what)
	}
	if strings.HasPrefix(s, "-") {
		return fmt.Errorf("gitx: %s %q looks like an option", what, s)
	}
	if strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("gitx: %s %q contains whitespace or control characters", what, s)
	}
	return nil
}

// checkBranch is checkRev plus the characters a branch name may not contain.
func checkBranch(what, s string) error {
	if err := checkRev(what, s); err != nil {
		return err
	}
	if strings.ContainsAny(s, ":?*[\\^~") || strings.Contains(s, "..") || strings.Contains(s, "@{") {
		return fmt.Errorf("gitx: %s %q is not a valid branch name", what, s)
	}
	return nil
}

func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// lockFetch serializes fetches per clone and honors ctx while waiting.
func (c *Client) lockFetch(ctx context.Context, clone string) (unlock func(), err error) {
	key := filepath.Clean(clone)
	c.mu.Lock()
	sem, ok := c.fetching[key]
	if !ok {
		sem = make(chan struct{}, 1)
		c.fetching[key] = sem
	}
	c.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("gitx: waiting for fetch lock on %s: %w", clone, ctx.Err())
	}
}

// lockRetryDelay is the pause before the one retry of a fetch that lost a ref
// lock to another process. A variable so tests can shorten it.
var lockRetryDelay = 500 * time.Millisecond

// fetch runs `git fetch --no-tags origin <refspec>` in mainClone under the
// per-clone lock. A "cannot lock ref" failure means another process (the CLI
// next to the daemon) is updating the same ref, which the in-process lock
// cannot prevent; it is retried once after lockRetryDelay.
func (c *Client) fetch(ctx context.Context, mainClone, label, refspec string) error {
	unlock, err := c.lockFetch(ctx, mainClone)
	if err != nil {
		return err
	}
	defer unlock()
	k := call{label: label, mutates: true, timeout: fetchTimeout}
	args := c.fetchArgs(ctx, mainClone, refspec)
	_, err = c.git(ctx, mainClone, k, args...)
	var ee *execx.ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Stderr, "cannot lock ref") {
		return err
	}
	select {
	case <-time.After(lockRetryDelay):
	case <-ctx.Done():
		return fmt.Errorf("gitx: %s: waiting to retry: %w", label, errors.Join(ctx.Err(), err))
	}
	_, err = c.git(ctx, mainClone, k, args...)
	return err
}

// fetchArgs is the fetch command line for mainClone. A clone whose origin is a
// github.com SSH URL is fetched over HTTPS with gh as the only credential
// helper instead: magnum's fetches must not depend on the user's SSH agent
// being unlocked (a locked agent serves no keys and every fetch failed with
// "Permission denied (publickey)"), while gh's token is always available. The
// refspecs name their destinations, so fetching from the URL updates the same
// refs as fetching from origin. Any other origin is fetched as is.
func (c *Client) fetchArgs(ctx context.Context, mainClone, refspec string) []string {
	url := ""
	if c.HTTPSFetch {
		url, _ = c.RemoteURL(ctx, mainClone)
	}
	opts, target := NetworkRemote(url, c.HTTPSFetch)
	return append(opts, "fetch", "--no-tags", target, refspec)
}

// EvalRefPrefix holds the refs magnum eval fetches pinned commits into, apart
// from refs/magnum/pr/* (the daemon's PR heads), which it never touches.
const EvalRefPrefix = "refs/magnum/eval/"

// FetchCommit makes sha (a full commit id) present in mainClone for a magnum
// eval replay, or for a post-merge round whose clone lacks the PR's merge
// commit. A commit already there costs one rev-parse. Otherwise it
// fetches the commit by id into refs/magnum/eval/<sha>, and when the server
// refuses that, PR number's head into refs/magnum/eval/pr-<number> (the
// commit is there unless the PR was force-pushed past it). It never writes
// refs/magnum/pr/*, so the daemon's checkouts are unaffected.
func (c *Client) FetchCommit(ctx context.Context, mainClone, sha string, number int) error {
	if !isOID(sha) {
		return fmt.Errorf("gitx: fetch commit: %q is not a full commit id", sha)
	}
	if _, err := c.RevParse(ctx, mainClone, sha); err == nil {
		return nil
	}
	byID := c.fetch(ctx, mainClone, "fetch commit "+sha[:12], "+"+sha+":"+EvalRefPrefix+sha)
	if byID == nil {
		return nil
	}
	if number <= 0 {
		return byID
	}
	ref := fmt.Sprintf("%spr-%d", EvalRefPrefix, number)
	if err := c.fetch(ctx, mainClone, fmt.Sprintf("fetch PR %d for %s", number, sha[:12]), fmt.Sprintf("+refs/pull/%d/head:%s", number, ref)); err != nil {
		return errors.Join(byID, err)
	}
	if _, err := c.RevParse(ctx, mainClone, sha); err != nil {
		return fmt.Errorf("gitx: commit %s is neither fetchable by id nor in PR %d's history (force-pushed away?): %w", sha[:12], number, err)
	}
	return nil
}

// FetchPR fetches the PR head (refs/pull/N/head) into refs/magnum/pr/N of
// mainClone, forcing the update so a force-pushed PR moves the ref, and returns
// the commit sha it now points to. Under --dry-run the fetch is only planned,
// so the final lookup returns whatever refs/magnum/pr/N already holds (the sha
// of an earlier real fetch, possibly stale) or fails with ErrNoSuchRef.
func (c *Client) FetchPR(ctx context.Context, mainClone string, number int) (string, error) {
	if number <= 0 {
		return "", fmt.Errorf("gitx: invalid PR number %d", number)
	}
	if err := checkDir(mainClone); err != nil {
		return "", fmt.Errorf("gitx: fetch PR %d: %w", number, err)
	}
	label := fmt.Sprintf("fetch PR %d", number)
	if err := c.fetch(ctx, mainClone, label, fmt.Sprintf("+refs/pull/%d/head:%s", number, PRRef(number))); err != nil {
		return "", err
	}
	sha, err := c.RevParse(ctx, mainClone, PRRef(number))
	if err != nil {
		return "", fmt.Errorf("gitx: fetch PR %d: %w", number, err)
	}
	return sha, nil
}

// FetchBranch updates refs/remotes/origin/<base> in mainClone from the remote.
func (c *Client) FetchBranch(ctx context.Context, mainClone, base string) error {
	if err := checkBranch("base branch", base); err != nil {
		return err
	}
	if err := checkDir(mainClone); err != nil {
		return fmt.Errorf("gitx: fetch %s: %w", base, err)
	}
	return c.fetch(ctx, mainClone, "fetch "+base, fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", base, remote, base))
}

// RevParse resolves ref to a full commit sha (tags are peeled). A ref that does
// not exist yields an error wrapping ErrNoSuchRef.
func (c *Client) RevParse(ctx context.Context, dir, ref string) (string, error) {
	if err := checkRev("ref", ref); err != nil {
		return "", err
	}
	res, err := c.git(ctx, dir, call{label: "rev-parse " + ref, probe: true}, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		if code, ok := exitCode(err); ok && code == 1 {
			return "", fmt.Errorf("gitx: rev-parse %s: %w", ref, ErrNoSuchRef)
		}
		return "", err
	}
	sha := res.Out()
	if !isOID(sha) {
		return "", fmt.Errorf("gitx: rev-parse %s: unexpected output %q", ref, sha)
	}
	return sha, nil
}

// SwitchDetach detaches dir's HEAD at ref, discarding tracked changes
// (untracked files stay). Callers verify the result with RevParse.
func (c *Client) SwitchDetach(ctx context.Context, dir, ref string) error {
	if err := checkRev("ref", ref); err != nil {
		return err
	}
	_, err := c.git(ctx, dir, call{label: "switch --detach " + ref, mutates: true, timeout: worktreeTimeout},
		"switch", "--quiet", "--discard-changes", "--detach", ref)
	return err
}

// ResetPlaceholder points the slot's placeholder branch at origin/<base>
// without upstream tracking, discarding tracked changes. It is idempotent.
// `switch -C --no-track` only stops git from creating tracking; a branch that
// already tracks something keeps it, so an existing upstream is unset
// afterwards (placeholders must not follow, or push to, a remote branch).
func (c *Client) ResetPlaceholder(ctx context.Context, dir, branch, base string) error {
	if err := checkBranch("placeholder branch", branch); err != nil {
		return err
	}
	if err := checkBranch("base branch", base); err != nil {
		return err
	}
	_, err := c.git(ctx, dir, call{label: fmt.Sprintf("reset %s to %s/%s", branch, remote, base), mutates: true, timeout: worktreeTimeout},
		"switch", "--quiet", "--discard-changes", "--no-track", "-C", branch, remote+"/"+base)
	if err != nil {
		return err
	}
	// `git branch --unset-upstream` fails on a branch without upstream, so ask
	// first; exit code 1 of `config --get` means the key is not set, and an
	// empty value names no upstream either.
	res, err := c.git(ctx, dir, call{label: "config branch." + branch + ".merge"}, "config", "--get", "branch."+branch+".merge")
	if err != nil {
		if code, isExit := exitCode(err); isExit && code == 1 {
			return nil
		}
		return err
	}
	if res.Out() == "" {
		return nil
	}
	_, err = c.git(ctx, dir, call{label: "branch --unset-upstream " + branch, mutates: true}, "branch", "--unset-upstream", branch)
	return err
}

// BranchDelete deletes a local branch (force = -D, otherwise -d). A branch that
// does not exist counts as deleted; git's refusal to delete an existing branch
// (checked out, unmerged) is returned.
func (c *Client) BranchDelete(ctx context.Context, mainClone, branch string, force bool) error {
	if err := checkBranch("branch", branch); err != nil {
		return err
	}
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := c.git(ctx, mainClone, call{label: "branch " + flag + " " + branch, mutates: true}, "branch", flag, branch)
	if err == nil {
		return nil
	}
	if _, lookup := c.RevParse(ctx, mainClone, "refs/heads/"+branch); errors.Is(lookup, ErrNoSuchRef) {
		return nil
	}
	return err
}

// magnumRefs is the only namespace UpdateRefDelete may touch.
const magnumRefs = "refs/magnum/"

// UpdateRefDelete deletes a ref under refs/magnum/ (for example PRRef(n)). It
// is idempotent: a missing ref is not an error. Other namespaces are refused so
// a bug can never delete a real branch or tag. A symbolic ref is never
// followed: one pointing outside refs/magnum/ is refused, and the delete runs
// with --no-deref so git removes the ref itself, not its target.
func (c *Client) UpdateRefDelete(ctx context.Context, mainClone, ref string) error {
	if err := checkBranch("ref", ref); err != nil {
		return err
	}
	if !strings.HasPrefix(ref, magnumRefs) || strings.HasSuffix(ref, "/") {
		return fmt.Errorf("gitx: refusing to delete %q: only refs/magnum/* may be deleted", ref)
	}
	// symbolic-ref -q exits 1 for a missing or non-symbolic ref and prints
	// the target of a symbolic one.
	res, err := c.git(ctx, mainClone, call{label: "symbolic-ref " + ref}, "symbolic-ref", "-q", ref)
	if err == nil {
		if target := res.Out(); target != "" && !strings.HasPrefix(target, magnumRefs) {
			return fmt.Errorf("gitx: refusing to delete %q: it is a symbolic ref to %q outside refs/magnum/", ref, target)
		}
	} else if code, isExit := exitCode(err); !isExit || code != 1 {
		return err
	}
	_, err = c.git(ctx, mainClone, call{label: "update-ref -d " + ref, mutates: true}, "update-ref", "-d", "--no-deref", ref)
	return err
}

// MergeBase returns the best common ancestor of a and b, or ErrNoMergeBase.
func (c *Client) MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	if err := checkRev("revision", a); err != nil {
		return "", err
	}
	if err := checkRev("revision", b); err != nil {
		return "", err
	}
	res, err := c.git(ctx, dir, call{label: fmt.Sprintf("merge-base %s %s", a, b)}, "merge-base", a, b)
	if err != nil {
		if code, ok := exitCode(err); ok && code == 1 {
			return "", fmt.Errorf("gitx: merge-base %s %s: %w", a, b, ErrNoMergeBase)
		}
		return "", err
	}
	sha := res.Out()
	if !isOID(sha) {
		return "", fmt.Errorf("gitx: merge-base %s %s: unexpected output %q", a, b, sha)
	}
	return sha, nil
}

// FileDiff returns the unified diff of one file from base to head (git diff
// base head -- path; path is relative to the repository's top and taken
// literally) as GitHub shows a pull request's patch: three lines of
// context, hunks never merged across a gap, and none of the user's settings
// that change the hunks (an external diff, textconv, a context size). ""
// when the file did not change; a binary file has a header and no hunks.
func (c *Client) FileDiff(ctx context.Context, dir, base, head, path string) (string, error) {
	for _, rev := range []string{base, head} {
		if err := checkRev("revision", rev); err != nil {
			return "", err
		}
	}
	if path == "" {
		return "", errors.New("gitx: diff needs a path")
	}
	res, err := c.git(ctx, dir, call{label: fmt.Sprintf("diff %s %s", base, head)},
		"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--unified=3", "--inter-hunk-context=0",
		base, head, "--", ":(top,literal)"+path)
	if err != nil {
		return "", err
	}
	return string(res.Stdout), nil
}

// ChangedPaths lists the files head changed relative to its merge base with
// base (git diff base...head), optionally limited to pathspecs such as "db/".
// Renames are reported as a deletion plus an addition so a move out of a
// watched directory still shows up. The result is nil when nothing matches.
func (c *Client) ChangedPaths(ctx context.Context, dir, base, head string, pathspecs ...string) ([]string, error) {
	return c.diffNames(ctx, dir, base, head, nil, pathspecs)
}

// ModifiedPaths is ChangedPaths without the files head adds: the paths that
// exist at the merge base and that head modifies, deletes or changes the
// type of (a rename counts as the deletion of its old path), whose history
// the base holds.
func (c *Client) ModifiedPaths(ctx context.Context, dir, base, head string) ([]string, error) {
	return c.diffNames(ctx, dir, base, head, []string{"--diff-filter=a"}, nil)
}

// ChangedUnder is ChangedPaths limited to the files at or under paths, each
// relative to the repository's top and taken literally (a directory or a
// file): what head changes there since its merge base with base, whatever
// base did since. One git diff; nil when head leaves them all alone.
func (c *Client) ChangedUnder(ctx context.Context, dir, base, head string, paths ...string) ([]string, error) {
	specs, err := literalPaths(paths)
	if err != nil {
		return nil, err
	}
	return c.diffNames(ctx, dir, base, head, nil, specs)
}

// literalPaths are paths as pathspecs taken literally from the
// repository's top; at least one, none empty.
func literalPaths(paths []string) ([]string, error) {
	if len(paths) == 0 || slices.Contains(paths, "") {
		return nil, errors.New("gitx: the comparison needs a path")
	}
	specs := make([]string, len(paths))
	for i, p := range paths {
		specs[i] = ":(top,literal)" + p
	}
	return specs, nil
}

// WorkTreeChanges lists the files at or under paths (each relative to the
// repository's top, taken literally: a directory or a file) whose state on
// disk differs from base, as a tool reading them sees them: committed and
// uncommitted changes and deletions (git diff base -- paths), and the
// untracked files, ignored ones included (git ls-files --others), except
// those skip reports (nil skips none), e.g. logs the tool never reads; a
// tracked change counts whatever skip says of its path. Sorted, without
// duplicates; nil when they all match base.
func (c *Client) WorkTreeChanges(ctx context.Context, dir, base string, skip func(untracked string) bool, paths ...string) ([]string, error) {
	if err := checkRev("revision", base); err != nil {
		return nil, err
	}
	if strings.Contains(base, "..") {
		return nil, fmt.Errorf("gitx: revision %q must not be a range", base)
	}
	specs, err := literalPaths(paths)
	if err != nil {
		return nil, err
	}
	diff, err := c.git(ctx, dir, call{label: "diff --name-only " + base},
		append([]string{"diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", base, "--"}, specs...)...)
	if err != nil {
		return nil, err
	}
	others, err := c.git(ctx, dir, call{label: "ls-files --others"}, append([]string{"ls-files", "-z", "--others", "--"}, specs...)...)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, p := range strings.Split(string(diff.Stdout), "\x00") {
		if p != "" {
			changed = append(changed, p)
		}
	}
	for _, p := range strings.Split(string(others.Stdout), "\x00") {
		if p != "" && (skip == nil || !skip(p)) {
			changed = append(changed, p)
		}
	}
	slices.Sort(changed)
	return slices.Compact(changed), nil
}

// diffNames lists the paths of `git diff --name-only base...head`, opts
// after the fixed options and pathspecs after "--".
func (c *Client) diffNames(ctx context.Context, dir, base, head string, opts, pathspecs []string) ([]string, error) {
	for _, rev := range []string{base, head} {
		if err := checkRev("revision", rev); err != nil {
			return nil, err
		}
		if strings.Contains(rev, "..") {
			return nil, fmt.Errorf("gitx: revision %q must not be a range", rev)
		}
	}
	args := append([]string{"diff", "--name-only", "-z", "--no-renames"}, opts...)
	args = append(append(args, base+"..."+head, "--"), pathspecs...)
	res, err := c.git(ctx, dir, call{label: fmt.Sprintf("diff --name-only %s...%s", base, head)}, args...)
	if err != nil {
		var ee *execx.ExitError
		if errors.As(err, &ee) && strings.Contains(ee.Stderr, "no merge base") {
			return nil, fmt.Errorf("gitx: diff %s...%s: %w", base, head, ErrNoMergeBase)
		}
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(res.Stdout), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// Commit is one commit of a file's log (FileLog).
type Commit struct {
	SHA     string // git's unique abbreviation of the commit id
	Date    string // the committer date, YYYY-MM-DD
	Subject string // the message's subject line ("" when the message is empty)
}

// FileLog returns the last n commits reachable from rev that changed path,
// newest first (git log -n <n> <rev> -- path, with git's default history
// simplification: a merge shows only when it changed the file against
// every parent; path is relative to the repository's top and taken
// literally). A path rev never had has no commits.
func (c *Client) FileLog(ctx context.Context, dir, rev, path string, n int) ([]Commit, error) {
	if err := checkRev("revision", rev); err != nil {
		return nil, err
	}
	if strings.Contains(rev, "..") {
		return nil, fmt.Errorf("gitx: revision %q must not be a range", rev)
	}
	if path == "" || n <= 0 {
		return nil, fmt.Errorf("gitx: log needs a path and a positive count (path %q, n %d)", path, n)
	}
	res, err := c.git(ctx, dir, call{label: "log " + rev},
		"log", "-n", strconv.Itoa(n), "-z", "--no-color", "--no-show-signature", "--format=%h%x00%cs%x00%s",
		rev, "--", ":(top,literal)"+path)
	if err != nil {
		return nil, err
	}
	out := string(res.Stdout)
	if out == "" {
		return nil, nil
	}
	// Each commit is "<sha> NUL <date> NUL <subject>", NUL-terminated (-z).
	f := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(f)%3 != 0 {
		return nil, fmt.Errorf("gitx: log %s: unexpected output of %d fields", rev, len(f))
	}
	commits := make([]Commit, 0, len(f)/3)
	for i := 0; i < len(f); i += 3 {
		commits = append(commits, Commit{SHA: f[i], Date: f[i+1], Subject: f[i+2]})
	}
	return commits, nil
}

// Empty tree ids of the two object formats: TreeFiles diffs a commit against
// the empty tree to list its files.
const (
	emptyTreeSHA1   = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	emptyTreeSHA256 = "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"
)

// TreeFiles lists the files of rev (a commit) that pathspecs match, with the
// pathspec rules ChangedPaths has ("db/", a glob, pathspec magic), each as
// "<mode> <blob id> <path>", in git's order; nil when none matches. It is
// `git diff-tree` of the empty tree against rev, unabbreviated, so the list
// changes exactly when a matching file's content, mode or name does. A rev
// that does not resolve is ErrNoSuchRef.
func (c *Client) TreeFiles(ctx context.Context, dir, rev string, pathspecs ...string) ([]string, error) {
	sha, err := c.RevParse(ctx, dir, rev)
	if err != nil {
		return nil, err
	}
	empty := emptyTreeSHA1
	if len(sha) == 64 {
		empty = emptyTreeSHA256
	}
	args := append([]string{"diff-tree", "-r", "-z", "--no-renames", "--no-abbrev", empty, sha, "--"}, pathspecs...)
	res, err := c.git(ctx, dir, call{label: "diff-tree " + sha[:7]}, args...)
	if err != nil {
		return nil, err
	}
	// -z raw records: ":<mode> <mode> <oid> <oid> <status>" NUL "<path>" NUL.
	parts := strings.Split(strings.TrimSuffix(string(res.Stdout), "\x00"), "\x00")
	var out []string
	for i := 0; i+1 < len(parts); i += 2 {
		f := strings.Fields(parts[i])
		if len(f) != 5 {
			return nil, fmt.Errorf("gitx: diff-tree %s: unexpected record %q", rev, parts[i])
		}
		out = append(out, f[1]+" "+f[3]+" "+parts[i+1])
	}
	return out, nil
}

var pullURLNumber = regexp.MustCompile(`/pull/(\d+)`)

// BranchPR returns the PR number recorded in `git config branch.<b>.pr` (the
// convention of Bohdan's `pull` helper; a PR URL is accepted too). ok is false
// when the key is not set.
func (c *Client) BranchPR(ctx context.Context, dir, branch string) (number int, ok bool, err error) {
	if err := checkBranch("branch", branch); err != nil {
		return 0, false, err
	}
	res, err := c.git(ctx, dir, call{label: "config branch." + branch + ".pr"}, "config", "--get", "branch."+branch+".pr")
	if err != nil {
		if code, isExit := exitCode(err); isExit && code == 1 {
			return 0, false, nil
		}
		return 0, false, err
	}
	val := res.Out()
	if n, convErr := strconv.Atoi(val); convErr == nil && n > 0 {
		return n, true, nil
	}
	if m := pullURLNumber.FindStringSubmatch(val); m != nil {
		if n, convErr := strconv.Atoi(m[1]); convErr == nil && n > 0 {
			return n, true, nil
		}
	}
	return 0, false, fmt.Errorf("gitx: branch.%s.pr has unparsable value %q", branch, val)
}

// Unpushed counts commits reachable from HEAD that exist on no remote-tracking
// ref and no refs/magnum/* ref, i.e. work a human committed in a slot that a
// reset would orphan. A HEAD sitting on a fetched PR ref counts as zero.
func (c *Client) Unpushed(ctx context.Context, dir string) (int, error) {
	return c.UnpushedRef(ctx, dir, "HEAD")
}

// UnpushedRef is Unpushed for an arbitrary ref (a placeholder branch, a
// detached sha): the commits reachable from ref that exist on no
// remote-tracking ref and no refs/magnum/* ref. Read-only.
func (c *Client) UnpushedRef(ctx context.Context, dir, ref string) (int, error) {
	if err := checkRev("ref", ref); err != nil {
		return 0, err
	}
	res, err := c.git(ctx, dir, call{label: "rev-list unpushed " + ref}, "rev-list", "--count", ref, "--not", "--remotes", "--glob=refs/magnum/*")
	if err != nil {
		return 0, err
	}
	n, convErr := strconv.Atoi(res.Out())
	if convErr != nil || n < 0 {
		return 0, fmt.Errorf("gitx: rev-list --count: unexpected output %q", res.Out())
	}
	return n, nil
}
