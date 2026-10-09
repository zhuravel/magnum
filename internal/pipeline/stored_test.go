package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// A judge-only replay (magnum eval run --judge-only) gives the judge the reviewers' reports of an
// earlier full replay: RoundInput.StoredReports runs no reviewer, sets none of their files aside, and
// lists the reports their verified runs of the round name, as a continued round does, while the judge
// gets the round's own prompts: one, or its own pass first and then the candidates when the round asks
// for its own pass, as a full round would. A role without a stored run is not listed.
func TestAStoredReportsRoundRunsTheJudgeAloneOnTheVerifiedReports(t *testing.T) {
	for _, ownPass := range []bool{false, true} {
		e := newEnv(t)
		dir := e.reportDir()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, role := range []agents.Role{agents.RoleClaude, agents.RoleCodexReview} {
			path := filepath.Join(dir, string(role)+".md")
			if err := os.WriteFile(path, []byte("stored "+string(role)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := e.st.CreateRun(e.ctx, store.Run{PRID: e.pr.ID, Round: 1, Role: string(role), Kind: store.RunInitial,
				TargetSHA: target, State: store.RunVerified, Outcome: new(ReportOK), ReportPath: new(path)}); err != nil {
				t.Fatal(err)
			}
		}
		judge := e.judgePosts(901, "COMMENTED", "COMMENT").behavior(t)
		e.ag.behaviors[agents.RoleJudge] = []behavior{judge}
		if ownPass {
			e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), judge}
		}
		in := e.input(KindInitial)
		in.StoredReports, in.OwnPass = true, ownPass

		res, err := e.r.RunRound(e.ctx, in)
		if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 901 {
			t.Fatalf("own pass %v: result = %+v, %v", ownPass, res, err)
		}
		if len(e.ag.submitsFor(agents.RoleClaude)) != 0 || len(e.ag.codexCalls) != 0 || len(e.ag.submitsFor(agents.RoleCodexReview)) != 0 {
			t.Errorf("own pass %v: a reviewer ran", ownPass)
		}
		for _, role := range []agents.Role{agents.RoleClaude, agents.RoleCodexReview} {
			path := filepath.Join(dir, string(role)+".md")
			if rep := res.Reports[role]; rep.Status != ReportOK || rep.Path != path {
				t.Errorf("own pass %v: %s report %+v, want the stored file", ownPass, role, rep)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != "stored "+string(role)+"\n" {
				t.Errorf("own pass %v: %s's stored report is gone: %q, %v", ownPass, role, b, err)
			}
		}
		if _, listed := res.Reports[agents.RoleSimplify]; listed {
			t.Errorf("own pass %v: claude-simplify, which has no stored run, is listed", ownPass)
		}
		submits := e.ag.submitsFor(agents.RoleJudge)
		want := 1
		if ownPass {
			want = 2
			mustContain(t, "own-pass prompt", submits[0].Text, "phase: own_pass")
		}
		if len(submits) != want {
			t.Fatalf("own pass %v: judge prompts %d, want %d", ownPass, len(submits), want)
		}
		last := submits[len(submits)-1].Text
		mustContain(t, "judge prompt", last, "mode: initial", "claude-review: "+filepath.Join(dir, "claude-review.md"),
			"codex-review: "+filepath.Join(dir, "codex-review.md"))
		if judgeRun := e.runOf(agents.RoleJudge, store.RunInitial); judgeRun.Round != 1 || judgeRun.State != store.RunVerified {
			t.Errorf("own pass %v: judge run %+v, want round 1 verified", ownPass, judgeRun)
		}
	}
}
