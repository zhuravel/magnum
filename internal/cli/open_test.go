package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/reveal"
	"github.com/zhuravel/magnum/internal/store"
)

func TestOpenRoleLabelsAndAliases(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-codex-judge", "p_1", "w_1")
	h.session(pr.ID, store.RoleClaude, store.SessionLive, "mg-talkable-5-claude-review", "p_2", "w_1")
	h.session(pr.ID, store.RoleSimplify, store.SessionLive, "mg-talkable-5-claude-simplify", "p_4", "w_1")
	for role, want := range map[string]string{
		"codex-judge": "mg-talkable-5-codex-judge", "judge": "mg-talkable-5-codex-judge",
		"claude-review": "mg-talkable-5-claude-review", "claude": "mg-talkable-5-claude-review", "Claude-Review": "mg-talkable-5-claude-review",
		"claude-simplify": "mg-talkable-5-claude-simplify", "simplify": "mg-talkable-5-claude-simplify",
	} {
		h.hd.focused = nil
		h.out.Reset()
		if code := h.cmd("open", "5", "--role", role, "--no-reveal"); code != 0 {
			t.Fatalf("--role %s: exit %d: %s", role, code, h.errb.String())
		}
		if strings.Join(h.hd.focused, ",") != want {
			t.Errorf("--role %s focused %v, want %s", role, h.hd.focused, want)
		}
	}
	h.out.Reset()
	if code := h.cmd("open", "5", "--role", "claude", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "talkable#5 claude-review: focused mg-talkable-5-claude-review")
}

func TestOpenFocusesTheAgentThenReveals(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.session(pr.ID, store.RoleClaude, store.SessionLive, "mg-talkable-5-claude", "p_2", "w_1")

	if code := h.cmd("open", "talkable#5", "--role", "claude", "--new-window"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Join(h.hd.focused, ",") != "mg-talkable-5-claude" {
		t.Fatalf("focused %v", h.hd.focused)
	}
	if len(h.reveals) != 1 || !h.reveals[0].NewWindow {
		t.Fatalf("reveals %+v", h.reveals)
	}
	actContains(t, h.out.String(), "talkable#5 claude-review: focused mg-talkable-5-claude")

	h.out.Reset()
	if code := h.cmd("open", "5", "--no-reveal", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var got actFocusResult
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, h.out.String())
	}
	if got.PR != "talkable#5" || got.Agent != "mg-talkable-5-judge" || got.PaneID != "p_1" || got.Reveal != nil || len(h.reveals) != 1 {
		t.Fatalf("json result %+v (reveals %d)", got, len(h.reveals))
	}
}

func TestOpenCodexPaneFallsBackToTheJudge(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.session(pr.ID, store.RoleCodexReview, store.SessionLive, "", "p_3", "w_1")
	h.hd.focusErr = map[string]error{"p_3": &herdr.Error{Method: "agent.focus", Code: herdr.CodeAgentNotFound, Message: "no agent"}}
	if code := h.cmd("open", "5", "--role", "codex"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Join(h.hd.focused, ",") != "mg-talkable-5-judge" {
		t.Fatalf("focused %v", h.hd.focused)
	}
	actContains(t, h.out.String(), "focused codex-judge beside the codex-review pane", string(reveal.ActionLaunched))
}

// TestOpenFocusesALivePaneWhenTheRolesPaneIsGone: magnum closes a retired
// session's pane (spec 58), and the operator can close a pane by hand.
// `magnum open` then focuses a live pane of the PR (the judge first) and
// says why: when the role's session is lost or closed with its pane gone,
// and when its session still reads live but herdr no longer has its agent.
// With no live pane left, it names the next round, which lays out new
// panes: herdr focuses agents only, so a workspace holding nothing but the
// operator's shell cannot be focused.
func TestOpenFocusesALivePaneWhenTheRolesPaneIsGone(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLost, "mg-talkable-5-judge", "p_1", "w_1")
	claude := h.session(pr.ID, store.RoleClaude, store.SessionLive, "mg-talkable-5-claude", "p_2", "w_1")
	if code := h.cmd("open", "5", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if got := strings.Join(h.hd.focused, ","); got != "mg-talkable-5-claude" {
		t.Fatalf("focused %v, want claude-review's agent", h.hd.focused)
	}
	actContains(t, h.out.String(), "talkable#5 codex-judge: focused mg-talkable-5-claude",
		"the codex-judge session is lost and its pane gone; focused claude-review", "magnum review talkable#5")
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("requests %+v: a lost session has nothing to restore", reqs)
	}

	// The session reads live, but its pane was closed by hand: herdr has no
	// agent there any more.
	h.hd.focused = nil
	h.out.Reset()
	h.hd.focusErr = map[string]error{"mg-talkable-5-claude": &herdr.Error{Method: "agent.focus", Code: herdr.CodeAgentNotFound, Message: "no agent"}}
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_9", "w_1")
	if code := h.cmd("open", "5", "--role", "claude", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if got := strings.Join(h.hd.focused, ","); got != "mg-talkable-5-judge" {
		t.Fatalf("focused %v, want the judge's agent", h.hd.focused)
	}
	actContains(t, h.out.String(), "the claude-review pane is gone; focused codex-judge")

	// Nothing live: the next round lays out the panes.
	if err := h.st.TransitionSession(h.ctx, claude.ID, nil, store.SessionLost, nil); err != nil {
		t.Fatal(err)
	}
	judge, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, store.RoleJudge)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionSession(h.ctx, judge.ID, nil, store.SessionClosed, nil); err != nil {
		t.Fatal(err)
	}
	h.errb.Reset()
	if code := h.cmd("open", "5", "--no-reveal"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "the codex-judge session of talkable#5 is closed", "magnum review talkable#5")
}

