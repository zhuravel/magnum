package evalrun

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// questionResult is a local judge's result whose provenance raises the plan's deliberate choice as
// a P2 question for the operator, beside a P3 finding of its own.
const questionResult = `{"status":"dry_run","verdict":"blocking","findings":{"P0":0,"P1":0,"P2":1,"P3":1},
"provenance":[
 {"id":"F1","title":"Is rounding the points down the operator's call? The plan calls it deliberate.","severity":"P2","path":"app/models/credit.rb","line":7,"sources":["judge"],"verdict":"posted","question":true},
 {"id":"F2","title":"A comment names the old total","severity":"P3","path":"app/models/credit.rb","line":20,"sources":["claude-review"],"verdict":"posted"}],
"planned_review":{"event":"REQUEST_CHANGES","body":"Blocking: 1 question.","comments":[
 {"path":"app/models/credit.rb","line":7,"body":"**[P2] Is rounding the points down the operator's call?**\n\nThe ticket is silent."},
 {"path":"app/models/credit.rb","line":20,"body":"**[P3] A comment names the old total (optional, no reply needed)**"}]}}`

// replayFixture is a machine with a local case frozen from round 1 of task 1 (newFreezeFixture's
// round) and saved as case task-1-round-1: the config watches example/* as the gh identity
// zhuravel, the factory's repository has a main clone, git is a fake that knows every commit, and
// the round's engine is a fake that records the round and answers with result (the judge's result
// file) and with its reviewers' reports.
type replayFixture struct {
	d      Deps
	git    *execx.Fake
	clone  string
	in     LocalInputs
	rounds []localRun
	mu     sync.Mutex
	// answer is the fake engine's round result beyond its outcome and report directory.
	answer pipeline.RoundResult
	// judgeFile is the report file the fake engine writes the judge's result to: the one of the
	// judge among the round's roles (codex-judge.json for the fixture's frozen roles).
	judgeFile string
}

func newReplayFixture(t *testing.T, result string) *replayFixture {
	t.Helper()
	ff := newFreezeFixture(t)
	in, _, err := FreezeLocal(context.Background(), ff.d, 1, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveLocalInputs(LocalCaseDir(ff.d.Layout, "task-1-round-1"), in); err != nil {
		t.Fatal(err)
	}
	f := &replayFixture{in: in, clone: t.TempDir(), git: &execx.Fake{}, judgeFile: "codex-judge.json"}
	cfg := ff.d.Config
	cfg.Identities = []config.Identity{{Name: "zhuravel", Kind: "gh", Login: "zhuravel"}}
	cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"*"}, Identity: "zhuravel", PollIdentity: "zhuravel"}}
	cfg.Factory.Repos[0].MainClone = f.clone
	cfg.Usage.CodexHome = t.TempDir()
	f.git.Rules = []execx.Rule{
		{Prefix: []string{"git", "-C", f.clone, "rev-parse"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			return execx.Result{Stdout: []byte(strings.TrimSuffix(c.Args[len(c.Args)-1], "^{commit}") + "\n")}, nil
		}},
		{Prefix: []string{"git", "-C", f.clone, "worktree"}},
	}
	f.d = Deps{Layout: ff.d.Layout, Config: cfg, Runner: f.git, Now: time.Now}
	f.d.localRound = func(_ context.Context, _ *config.Config, l paths.Layout, tag string, lr engine.LocalRound) (pipeline.RoundResult, error) {
		f.mu.Lock()
		f.rounds = append(f.rounds, localRun{layout: l, tag: tag, round: lr})
		f.mu.Unlock()
		dir := l.ReviewDir(lr.Owner, lr.Repo, lr.Number, lr.Head)
		writeFile(t, filepath.Join(dir, f.judgeFile), result)
		res := f.answer
		res.Outcome, res.ReportDir = cmpOr(res.Outcome, pipeline.OutcomeDryRun), dir
		return res, nil
	}
	return f
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// localEvalCase is the corpus's case of the fixture's round, with a defect it must raise as a P2
// question.
func localEvalCase(t *testing.T) eval.Case {
	t.Helper()
	ec := eval.Case{Name: "task-1-round-1", Kind: eval.KindLocal, PR: "example/example#900011", Head: frozenHead,
		Defects: []eval.Defect{{ID: "plan-choice", Title: "The plan's rounding is the operator's call", Severity: "P2", Question: true,
			Paths: []string{"app/models/credit.rb"}, Match: []string{"deliberate", "operator"}}}}
	if err := ec.Validate(); err != nil {
		t.Fatal(err)
	}
	return ec
}

