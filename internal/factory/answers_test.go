package factory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// shownTimes are the times the questions of the task were shown, by revision
// and index (a question nobody saw has the zero time).
func (h *harness) shownTimes(id int64) map[[2]int]time.Time {
	h.t.Helper()
	all, err := h.reg.Questions(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[[2]int]time.Time{}
	for _, q := range all {
		out[[2]int{q.Revision, q.Index}] = q.Shown
	}
	return out
}

// An answer from the liaison carries the time it noted before it asked: it is
// when the batch was shown, so the stats count the operator's time on it from
// then, and the answers are recorded as the liaison's, not the CLI's.
func TestAnAnswerFromTheLiaisonRecordsItsSurfaceAndWhenTheBatchWasShown(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12371", "feature", whenQuestion, offQuestion)
	h.tick()
	h.clock.Add(10 * time.Minute)
	shown := h.clock.Now().Add(-4 * time.Minute)

	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Surface: "liaison", Shown: shown,
		Answers: map[int]string{0: whenQuestion.Options[0].Label}, Needless: map[int]string{1: "ticket-answers"}})

	for key, got := range h.shownTimes(task.ID) {
		if !got.Equal(shown) {
			t.Errorf("question %v shown at %v, want %v: the whole batch was shown together, answered or not", key, got, shown)
		}
	}
	ins := h.answerInputs(task.ID)
	if len(ins) != 1 || ins[0].Surface != "liaison" {
		t.Fatalf("answer inputs %+v, want the one answer, from the liaison", ins)
	}
}

// A batch the liaison asked and the operator pointed at someone else is shown
// all the same: the operator read the questions.
func TestAnAskedBatchFromTheLiaisonIsShownAllTheSame(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12372", "feature", whenQuestion)
	h.tick()
	h.clock.Add(10 * time.Minute)
	shown := h.clock.Now().Add(-4 * time.Minute)

	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Surface: "liaison", Shown: shown, Answers: map[int]string{0: "ask:rev-ann"}})

	if got := h.shownTimes(task.ID)[[2]int{1, 0}]; !got.Equal(shown) {
		t.Errorf("the asked question shown at %v, want %v", got, shown)
	}
}

// An answer without a surface is the CLI's, and one without a shown time
// leaves the questions unshown, as every answer did before the liaison: no
// time is invented for the operator's card.
func TestAnAnswerWithoutASurfaceOrAShownTimeChangesNothingAboutThem(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12373", "feature", whenQuestion)
	h.tick()
	h.clock.Add(time.Minute)

	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[0].Label}})

	if got := h.shownTimes(task.ID)[[2]int{1, 0}]; !got.IsZero() {
		t.Errorf("the question shown at %v, want it unshown: the answer named no time", got)
	}
	ins := h.answerInputs(task.ID)
	if len(ins) != 1 || ins[0].Surface != "cli" {
		t.Fatalf("answer inputs %+v, want the one answer, from the CLI", ins)
	}
}

// A batch for questions that changed since is refused and nothing of it is
// kept: not the answers, and not the time they were said to be shown.
func TestAStaleBatchFromTheLiaisonRecordsNoShownTime(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12374", "feature", whenQuestion)
	h.tick()
	h.clock.Add(5 * time.Minute)
	carried := registry.Question{Text: whenQuestion.Text, Options: whenQuestion.Options, Recommended: -1, RaisedBy: "fx-judge"}
	if err := h.reg.ReplaceQuestions(h.ctx, task.ID, 2, []registry.Question{carried}); err != nil {
		t.Fatal(err)
	}

	q := h.sendAnswers(AnswerPayload{TaskID: task.ID, Revision: 1, Surface: "liaison", Shown: h.clock.Now().Add(-time.Minute),
		Answers: map[int]string{0: whenQuestion.Options[0].Label}})
	if q.State != registry.RequestFailed {
		t.Fatalf("request %s: %q; want it refused as stale", q.State, q.Result)
	}
	for key, got := range h.shownTimes(task.ID) {
		if !got.IsZero() {
			t.Errorf("question %v shown at %v, want none: the batch was refused", key, got)
		}
	}
	if _, err := os.Stat(filepath.Join(h.layout.FactoryTaskDir(task.ID), answersFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("answers.md: %v, want none written", err)
	}
}
