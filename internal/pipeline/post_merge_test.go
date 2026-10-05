package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
)

// A post-merge round (RoundInput.PostMerge, GitHub merged the PR before
// magnum reviewed its head): the judge's prompt says `post_merge: true`,
// names the previous head of a re-review, and asks for COMMENT both ways
// although the repository's policy says APPROVE and REQUEST_CHANGES; and the
// round dismisses nothing, not even its own stale CHANGES_REQUESTED, which
// the same round on an open PR dismisses (the control). The reviewers'
// prompts say the PR is merged, so their review does not stop at a closed
// PR.
func TestPostMergeRoundAsksForACommentAndDismissesNothing(t *testing.T) {
	for _, kind := range []string{KindRereview, KindInitial} {
		for _, postMerge := range []bool{true, false} {
			name := kind + "/open"
			if postMerge {
				name = kind + "/post-merge"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				e.cfg.Repos = append(e.cfg.Repos, config.Repo{Repo: "talkable/talkable", NoFindingsEvent: "APPROVE", BlockingEvent: "REQUEST_CHANGES"})
				fa := captureJudge(e)
				e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(506, "COMMENTED", "COMMENT").behavior(t)}
				in := e.input(kind)
				in.PostMerge = postMerge
				if kind == KindRereview {
					in.Round = 2
					in.Previous = &PreviousReview{ID: 900, Event: "CHANGES_REQUESTED", SHA: prevSHA, SubmittedAt: t0.Add(-3 * time.Hour),
						Login: "talkable[bot]"}
				}
				res, err := e.r.RunRound(e.ctx, in)
				if err != nil || res.Outcome != OutcomePosted || res.Event != "COMMENTED" {
					t.Fatalf("RunRound = %+v, %v", res, err)
				}
				jd := fa.data(t)
				prompt := e.ag.submitsFor(agents.RoleJudge)[0].Text
				claude := e.ag.submitsFor(agents.RoleClaude)[0].Text
				if merged := strings.Contains(claude, "The PR is already merged; review it anyway."); merged != postMerge {
					t.Fatalf("claude-review's prompt says the PR is merged: %v, want %v:\n%s", merged, postMerge, claude)
				}
				if !postMerge {
					if jd.PostMerge || jd.NoFindingsEvent != "APPROVE" || jd.BlockingEvent != "REQUEST_CHANGES" {
						t.Fatalf("open PR: judge data post_merge %v, events %s/%s, want the repository's APPROVE/REQUEST_CHANGES",
							jd.PostMerge, jd.NoFindingsEvent, jd.BlockingEvent)
					}
					if strings.Contains(prompt, "post_merge") {
						t.Fatalf("open PR: the judge prompt names post_merge:\n%s", prompt)
					}
					if kind == KindRereview && (len(e.gh.dismissed) != 1 || res.DismissedReviewID != 900) {
						t.Fatalf("open PR: dismissed %+v (result %d), want the stale review 900", e.gh.dismissed, res.DismissedReviewID)
					}
					return
				}
				if !jd.PostMerge || jd.NoFindingsEvent != "COMMENT" || jd.BlockingEvent != "COMMENT" {
					t.Fatalf("judge data post_merge %v, events %s/%s, want true and COMMENT/COMMENT", jd.PostMerge, jd.NoFindingsEvent, jd.BlockingEvent)
				}
				mustContain(t, "judge prompt", prompt, "\npost_merge: true\n", "\nno_findings_event: COMMENT\n", "\nblocking_event: COMMENT\n")
				if kind == KindRereview {
					mustContain(t, "judge prompt", prompt, "mode: rereview", "\nprevious_head_sha: "+prevSHA+"\n", "previous_review_id: 900")
				}
				if len(e.gh.dismissed) != 0 || res.DismissedReviewID != 0 {
					t.Fatalf("dismissed %+v (result %d), want nothing after a post-merge review", e.gh.dismissed, res.DismissedReviewID)
				}
				if evs := eventsOfKind(e.events(), "round.dismiss"); len(evs) != 0 {
					t.Fatalf("round.dismiss events: %+v", evs)
				}
			})
		}
	}
}