func TestOpenParkedPRWithoutDaemonPrintsTheManualResume(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	s := h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	if err := h.st.TransitionSession(h.ctx, s.ID, nil, store.SessionParked, func(u *store.SessionUpdate) {
		u.Set("session_id", "019a-uuid")
		u.Set("cwd", "/Users/x/Projects/talkable.review3")
		u.Set("agent_kind", "codex")
	}); err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("open", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "talkable#5 is parked and no daemon is running", "resume by hand: cd /Users/x/Projects/talkable.review3 && codex resume 019a-uuid")
	// Nothing stays queued to restore and pin the PR whenever a daemon starts.
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestOpenParkedPRWaitsForTheDaemon(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	s := h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	_ = h.st.TransitionSession(h.ctx, s.ID, nil, store.SessionParked, func(u *store.SessionUpdate) { u.Set("session_id", "u1") })
	h.pid = 99
	h.onSleep = func(h *actHarness) {
		if h.sleeps == 2 {
			h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_9", "w_9")
		}
	}
	if code := h.cmd("open", "5", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Join(h.hd.focused, ",") != "mg-talkable-5-judge" {
		t.Fatalf("focused %v", h.hd.focused)
	}
	p := actDecode[engine.OpenPayload](t, h.requests()[0].Payload)
	if p.Repo != "talkable/talkable" || p.Number != 5 || p.Role != store.RoleJudge {
		t.Fatalf("open payload %+v", p)
	}
	actContains(t, h.errb.String(), "asked the daemon to restore its sessions")
}

func TestOpenParkedPRWhenTheDaemonCannotRestore(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	s := h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	_ = h.st.TransitionSession(h.ctx, s.ID, nil, store.SessionParked, func(u *store.SessionUpdate) { u.Set("session_id", "u1") })
	h.pid = 99
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestFailed, `unknown request kind "open"`) }
	if code := h.cmd("open", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "this daemon cannot restore parked sessions yet", "magnum review talkable#5", "codex resume u1")
}

func TestOpenWithoutSessionsAndUsage(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRQueued)
	if code := h.cmd("open", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "talkable#5 has no codex-judge session yet", "magnum review talkable#5")
	h.errb.Reset()
	if code := h.cmd("open", "5", "--role", "nope"); code != 2 || !strings.Contains(h.errb.String(), `unknown role "nope"`) {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	h.errb.Reset()
	if code := h.cmd("open", "99"); code != 1 || !strings.Contains(h.errb.String(), "magnum review 99") {
		t.Fatalf("unknown PR: exit %d: %s", code, h.errb.String())
	}
}

