package factory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/factory/stats"
	"github.com/zhuravel/magnum/internal/jira"
)

// fileQuestion is a question as an agent writes it: options labelled
// labels, the one at rec recommended (-1: none).
func fileQuestion(text string, rec int, labels ...string) QuestionFile {
	q := QuestionFile{Text: text, Context: "the ticket does not say", Example: "a credit that expires on Fri 14 Nov",
		Why: "the coupon reminder does the same", Evidence: "app/models/credit.rb:12"}
	for _, l := range labels {
		q.Options = append(q.Options, registry.Option{Label: l, Means: "the email follows " + l})
	}
	if rec >= 0 {
		q.Recommended = &rec
	}
	return q
}

// questionsJSON is the JSON of a questions file holding qs.
func questionsJSON(t *testing.T, qs ...QuestionFile) string {
	t.Helper()
	b, err := json.Marshal(append([]QuestionFile{}, qs...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var (
	whenQuestion = fileQuestion("When does the reminder go out?", 0, "Seven days before it expires", "One day before it expires")
	whoQuestion  = fileQuestion("Who gets the reminder?", 0, "advocates only", "advocates and friends")
	offQuestion  = fileQuestion("Can a merchant turn it off?", -1, "yes, a switch in the settings", "no, always on")
)

// gateTask is an active task of kind at its questions gate, whose judge
// wrote the report, its acceptance checks and questions.json with qs.
func (h *harness) gateTask(key, kind string, qs ...QuestionFile) registry.Task {
	h.t.Helper()
	task := h.kindTask(key, kind, "questions")
	h.taskFile(task.ID, reportFile, goodReport)
	h.taskFile(task.ID, acceptanceFile, goodAcceptance)
	h.taskFile(task.ID, questionsFile, questionsJSON(h.t, qs...))
	return task
}

// sendAnswers sends p as the screen does, a ReqAnswer, and runs the tick
// that answers it (and advances the task); it returns the request as the
// tick left it.
func (h *harness) sendAnswers(p AnswerPayload) registry.Request {
	h.t.Helper()
	id, err := h.reg.EnqueueRequest(h.ctx, ReqAnswer, p)
	if err != nil {
		h.t.Fatal(err)
	}
	h.tick()
	q, err := h.reg.RequestByID(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return q
}

// mustAnswer is sendAnswers that fails the test when the daemon refuses.
func (h *harness) mustAnswer(p AnswerPayload) registry.Request {
	h.t.Helper()
	q := h.sendAnswers(p)
	if q.State != registry.RequestDone {
		h.t.Fatalf("the answers were refused: %s", q.Result)
	}
	return q
}

// newestQuestions are the questions of the task's newest revision.
func (h *harness) newestQuestions(id int64) []registry.Question {
	h.t.Helper()
	rev := h.reload(id).QuestionRev
	all, err := h.reg.Questions(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return slices.DeleteFunc(all, func(q registry.Question) bool { return q.Revision != rev })
}

func (h *harness) readTaskFile(id int64, name string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.layout.FactoryTaskDir(id), name))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func (h *harness) answerInputs(id int64) []registry.Input {
	h.t.Helper()
	ins, err := h.reg.Inputs(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return slices.DeleteFunc(ins, func(in registry.Input) bool { return in.Kind != registry.InputAnswer })
}

func TestGateWithQuestionsWaitsOnTheOperator(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12361", "feature", whenQuestion, offQuestion)
	h.tick()

	got := h.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWho != "you" || got.WaitsWhat != "2 questions before coding" {
		t.Fatalf("task %s waits %q: %q; want needs_you, you: 2 questions before coding", got.State, got.WaitsWho, got.WaitsWhat)
	}
	qs := h.newestQuestions(task.ID)
	if got.QuestionRev != 1 || len(qs) != 2 {
		t.Fatalf("revision %d with %d questions, want one revision of 2", got.QuestionRev, len(qs))
	}
	for i, want := range []QuestionFile{whenQuestion, offQuestion} {
		q := qs[i]
		wantRec := -1
		if want.Recommended != nil {
			wantRec = *want.Recommended
		}
		if q.Text != want.Text || q.State != registry.QuestionOpen || q.RaisedBy != "fx-judge" || q.Recommended != wantRec ||
			len(q.Options) != 2 || q.Context != want.Context || q.Evidence != want.Evidence {
			t.Errorf("question %d is %+v, want %q open, raised by fx-judge, recommending %d", i, q, want.Text, wantRec)
		}
	}
	if evs := h.events(task.ID, "alert"); len(evs) != 1 {
		t.Fatalf("alerts %+v, want one", evs)
	}

	// The batch is asked once: neither a later tick nor a restart asks it
	// again.
	h.tick()
	h.restart()
	h.tick()
	if got := h.reload(task.ID); got.QuestionRev != 1 || got.State != registry.TaskNeedsYou {
		t.Fatalf("after a restart the task is %s at revision %d, want needs_you at revision 1", got.State, got.QuestionRev)
	}
	if evs := h.events(task.ID, "alert"); len(evs) != 1 {
		t.Fatalf("alerts %+v, want still one", evs)
	}
}

func TestGateWithoutQuestionsPasses(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12362", "feature")
	h.tick()
	if got := h.reload(task.ID); got.Stage != "coding" || got.State != registry.TaskActive || got.QuestionRev != 0 {
		t.Fatalf("task %s at %s, revision %d; want active at coding, nothing asked", got.State, got.Stage, got.QuestionRev)
	}

	// No questions file at all is no question either.
	bare := h.kindTask("PR-12363", "chore", "questions")
	h.taskFile(bare.ID, reportFile, goodReport)
	h.tick()
	if got := h.reload(bare.ID); got.Stage != "coding" {
		t.Fatalf("a task without questions.json is at %s, want coding", got.Stage)
	}
	if evs := h.events(bare.ID, "alert"); len(evs) != 0 {
		t.Fatalf("alerts %+v, want none", evs)
	}
}

// The judge's questions.json, the critic's questions-fx-critic.json and an
// open question of intake (which Jira issue a prompt is) are one batch of
// one new revision; a role file's raised_by is its file's role.
func TestTheCriticsQuestionsJoinTheOneBatch(t *testing.T) {
	h := newHarness(t)
	h.stageCopy()
	task, err := h.reg.CreateTask(h.ctx, registry.Task{Prompt: "remind advocates before their credit expires", Kind: "feature",
		Repo: "example/shop", Title: "credit expiry reminder", State: registry.TaskActive, Stage: "questions", Branch: "zhuravel-credit-expiry"})
	if err != nil {
		t.Fatal(err)
	}
	intake := registry.Question{Text: whichIssue, Options: []registry.Option{{Label: "PR-12364 Credit expiry reminder"}, {Label: noIssue}},
		Recommended: 0, RaisedBy: intakeRole}
	if err := h.reg.ReplaceQuestions(h.ctx, task.ID, 1, []registry.Question{intake}); err != nil {
		t.Fatal(err)
	}
	h.taskFile(task.ID, reportFile, goodReport)
	h.taskFile(task.ID, acceptanceFile, goodAcceptance)
	h.taskFile(task.ID, questionsFile, questionsJSON(t, whenQuestion))
	critic := whoQuestion
	critic.RaisedBy = "fx-judge" // the file's name says whose it is
	h.taskFile(task.ID, "questions-fx-critic.json", questionsJSON(t, critic, offQuestion))
	h.tick()

	got := h.reload(task.ID)
	qs := h.newestQuestions(task.ID)
	if got.QuestionRev != 2 || got.WaitsWhat != "4 questions before coding" {
		t.Fatalf("task at revision %d waits %q, want revision 2: 4 questions before coding", got.QuestionRev, got.WaitsWhat)
	}
	var who []string
	for _, q := range qs {
		who = append(who, q.RaisedBy+": "+q.Text)
	}
	want := []string{intakeRole + ": " + whichIssue, "fx-judge: " + whenQuestion.Text, "fx-critic: " + whoQuestion.Text, "fx-critic: " + offQuestion.Text}
	if !slices.Equal(who, want) {
		t.Fatalf("the batch is %q, want %q", who, want)
	}
	all, err := h.reg.Questions(h.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if all[0].Revision != 1 || all[0].State != registry.QuestionVoided || all[0].VoidedBy != 2 {
		t.Fatalf("intake's first question is %+v, want voided by revision 2, which carries it", all[0])
	}
}

// A question two roles raise is asked once: the same text, whatever its
// case, spaces, end punctuation, backticks and double quotes (an agent's
// copy changes them), in the judge's questions.json and in an investigator's
// questions-fx-points.json is one question of the batch, the judge's (its
// file is read first), raised by both, and one answer serves both.
func TestAQuestionTwoRolesRaiseIsAskedOnce(t *testing.T) {
	h := newHarness(t)
	template := fileQuestion("Which email template does the reminder use?", 0, "the credit template", "a new template")
	task := h.gateTask("PR-12381", "feature", whenQuestion, template)
	again := fileQuestion("  when does the `reminder`\tgo \"out\". ", 1, "a week before", "the day before")
	h.taskFile(task.ID, "questions-fx-points.json", questionsJSON(t, again, whoQuestion))
	h.tick()

	got := h.reload(task.ID)
	qs := h.newestQuestions(task.ID)
	var who []string
	for _, q := range qs {
		who = append(who, q.RaisedBy+": "+q.Text)
	}
	want := []string{"fx-judge, fx-points: " + whenQuestion.Text, "fx-judge: " + template.Text, "fx-points: " + whoQuestion.Text}
	if got.QuestionRev != 1 || got.WaitsWhat != "3 questions before coding" || !slices.Equal(who, want) {
		t.Fatalf("revision %d waits %q with %q; want revision 1, 3 questions before coding: %q", got.QuestionRev, got.WaitsWhat, who, want)
	}
	if qs[0].Recommended != 0 || qs[0].Options[0].Label != whenQuestion.Options[0].Label {
		t.Fatalf("the question asked once is %+v, want the judge's, with its options", qs[0])
	}
	if evs := h.events(task.ID, "questions"); len(evs) != 1 || !strings.Contains(evs[0].Text, "1 repeat asked once") {
		t.Fatalf("questions events %+v, want one that counts the repeat", evs)
	}

	h.tick()
	if rev := h.reload(task.ID).QuestionRev; rev != 1 {
		t.Fatalf("a later tick made revision %d, want the batch asked once", rev)
	}
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[0].Label,
		1: template.Options[0].Label, 2: whoQuestion.Options[0].Label}})
	if got := h.reload(task.ID); got.Stage != "coding" || got.QuestionRev != 1 {
		t.Fatalf("after the answers the task is %s at %s, revision %d; want at coding, nothing asked again", got.State, got.Stage, got.QuestionRev)
	}
}

// A second reader (a role whose codex_fallback is another role of its
// stage: fx-points-codex beside fx-points) has no questions file: its open
// questions stay in its findings, for the judge to ask once what they add.
// A questions-fx-points-codex.json it, or a refused run of it, left anyway
// is never asked, even malformed, and never stops the task: the batch holds
// the judge's and fx-points' questions alone, the same question in two
// readers' words asked once.
func TestASecondReadersQuestionsFileIsNeverAsked(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12382", "feature", whenQuestion)
	h.taskFile(task.ID, "questions-fx-points.json", questionsJSON(t, whoQuestion))
	again := fileQuestion("Do advocates and their friends both get the reminder email?", 0, "advocates only", "both")
	h.taskFile(task.ID, "questions-fx-points-codex.json", questionsJSON(t, again, offQuestion))
	h.tick()

	got := h.reload(task.ID)
	var who []string
	for _, q := range h.newestQuestions(task.ID) {
		who = append(who, q.RaisedBy+": "+q.Text)
	}
	want := []string{"fx-judge: " + whenQuestion.Text, "fx-points: " + whoQuestion.Text}
	if got.QuestionRev != 1 || got.WaitsWhat != "2 questions before coding" || !slices.Equal(who, want) {
		t.Fatalf("revision %d waits %q with %q; want revision 1, 2 questions before coding: %q", got.QuestionRev, got.WaitsWhat, who, want)
	}

	broken := h.gateTask("PR-12383", "feature")
	h.taskFile(broken.ID, "questions-fx-points-codex.json", `[{"text": "half a question"`)
	h.tick()
	if got := h.reload(broken.ID); got.State != registry.TaskActive || got.Stage != "coding" || got.QuestionRev != 0 {
		t.Fatalf("a malformed questions file of the second reader left the task %s at %s waiting %q (revision %d), want it at coding",
			got.State, got.Stage, got.WaitsWhat, got.QuestionRev)
	}
}

// A question another role asked already is not asked again: the critic's
// repeat of the judge's open question adds no revision, and the one answer
// passes the gate.
func TestAQuestionAskedBeforeIsNotAskedAgainByAnotherRole(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12382", "feature", whenQuestion)
	h.tick()
	h.taskFile(task.ID, "questions-fx-critic.json", questionsJSON(t, fileQuestion("When does the reminder go out?", 1, "now", "later")))
	h.tick()
	if got := h.reload(task.ID); got.QuestionRev != 1 || got.WaitsWhat != "1 question before coding" {
		t.Fatalf("task at revision %d waits %q, want revision 1: 1 question before coding", got.QuestionRev, got.WaitsWhat)
	}
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[0].Label}})
	if got := h.reload(task.ID); got.Stage != "coding" || got.QuestionRev != 1 {
		t.Fatalf("after the answer the task is %s at %s, revision %d; want at coding, nothing asked again", got.State, got.Stage, got.QuestionRev)
	}
}

