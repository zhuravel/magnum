package factory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// checksOf are the task's checks of kind as "<result>[/<cause>]", but for
// the jest runs (their kind is spec too).
func (r *codingRig) checksOf(id int64, kind string) []string {
	r.t.Helper()
	cs, err := r.reg.Checks(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		if c.Kind != kind || strings.HasPrefix(c.Command, "bin/jest") {
			continue
		}
		s := c.Result
		if c.Cause != "" {
			s += "/" + c.Cause
		}
		out = append(out, s)
	}
	return out
}

// Lint runs on the changed files and counts only what it says about the
// changed lines (gitx.DiffLines): an offense on a line the change did not
// touch is not the change's.
func TestPrecheckLintsOnlyChangedLines(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12361", "bug")
	r.fix(task) // app/credit.rb's line 3 and a new spec
	var linted []string
	r.lint = func(files []string) string {
		linted = files
		return "app/credit.rb:3:5: C: Style/NumericLiterals: on the changed line\n" +
			"app/credit.rb:1:1: C: Style/Documentation: on an old line\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || !strings.Contains(res.Why, "lint") || !strings.Contains(res.Why, "1 offense on the changed lines") {
		t.Fatalf("precheck %+v, want a lint failure of 1 offense", res)
	}
	if !slices.Equal(linted, []string{"app/credit.rb", pointsSpec}) {
		t.Fatalf("lint ran on %q, want the changed files", linted)
	}
	report := r.readTask(task.ID, precheckFile)
	if !strings.Contains(report, "Style/NumericLiterals") || strings.Contains(report, "Style/Documentation") {
		t.Fatalf("%s names the wrong offenses:\n%s", precheckFile, report)
	}

	r.lint = func([]string) string { return "app/credit.rb:1:1: C: Style/Documentation: on an old line\n" }
	if res, err := r.e.precheck(r.ctx, task, r.cp); err != nil || !res.Passed {
		t.Fatalf("an offense on an old line only: %+v, %v; want passed", res, err)
	}
	if got := r.checksOf(task.ID, "lint"); !slices.Equal(got, []string{"fail/code", "pass"}) {
		t.Fatalf("lint checks %q", got)
	}
}

// A spec that the change's specs run and that is red on the base too is
// not the change's: recorded as a failure that is not ours, and the
// precheck passes. One the change turned red fails it.
func TestPrecheckDoesNotBlameASpecRedOnTheBase(t *testing.T) {
	r := newCodingRig(t)
	r.moveMaster("spec/models/credit_spec.rb", creditSpec+"expect: def bonus in app/credit.rb\n") // red on master
	r.git(r.main, "fetch", "--quiet", "origin")
	task := r.codingTask("PR-12362", "bug")
	base := r.git(r.copyDir, "rev-parse", "HEAD")
	head := r.fix(task)
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatalf("precheck %+v, want passed: the red spec is red on the base too", res)
	}
	if got := r.checksOf(task.ID, "spec"); !slices.Equal(got, []string{"fail/not_ours"}) {
		t.Fatalf("spec checks %q, want the red spec recorded as not ours", got)
	}
	runs := r.ran("bin/specs")
	if len(runs) != 2 || runs[0].Head != head || runs[1].Head != base || runs[1].Branch != "" ||
		!strings.Contains(runs[1].Script, "spec/models/credit_spec.rb") || strings.Contains(runs[1].Script, pointsSpec) {
		t.Fatalf("spec runs %+v; want the head's, then the base's of the red spec only", runs)
	}
	r.onBranch(task.Branch, head)
	if report := r.readTask(task.ID, precheckFile); !strings.Contains(report, "red on the base too") {
		t.Fatalf("%s does not say the spec is red on the base:\n%s", precheckFile, report)
	}

	// The change turns a green spec red: its own.
	r.commit("PR-12362 breaks the old spec", map[string]string{"app/credit.rb": "class Kredit\n  def points\n    10\n  end\nend\n"})
	res, err = r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || !strings.Contains(res.Why, "specs") || !strings.Contains(res.Why, "credit_spec.rb") {
		t.Fatalf("precheck %+v, want the change's red spec to fail it", res)
	}
	if got := r.checksOf(task.ID, "spec"); got[len(got)-1] != "fail/code" {
		t.Fatalf("spec checks %q, want the last one the change's", got)
	}
}

// A gem upgrade (a kind in skip_specs_for_kinds) runs no specs; its lint
// still runs.
func TestPrecheckSkipsSpecsForAGemUpgrade(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12363", "chore")
	r.commit("PR-12363 bump", map[string]string{"Gemfile.lock": "GEM\n  rails (8.1.1)\n", "app/credit.rb": creditRB + "# bumped\n"})
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v; want passed", res, err)
	}
	if n := len(r.ran("bin/specs")); n != 0 {
		t.Fatalf("%d spec runs for a gem upgrade, want none", n)
	}
	if got := r.checksOf(task.ID, "spec"); !slices.Equal(got, []string{"skipped"}) {
		t.Fatalf("spec checks %q, want one skipped", got)
	}
	if n := len(r.ran("bin/lint")); n != 1 {
		t.Fatalf("%d lint runs, want one", n)
	}
}

