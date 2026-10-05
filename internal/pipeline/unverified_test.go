package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// nextRound is an initial round numbered after the PR's latest, as the
// engine dispatches it.
func (e *env) nextRound() RoundInput {
	in := e.input(KindInitial)
	in.Round = 0
	return in
}

// unverifiedRound runs a round whose judge posts (post decides whether the
// review reaches GitHub) while GitHub fails every verification attempt, and
// returns the marker its review was asked to carry.
func (e *env) unverifiedRound(post judgePost) string {
	e.t.Helper()
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(e.t)}
	e.gh.failLists = verifyAttempts
	res, err := e.r.RunRound(e.ctx, e.nextRound())
	if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "verify on GitHub") {
		e.t.Fatalf("unverified round: result = %+v, err = %v", res, err)
	}
	if res.JudgeRunID == "" {
		e.t.Fatalf("unverified round: no judge run in %+v", res)
	}
	return res.JudgeRunID
}

// The judge posted, but GitHub failed every verification: the round ended in
// error and the PR is queued again. The next round on the same head finds
// the review by the earlier run's marker and adopts it, prompting nothing,
// so no second review is posted on the head. A later round on that head
// (a forced re-review) is a round like any other.
func TestNextRoundAdoptsAReviewItCouldNotVerify(t *testing.T) {
	e := newEnv(t)
	marker := e.unverifiedRound(e.judgePosts(800, "CHANGES_REQUESTED", "REQUEST_CHANGES"))
	submits, shells := len(e.ag.submits), len(e.ag.codexCalls)

	res, err := e.r.RunRound(e.ctx, e.nextRound())
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 800 || res.JudgeRunID != marker || res.Round != 1 {
		t.Fatalf("adopting round: result = %+v, err = %v", res, err)
	}
	if res.Event != "CHANGES_REQUESTED" || res.Findings["P2"] != 2 || res.ReviewCommit != target {
		t.Fatalf("adopted review: %+v", res)
	}
	if len(e.ag.submits) != submits || len(e.ag.codexCalls) != shells {
		t.Fatalf("the adopting round prompted: submits %d -> %d, shell runs %d -> %d", submits, len(e.ag.submits), shells, len(e.ag.codexCalls))
	}
	run, err := e.st.RunByID(e.ctx, marker)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != store.RunVerified || store.Deref(run.Outcome) != OutcomePosted || store.Deref(run.ReviewID) != 800 || run.Error != nil {
		t.Fatalf("judge run after adoption: %+v", run)
	}

	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(801, "COMMENTED", "COMMENT").behavior(t)}
	res, err = e.r.RunRound(e.ctx, e.nextRound())
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 801 || res.JudgeRunID == marker || res.Round != 2 {
		t.Fatalf("later round: result = %+v, err = %v", res, err)
	}
}

// The judge said it posted, but the review is not on GitHub when the next
// round looks: the round runs, and its judge is given the earlier run's id
// as its marker, so before posting it finds its own earlier review if it is
// there after all. Its review carries that marker and is verified by it.
func TestNextRoundGivesTheJudgeTheUnverifiedMarker(t *testing.T) {
	e := newEnv(t)
	lost := e.judgePosts(800, "CHANGES_REQUESTED", "REQUEST_CHANGES")
	lost.gh = nil // the result file says posted; GitHub never got it
	marker := e.unverifiedRound(lost)

	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(801, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.nextRound())
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 801 || res.JudgeRunID != marker {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if got := markerRunID(t, judge[len(judge)-1].Text); got != marker {
		t.Fatalf("the judge's run_id = %s, want the unverified run %s", got, marker)
	}
	if len(e.ag.shellCallsFor(agents.RoleCodexReview)) != 2 {
		t.Fatal("the round did not run its reviewers")
	}
}

// Only a review magnum could not verify on the round's own head is looked
// for: after a push, the next round reviews the new head as usual.
func TestUnverifiedReviewOfAnotherHeadIsNotAdopted(t *testing.T) {
	e := newEnv(t)
	marker := e.unverifiedRound(e.judgePosts(800, "CHANGES_REQUESTED", "REQUEST_CHANGES"))
	in := e.nextRound()
	in.TargetSHA = "def4567abc8901def4567abc8901def4567abc89"
	e.git.head = in.TargetSHA
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(801, "COMMENTED", "COMMENT").behavior(t)}
	e.ag.behaviors[agents.RoleClaude] = []behavior{writeReport("## P2 something\n")}
	res, _ := e.r.RunRound(e.ctx, in)
	if res.ReviewID == 800 {
		t.Fatalf("adopted the review of another head: %+v", res)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if got := markerRunID(t, judge[len(judge)-1].Text); got == marker {
		t.Fatalf("the judge got the marker of another head's run %s", marker)
	}
}

// A restart on a newer head drops the earlier marker: it stands for a review
// of the old head, which must never pass for the new head's.
func TestRestartDropsTheUnverifiedMarker(t *testing.T) {
	e := newEnv(t)
	lost := e.judgePosts(800, "CHANGES_REQUESTED", "REQUEST_CHANGES")
	lost.gh = nil
	marker := e.unverifiedRound(lost)

	in := e.nextRound()
	e.withRestarts(&in, 2)
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushThen(e, head2, writeReport("## old head\n")), writeReport("## new head\n")}
	post := e.judgePosts(801, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted || res.TargetSHA != head2 || res.ReviewID != 801 {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if res.JudgeRunID == marker {
		t.Fatalf("the review of %s carries the marker of the unverified run on %s", short(head2), short(target))
	}
}
