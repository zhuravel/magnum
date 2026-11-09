package evalrun

import (
	"context"
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
	f := &replayFixture{in: in, clone: t.TempDir(), git: &execx.Fake{}}
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
		writeFile(t, filepath.Join(dir, "codex-judge.json"), result)
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
// worktree gets (never the factory copy's, whose databases a task may be using), a scratch
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
	if lr.Env["WT_BRANCH"] != "magnum-pr-900011" {
		t.Errorf("env %v, want the per-PR worktree's for the synthetic number", lr.Env)
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
// config no longer has and a config without a gh identity stop it before any git or agent, a
// judge-only replay is refused (a local round runs every role), a case whose inputs are gone or are
// another round's says so, a round that went without a reviewer's report is missing_role, and a
// refused one is flagged so that it never replays again.
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
		if cr.Outcome != eval.OutcomeMissingRole || cr.Score != nil || !strings.Contains(cr.Error, "gone-review") || len(f.git.Calls) > 0 || len(f.rounds) > 0 {
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
