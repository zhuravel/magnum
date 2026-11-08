package staging

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	writeIn(t, mirror, markerFile, "acme/app\n") // magnum's mirror, which lost its refs/

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

// cloned is a client on a real treeless mirror of a fresh origin, cloned and
// read once, with its clock, mirror dir and origin.
func cloned(t *testing.T) (c *Client, now *time.Time, mirror, origin string, r *execx.Real) {
	t.Helper()
	r = hermetic(t)
	origin, _, _, _ = newOrigin(t, r)
	mirror = filepath.Join(t.TempDir(), "staging", "acme", "app.git")
	clock := time.Now()
	c = New(Deps{
		Git:    newGit(r, func(string) string { return "file://" + origin }),
		GitHub: newFakeGitHub(),
		Mirror: func(string) (string, bool) { return mirror, true },
		GitEnv: func(string) map[string]string { return map[string]string{} },
		Now:    func() time.Time { return clock },
	})
	refreshCloned(t, c, OpenPR{Number: 1, Base: "master"})
	return c, &clock, mirror, origin, r
}

// untouched fails the test when the mirror at dir moved or was cloned again:
// no dir beside it was made, and it is still the bare repository it was.
func untouched(t *testing.T, c *Client, r *execx.Real, mirror, head string) {
	t.Helper()
	if aside, _ := filepath.Glob(mirror + ".broken-*"); len(aside) != 0 {
		t.Errorf("the mirror was moved aside: %q", aside)
	}
	if _, busy := c.clonesInto(mirror); busy {
		t.Error("a clone runs")
	}
	if got := gitIn(t, r, mirror, "rev-parse", "HEAD"); got != head {
		t.Errorf("the mirror's HEAD is %s, want %s: it was cloned again", got, head)
	}
}

// The review's probe: a Refresh whose caller is already cancelled (a daemon
// stopping mid-tick) never takes a healthy mirror for none: it returns the
// cancel, and the mirror stays, with no clone.
func TestACancelledRefreshLeavesTheMirrorInPlace(t *testing.T) {
	t.Parallel()
	c, now, mirror, _, r := cloned(t)
	head := gitIn(t, r, mirror, "rev-parse", "HEAD")
	*now = now.Add(RefreshEvery)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Refresh(ctx, "acme/app", []OpenPR{{Number: 1, Base: "master"}}); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled Refresh: %v, want the cancel", err)
	}
	c.bg.Wait()
	untouched(t, c, r, mirror, head)
}

// A git that cannot run (exec: not found, a fork that fails) is no answer
// about the mirror: Refresh says the mirror could not be read, and the
// mirror stays, with no clone.
func TestAGitThatCannotRunLeavesTheMirrorInPlace(t *testing.T) {
	t.Parallel()
	_, now, mirror, origin, r := cloned(t)
	head := gitIn(t, r, mirror, "rev-parse", "HEAD")
	broken := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}, Err: fmt.Errorf("exec: %q: %w", "git", exec.ErrNotFound)}}}
	c := New(Deps{
		Git:    newGit(broken, func(string) string { return "file://" + origin }),
		GitHub: newFakeGitHub(),
		Mirror: func(string) (string, bool) { return mirror, true },
		GitEnv: func(string) map[string]string { return map[string]string{} },
		Now:    func() time.Time { return now.Add(RefreshEvery) },
	})
	err := c.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 1, Base: "master"}})
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("Refresh with no git: %v, want the mirror could not be read", err)
	}
	c.bg.Wait()
	for _, call := range broken.Calls {
		if slices.Contains(call.Args, "clone") {
			t.Errorf("a clone was started: %q", call.Args)
		}
	}
	untouched(t, c, r, mirror, head)
}

// Magnum's mirror whose origin is another repository's (a change of the
// URL's format) is a definite answer that it is no mirror of this one: it
// is moved aside under the flock, as a broken one is, and the mirror is
// cloned again from this repository's origin, never fetched into as it was.
func TestAMirrorWhoseOriginIsAnotherRepositoryIsMovedAsideAndClonedAgain(t *testing.T) {
	t.Parallel()
	_, now, mirror, _, r := cloned(t)
	refs := gitIn(t, r, mirror, "for-each-ref")
	other, _, p1, _ := newOrigin(t, r)
	c := New(Deps{
		Git:    newGit(r, func(string) string { return "file://" + other }),
		GitHub: newFakeGitHub(),
		Mirror: func(string) (string, bool) { return mirror, true },
		GitEnv: func(string) map[string]string { return map[string]string{} },
		Now:    func() time.Time { return now.Add(RefreshEvery) },
	})
	refreshCloned(t, c, OpenPR{Number: 1, Base: "master"})
	if got := gitIn(t, r, mirror, "config", "--get", "remote.origin.url"); got != "file://"+other {
		t.Errorf("the mirror's origin is %q, want this repository's %q", got, "file://"+other)
	}
	aside, _ := filepath.Glob(mirror + ".broken-*")
	if len(aside) != 1 {
		t.Fatalf("moved aside: %q, want the other repository's clone, once", aside)
	}
	if got := gitIn(t, r, aside[0], "for-each-ref"); got != refs {
		t.Errorf("the clone moved aside changed:\n%s\nwant\n%s", got, refs)
	}
	if got := one(c, PR{Repo: "acme/app", Number: 1, Head: p1, Base: "master"}); got.State == StateUnknown {
		t.Errorf("on the new mirror: %s %q", got.State, got.Why)
	}
}

