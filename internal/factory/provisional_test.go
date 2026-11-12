package factory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/tui"
)

// criticQuestion is the critic's question of a second batch, its first
// option recommended.
var criticQuestion = fileQuestion("Does the reminder skip merchants whose plan is paused?", 0, "skip them", "send it anyway")

// loneGate is a task of kind at its questions gate whose first batch (the
// judge's whenQuestion) you answered with its recommendation, before the
// report, and whose critic then wrote qs (none: no critic file).
func (h *harness) loneGate(key, kind string, qs ...QuestionFile) registry.Task {
	h.t.Helper()
	task := h.gateTask(key, kind, whenQuestion)
	first := registry.Question{Text: whenQuestion.Text, Options: whenQuestion.Options, Recommended: 0, RaisedBy: "fx-judge"}
	if err := h.reg.ReplaceQuestions(h.ctx, task.ID, 1, []registry.Question{first}); err != nil {
		h.t.Fatal(err)
	}
	h.clock.Add(time.Minute)
	if err := h.reg.AnswerBatch(h.ctx, task.ID, 1, map[int]string{0: whenQuestion.Options[0].Label}); err != nil {
		h.t.Fatal(err)
	}
	h.clock.Add(time.Minute)
	if len(qs) > 0 {
		h.taskFile(task.ID, "questions-fx-critic.json", questionsJSON(h.t, qs...))
	}
	return h.reload(task.ID)
}

// moveTo moves task id to stage in state, set adding the other changes.
func (h *harness) moveTo(id int64, state, stage string, set ...func(*registry.TaskUpdate)) {
	h.t.Helper()
	cur := h.reload(id)
	if err := h.reg.TransitionTask(h.ctx, id, []string{cur.State}, state, func(u *registry.TaskUpdate) {
		u.Set("stage", stage)
		for _, f := range set {
			f(u)
		}
	}); err != nil {
		h.t.Fatal(err)
	}
}

// provisionals is the task's provisional.json.
func (h *harness) provisionals(id int64) map[string]provisional {
	h.t.Helper()
	recs, err := readProvisional(h.layout.FactoryTaskDir(id))
	if err != nil {
		h.t.Fatal(err)
	}
	return recs
}

// screenRow is the task's row on the screen and its cards, as ScreenData
// draws them now.
func (h *harness) screenRow(task registry.Task) (tui.FactoryRow, tui.FactoryCards) {
	h.t.Helper()
	d, err := ScreenData(h.ctx, h.reg, nil, h.clock.Now(), time.Time{}, ScreenOptions{Config: h.cfg, Layout: h.layout})
	if err != nil {
		h.t.Fatal(err)
	}
	return rowOf(h.t, d, TaskKey(task)), d.Cards[task.ID]
}

// A lone question after a batch you answered (the critic's, its only one,
// with a recommended option) passes the questions gate on that option:
// coding starts at once, the question stays open, answers.md says coding
// went on its recommended option, an alert says it waits for you, and the
// screen keeps it on you, marked, with its card to answer.
func TestALoneQuestionAfterABatchYouAnsweredStartsCodingOnItsRecommendation(t *testing.T) {
	h := newHarness(t)
	task := h.loneGate("PR-12401", "feature", criticQuestion)
	h.tick()

	got := h.reload(task.ID)
	if got.Stage != "coding" || got.State != registry.TaskActive || waitsOnYou(got) || got.QuestionRev != 2 {
		t.Fatalf("task %s at %s waits %q: %q at revision %d; want active at coding, waiting on nobody, revision 2", got.State, got.Stage,
			got.WaitsWho, got.WaitsWhat, got.QuestionRev)
	}
	qs := h.newestQuestions(task.ID)
	if len(qs) != 1 || qs[0].State != registry.QuestionOpen || qs[0].RaisedBy != "fx-critic" || qs[0].Text != criticQuestion.Text {
		t.Fatalf("the newest revision %+v, want the critic's question, open", qs)
	}
	rec, ok := h.provisionals(task.ID)[QuestionKey(criticQuestion.Text)]
	if !ok || rec.Label != "skip them" || rec.Gate != "questions" || rec.RaisedBy != "fx-critic" {
		t.Fatalf("provisional.json holds %+v, want the critic's question on %q at the questions gate", rec, "skip them")
	}
	answers := h.readTaskFile(task.ID, answersFile)
	for _, want := range []string{whenQuestion.Text, criticQuestion.Text, "coding went on the recommended option", "skip them"} {
		if !strings.Contains(answers, want) {
			t.Errorf("answers.md lacks %q:\n%s", want, answers)
		}
	}
	alerts := h.events(task.ID, "alert")
	if len(alerts) != 1 || alerts[0].Text != "PR-12401: you: 1 question before the push (coding went on its recommended answer)" {
		t.Fatalf("alerts %+v, want the one that says coding went on the recommended answer", alerts)
	}
	if !h.e.alertStands(h.ctx, tasksByID([]registry.Task{got}), alerts[0]) {
		t.Fatal("the alert does not stand while the question is open")
	}

	row, cards := h.screenRow(got)
	if row.Group != tui.GroupNeedsYou || row.Move != "answer 1 question" || len(row.WaitsOn) == 0 || !row.WaitsOn[0].OnYou ||
		row.WaitsOn[0].What != provisionalWhat(1) {
		t.Fatalf("row %+v, want it on you: %q", row, provisionalWhat(1))
	}
	if cards.Question == nil || len(cards.Question.Questions) != 1 || !strings.Contains(cards.Question.Questions[0].Why, provisionalMark) ||
		cards.Question.Revision != 2 {
		t.Fatalf("the question card %+v, want revision 2's question marked %q", cards.Question, provisionalMark)
	}
}

