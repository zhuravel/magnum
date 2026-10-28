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

// A completion of round 1, whose judge went without codex-review's report
// (its pane was busy) and posted a clean review on the head: codex-review
// runs again and nobody else does; the judge, in the same-head re-review
// prompt, gets claude-review's report from round 1 (the file its verified
// run wrote, never set aside) and codex-review's late one, marked late, and
// is told to decide the late report's findings while the others stand.
// The round's record of missing reports lists only what it still missed:
// nothing.
func TestCompletionHandsTheJudgeTheOtherReportsOfItsRound(t *testing.T) {
	e := newEnv(t)
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
	fa := captureJudge(e)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindRereview)
	in.Round, in.SameHead = 2, true
	in.Previous = &PreviousReview{ID: 601, Event: "COMMENTED", SHA: target, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
	in.Complete = &Completion{Round: 1, Base: 1, Roles: []string{string(agents.RoleCodexReview)}}
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
		"The other reports stand as your last review judged them")
	if strings.Contains(prompt, "claude-review (late)") {
		t.Errorf("claude-review is marked late:\n%s", prompt)
	}
	rec, ok := ReadRoundMissingReports(e.ctx, e.st, e.pr.ID, 2)
	if !ok || len(rec.Unheard()) != 0 {
		t.Fatalf("round 2's record = %+v, %v: want nothing missing", rec, ok)
	}
}
