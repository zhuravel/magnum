package factory

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
)

// lintOffense is what bin/lint prints for an offense on the changed line.
const lintOffense = "app/credit.rb:3:5: C: Style/NumericLiterals: on the changed line\n"

// capWait starts the wait of a task whose writer's change the checks still
// refuse at the stage's prompt cap, with no retry of yours at the cap.
const capWait = "the fx-writer agent's change does not pass after 6 prompts in the stage: "

// retryCapWait starts the wait after the fix turns your retry at the cap
// gave.
const retryCapWait = "the fx-writer agent's change does not pass after 2 more prompts since your retry: "

// setLint makes bin/lint print out for every run ("": no offense).
func (r *codingRig) setLint(out string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lint = nil
	if out != "" {
		r.lint = func([]string) string { return out }
	}
}

// writerPrompts is how many prompts the writer got.
func (r *codingRig) writerPrompts() int { return len(r.rt.promptsTo("fx-writer")) }

// afterRetry runs the passes after your retry of task, in which the stage
// takes its writer's last end again and the checks run again, until the
// writer got want prompts, or the task left the live states, or done holds.
func (r *codingRig) afterRetry(task registry.Task, want int, done func() bool) {
	r.t.Helper()
	for range 8 {
		if !isLive(r.reload(task.ID)) || r.writerPrompts() >= want || (done != nil && done()) {
			return
		}
		r.clock.Add(time.Minute)
		r.tick()
		r.e.waitBackground()
		r.tick()
	}
}

// toPromptCap runs task key's writer, whose change lint refuses, to the
// coding stage's prompt cap: the task waits on you, the writer's
// runtime.MaxStagePrompts prompts spent.
func (r *codingRig) toPromptCap(key string) registry.Task {
	r.t.Helper()
	task := r.codingTask(key, "bug")
	r.setLint(lintOffense)
	r.startStage(task)
	r.fix(task)
	r.endTurns(task, 2*runtime.MaxStagePrompts)
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.HasPrefix(got.WaitsWhat, capWait) ||
		r.writerPrompts() != runtime.MaxStagePrompts {
		r.t.Fatalf("task %s waits %q after %d writer prompts; want you at the cap of %d", got.State, got.WaitsWhat, r.writerPrompts(),
			runtime.MaxStagePrompts)
	}
	return task
}

// retryFixTurns retries task, which waits on you at the writer's cap with
// had writer prompts, and runs the writer's turns while the checks refuse
// its change: it must get RetryFixTurns fix turns, each naming the lint's
// offense, and the task waits on you again, saying the count came from your
// retry.
func (r *codingRig) retryFixTurns(task registry.Task, had int) {
	r.t.Helper()
	r.retryAnswer(task.ID)
	r.afterRetry(task, had+1, nil)
	if got := r.reload(task.ID); r.writerPrompts() != had+1 || got.State != registry.TaskActive {
		r.t.Fatalf("after your retry task %s waits %q with %d writer prompts; want the writer's fix turn %d", got.State, got.WaitsWhat,
			r.writerPrompts(), had+1)
	}
	r.endTurns(task, 2*RetryFixTurns)
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != had+RetryFixTurns {
		r.t.Fatalf("the writer got %d prompts, want %d: %d more fix turns after your retry", len(ps), had+RetryFixTurns, RetryFixTurns)
	}
	for _, p := range ps[had:] {
		if !strings.HasPrefix(p.Text, fixLead) || !strings.Contains(p.Text, "lint: 1 offense") {
			r.t.Fatalf("a fix turn after your retry %q; want what failed", p.Text)
		}
	}
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.HasPrefix(got.WaitsWhat, retryCapWait) ||
		!strings.Contains(got.WaitsWhat, "lint: 1 offense on the changed lines") {
		r.t.Fatalf("task %s waits %q; want you again, %q and the last failure", got.State, got.WaitsWhat, retryCapWait)
	}
}

// Your retry at the coding stage's prompt cap runs the checks again, and
// the writer sees what they still refuse: RetryFixTurns fix turns, each
// naming what failed, then the task waits on you again, naming the count
// your retry gave.
func TestARetryAtThePromptCapGivesTheWriterTwoMoreFixTurns(t *testing.T) {
	r := newCodingRig(t)
	task := r.toPromptCap("PR-12401")
	r.retryFixTurns(task, runtime.MaxStagePrompts)
}

