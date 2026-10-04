package agents

import (
	"errors"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

func TestEnsureWorkspaceCreatesThreePanes(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()

	if !ws.Created || ws.WorkspaceID != "w1" || ws.TabID != "w1:t1" {
		t.Fatalf("workspace = %+v", ws)
	}
	if got := sortedKeys(ws.Panes); len(got) != 3 {
		t.Fatalf("panes = %v, want codex-judge, claude-review, codex-review", got)
	}
	if ws.Panes[RoleJudge] != "w1:p1" || ws.Panes[RoleClaude] != "w1:p2" || ws.Panes[RoleCodexReview] != "w1:p3" {
		t.Fatalf("panes = %v", ws.Panes)
	}
	c := e.h.creates[0]
	if c.Cwd != "/Users/x/Projects/talkable.review1" || c.Label != "talkable#11920" || c.Focus || c.Env["WT_BRANCH"] != "review1" {
		t.Fatalf("WorkspaceCreate options = %+v", c)
	}
	if len(e.h.splits) != 2 {
		t.Fatalf("splits = %+v", e.h.splits)
	}
	if s := e.h.splits[0]; s.From != "w1:p1" || s.Opts.Direction != herdr.SplitRight || s.Opts.Focus || s.Opts.Cwd != c.Cwd || s.Opts.Env["WT_BRANCH"] != "review1" {
		t.Fatalf("first split = %+v", s)
	}
	if s := e.h.splits[1]; s.From != "w1:p2" || s.Opts.Direction != herdr.SplitDown {
		t.Fatalf("second split = %+v", s)
	}
	wantLabels := map[string]string{"w1:p1": "PR #11920 codex-judge", "w1:p2": "PR #11920 claude-review", "w1:p3": "PR #11920 codex-review"}
	for p, l := range wantLabels {
		if e.h.renames[p] != l {
			t.Errorf("rename %s = %q, want %q", p, e.h.renames[p], l)
		}
	}

	judge := e.session(RoleJudge)
	if judge.State != store.SessionStarting || store.Deref(judge.HerdrPaneID) != "w1:p1" || store.Deref(judge.HerdrWorkspaceID) != "w1" ||
		store.Deref(judge.AgentName) != "mg-11920-codex-judge-5d01cf" || store.Deref(judge.AgentKind) != KindCodex ||
		store.Deref(judge.Cwd) != c.Cwd || judge.Env["WT_BRANCH"] != "review1" {
		t.Fatalf("judge session = %+v", judge)
	}
	claude := e.session(RoleClaude)
	if store.Deref(claude.AgentName) != "mg-11920-claude-review-5d01cf" || store.Deref(claude.AgentKind) != KindClaude {
		t.Fatalf("claude session = %+v", claude)
	}
	shell := e.session(RoleCodexReview)
	if shell.AgentName != nil || store.Deref(shell.AgentKind) != KindShell || store.Deref(shell.HerdrPaneID) != "w1:p3" {
		t.Fatalf("codex-review session = %+v", shell)
	}
}

func TestEnsureWorkspaceReusesLiveWorkspace(t *testing.T) {
	e := newEnv(t)
	first := e.workspace()
	second := e.workspace()
	if second.Created || second.WorkspaceID != first.WorkspaceID {
		t.Fatalf("second = %+v, want reuse of %s", second, first.WorkspaceID)
	}
	for r, p := range first.Panes {
		if second.Panes[r] != p {
			t.Errorf("pane %s = %s, want %s", r, second.Panes[r], p)
		}
	}
	if len(e.h.creates) != 1 || len(e.h.splits) != 2 {
		t.Fatalf("creates=%d splits=%d, want 1 and 2", len(e.h.creates), len(e.h.splits))
	}
	if n := len(e.sessions()); n != 3 {
		t.Fatalf("sessions = %d, want 3 (no duplicates)", n)
	}
}

func TestEnsureWorkspaceRepairsMissingPane(t *testing.T) {
	e := newEnv(t)
	first := e.workspace()
	e.h.removePane(first.Panes[RoleClaude])

	ws := e.workspace()
	if ws.Created || ws.WorkspaceID != first.WorkspaceID {
		t.Fatalf("ws = %+v", ws)
	}
	if ws.Panes[RoleClaude] == first.Panes[RoleClaude] || ws.Panes[RoleClaude] == "" {
		t.Fatalf("claude pane = %q, want a new pane", ws.Panes[RoleClaude])
	}
	last := e.h.splits[len(e.h.splits)-1]
	if last.From != first.Panes[RoleJudge] || last.Opts.Direction != herdr.SplitRight {
		t.Fatalf("repair split = %+v, want right of judge", last)
	}
	if got := store.Deref(e.session(RoleClaude).HerdrPaneID); got != ws.Panes[RoleClaude] {
		t.Fatalf("claude session pane = %s", got)
	}
	lost := 0
	for _, s := range e.sessions() {
		if s.Role == string(RoleClaude) && s.State == store.SessionLost {
			lost++
		}
	}
	if lost != 1 {
		t.Fatalf("lost claude sessions = %d, want 1", lost)
	}
}

func TestEnsureWorkspaceRecreatesWhenWorkspaceGone(t *testing.T) {
	e := newEnv(t)
	first := e.started()
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "01a0fe66-uuid")
	if id, err := e.m.RecordSessionID(e.ctx, e.session(RoleJudge)); err != nil || id != "01a0fe66-uuid" {
		t.Fatalf("RecordSessionID = %q, %v", id, err)
	}
	e.h.removeWorkspace(first.WorkspaceID)

	ws := e.workspace()
	if !ws.Created || ws.WorkspaceID == first.WorkspaceID {
		t.Fatalf("ws = %+v, want a new workspace", ws)
	}
	var lostJudge *store.Session
	for _, s := range e.sessions() {
		if s.Role == string(RoleJudge) && s.State == store.SessionLost {
			s := s
			lostJudge = &s
		}
	}
	if lostJudge == nil || store.Deref(lostJudge.SessionID) != "01a0fe66-uuid" {
		t.Fatalf("old judge session should be lost and keep its id: %+v", lostJudge)
	}
	id, err := e.m.ResumeID(e.ctx, e.pr.ID, RoleJudge)
	if err != nil || id != "01a0fe66-uuid" {
		t.Fatalf("ResumeID = %q, %v", id, err)
	}
	if e.session(RoleJudge).State != store.SessionStarting {
		t.Fatal("new judge session should be starting")
	}
}

