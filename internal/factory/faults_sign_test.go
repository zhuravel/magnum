package factory

import (
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// A check failure is a fault only with a positive sign of a broken
// environment; anything the change could have caused, or that the factory
// cannot read, is the writer's, as before the fault steps. An rspec run that
// fails to load a changed file, or that names errors outside of examples,
// names no "rspec ./…" line and is the writer's all the same: no run again,
// no question.
func TestAnRspecRunThatFailsOutsideOfExamplesIsTheWriters(t *testing.T) {
	for name, out := range map[string]string{
		"a file that does not load": "An error occurred while loading ./spec/models/credit_points_spec.rb.\n" +
			"Failure/Error: Credit::Bonus.new\nNameError:\n  uninitialized constant Credit::Bonus\n" +
			"No examples found.\n\nFinished in 0.0001 seconds\n0 examples, 0 failures, 1 error occurred outside of examples\n",
		"errors outside of examples": "An error occurred in a `before(:suite)` hook.\nFailure/Error: FactoryBot.lint\n" +
			"FactoryBot::InvalidFactoryError: The following factories are invalid:\n\n" +
			"Finished in 1.2 seconds\n12 examples, 0 failures, 1 error occurred outside of examples\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := newCodingRig(t)
			task := r.codingTask("PR-12580", "bug")
			r.fix(task)
			r.mu.Lock()
			r.adds = map[string]string{"bin/specs": out}
			r.mu.Unlock()
			res, err := r.e.precheck(r.ctx, task, r.cp)
			if err != nil || res.Passed || res.ForYou || res.fault != nil || !strings.Contains(res.Why, "names no failing spec") {
				t.Fatalf("precheck %+v, %v; want the writer's failure, no fault", res, err)
			}
			if n := len(r.ran("bin/specs")); n != 1 {
				t.Fatalf("the specs ran %d times, want once: no fault step", n)
			}
		})
	}
}

// A jest run's failures are read through its colors, and from its summary
// line: a run that names them, or none the factory can read, is the
// writer's; only one that printed nothing at all, or that a signal ended, is
// a fault. A coverage threshold the change missed is the writer's too.
func TestAJestFailureIsAFaultOnlyWithASignOfABrokenEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		exits     []int
		fault     bool
	}{
		{"failures in color", "\x1b[31m\x1b[1mFAIL\x1b[22m\x1b[39m app/javascript/credit.test.js\n  \x1b[1m● renders the points\x1b[22m\n", nil, false},
		{"only the summary", "Tests:       1 failed, 3 passed, 4 total\n", nil, false},
		{"a coverage threshold", `Jest: "global" coverage threshold for lines (80%) not met: 75.3%`, nil, false},
		{"output it cannot read", "something went sideways\n", nil, false},
		{"nothing at all", "", []int{1, 1}, true},
		{"a signal", "", []int{137, 137}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			task := r.codingTask("PR-12581", "bug")
			r.fix(task)
			r.mu.Lock()
			if tc.out != "" {
				r.adds = map[string]string{"bin/jest": tc.out}
			}
			r.exits = map[string][]int{"bin/jest": tc.exits}
			r.mu.Unlock()
			res, err := r.e.precheck(r.ctx, task, r.cp)
			if err != nil || res.Passed || (res.fault != nil) != tc.fault || res.ForYou != tc.fault {
				t.Fatalf("precheck %+v, %v; want a fault %v", res, err, tc.fault)
			}
			if want := map[bool]int{true: 2, false: 1}[tc.fault]; len(r.ran("bin/jest")) != want {
				t.Fatalf("jest ran %d times, want %d", len(r.ran("bin/jest")), want)
			}
		})
	}
}

// A lint whose offenses come in a format the factory cannot read (rubocop's
// --format json) is a configuration question, no fault: it runs once, and
// the precheck fails for you with the format's fix, as before the fault
// steps, no question with a skip.
func TestALintInAFormatTheFactoryCannotReadAsksForTheFormatNotAFault(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12582", "bug")
	r.fix(task)
	r.lint = func([]string) string {
		return `{"files":[{"path":"app/credit.rb","offenses":[{"message":"Style/NumericLiterals","location":{"line":3}}]}]}` + "\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !res.ForYou || res.fault != nil || !strings.Contains(res.Why, "rubocop --format emacs") {
		t.Fatalf("precheck %+v, %v; want the format's fix for you, no fault", res, err)
	}
	if n := len(r.ran("bin/lint")); n != 1 {
		t.Fatalf("the lint ran %d times, want once", n)
	}
}

