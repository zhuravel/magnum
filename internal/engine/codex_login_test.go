package engine

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/usage"
)

// fakeLogin is Deps.CodexLogin: the login it was last set to.
type fakeLogin struct {
	mu    sync.Mutex
	login usage.Login
	err   error
	reads int
}

func (f *fakeLogin) read(string) (usage.Login, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.login, f.err
}

func (f *fakeLogin) set(fingerprint string, changed time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.login, f.err = usage.Login{Fingerprint: fingerprint, ChangedAt: changed}, nil
}

const (
	loginOld = "0123456789ab"
	loginNew = "ba9876543210"
)

// newLoginHarness is a harness whose Codex login is fl's, and whose
// ResumeID answers the parked sessions' ids as agents does.
func newLoginHarness(t *testing.T) (*harness, *fakeLogin) {
	t.Helper()
	fl := &fakeLogin{err: usage.ErrNoLogin}
	h := newHarness(t, func(h *harness) { h.d.CodexLogin = fl.read })
	h.ag.resumeParked = true
	return h, fl
}

// liveSession adds a live agent session of role to the PR, started at
// started, with a session id and agent status.
func (h *harness) liveSession(prID int64, role, kind string, started time.Time, status herdr.Status) store.Session {
	h.t.Helper()
	name := fmt.Sprintf("mg-x-%d-%s", prID, role)
	s, err := h.st.CreateSession(h.ctx, store.Session{PRID: prID, Role: role, AgentName: &name, AgentKind: &kind,
		SessionID: new("sid-" + name), HerdrPaneID: new("p-" + name), HerdrWorkspaceID: new("w1"), State: store.SessionLive,
		AgentStatus: new(string(status)), StartedAt: started})
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// sessionState is the state of session id now.
func (h *harness) sessionState(id int64) string {
	h.t.Helper()
	s, err := h.st.SessionByID(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return s.State
}

// liveSessionOf is the PR's live session of role.
func (h *harness) liveSessionOf(prID int64, role string) store.Session {
	h.t.Helper()
	s, err := h.st.LiveSessionByPRRole(h.ctx, prID, role)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// changeLogin lets a minute pass (the login is read once a minute) and
// switches Codex's login to fingerprint, changed now.
func changeLogin(h *harness, fl *fakeLogin, fingerprint string) time.Time {
	h.advance(time.Minute)
	at := h.clock.Now()
	fl.set(fingerprint, at)
	return at
}

// TestAChangedCodexLoginRestartsTheCodexSessionsStartedBeforeIt: the
// operator switched Codex accounts while #2's Codex judge was live and idle.
// That judge parks through the early park (its next prompt resumes it in a
// new process), while #2's Claude sessions and the Codex judge of a pinned
// PR stay live; the usage-limit pause, the old account's, is lifted and its
// budget reading overruled, and it is said once: one codex.login_changed
// event and one toast. The kept login is the new fingerprint, first seen
// then; the same login read again restarts nothing.
func TestAChangedCodexLoginRestartsTheCodexSessionsStartedBeforeIt(t *testing.T) {
	h, fl := newLoginHarness(t)
	fl.set(loginOld, h.clock.Now().Add(-time.Hour))
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	judge := h.liveSessionOf(pr.ID, store.RoleJudge)
	if got := h.events("codex.login_changed"); len(got) != 0 {
		t.Fatalf("the first read of a login with no older session said %+v", got)
	}
	if v, _ := kvValue(h, KVCodexLogin); v != loginOld {
		t.Fatalf("kept login %q, want %q", v, loginOld)
	}

	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick()
	pinned := h.pr(3)
	if err := h.st.UpdatePR(h.ctx, pinned.ID, func(u *store.PRUpdate) { u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	pinnedJudge := h.liveSession(pinned.ID, store.RoleJudge, agents.KindCodex, h.clock.Now(), herdr.StatusIdle)
	h.e.setToolPause(h.ctx, agents.KindCodex, string(agents.HealthUsageLimit), "You've hit your usage limit", h.clock.Now().Add(48*time.Hour))

	seen := changeLogin(h, fl, loginNew)
	h.tick()
	if got := h.earlyParks(pr.ID); got != 1 || h.ag.count(fmt.Sprintf("park_sessions:%d:%s", pr.ID, store.RoleJudge)) != 1 {
		t.Fatalf("early parks of #2 = %d (%v), want one of the Codex judge alone", got, h.ag.all())
	}
	if st := h.sessionState(judge.ID); st != store.SessionParked {
		t.Fatalf("#2's judge is %s, want parked", st)
	}
	if got := h.liveRoles(pr.ID); strings.Join(got, ",") != "claude-review,claude-simplify" {
		t.Fatalf("#2's live sessions = %v, want its Claude ones", got)
	}
	if st := h.sessionState(pinnedJudge.ID); st != store.SessionLive {
		t.Fatalf("the pinned PR's Codex judge is %s, want live", st)
	}
	if _, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatal("the usage-limit pause of the old account stayed")
	}
	if v, _ := kvValue(h, KVUsageCodexOverruledAt); v != store.FormatTime(seen) {
		t.Fatalf("budget overruled at %q, want %v", v, seen)
	}
	if v, _ := kvValue(h, KVCodexLogin); v != loginNew {
		t.Fatalf("kept login %q, want %q", v, loginNew)
	}
	if v, _ := kvValue(h, KVCodexLoginAt); v != store.FormatTime(seen) {
		t.Fatalf("login first seen %q, want %v", v, seen)
	}
	want := "Codex login changed: 1 Codex session restarts with the new one; the usage-limit pause of codex is lifted"
	if evs := h.events("codex.login_changed"); len(evs) != 1 || evs[0].Level != "info" || evs[0].Message != want {
		t.Fatalf("codex.login_changed events = %+v, want one %q", evs, want)
	}
	if evs := kindOf(subjectEvents(t, h, "pr:talkable/talkable#2"), "agent.restarted"); len(evs) != 1 ||
		!strings.Contains(evs[0].Message, "codex-judge (the Codex login changed)") {
		t.Fatalf("agent.restarted events = %+v", evs)
	}
	if _, err := h.e.batch.Flush(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := toastsWith(h, "Codex login changed"); len(got) != 1 || !strings.Contains(got[0], "1 Codex session restarts with the new one") {
		t.Fatalf("toasts %q, want one", h.nh.shown())
	}

	// The same login read again, a token refresh later: nothing more.
	h.advance(time.Minute)
	fl.set(loginNew, h.clock.Now())
	h.tick()
	if evs := h.events("codex.login_changed"); len(evs) != 1 {
		t.Fatalf("codex.login_changed events = %d after the same login again", len(evs))
	}
	if st := h.sessionState(pinnedJudge.ID); st != store.SessionLive {
		t.Fatalf("the pinned PR's Codex judge is %s", st)
	}
}

// TestATokenRefreshOfTheSameAccountRestartsNothing: Codex rewrites auth.json
// when it refreshes its tokens; the account, and so the fingerprint, stays,
// and the live sessions with it.
func TestATokenRefreshOfTheSameAccountRestartsNothing(t *testing.T) {
	h, fl := newLoginHarness(t)
	fl.set(loginOld, h.clock.Now().Add(-time.Hour))
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	changeLogin(h, fl, loginOld) // a new mtime, the same account
	h.tick()
	if n := h.earlyParks(pr.ID); n != 0 {
		t.Fatalf("parked %d times after a token refresh", n)
	}
	if evs := h.events("codex.login_changed"); len(evs) != 0 {
		t.Fatalf("codex.login_changed after a refresh: %+v", evs)
	}
	if got := h.liveRoles(pr.ID); !strings.Contains(strings.Join(got, ","), store.RoleJudge) {
		t.Fatalf("live sessions %v, want the judge kept", got)
	}
}

// TestTheFirstReadRestartsTheSessionsOlderThanTheAuthFilesChange: a daemon
// that kept no login yet (the build before this one) reads one whose
// auth.json changed after a live Codex session started: that session ran
// with an older login and restarts, while one started after the change
// stays; the login counts from the file's change.
func TestTheFirstReadRestartsTheSessionsOlderThanTheAuthFilesChange(t *testing.T) {
	h, fl := newLoginHarness(t)
	pr := h.reviewedPR(2, "b1") // no auth.json yet: no login kept
	h.withSessionIDs(pr.ID)
	judge := h.liveSessionOf(pr.ID, store.RoleJudge)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick()
	switched := h.clock.Now().Add(time.Minute)
	h.advance(2 * time.Minute)
	later := h.liveSession(h.pr(3).ID, store.RoleJudge, agents.KindCodex, h.clock.Now(), herdr.StatusIdle)
	fl.set(loginNew, switched)
	h.advance(time.Minute)
	h.tick()
	if st := h.sessionState(judge.ID); st != store.SessionParked {
		t.Fatalf("the judge older than the auth file's change is %s, want parked", st)
	}
	if st := h.sessionState(later.ID); st != store.SessionLive {
		t.Fatalf("the judge started after the change is %s, want live", st)
	}
	if v, _ := kvValue(h, KVCodexLoginAt); v != store.FormatTime(switched) {
		t.Fatalf("login since %q, want the file's change %v", v, switched)
	}
	if evs := h.events("codex.login_changed"); len(evs) != 1 || !strings.HasPrefix(evs[0].Message, "Codex login changed: 1 Codex session restarts") {
		t.Fatalf("codex.login_changed events = %+v", evs)
	}
}

// TestABusyCodexSessionRestartsAfterItsTurn: a judge at work when the login
// changes is not interrupted; it parks once its turn ended, and no round
// starts on its PR before.
func TestABusyCodexSessionRestartsAfterItsTurn(t *testing.T) {
	h, fl := newLoginHarness(t)
	fl.set(loginOld, h.clock.Now().Add(-time.Hour))
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	judge := h.liveSessionOf(pr.ID, store.RoleJudge)
	setStatus := func(st herdr.Status) {
		if err := h.st.UpdateSession(h.ctx, judge.ID, func(u *store.SessionUpdate) { u.Set("agent_status", string(st)) }); err != nil {
			t.Fatal(err)
		}
	}
	setStatus(herdr.StatusWorking)
	changeLogin(h, fl, loginNew)
	h.tick()
	if n := h.earlyParks(pr.ID); n != 0 || h.sessionState(judge.ID) != store.SessionLive {
		t.Fatalf("a working judge was parked (%d early parks)", n)
	}
	setStatus(herdr.StatusIdle)
	h.advance(30 * time.Second)
	h.tick()
	if st := h.sessionState(judge.ID); st != store.SessionParked || h.earlyParks(pr.ID) != 1 {
		t.Fatalf("after its turn the judge is %s (%d early parks), want parked once", st, h.earlyParks(pr.ID))
	}
}

// TestNoRoundStartsBeforeAMarkedSessionRestarts: while the park of a session
// the login change marked fails, the PR's rounds wait (no prompt reaches the
// old process); once it parks, the round resumes the judge in a new process.
func TestNoRoundStartsBeforeAMarkedSessionRestarts(t *testing.T) {
	h, fl := newLoginHarness(t)
	fl.set(loginOld, h.clock.Now().Add(-time.Hour))
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.ag.parkSessionsErr = fmt.Errorf("agents: quit session: %w", agents.ErrBusy)
	changeLogin(h, fl, loginNew)
	h.tick()
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(5 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	if v, _ := kvValue(h, store.KVPRGate(pr.ID)); !strings.Contains(v, "codex-judge restarts in a new process first (the Codex login changed)") {
		t.Fatalf("gate %q, want the restart", v)
	}

	h.ag.mu.Lock()
	h.ag.parkSessionsErr = nil
	h.ag.mu.Unlock()
	h.advance(time.Hour) // past the park's backoff
	h.tick()
	h.advance(time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].TargetSHA != "b2" {
		t.Fatalf("rounds = %d, want the re-review of b2 once the judge parked", len(ins))
	}
	if n := h.ag.count(fmt.Sprintf("start:%d:%s:sid-%s", pr.ID, store.RoleJudge, store.RoleJudge)); n != 1 {
		t.Fatalf("judge resumes = %d (%v), want one of its session id", n, h.ag.all())
	}
}

// judgeLimitRound is a round script whose judge turn, in the PR's live judge
// session, ends on Codex's usage limit; a continued round posts.
func judgeLimitRound(t *testing.T, h *harness) func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
	t.Helper()
	return func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.Kind == pipeline.KindContinue {
			return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: in.Round, ReviewID: 77, Event: "COMMENTED", ReviewCommit: in.TargetSHA}, nil
		}
		round, err := nextRound(h, in.PR.ID)
		if err != nil {
			return failRound(t, err)
		}
		judge, err := h.st.LiveSessionByPRRole(h.ctx, in.PR.ID, store.RoleJudge)
		if err != nil {
			return failRound(t, err)
		}
		sent := h.clock.Now()
		run, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: round, Role: store.RoleJudge, SessionID: &judge.ID, Kind: in.Kind,
			TargetSHA: in.TargetSHA, Identity: in.PR.Identity, ReviewerLogin: "talkable[bot]", State: store.RunFailed,
			Outcome: new(pipeline.OutcomeUsageLimit), PromptText: "judge", SubmittedAt: &sent})
		if err != nil {
			return failRound(t, err)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeUsageLimit, Round: round, JudgeRunID: run.ID, Error: "judge pane: ■ You've hit your usage limit",
			Pause: &pipeline.Pause{Kind: string(agents.HealthUsageLimit), Tool: agents.KindCodex, Detail: "■ You've hit your usage limit",
				Until: h.clock.Now().Add(48 * time.Hour), Proven: true, RunID: run.ID}}, nil
	}
}

// TestAPausedRoundWhoseJudgeWasParkedContinuesByResumingIt: a round paused on
// the old account's usage limit (Codex paused), then the login changes: the
// judge parks, the pause is lifted, and the paused round continues the
// judge's turn in a new process resumed with its session id.
func TestAPausedRoundWhoseJudgeWasParkedContinuesByResumingIt(t *testing.T) {
	h, fl := newLoginHarness(t)
	fl.set(loginOld, h.clock.Now().Add(-time.Hour))
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.rd.script = judgeLimitRound(t, h)
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRPaused)
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); !ok || p.Reason != string(agents.HealthUsageLimit) {
		t.Fatalf("codex pause %+v (%v), want the usage limit of a judge under the current login", p, ok)
	}

	changeLogin(h, fl, loginNew)
	h.tick()
	h.advance(time.Minute)
	h.tick()
	ins := h.rd.all()
	if last := ins[len(ins)-1]; last.Kind != pipeline.KindContinue || last.TargetSHA != "b2" {
		t.Fatalf("last round %s at %s, want the continue of b2", last.Kind, last.TargetSHA)
	}
	if n := h.ag.count(fmt.Sprintf("start:%d:%s:sid-%s", pr.ID, store.RoleJudge, store.RoleJudge)); n != 1 {
		t.Fatalf("judge resumes = %d (%v), want one with its session id", n, h.ag.all())
	}
	h.wantState(2, store.PRReviewed)
}