// TestOpenNamesTheAutomationPermission: when macOS refuses magnum's
// AppleScript (error -1743), `magnum open` says which permission and where to
// grant it, instead of reporting the terminal activated.
func TestOpenNamesTheAutomationPermission(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	run := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"/bin/ps"}, Result: execx.Result{Stdout: []byte("100 ttys050  herdr --session default\n")}},
		{Prefix: []string{"/usr/bin/osascript"}, Result: execx.Result{Code: 1,
			Stderr: []byte("execution error: Not authorized to send Apple events to iTerm. (-1743)\n")}},
		{Prefix: []string{"/usr/bin/open"}},
	}}
	h.d.Reveal = func(ctx context.Context, o reveal.Options) (reveal.Outcome, error) {
		return reveal.Reveal(ctx, run, config.Terminal{App: "iTerm2", Session: "default"}, "herdr", o)
	}
	if code := h.cmd("open", "5"); code != 1 {
		t.Fatalf("exit %d, want 1\nstdout %s\nstderr %s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.errb.String(), "focused mg-talkable-5-judge in herdr", "Automation", "-1743",
		"System Settings → Privacy & Security → Automation → allow iTerm")
	if s := h.errb.String() + h.out.String(); strings.Contains(s, "activated") || strings.Contains(s, "fix [terminal]") {
		t.Fatalf("names the wrong fix:\n%s", s)
	}
	if len(run.CallsWithPrefix("/usr/bin/open")) != 0 {
		t.Fatal("brought iTerm forward anyway")
	}
}

// While a fix loop's fixer works, `o` and `magnum open` without --role
// focus the fixer's pane, not the judge's (which may be parked or not even
// started); --role still picks any role, the fixer included.
func TestOpenFocusesTheFixerWhileItWorks(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.session(pr.ID, "claude-fix", store.SessionLive, "mg-talkable-5-fix", "p_4", "w_1")
	b, _ := json.Marshal(engine.FixState{Cycle: 1, Cap: 3, Phase: engine.FixPhaseFixing, Role: "claude-fix", Head: "abc1234", Since: "abc1234", By: "magnum fix"})
	if err := h.st.SetKV(context.Background(), engine.KVPRFix(pr.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("open", "5", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "claude-fix: focused mg-talkable-5-fix")
	if code := h.cmd("open", "5", "--role", "codex-judge", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if code := h.cmd("open", "5", "--role", "claude-fix", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if got := strings.Join(h.hd.focused, ","); got != "mg-talkable-5-fix,mg-talkable-5-judge,mg-talkable-5-fix" {
		t.Fatalf("focused %v", h.hd.focused)
	}
}

// openGoneFixture seeds talkable#5 with its judge's session in state (lost or
// closed, its pane gone) and parked sessions of other roles, started the given
// duration ago; a daemon (pid 99) answers, and the pane of the role asked to
// restore comes up at the second poll.
func (h *actHarness) openGoneFixture(judge string, parked map[string]time.Duration) store.PR {
	h.t.Helper()
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, judge, "mg-talkable-5-codex-judge", "", "")
	for role, ago := range parked {
		_, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: role, State: store.SessionParked,
			SessionID: new("u-" + role), StartedAt: h.now.Add(-ago)})
		if err != nil {
			h.t.Fatal(err)
		}
	}
	h.pid = 99
	h.onSleep = func(h *actHarness) {
		for _, r := range h.requests() {
			p := actDecode[engine.OpenPayload](h.t, r.Payload)
			if live, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, p.Role); err != nil || store.Deref(live.HerdrPaneID) == "" {
				h.session(pr.ID, p.Role, store.SessionLive, "mg-talkable-5-"+p.Role, "p_9", "w_9")
			}
		}
	}
	return pr
}

// `magnum open` of a PR whose judge's session is closed used to refuse and send
// the operator to `magnum review`, with a parked conversation of another role
// one `--role` away from a restore (2026-10-27, a PR whose claude-review
// session was parked). Without --role it now restores through the newest
// parked session of any role (by started_at), the judge's own session lost
// as well as closed.
func TestOpenWithoutARoleRestoresThroughAParkedSessionWhenTheJudgesIsClosed(t *testing.T) {
	for _, judge := range []string{store.SessionClosed, store.SessionLost} {
		t.Run(judge, func(t *testing.T) {
			h := newActHarness(t)
			h.openGoneFixture(judge, map[string]time.Duration{
				store.RoleClaude:   10 * time.Minute, // the newest parked
				store.RoleSimplify: time.Hour,
			})
			if code := h.cmd("open", "5", "--no-reveal"); code != 0 {
				t.Fatalf("exit %d: %s", code, h.errb.String())
			}
			reqs := h.requests()
			if len(reqs) != 1 || actDecode[engine.OpenPayload](t, reqs[0].Payload).Role != store.RoleClaude {
				t.Fatalf("requests %+v, want one open of the newest parked role %s", reqs, store.RoleClaude)
			}
			actContains(t, h.out.String(), "talkable#5 claude-review: focused mg-talkable-5-claude-review")
			actContains(t, h.errb.String(), "talkable#5 is parked; asked the daemon to restore its sessions")
			if strings.Contains(h.errb.String(), "starts a new round with fresh sessions") {
				t.Fatalf("refused anyway:\n%s", h.errb.String())
			}
		})
	}
}