// A local case replays its round from the frozen inputs alone: a fresh detached worktree of the
// factory's main clone at the frozen head, under the same case worktree path as a posted case,
// removed after; the round's engine gets that worktree, the frozen base, head, branch, title, plan,
// ticket with the answers, roles and synthetic number, the per-PR environment a posted replay's
// worktree gets, under the case worktree's own workspace name (never the factory copy's, whose
// databases a task may be using, nor a synthetic PR's magnum-pr-<N>), a scratch
// directory under the run's (never the factory's task directory); and the round's findings are
// scored with the posted cases' scoring, a question finding the P2 question the defect wants. The
// run keeps the findings it scored.
func TestRunCaseReplaysALocalCaseFromItsFrozenInputs(t *testing.T) {
	f := newReplayFixture(t, questionResult)
	ec := localEvalCase(t)
	o := Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"}
	if err := os.RemoveAll(f.d.Layout.FactoryTasks()); err != nil { // the task's directory may be gone by now
		t.Fatal(err)
	}
	cr := RunCase(context.Background(), f.d, ec, o)
	if cr.Outcome != pipeline.OutcomeDryRun || cr.Score == nil || cr.Kind != eval.KindLocal || cr.Error != "" {
		t.Fatalf("case run %+v", cr)
	}
	if s := cr.Score; s.Found != 1 || s.Total != 1 || !s.Defects[0].SeverityOK || s.Noise != 1 {
		t.Fatalf("score %+v, want the question found and the P3 noise", s)
	}
	if len(cr.Findings) < 2 || !cr.Findings[0].Question || cr.ResultFile == "" {
		t.Fatalf("recorded findings %+v (result file %q)", cr.Findings, cr.ResultFile)
	}
	if len(f.rounds) != 1 {
		t.Fatalf("rounds %d", len(f.rounds))
	}
	r := f.rounds[0]
	wt := caseWorktree(f.d, o, ec.Name)
	lr := r.round
	if lr.Checkout != wt || lr.MainClone != wt || lr.Head != frozenHead || lr.BaseSHA != frozenBase || lr.Number != 900011 ||
		lr.HeadRef != f.in.Branch || lr.Title != frozenTitle || lr.BaseRef != "main" || !slices.Equal(lr.Watch.Roles, f.in.Roles) ||
		lr.Identity != "zhuravel" {
		t.Fatalf("round %+v", lr)
	}
	if desc, _ := os.ReadFile(lr.DescriptionFile); string(desc) != "# "+frozenTitle+"\n\n"+frozenPlan+"\n" {
		t.Errorf("description.md %q", desc)
	}
	if ticket, _ := os.ReadFile(lr.TicketFile); string(ticket) != LocalTicket(frozenTicket, answersAsWritten) {
		t.Errorf("ticket.md %q, want the ticket and the answers", ticket)
	}
	if !strings.HasPrefix(lr.Env["WT_BRANCH"], localSlugPrefix) {
		t.Errorf("env %v, want the per-PR env under the case worktree's own name", lr.Env)
	}
	if !within(filepath.Join(o.RunDir, "cases", ec.Name), r.layout.Scratch) {
		t.Errorf("scratch %s, want it under the run's case directory", r.layout.Scratch)
	}
	if _, err := os.Stat(f.d.Layout.FactoryTasks()); !os.IsNotExist(err) {
		t.Errorf("the replay wrote the factory's task directories: %v", err)
	}
	add := f.git.CallsWithPrefix("git", "-C", f.clone, "worktree", "add")
	if len(add) != 1 || !slices.Contains(add[0].Args, wt) || add[0].Args[len(add[0].Args)-1] != frozenHead || !slices.Contains(add[0].Args, "--detach") {
		t.Fatalf("worktree add %v", add)
	}
	if rm := f.git.CallsWithPrefix("git", "-C", f.clone, "worktree", "remove"); len(rm) != 1 || !slices.Contains(rm[0].Args, wt) {
		t.Fatalf("worktree remove %v", rm)
	}
	got := eval.Rescore(eval.Run{Cases: []eval.CaseRun{cr}}, &eval.Corpus{Cases: []eval.Case{ec}}, nil)
	if s := got.Cases[0].Score; s == nil || s.Found != 1 || s.Noise != 1 {
		t.Fatalf("rescored %+v, want the same score", s)
	}
}

