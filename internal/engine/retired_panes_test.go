package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// operatorShell adds a pane no session records (the operator's shell) to
// the workspace of PR #n's live judge, which keeps that workspace open
// through a park, and returns the workspace, the shell and the PR's panes
// there.
func operatorShell(t *testing.T, h *harness, sh *stuckHerdr, n int) (ws, operator string, ours []string) {
	t.Helper()
	judge, err := h.st.LiveSessionByPRRole(h.ctx, h.pr(n).ID, "codex-judge")
	if err != nil {
		t.Fatal(err)
	}
	ws = deref(judge.HerdrWorkspaceID)
	ours = sh.panesOf(ws)
	sh.mu.Lock()
	operator = sh.newPaneLocked(ws, deref(judge.Cwd)).ID
	sh.mu.Unlock()
	return ws, operator, ours
}

// panesClosed are the pr.panes_closed events of PR #n with their count
// and roles.
func panesClosed(t *testing.T, h *harness, n int) []panesClosedData {
	t.Helper()
	var out []panesClosedData
	for _, ev := range approvalEvents(t, h, n, agents.EventPanesClosed) {
		var d panesClosedData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

type panesClosedData struct {
	Count int      `json:"count"`
	Roles []string `json:"roles"`
	Panes []string `json:"panes"`
}

// sessionStates counts PR #n's sessions by state.
func sessionStates(t *testing.T, h *harness, n int) map[string]int {
	t.Helper()
	ss, err := h.st.SessionsByPR(h.ctx, h.pr(n).ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, s := range ss {
		out[s.State]++
	}
	return out
}

// sameSet reports whether a and b hold the same strings.
func sameSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// wantResumedElsewhere fails unless the agents started since start (the
// judge and claude-review) resumed their parked conversations, none in a
// pane of closed.
func wantResumedElsewhere(t *testing.T, h *harness, sh *stuckHerdr, start int, closed []string) {
	t.Helper()
	starts := sh.startOpts()[start:]
	if len(starts) != 2 {
		cur := h.pr(2)
		t.Fatalf("starts = %+v, want the judge and claude-review resumed (rounds %d, state %s, last error %q)", starts, len(h.rd.all()), cur.State, deref(cur.LastError))
	}
	for _, o := range starts {
		if !slices.Contains(o.Args, "resume") && !slices.Contains(o.Args, "--resume") {
			t.Fatalf("%s started fresh (%q): want its parked conversation resumed", o.Name, o.Args)
		}
		if slices.Contains(closed, o.PaneID) {
			t.Fatalf("%s resumed in %s, a closed pane", o.Name, o.PaneID)
		}
	}
}

// TestAnIdleParkClosesItsPane: the idle park of a reviewed PR left every
// pane it parked as a shell ("Resume this session with: …") beside the
// operator's shell, which keeps the workspace open; only a fresh start
// closed them (spec 52), so talkable#11932 gathered 21 panes. The idle park
// now closes them: the operator's shell alone remains, the sessions stay
// parked (codex-review's shell session closed), one pr.panes_closed follows
// pr.parked, and the next round resumes each conversation in a new pane.
func TestAnIdleParkClosesItsPane(t *testing.T) {
	h, sh := newStuckHarness(t)
	h.reviewedPR(2, "b1")
	ws, operator, ours := operatorShell(t, h, sh, 2)

	h.advance(2*time.Hour + time.Minute) // past every agent's threshold, the judge's judge_fresh_after included: the PR's park
	h.tick()
	if got := sh.panesOf(ws); !slices.Equal(got, []string{operator}) {
		t.Fatalf("workspace panes after the idle park = %v, want the operator's shell %s alone", got, operator)
	}
	if st := sessionStates(t, h, 2); st[store.SessionParked] != 3 || st[store.SessionClosed] != 1 || st[store.SessionLive] != 0 {
		t.Fatalf("sessions = %v, want the three agents parked and codex-review's shell closed", st)
	}
	if !h.hasEvent("pr:talkable/talkable#2", "pr.parked") {
		t.Fatal("no pr.parked event")
	}
	if got := panesClosed(t, h, 2); len(got) != 1 || got[0].Count != len(ours) || !sameSet(got[0].Panes, ours) {
		t.Fatalf("%s events = %+v, want one naming the round's panes %v", agents.EventPanesClosed, got, ours)
	}

	start := len(sh.startOpts())
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	wantResumedElsewhere(t, h, sh, start, ours)
}

// TestARoundsEndParkClosesItsPane: once a round ended and the PR's next
// round waits behind `magnum pause` (or a drain, the daily cap, a far
// retry), its agents are parked after park_idle_after like a reviewed PR's,
// and that park closes their panes too; the round that follows the pause
// resumes them in new panes.
func TestARoundsEndParkClosesItsPane(t *testing.T) {
	h, sh := newStuckHarness(t)
	h.reviewedPR(2, "b1")
	ws, operator, ours := operatorShell(t, h, sh, 2)
	h.e.setKV(h.ctx, store.KVDaemonPaused, "1")
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.wantState(2, store.PRRereviewPending)

	h.advance(3 * time.Hour)
	h.tick()
	h.wantState(2, store.PRRereviewPending)
	if w := waitOf(t, h, 2); w.Reason != WaitPaused {
		t.Fatalf("wait = %+v, want the pause", w)
	}
	if got := sh.panesOf(ws); !slices.Equal(got, []string{operator}) {
		t.Fatalf("workspace panes after the park = %v, want the operator's shell %s alone", got, operator)
	}
	if got := panesClosed(t, h, 2); len(got) != 1 || !sameSet(got[0].Panes, ours) {
		t.Fatalf("%s events = %+v, want one naming %v", agents.EventPanesClosed, got, ours)
	}

	start := len(sh.startOpts())
	h.e.delKV(h.ctx, store.KVDaemonPaused)
	h.tick()
	wantResumedElsewhere(t, h, sh, start, ours)
}

// leftover is a group of retired sessions talkable#11932 kept a pane for.
type leftover struct {
	role, kind, state string
	n                 int
}

// TestTheRestartClosesTheLeftoverPanes: talkable#11932's workspace held 21
// panes on 2026-10-17 (2 claude-fix, 3 codex-judge, 7 claude-review, 7
// codex-review, 2 claude-whiteboard) for 4 live sessions, with 19 sessions
// parked and 9 lost, since no fresh start ran on it after spec 52 closed
// the panes only there. The restart's reconcile closes the 17 leftovers in
// one go: the 4 live panes and the operator's shell remain, one
// pr.panes_closed names the 17 and their five roles, no session changes
// state, the parked fixer stays resumable, and a second reconcile finds
// nothing to close and records nothing.
func TestTheRestartClosesTheLeftoverPanes(t *testing.T) {
	h, sh := newStuckHarness(t)
	h.reviewedPR(2, "b1")
	pr := h.pr(2)
	ws, operator, live := operatorShell(t, h, sh, 2)
	judge, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "codex-judge")
	if err != nil {
		t.Fatal(err)
	}
	cwd := deref(judge.Cwd)
	session := func(role, kind, state, pane string, i int) {
		t.Helper()
		s := store.Session{PRID: pr.ID, Role: role, AgentKind: new(kind), HerdrPaneID: new(pane), HerdrWorkspaceID: new(ws),
			Cwd: new(cwd), State: state}
		if kind != agents.KindShell {
			s.AgentName = new(agents.AgentName("talkable/talkable", 2, agents.Role(role)))
			if state == store.SessionParked {
				s.SessionID = new(fmt.Sprintf("%s-%d", role, i))
			}
		}
		if _, err := h.st.CreateSession(h.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	// claude-simplify ran on the first round only; its session was parked
	// and its pane went long ago. The whiteboard's writer is the fourth
	// live session.
	simplify, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "claude-simplify")
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.PaneClose(h.ctx, deref(simplify.HerdrPaneID)); err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionSession(h.ctx, simplify.ID, nil, store.SessionParked, func(u *store.SessionUpdate) {
		u.Set("session_id", "simplify-1")
	}); err != nil {
		t.Fatal(err)
	}
	live = slices.DeleteFunc(live, func(p string) bool { return p == deref(simplify.HerdrPaneID) })
	sh.mu.Lock()
	board := sh.newPaneLocked(ws, cwd).ID
	sh.mu.Unlock()
	sh.addAgent(board, agents.AgentName("talkable/talkable", 2, "claude-whiteboard"), "claude", herdr.StatusIdle)
	session("claude-whiteboard", "claude", store.SessionLive, board, 0)
	live = append(live, board)
	closedBefore := len(sh.closedPanes())

	var retired []string
	for _, g := range []leftover{
		{"claude-fix", "claude", store.SessionParked, 2},
		{"codex-judge", "codex", store.SessionParked, 1}, {"codex-judge", "codex", store.SessionLost, 1},
		{"claude-review", "claude", store.SessionParked, 4}, {"claude-review", "claude", store.SessionLost, 2},
		{"codex-review", agents.KindShell, store.SessionClosed, 4}, {"codex-review", agents.KindShell, store.SessionLost, 2},
		{"claude-whiteboard", "claude", store.SessionParked, 1},
	} {
		for i := range g.n {
			sh.mu.Lock()
			p := sh.newPaneLocked(ws, cwd).ID
			sh.mu.Unlock()
			session(g.role, g.kind, g.state, p, i)
			retired = append(retired, p)
		}
	}
	// Sessions whose panes went long ago (closed by hand, or with herdr).
	for i := range 10 {
		session("claude-review", "claude", store.SessionParked, fmt.Sprintf("%s:p9%02d", ws, i), 10+i)
	}
	for i := range 4 {
		session("codex-judge", "codex", store.SessionLost, fmt.Sprintf("%s:p8%02d", ws, i), i)
	}
	if n := len(sh.panesOf(ws)); n != 22 {
		t.Fatalf("workspace panes = %d, want 21 and the operator's shell", n)
	}
	states := sessionStates(t, h, 2)
	if states[store.SessionLive]+states[store.SessionStarting] != 4 || states[store.SessionParked] != 19 || states[store.SessionLost] != 9 {
		t.Fatalf("sessions = %v, want 4 live (codex-review's shell starting), 19 parked and 9 lost", states)
	}

	h.e = New(h.d) // the restart
	h.startup()
	if got := sh.panesOf(ws); !sameSet(got, append(slices.Clone(live), operator)) {
		t.Fatalf("workspace panes after the restart = %v, want the live %v and the operator's shell %s", got, live, operator)
	}
	if closes := sh.closedPanes()[closedBefore:]; !sameSet(closes, retired) {
		t.Fatalf("pane closes = %v, want the 17 leftovers %v", closes, retired)
	}
	got := panesClosed(t, h, 2)
	if len(got) != 1 || got[0].Count != 17 || !sameSet(got[0].Panes, retired) ||
		!slices.Equal(got[0].Roles, []string{"claude-fix", "claude-review", "claude-whiteboard", "codex-judge", "codex-review"}) {
		t.Fatalf("%s events = %+v, want one naming the 17 leftovers %v", agents.EventPanesClosed, got, retired)
	}
	if after := sessionStates(t, h, 2); !mapsEqual(after, states) {
		t.Fatalf("sessions after the restart = %v, want them as they were %v", after, states)
	}
	if id, err := h.d.Agents.ResumeID(h.ctx, pr.ID, "claude-fix"); err != nil || id != "claude-fix-1" {
		t.Fatalf("the fixer's resume id = %q, %v: want its parked conversation", id, err)
	}

	h.advance(11 * time.Minute) // reconcile_interval
	h.tick()
	if got := panesClosed(t, h, 2); len(got) != 1 {
		t.Fatalf("%s events after a second reconcile = %+v, want none new", agents.EventPanesClosed, got)
	}
}

// mapsEqual reports whether two counts are the same.
func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestThePeriodicReconcileClosesPanesButRecordsNothingWhenIdle: the
// reconcile (every reconcile_interval) closes what a park, a lost agent or
// an older daemon left (a parked fixer's pane, the pane of a launch that
// died), one pr.panes_closed for the lot; the next reconciles find nothing
// to close and record nothing. A PR a round holds is left to its round
// until it lets go, and the live panes and the operator's shell stay.
func TestThePeriodicReconcileClosesPanesButRecordsNothingWhenIdle(t *testing.T) {
	h, sh := newStuckHarness(t)
	h.cfg.Daemon.ParkIdleAfter.Duration = 0 // the reviewers would park 15 minutes after the round: this is the reconcile's test
	h.reviewedPR(2, "b1")
	pr := h.pr(2)
	ws, operator, live := operatorShell(t, h, sh, 2)
	judge, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "codex-judge")
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, s := range []store.Session{
		{Role: "claude-fix", AgentKind: new("claude"), State: store.SessionParked, SessionID: new("fix-1")},
		{Role: "claude-review", AgentKind: new("claude"), State: store.SessionLost},
	} {
		sh.mu.Lock()
		p := sh.newPaneLocked(ws, deref(judge.Cwd)).ID
		sh.mu.Unlock()
		s.PRID, s.HerdrPaneID, s.HerdrWorkspaceID, s.Cwd = pr.ID, new(p), new(ws), judge.Cwd
		s.AgentName = new(agents.AgentName("talkable/talkable", 2, agents.Role(s.Role)))
		if _, err := h.st.CreateSession(h.ctx, s); err != nil {
			t.Fatal(err)
		}
		left = append(left, p)
	}

	// A round holds the PR: its setup reuses or replaces a lost pane, so
	// the reconcile leaves the PR alone.
	if !h.e.reserve(pr.ID, &roundHandle{cancel: func() {}}) {
		t.Fatal("reserve")
	}
	h.advance(11 * time.Minute)
	h.tick()
	if got := panesClosed(t, h, 2); len(got) != 0 || len(sh.panesOf(ws)) != len(live)+3 {
		t.Fatalf("under a round: events %+v, panes %v: want nothing closed", got, sh.panesOf(ws))
	}
	h.e.unreserve(pr.ID)

	h.advance(11 * time.Minute)
	h.tick()
	if got := sh.panesOf(ws); !sameSet(got, append(slices.Clone(live), operator)) {
		t.Fatalf("workspace panes = %v, want the live %v and the operator's shell", got, live)
	}
	if got := panesClosed(t, h, 2); len(got) != 1 || !sameSet(got[0].Panes, left) ||
		!slices.Equal(got[0].Roles, []string{"claude-fix", "claude-review"}) {
		t.Fatalf("%s events = %+v, want one naming %v", agents.EventPanesClosed, got, left)
	}
	reconciled, _ := h.e.kvTime(h.ctx, store.KVDaemonLastReconcile)
	for range 2 {
		h.advance(11 * time.Minute)
		h.tick()
	}
	if last, _ := h.e.kvTime(h.ctx, store.KVDaemonLastReconcile); !last.After(reconciled) {
		t.Fatalf("last reconcile %v, want later than %v", last, reconciled)
	}
	if got := panesClosed(t, h, 2); len(got) != 1 {
		t.Fatalf("%s events after idle reconciles = %+v, want none new", agents.EventPanesClosed, got)
	}
}

// TestALostAgentsPaneIsClosedAtOnce: an agent that went away while no
// round runs (its CLI exited, the operator quit it) leaves its pane as a
// shell; the tick that marks the session lost closes that pane, while the
// lost conversation stays resumable. A session lost while a round holds the
// PR keeps its pane for that round, which reuses or replaces it.
func TestALostAgentsPaneIsClosedAtOnce(t *testing.T) {
	h, sh := newStuckHarness(t)
	h.reviewedPR(2, "b1")
	pr := h.pr(2)
	ws, operator, _ := operatorShell(t, h, sh, 2)
	claude, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "claude-review")
	if err != nil {
		t.Fatal(err)
	}
	gone := func(name string) {
		sh.mu.Lock()
		sh.agents = slices.DeleteFunc(sh.agents, func(a herdr.AgentInfo) bool { return a.Name == name })
		sh.mu.Unlock()
	}

	if !h.e.reserve(pr.ID, &roundHandle{cancel: func() {}}) {
		t.Fatal("reserve")
	}
	gone(deref(claude.AgentName))
	h.advance(time.Minute)
	h.tick()
	if s, err := h.st.SessionByID(h.ctx, claude.ID); err != nil || s.State != store.SessionLost {
		t.Fatalf("claude-review session = %+v, %v: want lost", s, err)
	}
	if !slices.Contains(sh.panesOf(ws), deref(claude.HerdrPaneID)) || len(panesClosed(t, h, 2)) != 0 {
		t.Fatalf("under a round: panes %v: want the lost pane kept for the round", sh.panesOf(ws))
	}
	h.e.unreserve(pr.ID)

	judge, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "codex-judge")
	if err != nil {
		t.Fatal(err)
	}
	gone(deref(judge.AgentName))
	h.advance(time.Minute)
	h.tick()
	panes := sh.panesOf(ws)
	if slices.Contains(panes, deref(judge.HerdrPaneID)) || slices.Contains(panes, deref(claude.HerdrPaneID)) || !slices.Contains(panes, operator) {
		t.Fatalf("panes %v: want both lost panes closed, the operator's shell kept", panes)
	}
	got := panesClosed(t, h, 2)
	if len(got) != 1 || !slices.Equal(got[0].Roles, []string{"claude-review", "codex-judge"}) {
		t.Fatalf("%s events = %+v, want one for the two lost panes", agents.EventPanesClosed, got)
	}
	if id, err := h.d.Agents.ResumeID(h.ctx, pr.ID, "codex-judge"); err != nil || id == "" {
		t.Fatalf("the judge's resume id = %q, %v: want its lost conversation", id, err)
	}
}