// A role that asks a question another role asked and you answered gets
// your answers as its next prompt, not a new question: the writer in coding
// repeats the judge's answered question.
func TestARoleRepeatingAnAnsweredQuestionGetsTheAnswers(t *testing.T) {
	h := newHarness(t)
	codingEndsOnFindings(h)
	task := h.gateTask("PR-12391", "feature", whenQuestion)
	h.tick()
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[0].Label}})
	if got := h.reload(task.ID); got.Stage != "coding" {
		t.Fatalf("after the answer the task is %s at %s, want at coding", got.State, got.Stage)
	}
	h.startStage(task)
	if n := len(h.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts, want its first", n)
	}
	h.taskFile(task.ID, "questions-fx-writer.json", questionsJSON(t, fileQuestion("When does the reminder go out?", 1, "now", "later")))
	h.ended(task, "fx-writer")
	h.tick()

	got := h.reload(task.ID)
	ps := h.rt.promptsTo("fx-writer")
	if got.QuestionRev != 1 || got.State != registry.TaskActive || len(ps) != 2 || !strings.HasPrefix(ps[1].Text, answersLead) {
		t.Fatalf("task %s at revision %d, the writer's prompts %+v; want active at revision 1, the answers as its next prompt",
			got.State, got.QuestionRev, ps)
	}
}

func TestABugThatDidNotReproduceAsksTheOperatorWithOptions(t *testing.T) {
	notReproduced := reportWith("Proof", "## Proof\n- Reproduced: no. The spec passes on master.\n")
	for _, tc := range []struct {
		name, report, questions string
		judges                  bool // the question asked is the judge's, not the factory's own
	}{
		{"the judge's question", notReproduced, reproQuestion, true},
		{"no proof line", reportWith("Proof", ""), "", false},
		{"a proof that names none", reportWith("Proof", "## Proof\nThe spec passes; see findings-fx-repro.md.\n"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			task := h.kindTask("PR-12365", "bug", "reproduction")
			h.taskFile(task.ID, reportFile, tc.report)
			if tc.questions != "" {
				h.taskFile(task.ID, questionsFile, tc.questions)
			}
			h.tick()

			got := h.reload(task.ID)
			if got.State != registry.TaskNeedsYou || got.Stage != "reproduction" || got.WaitsWhat != "1 question before coding" {
				t.Fatalf("task %s at %s waits %q; want needs_you at reproduction: 1 question before coding", got.State, got.Stage, got.WaitsWhat)
			}
			qs := h.newestQuestions(task.ID)
			if len(qs) != 1 {
				t.Fatalf("questions %+v, want the reproduction question", qs)
			}
			q := qs[0]
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			for _, want := range ReproductionOptions {
				if !slices.Contains(labels, want) {
					t.Errorf("the reproduction question's options %q lack %q", labels, want)
				}
			}
			if q.Recommended < 0 || q.Why == "" {
				t.Errorf("the reproduction question recommends %d (%q), want one option and why", q.Recommended, q.Why)
			}
			if judges := q.RaisedBy == "fx-judge"; judges != tc.judges {
				t.Errorf("the question was raised by %q, want the judge's: %v", q.RaisedBy, tc.judges)
			}
		})
	}

	// An explicit "Reproduced: yes", with a list marker or emphasis, passes.
	for _, proof := range []string{"Reproduced: yes. The spec fails.", "- Reproduced: yes", "**Reproduced:** yes, by bin/rspec"} {
		h := newHarness(t)
		task := h.kindTask("PR-12366", "bug", "reproduction")
		h.taskFile(task.ID, reportFile, reportWith("Proof", "## Proof\n"+proof+"\n"))
		h.tick()
		if got := h.reload(task.ID); got.Stage != "coding" || got.QuestionRev != 0 {
			t.Fatalf("a Proof of %q left the task %s at %s, revision %d; want coding", proof, got.State, got.Stage, got.QuestionRev)
		}
	}
}

