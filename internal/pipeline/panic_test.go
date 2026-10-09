package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// panics is a role's turn that panics in the round's own code (here, the
// fake agent's Submit, on the role's stage goroutine).
func panics(msg string) behavior {
	return func(*fakeAgents, store.Run, string) error { panic(msg) }
}

// A reviewer whose stage goroutine panics ends as a failed report, its run
// failed: the other reviewers and the judge go on, the round posts without
// it, and the panic is recorded (round.panic). It never takes the daemon
// down with every other round in flight.
func TestAReviewerThatPanicsFailsAloneAndTheRoundGoesOn(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleClaude] = []behavior{panics("nil map in the reviewer")}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(520, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 520 {
		t.Fatalf("result = %+v", res)
	}
	claude := res.Reports[agents.RoleClaude]
	if claude.Status != ReportFailed || !strings.Contains(claude.Detail, "panicked: nil map in the reviewer") {
		t.Errorf("claude report = %+v", claude)
	}
	if codex := res.Reports[agents.RoleCodexReview]; codex.Status != ReportOK {
		t.Errorf("codex report = %+v, want ok: another role's panic does not touch it", codex)
	}
	if run := e.runOf(agents.RoleClaude, store.RunInitial); run.State != store.RunFailed || store.Deref(run.Outcome) != ReportFailed {
		t.Errorf("claude run = %s / %v", run.State, store.Deref(run.Outcome))
	}
	if evs := e.eventsOf("round.panic"); len(evs) != 1 || evs[0].Level != "error" || !strings.Contains(evs[0].Message, "claude-review") {
		t.Errorf("round.panic events = %+v", evs)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) != 1 {
		t.Fatalf("judge submits = %d", len(judge))
	}
	mustContain(t, "judge prompt", judge[0].Text, "claude-review: missing (failed)")
}

// The judge's own pass panicking on its goroutine is a failed own pass: the
// reviewers go on and the candidates prompt asks for the pass, as after any
// failed own pass.
func TestAnOwnPassThatPanicsIsAFailedOwnPass(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{panics("own pass bug"), e.judgePosts(521, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 521 {
		t.Fatalf("result = %+v", res)
	}
	if res.OwnPass == nil || res.OwnPass.Status != ReportFailed || !strings.Contains(res.OwnPass.Detail, "panicked: own pass bug") {
		t.Errorf("own pass = %+v", res.OwnPass)
	}
	if run := e.runOf(agents.RoleJudge, store.RunOwnPass); run.State != store.RunFailed {
		t.Errorf("own-pass run = %s", run.State)
	}
	for _, role := range []agents.Role{agents.RoleClaude, agents.RoleCodexReview} {
		if rep := res.Reports[role]; rep.Status != ReportOK {
			t.Errorf("%s report = %+v", role, rep)
		}
	}
	if evs := e.eventsOf("round.panic"); len(evs) != 1 {
		t.Errorf("round.panic events = %+v", evs)
	}
}

// A file's history whose read panics fails the history, which only warns:
// the round goes on without history.json.
func TestAHistoryThatPanicsOnlyWarns(t *testing.T) {
	e := newEnv(t)
	e.git.modified = []string{"app/models/order.rb"}
	e.git.logPanics = true
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(522, "COMMENTED", "COMMENT").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	e.noHistory(e.reportDir())
	evs := e.eventsOf("round.history")
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "panicked") || strings.Contains(evs[0].Message, "order.rb") {
		t.Fatalf("round.history events = %+v", evs)
	}
}
