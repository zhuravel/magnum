package staging

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// hermetic is an execx runner whose git reads no user or system config and
// commits as a fixed identity.
func hermetic(t *testing.T) *execx.Real {
	t.Helper()
	return &execx.Real{BaseEnv: []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"LC_ALL=C",
	}}
}

// gitIn runs git in dir with r and fails the test on an error.
func gitIn(t *testing.T, r *execx.Real, dir string, args ...string) string {
	t.Helper()
	res, err := r.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)})
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(res.Out())
}

// writeIn writes text to name under dir.
func writeIn(t *testing.T, dir, name, text string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newOrigin is a tiny origin in a temp dir: master at c1, a PR branch pr1
// at p1 (refs/pull/1/head), and staging-void with pr1 merged in by a
// two-parent merge; its HEAD is master.
func newOrigin(t *testing.T, r *execx.Real) (origin, c1, p1, merge string) {
	t.Helper()
	origin = t.TempDir()
	git := func(args ...string) string { t.Helper(); return gitIn(t, r, origin, args...) }
	git("init", "-q", "-b", "master")
	git("config", "uploadpack.allowFilter", "true")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	writeIn(t, origin, "app/a.rb", "a\n")
	git("add", ".")
	git("commit", "-qm", "c1")
	c1 = git("rev-parse", "HEAD")
	git("branch", "staging-void")
	git("checkout", "-qb", "pr1")
	writeIn(t, origin, "app/b.rb", "b\n")
	git("add", ".")
	git("commit", "-qm", "p1")
	p1 = git("rev-parse", "HEAD")
	git("update-ref", "refs/pull/1/head", p1)
	git("checkout", "-q", "staging-void")
	git("merge", "-q", "--no-ff", "-m", "Merge branch 'pr1' into staging-void", "pr1")
	merge = git("rev-parse", "HEAD")
	git("checkout", "-q", "master")
	return origin, c1, p1, merge
}

// refreshCloned refreshes once, waits for the first clone it starts in the
// background, and refreshes again, failing the test on an error.
func refreshCloned(t *testing.T, c *Client, open ...OpenPR) {
	t.Helper()
	if err := c.Refresh(context.Background(), "acme/app", open); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	c.bg.Wait()
	if err := c.Refresh(context.Background(), "acme/app", open); err != nil {
		t.Fatalf("Refresh after the clone: %v", err)
	}
}

// A tiny origin in a temp dir: master, a PR branch and staging-void with
// the PR merged in by a two-parent merge. The mirror is a treeless bare
// clone of it, and the PR is latest once a deployment holds the merge,
// pushed with CI failed on its own path before.
func TestARealTreelessMirrorTellsLatestAndPushed(t *testing.T) {
	t.Parallel()
	r := hermetic(t)
	origin, c1, p1, merge := newOrigin(t, r)

	gh := newFakeGitHub()
	gh.deploys["void"] = []Deployment{{ID: 1, SHA: merge, Ref: merge, CreatedAt: time.Now(), Success: true}}
	now := time.Now()
	mirror := filepath.Join(t.TempDir(), "acme", "app.git")
	c := New(Deps{
		Git:    newGit(r, func(repo string) string { return "file://" + origin }),
		GitHub: gh,
		Mirror: func(string) (string, bool) { return mirror, true },
		GitEnv: func(string) map[string]string { return map[string]string{} },
		Now:    func() time.Time { return now },
	})
	pr := PR{Repo: "acme/app", Number: 1, Head: p1, Base: "master"}
	refreshCloned(t, c, OpenPR{Number: 1, Base: "master"})
	if got := one(c, pr); got.State != StateLatest || got.InTip != p1 || got.Tip != merge {
		t.Fatalf("deployed at the merge: %+v, want latest at p1 with the merge as the tip", got)
	}
	res, err := r.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"-C", mirror, "config", "remote.origin.partialclonefilter"}})
	if err != nil || strings.TrimSpace(res.Out()) != "tree:0" {
		t.Errorf("the mirror's filter: %q (%v), want tree:0", res.Out(), err)
	}

	// The deployment is of the tip before the merge, and ci.yml failed on
	// the PR's own file: its changed paths come from the treeless mirror.
	gh.deploys["void"] = []Deployment{{ID: 2, SHA: c1, Ref: c1, CreatedAt: time.Now(), Success: true}}
	gh.runs[merge] = []Run{{ID: 5, URL: "https://github.com/acme/app/actions/runs/5", Status: "completed", Conclusion: "failure",
		Failed: []Job{{Name: "rubocop", Paths: []string{"app/b.rb"}}}}}
	now = now.Add(RefreshEvery)
	if err := c.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 1, Base: "master"}}); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if got := one(c, pr); got.State != StatePushed || got.Detail != DetailCIFailedOurs || got.DeployedTip != c1 {
		t.Errorf("deployed before the merge, CI failed on app/b.rb: %+v, want pushed, ci_failed_ours", got)
	}
}

