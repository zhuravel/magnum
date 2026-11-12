package factory

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// passes runs the passes of task's stage (a tick, the checks off the loop,
// a tick) until done holds or the task left the live states, 8 at most.
func (r *codingRig) passes(task registry.Task, done func() bool) {
	r.t.Helper()
	for range 8 {
		if !isLive(r.reload(task.ID)) || done() {
			return
		}
		r.clock.Add(time.Minute)
		r.tick()
		r.e.waitBackground()
		r.tick()
	}
}

// lintCrashesFor makes bin/lint exit 2 naming no offense, with stderr,
// for its first n runs, then run as it does (no offense).
func (r *codingRig) lintCrashesFor(n int, stderr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	runs := 0
	r.lintCrash = func([]string) string {
		if runs++; runs <= n {
			return stderr
		}
		return ""
	}
}

// optionLabels are the labels of q's options.
func optionLabels(q registry.Question) []string {
	var out []string
	for _, o := range q.Options {
		out = append(out, o.Label)
	}
	return out
}

// A lint that exits non-zero and names no offense names no failure of the
// code: it runs again, then once more after the repository's installs, and
// when that third run passes, the writer never hears of it and nobody is
// asked. Each step is an event and an alert.
func TestALintThatNamesNoOffenseRunsAgainThenAfterTheInstalls(t *testing.T) {
	r := installsRig(t)
	task := r.codingTask("PR-12560", "bug")
	r.startStage(task)
	r.fix(task)
	r.lintCrashesFor(2, "Error: rubocop's server went away\n")
	r.endTurns(task, 1)
	if n := len(r.ran("bin/lint")); n != 3 {
		t.Fatalf("the lint ran %d times, want 3: its run, again, and after the installs", n)
	}
	if n := len(r.ran("bundle install")); n != 1 {
		t.Fatalf("bundle install ran %d times, want the one heal", n)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 1 {
		t.Fatalf("the writer got %d prompts (%+v), want none past its first: the check's fault is not its", len(ps), ps)
	}
	if got := r.reload(task.ID); got.State == registry.TaskNeedsYou {
		t.Fatalf("task waits on you %q, want it on", got.WaitsWhat)
	}
	var steps, alerts int
	for _, ev := range r.events(task.ID, "check") {
		if strings.Contains(ev.Text, "named no failure of the change: the factory") {
			steps++
		}
	}
	for _, ev := range r.events(task.ID, "alert") {
		if strings.Contains(ev.Text, "heals a check by itself") {
			alerts++
		}
	}
	if steps != 2 || alerts != 2 {
		t.Fatalf("%d step events and %d alerts, want one of each for each step", steps, alerts)
	}
}

// precheckSpecRuns counts the precheck's spec runs: the change's at the
// head and the red file's on the base (the proof's runs aside).
func (r *codingRig) precheckSpecRuns() int {
	n := 0
	for _, run := range r.ran("bin/specs") {
		if precheckSpecs(run) || strings.HasPrefix(run.Script, "bin/specs spec/models/credit_spec.rb") {
			n++
		}
	}
	return n
}

// baseBounds records the bound of each spec run on the base (a run of the
// red files without the change's).
func (r *codingRig) baseBounds() func() []time.Duration {
	var mu sync.Mutex
	var bounds []time.Duration
	r.mu.Lock()
	r.seenMise = func(c execx.Cmd) {
		if s := scriptOf(c); strings.HasPrefix(s, "bin/specs spec/models/credit_spec.rb") {
			mu.Lock()
			bounds = append(bounds, c.Timeout)
			mu.Unlock()
		}
	}
	r.mu.Unlock()
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(bounds)
	}
}

// hangingBase starts task key's coding stage, whose writer's change has a
// spec red at the head whose run on the base never ends: after the writer's
// turn, the base run hangs through its fault steps, and the task asks you.
func (r *codingRig) hangingBase(key string) (registry.Task, func() []time.Duration) {
	r.t.Helper()
	r.redOnMaster()
	task := r.codingTask(key, "bug")
	r.startStage(task)
	r.fix(task)
	r.smallCaps()
	bounds := r.baseBounds()
	r.hangOnTheBase("1 example so far\n")
	r.endTurns(task, 1)
	return r.reload(task.ID), bounds
}

// A run on the base that hangs runs again, then once with its bound doubled
// (up to precheck.cap_max); when that hangs too, the task asks you a
// question with the skip, the longer bound and the drop, the longer bound
// recommended, and its NEEDS YOU names the decision, never "retry". The
// writer never hears of it.
func TestABaseRunThatHangsGetsADoubledBoundThenAQuestion(t *testing.T) {
	r := newCodingRig(t)
	got, bounds := r.hangingBase("PR-12561")
	if b := bounds(); !slices.Equal(b, []time.Duration{600 * time.Millisecond, 600 * time.Millisecond, 1200 * time.Millisecond}) {
		t.Fatalf("base runs bounded by %v, want its run, again, then one with its bound doubled", b)
	}
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "skip this check for this task, run it with a longer bound or drop the task?") ||
		strings.Contains(strings.ToLower(got.WaitsWhat), "retry") {
		t.Fatalf("task %s waits %q, want the question's decision and no retry", got.State, got.WaitsWhat)
	}
	qs := r.newestQuestions(got.ID)
	if len(qs) != 1 || qs[0].RaisedBy != factoryAsks || !slices.Equal(optionLabels(qs[0]), []string{optionSkip, optionLonger, optionDrop}) ||
		qs[0].Recommended != 1 || !strings.Contains(qs[0].Context, "ran it with its bound doubled") {
		t.Fatalf("questions %+v, want the factory's one with skip, a longer bound (recommended) and drop", qs)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 1 {
		t.Fatalf("the writer got %d prompts, want no fix turn for a run that hangs", len(ps))
	}
}

