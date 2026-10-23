package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
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
	return e.staleRowWith(pr, role, state, pane, nil)
}

// staleRowWith is staleRow with edit applied to the row before it is
// recorded.
func (e *env) staleRowWith(pr store.PR, role Role, state, pane string, edit func(*store.Session)) store.Session {
	e.t.Helper()
	ws, _, _ := strings.Cut(pane, ":")
	name := AgentName("talkable/talkable", pr.Number, role)
	x := store.Session{PRID: pr.ID, Role: string(role), AgentName: &name, AgentKind: new(KindCodex),
		SessionID: new(fmt.Sprintf("uuid-%d-%s", pr.Number, role)), HerdrWorkspaceID: new(ws), HerdrTabID: new(ws + ":t1"),
		HerdrPaneID: new(pane), Cwd: new(e.slotOf(pr)), State: state}
	if edit != nil {
		edit(&x)
	}
	s, err := e.st.CreateSession(e.ctx, x)
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

// evalPane is a pane of an eval replay's workspace in the fake herdr: its
// id, its working directory and the name of the agent herdr lists there
// ("" = none, detected = a CLI herdr detected there and names not).
type evalPane struct{ id, cwd, agent string }

// detected is evalPane's agent herdr lists without a name.
const detected = "(detected)"

// The eval replay's checkout and the names of its agents (tagged "eval",
// so never the PR's own).
const evalCheckout = "/Users/x/Projects/talkable.eval/11932"

var evalJudge, evalReviewer = TaggedAgentName("eval", "talkable/talkable", 11932, RoleJudge),
	TaggedAgentName("eval", "talkable/talkable", 11932, RoleClaude)

// evalWorkspace is herdr's workspace ws of an eval replay of talkable#11932
// after a restart gave it ids that rows of the live registry still record:
// the replay keeps its sessions in a scratch registry, so no row read here
// records ws or its panes.
func (e *env) evalWorkspace(ws string, panes ...evalPane) {
	e.t.Helper()
	e.h.mu.Lock()
	e.h.workspaces = append(e.h.workspaces, herdr.Workspace{ID: ws, Label: "eval talkable#11932"})
	e.h.mu.Unlock()
	for _, p := range panes {
		e.h.addPane(herdr.Pane{ID: p.id, WorkspaceID: ws, TabID: ws + ":t1", Cwd: p.cwd})
		switch p.agent {
		case "":
		case detected:
			e.h.addAgent(herdr.AgentInfo{PaneID: p.id, WorkspaceID: ws, TabID: ws + ":t1", Agent: KindCodex, AgentStatus: herdr.StatusDone})
		default:
			e.h.addAgent(herdr.AgentInfo{PaneID: p.id, WorkspaceID: ws, TabID: ws + ":t1", Name: p.agent, Agent: KindCodex,
				AgentStatus: herdr.StatusIdle})
		}
	}
}

// setPaneCwd makes herdr report dir as pane's working directory.
func (f *fakeHerdr) setPaneCwd(pane, dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.panes {
		if f.panes[i].ID == pane {
			f.panes[i].Cwd = dir
		}
	}
}

// reusedReasons maps the reason of each EventHerdrIDsReused event of pr to
// the rows it names; each is at info, one per reason, and names no
// directory.
func (e *env) reusedReasons(pr store.PR) map[string][]int64 {
	e.t.Helper()
	out := map[string][]int64{}
	for _, ev := range e.reusedEvents(pr) {
		var data struct {
			Reason   string  `json:"reason"`
			Sessions []int64 `json:"sessions"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			e.t.Fatal(err)
		}
		if _, dup := out[data.Reason]; dup || ev.Level != "info" {
			e.t.Fatalf("%s event %+v: want one at info per reason", EventHerdrIDsReused, ev)
		}
		out[data.Reason] = data.Sessions
		for _, dir := range []string{evalCheckout, "/Users/x/notes", "/Users/x/Projects/talkable.review"} {
			if strings.Contains(ev.Message+string(ev.Data), dir) {
				e.t.Fatalf("the event names a directory (%s): %+v", dir, ev)
			}
		}
	}
	return out
}

// At 15:10:53Z on 2026-10-23 the live daemon set up talkable#12069 and
// closed pane 145, which held an eval replay's codex-review in the replay's
// own workspace: the replay keeps its sessions in a scratch registry, so no
// row the daemon reads records that pane, and after herdr's restart its id
// was one a retired row of the PR still recorded; a second later the replay
// lost its session. A retired row's pane is closed only while herdr shows it
// is still the session's: the agent there carries the row's agent name, or,
// with no agent, the pane works in the PR's checkout. One herdr shows with
// another agent, or as a shell elsewhere (the replay's, the operator's),
// stays; the row's herdr ids are dropped, with one event that names the
// ids, never the pane's directory. A CLI herdr detected but did not start
// is no agent magnum named: never the agent of a row with an agent name,
// and where a shell role's pane was, its pane must still work in the
// checkout.
func TestARetiredRowNeverClosesAPaneHerdrShowsAsSomeoneElses(t *testing.T) {
	shellRow := func(s *store.Session) { s.AgentName, s.AgentKind, s.SessionID = nil, new(KindShell), nil }
	for _, c := range []struct {
		name   string
		pane   evalPane
		state  string
		edit   func(*store.Session)
		reason string
	}{
		{"an eval agent", evalPane{"w7:p1", evalCheckout, evalReviewer}, store.SessionParked, nil, "another_agent"},
		{"the eval's codex-review shell", evalPane{"w7:p1", evalCheckout, ""}, store.SessionParked, nil, "outside_checkout"},
		{"the operator's shell", evalPane{"w7:p1", "/Users/x/notes", ""}, store.SessionLost, nil, "outside_checkout"},
		{"the operator's CLI in the checkout", evalPane{"w7:p1", "/Users/x/Projects/talkable.review1", detected}, store.SessionParked, nil, "another_agent"},
		{"the operator's CLI where a shell role's pane was", evalPane{"w7:p1", "/Users/x/notes", detected}, store.SessionClosed, shellRow, "outside_checkout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.h.restart()
			e.evalWorkspace("w7", c.pane)
			parked := e.staleRowWith(e.pr, RoleJudge, c.state, "w7:p1", c.edit)

			if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil {
				t.Fatalf("close retired panes: %v", err)
			}
			if len(e.h.paneCloses) != 0 || len(e.h.closed) != 0 {
				t.Fatalf("pane closes %v, workspace closes %v: want nothing closed", e.h.paneCloses, e.h.closed)
			}
			if !e.herdrIDsCleared(parked.ID) {
				t.Fatal("the retired row keeps herdr ids that are not its session's")
			}
			if got, err := e.st.SessionByID(e.ctx, parked.ID); err != nil || got.State != c.state || store.Deref(got.SessionID) != store.Deref(parked.SessionID) {
				t.Fatalf("retired row = %+v, %v: want it %s with its conversation", got, err, c.state)
			}
			if got := e.reusedReasons(e.pr); !maps.EqualFunc(got, map[string][]int64{c.reason: {parked.ID}}, slices.Equal) {
				t.Fatalf("%s events by reason = %v, want one %s naming row %d", EventHerdrIDsReused, got, c.reason, parked.ID)
			}
			// Settled: the next reconcile finds nothing of the PR there.
			if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil || len(e.h.paneCloses) != 0 || len(e.reusedEvents(e.pr)) != 1 {
				t.Fatalf("again: %v, pane closes %v: want nothing closed and no second event", err, e.h.paneCloses)
			}
		})
	}
}

// What herdr still shows as the session's is closed as before: a parked
// row's own shell in the PR's checkout (or below it), a lost row's pane
// that carries the session's agent, wherever it works, a shell role's pane
// where herdr detects the CLI its command ran (an agent herdr names none
// of, for a row with no agent name), and the shell of a row that records
// no cwd in the PR's slot.
func TestARetiredRowsPaneHerdrStillShowsAsTheSessionsCloses(t *testing.T) {
	for _, c := range []struct {
		name  string
		state string
		pane  evalPane
		edit  func(*store.Session)
	}{
		{"a parked row's shell in the checkout", store.SessionParked, evalPane{"w7:p1", "/Users/x/Projects/talkable.review1/app", ""}, nil},
		{"a lost row's agent", store.SessionLost, evalPane{"w7:p1", "/Users/x/elsewhere", AgentName("talkable/talkable", 11920, RoleJudge)}, nil},
		{"a shell role's detected CLI", store.SessionClosed, evalPane{"w7:p1", "/Users/x/Projects/talkable.review1", detected},
			func(s *store.Session) { s.AgentName, s.AgentKind, s.SessionID = nil, new(KindShell), nil }},
		{"a row without a cwd in the PR's slot", store.SessionParked, evalPane{"w7:p1", "/Users/x/Projects/talkable.review1", ""},
			func(s *store.Session) { s.Cwd = nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.h.restart()
			e.evalWorkspace("w7", c.pane) // labelled as the replay's: the label decides nothing
			if _, err := e.st.CreateSlot(e.ctx, store.Slot{Name: "review1", RepoFullName: "talkable/talkable", Kind: store.SlotKindPool,
				Path: e.slotOf(e.pr), State: store.SlotClaimed, PRID: &e.pr.ID}); err != nil {
				t.Fatal(err)
			}
			row := e.staleRowWith(e.pr, RoleJudge, c.state, "w7:p1", c.edit)
			if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil {
				t.Fatalf("close retired panes: %v", err)
			}
			if !slices.Equal(e.h.paneCloses, []string{"w7:p1"}) {
				t.Fatalf("pane closes %v, want the session's own pane", e.h.paneCloses)
			}
			if e.herdrIDsCleared(row.ID) || len(e.reusedEvents(e.pr)) != 0 {
				t.Fatalf("the row's ids dropped = %v, %d events: want neither", e.herdrIDsCleared(row.ID), len(e.reusedEvents(e.pr)))
			}
		})
	}
}

// Park closes a workspace of the PR's live and lost sessions, and the next
// round's workspace reuses one, only while every pane in it is still the
// PR's: after herdr's restart an eval replay's workspace took the ids of the
// PR's lost claude-review and codex-review rows, so Park would have closed
// the replay's workspace, and the next round would have split its panes
// there and started its agents in the replay's panes. Neither touches it
// now. When herdr shows the replay's agents in both panes, both rows drop
// their ids; when one pane is still the PR's own shell, its row keeps its
// ids (the retired-pane close takes that pane), and the workspace, which
// holds the replay's agent (or the operator's shell elsewhere) too, is
// neither closed nor reused; a row whose pane is gone drops its ids for the
// workspace it records.
func TestParkAndTheReuseRefuseAWorkspaceWithAPaneThatIsNotThePRs(t *testing.T) {
	own := evalPane{"w7:p1", "/Users/x/Projects/talkable.review1", ""}
	replays := evalPane{"w7:p2", evalCheckout, evalReviewer}
	for _, path := range []string{"park", "reuse"} {
		for _, c := range []struct {
			name       string
			p1, p2     evalPane // the workspace's panes
			claudePane string   // the pane the lost claude-review row records (codex-review's: w7:p2)
			dropped    func(claude, codex store.Session) []int64
		}{
			{"both panes the replay's", evalPane{"w7:p1", evalCheckout, evalJudge}, replays, "w7:p1",
				func(claude, codex store.Session) []int64 { return []int64{claude.ID, codex.ID} }},
			{"one pane still the PR's", own, replays, "w7:p1",
				func(_, codex store.Session) []int64 { return []int64{codex.ID} }},
			{"one pane the operator's shell", own, evalPane{"w7:p2", "/Users/x/notes", ""}, "w7:p1",
				func(_, codex store.Session) []int64 { return []int64{codex.ID} }},
			{"one row's pane gone", evalPane{"w7:p1", evalCheckout, evalJudge}, replays, "w7:p3",
				func(claude, codex store.Session) []int64 { return []int64{claude.ID, codex.ID} }},
		} {
			t.Run(path+"/"+c.name, func(t *testing.T) {
				e := newEnv(t)
				e.h.restart()
				e.evalWorkspace("w7", c.p1, c.p2)
				claude := e.staleRow(e.pr, RoleClaude, store.SessionLost, c.claudePane)
				codex := e.staleRow(e.pr, RoleCodexReview, store.SessionLost, "w7:p2")

				switch path {
				case "park":
					if err := e.m.Park(e.ctx, e.pr); err != nil {
						t.Fatalf("park: %v", err)
					}
				case "reuse":
					ws := e.workspace()
					if ws.WorkspaceID == "w7" || !ws.Created {
						t.Fatalf("workspace %+v, want a new one, not the replay's w7", ws)
					}
					for r, p := range ws.Panes {
						if strings.HasPrefix(p, "w7:") {
							t.Fatalf("%s took the replay's pane %s", r, p)
						}
					}
					for _, s := range e.h.splits {
						if strings.HasPrefix(s.From, "w7:") {
							t.Fatalf("a split from the replay's pane %s", s.From)
						}
					}
				}
				if len(e.h.paneCloses) != 0 || len(e.h.closed) != 0 {
					t.Fatalf("pane closes %v, workspace closes %v: want nothing of the replay's closed", e.h.paneCloses, e.h.closed)
				}
				want := c.dropped(claude, codex)
				for _, s := range []store.Session{claude, codex} {
					if e.herdrIDsCleared(s.ID) != slices.Contains(want, s.ID) {
						t.Fatalf("%s row's ids cleared = %v, want %v", s.Role, e.herdrIDsCleared(s.ID), slices.Contains(want, s.ID))
					}
					if got, _ := e.st.SessionByID(e.ctx, s.ID); got.State != store.SessionLost || store.Deref(got.SessionID) == "" {
						t.Fatalf("%s row = %+v, want it still lost with its conversation", s.Role, got)
					}
				}
				var named []int64
				for _, ids := range e.reusedReasons(e.pr) {
					named = append(named, ids...)
				}
				if slices.Sort(named); !slices.Equal(named, want) {
					t.Fatalf("%s events name rows %v, want %v", EventHerdrIDsReused, named, want)
				}
			})
		}
	}
}

// Live rows herdr restarted under, before the observer marked them lost,
// record what is an eval replay's workspace now. Park never closes it (a
// live row keeps its ids: every pane in it is the PR's by the rows, but
// none is by herdr) and retires the rows, whose agents are gone; the next
// retired-pane close then drops their ids and closes nothing.
func TestParkNeverClosesAWorkspaceItsLiveRowsRecordWhereHerdrShowsAReplay(t *testing.T) {
	e := newEnv(t)
	e.h.restart()
	e.evalWorkspace("w7", evalPane{"w7:p1", evalCheckout, evalJudge}, evalPane{"w7:p2", evalCheckout, evalReviewer})
	judge := e.staleRow(e.pr, RoleJudge, store.SessionLive, "w7:p1")
	claude := e.staleRow(e.pr, RoleClaude, store.SessionLive, "w7:p2")

	if err := e.m.Park(e.ctx, e.pr); err != nil {
		t.Fatalf("park: %v", err)
	}
	if len(e.h.paneCloses) != 0 || len(e.h.closed) != 0 || len(e.h.keys) != 0 {
		t.Fatalf("pane closes %v, workspace closes %v, keys %+v: want nothing of the replay's touched", e.h.paneCloses, e.h.closed, e.h.keys)
	}
	for _, s := range []store.Session{judge, claude} {
		if got, _ := e.st.SessionByID(e.ctx, s.ID); got.State != store.SessionParked || e.herdrIDsCleared(s.ID) {
			t.Fatalf("%s row = %+v, want it parked, its ids kept for the retired-pane close", s.Role, got)
		}
	}
	if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil || len(e.h.paneCloses) != 0 || len(e.h.closed) != 0 {
		t.Fatalf("close retired panes: %v, pane closes %v, workspace closes %v: want nothing closed", err, e.h.paneCloses, e.h.closed)
	}
	if got := e.reusedReasons(e.pr); !maps.EqualFunc(got, map[string][]int64{"another_agent": {judge.ID, claude.ID}}, slices.Equal) {
		t.Fatalf("%s events by reason = %v, want both rows as another_agent", EventHerdrIDsReused, got)
	}
}

// A live session's pane may sit in another workspace (StartAgent adopted a
// conversation herdr restored there), and the reused workspace keeps it for
// the role; once herdr shows another agent there (an eval replay's, after a
// restart), the row is marked lost and the role gets a pane of its own in
// the PR's workspace.
func TestAReusedWorkspaceKeepsNoLivePaneHerdrShowsAsSomeoneElses(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	judge := e.session(RoleJudge)
	e.evalWorkspace("w7", evalPane{"w7:p1", evalCheckout, evalJudge})
	if err := e.st.TransitionSession(e.ctx, judge.ID, nil, store.SessionLive, func(u *store.SessionUpdate) {
		u.Set("herdr_pane_id", "w7:p1")
		u.Set("herdr_workspace_id", "w7")
	}); err != nil {
		t.Fatal(err)
	}

	again := e.workspace()
	if again.WorkspaceID != ws.WorkspaceID || again.Created {
		t.Fatalf("workspace %+v, want the PR's %s reused", again, ws.WorkspaceID)
	}
	if p := again.Panes[RoleJudge]; p == "w7:p1" || !strings.HasPrefix(p, ws.WorkspaceID+":") {
		t.Fatalf("the judge's pane %s, want a new one in %s, not the replay's", p, ws.WorkspaceID)
	}
	if got, _ := e.st.SessionByID(e.ctx, judge.ID); got.State != store.SessionLost {
		t.Fatalf("the judge's old row = %s, want lost", got.State)
	}
	if len(e.h.paneCloses) != 0 || len(e.h.closed) != 0 {
		t.Fatalf("pane closes %v, workspace closes %v: want none", e.h.paneCloses, e.h.closed)
	}
}

// The early park closes the session's own pane once its agent quit, by the
// id its row records: after herdr's restart, before the observer marked the
// row lost, that id named an operator's shell elsewhere, which the close
// would have taken. The pane stays (the event's note says so, without its
// directory) and the session is parked all the same.
func TestAnEarlyParkLeavesARecordedPaneHerdrShowsAsSomeoneElses(t *testing.T) {
	e, ws := parkSessionsEnv(t)
	pane := ws.Panes[RoleClaude]
	e.h.removeAgent(claudeAgent) // herdr restarted: the agent is gone, and its pane id names the operator's shell
	e.h.setPaneCwd(pane, "/Users/x/notes")
	claude := e.session(RoleClaude)

	parked, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
	if err != nil || len(parked) != 1 {
		t.Fatalf("park sessions = %+v, %v: want claude-review parked", parked, err)
	}
	if len(e.h.paneCloses) != 0 || len(e.h.closed) != 0 {
		t.Fatalf("pane closes %v, workspace closes %v: want the operator's shell left", e.h.paneCloses, e.h.closed)
	}
	if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked {
		t.Fatalf("claude session = %s, want parked", got.State)
	}
	evs := e.eventsOf(EventSessionParked)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "stays") || strings.Contains(evs[0].Message, "/Users/x/notes") {
		t.Fatalf("%s events = %+v, want one that says the pane stays, without its directory", EventSessionParked, evs)
	}
}

// A park quits an agent by keys to the pane its row records. herdr
// restarted, restored claude-review's conversation under its name in a new
// pane before the observer moved the row there, and gave the row's old pane
// id to an eval replay's agent: neither the plain nor the hard park sends
// that pane a key or closes it or its workspace (ErrBusy, the session live:
// the observer moves the row to its agent's pane at its next tick, and the
// next park quits it there); the judge, in its own pane, is parked.
func TestAParkSendsNoKeyToARecordedPaneThatCarriesAnotherAgent(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(fmt.Sprintf("hard=%v", hard), func(t *testing.T) {
			e := newEnv(t)
			ws := e.started()
			e.h.setAgentSession(judgeAgent, "judge-uuid")
			e.h.setAgentSession(claudeAgent, "claude-id")
			e.observe()
			old := ws.Panes[RoleClaude]
			e.h.removeAgent(claudeAgent)
			e.h.addPane(herdr.Pane{ID: "w9:p1", WorkspaceID: "w9", TabID: "w9:t1", Cwd: "/Users/x/Projects/talkable.review1"})
			e.h.addAgent(herdr.AgentInfo{PaneID: "w9:p1", WorkspaceID: "w9", Name: claudeAgent, Agent: KindClaude, AgentStatus: herdr.StatusIdle})
			e.h.addAgent(herdr.AgentInfo{PaneID: old, WorkspaceID: ws.WorkspaceID, Name: evalReviewer, Agent: KindClaude, AgentStatus: herdr.StatusIdle,
				AgentSession: &herdr.AgentSession{Kind: "id", Value: "eval-conversation"}})
			e.h.busy[old] = true
			claude, judge := e.session(RoleClaude), e.session(RoleJudge)

			park := e.m.Park
			if hard {
				park = e.m.ParkHard
			}
			if err := park(e.ctx, e.pr); !errors.Is(err, ErrBusy) {
				t.Fatalf("park: err = %v, want ErrBusy", err)
			}
			if got := e.keysTo(old); len(got) != 0 {
				t.Fatalf("keys to the replay's pane %s = %q, want none", old, got)
			}
			if slices.Contains(e.h.paneCloses, old) || len(e.h.closed) != 0 {
				t.Fatalf("pane closes %v, workspace closes %v: want the eval agent's pane %s left", e.h.paneCloses, e.h.closed, old)
			}
			if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionLive || store.Deref(got.SessionID) != "claude-id" {
				t.Fatalf("claude session = %s (conversation %q), want live with its own conversation", got.State, store.Deref(got.SessionID))
			}
			if got, _ := e.st.SessionByID(e.ctx, judge.ID); got.State != store.SessionParked {
				t.Fatalf("judge session = %s, want parked", got.State)
			}
			if evs := e.eventsOf(EventParkForced); len(evs) != 0 {
				t.Fatalf("%s events = %+v, want none", EventParkForced, evs)
			}
			if len(e.logs.records("no key sent")) != 1 {
				t.Fatalf("logs %q, want one line that says no key was sent", e.logs.all())
			}
		})
	}
}

// A quit by keys to a pane the row records whose session's agent herdr
// lists nowhere, the pane being the operator's shell elsewhere now (herdr
// restarted), sends it no key: the session's agent is gone, which counts as
// quit, so the session is parked, and the retired-pane close then drops its
// ids and closes nothing.
func TestAQuitSendsNoKeyToARecordedPaneHerdrGaveToSomeoneElse(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.setAgentSession(claudeAgent, "claude-id")
	e.observe()
	pane := ws.Panes[RoleClaude]
	e.h.removeAgent(claudeAgent)
	e.h.setPaneCwd(pane, "/Users/x/notes")
	e.h.busy[pane] = true // the operator runs something there
	claude := e.session(RoleClaude)

	if err := e.m.Quit(e.ctx, claude); err != nil {
		t.Fatalf("quit: %v", err)
	}
	if got := e.keysTo(pane); len(got) != 0 {
		t.Fatalf("keys to the operator's pane %s = %q, want none", pane, got)
	}
	if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked || store.Deref(got.SessionID) != "claude-id" {
		t.Fatalf("claude session = %+v, want parked with its conversation", got)
	}
	if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil || len(e.h.paneCloses) != 0 {
		t.Fatalf("close retired panes: %v, pane closes %v: want none", err, e.h.paneCloses)
	}
	if got := e.reusedReasons(e.pr); !maps.EqualFunc(got, map[string][]int64{"outside_checkout": {claude.ID}}, slices.Equal) {
		t.Fatalf("%s events by reason = %v, want claude-review's row as outside_checkout", EventHerdrIDsReused, got)
	}
}

// A hard park looks at the pane again before its exit keys and its close:
// herdr restarted while the stuck agent ignored its two ctrl+c, restored
// its conversation under its name in a new pane, and the old pane id names
// an eval replay's agent now. Neither the exit keys nor the close reach it
// (ErrBusy, the session live).
func TestAHardParkForcesNothingOnAPaneHerdrGaveAwayDuringItsQuit(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.addPane(herdr.Pane{ID: ws.WorkspaceID + ":p99", WorkspaceID: ws.WorkspaceID}) // a shell no session owns keeps the workspace
	e.h.setAgentSession(judgeAgent, "judge-uuid")
	e.h.setAgentSession(claudeAgent, "claude-id")
	e.observe()
	stuck := ws.Panes[RoleClaude]
	e.h.busy[stuck] = true
	ctrlCs := 0
	e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
		if target == stuck && slices.Contains(keys, "ctrl+c") {
			if ctrlCs++; ctrlCs == 2 { // herdr restarts: the id is the replay's agent's now
				for i := range f.agents {
					if f.agents[i].PaneID == stuck {
						f.agents[i].Name = evalReviewer
					}
				}
				f.panes = append(f.panes, herdr.Pane{ID: "w9:p1", WorkspaceID: "w9", Cwd: "/Users/x/Projects/talkable.review1"})
				f.agents = append(f.agents, herdr.AgentInfo{PaneID: "w9:p1", WorkspaceID: "w9", Name: claudeAgent, Agent: KindClaude,
					AgentStatus: herdr.StatusIdle})
			}
		}
	}
	claude := e.session(RoleClaude)

	if err := e.m.ParkHard(e.ctx, e.pr); !errors.Is(err, ErrBusy) {
		t.Fatalf("hard park: err = %v, want ErrBusy", err)
	}
	if got := e.keysTo(stuck); !slices.EqualFunc(got, [][]string{{"ctrl+c"}, {"ctrl+c"}}, slices.Equal) {
		t.Fatalf("keys to %s = %q, want its two ctrl+c and no exit keys", stuck, got)
	}
	if slices.Contains(e.h.paneCloses, stuck) || len(e.h.closed) != 0 {
		t.Fatalf("pane closes %v, workspace closes %v: want the replay's pane %s left", e.h.paneCloses, e.h.closed, stuck)
	}
	if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionLive {
		t.Fatalf("claude session = %s, want live", got.State)
	}
	if evs := e.eventsOf(EventParkForced); len(evs) != 0 {
		t.Fatalf("%s events = %+v, want none", EventParkForced, evs)
	}
}
