package slots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
)

// testKeyEnv stands for an App identity's private_key_env.
const testKeyEnv = "MAGNUM_TEST_APP_PEM"

// heavyUnsetWant is what every heavy command removes from the environment it
// inherits: git's redirections, the GitHub tokens and the App keys.
func heavyUnsetWant() []string {
	return slices.Concat(gitx.ScrubbedEnv(), DefaultStripEnv, []string{testKeyEnv})
}

func missingUnset(c execx.Cmd) []string {
	var miss []string
	for _, k := range heavyUnsetWant() {
		if !slices.Contains(c.Unset, k) {
			miss = append(miss, k)
		}
	}
	return miss
}

// Pool scripts run PR-controlled code (bundle install on the PR's Gemfile):
// the setup, the post-checkout install and the teardown never inherit a
// GitHub token, an App's private key or a variable that points git at
// another repository from the daemon's environment.
func TestPoolScriptsNeverInheritTheDaemonsSecrets(t *testing.T) {
	h := newHarness(t)
	h.m = h.newManager(Deps{SecretEnv: []string{testKeyEnv}})
	sl, _ := h.claimedCheckout(7, h.shaPR7)
	if err := h.m.Release(h.ctx, sl, h.pool, "pr_closed"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := h.m.Remove(h.ctx, h.slot(sl.Name), h.pool, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	calls := h.scriptCalls("")
	ran := map[string]bool{}
	for _, mc := range calls {
		ran[mc.Script] = true
		if miss := missingUnset(mc.Cmd); len(miss) > 0 {
			t.Errorf("%q inherits %v (unset %v)", mc.Script, miss, mc.Cmd.Unset)
		}
	}
	for _, s := range slices.Concat(h.pool.Setup, h.pool.PostCheckout, h.pool.Teardown) {
		if !ran[s] {
			t.Errorf("%q never ran: %v", s, calls)
		}
	}
}

// A per-PR worktree's hooks run the PR's code too: through mise or, without
// it, through /bin/sh, the process gets neither the tokens nor the App key
// the daemon has; a value the [[repo]] env sets on purpose is still set.
func TestPerPRHooksNeverInheritTheDaemonsSecrets(t *testing.T) {
	t.Run("through mise", func(t *testing.T) {
		f := newPerPR(t, true)
		h := f.h
		writeFile(t, filepath.Join(f.main, WTConfigFile), "[post-start]\nsetup = \"./bin/setup\"\n")
		h.m = h.newManager(Deps{SecretEnv: []string{testKeyEnv}})
		rec := f.recordHooks(t)
		if _, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7); err != nil {
			t.Fatal(err)
		}
		if len(rec.execs) != 1 {
			t.Fatalf("execs = %+v", rec.execs)
		}
		if miss := missingUnset(rec.execs[0].Cmd); len(miss) > 0 {
			t.Fatalf("the hook inherits %v (unset %v)", miss, rec.execs[0].Cmd.Unset)
		}
	})
	t.Run("through /bin/sh", func(t *testing.T) {
		f := newPerPR(t, true)
		h := f.h
		out := filepath.Join(t.TempDir(), "env")
		writeFile(t, filepath.Join(f.main, WTConfigFile), "[post-start]\nsetup = \"env > '"+out+"'\"\n")
		h.m = h.newManager(Deps{SecretEnv: []string{testKeyEnv}, LookPath: func(string) (string, error) { return "", errors.New("not found") }})
		// The daemon's environment: a token, an App key and a GIT_DIR.
		daemon := &execx.Real{BaseEnv: append(os.Environ(),
			"GH_TOKEN=gho_daemon", "GITHUB_TOKEN=ghs_daemon", testKeyEnv+"=-----BEGIN PRIVATE KEY-----",
			"GIT_DIR=/elsewhere/.git", "MAGNUM_TEST_KEPT=kept")}
		h.fake.Rules = append([]execx.Rule{{Prefix: []string{"/bin/sh"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			return daemon.Run(context.Background(), c)
		}}}, h.fake.Rules...)
		if _, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("the hook did not run: %v", err)
		}
		env := string(b)
		for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", testKeyEnv, "GIT_DIR"} {
			if strings.Contains(env, "\n"+k+"=") || strings.HasPrefix(env, k+"=") {
				t.Errorf("the hook got %s", k) // never the value: the test's environment may hold real tokens
			}
		}
		if !strings.Contains(env, "\nMAGNUM_TEST_KEPT=kept\n") || !strings.Contains(env, "\nWT_BRANCH=magnum-pr-7\n") {
			t.Error("the hook lost the rest of its environment (MAGNUM_TEST_KEPT, WT_BRANCH)")
		}
	})
}
