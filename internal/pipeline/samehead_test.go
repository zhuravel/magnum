package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
)

// A re-review of the head the judge last reviewed (RoundInput.SameHead):
// the judge alone gets one prompt, though the judge's own pass is on, at its
// rereview effort, saying the head is unchanged instead of asking for the
// new commits; in a fresh session (recovery) too. An ordinary re-review
// says nothing of it.
func TestASameHeadReReviewPromptsTheJudgeOnceThatTheHeadIsUnchanged(t *testing.T) {
	for _, kind := range []string{KindRereview, KindRecovery} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			fa := captureJudge(e)
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
			judge := e.judgeRole()
			in := e.ownInput(kind)
			in.Round, in.SameHead, in.Roles = 2, true, []config.Role{judge}
			in.Previous = &PreviousReview{ID: 601, Event: "CHANGES_REQUESTED", SHA: target, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
			res, err := e.r.RunRound(e.ctx, in)
			if err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			jd := fa.data(t)
			if !jd.SameHead || jd.Phase != "" || len(jd.Reports) != 0 || jd.PreviousHeadSHA != target {
				t.Fatalf("judge data: same head %v phase %q reports %d previous head %s", jd.SameHead, jd.Phase, len(jd.Reports), jd.PreviousHeadSHA)
			}
			if want := judge.EffortFor(true); jd.Effort != want || want == judge.Effort {
				t.Fatalf("judge effort = %q, want its rereview effort %q (effort %q)", jd.Effort, want, judge.Effort)
			}
			submits := e.ag.submitsFor(agents.RoleJudge)
			if len(submits) != 1 || res.OwnPass != nil {
				t.Fatalf("judge submits = %d, own pass %+v: want one prompt", len(submits), res.OwnPass)
			}
			prompt := submits[0].Text
			mustContain(t, "judge prompt", prompt, "re-read the replies and the comments since your last review, re-decide each earlier finding "+
				"under the reply contract, and run no check your last review already ran on this head. Post one short review.")
			for _, gone := range []string{"New commits were pushed", "git log --oneline", "andidate reports for this head"} {
				if strings.Contains(prompt, gone) {
					t.Errorf("judge prompt says %q:\n%s", gone, prompt)
				}
			}
		})
	}

	e := newEnv(t)
	fa := captureJudge(e)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(603, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindRereview)
	in.Round, in.Roles = 2, []config.Role{e.judgeRole()}
	in.Previous = &PreviousReview{ID: 601, Event: "COMMENTED", SHA: prevSHA}
	if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if jd := fa.data(t); jd.SameHead {
		t.Fatal("an ordinary re-review is a same-head one")
	}
	mustContain(t, "ordinary judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "New commits were pushed", "git log --oneline")
}
