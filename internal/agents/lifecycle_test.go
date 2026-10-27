package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
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

// Quit stops a Claude agent the way the early park does: Claude 2.1.311
// ignored two ctrl+c 1 s apart at its idle prompt, so the quit before a
// checkout that changes AGENTS.md failed every attempt, while one ctrl+c
// and its exit keys quit it within seconds. The same look runs before each
// key: a composer that is not bare gets no key, and the session stays live
// with ErrBusy and no timeout. Quit closes no pane.
func TestQuitQuitsClaudeWithItsExitKeys(t *testing.T) {
	t.Run("they quit it", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		pane := ws.Panes[RoleClaude]
		e.atPrompt(pane)
		e.h.onKeys = deafToCtrlC(pane)
		claude := e.session(RoleClaude)
		if err := e.m.Quit(e.ctx, claude); err != nil {
			t.Fatal(err)
		}
		if got, want := e.keysTo(pane), [][]string{{"ctrl+c"}, e.exitKeys(KindClaude)}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("keys to the claude pane = %q, want %q: one ctrl+c, then the exit keys", got, want)
		}
		if !slices.Equal(e.slept, []time.Duration{time.Second}) {
			t.Fatalf("slept %v, want the pause between the ctrl+c and the exit keys", e.slept)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked || store.Deref(got.SessionID) != "claude-id" {
			t.Fatalf("claude session = %+v, want parked with its id", got)
		}
		if len(e.h.paneCloses) != 0 || len(e.h.closed) != 0 {
			t.Fatalf("pane closes %v, workspace closes %v: want none", e.h.paneCloses, e.h.closed)
		}
	})
	t.Run("a draft in the composer", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		pane := ws.Panes[RoleClaude]
		e.atPrompt(pane)
		e.h.reads[pane] = strings.Replace(claudeAtPrompt, "❯ \n", "❯ say banana\n", 1)
		e.h.onKeys = deafToCtrlC(pane)
		claude := e.session(RoleClaude)
		err := e.m.Quit(e.ctx, claude)
		if !errors.Is(err, ErrBusy) || herdr.IsTimeout(err) {
			t.Fatalf("err = %v, want ErrBusy without herdr's timeout", err)
		}
		if got := e.keysTo(pane); len(got) != 0 {
			t.Fatalf("keys to the claude pane = %q, want none", got)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionLive {
			t.Fatalf("state = %s, want still live", got.State)
		}
	})
	t.Run("no conversation named", func(t *testing.T) {
		e := newEnv(t)
		ws := e.started()
		pane := ws.Panes[RoleClaude]
		e.atPrompt(pane)
		e.h.onKeys = quitsOnCtrlC
		claude := e.session(RoleClaude)
		if err := e.m.Quit(e.ctx, claude); err != nil {
			t.Fatal(err)
		}
		if got, want := e.keysTo(pane), [][]string{{"ctrl+c"}, {"ctrl+c"}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("keys to the claude pane = %q, want %q: the look has no conversation to find", got, want)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionClosed {
			t.Fatalf("state = %s, want closed: nothing to resume", got.State)
		}
	})
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

// A park whose agent stays in its pane's foreground after two ctrl+c waits
// (ErrBusy, the herdr timeout kept). The hard park goes on: the kind's exit
// keys, a second wait, then that pane alone is closed, never the workspace
// (a shell no session owns shares it, as on talkable#11932), and its
// session is closed, never parked, with one pr.park_forced event that names
// the role, the pane and the cause, never the pane's text.
func TestAHardParkClosesThePaneOfAnAgentThatWillNotQuit(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	foreign := ws.WorkspaceID + ":p99"
	e.h.addPane(herdr.Pane{ID: foreign, WorkspaceID: ws.WorkspaceID})
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
	e.h.setAgentSession("mg-11920-claude-review-5d01cf", "claude-id")
	e.observe()
	stuck := ws.Panes[RoleClaude]
	e.h.busy[stuck] = true
	e.h.reads[stuck] = "SECRET-PANE-TEXT"
	claude := e.session(RoleClaude)
	started := len(e.h.callsWith("WaitIdleShell " + stuck)) // the agent's start waited for its shell

	err := e.m.Park(e.ctx, e.pr)
	if !errors.Is(err, ErrBusy) || !herdr.IsTimeout(err) {
		t.Fatalf("plain park: err = %v, want ErrBusy with the herdr timeout", err)
	}
	if got := e.keysTo(stuck); len(got) != 2 || len(e.h.paneCloses) != 0 {
		t.Fatalf("plain park: keys %q, pane closes %v: want two ctrl+c and no close", got, e.h.paneCloses)
	}

	if err := e.m.ParkHard(e.ctx, e.pr); err != nil {
		t.Fatalf("hard park: %v", err)
	}
	want := [][]string{{"ctrl+c"}, {"ctrl+c"}, {"ctrl+c"}, {"ctrl+c"}, {"/", "e", "x", "i", "t", "enter"}}
	if got := e.keysTo(stuck); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("keys to %s = %q, want %q", stuck, got, want)
	}
	if waits := len(e.h.callsWith("WaitIdleShell "+stuck)) - started; waits != 3 {
		t.Fatalf("waits for an idle shell = %d, want one plain and two hard", waits)
	}
	if !slices.Equal(e.h.paneCloses, []string{stuck}) || len(e.h.closed) != 0 {
		t.Fatalf("pane closes %v, workspace closes %v: want the stuck pane alone", e.h.paneCloses, e.h.closed)
	}
	snap, _ := e.h.Snapshot(e.ctx)
	if _, ok := paneIndex(snap)[foreign]; !ok {
		t.Fatal("the shell no session owns was closed")
	}
	got, _ := e.st.SessionByID(e.ctx, claude.ID)
	if got.State != store.SessionClosed || got.ClosedAt == nil {
		t.Fatalf("claude session = %s (closed_at %v), want closed", got.State, got.ClosedAt)
	}
	if judge, _ := e.st.SessionByID(e.ctx, e.sessions()[0].ID); judge.State != store.SessionParked {
		t.Fatalf("judge session = %s, want parked", judge.State)
	}
	evs := e.eventsOf(EventParkForced)
	if len(evs) != 1 || evs[0].Level != "info" {
		t.Fatalf("%s events = %+v, want one info", EventParkForced, evs)
	}
	var data map[string]string
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["role"] != "claude-review" || data["pane"] != stuck || data["cause"] != "no idle shell after ctrl+c and the exit keys; pane closed" {
		t.Fatalf("event data = %v", data)
	}
	if strings.Contains(evs[0].Message+string(evs[0].Data), "SECRET-PANE-TEXT") {
		t.Fatalf("the event carries the pane's text: %+v", evs[0])
	}
}

