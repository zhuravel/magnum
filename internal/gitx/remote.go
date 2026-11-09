package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// RemoteURL returns the URL of origin in dir. Credentials embedded in the URL
// are stripped (see stripUserinfo) so the value is safe to log.
func (c *Client) RemoteURL(ctx context.Context, dir string) (string, error) {
	res, err := c.git(ctx, dir, call{label: "remote get-url " + remote, probe: true}, "remote", "get-url", remote)
	if err != nil {
		return "", err
	}
	return stripUserinfo(res.Out()), nil
}

// NetworkRemote is how a network command (fetch, ls-remote) reaches a clone
// whose origin is originURL. With https set, a github.com SSH origin is reached
// at its HTTPS URL with gh as the only credential helper: opts go before the
// subcommand and target replaces "origin", so a locked or empty ssh-agent does
// not matter. Anything else (https off, an HTTPS origin, an SSH origin on
// another host, an unknown origin) names origin itself with no options.
func NetworkRemote(originURL string, https bool) (opts []string, target string) {
	originURL = strings.TrimSpace(originURL)
	if https && IsSSHURL(originURL) {
		if r, ok := ParseRemote(originURL); ok && r.Host == "github.com" {
			return []string{"-c", "credential.helper=", "-c", "credential.helper=" + GHCredentialHelper},
				GitHubHTTPSURL(r.Owner, r.Repo)
		}
	}
	return nil, remote
}

// LsRemote runs `git ls-remote <origin> patterns...` in dir, reaching origin
// the way fetches do (NetworkRemote with HTTPSFetch), and returns its output.
// A failure is an expected answer (logged at Debug): callers probe with it.
func (c *Client) LsRemote(ctx context.Context, dir string, timeout time.Duration, patterns ...string) (string, error) {
	url := ""
	if c.HTTPSFetch {
		url, _ = c.RemoteURL(ctx, dir)
	}
	opts, target := NetworkRemote(url, c.HTTPSFetch)
	args := append(append(opts, "ls-remote", target), patterns...)
	res, err := c.git(ctx, dir, call{label: "ls-remote " + target, timeout: timeout, probe: true}, args...)
	if err != nil {
		return "", err
	}
	return res.Out(), nil
}

// ErrNotDescendant is PushLease's refusal of a commit that does not
// descend from the remote head it expects: the push would rewrite history.
var ErrNotDescendant = errors.New("gitx: the commit does not descend from the remote head")

// pushTimeout bounds a push.
const pushTimeout = 5 * time.Minute

// PushLease pushes commit sha (a full id) to origin's refs/heads/<branch>
// from dir with a lease: `git push --force-with-lease=refs/heads/<branch>:
// <expect> origin <sha>:refs/heads/<branch>`, reaching origin the way
// fetches do (NetworkRemote with HTTPSFetch). expect is the remote head the
// caller recorded, "" for a new branch, which must not exist on origin then.
// The remote branch moved since (or a branch that exists for an expect of
// "") fails the push and leaves it as it is. A sha that does not descend
// from expect is refused before anything is pushed (ErrNotDescendant), so
// the lease never rewrites history; an expect dir does not have is an
// error too, never a push.
func (c *Client) PushLease(ctx context.Context, dir, sha, branch, expect string) error {
	url := ""
	if c.HTTPSFetch {
		url, _ = c.RemoteURL(ctx, dir)
	}
	opts, target := NetworkRemote(url, c.HTTPSFetch)
	return c.pushLease(ctx, dir, opts, target, sha, branch, expect, nil)
}

