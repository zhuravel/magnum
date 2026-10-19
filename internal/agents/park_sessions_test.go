package agents

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// quitsOnCtrlC makes a ctrl+c quit the agent of the pane it reaches: herdr
// no longer lists it (onKeys runs with the fake's lock held).
func quitsOnCtrlC(f *fakeHerdr, target string, keys []string) {
	if slices.Contains(keys, "ctrl+c") {
		f.agents = slices.DeleteFunc(f.agents, func(a herdr.AgentInfo) bool { return a.PaneID == target })
	}
}

// parkSessionsEnv is a PR whose judge and claude-review run with recorded
// session ids, beside the codex-review shell.
func parkSessionsEnv(t *testing.T) (*env, Workspace) {
	t.Helper()
	e := newEnv(t)
	ws := e.started()
	e.h.setAgentSession(judgeAgent, "judge-uuid")
	e.h.setAgentSession(claudeAgent, "claude-id")
	e.observe() // records the session ids
	return e, ws
}

// parkEvents are the kinds of the PR's early-park events, in order.
func (e *env) parkEvents() []string {
	e.t.Helper()
	evs, err := e.st.EventsBySubject(e.ctx, "pr:talkable/talkable#11920", 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if strings.HasPrefix(ev.Kind, "pr.session_park") {
			out = append(out, ev.Kind)
		}
	}
	return out
}

// The early park of the idle park parks the sessions it is given alone: the
// selected agent gets its two ctrl+c, becomes parked with its conversation
// once its pane is an idle shell, between a begin and an ok event, and only
// its pane is closed by the retired-pane close; the judge, the codex-review
// shell and the workspace stay.
func TestParkSessionsParksTheSelectedAgentAlone(t *testing.T) {
	e, ws := parkSessionsEnv(t)
	e.h.onKeys = quitsOnCtrlC
	claude, judge, shell := e.session(RoleClaude), e.session(RoleJudge), e.session(RoleCodexReview)

	parked, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
	if err != nil || len(parked) != 1 || parked[0].ID != claude.ID {
		t.Fatalf("park sessions = %+v, %v: want claude-review parked", parked, err)
	}
	if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked || store.Deref(got.SessionID) != "claude-id" {
		t.Fatalf("claude session = %+v, want parked with its id", got)
	}
	for _, s := range []store.Session{judge, shell} {
		if got, _ := e.st.SessionByID(e.ctx, s.ID); got.State != s.State {
			t.Fatalf("%s session = %s, want it still %s", s.Role, got.State, s.State)
		}
	}
	if got := e.keysTo(ws.Panes[RoleClaude]); len(got) != 2 {
		t.Fatalf("keys to the claude pane = %q, want its two ctrl+c", got)
	}
	if got := e.keysTo(ws.Panes[RoleJudge]); len(got) != 0 {
		t.Fatalf("keys to the judge pane = %q, want none", got)
	}
	if !slices.Equal(e.h.paneCloses, []string{ws.Panes[RoleClaude]}) || len(e.h.closed) != 0 {
		t.Fatalf("pane closes %v, workspace closes %v: want the claude pane alone", e.h.paneCloses, e.h.closed)
	}
	if got := e.parkEvents(); !slices.Equal(got, []string{EventSessionParkBegin, EventSessionParked}) {
		t.Fatalf("early-park events = %v, want its begin and ok", got)
	}
	if id, _ := e.m.ResumeID(e.ctx, e.pr.ID, RoleClaude); id != "claude-id" {
		t.Fatalf("ResumeID claude = %q, want claude-id", id)
	}
	if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil {
		t.Fatalf("close retired panes: %v", err)
	}
	if len(e.h.paneCloses) != 1 {
		t.Fatalf("pane closes %v, want nothing left for the retired-pane close", e.h.paneCloses)
	}
	// A session that is no longer live is skipped.
	if parked, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID}); err != nil || len(parked) != 0 {
		t.Fatalf("park a parked session again = %+v, %v: want nothing", parked, err)
	}
	if got := e.keysTo(ws.Panes[RoleClaude]); len(got) != 2 {
		t.Fatalf("keys to the claude pane = %q, want no more", got)
	}
}

