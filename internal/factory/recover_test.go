package factory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

// startup runs the engine's startup, as a daemon does before its first
// tick.
func (h *harness) startup() {
	h.t.Helper()
	if err := h.e.startup(h.ctx); err != nil {
		h.t.Fatalf("startup: %v", err)
	}
}

// crash is a daemon that stopped without its drain, then started again: a
// fresh engine on the same registry and collaborators, and its startup.
// The old engine's work off the loop is left as a crash leaves it: nothing
// of it is read, and it ends with the test.
func (h *harness) crash() {
	h.t.Helper()
	h.t.Cleanup(h.e.stopBackground)
	h.e = New(h.deps)
	h.t.Cleanup(h.e.stopBackground)
	h.startup()
}

// retry sends your retry of task id as the screen does and runs the tick
// that answers it; the request as that tick left it.
func (h *harness) retry(id int64) registry.Request {
	h.t.Helper()
	qid, err := h.reg.EnqueueRequest(h.ctx, ReqRetry, RetryPayload{TaskID: id, Surface: "screen"})
	if err != nil {
		h.t.Fatal(err)
	}
	h.tick()
	q, err := h.reg.RequestByID(h.ctx, qid)
	if err != nil {
		h.t.Fatal(err)
	}
	return q
}

// entryOf is the task's newest ledger entry of role.
func (h *harness) entryOf(id int64, role string) registry.LedgerEntry {
	h.t.Helper()
	es, err := h.reg.LedgerOf(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, e := range slices.Backward(es) {
		if e.Role == role {
			return e
		}
	}
	h.t.Fatalf("no ledger entry of %s", role)
	return registry.LedgerEntry{}
}

// waitsOn fails the test unless task id is in state, waiting who: what.
func (h *harness) waitsOn(id int64, state, who, what string) {
	h.t.Helper()
	if got := h.reload(id); got.State != state || got.WaitsWho != who || got.WaitsWhat != what {
		h.t.Fatalf("task %s waits %q: %q; want %s, %s: %s", got.State, got.WaitsWho, got.WaitsWhat, state, who, what)
	}
}

// eventTexts are the texts of the task's events of kind.
func (h *harness) eventTexts(id int64, kind string) []string {
	h.t.Helper()
	var out []string
	for _, ev := range h.events(id, kind) {
		out = append(out, ev.Text)
	}
	return out
}

// The startup's recovery runs in one order, after the copies a base run
// left detached are restored: the servers a stopped daemon left, the
// sessions of every task whose agents may live (not a stopped or a queued
// one), the workspaces no row names, then the prompts of unknown delivery.
// What each step found is in the event log; a prompt still unknown leaves
// its task waiting on it.
func TestStartupRunsTheRecoveryStepsInOrder(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	active := h.task("PR-12401", registry.TaskActive, "investigate")
	h.task("PR-12402", registry.TaskNeedsYou, "investigate")
	h.task("PR-12403", registry.TaskStopped, "investigate")
	h.task("PR-12404", registry.TaskQueued, "")
	a, err := h.reg.BeginAttempt(h.ctx, active.ID, "investigate")
	if err != nil {
		t.Fatal(err)
	}
	e, err := h.reg.AppendLedger(h.ctx, registry.LedgerEntry{TaskID: active.ID, AttemptID: a.ID, Role: "fx-repro", RunID: "run-9",
		Hash: runtime.PromptHash("reproduce the bug"), Text: "reproduce the bug"})
	if err != nil {
		t.Fatal(err)
	}
	h.rt.changed = map[int64][]string{active.ID: {"fx-repro rebound"}}
	h.rt.orphans = []runtime.Orphan{
		{Workspace: "w3", Label: "factory PR-12401 " + a.ID, Attempt: a.ID, TaskID: active.ID, Adopted: true, Why: "its attempt's launch takes it over"},
		{Workspace: "w4", Label: "factory PR-12399 t99-investigate-1", Attempt: "t99-investigate-1", Why: "the registry has no attempt t99-investigate-1"},
	}
	h.copies.onReconcile = func() { h.rt.note("servers") }

	h.restart()
	h.startup()

	want := []string{"servers", "recover PR-12401", "recover PR-12402", "workspaces", fmt.Sprintf("reconcile %d", e.ID)}
	if got := h.rt.callList(); !slices.Equal(got, want) {
		t.Fatalf("startup ran %q, want %q", got, want)
	}
	if got := h.eventTexts(active.ID, "recover"); !slices.Equal(got, []string{"the agents after the restart: fx-repro rebound"}) {
		t.Fatalf("recover events %q", got)
	}
	if got := h.eventTexts(active.ID, "workspace"); !slices.Equal(got, []string{
		fmt.Sprintf("workspace %q", "factory PR-12401 "+a.ID) + ", which no row names (a launch the daemon stopped in), is adopted by its attempt's launch"}) {
		t.Fatalf("workspace events %q", got)
	}
	if got := h.eventTexts(0, "workspace"); !slices.Equal(got, []string{
		`closed workspace "factory PR-12399 t99-investigate-1", which no row names: the registry has no attempt t99-investigate-1`}) {
		t.Fatalf("the daemon's workspace events %q", got)
	}
	h.waitsOn(active.ID, registry.TaskActive, waitAgent, "prompt delivery unknown (fx-repro)")
	if got := h.eventTexts(active.ID, "prompt"); len(got) != 1 || !strings.Contains(got[0], "nothing is sent to fx-repro") {
		t.Fatalf("prompt events %q", got)
	}
}

// A herdr that does not answer at the startup holds the agents' recovery
// until it does: once its try is due, the first tick herdr answers runs
// it, once, and a tick herdr does not answer runs nothing.
func TestTheAgentsAreRecoveredOnceHerdrAnswers(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	h.task("PR-12405", registry.TaskActive, "investigate")
	down := errors.New("herdr: dial unix herdr.sock: connect: no such file or directory")
	h.rt.recoverErr = down

	h.startup()
	if got := h.rt.callList(); !slices.Equal(got, []string{"recover PR-12405", "workspaces"}) {
		t.Fatalf("startup ran %q", got)
	}
	if evs := h.events(0, "recover"); len(evs) != 1 || evs[0].Phase != registry.PhaseFail {
		t.Fatalf("recover events %+v, want the failure", evs)
	}
	h.clock.Add(time.Minute)
	h.e.d.HerdrUp = func(context.Context) error { return down }
	h.tick()
	if got := h.rt.callList(); len(got) != 2 {
		t.Fatalf("calls %q on a tick herdr did not answer", got)
	}
	h.rt.recoverErr, h.e.d.HerdrUp = nil, nil
	h.tick()
	h.tick()
	if got := h.rt.callList(); !slices.Equal(got, []string{"recover PR-12405", "workspaces", "recover PR-12405", "workspaces"}) {
		t.Fatalf("calls %q, want the recovery once more on the first tick herdr answers", got)
	}
}

// A prompt the daemon stopped in the middle of, which its transcript shows
// arrived, is settled delivered at the startup: its run is watched (its end
// is taken and verified), and the role gets nothing again.
func TestRestartAfterAPromptArrivedDoesNotSendItTwice(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12406", "bug", "investigate")
	h.rt.sendUnknown("fx-repro") // the daemon stops between the prompt and its verdict
	h.startStage(task)
	first := h.lastRun("fx-repro")

	h.rt.setVerdict(registry.DeliveredYes) // its transcript holds the prompt
	h.crash()
	h.startStage(task)
	if ps := h.rt.promptsTo("fx-repro"); len(ps) != 1 {
		t.Fatalf("fx-repro got %d prompts, want the one from before the restart", len(ps))
	}
	if got := h.eventTexts(task.ID, "prompt"); len(got) != 1 || !strings.Contains(got[0], "arrived before the restart") {
		t.Fatalf("prompt events %q", got)
	}
	for _, role := range []string{"fx-repro", "fx-cause", "fx-history"} {
		h.taskFile(task.ID, findingsFile(role), role+" found it in app/models/campaign.rb:88\n")
	}
	h.ended(task, "fx-repro", "fx-cause", "fx-history")
	h.tick()
	if got := h.reload(task.ID); got.Stage != "judge" {
		t.Fatalf("task %s at %s waits %q, want past the investigation", got.State, got.Stage, got.WaitsWhat)
	}
	if !slices.Contains(h.rt.verifies(), verifyCall{RunID: first, OK: true}) {
		t.Fatalf("Verify calls %+v, want the run from before the restart verified", h.rt.verifies())
	}
	if n := len(h.rt.promptsTo("fx-repro")); n != 1 {
		t.Fatalf("fx-repro got %d prompts, want 1", n)
	}
}

// A prompt nobody can prove either way at the startup is sent to nobody:
// the task waits on it. Once a later read proves it never arrived
// (EventUndelivered), the role gets it once, and a restart after that sends
// nothing more.
func TestRestartBeforeAPromptArrivedSendsItOnceAfterProof(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12407", "bug", "investigate")
	h.rt.sendUnknown("fx-repro")
	h.startStage(task)
	entry := h.entryOf(task.ID, "fx-repro")

	h.crash() // the startup's read finds no such turn yet: unknown
	h.startStage(task)
	h.tick()
	if n := len(h.rt.promptsTo("fx-repro")); n != 1 {
		t.Fatalf("fx-repro got %d prompts while its delivery is unknown, want 1", n)
	}
	h.waitsOn(task.ID, registry.TaskActive, waitAgent, "prompt delivery unknown (fx-repro)")

	// The observe pass's second read, RedeliverAfter later: no such turn.
	h.rt.setVerdict(registry.DeliveredNo)
	if v, err := h.rt.Reconcile(h.ctx, entry); err != nil || v != registry.DeliveredNo {
		t.Fatalf("Reconcile = %s, %v", v, err)
	}
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", RunID: entry.RunID, Kind: runtime.EventUndelivered,
		Reason: "the prompt to fx-repro never arrived (its transcript, read twice, has no such turn): it goes once more"})
	h.tick()
	ps := h.rt.promptsTo("fx-repro")
	if len(ps) != 2 || ps[1].Text != ps[0].Text {
		t.Fatalf("fx-repro's prompts %+v, want its first prompt once more", ps)
	}
	if got := h.reload(task.ID); got.WaitsWho == waitAgent {
		t.Fatalf("the task still waits %q: %q", got.WaitsWho, got.WaitsWhat)
	}
	h.crash()
	h.startStage(task)
	h.tick()
	if n := len(h.rt.promptsTo("fx-repro")); n != 2 {
		t.Fatalf("fx-repro got %d prompts after another restart, want still 2", n)
	}
}

