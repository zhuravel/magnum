package pipeline

import (
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

func TestBlockedWithoutTrustDialogKeepsOutcomes(t *testing.T) {
	e := newEnv(t)
	e.ag.blocked[agents.RoleClaude] = 1
	e.ag.blocked[agents.RoleJudge] = 1
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if got := res.Reports[agents.RoleClaude]; got.Status != string(agents.HealthBlocked) {
		t.Fatalf("claude report = %+v, want blocked", got)
	}
	if res.Outcome != OutcomeBlocked || res.Nudged {
		t.Fatalf("result = %+v, want blocked", res)
	}
	if len(e.ag.submitsFor(agents.RoleJudge)) != 1 || len(e.ag.submitsFor(agents.RoleClaude)) != 1 {
		t.Fatalf("judge submits %d, claude submits %d", len(e.ag.submitsFor(agents.RoleJudge)), len(e.ag.submitsFor(agents.RoleClaude)))
	}
	if r := e.runOf(agents.RoleJudge, store.RunInitial); r.State != store.RunFailed {
		t.Fatalf("judge run = %+v", r)
	}
}

func TestJudgePaneTrustDialogIsBlocked(t *testing.T) {
	e := newEnv(t)
	// The judge ended its turn quietly, and its pane shows the trust dialog.
	e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently()}
	e.ag.reads[agents.RoleJudge] = "Trust this folder?\n› 1. Trust and continue\n  2. Quit\n"
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeBlocked || res.Nudged {
		t.Fatalf("result = %+v", res)
	}
}
