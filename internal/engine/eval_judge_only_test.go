package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// storedReports writes the reviewers' reports of an earlier full replay into the replay's report
// directory, as evalrun copies them, and returns them for EvalCase.Reports: claude-review's and
// codex-review's, and claude-simplify's missing (its full replay wrote none).
func storedReports(t *testing.T, h *harness, c EvalCase) []agents.Report {
	t.Helper()
	dir := h.layout.ReviewDir(c.Owner, c.Repo, c.Number, c.Head)
	var out []agents.Report
	for _, name := range []string{store.RoleClaude, store.RoleCodexReview} {
		role, ok := h.cfg.RoleByNameOrAlias(&c.Watch, name)
		if !ok {
			t.Fatalf("no role %s", name)
		}
		path := filepath.Join(dir, role.ReportFile())
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("stored "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out = append(out, agents.Report{Role: name, Path: path, Status: pipeline.ReportOK})
	}
	return append(out, agents.Report{Role: store.RoleSimplify, Status: pipeline.ReportMissing, Missing: true})
}

// A judge-only replay runs the judge alone on the reviewer reports of the case's earlier full replay:
// only the judge's agent starts, the reviewers' runs are seeded verified with the copied files (a
// missing one as a run that did not verify, so the judge is told it is missing), and the round is the
// blind dry run of the same-head judge-alone shape with the reviewers still named, on the stored
// round's number, reading the stored reports (pipeline.RoundInput.StoredReports).
func TestAJudgeOnlyReplayRunsOnlyTheJudgeAgainstTheStoredReports(t *testing.T) {
	h := newEvalHerdr()
	r := newEvalReplay(t, h, "eval-a1b2c3")
	r.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun}, nil
	}
	c := evalCaseIn(r, evalCheckout(t))
	c.JudgeOnly, c.Reports = true, storedReports(t, r, c)

	res, pr, err := r.e.RunEval(r.ctx, c)
	if err != nil || res.Outcome != pipeline.OutcomeDryRun {
		t.Fatalf("RunEval: %+v, %v", res, err)
	}
	for _, name := range h.startNames() {
		if !strings.Contains(name, "-"+store.RoleJudge+"-") {
			t.Errorf("agent %s started; want the judge alone (%v)", name, h.startNames())
		}
	}
	if len(h.startNames()) != 1 {
		t.Fatalf("agents started: %v, want the judge", h.startNames())
	}
	if len(r.rd.inputs) != 1 {
		t.Fatalf("rounds: %d", len(r.rd.inputs))
	}
	in := r.rd.inputs[0]
	if !in.StoredReports || !in.DryRun || !in.Blind || in.Kind != pipeline.KindInitial || in.Round != evalReportsRound || in.MaxRestarts != 0 {
		t.Fatalf("round input: stored=%v dry=%v blind=%v kind=%s round=%d restarts=%d",
			in.StoredReports, in.DryRun, in.Blind, in.Kind, in.Round, in.MaxRestarts)
	}
	var names []string
	for _, role := range in.Roles {
		names = append(names, role.Name)
	}
	for _, want := range []string{store.RoleClaude, store.RoleCodexReview, store.RoleJudge} {
		if !slices.Contains(names, want) {
			t.Errorf("round roles %v lack %s: the judge's prompt names the reviewers' reports by role", names, want)
		}
	}
	runs, err := r.st.RunsByPR(r.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	byRole := map[string]store.Run{}
	for _, run := range runs {
		byRole[run.Role] = run
	}
	for _, rep := range c.Reports[:2] {
		run := byRole[rep.Role]
		if run.State != store.RunVerified || run.Round != evalReportsRound || run.TargetSHA != c.Head || store.Deref(run.ReportPath) != rep.Path {
			t.Errorf("%s run %+v, want verified in round %d with %s", rep.Role, run, evalReportsRound, rep.Path)
		}
	}
	if run := byRole[store.RoleSimplify]; run.ID == "" || run.State == store.RunVerified || run.Round != evalReportsRound {
		t.Errorf("claude-simplify run %+v, want one that did not verify", run)
	}
}

// A judge-only replay needs the reviewers' reports, each in the round's report directory under its
// role's file: without them, or with one elsewhere, it fails before any agent starts.
func TestAJudgeOnlyReplayRefusesReportsItCannotRead(t *testing.T) {
	h := newEvalHerdr()
	r := newEvalReplay(t, h, "eval-a1b2c3")
	c := evalCaseIn(r, evalCheckout(t))
	c.JudgeOnly = true
	if _, _, err := r.e.RunEval(r.ctx, c); err == nil || !strings.Contains(err.Error(), "report") {
		t.Fatalf("no reports: %v", err)
	}
	c.Reports = []agents.Report{{Role: store.RoleClaude, Path: filepath.Join(t.TempDir(), "claude-review.md"), Status: pipeline.ReportOK}}
	if _, _, err := r.e.RunEval(r.ctx, c); err == nil || !strings.Contains(err.Error(), store.RoleClaude) {
		t.Fatalf("a report outside the round's directory: %v", err)
	}
	if len(h.startNames()) != 0 || len(r.rd.inputs) != 0 {
		t.Fatalf("agents %v, rounds %d: want none", h.startNames(), len(r.rd.inputs))
	}
}