// Review Focus 2: a prompt whose transcript cannot be read is never sent
// twice, through three restarts, your retry and its bound: the task waits on
// it, then needs you to look at the pane, and nothing goes to the role.
func TestAPromptOfUnknownDeliveryIsNeverSentTwice(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12408", "bug", "investigate")
	h.rt.sendUnknown("fx-repro")
	h.startStage(task)
	for i := range 3 {
		h.crash()
		h.startStage(task)
		h.tick()
		if n := len(h.rt.promptsTo("fx-repro")); n != 1 {
			t.Fatalf("restart %d: fx-repro got %d prompts, want 1", i+1, n)
		}
		h.waitsOn(task.ID, registry.TaskActive, waitAgent, "prompt delivery unknown (fx-repro)")
		h.clock.Add(5 * time.Minute)
	}
	if n := len(slices.DeleteFunc(h.rt.callList(), func(c string) bool { return !strings.HasPrefix(c, "reconcile") })); n != 3 {
		t.Fatalf("%d reads at the startups, want one at each", n)
	}
	h.clock.Add(time.Minute) // past runtime.UnknownAfter
	h.tick()
	why := runtime.UnknownReason(task.ID, "fx-repro")
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, why)
	if q := h.retry(task.ID); q.State != registry.RequestDone {
		t.Fatalf("retry %s: %q", q.State, q.Result)
	}
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, why)
	if n := len(h.rt.promptsTo("fx-repro")); n != 1 {
		t.Fatalf("fx-repro got %d prompts after your retry, want 1", n)
	}
}