// pushLease is PushLease's and PushLeaseVia's core: the checks, then the
// push of sha to target's refs/heads/<branch> with a lease on expect, opts
// before the subcommand, env over the push (PushLeaseViaAs; nil for none).
func (c *Client) pushLease(ctx context.Context, dir string, opts []string, target, sha, branch, expect string, env map[string]string) error {
	if !isOID(sha) {
		return fmt.Errorf("gitx: push: %q is not a full commit id", sha)
	}
	if expect != "" && !isOID(expect) {
		return fmt.Errorf("gitx: push: the expected remote head %q is not a full commit id", expect)
	}
	if err := checkBranch("branch", branch); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	if expect != "" {
		ok, err := c.IsAncestor(ctx, dir, expect, sha)
		if err != nil {
			return fmt.Errorf("gitx: push %s: the remote head %s: %w", branch, expect[:12], err)
		}
		if !ok {
			return fmt.Errorf("gitx: push %s: %s over %s: %w", branch, sha[:12], expect[:12], ErrNotDescendant)
		}
	}
	args := slices.Concat(opts, []string{"push", "--quiet", "--force-with-lease=" + ref + ":" + expect, target, sha + ":" + ref})
	_, err := c.git(ctx, dir, call{label: "push " + branch, mutates: true, timeout: pushTimeout, env: env}, args...)
	return err
}

// GHCredentialHelper is the git credential helper that answers with gh's
// login for github.com (`gh auth git-credential`).
const GHCredentialHelper = "!gh auth git-credential"

// GitHubHTTPSURL is the https clone URL of github.com/<owner>/<repo>.
func GitHubHTTPSURL(owner, repo string) string {
	return "https://github.com/" + owner + "/" + repo + ".git"
}

// GitHubSSHURL is the scp-like SSH clone URL of github.com/<owner>/<repo>.
func GitHubSSHURL(owner, repo string) string {
	return "git@github.com:" + owner + "/" + repo + ".git"
}

// CloneOptions adjust Clone.
type CloneOptions struct {
	// CredentialHelper, when set, is the only credential helper of the clone
	// command and of the new clone: it is passed as `git -c
	// credential.helper= -c credential.helper=<it> clone` (the empty value
	// clears the user's helpers, such as a keychain holding another login)
	// and written into the clone's local config with the same two
	// `clone --config` values, so later fetches use it too.
	CredentialHelper string
}

// Clone clones url into the absolute path dest (parents are created). URLs with
// embedded credentials are refused; authentication comes from the user's git
// credential helper or ssh agent.
func (c *Client) Clone(ctx context.Context, repoURL, dest string) error {
	return c.CloneWith(ctx, repoURL, dest, CloneOptions{})
}

// CloneWith is Clone with options (see CloneOptions).
func (c *Client) CloneWith(ctx context.Context, repoURL, dest string, opt CloneOptions) error {
	if repoURL == "" {
		return errors.New("gitx: clone: empty URL")
	}
	if strings.HasPrefix(repoURL, "-") || strings.ContainsFunc(repoURL, func(r rune) bool { return r <= ' ' }) {
		return errors.New("gitx: clone: malformed URL")
	}
	if stripUserinfo(repoURL) != repoURL {
		return errors.New("gitx: clone: refusing a URL with embedded credentials")
	}
	if err := checkAbsPath(dest); err != nil {
		return err
	}
	var global, local []string
	if h := opt.CredentialHelper; h != "" {
		if strings.ContainsAny(h, "\n\r\x00") {
			return errors.New("gitx: clone: malformed credential helper")
		}
		global = []string{"-c", "credential.helper=", "-c", "credential.helper=" + h}
		local = []string{"--config", "credential.helper=", "--config", "credential.helper=" + h}
	}
	args := slices.Concat(global, []string{"clone", "--quiet"}, local, []string{"--", repoURL, dest})
	cmd := execx.Cmd{
		Name:    "git",
		Args:    args,
		Env:     gitEnv(true),
		Unset:   ScrubbedEnv(),
		Timeout: cloneTimeout,
		Mutates: true,
		Label:   "git clone " + repoURL,
	}
	if _, err := c.run.Run(ctx, cmd); err != nil {
		return fmt.Errorf("gitx: clone %s: %w", repoURL, err)
	}
	return nil
}

// IsSSHURL reports whether a remote URL goes over SSH: an ssh:// (or
// git+ssh://) URL, or git's scp-like [user@]host:path form, which git
// recognises only when no slash comes before the first colon (so a local path
// is never one).
func IsSSHURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if scheme, _, ok := strings.Cut(raw, "://"); ok {
		switch strings.ToLower(scheme) {
		case "ssh", "git+ssh", "ssh+git":
			return true
		}
		return false
	}
	colon := strings.IndexByte(raw, ':')
	return colon > 0 && !strings.Contains(raw[:colon], "/")
}