// What a local case's replay cannot score is reported, never scored as a miss: frozen roles the
// config no longer has and a config without a gh identity stop it before any git or agent, both
// config_error (running it again changes nothing until the config does), a judge-only replay is
// refused (a local round runs every role), a case whose inputs are gone or are another round's says
// so, a round that went without a reviewer's report is missing_role, and a refused one is flagged so
// that it never replays again.
func TestRunCaseReportsALocalCaseItCannotScore(t *testing.T) {
	o := func(f *replayFixture) Options {
		return Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"}
	}
	t.Run("a role the config lacks", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		f.in.Roles = append(f.in.Roles, "gone-review")
		dir := LocalCaseDir(f.d.Layout, "task-1-round-1")
		mustRemoveAll(t, dir)
		if err := SaveLocalInputs(dir, f.in); err != nil {
			t.Fatal(err)
		}
		cr := RunCase(context.Background(), f.d, localEvalCase(t), o(f))
		if cr.Outcome != eval.OutcomeConfigError || cr.Score != nil || !strings.Contains(cr.Error, "gone-review") || len(f.git.Calls) > 0 || len(f.rounds) > 0 {
			t.Fatalf("case run %+v (%d git calls, %d rounds)", cr, len(f.git.Calls), len(f.rounds))
		}
	})
	t.Run("no gh identity", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		f.d.Config.Identities = []config.Identity{{Name: "zhuravel", Kind: "app", Login: "talkable[bot]"}}
		cr := RunCase(context.Background(), f.d, localEvalCase(t), o(f))
		if cr.Outcome != eval.OutcomeConfigError || cr.Score != nil || !strings.Contains(cr.Error, "[[identity]]") || len(f.git.Calls) > 0 {
			t.Fatalf("case run %+v (%d git calls)", cr, len(f.git.Calls))
		}
	})
	t.Run("judge-only", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		opts := o(f)
		opts.JudgeOnly = true
		if cr := RunCase(context.Background(), f.d, localEvalCase(t), opts); cr.Outcome != OutcomeError || !strings.Contains(cr.Error, "every role") {
			t.Fatalf("case run %+v", cr)
		}
	})
	t.Run("no inputs", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		mustRemoveAll(t, LocalCaseDir(f.d.Layout, "task-1-round-1"))
		if cr := RunCase(context.Background(), f.d, localEvalCase(t), o(f)); cr.Outcome != OutcomeError || !strings.Contains(cr.Error, "eval add") {
			t.Fatalf("case run %+v", cr)
		}
	})
	t.Run("another round's inputs", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		ec := localEvalCase(t)
		ec.Head = frozenBase
		if cr := RunCase(context.Background(), f.d, ec, o(f)); cr.Outcome != OutcomeError || !strings.Contains(cr.Error, "frozen") || len(f.git.Calls) > 0 {
			t.Fatalf("case run %+v", cr)
		}
	})
	t.Run("a reviewer's report missing", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		f.answer.Reports = map[agents.Role]pipeline.RoleReport{"codex-review": {Status: pipeline.ReportMissing}, "claude-review": {Status: pipeline.ReportOK}}
		cr := RunCase(context.Background(), f.d, localEvalCase(t), o(f))
		if cr.Outcome != eval.OutcomeMissingRole || cr.Score != nil || !strings.Contains(cr.Error, "codex-review") {
			t.Fatalf("case run %+v", cr)
		}
	})
	t.Run("refused", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		f.answer.Outcome, f.answer.Refusal = pipeline.OutcomeRefused, &pipeline.Refusal{Role: "codex-judge", Kind: "codex"}
		cr := RunCase(context.Background(), f.d, localEvalCase(t), o(f))
		if cr.Outcome != pipeline.OutcomeRefused || cr.Score != nil {
			t.Fatalf("case run %+v", cr)
		}
		flagged, err := eval.LoadFlagged(RunsRoot(f.d.Layout))
		if _, ok := flagged["task-1-round-1"]; err != nil || !ok {
			t.Fatalf("flagged %v, %v; want the case", flagged, err)
		}
		if why := Refused(context.Background(), f.d, localEvalCase(t)); !strings.Contains(why, "flagged") {
			t.Fatalf("Refused = %q, want the flag", why)
		}
	})
}

