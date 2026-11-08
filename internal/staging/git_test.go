package staging

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// Every git command of the mirror names it with --git-dir and GIT_DIR, never
// -C alone, so git's discovery never walks up to an enclosing repository;
// GIT_CEILING_DIRECTORIES stops it there too, and the operator's global
// and system git config never apply (only the identity's env answers).
func TestEveryMirrorCommandNamesTheMirrorAndIsolatesGitConfig(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "acme", "app.git")
	for _, sub := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/master\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"git", "--git-dir=" + dir}, Fn: func(c execx.Cmd) (execx.Result, error) {
			switch {
			case slices.Contains(c.Args, "--is-bare-repository"):
				return execx.Result{Stdout: []byte("true\n")}, nil
			case slices.Contains(c.Args, "symbolic-ref"):
				return execx.Result{Stdout: []byte("master\n")}, nil
			case slices.Contains(c.Args, "remote.origin.url"):
				return execx.Result{Stdout: []byte("https://github.com/acme/app.git\n")}, nil
			case slices.Contains(c.Args, "remote.origin.partialclonefilter"):
				return execx.Result{Stdout: []byte("tree:0\n")}, nil
			}
			return execx.Result{}, nil
		}},
	}}
	g := newGit(f, func(string) string { return "https://github.com/acme/app.git" })
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	if base, err := g.Base(ctx, dir, "acme/app"); err != nil || base != "master" {
		t.Fatalf("Base = %q, %v", base, err)
	}
	idEnv := map[string]string{"GH_CONFIG_DIR": "/id", "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "url.x.insteadOf", "GIT_CONFIG_VALUE_0": "https://github.com/"}
	if err := g.Fetch(ctx, dir, idEnv, []string{"+refs/heads/staging-*:refs/heads/staging-*", "+refs/pull/7/head:refs/pull/7/head"}); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() error{
		func() error { _, err := g.Refs(ctx, dir, "refs/heads/"); return err },
		func() error { _, err := g.FirstParent(ctx, dir, sha, 401); return err },
		func() error { _, err := g.Graph(ctx, dir, []string{sha}, []string{"refs/heads/master"}); return err },
		func() error {
			_, err := g.ChangedPaths(ctx, dir, map[string]string{}, "refs/heads/master", sha)
			return err
		},
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.Calls) < 9 {
		t.Fatalf("%d git calls, want every method's", len(f.Calls))
	}
	for _, c := range f.Calls {
		if slices.Contains(c.Args, "-C") {
			t.Errorf("%q runs with -C", c.Args)
		}
		if c.Args[0] != "--git-dir="+dir || c.Env["GIT_DIR"] != dir || c.Env["GIT_CEILING_DIRECTORIES"] != filepath.Dir(dir) {
			t.Errorf("%q: GIT_DIR %q, ceiling %q; want the mirror named and the ceiling at its parent", c.Args, c.Env["GIT_DIR"], c.Env["GIT_CEILING_DIRECTORIES"])
		}
		if c.Env["GIT_CONFIG_GLOBAL"] != "/dev/null" || c.Env["GIT_CONFIG_NOSYSTEM"] != "1" {
			t.Errorf("%q reads the operator's git config: %v", c.Args, c.Env)
		}
		home := filepath.Join(filepath.Dir(filepath.Dir(dir)), ".home")
		if c.Env["HOME"] != home || c.Env["GIT_ASKPASS"] != "/usr/bin/false" || c.Env["SSH_ASKPASS"] != "/usr/bin/false" || c.Env["GIT_TERMINAL_PROMPT"] != "0" {
			t.Errorf("%q: HOME %q, askpass %q, prompt %q; want an empty HOME under mirror_dir (no ~/.netrc), no askpass, no prompt",
				c.Args, c.Env["HOME"], c.Env["GIT_ASKPASS"], c.Env["GIT_TERMINAL_PROMPT"])
		}
		for _, v := range []string{"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT"} {
			if !slices.Contains(c.Unset, v) {
				t.Errorf("%q inherits %s", c.Args, v)
			}
		}
		for k := range c.Env {
			if strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") || k == "GIT_CONFIG_COUNT" {
				t.Errorf("%q carries %s: config from the env", c.Args, k)
			}
		}
	}
	fetch := f.Calls[4]
	if fetch.Env["GH_CONFIG_DIR"] != "/id" || !slices.Contains(fetch.Args, "credential.helper=") || !slices.Contains(fetch.Args, "fetch") {
		t.Errorf("the fetch: env %v, args %q; want the identity's env and the helpers reset", fetch.Env, fetch.Args)
	}
	if h := fetch.Env["MAGNUM_STAGING_HOME"]; h == "" || !slices.ContainsFunc(fetch.Args, func(a string) bool { return strings.Contains(a, `HOME="$MAGNUM_STAGING_HOME" gh auth git-credential`) }) {
		t.Errorf("the fetch: gh is not given the real HOME (%q): %q", h, fetch.Args)
	}
}