// Nothing parked, nothing to restore through: today's refusal, and no request.
// A role named with --role is that role's own question: its closed session
// refuses even beside a parked one of another role.
func TestOpenOfAClosedJudgeRefusesWhenNoSessionIsParkedOrARoleIsNamed(t *testing.T) {
	h := newActHarness(t)
	pr := h.openGoneFixture(store.SessionClosed, nil)
	h.session(pr.ID, store.RoleClaude, store.SessionClosed, "mg-talkable-5-claude-review", "", "")
	if code := h.cmd("open", "5", "--no-reveal"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "the codex-judge session of talkable#5 is closed", "starts a new round with fresh sessions")

	if _, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: store.RoleSimplify, State: store.SessionParked, SessionID: new("u-s")}); err != nil {
		t.Fatal(err)
	}
	h.errb.Reset()
	if code := h.cmd("open", "5", "--no-reveal", "--role", "codex-judge"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "the codex-judge session of talkable#5 is closed", "starts a new round with fresh sessions")
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("requests %+v: nothing was to be restored", reqs)
	}
}

// A live pane of the PR is what an open focuses, as before: the restore
// replaces the refusal, not the focus.
func TestOpenOfAClosedJudgeFocusesALivePaneBeforeItRestoresAParkedOne(t *testing.T) {
	h := newActHarness(t)
	pr := h.openGoneFixture(store.SessionClosed, map[string]time.Duration{store.RoleClaude: time.Minute})
	h.session(pr.ID, store.RoleSimplify, store.SessionLive, "mg-talkable-5-claude-simplify", "p_4", "w_1")
	if code := h.cmd("open", "5", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if got := strings.Join(h.hd.focused, ","); got != "mg-talkable-5-claude-simplify" {
		t.Fatalf("focused %v, want the live pane", h.hd.focused)
	}
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("requests %+v: a live pane needs no restore", reqs)
	}
}

// The board's o asks the question open decides: the parked session of another
// role restores the PR when the judge's is gone, as open does without --role
// (openRoleOf names that role, openWouldRestore says it restores); a live
// session of the PR, no parked one and a judge that never had a session
// restore nothing.
func TestTheBoardsOpenFollowsTheParkedSessionWhenTheJudgesIsGone(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	judgeGone := store.Session{Role: store.RoleJudge, State: store.SessionClosed}
	lost := store.Session{Role: store.RoleJudge, State: store.SessionLost}
	older := store.Session{Role: store.RoleSimplify, State: store.SessionParked, StartedAt: h.now.Add(-time.Hour)}
	claude := store.Session{Role: store.RoleClaude, State: store.SessionParked, StartedAt: h.now}
	liveSimplify := store.Session{Role: store.RoleSimplify, State: store.SessionLive}
	for _, tc := range []struct {
		name string
		all  []store.Session
		role string // what the board's row says open opens
		want bool
	}{
		{"closed judge, parked claude-review", []store.Session{judgeGone, claude}, store.RoleClaude, true},
		{"the newest parked one, whatever the order", []store.Session{judgeGone, claude, older}, store.RoleClaude, true},
		{"lost judge, parked claude-review", []store.Session{lost, claude}, store.RoleClaude, true},
		{"a live session of the PR", []store.Session{judgeGone, claude, liveSimplify}, store.RoleJudge, false},
		{"no parked session", []store.Session{judgeGone}, store.RoleJudge, false},
		{"no judge session at all", []store.Session{claude}, store.RoleJudge, false},
	} {
		if got := openWouldRestore(tc.all, store.RoleJudge, openRestorable(h.d.Cfg, "talkable/talkable")); got != tc.want {
			t.Errorf("%s: openWouldRestore %v, want %v", tc.name, got, tc.want)
		}
		if got := openRoleOf(h.ctx, h.st, h.d.Cfg, pr.ID, "talkable/talkable", tc.all); got != tc.role {
			t.Errorf("%s: openRoleOf %s, want %s", tc.name, got, tc.role)
		}
	}
}