// You answer "drop" to the reproduction question: the task stops.
func TestDroppingABugThatDidNotReproduceStopsIt(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12367", "bug", "reproduction")
	h.taskFile(task.ID, reportFile, reportWith("Proof", "## Proof\nReproduced: no.\n"))
	h.taskFile(task.ID, questionsFile, reproQuestion)
	h.tick()
	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "drop"}})
	if got := h.reload(task.ID); got.State != registry.TaskStopped {
		t.Fatalf("task %s at %s waiting %q, want stopped", got.State, got.Stage, got.WaitsWhat)
	}
	if !slices.Contains(h.copies.releases(), task.ID) {
		t.Fatalf("releases %v, want the dropped task's copy released", h.copies.releases())
	}
}

// Another answer to the reproduction question is acted on: the judge
// amends the report with it, and the gate reads the amended Proof.
func TestAReproductionAnswerAmendsTheReportBeforeTheGateReadsItAgain(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12368", "bug", "reproduction")
	h.taskFile(task.ID, reportFile, reportWith("Proof", "## Proof\nReproduced: no.\n"))
	h.taskFile(task.ID, acceptanceFile, goodAcceptance)
	h.taskFile(task.ID, questionsFile, reproQuestion)
	h.tick()
	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "the merchant must have two currencies"}})
	h.e.waitBackground()
	h.tick()
	ps := h.rt.promptsTo("fx-judge")
	if len(ps) != 1 || !strings.Contains(ps[0].Text, filepath.Join(h.layout.FactoryTaskDir(task.ID), answersFile)) {
		t.Fatalf("the judge's prompts %+v, want one amend naming answers.md", ps)
	}
	if got := h.reload(task.ID); got.Stage != "reproduction" || got.State != registry.TaskActive {
		t.Fatalf("task %s at %s, want active at reproduction while the judge amends", got.State, got.Stage)
	}

	h.taskFile(task.ID, reportFile, goodReport) // Reproduced: yes
	h.taskFile(task.ID, acceptanceFile, goodAcceptance)
	h.taskFile(task.ID, questionsFile, "[]")
	h.ended(task, "fx-judge")
	h.tick()
	if got := h.reload(task.ID); got.Stage != "coding" {
		t.Fatalf("task %s at %s waiting %q, want coding after the amended report reproduced the bug", got.State, got.Stage, got.WaitsWhat)
	}
}

func TestAnswersMoveTheTaskToCoding(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12369", "bug", whenQuestion)
	h.tick()
	h.clock.Add(time.Minute)
	q := h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[0].Label}})
	if !strings.Contains(q.Result, "PR-12369") {
		t.Fatalf("the request's result %q does not name the task", q.Result)
	}
	h.tick() // the reproduction gate: the report says Reproduced: yes
	got := h.reload(task.ID)
	if got.State != registry.TaskActive || got.Stage != "coding" || got.WaitsWho != "" || got.WaitsWhat != "" {
		t.Fatalf("task %s at %s waits %q: %q; want active at coding, waiting on nothing", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	qs := h.newestQuestions(task.ID)
	if qs[0].State != registry.QuestionAnswered || qs[0].Answer != whenQuestion.Options[0].Label || !qs[0].TookRecommendation {
		t.Fatalf("question %+v, want answered with the recommendation", qs[0])
	}
	ins := h.answerInputs(task.ID)
	if len(ins) != 1 || ins[0].Text != whenQuestion.Options[0].Label || ins[0].Role != "fx-judge" || ins[0].Stage != "questions" {
		t.Fatalf("answer inputs %+v, want the one answer, at questions, to fx-judge's question", ins)
	}
	if n := len(h.rt.launchedRoles()); n != 0 {
		t.Fatalf("%d launches, want no amend for an answer that took the recommendation", n)
	}
}

func TestAnAnswerToAStaleRevisionIsRefused(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12370", "feature", whenQuestion)
	h.tick()
	h.clock.Add(5 * time.Minute)
	changed := h.clock.Now()
	carried := registry.Question{Text: whenQuestion.Text, Options: whenQuestion.Options, Recommended: -1, RaisedBy: "fx-judge"}
	if err := h.reg.ReplaceQuestions(h.ctx, task.ID, 2, []registry.Question{carried}); err != nil {
		t.Fatal(err)
	}

	q := h.sendAnswers(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[0].Label}})
	want := "the questions changed at " + changed.Local().Format("15:04") + ", nothing was sent"
	if q.State != registry.RequestFailed || q.Result != want {
		t.Fatalf("request %s: %q; want failed: %q", q.State, q.Result, want)
	}
	all, err := h.reg.Questions(h.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(all, func(q registry.Question) bool { return q.State == registry.QuestionAnswered }) {
		t.Fatalf("questions %+v, want none answered", all)
	}
	if _, err := os.Stat(filepath.Join(h.layout.FactoryTaskDir(task.ID), answersFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("answers.md: %v, want none written", err)
	}
	if ins := h.answerInputs(task.ID); len(ins) != 0 {
		t.Fatalf("answer inputs %+v, want none", ins)
	}
}

// The answers reach the agents as a data file: answers.md in the task's
// directory, which the writer's prompt names by its path, never by its
// words.
func TestAnswersReachTheWriterAsADataFile(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12371", "feature", whenQuestion, offQuestion)
	h.tick()
	h.clock.Add(time.Minute)
	const own = "Only for credits above 10 dollars, MARKER-a9"
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[0].Label, 1: own}})

	answers := h.readTaskFile(task.ID, answersFile)
	for _, want := range []string{whenQuestion.Text, whenQuestion.Options[0].Label, offQuestion.Text, own} {
		if !strings.Contains(answers, want) {
			t.Errorf("answers.md lacks %q:\n%s", want, answers)
		}
	}
	coding, _ := h.e.stageOf(h.reload(task.ID), "coding")
	writer, _ := h.cfg.FactoryRole("fx-writer")
	text, err := h.e.renderRole(h.ctx, h.reload(task.ID), coding, writer, writer, h.stageCopy())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, filepath.Join(h.layout.FactoryTaskDir(task.ID), answersFile)) || strings.Contains(text, "MARKER-a9") {
		t.Fatalf("the writer's prompt must name answers.md and carry none of its words:\n%s", text)
	}
}