// Only a sha or one of the mirror's own refs reaches git as a revision:
// ranges, exclusions, reflog entries and message searches are refused
// before any call.
func TestOnlyShasAndTheMirrorsRefsReachGit(t *testing.T) {
	t.Parallel()
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}, Result: execx.Result{}}}}
	g := newGit(f, nil)
	ctx := context.Background()
	dir := "/m/acme/app.git"
	for _, rev := range []string{"HEAD~1", "^" + strings.Repeat("a", 40), "a..b", ":/fix", "master@{1}", "refs/heads/a..b",
		"refs/remotes/origin/x", strings.Repeat("A", 40), strings.Repeat("a", 39), "staging-void", "-x"} {
		if _, err := g.FirstParent(ctx, dir, rev, 1); err == nil {
			t.Errorf("FirstParent(%q): no refusal", rev)
		}
		if _, err := g.Graph(ctx, dir, []string{rev}, nil); err == nil {
			t.Errorf("Graph(include %q): no refusal", rev)
		}
		if _, err := g.Graph(ctx, dir, []string{strings.Repeat("a", 40)}, []string{rev}); err == nil {
			t.Errorf("Graph(exclude %q): no refusal", rev)
		}
	}
	for _, spec := range []string{"+refs/heads/a..b:refs/heads/a..b", "+refs/pull/x/head:refs/pull/x/head", "+refs/heads/a:refs/heads/b", "refs/heads/a:refs/heads/a"} {
		if err := g.Fetch(ctx, dir, nil, []string{spec}); err == nil {
			t.Errorf("Fetch(%q): no refusal", spec)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("%d git calls, want none: %q", len(f.Calls), f.Calls[0].Args)
	}
	for _, rev := range []string{strings.Repeat("a", 40), strings.Repeat("b", 64), "refs/heads/feat/a-1", "refs/pull/12/head"} {
		if _, err := g.Graph(ctx, dir, []string{rev}, nil); err != nil {
			t.Errorf("Graph(%q): %v", rev, err)
		}
	}
}

// A dir with HEAD and objects/ but no refs/ is no repository to git, which
// then walks up to the repository around it. Base calls it no mirror, and
// never runs a git command that discovery could carry outside it.
func TestADirWithoutRefsIsNoMirror(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "app.git")
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/master\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &execx.Fake{}
	if _, err := newGit(f, nil).Base(context.Background(), dir, "acme/app"); !errors.Is(err, ErrNoMirror) {
		t.Errorf("Base of a dir without refs/: %v, want ErrNoMirror", err)
	}
	if _, err := newGit(f, nil).Base(context.Background(), filepath.Join(t.TempDir(), "absent.git"), "acme/app"); !errors.Is(err, ErrNoMirror) {
		t.Errorf("Base of an absent dir: %v, want ErrNoMirror", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("%d git calls, want none", len(f.Calls))
	}
}

// The config git takes from the environment (GIT_CONFIG_COUNT and its
// numbered pairs, GIT_CONFIG_PARAMETERS) is never inherited.
func TestConfigFromTheEnvironmentIsScrubbed(t *testing.T) {
	t.Parallel()
	got := configVars([]string{"PATH=/bin", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=a", "GIT_CONFIG_VALUE_0=b", "GIT_CONFIG_KEY_17=c",
		"GIT_CONFIG_VALUE_17=d", "GIT_CONFIG_PARAMETERS='x'", "GIT_CONFIG_GLOBAL=/x", "HOME=/h"})
	for _, want := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_17", "GIT_CONFIG_VALUE_17", "GIT_CONFIG_PARAMETERS"} {
		if !slices.Contains(got, want) {
			t.Errorf("configVars lacks %s: %q", want, got)
		}
	}
	if slices.Contains(got, "PATH") || slices.Contains(got, "HOME") {
		t.Errorf("configVars takes more than git's config: %q", got)
	}
}
