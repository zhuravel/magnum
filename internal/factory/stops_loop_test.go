package factory

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
)

// loopWait is the NEEDS YOU line of a task whose fx-repro agent the
// runtime's loop guard found making no progress: the question's decision,
// never "retry".
const loopWait = "1 question before investigate goes on: the fx-repro agent makes no progress: the same failure 3 times: " +
	"give a direction, start the role in a fresh session or drop the task?"

// loopNoProgress starts task key's investigate stage and has the runtime's
// loop guard say fx-repro makes no progress: the task asks you its question.
func (h *harness) loopNoProgress(key string) registry.Task {
	h.t.Helper()
	task := h.kindTask(key, "bug", "investigate")
	h.startStage(task)
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", RunID: h.lastRun("fx-repro"), Kind: runtime.EventNoProgress,
		Reason: "no progress: the same failure 3 times"})
	h.tick()
	got := h.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != loopWait {
		h.t.Fatalf("task %s waits %q, want needs_you: %s", got.State, got.WaitsWhat, loopWait)
	}
	return got
}

// The runtime's loop guard that finds a role making no progress (it
// compacted the session, then started a fresh one, and the role still loops)
// asks you a question with concrete options, as the writer's fix stop does:
// give a direction, start the role in a fresh session, or drop the task;
// never "retry". A prompt the guard refuses asks the same, once.
func TestALoopWithoutProgressAsksAQuestionNeverRetry(t *testing.T) {
	h := newHarness(t)
	task := h.loopNoProgress("PR-12550")
	qs := h.newestQuestions(task.ID)
	if len(qs) != 1 || qs[0].RaisedBy != factoryAsks || !slices.Equal(optionLabels(qs[0]), []string{optionDirection, optionFresh, optionDrop}) {
		t.Fatalf("questions %+v, want the factory's one with a direction, a fresh session and drop", qs)
	}
	for _, text := range []string{task.WaitsWhat, qs[0].Text, qs[0].Context} {
		if strings.Contains(strings.ToLower(text), "retry") {
			t.Fatalf("%q offers a retry", text)
		}
	}

	g := newHarness(t)
	refused := g.kindTask("PR-12551", "bug", "investigate")
	g.rt.setSendErr(fmt.Errorf("send to fx-repro: 6 prompts in the investigate stage: %w", runtime.ErrNoProgress))
	g.startStage(refused)
	got := g.reload(refused.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "makes no progress") ||
		!strings.HasSuffix(got.WaitsWhat, "give a direction, start the role in a fresh session or drop the task?") {
		t.Fatalf("task %s waits %q, want the loop's question", got.State, got.WaitsWhat)
	}
	if qs := g.newestQuestions(refused.ID); len(qs) != 1 {
		t.Fatalf("questions %+v, want one: each role's refused prompt asks once", qs)
	}
}

// Your answer starts the role in a fresh session, whose loop ladder starts
// over (Runtime.SendFresh), with your words when you gave a direction; a
// drop stops the task.
func TestYourAnswerToALoopsQuestionStartsTheRoleAfreshOrDropsTheTask(t *testing.T) {
	for _, tc := range []struct {
		name, answer, words string
	}{
		{"a fresh session", optionFresh, ""},
		{"a direction", optionDirection + ": read the seeds first", "read the seeds first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			task := h.loopNoProgress("PR-12552")
			before := len(h.rt.promptsTo("fx-repro"))
			h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: task.QuestionRev, Answers: map[int]string{0: tc.answer}})
			ps := h.rt.promptsTo("fx-repro")
			if len(ps) != before+1 || !ps[len(ps)-1].Fresh || !strings.HasPrefix(ps[len(ps)-1].Text, answerLead) ||
				!strings.Contains(ps[len(ps)-1].Text, tc.words) {
				t.Fatalf("prompts to fx-repro %+v; want one fresh session naming %q", ps[before:], tc.words)
			}
			if got := h.reload(task.ID); got.State != registry.TaskActive {
				t.Fatalf("task %s waits %q after your answer, want it at work", got.State, got.WaitsWhat)
			}
		})
	}
	h := newHarness(t)
	task := h.loopNoProgress("PR-12553")
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: task.QuestionRev, Answers: map[int]string{0: optionDrop}})
	if got := h.reload(task.ID); got.State != registry.TaskStopped {
		t.Fatalf("task %s after your drop, want it stopped", got.State)
	}
}
