package agents

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

const judgeAgent = "mg-11920-codex-judge-5d01cf"

// settleOnResult verifies run id as the round does when the judge's result
// file ends its wait while the agent still works (finishRun: ended_at and
// verified_at now).
func (e *env) settleOnResult(id string) {
	e.t.Helper()
	now := e.clock.Now()
	if err := e.st.TransitionRun(e.ctx, id, []string{store.RunSubmitted, store.RunWorking}, store.RunVerified, func(u *store.RunUpdate) {
		u.Set("outcome", "posted")
		u.Set("ended_at", now)
		u.Set("verified_at", now)
	}); err != nil {
		e.t.Fatal(err)
	}
}

// judgeSettledWhileWorking prompts the judge, lets it work past the
// prompt's grace and verifies its run on its result file while the agent
// still shows working; it returns the run id.
func (e *env) judgeSettledWhileWorking() string {
	e.t.Helper()
	e.started()
	e.clock.Add(3 * time.Minute)
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review")
	if err != nil {
		e.t.Fatal(err)
	}
	e.clock.Add(5 * time.Minute) // past humanGrace after the prompt
	if o := e.observe()[RoleJudge]; o.Kind != "" || o.Status != herdr.StatusWorking {
		e.t.Fatalf("working on the run = %+v", o)
	}
	e.settleOnResult(id)
	return id
}

// The round goes on as soon as the judge's result file is written, while the
// agent may still print its last message: that work is the run's, so it never
// counts as a human typing (no human_active_at, no cooldown), until the agent
// is seen idle; working after that is a human again.
func TestAJudgeFinishingItsTurnAfterItsResultIsNotAHuman(t *testing.T) {
	e := newEnv(t)
	e.judgeSettledWhileWorking()
	for tick := 1; tick <= 2; tick++ {
		e.clock.Add(30 * time.Second)
		if o := e.observe()[RoleJudge]; o.Kind != "" || o.Status != herdr.StatusWorking {
			t.Fatalf("tick %d after the result = %+v, want the run's own work", tick, o)
		}
	}
	if pr := e.reloadPR(); pr.HumanActiveAt != nil {
		t.Fatalf("human_active_at = %v, want none", pr.HumanActiveAt)
	}

	e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != "" {
		t.Fatalf("idle = %+v", o)
	}
	e.h.setAgentStatus(judgeAgent, herdr.StatusWorking)
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != ObsHumanActive {
		t.Fatalf("working after idle = %+v, want human_active", o)
	}
	if pr := e.reloadPR(); pr.HumanActiveAt == nil || !pr.HumanActiveAt.Equal(e.clock.Now()) {
		t.Fatalf("human_active_at = %v", pr.HumanActiveAt)
	}
}

// A reviewer keeps the old rule: working after its run ended, past the
// prompt's grace, is a human.
func TestAReviewerWorkingAfterItsRunEndedIsAHuman(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.clock.Add(3 * time.Minute)
	id, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunInitial, "review")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Add(5 * time.Minute)
	e.observe()
	e.settleOnResult(id)
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleClaude]; o.Kind != ObsHumanActive {
		t.Fatalf("claude = %+v, want human_active", o)
	}
}

// The next prompt to a judge still finishing its turn waits until herdr
// shows the agent idle, then goes out; nothing is typed into the turn.
func TestAPromptWaitsForAJudgeFinishingItsTurn(t *testing.T) {
	e := newEnv(t)
	e.judgeSettledWhileWorking()
	e.m.sleep = func(ctx context.Context, d time.Duration) error {
		e.slept = append(e.slept, d)
		if len(e.slept) == 2 {
			e.h.setAgentStatus(judgeAgent, herdr.StatusIdle)
		}
		return nil
	}
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunRereview, "next")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !slices.Equal(e.slept, []time.Duration{tailPoll, tailPoll}) {
		t.Fatalf("slept %v, want two polls", e.slept)
	}
	calls := e.h.callsWith("")
	var seq []string // from the first prompt on
	for _, c := range calls[slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "AgentPrompt") }):] {
		if strings.HasPrefix(c, "PaneGet") || strings.HasPrefix(c, "AgentPrompt") {
			seq = append(seq, strings.Fields(c)[0])
		}
	}
	want := []string{"AgentPrompt", "PaneGet", "PaneGet", "PaneGet", "AgentPrompt"}
	if !slices.Equal(seq, want) {
		t.Fatalf("calls %v, want %v", seq, want)
	}
	if r := e.run1(id); r.State != store.RunWorking {
		t.Fatalf("run = %+v, want working", r)
	}
	if pr := e.reloadPR(); pr.HumanActiveAt != nil {
		t.Fatalf("human_active_at = %v", pr.HumanActiveAt)
	}
}

// A judge that never stops working refuses the prompt as busy after
// tailWait: nothing is sent, and the run is abandoned, not left pending.
func TestAPromptToAJudgeThatKeepsWorkingIsRefusedAsBusy(t *testing.T) {
	e := newEnv(t)
	e.judgeSettledWhileWorking()
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunRereview, "next")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Prompt = %v, want ErrBusy", err)
	}
	if len(e.h.prompts) != 1 {
		t.Fatalf("prompts = %d, want only the first", len(e.h.prompts))
	}
	if n := len(e.slept); time.Duration(n)*tailPoll != tailWait {
		t.Fatalf("slept %d polls, want %s", n, tailWait)
	}
	if r := e.run1(id); r.State != store.RunAbandoned {
		t.Fatalf("run = %+v, want abandoned", r)
	}
}
