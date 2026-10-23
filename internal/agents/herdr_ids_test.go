package agents

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// restart is herdr's restart: every workspace, pane and agent is gone, and
// herdr numbers new workspaces and panes from the start again.
func (f *fakeHerdr) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaces, f.panes, f.agents = nil, nil, nil
	f.nextWS, f.nextPane = 0, map[string]int{}
}

// otherPR registers PR n of the env's repository.
func (e *env) otherPR(n int) store.PR {
	e.t.Helper()
	up, err := e.st.UpsertPRFromGitHub(e.ctx, store.GitHubPR{RepoID: e.repo.ID, NodeID: fmt.Sprintf("PR_%d", n), Number: n,
		URL: fmt.Sprintf("https://github.com/talkable/talkable/pull/%d", n), HeadSHA: "def5678abc1234def5678abc1234def5678abc1",
		Title: new("Ignored title"), GHState: store.GHOpen, InitialState: store.PRReviewing, Identity: "talkable-app"})
	if err != nil {
		e.t.Fatalf("UpsertPRFromGitHub: %v", err)
	}
	return up.PR
}

// slotOf is PR pr's checkout: e.workspace's for the env's PR.
func (e *env) slotOf(pr store.PR) string {
	if pr.ID == e.pr.ID {
		return "/Users/x/Projects/talkable.review1"
	}
	return fmt.Sprintf("/Users/x/Projects/talkable.review%d", pr.Number)
}

// workspaceOf lays out PR pr's workspace in its own checkout with roles:
// starting sessions in new panes, whose shells are idle until their agents
// start.
func (e *env) workspaceOf(pr store.PR, roles ...config.Role) Workspace {
	e.t.Helper()
	ws, err := e.m.EnsureWorkspace(e.ctx, pr, e.slotOf(pr), map[string]string{"MAGNUM_PR_URL": pr.URL}, fmt.Sprintf("talkable#%d", pr.Number), roles)
	if err != nil {
		e.t.Fatalf("EnsureWorkspace %d: %v", pr.Number, err)
	}
	return ws
}

