package staging

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// TestLiveTreelessFetchOfAPublicRepository runs the real git path against
// github.com, read-only: a treeless bare clone of the public zhuravel/magnum
// into a temp mirror, with git's empty HOME, its scrubbed config and gh as
// the only credential helper (the ghHelper line), then Base and one fetch of
// the default branch's tip. No push, no staging branch, no pull ref. It runs
// only with MAGNUM_LIVE_GITHUB=1, and skips when git or gh is missing.
func TestLiveTreelessFetchOfAPublicRepository(t *testing.T) {
	if os.Getenv("MAGNUM_LIVE_GITHUB") != "1" {
		t.Skip("set MAGNUM_LIVE_GITHUB=1 to fetch zhuravel/magnum from github.com")
	}
	for _, tool := range []string{"git", "gh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	const repo = "zhuravel/magnum"
	g := NewGit(&execx.Real{})
	dir := filepath.Join(t.TempDir(), "staging", "zhuravel", "magnum.git")
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{} // a public repository: gh answers only if git asks
	if err := g.Clone(ctx, dir, repo, env); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	base, err := g.Base(ctx, dir, repo)
	if err != nil {
		t.Fatalf("Base: %v", err)
	}
	if err := g.Fetch(ctx, dir, env, []string{"+refs/heads/" + base + ":refs/heads/" + base}); err != nil {
		t.Fatalf("Fetch of %s: %v", base, err)
	}
	refs, err := g.Refs(ctx, dir, "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	tip := refs["refs/heads/"+base]
	if !isOID(tip) || len(refs) != 1 {
		t.Fatalf("refs = %v, want the default branch %s alone, at a sha", refs, base)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dir)), ".home", ".netrc")); err == nil {
		t.Error("git's HOME has a .netrc")
	}
	if chain, err := g.FirstParent(ctx, dir, tip, 3); err != nil || len(chain) == 0 || !strings.EqualFold(chain[0].SHA, tip) {
		t.Errorf("FirstParent(%s) = %v, %v", tip, chain, err)
	}
	t.Logf("%s: default branch %s at %s", repo, base, tip)
}
