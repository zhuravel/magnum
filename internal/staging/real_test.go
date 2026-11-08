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

// A tiny origin in a temp dir: master, a PR branch and staging-void with
// the PR merged in by a two-parent merge. The mirror is a treeless bare
// clone of it, and the PR is latest once a deployment holds the merge,
// pushed with CI failed on its own path before.
func TestARealTreelessMirrorTellsLatestAndPushed(t *testing.T) {
	t.Parallel()
	r := hermetic(t)
	origin := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		res, err := r.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", origin}, args...)})
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(res.Out())
	}
	write := func(name, text string) {
		t.Helper()
		p := filepath.Join(origin, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "master")
	git("config", "uploadpack.allowFilter", "true")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	write("app/a.rb", "a\n")
	git("add", ".")
	git("commit", "-qm", "c1")
	c1 := git("rev-parse", "HEAD")
	git("branch", "staging-void")
	git("checkout", "-qb", "pr1")
	write("app/b.rb", "b\n")
	git("add", ".")
	git("commit", "-qm", "p1")
	p1 := git("rev-parse", "HEAD")
	git("update-ref", "refs/pull/1/head", p1)
	git("checkout", "-q", "staging-void")
	git("merge", "-q", "--no-ff", "-m", "Merge branch 'pr1' into staging-void", "pr1")
	merge := git("rev-parse", "HEAD")
	git("checkout", "-q", "master")

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
	if err := c.Refresh(context.Background(), "acme/app", []int{1}); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
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
	if err := c.Refresh(context.Background(), "acme/app", []int{1}); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if got := one(c, pr); got.State != StatePushed || got.Detail != DetailCIFailedOurs || got.DeployedTip != c1 {
		t.Errorf("deployed before the merge, CI failed on app/b.rb: %+v, want pushed, ci_failed_ours", got)
	}
}