// staleRow records a session of PR pr in state at herdr ids that a herdr
// restart has since given (or will give) to another PR.
func (e *env) staleRow(pr store.PR, role Role, state, pane string) store.Session {
	e.t.Helper()
	ws, _, _ := strings.Cut(pane, ":")
	name := AgentName("talkable/talkable", pr.Number, role)
	s, err := e.st.CreateSession(e.ctx, store.Session{PRID: pr.ID, Role: string(role), AgentName: &name, AgentKind: new(KindCodex),
		SessionID: new(fmt.Sprintf("uuid-%d-%s", pr.Number, role)), HerdrWorkspaceID: new(ws), HerdrTabID: new(ws + ":t1"),
		HerdrPaneID: new(pane), Cwd: new(e.slotOf(pr)), State: state})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// herdrIDsCleared reports whether session id records no herdr ids any more.
func (e *env) herdrIDsCleared(id int64) bool {
	e.t.Helper()
	s, err := e.st.SessionByID(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return s.HerdrPaneID == nil && s.HerdrWorkspaceID == nil && s.HerdrTabID == nil
}

func (e *env) reusedEvents(pr store.PR) []store.Event {
	e.t.Helper()
	all, err := e.st.EventsBySubject(e.ctx, fmt.Sprintf("pr:talkable/talkable#%d", pr.Number), 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.DeleteFunc(all, func(ev store.Event) bool { return ev.Kind != EventHerdrIDsReused })
}

func (e *env) sessionsOf(pr store.PR) []store.Session {
	e.t.Helper()
	ss, err := e.st.SessionsByPR(e.ctx, pr.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return ss
}

func paneIDs(ws Workspace) []string {
	out := make([]string, 0, len(ws.Panes))
	for _, p := range ws.Panes {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// herdr restarted at 08:37:24Z on 2026-10-23 and numbered new workspaces
// and panes from the start again, so the ids of PR A's parked, lost and
// closed sessions of the day before named the new workspace and panes of PR
// B's starting sessions: A's retired-pane close, its park and its next
// workspace closed or took B's (22 of the 23 agent.lost events since). None
// of the three touches what another PR's starting or live session records:
// A's stale rows drop their herdr ids (one event), and nothing of B's
// closes or is reused.
func TestAStaleRowsHerdrIDsThatAnotherPRUsesNowAreNeverClosedOrReused(t *testing.T) {
	for _, path := range []string{"retire", "park", "reuse"} {
		t.Run(path, func(t *testing.T) {
			e := newEnv(t)
			a := e.pr
			b := e.otherPR(11921)
			e.h.restart()
			wsB := e.workspaceOf(b, e.coreRoles()...) // w1: w1:p1, w1:p2, w1:p3, all starting
			if wsB.WorkspaceID != "w1" {
				t.Fatalf("B's workspace %s, want herdr's first id again", wsB.WorkspaceID)
			}
			parked := e.staleRow(a, RoleJudge, store.SessionParked, wsB.Panes[RoleJudge])
			lost := e.staleRow(a, RoleClaude, store.SessionLost, wsB.Panes[RoleClaude])
			closed := e.staleRow(a, RoleCodexReview, store.SessionClosed, wsB.Panes[RoleCodexReview])
			splits, starts := len(e.h.splits), len(e.h.starts)

			switch path {
			case "retire":
				if err := e.m.CloseRetiredPanes(e.ctx, a); err != nil {
					t.Fatalf("close retired panes: %v", err)
				}
			case "park":
				if err := e.m.Park(e.ctx, a); err != nil {
					t.Fatalf("park: %v", err)
				}
			case "reuse":
				ws := e.workspace() // A's next round
				if ws.WorkspaceID == wsB.WorkspaceID || !ws.Created {
					t.Fatalf("A's workspace %+v, want a new one, not B's %s", ws, wsB.WorkspaceID)
				}
				for r, p := range ws.Panes {
					if slices.Contains(paneIDs(wsB), p) {
						t.Fatalf("A's %s took B's pane %s", r, p)
					}
				}
			}
			if len(e.h.paneCloses) != 0 || len(e.h.closed) != 0 {
				t.Fatalf("pane closes %v, workspace closes %v: want nothing of B's closed", e.h.paneCloses, e.h.closed)
			}
			for _, c := range e.h.splits[splits:] {
				if slices.Contains(paneIDs(wsB), c.From) {
					t.Fatalf("a split from B's pane %s", c.From)
				}
			}
			for _, o := range e.h.starts[starts:] {
				if slices.Contains(paneIDs(wsB), o.PaneID) {
					t.Fatalf("an agent started in B's pane %s", o.PaneID)
				}
			}
			snap, _ := e.h.Snapshot(e.ctx)
			for r, p := range wsB.Panes {
				if _, ok := paneIndex(snap)[p]; !ok {
					t.Fatalf("B's %s pane %s is gone", r, p)
				}
			}
			for _, s := range []store.Session{parked, lost, closed} {
				if !e.herdrIDsCleared(s.ID) {
					t.Fatalf("A's stale %s row %d keeps B's herdr ids", s.State, s.ID)
				}
			}
			if got, err := e.st.SessionByID(e.ctx, lost.ID); err != nil || got.State != store.SessionLost || store.Deref(got.SessionID) == "" {
				t.Fatalf("A's lost row = %+v, %v: want it still lost, its conversation kept", got, err)
			}
			evs := e.reusedEvents(a)
			if len(evs) != 1 || evs[0].Level != "info" {
				t.Fatalf("%s events %+v, want one info", EventHerdrIDsReused, evs)
			}
			var data struct {
				Sessions []int64 `json:"sessions"`
			}
			if err := json.Unmarshal(evs[0].Data, &data); err != nil || !slices.Equal(data.Sessions, []int64{parked.ID, lost.ID, closed.ID}) {
				t.Fatalf("event data %s (%v), want sessions %d, %d and %d", evs[0].Data, err, parked.ID, lost.ID, closed.ID)
			}
			for _, s := range e.sessionsOf(b) {
				if s.State != store.SessionStarting {
					t.Fatalf("B's session %+v, want it still starting", s)
				}
			}
		})
	}
}

// At 13:26:13Z the retired-pane closes of two PRs (4 panes and 3) took the
// three new workspaces of three other PRs, whose sessions were starting in
// idle shells: three setups failed and eleven sessions were lost. Over one
// snapshot, as the engine's reconcile closes them, nothing new closes.
func TestTwoPRsOldRowsCloseNoneOfTheNewWorkspacesHerdrNumberedAgain(t *testing.T) {
	e := newEnv(t)
	old1, old2 := e.pr, e.otherPR(11930)
	news := []store.PR{e.otherPR(67), e.otherPR(66), e.otherPR(12080)}
	e.h.restart()
	var wss []Workspace
	for i, pr := range news {
		roles := e.coreRoles()
		if i > 0 {
			roles = e.specs(RoleJudge, RoleClaude)
		}
		wss = append(wss, e.workspaceOf(pr, roles...))
	}
	var stale []store.Session
	for _, row := range []struct {
		pr    store.PR
		role  Role
		state string
		pane  string
	}{
		{old1, RoleJudge, store.SessionParked, wss[0].Panes[RoleJudge]},
		{old1, RoleClaude, store.SessionLost, wss[0].Panes[RoleClaude]},
		{old1, RoleCodexReview, store.SessionClosed, wss[0].Panes[RoleCodexReview]},
		{old1, RoleJudge, store.SessionParked, wss[1].Panes[RoleJudge]},
		{old2, RoleJudge, store.SessionParked, wss[1].Panes[RoleClaude]},
		{old2, RoleClaude, store.SessionLost, wss[2].Panes[RoleJudge]},
		{old2, RoleCodexReview, store.SessionParked, wss[2].Panes[RoleClaude]},
	} {
		stale = append(stale, e.staleRow(row.pr, row.role, row.state, row.pane))
	}
	snap, err := e.h.Snapshot(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range []store.PR{old1, old2} {
		if err := e.m.CloseRetiredPanesIn(e.ctx, pr, snap); err != nil {
			t.Fatalf("close the retired panes of %d: %v", pr.Number, err)
		}
	}
	if len(e.h.paneCloses) != 0 || len(e.h.closed) != 0 {
		t.Fatalf("pane closes %v, workspace closes %v: want none of the new workspaces' panes closed", e.h.paneCloses, e.h.closed)
	}
	for _, s := range stale {
		if !e.herdrIDsCleared(s.ID) {
			t.Fatalf("stale row %d keeps its herdr ids", s.ID)
		}
	}
	if n1, n2 := len(e.reusedEvents(old1)), len(e.reusedEvents(old2)); n1 != 1 || n2 != 1 {
		t.Fatalf("%s events = %d and %d, want one per old PR", EventHerdrIDsReused, n1, n2)
	}
	// The old rows are settled: the next reconcile has nothing of theirs to look at.
	prs, err := e.st.PRsWithPanes(e.ctx, []string{wss[0].Panes[RoleJudge], wss[1].Panes[RoleClaude], wss[2].Panes[RoleJudge]})
	if err != nil || slices.Contains(prs, old1.ID) || slices.Contains(prs, old2.ID) {
		t.Fatalf("PRs with the new panes = %v, %v: want none of the old ones", prs, err)
	}
}

// The newer of two live rows that record one pane owns it: an older live row
// of another PR (herdr restarted before the observer marked it lost) does
// not keep the PR from reusing its own workspace or closing its own retired
// panes, a live row never loses its ids, and the older row's park closes
// nothing of the newer's.
func TestAnOlderLiveRowOfAnotherPRLeavesTheNewerOwnerItsPanes(t *testing.T) {
	e := newEnv(t)
	a := e.pr
	b := e.otherPR(11921)
	e.h.restart()
	// A's row from before the restart, still live: herdr's first pane id.
	old := e.staleRow(a, RoleJudge, store.SessionLive, "w1:p1")
	ws := e.workspaceOf(b, e.coreRoles()...)
	if ws.Panes[RoleJudge] != "w1:p1" {
		t.Fatalf("B's judge pane %s, want herdr's first id again", ws.Panes[RoleJudge])
	}
	again := e.workspaceOf(b, e.coreRoles()...)
	if again.WorkspaceID != ws.WorkspaceID || again.Created || again.Panes[RoleJudge] != ws.Panes[RoleJudge] {
		t.Fatalf("B's workspace %+v, want its own %+v reused", again, ws)
	}
	// B's codex-review retires; its pane is B's to close.
	for _, s := range e.sessionsOf(b) {
		if s.Role == string(RoleCodexReview) {
			if err := e.st.TransitionSession(e.ctx, s.ID, nil, store.SessionParked, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.m.CloseRetiredPanes(e.ctx, b); err != nil {
		t.Fatalf("close retired panes: %v", err)
	}
	if !slices.Equal(e.h.paneCloses, []string{ws.Panes[RoleCodexReview]}) {
		t.Fatalf("pane closes %v, want B's retired codex-review pane", e.h.paneCloses)
	}
	if e.herdrIDsCleared(old.ID) {
		t.Fatal("a live row lost its herdr ids")
	}
	if err := e.m.Park(e.ctx, a); err != nil {
		t.Fatalf("park A: %v", err)
	}
	if len(e.h.closed) != 0 || len(e.h.paneCloses) != 1 {
		t.Fatalf("workspace closes %v, pane closes %v after A's park: want B's left alone", e.h.closed, e.h.paneCloses)
	}
}