// The early park keeps Park's guards: a run of the PR in flight, a
// selected agent at work or blocked, a selected session without a
// conversation to resume and the pane of an unlisted agent that runs a
// command are refused before any key and any event; an agent that does not
// quit stays live with ErrBusy, never escalated to the hard mode.
func TestParkSessionsRefusesWhatParkRefuses(t *testing.T) {
	refused := func(t *testing.T, e *env, s store.Session, err error) {
		t.Helper()
		if len(e.h.keys) != 0 {
			t.Fatalf("keys = %+v, want none", e.h.keys)
		}
		if got, _ := e.st.SessionByID(e.ctx, s.ID); got.State != store.SessionLive {
			t.Fatalf("state = %s, want live", got.State)
		}
		if got := e.parkEvents(); len(got) != 0 {
			t.Fatalf("early-park events %v for a refusal", got)
		}
	}
	t.Run("a run in flight", func(t *testing.T) {
		e, _ := parkSessionsEnv(t)
		if _, err := e.m.NewRun(e.ctx, e.pr, RoleJudge, store.RunInitial, 1); err != nil {
			t.Fatal(err)
		}
		claude := e.session(RoleClaude)
		_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("err = %v, want ErrBusy", err)
		}
		refused(t, e, claude, err)
	})
	for _, status := range []herdr.Status{herdr.StatusWorking, herdr.StatusBlocked} {
		t.Run("an agent "+string(status), func(t *testing.T) {
			e, _ := parkSessionsEnv(t)
			e.h.setAgentStatus(claudeAgent, status)
			claude := e.session(RoleClaude)
			_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
			if !errors.Is(err, ErrBusy) {
				t.Fatalf("err = %v, want ErrBusy", err)
			}
			refused(t, e, claude, err)
		})
	}
	t.Run("no conversation to resume", func(t *testing.T) {
		e := newEnv(t)
		e.started() // no session ids
		e.h.onKeys = quitsOnCtrlC
		claude := e.session(RoleClaude)
		_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
		if err == nil || errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "no conversation to resume") {
			t.Fatalf("err = %v, want the refusal of a session without a conversation", err)
		}
		refused(t, e, claude, err)
	})
	t.Run("no agent listed, a pane that runs a command", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		e.h.removeAgent(claudeAgent)
		e.h.busy[ws.Panes[RoleClaude]] = true
		claude := e.session(RoleClaude)
		_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
		if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "runs a command") {
			t.Fatalf("err = %v, want ErrBusy for the pane that runs a command", err)
		}
		refused(t, e, claude, err)
	})
	t.Run("no agent listed, an idle shell", func(t *testing.T) {
		e, _ := parkSessionsEnv(t)
		e.h.removeAgent(claudeAgent)
		claude := e.session(RoleClaude)
		if _, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID}); err != nil {
			t.Fatalf("park sessions: %v", err)
		}
		if len(e.h.keys) != 0 {
			t.Fatalf("keys = %+v, want none: the agent is gone", e.h.keys)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked {
			t.Fatalf("state = %s, want parked", got.State)
		}
	})
	t.Run("an agent that does not quit", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		e.h.busy[ws.Panes[RoleClaude]] = true
		claude := e.session(RoleClaude)
		_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
		if !errors.Is(err, ErrBusy) || !herdr.IsTimeout(err) {
			t.Fatalf("err = %v, want ErrBusy with herdr's timeout", err)
		}
		if got := e.keysTo(ws.Panes[RoleClaude]); len(got) != 2 {
			t.Fatalf("keys to the claude pane = %q, want the two ctrl+c alone (no exit keys)", got)
		}
		if len(e.h.paneCloses) != 0 {
			t.Fatalf("pane closes %v, want none: no hard park", e.h.paneCloses)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionLive {
			t.Fatalf("state = %s, want live", got.State)
		}
		if got := e.parkEvents(); !slices.Equal(got, []string{EventSessionParkBegin, EventSessionParkFailed}) {
			t.Fatalf("early-park events = %v, want its begin and fail", got)
		}
	})
}

