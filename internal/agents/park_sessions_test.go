package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// quitsOnCtrlC makes a ctrl+c quit the agent of the pane it reaches: its
// pane is an idle shell and herdr no longer lists it (onKeys runs with the
// fake's lock held).
func quitsOnCtrlC(f *fakeHerdr, target string, keys []string) {
	if slices.Contains(keys, "ctrl+c") {
		f.busy[target] = false
		f.agents = slices.DeleteFunc(f.agents, func(a herdr.AgentInfo) bool { return a.PaneID == target })
	}
}

// atPrompt makes the agent of pane its foreground process, idle at Claude
// Code's bare composer (claudeAtPrompt).
func (e *env) atPrompt(pane string) {
	e.h.busy[pane] = true
	e.h.reads[pane] = claudeAtPrompt
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
// selected agent gets its ctrl+c (it quits there, so its kind's exit keys
// are never typed into the shell it left), becomes parked with its
// conversation once its pane is an idle shell, between a begin and an ok
// event, and only its pane is closed by the retired-pane close; the judge,
// the codex-review shell and the workspace stay.
func TestParkSessionsParksTheSelectedAgentAlone(t *testing.T) {
	e, ws := parkSessionsEnv(t)
	e.atPrompt(ws.Panes[RoleClaude])
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
	if got := e.keysTo(ws.Panes[RoleClaude]); !slices.EqualFunc(got, [][]string{{"ctrl+c"}}, slices.Equal) {
		t.Fatalf("keys to the claude pane = %q, want its ctrl+c alone", got)
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
	if got := e.keysTo(ws.Panes[RoleClaude]); len(got) != 1 {
		t.Fatalf("keys to the claude pane = %q, want no more", got)
	}
}

// The early park keeps Park's guards: a run of the PR in flight, a
// selected agent at work or blocked, a selected session without a
// conversation to resume and the pane of an unlisted agent that runs a
// command are refused before any key and any event; an agent that quits
// neither at its ctrl+c nor at its kind's exit keys stays live with
// ErrBusy, never escalated to the hard mode.
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
	t.Run("an agent that ignores its exit keys too", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		e.h.busy[ws.Panes[RoleClaude]] = true
		e.h.reads[ws.Panes[RoleClaude]] = claudeAtPrompt
		claude := e.session(RoleClaude)
		_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
		if !errors.Is(err, ErrBusy) || !herdr.IsTimeout(err) {
			t.Fatalf("err = %v, want ErrBusy with herdr's timeout", err)
		}
		if got, want := e.keysTo(ws.Panes[RoleClaude]), [][]string{{"ctrl+c"}, e.exitKeys(KindClaude)}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("keys to the claude pane = %q, want %q: one ctrl+c, the exit keys, nothing after", got, want)
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
// right after the exit keys' quit was confirmed cannot cut between the close
// and the retire; a stop during the pause after the ctrl+c sends nothing
// more and retires nothing. A pane herdr will not close leaves the session
// parked all the same, its pane for a later CloseRetiredPanes.
func TestAnEarlyParkClosesThePaneBeforeItParks(t *testing.T) {
	t.Run("herdr still lists the quit agent", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		e.atPrompt(ws.Panes[RoleClaude])
		e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
			if slices.Contains(keys, "ctrl+c") {
				f.busy[target] = false // the agent quits; herdr still lists it
			}
		}
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
			t.Fatalf("slept %v, want only the pause after its ctrl+c: no polling", e.slept)
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
		pane := ws.Panes[RoleClaude]
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		e.h.busy[pane] = true
		e.h.reads[pane] = claudeAtPrompt
		e.h.onKeys = deafToCtrlC(pane)
		e.h.onIdleShell = func(p string) {
			if p == pane {
				cancel() // the daemon stops as the wait sees the exit keys' quit
			}
		}
		claude := e.session(RoleClaude)
		if _, err := e.m.ParkSessions(ctx, e.pr, []int64{claude.ID}); err != nil {
			t.Fatalf("park sessions: %v", err)
		}
		if ctx.Err() == nil {
			t.Fatal("the stop never came: the wait saw no idle shell")
		}
		if !slices.Equal(e.h.paneCloses, []string{pane}) {
			t.Fatalf("pane closes %v, want the pane closed after the stop", e.h.paneCloses)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked {
			t.Fatalf("claude session = %s, want parked: the close and the retire both done", got.State)
		}
	})
	t.Run("a stop during the pause after the ctrl+c", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		pane := ws.Panes[RoleClaude]
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		e.h.busy[pane] = true
		e.h.reads[pane] = claudeAtPrompt
		e.h.onKeys = deafToCtrlC(pane)
		e.onSleep = cancel
		claude := e.session(RoleClaude)
		if _, err := e.m.ParkSessions(ctx, e.pr, []int64{claude.ID}); !errors.Is(err, context.Canceled) {
			t.Fatalf("park sessions: %v, want the stop's error", err)
		}
		if got := e.keysTo(pane); !slices.EqualFunc(got, [][]string{{"ctrl+c"}}, slices.Equal) {
			t.Fatalf("keys to the claude pane = %q, want the ctrl+c alone: nothing after the stop", got)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionLive || len(e.h.paneCloses) != 0 {
			t.Fatalf("state %s, pane closes %v: want live, nothing closed", got.State, e.h.paneCloses)
		}
	})
	t.Run("a pane herdr will not close", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		e.atPrompt(ws.Panes[RoleClaude])
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
	e.atPrompt(ws.Panes[RoleClaude])
	e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
		if target == ws.Panes[RoleClaude] {
			quitsOnCtrlC(f, target, keys)
		}
	}
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

// exitKeys are the exit keys of the configured kind.
func (e *env) exitKeys(kind string) []string {
	e.t.Helper()
	k, ok := e.cfg.KindSpec(kind)
	if !ok || len(k.ExitKeys) == 0 {
		e.t.Fatalf("kind %s has no exit keys", kind)
	}
	return k.ExitKeys
}

// setTerminal gives pane and the agent herdr lists in it the terminal id.
func (f *fakeHerdr) setTerminal(pane, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.panes {
		if f.panes[i].ID == pane {
			f.panes[i].TerminalID = id
		}
	}
	for i := range f.agents {
		if f.agents[i].PaneID == pane {
			f.agents[i].TerminalID = id
		}
	}
}

// composerRule is the full-width rule under Claude Code's composer, and
// composerTop the rule over it, with the session's name.
const (
	composerRule = "────────────────────────────────────────────────────────"
	composerTop  = "──── PR #11920 claude-review - talkable ─"
)

// claudeAtPrompt is Claude Code's idle layout after a ctrl+c: the composer's
// labelled top rule, the bare prompt, its bottom rule, two status lines.
const claudeAtPrompt = "⏺ Review written.\n\n" + composerTop + "\n❯ \n" + composerRule + "\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n  Press Ctrl-C again to exit\n"

// deafToCtrlC makes the agent of pane ignore ctrl+c at its idle prompt, as
// Claude 2.1.301 and 2.1.303 did (its pane no idle shell after two), and
// quit at its kind's exit keys: the enter that ends them leaves an idle
// shell, and herdr lists the agent no more (onKeys runs with the fake's
// lock held).
func deafToCtrlC(pane string) func(f *fakeHerdr, target string, keys []string) {
	return func(f *fakeHerdr, target string, keys []string) {
		if target == pane && slices.Contains(keys, "enter") {
			f.busy[pane] = false
			f.agents = slices.DeleteFunc(f.agents, func(a herdr.AgentInfo) bool { return a.PaneID == pane })
		}
	}
}

// The early park quits an agent with its kind's exit keys: one ctrl+c,
// which clears Claude's input line, then, after a pause and a fresh look
// that finds the session's own conversation still at its empty prompt (the
// screen read last, right before the keys), the exit keys (/exit and enter)
// and the wait for an idle shell. A Claude that ignores ctrl+c is parked
// so, its pane closed, and the ok event says how it quit.
func TestAnEarlyParkQuitsClaudeWithItsExitKeys(t *testing.T) {
	e, ws := parkSessionsEnv(t)
	pane := ws.Panes[RoleClaude]
	e.h.busy[pane] = true
	e.h.reads[pane] = claudeAtPrompt
	e.h.setTerminal(pane, "term-1") // the terminal herdr gave stays the same
	e.h.onKeys = deafToCtrlC(pane)
	claude := e.session(RoleClaude)

	parked, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
	if err != nil || len(parked) != 1 || parked[0].ID != claude.ID {
		t.Fatalf("park sessions = %+v, %v: want claude-review parked", parked, err)
	}
	if got, want := e.keysTo(pane), [][]string{{"ctrl+c"}, e.exitKeys(KindClaude)}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("keys to the claude pane = %q, want %q: one ctrl+c, then the exit keys", got, want)
	}
	if !slices.Equal(e.slept, []time.Duration{time.Second}) {
		t.Fatalf("slept %v, want the pause between the ctrl+c and the exit keys", e.slept)
	}
	exitSend := "PaneSendKeys " + pane + " " + strings.Join(e.exitKeys(KindClaude), ",")
	calls := e.h.calls
	if i := slices.Index(calls, exitSend); i < 1 || calls[i-1] != "PaneRead "+pane+" visible 0" {
		t.Fatalf("herdr calls %q: want the screen read right before %q", calls, exitSend)
	}
	if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionParked || store.Deref(got.SessionID) != "claude-id" {
		t.Fatalf("claude session = %+v, want parked with its id", got)
	}
	if !slices.Equal(e.h.paneCloses, []string{pane}) || len(e.h.closed) != 0 {
		t.Fatalf("pane closes %v, workspace closes %v: want the claude pane alone", e.h.paneCloses, e.h.closed)
	}
	if got := e.keysTo(ws.Panes[RoleJudge]); len(got) != 0 {
		t.Fatalf("keys to the judge pane = %q, want none", got)
	}
	evs := e.eventsOf(EventSessionParked)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "quit with its exit keys") {
		t.Fatalf("%s events = %+v, want one saying it quit with its exit keys", EventSessionParked, evs)
	}
}

// A kind without exit keys (Codex's default, or Claude's set to []) is quit
// by the early park as before: two ctrl+c 1 s apart and the wait.
func TestAnEarlyParkOfAKindWithoutExitKeysSendsTwoCtrlC(t *testing.T) {
	twoCtrlC := [][]string{{"ctrl+c"}, {"ctrl+c"}}
	t.Run("they quit it", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		e.setKind(KindClaude, func(k *config.Kind) { k.ExitKeys = nil })
		e.h.onKeys = quitsOnCtrlC
		claude, judge := e.session(RoleClaude), e.session(RoleJudge)
		parked, err := e.m.ParkSessions(e.ctx, e.pr, []int64{judge.ID, claude.ID})
		if err != nil || len(parked) != 2 {
			t.Fatalf("park sessions = %+v, %v: want the judge and claude-review parked", parked, err)
		}
		for _, r := range []Role{RoleJudge, RoleClaude} {
			if got := e.keysTo(ws.Panes[r]); !slices.EqualFunc(got, twoCtrlC, slices.Equal) {
				t.Fatalf("keys to the %s pane = %q, want two ctrl+c", r, got)
			}
		}
		for _, ev := range e.eventsOf(EventSessionParked) {
			if strings.Contains(ev.Message, "exit keys") {
				t.Fatalf("%s event %q: no exit keys were sent", EventSessionParked, ev.Message)
			}
		}
	})
	t.Run("an agent that ignores them", func(t *testing.T) {
		e, ws := parkSessionsEnv(t)
		e.setKind(KindClaude, func(k *config.Kind) { k.ExitKeys = nil })
		e.h.busy[ws.Panes[RoleClaude]] = true
		claude := e.session(RoleClaude)
		_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
		if !errors.Is(err, ErrBusy) || !herdr.IsTimeout(err) {
			t.Fatalf("err = %v, want ErrBusy with herdr's timeout", err)
		}
		if got := e.keysTo(ws.Panes[RoleClaude]); !slices.EqualFunc(got, twoCtrlC, slices.Equal) {
			t.Fatalf("keys to the claude pane = %q, want two ctrl+c", got)
		}
		if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionLive || len(e.h.paneCloses) != 0 {
			t.Fatalf("state %s, pane closes %v: want live, nothing closed", got.State, e.h.paneCloses)
		}
	})
}