// OwnerUsesSSH reports whether a clone directly under cloneRoot of any
// github.com/<owner>/* repository has an SSH origin. Directories are read as
// FindClone reads them (".git" required, "__worktrees" skipped, origins
// cached until the clone's config changes), so right after a FindClone miss
// over the same root it costs no subprocess. A missing root is (false, nil).
func (c *Client) OwnerUsesSSH(ctx context.Context, cloneRoot, owner string) (bool, error) {
	if badPathSegment(owner) || cloneRoot == "" {
		return false, fmt.Errorf("gitx: owner transport: invalid owner %q or clone root %q", owner, cloneRoot)
	}
	root := filepath.Clean(cloneRoot)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("gitx: owner transport for %s: %w", owner, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), WorktreesSuffix) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("gitx: owner transport for %s: %w", owner, err)
		}
		url, ok := c.cachedOrigin(ctx, filepath.Join(root, e.Name()))
		if !ok {
			continue
		}
		if r, ok := ParseRemote(url); ok && r.Host == "github.com" && strings.EqualFold(r.Owner, owner) && IsSSHURL(url) {
			return true, nil
		}
	}
	return false, nil
}

// stripUserinfo removes credentials from a URL so it is safe to log and so
// Clone can refuse it. http(s) URLs lose the whole user[:password]@ part (a
// token often sits in the user name); every other scheme (ssh://, git://, ...)
// loses only a password, because its user name (git@) is not a secret and the
// URL must stay cloneable. The text is edited, not re-serialized, so a URL
// that url.Parse rejects (a stray percent sign in the password) is stripped
// the same way: everything up to the last '@' of the authority goes. The
// scp-like git@host:path form and local paths have no authority and are
// returned unchanged.
func stripUserinfo(raw string) string {
	scheme, rest, hasAuthority := strings.Cut(raw, "://")
	if !hasAuthority {
		return raw
	}
	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, tail = rest[:i], rest[i:]
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return raw
	}
	userinfo, host := authority[:at], authority[at+1:]
	switch strings.ToLower(scheme) {
	case "http", "https":
		return scheme + "://" + host + tail
	}
	user, _, hasPassword := strings.Cut(userinfo, ":")
	if !hasPassword {
		return raw
	}
	return scheme + "://" + user + "@" + host + tail
}

// Remote identifies a GitHub repository parsed from a remote URL.
type Remote struct {
	Host  string // lower-cased, e.g. github.com
	Owner string
	Repo  string // without a .git suffix
}

// FullName returns "owner/repo".
func (r Remote) FullName() string { return r.Owner + "/" + r.Repo }

// ParseRemote understands https, http, git and ssh URLs and the scp-like
// git@host:owner/repo.git form. ok is false for local paths and anything that
// does not look like host/owner/repo.
func ParseRemote(raw string) (Remote, bool) {
	raw = strings.TrimSpace(raw)
	var host, path string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return Remote{}, false
		}
		host, path = u.Hostname(), u.Path
	case strings.Contains(raw, "@") && strings.Contains(raw[strings.Index(raw, "@"):], ":"):
		rest := raw[strings.Index(raw, "@")+1:]
		host, path, _ = strings.Cut(rest, ":")
	default:
		return Remote{}, false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	owner, repo, ok := strings.Cut(path, "/")
	if host == "" || !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return Remote{}, false
	}
	return Remote{Host: strings.ToLower(host), Owner: owner, Repo: repo}, true
}

// ghOnlyHelper is the git options that make gh the only credential helper
// of one command (the empty value clears the user's helpers, such as a
// keychain holding the operator's login): with a pane's GH_CONFIG_DIR, gh
// answers with that identity's token.
var ghOnlyHelper = []string{"-c", "credential.helper=", "-c", "credential.helper=" + GHCredentialHelper}

