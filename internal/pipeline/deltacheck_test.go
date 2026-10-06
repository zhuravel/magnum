package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/textx"
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

	// The delta's one line is one line.
	e1 := newEnv(t)
	e1.ag.behaviors[agents.RoleJudge] = []behavior{e1.judgePosts(604, "APPROVED", "APPROVE").behavior(t)}
	in1 := e1.input(KindRereview)
	in1.Round, in1.Previous, in1.Roles = 2, in.Previous, in.Roles
	in1.DeltaCheck = &DeltaCheck{Lines: 1, Files: in.DeltaCheck.Files[1:]}
	if res, err := e1.r.RunRound(e1.ctx, in1); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	mustContain(t, "one-line judge prompt", e1.ag.submitsFor(agents.RoleJudge)[0].Text, "Only the commits since your last review changed (1 line; ")

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

// A delta check whose judge starts in a fresh session (an identity
// migration, a lost session) runs as a recovery of the judge alone: the
// recovery prompt, which rebuilds the judge's context from its previous
// review and threads, says delta_check and asks for a review of just the
// commits since that review, at the judge's rereview effort. Its threads
// file is read as a recovery's is.
func TestAFreshJudgeChecksADeltaWithTheRecoveryPrompt(t *testing.T) {
	e := newEnv(t)
	fa := captureJudge(e)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "APPROVED", "APPROVE").behavior(t)}
	in := e.input(KindRecovery)
	in.Round = 2
	in.Previous = &PreviousReview{ID: 601, Event: "APPROVED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
	roles := e.r.Config.RolesFor(nil)
	judge := roles[slices.IndexFunc(roles, func(r config.Role) bool { return r.Judge })]
	in.Roles = []config.Role{judge}
	in.DeltaCheck = &DeltaCheck{Lines: 1, Files: []DeltaFile{{Path: "app/models/coupon.rb", Status: "modified"}}}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	path := filepath.Join(res.ReportDir, DeltaCheckFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s: %v", DeltaCheckFile, err)
	}
	jd := fa.data(t)
	if !jd.DeltaCheck || jd.DeltaLines != 1 || jd.DeltaFile != path || len(jd.Reports) != 0 {
		t.Fatalf("judge data: check %v lines %d file %q reports %d", jd.DeltaCheck, jd.DeltaLines, jd.DeltaFile, len(jd.Reports))
	}
	if want := judge.EffortFor(true); jd.Effort != want || want == judge.Effort {
		t.Fatalf("judge effort = %q, want its rereview effort %q (effort %q)", jd.Effort, want, judge.Effort)
	}
	prompt := e.ag.submitsFor(agents.RoleJudge)[0].Text
	mustContain(t, "judge prompt", prompt, "\nmode: recovery\n", "\ndelta_check: true\n", "\nprevious_review_id: 601\n",
		"Only the commits since your last review (`"+textx.ShortSHA(prevSHA)+"`) changed (1 line; files listed in "+path+"). "+
			"Read your previous review and its threads for context, then review just those changes; the rest stands as reviewed.")
	for _, r := range roles {
		if !r.Judge && len(e.ag.submitsFor(agents.Role(r.Name))) != 0 {
			t.Errorf("%s was prompted in a delta check", r.Name)
		}
	}

	// An ordinary recovery says nothing of a delta check, at the full effort.
	e2 := newEnv(t)
	fa2 := captureJudge(e2)
	e2.ag.behaviors[agents.RoleJudge] = []behavior{e2.judgePosts(603, "COMMENTED", "COMMENT").behavior(t)}
	in2 := e2.input(KindRecovery)
	in2.Round, in2.Previous, in2.Roles = 2, in.Previous, in.Roles
	if res2, err := e2.r.RunRound(e2.ctx, in2); err != nil || res2.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res2, err)
	}
	if jd := fa2.data(t); jd.DeltaCheck || jd.DeltaFile != "" || jd.Effort != judge.Effort {
		t.Fatalf("judge data of an ordinary recovery: check %v file %q effort %q", jd.DeltaCheck, jd.DeltaFile, jd.Effort)
	}
	if p := e2.ag.submitsFor(agents.RoleJudge)[0].Text; strings.Contains(p, "delta_check") || strings.Contains(p, "Only the commits") {
		t.Fatalf("ordinary recovery prompt mentions a delta check:\n%s", p)
	}
}
