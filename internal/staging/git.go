package staging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
)

// The timeouts of the mirror's git commands.
const (
	cloneTimeout = 30 * time.Minute
	fetchTimeout = 5 * time.Minute
	readTimeout  = 2 * time.Minute
	pathsTimeout = 3 * time.Minute
)

// gitOptions go before every subcommand: no hook of the mirror's runs, gh
// (with the env's GH_CONFIG_DIR) is the only credential helper, so no
// keychain login of the operator's answers, never ssh (the operator's key),
// and no gc or maintenance is left running after the command in the
// background.
var gitOptions = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "credential.helper=", "-c", "credential.helper=" + ghHelper,
	"-c", "protocol.ssh.allow=never",
	"-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false",
}

// ghHelper is gh as git's credential helper, run with the operator's real
// HOME (realHomeVar): git itself runs with an empty one (gitHome), but gh
// finds the identity's login through GH_CONFIG_DIR, or for a gh identity
// through that HOME and its keychain.
const ghHelper = `!HOME="$` + realHomeVar + `" gh auth git-credential`

// realHomeVar carries the operator's HOME to ghHelper.
const realHomeVar = "MAGNUM_STAGING_HOME"

// missingRef is how git fetch names a refspec source the remote lacks.
var missingRef = regexp.MustCompile(`couldn't find remote ref (\S+)`)

// execGit is the real Git: git through an execx.Runner, always in the
// mirror's directory.
type execGit struct {
	run execx.Runner
	url func(repo string) string
	// home is the operator's HOME, for gh alone (ghHelper).
	home string
}

// NewGit returns the real Git: a mirror is a treeless bare clone of
// https://github.com/<owner>/<repo>.git.
func NewGit(r execx.Runner) Git {
	return newGit(r, func(repo string) string {
		owner, name, _ := strings.Cut(repo, "/")
		return gitx.GitHubHTTPSURL(owner, name)
	})
}

// newGit is NewGit with the remote's URL of a repository (tests clone a
// local file:// origin).
func newGit(r execx.Runner, url func(repo string) string) Git {
	home, _ := os.UserHomeDir()
	return &execGit{run: r, url: url, home: home}
}

// gitHome is the HOME git runs with: an empty directory in mirror_dir
// (beside the owners' directories), so no ~/.netrc, ~/.gitconfig or
// includeIf of the operator's ever applies. root is mirror_dir.
func gitHome(root string) string { return filepath.Join(root, ".home") }

// configVars are the variables of environ that give git config (
// GIT_CONFIG_COUNT with its GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n>, and
// GIT_CONFIG_PARAMETERS, which -c writes): an insteadOf or a URL-scoped
// credential helper there would reach the fetch.
func configVars(environ []string) []string {
	out := []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			out = append(out, k)
		}
	}
	return out
}

// cmd is `git --git-dir=<dir> <gitOptions> args...` in dir, with GIT_DIR
// set to dir too and GIT_CEILING_DIRECTORIES at its parent: git never
// looks for a repository, so it can never walk up into one around the
// mirror. dir is <mirror_dir>/<owner>/<name>.git.
func (g *execGit) cmd(dir, label string, timeout time.Duration, mutates, network bool, env map[string]string, args ...string) execx.Cmd {
	c := g.base(dir, filepath.Dir(filepath.Dir(dir)), label, timeout, mutates, network, env)
	c.Args = slices.Concat([]string{"--git-dir=" + dir}, gitOptions, args)
	c.Env["GIT_DIR"] = dir
	return c
}

// base is a git command run in cwd with the mirror's environment and no
// repository named (the clone's): GIT_CEILING_DIRECTORIES stops git's
// search for one at cwd's parent. Of env (the identity's) only what is no
// git config and no HOME is kept: GH_CONFIG_DIR and the like, gh's login.
// Git reads no global or system config and no config from the environment,
// runs with an empty HOME in mirror_dir root (no ~/.netrc), and never
// prompts or runs an askpass; offline commands may not fetch a missing
// object (GIT_NO_LAZY_FETCH), and none takes optional locks.
func (g *execGit) base(cwd, root, label string, timeout time.Duration, mutates, network bool, env map[string]string) execx.Cmd {
	e := map[string]string{}
	for k, v := range env {
		if !strings.HasPrefix(k, "GIT_CONFIG") && k != "HOME" && k != realHomeVar {
			e[k] = v
		}
	}
	home := gitHome(root)
	// Git also runs without it: then there is no ~/.netrc either.
	_ = os.MkdirAll(home, 0o700)
	maps.Copy(e, map[string]string{ //nolint:gosec // G101: the names of git's askpass variables, no credential
		"HOME": home, realHomeVar: g.home,
		"GIT_TERMINAL_PROMPT": "0", "GIT_ASKPASS": "/usr/bin/false", "SSH_ASKPASS": "/usr/bin/false",
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CEILING_DIRECTORIES": filepath.Dir(cwd),
	})
	if !mutates {
		e["GIT_OPTIONAL_LOCKS"] = "0"
	}
	if !network {
		e["GIT_NO_LAZY_FETCH"] = "1"
	}
	return execx.Cmd{
		Name:    "git",
		Dir:     cwd,
		Env:     e,
		Unset:   slices.Concat(gitx.ScrubbedEnv(), configVars(os.Environ())),
		Timeout: timeout,
		Mutates: mutates,
		Label:   "git staging " + label,
	}
}

