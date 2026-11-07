package agents

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/identity"
)

// fixerAppIdentity is a dedicated fixer App, narrowed, with its bot's id.
func fixerAppIdentity() config.Identity {
	return config.Identity{Name: "fixer-app", Kind: "app", Login: "fixer-app[bot]", AppID: 7, ClientID: "Iv-fix", InstallationID: 8,
		PrivateKeyEnv: "FIX_KEY", TokenRepositories: []string{"talkable"},
		TokenPermissions: map[string]string{"contents": "write", "pull_requests": "write"}, UserID: 4242}
}

// fixerPaneEnvs starts a judge and a fix role acting as id, with the
// identity env lookup and the fixer env of the daemon (identity.FixerEnv),
// and returns the fix role's split env, its session env and the judge's.
func fixerPaneEnvs(t *testing.T, id config.Identity, idEnv map[string]string) (split, session, judge map[string]string) {
	t.Helper()
	e := newEnv(t)
	cfg := &config.Config{Identities: []config.Identity{id}}
	e.m.d.IdentityEnv = func(name string) (map[string]string, bool) { return idEnv, name == id.Name }
	e.m.d.FixerEnv = identity.FixerEnv(cfg)
	fix := e.spec(config.RoleClaudeFix)
	fix.Identity = id.Name
	if _, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review1", appBaseEnv(e.pr.URL), "talkable#11920",
		[]config.Role{e.spec(RoleJudge), fix}); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	return e.h.splits[0].Opts.Env, e.session(Role(fix.Name)).Env, e.h.creates[0].Env
}

// A dedicated fixer App's pane commits as its bot, unsigned, and refuses a
// plain `git push` to github.com, all through the pane's env: the copies
// share one clone's config, which nothing writes. The posting App's judge
// pane gets none of it.
func TestAnAppFixPaneCommitsAsTheBotUnsignedThroughEnvOnly(t *testing.T) {
	split, session, judge := fixerPaneEnvs(t, fixerAppIdentity(),
		map[string]string{"GH_CONFIG_DIR": "/state/gh/fixer-app", "GH_TOKEN": "", "GITHUB_TOKEN": "", "GH_HOST": "github.com"})
	const email = "4242+fixer-app[bot]@users.noreply.github.com"
	const refused = "url./use-magnum-push-not-git-push/.pushInsteadOf"
	want := map[string]string{
		"GH_CONFIG_DIR":   "/state/gh/fixer-app",
		"GIT_AUTHOR_NAME": "fixer-app[bot]", "GIT_COMMITTER_NAME": "fixer-app[bot]",
		"GIT_AUTHOR_EMAIL": email, "GIT_COMMITTER_EMAIL": email,
		"GIT_CONFIG_COUNT": "4",
		"GIT_CONFIG_KEY_0": "commit.gpgsign", "GIT_CONFIG_VALUE_0": "false",
		"GIT_CONFIG_KEY_1": refused, "GIT_CONFIG_VALUE_1": "https://github.com/",
		"GIT_CONFIG_KEY_2": refused, "GIT_CONFIG_VALUE_2": "git@github.com:",
		"GIT_CONFIG_KEY_3": refused, "GIT_CONFIG_VALUE_3": "ssh://git@github.com/",
		"MAGNUM_PUSH_NO_WORKFLOWS": "1",
		"MAGNUM_PR_URL":            "https://github.com/talkable/talkable/pull/11920",
	}
	for what, env := range map[string]map[string]string{"split": split, "session": session} {
		for k, v := range want {
			if env[k] != v {
				t.Errorf("the fixer's %s env %s = %q, want %q", what, k, env[k], v)
			}
		}
	}
	for k := range judge {
		if strings.HasPrefix(k, "GIT_") || k == "MAGNUM_PUSH_NO_WORKFLOWS" {
			t.Errorf("the judge's pane env has %s", k)
		}
	}
}

// A gh fixer is the operator: its pane keeps the operator's git identity,
// signing and push, so it gets no GIT_* variable and no workflow refusal.
func TestAGhFixPaneKeepsTheOperatorsGitIdentity(t *testing.T) {
	split, session, _ := fixerPaneEnvs(t, config.Identity{Name: "me", Kind: "gh", Login: "zhuravel"}, map[string]string{"GH_HOST": "github.com"})
	for what, env := range map[string]map[string]string{"split": split, "session": session} {
		for k := range env {
			if strings.HasPrefix(k, "GIT_") || k == "MAGNUM_PUSH_NO_WORKFLOWS" {
				t.Errorf("the gh fixer's %s env has %s", what, k)
			}
		}
	}
}