// LsRemoteVia runs `git ls-remote <url> patterns...` in dir with gh as
// git's only credential helper, and returns its output; `magnum push`
// reads a branch's head on GitHub so, whatever form origin has. A failure
// is an expected answer (logged at Debug).
func (c *Client) LsRemoteVia(ctx context.Context, dir, url string, timeout time.Duration, patterns ...string) (string, error) {
	if err := checkURL(url); err != nil {
		return "", err
	}
	args := slices.Concat(ghOnlyHelper, []string{"ls-remote", url}, patterns)
	res, err := c.git(ctx, dir, call{label: "ls-remote " + url, timeout: timeout, probe: true}, args...)
	if err != nil {
		return "", err
	}
	return res.Out(), nil
}

// LsRemoteGitHub is LsRemoteVia of github.com/<owner>/<repo> over HTTPS
// (GitHubHTTPSURL).
func (c *Client) LsRemoteGitHub(ctx context.Context, dir, owner, repo string, timeout time.Duration, patterns ...string) (string, error) {
	url, err := gitHubURL(owner, repo)
	if err != nil {
		return "", err
	}
	return c.LsRemoteVia(ctx, dir, url, timeout, patterns...)
}

// PushLeaseVia is PushLease to url instead of origin, with gh as git's
// only credential helper (`git -c credential.helper= -c
// credential.helper='!gh auth git-credential' push --force-with-lease=
// refs/heads/<branch>:<expect> <url> <sha>:refs/heads/<branch>`), with
// PushLease's guards: never a bare --force or a + refspec, and a sha that
// does not descend from expect refused before anything is pushed
// (ErrNotDescendant). `magnum push` pushes a fixer App's commits so, as
// the App, whatever form origin has and whichever helper the operator's
// config names.
func (c *Client) PushLeaseVia(ctx context.Context, dir, url, sha, branch, expect string) error {
	return c.PushLeaseViaAs(ctx, dir, url, sha, branch, expect, nil)
}

// PushLeaseViaAs is PushLeaseVia with env over the push, never over a key
// of gitx's own: the daemon's push as a fixer App, whose GH_CONFIG_DIR
// (with GH_TOKEN and GITHUB_TOKEN blanked, identity.Source.Env) holds the
// token gh answers git with, or whose GH_TOKEN is the daemon's own push
// token (identity.App.DaemonPushEnv), which gh answers with first. No
// value of env is ever logged.
func (c *Client) PushLeaseViaAs(ctx context.Context, dir, url, sha, branch, expect string, env map[string]string) error {
	if err := checkURL(url); err != nil {
		return err
	}
	return c.pushLease(ctx, dir, ghOnlyHelper, url, sha, branch, expect, env)
}

// PushLeaseGitHubAs is PushLeaseViaAs to github.com/<owner>/<repo> over
// HTTPS (GitHubHTTPSURL).
func (c *Client) PushLeaseGitHubAs(ctx context.Context, dir, owner, repo, sha, branch, expect string, env map[string]string) error {
	url, err := gitHubURL(owner, repo)
	if err != nil {
		return err
	}
	return c.PushLeaseViaAs(ctx, dir, url, sha, branch, expect, env)
}

// PushLeaseGitHub is PushLeaseVia to github.com/<owner>/<repo> over HTTPS
// (GitHubHTTPSURL).
func (c *Client) PushLeaseGitHub(ctx context.Context, dir, owner, repo, sha, branch, expect string) error {
	url, err := gitHubURL(owner, repo)
	if err != nil {
		return err
	}
	return c.PushLeaseVia(ctx, dir, url, sha, branch, expect)
}

// gitHubURL is GitHubHTTPSURL of owner and repo, each one path segment.
func gitHubURL(owner, repo string) (string, error) {
	if badPathSegment(owner) || badPathSegment(repo) {
		return "", fmt.Errorf("gitx: %q/%q names no GitHub repository", owner, repo)
	}
	return GitHubHTTPSURL(owner, repo), nil
}

// checkURL refuses a URL git could read as an option, one with spaces or
// control characters, and one with embedded credentials.
func checkURL(url string) error {
	if url == "" || strings.HasPrefix(url, "-") || strings.ContainsFunc(url, func(r rune) bool { return r <= ' ' }) {
		return fmt.Errorf("gitx: malformed remote URL %q", url)
	}
	if stripUserinfo(url) != url {
		return errors.New("gitx: refusing a URL with embedded credentials")
	}
	return nil
}
