package factory

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A lint crash the change caused (a plugin its .rubocop.yml names that
// cannot load) names no offense: the writer's, with no accept offered, and
// an accept of its line (one an older build offered) never passes a later
// run that crashes the same way.
func TestALintCrashTheChangeCausedOffersNoAccept(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12609", "bug")
	r.fix(task)
	r.commit("PR-12609 the lint's own change", map[string]string{".rubocop.yml": "plugins:\n  - rubocop-rspec_rails\n"})
	r.lintCrash = func([]string) string {
		return "cannot load such file -- rubocop-rspec_rails\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || res.ForYou || !strings.Contains(res.Why, ".rubocop.yml") || !res.unnamed ||
		slices.Contains(optionLabels(fixQuestionOf(res).q), optionAccept) {
		t.Fatalf("precheck %+v, %v; want the writer's unnamed failure naming .rubocop.yml, its question with no accept", res, err)
	}
	dir := r.layout.FactoryTaskDir(task.ID)
	rec, err := readStops(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.Accepted = append(rec.Accepted, accepted{Check: res.check, Failures: res.failures})
	if err := writeStops(dir, rec); err != nil {
		t.Fatal(err)
	}
	if again, err := r.e.precheck(r.ctx, task, r.cp); err != nil || again.Passed {
		t.Fatalf("precheck %+v, %v after an accept of the crash's line; want it still failing", again, err)
	}
}

// A jest run that names only its summary ("Tests: 1 failed, …") names no
// failing test: no accept is offered for it, in the precheck as in the
// proof.
func TestAJestRunThatNamesOnlyItsSummaryOffersNoAccept(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12610", "bug")
	r.fix(task)
	r.mu.Lock()
	r.adds = map[string]string{"bin/jest": "Tests:       1 failed, 3 passed, 4 total\n"}
	r.mu.Unlock()
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !res.unnamed || slices.Contains(optionLabels(fixQuestionOf(res).q), optionAccept) {
		t.Fatalf("precheck %+v, %v; want the writer's unnamed failure, its question with no accept", res, err)
	}
	_, f, unnamed := checkResult{Proof: &Proof{Why: "app/javascript/credit.test.js fails at the head (3f2a1bc)",
		Head: ProofRun{Exit: 1, Output: "Tests:       5 failed, 3 passed, 8 total\n"}}}.failedCheck()
	if !unnamed || len(f) == 0 {
		t.Fatalf("the proof's failures %q (unnamed %v); want its summary, unnamed", f, unnamed)
	}
}

// A gcc-style row ("path:line:col: warning: …", shellcheck -f gcc,
// swiftlint, clang) is an offense like any: one on a changed line fails
// the precheck. Only a Ruby warning of a file outside the copy is none.
func TestAGccStyleWarningOnAChangedLineCounts(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12611", "bug")
	r.fix(task)
	r.lint = func([]string) string {
		return "spec/models/credit_points_spec.rb:1:1: warning: Use 'cd ... || exit' in case cd fails. [SC2164]\n" +
			"spec/models/credit_points_spec.rb:40:6: error: Double quote array expansions. [SC2068]\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !strings.Contains(res.Why, "1 offense on the changed lines") {
		t.Fatalf("precheck %+v, %v; want the writer's warning on the changed line", res, err)
	}
}

// A lint that prints the copy's physical path (RuboCop's and eslint's
// getcwd) under a copy path that goes through a symlink names the copy's
// files all the same: an offense on a changed line fails the precheck.
func TestAnOffenseUnderTheCopysPhysicalPathCounts(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12612", "bug")
	r.fix(task)
	link := filepath.Join(t.TempDir(), "copy-link")
	if err := os.Symlink(r.copyDir, link); err != nil {
		t.Fatal(err)
	}
	phys, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	r.lint = func([]string) string {
		return phys + "/spec/models/credit_points_spec.rb:1:1: C: Style: the writer's\n"
	}
	cp := r.cp
	cp.Path = link
	res, err := r.e.precheck(r.ctx, task, cp)
	if err != nil || res.Passed || !strings.Contains(res.Why, "1 offense on the changed lines") {
		t.Fatalf("precheck %+v, %v; want the writer's offense, named through the copy's physical path", res, err)
	}
}

// An offense that only moved (its line, its column, the numbers in its
// message) is the same failure for the writer's progress, read from every
// failing check's failures ("<check>: <failure>"): three turns of it are no
// progress. Another offense is progress.
func TestAnOffenseThatOnlyMovedIsNoProgress(t *testing.T) {
	const key = "precheck.lint `bin/lint {files}`"
	turn := func(f string) turnFailure {
		return turnFailure{Check: key, Failures: []string{key + ": " + f}}
	}
	moved := []turnFailure{
		turn("app/models/credit.rb:12:3: C: Metrics/AbcSize: Assignment Branch Condition size for points is too high. [<5, 18, 6> 19.65/17]"),
		turn("app/models/credit.rb:14:3: C: Metrics/AbcSize: Assignment Branch Condition size for points is too high. [<5, 19, 6> 20.1/17]"),
		turn("app/models/credit.rb:15:3: C: Metrics/AbcSize: Assignment Branch Condition size for points is too high. [<6, 19, 6> 20.8/17]"),
	}
	if progressed(moved) {
		t.Fatal("an offense that only moved reads as progress")
	}
	other := append(slices.Clone(moved[:2]), turn("app/models/credit.rb:15:3: C: Style/GuardClause: Use a guard clause."))
	if !progressed(other) {
		t.Fatal("another offense reads as no progress")
	}
	sc := func(code string) turnFailure {
		return turn("bin/deploy.sh:12:1: warning: Quote this to prevent word splitting. [" + code + "]")
	}
	if sameFailure(sc("SC2086"), sc("SC2068")) || !sameFailure(turn("app/a.rb:3:5: C: Style/Rule1: x"), turn("app/a.rb:9:5: C: Style/Rule1: x")) ||
		sameFailure(turn("app/a.rb:3:5: C: Style/Rule1: x"), turn("app/a.rb:3:5: C: Style/Rule2: x")) {
		t.Fatal("an offense's code or its cop's name was read as a number that changes")
	}
}

// jest's summary ("Tests: 5 failed, …") is no lint offense: its count stays
// in its key, so 5 failing tests then 1 is progress, and an accept of a
// summary never covers more failing tests.
func TestJestsSummaryKeepsItsCount(t *testing.T) {
	const proof = "the factory's proof"
	a := turnFailure{Check: proof, Failures: []string{"Test Suites: 1 failed, 1 total", "Tests:       5 failed, 3 passed, 8 total"}}
	b := turnFailure{Check: proof, Failures: []string{"Test Suites: 1 failed, 1 total", "Tests:       1 failed, 7 passed, 8 total"}}
	if sameFailure(a, b) {
		t.Fatal("5 failing tests then 1 read as the same failure")
	}
	rec := stopsRecord{Accepted: []accepted{{Check: "precheck.jest", Failures: []string{"Tests:       1 failed, 3 passed, 4 total"}}}}
	if rec.accepts("precheck.jest", []string{"Tests:       2 failed, 2 passed, 4 total"}) {
		t.Fatal("an accept of 1 failing test covers 2")
	}
}

// yarn v1's footer ("info Visit https://yarnpkg.com/… for documentation
// about this command.", "error Command failed with exit code 1.") is noise:
// the failure is the line the tool printed.
func TestYarnsFooterIsNoFailureLine(t *testing.T) {
	got := firstFailing(ProofRun{Exit: 1, out: []byte("yarn run v1.22.19\n$ prettier --check src/a.js\nChecking formatting...\n" +
		"info Visit https://yarnpkg.com/en/docs/cli/run for documentation about this command.\n"),
		errOut: []byte("error Command failed with exit code 1.\n[warn] src/a.js\n")})
	if !slices.Equal(got, []string{"[warn] src/a.js"}) {
		t.Fatalf("the failure read %q; want the warning, past yarn's footer", got)
	}
}

// A Ruby warning of a gem installed under the copy (BUNDLE_PATH=vendor/bundle,
// untracked) is no offense: a lint that prints one and then crashes (exit
// 2) never passes as "1 offense, none on a changed line". A row of a file
// git tracks in the copy still counts.
func TestARubyWarningOfAGemUnderTheCopyIsNoOffense(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12613", "bug")
	r.fix(task)
	r.commit("PR-12613 the lint's own change", map[string]string{".rubocop.yml": "plugins:\n  - rubocop-rspec_rails\n"})
	r.lintCrash = func([]string) string {
		return r.copyDir + "/vendor/bundle/ruby/3.3.0/gems/activesupport-7.0.8/lib/active_support/notifications/fanout.rb:3: " +
			"warning: mutex_m was loaded from the standard library, but will no longer be part of the default gems\n" +
			"cannot load such file -- rubocop-rspec_rails\n"
	}
	if res, err := r.e.precheck(r.ctx, task, r.cp); err != nil || res.Passed {
		t.Fatalf("precheck %+v, %v; want the crash's failure, never a pass on a vendored gem's warning", res, err)
	}
	r.lintCrash = nil
	r.lint = func([]string) string { return "README:1:1: warning: a line of a tracked file\n" }
	if res, err := r.e.precheck(r.ctx, task, r.cp); err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v; want a pass: a tracked file's offense on no changed line", res, err)
	}
}

// jest's own banners ("● Validation Warning:", "● Validation Error:",
// "● Deprecation Warning:", "● Multiple configurations found:") name no
// failing test: a run that names nothing else offers no accept, in the
// precheck as in the proof.
func TestJestsBannersNameNoFailingTest(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12614", "bug")
	r.fix(task)
	r.mu.Lock()
	r.adds = map[string]string{"bin/jest": "\u25cf Validation Warning:\n\n  Unknown option \"testURL\" with value \"http://localhost\" was found.\n"}
	r.mu.Unlock()
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !res.unnamed || slices.Contains(optionLabels(fixQuestionOf(res).q), optionAccept) {
		t.Fatalf("precheck %+v, %v; want the writer's unnamed failure, its question with no accept", res, err)
	}
	for _, banner := range []string{"Validation Error:", "Deprecation Warning:", "Multiple configurations found:"} {
		_, f, unnamed := checkResult{Proof: &Proof{Why: "app/javascript/credit.test.js fails at the head (3f2a1bc)",
			Head: ProofRun{Exit: 1, Output: "\u25cf " + banner + "\n"}}}.failedCheck()
		if !unnamed {
			t.Fatalf("the proof's failures %q for %q are named; want them unnamed", f, banner)
		}
	}
	if fs := jestFailures("\u25cf Deprecation Warning:\nFAIL app/javascript/credit.test.js\n  \u2715 renders the points\n"); !jestNamed(fs) ||
		slices.Contains(fs, "\u25cf Deprecation Warning:") {
		t.Fatalf("jest's failures %q; want the failing test named, never the banner", fs)
	}
}

// A lint's row whose file git cannot tell tracked or not (ls-tree fails)
// makes its report one the factory cannot read: the writer's, never a pass
// on a gem's warning under the copy, nor the crash's way.
func TestARowGitCannotPlaceMakesTheReportUnreadable(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12615", "bug")
	r.fix(task)
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, "ls-tree") && strings.Contains(strings.Join(args, " "), "vendor/bundle") {
			return errors.New("fatal: unable to read tree")
		}
		return nil
	})
	r.lintCrash = func([]string) string {
		return r.copyDir + "/vendor/bundle/ruby/3.3.0/gems/activesupport-7.0.8/lib/active_support/notifications/fanout.rb:3: " +
			"warning: mutex_m was loaded from the standard library\ncannot load such file -- rubocop-rspec_rails\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || res.ForYou || res.fault != nil || !res.unnamed || !strings.Contains(res.Why, "cannot read") {
		t.Fatalf("precheck %+v, %v; want the writer's unnamed failure of a report the factory cannot read", res, err)
	}
}