// laidOut reports whether dir looks like a bare repository on disk: a
// regular HEAD, objects/ and refs/ (without refs/, git takes it for no
// repository and looks above it).
func laidOut(dir string) bool {
	head, err := os.Lstat(filepath.Join(dir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	for _, sub := range []string{"objects", "refs"} {
		if fi, err := os.Lstat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// noSuchKey reports whether err is `git config --get`'s definite answer that
// the key is not set: exit 1.
func noSuchKey(err error) bool {
	ee, ok := errors.AsType[*execx.ExitError](err)
	return ok && ee.Code == 1
}

// notARepository reports whether err is git's definite answer that the dir
// it was named is no repository: exit 128, "not a git repository".
func notARepository(err error) bool {
	ee, ok := errors.AsType[*execx.ExitError](err)
	return ok && ee.Code == 128 && strings.Contains(ee.Stderr, "not a git repository")
}

// Base answers ErrNoMirror only on a definite answer, and never once ctx is
// done: the layout check fails, git exits 128 with "not a git repository",
// git calls the dir not bare, or its origin is not repo's (or it has none:
// a dir of someone's bare clones, a repository renamed), so Refresh moves
// it aside and clones again, never fetching into it. Anything else (a git
// that cannot run, a signal, a timeout, a cancel) is a plain error that
// leaves the dir alone, and so is a mirror of repo that is not treeless.
func (g *execGit) Base(ctx context.Context, dir, repo string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("mirror dir %q is not absolute", dir)
	}
	if !laidOut(dir) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s is not laid out as a bare repository (HEAD, objects/, refs/): %w", dir, ErrNoMirror)
	}
	read := func(label string, args ...string) (string, error) {
		res, err := g.run.Run(ctx, g.cmd(dir, label, readTimeout, false, false, nil, args...))
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		return strings.TrimSpace(res.Out()), err
	}
	bare, err := read("is bare", "rev-parse", "--is-bare-repository")
	switch {
	case notARepository(err):
		return "", fmt.Errorf("%s is no repository to git: %w", dir, ErrNoMirror)
	case err != nil:
		return "", fmt.Errorf("checking %s: %w", dir, err)
	case bare != "true":
		return "", fmt.Errorf("%s is not a bare repository to git (bare: %q): %w", dir, bare, ErrNoMirror)
	}
	url, err := read("origin", "config", "--get", "remote.origin.url")
	switch {
	case ctx.Err() != nil:
		return "", err
	case err != nil && !noSuchKey(err):
		return "", fmt.Errorf("reading the origin of %s: %w", dir, err)
	case err != nil || !strings.EqualFold(url, g.url(repo)):
		return "", fmt.Errorf("%s is no mirror of %s: its origin is another repository's, or none: %w", dir, repo, ErrNoMirror)
	}
	if filter, err := read("filter", "config", "--get", "remote.origin.partialclonefilter"); err != nil || filter != "tree:0" {
		if ctx.Err() != nil {
			return "", err
		}
		return "", fmt.Errorf("%s is no mirror of %s: it is not treeless (tree:0)", dir, repo)
	}
	base, err := read("head", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("the mirror's HEAD: %w", err)
	}
	if !validBranch(base) {
		return "", fmt.Errorf("the mirror's HEAD names no branch: %q", base)
	}
	return base, nil
}

func (g *execGit) Clone(ctx context.Context, dir, repo string, env map[string]string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("mirror dir %q is not absolute", dir)
	}
	switch _, err := os.Lstat(dir); {
	case err == nil:
		return fmt.Errorf("%s exists: a clone needs it absent", dir)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s: %w", dir, err)
	}
	url := g.url(repo)
	if strings.HasPrefix(url, "-") || strings.ContainsFunc(url, func(r rune) bool { return r <= ' ' }) {
		return fmt.Errorf("malformed URL %q", url)
	}
	// Clone beside the mirror and move it in, so a cut clone never leaves a
	// half mirror where the next Refresh looks.
	tmp := dir + ".clone"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	parent := filepath.Dir(dir)
	cmd := g.base(parent, filepath.Dir(parent), "clone "+repo, cloneTimeout, true, true, env)
	cmd.Args = slices.Concat([]string{"-C", parent}, gitOptions,
		[]string{"clone", "--quiet", "--bare", "--filter=tree:0", "--single-branch", "--no-tags", "--", url, tmp})
	if _, err := g.run.Run(ctx, cmd); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("clone: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("moving the clone into %s: %w", dir, err)
	}
	return nil
}

// specRe is a refspec a fetch may carry: a forced one that writes a ref of
// the same name.
var specRe = regexp.MustCompile(`^\+(refs/[^:]+):(refs/[^:]+)$`)

// validSpec reports whether spec is one of the mirror's refspecs: the
// staging glob, or a branch or pull ref fetched into itself.
func validSpec(spec string) bool {
	if spec == "+refs/heads/staging-*:refs/heads/staging-*" {
		return true
	}
	m := specRe.FindStringSubmatch(spec)
	return m != nil && m[1] == m[2] && validRef(m[1])
}

func (g *execGit) Fetch(ctx context.Context, dir string, env map[string]string, refspecs []string) error {
	for _, spec := range refspecs {
		if !validSpec(spec) {
			return fmt.Errorf("refspec %q is not one of the mirror's", spec)
		}
	}
	args := slices.Concat([]string{"fetch", "--quiet", "--filter=tree:0", "--no-tags", "--prune", "--no-write-fetch-head", "origin"}, refspecs)
	_, err := g.run.Run(ctx, g.cmd(dir, "fetch", fetchTimeout, true, true, env, args...))
	if ee, ok := errors.AsType[*execx.ExitError](err); ok {
		if m := missingRef.FindStringSubmatch(ee.Stderr); m != nil {
			return &MissingRefError{Ref: m[1]}
		}
	}
	return err
}

func (g *execGit) Refs(ctx context.Context, dir string, prefixes ...string) (map[string]string, error) {
	args := []string{"for-each-ref", "--format=%(objectname) %(refname)"}
	for _, p := range prefixes {
		if !strings.HasPrefix(p, "refs/") {
			return nil, fmt.Errorf("ref prefix %q is not under refs/", p)
		}
		args = append(args, p)
	}
	res, err := g.run.Run(ctx, g.cmd(dir, "refs", readTimeout, false, false, nil, args...))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for line := range strings.Lines(res.Out()) {
		if sha, name, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			out[name] = sha
		}
	}
	return out, nil
}

