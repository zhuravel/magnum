package factory

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
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
// --format json) shows no sign of a broken environment: it is the
// writer's, with its output, as any failure the factory cannot read; it
// runs once, and nobody is asked.
func TestALintInAFormatTheFactoryCannotReadIsTheWriters(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12582", "bug")
	r.fix(task)
	r.lint = func([]string) string {
		return `{"files":[{"path":"app/credit.rb","offenses":[{"message":"Style/NumericLiterals","location":{"line":3}}]}]}` + "\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || res.ForYou || res.fault != nil || !strings.Contains(res.Why, "names no offense by path:line") {
		t.Fatalf("precheck %+v, %v; want the writer's failure, no question and no fault", res, err)
	}
	if n := len(r.ran("bin/lint")); n != 1 {
		t.Fatalf("the lint ran %d times, want once", n)
	}
}

// stylelint's exit 2 is its report of offenses (stylelint 17 prints it on
// stderr, in rows of line:column under the file's line), never a fault: its
// offenses are read by the changed lines (none here: the change leaves
// a.css), and nobody is asked.
func TestStylelintsExit2WithItsReportIsNeverAFault(t *testing.T) {
	r := newCodingRig(t)
	r.cfg.Factory.Repos[0].Precheck.Lint = []string{"bin/lint stylelint {files}"}
	task := r.codingTask("PR-12591", "bug")
	r.fix(task)
	r.lintCrash = func([]string) string {
		return "\napp/assets/stylesheets/a.css\n  1:3   \u2716  Empty block               block-no-empty\n" +
			"  2:12  \u2716  Invalid hex color \"#zzz\"  color-no-invalid-hex\n\n\u2716 2 problems (2 errors, 0 warnings)\n\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed || res.ForYou || res.fault != nil {
		t.Fatalf("precheck %+v, %v; want it passed: its offenses are on no changed line, never a fault", res, err)
	}
	if n := len(r.ran("bin/lint")); n != 1 {
		t.Fatalf("the lint ran %d times, want once: no fault step", n)
	}
	if got := faultSign(2, "", "a.css\n  1:3  \u2716  Empty block  block-no-empty\n", envCrash{}, false, "npx stylelint {files}"); got != "" {
		t.Fatalf("stylelint's exit 2 reads as the sign %q", got)
	}
}

// A lint's exit 2 with an error line is a fault's sign, but the branch's
// when the change touches what the lint loads (its configuration, its
// cops, its gems): after the fault steps, it is the writer's, never a
// question.
func TestALintsErrorExitIsTheWritersWhenTheBranchChangedWhatItLoads(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12592", "bug")
	r.fix(task)
	r.commit("PR-12592 the lint's own change", map[string]string{".rubocop.yml": "Style/Gone:\n  Enabled: true\n"})
	r.lintCrash = func([]string) string {
		return "Error: unrecognized cop or department Style/Gone found in .rubocop.yml\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || res.ForYou || res.fault != nil || !strings.Contains(res.Why, ".rubocop.yml") {
		t.Fatalf("precheck %+v, %v; want the writer's failure naming .rubocop.yml, no question", res, err)
	}
}

// A test runner that a segfault or an abort ended (139, 134), or that ran
// out of heap, while the change touches the code it tests, crashed on the
// change: the writer's, at once, with no fault step. A kill of the
// machine's (137) is still a fault.
func TestATestRunnersSegfaultOrHeapAbortOnChangedCodeIsTheWriters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		crash fakeCrash
		fault bool
	}{
		{"a segfault", fakeCrash{139, "/gems/ffi/lib/ffi.rb:12: [BUG] Segmentation fault at 0x0000000000000000\n"}, false},
		{"a heap's abort", fakeCrash{134, "FATAL ERROR: Reached heap limit Allocation failed - JavaScript heap out of memory\n"}, false},
		{"a kill", fakeCrash{137, "Killed\n"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			task := r.codingTask("PR-12593", "bug")
			r.fix(task)
			r.mu.Lock()
			r.crashes = map[string][]fakeCrash{"bin/specs": {tc.crash, tc.crash, tc.crash}}
			r.mu.Unlock()
			res, err := r.e.precheck(r.ctx, task, r.cp)
			if err != nil || res.Passed || (res.fault != nil) != tc.fault {
				t.Fatalf("precheck %+v, %v; want a fault %v", res, err, tc.fault)
			}
			if !tc.fault && (res.ForYou || len(r.ran("bin/specs")) != 1) {
				t.Fatalf("precheck %+v after %d runs; want the writer's failure after one run", res, len(r.ran("bin/specs")))
			}
		})
	}
}

