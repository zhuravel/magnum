package agents

import (
	"errors"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// observe runs one tick and returns the observations keyed by role.
func (e *env) observe() map[Role]Observation {
	e.t.Helper()
	obs, err := e.m.Observe(e.ctx)
	if err != nil {
		e.t.Fatalf("Observe: %v", err)
	}
	out := map[Role]Observation{}
	for _, o := range obs {
		out[o.Role] = o
	}
	return out
}

func TestObserveCompletionNeedsTwoIdleTicks(t *testing.T) {
	e := newEnv(t)
	e.started()
	const judge = "mg-11920-codex-judge-5d01cf"
	e.clock.Add(3 * time.Minute)
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review")
	if err != nil {
		t.Fatal(err)
	}

	e.clock.Add(30 * time.Second)
	o := e.observe()[RoleJudge]
	if o.Kind != "" || o.Status != herdr.StatusWorking || o.Run == nil || o.Run.ID != id {
		t.Fatalf("tick 1 (working) = %+v", o)
	}

	e.h.setAgentStatus(judge, herdr.StatusIdle)
	e.clock.Add(30 * time.Second)
	o = e.observe()[RoleJudge]
	if o.Kind != "" || o.Session.IdleTicks != 1 {
		t.Fatalf("tick 2 (first idle) = %+v", o)
	}

	// A flicker back to working resets the count.
	e.h.setAgentStatus(judge, herdr.StatusWorking)
	e.clock.Add(30 * time.Second)
	if o = e.observe()[RoleJudge]; o.Session.IdleTicks != 0 || o.Kind != "" {
		t.Fatalf("tick 3 (working again) = %+v", o)
	}

	e.h.setAgentStatus(judge, herdr.StatusDone)
	e.clock.Add(30 * time.Second)
	e.observe()
	e.h.setAgentStatus(judge, herdr.StatusIdle)
	e.clock.Add(30 * time.Second)
	o = e.observe()[RoleJudge]
	if o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
		t.Fatalf("tick 5 = %+v, want completed", o)
	}
	r := e.run1(id)
	if r.State != store.RunEnded || r.EndedAt == nil {
		t.Fatalf("run = %+v, want ended", r)
	}
	// Completion fires once.
	e.clock.Add(30 * time.Second)
	if o = e.observe()[RoleJudge]; o.Kind != "" {
		t.Fatalf("tick 6 = %+v, want nothing", o)
	}
	s := e.session(RoleJudge)
	if store.Deref(s.AgentStatus) != "idle" || s.AgentStatusAt == nil {
		t.Fatalf("session status = %+v", s)
	}
}

func TestObserveSubmittedRunMarkedWorking(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.h.promptStatus = herdr.StatusIdle // ack came back before working was seen
	e.h.errs["AgentPrompt"] = &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentPromptStalled, Message: "stalled"}
	id, _ := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "x")
	if e.run1(id).State != store.RunSubmitted {
		t.Fatal("stalled run should be submitted")
	}
	e.h.setAgentStatus("mg-11920-codex-judge-5d01cf", herdr.StatusWorking)
	e.observe()
	if r := e.run1(id); r.State != store.RunWorking || r.WorkingSeenAt == nil {
		t.Fatalf("run = %+v, want working", r)
	}
}

func TestObserveHumanActive(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.clock.Add(10 * time.Minute) // past the start grace
	e.h.setAgentStatus("mg-11920-claude-review-5d01cf", herdr.StatusWorking)
	obs := e.observe()
	if obs[RoleClaude].Kind != ObsHumanActive {
		t.Fatalf("claude = %+v, want human_active", obs[RoleClaude])
	}
	if obs[RoleJudge].Kind != "" {
		t.Fatalf("judge = %+v, want nothing", obs[RoleJudge])
	}
	pr := e.reloadPR()
	if pr.HumanActiveAt == nil || !pr.HumanActiveAt.Equal(e.clock.Now()) {
		t.Fatalf("human_active_at = %v", pr.HumanActiveAt)
	}
}

func TestObserveNoHumanActiveRightAfterStartOrPrompt(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.h.setAgentStatus("mg-11920-claude-review-5d01cf", herdr.StatusWorking)
	if o := e.observe()[RoleClaude]; o.Kind == ObsHumanActive {
		t.Fatal("working right after start is not a human")
	}
}

