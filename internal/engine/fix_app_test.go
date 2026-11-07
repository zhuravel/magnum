package engine

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// fixerAppName and fixerAppLogin are the dedicated fixer App of
// withFixerApp.
const (
	fixerAppName  = "fixer-app"
	fixerAppLogin = "fixer-app[bot]"
)

// withFixerApp gives the talkable watch a fixer of its own: a GitHub App
// no watch reviews, polls or approves as, narrowed to its repository and to
// contents and pull_requests write, named by the watch's fix_identity.
func withFixerApp(h *harness) {
	h.cfg.Identities = append(h.cfg.Identities, config.Identity{Name: fixerAppName, Kind: "app", Login: fixerAppLogin,
		AppID: 7, ClientID: "Iv-fix", InstallationID: 8, PrivateKeyEnv: "MAGNUM_TEST_FIX_KEY",
		TokenRepositories: []string{"talkable"}, TokenPermissions: map[string]string{"contents": "write", "pull_requests": "write"},
		UserID: 4242})
	h.cfg.Watches[0].FixIdentity = fixerAppName
	app := &fakeAppIdentity{fakeIdentity{name: fixerAppName, login: fixerAppLogin, kind: "app"}}
	h.ids[fixerAppName] = &app.fakeIdentity
	h.d.Identities[fixerAppName] = app
}

// A watch whose fix_identity names a dedicated App runs its fixer as that
// App: `magnum fix` starts the loop, and the fixer's pane selects the App's
// gh config, not the posting App's and not the operator's login. Before,
// prepareFix refused every App ("is an App; name a gh identity").
func TestPrepareFixRunsAsADedicatedApp(t *testing.T) {
	h, git := fixHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(id); !strings.Contains(res, "fixing talkable/talkable#2") {
		t.Fatalf("answer = %q, want the loop started", res)
	}
	if env := h.ag.paneEnv("claude-fix"); env["GH_CONFIG_DIR"] != "/state/gh/"+fixerAppName {
		t.Errorf("the fixer's pane env = %v, want the fixer App's gh config", env)
	}
}

// The App a watch reviews as is never the fixer: `magnum fix` refuses it,
// and says what to set instead.
func TestPrepareFixRefusesTheReviewApp(t *testing.T) {
	h, _ := fixHarness(t, func(h *harness) { h.cfg.Watches[0].FixIdentity = "talkable-app" })
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	id := h.fix(FixPayload{})
	h.handle()
	const want = "the fix role's identity talkable-app is the App magnum reviews as. Set fix_identity to your gh identity or a dedicated App"
	if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
		t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
	}
	if h.ag.count("submit:") != 0 {
		t.Error("a refused request started the fixer")
	}
}