// pullRefRe is a PR's pull ref.
var pullRefRe = regexp.MustCompile(`^refs/pull/[0-9]+/head$`)

// isOID reports whether s is a full commit id: 40 or 64 lower-case hex
// digits.
func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') })
}

// validRef reports whether ref is one of the mirror's own refs: a branch
// (refs/heads/<validBranch>) or a pull ref.
func validRef(ref string) bool {
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return validBranch(b)
	}
	return pullRefRe.MatchString(ref)
}

// checkRevs refuses a revision that is neither a full sha nor one of the
// mirror's own refs: no option, range, exclusion, reflog entry or message
// search ever reaches git, whatever GitHub answered.
func checkRevs(revs ...string) error {
	for _, r := range revs {
		if !isOID(r) && !validRef(r) {
			return fmt.Errorf("revision %q is no sha and none of the mirror's refs", r)
		}
	}
	return nil
}

func (g *execGit) FirstParent(ctx context.Context, dir, rev string, limit int) ([]Commit, error) {
	if err := checkRevs(rev); err != nil {
		return nil, err
	}
	res, err := g.run.Run(ctx, g.cmd(dir, "first-parent", readTimeout, false, false, nil,
		"rev-list", "--first-parent", "--max-count="+strconv.Itoa(limit), "--no-commit-header", "--format=%ct %H %P", "--end-of-options", rev))
	if err != nil {
		return nil, err
	}
	var out []Commit
	for line := range strings.Lines(res.Out()) {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		ct, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("rev-list line %q: %w", line, err)
		}
		out = append(out, Commit{SHA: f[1], Parents: f[2:], At: time.Unix(ct, 0)})
	}
	return out, nil
}

func (g *execGit) Graph(ctx context.Context, dir string, include, exclude []string) ([]Commit, error) {
	if err := checkRevs(slices.Concat(include, exclude)...); err != nil {
		return nil, err
	}
	// The revisions go on stdin: an open PR count or a window does not
	// bound argv.
	var stdin bytes.Buffer
	for _, r := range include {
		stdin.WriteString(r + "\n")
	}
	for _, r := range exclude {
		stdin.WriteString("^" + r + "\n")
	}
	cmd := g.cmd(dir, "graph", readTimeout, false, false, nil, "rev-list", "--parents", "--topo-order", "--stdin")
	cmd.Stdin = stdin.Bytes()
	res, err := g.run.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, errors.New("rev-list's output was cut: the graph is too large")
	}
	var out []Commit
	for line := range strings.Lines(res.Out()) {
		if f := strings.Fields(line); len(f) > 0 {
			out = append(out, Commit{SHA: f[0], Parents: f[1:]})
		}
	}
	return out, nil
}

func (g *execGit) ChangedPaths(ctx context.Context, dir string, env map[string]string, base, head string) ([]string, error) {
	if err := checkRevs(base, head); err != nil {
		return nil, err
	}
	res, err := g.run.Run(ctx, g.cmd(dir, "changed paths", pathsTimeout, false, true, env,
		"diff", "--name-only", "--no-renames", "-z", "--end-of-options", base+"..."+head))
	if err != nil {
		return nil, err
	}
	var out []string
	for p := range strings.SplitSeq(string(res.Stdout), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}
