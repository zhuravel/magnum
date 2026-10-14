package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// neverUp makes the judge's next n prompts fail as herdr answered a fresh
// judge whose agent it never registered (a live case: agent.start answered
// ok in 11 ms, the CLI never came up, and the prompt failed with
// agent_not_ready after Submit's wait) and gives in a RestartJudge that
// counts its calls and, as the engine's does, marks the gone session lost
// and starts the judge again in a new live session.
func neverUp(t *testing.T, e *env, in *RoundInput, n int) *int {
	t.Helper()
	e.ag.notReady[agents.RoleJudge] = n
	restarts := new(int)
	in.RestartJudge = func(ctx context.Context) error {
		*restarts++
		if err := e.st.TransitionSession(ctx, e.sess[agents.RoleJudge].ID, nil, store.SessionLost, nil); err != nil {
			return err
		}
		e.addSession(e.r.Config.JudgeFor(nil))
		return nil
	}
	return restarts
}

// A judge whose agent herdr never registered is started once more, for
// the own pass and for the candidates prompt alike, and the round goes on;
// a second failure ends the round as before.
func TestAJudgeThatNeverComesUpIsStartedOnceMore(t *testing.T) {
	t.Run("own pass", func(t *testing.T) {
		e := newEnv(t)
		e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), e.judgePosts(811, "COMMENTED", "COMMENT").behavior(t)}
		in := e.ownInput(KindInitial)
		restarts := neverUp(t, e, &in, 1)
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 811 {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		if *restarts != 1 {
			t.Fatalf("restarts = %d, want 1", *restarts)
		}
		own := e.runsOf(agents.RoleJudge, store.RunOwnPass)
		if len(own) != 2 || own[0].State != store.RunFailed || own[1].State != store.RunVerified {
			t.Fatalf("own-pass runs = %+v, want the unregistered one failed and the second verified", own)
		}
		if res.OwnPass == nil || res.OwnPass.Status != ReportOK || res.OwnPass.RunID != own[1].ID {
			t.Fatalf("own pass = %+v", res.OwnPass)
		}
		if evs := e.eventsOf("round.judge_restarted"); len(evs) != 1 || !strings.Contains(evs[0].Message, herdr.CodeAgentNotReady) {
			t.Errorf("round.judge_restarted events = %v", evs)
		}
	})
	t.Run("own pass, never up again", func(t *testing.T) {
		e := newEnv(t)
		e.ag.hangs = map[agents.Role]bool{agents.RoleClaude: true} // at work until the round cuts it
		in := e.ownInput(KindInitial)
		restarts := neverUp(t, e, &in, 2)
		res, err := e.r.RunRound(e.ctx, in)
		if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "also after a restart of the judge") ||
			!strings.Contains(res.Error, herdr.CodeAgentNotReady) {
			t.Fatalf("RunRound = %+v, %v; want the round ended at once", res, err)
		}
		if *restarts != 1 {
			t.Fatalf("restarts = %d, want 1", *restarts)
		}
	})
	t.Run("candidates prompt", func(t *testing.T) {
		e := newEnv(t)
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(812, "COMMENTED", "COMMENT").behavior(t)}
		in := e.input(KindInitial)
		restarts := neverUp(t, e, &in, 1)
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 812 {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		if *restarts != 1 {
			t.Fatalf("restarts = %d, want 1", *restarts)
		}
		runs := e.runsOf(agents.RoleJudge, KindInitial)
		if len(runs) != 2 || runs[0].State != store.RunAbandoned || runs[0].SubmittedAt != nil || runs[1].State != store.RunVerified {
			t.Fatalf("judge runs = %+v, want the unregistered one abandoned unsent and the second verified", runs)
		}
		// The second run is the round's marker: the engine's crash recovery
		// and the unverified-review search both skip a judge run never sent.
		if res.JudgeRunID != runs[1].ID {
			t.Fatalf("JudgeRunID = %s, want the second run %s", res.JudgeRunID, runs[1].ID)
		}
		submits := e.ag.submitsFor(agents.RoleJudge)
		if len(submits) != 2 || markerRunID(t, submits[0].Text) != runs[0].ID || markerRunID(t, submits[1].Text) != runs[1].ID {
			t.Fatalf("judge prompts = %d, want the candidates twice, each quoting its run", len(submits))
		}
		if evs := e.eventsOf("round.judge_restarted"); len(evs) != 1 || !strings.Contains(evs[0].Message, herdr.CodeAgentNotReady) {
			t.Errorf("round.judge_restarted events = %v", evs)
		}
	})
	t.Run("candidates prompt, then a nudge", func(t *testing.T) {
		e := newEnv(t)
		e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently(), e.judgePosts(813, "COMMENTED", "COMMENT").behavior(t)}
		e.ag.reads[agents.RoleJudge] = "Done reading the diff.\n"
		in := e.input(KindInitial)
		neverUp(t, e, &in, 1)
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil || res.Outcome != OutcomePosted || !res.Nudged || res.ReviewID != 813 {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		runs := e.runsOf(agents.RoleJudge, KindInitial)
		submits := e.ag.submitsFor(agents.RoleJudge)
		if len(runs) != 2 || len(submits) != 3 || markerRunID(t, submits[2].Text) != runs[1].ID {
			t.Fatalf("judge runs %d, prompts %d: want the nudge to quote the new run %s", len(runs), len(submits), runs[len(runs)-1].ID)
		}
	})
	t.Run("candidates prompt, never up again", func(t *testing.T) {
		e := newEnv(t)
		in := e.input(KindInitial)
		restarts := neverUp(t, e, &in, 2)
		res, err := e.r.RunRound(e.ctx, in)
		if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "judge prompt failed") ||
			!strings.Contains(res.Error, herdr.CodeAgentNotReady) {
			t.Fatalf("RunRound = %+v, %v; want the round ended in error", res, err)
		}
		if *restarts != 1 {
			t.Fatalf("restarts = %d, want 1", *restarts)
		}
		for _, r := range e.runsOf(agents.RoleJudge, KindInitial) {
			if r.State != store.RunAbandoned && r.State != store.RunFailed {
				t.Errorf("judge run %s is %s, want it ended", r.ID, r.State)
			}
		}
	})
}
