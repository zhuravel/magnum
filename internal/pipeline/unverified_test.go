package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
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
	// The adopted review is a verified one: it gets the identity's footer.
	if len(e.gh.updates) != 1 || e.gh.updates[0].ID != 800 || !strings.Contains(e.gh.updates[0].Body, "\n\n"+footerMarker+"\n**Reviewed commit:**") {
		t.Fatalf("footer on the adopted review: %+v", e.gh.updates)
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

// A round that ends (an abort, the daemon stopping) while it adopts a review
// it could not verify still finishes the review's follow-ups, as after a
// review verified at once: the footer edit, and the dismissal of the stale
// CHANGES_REQUESTED the adopted review supersedes.
func TestAnAdoptedReviewsFollowUpsOutliveTheRound(t *testing.T) {
	e := newEnv(t)
	marker := e.unverifiedRound(e.judgePosts(802, "COMMENTED", "COMMENT"))
	in := e.nextRound()
	in.Previous = &PreviousReview{ID: 900, Event: "CHANGES_REQUESTED", SHA: prevSHA, SubmittedAt: t0.Add(-3 * time.Hour)}
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.gh.onUpdate = cancel

	res, _ := e.r.RunRound(ctx, in)
	if ctx.Err() == nil {
		t.Fatal("the footer edit never ran: the round was not cancelled during the follow-ups")
	}
	if res.ReviewID != 802 || res.JudgeRunID != marker {
		t.Fatalf("adopting round: result = %+v", res)
	}
	if len(e.gh.updates) != 1 || e.gh.updates[0].ID != 802 {
		t.Errorf("footer edits = %+v, want one of review 802", e.gh.updates)
	}
	if len(e.gh.dismissed) != 1 || e.gh.dismissed[0].ID != 900 {
		t.Fatalf("dismissed %+v, want review 900", e.gh.dismissed)
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
		t.Fatalf("the review of %s carries the marker of the unverified run on %s", textx.ShortSHA(head2), textx.ShortSHA(target))
	}
}

// gaveUpRound runs a round whose judge ends its turn and then its nudge's
// without a result, so the round ends needing attention, and returns the
// marker its review was asked to carry.
func (e *env) gaveUpRound() string {
	e.t.Helper()
	e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently(), endSilently()}
	e.ag.reads[agents.RoleJudge] = "Selected model is at capacity.\n"
	res, err := e.r.RunRound(e.ctx, e.nextRound())
	if err != nil || res.Outcome != OutcomeNeedsAttention || !strings.HasPrefix(res.Error, agents.JudgeNoResult) || res.JudgeRunID == "" {
		e.t.Fatalf("the round that gave up: result = %+v, err = %v", res, err)
	}
	return res.JudgeRunID
}

// postsLate is the judge's late work after its round gave up on its
// result: its review on GitHub with the round's marker, and its result file.
func (e *env) postsLate(marker string, p judgePost) {
	e.t.Helper()
	path := filepath.Join(e.reportDir(), "codex-judge.json")
	if err := p.behavior(e.t)(e.ag, store.Run{ID: marker, ReportPath: &path}, "run_id "+marker); err != nil {
		e.t.Fatal(err)
	}
}

// On 2026-11-05 a judge's turn and its nudge's ended without its result,
// and the round ended needing attention; the judge worked on and posted its
// review, with the round's marker, 15 minutes later, while the PR stayed
// parked as failed. The next round on that head finds that review by the
// marker and adopts it with the late result file's findings, prompting
// nothing, as it adopts a review GitHub could not verify.
func TestNextRoundAdoptsTheLateReviewOfAJudgeItsRoundGaveUpOn(t *testing.T) {
	e := newEnv(t)
	marker := e.gaveUpRound()
	late := e.judgePosts(810, "CHANGES_REQUESTED", "REQUEST_CHANGES")
	late.keepWorking = true
	e.postsLate(marker, late)
	submits, shells := len(e.ag.submits), len(e.ag.codexCalls)

	res, err := e.r.RunRound(e.ctx, e.nextRound())
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 810 || res.JudgeRunID != marker || res.Round != 1 {
		t.Fatalf("adopting round: result = %+v, err = %v", res, err)
	}
	if res.Event != "CHANGES_REQUESTED" || res.Findings["P2"] != 2 {
		t.Fatalf("adopted review: %+v, want the late result's findings", res)
	}
	if len(e.ag.submits) != submits || len(e.ag.codexCalls) != shells {
		t.Fatalf("the adopting round prompted: submits %d -> %d, shell runs %d -> %d", submits, len(e.ag.submits), shells, len(e.ag.codexCalls))
	}
	run, err := e.st.RunByID(e.ctx, marker)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != store.RunVerified || store.Deref(run.ReviewID) != 810 || run.Error != nil {
		t.Fatalf("the judge's run after adoption: %+v", run)
	}
	var adopted []store.Event
	for _, ev := range e.events() {
		if ev.Kind == "round.adopted" {
			adopted = append(adopted, ev)
		}
	}
	if len(adopted) != 1 || !strings.Contains(adopted[0].Message, "after its round ended without its result") {
		t.Fatalf("round.adopted events %+v, want one naming the late review", adopted)
	}
}

// Without a late review on GitHub, the round after the one that gave up on
// its judge's result runs as any round, and its judge is given that round's
// marker: a judge still at work on the old round that posts with it is
// found before a second review is posted. The round says so, as news, not
// as a failure.
func TestARoundAfterAJudgeGaveUpGivesItsJudgeTheMarkerWhenNoLateReviewLanded(t *testing.T) {
	e := newEnv(t)
	marker := e.gaveUpRound()

	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(811, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.nextRound())
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 811 || res.JudgeRunID != marker {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if got := markerRunID(t, judge[len(judge)-1].Text); got != marker {
		t.Fatalf("the judge's run_id = %s, want the run of the round that gave up %s", got, marker)
	}
	var said []store.Event
	for _, ev := range e.events() {
		if ev.Kind == "round.unverified" {
			said = append(said, ev)
		}
	}
	if len(said) != 1 || said[0].Level != "info" || !strings.Contains(said[0].Message, "stopped without its result and it is not on GitHub") {
		t.Fatalf("round.unverified events %+v, want one info event naming the judge that stopped", said)
	}
}

// A round whose judge is another one (a judge_fallback in the place of a
// kind a shared launch failure holds) adopts the late review of the judge
// whose round gave up on its result all the same: the review is the head's,
// by the round's identity, whichever judge wrote it.
func TestARoundOfAnotherJudgeAdoptsTheLateReview(t *testing.T) {
	e := newEnv(t)
	marker := e.gaveUpRound()
	late := e.judgePosts(812, "COMMENTED", "COMMENT")
	late.keepWorking = true
	e.postsLate(marker, late)
	fallback, ok := e.cfg.RoleByNameOrAlias(nil, "claude-judge")
	if !ok {
		t.Fatal("no claude-judge in the defaults")
	}
	in := e.nextRound()
	for _, r := range e.cfg.RolesFor(nil) {
		if r.Judge {
			r = fallback
		}
		in.Roles = append(in.Roles, r)
	}
	submits := len(e.ag.submits)

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 812 || res.JudgeRunID != marker {
		t.Fatalf("the other judge's round: result = %+v, err = %v", res, err)
	}
	if len(e.ag.submits) != submits {
		t.Fatalf("the adopting round prompted: submits %d -> %d", submits, len(e.ag.submits))
	}
}
