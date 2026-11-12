package factory

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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

// Two roles whose recoveries run out in one pass both ask you: neither
// question is dropped, so no role sits lost with nothing to ask.
func TestEveryLostRoleOfAPassAsksItsQuestion(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12546", "bug", "investigate")
	h.startStage(task)
	for _, role := range []string{"fx-repro", "fx-cause"} {
		h.observeEvents(runtime.Event{TaskID: task.ID, Role: role, RunID: h.lastRun(role), Kind: runtime.EventLost,
			Reason: "no progress after 3 recoveries"})
	}
	h.tick()
	got := h.reload(task.ID)
	var asked []string
	for _, q := range h.newestQuestions(task.ID) {
		if q.State == registry.QuestionOpen {
			asked = append(asked, q.Text)
		}
	}
	if got.State != registry.TaskNeedsYou || len(asked) != 2 || !strings.Contains(strings.Join(asked, "\n"), "fx-cause") {
		t.Fatalf("task %s waits %q with questions %q; want both lost roles asked", got.State, got.WaitsWhat, asked)
	}
}

// After a restart, a stage rebuilt for your "start the role again" asks
// the runtime to recover nothing: your answer's fresh start takes the
// role, and no stale "lost" asks the same question again.
func TestARebuildForYourStartAnswerRecoversNothing(t *testing.T) {
	h := newHarness(t)
	task := h.lostAfterRecoveries("PR-12547")
	h.rt.abandonRun(h.lastRun("fx-repro"), runtime.RecoveryPrefix+"its recoveries ran out")
	h.restart()
	recovered := len(h.rt.recoveries())
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: task.QuestionRev, Answers: map[int]string{0: optionStart}})
	h.tick()
	if n := len(h.rt.recoveries()); n != recovered {
		t.Fatalf("%d recoveries asked of the runtime after your start answer, want none: the fresh start takes the role", n-recovered)
	}
}

// A role lost before its first prompt (its CLI exits at its start) starts
// on your answer as a fresh launch: its recoveries start over (StartOver)
// and the stage's first prompt goes to it, your answer's words after it,
// never a fresh session of a prompt it never had.
func TestYourAnswerForARoleLostBeforeItsFirstPromptStartsItWithItsFirstPrompt(t *testing.T) {
	for _, tc := range []struct{ name, answer, words string }{
		{"start it again", optionStart, ""},
		{"a direction", optionDirection + ": the seeds need the staging locale", "the seeds need the staging locale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			task := h.kindTask("PR-12548", "bug", "investigate")
			h.rt.mu.Lock()
			h.rt.sendErr = errors.New("agent: no session")
			h.rt.mu.Unlock()
			h.startStage(task)
			h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventLost, Reason: "no progress after 3 recoveries"})
			h.clock.Add(time.Minute) // past the failed sends' wait
			h.tick()
			got := h.reload(task.ID)
			if got.State != registry.TaskNeedsYou {
				t.Fatalf("task %s waits %q; want the lost question", got.State, got.WaitsWhat)
			}
			h.rt.mu.Lock()
			h.rt.sendErr = nil
			h.rt.mu.Unlock()
			h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: tc.answer}})
			h.clock.Add(time.Minute)
			h.tick()
			ps := h.rt.promptsTo("fx-repro")
			if len(ps) != 1 || ps[0].Fresh || !strings.Contains(ps[0].Text, answerLead) || !strings.Contains(ps[0].Text, tc.words) {
				t.Fatalf("prompts to fx-repro %+v; want its first prompt, your answer after it, no fresh session", ps)
			}
			if over := h.rt.startOvers(); !slices.Contains(over, TaskKey(task)+"/fx-repro") {
				t.Fatalf("start-overs %q; want fx-repro's", over)
			}
		})
	}
}