// Your recommended answer closes a question coding went on: no amend, no
// new turn, the alert no longer stands and the screen lets the task be.
func TestTheRecommendedAnswerToAProvisionalQuestionClosesItWithNoNewTurn(t *testing.T) {
	r := newCodingRig(t)
	h := r.harness
	task := h.loneGate("PR-12402", "feature", criticQuestion)
	r.git(r.copyDir, "switch", "--quiet", "--no-track", "-c", task.Branch, "origin/master")
	h.startStage(task)
	if ps := h.rt.promptsTo("fx-writer"); len(ps) != 1 || strings.Contains(ps[0].Text, changedLead) {
		t.Fatalf("the writer's prompts %+v, want its first, naming no change", ps)
	}

	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 2, Answers: map[int]string{0: "skip them"}})
	h.tick()
	if qs := h.newestQuestions(task.ID); qs[0].State != registry.QuestionAnswered || !qs[0].TookRecommendation {
		t.Fatalf("the question %+v, want answered with its recommendation", qs[0])
	}
	if ps := h.rt.promptsTo("fx-writer"); len(ps) != 1 {
		t.Fatalf("the writer got %d prompts, want no new turn for the recommended answer", len(ps))
	}
	if got := h.rt.launchedRoles(); slices.ContainsFunc(got, func(roles []string) bool { return slices.Contains(roles, "fx-judge") }) {
		t.Fatalf("launches %v, want no amend", got)
	}
	got := h.reload(task.ID)
	if row, _ := h.screenRow(got); row.Group == tui.GroupNeedsYou {
		t.Fatalf("row %+v, want the task off you once the question closed", row)
	}
	if h.e.alertStands(h.ctx, tasksByID([]registry.Task{got}), h.events(task.ID, "alert")[0]) {
		t.Fatal("the alert still stands after your answer")
	}

	r.fix(task)
	h.ended(task, "fx-writer")
	h.tick()
	h.e.waitBackground()
	h.tick()
	if got := h.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task %s at %s waits %q, want past coding", got.State, got.Stage, got.WaitsWhat)
	}
}