// A role that ends its turn with questions in the coding stage suspends
// the stage (no nudge); your answers resume it, in the same attempt and
// role, with the answers as its next prompt, and the stage's end fact (the
// factory's proof and precheck of the writer's commit) is checked again.
func TestAQuestionInCodingSuspendsTheStageAndResumesIt(t *testing.T) {
	r := newCodingRig(t)
	h := r.harness
	task := r.codingTask("PR-12372", "feature")
	h.startStage(task)
	first := h.rt.promptsTo("fx-writer")
	if len(first) != 1 {
		t.Fatalf("the writer got %d prompts, want its first", len(first))
	}
	h.taskFile(task.ID, "questions-fx-writer.json", questionsJSON(t, fileQuestion("Reuse CreditMailer or write a new mailer?", 0,
		"reuse CreditMailer", "a new ReminderMailer")))
	h.ended(task, "fx-writer")
	h.tick()

	got := h.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != "1 question before coding goes on" {
		t.Fatalf("task %s waits %q, want needs_you: 1 question before coding goes on", got.State, got.WaitsWhat)
	}
	if n := len(h.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts, want no nudge for a turn that asked", n)
	}
	s, err := h.reg.OpenSuspension(h.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	qs := h.newestQuestions(task.ID)
	if s.Stage != "coding" || s.AttemptID != first[0].Attempt || s.Role != "fx-writer" || len(qs) != 1 || qs[0].RaisedBy != "fx-writer" {
		t.Fatalf("suspension %+v with questions %+v; want coding's attempt %s, fx-writer's question", s, qs, first[0].Attempt)
	}

	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: "reuse CreditMailer"}})
	ps := h.rt.promptsTo("fx-writer")
	if len(ps) != 2 || ps[1].Attempt != first[0].Attempt ||
		!strings.Contains(ps[1].Text, filepath.Join(h.layout.FactoryTaskDir(task.ID), answersFile)) {
		t.Fatalf("the writer's prompts %+v, want the answers next, in attempt %s", ps, first[0].Attempt)
	}
	if strings.Contains(ps[1].Text, "reuse CreditMailer") {
		t.Fatalf("the prompt carries the answer's words:\n%s", ps[1].Text)
	}
	if _, err := h.reg.OpenSuspension(h.ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("OpenSuspension = %v, want the suspension resumed", err)
	}
	if got := h.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho != "" {
		t.Fatalf("task %s waits %q: %q, want active", got.State, got.WaitsWho, got.WaitsWhat)
	}

	r.fix(task)
	h.ended(task, "fx-writer")
	h.tick()
	h.e.waitBackground() // the proof and the precheck run off the loop
	h.tick()
	if got := h.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task %s at %s waiting %q, want past coding once its end fact holds", got.State, got.Stage, got.WaitsWhat)
	}
	if as := h.attempts(task.ID); len(as) != 1 {
		t.Fatalf("attempts %+v, want the one attempt suspended and resumed", as)
	}
}

// A turn that ended on questions did what it was asked: its run is voided,
// never failed, so nothing that counts failed turns (the stats, a learner
// reading the runs) takes it for a prompt's fault.
func TestATurnThatAskedIsVoidedNotFailed(t *testing.T) {
	h := newHarness(t)
	codingEndsOnFindings(h)
	task := h.kindTask("PR-12382", "feature", "coding")
	h.startStage(task)
	asked := h.lastRun("fx-writer")
	h.taskFile(task.ID, "questions-fx-writer.json", questionsJSON(t, fileQuestion("Reuse CreditMailer?", 0, "yes", "no")))
	h.ended(task, "fx-writer")
	h.tick()

	want := verifyCall{RunID: asked, Why: "it asked questions: the stage waits for your answers"}
	if vs := h.rt.voids(); !slices.Equal(vs, []verifyCall{want}) {
		t.Fatalf("Void calls %+v, want the turn that asked voided", vs)
	}
	if slices.ContainsFunc(h.rt.verifies(), func(v verifyCall) bool { return v.RunID == asked }) {
		t.Fatalf("Verify calls %+v: the turn that asked was settled as failed", h.rt.verifies())
	}
	if got := h.reload(task.ID); got.State != registry.TaskNeedsYou {
		t.Fatalf("task %s, want needs_you for its question", got.State)
	}
}

// A role whose last turn that may have arrived was given up by a recovery
// (the runtime's RecoveryPrefix: its session quit and no new turn took its
// place, or the new turn's prompt proven never to have arrived) is
// recovered once the stage is rebuilt, after a restart too: the runtime's
// memory of it is gone, and the stage would wait in silence. A role whose
// last turn paused is not: the stage sends it the prompt to go on.
func TestARebuiltStageRecoversARoleARecoveryLeftWithoutATurn(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12383", "bug", "investigate")
	h.startStage(task)
	h.rt.abandonRun(h.lastRun("fx-repro"), runtime.RecoveryPrefix+"its turn hung")
	h.rt.abandonRun(h.lastRun("fx-history"), runtime.PausedPrefix+"usage: claude limit until 16:00")
	h.restart()
	h.startStage(task)

	if got := h.rt.recoveries(); !slices.Equal(got, []string{TaskKey(task) + "/fx-repro"}) {
		t.Fatalf("RecoverLost calls %v, want fx-repro's alone", got)
	}
	h.tick()
	if got := h.rt.recoveries(); len(got) != 1 {
		t.Fatalf("RecoverLost calls %v, want one: the stage known to this daemon asks no more", got)
	}
}

// Your retry of a task whose role got stuck with nothing at work (every
// step failed once the recovery quit its session, so its last turn is one a
// recovery gave up) goes on: the retry rebuilds the stage, as a restart
// does, and the role is recovered. Before, the retry kept the stage as it
// was, which waited for the end of a turn that no longer existed.
func TestARetryAfterAStuckRoleWithNoTurnRecoversIt(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12384", "bug", "investigate")
	h.startStage(task)
	h.rt.abandonRun(h.lastRun("fx-repro"), runtime.RecoveryPrefix+"its turn hung")
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventStuck})
	h.tick()
	if got := h.reload(task.ID); got.State != registry.TaskNeedsYou || got.WaitsWhat != "the fx-repro agent is stuck" {
		t.Fatalf("task %s waits %q, want needs_you: the fx-repro agent is stuck", got.State, got.WaitsWhat)
	}

	if q := h.retry(task.ID); q.State != registry.RequestDone {
		t.Fatalf("your retry: %+v", q)
	}
	h.tick()
	if got := h.rt.recoveries(); !slices.Equal(got, []string{TaskKey(task) + "/fx-repro"}) {
		t.Fatalf("RecoverLost calls %v after your retry, want fx-repro's", got)
	}
}

// A restart between your answers and the resumed role's prompt sends that
// prompt once.
func TestARestartBetweenTheAnswersAndTheResumeSendsTheAnswersOnce(t *testing.T) {
	h := newHarness(t)
	codingEndsOnFindings(h)
	task := h.kindTask("PR-12373", "feature", "coding")
	h.startStage(task)
	h.taskFile(task.ID, "questions-fx-writer.json", questionsJSON(t, fileQuestion("Reuse CreditMailer?", 0, "yes", "no")))
	h.ended(task, "fx-writer")
	h.tick()
	h.restart() // a restart while the stage waits for your answers asks nothing again
	h.startStage(task)
	if got := h.reload(task.ID); got.State != registry.TaskNeedsYou || got.QuestionRev != 1 {
		t.Fatalf("after a restart the task is %s at revision %d, want needs_you at 1", got.State, got.QuestionRev)
	}

	h.rt.setSendErr(&runtime.WaitError{Who: "usage", What: "claude limit until 16:00"}) // the answers prompt waits
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "yes"}})
	if n := len(h.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts while Send waited, want only its first", n)
	}
	h.restart()
	h.rt.setSendErr(nil)
	h.startStage(task)
	h.restart()
	h.startStage(task)
	ps := h.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, answersLead) {
		t.Fatalf("the writer's prompts %+v, want the answers once after the restarts", ps)
	}
	if _, err := h.reg.OpenSuspension(h.ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("OpenSuspension = %v, want resumed", err)
	}

	// The turn the answers started ends with the questions file as it was:
	// its end fact decides, no answers go out again.
	h.taskFile(task.ID, "findings-fx-writer.md", "committed abc1234\n")
	h.ended(task, "fx-writer")
	h.tick()
	if got := h.reload(task.ID); got.Stage != "local_review" || len(h.rt.promptsTo("fx-writer")) != 2 {
		t.Fatalf("task %s at %s with %d prompts to the writer, want past coding on the two", got.State, got.Stage, len(h.rt.promptsTo("fx-writer")))
	}
}

