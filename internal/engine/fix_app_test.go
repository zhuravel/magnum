package engine

import (
	"fmt"
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

// fixerSession gives PR pr a live, idle fixer session whose pane was made
// with env: a pane an earlier cycle opened.
func fixerSession(h *harness, pr store.PR, env map[string]string) store.Session {
	h.t.Helper()
	name := "mg-t-2-claude-fix"
	s, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: "claude-fix", AgentName: &name, AgentKind: new("claude"),
		HerdrPaneID: new("p-claude-fix"), HerdrWorkspaceID: new("w1"), State: store.SessionLive, Env: env})
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// A fixer pane keeps the env it was made with, so a pane an earlier cycle
// opened as the operator's gh login would push and reply as the operator
// after the watch's fixer became an App: the cycle parks that session
// first (its conversation resumes in a new pane with the App's env), says
// so in a fix.pane_renewed event that names the identity, never the env,
// and the new pane acts as the App.
func TestAFixerPaneOfAnotherIdentityIsParkedBeforeTheCycle(t *testing.T) {
	h, git := fixHarness(t, withFixerApp)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	old := fixerSession(h, pr, map[string]string{"GH_HOST": "github.com", "MAGNUM_PR_URL": pr.URL})
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)

	park, ensure := h.callIndex(fmt.Sprintf("park_sessions:%d:claude-fix", pr.ID), 0), -1
	for i, c := range h.ag.all() {
		if strings.HasPrefix(c, "ensure_workspace:") && strings.HasSuffix(c, ":claude-fix") {
			ensure = i
		}
	}
	if park < 0 || ensure < 0 || park > ensure {
		t.Fatalf("calls %v: want the old fixer parked before the workspace", h.ag.all())
	}
	if s := h.sessionState(old.ID); s != store.SessionParked {
		t.Errorf("the old fixer session is %s, want parked", s)
	}
	evs := h.fixEvents("fix.pane_renewed")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, fixerAppName) || strings.Contains(evs[0].Message, "/state/gh") {
		t.Fatalf("fix.pane_renewed = %+v, want one naming the identity, not the env", evs)
	}
	if env := h.ag.paneEnv("claude-fix"); env["GH_CONFIG_DIR"] != "/state/gh/"+fixerAppName || env["GIT_AUTHOR_NAME"] != fixerAppLogin {
		t.Errorf("the new pane env = %v, want the App's", env)
	}
}

// A gh fixer's pane made before magnum set MAGNUM_PUSH_BRANCH and the
// other branch variables is the same identity: a missing MAGNUM_* key
// renews nothing, and the cycle resumes the fixer in its pane. The pane
// env of a new one carries the PR's branch, the default branch and the
// base, which magnum push reads.
func TestAGhFixPaneIsNotRenewedForTheNewMagnumVariables(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixerSession(h, pr, map[string]string{"MAGNUM_PR_URL": pr.URL, "WT_BRANCH": "review1"})
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	if i := h.callIndex("park_sessions:", 0); i >= 0 {
		t.Errorf("calls %v: a gh fixer's pane was parked for the MAGNUM_* variables", h.ag.all())
	}
	if len(h.fixEvents("fix.pane_renewed")) != 0 {
		t.Error("fix.pane_renewed for a gh fixer")
	}
	env := h.ag.paneEnv("claude-fix")
	if env["MAGNUM_PUSH_BRANCH"] != deref(h.pr(2).HeadRef) || env["MAGNUM_DEFAULT_BRANCH"] == "" || env["MAGNUM_BASE_BRANCH"] == "" {
		t.Errorf("the fixer's pane env = %v, want the PR's branch, the default branch and the base", env)
	}
}