// Your answer "skip" skips the check for the task: the precheck runs no
// specs and records them skipped, the task goes on, and the PR body names
// the check as skipped. A longer bound runs the base run with it.
func TestYourAnswerToACheckQuestionSkipsItOrGivesItYourBound(t *testing.T) {
	t.Run("skip", func(t *testing.T) {
		r := newCodingRig(t)
		task, _ := r.hangingBase("PR-12562")
		before := r.precheckSpecRuns()
		r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: task.QuestionRev, Answers: map[int]string{0: optionSkip}})
		r.passes(task, func() bool { return r.reload(task.ID).Stage != "coding" })
		if n := r.precheckSpecRuns() - before; n != 0 {
			t.Fatalf("the precheck's specs ran %d times after your skip", n)
		}
		if got := r.reload(task.ID); got.Stage == "coding" || got.State == registry.TaskNeedsYou {
			t.Fatalf("task at %s %s waits %q, want it past coding", got.Stage, got.State, got.WaitsWhat)
		}
		if got := r.checksOf(task.ID, "spec"); got[len(got)-1] != registry.CheckSkipped {
			t.Fatalf("spec checks %q, want the last one skipped", got)
		}
		if s := stopsSentences(r.layout.FactoryTaskDir(task.ID)); !strings.Contains(s, "skipped precheck.specs") {
			t.Fatalf("the PR body's sentences %q, want the skipped check named", s)
		}
	})
	t.Run("a longer bound", func(t *testing.T) {
		r := newCodingRig(t)
		task, bounds := r.hangingBase("PR-12563")
		r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: task.QuestionRev, Answers: map[int]string{0: optionLonger + ": 2s"}})
		r.passes(task, func() bool { return len(bounds()) > 3 })
		if b := bounds(); len(b) < 4 || b[3] != 2*time.Second {
			t.Fatalf("base runs bounded by %v, want the next by your 2s", b)
		}
	})
}

// The fault steps a precheck spent at a head are kept (check-faults.json):
// a restart in the middle of them, during the heal, takes the heal and
// never the run again a second time, so no step is doubled or lost.
func TestARestartInTheMiddleOfTheFaultStepsNeitherDoublesNorLosesOne(t *testing.T) {
	r := installsRig(t)
	task := r.codingTask("PR-12564", "bug")
	r.fix(task)
	r.lintCrashesFor(2, "Error: rubocop's server went away\n")
	ctx, cancel := context.WithCancel(r.ctx)
	r.blocks["bundle"] = make(chan struct{}) // the heal runs until the daemon stops
	r.setDuring(func(_ string, run scriptRun) {
		if strings.HasPrefix(run.Script, "bundle install") {
			cancel()
		}
	})
	if _, err := r.e.precheck(ctx, task, r.cp); err == nil {
		t.Fatal("the precheck the daemon's stop cut ended with no error")
	}
	if n := len(r.ran("bin/lint")); n != 2 {
		t.Fatalf("the lint ran %d times before the stop, want its run and its run again", n)
	}
	r.setDuring(nil)
	r.mu.Lock()
	delete(r.blocks, "bundle")
	r.mu.Unlock()
	r.restart()
	r.lintCrashesFor(1, "Error: rubocop's server went away\n")
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed {
		t.Fatalf("precheck after the restart %+v, %v; want it passed after the heal", res, err)
	}
	if n := len(r.ran("bin/lint")); n != 4 {
		t.Fatalf("the lint ran %d times in all, want 4: no run again a second time, the heal's run once", n)
	}
	if n := len(r.ran("bundle install")); n != 2 {
		t.Fatalf("bundle install ran %d times, want the cut one and the heal after the restart", n)
	}
}

// Your retry still works on a check's question, as before it: the question
// closes and the check runs again, once, at the same head; it spends no
// fault step (they were spent), so a check that still names no failure asks
// again at once.
func TestYourRetryOfACheckQuestionRunsTheCheckOnceAndSpendsNoStep(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12565", "bug")
	r.startStage(task)
	r.fix(task)
	r.setLintCrash("Error: boom\n")
	r.endTurns(task, 1)
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "skip this check for this task") {
		t.Fatalf("task %s waits %q, want the check's question", got.State, got.WaitsWhat)
	}
	runs := len(r.ran("bin/lint"))
	faults := r.readTask(task.ID, checkFaultsFile)
	if answer := r.retryAnswer(task.ID); !strings.Contains(answer, "goes on") {
		t.Fatalf("your retry answers %q, want it to go on", answer)
	}
	r.passes(task, func() bool { return r.reload(task.ID).State == registry.TaskNeedsYou })
	if n := len(r.ran("bin/lint")) - runs; n != 1 {
		t.Fatalf("the lint ran %d times after your retry, want once", n)
	}
	if r.readTask(task.ID, checkFaultsFile) != faults {
		t.Fatalf("%s changed after your retry:\n%s", checkFaultsFile, r.readTask(task.ID, checkFaultsFile))
	}
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || got.QuestionRev <= task.QuestionRev+1 {
		t.Fatalf("task %s at revision %d waits %q, want the question asked again", got.State, got.QuestionRev, got.WaitsWhat)
	}
}
