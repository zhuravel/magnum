package pipeline

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// miniHerdr is just enough of herdr for agents.Manager's Submit, Observe and
// ReadRecent: one judge agent whose status the test controls. Methods the
// round never reaches fail.
type miniHerdr struct {
	mu      sync.Mutex
	name    string
	pane    string
	status  herdr.Status
	prompts []string
	// dialog is a trust dialog on screen: prompts fail with agent_blocked
	// until Enter answers it.
	dialog string
	keys   []string
}

var _ agents.Herdr = (*miniHerdr)(nil)

func (h *miniHerdr) snapshot() herdr.Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return herdr.Snapshot{
		Panes:  []herdr.Pane{{ID: h.pane, WorkspaceID: "w1", Agent: agents.KindCodex, AgentStatus: h.status}},
		Agents: []herdr.AgentInfo{{Name: h.name, PaneID: h.pane, WorkspaceID: "w1", Agent: agents.KindCodex, AgentStatus: h.status}},
	}
}

func (h *miniHerdr) setStatus(s herdr.Status) { h.mu.Lock(); h.status = s; h.mu.Unlock() }

func (h *miniHerdr) lastPrompt() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prompts[len(h.prompts)-1]
}

func (h *miniHerdr) Snapshot(context.Context) (herdr.Snapshot, error) { return h.snapshot(), nil }

func (h *miniHerdr) AgentPrompt(ctx context.Context, target, text string, wait *herdr.PromptWait) (herdr.AgentInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if target != h.name {
		return herdr.AgentInfo{}, fmt.Errorf("prompt to %q: %w", target, herdr.ErrUnavailable)
	}
	if h.dialog != "" {
		return herdr.AgentInfo{}, &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentBlocked, Message: "agent is blocked"}
	}
	h.prompts = append(h.prompts, text)
	h.status = herdr.StatusWorking
	return herdr.AgentInfo{Name: h.name, PaneID: h.pane, AgentStatus: herdr.StatusWorking,
		AgentSession: &herdr.AgentSession{Kind: "id", Value: "01a0fe66-uuid"}}, nil
}

func (h *miniHerdr) AgentRead(_ context.Context, _ string, o herdr.ReadOptions) (herdr.ReadResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dialog != "" && o.Source == herdr.SourceVisible {
		return herdr.ReadResult{Text: h.dialog}, nil
	}
	return herdr.ReadResult{Text: "Posted.\n"}, nil
}

func (h *miniHerdr) PaneRead(context.Context, string, herdr.ReadOptions) (herdr.ReadResult, error) {
	return herdr.ReadResult{Text: "Posted.\n"}, nil
}

var errUnused = fmt.Errorf("not used by a round: %w", herdr.ErrUnavailable)

func (h *miniHerdr) WorkspaceCreate(context.Context, herdr.WorkspaceCreateOptions) (herdr.WorkspaceCreated, error) {
	return herdr.WorkspaceCreated{}, errUnused
}
func (h *miniHerdr) WorkspaceClose(context.Context, string) error { return errUnused }
func (h *miniHerdr) PaneSplit(context.Context, string, herdr.SplitOptions) (herdr.Pane, error) {
	return herdr.Pane{}, errUnused
}
func (h *miniHerdr) PaneRename(context.Context, string, string) error { return errUnused }
func (h *miniHerdr) PaneRun(context.Context, string, string) error    { return errUnused }
func (h *miniHerdr) PaneWaitOutput(context.Context, string, herdr.WaitOutputOptions) (herdr.OutputMatch, error) {
	return herdr.OutputMatch{}, errUnused
}
func (h *miniHerdr) PaneGet(context.Context, string) (herdr.Pane, error) {
	return herdr.Pane{}, errUnused
}
func (h *miniHerdr) PaneSendKeys(context.Context, string, ...string) error {
	return errUnused
}
func (h *miniHerdr) PaneProcessInfo(context.Context, string) (herdr.ProcessInfo, error) {
	return herdr.ProcessInfo{}, errUnused
}
func (h *miniHerdr) WaitIdleShell(context.Context, string, time.Duration) (herdr.ProcessInfo, error) {
	return herdr.ProcessInfo{}, errUnused
}
func (h *miniHerdr) AgentStart(context.Context, herdr.AgentStartOptions) (herdr.AgentStarted, error) {
	return herdr.AgentStarted{}, errUnused
}
func (h *miniHerdr) AgentRename(context.Context, string, string) error { return errUnused }