// TestAStaleSessionsUsageLimitPausesNoKind: a usage limit hit by a judge
// session started before the current Codex login was first seen is the old
// account's: Codex is not paused, the round pauses with a retry a minute
// later, one info event says so, the session parks, and the retry resumes
// it in a new process. The same limit in a session started under the
// current login pauses Codex as before.
func TestAStaleSessionsUsageLimitPausesNoKind(t *testing.T) {
	for _, stale := range []bool{true, false} {
		t.Run(fmt.Sprintf("stale=%v", stale), func(t *testing.T) {
			h, fl := newLoginHarness(t)
			fl.set(loginOld, h.clock.Now().Add(-time.Hour))
			pr := h.reviewedPR(2, "b1")
			h.withSessionIDs(pr.ID)
			judge := h.liveSessionOf(pr.ID, store.RoleJudge)
			if stale { // the login changed while the judge was at work, so it stayed live
				h.e.setKV(h.ctx, KVCodexLoginAt, store.FormatTime(judge.StartedAt.Add(time.Second)))
			}
			h.rd.script = judgeLimitRound(t, h)
			pollPR(h, 40*time.Minute, 2, "b2")
			h.advance(5 * time.Minute)
			paused := h.clock.Now()
			h.tick()
			p := h.wantState(2, store.PRPaused)
			_, kindPaused := h.e.toolPause(h.ctx, agents.KindCodex)
			evs := kindOf(subjectEvents(t, h, "pr:talkable/talkable#2"), "codex.stale_usage_limit")
			if !stale {
				if !kindPaused || len(evs) != 0 || len(h.events("tool.paused")) != 1 {
					t.Fatalf("a current session's limit: codex paused %v, stale events %+v", kindPaused, evs)
				}
				return
			}
			if kindPaused || len(h.events("tool.paused")) != 0 {
				t.Fatal("the old login's usage limit paused codex")
			}
			if p.NextAttemptAt == nil || !p.NextAttemptAt.Equal(paused.Add(time.Minute)) {
				t.Fatalf("retry at %v, want a minute after %v", p.NextAttemptAt, paused)
			}
			if !strings.Contains(deref(p.LastError), "it restarts, and the round retries in a minute") {
				t.Fatalf("last_error %q", deref(p.LastError))
			}
			if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "codex-judge hit a usage limit in a session started at") ||
				!strings.Contains(evs[0].Message, "codex is not paused") {
				t.Fatalf("codex.stale_usage_limit events = %+v", evs)
			}
			h.advance(30 * time.Second)
			h.tick()
			if st := h.sessionState(judge.ID); st != store.SessionParked {
				t.Fatalf("the stale judge is %s, want parked", st)
			}
			h.advance(time.Minute)
			h.tick()
			if last := h.rd.all()[len(h.rd.all())-1]; last.Kind != pipeline.KindContinue {
				t.Fatalf("last round %s, want the continue", last.Kind)
			}
			if n := h.ag.count(fmt.Sprintf("start:%d:%s:sid-%s", pr.ID, store.RoleJudge, store.RoleJudge)); n != 1 {
				t.Fatalf("judge resumes = %d (%v)", n, h.ag.all())
			}
		})
	}
}

