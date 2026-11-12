package factory

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// fixQuestionOf is the fix question a precheck's result res would ask.
func fixQuestionOf(res PrecheckResult) stopQuestion {
	return fixQuestion(registry.Attempt{ID: "t1-coding-1", Stage: "coding", N: 1}, &stageRole{runAs: config.Role{Name: "fx-writer"}},
		&endProblem{check: res.check, failures: res.failures, unnamed: res.unnamed, why: res.Why}, fixBudget{})
}

// specsAgain runs task's precheck again with bin/specs printing out.
func (r *codingRig) specsAgain(task registry.Task, out string) PrecheckResult {
	r.t.Helper()
	r.mu.Lock()
	r.adds = map[string]string{"bin/specs": out}
	r.mu.Unlock()
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// "Accept this failing check as known" is offered only for a failure that
// names its examples, tests or offenses: one that names nothing (an error
// outside of examples, an unreadable output, a coverage threshold) never
// gets it, as a line read from such a run cannot tell a new failure from
// the old one.
func TestAFailureThatNamesNothingOffersNoAccept(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12600", "bug")
	r.fix(task)
	for _, out := range []string{afterSuiteRun("11866"), beforeSuiteRun("1"), "something went sideways\n"} {
		res := r.specsAgain(task, out)
		if res.Passed || !res.unnamed || slices.Contains(optionLabels(fixQuestionOf(res).q), optionAccept) {
			t.Fatalf("precheck %+v; want the writer's unnamed failure, its question with no accept", res)
		}
	}
	named := r.specsAgain(task, "Failures:\n\n  1) Credit counts\nrspec ./spec/models/credit_points_spec.rb:3 # Credit counts\n")
	if named.unnamed || !slices.Contains(optionLabels(fixQuestionOf(named).q), optionAccept) {
		t.Fatalf("precheck %+v; want the named failure's question to offer the accept", named)
	}
}

// A run's banner and its tools' versions are no failure ("Checking
// formatting...", "yarn run v1.22.19"): the failure is the line after
// them.
func TestABannerIsNoFailureLine(t *testing.T) {
	read := func(stdout, stderr string) []string {
		return firstFailing(ProofRun{Exit: 1, out: []byte(stdout), errOut: []byte(stderr)})
	}
	if got := read("yarn run v1.22.19\n$ prettier --check src/a.js\nChecking formatting...\n", "[warn] src/a.js\n"); !slices.Equal(got,
		[]string{"[warn] src/a.js"}) {
		t.Fatalf("the failure read %q; want the warning, past the banners", got)
	}
}

// jest's failing tests carry their duration ("✕ renders the points (12
// ms)"): the keys strip it, so the same test failing again is the same
// failure (no progress) and your accept of it holds.
func TestJestsFailureKeysCarryNoTiming(t *testing.T) {
	a := jestFailures("FAIL app/javascript/credit.test.js\n  ✕ renders the points (12 ms)\n")
	b := jestFailures("FAIL app/javascript/credit.test.js\n  ✕ renders the points (1.4 s)\n")
	if !slices.Equal(a, b) || slices.ContainsFunc(a, func(f string) bool { return strings.Contains(f, " ms)") }) {
		t.Fatalf("jest's keys %q and %q; want the same, with no timing", a, b)
	}
}

// The proof's failure that names no example is its why, whose head's sha
// changes with each commit: the key reads the sha as any, so a writer that
// fails the same way at each commit makes no progress.
func TestTheProofsUnnamedFailureReadsNoSha(t *testing.T) {
	failed := func(head string) []string {
		_, f, _ := checkResult{Proof: &Proof{Why: "app/javascript/credit.test.js fails at the head (" + head + ")"}}.failedCheck()
		return f
	}
	if a, b := failed("3f2a1bc"), failed("9e8d7c6"); !slices.Equal(a, b) {
		t.Fatalf("the proof's failures %q and %q; want the same, its sha read as any", a, b)
	}
}

// A turn records every failing check's failures, not the first check's
// alone: a writer that keeps a lint offense and fixes the red specs made
// progress.
func TestATurnRecordsEveryFailingChecksFailures(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12602", "bug")
	r.fix(task)
	r.lint = func([]string) string { return "spec/models/credit_points_spec.rb:1:1: C: Style: the writer's" }
	red := "Failures:\n\n  1) Credit counts\nrspec ./spec/models/credit_points_spec.rb:3 # Credit counts\n"
	one := r.specsAgain(task, red+"rspec ./spec/models/credit_points_spec.rb:7 # Credit sums\n")
	two := r.specsAgain(task, red)
	turn := func(res PrecheckResult) turnFailure {
		return turnOf(registry.Attempt{ID: "a"}, &stageRole{runAs: config.Role{Name: "fx-writer"}}, "run",
			&endProblem{check: res.check, failures: res.failures, all: res.all}, r.clock.Now())
	}
	if sameFailure(turn(one), turn(two)) {
		t.Fatalf("turns of %q and %q read as the same failure; want the specs' fewer failures as progress", one.all, two.all)
	}
}

// A lint accept is keyed on the offense's file, its cop or rule and its
// message, never its line and column, and counts its offenses: it holds
// after the lines above an accepted offense shift, and after a number in
// its message changes (LineLength's [121/120]), but a second offense of the
// same key in the file is new and fails.
func TestALintAcceptSurvivesShiftedLinesAndCountsItsOffenses(t *testing.T) {
	known := []string{"app/models/credit.rb:12:5: C: Layout/LineLength: Line is too long. [121/120]"}
	rec := stopsRecord{Accepted: []accepted{{Check: lintKey("bin/lint {files}"), Failures: known}}}
	key := lintKey("bin/lint {files}")
	if !rec.accepts(key, []string{"app/models/credit.rb:14:5: C: Layout/LineLength: Line is too long. [125/120]"}) {
		t.Fatal("the accepted offense, its lines shifted and its length grown, no longer matches itself")
	}
	if rec.accepts(key, []string{"app/models/credit.rb:14:5: C: Layout/LineLength: Line is too long. [125/120]",
		"app/models/credit.rb:30:1: C: Layout/LineLength: Line is too long. [133/120]"}) {
		t.Fatal("a second offense of the accepted key in the same file passes as accepted")
	}
	if rec.accepts(key, []string{"app/models/credit.rb:12:5: C: Style/GuardClause: Use a guard clause."}) {
		t.Fatal("another cop's offense passes as accepted")
	}
}

// eslint's fatal error of a file (a parse error, no line: "0:0") is an
// offense of the whole file: a file the change touched fails the lint for
// the writer, never passes as "none on a changed line".
func TestALintsFatalErrorOfAChangedFileIsTheWriters(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12603", "bug")
	r.fix(task)
	r.lint = func([]string) string {
		return r.copyDir + "/spec/models/credit_points_spec.rb\n  0:0  error  Parsing error: ESLint was configured to run on " +
			"this file, but that TSConfig does not include this file\n\n\u2716 1 problem (1 error, 0 warnings)\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || res.ForYou || !strings.Contains(res.Why, "1 offense on the changed lines") {
		t.Fatalf("precheck %+v, %v; want the writer's failure on the changed file", res, err)
	}
}

// A signal's death is told by the signal, not by an exit code: execx
// reports -1 with the signal, and the precheck reads it as 128 plus it
// (139 for SIGSEGV), the exit a segfault's rule and the signal's sign read.
func TestASignalsDeathReadsAsItsExit(t *testing.T) {
	if got := signalExit(-1, syscall.SIGSEGV); got != 139 {
		t.Fatalf("signalExit(-1, SIGSEGV) = %d, want 139", got)
	}
	if got := signalExit(2, 0); got != 2 {
		t.Fatalf("signalExit(2, none) = %d, want 2", got)
	}
}

// The lint's offenses are read from its whole output, never the 8 KiB the
// factory keeps: an offense on a changed line ahead of 10 KiB of others
// fails the lint for the writer.
func TestALintsOffensesAreReadFromItsWholeOutput(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12604", "bug")
	r.fix(task)
	r.lint = func([]string) string {
		out := "spec/models/credit_points_spec.rb:1:1: C: Style: the writer's own\n"
		for i := range 200 {
			out += fmt.Sprintf("app/legacy.rb:%d:1: C: Layout: an offense of the code the change left alone\n", i+10)
		}
		return out
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !strings.Contains(res.Why, "1 offense on the changed lines") {
		t.Fatalf("precheck %+v, %v; want the writer's offense on the changed line, past the kept tail", res, err)
	}
}

// A command missing for a lint run through a script the change defines
// (package.json's script, a Makefile's recipe) is the change's: those files
// count as the crash's cause, as the lint's own script does.
func TestAMissingCommandOfAScriptTheChangeDefinesIsTheChanges(t *testing.T) {
	crash := envCrash{cmd: "make lint {files}", sig: envSignature{scriptOnly: true, lacks: "a command"}}
	for _, f := range []string{"Makefile", "package.json", "lib/tasks/lint.rake", "bin/lint"} {
		if got := branchCaused(crash, "/bin/sh: eslnt: command not found", []string{"app/x.rb", f}); !slices.Contains(got, f) {
			t.Errorf("branchCaused with %s changed = %q; want it", f, got)
		}
	}
}

// A file the change deleted counts among the files it touched: a lint whose
// error names a configuration the change deleted is the change's, never the
// environment's.
func TestALintErrorNamingAFileTheChangeDeletedIsTheChanges(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12605", "bug")
	r.fix(task)
	r.git(r.copyDir, "rm", "--quiet", "README")
	r.git(r.copyDir, "commit", "--quiet", "-m", "PR-12605 drops the README")
	r.lintCrash = func([]string) string { return "Error: Configuration file not found: " + r.copyDir + "/README\n" }
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || res.ForYou || res.fault != nil || !strings.Contains(res.Why, "README") {
		t.Fatalf("precheck %+v, %v; want the writer's failure naming the deleted README", res, err)
	}
}

// A Ruby warning a gem prints ("/gems/foo/lib/foo.rb:5: warning: …") is
// no offense, nor is any offense in a file outside the copy: a lint that
// exits 2 with such lines and an error names no offense, and goes the
// no-offense way, never passing as "none on a changed line".
func TestARubyWarningOfAGemIsNoOffense(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12606", "bug")
	r.fix(task)
	r.lintCrash = func([]string) string {
		return "/usr/lib/ruby/gems/3.4.0/gems/foo-1.2/lib/foo.rb:5: warning: ostruct was loaded from the standard library\n" +
			"Error: cannot load such file -- rubocop-foo\n"
	}
	if res, err := r.e.precheck(r.ctx, task, r.cp); err != nil || res.Passed {
		t.Fatalf("precheck %+v, %v; want the lint's failure, never a pass on a gem's warning", res, err)
	}
}

// RuboCop's fatal row (F: Lint/Syntax, a missing `end` named at the file's
// last line) in a file the change touched is the writer's, whatever its line.
func TestARuboCopFatalRowOfAChangedFileIsTheWriters(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12607", "bug")
	r.fix(task)
	r.lint = func([]string) string {
		return "spec/models/credit_points_spec.rb:40:1: F: Lint/Syntax: unexpected token $end\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !strings.Contains(res.Why, "1 offense on the changed lines") {
		t.Fatalf("precheck %+v, %v; want the writer's failure on the changed file", res, err)
	}
}

// eslint's 0:0 warning of a changed file ("File ignored because no
// matching configuration was supplied") is no fatal error: the lint passes
// it as an offense on no changed line.
func TestALintsZeroZeroWarningIsNoFatalError(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12608", "bug")
	r.fix(task)
	r.lint = func([]string) string {
		return r.copyDir + "/spec/models/credit_points_spec.rb\n  0:0  warning  File ignored because no matching configuration was " +
			"supplied\n\n\u2716 1 problem (0 errors, 1 warning)\n"
	}
	if res, err := r.e.precheck(r.ctx, task, r.cp); err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v; want it passed: a warning is no fatal error", res, err)
	}
}

// A script-defining file the change touched is a missing command's cause
// only when it defines or names that command: a Makefile whose recipe runs
// it, a bin/ file of its name; one that names it not is the environment's.
func TestAScriptFileCausesOnlyTheMissingCommandItNames(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, text string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", "lint:\n\teslnt app\n")
	write("lib/tasks/cleanup.rake", "task :cleanup do\nend\n")
	crash := envCrash{cmd: "make lint {files}", copy: dir, line: "/bin/sh: eslnt: command not found",
		sig: envSignature{scriptOnly: true, lacks: "a command"}}
	if got := branchCaused(crash, crash.line, []string{"Makefile", "lib/tasks/cleanup.rake", "bin/eslnt"}); !slices.Equal(got,
		[]string{"Makefile", "bin/eslnt"}) {
		t.Fatalf("branchCaused = %q; want the Makefile that runs eslnt and bin/eslnt, never the rake task", got)
	}
	crash.line = "/bin/sh: shellcheck: command not found"
	if got := branchCaused(crash, crash.line, []string{"Makefile", "lib/tasks/cleanup.rake"}); len(got) != 0 {
		t.Fatalf("branchCaused = %q for a command no changed file names; want none", got)
	}
}