// The wait on you for a prompt of unknown delivery names the command that
// lets it go, and that let-go (magnum factory let-go, from the CLI) takes
// the task off the wait, records your input from the CLI and sends the role
// nothing again: its turn is not watched, so the task waits on its file. A
// second let-go finds nothing left and is refused.
func TestALetGoFromTheCLIGoesOnAndResendsNothing(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12391", "bug", "investigate")
	h.rt.sendUnknown("fx-repro")
	h.startStage(task)
	h.clock.Add(runtime.UnknownAfter)
	h.tick()
	why := fmt.Sprintf("a prompt to fx-repro may or may not have arrived: look at its pane, then let it go with "+
		"`magnum factory let-go %d --role fx-repro`", task.ID)
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, why)

	letGo := func() registry.Request {
		t.Helper()
		qid, err := h.reg.EnqueueRequest(h.ctx, ReqLetGo, LetGoPayload{TaskID: task.ID, Role: "fx-repro", Surface: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		h.tick()
		q, err := h.reg.RequestByID(h.ctx, qid)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	if q := letGo(); q.State != registry.RequestDone {
		t.Fatalf("the let-go is %s: %q", q.State, q.Result)
	}
	got := h.reload(task.ID)
	if runtime.IsUnknownReason(got.WaitsWhat) || !strings.Contains(got.WaitsWhat, "the prompt to fx-repro was let go") {
		t.Fatalf("after the let-go the task is %s waiting %q, want the let-go's wait on the role's file", got.State, got.WaitsWhat)
	}
	ins, err := h.reg.Inputs(h.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(ins, func(in registry.Input) bool { return in.Surface == "cli" && in.Role == "fx-repro" }) {
		t.Fatalf("inputs %+v, want your let-go from the CLI", ins)
	}
	if q := letGo(); q.State != registry.RequestFailed || !strings.Contains(q.Result, "nothing to let go") {
		t.Fatalf("a second let-go is %s: %q, want refused: nothing is left", q.State, q.Result)
	}
	h.tick()
	if n := len(h.rt.promptsTo("fx-repro")); n != 1 {
		t.Fatalf("fx-repro got %d prompts, want the one you let go and no other", n)
	}
}

// Task 9's N6: the wait on you names every prompt of unknown delivery, one
// that comes while the task already needs you for another too; letting
// one go keeps the others' wait. A task that needs you for something else
// keeps that reason, and once you let it go on its stage names the unknown
// prompt itself: none is dropped.
func TestEveryUnknownDeliveryIsNamedWhileTheTaskNeedsYou(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12409", "bug", "investigate")
	h.rt.sendUnknown("fx-repro")
	h.startStage(task)
	a := h.attempts(task.ID)[0]
	h.clock.Add(10 * time.Minute)
	cause, err := h.reg.AppendLedger(h.ctx, registry.LedgerEntry{TaskID: task.ID, AttemptID: a.ID, Role: "fx-cause", RunID: "run-cause-2",
		Hash: runtime.PromptHash("go on"), Text: "go on"})
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Add(5 * time.Minute)
	unknown := func(role string) runtime.Event {
		return runtime.Event{TaskID: task.ID, Role: role, Kind: runtime.EventDeliveryUnknown, Reason: runtime.UnknownReason(task.ID, role)}
	}
	h.observeEvents(unknown("fx-repro"))
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, runtime.UnknownReason(task.ID, "fx-repro"))
	h.clock.Add(10 * time.Minute)
	h.observeEvents(unknown("fx-cause"))
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, runtime.UnknownReason(task.ID, "fx-repro", "fx-cause"))

	letGo := func(role string) {
		t.Helper()
		if _, err := h.reg.EnqueueRequest(h.ctx, ReqLetGo, LetGoPayload{TaskID: task.ID, Role: role, Surface: "screen"}); err != nil {
			t.Fatal(err)
		}
		h.tick()
	}
	letGo("fx-repro")
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, runtime.UnknownReason(task.ID, "fx-cause"))

	other := h.kindTask("PR-12410", "bug", "investigate")
	h.rt.sendUnknown("fx-cause")
	h.startStage(other)
	if err := h.reg.TransitionTask(h.ctx, other.ID, nil, registry.TaskNeedsYou, func(u *registry.TaskUpdate) {
		u.Set("waits_who", waitsYou)
		u.Set("waits_what", "the fx-history agent wrote no valid findings-fx-history.md")
	}); err != nil {
		t.Fatal(err)
	}
	h.clock.Add(runtime.UnknownAfter)
	h.observeEvents(runtime.Event{TaskID: other.ID, Role: "fx-cause", Kind: runtime.EventDeliveryUnknown, Reason: runtime.UnknownReason(other.ID, "fx-cause")})
	h.waitsOn(other.ID, registry.TaskNeedsYou, waitsYou, "the fx-history agent wrote no valid findings-fx-history.md")
	h.taskFile(other.ID, findingsFile("fx-history"), "a guard from 2024\n")
	if q := h.retry(other.ID); q.State != registry.RequestDone {
		t.Fatalf("retry %s: %q", q.State, q.Result)
	}
	h.waitsOn(other.ID, registry.TaskNeedsYou, waitsYou, runtime.UnknownReason(other.ID, "fx-cause"))
	_ = cause
}

// A run at work when the daemon stopped goes on being observed after the
// restart: the startup binds its task's sessions again, its role gets no
// prompt again, and its end is taken and verified.
func TestRestartDuringWorkKeepsObservingTheRun(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12411", "bug", "investigate")
	h.startStage(task)
	runs := map[string]string{}
	for _, role := range []string{"fx-repro", "fx-cause", "fx-history"} {
		runs[role] = h.lastRun(role)
	}

	h.crash()
	if got := h.rt.callList(); !slices.Contains(got, "recover PR-12411") {
		t.Fatalf("startup ran %q, want the task's sessions recovered", got)
	}
	h.startStage(task)
	if n := len(h.rt.allPrompts()); n != 3 {
		t.Fatalf("%d prompts after the restart, want the 3 from before it", n)
	}
	for _, role := range []string{"fx-repro", "fx-cause", "fx-history"} {
		h.taskFile(task.ID, findingsFile(role), role+" found it in app/models/campaign.rb:88\n")
	}
	h.ended(task, "fx-repro", "fx-cause", "fx-history")
	h.tick()
	if got := h.reload(task.ID); got.Stage != "judge" {
		t.Fatalf("task %s at %s waits %q, want past the investigation", got.State, got.Stage, got.WaitsWhat)
	}
	for role, run := range runs {
		if !slices.Contains(h.rt.verifies(), verifyCall{RunID: run, OK: true}) {
			t.Errorf("%s's run %s from before the restart was not verified: %+v", role, run, h.rt.verifies())
		}
	}
}

// A restart during the question batch loses no answer and repeats nothing:
// a restart while the stage waits for your answers asks nothing again; one
// between your answers and the resumed role's prompt sends that prompt
// once; one while the judge amends its report sends no second amend; and
// the batch handled again after a crash (its request still pending)
// stores and records nothing twice.
func TestRestartDuringTheQuestionBatchKeepsTheAnswers(t *testing.T) {
	h := newHarness(t)
	codingEndsOnFindings(h)
	task := h.kindTask("PR-12412", "feature", "coding")
	h.startStage(task)
	h.taskFile(task.ID, "questions-fx-writer.json", questionsJSON(t, fileQuestion("Reuse CreditMailer?", 0, "yes", "no")))
	h.ended(task, "fx-writer")
	h.tick()
	h.crash()
	h.startStage(task)
	if got := h.reload(task.ID); got.State != registry.TaskNeedsYou || got.QuestionRev != 1 {
		t.Fatalf("after a restart the task is %s at revision %d, want needs_you at 1", got.State, got.QuestionRev)
	}
	if evs := h.events(task.ID, "alert"); len(evs) != 1 {
		t.Fatalf("alerts %+v, want the batch asked once", evs)
	}

	h.rt.setSendErr(&runtime.WaitError{Who: "usage", What: "claude limit until 16:00"}) // the answers prompt waits
	answers := AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "yes"}}
	h.mustAnswer(answers)
	h.crash()
	h.rt.setSendErr(nil)
	h.startStage(task)
	h.crash()
	h.startStage(task)
	ps := h.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, answersLead) {
		t.Fatalf("the writer's prompts %+v, want the answers once after the restarts", ps)
	}
	// The same batch again: the daemon took it and stopped before the
	// request ended.
	h.mustAnswer(answers)
	if ins := h.answerInputs(task.ID); len(ins) != 1 {
		t.Fatalf("answer inputs %+v after the batch came again, want the one", ins)
	}
	if qs := h.newestQuestions(task.ID); len(qs) != 1 || qs[0].State != registry.QuestionAnswered || qs[0].Answer != "yes" {
		t.Fatalf("questions %+v, want the one answered yes", qs)
	}

	amend := newHarness(t)
	gated := amend.gateTask("PR-12413", "feature", whenQuestion)
	amend.tick()
	amend.clock.Add(time.Minute)
	amend.mustAnswer(AnswerPayload{TaskID: gated.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[1].Label}})
	amend.e.waitBackground()
	amend.tick()
	if n := len(amend.rt.promptsTo("fx-judge")); n != 1 {
		t.Fatalf("the judge got %d prompts, want its amend", n)
	}
	amend.crash()
	amend.startStage(gated)
	if n := len(amend.rt.promptsTo("fx-judge")); n != 1 {
		t.Fatalf("the judge got %d prompts after a restart, want its one amend", n)
	}
}

