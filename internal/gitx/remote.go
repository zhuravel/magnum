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