// Your drop that stops.json recorded but that did not apply (the task's
// end failed) is applied at the next pass, never lost.
func TestADropThatDidNotApplyIsAppliedAtTheNextPass(t *testing.T) {
	h := newHarness(t)
	task := h.lostAfterRecoveries("PR-12549")
	dir := h.layout.FactoryTaskDir(task.ID)
	rec, err := readStops(dir)
	if err != nil || len(rec.Asks) != 1 {
		t.Fatalf("stops.json %+v (%v); want the one question", rec.Asks, err)
	}
	rec.Asks[0].Decision, rec.Asks[0].Decided = decideDrop, h.clock.Now()
	if err := writeStops(dir, rec); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.TransitionTask(h.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) { u.Because("your answer") }); err != nil {
		t.Fatal(err) // your answer made it active; its end then failed
	}
	if ended, err := h.e.decideStops(h.ctx, h.reload(task.ID)); err != nil || !ended {
		t.Fatalf("decideStops = %v, %v; want the recorded drop applied", ended, err)
	}
	if got := h.reload(task.ID); isLive(got) || got.State == registry.TaskNeedsYou {
		t.Fatalf("task %s after the recorded drop; want it stopped", got.State)
	}
}

// A second answer for a role still lost before its first prompt is applied
// too: its recoveries start over again and its first prompt carries the
// newer answer, never the first one's.
func TestASecondAnswerForARoleStillLostBeforeItsFirstPromptIsApplied(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12550", "bug", "investigate")
	h.rt.mu.Lock()
	h.rt.sendErr = errors.New("agent: no session")
	h.rt.mu.Unlock()
	h.startStage(task)
	lose := func() registry.Task {
		h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventLost, Reason: "no progress after 3 recoveries"})
		h.clock.Add(time.Minute)
		h.tick()
		got := h.reload(task.ID)
		if got.State != registry.TaskNeedsYou {
			t.Fatalf("task %s waits %q; want the lost question", got.State, got.WaitsWhat)
		}
		return got
	}
	got := lose()
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: optionStart}})
	h.clock.Add(time.Minute)
	h.tick() // its first prompt still finds no session
	got = lose()
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: optionDirection + ": use the seeds"}})
	h.rt.mu.Lock()
	h.rt.sendErr = nil
	h.rt.mu.Unlock()
	h.clock.Add(time.Minute)
	h.tick()
	ps := h.rt.promptsTo("fx-repro")
	if over := h.rt.startOvers(); len(over) != 2 || len(ps) != 1 || !strings.Contains(ps[0].Text, "use the seeds") {
		t.Fatalf("start-overs %q, prompts %+v; want two start-overs and the first prompt with the second answer's words", over, ps)
	}
}

// A role's needs-you event in a pass that asks another role's stop question
// is kept, never dropped: once you answered, the task waits on you for it.
func TestAnotherRolesNeedsYouEventOutlivesAStopQuestion(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12551", "bug", "investigate")
	h.startStage(task)
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-cause", RunID: h.lastRun("fx-cause"), Kind: runtime.EventStuck,
		Reason: "the turn hung"},
		runtime.Event{TaskID: task.ID, Role: "fx-repro", RunID: h.lastRun("fx-repro"), Kind: runtime.EventLost,
			Reason: "no progress after 3 recoveries"})
	h.tick()
	got := h.reload(task.ID)
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: optionStart}})
	h.tick()
	if after := h.reload(task.ID); after.State != registry.TaskNeedsYou || !strings.Contains(after.WaitsWhat, "fx-cause agent is stuck") {
		t.Fatalf("task %s waits %q after your answer; want the stuck fx-cause told", after.State, after.WaitsWhat)
	}
}

// Two roles' needs-you events in one pass with no question are both told:
// the task waits on you for each, named in one line.
func TestTwoRolesNeedsYouEventsAreBothTold(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12552", "bug", "investigate")
	h.startStage(task)
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-cause", RunID: h.lastRun("fx-cause"), Kind: runtime.EventStuck,
		Reason: "the turn hung"},
		runtime.Event{TaskID: task.ID, Role: "fx-repro", RunID: h.lastRun("fx-repro"), Kind: runtime.EventBlocked,
			Reason: "dialog: Allow this?"})
	h.tick()
	if got := h.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "fx-cause agent is stuck") ||
		!strings.Contains(got.WaitsWhat, "fx-repro agent shows a prompt") {
		t.Fatalf("task %s waits %q; want both roles told", got.State, got.WaitsWhat)
	}
}
