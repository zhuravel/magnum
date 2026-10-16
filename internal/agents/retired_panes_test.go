package agents

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// A fresh start closes the panes of the sessions its park retired, so a
// parked session may have no pane: its conversation is kept, Recover leaves
// it parked, and the role's next start resumes it (resume {session}) in a
// new pane, as it resumes a lost one.
func TestAParkedSessionWithoutAPaneResumesInANewPane(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	foreign := ws.WorkspaceID + ":p99"
	e.h.addPane(herdr.Pane{ID: foreign, WorkspaceID: ws.WorkspaceID})
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
	e.h.setAgentSession("mg-11920-claude-review-5d01cf", "claude-id")
	if err := e.m.Park(e.ctx, e.pr); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil {
		t.Fatalf("close retired panes: %v", err)
	}
	old := []string{ws.Panes[RoleJudge], ws.Panes[RoleClaude], ws.Panes[RoleCodexReview]}
	if got := slices.Sorted(slices.Values(e.h.paneCloses)); !slices.Equal(got, slices.Sorted(slices.Values(old))) {
		t.Fatalf("pane closes %v, want the three retired panes %v", got, old)
	}
	snap, _ := e.h.Snapshot(e.ctx)
	if _, ok := paneIndex(snap)[foreign]; !ok {
		t.Fatal("the shell no session owns was closed")
	}
	evs := e.eventsOf(EventPanesClosed)
	if len(evs) != 1 || evs[0].Level != "info" {
		t.Fatalf("%s events = %+v, want one info", EventPanesClosed, evs)
	}
	var data struct {
		Count int      `json:"count"`
		Roles []string `json:"roles"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Count != 3 || !slices.Equal(data.Roles, []string{"claude-review", "codex-judge", "codex-review"}) {
		t.Fatalf("event data = %+v", data)
	}

	if rec, err := e.m.Recover(e.ctx, e.pr); err != nil || len(rec) != 0 {
		t.Fatalf("recover = %+v, %v: want nothing restored, nothing lost", rec, err)
	}
	resume, err := e.m.ResumeID(e.ctx, e.pr.ID, RoleJudge)
	if err != nil || resume != "judge-uuid" {
		t.Fatalf("resume id = %q, %v", resume, err)
	}
	again := e.workspace()
	pane := again.Panes[RoleJudge]
	if slices.Contains(old, pane) {
		t.Fatalf("the judge's pane %s is a closed one", pane)
	}
	starts := len(e.h.starts)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), pane, resume); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(e.h.starts) != starts+1 || e.h.starts[starts].PaneID != pane || !slices.Contains(e.h.starts[starts].Args, "judge-uuid") {
		t.Fatalf("starts = %+v, want the judge resumed in %s", e.h.starts[starts:], pane)
	}
	if s := e.session(RoleJudge); s.State != store.SessionLive || store.Deref(s.ResumedFrom) != "judge-uuid" || store.Deref(s.HerdrPaneID) != pane {
		t.Fatalf("judge session = %+v", s)
	}
}

// The panes CloseRetiredPanes closes are only the retired sessions' own
// idle shells: a pane a starting or live session still records (a new
// session of the judge starting in its old pane), one that runs something
// and one no session records stay.
func TestClosingRetiredPanesKeepsWhatIsStillInUse(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
	e.h.setAgentSession("mg-11920-claude-review-5d01cf", "claude-id")
	foreign := ws.WorkspaceID + ":p99"
	e.h.addPane(herdr.Pane{ID: foreign, WorkspaceID: ws.WorkspaceID})
	if err := e.m.Park(e.ctx, e.pr); err != nil {
		t.Fatalf("park: %v", err)
	}
	// The claude-review pane runs the operator's build; a starting session
	// of the judge records the judge's pane.
	e.h.busy[ws.Panes[RoleClaude]] = true
	name := AgentName("talkable/talkable", 11920, RoleJudge)
	if _, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: string(RoleJudge), AgentName: &name, AgentKind: new(KindCodex),
		HerdrPaneID: new(ws.Panes[RoleJudge]), HerdrWorkspaceID: new(ws.WorkspaceID), State: store.SessionStarting}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil {
		t.Fatalf("close retired panes: %v", err)
	}
	if !slices.Equal(e.h.paneCloses, []string{ws.Panes[RoleCodexReview]}) {
		t.Fatalf("pane closes %v, want the codex-review pane alone", e.h.paneCloses)
	}
	if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil || len(e.h.paneCloses) != 1 || len(e.eventsOf(EventPanesClosed)) != 1 {
		t.Fatalf("again: %v, pane closes %v: want nothing more closed and no second event", err, e.h.paneCloses)
	}
}

// A lost session's pane is closed like a parked one's (spec 58): its agent
// is gone, the pane shows a shell with "Resume this session with: …", and
// the next workspace splits a pane of its own when it does not reuse it;
// talkable#11932 kept 9 lost sessions' panes beside 19 parked ones. A pane
// id a lost session recorded that herdr now shows in another workspace is
// not that session's pane and stays. When the closes leave a workspace with
// no pane at all, the workspace is closed too, so the next start never
// reuses a workspace it cannot split a pane in.
func TestALostSessionsPaneIsClosed(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	foreign := ws.WorkspaceID + ":p99"
	e.h.addPane(herdr.Pane{ID: foreign, WorkspaceID: ws.WorkspaceID})
	e.h.removeAgent("mg-11920-codex-judge-5d01cf") // the judge's agent exited: a shell again
	e.clock.Add(LostGrace + time.Second)
	if obs := e.observe(); obs[RoleJudge].Kind != ObsLost {
		t.Fatalf("judge = %+v, want lost", obs[RoleJudge])
	}
	e.h.addWorkspace("w9")
	e.h.addPane(herdr.Pane{ID: "w9:p1", WorkspaceID: "w9"}) // the operator's, with an id an old session recorded
	name := AgentName("talkable/talkable", 11920, RoleClaude)
	if _, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: string(RoleClaude), AgentName: &name, AgentKind: new(KindClaude),
		HerdrPaneID: new("w9:p1"), HerdrWorkspaceID: new(ws.WorkspaceID), State: store.SessionLost}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil {
		t.Fatalf("close retired panes: %v", err)
	}
	if !slices.Equal(e.h.paneCloses, []string{ws.Panes[RoleJudge]}) || len(e.h.closed) != 0 {
		t.Fatalf("pane closes %v, workspace closes %v: want the lost judge's pane alone", e.h.paneCloses, e.h.closed)
	}
	if s := e.sessions()[0]; s.Role != string(RoleJudge) || s.State != store.SessionLost {
		t.Fatalf("judge session = %+v, want it still lost (resumable)", s)
	}

	// Every session lost, and no pane but theirs: the workspace goes with
	// its last pane, and the next one is laid out anew.
	for _, s := range e.sessions() {
		if err := e.st.TransitionSession(e.ctx, s.ID, nil, store.SessionLost, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.h.removePane(foreign)
	e.h.removeAgent("mg-11920-claude-review-5d01cf")
	if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil {
		t.Fatalf("close retired panes: %v", err)
	}
	want := []string{ws.Panes[RoleJudge], ws.Panes[RoleClaude], ws.Panes[RoleCodexReview]}
	if got := slices.Sorted(slices.Values(e.h.paneCloses)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("pane closes %v, want every pane of %s %v", got, ws.WorkspaceID, want)
	}
	if !slices.Equal(e.h.closed, []string{ws.WorkspaceID}) {
		t.Fatalf("workspace closes %v, want the emptied %s", e.h.closed, ws.WorkspaceID)
	}
	again := e.workspace()
	if again.WorkspaceID == ws.WorkspaceID || !again.Created {
		t.Fatalf("next workspace = %+v, want a new one", again)
	}
	snap, _ := e.h.Snapshot(e.ctx)
	if _, ok := paneIndex(snap)["w9:p1"]; !ok {
		t.Fatal("the other workspace's pane was closed")
	}
}

// The release's park (cleanup's Park: `magnum release`, the default cleanup
// of a merged PR) closes the panes it retired, as every park that works
// does (spec 58); a park that fails closes nothing.
func TestTheReleaseParkClosesItsPanes(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.addPane(herdr.Pane{ID: ws.WorkspaceID + ":p99", WorkspaceID: ws.WorkspaceID})
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
	e.h.setAgentSession("mg-11920-claude-review-5d01cf", "claude-id")
	e.h.setAgentStatus("mg-11920-codex-judge-5d01cf", herdr.StatusWorking)
	if err := e.m.ParkClosingPanes(e.ctx, e.pr); !errors.Is(err, ErrBusy) || len(e.h.paneCloses) != 0 {
		t.Fatalf("busy park = %v, pane closes %v: want ErrBusy and nothing closed", err, e.h.paneCloses)
	}
	e.h.setAgentStatus("mg-11920-codex-judge-5d01cf", herdr.StatusIdle)
	if err := e.m.ParkClosingPanes(e.ctx, e.pr); err != nil {
		t.Fatalf("park: %v", err)
	}
	old := []string{ws.Panes[RoleJudge], ws.Panes[RoleClaude], ws.Panes[RoleCodexReview]}
	if got := slices.Sorted(slices.Values(e.h.paneCloses)); !slices.Equal(got, slices.Sorted(slices.Values(old))) {
		t.Fatalf("pane closes %v, want the three retired panes %v", got, old)
	}
	if s := e.sessions()[0]; s.State != store.SessionParked {
		t.Fatalf("judge session = %+v, want parked", s)
	}
}