// A stage that waits on the person you asked goes back to work once their
// answer came, even when another wait (a copy's claim after a restart)
// replaced that wait meanwhile.
func TestAnAnsweredAskResumesTheStageWhateverItsWaitSaid(t *testing.T) {
	h := newHarness(t)
	codingEndsOnFindings(h)
	task := h.kindTask("PR-12379", "feature", "coding")
	h.startStage(task)
	h.taskFile(task.ID, "questions-fx-writer.json", questionsJSON(t, fileQuestion("Reuse CreditMailer?", 0, "yes", "no")))
	h.ended(task, "fx-writer")
	h.tick()
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "ask:rev-ann"}})
	if got := h.reload(task.ID); got.State != registry.TaskWaiting || got.WaitsWho != "rev-ann" {
		t.Fatalf("task %s waits %q: %q, want waiting on rev-ann", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if err := h.reg.TransitionTask(h.ctx, task.ID, nil, "", func(u *registry.TaskUpdate) {
		u.Set("waits_who", "copy")
		u.Set("waits_what", "preparing factory1")
	}); err != nil {
		t.Fatal(err)
	}
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "yes"}})
	if got := h.reload(task.ID); got.State != registry.TaskActive {
		t.Fatalf("task %s waits %q: %q after the answer came, want active", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if ps := h.rt.promptsTo("fx-writer"); len(ps) != 2 || !strings.HasPrefix(ps[1].Text, answersLead) {
		t.Fatalf("the writer's prompts %+v, want the answers", ps)
	}
}

// A role whose end fact holds and whose questions a later gate asks
// (an investigator, the critic) passes: the questions wait for the gate.
func TestQuestionsOfARoleThatPassedWaitForTheGate(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12374", "feature", "investigate")
	h.startStage(task)
	h.taskFile(task.ID, "findings-fx-points.md", "point 1 exists in app/models/credit.rb:12\n")
	h.taskFile(task.ID, "questions-fx-points.json", questionsJSON(t, whoQuestion))
	h.taskFile(task.ID, "findings-fx-points-codex.md", "point 1 exists in app/models/credit.rb:12\n")
	h.ended(task, "fx-points", "fx-points-codex")
	h.tick()
	if got := h.reload(task.ID); got.Stage != "judge" || got.QuestionRev != 0 {
		t.Fatalf("task %s at %s, revision %d; want at judge with nothing asked yet", got.State, got.Stage, got.QuestionRev)
	}
}

// A role whose end fact fails on its questions file itself (the judge's
// questions.json lacks the reproduction question its report needs) gets
// its nudge about that file: the stage is not suspended for the other
// questions in it.
func TestAQuestionsFileTheEndFactRefusesGetsTheNudge(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12380", "bug", "judge")
	h.startStage(task)
	h.taskFile(task.ID, reportFile, reportWith("Proof", "## Proof\nReproduced: no.\n"))
	h.taskFile(task.ID, acceptanceFile, goodAcceptance)
	h.taskFile(task.ID, questionsFile, questionsJSON(t, whenQuestion))
	h.ended(task, "fx-judge")
	h.tick()
	ps := h.rt.promptsTo("fx-judge")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, nudgeLead) || !strings.Contains(ps[1].Text, "reproduction question") {
		t.Fatalf("the judge's prompts %+v, want a nudge for the reproduction question", ps)
	}
	if got := h.reload(task.ID); got.QuestionRev != 0 || got.State != registry.TaskActive {
		t.Fatalf("task %s at revision %d, want active with nothing asked", got.State, got.QuestionRev)
	}
}

// codingEndsOnFindings makes the coding stage of every kind end on the
// findings of its roles: a stand-in end fact for the tests of the
// questions' flow, which need no git (the coding stage's own proof is
// coding_test.go's and proof_test.go's).
func codingEndsOnFindings(h *harness) {
	for name, k := range h.cfg.Factory.Kinds {
		stages := slices.Clone(k.Stages)
		if i := stageIndex(k, "coding"); i >= 0 {
			stages[i].End = config.FactoryEndFindings
		}
		k.Stages = stages
		h.cfg.Factory.Kinds[name] = k
	}
}

// When an answer takes another option than the recommended one, or is
// your own words, one turn of the judge amends report.md (and rewrites its
// acceptance checks and questions) before coding: it passes only on files
// newer than answers.md.
func TestAnswersThatChangeThePlanAmendTheReportFirst(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12375", "feature", whenQuestion)
	h.tick()
	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[1].Label}})
	h.e.waitBackground() // the copy's claim
	h.tick()

	dir := h.layout.FactoryTaskDir(task.ID)
	if got := h.rt.launchedRoles(); len(got) != 1 || !slices.Equal(got[0], []string{"fx-judge"}) {
		t.Fatalf("launches %v, want the judge's amend", got)
	}
	ps := h.rt.promptsTo("fx-judge")
	if len(ps) != 1 || !strings.Contains(ps[0].Text, "amend") {
		t.Fatalf("the judge's prompts %+v, want one amend", ps)
	}
	for _, f := range []string{answersFile, reportFile, acceptanceFile, questionsFile} {
		if !strings.Contains(ps[0].Text, filepath.Join(dir, f)) {
			t.Errorf("the amend prompt does not name %s:\n%s", f, ps[0].Text)
		}
	}
	if got := h.reload(task.ID); got.State != registry.TaskActive || got.Stage != "questions" {
		t.Fatalf("task %s at %s, want active at questions until the report is amended", got.State, got.Stage)
	}

	// A restart while the judge amends sends no second amend.
	h.restart()
	h.startStage(task)
	if n := len(h.rt.promptsTo("fx-judge")); n != 1 {
		t.Fatalf("the judge got %d prompts after a restart, want its one amend", n)
	}

	// A report rewritten without the acceptance checks is not amended: the
	// files must all be newer than answers.md.
	h.clock.Add(time.Minute)
	h.taskFile(task.ID, reportFile, strings.Replace(goodReport, "none\n\n## Cause", "- Send the reminder one day before.\n\n## Cause", 1))
	h.ended(task, "fx-judge")
	h.tick()
	ps = h.rt.promptsTo("fx-judge")
	if len(ps) != 2 || !strings.Contains(ps[1].Text, acceptanceFile) {
		t.Fatalf("the judge's prompts %+v, want a nudge about acceptance.json", ps)
	}
	h.taskFile(task.ID, acceptanceFile, goodAcceptance)
	h.taskFile(task.ID, questionsFile, "[]")
	h.ended(task, "fx-judge")
	h.tick()
	if got := h.reload(task.ID); got.Stage != "coding" {
		t.Fatalf("task %s at %s waiting %q, want coding after the amend", got.State, got.Stage, got.WaitsWhat)
	}
	arts, err := h.reg.Artifacts(h.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(arts, func(a registry.Artifact) bool { return a.Kind == "report" && a.Rev == 1 }) {
		t.Fatalf("artifacts %+v, want the amended report recorded", arts)
	}
}

func TestAnAnswerThatTakesTheRecommendationNeedsNoAmend(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12376", "feature", whenQuestion, whoQuestion)
	h.tick()
	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1,
		Answers: map[int]string{0: whenQuestion.Options[0].Label, 1: " " + whoQuestion.Options[0].Label + " "}})
	if got := h.reload(task.ID); got.Stage != "coding" {
		t.Fatalf("task %s at %s waiting %q, want coding", got.State, got.Stage, got.WaitsWhat)
	}
	if n := len(h.rt.launchedRoles()); n != 0 {
		t.Fatalf("%d launches, want no amend", n)
	}
}

