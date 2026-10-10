package agents

import (
	"maps"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// appBaseEnv is the pane env of a watch that posts as an App: the App's gh
// config dir with the tokens blanked, plus the slot and MAGNUM_* context.
func appBaseEnv(prURL string) map[string]string {
	return map[string]string{
		"GH_CONFIG_DIR": "/state/gh/talkable-app", "GH_TOKEN": "", "GITHUB_TOKEN": "", "GH_HOST": "github.com",
		"MAGNUM_PR_URL": prURL, "MAGNUM_IDENTITY": "app", "WT_BRANCH": "review1",
	}
}

func hasKey(env map[string]string, key string) bool { _, ok := env[key]; return ok }

func TestAFixRolesPaneGetsItsIdentitysEnv(t *testing.T) {
	e := newEnv(t)
	var asked []string
	e.m.d.IdentityEnv = func(name string) (map[string]string, bool) {
		asked = append(asked, name)
		if name != "me" {
			return nil, false
		}
		return map[string]string{"GH_HOST": "github.com", "ME_ONLY": "1"}, true
	}
	fix := e.spec(config.RoleClaudeFix)
	if fix.Identity != "me" {
		t.Fatalf("the built-in fix role's identity = %q, want me", fix.Identity)
	}

	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review1", appBaseEnv(e.pr.URL), "talkable#11920",
		[]config.Role{e.spec(RoleJudge), fix})
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if ws.Panes[Role(fix.Name)] == "" {
		t.Fatalf("no pane for the fix role: %v", ws.Panes)
	}

	// The judge posts as the watch's App and keeps its env, in the workspace's
	// root pane and in its session row.
	judgeEnv := e.h.creates[0].Env
	if judgeEnv["GH_CONFIG_DIR"] != "/state/gh/talkable-app" || hasKey(judgeEnv, "ME_ONLY") {
		t.Errorf("the judge's pane env = %v, want the App's", judgeEnv)
	}
	if got := e.session(RoleJudge).Env; got["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
		t.Errorf("the judge's session env = %v", got)
	}

	// The fixer acts as the operator: the App's gh selection and its blanked
	// tokens are gone, the identity's env is laid over, and the slot and the
	// MAGNUM_* context stay.
	fixEnv := e.h.splits[0].Opts.Env
	sessionEnv := e.session(Role(fix.Name)).Env
	for what, env := range map[string]map[string]string{"split": fixEnv, "session": sessionEnv} {
		for _, key := range []string{"GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN"} {
			if hasKey(env, key) {
				t.Errorf("the fix role's %s env keeps %s: %v", what, key, env)
			}
		}
		if env["GH_HOST"] != "github.com" || env["ME_ONLY"] != "1" {
			t.Errorf("the fix role's %s env lacks the identity's env: %v", what, env)
		}
		if env["MAGNUM_PR_URL"] != e.pr.URL || env["MAGNUM_IDENTITY"] != "app" || env["WT_BRANCH"] != "review1" {
			t.Errorf("the fix role's %s env lost the PR's context: %v", what, env)
		}
	}
	if len(asked) == 0 || asked[0] != "me" {
		t.Errorf("identities asked = %v, want me", asked)
	}
}

func TestAPaneEnvIsUnchangedWhenItsIdentityDoesNotResolve(t *testing.T) {
	cases := map[string]func(name string) (map[string]string, bool){
		"no identity lookup":     nil,
		"an undeclared identity": func(string) (map[string]string, bool) { return nil, false },
	}
	for name, lookup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.m.d.IdentityEnv = lookup
			base := appBaseEnv(e.pr.URL)
			want := maps.Clone(base)

			_, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review1", base, "talkable#11920",
				[]config.Role{e.spec(config.RoleClaudeFix)})
			if err != nil {
				t.Fatalf("EnsureWorkspace: %v", err)
			}
			if got := e.h.creates[0].Env; !maps.Equal(got, want) {
				t.Errorf("pane env = %v, want the base env %v", got, want)
			}
		})
	}
}

func TestARolesOwnEnvOutranksItsIdentitysEnv(t *testing.T) {
	e := newEnv(t)
	e.m.d.IdentityEnv = func(string) (map[string]string, bool) {
		return map[string]string{"GH_HOST": "github.com", "WHO": "identity"}, true
	}
	fix := e.spec(config.RoleClaudeFix)
	fix.Env = map[string]string{"WHO": "role"}

	_, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review1", appBaseEnv(e.pr.URL), "talkable#11920", []config.Role{fix})
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if got := e.h.creates[0].Env["WHO"]; got != "role" {
		t.Errorf("WHO = %q, want the role's own env to win", got)
	}
}