// Two parked sessions that started together (the registry keeps seconds):
// the later one, the higher id (SessionsByPR lists by id), stands in for the
// judge's, whichever role it is.
func TestOpenRestoreRoleTakesTheLaterOfTwoParkedSessionsThatStartedTogether(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	judge := store.Session{ID: 1, Role: store.RoleJudge, State: store.SessionClosed, StartedAt: at.Add(-time.Hour)}
	claude := store.Session{ID: 2, Role: store.RoleClaude, State: store.SessionParked, StartedAt: at}
	simplify := store.Session{ID: 3, Role: store.RoleSimplify, State: store.SessionParked, StartedAt: at}
	if got := openRestoreRole([]store.Session{judge, claude, simplify}, store.RoleJudge, anyRole); got != store.RoleSimplify {
		t.Errorf("claude-review (id 2) and claude-simplify (id 3) parked together: %s, want %s", got, store.RoleSimplify)
	}
	simplify.ID, claude.ID = 2, 3
	if got := openRestoreRole([]store.Session{judge, simplify, claude}, store.RoleJudge, anyRole); got != store.RoleClaude {
		t.Errorf("claude-simplify (id 2) and claude-review (id 3) parked together: %s, want %s", got, store.RoleClaude)
	}
}

// anyRole lets any parked role stand in (openRestoreRole), as without a
// config.
func anyRole(string) bool { return true }

// Park parks every session with a session id, the fixer's (claude-fix) and
// the whiteboard writer's (claude-whiteboard) and a judge in reserve
// (claude-judge) too, and the daemon's open restores only the roles the
// watch runs (config.Config.RolesFor): the newest parked session of any role
// made open ask y/N and then fail with "unknown role", or keep the judge's
// refusal beside an older parked claude-review. Open, and the board's o,
// stand in only with a role the daemon's open takes: claude-review here,
// and with none parked the judge's refusal, no request and no question.
func TestOpenStandsInOnlyWithARoleTheDaemonRestores(t *testing.T) {
	h := newActHarness(t)
	pr := h.openGoneFixture(store.SessionClosed, map[string]time.Duration{
		"claude-fix":        time.Minute,
		"claude-whiteboard": 2 * time.Minute,
		"claude-judge":      3 * time.Minute,
		store.RoleClaude:    time.Hour, // the oldest parked, the only one the daemon restores
	})
	all, err := h.st.SessionsByPR(h.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := openRoleOf(h.ctx, h.st, h.d.Cfg, pr.ID, "talkable/talkable", all); got != store.RoleClaude {
		t.Errorf("openRoleOf = %s, want %s", got, store.RoleClaude)
	}
	if code := h.cmd("open", "5", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || actDecode[engine.OpenPayload](t, reqs[0].Payload).Role != store.RoleClaude {
		t.Fatalf("requests %+v, want one open of %s", reqs, store.RoleClaude)
	}
	actContains(t, h.out.String(), "talkable#5 claude-review: focused mg-talkable-5-claude-review")

	h2 := newActHarness(t)
	pr2 := h2.openGoneFixture(store.SessionClosed, map[string]time.Duration{"claude-fix": time.Minute, "claude-whiteboard": time.Hour})
	all2, err := h2.st.SessionsByPR(h2.ctx, pr2.ID)
	if err != nil {
		t.Fatal(err)
	}
	role := openRoleOf(h2.ctx, h2.st, h2.d.Cfg, pr2.ID, "talkable/talkable", all2)
	if restores := openWouldRestore(all2, role, openRestorable(h2.d.Cfg, "talkable/talkable")); role != store.RoleJudge || restores {
		t.Errorf("openRoleOf = %s (restores %v), want %s and no restore", role, restores, store.RoleJudge)
	}
	if code := h2.cmd("open", "5", "--no-reveal"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, h2.errb.String(), "the codex-judge session of talkable#5 is closed", "starts a new round with fresh sessions")
	if reqs := h2.requests(); len(reqs) != 0 {
		t.Fatalf("requests %+v: no parked session the daemon restores", reqs)
	}
}