func TestEnsureWorkspaceMovedSlotParksOldWorkspace(t *testing.T) {
	e := newEnv(t)
	first := e.started()
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
	if _, err := e.m.RecordSessionID(e.ctx, e.session(RoleJudge)); err != nil {
		t.Fatal(err)
	}

	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review4", map[string]string{"WT_BRANCH": "review4"}, "talkable#11920", e.coreRoles())
	if err != nil {
		t.Fatal(err)
	}
	if !ws.Created || ws.MovedFrom != "/Users/x/Projects/talkable.review1" {
		t.Fatalf("ws = %+v", ws)
	}
	if len(e.h.closed) != 1 || e.h.closed[0] != first.WorkspaceID {
		t.Fatalf("closed = %v, want old workspace closed", e.h.closed)
	}
	if id, _ := e.m.ResumeID(e.ctx, e.pr.ID, RoleJudge); id != "judge-uuid" {
		t.Fatalf("ResumeID = %q, want parked judge id", id)
	}
}

func TestEnsureWorkspaceMovedSlotBusy(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.h.setAgentStatus("mg-11920-codex-judge-5d01cf", herdr.StatusWorking)
	_, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review4", nil, "talkable#11920", e.coreRoles())
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

func TestEnsurePaneSimplify(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	ws2, err := e.m.EnsurePane(e.ctx, e.pr, ws, "/Users/x/Projects/talkable.review1", map[string]string{"WT_BRANCH": "review1"}, e.spec(RoleSimplify))
	if err != nil {
		t.Fatal(err)
	}
	p := ws2.Panes[RoleSimplify]
	last := e.h.splits[len(e.h.splits)-1]
	if p == "" || last.From != ws.Panes[RoleJudge] || last.Opts.Direction != herdr.SplitDown || last.New != p {
		t.Fatalf("simplify pane %q from split %+v", p, last)
	}
	if e.h.renames[p] != "PR #11920 claude-simplify" {
		t.Fatalf("rename = %q", e.h.renames[p])
	}
	s := e.session(RoleSimplify)
	if store.Deref(s.AgentName) != "mg-11920-claude-simplify-5d01cf" || store.Deref(s.AgentKind) != KindClaude {
		t.Fatalf("simplify session = %+v", s)
	}
	// Idempotent.
	ws3, err := e.m.EnsurePane(e.ctx, e.pr, ws2, "/Users/x/Projects/talkable.review1", nil, e.spec(RoleSimplify))
	if err != nil || ws3.Panes[RoleSimplify] != p || len(e.h.splits) != 3 {
		t.Fatalf("second EnsurePane = %v, %v (splits %d)", ws3.Panes, err, len(e.h.splits))
	}
	if _, ok := ws.Panes[RoleSimplify]; ok || len(ws.Roles) != 3 {
		t.Fatalf("EnsurePane modified its input: %+v", ws)
	}
}

func TestEnsureWorkspaceHerdrDown(t *testing.T) {
	e := newEnv(t)
	e.h.errs["Snapshot"] = herdr.ErrUnavailable
	_, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/x", nil, "l", e.coreRoles())
	if !errors.Is(err, herdr.ErrUnavailable) {
		t.Fatalf("err = %v, want herdr.ErrUnavailable", err)
	}
}

func TestEnsureWorkspaceMovedSlotClosesWorkspaceOfLostSessions(t *testing.T) {
	e := newEnv(t)
	first := e.started()
	// Both agents exited (their panes remain) and the shell pane is gone:
	// Observe marks every session lost, none is live.
	e.h.removeAgent("mg-11920-codex-judge-5d01cf")
	e.h.removeAgent("mg-11920-claude-review-5d01cf")
	e.h.removePane(first.Panes[RoleCodexReview])
	e.clock.Add(LostGrace + time.Second)
	e.observe()
	for _, s := range e.sessions() {
		if s.State != store.SessionLost {
			t.Fatalf("session %s = %s, want lost", s.Role, s.State)
		}
	}
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review4", nil, "talkable#11920", e.coreRoles())
	if err != nil {
		t.Fatal(err)
	}
	if !ws.Created || ws.MovedFrom != "/Users/x/Projects/talkable.review1" {
		t.Fatalf("ws = %+v", ws)
	}
	if len(e.h.closed) != 1 || e.h.closed[0] != first.WorkspaceID {
		t.Fatalf("closed = %v, want the old workspace closed", e.h.closed)
	}
}