// Another answer to a question coding went on changed the plan: once the
// writer's turn in flight ended (the stage neither prompts nor passes
// meanwhile), the judge amends the report, told that coding began, then the
// writer's next turn names the answer (never its words) and the stage
// passes only after it. A restart while the judge amends sends nothing
// twice.
func TestADifferentAnswerInCodingAmendsTheReportThenReachesTheWriter(t *testing.T) {
	r := newCodingRig(t)
	h := r.harness
	task := h.loneGate("PR-12403", "feature", criticQuestion)
	r.git(r.copyDir, "switch", "--quiet", "--no-track", "-c", task.Branch, "origin/master")
	h.startStage(task)

	h.clock.Add(time.Minute)
	const own = "send it anyway, MARKER-7d2e"
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 2, Answers: map[int]string{0: own}})
	h.tick()
	if n := len(h.rt.promptsTo("fx-judge")); n != 0 {
		t.Fatalf("the judge got %d prompts while the writer's turn runs, want none", n)
	}

	// The writer's turn ends with its fix: the stage does not pass, the
	// judge amends first.
	r.fix(task)
	h.ended(task, "fx-writer")
	h.tick()
	h.e.waitBackground() // the proof and the precheck run off the loop
	h.tick()
	h.e.waitBackground()
	h.tick()
	if got := h.reload(task.ID); got.Stage != "coding" {
		t.Fatalf("task %s at %s, want still at coding until the writer follows the answer", got.State, got.Stage)
	}
	judge := h.rt.promptsTo("fx-judge")
	if len(judge) != 1 || !strings.Contains(judge[0].Text, "Coding began on the recommended answer of question 1 of revision 2 (asked by fx-critic)") {
		t.Fatalf("the judge's prompts %+v, want one amend that says coding began", judge)
	}
	if n := len(h.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts before the amend ended, want its first only", n)
	}

	h.restart()
	h.tick()
	if n := len(h.rt.promptsTo("fx-judge")); n != 1 {
		t.Fatalf("the judge got %d prompts after a restart, want its one amend", n)
	}

	h.clock.Add(time.Minute)
	h.taskFile(task.ID, reportFile, strings.Replace(goodReport, "none\n\n## Cause", "- Send it to paused plans too.\n\n## Cause", 1))
	h.taskFile(task.ID, acceptanceFile, goodAcceptance)
	h.taskFile(task.ID, questionsFile, "[]")
	// The restart lost the result of the writer's checks, so its first tick
	// runs them again off the loop, and the stage holds the writer's turn
	// until they end: wait for them, as a slow machine does.
	h.e.waitBackground()
	h.ended(task, "fx-judge")
	h.tick()
	ws := h.rt.promptsTo("fx-writer")
	if len(ws) != 2 || !strings.Contains(ws[1].Text, changedLead) || !strings.Contains(ws[1].Text, "question 1 of revision 2 (asked by fx-critic)") ||
		!strings.Contains(ws[1].Text, filepath.Join(h.layout.FactoryTaskDir(task.ID), answersFile)) || strings.Contains(ws[1].Text, "MARKER-7d2e") {
		t.Fatalf("the writer's prompts %+v, want a second turn that names the changed answer by its place, never its words", ws)
	}
	if ws[1].Attempt != ws[0].Attempt {
		t.Fatalf("the writer's turn is in attempt %s, want the coding stage's %s", ws[1].Attempt, ws[0].Attempt)
	}
	if got := h.reload(task.ID); got.Stage != "coding" {
		t.Fatalf("task at %s, want coding until the writer's turn ends", got.Stage)
	}
	// A restart while that turn runs: its first turn's verification is no end
	// of it, and nothing is sent again.
	r.restart()
	h.startStage(task) // the claim of the copy again, then the stage's pass
	h.tick()
	if got := h.reload(task.ID); got.Stage != "coding" || len(h.rt.promptsTo("fx-writer")) != 2 {
		t.Fatalf("after a restart the task is at %s and the writer got %d prompts; want coding, the two", got.Stage,
			len(h.rt.promptsTo("fx-writer")))
	}

	r.commit("PR-12403 paused plans too", map[string]string{"app/credit.rb": strings.Replace(creditRB, "    0\n", "    11\n", 1),
		"spec/models/credit_points_spec.rb": "expect: 11 in app/credit.rb\n"})
	h.ended(task, "fx-writer")
	h.tick()
	h.e.waitBackground()
	h.tick()
	if got := h.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task %s at %s waits %q, want past coding once the writer followed the answer", got.State, got.Stage, got.WaitsWhat)
	}
	if cs, err := h.e.pendingChanges(h.ctx, task); err != nil || len(cs) != 0 {
		t.Fatalf("pending changes %+v (%v), want none once handed", cs, err)
	}
}