// The early park ends as the PR's park ends, which closes the workspace
// before it retires the sessions: once the quit reached an idle shell the
// session's own pane is closed (herdr drops the agent with it, whether or
// not it still listed it) and only then is the session parked, so no
// parked row is left whose pane herdr still shows with the quit
// conversation, which a restart's Recover would restore as live. A stop
// right after the quit cannot cut between the close and the retire. A pane
// herdr will not close leaves the session parked all the same, its pane for
// a later CloseRetiredPanes.
func TestAnEarlyParkClosesThePaneBeforeItParks(t *testing.T) {
	t.Run("herdr still lists the quit agent", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		claude := e.session(RoleClaude)
		var stateAtClose string
		e.h.onPaneClose = func(pane string) {
			if got, err := e.st.SessionByID(e.ctx, claude.ID); err == nil && pane == ws.Panes[RoleClaude] {
				stateAtClose = got.State
			}
		}
		parked, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
		if err != nil || len(parked) != 1 {
			t.Fatalf("park sessions = %+v, %v: want claude-review parked", parked, err)
		}
		if stateAtClose != store.SessionLive {
			t.Fatalf("session at its pane's close = %q, want live: the pane closes before the retire", stateAtClose)
		}
		if len(e.slept) != 1 {
			t.Fatalf("slept %v, want only the pause between the two ctrl+c: no polling", e.slept)
		}
		if !slices.Equal(e.h.paneCloses, []string{ws.Panes[RoleClaude]}) {
			t.Fatalf("pane closes %v, want the session's own pane", e.h.paneCloses)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked {
			t.Fatalf("claude session = %s, want parked", got.State)
		}
		snap, _ := e.h.Snapshot(e.ctx)
		if _, listed := snap.AgentByName(claudeAgent); listed {
			t.Fatal("herdr still lists the quit agent after its pane closed")
		}
		// A restart's recovery finds no pane to restore the parked session into.
		rec, err := e.m.Recover(e.ctx, e.pr)
		if err != nil || slices.ContainsFunc(rec, func(r Recovered) bool { return r.Role == RoleClaude }) {
			t.Fatalf("recover = %+v, %v: want the parked claude-review left parked", rec, err)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked {
			t.Fatalf("claude session after recovery = %s, want parked", got.State)
		}
	})
	t.Run("a stop right after the quit", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		sent := 0
		e.h.onKeys = func(_ *fakeHerdr, target string, _ []string) {
			if sent++; target == ws.Panes[RoleClaude] && sent == 2 {
				cancel() // the daemon stops as the quit reaches its idle shell
			}
		}
		claude := e.session(RoleClaude)
		if _, err := e.m.ParkSessions(ctx, e.pr, []int64{claude.ID}); err != nil {
			t.Fatalf("park sessions: %v", err)
		}
		if !slices.Equal(e.h.paneCloses, []string{ws.Panes[RoleClaude]}) {
			t.Fatalf("pane closes %v, want the pane closed after the stop", e.h.paneCloses)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked {
			t.Fatalf("claude session = %s, want parked: the close and the retire both done", got.State)
		}
	})
	t.Run("a pane herdr will not close", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		e.h.onKeys = quitsOnCtrlC
		e.h.errs["PaneClose"] = &herdr.Error{Code: "internal", Message: "pane close refused"}
		claude := e.session(RoleClaude)
		if _, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID}); err != nil {
			t.Fatalf("park sessions: %v", err)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked {
			t.Fatalf("claude session = %s, want parked all the same", got.State)
		}
		delete(e.h.errs, "PaneClose")
		if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil {
			t.Fatalf("close retired panes: %v", err)
		}
		if got := e.h.paneCloses; len(got) != 2 || got[1] != ws.Panes[RoleClaude] {
			t.Fatalf("pane closes %v, want the refused close and the later one", got)
		}
	})
}

// A batch where one session parks and another fails returns the one it
// parked beside the failure, each with its own events.
func TestParkSessionsReturnsWhatItParkedBesideWhatFailed(t *testing.T) {
	e, ws := parkSessionsEnv(t)
	e.h.onKeys = quitsOnCtrlC
	e.h.busy[ws.Panes[RoleJudge]] = true // the judge's agent ignores its ctrl+c
	claude, judge := e.session(RoleClaude), e.session(RoleJudge)
	parked, err := e.m.ParkSessions(e.ctx, e.pr, []int64{judge.ID, claude.ID})
	if !errors.Is(err, ErrBusy) || len(parked) != 1 || parked[0].ID != claude.ID {
		t.Fatalf("park sessions = %+v, %v: want claude-review parked beside the judge's ErrBusy", parked, err)
	}
	if got, _ := e.st.SessionByID(e.ctx, judge.ID); got.State != store.SessionLive {
		t.Fatalf("judge = %s, want live", got.State)
	}
	want := []string{EventSessionParkBegin, EventSessionParkFailed, EventSessionParkBegin, EventSessionParked}
	if got := e.parkEvents(); !slices.Equal(got, want) {
		t.Fatalf("early-park events = %v, want %v", got, want)
	}
}
