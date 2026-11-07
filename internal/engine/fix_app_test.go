package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
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

// A fixer App's tokens reach its token_repositories alone: on a PR of
// another repository of the watch the mint still works (the repository is
// in the installation), the fixer edits and commits as the bot, and then
// every gh call on the PR and its push fail. `magnum fix` refuses such a PR
// before anything starts, and names the key to add; a PR of a repository
// the list names (any case) starts as before.
func TestPrepareFixRefusesARepositoryTheFixerAppsTokensDoNotReach(t *testing.T) {
	narrowTo := func(repos ...string) func(*harness) {
		return func(h *harness) {
			for i := range h.cfg.Identities {
				if h.cfg.Identities[i].Name == fixerAppName {
					h.cfg.Identities[i].TokenRepositories = repos
				}
			}
		}
	}
	h, git := fixHarness(t, withFixerApp, narrowTo("talkable-esp"))
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.handle()
	const want = "the fixer App fixer-app's tokens do not reach talkable/talkable: add talkable to its token_repositories"
	if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
		t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
	}
	if h.ag.count("submit:") != 0 || slices.ContainsFunc(h.ag.all(), func(c string) bool { return strings.HasSuffix(c, ":claude-fix") }) {
		t.Errorf("calls %v: a refused request made the fixer's pane", h.ag.all())
	}

	h, git = fixHarness(t, withFixerApp, narrowTo("talkable-esp", "Talkable"))
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id = h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(id); !strings.Contains(res, "fixing talkable/talkable#2") {
		t.Fatalf("answer = %q, want the loop started", res)
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

// The commits' author tells the identity too: a pane made as the same App
// before the operator corrected its user_id has the same gh config but
// another GIT_AUTHOR_EMAIL, and would keep committing with the wrong
// noreply email (which magnum push then refuses), so the cycle parks it
// and the fixer resumes in a pane with the right one.
func TestAFixerPaneOfAnotherCommitEmailIsParkedBeforeTheCycle(t *testing.T) {
	h, git := fixHarness(t, withFixerApp)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	role := *h.fixRole()
	role.Identity = fixerAppName
	made, err := h.ag.PaneEnv(map[string]string{"MAGNUM_PR_URL": pr.URL}, role)
	if err != nil {
		t.Fatal(err)
	}
	right := made["GIT_AUTHOR_EMAIL"]
	made["GIT_AUTHOR_EMAIL"] = "1+" + fixerAppLogin + "@users.noreply.github.com" // the user_id before the fix
	if right == "" || made["GH_CONFIG_DIR"] != "/state/gh/"+fixerAppName {
		t.Fatalf("the App's pane env = %v", made)
	}
	old := fixerSession(h, pr, made)
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	if s := h.sessionState(old.ID); s != store.SessionParked {
		t.Errorf("the old fixer session is %s, want parked", s)
	}
	if evs := h.fixEvents("fix.pane_renewed"); len(evs) != 1 {
		t.Errorf("fix.pane_renewed = %+v, want one", evs)
	}
	if env := h.ag.paneEnv("claude-fix"); env["GIT_AUTHOR_EMAIL"] != right {
		t.Errorf("the new pane's GIT_AUTHOR_EMAIL = %q, want %q", env["GIT_AUTHOR_EMAIL"], right)
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

// fixInput is the last fix prompt input h's fake agents got.
func (h *harness) fixInput() agents.FixPromptInput {
	h.t.Helper()
	h.ag.mu.Lock()
	defer h.ag.mu.Unlock()
	if len(h.ag.fixInputs) == 0 {
		h.t.Fatal("no fix prompt was rendered")
	}
	return h.ag.fixInputs[len(h.ag.fixInputs)-1]
}

// A fixer App's prompt names the `magnum push` line of the slot's checkout
// and the App's login, and its external file says it posts as the App
// (`fixer`); a gh fixer's names neither, and its file says the author.
func TestAnAppFixersPromptNamesMagnumPushAndItsLogin(t *testing.T) {
	for name, tc := range map[string]struct {
		mods       []func(*harness)
		push       bool
		login, fix string
	}{
		"app": {[]func(*harness){withFixerApp}, true, fixerAppLogin, fixerAppLogin},
		"gh":  {nil, false, "", "zhuravel"},
	} {
		t.Run(name, func(t *testing.T) {
			h, git := fixHarness(t, tc.mods...)
			fixablePR(h, "zhuravel", map[string]int{"P2": 1})
			fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
			id := h.fix(FixPayload{})
			h.handle()
			h.wantDone(id)
			in := h.fixInput()
			wantPush := ""
			if tc.push {
				wantPush = agents.PushLine(h.layout.Binary(), h.slot("review1").Path, "master")
			}
			if in.PushCommand != wantPush || in.FixerLogin != tc.login {
				t.Errorf("push %q, login %q; want %q, %q", in.PushCommand, in.FixerLogin, wantPush, tc.login)
			}
			b, err := os.ReadFile(in.ExternalFile)
			if err != nil {
				t.Fatal(err)
			}
			var x struct {
				Fixer string `json:"fixer"`
			}
			if err := json.Unmarshal(b, &x); err != nil || x.Fixer != tc.fix {
				t.Errorf("the external file's fixer = %q (%v), want %q", x.Fixer, err, tc.fix)
			}
		})
	}
}

// A PR retargeted while its fixer App's pane stays live (a stacked PR
// whose base merged) keeps the old base in the pane's MAGNUM_BASE_BRANCH,
// made with the pane, which the cycle keeps (the same identity): its push
// line names the PR's base as the cycle reads it, which magnum push takes
// over the pane's, so the fixer's merge of the new base is pushed.
func TestAnAppFixersPushLineNamesThePRsBaseOfTheCycle(t *testing.T) {
	h, git := fixHarness(t, withFixerApp)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	role := *h.fixRole()
	role.Identity = fixerAppName
	made, err := h.ag.PaneEnv(map[string]string{"MAGNUM_PR_URL": pr.URL, "MAGNUM_BASE_BRANCH": "master"}, role)
	if err != nil {
		t.Fatal(err)
	}
	fixerSession(h, pr, made)
	if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE prs SET base_ref = 'release' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	if i := h.callIndex("park_sessions:", 0); i >= 0 {
		t.Errorf("calls %v: the App's own pane was parked", h.ag.all())
	}
	if want := agents.PushLine(h.layout.Binary(), h.slot("review1").Path, "release"); h.fixInput().PushCommand != want {
		t.Errorf("push line %q, want %q", h.fixInput().PushCommand, want)
	}
}

// appFixJob is a fix job of PR pr on the talkable watch, whose review the
// posting App left.
func appFixJob(h *harness, pr store.PR) *fixJob {
	h.t.Helper()
	repo, err := h.st.RepoByID(h.ctx, pr.RepoID)
	if err != nil {
		h.t.Fatal(err)
	}
	return &fixJob{repo: repo, watch: h.cfg.Watches[0], pr: pr, login: magnumApp, started: h.clock.Now().Add(-time.Hour)}
}

// appReply is a fix-marked reply of the fixer App in an external thread.
func appReply(id int64, at time.Time) github.ThreadComment {
	return github.ThreadComment{ID: id, AuthorLogin: "fixer-app", AuthorType: "Bot", AuthorAssociation: "NONE",
		Body: "Fixed in abc1234: the guard\n\n" + postreview.FixReplyMarker, URL: prPage + "#discussion_r" + itoa(id), CreatedAt: at}
}

// The fixer App's fix-marked reply answers another reviewer's thread, as
// the author's reply or a gh fixer's does: fixLogins, the logins whose
// marked replies answer an item, carry the App's, and the external file
// lists it among fixer_logins. Before, a thread the App answered stayed
// open for the next cycle's fixer.
func TestTheAppFixersRepliesAnswerExternalItems(t *testing.T) {
	h := newHarness(t, withFixerApp)
	pr := h.reviewedPR(2, "b1")
	own := h.e.fixLogins(h.ctx, appFixJob(h, pr))
	if !slices.Contains(own, fixerAppLogin) {
		t.Fatalf("fixLogins = %v, want the fixer App's login", own)
	}
	at := time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)
	th := extThread(100, "a.rb", 1, at, "Is this nil-safe?", "rev-ann")
	th.Comments = append(th.Comments, appReply(101, at.Add(time.Minute)))
	x, _ := externalOf(externalInput{Threads: []github.Thread{th}, CommentsComplete: true, Author: "alice", Own: own})
	if len(x.Threads) != 1 || !x.Threads[0].Answered || !slices.Contains(x.FixerLogins, fixerAppLogin) {
		t.Errorf("external file = %+v, want the thread answered by the App and its login among fixer_logins", x)
	}
	// The magnum's own logins of a reply round never take the App: its
	// replies are the fixer's, not magnum's.
	if slices.Contains(h.e.ownLogins(h.ctx, h.cfg.Watches[0], pr), fixerAppLogin) {
		t.Error("ownLogins lists the fixer App")
	}
}

// Under a fixer App the fixer never posts as the author, so the author's
// own replies during a run are theirs, not unmarked replies of the fixer's;
// the App's are the fixer's. Under a gh fixer the author still counts.
func TestUnderAnAppFixerTheAuthorsHandRepliesAreNotUnmarked(t *testing.T) {
	h := newHarness(t, withFixerApp)
	pr := h.reviewedPR(2, "b1")
	job := appFixJob(h, pr)
	asFixer := h.e.sinceFixStart(h.ctx, job)
	now := h.clock.Now()
	if asFixer("alice", "User", now) {
		t.Error("under a fixer App the author's reply counts as the fixer's")
	}
	if !asFixer("fixer-app", "Bot", now) {
		t.Error("the fixer App's reply does not count as the fixer's")
	}
	if asFixer("fixer-app", "Bot", job.started.Add(-time.Minute)) {
		t.Error("a reply before the run started counts as the fixer's")
	}

	gh := newHarness(t)
	ghPR := gh.reviewedPR(2, "b1")
	if !gh.e.sinceFixStart(gh.ctx, appFixJob(gh, ghPR))("alice", "User", gh.clock.Now()) {
		t.Error("under a gh fixer the author's reply no longer counts as the fixer's")
	}
}

// The fixer App's reply in one of magnum's threads answers magnum as the
// author's own would, though GitHub gives an App no association with the
// repository: the poll counts it as a reply, and a reply round decides on
// it. Its remark anywhere else (a conversation comment, a review in
// another reviewer's thread) starts nothing.
func TestAnAppFixersReplyInMagnumsThreadStartsAReplyRound(t *testing.T) {
	inThread := func(h *harness) github.Remark {
		r := threadReply(h, fixerAppLogin)
		r.Bot, r.Association = true, "NONE"
		return r
	}
	for name, c := range map[string]struct {
		remark func(h *harness) github.Remark
		starts bool
	}{
		"in magnum's thread": {inThread, true},
		"a conversation comment": {func(h *harness) github.Remark {
			return github.Remark{At: h.clock.Now(), Author: fixerAppLogin, Bot: true, Association: "NONE"}
		}, false},
		"in another reviewer's thread": {func(h *harness) github.Remark {
			return github.Remark{Review: true, At: h.clock.Now(), Author: fixerAppLogin, Bot: true, Association: "NONE",
				Answers: []string{"rev-ann"}, AnswersKnown: true, Comments: 1}
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, withFixerApp)
			h.reviewedPR(2, "b1")
			h.advance(10 * time.Minute)
			reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{c.remark(h)}})
			want := store.PRReviewed
			if c.starts {
				want = store.PRRereviewPending
			}
			h.wantState(2, want)
		})
	}
	// Without a fixer App the same remark is a bot's without association: nothing.
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.advance(10 * time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{inThread(h)}})
	h.wantState(2, store.PRReviewed)
}

// The retro tells the fixer's replies from the author's by its marker and
// by the logins it takes as magnum's own (retroOwnLogins), which list every
// declared identity's: the fixer App's too. A pin: it held before.
func TestTheRetroTagsTheFixerAppsRepliesAsTheFixers(t *testing.T) {
	h := newHarness(t, withFixerApp)
	pr := h.reviewedPR(2, "b1")
	if got := h.e.retroOwnLogins(pr, nil); !slices.Contains(got, fixerAppLogin) {
		t.Errorf("retroOwnLogins = %v, want the fixer App's login", got)
	}
}