// One precheck at a time runs specs on the machine: while another holds
// the lock (Layout.FactorySpecsLock), the task's checks wait and say so, and
// they go on once it is free.
func TestPrecheckTakesTheMachineWideSpecLock(t *testing.T) {
	r := newCodingRig(t)
	lock := r.layout.FactorySpecsLock()
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, held, err := engine.AcquireLock(lock) // another precheck runs specs
	if err != nil || held {
		t.Fatalf("AcquireLock = %v, %v", held, err)
	}
	task := r.codingTask("PR-12364", "bug")
	r.startStage(task)
	r.fix(task)
	r.ended(task, "fx-writer")
	r.tick()
	waitFor(t, "the check to wait for the lock", func() bool { return strings.Contains(r.e.checkNote(task.ID), "another precheck runs specs") })
	r.tick()
	got := r.reload(task.ID)
	if got.WaitsWho != waitPrecheck || !strings.Contains(got.WaitsWhat, "another precheck runs specs") {
		t.Fatalf("task waits %q: %q, want the spec lock named", got.WaitsWho, got.WaitsWhat)
	}
	if n := len(r.ran("bin/specs")); n != 0 {
		t.Fatalf("%d spec runs under another's lock", n)
	}
	unlock()
	r.e.waitBackground()
	r.tick()
	if got := r.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task at %s waiting %q after the lock was free, want local_review", got.Stage, got.WaitsWhat)
	}
}

// The cap stops a precheck that runs too long, with a plain reason.
func TestPrecheckStopsAtItsCap(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12365", "bug")
	r.fix(task)
	fire := make(chan time.Time)
	r.e.after = func(d time.Duration) <-chan time.Time {
		if d != 10*time.Minute {
			t.Errorf("the cap's timer is %s, want 10m", d)
		}
		return fire
	}
	r.blocks["bin/lint"] = make(chan struct{}) // never ends on its own
	done := make(chan PrecheckResult, 1)
	go func() {
		res, err := r.e.precheck(r.ctx, task, r.cp)
		if err != nil {
			t.Errorf("precheck: %v", err)
		}
		done <- res
	}()
	<-r.entered
	close(fire)
	res := <-done
	if res.Passed || res.Why != "precheck: over 10m" {
		t.Fatalf("precheck %+v, want it stopped: precheck: over 10m", res)
	}
}

// Each migration the branch adds runs up and down with the repository's
// commands, in order; a gh-ost migration is not run (its down refuses by
// design: a person runs it), and every one is named with its flags.
func TestPrecheckRunsEachNewMigrationUpAndDown(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12366", "feature")
	plain := func(class, table string) string {
		return "class " + class + " < ActiveRecord::Migration[7.1]\n  def change\n    add_column :" + table + ", :x, :integer\n  end\nend\n"
	}
	r.commit("PR-12366 columns", map[string]string{
		"db/migrate/20261104090000_add_x_to_credits.rb": plain("AddXToCredits", "credits"),
		"db/migrate/20261104090100_add_x_to_notes.rb":   plain("AddXToNotes", "notes"),
		"db/migrate/20261104090200_ghost_users.rb": "class GhostUsers < ActiveRecord::Migration[7.1]\n  include GhostCommandGenerator[:production]\n\n" +
			"  def up\n    add_column :users, :x, :integer\n  end\nend\n",
	})
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v; want passed", res, err)
	}
	var scripts []string
	for _, s := range r.ran("bin/rails") {
		scripts = append(scripts, s.Script)
	}
	want := []string{
		"bin/rails db:migrate:up VERSION=20261104090000", "bin/rails db:migrate:down VERSION=20261104090000",
		"bin/rails db:migrate:up VERSION=20261104090100", "bin/rails db:migrate:down VERSION=20261104090100",
	}
	if !slices.Equal(scripts, want) {
		t.Fatalf("migration scripts %q, want %q", scripts, want)
	}
	if got := r.checksOf(task.ID, "migration"); !slices.Equal(got, []string{"pass", "pass", "pass", "pass", "skipped"}) {
		t.Fatalf("migration checks %q", got)
	}
	if len(res.Migrations) != 3 || !res.Migrations[2].Ghost || !slices.Equal(res.Migrations[2].Envs, []string{"production"}) {
		t.Fatalf("migrations %+v, want the three named, the gh-ost one with its environments", res.Migrations)
	}
	if report := r.readTask(task.ID, precheckFile); !strings.Contains(report, "20261104090200_ghost_users.rb") || !strings.Contains(report, "production") {
		t.Fatalf("%s does not name the gh-ost migration:\n%s", precheckFile, report)
	}

	// A down that fails fails the precheck, naming the migration.
	r.migrate = func(_, script string) int {
		if strings.Contains(script, "down VERSION=20261104090100") {
			return 1
		}
		return 0
	}
	res, err = r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !strings.Contains(res.Why, "20261104090100_add_x_to_notes.rb") {
		t.Fatalf("precheck %+v, %v; want a failure naming the migration whose down failed", res, err)
	}
}

// A precheck command the deny list refuses never runs: the precheck fails
// with the reason.
func TestAPrecheckCommandTheDenyListRefusesFailsWithTheReason(t *testing.T) {
	r := newCodingRig(t)
	r.cfg.Factory.Repos[0].Precheck.Lint = []string{"kubectl exec web-1 -- bin/lint {files}"}
	task := r.codingTask("PR-12367", "bug")
	r.fix(task)
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || !res.ForYou || !strings.Contains(res.Why, "kubectl exec") || !strings.Contains(res.Why, "precheck.lint") {
		t.Fatalf("precheck %+v, want it failed for you with the deny list's reason and the key", res)
	}
	if n := len(r.ran("kubectl")) + len(r.ran("bin/lint")); n != 0 {
		t.Fatalf("%d runs of the denied command", n)
	}
	var lint []registry.Check
	cs, _ := r.reg.Checks(r.ctx, task.ID)
	for _, c := range cs {
		if c.Kind == "lint" {
			lint = append(lint, c)
		}
	}
	if len(lint) != 1 || lint[0].Result != "fail" {
		t.Fatalf("lint checks %+v, want the denied one failed", lint)
	}
}