// A fault's question offers a fourth way, "hand it to the writer", and
// recommends it when what the check printed names a file the branch
// changed; the answer gives the writer a fix turn that names the failure.
func TestAFaultThatNamesAChangedFileRecommendsHandingItToTheWriter(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12583", "bug")
	r.startStage(task)
	r.fix(task)
	r.setLintCrash("Error: unexpected token in app/credit.rb\n")
	r.endTurns(task, 1)
	got := r.reload(task.ID)
	qs := r.newestQuestions(task.ID)
	if got.State != registry.TaskNeedsYou || len(qs) != 1 ||
		!slices.Equal(optionLabels(qs[0]), []string{optionSkip, optionLonger, optionDrop, optionWriter}) || qs[0].Recommended != 3 {
		t.Fatalf("task %s waits %q, questions %+v; want the four ways, the writer's recommended", got.State, got.WaitsWhat, qs)
	}
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: optionWriter}})
	r.passes(task, func() bool { return r.writerPrompts() > 1 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, fixLead) || !strings.Contains(ps[1].Text, "unexpected token in app/credit.rb") {
		t.Fatalf("the writer's prompts %+v; want a fix turn naming the lint's error", ps)
	}
}

// A fault never passes: a spec run that printed nothing at all, whose run
// again the precheck's cap stopped, fails the precheck for you, naming the
// crash, never "passed, unfinished".
func TestAFaultWhoseRunAgainTheCapStopsNeverPasses(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12584", "bug")
	r.fix(task)
	fire := make(chan time.Time)
	r.e.after = func(time.Duration) <-chan time.Time { return fire }
	r.mu.Lock()
	r.exits = map[string][]int{"bin/specs": {1}}
	r.mu.Unlock()
	var once sync.Once
	r.setHang(func(run scriptRun) bool {
		if !precheckSpecs(run) || r.precheckSpecRuns() < 1 {
			return false
		}
		once.Do(func() { close(fire) })
		return true // the run again: the cap stops it
	})
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !res.ForYou || res.fault == nil || !strings.Contains(res.Why, "printed nothing") {
		t.Fatalf("precheck %+v, %v; want the crash for you, never a pass", res, err)
	}
}

// A lint that hangs through the whole cap and then passes on its run again
// leaves the specs and jest their own cap: they run, and the precheck passes
// with no note of what the cap kept from running.
func TestALintThatSpentTheCapAndThenPassesLeavesTheSpecsTheirCap(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12585", "bug")
	r.fix(task)
	var mu sync.Mutex
	var timers []chan time.Time
	r.e.after = func(time.Duration) <-chan time.Time {
		mu.Lock()
		defer mu.Unlock()
		ch := make(chan time.Time)
		timers = append(timers, ch)
		return ch
	}
	r.blocks["bin/lint"] = make(chan struct{}) // its first run never ends on its own
	done := make(chan PrecheckResult, 1)
	go func() {
		res, err := r.e.precheck(r.ctx, task, r.cp)
		if err != nil {
			t.Errorf("precheck: %v", err)
		}
		done <- res
	}()
	<-r.entered
	r.mu.Lock()
	delete(r.blocks, "bin/lint") // its run again ends at once
	r.mu.Unlock()
	mu.Lock()
	close(timers[0]) // the cap
	mu.Unlock()
	res := <-done
	if !res.Passed || res.Unfinished != "" {
		t.Fatalf("precheck %+v; want passed with the specs and jest run", res)
	}
	if len(r.ran("bin/specs")) == 0 || len(r.ran("bin/jest")) == 0 {
		t.Fatal("the specs or jest did not run after the lint spent the cap")
	}
}