// A local round the daemon stopped in (a crash: nothing ends it) runs
// again after the restart in the same cycle, as the same round of the loop:
// its synthetic PR number is the same, so the round's start closes the
// workspace the cut one left by its label (closeEvalLeftovers). The
// startup says so; the cycle records its minutes.
func TestRestartDuringALocalReviewRoundRunsTheRoundAgain(t *testing.T) {
	r := newReviewRig(t, result())
	r.rv.block = make(chan struct{})
	task, _ := r.reviewTask("PR-12414")
	r.startBlockedRound()
	r.rv.mu.Lock()
	r.rv.block = nil // the round after the restart runs to its end
	r.rv.mu.Unlock()

	r.clock.Add(20 * time.Minute)
	r.crash()
	r.e.lockPoll = 5 * time.Millisecond
	number := evalrun.LocalNumber(task.ID, 1)
	if got := r.eventTexts(task.ID, "review"); !slices.ContainsFunc(got, func(s string) bool {
		return strings.Contains(s, "cut by the restart") && strings.Contains(s, fmt.Sprintf("#%d", number))
	}) {
		t.Fatalf("review events %q, want the cut round named with its synthetic number", got)
	}
	r.clock.Add(20 * time.Minute)
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" })
	calls := r.rv.calls()
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(calls) != 2 {
		t.Fatalf("task at %s after %d rounds, want pr_body after the round ran again", got.Stage, len(calls))
	}
	if calls[1].Round != calls[0].Round || calls[1].TaskID != calls[0].TaskID ||
		evalrun.LocalNumber(calls[1].TaskID, calls[1].Round) != number {
		t.Fatalf("the round ran again as round %d of task %d, want round %d of task %d: one label", calls[1].Round, calls[1].TaskID,
			calls[0].Round, calls[0].TaskID)
	}
	cs := r.cycles(task.ID)
	if len(cs) != 1 || cs[0].Exit != registry.ExitClean || cs[0].Ended.Sub(cs[0].Started) < 40*time.Minute {
		t.Fatalf("cycles %+v, want the one cycle, clean, its minutes from its start to its end", cs)
	}
}

// The review stage's fix turn is rebuilt after a restart from the ledger
// (agentsRunOf): the writer gets no second fix prompt, and its end goes on
// with the loop.
func TestRestartDuringTheReviewsFixTurnKeepsTheWritersPrompt(t *testing.T) {
	r := newReviewRig(t, result(refundFinding), result())
	task, _ := r.reviewTask("PR-12415")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	r.crash()
	r.e.lockPoll = 5 * time.Millisecond
	r.run(3, nil)
	if n := len(r.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts after the restart, want its one fix turn", n)
	}
	r.writerFixes(task, 1, fixedCredit(1), nil)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("task at %s after %d rounds, want pr_body after the fix's round", got.Stage, len(r.rv.calls()))
	}
}

// publishCut leaves the state a daemon stopped inside task's publish
// leaves: your push decided (the request taken, no tick after it), the
// publish's effect begun and the approved head pushed to the card's branch.
func (r *readyRig) publishCut(key string) (registry.Task, string, string) {
	r.t.Helper()
	task, head := r.toCard(key)
	c := r.card(task.ID)
	qid, err := r.reg.EnqueueRequest(r.ctx, ReqDecide, DecidePayload{TaskID: task.ID, Choice: ChoicePush, HeadSHA: c.Head,
		ReportHash: c.ReportHash, DiffHash: c.DiffHash, Shown: c.Drawn, Surface: "screen"})
	if err != nil {
		r.t.Fatal(err)
	}
	r.e.handleRequests(r.ctx)
	if q, err := r.reg.RequestByID(r.ctx, qid); err != nil || q.State != registry.RequestDone {
		r.t.Fatalf("push request %+v (%v)", q, err)
	}
	if _, _, err := r.reg.BeginEffect(r.ctx, publishKey(task.ID, head), "publish", task.ID); err != nil {
		r.t.Fatal(err)
	}
	branch := cmp.Or(c.Remote, task.Branch)
	r.git(r.copyDir, "push", "--quiet", "origin", head+":refs/heads/"+branch)
	return task, head, branch
}

// countPushes counts the git pushes from now on.
func (r *readyRig) countPushes() *int {
	n := new(int)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failGit = func(args []string) error {
		if slices.Contains(args, "push") {
			*n++
		}
		return nil
	}
	return n
}

// A daemon stopped between the push and the PR finds the pushed head
// after the restart: the startup names the publish in flight, its stage
// runs its guard again (the remote branch, the PRs of the branch), pushes
// nothing and opens the PR once.
func TestRestartBetweenThePushAndThePRFindsThePushedHead(t *testing.T) {
	r := newReadyRig(t)
	task, head, branch := r.publishCut("PR-12416")
	r.crash()
	pushes := r.countPushes()
	if got := r.eventTexts(task.ID, "effect"); len(got) != 1 || !strings.Contains(got[0], publishKey(task.ID, head)) {
		t.Fatalf("effect events %q, want the publish in flight named", got)
	}
	got := r.published(task.ID)
	eff, err := r.reg.EffectByKey(r.ctx, publishKey(task.ID, head))
	if got.State != registry.TaskPublished || *pushes != 0 || len(r.gh.createCalls()) != 1 || err != nil || eff.State != registry.EffectOK ||
		!eff.Resumed || r.remoteHead(branch) != head {
		t.Fatalf("task %s waits %q; %d pushes, %d PRs opened; effect %+v (%v); remote %q", got.State, got.WaitsWhat, *pushes,
			len(r.gh.createCalls()), eff, err, r.remoteHead(branch))
	}
}

// A daemon stopped after the PR opened, before its effect ended, finds the
// PR after the restart: nothing is pushed or opened again, and that PR is
// the task's.
func TestRestartDuringPublishFindsTheExistingPR(t *testing.T) {
	r := newReadyRig(t)
	task, head, branch := r.publishCut("PR-12417")
	r.gh.addPR(branch, PRView{Number: 4590, URL: "https://github.com/example/shop/pull/4590", Author: "zhuravel", Base: "master", HeadSHA: head})
	r.crash()
	pushes := r.countPushes()
	got := r.published(task.ID)
	if got.State != registry.TaskPublished || got.PRNumber != 4590 || *pushes != 0 || len(r.gh.createCalls()) != 0 {
		t.Fatalf("task %s waits %q, PR %d; %d pushes, %d PRs opened", got.State, got.WaitsWhat, got.PRNumber, *pushes, len(r.gh.createCalls()))
	}
}