// "Ask someone" keeps the question open: the task waits on that person,
// with the comment drafted in the task's directory, until the answer
// arrives.
func TestAskSomeoneKeepsTheQuestionOpenUntilTheAnswerArrives(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12377", "feature", whenQuestion, offQuestion)
	h.tick()
	h.clock.Add(time.Minute)
	q := h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[0].Label, 1: "ask:rev-ann"}})

	got := h.reload(task.ID)
	if got.State != registry.TaskWaiting || got.WaitsWho != "rev-ann" || got.WaitsWhat != "a question in Jira" {
		t.Fatalf("task %s waits %q: %q, want waiting, rev-ann: a question in Jira", got.State, got.WaitsWho, got.WaitsWhat)
	}
	qs := h.newestQuestions(task.ID)
	if qs[0].State != registry.QuestionAnswered || qs[1].State != registry.QuestionOpen {
		t.Fatalf("questions %+v, want the first answered and the asked one open", qs)
	}
	comment := h.readTaskFile(task.ID, "jira-comment-1-2.md")
	if !strings.Contains(comment, "rev-ann") || !strings.Contains(comment, offQuestion.Text) {
		t.Fatalf("the drafted comment does not ask rev-ann the question:\n%s", comment)
	}
	if !strings.Contains(q.Result, "jira-comment-1-2.md") || !strings.Contains(q.Result, "--revision 1 --question 2") {
		t.Fatalf("the request's result %q does not say where the comment is", q.Result)
	}
	if ins := h.answerInputs(task.ID); len(ins) != 2 || ins[1].Text != "ask:rev-ann" {
		t.Fatalf("answer inputs %+v, want the answer and the ask", ins)
	}
	// The same batch again (a request the daemon took before a restart)
	// stores and records nothing twice.
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[0].Label, 1: "ask:rev-ann"}})
	if ins := h.answerInputs(task.ID); len(ins) != 2 {
		t.Fatalf("answer inputs %+v after the batch came again, want still the two", ins)
	}

	h.tick()
	h.tick()
	if got := h.reload(task.ID); got.State != registry.TaskWaiting || got.QuestionRev != 1 || got.Stage != "questions" {
		t.Fatalf("task %s at %s, revision %d; want still waiting on rev-ann at revision 1", got.State, got.Stage, got.QuestionRev)
	}

	h.clock.Add(time.Hour)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{1: offQuestion.Options[1].Label}})
	if got := h.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho == "rev-ann" {
		t.Fatalf("task %s waits %q: %q after the answer arrived, want active", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if qs := h.newestQuestions(task.ID); qs[1].State != registry.QuestionAnswered {
		t.Fatalf("the asked question is %s, want answered", qs[1].State)
	}
}

// The amend turn's prompt is one the daemon loads at startup and renders
// with this build, as the roles' prompts are; so is a stage's own prompt.
func TestTheAmendPromptIsLoadedAndCheckedAtStartup(t *testing.T) {
	cfg := config.Defaults()
	if names := PromptNames(cfg); !slices.Contains(names, AmendPrompt) || !slices.Contains(names, "factory-judge.md") {
		t.Fatalf("PromptNames = %v, want the roles' prompts and %s", names, AmendPrompt)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, AmendPrompt), []byte("{{template \"task\" .}} {{.NoSuchField}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := config.Defaults()
	broken.Pipeline.PromptsDir = dir
	if err := CheckPrompts(broken); err == nil || !strings.Contains(err.Error(), "amend") || !strings.Contains(err.Error(), AmendPrompt) {
		t.Fatalf("CheckPrompts with a broken %s = %v, want it named", AmendPrompt, err)
	}

	staged := config.Defaults()
	k := staged.Factory.Kinds["bug"]
	k.Stages = slices.Clone(k.Stages)
	k.Stages[stageIndex(k, "pr_body")].Prompt = "factory-pr-body-x.md"
	staged.Factory.Kinds["bug"] = k
	if !slices.Contains(PromptNames(staged), "factory-pr-body-x.md") {
		t.Fatalf("PromptNames = %v, want the stage's own prompt", PromptNames(staged))
	}
	if err := CheckPrompts(staged); err == nil || !strings.Contains(err.Error(), "stage pr_body") {
		t.Fatalf("CheckPrompts with a stage prompt that does not exist = %v, want the stage named", err)
	}
}

func TestANeedlessFlagIsRecordedWithItsReason(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12378", "feature", whenQuestion, whoQuestion)
	h.tick()
	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Needless: map[int]string{1: "obvious"}})
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1,
		Answers: map[int]string{0: whenQuestion.Options[0].Label, 1: whoQuestion.Options[0].Label}})
	qs := h.newestQuestions(task.ID)
	if qs[1].Flag != registry.FlagNeedless || qs[1].FlagWhy != "obvious" || qs[0].Flag != "" {
		t.Fatalf("questions %+v, want the second flagged needless: obvious, the first not flagged", qs)
	}
	if got := h.reload(task.ID); got.Stage != "coding" {
		t.Fatalf("task at %s, want coding", got.Stage)
	}

	// A needless flag names a question of the revision and a reason.
	for _, p := range []AnswerPayload{
		{TaskID: task.ID, Revision: 1, Needless: map[int]string{5: "obvious"}},
		{TaskID: task.ID, Revision: 1, Needless: map[int]string{0: " "}},
	} {
		if q := h.sendAnswers(p); q.State != registry.RequestFailed {
			t.Errorf("needless %v was taken: %s", p.Needless, q.Result)
		}
	}
}

// Your answers go out once per batch: a turn after them that fails its end
// fact gets its one nudge, then the task waits on you for the file, however
// long the questions file of the role stays as it was.
func TestATurnAfterTheAnswersThatFailsItsEndFactGetsOneNudgeThenNeedsYou(t *testing.T) {
	h := newHarness(t)
	codingEndsOnFindings(h)
	task := h.kindTask("PR-12381", "feature", "coding")
	h.startStage(task)
	h.taskFile(task.ID, "questions-fx-writer.json", questionsJSON(t, fileQuestion("Reuse CreditMailer?", 0, "yes", "no")))
	h.ended(task, "fx-writer")
	h.tick()
	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "yes"}})
	for range 2 { // the answers' turn, then the nudge's, end without findings-fx-writer.md
		h.clock.Add(time.Minute)
		h.ended(task, "fx-writer")
		h.tick()
	}
	var leads []string
	for _, p := range h.rt.promptsTo("fx-writer") {
		leads = append(leads, strings.SplitN(p.Text, ":", 2)[0])
	}
	if len(leads) != 3 || !strings.HasPrefix(leads[1], answersLead) || !strings.HasPrefix(leads[2], nudgeLead) {
		t.Fatalf("the writer's prompts begin %q, want its first, the answers and one nudge", leads)
	}
	got := h.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != "the fx-writer agent wrote no valid findings-fx-writer.md" {
		t.Fatalf("task %s waits %q, want needs_you: the fx-writer agent wrote no valid findings-fx-writer.md", got.State, got.WaitsWhat)
	}
}

// A turn after the answers that stopped on a pause goes on with its
// continue prompt, and a valid file then passes the stage: no second
// answers prompt.
func TestAContinueAfterTheAnswersEndsOnItsEndFact(t *testing.T) {
	h := newHarness(t)
	codingEndsOnFindings(h)
	task := h.kindTask("PR-12382", "feature", "coding")
	h.startStage(task)
	h.taskFile(task.ID, "questions-fx-writer.json", questionsJSON(t, fileQuestion("Reuse CreditMailer?", 0, "yes", "no")))
	h.ended(task, "fx-writer")
	h.tick()
	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "yes"}})
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-writer", RunID: h.lastRun("fx-writer"), Kind: runtime.EventPaused,
		Reason: "overload: claude is overloaded, tries again at 09:05"})
	h.tick()
	h.clock.Add(time.Minute)
	h.taskFile(task.ID, "findings-fx-writer.md", "committed abc1234\n")
	h.ended(task, "fx-writer")
	h.tick()
	if got := h.reload(task.ID); got.Stage != "local_review" || len(h.rt.promptsTo("fx-writer")) != 3 {
		t.Fatalf("task %s at %s with %d prompts to the writer, want past coding on its first, the answers and the continue",
			got.State, got.Stage, len(h.rt.promptsTo("fx-writer")))
	}
}

// "drop" at the questions gate (the default bug kind asks the judge's
// reproduction question there) stops the task at once: no amend of the
// report, however the other answers changed the plan.
func TestDroppingABugAtTheQuestionsGateStopsItWithoutAnAmend(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12383", "bug", "questions")
	h.taskFile(task.ID, reportFile, reportWith("Proof", "## Proof\nReproduced: no.\n"))
	h.taskFile(task.ID, acceptanceFile, goodAcceptance)
	var repro []QuestionFile
	if err := json.Unmarshal([]byte(reproQuestion), &repro); err != nil {
		t.Fatal(err)
	}
	h.taskFile(task.ID, questionsFile, questionsJSON(t, repro[0], whenQuestion))
	h.tick()
	h.clock.Add(time.Minute)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "drop", 1: whenQuestion.Options[1].Label}})
	h.e.waitBackground()
	h.tick()
	if got := h.reload(task.ID); got.State != registry.TaskStopped {
		t.Fatalf("task %s at %s waiting %q, want stopped", got.State, got.Stage, got.WaitsWhat)
	}
	if n := len(h.rt.promptsTo("fx-judge")); n != 0 {
		t.Fatalf("the judge got %d prompts, want no amend for a dropped task", n)
	}
}

