package staging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
// keychain login of the operator's answers, and no gc or maintenance is
// left running after the command in the background.
var gitOptions = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "credential.helper=", "-c", "credential.helper=" + gitx.GHCredentialHelper,
	"-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false",
}

// missingRef is how git fetch names a refspec source the remote lacks.
var missingRef = regexp.MustCompile(`couldn't find remote ref (\S+)`)

// execGit is the real Git: git through an execx.Runner, always in the
// mirror's directory.
type execGit struct {
	run execx.Runner
	url func(repo string) string
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
func newGit(r execx.Runner, url func(repo string) string) Git { return &execGit{run: r, url: url} }

// cmd is `git -C dir <gitOptions> args...` with env over the network
// command's environment; offline ones may not fetch a missing object
// (GIT_NO_LAZY_FETCH), and none prompts or takes optional locks.
func (g *execGit) cmd(dir, label string, timeout time.Duration, mutates, network bool, env map[string]string, args ...string) execx.Cmd {
	e := maps.Clone(env)
	if e == nil {
		e = map[string]string{}
	}
	e["GIT_TERMINAL_PROMPT"] = "0"
	if !mutates {
		e["GIT_OPTIONAL_LOCKS"] = "0"
	}
	if !network {
		e["GIT_NO_LAZY_FETCH"] = "1"
	}
	return execx.Cmd{
		Name:    "git",
		Args:    slices.Concat([]string{"-C", dir}, gitOptions, args),
		Dir:     dir,
		Env:     e,
		Unset:   gitx.ScrubbedEnv(),
		Timeout: timeout,
		Mutates: mutates,
		Label:   "git staging " + label,
	}
}

// isMirror reports whether dir is a bare repository: HEAD and objects/.
func isMirror(dir string) bool {
	head, err := os.Stat(filepath.Join(dir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	objects, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && objects.IsDir()
}

func (g *execGit) Ensure(ctx context.Context, dir, repo string, env map[string]string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("mirror dir %q is not absolute", dir)
	}
	if !isMirror(dir) {
		// Clone beside the mirror and move it in, so a cut clone never
		// leaves a half mirror where the next Refresh looks.
		tmp := dir + ".clone"
		if err := os.RemoveAll(tmp); err != nil {
			return "", err
		}
		url := g.url(repo)
		if strings.HasPrefix(url, "-") {
			return "", fmt.Errorf("malformed URL %q", url)
		}
		parent := filepath.Dir(dir)
		cmd := g.cmd(parent, "clone "+repo, cloneTimeout, true, true, env,
			"clone", "--quiet", "--bare", "--filter=tree:0", "--single-branch", "--no-tags", "--", url, tmp)
		if _, err := g.run.Run(ctx, cmd); err != nil {
			_ = os.RemoveAll(tmp)
			return "", fmt.Errorf("clone: %w", err)
		}
		if err := os.Rename(tmp, dir); err != nil {
			_ = os.RemoveAll(tmp)
			return "", fmt.Errorf("%s exists and is not a mirror: %w", dir, err)
		}
	}
	res, err := g.run.Run(ctx, g.cmd(dir, "head", readTimeout, false, false, nil, "symbolic-ref", "--short", "HEAD"))
	if err != nil {
		return "", fmt.Errorf("the mirror's HEAD: %w", err)
	}
	base := strings.TrimSpace(res.Out())
	if base == "" || strings.HasPrefix(base, "-") {
		return "", fmt.Errorf("the mirror's HEAD names no branch: %q", base)
	}
	return base, nil
}

func (g *execGit) Fetch(ctx context.Context, dir string, env map[string]string, refspecs []string) error {
	for _, spec := range refspecs {
		if !strings.HasPrefix(spec, "+refs/") {
			return fmt.Errorf("refspec %q is not a forced refs/ refspec", spec)
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

// checkRevs refuses a revision git could read as an option.
func checkRevs(revs ...string) error {
	for _, r := range revs {
		if r == "" || strings.HasPrefix(r, "-") || strings.ContainsFunc(r, func(c rune) bool { return c <= ' ' }) {
			return fmt.Errorf("revision %q is not one", r)
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