// The event log survives a restart as it was: every event before it keeps
// its row, and the startup's own come after them.
func TestRestartKeepsTheEventLog(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12418", "bug", "investigate")
	h.rt.sendUnknown("fx-repro")
	h.startStage(task)
	h.e.event(h.ctx, 0, "factory", registry.PhaseInfo, "a daemon's own event")
	before := map[int64][]registry.Event{}
	for _, id := range []int64{0, task.ID} {
		evs, err := h.reg.Events(h.ctx, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 0 {
			t.Fatalf("task %d has no events to keep", id)
		}
		before[id] = evs
	}

	h.crash()
	for id, old := range before {
		evs, err := h.reg.Events(h.ctx, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) <= len(old) && id == task.ID {
			t.Fatalf("task %d has %d events after the restart, want the startup's after the %d before", id, len(evs), len(old))
		}
		if kept := evs[:len(old)]; !slices.Equal(kept, old) { // oldest first
			t.Fatalf("task %d's events before the restart %+v, after it %+v", id, old, kept)
		}
		for _, ev := range evs[len(old):] {
			if ev.ID <= old[len(old)-1].ID {
				t.Fatalf("the startup's event %+v comes before the last one of the old daemon", ev)
			}
		}
	}
	if got := h.eventTexts(task.ID, "claim"); slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "a crash") }) {
		t.Fatalf("claim events %q: a claim that ended is no crash's", got)
	}
}

// Your retry returns a task that needs you to work at its stage: your input
// is recorded, and the stage's own checks run again (a role whose file you
// fixed passes).
func TestRetryReturnsATaskThatNeedsYouToItsStage(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12419", "bug", "investigate")
	h.startStage(task)
	h.taskFile(task.ID, findingsFile("fx-repro"), "it fails\n")
	h.taskFile(task.ID, findingsFile("fx-history"), "a guard from 2024\n")
	h.ended(task, "fx-repro", "fx-history", "fx-cause")
	h.tick() // fx-cause is nudged
	h.ended(task, "fx-cause")
	h.tick()
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, "the fx-cause agent wrote no valid findings-fx-cause.md")

	h.taskFile(task.ID, findingsFile("fx-cause"), "the cause is app/models/campaign.rb:88\n")
	q := h.retry(task.ID)
	if q.State != registry.RequestDone || q.Result != "PR-12419 goes on at investigate" {
		t.Fatalf("retry %s: %q", q.State, q.Result)
	}
	if got := h.reload(task.ID); got.Stage != "judge" || got.State != registry.TaskActive {
		t.Fatalf("task %s at %s waits %q, want past the investigation on your fixed file", got.State, got.Stage, got.WaitsWhat)
	}
	ins, err := h.reg.Inputs(h.ctx, task.ID)
	if err != nil || !slices.ContainsFunc(ins, func(in registry.Input) bool {
		return in.Kind == registry.InputRetry && in.Surface == "screen" && in.Stage == "investigate" && in.Role == ""
	}) {
		t.Fatalf("inputs %+v (%v), want your retry", ins, err)
	}
	if n := len(h.rt.promptsTo("fx-cause")); n != 2 {
		t.Fatalf("fx-cause got %d prompts, want no prompt from your retry", n)
	}
}

// A retry of a task that does not wait on you is refused in plain words,
// and changes nothing.
func TestRetryRefusesATaskThatDoesNotNeedYou(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	for key, state := range map[string]string{"PR-12420": registry.TaskActive, "PR-12421": registry.TaskStopped} {
		task := h.task(key, state, "investigate")
		q := h.retry(task.ID)
		want := fmt.Sprintf("%s is %s, not waiting on you: there is nothing to retry", TaskKey(task), state)
		if q.State != registry.RequestFailed || q.Result != want {
			t.Fatalf("retry of a %s task: %s %q, want failed %q", state, q.State, q.Result, want)
		}
		if got := h.reload(task.ID); got.State != state {
			t.Fatalf("the %s task is %s after the refused retry", state, got.State)
		}
	}
}

// A retry never lifts Codex-never: the task's Codex roles still run as their
// Claude fallback.
func TestRetryNeverLiftsCodexNever(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	task := h.task("PR-12422", registry.TaskNeedsYou, "investigate")
	if err := h.reg.TransitionTask(h.ctx, task.ID, nil, "", func(u *registry.TaskUpdate) {
		u.Set("codex_never", true)
		u.Set("waits_who", waitsYou)
		u.Set("waits_what", "the fx-cause agent was refused: codex: flagged")
	}); err != nil {
		t.Fatal(err)
	}
	if q := h.retry(task.ID); q.State != registry.RequestDone {
		t.Fatalf("retry %s: %q", q.State, q.Result)
	}
	got := h.reload(task.ID)
	cause, _ := h.cfg.FactoryRole("fx-cause")
	if runs, err := h.rt.RunsAs(got, cause); !got.CodexNever || err != nil || runs.Name != "fx-cause-claude" {
		t.Fatalf("after the retry Codex-never is %v and fx-cause runs as %q (%v)", got.CodexNever, runs.Name, err)
	}
}

// A prompt you let go is never sent again, and the stage does not stall on
// it: its role's turn is not watched, so the task needs you with a plain
// reason; once its file holds, your retry lets it pass. A restart keeps it.
func TestALetGoPromptWaitsOnYouInsteadOfStalling(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12423", "bug", "investigate")
	h.rt.sendUnknown("fx-repro")
	h.startStage(task)
	h.clock.Add(runtime.UnknownAfter)
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventDeliveryUnknown, Reason: runtime.UnknownReason(task.ID, "fx-repro")})
	if _, err := h.reg.EnqueueRequest(h.ctx, ReqLetGo, LetGoPayload{TaskID: task.ID, Role: "fx-repro", Surface: "screen"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	why := "the prompt to fx-repro was let go, and its turn is not watched: once findings-fx-repro.md is written (tell its agent in its pane), " +
		"retry the task; or stop it"
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, why)

	h.crash()
	h.retry(task.ID)
	h.e.waitBackground() // the fresh daemon claims the task's copy first
	h.tick()
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, why)

	for _, role := range []string{"fx-repro", "fx-cause", "fx-history"} {
		h.taskFile(task.ID, findingsFile(role), role+" found it in app/models/campaign.rb:88\n")
	}
	h.ended(task, "fx-cause", "fx-history")
	h.retry(task.ID)
	if got := h.reload(task.ID); got.Stage != "judge" {
		t.Fatalf("task %s at %s waits %q, want past the investigation", got.State, got.Stage, got.WaitsWhat)
	}
	if n := len(h.rt.promptsTo("fx-repro")); n != 1 {
		t.Fatalf("fx-repro got %d prompts, want the one you let go and no other", n)
	}
}

// A stop or a publish the daemon stopped in the middle of is finished at
// the startup: the task's agents are parked, a stopped task's open attempts
// end, and the copy it holds is released (a release refused while its
// agents lived would be refused for good).
func TestAStopOrAPublishCutByACrashIsFinishedAtStartup(t *testing.T) {
	h := newHarness(t)
	stopped := h.task("PR-12424", registry.TaskStopped, "investigate")
	published := h.task("PR-12425", registry.TaskPublished, "publish")
	needs := h.task("PR-12426", registry.TaskNeedsYou, "investigate")
	a, err := h.reg.BeginAttempt(h.ctx, stopped.ID, "investigate")
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range []registry.Task{stopped, published, needs} {
		h.copies.reserve(task.ID, fmt.Sprintf("factory%d", i+1))
	}

	h.restart()
	h.startup()
	if got := h.rt.parked(); !slices.Equal(got, []int64{stopped.ID, published.ID}) {
		t.Fatalf("parked %v, want the stopped and the published tasks' agents", got)
	}
	if got := h.copies.releases(); !slices.Equal(got, []int64{stopped.ID, published.ID}) {
		t.Fatalf("released %v at the startup, want the stopped and the published tasks' copies", got)
	}
	if got, err := h.reg.AttemptByID(h.ctx, a.ID); err != nil || got.State != registry.AttemptFailed {
		t.Fatalf("the stopped task's attempt %+v (%v), want failed", got, err)
	}
}