// A fault's question offers a fourth way, "hand it to the writer", and the
// answer gives the writer a fix turn that names the failure. A fault whose
// output names a file the branch changed is the branch's: the writer gets
// it after the fault steps, and nobody is asked (a question about one
// would recommend the writer).
func TestAFaultThatNamesAChangedFileRecommendsHandingItToTheWriter(t *testing.T) {
	if q := faultQuestion(checkFault{key: "precheck.specs", what: "did not end", namesChanged: true}); q.q.Recommended != 3 {
		t.Fatalf("a fault naming a changed file recommends option %d, want the writer's", q.q.Recommended)
	}
	named := newCodingRig(t)
	named.cfg.Factory.Repos[0].Precheck.Jest = ""
	ntask := named.codingTask("PR-12594", "bug")
	named.startStage(ntask)
	named.fix(ntask)
	named.setLintCrash("Error: unexpected token in app/credit.rb\n")
	named.endTurns(ntask, 1)
	named.passes(ntask, func() bool { return named.writerPrompts() > 1 || !isLive(named.reload(ntask.ID)) })
	if ps := named.rt.promptsTo("fx-writer"); len(ps) != 2 || !strings.Contains(ps[1].Text, "unexpected token in app/credit.rb") ||
		len(named.newestQuestions(ntask.ID)) != 0 {
		t.Fatalf("the writer's prompts %+v; want a fix turn naming the error in the changed file, no question", ps)
	}

	r := newCodingRig(t)
	task := r.codingTask("PR-12583", "bug")
	r.startStage(task)
	r.fix(task)
	r.setLintCrash("Error: rubocop's server went away\n")
	r.endTurns(task, 1)
	got := r.reload(task.ID)
	qs := r.newestQuestions(task.ID)
	if got.State != registry.TaskNeedsYou || len(qs) != 1 ||
		!slices.Equal(optionLabels(qs[0]), []string{optionSkip, optionLonger, optionDrop, optionWriter}) {
		t.Fatalf("task %s waits %q, questions %+v; want the four ways", got.State, got.WaitsWhat, qs)
	}
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: optionWriter}})
	r.passes(task, func() bool { return r.writerPrompts() > 1 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, fixLead) || !strings.Contains(ps[1].Text, "rubocop's server went away") {
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

// rspec 3.13's output of a run whose error is outside of examples, as it
// printed it on scratch files with --order random (an after(:suite) and a
// before(:suite) hook that raise), with its seed for a run; and the start of
// Ruby 4.0's stderr of a spec that segfaults (exit 139), with its address.
func afterSuiteRun(seed string) string {
	return "\nRandomized with seed " + seed + "\n..\nAn error occurred in an `after(:suite)` hook.\n" +
		"Failure/Error: RSpec.configure { |c| c.after(:suite) { raise \"the suite left a file behind\" } }\n\n" +
		"RuntimeError:\n  the suite left a file behind\n# ./spec/credit_spec.rb:1:in 'block (2 levels) in <top (required)>'\n\n\n" +
		"Finished in 0.0218 seconds (files took 0.43136 seconds to load)\n2 examples, 0 failures, 1 error occurred outside of examples\n\n" +
		"Randomized with seed " + seed + "\n\n"
}

func beforeSuiteRun(seed string) string {
	return "\nRandomized with seed " + seed + "\n\nAn error occurred in a `before(:suite)` hook.\n" +
		"Failure/Error: RSpec.configure { |c| c.before(:suite) { raise ArgumentError, \"FactoryBot.lint found 2 invalid factories\" } }\n\n" +
		"ArgumentError:\n  FactoryBot.lint found 2 invalid factories\n# ./spec/credit_spec.rb:1:in 'block (2 levels) in <top (required)>'\n\n\n" +
		"Finished in 0.00522 seconds (files took 0.08076 seconds to load)\n0 examples, 0 failures, 1 error occurred outside of examples\n\n" +
		"Randomized with seed " + seed + "\n\n"
}

func segfaultRun(addr string) string {
	return "/copy/spec/credit_spec.rb:2: [BUG] Segmentation fault at " + addr + "\n" +
		"ruby 4.0.9 (2026-10-23 revision 3e93b23afc) +PRISM [arm64-darwin27]\n\n" +
		"-- Crash Report log information --------------------------------------------\n"
}

// A failure no example names reads the same on every run: the error's own
// line (the exception's class and message, else "An error occurred in …",
// else "Failure/Error: …", else a [BUG] line), past the noise of a run
// (rspec's seed, its run options, progress dots, its timing), hex addresses
// read as 0x?. Read on its first line, an error outside of examples gave
// "Randomized with seed N", new each run, so no turn ever failed the same
// way twice.
func TestAFailureNoExampleNamesReadsTheSameOnEveryRun(t *testing.T) {
	read := func(stdout, stderr string) []string {
		return firstFailing(ProofRun{Exit: 1, out: []byte(stdout), errOut: []byte(stderr)})
	}
	for _, tc := range []struct {
		name     string
		one, two []string
		want     string
	}{
		{"an after(:suite) error", read(afterSuiteRun("11866"), ""), read(afterSuiteRun("4242"), ""),
			"RuntimeError: the suite left a file behind"},
		{"a before(:suite) error", read(beforeSuiteRun("33502"), ""), read(beforeSuiteRun("7"), ""),
			"ArgumentError: FactoryBot.lint found 2 invalid factories"},
		{"a segfault", read("", segfaultRun("0x0000000000000000")), read("", segfaultRun("0x00007ff8a1b2c3d4")),
			"/copy/spec/credit_spec.rb:2: [BUG] Segmentation fault at 0x?"},
		{"progress and options alone", read("Run options: include {:focus=>true}\n....F..\nsomething went sideways\n", ""),
			read("Run options: include {:focus=>true}\n.......\nsomething went sideways\n", ""), "something went sideways"},
	} {
		if !slices.Equal(tc.one, tc.two) || len(tc.one) != 1 || tc.one[0] != tc.want {
			t.Errorf("%s: read %q and %q; want %q both times", tc.name, tc.one, tc.two, tc.want)
		}
	}
}

// Your accept of a failure no example names holds for that failure, read
// the same on every run (firstFailing: its seed is no part of it), so the
// question never comes back for it. A new failure of the check still fails,
// as the option says: a spec file that no longer loads, and a failing
// example.
func TestAnAcceptOfAFailureNoExampleNamesHoldsForItsCheck(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12595", "bug")
	r.fix(task)
	r.mu.Lock()
	r.adds = map[string]string{"bin/specs": afterSuiteRun("11866")}
	r.mu.Unlock()
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !res.unnamed {
		t.Fatalf("precheck %+v, %v; want the writer's failure, read with no example named", res, err)
	}
	p := &endProblem{check: res.check, failures: res.failures, unnamed: res.unnamed, why: res.Why}
	q := fixQuestion(registry.Attempt{ID: "t1-coding-1", Stage: "coding", N: 1}, &stageRole{runAs: config.Role{Name: "fx-writer"}}, p,
		fixBudget{})
	a := q.ask
	a.Decision = decideAccept
	dir := r.layout.FactoryTaskDir(task.ID)
	rec, _ := readStops(dir)
	rec.apply(a)
	if err := writeStops(dir, rec); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.adds = map[string]string{"bin/specs": afterSuiteRun("4242")}
	r.mu.Unlock()
	if res, err := r.e.precheck(r.ctx, task, r.cp); err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v after your accept, on a new seed; want it passed", res, err)
	}
	r.mu.Lock()
	r.adds = map[string]string{"bin/specs": "\nRandomized with seed 5\n\nAn error occurred while loading ./spec/models/credit_points_spec.rb.\n" +
		"Failure/Error: def points(\n\nSyntaxError:\n  --> ./app/credit.rb\n\n" +
		"Finished in 0.00004 seconds (files took 0.2 seconds to load)\n0 examples, 0 failures, 1 error occurred outside of examples\n"}
	r.mu.Unlock()
	if res, err := r.e.precheck(r.ctx, task, r.cp); err != nil || res.Passed {
		t.Fatalf("precheck %+v, %v on a spec file that no longer loads; want it failed: a new failure of the check", res, err)
	}
	r.mu.Lock()
	r.adds = map[string]string{"bin/specs": "Failures:\n\n  1) Credit counts\nrspec ./spec/models/credit_points_spec.rb:3 # Credit counts\n"}
	r.mu.Unlock()
	if res, err := r.e.precheck(r.ctx, task, r.cp); err != nil || res.Passed {
		t.Fatalf("precheck %+v, %v on a failing example; want it failed: the accept covers the unnamed failure only", res, err)
	}
}

// eslint's stylish report and stylelint's name the file on a line of its
// own and each offense on a row of line:column under it: the factory reads
// the file from that line, so the changed-line filter holds for them as for
// path:line offenses. An offense on a changed line fails the lint for the
// writer; one only on lines the change left passes it.
func TestALintsRowsUnderTheirFileAreReadByTheChangedLines(t *testing.T) {
	const spec = "spec/models/credit_points_spec.rb" // the change adds it: its line 1 is a changed line
	for _, tc := range []struct {
		name   string
		stderr bool // stylelint 17 prints its report on stderr, with exit 2
		report func(copyDir string, line int) string
	}{
		{"eslint's stylish", false, func(copyDir string, line int) string {
			return fmt.Sprintf("\n%s/%s\n  %d:5  error  'x' is assigned a value but never used  no-unused-vars\n\n"+
				"\u2716 1 problem (1 error, 0 warnings)\n", copyDir, spec, line)
		}},
		{"stylelint's", true, func(_ string, line int) string {
			return fmt.Sprintf("\n%s\n  %d:3   \u2716  Empty block  block-no-empty\n\n\u2716 1 problem (1 error, 0 warnings)\n\n", spec, line)
		}},
	} {
		for _, at := range []struct {
			line   int
			passed bool
		}{{1, false}, {40, true}} {
			t.Run(fmt.Sprintf("%s at line %d", tc.name, at.line), func(t *testing.T) {
				r := newCodingRig(t)
				task := r.codingTask("PR-12596", "bug")
				r.fix(task)
				report := tc.report(r.copyDir, at.line)
				if tc.stderr {
					r.lintCrash = func([]string) string { return report }
				} else {
					r.lint = func([]string) string { return report }
				}
				res, err := r.e.precheck(r.ctx, task, r.cp)
				if err != nil || res.Passed != at.passed || res.ForYou || res.fault != nil {
					t.Fatalf("precheck %+v, %v; want passed %v, never for you or a fault", res, err, at.passed)
				}
				if !at.passed && (!strings.Contains(res.Why, "1 offense on the changed lines") || res.unnamed) {
					t.Fatalf("the failure %q (unnamed %v); want the offense on the changed line, named", res.Why, res.unnamed)
				}
			})
		}
	}
}

// A report whose rows have no file's line above them (its tail, cut at
// outputKeep, lost the first file's line) is one the factory cannot read:
// the writer's, with its output, never read as named offenses, whose
// changed-line filter would drop the orphan rows and pass the lint.
func TestALintReportWithRowsOfNoFileIsTheWriters(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12597", "bug")
	r.fix(task)
	r.lint = func([]string) string {
		return "  3:5  error  'x' is assigned a value but never used  no-unused-vars\n" + // the first file's line was cut off
			"  7:1  error  Unexpected console statement  no-console\n\n" +
			"app/javascript/other.js\n  40:2  warning  Missing semicolon  semi\n\n\u2716 3 problems (2 errors, 1 warning)\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || res.ForYou || res.fault != nil || !res.unnamed ||
		!strings.Contains(res.Why, "names no offense by path:line") {
		t.Fatalf("precheck %+v, %v; want the writer's failure of an output it cannot read", res, err)
	}
	if offenses, orphans := namedOffenses(r.lint(nil)); !orphans || len(offenses) != 1 {
		t.Fatalf("namedOffenses = %+v, %v; want the one row under its file and the orphan rows told", offenses, orphans)
	}
}

// jestCoverageRun is a jest run with --coverage whose tests pass and whose
// coverage misses its global threshold, at lines percent.
func jestCoverageRun(lines string) string {
	rule := "----------|---------|----------|---------|---------|-------------------\n"
	return "PASS app/javascript/credit.test.js\n  \u2713 renders the points (12 ms)\n\n" + rule +
		"File      | % Stmts | % Branch | % Funcs | % Lines | Uncovered Line #s \n" + rule +
		"All files |   " + lines + " |      50 |     100 |   " + lines + " |                   \n" +
		" credit.js |   " + lines + " |      50 |     100 |   " + lines + " | 12-14             \n" + rule +
		"Jest: \"global\" coverage threshold for lines (80%) not met: " + lines + "%\n" +
		"Test Suites: 1 passed, 1 total\nTests:       1 passed, 1 total\nSnapshots:   0 total\nTime:        1.234 s\n"
}

// A jest run that misses its coverage threshold reads as its threshold's
// line, never as its table's rules, its header or a passing test's line,
// so coverage that rises is progress: the writer's next turns are not
// three of one failure.
func TestARisingCoverageIsProgress(t *testing.T) {
	read := func(lines string) []string {
		return firstFailing(ProofRun{Exit: 1, out: []byte(jestCoverageRun(lines))})
	}
	if got := read("75.3"); len(got) != 1 || got[0] != `Jest: "global" coverage threshold for lines (80%) not met: 75.3%` {
		t.Fatalf("the failure read %q; want the threshold's line", got)
	}
	turn := func(lines string) turnFailure { return turnFailure{Check: "precheck.jest", Failures: read(lines)} }
	if !progressed([]turnFailure{turn("70.1"), turn("75.3"), turn("78.9")}) || progressed([]turnFailure{turn("75.3"), turn("75.3"), turn("75.3")}) {
		t.Fatal("rising coverage reads as no progress, or the same coverage as progress")
	}
}