// The keys reach only the session's own conversation at its bare prompt:
// the guards refuse an agent at work or blocked before any key, the same
// look before the ctrl+c sends no key at all to an agent whose status is
// unknown or whose screen shows text in the composer, a dialog, or cannot
// be read, and the look after the ctrl+c sends no exit keys when herdr no
// longer has that conversation idle in the pane (it works, shows a dialog
// or is unknown; another agent, conversation or terminal; a command in the
// pane's foreground) or the screen read last shows text in the composer, a
// dialog, or cannot be read. The session stays live with ErrBusy (never a
// timeout, which would make the PR's park hard), nothing closed.
func TestAnEarlyParkSendsTheExitKeysOnlyToItsAgentsIdlePrompt(t *testing.T) {
	for _, status := range []herdr.Status{herdr.StatusWorking, herdr.StatusBlocked} {
		t.Run("an agent "+string(status)+" before the ctrl+c", func(t *testing.T) {
			e, ws := parkSessionsEnv(t)
			e.h.busy[ws.Panes[RoleClaude]] = true
			e.h.setAgentStatus(claudeAgent, status)
			claude := e.session(RoleClaude)
			if _, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID}); !errors.Is(err, ErrBusy) {
				t.Fatalf("err = %v, want ErrBusy", err)
			}
			if len(e.h.keys) != 0 {
				t.Fatalf("keys = %+v, want none", e.h.keys)
			}
		})
	}
	composerText := func(f *fakeHerdr, pane string) {
		f.reads[pane] = "⏺ Review written.\n\n" + composerTop + "\n❯ say only the word banana\n" + composerRule + "\n  ? for shortcuts\n"
	}
	dialog := func(f *fakeHerdr, pane string) {
		// An approval under a bare composer: the composer alone would pass.
		f.reads[pane] = "⏺ Review written.\n\n" + composerTop + "\n❯ \n" + composerRule + "\n Bash command\n rm -rf out\n" +
			" Do you want to proceed? (y/n)\n"
	}
	unreadable := func(f *fakeHerdr, _ string) {
		f.errs["PaneRead"] = fmt.Errorf("herdr pane.read: %w", herdr.ErrTimeout)
	}
	before := map[string]func(f *fakeHerdr, pane string){
		"an unknown status before the ctrl+c":          func(f *fakeHerdr, _ string) { setStatusLocked(f, claudeAgent, herdr.StatusUnknown) },
		"a composer with text before the ctrl+c":       composerText,
		"a dialog before the ctrl+c":                   dialog,
		"a screen herdr cannot read before the ctrl+c": unreadable,
	}
	for name, change := range before {
		t.Run(name, func(t *testing.T) {
			e, ws := parkSessionsEnv(t)
			pane := ws.Panes[RoleClaude]
			e.atPrompt(pane)
			e.h.mu.Lock()
			change(e.h, pane)
			e.h.mu.Unlock()
			claude := e.session(RoleClaude)
			_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
			if !errors.Is(err, ErrBusy) || herdr.IsTimeout(err) {
				t.Fatalf("err = %v, want ErrBusy without a timeout", err)
			}
			if len(e.h.keys) != 0 {
				t.Fatalf("keys = %+v, want none: the screen was not bare before the ctrl+c", e.h.keys)
			}
			if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionLive || len(e.h.paneCloses) != 0 {
				t.Fatalf("state %s, pane closes %v: want live, nothing closed", got.State, e.h.paneCloses)
			}
		})
	}
	after := map[string]func(f *fakeHerdr, pane string){
		"an agent at work after the ctrl+c":  func(f *fakeHerdr, _ string) { setStatusLocked(f, claudeAgent, herdr.StatusWorking) },
		"a blocked agent after the ctrl+c":   func(f *fakeHerdr, _ string) { setStatusLocked(f, claudeAgent, herdr.StatusBlocked) },
		"an unknown status after the ctrl+c": func(f *fakeHerdr, _ string) { setStatusLocked(f, claudeAgent, herdr.StatusUnknown) },
		"another agent in the pane": func(f *fakeHerdr, pane string) {
			for i := range f.agents {
				if f.agents[i].PaneID == pane {
					f.agents[i].Agent = KindCodex
				}
			}
		},
		"a command in the pane's foreground": func(f *fakeHerdr, pane string) {
			f.agents = slices.DeleteFunc(f.agents, func(a herdr.AgentInfo) bool { return a.PaneID == pane })
		},
		"another conversation in the pane": func(f *fakeHerdr, pane string) {
			for i := range f.agents {
				if f.agents[i].PaneID == pane {
					f.agents[i].AgentSession = &herdr.AgentSession{Kind: "id", Value: "another-conversation"}
				}
			}
		},
		"another terminal in the pane": func(f *fakeHerdr, pane string) {
			for i := range f.panes {
				if f.panes[i].ID == pane {
					f.panes[i].TerminalID = "term-2"
				}
			}
		},
		"a composer with text after the ctrl+c":       composerText,
		"a dialog after the ctrl+c":                   dialog,
		"a screen herdr cannot read after the ctrl+c": unreadable,
	}
	for name, change := range after {
		t.Run(name, func(t *testing.T) {
			e, ws := parkSessionsEnv(t)
			pane := ws.Panes[RoleClaude]
			e.h.busy[pane] = true // the ctrl+c leaves no idle shell
			e.h.reads[pane] = claudeAtPrompt
			e.h.setTerminal(pane, "term-1")
			e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
				if target == pane && slices.Contains(keys, "ctrl+c") {
					change(f, pane)
				}
			}
			claude := e.session(RoleClaude)
			_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
			if !errors.Is(err, ErrBusy) || herdr.IsTimeout(err) {
				t.Fatalf("err = %v, want ErrBusy without a timeout", err)
			}
			if got := e.keysTo(pane); !slices.EqualFunc(got, [][]string{{"ctrl+c"}}, slices.Equal) {
				t.Fatalf("keys to the claude pane = %q, want the ctrl+c alone: no exit keys", got)
			}
			if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionLive || len(e.h.paneCloses) != 0 {
				t.Fatalf("state %s, pane closes %v: want live, nothing closed", got.State, e.h.paneCloses)
			}
		})
	}
}