// A claim a crash cut (it ran off the loop, its setup unfinished) is never
// left half done: its begin is ended at the startup, and the task's next
// claim runs the claim's steps again and hands the task its copy.
func TestAClaimCutByACrashRunsAgainAfterTheRestart(t *testing.T) {
	h := newHarness(t)
	h.copies.block = make(chan struct{})
	h.copies.claimed = Copy{Name: "factory1", Path: t.TempDir(), Number: 1}
	task := h.task("PR-12427", registry.TaskActive, "investigate")
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, err := h.e.claimCopy(ctx, t)
		return false, err
	})
	h.tick()
	waitFor(t, "the claim to start", func() bool { return h.copies.claimCount() == 1 })
	h.copies.mu.Lock()
	h.copies.block = nil // the claim after the restart runs to its end
	h.copies.mu.Unlock()

	h.crash()
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, err := h.e.claimCopy(ctx, t)
		return false, err
	})
	h.wantClaimsClosed(task.ID)
	h.tick()
	h.e.waitBackground()
	h.tick()
	if got := h.reload(task.ID); got.Copy != "factory1" || h.copies.claimCount() != 2 {
		t.Fatalf("task holds %q after %d claims, want factory1 from the claim run again", got.Copy, h.copies.claimCount())
	}
	h.wantClaimsClosed(task.ID)
}

// undeliveredEvent is the observe pass's word that entry e's prompt never
// arrived, its second read having proven it (the runtime settles it first).
func (h *harness) undeliveredEvent(e registry.LedgerEntry) {
	h.t.Helper()
	h.rt.setVerdict(registry.DeliveredNo)
	if v, err := h.rt.Reconcile(h.ctx, e); err != nil || v != registry.DeliveredNo {
		h.t.Fatalf("Reconcile = %s, %v", v, err)
	}
	h.rt.setVerdict(registry.DeliveredUnknown)
	h.observeEvents(runtime.Event{TaskID: e.TaskID, Role: e.Role, RunID: e.RunID, Kind: runtime.EventUndelivered,
		Reason: "the prompt to " + e.Role + " never arrived (its transcript, read twice, has no such turn): it goes once more"})
}

// Task 9's N5 end to end: a prompt of unknown delivery older than
// runtime.UnknownAfter whose proof the startup's read began (its transcript
// is written past it) holds its task, which never needs you meanwhile; the
// proof's "no" lets the role have its prompt once more, and once only.
func TestARestartMidProofHoldsTheTaskAndNeverCallsYou(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12430", "bug", "investigate")
	h.rt.sendUnknown("fx-repro")
	h.startStage(task)
	entry := h.entryOf(task.ID, "fx-repro")
	h.rt.canProve(entry.ID)
	h.clock.Add(runtime.UnknownAfter + time.Minute)

	h.crash() // the startup's read begins the proof
	h.startStage(task)
	h.tick()
	h.waitsOn(task.ID, registry.TaskActive, waitAgent, "prompt delivery unknown (fx-repro)")
	if evs := h.events(task.ID, "alert"); len(evs) != 0 {
		t.Fatalf("alerts %+v while the proof runs, want none", evs)
	}

	h.undeliveredEvent(entry)
	h.tick()
	ps := h.rt.promptsTo("fx-repro")
	if len(ps) != 2 || ps[1].Text != ps[0].Text {
		t.Fatalf("fx-repro's prompts %+v, want its first prompt once more", ps)
	}
	h.tick()
	h.crash()
	h.startStage(task)
	if n := len(h.rt.promptsTo("fx-repro")); n != 2 {
		t.Fatalf("fx-repro got %d prompts, want 2", n)
	}
	if got := h.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho == waitAgent {
		t.Fatalf("task %s waits %q: %q, want at work", got.State, got.WaitsWho, got.WaitsWhat)
	}
}

// A task sent to you for a prompt nobody could tell yet comes back once a
// proof settles it: the wait names the prompts still unknown, and with none
// left the task goes back to work and each role gets its prompt once more.
func TestAProofThatAPromptNeverArrivedBringsTheTaskBackFromYou(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12431", "bug", "investigate")
	h.rt.sendUnknown("fx-repro")
	h.rt.sendUnknown("fx-cause")
	h.startStage(task)
	repro, cause := h.entryOf(task.ID, "fx-repro"), h.entryOf(task.ID, "fx-cause")
	h.clock.Add(runtime.UnknownAfter)
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventDeliveryUnknown, Reason: runtime.UnknownReason(task.ID, "fx-repro")})
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, runtime.UnknownReason(task.ID, "fx-repro", "fx-cause"))

	h.undeliveredEvent(repro)
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, runtime.UnknownReason(task.ID, "fx-cause"))
	h.undeliveredEvent(cause)
	if got := h.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho == waitsYou {
		t.Fatalf("task %s waits %q: %q after both proofs, want at work", got.State, got.WaitsWho, got.WaitsWhat)
	}
	h.tick()
	h.tick()
	for _, role := range []string{"fx-repro", "fx-cause"} {
		if ps := h.rt.promptsTo(role); len(ps) != 2 || ps[1].Text != ps[0].Text {
			t.Errorf("%s's prompts %+v, want its first prompt once more", role, ps)
		}
	}
	if n := len(h.rt.promptsTo("fx-history")); n != 1 {
		t.Fatalf("fx-history got %d prompts, want 1", n)
	}
}

// deliveredEvent is the observe pass's word that entry e's prompt arrived,
// a later read having found it in its transcript (the runtime settles it
// first: the entry delivered, its run watched).
func (h *harness) deliveredEvent(e registry.LedgerEntry) {
	h.t.Helper()
	h.rt.setVerdict(registry.DeliveredYes)
	if v, err := h.rt.Reconcile(h.ctx, e); err != nil || v != registry.DeliveredYes {
		h.t.Fatalf("Reconcile = %s, %v", v, err)
	}
	h.rt.setVerdict(registry.DeliveredUnknown)
	h.observeEvents(runtime.Event{TaskID: e.TaskID, Role: e.Role, RunID: e.RunID, Kind: runtime.EventDelivered,
		Reason: "the prompt to " + e.Role + " arrived (its transcript holds it): its run is watched, never sent again"})
}

