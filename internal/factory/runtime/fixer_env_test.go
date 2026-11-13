package runtime

import (
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// The factory's fixer pushes and answers its PR's threads as the fix
// identity of the PR's watch, as magnum's fixer does: its pane carries that
// identity's GitHub env (a fixer App's gh config dir, which holds the App's
// token) and its git env (the App's bot, a plain push refused), and every
// other variable that may hold a GitHub token blank, whatever the copy's
// env or the herdr server's would give it. Another role of the same task
// acts as you, as before: no identity's env reaches it.
func TestAFixerPaneCarriesTheAppTokenAlone(t *testing.T) {
	g := newRig(t)
	const appDir = "/state/gh/fixer-app"
	g.identityEnv = func(name string) (map[string]string, bool) {
		if name != "fixer-app" {
			return nil, false
		}
		return map[string]string{"GH_CONFIG_DIR": appDir, "GH_TOKEN": "", "GITHUB_TOKEN": "", "GH_HOST": "github.com"}, true
	}
	g.fixerEnv = func(identity string) map[string]string {
		if identity != "fixer-app" {
			return nil
		}
		return map[string]string{"GIT_AUTHOR_NAME": "fixer-app[bot]", "GIT_COMMITTER_NAME": "fixer-app[bot]", "MAGNUM_PUSH_NO_WORKFLOWS": "1"}
	}
	i := slices.IndexFunc(g.cfg.Factory.Roles, func(r config.Role) bool { return r.Name == config.FactoryFixerRole })
	g.cfg.Factory.Roles[i].Identity = "fixer-app" // as Config.FactoryFixerFor resolves the watch's fix_identity
	g.copyEnv = map[string]string{"RAILS_ENV": "development", "GH_CONFIG_DIR": "/Users/me/.config/gh",
		"GITHUB_TOKEN": "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "GITHUB_PERSONAL_ACCESS_TOKEN": "github_pat_abcdefghij0123456789"}
	g.restart()

	g.launch(config.FactoryFixerRole, "fx-repro")

	fixer := g.session(config.FactoryFixerRole)
	want := map[string]string{"GH_CONFIG_DIR": appDir, "GH_TOKEN": "", "GITHUB_TOKEN": "", "GITHUB_PERSONAL_ACCESS_TOKEN": "", "GH_HOST": "github.com",
		"GIT_AUTHOR_NAME": "fixer-app[bot]", "MAGNUM_PUSH_NO_WORKFLOWS": "1", "RAILS_ENV": "development"}
	for k, v := range want {
		if got, ok := fixer.Env[k]; !ok || got != v {
			t.Errorf("the fixer's pane has %s = %q (set %v), want %q", k, got, ok, v)
		}
	}
	for k, v := range fixer.Env {
		if v != "" && (k == "GH_TOKEN" || k == "GITHUB_TOKEN" || k == "GITHUB_PERSONAL_ACCESS_TOKEN") {
			t.Errorf("the fixer's pane carries a token of another account in %s", k)
		}
	}
	other := g.session("fx-repro")
	if dir := other.Env["GH_CONFIG_DIR"]; dir == appDir {
		t.Errorf("fx-repro's pane names the fixer App's gh config dir %q: it acts as you", dir)
	}
	if _, ok := other.Env["GIT_AUTHOR_NAME"]; ok {
		t.Errorf("fx-repro's pane commits as %q: only the fixer acts as its identity", other.Env["GIT_AUTHOR_NAME"])
	}
}

// A fixer whose identity does not resolve (a runtime without identities,
// or one it cannot make the env of) is never started with the env it was
// given: its launch fails, as magnum's fixer's does, rather than push as
// you.
func TestAFixerWhoseIdentityDoesNotResolveStartsNot(t *testing.T) {
	g := newRig(t)
	i := slices.IndexFunc(g.cfg.Factory.Roles, func(r config.Role) bool { return r.Name == config.FactoryFixerRole })
	g.cfg.Factory.Roles[i].Identity = "fixer-app"
	g.restart()
	if err := g.r.Launch(g.ctx, g.task, g.attempt, g.claimed(), g.roles(config.FactoryFixerRole)); err == nil {
		t.Fatal("the fixer launched with no identity's env")
	}
	if starts := g.h.startCalls(); len(starts) != 0 {
		t.Fatalf("%d agents started, want none", len(starts))
	}
}