func (h *miniHerdr) AgentSendKeys(_ context.Context, target string, keys ...string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if target != h.name {
		return errUnused
	}
	h.keys = append(h.keys, keys...)
	if h.dialog != "" && len(keys) == 1 && keys[0] == "enter" {
		h.dialog, h.status = "", herdr.StatusIdle
	}
	return nil
}

// TestRealAgentsManagerJudgeTurn drives a continued round through the real
// agents.Manager: Submit moves the judge run to working, and engine ticks
// (agents.ObserveSnapshot, simulated from the poll sleep) end it after two
// idle observations; the pipeline then verifies on GitHub.
func TestRealAgentsManagerJudgeTurn(t *testing.T) {
	e := newEnv(t)
	judge := e.sess[agents.RoleJudge]
	h := &miniHerdr{name: store.Deref(judge.AgentName), pane: store.Deref(judge.HerdrPaneID), status: herdr.StatusIdle}
	run := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Stdout: []byte("Logged in using ChatGPT\n")}},
	}}
	m := agents.New(agents.Deps{Herdr: h, Store: e.st, Runner: run, Config: e.cfg, Layout: e.layout, Clock: e.clock.Now})
	e.r.Agents = m

	ticks := 0
	e.r.Sleep = func(ctx context.Context, d time.Duration) error {
		e.clock.Add(d)
		ticks++
		if ticks == 3 { // the turn finishes: review posted, agent idle
			id := markerRunID(t, h.lastPrompt())
			e.gh.add(github.Review{DatabaseID: 701, State: "COMMENTED", Body: "ok <!-- magnum:run=" + id + " head=abc1234 -->",
				URL: "https://github.com/talkable/talkable/pull/11920#pullrequestreview-701", SubmittedAt: e.clock.Now(),
				CommitOid: target, AuthorLogin: "talkable", AuthorType: "Bot"},
				github.RESTReview{ID: 701, UserLogin: "talkable[bot]", UserType: "Bot", CommitID: target, State: "COMMENTED", SubmittedAt: e.clock.Now()})
			h.setStatus(herdr.StatusIdle)
		}
		if _, err := m.ObserveSnapshot(ctx, h.snapshot()); err != nil {
			t.Errorf("ObserveSnapshot: %v", err)
		}
		return nil
	}

	res, err := e.r.RunRound(e.ctx, e.input(KindContinue))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 701 || res.Event != "COMMENTED" {
		t.Fatalf("result = %+v", res)
	}
	if len(h.prompts) != 1 {
		t.Errorf("prompts = %d", len(h.prompts))
	}
	// Ticks 1-2 see the agent working; tick 3 flips it idle (idle_ticks 1),
	// tick 4 ends the run (idle_ticks 2), and the next poll sees it ended.
	if ticks != 4 {
		t.Errorf("ticks = %d, want 4", ticks)
	}
	r := e.runOf(agents.RoleJudge, store.RunContinue)
	if r.State != store.RunVerified || r.WorkingSeenAt == nil || r.EndedAt == nil || r.SubmittedAt == nil || store.Deref(r.ReviewID) != 701 {
		t.Errorf("judge run = %+v", r)
	}
	if s, err := e.st.SessionByID(e.ctx, judge.ID); err != nil || store.Deref(s.SessionID) != "01a0fe66-uuid" {
		t.Errorf("session id not recorded: %+v %v", s, err)
	}
}

// codexTrustDialog is Codex's first-launch folder-trust dialog on screen.
const codexTrustDialog = "  Folder access\n\n  /Users/x/Projects/talkable.review1\n\n  Trust this folder?\n  Codex can read, edit, and run files here …\n\n" +
	"› 1. Trust and continue\n  2. Quit\n\n  enter continue · esc quit\n"