// A clone writes the marker that makes the mirror magnum's: the repository's
// name, in the git dir.
func TestACloneMarksTheMirrorAsMagnums(t *testing.T) {
	t.Parallel()
	_, _, mirror, _, _ := cloned(t)
	if got, err := os.ReadFile(filepath.Join(mirror, markerFile)); err != nil || strings.TrimSpace(string(got)) != "acme/app" {
		t.Errorf("the marker: %q, %v; want acme/app", got, err)
	}
}

// A bare clone at the mirror's path that magnum did not make (no marker) is
// never moved or deleted: when its origin is this repository's (a mirror from
// before the marker), it is adopted and marked; when it is another's, the
// Refresh fails and touches nothing.
func TestABareCloneWithoutTheMarkerIsAdoptedOnlyWhenItsOriginIsThisRepositorys(t *testing.T) {
	t.Parallel()
	r := hermetic(t)
	origin, _, p1, _ := newOrigin(t, r)
	root := t.TempDir()
	mirror := filepath.Join(root, "staging", "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, r, filepath.Dir(mirror), "clone", "-q", "--bare", "--filter=tree:0", "--single-branch", "--no-tags", "--", "file://"+origin, mirror)
	head := gitIn(t, r, mirror, "rev-parse", "HEAD")
	clock := time.Now()
	client := func(url string) *Client {
		return New(Deps{
			Git:    newGit(r, func(string) string { return url }),
			GitHub: newFakeGitHub(),
			Mirror: func(string) (string, bool) { return mirror, true },
			GitEnv: func(string) map[string]string { return map[string]string{} },
			Now:    func() time.Time { return clock },
		})
	}
	other := client("https://github.com/acme/other.git")
	err := other.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 1, Base: "master"}})
	if err == nil || !strings.Contains(err.Error(), "not magnum's mirror") {
		t.Errorf("another repository's clone without the marker: %v, want not magnum's mirror", err)
	}
	other.bg.Wait()
	untouched(t, other, r, mirror, head)
	if _, err := os.Stat(filepath.Join(mirror, markerFile)); err == nil {
		t.Error("another repository's clone was marked")
	}

	clock = clock.Add(RefreshEvery)
	mine := client("file://" + origin)
	if err := mine.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 1, Base: "master"}}); err != nil {
		t.Fatalf("this repository's clone without the marker: %v", err)
	}
	mine.bg.Wait()
	untouched(t, mine, r, mirror, head)
	if got, err := os.ReadFile(filepath.Join(mirror, markerFile)); err != nil || strings.TrimSpace(string(got)) != "acme/app" {
		t.Errorf("adopted, the marker is %q (%v)", got, err)
	}
	if got := one(mine, PR{Repo: "acme/app", Number: 1, Head: p1, Base: "master"}); got.State == StateUnknown {
		t.Errorf("on the adopted mirror: %s %q", got.State, got.Why)
	}
}

// GIT_CONFIG in the environment points git config at another file, where
// the origin read finds none: it is scrubbed, so a healthy mirror is never
// taken for none, moved aside and cloned again at every Refresh.
func TestGitConfigInTheEnvironmentNeverMovesTheMirror(t *testing.T) {
	t.Parallel()
	_, now, mirror, origin, r := cloned(t)
	head := gitIn(t, r, mirror, "rev-parse", "HEAD")
	other := filepath.Join(t.TempDir(), "other.config")
	writeIn(t, filepath.Dir(other), filepath.Base(other), "[core]\n\tbare = true\n")
	polluted := &execx.Real{BaseEnv: append(slices.Clone(r.BaseEnv), "GIT_CONFIG="+other, "GIT_CONFIG_SYSTEM="+other)}
	clock := now.Add(RefreshEvery)
	c := New(Deps{
		Git:    newGit(polluted, func(string) string { return "file://" + origin }),
		GitHub: newFakeGitHub(),
		Mirror: func(string) (string, bool) { return mirror, true },
		GitEnv: func(string) map[string]string { return map[string]string{} },
		Now:    func() time.Time { return clock },
	})
	for range 3 {
		if err := c.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 1, Base: "master"}}); err != nil {
			t.Errorf("Refresh with GIT_CONFIG set: %v", err)
		}
		c.bg.Wait()
		clock = clock.Add(RefreshEvery)
	}
	untouched(t, c, r, mirror, head)
}