// Past coding, another answer to a question coding went on sends the task
// back to coding (rework.json of kind answer) once its stage has nothing in
// flight, never while a local round's cycle is open: there the judge amends
// the report first, then the writer's first prompt of the new attempt names
// the answer, its commits kept. From its ready stage, which waits for that
// answer, the task goes back at once.
func TestADifferentAnswerPastCodingGoesBackToCoding(t *testing.T) {
	r := newCodingRig(t)
	h := r.harness
	task := h.loneGate("PR-12404", "feature", criticQuestion)
	r.git(r.copyDir, "switch", "--quiet", "--no-track", "-c", task.Branch, "origin/master")
	h.startStage(task)
	head := r.fix(task)
	h.ended(task, "fx-writer")
	h.tick()
	h.e.waitBackground()
	h.tick()
	if got := h.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task %s at %s waits %q, want at its local review", got.State, got.Stage, got.WaitsWhat)
	}
	cy, err := h.reg.BeginCycle(h.ctx, task.ID, head, "")
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 2, Answers: map[int]string{0: "send it anyway"}})
	h.tick()
	if got := h.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task at %s while its review cycle is open, want local_review", got.Stage)
	}
	if err := h.reg.EndCycle(h.ctx, cy.ID, head, "", "", 0); err != nil {
		t.Fatal(err)
	}
	h.tick()
	got := h.reload(task.ID)
	if got.Stage != "coding" || got.State != registry.TaskActive {
		t.Fatalf("task %s at %s, want back at coding", got.State, got.Stage)
	}
	if rw, ok := readRework(h.layout.FactoryTaskDir(task.ID)); !ok || rw.Kind != reworkAnswer || rw.Head != head {
		t.Fatalf("rework.json %+v (%v), want kind %s at %s", rw, ok, reworkAnswer, head)
	}
	if evs := h.events(task.ID, "ready"); len(evs) == 0 || !strings.Contains(evs[len(evs)-1].Text, "question 1 of revision 2 (asked by fx-critic)") {
		t.Fatalf("events %+v, want the move back naming the answer", evs)
	}

	h.tick()
	h.e.waitBackground()
	h.tick()
	if judge := h.rt.promptsTo("fx-judge"); len(judge) != 1 || !strings.Contains(judge[0].Text, "Coding began") {
		t.Fatalf("the judge's prompts %+v, want the amend first", judge)
	}
	if n := len(h.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts before the amend ended, want its first only", n)
	}
	h.clock.Add(time.Minute)
	h.taskFile(task.ID, reportFile, goodReport)
	h.taskFile(task.ID, acceptanceFile, goodAcceptance)
	h.taskFile(task.ID, questionsFile, "[]")
	h.ended(task, "fx-judge")
	h.tick()
	h.e.waitBackground()
	h.tick()
	ws := h.rt.promptsTo("fx-writer")
	if len(ws) != 2 || ws[1].Attempt == ws[0].Attempt || !strings.Contains(ws[1].Text, changedLead) {
		t.Fatalf("the writer's prompts %+v, want a new attempt's first prompt naming the changed answer", ws)
	}
	if b := r.git(r.copyDir, "rev-parse", "HEAD"); b != head {
		t.Fatalf("the copy at %s, want the branch kept at %s", b, head)
	}

	// At its ready stage the task waits for the answer; another one sends it
	// back to coding at once.
	ready := h.loneGate("PR-12405", "feature", criticQuestion)
	h.tick()
	h.moveTo(ready.ID, registry.TaskNeedsYou, "ready", func(u *registry.TaskUpdate) {
		u.Set("waits_who", waitsYou)
		u.Set("waits_what", provisionalWhat(1))
	})
	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: ready.ID, Revision: 2, Answers: map[int]string{0: "send it anyway"}})
	if got := h.reload(ready.ID); got.Stage != "coding" || got.State != registry.TaskActive {
		t.Fatalf("task %s at %s, want back at coding from its ready stage", got.State, got.Stage)
	}
}

// A question coding went on holds the ready card: the task waits on you for
// its answer and no push is offered; your recommended answer draws the card,
// and the push goes.
func TestTheReadyCardWaitsForTheAnswerOfAQuestionCodingWentOn(t *testing.T) {
	r := newReadyRig(t)
	task := r.codingTask("PR-12406", "bug")
	first := registry.Question{Text: whenQuestion.Text, Options: whenQuestion.Options, Recommended: 0, RaisedBy: "fx-judge"}
	if err := r.reg.ReplaceQuestions(r.ctx, task.ID, 1, []registry.Question{first}); err != nil {
		t.Fatal(err)
	}
	if err := r.reg.AnswerBatch(r.ctx, task.ID, 1, map[int]string{0: whenQuestion.Options[0].Label}); err != nil {
		t.Fatal(err)
	}
	lone := registry.Question{Text: criticQuestion.Text, Options: criticQuestion.Options, Recommended: 0, RaisedBy: "fx-critic"}
	if err := r.reg.ReplaceQuestions(r.ctx, task.ID, 2, []registry.Question{lone}); err != nil {
		t.Fatal(err)
	}
	dir := r.layout.FactoryTaskDir(task.ID)
	if err := writeProvisional(dir, map[string]provisional{QuestionKey(lone.Text): {Text: lone.Text, RaisedBy: "fx-critic", Revision: 2,
		Label: "skip them", Gate: "questions", At: r.clock.Now()}}); err != nil {
		t.Fatal(err)
	}

	got, _ := r.cardOf(r.reload(task.ID))
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != provisionalWhat(1) {
		t.Fatalf("task %s at %s waits %q, want on you: %q", got.State, got.Stage, got.WaitsWhat, provisionalWhat(1))
	}
	if _, err := os.Stat(filepath.Join(dir, readyCardFile)); !os.IsNotExist(err) {
		t.Fatalf("ready-card.json: %v, want no card before the answer", err)
	}
	if err := r.e.decide(r.ctx, DecidePayload{TaskID: task.ID, Choice: ChoicePush}); err == nil {
		t.Fatal("a push before the answer went through")
	}
	if row, cards := r.screenRow(got); cards.Question == nil || !strings.Contains(row.WaitsOn[0].Long, criticQuestion.Text) {
		t.Fatalf("row %+v with cards %+v, want the question to answer, named", row, cards)
	}

	r.clock.Add(time.Minute)
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 2, Answers: map[int]string{0: "skip them"}})
	r.run(4, func() bool { return strings.HasPrefix(r.reload(task.ID).WaitsWhat, readyWait) })
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("the push after your answer: %s %q", q.State, q.Result)
	}
}