// Each other answer to the reproduction question at the questions gate is
// acted on once: one amend of the report, wherever it runs, and the
// question is not asked again once the amended Proof reproduced the bug.
func TestEachReproductionAnswerAtTheQuestionsGateIsActedOnOnce(t *testing.T) {
	for _, answer := range []string{"give a hint", "take the other cause", "try Docker or a staging"} {
		t.Run(answer, func(t *testing.T) {
			h := newHarness(t)
			task := h.kindTask("PR-12384", "bug", "questions")
			h.taskFile(task.ID, reportFile, reportWith("Proof", "## Proof\nReproduced: no.\n"))
			h.taskFile(task.ID, acceptanceFile, goodAcceptance)
			h.taskFile(task.ID, questionsFile, reproQuestion)
			h.tick()
			h.clock.Add(time.Minute)
			h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: answer}})
			h.tick()
			h.e.waitBackground()
			h.tick()
			if n := len(h.rt.promptsTo("fx-judge")); n != 1 {
				t.Fatalf("the judge got %d prompts, want one amend", n)
			}
			h.clock.Add(time.Minute)
			h.taskFile(task.ID, reportFile, goodReport)
			h.taskFile(task.ID, acceptanceFile, goodAcceptance)
			h.taskFile(task.ID, questionsFile, "[]")
			h.ended(task, "fx-judge")
			h.tick()
			h.tick()
			got := h.reload(task.ID)
			if got.Stage != "coding" || got.QuestionRev != 1 || len(h.rt.promptsTo("fx-judge")) != 1 {
				t.Fatalf("task %s at %s, revision %d, %d prompts to the judge; want coding on one amend, nothing asked again",
					got.State, got.Stage, got.QuestionRev, len(h.rt.promptsTo("fx-judge")))
			}
		})
	}
}

// linkRig is an intake rig (Jira behind a fake curl) whose task of a free
// prompt waits at the questions gate on intake's question which Jira issue
// it is; its judge wrote the report.
func linkRig(t *testing.T) (*intakeRig, registry.Task) {
	t.Helper()
	r := newIntakeRig(t)
	r.stage(config.FactoryStageAgents, r.e.runAgents) // the amend turn runs
	r.stageCopy()
	task, err := r.reg.CreateTask(r.ctx, registry.Task{Prompt: "remind advocates before their credit expires", Kind: "feature",
		Repo: "talkable/talkable", Title: "remind advocates before their credit expires", State: registry.TaskActive, Stage: "questions",
		Branch: "zhuravel-remind-advocates"})
	if err != nil {
		t.Fatal(err)
	}
	q := registry.Question{Text: whichIssue, RaisedBy: intakeRole, Recommended: 0, Options: []registry.Option{
		{Label: "PR-12345 Credit expiry reminder email", Means: "the task follows PR-12345"}, {Label: noIssue, Means: "your words only"}}}
	if err := r.reg.ReplaceQuestions(r.ctx, task.ID, 1, []registry.Question{q}); err != nil {
		t.Fatal(err)
	}
	r.harness.taskFile(task.ID, reportFile, goodReport)
	r.harness.taskFile(task.ID, acceptanceFile, goodAcceptance)
	r.harness.taskFile(task.ID, questionsFile, "[]")
	r.tick()
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || got.WaitsWhat != "1 question before coding" {
		t.Fatalf("task %s waits %q, want needs_you: 1 question before coding", got.State, got.WaitsWhat)
	}
	return r, task
}

// An answer to "Which Jira issue is this?" that names a key links the task
// to it (its branch stays), reads the ticket into ticket.md (waiting while
// Jira is down) and has the judge amend the report with it; a restart
// repeats none of the three.
func TestAnAnswerThatNamesAJiraKeyLinksTheTaskAndReadsItsTicket(t *testing.T) {
	r, task := linkRig(t)
	r.site.setDown(7)
	r.clock.Add(time.Minute)
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "PR-12345 Credit expiry reminder email"}})
	got := r.reload(task.ID)
	if got.JiraKey != "PR-12345" || got.Branch != task.Branch || got.State != registry.TaskActive || got.WaitsWho != "Jira" ||
		got.WaitsWhat != "read PR-12345 when Jira answers" {
		t.Fatalf("task %+v; want PR-12345 linked, its branch kept, active, Jira: read PR-12345 when Jira answers", got)
	}
	ticket := filepath.Join(r.layout.FactoryTaskDir(task.ID), "ticket.md")
	if _, err := os.Stat(ticket); !errors.Is(err, os.ErrNotExist) || len(r.rt.launchedRoles()) != 0 {
		t.Fatalf("ticket.md: %v, launches %v; want neither while Jira is down", err, r.rt.launchedRoles())
	}

	r.restart()
	r.site.setDown(0)
	r.tick()
	r.e.waitBackground()
	r.tick()
	if b, err := os.ReadFile(ticket); err != nil || !strings.Contains(string(b), "Credit expiry reminder email") {
		t.Fatalf("ticket.md = %q, %v; want PR-12345's ticket", b, err)
	}
	got = r.reload(task.ID)
	if r.site.reads() != 1 || got.WaitsWho == "Jira" || got.Title != "Credit expiry reminder email" {
		t.Fatalf("%d reads of Jira, task waits %q: %q, titled %q; want one read and the ticket's title", r.site.reads(), got.WaitsWho,
			got.WaitsWhat, got.Title)
	}
	ps := r.rt.promptsTo("fx-judge")
	if len(ps) != 1 || !strings.Contains(ps[0].Text, ticket) || !strings.Contains(ps[0].Text, "amend") {
		t.Fatalf("the judge's prompts %+v, want one amend that names ticket.md", ps)
	}

	r.restart()
	r.startStage(task)
	if n := len(r.rt.promptsTo("fx-judge")); n != 1 || r.site.reads() != 1 {
		t.Fatalf("after a restart: %d amends, %d reads of Jira; want one each", n, r.site.reads())
	}
	r.clock.Add(time.Minute)
	r.harness.taskFile(task.ID, reportFile, goodReport)
	r.harness.taskFile(task.ID, acceptanceFile, goodAcceptance)
	r.harness.taskFile(task.ID, questionsFile, "[]")
	r.ended(task, "fx-judge")
	r.tick()
	if got := r.reload(task.ID); got.Stage != "coding" || len(r.rt.promptsTo("fx-judge")) != 1 {
		t.Fatalf("task %s at %s, want coding after the one amend", got.State, got.Stage)
	}
}

// A key another task holds is refused, naming that task; the question
// stays open and the task links nothing.
func TestAJiraKeyAnotherTaskHoldsIsRefusedAndTheQuestionStaysOpen(t *testing.T) {
	r, task := linkRig(t)
	other, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12345", Kind: "feature", Repo: "talkable/talkable", State: registry.TaskActive})
	if err != nil {
		t.Fatal(err)
	}
	q := r.sendAnswers(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "PR-12345 Credit expiry reminder email"}})
	if q.State != registry.RequestFailed || !strings.Contains(q.Result, fmt.Sprintf("task %d", other.ID)) {
		t.Fatalf("request %s: %q; want refused, naming task %d", q.State, q.Result, other.ID)
	}
	if got := r.reload(task.ID); got.JiraKey != "" || got.State != registry.TaskNeedsYou {
		t.Fatalf("task %s with key %q, want needs_you with none", got.State, got.JiraKey)
	}
	if qs := r.newestQuestions(task.ID); qs[0].State != registry.QuestionOpen {
		t.Fatalf("the question is %s, want open", qs[0].State)
	}
}