// A local round that a usage limit or a logout stopped ends usage_limit or login_required, never
// missing_role, also when a reviewer's report is missing: the round's own outcome when its judge
// stopped so, else the status of a missing reviewer's report that says so (the first by name). A
// pause outcome is what stops `eval run` and the skill gate; missing_role would skip a gate's row,
// and the gate could pass without the guard.
func TestALocalReplayAUsageLimitOrALogoutStoppedEndsSoNotMissingRole(t *testing.T) {
	o := func(f *replayFixture) Options {
		return Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"}
	}
	for _, tc := range []struct {
		name, outcome string
		reports       map[agents.Role]pipeline.RoleReport
		want, role    string
	}{
		{"the judge's usage limit, a reviewer's report missing", pipeline.OutcomeUsageLimit,
			map[agents.Role]pipeline.RoleReport{"codex-review": {Status: pipeline.ReportMissing}, "claude-review": {Status: pipeline.ReportOK}},
			pipeline.OutcomeUsageLimit, ""},
		{"the judge's logout, a reviewer's report missing", pipeline.OutcomeLoginRequired,
			map[agents.Role]pipeline.RoleReport{"codex-review": {Status: pipeline.ReportLost}, "claude-review": {Status: pipeline.ReportOK}},
			pipeline.OutcomeLoginRequired, ""},
		{"a reviewer's usage limit", pipeline.OutcomeDryRun,
			map[agents.Role]pipeline.RoleReport{"codex-review": {Status: pipeline.OutcomeUsageLimit}, "claude-review": {Status: pipeline.ReportOK}},
			pipeline.OutcomeUsageLimit, "codex-review"},
		{"a reviewer's logout after another's lost pane", pipeline.OutcomeDryRun,
			map[agents.Role]pipeline.RoleReport{"codex-review": {Status: pipeline.OutcomeLoginRequired}, "claude-review": {Status: pipeline.ReportLost}},
			pipeline.OutcomeLoginRequired, "codex-review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplayFixture(t, questionResult)
			f.answer.Outcome, f.answer.Reports = tc.outcome, tc.reports
			cr := RunCase(context.Background(), f.d, localEvalCase(t), o(f))
			if cr.Outcome != tc.want || cr.Score != nil || cr.Error == "" {
				t.Fatalf("case run %+v, want %s", cr, tc.want)
			}
			if tc.role != "" && !strings.Contains(cr.Error, tc.role) {
				t.Fatalf("error %q, want it to name %s", cr.Error, tc.role)
			}
		})
	}
}

