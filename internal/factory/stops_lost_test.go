package factory

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
)

// lostWait is the NEEDS YOU line of a task whose fx-repro pane was lost
// after its recoveries: the question's decision, never "retry".
const lostWait = "1 question before investigate goes on: the fx-repro agent's pane was lost: no progress after 3 recoveries: " +
	"start the role again, give a direction or drop the task?"

// lostAfterRecoveries starts task key's investigate stage and has the
// runtime say fx-repro's recoveries ran out: the task asks you its question.
func (h *harness) lostAfterRecoveries(key string) registry.Task {
	h.t.Helper()
	task := h.kindTask(key, "bug", "investigate")
	h.startStage(task)
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", RunID: h.lastRun("fx-repro"), Kind: runtime.EventLost,
		Reason: "no progress after 3 recoveries"})
	h.tick()
	got := h.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != lostWait {
		h.t.Fatalf("task %s waits %q, want needs_you: %s", got.State, got.WaitsWhat, lostWait)
	}
	return got
}

// A role whose lost pane's recoveries ran out asks you a question with
// concrete options (start it again, give it a direction, drop the task),
// raised by the factory, once: a later pass, a restart and the same event
// again ask nothing twice. The NEEDS YOU line names the decision, never
// "retry".
func TestALostPaneAfterItsRecoveriesAsksAQuestionNeverRetry(t *testing.T) {
	h := newHarness(t)
	task := h.lostAfterRecoveries("PR-12540")
	qs := h.newestQuestions(task.ID)
	if len(qs) != 1 || qs[0].RaisedBy != factoryAsks || qs[0].State != registry.QuestionOpen {
		t.Fatalf("questions %+v, want the factory's one open question", qs)
	}
	var labels []string
	for _, o := range qs[0].Options {
		labels = append(labels, o.Label)
	}
	if !slices.Equal(labels, []string{optionStart, optionDirection, optionDrop}) {
		t.Fatalf("options %q, want start, a direction and drop", labels)
	}
	for _, text := range []string{task.WaitsWhat, qs[0].Text, qs[0].Context} {
		if strings.Contains(strings.ToLower(text), "retry") {
			t.Fatalf("%q offers a retry", text)
		}
	}

	h.restart()
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventLost, Reason: "no progress after 3 recoveries"})
	h.tick()
	if got := h.reload(task.ID); got.QuestionRev != task.QuestionRev || got.WaitsWhat != lostWait {
		t.Fatalf("task at revision %d waits %q after a restart; want the one question, still asked", got.QuestionRev, got.WaitsWhat)
	}
}

// Your answer "start the role again" starts it in a fresh session with its
// context (the attempt's first prompt, then the words that say why), once:
// a restart after it sends nothing again. A direction goes to that fresh
// session in your words.
func TestYourAnswerToALostPanesQuestionStartsTheRoleAfresh(t *testing.T) {
	for _, tc := range []struct {
		name, answer, words string
	}{
		{"start it again", optionStart, ""},
		{"a direction", optionDirection + ": the seeds need the staging locale", "the seeds need the staging locale"},
		{"your own words", "the seeds need the staging locale", "the seeds need the staging locale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			task := h.lostAfterRecoveries("PR-12541")
			before := len(h.rt.promptsTo("fx-repro"))
			h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: task.QuestionRev, Answers: map[int]string{0: tc.answer}})
			ps := h.rt.promptsTo("fx-repro")
			if len(ps) != before+1 || !ps[len(ps)-1].Fresh || !strings.HasPrefix(ps[len(ps)-1].Text, answerLead) ||
				!strings.Contains(ps[len(ps)-1].Text, tc.words) {
				t.Fatalf("prompts to fx-repro %+v; want one fresh session that starts with %q and names %q", ps[before:], answerLead, tc.words)
			}
			if got := h.reload(task.ID); got.State != registry.TaskActive {
				t.Fatalf("task %s waits %q after your answer, want it at work", got.State, got.WaitsWhat)
			}
			h.restart()
			h.tick()
			if n := len(h.rt.promptsTo("fx-repro")); n != before+1 {
				t.Fatalf("%d prompts to fx-repro after a restart, want the fresh start once", n-before)
			}
		})
	}
}

// Your answer "drop the task" stops it, recorded as your drop.
func TestYourDropOfALostPanesQuestionStopsTheTask(t *testing.T) {
	h := newHarness(t)
	task := h.lostAfterRecoveries("PR-12542")
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: task.QuestionRev, Answers: map[int]string{0: optionDrop}})
	if got := h.reload(task.ID); got.State != registry.TaskStopped {
		t.Fatalf("task %s waits %q after your drop, want it stopped", got.State, got.WaitsWhat)
	}
	ins, err := h.reg.Inputs(h.ctx, task.ID)
	if err != nil || !slices.ContainsFunc(ins, func(in registry.Input) bool { return in.Kind == registry.InputDrop }) {
		t.Fatalf("inputs %+v (%v), want your drop", ins, err)
	}
}

// Your retry still works on a lost pane's question, as before it: the
// question closes, the stage is rebuilt and the runtime recovers the role,
// whose recoveries your retry starts over without spending one.
func TestYourRetryOfALostPanesQuestionClosesItAndRecoversTheRole(t *testing.T) {
	h := newHarness(t)
	task := h.lostAfterRecoveries("PR-12543")
	h.rt.abandonRun(h.lastRun("fx-repro"), runtime.RecoveryPrefix+"its pane was lost")
	if q := h.retry(task.ID); q.State != registry.RequestDone {
		t.Fatalf("retry %s: %s, want it accepted", q.State, q.Result)
	}
	h.tick()
	got := h.reload(task.ID)
	if got.State != registry.TaskActive {
		t.Fatalf("task %s waits %q after your retry, want it at work", got.State, got.WaitsWhat)
	}
	if open := h.newestQuestions(task.ID); slices.ContainsFunc(open, func(q registry.Question) bool { return q.State == registry.QuestionOpen }) {
		t.Fatalf("questions %+v after your retry, want the stop's closed", open)
	}
	if rec := h.rt.recoveries(); !slices.Contains(rec, TaskKey(task)+"/fx-repro") {
		t.Fatalf("recoveries %q after your retry, want fx-repro's", rec)
	}
}