// A task sent to you for a prompt nobody could tell yet comes back once a
// later read finds the prompt in its transcript: the wait keeps only the
// prompts still unknown, and with none left the task goes back to work,
// the found runs watched and nothing sent again; their ends pass the stage.
func TestALaterReadThatFindsThePromptBringsTheTaskBackFromYou(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12437", "bug", "investigate")
	h.rt.sendUnknown("fx-repro")
	h.rt.sendUnknown("fx-cause")
	h.startStage(task)
	repro, cause := h.entryOf(task.ID, "fx-repro"), h.entryOf(task.ID, "fx-cause")
	h.clock.Add(runtime.UnknownAfter)
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventDeliveryUnknown, Reason: runtime.UnknownReason(task.ID, "fx-repro")})
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, runtime.UnknownReason(task.ID, "fx-repro", "fx-cause"))

	h.deliveredEvent(repro)
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, runtime.UnknownReason(task.ID, "fx-cause"))
	h.deliveredEvent(cause)
	if got := h.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho == waitsYou {
		t.Fatalf("task %s waits %q: %q after both reads, want at work", got.State, got.WaitsWho, got.WaitsWhat)
	}
	h.tick()
	h.tick()
	for _, e := range []registry.LedgerEntry{repro, cause} {
		if state, _, err := h.rt.RunState(h.ctx, e.RunID); err != nil || state != store.RunWorking {
			t.Errorf("%s's run is %s (%v), want watched", e.Role, state, err)
		}
	}
	if n := len(h.rt.allPrompts()); n != 3 {
		t.Fatalf("%d prompts, want the 3 from before: nothing sent again", n)
	}
	for _, role := range []string{"fx-repro", "fx-cause", "fx-history"} {
		h.taskFile(task.ID, findingsFile(role), role+" found it in app/models/campaign.rb:88\n")
	}
	h.ended(task, "fx-repro", "fx-cause", "fx-history")
	h.tick()
	if got := h.reload(task.ID); got.Stage != "judge" {
		t.Fatalf("task %s at %s waits %q, want past the investigation on the found runs", got.State, got.Stage, got.WaitsWhat)
	}
}

// The agents' recovery that fails for a reason of the task's own (its
// store, not herdr) is not run again on every tick: it backs off, runs
// again only for the tasks it failed for, and tells each cause once.
func TestTheAgentsRecoveryBacksOffAndRetriesOnlyWhatFailed(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	failing := h.task("PR-12432", registry.TaskActive, "investigate")
	h.task("PR-12433", registry.TaskActive, "investigate")
	h.rt.setFailRecover(failing.ID, errors.New("agents: recover pr 1: database is locked"))

	h.startup()
	want := []string{"recover PR-12432", "recover PR-12433", "workspaces"}
	if got := h.rt.callList(); !slices.Equal(got, want) {
		t.Fatalf("startup ran %q, want %q", got, want)
	}
	h.tick()
	h.tick()
	if got := h.rt.callList(); !slices.Equal(got, want) {
		t.Fatalf("calls %q: the recovery ran again before its backoff", got)
	}
	h.clock.Add(time.Minute)
	h.tick()
	want = append(want, "recover PR-12432")
	if got := h.rt.callList(); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q: only the task that failed", got, want)
	}
	if evs := h.events(0, "recover"); len(evs) != 1 || evs[0].Phase != registry.PhaseFail {
		t.Fatalf("recover events %+v, want the cause told once", evs)
	}
	h.rt.setFailRecover(failing.ID, nil)
	h.clock.Add(2 * time.Minute)
	h.tick()
	h.clock.Add(time.Hour)
	h.tick()
	want = append(want, "recover PR-12432")
	if got := h.rt.callList(); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q: no more once it went through", got, want)
	}
}

// A retry of a task that waits for your answers is refused, naming how to
// answer: the stage would only ask the same batch again.
func TestRetryOfATaskThatWaitsForYourAnswersIsRefused(t *testing.T) {
	h := newHarness(t)
	task := h.gateTask("PR-12434", "feature", whenQuestion)
	h.tick()
	before := h.reload(task.ID)
	if before.State != registry.TaskNeedsYou {
		t.Fatalf("task %s waits %q, want your answers", before.State, before.WaitsWhat)
	}
	q := h.retry(task.ID)
	if q.State != registry.RequestFailed || !strings.Contains(q.Result, "magnum factory answer") {
		t.Fatalf("retry %s: %q, want refused naming magnum factory answer", q.State, q.Result)
	}
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, before.WaitsWhat)
	if evs := h.events(task.ID, "alert"); len(evs) != 1 {
		t.Fatalf("alerts %+v, want the batch's one", evs)
	}
}

// A prompt you let go that started a later turn of a role (here the one
// that hands it your answers) is never passed on a file its earlier turn
// wrote: only a file written after that prompt holds.
func TestALetGoAnswersPromptNeedsTheRolesNewFile(t *testing.T) {
	h := newHarness(t)
	codingEndsOnFindings(h)
	task := h.kindTask("PR-12435", "feature", "coding")
	h.startStage(task)
	h.taskFile(task.ID, findingsFile("fx-writer"), "committed abc1234\n")
	h.taskFile(task.ID, "questions-fx-writer.json", questionsJSON(t, fileQuestion("Reuse CreditMailer?", 0, "yes", "no")))
	h.ended(task, "fx-writer")
	h.tick()
	h.clock.Add(time.Minute)
	h.rt.sendUnknown("fx-writer")
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "yes"}})
	if ps := h.rt.promptsTo("fx-writer"); len(ps) != 2 || !strings.HasPrefix(ps[1].Text, answersLead) {
		t.Fatalf("the writer's prompts %+v, want its answers", ps)
	}
	h.clock.Add(runtime.UnknownAfter)
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-writer", Kind: runtime.EventDeliveryUnknown, Reason: runtime.UnknownReason(task.ID, "fx-writer")})
	if _, err := h.reg.EnqueueRequest(h.ctx, ReqLetGo, LetGoPayload{TaskID: task.ID, Role: "fx-writer", Surface: "screen"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, "the prompt to fx-writer was let go, and its turn is not watched: once "+
		"findings-fx-writer.md is written (tell its agent in its pane), retry the task; or stop it")

	h.taskFile(task.ID, findingsFile("fx-writer"), "committed def5678 with your answer\n")
	h.retry(task.ID)
	if got := h.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task %s at %s waits %q, want past coding on the file written after your answers", got.State, got.Stage, got.WaitsWhat)
	}
}

// Your retry of a task at its publish runs the publish's own checks again
// before anything is pushed: a branch that moved since the card needs you
// again and nothing leaves the Mac; once it is back, the retry publishes.
func TestARetryAtThePublishRunsItsGuardsAgain(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12436")
	r.gh.mu.Lock()
	r.gh.active = "alice"
	r.gh.mu.Unlock()
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	if got := r.published(task.ID); got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "gh acts as alice") {
		t.Fatalf("task %s waits %q, want you for the gh account", got.State, got.WaitsWhat)
	}
	r.gh.mu.Lock()
	r.gh.active = ""
	r.gh.mu.Unlock()
	moved := r.commit("PR-12436 a commit after the card", map[string]string{"app/notes.rb": "# moved\n"})

	r.retry(task.ID)
	r.run(3, nil)
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "the branch changed after you read it") ||
		!strings.Contains(got.WaitsWhat, short(moved)) || r.remoteHead(task.Branch) != "" || len(r.gh.createCalls()) != 0 {
		t.Fatalf("task %s waits %q; remote %q, %d PRs: want nothing pushed on a moved branch", got.State, got.WaitsWhat,
			r.remoteHead(task.Branch), len(r.gh.createCalls()))
	}
	r.git(r.copyDir, "reset", "--quiet", "--keep", head)
	r.retry(task.ID)
	if got := r.published(task.ID); got.State != registry.TaskPublished || r.remoteHead(task.Branch) != head {
		t.Fatalf("task %s waits %q, remote %q: want published at %s", got.State, got.WaitsWhat, r.remoteHead(task.Branch), short(head))
	}
}

