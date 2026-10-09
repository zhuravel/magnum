package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// hookNames are the hooks git runs for the commands magnum gives it: a
// checkout (switch, worktree add, clone), a ref update (fetch, update-ref,
// branch -D) and a merge.
var hookNames = []string{"post-checkout", "reference-transaction", "post-merge", "post-index-change"}

// writeHooks puts every hook of hookNames into dir, each appending its name
// to marker.
func writeHooks(t *testing.T, dir, marker string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range hookNames {
		script := "#!/bin/sh\necho " + name + " >> '" + marker + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// ranHooks lists the hooks that wrote to marker, and removes it.
func ranHooks(t *testing.T, marker string) []string {
	t.Helper()
	b, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	ran := strings.Fields(string(b))
	slices.Sort(ran)
	return slices.Compact(ran)
}

// A PR's setup script (bundle install, npm install) may set a relative
// core.hooksPath in the clone (husky's .husky/_, lefthook), and the clone
// may carry hooks in .git/hooks: no git command of magnum's runs them, so a
// later checkout never runs code from a PR's tree outside the sandbox.
func TestRealGitNeverRunsTheClonesHooks(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "hooks-ran")
	writeHooks(t, filepath.Join(fx.clone, ".git", "hooks"), marker)
	writeHooks(t, filepath.Join(fx.clone, ".husky", "_"), marker)
	fx.git(fx.clone, "config", "core.hooksPath", ".husky/_")

	// Control: git without magnum's settings runs them.
	fx.git(fx.clone, "switch", "--quiet", "--detach", fx.base)
	if ran := ranHooks(t, marker); !slices.Contains(ran, "post-checkout") {
		t.Fatalf("the control switch ran hooks %q, want post-checkout: the hooks are not set up", ran)
	}

	sha, err := fx.c.FetchPR(ctx, fx.clone, 7)
	if err != nil {
		t.Fatalf("FetchPR: %v", err)
	}
	if err := fx.c.ResetPlaceholder(ctx, fx.clone, "review1", "main"); err != nil {
		t.Fatalf("ResetPlaceholder: %v", err)
	}
	if err := fx.c.SwitchDetach(ctx, fx.clone, sha); err != nil {
		t.Fatalf("SwitchDetach: %v", err)
	}
	wt := filepath.Join(filepath.Dir(fx.clone), "clone__worktrees", "pr-7")
	if err := fx.c.WorktreeAdd(ctx, fx.clone, wt, PRRef(7), true, ""); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if _, err := fx.c.Status(ctx, wt); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if err := fx.c.WorktreeRemove(ctx, fx.clone, wt, true); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}
	if err := fx.c.UpdateRefDelete(ctx, fx.clone, PRRef(7)); err != nil {
		t.Fatalf("UpdateRefDelete: %v", err)
	}
	if err := fx.c.BranchDelete(ctx, fx.clone, "review1", true); err != nil {
		t.Fatalf("BranchDelete: %v", err)
	}
	if ran := ranHooks(t, marker); len(ran) != 0 {
		t.Fatalf("gitx ran the clone's hooks %q", ran)
	}
}

// A clone runs post-checkout from the hooks directory the user's config
// names; magnum's clone runs none.
func TestRealCloneRunsNoHooks(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "hooks-ran")
	hooks := filepath.Join(t.TempDir(), "hooks")
	writeHooks(t, hooks, marker)
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[core]\n\thooksPath = "+hooks+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &execx.Real{BaseEnv: slices.Clone(fx.r.BaseEnv)}
	for i, kv := range r.BaseEnv {
		if strings.HasPrefix(kv, "GIT_CONFIG_GLOBAL=") {
			r.BaseEnv[i] = "GIT_CONFIG_GLOBAL=" + global
		}
	}

	// Control: a plain clone with that config runs post-checkout.
	control := filepath.Join(filepath.Dir(fx.clone), "control")
	if _, err := r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"clone", "--quiet", fx.origin, control}}); err != nil {
		t.Fatal(err)
	}
	if ran := ranHooks(t, marker); !slices.Contains(ran, "post-checkout") {
		t.Fatalf("the control clone ran hooks %q, want post-checkout: the hooks are not set up", ran)
	}

	dest := filepath.Join(filepath.Dir(fx.clone), "magnum-clone")
	if err := New(r).CloneWith(ctx, fx.origin, dest, CloneOptions{CredentialHelper: GHCredentialHelper}); err != nil {
		t.Fatalf("CloneWith: %v", err)
	}
	if ran := ranHooks(t, marker); len(ran) != 0 {
		t.Fatalf("CloneWith ran hooks %q", ran)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("the clone has no checkout: %v", err)
	}
}