// TestEveryParkThatWorksClosesItsPanes: the idle park, the release of a
// Codex-flagged PR, an eviction and `magnum abort` close the panes of the
// sessions they parked once the park worked (the fresh start already did),
// and a park that fails closes nothing.
func TestEveryParkThatWorksClosesItsPanes(t *testing.T) {
	closesAfterPark := func(h *harness, prID int64) bool {
		calls := h.ag.all()
		i := slices.Index(calls, fmt.Sprintf("park:%d", prID))
		return i >= 0 && slices.Contains(calls[i:], fmt.Sprintf("close_panes:%d", prID))
	}
	t.Run("idle park", func(t *testing.T) {
		h := newHarness(t)
		pr := h.reviewedPR(2, "b1")
		h.advance(2*time.Hour + time.Minute)
		h.tick()
		if h.parks(pr.ID) != 1 || !closesAfterPark(h, pr.ID) {
			t.Fatalf("calls %v: want the park, then the close", h.ag.all())
		}
	})
	t.Run("a park that fails", func(t *testing.T) {
		h := newHarness(t)
		pr := h.reviewedPR(2, "b1")
		h.failParks(fmt.Errorf("agents: park pr 2: claude-review is working: %w", agents.ErrBusy))
		h.advance(2*time.Hour + time.Minute)
		h.tick()
		if h.parks(pr.ID) != 1 || h.ag.count("close_panes:") != 0 {
			t.Fatalf("calls %v: want the park tried and nothing closed", h.ag.all())
		}
	})
	t.Run("Codex-flagged release", func(t *testing.T) {
		h := newHarness(t, noFallback) // a fallback judge would keep the PR's agents and slot
		h.refusedPR()
		pr := h.pr(2)
		h.advance(time.Minute)
		h.tick()
		if h.parks(pr.ID) != 1 || !closesAfterPark(h, pr.ID) {
			t.Fatalf("calls %v: want the park, then the close", h.ag.all())
		}
	})
	t.Run("eviction", func(t *testing.T) {
		h := newHarness(t)
		first := h.reviewedPR(2, "b1")
		h.advance(31 * time.Minute) // past min_warm
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
		h.tick()
		h.advance(5 * time.Minute)
		h.tick()
		if !slices.Contains(h.sl.all(), "release:review1:evicted") || !closesAfterPark(h, first.ID) {
			t.Fatalf("agents %v, slots %v: want the eviction's park, then the close", h.ag.all(), h.sl.all())
		}
	})
	t.Run("abort", func(t *testing.T) {
		h := newHarness(t)
		pr := h.runningRound(7, "h7", false)
		id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "7"}})
		h.tick()
		if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "sessions parked") {
			t.Fatalf("abort: %s %q", r.State, deref(r.Result))
		}
		if h.ag.count(fmt.Sprintf("park_hard:%d", pr.ID)) != 0 || h.ag.count(fmt.Sprintf("close_panes:%d", pr.ID)) != 1 || !closesAfterPark(h, pr.ID) {
			t.Fatalf("calls %v: want the first, plain park and then the close", h.ag.all())
		}
	})
}