// A park whose quit of one agent fails (it stays in its pane's foreground
// after two ctrl+c) goes on to quit the PR's other agents and returns the
// failures joined, rather than stopping at the first: on talkable#11932 four
// idle sessions stayed live for a day because one Claude pane ignored
// ctrl+c. The stuck agent stays live for the next try, which the hard park
// settles; both agents stuck fail with both quits named.
func TestAParkThatCannotQuitOneAgentStillParksTheOthers(t *testing.T) {
	setup := func(t *testing.T) (*env, Workspace) {
		t.Helper()
		e := newEnv(t)
		ws := e.started()
		e.h.addPane(herdr.Pane{ID: ws.WorkspaceID + ":p99", WorkspaceID: ws.WorkspaceID}) // a shell no session owns: the workspace stays open
		e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
		e.h.setAgentSession("mg-11920-claude-review-5d01cf", "claude-id")
		e.observe()
		return e, ws
	}
	t.Run("the first agent is stuck", func(t *testing.T) {
		e, ws := setup(t)
		stuck := ws.Panes[RoleJudge] // the first session the park tries
		e.h.busy[stuck] = true
		judge, claude := e.session(RoleJudge), e.session(RoleClaude)

		err := e.m.Park(e.ctx, e.pr)
		if !errors.Is(err, ErrBusy) || !herdr.IsTimeout(err) || !strings.Contains(err.Error(), "quit session") {
			t.Fatalf("park: err = %v, want the stuck agent's quit failure, ErrBusy with the herdr timeout", err)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked || store.Deref(got.SessionID) != "claude-id" {
			t.Fatalf("claude session = %+v, want parked with its id after the judge's quit failed", got)
		}
		if got, _ := e.st.SessionByID(e.ctx, judge.ID); got.State != store.SessionLive {
			t.Fatalf("judge session = %s, want live for the next try", got.State)
		}
		if got := e.keysTo(ws.Panes[RoleClaude]); len(got) != 2 {
			t.Fatalf("keys to the claude pane = %q, want its two ctrl+c", got)
		}
		if err := e.m.ParkHard(e.ctx, e.pr); err != nil {
			t.Fatalf("hard park: %v", err)
		}
		if got, _ := e.st.SessionByID(e.ctx, judge.ID); got.State != store.SessionClosed {
			t.Fatalf("judge session after the hard park = %s, want closed", got.State)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked {
			t.Fatalf("claude session after the hard park = %s, want it still parked", got.State)
		}
	})
	t.Run("both agents are stuck", func(t *testing.T) {
		e, ws := setup(t)
		e.h.busy[ws.Panes[RoleJudge]] = true
		e.h.busy[ws.Panes[RoleClaude]] = true
		judge, claude := e.session(RoleJudge), e.session(RoleClaude)
		err := e.m.Park(e.ctx, e.pr)
		if !errors.Is(err, ErrBusy) || !herdr.IsTimeout(err) {
			t.Fatalf("park: err = %v, want ErrBusy with the herdr timeout", err)
		}
		for _, s := range []store.Session{judge, claude} {
			if !strings.Contains(err.Error(), fmt.Sprintf("quit session %d", s.ID)) {
				t.Fatalf("park: err = %v, want it to name the quit of session %d", err, s.ID)
			}
			if got, _ := e.st.SessionByID(e.ctx, s.ID); got.State != store.SessionLive {
				t.Fatalf("%s session = %s, want live", s.Role, got.State)
			}
		}
	})
	t.Run("a stop ends the park at once", func(t *testing.T) {
		e, ws := setup(t)
		e.h.busy[ws.Panes[RoleJudge]] = true
		ctx, cancel := context.WithCancel(e.ctx)
		e.h.onKeys = func(_ *fakeHerdr, _ string, _ []string) { cancel() }
		err := e.m.Park(ctx, e.pr)
		if err == nil {
			t.Fatal("park: want the stop's error")
		}
		if got := e.keysTo(ws.Panes[RoleClaude]); len(got) != 0 {
			t.Fatalf("keys to the claude pane = %q, want none after the stop", got)
		}
	})
}

// The hard park closes a pane only when the agent still runs after its exit
// keys: one the keys quit is parked as by a plain quit, nothing closed; a
// pane herdr refuses to close leaves the session live and the park busy; a
// run in flight refuses the hard park as the plain one, before any key; a
// kind without exit keys goes straight to the close.
func TestAHardParkClosesNothingTheExitKeysQuitAndWaitsWhenHerdrRefuses(t *testing.T) {
	setup := func(t *testing.T) (*env, string, store.Session) {
		t.Helper()
		e := newEnv(t)
		ws := e.started()
		e.h.addPane(herdr.Pane{ID: ws.WorkspaceID + ":p99", WorkspaceID: ws.WorkspaceID})
		e.h.setAgentSession("mg-11920-claude-review-5d01cf", "claude-id")
		e.observe()
		stuck := ws.Panes[RoleClaude]
		e.h.busy[stuck] = true
		claude := e.session(RoleClaude)
		return e, stuck, claude
	}
	t.Run("the exit keys quit it", func(t *testing.T) {
		e, stuck, claude := setup(t)
		e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
			if target == stuck && slices.Contains(keys, "enter") {
				f.busy[stuck] = false
			}
		}
		if err := e.m.ParkHard(e.ctx, e.pr); err != nil {
			t.Fatal(err)
		}
		got, _ := e.st.SessionByID(e.ctx, claude.ID)
		if got.State != store.SessionParked || len(e.h.paneCloses) != 0 || len(e.eventsOf(EventParkForced)) != 0 {
			t.Fatalf("session %s, pane closes %v: want parked, nothing closed, no event", got.State, e.h.paneCloses)
		}
	})
	t.Run("herdr refuses the close", func(t *testing.T) {
		e, stuck, claude := setup(t)
		e.h.errs["PaneClose"] = &herdr.Error{Method: "pane.close", Code: herdr.CodeInvalidRequest, Message: "no"}
		err := e.m.ParkHard(e.ctx, e.pr)
		if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "close pane "+stuck) {
			t.Fatalf("err = %v, want ErrBusy naming the close", err)
		}
		got, _ := e.st.SessionByID(e.ctx, claude.ID)
		if got.State != store.SessionLive || len(e.eventsOf(EventParkForced)) != 0 {
			t.Fatalf("session %s: want still live, no event", got.State)
		}
	})
	t.Run("a run in flight", func(t *testing.T) {
		e, stuck, claude := setup(t)
		run, err := e.m.NewRun(e.ctx, e.pr, RoleClaude, store.RunFix, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.m.ParkHard(e.ctx, e.pr); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "run "+run.ID) {
			t.Fatalf("err = %v, want the run's refusal", err)
		}
		if got := e.keysTo(stuck); len(got) != 0 || len(e.h.paneCloses) != 0 {
			t.Fatalf("keys %q, pane closes %v: want none while the run is in flight", got, e.h.paneCloses)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionLive {
			t.Fatalf("session = %s, want live", got.State)
		}
	})
	t.Run("a kind without exit keys", func(t *testing.T) {
		e, stuck, claude := setup(t)
		e.setKind(KindClaude, func(k *config.Kind) { k.ExitKeys = nil })
		if err := e.m.ParkHard(e.ctx, e.pr); err != nil {
			t.Fatal(err)
		}
		if got := e.keysTo(stuck); len(got) != 2 || !slices.Equal(e.h.paneCloses, []string{stuck}) {
			t.Fatalf("keys %q, pane closes %v: want two ctrl+c, then the close", got, e.h.paneCloses)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionClosed {
			t.Fatalf("session = %s, want closed", got.State)
		}
	})
}