// stylelint's syntax row ("45:1  \u2716  Unclosed block  CssSyntaxError") in a
// file the change touched is the writer's whatever its line, as eslint's
// Parsing error and RuboCop's Lint/Syntax are.
func TestAStylelintSyntaxRowOfATouchedFileIsTheWriters(t *testing.T) {
	r := newCodingRig(t)
	r.cfg.Factory.Repos[0].Precheck.Lint = []string{"bin/lint stylelint {files}"}
	task := r.codingTask("PR-12616", "bug")
	r.fix(task)
	r.commit("PR-12616 the points' badge", map[string]string{"app/assets/stylesheets/a.css": ".points { color: red; }\n"})
	r.lintCrash = func([]string) string {
		return "\napp/assets/stylesheets/a.css\n  45:1  \u2716  Unclosed block  CssSyntaxError\n\n\u2716 1 problem (1 error, 0 warnings)\n\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !strings.Contains(res.Why, "1 offense on the changed lines") {
		t.Fatalf("precheck %+v, %v; want the writer's syntax error in the touched file", res, err)
	}
}

// An unnamed failure is keyed on every meaningful line it prints (noise
// excluded), each once: the same run reads the same, and a run that lost
// one of its lines (c.js fixed while a.js still fails) is progress.
func TestEveryLineOfAnUnnamedFailureIsItsKey(t *testing.T) {
	read := func(stderr string) []string {
		return firstFailing(ProofRun{Exit: 1, out: []byte("yarn run v1.22.19\n$ prettier --check src\nChecking formatting...\n"),
			errOut: []byte(stderr)})
	}
	const both = "[warn] src/a.js\n[warn] src/c.js\n[warn] src/a.js\n"
	a, b, one := read(both), read(both), read("[warn] src/a.js\n")
	if !slices.Equal(a, []string{"[warn] src/a.js", "[warn] src/c.js"}) || !slices.Equal(a, b) {
		t.Fatalf("the failure read %q and %q; want every line once, the same on every run", a, b)
	}
	const key = "precheck.lint `yarn prettier --check {files}`"
	turn := func(fs []string) turnFailure {
		t := turnFailure{Check: key}
		for _, f := range fs {
			t.Failures = append(t.Failures, key+": "+f)
		}
		return t
	}
	if !progressed([]turnFailure{turn(a), turn(b), turn(one)}) || progressed([]turnFailure{turn(a), turn(b), turn(a)}) {
		t.Fatal("a fixed file of an unnamed failure is no progress, or the same failure is")
	}
}