// TestRealAgentsManagerAnswersJudgeTrustDialog: a judge started moments ago
// and never prompted waits on Codex's folder-trust dialog, so its prompt is
// rejected with agent_blocked; the real agents.Manager (Submit) answers the
// dialog and re-sends the prompt once, and the review is then verified as
// usual.
func TestRealAgentsManagerAnswersJudgeTrustDialog(t *testing.T) {
	e := newEnv(t)
	judge := e.sess[agents.RoleJudge]
	if err := e.st.UpdateSession(e.ctx, judge.ID, func(u *store.SessionUpdate) { u.Set("started_at", e.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	h := &miniHerdr{name: store.Deref(judge.AgentName), pane: store.Deref(judge.HerdrPaneID), status: herdr.StatusBlocked,
		dialog: codexTrustDialog}
	run := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Stdout: []byte("Logged in using ChatGPT\n")}},
	}}
	var agentSleeps []time.Duration
	m := agents.New(agents.Deps{Herdr: h, Store: e.st, Runner: run, Config: e.cfg, Layout: e.layout, Clock: e.clock.Now,
		Sleep: func(ctx context.Context, d time.Duration) error { agentSleeps = append(agentSleeps, d); return nil }})
	e.r.Agents = m

	ticks := 0
	e.r.Sleep = func(ctx context.Context, d time.Duration) error {
		e.clock.Add(d)
		ticks++
		if ticks == 2 {
			id := markerRunID(t, h.lastPrompt())
			e.gh.add(github.Review{DatabaseID: 702, State: "COMMENTED", Body: "ok <!-- magnum:run=" + id + " head=abc1234 -->",
				URL: "https://github.com/talkable/talkable/pull/11920#pullrequestreview-702", SubmittedAt: e.clock.Now(),
				CommitOid: target, AuthorLogin: "talkable", AuthorType: "Bot"},
				github.RESTReview{ID: 702, UserLogin: "talkable[bot]", UserType: "Bot", CommitID: target, State: "COMMENTED", SubmittedAt: e.clock.Now()})
			h.setStatus(herdr.StatusIdle)
		}
		if _, err := m.ObserveSnapshot(ctx, h.snapshot()); err != nil {
			t.Errorf("ObserveSnapshot: %v", err)
		}
		return nil
	}

	res, err := e.r.RunRound(e.ctx, e.input(KindContinue))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 702 {
		t.Fatalf("result = %+v", res)
	}
	if len(h.keys) != 1 || h.keys[0] != "enter" || len(h.prompts) != 1 {
		t.Fatalf("keys = %v, prompts sent = %d", h.keys, len(h.prompts))
	}
	r := e.runOf(agents.RoleJudge, store.RunContinue)
	if r.State != store.RunVerified || r.SubmittedAt == nil || r.Error != nil {
		t.Errorf("judge run = %+v", r)
	}
	answered := false
	for _, ev := range e.events() {
		answered = answered || ev.Kind == agents.EventTrustDialogAnswered
	}
	if !answered {
		t.Errorf("no %s event", agents.EventTrustDialogAnswered)
	}
	if len(agentSleeps) == 0 {
		t.Error("the readiness wait must go through Deps.Sleep")
	}
}

// A judge that started long ago is past the first-launch window: whatever
// its screen shows, nothing is pressed and the round ends blocked.
func TestRealAgentsManagerLeavesOldJudgesDialog(t *testing.T) {
	e := newEnv(t) // the judge's session started an hour ago
	judge := e.sess[agents.RoleJudge]
	h := &miniHerdr{name: store.Deref(judge.AgentName), pane: store.Deref(judge.HerdrPaneID), status: herdr.StatusBlocked,
		dialog: codexTrustDialog}
	run := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Stdout: []byte("Logged in using ChatGPT\n")}},
	}}
	e.r.Agents = agents.New(agents.Deps{Herdr: h, Store: e.st, Runner: run, Config: e.cfg, Layout: e.layout, Clock: e.clock.Now,
		Sleep: func(context.Context, time.Duration) error { return nil }})
	res, _ := e.r.RunRound(e.ctx, e.input(KindContinue))
	if res.Outcome != OutcomeBlocked || len(h.keys) != 0 || len(h.prompts) != 0 {
		t.Fatalf("outcome %s, keys %v, prompts %d", res.Outcome, h.keys, len(h.prompts))
	}
}