// A local round that went without a reviewer's report keeps the pane line its agent died on at the
// end of its error, as a posted replay's error carries it (agents.PaneErrorOf): the round's own error,
// a judge's that died with it, else the line of a missing reviewer's report, the first by name whose
// report has one (LocalResult.MissingDetail), never the first missing reviewer's when it has none,
// nor a later one's. A CLI's outage that kills every launch (a Codex bootstrap failure) is then told
// from a lost pane of this round alone.
func TestALocalReplayWithoutAReviewersReportKeepsThePaneLineItDiedOn(t *testing.T) {
	o := func(f *replayFixture) Options {
		return Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"}
	}
	const judgeLine, reviewerLine = "Error: account/read failed during TUI bootstrap", "Error: the reviewer's launch failed"
	const laterLine = "Error: a later reviewer's launch failed"
	refused := func(role, line string) string {
		return "agents: prompt mg-" + role + ": herdr agent.prompt: agent_not_found: agent target mg-" + role + " not found; the pane shows: " + line
	}
	for _, tc := range []struct {
		name, roundErr, detail, want string
		// reports, when set, are the round's reports in place of codex-review's with detail and
		// claude-review's, and without names the missing reviewers the error starts with.
		reports map[agents.Role]pipeline.RoleReport
		without string
	}{
		{"the judge's", "pipeline: judge prompt refused, also after a restart of the judge: " + refused("codex-judge", judgeLine), "", judgeLine, nil, ""},
		{"the reviewer's", "", refused("codex-review", reviewerLine), reviewerLine, nil, ""},
		{"the judge's over the reviewer's", "pipeline: judge prompt refused: " + refused("codex-judge", judgeLine), refused("codex-review", reviewerLine), judgeLine, nil, ""},
		{"none", "", "codex-review: no report after 30m0s", "", nil, ""},
		{"the first reviewer's by name that has one", "", "", reviewerLine, map[agents.Role]pipeline.RoleReport{
			"claude-review":   {Status: pipeline.ReportTimeout, Detail: "claude-review: no report after 30m0s"},
			"claude-simplify": {Status: pipeline.ReportFailed, Detail: refused("claude-simplify", reviewerLine)},
			"codex-review":    {Status: pipeline.ReportFailed, Detail: refused("codex-review", laterLine)},
		}, "claude-review, claude-simplify, codex-review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplayFixture(t, questionResult)
			f.answer.Error = tc.roundErr
			if tc.roundErr != "" {
				f.answer.Outcome = pipeline.OutcomeError
			}
			f.answer.Reports = map[agents.Role]pipeline.RoleReport{"codex-review": {Status: pipeline.ReportFailed, Detail: tc.detail},
				"claude-review": {Status: pipeline.ReportOK}}
			if tc.reports != nil {
				f.answer.Reports = tc.reports
			}
			cr := RunCase(context.Background(), f.d, localEvalCase(t), o(f))
			if cr.Outcome != eval.OutcomeMissingRole || cr.Score != nil ||
				!strings.HasPrefix(cr.Error, "the round went without the report of "+cmpOr(tc.without, "codex-review")+":") {
				t.Fatalf("case run %+v", cr)
			}
			if got := agents.PaneErrorOf(cr.Error); got != tc.want {
				t.Fatalf("the pane line of %q is %q, want %q", cr.Error, got, tc.want)
			}
		})
	}
}