// "Run it with a longer bound" bounds a spec run's fault too: your bound
// when it is longer than the last one, else twice the last one, which the
// event names.
func TestALongerBoundReachesTheSpecRunsFault(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12586", "bug")
	r.startStage(task)
	r.fix(task)
	var mu sync.Mutex
	var bounds []time.Duration
	r.seenMise = func(c execx.Cmd) {
		if s := scriptOf(c); strings.HasPrefix(s, "bin/specs ") && strings.Contains(s, "app/credit.rb") {
			mu.Lock()
			bounds = append(bounds, c.Timeout)
			mu.Unlock()
		}
	}
	r.mu.Lock()
	r.exits = map[string][]int{"bin/specs": {0, 0, 1, 1}} // the proof's two runs, then the precheck's run and its run again
	r.mu.Unlock()
	r.endTurns(task, 1)
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "precheck.specs") {
		t.Fatalf("task %s waits %q, want the specs' question", got.State, got.WaitsWhat)
	}
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: optionLonger + ": 5m"}})
	r.passes(task, func() bool { mu.Lock(); defer mu.Unlock(); return len(bounds) > 2 })
	mu.Lock()
	defer mu.Unlock()
	if len(bounds) < 3 || bounds[2] != 21*time.Minute {
		t.Fatalf("the specs' runs were bounded by %v, want the next by 21m: 5m is no longer than the cap, so twice it", bounds)
	}
	if !slices.ContainsFunc(r.events(task.ID, "stop"), func(e registry.Event) bool { return strings.Contains(e.Text, "21m") }) {
		t.Fatalf("no stop event names the bound: %+v", r.events(task.ID, "stop"))
	}
}

// The heal's begin event is written even when the precheck's cap ended its
// context: a lint that hung through the cap still has its heal recorded.
func TestTheHealOfALintThatSpentTheCapIsRecorded(t *testing.T) {
	r := installsRig(t)
	task := r.codingTask("PR-12587", "bug")
	r.fix(task)
	fire := make(chan time.Time)
	r.e.after = func(time.Duration) <-chan time.Time { return fire }
	r.blocks["bin/lint"] = make(chan struct{}) // it never ends on its own
	done := make(chan error, 1)
	go func() {
		_, err := r.e.precheck(r.ctx, task, r.cp)
		done <- err
	}()
	<-r.entered
	close(fire) // the cap, and every bound after it
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(r.events(task.ID, "heal"), func(e registry.Event) bool { return e.Phase == registry.PhaseBegin }) {
		t.Fatalf("heal events %+v, want its begin", r.events(task.ID, "heal"))
	}
}

// A longer bound comes only from the option's own field or from "bound
// <d>" in your words, never from a duration elsewhere in free text, and it
// is longer than the check's last bound, else twice that one.
func TestALongerBoundIsTakenOnlyWhereItIsNamedAndIsLonger(t *testing.T) {
	ask := stopAsk{Kind: stopCheck, Check: "precheck.specs", Bound: (10 * time.Minute).String()}
	for answer, want := range map[string][2]string{
		"it took 40m yesterday, look at the seeds": {decideAgain, ""},
		"bound 40m":            {decideLonger, "40m0s"},
		optionLonger + ": 40m": {decideLonger, "40m0s"},
		optionLonger + ": 5m":  {decideLonger, "20m0s"},
		optionLonger:           {decideLonger, "20m0s"},
		"please, bound 5m":     {decideLonger, "20m0s"},
	} {
		if d, words := decideStop(ask, answer); d != want[0] || (want[1] != "" && words != want[1]) {
			t.Errorf("decideStop(%q) = %q, %q; want %q, %q", answer, d, words, want[0], want[1])
		}
	}
}

// A fault's run again that did not start for a reason of the moment (EAGAIN
// on a loaded machine) is the machine's, as any start failure: the precheck
// ends on that error, which the infrastructure hold tries again, never on
// the fault's question.
func TestAFaultsRunAgainThatDidNotStartIsTheMachines(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12590", "bug")
	r.fix(task)
	var mu sync.Mutex
	runs := 0
	r.mu.Lock()
	r.exits = map[string][]int{"bin/specs": {1}} // its run prints nothing: a fault's sign
	r.startErr = func(run scriptRun) error {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(run.Script, "bin/specs") {
			if runs++; runs == 2 {
				return &os.SyscallError{Syscall: "fork/exec", Err: syscall.EAGAIN}
			}
		}
		return nil
	}
	r.mu.Unlock()
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err == nil || !errors.Is(err, syscall.EAGAIN) || res.ForYou || res.fault != nil {
		t.Fatalf("precheck %+v, %v; want the machine's error, no question for you", res, err)
	}
}
