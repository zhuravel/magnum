package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
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

// The live case (talkable#11483, 2026-10-13): RSpec finished red on the head
// 15 minutes after magnum's re-review of it said "CI is red: Dependency Audit
// failed, unrelated", with two failures the PR causes, which a colleague's
// review reported an hour later. The judge check the engine starts for it
// (RoundInput.CIRed, a same-head round) tells the judge that CI finished red
// after its review, with the head's failing checks in failing-checks.json as
// always; in a fresh session (recovery) too. A same-head round that is no
// CI follow-up, and a CI flag on a round with new commits, say nothing of it.
func TestACIFollowUpTellsTheJudgeCIFinishedRedAfterItsReview(t *testing.T) {
	failed := store.CheckResult{Name: "rspec", State: store.CheckFailed, Workflow: "RSpec", At: t0.Add(-10 * time.Minute)}
	const ciRed = "CI finished red on this head after your review: `failing_checks` lists the failed checks"
	run := func(t *testing.T, kind string, sameHead, ciRedFlag bool) (agents.JudgeData, string, string) {
		t.Helper()
		e := newEnv(t)
		e.setCI(store.CIStatus{SHA: target, State: "FAILURE", Total: 1, Complete: true, Checks: []store.CheckResult{failed}})
		fa := captureJudge(e)
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(604, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t)}
		in := e.input(kind)
		in.Round, in.SameHead, in.CIRed, in.Roles = 2, sameHead, ciRedFlag, []config.Role{e.judgeRole()}
		if sameHead {
			in.Previous = &PreviousReview{ID: 601, Event: "COMMENTED", SHA: target, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
		} else {
			in.Previous = &PreviousReview{ID: 601, Event: "COMMENTED", SHA: prevSHA}
		}
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil || res.Outcome != OutcomePosted {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		return fa.data(t), e.ag.submitsFor(agents.RoleJudge)[0].Text, filepath.Join(res.ReportDir, FailingChecksFile)
	}
	for _, kind := range []string{KindRereview, KindRecovery} {
		t.Run(kind, func(t *testing.T) {
			jd, prompt, file := run(t, kind, true, true)
			if !jd.CIRed || !jd.SameHead {
				t.Fatalf("judge data: CI red %v, same head %v", jd.CIRed, jd.SameHead)
			}
			mustContain(t, "judge prompt", prompt, ciRed, "Post exactly one review on this head", "\nfailing_checks: "+file+"\n")
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("%s: %v", FailingChecksFile, err)
			}
		})
	}
	t.Run("a same-head round without it", func(t *testing.T) {
		jd, prompt, _ := run(t, KindRereview, true, false)
		if jd.CIRed || strings.Contains(prompt, ciRed) {
			t.Fatalf("CI red %v in a same-head round that is no CI follow-up:\n%s", jd.CIRed, prompt)
		}
	})
	t.Run("new commits", func(t *testing.T) {
		jd, prompt, _ := run(t, KindRereview, false, true)
		if jd.CIRed || strings.Contains(prompt, ciRed) {
			t.Fatalf("CI red %v in a re-review of new commits:\n%s", jd.CIRed, prompt)
		}
	})
}