// commitAt commits files on the copy's branch as the writer would, dated at
// (the committer's and the author's date: the fake clock's), and returns
// the head.
func (r *codingRig) commitAt(at time.Time, msg string, files map[string]string) string {
	r.t.Helper()
	for rel, text := range files {
		r.write(r.copyDir, rel, text)
		r.git(r.copyDir, "add", "--", rel)
	}
	date := at.UTC().Format(time.RFC3339)
	if _, err := r.real.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"-C", r.copyDir, "commit", "--quiet", "-m", msg},
		Env: map[string]string{"GIT_COMMITTER_DATE": date, "GIT_AUTHOR_DATE": date}}); err != nil {
		r.t.Fatal(err)
	}
	return r.git(r.copyDir, "rev-parse", "HEAD")
}

// Check A at the coding stage's own end (the factory's proof and precheck
// at the head): a writer that committed, wrote its proof.json and asked
// questions was suspended before its head was ever judged. When you let go
// of the prompt that hands it your answers, the stage does not pass on
// that head and that proof.json: the task waits on you until the writer
// commits after that prompt, and then the factory's checks judge the new
// head.
func TestALetGoAnswersTurnAtTheCodingEndNeedsAHeadAfterIt(t *testing.T) {
	r := newCodingRig(t)
	h := r.harness
	task := r.codingTask("PR-12440", "feature")
	h.startStage(task)
	r.commitAt(h.clock.Now(), "PR-12440 Credit#points counts the activities", map[string]string{
		"app/credit.rb":                     strings.Replace(creditRB, "    0\n", "    10\n", 1),
		"spec/models/credit_points_spec.rb": "expect: 10 in app/credit.rb\n",
	})
	h.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	h.taskFile(task.ID, "questions-fx-writer.json", questionsJSON(t, fileQuestion("Reuse CreditMailer?", 0, "yes", "no")))
	h.ended(task, "fx-writer")
	h.tick()
	if got := h.reload(task.ID); got.State != registry.TaskNeedsYou || got.QuestionRev != 1 {
		t.Fatalf("task %s waits %q, want your answers", got.State, got.WaitsWhat)
	}
	h.clock.Add(time.Minute)
	h.rt.sendUnknown("fx-writer")
	h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "yes"}})
	h.clock.Add(runtime.UnknownAfter)
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-writer", Kind: runtime.EventDeliveryUnknown, Reason: runtime.UnknownReason(task.ID, "fx-writer")})
	if _, err := h.reg.EnqueueRequest(h.ctx, ReqLetGo, LetGoPayload{TaskID: task.ID, Role: "fx-writer", Surface: "screen"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.e.waitBackground() // any check of the head runs off the loop
	h.tick()
	why := "the prompt to fx-writer was let go, and its turn is not watched: once its agent has committed on " + task.Branch +
		" or written proof.json since (tell it in its pane), retry the task; or stop it"
	h.waitsOn(task.ID, registry.TaskNeedsYou, waitsYou, why)

	h.clock.Add(time.Minute)
	head := r.commitAt(h.clock.Now(), "PR-12440 reuse CreditMailer, as answered", map[string]string{"app/credit.rb": strings.Replace(creditRB,
		"    0\n", "    10\n", 1) + "# reuses CreditMailer\n"})
	h.retry(task.ID)
	h.e.waitBackground()
	h.tick()
	if got := h.reload(task.ID); got.Stage != "local_review" || got.HeadSHA != head {
		t.Fatalf("task %s at %s waits %q, head %s; want past coding at %s", got.State, got.Stage, got.WaitsWhat, short(got.HeadSHA), short(head))
	}
	if n := len(h.rt.promptsTo("fx-writer")); n != 2 {
		t.Fatalf("the writer got %d prompts, want its two: the let-go one is never sent again", n)
	}
}

// Check A at a review's fix turn, which shares the coding stage's end: a
// fix prompt you let go never passes on what the writer left before it;
// once the writer's fix turn ran after it (its commit, proof.json and
// reply), your retry lets the loop go on, and the let-go prompt is never
// sent again.
func TestALetGoReviewFixPromptNeedsTheWritersTurnAfterIt(t *testing.T) {
	r := newReviewRig(t, result(refundFinding), result())
	task, _ := r.reviewTask("PR-12442")
	r.rt.sendUnknown("fx-writer")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	r.clock.Add(runtime.UnknownAfter)
	r.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-writer", Kind: runtime.EventDeliveryUnknown, Reason: runtime.UnknownReason(task.ID, "fx-writer")})
	if _, err := r.reg.EnqueueRequest(r.ctx, ReqLetGo, LetGoPayload{TaskID: task.ID, Role: "fx-writer", Surface: "screen"}); err != nil {
		t.Fatal(err)
	}
	r.run(3, nil)
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou ||
		!strings.HasPrefix(got.WaitsWhat, "the prompt to fx-writer was let go, and its turn is not watched: once ") {
		t.Fatalf("task %s at %s waits %q, want you: the fix turn you let go left nothing of its own", got.State, got.Stage, got.WaitsWhat)
	}

	r.writerFixes(task, 1, fixedCredit(1), nil)
	r.letGoOn(task.ID)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("task %s at %s waits %q after %d rounds, want pr_body after the fix's round", got.State, got.Stage, got.WaitsWhat,
			len(r.rv.calls()))
	}
	if n := len(r.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts, want the one you let go and no other", n)
	}
}

// A retry is never refused over a malformed asks.json when no question is
// open: the file is read only to tell whether open questions wait on you.
func TestARetryGoesOnWhateverAMalformedAsksFileSays(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	task := h.task("PR-12441", registry.TaskNeedsYou, "investigate")
	h.taskFile(task.ID, asksFile, "{not json")
	if q := h.retry(task.ID); q.State != registry.RequestDone {
		t.Fatalf("retry %s: %q, want it to go on", q.State, q.Result)
	}
	if got := h.reload(task.ID); got.State != registry.TaskActive {
		t.Fatalf("task %s after the retry, want active", got.State)
	}
}

// A read of the head's commit time that fails keeps a let-go role where it
// is (checked again on the next pass), never the untrue reason "no commit
// since the prompt you let go".
func TestALetGoCheckWhoseGitFailsKeepsTheTaskWhereItIs(t *testing.T) {
	h := newHarness(t, func(d *Deps) {
		d.Git = gitx.New(&execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}, Result: execx.Result{Code: 128},
			Err: &execx.ExitError{Code: 128, Stderr: "fatal: unable to read the object"}}}})
	})
	task := h.kindTask("PR-12399", "feature", "coding")
	st, ok := h.e.writerStage(task)
	if !ok {
		t.Fatal("the feature kind has no coding stage")
	}
	role, _ := h.cfg.FactoryRole("fx-writer")
	r := &stageRole{role: role, runAs: role, letGo: true, letGoSince: h.clock.Now().Add(-time.Minute)}
	p, err := h.e.beforeLetGo(h.ctx, &task, st, r, &agentsRun{copy: Copy{Path: t.TempDir()}})
	if err == nil || p != nil {
		t.Fatalf("beforeLetGo = %+v, %v; want the read's error and no problem", p, err)
	}
}