// A key Jira refuses (a typo: no such issue, or one the token cannot see)
// is no dead end: the link is undone and the question is asked again, in a
// new revision that gives the cause; your next answer links the right key.
func TestAKeyJiraRefusesIsUnlinkedAndTheQuestionAskedAgain(t *testing.T) {
	r, task := linkRig(t)
	r.clock.Add(time.Minute)
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "PR-12399"}})
	got := r.reload(task.ID)
	if got.JiraKey != "" || got.QuestionRev != 2 || got.State != registry.TaskNeedsYou || got.WaitsWhat != "1 question before coding" {
		t.Fatalf("task %s with key %q at revision %d waits %q; want needs_you with no key at revision 2: 1 question before coding",
			got.State, got.JiraKey, got.QuestionRev, got.WaitsWhat)
	}
	qs := r.newestQuestions(task.ID)
	want := "PR-12399 is not in Jira: type the key again or answer no issue"
	if len(qs) != 1 || qs[0].Text != whichIssue || qs[0].State != registry.QuestionOpen || !strings.Contains(qs[0].Context, want) ||
		len(qs[0].Options) != 2 {
		t.Fatalf("revision 2 is %+v, want the question which Jira issue again, open, its options kept, saying %q", qs, want)
	}

	r.clock.Add(time.Minute)
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 2, Answers: map[int]string{0: "PR-12345 Credit expiry reminder email"}})
	if got := r.reload(task.ID); got.JiraKey != "PR-12345" || got.Title != "Credit expiry reminder email" {
		t.Fatalf("task with key %q titled %q, want PR-12345 linked and its ticket read", got.JiraKey, got.Title)
	}
	if answers := r.taskFile(task.ID, answersFile); strings.Contains(answers, "PR-12399") || !strings.Contains(answers, "PR-12345") {
		t.Fatalf("answers.md must hold the answer that stands, not the refused key:\n%s", answers)
	}
}

// Only an issue Jira cannot show (no such issue, or one the token may not
// see) asks for the key again; Jira's other refusals (a token file gone, a
// wrong token, another status) are yours to fix, the link kept.
func TestOnlyAnIssueJiraCannotShowAsksForTheKeyAgain(t *testing.T) {
	for class, want := range map[string]bool{jira.ClassNotFound: true, jira.ClassForbidden: true, jira.ClassUnauthorized: false,
		jira.ClassTokenFile: false, jira.ClassStatus: false, jira.ClassMalformed: false} {
		if got := jiraRefusesKey(&jira.Error{Class: class}); got != want {
			t.Errorf("jiraRefusesKey(%s) = %v, want %v", class, got, want)
		}
	}
	if jiraRefusesKey(errors.New("curl: exit 7")) {
		t.Error("an error that is not Jira's refuses the key")
	}
}

// A restart between the write of ticket.md and the end of the link (the
// title, the wait for Jira) ends the link: ticket.md is there, so Jira is
// not read again.
func TestARestartAfterTheTicketWasWrittenEndsTheLink(t *testing.T) {
	r, task := linkRig(t)
	r.site.setDown(7)
	r.clock.Add(time.Minute)
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "PR-12345 Credit expiry reminder email"}})
	r.site.setDown(0)
	tk, err := r.client.Ticket(r.ctx, "PR-12345") // the read that wrote ticket.md before the daemon stopped
	if err != nil {
		t.Fatal(err)
	}
	r.harness.taskFile(task.ID, ticketFile, string(tk.Markdown()))
	reads := r.site.reads()

	r.restart()
	r.tick()
	got := r.reload(task.ID)
	if got.WaitsWho == "Jira" || got.Title != "Credit expiry reminder email" || r.site.reads() != reads {
		t.Fatalf("task waits %q: %q titled %q after %d more reads; want the link ended from ticket.md, no read", got.WaitsWho,
			got.WaitsWhat, got.Title, r.site.reads()-reads)
	}
}

// Jira that never answers the read of a linked key sends the task to you
// after 6 hours, as a start's wait for Jira does.
func TestALinkedKeyJiraNeverAnswersNeedsYouAfterSixHours(t *testing.T) {
	r, task := linkRig(t)
	r.site.setDown(7)
	r.clock.Add(time.Minute)
	answered := r.clock.Now()
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "PR-12345 Credit expiry reminder email"}})
	for _, d := range []time.Duration{time.Hour, 3 * time.Hour, 5*time.Hour + 59*time.Minute} {
		r.clock.Set(answered.Add(d))
		r.tick()
		if got := r.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho != "Jira" {
			t.Fatalf("at +%s the task is %s waiting %q: %q, want still waiting on Jira", d, got.State, got.WaitsWho, got.WaitsWhat)
		}
	}
	r.clock.Set(answered.Add(6 * time.Hour))
	r.tick()
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || got.WaitsWhat != "Jira did not answer for 6h" {
		t.Fatalf("after 6h the task is %s waiting %q, want needs_you: Jira did not answer for 6h", got.State, got.WaitsWhat)
	}
}

// "no issue", recommended or not, links nothing, reads no ticket and needs
// no amend.
func TestNoIssueLinksNothingAndNeedsNoAmend(t *testing.T) {
	r, task := linkRig(t)
	r.clock.Add(time.Minute)
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: noIssue}})
	got := r.reload(task.ID)
	if got.Stage != "coding" || got.JiraKey != "" || r.site.reads() != 0 || len(r.rt.launchedRoles()) != 0 {
		t.Fatalf("task %s at %s, key %q, %d reads, launches %v; want coding with nothing linked, read or amended", got.State, got.Stage,
			got.JiraKey, r.site.reads(), r.rt.launchedRoles())
	}
}

// ask:<login> names a GitHub login, and never you: anything else is
// refused, the question stays open and nothing is drafted.
func TestAnAskNamesAGitHubLogin(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12385", "feature", whenQuestion)
	h.tick()
	for _, bad := range []string{"ask:@rev-ann", "ask:you", "ask:You", "ask:rev_ann", "ask:", "ask:-rev"} {
		if q := h.sendAnswers(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: bad}}); q.State != registry.RequestFailed {
			t.Errorf("%q was taken: %s", bad, q.Result)
		}
	}
	if qs := h.newestQuestions(task.ID); qs[0].State != registry.QuestionOpen {
		t.Fatalf("the question is %s, want open", qs[0].State)
	}
	if _, err := os.Stat(filepath.Join(h.layout.FactoryTaskDir(task.ID), asksFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("asks.json: %v, want none", err)
	}
	if got := h.reload(task.ID); got.State != registry.TaskNeedsYou || got.WaitsWho != "you" {
		t.Fatalf("task %s waits %q: %q, want still needs_you", got.State, got.WaitsWho, got.WaitsWhat)
	}
}

// A Codex judge that ran the report as its Claude fallback (a task Codex
// never works on, or one where it yielded to Codex's soft cap, which marks
// nothing) raises questions.json's questions under the name it ran by, the
// name its run that ended on the report has: stats find it as the judge, so
// a question it alone raised counts for the investigator whose findings it
// cites, never for the judge.
func TestAJudgeThatRanAsItsFallbackRaisesItsQuestionsUnderThatName(t *testing.T) {
	for _, never := range []bool{true, false} {
		t.Run(fmt.Sprintf("codex never %v", never), func(t *testing.T) {
			h := newHarness(t)
			i := slices.IndexFunc(h.cfg.Factory.Roles, func(r config.Role) bool { return r.Name == "fx-judge" })
			claude := h.cfg.Factory.Roles[i]
			claude.Name = "fx-judge-claude"
			h.cfg.Factory.Roles[i].Kind = config.KindCodex
			h.cfg.Factory.Roles = append(h.cfg.Factory.Roles, claude)
			h.cfg.Factory.CodexFallback["fx-judge"] = "fx-judge-claude"
			q := offQuestion
			q.Evidence = "findings-fx-cause.md: the guard"
			task := h.gateTask("PR-12347", "bug", q)
			if never {
				if err := h.reg.TransitionTask(h.ctx, task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("codex_never", true) }); err != nil {
					t.Fatal(err)
				}
			}
			a, err := h.reg.BeginAttempt(h.ctx, task.ID, "judge")
			if err != nil {
				t.Fatal(err)
			}
			if err := h.reg.RecordRoleRun(h.ctx, registry.RoleRun{AttemptID: a.ID, Role: "fx-judge-claude", EndFact: config.FactoryEndReport,
				Outcome: "verified"}); err != nil {
				t.Fatal(err)
			}
			h.tick()
			qs, err := h.reg.Questions(h.ctx, task.ID)
			if err != nil || len(qs) != 1 || qs[0].RaisedBy != "fx-judge-claude" {
				t.Fatalf("questions %+v (%v), want the judge's one raised by fx-judge-claude, as it ran", qs, err)
			}
			rep, err := stats.ComputeAt(h.ctx, h.reg, time.Time{}, h.clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Tasks) != 1 || rep.Tasks[0].QuestionsFrom["fx-cause"] != 1 || len(rep.Tasks[0].QuestionsFrom) != 1 {
				t.Fatalf("stats %+v, want the question counted for fx-cause, whose findings the judge cites", rep.Tasks)
			}
		})
	}
}