// The exit keys need a bare composer, read from the screen's structure:
// the region between the last two composer rules (full-width rules from
// column 0, maybe labelled with the session's name; the status lines under
// the bottom one skipped) holds exactly one line, and that raw line is a
// prompt mark alone. Claude Code's idle layout is the only screen here
// that gets the keys: two real idle panes of Claude 2.1.303 (testdata, read
// from a 113-column pane and from talkable#12051's 53-column one, whose
// labelled top rule starts with 16 rule characters, under three status
// lines) and layouts like them. Typed text (the placeholder or one like
// it, a draft, a pasted block, a continuation line that is a prompt mark, a
// box character or a short rule), a stale prompt above a second rule, a
// short rule as the top border, a screen without rules, a composer with
// more lines under it than status lines, a labelled closing rule, a status
// line that starts with a prompt mark and a carriage return before the mark
// send no key at all and fail busy; CRLF line ends do not matter.
func TestAnEarlyParkSendsTheExitKeysOnlyAtABareComposer(t *testing.T) {
	idle := func(file string) string {
		b, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	screen := func(composer ...string) string {
		return "⏺ Review written.\n\n" + composerTop + "\n" + strings.Join(composer, "\n") + "\n" + composerRule +
			"\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n  ? for shortcuts\n"
	}
	for _, c := range []struct {
		name, screen string
		sends        bool
	}{
		{"claude 2.1.303's idle pane, 113 columns", idle("claude_idle_wide.txt"), true},
		{"claude 2.1.303's idle pane, 53 columns", idle("claude_idle_narrow.txt"), true},
		{"claude's idle layout", claudeAtPrompt, true},
		{"a bare prompt with no top rule", "⏺ Review written.\n\n❯\n" + composerRule + "\n  ? for shortcuts\n", false},
		{"a bare prompt between plain rules", "⏺ Review written.\n\n" + composerRule + "\n  ❯\n" + composerRule + "\n  ? for shortcuts\n", true},
		{"typed text like the placeholder", screen(`❯ Try "say banana"`), false},
		{"the placeholder itself", screen(`❯ Try "refactor <filepath>"`), false},
		{"a draft", screen("❯ say banana"), false},
		{"a draft of several lines", screen("❯ ", "  say banana"), false},
		{"a pasted block on the continuation line", screen("❯ ", "  [Pasted text #1 +12 lines]"), false},
		{"a draft whose continuation line is a prompt mark", screen("❯ say banana", "  ❯"), false},
		{"a stale prompt above a second rule", "⏺ Review written.\n\n" + composerTop + "\n❯\n" + composerRule + "\n" + composerRule +
			"\n  say banana\n", false},
		{"a typed rule character after the mark", screen("❯ ─"), false},
		{"a continuation of only a box side", screen("❯", "│"), false},
		{"a typed short rule on the continuation line", screen("❯", "  ───"), false},
		{"a typed long rule on the continuation line", screen("❯", "  "+composerRule), false},
		{"a short rule as the top border", "⏺ Review written.\n───\n❯\n" + composerRule + "\n  ? for shortcuts\n", false},
		{"no rules", "⏺ Review written.\n\n❯ \n  ? for shortcuts\n", false},
		{"a composer above more lines than the status lines", claudeAtPrompt + strings.Repeat("  more output\n", 5), false},
		{"a bare prompt without the composer's closing rule", "⏺ Review written.\n\n" + composerTop + "\n❯ \n\n", false},
		{"a labelled rule as the closing rule, a draft under it", "⏺ Review written.\n\n" + composerRule[:20*len("─")] + "\n❯\n" + composerTop +
			"\n❯ draft\n", false},
		{"a labelled closing rule", "⏺ Review written.\n\n" + composerTop + "\n❯\n" + composerTop + "\n  ? for shortcuts\n", false},
		{"a status line that starts with a prompt mark", claudeAtPrompt + "  ❯ draft\n", false},
		{"a carriage return before the prompt mark", screen("\r❯"), false},
		{"CRLF line ends", strings.ReplaceAll(claudeAtPrompt, "\n", "\r\n"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, ws := parkSessionsEnv(t)
			pane := ws.Panes[RoleClaude]
			e.h.busy[pane] = true
			e.h.reads[pane] = c.screen
			e.h.onKeys = deafToCtrlC(pane)
			claude := e.session(RoleClaude)
			_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
			got, _ := e.st.SessionByID(e.ctx, claude.ID)
			keys := e.keysTo(pane)
			if c.sends {
				if want := [][]string{{"ctrl+c"}, e.exitKeys(KindClaude)}; err != nil || got.State != store.SessionParked || !slices.EqualFunc(keys, want, slices.Equal) {
					t.Fatalf("err %v, state %s, keys %q: want parked after %q", err, got.State, keys, want)
				}
				return
			}
			if !errors.Is(err, ErrBusy) || herdr.IsTimeout(err) {
				t.Fatalf("err = %v, want ErrBusy without a timeout", err)
			}
			if len(keys) != 0 {
				t.Fatalf("keys to the claude pane = %q, want none: the screen is not bare before the ctrl+c", keys)
			}
			if got.State != store.SessionLive || len(e.h.paneCloses) != 0 {
				t.Fatalf("state %s, pane closes %v: want live, nothing closed", got.State, e.h.paneCloses)
			}
		})
	}
}

// The conversation the keys may reach is the one the park found: the
// snapshot's agent_session, else the id the session's row recorded, taken
// before the ctrl+c, whose id refresh records what herdr shows then. A
// conversation B in place at the first look gets no key; one that replaced
// it between that look and the refresh gets the ctrl+c alone.
func TestAnEarlyParkKeepsTheConversationItFoundBeforeTheCtrlC(t *testing.T) {
	for _, c := range []struct {
		name string
		// shows are the conversations herdr reports for the pane at its
		// first, second, ... PaneGet (the first look, the ctrl+c's id
		// refresh, the look after the ctrl+c).
		shows []string
		want  [][]string
	}{
		{"at the first look", []string{"conversation-b"}, nil},
		{"before the ctrl+c's id refresh", []string{"claude-id", "conversation-b"}, [][]string{{"ctrl+c"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, ws := parkSessionsEnv(t)
			pane := ws.Panes[RoleClaude]
			e.atPrompt(pane)
			e.h.onKeys = deafToCtrlC(pane)
			e.h.mu.Lock()
			for i := range e.h.agents {
				if e.h.agents[i].PaneID == pane {
					e.h.agents[i].AgentSession = nil // the park's snapshot shows no conversation
				}
			}
			e.h.mu.Unlock()
			gets := 0
			e.h.onPaneGet = func(f *fakeHerdr, p string) {
				if p != pane {
					return
				}
				if gets++; gets > len(c.shows) {
					return
				}
				for i := range f.agents {
					if f.agents[i].PaneID == pane {
						f.agents[i].AgentSession = &herdr.AgentSession{Kind: "id", Value: c.shows[gets-1]}
					}
				}
			}
			claude := e.session(RoleClaude)
			if store.Deref(claude.SessionID) != "claude-id" {
				t.Fatalf("session id = %q, want the recorded claude-id", store.Deref(claude.SessionID))
			}
			_, err := e.m.ParkSessions(e.ctx, e.pr, []int64{claude.ID})
			if !errors.Is(err, ErrBusy) || gets < len(c.shows) {
				t.Fatalf("err = %v (%d pane reads), want ErrBusy for conversation B", err, gets)
			}
			if got := e.keysTo(pane); !slices.EqualFunc(got, c.want, slices.Equal) {
				t.Fatalf("keys to the claude pane = %q, want %q: no exit keys for conversation B", got, c.want)
			}
			if got, _ := e.st.SessionByID(e.ctx, claude.ID); got.State != store.SessionLive || len(e.h.paneCloses) != 0 {
				t.Fatalf("state %s, pane closes %v: want live, nothing closed", got.State, e.h.paneCloses)
			}
		})
	}
}