func mustRemoveAll(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

// The review's body is scored as a posted case's is, never as noise: the finding a local result makes
// of it, when its counts hold a P0 to P2 no finding carries, is the body, which only a defect with
// body = true reads.
func TestALocalCasesReviewBodyIsNeverNoise(t *testing.T) {
	const bodyOnly = `{"status":"dry_run","verdict":"blocking","findings":{"P0":0,"P1":1,"P2":0,"P3":0},"provenance":[],
"planned_review":{"event":"REQUEST_CHANGES","body":"Blocking: the refund keeps its points, which the plan calls deliberate.","comments":[]}}`
	for name, tc := range map[string]struct {
		body  bool
		found int
	}{"a defect on a file": {false, 0}, "a defect the body may report": {true, 1}} {
		f := newReplayFixture(t, bodyOnly)
		ec := localEvalCase(t)
		ec.Defects[0].Question, ec.Defects[0].Body = false, tc.body
		cr := RunCase(context.Background(), f.d, ec, Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"})
		if cr.Score == nil || cr.Score.Noise != 0 || cr.Score.Found != tc.found {
			t.Errorf("%s: case run %+v (score %+v), want found %d and no noise", name, cr, cr.Score, tc.found)
		}
	}
}

// A local case whose frozen inputs are gone names the command that freezes its round again, with
// the task and the round its synthetic number names.
func TestALocalCaseWithoutInputsNamesTheAddThatFreezesIt(t *testing.T) {
	f := newReplayFixture(t, questionResult)
	mustRemoveAll(t, LocalCaseDir(f.d.Layout, "task-1-round-1"))
	cr := RunCase(context.Background(), f.d, localEvalCase(t), Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"})
	if !strings.Contains(cr.Error, "magnum eval add --local-task 1 --round 1 --name task-1-round-1") {
		t.Fatalf("case run %+v", cr)
	}
}

// A local case is scored on the result of the judge its round ran with, its frozen roles' judge, not
// the watch's: a round the factory ran with another judge (judge_fallback's, on a task Codex never
// works on again) is scored on that judge's result, the review's body included, and the run records
// that file.
func TestALocalCaseIsScoredOnItsFrozenJudgesResult(t *testing.T) {
	const bodyOnly = `{"status":"dry_run","verdict":"blocking","findings":{"P0":0,"P1":1,"P2":0,"P3":0},"provenance":[],
"planned_review":{"event":"REQUEST_CHANGES","body":"Blocking: the refund keeps its points, which the plan calls deliberate.","comments":[]}}`
	f := newReplayFixture(t, bodyOnly)
	f.in.Roles, f.judgeFile = []string{"claude-review", "second-judge"}, "second-judge.json"
	for i := range f.d.Config.Roles {
		if f.d.Config.Roles[i].Name == "second-judge" {
			f.d.Config.Roles[i].Output = f.judgeFile // a file of its own, not the watch's judge's
		}
	}
	dir := LocalCaseDir(f.d.Layout, "task-1-round-1")
	mustRemoveAll(t, dir)
	if err := SaveLocalInputs(dir, f.in); err != nil {
		t.Fatal(err)
	}
	ec := localEvalCase(t)
	ec.Defects[0].Question, ec.Defects[0].Body = false, true
	cr := RunCase(context.Background(), f.d, ec, Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"})
	if cr.Score == nil || cr.Score.Found != 1 || cr.Score.Event != "REQUEST_CHANGES" {
		t.Fatalf("case run %+v (score %+v), want the body's defect found", cr, cr.Score)
	}
	if filepath.Base(cr.ResultFile) != "second-judge.json" {
		t.Fatalf("result file %s, want the frozen judge's", cr.ResultFile)
	}
	if _, err := os.Stat(cr.ResultFile); err != nil {
		t.Fatalf("the recorded result file: %v", err)
	}
}

// A synthetic PR number names its task and round back, the loop's last round (config.MaxFactoryCycles,
// 10) included, which LocalNumber puts where the next task's tens would start; a number no round has
// names none.
func TestLocalRoundOfNamesTheTaskAndRoundOfASyntheticNumber(t *testing.T) {
	for _, tc := range []struct {
		task  int64
		round int
	}{{1, 1}, {1, 9}, {7, config.MaxFactoryCycles}, {8, 1}, {123, 4}} {
		task, round, ok := LocalRoundOf(LocalNumber(tc.task, tc.round))
		if !ok || task != tc.task || round != tc.round {
			t.Errorf("LocalRoundOf(LocalNumber(%d, %d)) = %d, %d, %v", tc.task, tc.round, task, round, ok)
		}
	}
	for _, n := range []int{0, 12212, LocalNumber(0, 9), localNumberBase + 10} {
		if task, round, ok := LocalRoundOf(n); ok {
			t.Errorf("LocalRoundOf(%d) = task %d, round %d; want none", n, task, round)
		}
	}
}

// A local case of the loop's last round whose frozen inputs are gone names the add of that round, not
// of round 0 of the next task.
func TestALocalCaseOfTheLastRoundNamesItsOwnAdd(t *testing.T) {
	f := newReplayFixture(t, questionResult)
	ec := localEvalCase(t)
	ec.Name, ec.PR = "task-7-round-10", fmt.Sprintf("example/example#%d", LocalNumber(7, config.MaxFactoryCycles))
	if err := ec.Validate(); err != nil {
		t.Fatal(err)
	}
	cr := RunCase(context.Background(), f.d, ec, Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"})
	if !strings.Contains(cr.Error, "magnum eval add --local-task 7 --round 10 --name task-7-round-10") {
		t.Fatalf("case run %+v", cr)
	}
}

// A local replay copies the live notes into its round's scratch layout and records their hashes, and
// with --no-notes (Options.NoNotes) copies none and records none, its round reading no notes.
func TestALocalReplayCopiesTheNotesUnlessToldNot(t *testing.T) {
	for _, noNotes := range []bool{false, true} {
		f := newReplayFixture(t, questionResult)
		writeFile(t, engine.NotesPath(f.d.Layout, "example", "example"), "# Notes\n\nCredits round down.\n")
		cr := RunCase(context.Background(), f.d, localEvalCase(t), Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"),
			AgentTag: "eval-test", NoNotes: noNotes})
		if cr.Score == nil || len(f.rounds) != 1 {
			t.Fatalf("no-notes %v: case run %+v", noNotes, cr)
		}
		r := f.rounds[0]
		_, err := os.Stat(engine.NotesPath(r.layout, "example", "example"))
		copied := err == nil
		if r.round.Notes == noNotes || copied == noNotes || (len(cr.Notes) == 0) != noNotes {
			t.Errorf("no-notes %v: the round reads notes %v, its scratch has them %v, the run records %v", noNotes, r.round.Notes, copied, cr.Notes)
		}
	}
}

// A local replay on a clone that lacks the frozen head or base fetches each by its id before it adds
// the worktree, and one it cannot fetch fails the case before any agent starts.
func TestALocalReplayFetchesAMissingCommitByID(t *testing.T) {
	for _, fetchable := range []bool{true, false} {
		f := newReplayFixture(t, questionResult)
		var mu sync.Mutex
		have := map[string]bool{}
		f.git.Rules = append([]execx.Rule{
			{Prefix: []string{"git", "-C", f.clone, "rev-parse"}, Fn: func(c execx.Cmd) (execx.Result, error) {
				sha := strings.TrimSuffix(c.Args[len(c.Args)-1], "^{commit}")
				mu.Lock()
				defer mu.Unlock()
				if (sha == frozenHead || sha == frozenBase) && !have[sha] {
					return execx.Result{}, &execx.ExitError{Code: 128, Stderr: "fatal: needed a single revision"}
				}
				return execx.Result{Stdout: []byte(sha + "\n")}, nil
			}},
			{Prefix: []string{"git", "-C", f.clone, "remote"}, Result: execx.Result{Stdout: []byte("https://example.invalid/example/example.git\n")}},
			{Prefix: []string{"git", "-C", f.clone, "config"}, Result: execx.Result{Stdout: []byte("https://example.invalid/example/example.git\n")}},
			{Prefix: []string{"git", "-C", f.clone, "fetch"}, Fn: func(c execx.Cmd) (execx.Result, error) {
				if !fetchable {
					return execx.Result{}, &execx.ExitError{Code: 128, Stderr: "fatal: remote error: upload-pack: not our ref"}
				}
				mu.Lock()
				defer mu.Unlock()
				for _, sha := range []string{frozenHead, frozenBase} {
					if slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "+"+sha+":") }) {
						have[sha] = true
					}
				}
				return execx.Result{}, nil
			}},
		}, f.git.Rules...)
		cr := RunCase(context.Background(), f.d, localEvalCase(t), Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"})
		fetched := f.git.CallsWithPrefix("git", "-C", f.clone, "fetch")
		add := f.git.CallsWithPrefix("git", "-C", f.clone, "worktree", "add")
		if fetchable {
			if cr.Score == nil || len(fetched) != 2 || !have[frozenHead] || !have[frozenBase] || len(add) != 1 {
				t.Errorf("fetchable: case run %+v, fetches %v, worktree adds %d", cr, fetched, len(add))
			}
			continue
		}
		if cr.Outcome != OutcomeError || !strings.Contains(cr.Error, "cannot be fetched by id") || len(add) != 0 || len(f.rounds) != 0 {
			t.Errorf("not fetchable: case run %+v, worktree adds %d, rounds %d", cr, len(add), len(f.rounds))
		}
	}
}

// factoryExec runs q on the factory's registry of layout l.
func factoryExec(t *testing.T, l paths.Layout, q string) {
	t.Helper()
	db, err := sql.Open("sqlite", l.FactoryDB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

// A local case whose round ran a Codex role is never replayed on a task Codex must never see again:
// the factory marked it (Codex refused it, and the account is at stake) when the case was added or
// since, its registry cannot say, or the PR the task published is flagged (an earlier replay of it
// refused, or the live registry's Codex flag). Nothing is checked out and no agent starts. A round
// that ran without Codex replays whatever the mark says.
func TestALocalCaseOfATaskCodexMustNotSeeIsNotReplayedWithCodex(t *testing.T) {
	o := func(f *replayFixture) Options {
		return Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"), AgentTag: "eval-test"}
	}
	refused := func(t *testing.T, f *replayFixture, want string) {
		t.Helper()
		cr := RunCase(context.Background(), f.d, localEvalCase(t), o(f))
		if cr.Outcome != OutcomeFlagged || cr.Score != nil || !strings.Contains(cr.Error, want) || len(f.git.Calls) > 0 || len(f.rounds) > 0 {
			t.Fatalf("case run %+v (%d git calls, %d rounds), want it refused naming %q", cr, len(f.git.Calls), len(f.rounds), want)
		}
		if why := Refused(context.Background(), f.d, localEvalCase(t)); !strings.Contains(why, want) {
			t.Fatalf("Refused = %q, want %q", why, want)
		}
	}
	resave := func(t *testing.T, f *replayFixture) {
		t.Helper()
		dir := LocalCaseDir(f.d.Layout, "task-1-round-1")
		mustRemoveAll(t, dir)
		if err := SaveLocalInputs(dir, f.in); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("the factory's mark", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		factoryExec(t, f.d.Layout, "UPDATE tasks SET codex_never = 1 WHERE id = 1")
		refused(t, f, "Codex never works on task 1 again")
	})
	t.Run("the mark frozen with the case", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		f.in.CodexNever = true
		resave(t, f)
		for _, p := range []string{f.d.Layout.FactoryDB(), f.d.Layout.FactoryDB() + "-wal", f.d.Layout.FactoryDB() + "-shm"} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		refused(t, f, "Codex never works on task 1 again")
	})
	t.Run("a registry that cannot say", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		for _, p := range []string{f.d.Layout.FactoryDB() + "-wal", f.d.Layout.FactoryDB() + "-shm"} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		writeFile(t, f.d.Layout.FactoryDB(), "not a database")
		refused(t, f, "cannot say")
	})
	t.Run("the published PR's live flag", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		factoryExec(t, f.d.Layout, "INSERT INTO pr_links (task_id, repo, number, opened_at) VALUES (1, 'example/example', 7, '2026-11-05T12:00:00Z')")
		st := storetest.OpenFile(t, f.d.Layout.DB())
		ctx := context.Background()
		repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_example", Owner: "example", Name: "example", Mode: store.RepoModePerPR})
		if err != nil {
			t.Fatal(err)
		}
		up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PR_7", Number: 7, URL: "https://github.com/example/example/pull/7",
			HeadSHA: frozenHead, GHState: store.GHOpen, InitialState: store.PRIneligible, Identity: "zhuravel"})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(engine.CodexFlag{Kind: "codex", Role: "codex-judge", Run: "r-1", At: time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)})
		if err := st.SetKV(ctx, store.KVPRCodexFlag(up.PR.ID), string(b)); err != nil {
			t.Fatal(err)
		}
		refused(t, f, "Codex flagged this PR")
	})
	t.Run("the published PR's refused replay", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		factoryExec(t, f.d.Layout, "INSERT INTO pr_links (task_id, repo, number, opened_at) VALUES (1, 'example/example', 7, '2026-11-05T12:00:00Z')")
		if err := eval.MarkFlagged(RunsRoot(f.d.Layout), eval.Flagged{Case: "published-7", PR: "example/example#7", Run: "20261107-100000", Reason: "Codex refused"}); err != nil {
			t.Fatal(err)
		}
		refused(t, f, "flagged in run 20261107-100000")
	})
	t.Run("a round without Codex", func(t *testing.T) {
		f := newReplayFixture(t, questionResult)
		factoryExec(t, f.d.Layout, "UPDATE tasks SET codex_never = 1 WHERE id = 1")
		f.in.Roles, f.in.CodexNever, f.judgeFile = []string{"claude-review", "second-judge"}, true, "second-judge.json"
		for i := range f.d.Config.Roles {
			if f.d.Config.Roles[i].Name == "second-judge" {
				f.d.Config.Roles[i].Kind, f.d.Config.Roles[i].Output = config.KindClaude, f.judgeFile
			}
		}
		resave(t, f)
		if cr := RunCase(context.Background(), f.d, localEvalCase(t), o(f)); cr.Score == nil || len(f.rounds) != 1 {
			t.Fatalf("case run %+v, want it replayed without Codex", cr)
		}
	})
}