func TestObserveLostAndBlocked(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "judge-uuid")
	e.h.setAgentStatus("mg-11920-claude-review-5d01cf", herdr.StatusBlocked)
	e.h.removePane(ws.Panes[RoleCodexReview])
	e.clock.Add(LostGrace + time.Second)
	obs := e.observe()
	if obs[RoleClaude].Kind != ObsBlocked {
		t.Fatalf("claude = %+v, want blocked", obs[RoleClaude])
	}
	if obs[RoleCodexReview].Kind != ObsLost {
		t.Fatalf("codex-review = %+v, want lost", obs[RoleCodexReview])
	}
	if got := store.Deref(e.session(RoleJudge).SessionID); got != "judge-uuid" {
		t.Fatalf("Observe should record the session id, got %q", got)
	}

	e.h.removeAgent("mg-11920-codex-judge-5d01cf") // agent exited, pane is a shell again
	obs = e.observe()
	if obs[RoleJudge].Kind != ObsLost {
		t.Fatalf("judge = %+v, want lost", obs[RoleJudge])
	}
	var lost store.Session
	for _, s := range e.sessions() {
		if s.Role == string(RoleJudge) {
			lost = s
		}
	}
	if lost.State != store.SessionLost || store.Deref(lost.SessionID) != "judge-uuid" {
		t.Fatalf("judge session = %+v, want lost with id kept", lost)
	}
	if _, err := e.st.LiveSessionByPRRole(e.ctx, e.pr.ID, string(RoleCodexReview)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("codex-review should no longer be live: %v", err)
	}
}

func TestObserveStartingSessionNotLost(t *testing.T) {
	e := newEnv(t)
	e.workspace() // sessions starting, agents not started yet
	obs := e.observe()
	for r, o := range obs {
		if o.Kind != "" {
			t.Fatalf("%s = %+v, want nothing while starting", r, o)
		}
	}
}

func TestReadRecentAndHealth(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.reads["mg-11920-codex-judge-5d01cf"] = "■ You've hit your usage limit. Try again in 2 hours 5 minutes."
	s := e.session(RoleJudge)
	text, err := e.m.ReadRecent(e.ctx, s, 80)
	if err != nil || text == "" {
		t.Fatalf("ReadRecent = %q, %v", text, err)
	}
	if got := e.h.callsWith("AgentRead"); len(got) != 1 || got[0] != "AgentRead mg-11920-codex-judge-5d01cf recent_unwrapped 80" {
		t.Fatalf("AgentRead calls = %v", got)
	}
	h, err := e.m.CheckHealth(e.ctx, s)
	if err != nil || h.Kind != HealthUsageLimit || h.ResetAt == nil || !h.ResetAt.Equal(e.clock.Now().Add(2*time.Hour+5*time.Minute)) {
		t.Fatalf("CheckHealth = %+v, %v", h, err)
	}

	// Agent read fails (agent gone) -> pane read.
	e.h.errs["AgentRead"] = &herdr.Error{Method: "agent.read", Code: herdr.CodeAgentNotFound, Message: "x"}
	e.h.reads[ws.Panes[RoleJudge]] = "pane text"
	if text, err = e.m.ReadRecent(e.ctx, s, 40); err != nil || text != "pane text" {
		t.Fatalf("fallback ReadRecent = %q, %v", text, err)
	}
	// Shell panes are read directly.
	if _, err := e.m.ReadRecent(e.ctx, e.session(RoleCodexReview), 40); err != nil {
		t.Fatal(err)
	}
	if got := e.h.callsWith("PaneRead " + ws.Panes[RoleCodexReview]); len(got) != 1 {
		t.Fatalf("shell pane read = %v", got)
	}
}

func TestCountWorking(t *testing.T) {
	agents := []herdr.AgentInfo{
		{Agent: "codex", AgentStatus: herdr.StatusWorking},
		{Agent: "codex", AgentStatus: herdr.StatusIdle},
		{Agent: "claude", AgentStatus: herdr.StatusWorking},
		{Agent: "codex", AgentStatus: herdr.StatusBlocked},
	}
	if n := CountWorking(agents, KindCodex); n != 1 {
		t.Fatalf("CountWorking codex = %d, want 1", n)
	}
}
