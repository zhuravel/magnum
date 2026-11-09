package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
)

var (
	addHead = strings.Repeat("ab", 20)
	addBase = strings.Repeat("cd", 20)
)

// localRoundOnDisk is what the factory leaves of round 1 (cycle 1) of task 1's local review on
// talkable/talkable: the task, the cycle and its begin event in the factory's registry, the round's
// result in the task's directory, and its scratch directory with the description, the ticket and the
// prepass.json it read.
func localRoundOnDisk(t *testing.T, l paths.Layout) { t.Helper(); localRoundOnDiskAt(t, l, 1) }

// localRoundOnDiskAt is localRoundOnDisk with round `round` in cycle 1 (a second opinion's round 11
// past the loop's ten is one).
func localRoundOnDiskAt(t *testing.T, l paths.Layout, round int) {
	t.Helper()
	registrytest.Seed(t, l.FactoryDB())
	db, err := sql.Open("sqlite", l.FactoryDB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`INSERT INTO tasks (id, jira_key, repo, title, branch, state, created_at, updated_at) VALUES (1, 'PR-12361', 'talkable/talkable',
			'Credit points count the activities', 'zhuravel-PR-12361-credit-points', 'done', '2026-11-05T08:00:00Z', '2026-11-05T08:00:00Z')`,
		`INSERT INTO review_cycles (task_id, n, head_before, started_at) VALUES (1, 1, '` + addHead + `', '2026-11-05T09:00:00Z')`,
		`INSERT INTO events (at, task_id, kind, phase, text) VALUES ('2026-11-05T09:00:00Z', 1, 'review', 'begin',
			'round ` + strconv.Itoa(round) + ` (cycle 1) at ` + addHead[:12] + `: codex-review, codex-judge')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("fixture: %v\n%s", err, q)
		}
	}
	scratch := filepath.Join(evalrun.LocalRoot(l, 1), strconv.Itoa(round)+"-local-0a1b2c")
	report := paths.Layout{Scratch: scratch}.ReviewDir("talkable", "talkable", evalrun.LocalNumber(1, round), addHead)
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(scratch, evalrun.LocalDescriptionFile), "# PR-12361 Credit points count the activities\n\n## Plan\n\nRound them down: deliberate.\n")
	write(filepath.Join(scratch, evalrun.LocalTicketFile), "# PR-12361\n\nCount the activities.\n")
	pp, _ := json.Marshal(pipeline.Prepass{HeadSHA: addHead, BaseSHA: addBase})
	write(filepath.Join(report, pipeline.PrepassFile), string(pp))
	res, _ := json.Marshal(evalrun.LocalResult{Outcome: pipeline.OutcomeDryRun, Verdict: "clean", Round: round, Scratch: scratch, ReportDir: report,
		Findings: []evalrun.LocalFinding{{Severity: "P3", Title: "Is rounding down wanted?", Path: "app/models/credit.rb", Line: 7, Question: true}}})
	write(filepath.Join(l.FactoryTaskDir(1), evalrun.LocalResultFile(1)), string(res))
}

// `magnum eval add --local-task 1 --round 1` freezes what that round read in the case's own
// directory beside the learned corpus and prints the case for the local cases' corpus beside it
// (never eval.toml, which builds from before local cases refuse whole), with the round's findings and
// a defect to fill in: the printed case, given a defect, is a valid local case of the round's
// synthetic PR. The same case is never frozen twice.
func TestEvalAddFreezesALocalRoundAndPrintsItsCase(t *testing.T) {
	f := newInspFixture(t)
	f.Ctx.Layout.UserConfig = filepath.Join(f.Home, "user", "config.toml") // the operator's corpus is beside it, never the real one
	localRoundOnDisk(t, f.Ctx.Layout)
	if code := execute(f.Ctx, []string{"eval", "add", "--local-task", "1", "--round", "1"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	out := f.Out.String()
	for _, want := range []string{"round 1 (cycle 1) of factory task 1", "codex-review, codex-judge", "P3 question app/models/credit.rb:7 Is rounding down wanted?",
		`name = "task-1-round-1"`, `kind = "local"`, `pr = "talkable/talkable#900011"`, `head = "` + addHead + `"`, "# [[case.defect]]", "# question = true",
		"magnum eval run --case task-1-round-1", filepath.Join("learn", "local", "cases.toml"), "not eval.toml"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	in, err := evalrun.LoadLocalInputs(evalrun.LocalCaseDir(f.Ctx.Layout, "task-1-round-1"))
	if err != nil || in.HeadSHA != addHead || in.BaseSHA != addBase || in.Repo != "talkable/talkable" {
		t.Fatalf("frozen inputs %+v, %v", in, err)
	}
	at := strings.Index(out, "[[case]]")
	if at < 0 {
		t.Fatalf("no [[case]] in the output:\n%s", out)
	}
	block := out[at:]
	block = strings.ReplaceAll(block, "# [[case.defect]]", "[[case.defect]]")
	block += "\nid = \"plan-choice\"\ntitle = \"t\"\nquestion = true\nmatch = [\"deliberate\"]\n"
	corpus := filepath.Join(t.TempDir(), "eval.toml")
	if err := os.WriteFile(corpus, []byte(block), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := eval.LoadCorpus(corpus)
	if err != nil {
		t.Fatalf("the printed case does not load: %v\n%s", err, block)
	}
	if cs := c.Cases[0]; !cs.Local() || cs.PR != in.PR() || cs.Head != in.HeadSHA {
		t.Fatalf("case %+v", cs)
	}
	f.Out.Reset()
	if code := execute(f.Ctx, []string{"eval", "add", "--local-task", "1", "--round", "1"}); code != 1 || !strings.Contains(f.Err.String(), "exists") {
		t.Fatalf("a second add: exit %d; stderr %s", code, f.Err)
	}
}

// `eval add` refuses what it cannot freeze, saying why: no task or round flag, a round the task did
// not run, a factory that never ran here, a case name the corpus has or that is no case name.
func TestEvalAddRefusesWhatItCannotFreeze(t *testing.T) {
	f := newInspFixture(t)
	f.Ctx.Layout.UserConfig = filepath.Join(f.Home, "user", "config.toml")
	if code := execute(f.Ctx, []string{"eval", "add", "--local-task", "1", "--round", "1"}); code != 1 ||
		!strings.Contains(f.Err.String(), "registry does not exist") {
		t.Fatalf("no factory: exit %d; stderr %s", code, f.Err)
	}
	localRoundOnDisk(t, f.Ctx.Layout)
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"eval", "add", "--round", "1"}, 2, "--local-task"},
		{[]string{"eval", "add", "--local-task", "1"}, 2, "--round"},
		{[]string{"eval", "add", "--local-task", "1", "--round", "2"}, 1, "round 1 (cycle 1)"},
		{[]string{"eval", "add", "--local-task", "1", "--round", "1", "--name", "Bad Name"}, 2, "--name"},
	} {
		f.Err.Reset()
		if code := execute(f.Ctx, tc.args); code != tc.code || !strings.Contains(f.Err.String(), tc.want) {
			t.Errorf("%v: exit %d, stderr %s; want %d naming %q", tc.args, code, f.Err, tc.code, tc.want)
		}
	}
	corpus := filepath.Join(f.Ctx.Layout.ConfigDir(), evalCorpusFile)
	if err := os.MkdirAll(filepath.Dir(corpus), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corpus, []byte("[[case]]\nname = \"task-1-round-1\"\npr = \"talkable/talkable#3\"\nhead = \""+addHead+
		"\"\n\n[[case.defect]]\nid = \"d\"\ntitle = \"t\"\nmatch = [\"x\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.Err.Reset()
	if code := execute(f.Ctx, []string{"eval", "add", "--local-task", "1", "--round", "1"}); code != 1 || !strings.Contains(f.Err.String(), "already") {
		t.Fatalf("a name the corpus has: exit %d; stderr %s", code, f.Err)
	}
	if err := os.Remove(corpus); err != nil {
		t.Fatal(err)
	}
	writeEvalFile(t, f.Ctx.Layout.LearnLocalCorpus(), "[[case]]\nname = \"task-1-round-1\"\nkind = \"local\"\npr = \"talkable/talkable#900011\"\nhead = \""+addHead+
		"\"\n\n[[case.defect]]\nid = \"d\"\ntitle = \"t\"\nquestion = true\nmatch = [\"x\"]\n")
	f.Err.Reset()
	if code := execute(f.Ctx, []string{"eval", "add", "--local-task", "1", "--round", "1"}); code != 1 || !strings.Contains(f.Err.String(), "already") {
		t.Fatalf("a name the local cases have: exit %d; stderr %s", code, f.Err)
	}
	if _, err := os.Stat(evalrun.LocalCaseDir(f.Ctx.Layout, "task-1-round-1")); !os.IsNotExist(err) {
		t.Fatalf("a refused add froze the round: %v", err)
	}
}

// `eval run` says a case is local on its line, and a local case whose frozen inputs are gone fails
// naming how to freeze its round again.
func TestEvalRunNamesALocalCaseAndItsMissingInputs(t *testing.T) {
	f := newInspFixture(t)
	corpus := filepath.Join(t.TempDir(), "eval.toml")
	if err := os.WriteFile(corpus, []byte("[[case]]\nname = \"task-1-round-1\"\nkind = \"local\"\npr = \"talkable/talkable#900011\"\nhead = \""+addHead+
		"\"\n\n[[case.defect]]\nid = \"d\"\ntitle = \"t\"\nquestion = true\nseverity = \"P2\"\nmatch = [\"x\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := execute(f.Ctx, []string{"eval", "run", "--corpus", corpus}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	out := f.Out.String()
	if !strings.Contains(out, "task-1-round-1 talkable/talkable#900011 (local round 1 of task 1) at "+addHead[:7]) || !strings.Contains(out, "eval add") {
		t.Fatalf("output:\n%s", out)
	}
}

// `eval run` names a local case of the loop's last round, and of a round past it, by its own task and
// round.
func TestEvalCaseWhatNamesTheLastRoundOfItsTask(t *testing.T) {
	ec := eval.Case{Name: "task-7-round-10", Kind: eval.KindLocal, PR: "talkable/talkable#900080", Head: addHead,
		Defects: []eval.Defect{{ID: "d", Title: "t", Match: []string{"x"}}}}
	if err := ec.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := evalCaseWhat(ec); got != "talkable/talkable#900080 (local round 10 of task 7)" {
		t.Fatalf("evalCaseWhat = %q", got)
	}
	ec.Name, ec.PR = "task-7-round-11", "talkable/talkable#9000711"
	if err := ec.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := evalCaseWhat(ec); got != "talkable/talkable#9000711 (local round 11 of task 7)" {
		t.Fatalf("evalCaseWhat = %q", got)
	}
}

// `eval add --round 11` freezes a round past the loop's ten (a second opinion's) under its own number,
// 9000111, and the case it prints names that number's rule.
func TestEvalAddTakesARoundPastTheLoopsTen(t *testing.T) {
	f := newInspFixture(t)
	f.Ctx.Layout.UserConfig = filepath.Join(f.Home, "user", "config.toml")
	localRoundOnDiskAt(t, f.Ctx.Layout, 11)
	if code := execute(f.Ctx, []string{"eval", "add", "--local-task", "1", "--round", "11"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	out := f.Out.String()
	for _, want := range []string{`name = "task-1-round-11"`, `pr = "talkable/talkable#9000111"`, "9000000 + task × 100 + round"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, round := range []string{"0", "100"} {
		f.Err.Reset()
		if code := execute(f.Ctx, []string{"eval", "add", "--local-task", "1", "--round", round}); code != 2 || !strings.Contains(f.Err.String(), "1 to 99") {
			t.Errorf("--round %s: exit %d; stderr %s", round, code, f.Err)
		}
	}
}

// `eval add` of a round of a task the factory marked never for Codex again says that its replay is
// refused while the round's roles run Codex.
func TestEvalAddSaysATasksCodexMark(t *testing.T) {
	f := newInspFixture(t)
	f.Ctx.Layout.UserConfig = filepath.Join(f.Home, "user", "config.toml")
	localRoundOnDisk(t, f.Ctx.Layout)
	db, err := sql.Open("sqlite", f.Ctx.Layout.FactoryDB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE tasks SET codex_never = 1 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if code := execute(f.Ctx, []string{"eval", "add", "--local-task", "1", "--round", "1"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	if out := f.Out.String(); !strings.Contains(out, "Codex never works on task 1 again") || !strings.Contains(out, "codex-review, codex-judge") {
		t.Fatalf("output:\n%s", out)
	}
}

// `eval add` of a round whose ticket file did not hold your answers, when one came after the round
// began, says that the answers are rebuilt from the factory's registry as they stood at the round's
// start and how many it left out; a round with nothing answered since says nothing of answers.
func TestEvalAddSaysTheAnswersWereRebuiltAsOfTheRoundsStart(t *testing.T) {
	f := newInspFixture(t)
	f.Ctx.Layout.UserConfig = filepath.Join(f.Home, "user", "config.toml")
	localRoundOnDisk(t, f.Ctx.Layout)
	if code := execute(f.Ctx, []string{"eval", "add", "--local-task", "1", "--round", "1", "--name", "before-any-answer"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	if out := f.Out.String(); strings.Contains(out, "rebuilt") {
		t.Fatalf("nothing answered since, output:\n%s", out)
	}
	db, err := sql.Open("sqlite", f.Ctx.Layout.FactoryDB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`INSERT INTO questions (id, task_id, revision, idx, text, answer, state, raised_by, asked_at, answered_at) VALUES (1, 1, 1, 0,
			'Count refunds?', 'only paid ones', 'answered', 'fx-judge', '2026-11-05T08:00:00Z', '2026-11-05T08:30:00Z')`,
		`INSERT INTO question_answers (question_id, answer, at) VALUES (1, 'only paid ones', '2026-11-05T08:30:00Z')`,
		`INSERT INTO questions (id, task_id, revision, idx, text, answer, state, raised_by, asked_at, answered_at) VALUES (2, 1, 2, 0,
			'Round the points down?', 'yes', 'answered', 'fx-judge', '2026-11-05T09:40:00Z', '2026-11-05T10:00:00Z')`,
		`INSERT INTO question_answers (question_id, answer, at) VALUES (2, 'yes', '2026-11-05T10:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f.Out.Reset()
	if code := execute(f.Ctx, []string{"eval", "add", "--local-task", "1", "--round", "1"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	out := f.Out.String()
	for _, want := range []string{"rebuilt from the factory's registry as they stood when the round began (2026-11-05T09:00:00Z)",
		"leaving out 1 answer given at or after it"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	in, err := evalrun.LoadLocalInputs(evalrun.LocalCaseDir(f.Ctx.Layout, "task-1-round-1"))
	if err != nil || !strings.Contains(in.AnswersMD, "only paid ones") || strings.Contains(in.AnswersMD, "Round the points down?") {
		t.Fatalf("frozen answers %q, %v; want only the one given before the round", in.AnswersMD, err)
	}
}

// `eval run` reads the local cases' corpus beside the learned one, without --corpus, and names it on
// its start line.
func TestEvalRunReadsTheLocalCasesCorpus(t *testing.T) {
	f := newInspFixture(t)
	writeEvalFile(t, f.Ctx.Layout.LearnLocalCorpus(), "[[case]]\nname = \"task-1-round-1\"\nkind = \"local\"\npr = \"talkable/talkable#900011\"\nhead = \""+addHead+
		"\"\n\n[[case.defect]]\nid = \"d\"\ntitle = \"t\"\nquestion = true\nmatch = [\"x\"]\n")
	if code := execute(f.Ctx, []string{"eval", "run"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	out := f.Out.String()
	if !strings.Contains(out, "1 case(s) (1 local)") || !strings.Contains(out, "cases.toml") ||
		!strings.Contains(out, "task-1-round-1 talkable/talkable#900011 (local round 1 of task 1)") {
		t.Fatalf("output:\n%s", out)
	}
}
