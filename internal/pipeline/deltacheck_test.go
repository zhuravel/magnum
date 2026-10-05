package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
)

// A delta check: the judge alone, whose prompt says delta_check and names
// the file that lists the delta's files (in the report directory: the file
// names are the PR's, never in a prompt); no reviewer runs or is waited for.
func TestADeltaCheckRunsTheJudgeAloneAndNamesItsFiles(t *testing.T) {
	e := newEnv(t)
	fa := captureJudge(e)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "APPROVED", "APPROVE").behavior(t)}
	in := e.input(KindRereview)
	in.Round = 2
	in.Previous = &PreviousReview{ID: 601, Event: "APPROVED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
	roles := e.r.Config.RolesFor(nil)
	in.Roles = roles[slices.IndexFunc(roles, func(r config.Role) bool { return r.Judge }):][:1]
	in.DeltaCheck = &DeltaCheck{Lines: 4, Files: []DeltaFile{
		{Path: "app/assets/images/mailer/hero.gif", Status: "modified", Binary: true},
		{Path: "app/views/mailer/welcome.html.erb", Status: "modified"},
	}}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	path := filepath.Join(res.ReportDir, DeltaCheckFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got DeltaCheck
	if err := json.Unmarshal(b, &got); err != nil || got.Lines != 4 || !slices.Equal(got.Files, in.DeltaCheck.Files) {
		t.Fatalf("%s = %s (%v)", DeltaCheckFile, b, err)
	}
	jd := fa.data(t)
	if !jd.DeltaCheck || jd.DeltaLines != 4 || jd.DeltaFile != path || len(jd.Reports) != 0 {
		t.Fatalf("judge data: check %v lines %d file %q reports %d", jd.DeltaCheck, jd.DeltaLines, jd.DeltaFile, len(jd.Reports))
	}
	prompt := e.ag.submitsFor(agents.RoleJudge)[0].Text
	mustContain(t, "judge prompt", prompt, "\ndelta_check: true\n", "Only the commits since your last review changed (4 lines; files listed in "+path+").")
	for _, r := range roles {
		if !r.Judge && len(e.ag.submitsFor(agents.Role(r.Name))) != 0 {
			t.Errorf("%s was prompted in a delta check", r.Name)
		}
	}

	// An ordinary re-review writes no such file and says nothing of it.
	e2 := newEnv(t)
	fa2 := captureJudge(e2)
	e2.ag.behaviors[agents.RoleJudge] = []behavior{e2.judgePosts(603, "COMMENTED", "COMMENT").behavior(t)}
	in2 := e2.input(KindRereview)
	in2.Round, in2.Previous, in2.Roles = 2, in.Previous, in.Roles
	res2, err := e2.r.RunRound(e2.ctx, in2)
	if err != nil || res2.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res2, err)
	}
	if _, err := os.Stat(filepath.Join(res2.ReportDir, DeltaCheckFile)); !os.IsNotExist(err) {
		t.Fatalf("%s of an ordinary re-review: %v", DeltaCheckFile, err)
	}
	if jd := fa2.data(t); jd.DeltaCheck || jd.DeltaFile != "" {
		t.Fatalf("judge data of an ordinary re-review: %+v", jd)
	}
}
