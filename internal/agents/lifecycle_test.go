package agents

import (
	"errors"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

func TestQuitSendsTwoCtrlCAndParks(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
	if _, err := e.m.RecordSessionID(e.ctx, e.session(RoleJudge)); err != nil {
		t.Fatal(err)
	}
	s := e.session(RoleJudge)
	if err := e.m.Quit(e.ctx, s); err != nil {
		t.Fatal(err)
	}
	if len(e.h.keys) != 2 || e.h.keys[0].Target != ws.Panes[RoleJudge] || e.h.keys[0].Keys[0] != "ctrl+c" || e.h.keys[1].Keys[0] != "ctrl+c" {
		t.Fatalf("keys = %+v", e.h.keys)
	}
	if len(e.slept) != 1 || e.slept[0] != time.Second {
		t.Fatalf("slept = %v, want one 1s pause between the keys", e.slept)
	}
	got, _ := e.st.SessionByID(e.ctx, s.ID)
	if got.State != store.SessionParked || store.Deref(got.SessionID) != "judge-uuid" {
		t.Fatalf("session = %+v, want parked with id", got)
	}
	// Without a session id there is nothing to resume: closed.
	c := e.session(RoleClaude)
	if err := e.m.Quit(e.ctx, c); err != nil {
		t.Fatal(err)
	}
	got, _ = e.st.SessionByID(e.ctx, c.ID)
	if got.State != store.SessionClosed || got.ClosedAt == nil {
		t.Fatalf("claude session = %+v, want closed", got)
	}
}

func TestQuitAgentStillRunning(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.busy[ws.Panes[RoleJudge]] = true
	s := e.session(RoleJudge)
	if err := e.m.Quit(e.ctx, s); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if got, _ := e.st.SessionByID(e.ctx, s.ID); got.State != store.SessionLive {
		t.Fatalf("state = %s, want still live", got.State)
	}
}

func TestParkClosesIdleWorkspace(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
	e.h.setAgentSession("mg-11920-claude-review-5d01cf", "claude-id")
	e.observe() // records session ids
	if err := e.m.Park(e.ctx, e.pr); err != nil {
		t.Fatal(err)
	}
	if len(e.h.closed) != 1 || e.h.closed[0] != ws.WorkspaceID {
		t.Fatalf("closed = %v", e.h.closed)
	}
	states := map[string]string{}
	for _, s := range e.sessions() {
		states[s.Role] = s.State
	}
	if states["codex-judge"] != store.SessionParked || states["claude-review"] != store.SessionParked || states["codex-review"] != store.SessionClosed {
		t.Fatalf("states = %v", states)
	}
	if id, _ := e.m.ResumeID(e.ctx, e.pr.ID, RoleClaude); id != "claude-id" {
		t.Fatalf("ResumeID claude = %q", id)
	}
	// Parking again is a no-op.
	if err := e.m.Park(e.ctx, e.pr); err != nil {
		t.Fatal(err)
	}
}

func TestParkRefusesBusy(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.setAgentStatus("mg-11920-claude-review-5d01cf", herdr.StatusWorking)
	if err := e.m.Park(e.ctx, e.pr); !errors.Is(err, ErrBusy) {
		t.Fatalf("working agent: err = %v, want ErrBusy", err)
	}
	e.h.setAgentStatus("mg-11920-claude-review-5d01cf", herdr.StatusIdle)
	e.h.busy[ws.Panes[RoleCodexReview]] = true
	if err := e.m.Park(e.ctx, e.pr); !errors.Is(err, ErrBusy) {
		t.Fatalf("running codex review: err = %v, want ErrBusy", err)
	}
	e.h.busy[ws.Panes[RoleCodexReview]] = false
	run, _ := e.m.NewRun(e.ctx, e.pr, RoleJudge, store.RunInitial, 1)
	if err := e.m.Park(e.ctx, e.pr); !errors.Is(err, ErrBusy) {
		t.Fatalf("run in flight (%s): err = %v, want ErrBusy", run.ID, err)
	}
	if len(e.h.closed) != 0 {
		t.Fatal("busy park must not close the workspace")
	}
}

func TestParkKeepsWorkspaceWithForeignPanes(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.addPane(herdr.Pane{ID: ws.WorkspaceID + ":p99", WorkspaceID: ws.WorkspaceID}) // the user split a pane here
	if err := e.m.Park(e.ctx, e.pr); err != nil {
		t.Fatal(err)
	}
	if len(e.h.closed) != 0 {
		t.Fatalf("workspace with a foreign pane must not be closed: %v", e.h.closed)
	}
	if len(e.h.keys) != 4 {
		t.Fatalf("keys = %+v, want two ctrl+c per agent (judge, claude)", e.h.keys)
	}
	for _, s := range e.sessions() {
		if s.State == store.SessionLive || s.State == store.SessionStarting {
			t.Fatalf("session %s still %s", s.Role, s.State)
		}
	}
}

func TestRecoverRebindsAndMarksLost(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
	e.observe()
	// herdr restarted: the judge's conversation now lives in a new pane, the
	// claude pane is gone without a session id, the shell pane survived.
	e.h.removePane(ws.Panes[RoleJudge])
	e.h.removePane(ws.Panes[RoleClaude])
	e.h.addPane(herdr.Pane{ID: ws.WorkspaceID + ":p7", WorkspaceID: ws.WorkspaceID, TabID: ws.WorkspaceID + ":t1"})
	e.h.addAgent(herdr.AgentInfo{PaneID: ws.WorkspaceID + ":p7", WorkspaceID: ws.WorkspaceID, Agent: "codex",
		AgentStatus: herdr.StatusIdle, AgentSession: &herdr.AgentSession{Kind: "id", Value: "judge-uuid"}})

	rec, err := e.m.Recover(e.ctx, e.pr)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[Role]RecoverAction{}
	for _, r := range rec {
		actions[r.Role] = r.Action
	}
	if actions[RoleJudge] != RecoverRebound || actions[RoleClaude] != RecoverLost || actions[RoleCodexReview] != RecoverOK {
		t.Fatalf("actions = %v", actions)
	}
	j := e.session(RoleJudge)
	if store.Deref(j.HerdrPaneID) != ws.WorkspaceID+":p7" || j.State != store.SessionLive {
		t.Fatalf("judge = %+v", j)
	}
	if len(e.h.agentRen) != 1 || e.h.agentRen[0] != ws.WorkspaceID+":p7=mg-11920-codex-judge-5d01cf" {
		t.Fatalf("agent renames = %v", e.h.agentRen)
	}
}

func TestRecoverRestoresParkedSession(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
	e.observe()
	if err := e.m.Park(e.ctx, e.pr); err != nil {
		t.Fatal(err)
	}
	// herdr brought the parked conversation back on its own.
	e.h.addWorkspace("w5")
	e.h.addPane(herdr.Pane{ID: "w5:p1", WorkspaceID: "w5", TabID: "w5:t1"})
	e.h.addAgent(herdr.AgentInfo{PaneID: "w5:p1", WorkspaceID: "w5", Agent: "codex", AgentStatus: herdr.StatusIdle,
		AgentSession: &herdr.AgentSession{Kind: "id", Value: "judge-uuid"}})
	rec, err := e.m.Recover(e.ctx, e.pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec) != 1 || rec[0].Action != RecoverRestored || rec[0].Role != RoleJudge {
		t.Fatalf("recovered = %+v", rec)
	}
	j := e.session(RoleJudge)
	if store.Deref(j.HerdrPaneID) != "w5:p1" || j.State != store.SessionLive {
		t.Fatalf("judge = %+v", j)
	}
}
