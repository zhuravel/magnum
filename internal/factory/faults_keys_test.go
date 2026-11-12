package factory

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// acceptUnnamed runs task's precheck with bin/specs printing out, records
// your accept of its unnamed failure (fixQuestion's ask, decided accept),
// and returns the result.
func (r *codingRig) acceptUnnamed(task registry.Task, out string) PrecheckResult {
	r.t.Helper()
	r.mu.Lock()
	r.adds = map[string]string{"bin/specs": out}
	r.mu.Unlock()
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !res.unnamed {
		r.t.Fatalf("precheck %+v, %v; want the writer's unnamed failure", res, err)
	}
	q := fixQuestion(registry.Attempt{ID: "t1-coding-1", Stage: "coding", N: 1}, &stageRole{runAs: config.Role{Name: "fx-writer"}},
		&endProblem{check: res.check, failures: res.failures, unnamed: res.unnamed, noAccept: res.noAccept, why: res.Why}, fixBudget{})
	if !slices.Contains(optionLabels(q.q), optionAccept) {
		return res
	}
	a := q.ask
	a.Decision = decideAccept
	dir := r.layout.FactoryTaskDir(task.ID)
	rec, _ := readStops(dir)
	rec.apply(a)
	if err := writeStops(dir, rec); err != nil {
		r.t.Fatal(err)
	}
	return res
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

// An accept of a failure no example names holds only while the whole
// failure stays the same: the set of the run's stable failure lines, never
// its first one alone. The same first error followed by a new error outside
// of examples fails again, as the option says. A run that ran no example or
// could not load a spec file is never accepted: the question offers no
// accept for it, so nothing the change breaks after it stays hidden.
func TestAnAcceptHoldsOnlyWhileTheWholeUnnamedFailureStaysTheSame(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12600", "bug")
	r.fix(task)
	r.acceptUnnamed(task, afterSuiteRun("11866"))
	if res := r.specsAgain(task, afterSuiteRun("4242")); !res.Passed {
		t.Fatalf("precheck %+v on the accepted failure, a new seed; want it passed", res)
	}
	more := strings.Replace(afterSuiteRun("7"), "\n\nFinished in", "\n\nAn error occurred in an `after(:suite)` hook.\n"+
		"Failure/Error: Billing.flush\n\nNameError:\n  uninitialized constant Billing\n\nFinished in", 1)
	more = strings.ReplaceAll(more, "1 error occurred outside of examples", "2 errors occurred outside of examples")
	if res := r.specsAgain(task, more); res.Passed {
		t.Fatalf("precheck %+v with a new error after the accepted one; want it failed", res)
	}

	z := newCodingRig(t)
	ztask := z.codingTask("PR-12601", "bug")
	z.fix(ztask)
	if res := z.acceptUnnamed(ztask, beforeSuiteRun("33502")); !res.noAccept {
		t.Fatalf("precheck %+v of a run of 0 examples; want no accept offered", res)
	}
	if res := z.specsAgain(ztask, beforeSuiteRun("1")); res.Passed {
		t.Fatalf("precheck %+v of a run of 0 examples after the question; want it failed", res)
	}
}

// A run's banner and its tools' versions are no failure ("Checking
// formatting...", "yarn run v1.22.19"): the failure is the line after
// them, so an accept of one file's warning never covers another's. A run
// that printed nothing but such noise is unreadable: the writer's, with no
// accept offered.
func TestABannerIsNoFailureLine(t *testing.T) {
	read := func(stdout, stderr string) []string {
		return firstFailing(ProofRun{Exit: 1, out: []byte(stdout), errOut: []byte(stderr)})
	}
	if got := read("yarn run v1.22.19\n$ prettier --check src/a.js\nChecking formatting...\n", "[warn] src/a.js\n"); !slices.Equal(got,
		[]string{"[warn] src/a.js"}) {
		t.Fatalf("the failure read %q; want the warning, past the banners", got)
	}
	if !unreadableRun(ProofRun{Exit: 1, out: []byte("yarn run v1.22.19\nChecking formatting...\nDone in 0.42s.\n")}) {
		t.Fatal("a run of banners alone reads as a failure line")
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

// SimpleCov's key keeps its minimum: an accept of a coverage under 90%
// never covers a coverage under another minimum.
func TestASimpleCovAcceptKeepsItsMinimum(t *testing.T) {
	line := func(at, minimum string) string {
		return "Line coverage (" + at + "%) is below the expected minimum coverage (" + minimum + "%)."
	}
	if acceptKey(line("82.35", "90.00")) != acceptKey(line("80.10", "90.00")) || acceptKey(line("82.35", "90.00")) == acceptKey(line("82.35", "95.00")) {
		t.Fatalf("acceptKey reads %q and %q; want the coverage as any and the minimum kept", acceptKey(line("82.35", "90.00")),
			acceptKey(line("82.35", "95.00")))
	}
}