// TestOnlyTheLoginsFingerprintReachesTheRegistry: with the real reader over
// an auth.json full of tokens, a change of account leaves its fingerprint
// in kv and nothing else of the file in kv, the events or the log.
func TestOnlyTheLoginsFingerprintReachesTheRegistry(t *testing.T) {
	home := t.TempDir()
	logs := &syncBuffer{}
	writeAuth := func(account string) {
		body := fmt.Sprintf(`{"OPENAI_API_KEY":null,"tokens":{"id_token":"eyJSECRET-id","access_token":"eyJSECRET-access","refresh_token":"rt-SECRET","account_id":%q},"last_refresh":"2026-10-21T22:00:00Z"}`, account)
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAuth("acct-PRIVATE-one")
	h := newHarness(t, func(h *harness) {
		h.cfg.Usage.CodexHome = home
		h.d.CodexLogin = usage.CodexLogin
		h.d.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	h.ag.resumeParked = true
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	writeAuth("acct-PRIVATE-two")
	h.advance(time.Minute)
	h.tick()
	if evs := h.events("codex.login_changed"); len(evs) != 1 {
		t.Fatalf("codex.login_changed events = %+v, want the change", evs)
	}
	fp, _ := kvValue(h, KVCodexLogin)
	if len(fp) != 12 {
		t.Fatalf("kept login %q, want a 12-digit fingerprint", fp)
	}
	dump := strings.Join([]string{dumpRows(h, "SELECT key || '=' || value FROM kv"),
		dumpRows(h, "SELECT message || ' ' || COALESCE(data_json, '') FROM events"), logs.String()}, "\n")
	for _, secret := range []string{"SECRET", "PRIVATE", "acct-", "eyJ"} {
		if strings.Contains(dump, secret) {
			t.Fatalf("%q of auth.json reached the registry or the log", secret)
		}
	}
}

// dumpRows is every row of q, a query of one text column, a line each.
func dumpRows(h *harness, q string) string {
	h.t.Helper()
	rows, err := h.st.DB().QueryContext(h.ctx, q)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			h.t.Fatal(err)
		}
		b.WriteString(s + "\n")
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return b.String()
}