// The reviewer's case: a mirror dir with HEAD and objects/ but no refs/,
// inside a repository of the operator's that has the same origin. To git,
// no repository, so -C would walk up to the enclosing one and the forced
// fetch would rewrite its refs. The dir is moved aside, never repaired in
// place, the mirror is cloned again, and the enclosing repository's refs
// and config stay as they were.
func TestAMirrorDirWithoutRefsInsideARepositoryIsMovedAsideAndClonedAgain(t *testing.T) {
	t.Parallel()
	r := hermetic(t)
	origin, _, p1, merge := newOrigin(t, r)
	outer := t.TempDir()
	gitIn(t, r, outer, "init", "-q", "-b", "master")
	writeIn(t, outer, "README", "outer\n")
	gitIn(t, r, outer, "add", ".")
	gitIn(t, r, outer, "commit", "-qm", "outer")
	gitIn(t, r, outer, "remote", "add", "origin", "file://"+origin)
	refsBefore := gitIn(t, r, outer, "for-each-ref")
	configBefore, err := os.ReadFile(filepath.Join(outer, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(outer, "state", "staging", "acme", "app.git")
	if err := os.MkdirAll(filepath.Join(mirror, "objects"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeIn(t, mirror, "HEAD", "ref: refs/heads/master\n")

	gh := newFakeGitHub()
	gh.deploys["void"] = []Deployment{{ID: 1, SHA: merge, Ref: merge, CreatedAt: time.Now(), Success: true}}
	now := time.Now()
	c := New(Deps{
		Git:    newGit(r, func(string) string { return "file://" + origin }),
		GitHub: gh,
		Mirror: func(string) (string, bool) { return mirror, true },
		GitEnv: func(string) map[string]string { return map[string]string{} },
		Now:    func() time.Time { return now },
	})
	refreshCloned(t, c, OpenPR{Number: 1, Base: "master"})
	if got := one(c, PR{Repo: "acme/app", Number: 1, Head: p1, Base: "master"}); got.State != StateLatest {
		t.Errorf("on the new mirror: %+v, want latest", got)
	}
	if after := gitIn(t, r, outer, "for-each-ref"); after != refsBefore {
		t.Errorf("the enclosing repository's refs changed:\n%s\nwant\n%s", after, refsBefore)
	}
	if after, _ := os.ReadFile(filepath.Join(outer, ".git", "config")); string(after) != string(configBefore) {
		t.Errorf("the enclosing repository's config changed:\n%s", after)
	}
	aside, _ := filepath.Glob(mirror + ".broken-*")
	if len(aside) != 1 {
		t.Fatalf("moved aside: %q, want one dir", aside)
	}
	if _, err := os.Stat(filepath.Join(aside[0], "HEAD")); err != nil {
		t.Errorf("the dir moved aside lost its HEAD: %v", err)
	}
	if bare := gitIn(t, r, mirror, "rev-parse", "--is-bare-repository"); bare != "true" {
		t.Errorf("the new mirror: bare %q", bare)
	}
}
