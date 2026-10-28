package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// completedRound is round 1 of the env's PR on target as a completion finds
// it: claude-review's report written and verified, codex-review busy
// (missing in the round's record), and the judge's review 601 posted. It
// returns claude-review's report and the completion of round 1 (round 2).
func completedRound(t *testing.T, e *env) (string, RoundInput) {
	t.Helper()
	dir := e.reportDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(dir, "claude-review.md")
	if err := os.WriteFile(claude, []byte("## P3 round one's claude finding\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ended := t0.Add(-time.Hour)
	for _, r := range []store.Run{
		{Role: string(agents.RoleClaude), State: store.RunVerified, Outcome: new(ReportOK), ReportPath: new(claude)},
		{Role: string(agents.RoleCodexReview), State: store.RunAbandoned, Outcome: new(ReportBusy)},
		{Role: string(agents.RoleJudge), State: store.RunVerified, Outcome: new(OutcomePosted), ReviewID: new(int64(601))},
	} {
		r.PRID, r.Round, r.Kind, r.TargetSHA, r.EndedAt = e.pr.ID, 1, store.RunInitial, target, &ended
		if _, err := e.st.CreateRun(e.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal([]MissingReports{{Round: 1, Head: target, Missing: []MissingReport{{Role: string(agents.RoleCodexReview), Status: ReportBusy}}}})
	if err := e.st.SetKV(e.ctx, KVMissingReports(e.pr.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	in := e.input(KindRereview)
	in.Round, in.SameHead = 2, true
	in.Previous = &PreviousReview{ID: 601, Event: "COMMENTED", SHA: target, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
	in.Complete = &Completion{Round: 1, Base: 1, Roles: []string{string(agents.RoleCodexReview)}}
	return claude, in
}

// A completion of round 1, whose judge went without codex-review's report
// (its pane was busy) and posted a clean review on the head: codex-review
// runs again and nobody else does; the judge, in the same-head re-review
// prompt, gets claude-review's report from round 1 (the file its verified
// run wrote, never set aside) and codex-review's late one, marked late, and
// is told to decide the late report's findings while the others stand, a
// late candidate its last review posted being that finding, not a new one.
// The round's record of missing reports lists only what it still missed:
// nothing.
func TestCompletionHandsTheJudgeTheOtherReportsOfItsRound(t *testing.T) {
	e := newEnv(t)
	dir := e.reportDir()
	claude, in := completedRound(t, e)
	fa := captureJudge(e)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if n := len(e.ag.submitsFor(agents.RoleClaude)); n != 0 {
		t.Fatalf("claude-review prompted %d times, want its round 1 report", n)
	}
	if n := len(e.ag.shellCallsFor(agents.RoleCodexReview)); n != 1 {
		t.Fatalf("codex-review ran %d times, want once", n)
	}
	if _, err := os.Stat(claude + ".prev"); err == nil {
		t.Fatal("claude-review's report was set aside")
	}
	jd := fa.data(t)
	if !jd.SameHead || len(jd.Reports) != 2 {
		t.Fatalf("judge data: same head %v reports %+v", jd.SameHead, jd.Reports)
	}
	for _, r := range jd.Reports {
		switch r.Role {
		case string(agents.RoleClaude):
			if r.Missing || r.Path != claude || r.Late {
				t.Errorf("claude-review's report = %+v, want round 1's file, not late", r)
			}
		case string(agents.RoleCodexReview):
			if r.Missing || r.Path != filepath.Join(dir, "codex-review.md") || !r.Late {
				t.Errorf("codex-review's report = %+v, want the late one", r)
			}
		default:
			t.Errorf("unexpected report %+v", r)
		}
	}
	prompt := e.ag.submitsFor(agents.RoleJudge)[0].Text
	mustContain(t, "judge prompt", prompt, "codex-review (late): "+filepath.Join(dir, "codex-review.md"),
		"claude-review: "+claude, "Your last review went without codex-review's report",
		"A late candidate your last review already posted is that finding, never a new one",
		"The other reports stand as your last review judged them")
	if strings.Contains(prompt, "claude-review (late)") {
		t.Errorf("claude-review is marked late:\n%s", prompt)
	}
	rec, ok := ReadRoundMissingReports(e.ctx, e.st, e.pr.ID, 2)
	if !ok || len(rec.Unheard()) != 0 {
		t.Fatalf("round 2's record = %+v, %v: want nothing missing", rec, ok)
	}
}

// The reviewer's M2: when codex-review leaves no report again, the
// completion has nothing new to judge. Its judge is not prompted and posts
// nothing; the round ends unheard (OutcomeUnheard), and the review it
// completes keeps its record.
func TestACompletionWhoseLateReportsAreAllMissingPromptsNoJudge(t *testing.T) {
	e := newEnv(t)
	_, in := completedRound(t, e)
	e.ag.codex = func(*fakeAgents, codexCall) error { return nil } // ends without its report
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomeUnheard {
		t.Fatalf("RunRound = %+v, %v: want the round unheard", res, err)
	}
	if n := len(e.ag.submitsFor(agents.RoleJudge)); n != 0 {
		t.Fatalf("judge prompted %d times, want none", n)
	}
	if n := len(e.ag.shellCallsFor(agents.RoleCodexReview)); n != 1 {
		t.Fatalf("codex-review ran %d times, want once", n)
	}
	if rep := res.Reports[agents.RoleCodexReview]; rep.Status != ReportMissing {
		t.Fatalf("codex-review's report = %+v, want missing", rep)
	}
	if _, ok := ReadRoundMissingReports(e.ctx, e.st, e.pr.ID, 2); ok {
		t.Fatal("the completion recorded its missing reports: nothing was judged")
	}
}

// Codex's N1: a completion round a stop cut short before its judge's
// prompt, on a head with no completion round left, resumes in that round
// (Completion.Resume): codex-review does not run again; the judge gets the
// late report the round's run verified and claude-review's standing one.
// With no verified late report the round ends unheard, posting nothing.
func TestAResumedCompletionJudgesTheLateReportItsRoundVerified(t *testing.T) {
	for _, verified := range []bool{true, false} {
		t.Run(map[bool]string{true: "verified", false: "not verified"}[verified], func(t *testing.T) {
			e := newEnv(t)
			dir := e.reportDir()
			claude, in := completedRound(t, e)
			in.Complete.Resume = true
			codex := filepath.Join(dir, "codex-review.md")
			state, outcome := store.RunAbandoned, ReportCancelled
			if verified {
				state, outcome = store.RunVerified, ReportOK
				if err := os.WriteFile(codex, []byte(agents.ReportMarker("r-late")+"\n[P2] codex's late finding\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.st.CreateRun(e.ctx, store.Run{PRID: e.pr.ID, Round: 2, Role: string(agents.RoleCodexReview), Kind: store.RunRereview,
				TargetSHA: target, State: state, Outcome: new(outcome), ReportPath: new(codex)}); err != nil {
				t.Fatal(err)
			}
			fa := captureJudge(e)
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
			res, err := e.r.RunRound(e.ctx, in)
			if n := len(e.ag.shellCallsFor(agents.RoleCodexReview)); n != 0 {
				t.Fatalf("codex-review ran %d times, want none: its round ran it", n)
			}
			if !verified {
				if err != nil || res.Outcome != OutcomeUnheard || len(e.ag.submitsFor(agents.RoleJudge)) != 0 {
					t.Fatalf("RunRound = %+v, %v, judge prompts %d: want it unheard, no judge", res, err, len(e.ag.submitsFor(agents.RoleJudge)))
				}
				return
			}
			if err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			if _, err := os.Stat(codex + ".prev"); err == nil {
				t.Fatal("the verified late report was set aside")
			}
			jd := fa.data(t)
			want := map[string]string{string(agents.RoleClaude): claude, string(agents.RoleCodexReview): codex}
			if len(jd.Reports) != 2 {
				t.Fatalf("reports %+v", jd.Reports)
			}
			for _, r := range jd.Reports {
				if r.Missing || r.Path != want[r.Role] || r.Late != (r.Role == string(agents.RoleCodexReview)) {
					t.Errorf("report %+v, want %s", r, want[r.Role])
				}
			}
		})
	}
}