// A lone question waits for you as before when it has no recommended
// option, when its batch is the task's first, when two questions are open,
// when it is the reproduction question, and when provisional_lone_answer is
// off.
func TestALoneQuestionWaitsAsBeforeOutsideItsCase(t *testing.T) {
	repro := fileQuestion("The bug did not reproduce: how does the task go on?", 0, ReproductionOptions...)
	for name, tc := range map[string]struct {
		kind string
		set  func(h *harness, key string) registry.Task
		want string
	}{
		"no recommended option": {"feature", func(h *harness, key string) registry.Task { return h.loneGate(key, "feature", offQuestion) },
			"1 question before coding"},
		"the first batch": {"feature", func(h *harness, key string) registry.Task { return h.gateTask(key, "feature", criticQuestion) },
			"1 question before coding"},
		"two open questions": {"feature", func(h *harness, key string) registry.Task {
			return h.loneGate(key, "feature", criticQuestion, whoQuestion)
		}, "2 questions before coding"},
		"the reproduction question": {"bug", func(h *harness, key string) registry.Task { return h.loneGate(key, "bug", repro) },
			"1 question before coding"},
		"the key off": {"feature", func(h *harness, key string) registry.Task {
			h.cfg.Factory.ProvisionalLoneAnswer = false
			return h.loneGate(key, "feature", criticQuestion)
		}, "1 question before coding"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			task := tc.set(h, "PR-12407")
			h.tick()
			got := h.reload(task.ID)
			if got.State != registry.TaskNeedsYou || got.Stage != "questions" || !strings.HasPrefix(got.WaitsWhat, tc.want) {
				t.Fatalf("task %s at %s waits %q, want on you at questions: %q", got.State, got.Stage, got.WaitsWhat, tc.want)
			}
			if recs := h.provisionals(task.ID); len(recs) != 0 {
				t.Fatalf("provisional.json %+v, want none", recs)
			}
		})
	}
}

// A restart after the provisional pass neither asks the question again nor
// alerts twice, and keeps what coding went on: your other answer after it
// is still a change for the writer.
func TestARestartAfterTheProvisionalPassAsksNothingTwiceAndLosesNothing(t *testing.T) {
	h := newHarness(t)
	task := h.loneGate("PR-12408", "feature", criticQuestion)
	h.tick()
	h.restart()
	h.tick()
	h.tick()
	got := h.reload(task.ID)
	if got.QuestionRev != 2 || got.Stage != "coding" || waitsOnYou(got) {
		t.Fatalf("after a restart the task is %s at %s, revision %d, waiting %q; want at coding, revision 2", got.State, got.Stage,
			got.QuestionRev, got.WaitsWhat)
	}
	if n := len(h.events(task.ID, "alert")); n != 1 {
		t.Fatalf("%d alerts, want the one", n)
	}
	passes := slices.DeleteFunc(h.events(task.ID, "questions"), func(ev registry.Event) bool {
		return !strings.Contains(ev.Text, "passed on the recommended answer")
	})
	if len(passes) != 1 {
		t.Fatalf("pass events %+v, want one", passes)
	}

	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 2, Answers: map[int]string{0: "send it anyway"}})
	h.restart()
	cs, err := h.e.pendingChanges(h.ctx, task)
	if err != nil || len(cs) != 1 || cs[0].rec.Label != "skip them" || cs[0].q.Answer != "send it anyway" {
		t.Fatalf("after a restart the changes are %+v (%v), want your answer's", cs, err)
	}
}