// Each retry at the cap gives RetryFixTurns again, counted from that retry:
// the first retry's turns spent, a second retry gives the writer two more.
func TestASecondRetryAtThePromptCapGivesTwoMoreFixTurnsAgain(t *testing.T) {
	r := newCodingRig(t)
	task := r.toPromptCap("PR-12402")
	r.retryFixTurns(task, runtime.MaxStagePrompts)
	r.retryFixTurns(task, runtime.MaxStagePrompts+RetryFixTurns)
}

// Your retry while the task waits on you for another cause (here a lint
// that crashed), one prompt short of the cap, changes nothing of it: the
// writer's prompts still stop at runtime.MaxStagePrompts in the attempt,
// with no turn more, and the wait reads as it does with no retry.
func TestARetryBelowThePromptCapLeavesTheCapAsItWas(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12403", "bug")
	r.setLint(lintOffense)
	r.startStage(task)
	r.fix(task)
	r.endTurns(task, runtime.MaxStagePrompts-2)
	if n := r.writerPrompts(); n != runtime.MaxStagePrompts-1 {
		t.Fatalf("the writer got %d prompts, want %d before the lint crashes", n, runtime.MaxStagePrompts-1)
	}
	r.commit("PR-12403 another try at the lint", map[string]string{"README": "shop, with credits\n"})
	r.setLintCrash("Error: boom\n") // the checks of the new head stop on it
	r.endTurns(task, 2)
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || strings.Contains(got.WaitsWhat, "does not pass") {
		t.Fatalf("task %s waits %q; want you for the lint's crash", got.State, got.WaitsWhat)
	}

	r.setLintCrash("")
	r.retryAnswer(task.ID)
	r.afterRetry(task, runtime.MaxStagePrompts, nil)
	r.endTurns(task, 2*RetryFixTurns)
	got := r.reload(task.ID)
	if n := r.writerPrompts(); n != runtime.MaxStagePrompts || got.State != registry.TaskNeedsYou || !strings.HasPrefix(got.WaitsWhat, capWait) {
		t.Fatalf("task %s waits %q after %d writer prompts; want you at the cap of %d, as with no retry", got.State, got.WaitsWhat, n,
			runtime.MaxStagePrompts)
	}
}

// Your retry at the cap whose checks pass (you fixed the change yourself)
// sends the writer nothing: its role is done and the coding stage passes.
func TestARetryAtThePromptCapWhoseChecksPassSendsNoFixTurn(t *testing.T) {
	r := newCodingRig(t)
	task := r.toPromptCap("PR-12404")
	r.setLint("")
	r.retryAnswer(task.ID)
	codingPassed := func() bool {
		for _, a := range r.attempts(task.ID) {
			if a.Stage == "coding" && a.State == registry.AttemptVerified {
				return true
			}
		}
		return false
	}
	r.afterRetry(task, runtime.MaxStagePrompts+1, codingPassed)
	if got := r.reload(task.ID); !codingPassed() || r.writerPrompts() != runtime.MaxStagePrompts || got.State == registry.TaskNeedsYou {
		t.Fatalf("task %s at %s waits %q with %d writer prompts; want the coding stage passed and no fix turn", got.State, got.Stage,
			got.WaitsWhat, r.writerPrompts())
	}
}

// A merge turn keeps its one fix turn for the base after your retry: the
// base still unmerged goes back to you at once, with no fix turn more, as
// it did before the retry.
func TestAMergeTurnGetsNoFixTurnMoreAfterARetry(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toConflict("PR-12405", nil)
	r.run(3, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
	r.clock.Add(time.Minute) // the merge turn ends without the merge
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	r.run(6, func() bool { return r.writerPrompts() > 2 || !isLive(r.reload(task.ID)) })
	r.clock.Add(time.Minute) // and so does its one fix turn
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
	const unmerged = "the fx-writer agent did not merge origin/master"
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || got.WaitsWhat != unmerged || r.writerPrompts() != 3 {
		t.Fatalf("task %s waits %q with %d writer prompts; want you, the base unmerged after its fix turn", got.State, got.WaitsWhat,
			r.writerPrompts())
	}

	r.retryAnswer(task.ID)
	r.afterRetry(task, 4, nil)
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || got.WaitsWhat != unmerged || r.writerPrompts() != 3 {
		t.Fatalf("after your retry task %s waits %q with %d writer prompts; want you again with no fix turn more", got.State,
			got.WaitsWhat, r.writerPrompts())
	}
}
