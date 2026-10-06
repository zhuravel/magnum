package engine

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// sameHeadHarness is a harness with [triage] on (its model keeps every
// role) whose PR #2 magnum reviewed at b1; it returns how many triage
// calls round 1 made and a function that counts them now.
func sameHeadHarness(t *testing.T) (*harness, int, func() int) {
	t.Helper()
	h, model := triageHarness(t, modelAnswers(`{"run": ["codex-judge", "claude-review", "codex-review", "claude-simplify"]}`))
	reqSetFiles(h, "master...b1", codePatch(5))
	h.reviewedPR(2, "b1")
	calls := func() int { return len(model.Calls) }
	return h, calls(), calls
}

// The live case (2026-10-06): `magnum review` on a PR whose head magnum had
// reviewed, to re-read an author's reply, ran claude-review, codex-review and
// the judge for 17 minutes though no code changed. A re-review with no new
// commits, forced or requested on GitHub, runs the judge alone now, like a
// delta check: no triage, no reruns, no restarts, and an event that says why.
func TestAReReviewOfTheSameHeadRunsTheJudgeAlone(t *testing.T) {
	for name, ask := range map[string]func(h *harness){
		"magnum review": func(h *harness) {
			h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
			h.tick()
		},
		"a review request": func(h *harness) {
			reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
			h.advance(time.Minute) // the request's debounce
			h.tick()
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, triaged, calls := sameHeadHarness(t)
			if triaged == 0 {
				t.Fatal("round 1 was not triaged")
			}
			h.advance(time.Minute)
			ask(h)
			ins := h.rd.all()
			if len(ins) != 2 {
				t.Fatalf("rounds = %d, want the re-review", len(ins))
			}
			in := ins[1]
			if in.Kind != pipeline.KindRereview || in.TargetSHA != "b1" || !in.SameHead || in.DeltaCheck != nil || in.MaxRestarts != 0 ||
				!slices.Equal(roleNames(in.Roles), []string{"codex-judge"}) {
				t.Fatalf("input: kind %s target %s same head %v check %v restarts %d roles %v",
					in.Kind, in.TargetSHA, in.SameHead, in.DeltaCheck, in.MaxRestarts, roleNames(in.Roles))
			}
			if got := h.workspaceRoles(in.PR.ID); !slices.Equal(got, []string{"codex-judge"}) {
				t.Fatalf("workspace roles = %v, want the judge alone", got)
			}
			if n := calls(); n != triaged {
				t.Fatalf("triage calls = %d, want round 1's %d", n, triaged)
			}
			if evs := approvalEvents(t, h, 2, "round.rerun_role"); len(evs) != 0 {
				t.Fatalf("round.rerun_role events: %+v", evs)
			}
			same := approvalEvents(t, h, 2, "round.same_head")
			if len(same) != 1 || same[0].Message != "same head: the judge re-decides the threads (no commits since b1)" {
				t.Fatalf("round.same_head events: %+v", same)
			}
			starts := approvalEvents(t, h, 2, "engine.round_start")
			if len(starts) != 2 || starts[1].Message != "rereview round in review1 at b1: codex-judge" {
				t.Fatalf("round starts: %+v", starts)
			}
			h.wantState(2, store.PRReviewed)
		})
	}
}

// A request that names roles (`--role`, `--simplify`) or asks for fresh
// sessions (`--fresh`) keeps the full round on the same head.
func TestAReReviewOfTheSameHeadThatNamesRolesOrFreshSessionsRunsInFull(t *testing.T) {
	for name, p := range map[string]ReviewPayload{
		"--role":     {PRTarget: PRTarget{Ref: "2"}, Roles: []string{"claude-simplify"}},
		"--simplify": {PRTarget: PRTarget{Ref: "2"}, Simplify: true},
		"--fresh":    {PRTarget: PRTarget{Ref: "2"}, Fresh: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.reviewedPR(2, "b1")
			h.advance(time.Minute)
			h.enqueue(ReqReview, p)
			h.tick()
			ins := h.rd.all()
			if len(ins) != 2 || ins[1].SameHead || len(ins[1].Roles) < 3 {
				t.Fatalf("rounds = %d, second same head %v roles %v: want a full re-review", len(ins), ins[len(ins)-1].SameHead, roleNames(ins[len(ins)-1].Roles))
			}
			if got := h.workspaceRoles(ins[1].PR.ID); len(got) < 3 {
				t.Fatalf("workspace roles = %v, want every reviewer", got)
			}
			if evs := approvalEvents(t, h, 2, "round.same_head"); len(evs) != 0 {
				t.Fatalf("round.same_head events: %+v", evs)
			}
		})
	}
}

// A push the checkout finds after the dispatch is a re-review of new
// commits: the round runs in full, with an event that says so.
func TestASameHeadReReviewWhoseCheckoutFindsANewHeadRunsInFull(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.sl.mu.Lock()
	h.sl.moveHead = "b2" // the fetch finds a push the poller has not seen
	h.sl.mu.Unlock()
	h.advance(time.Minute)
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].SameHead || ins[1].TargetSHA != "b2" || len(ins[1].Roles) < 3 {
		t.Fatalf("rounds = %d, second same head %v target %s roles %v: want a full re-review of b2", len(ins), ins[len(ins)-1].SameHead,
			ins[len(ins)-1].TargetSHA, roleNames(ins[len(ins)-1].Roles))
	}
	dropped := approvalEvents(t, h, 2, "round.same_head_dropped")
	if len(dropped) != 1 || dropped[0].Message != "a full round instead of the judge alone: the checkout found b2, not the reviewed b1" {
		t.Fatalf("round.same_head_dropped events: %+v", dropped)
	}
}

// A judge whose session is gone re-decides the threads in a fresh one, as a
// delta check's does: a recovery of the judge alone at its rereview effort,
// which reads its previous review and threads first.
func TestASameHeadReReviewWhoseJudgeLostItsSessionRunsWithAFreshOne(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	parkSessions(h, 2)
	h.advance(time.Minute)
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the re-review", len(ins))
	}
	in := ins[1]
	if in.Kind != pipeline.KindRecovery || !in.SameHead || in.TargetSHA != "b1" || !slices.Equal(roleNames(in.Roles), []string{"codex-judge"}) {
		t.Fatalf("input: kind %s same head %v target %s roles %v", in.Kind, in.SameHead, in.TargetSHA, roleNames(in.Roles))
	}
	if n := h.ag.count(fmt.Sprintf("pane:%d:", in.PR.ID)); n != 0 {
		t.Fatalf("panes added for other roles: %v", h.ag.all())
	}
	h.ag.mu.Lock()
	starts := slices.Clone(h.ag.efforts)
	h.ag.mu.Unlock()
	if last := starts[len(starts)-1]; last != "codex-judge:high:" {
		t.Fatalf("judge started as %q (starts %v), want a fresh session at its rereview effort", last, starts)
	}
	fresh := approvalEvents(t, h, 2, "round.same_head_fresh")
	if len(fresh) != 1 || fresh[0].Message != "same head with a fresh judge session: the judge's session is gone" {
		t.Fatalf("round.same_head_fresh events: %+v", fresh)
	}
}
