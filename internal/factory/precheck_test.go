package factory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/dblock"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
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

// The cap stops a precheck that runs too long. In the lint, the first
// step, a stop at the cap is no note and no fix turn: a lint that takes the
// whole cap waits on something (a lock, a server). It names no failure of
// the code, so it runs again on a bound of its own, the cap's, then with
// that bound doubled; then the precheck fails for you, naming the command,
// that it hangs and what the factory tried, and the steps it kept from
// starting did not run.
func TestPrecheckStopsAtItsCap(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12365", "bug")
	r.fix(task)
	fire := make(chan time.Time)
	var mu sync.Mutex
	var timers []time.Duration
	r.e.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		timers = append(timers, d)
		mu.Unlock()
		return fire
	}
	r.blocks["bin/lint"] = make(chan struct{}) // never ends on its own
	r.printedErr["bin/lint"] = "rubocop: waiting for the \x1b[1mserver\x1b[0m lock\n"
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
	if res.Passed || !res.ForYou || res.fault == nil || !res.fault.hang ||
		res.Why != "precheck: precheck.lint `bin/lint app/credit.rb spec/models/credit_points_spec.rb` did not end within 21m: it hangs; "+
			"fix: find what it waits for in the copy (a lock, a server, a prompt); the factory ran it again and ran it with its bound doubled, 21m" {
		t.Fatalf("precheck %+v; want a failure for you: the lint hangs after its steps", res)
	}
	mu.Lock()
	if want := []time.Duration{10*time.Minute + 30*time.Second, 10*time.Minute + 30*time.Second, 21 * time.Minute}; !slices.Equal(timers, want) {
		t.Errorf("the timers %v, want the cap's of 10m30s (10m, and 15s for each of the 2 files), its own again, then doubled", timers)
	}
	mu.Unlock()
	if res.Unfinished != "the specs and the jest tests did not run within the cap of 10m30s" {
		t.Fatalf("unfinished %q, want the specs and jest, which did not run", res.Unfinished)
	}
	if got := r.checksOf(task.ID, "lint"); !slices.Equal(got, []string{registry.CheckSkipped, registry.CheckSkipped, registry.CheckSkipped}) {
		t.Fatalf("lint checks %q, want each stopped run skipped", got)
	}
	if report := r.readTask(task.ID, precheckFile); !strings.Contains(report, "on stderr (at most 20):\n\n```\nrubocop: waiting for the server lock\n```") {
		t.Fatalf("%s does not keep the hung lint's stderr, stripped:\n%s", precheckFile, report)
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

// The factory's own spec and migration commands take the copy's database
// lock, the one `magnum db-lock` takes for the reviewers of a local round,
// so the two never use the copy's databases at once: the lock is held while
// a spec or migration command runs and free while lint runs; a command
// waits while a reviewer holds it.
func TestThePrecheckTakesTheCopysDatabaseLock(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12379", "feature")
	r.fix(task)
	r.commit("PR-12379 a column", map[string]string{"db/migrate/20261104090000_add_x_to_credits.rb": "class AddXToCredits < ActiveRecord::Migration[7.1]\n" +
		"  def change\n    add_column :credits, :x, :integer\n  end\nend\n"})
	path := r.layout.DBLock(r.copyDir)
	held := map[string]bool{}
	var mu sync.Mutex
	r.setDuring(func(_ string, run scriptRun) {
		l, err := dblock.Acquire(context.Background(), path, dblock.Holder{Role: "probe", Command: "probe"}, 0, nil)
		mu.Lock()
		defer mu.Unlock()
		held[strings.Fields(run.Script)[0]] = err != nil
		if err == nil {
			l.Release()
		}
	})
	if res, err := r.e.precheck(r.ctx, task, r.cp); err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v; want passed", res, err)
	}
	mu.Lock()
	if !held["bin/specs"] || !held["bin/rails"] || held["bin/lint"] {
		t.Fatalf("the copy's database lock held while each ran: %v; want specs and migrations under it, lint not", held)
	}
	mu.Unlock()

	r.setDuring(nil)
	reviewer, err := dblock.Acquire(r.ctx, path, dblock.Holder{Role: "claude-review", Command: "bin/rspec spec/models/credit_spec.rb"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan PrecheckResult, 1)
	go func() {
		res, _ := r.e.precheck(r.ctx, task, r.cp)
		done <- res
	}()
	waitFor(t, "the precheck to wait for the copy's databases", func() bool {
		return strings.Contains(r.e.checkNote(task.ID), "waits for the copy's databases")
	})
	if n := len(r.ran("bin/specs")); n != 1 {
		t.Fatalf("%d spec runs while a reviewer holds the copy's databases, want the first precheck's only", n)
	}
	reviewer.Release()
	if res := <-done; !res.Passed {
		t.Fatalf("precheck %+v after the reviewer's command ended, want passed", res)
	}
}

// A lint that exits non-zero and names no offense by path:line needs you,
// and the ask carries the line of its stderr that says why it failed: the
// last one, past mise's own warnings, the stack's frames and the code
// frames of the branch's source, redacted (no token in it) and clipped.
// Its stderr says why, so no format hint stands in for the fix.
func TestALintCrashsAskNamesItsLastStderrLine(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12380", "bug")
	r.fix(task)
	r.lintCrash = func([]string) string {
		return "mise WARN Config files in /tmp/shop.factory1/.mise.toml are not trusted.\n" +
			"Error: .rubocop.yml names Style/Gone, which this rubocop lacks (token ghp_abcdefghijklmnopqrstuvwxyz0123456789)\n" +
			"/gems/rubocop/lib/rubocop/config.rb:12:in 'validate'\n" +
			"\tfrom /gems/rubocop/exe/rubocop:15:in '<main>'\n" +
			"> 3 | secret_source_line = 1\n" +
			"    |                     ^\n" +
			"mise WARN Config files in /tmp/shop.factory1/.mise.toml are not trusted.\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || !res.ForYou {
		t.Fatalf("precheck %+v, want a failure for you", res)
	}
	for _, want := range []string{"precheck.lint", "names no offense by path:line", "Style/Gone, which this rubocop lacks"} {
		if !strings.Contains(res.Why, want) {
			t.Errorf("the ask %q does not name %q", res.Why, want)
		}
	}
	for _, not := range []string{"mise WARN", "config.rb:12", "exe/rubocop", "ghp_abcdefghij", "secret_source_line", "rubocop --format emacs"} {
		if strings.Contains(res.Why, not) {
			t.Errorf("the ask %q names %q", res.Why, not)
		}
	}
}

// The line of stderr the ask carries is kept (the task's wait, its alert,
// precheck.md), so it is scrubbed as all such text, not only redacted: the
// values of a database's password in it are masked.
func TestALintCrashsAskKeepsNoSecretOfItsStderr(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12416", "bug")
	r.fix(task)
	r.lintCrash = func([]string) string {
		return "Error: cannot connect: PGPASSWORD=" + leakSecrets[8] + " mysql -uroot -p" + leakSecrets[7] + " app\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	report := r.readTask(task.ID, precheckFile)
	if !res.ForYou || !strings.Contains(res.Why, "PGPASSWORD=<redacted>") {
		t.Fatalf("precheck %+v; want a failure for you naming its stderr's line, scrubbed", res)
	}
	if got := leaks(res.Why + report); len(got) > 0 {
		t.Fatalf("the ask or %s holds %q: %q", precheckFile, got, res.Why)
	}
}

// A lint command that names its format already gets no hint to name one:
// the hint was wrong when rubocop crashed with `--format emacs` given.
func TestALintCrashsAskDropsTheFormatHintWhenTheCommandNamesOne(t *testing.T) {
	for _, lint := range []string{"bin/lint --format emacs {files}", "bin/lint --format=unix {files}", "bin/lint -f emacs {files}"} {
		r := newCodingRig(t)
		r.cfg.Factory.Repos[0].Precheck.Lint = []string{lint}
		task := r.codingTask("PR-12381", "bug")
		r.fix(task)
		r.lintCrash = func([]string) string { return "Error: something else entirely\n" }
		res, err := r.e.precheck(r.ctx, task, r.cp)
		if err != nil {
			t.Fatal(err)
		}
		if !res.ForYou || !strings.Contains(res.Why, "something else entirely") {
			t.Fatalf("%s: precheck %+v, want a failure for you naming its stderr", lint, res)
		}
		if strings.Contains(res.Why, "--format emacs, eslint") || strings.Contains(res.Why, "have it print") {
			t.Fatalf("%s: the ask %q hints at a format the command names already", lint, res.Why)
		}
	}
}

// leakSecrets are the values of leakProbe, command output that holds a
// secret in each shape execx.Redact leaves and scrub.Secrets masks (and
// scrub.Text with it): an env pair, a Rails secret, a Ruby hash, OpenAI,
// Stripe and AWS keys, a URL's password, mysql's -p and PGPASSWORD. The
// tokens are built in parts, so no scanner reads the source as holding one.
var leakSecrets = []string{"hunter2-env", "f3b1c9e8a7d6", "hunter2-hash", "sk-" + "proj-MADEUPMADEUPMADEUP1234",
	"sk_" + "test_MADEUPMADEUP123456", "AKIA" + "MADEUPMADEUPMADE", "mysqlpw-url", "mysqlpw-flag", "pgpw-env"}

var leakProbe = strings.Join([]string{
	"DB_PASSWORD=" + leakSecrets[0],
	`secret_key_base: "` + leakSecrets[1] + `"`,
	`{"password"=>"` + leakSecrets[2] + `"}`,
	"openai " + leakSecrets[3],
	"stripe " + leakSecrets[4],
	"aws " + leakSecrets[5],
	"DATABASE_URL=mysql2://app:" + leakSecrets[6] + "@db.example.com/app",
	"mysql -uroot -p" + leakSecrets[7] + " app",
	"PGPASSWORD=" + leakSecrets[8] + " psql -h db -U app",
}, "\n")

// leaks are the leakSecrets that text holds.
func leaks(text string) []string {
	var out []string
	for _, s := range leakSecrets {
		if strings.Contains(text, s) {
			out = append(out, s)
		}
	}
	return out
}

// precheck.md is kept, and the pr-body agent reads it into a PR on GitHub:
// what a failing lint, spec run, jest run or migration printed goes into it
// with its secrets masked (scrub.Secrets), not only redacted.
func TestPrecheckMdKeepsNoSecretACheckPrinted(t *testing.T) {
	for _, word := range []string{"bin/lint", "bin/specs", "bin/jest", "bin/rails"} {
		t.Run(strings.TrimPrefix(word, "bin/"), func(t *testing.T) {
			r := newCodingRig(t)
			task := r.codingTask("PR-12412", "feature")
			r.fix(task)
			if word == "bin/rails" {
				r.commit("PR-12412 a column", map[string]string{"db/migrate/20261111090000_add_x_to_credits.rb": "class AddXToCredits < " +
					"ActiveRecord::Migration[7.1]\n  def change\n    add_column :credits, :x, :integer\n  end\nend\n"})
			}
			r.adds = map[string]string{word: leakProbe}
			res, err := r.e.precheck(r.ctx, task, r.cp)
			if err != nil {
				t.Fatal(err)
			}
			report := r.readTask(task.ID, precheckFile)
			if res.Passed || !strings.Contains(report, "DB_PASSWORD=<redacted>") {
				t.Fatalf("precheck %+v; want it failed, %s naming the output:\n%s", res, precheckFile, report)
			}
			if got := leaks(report + res.Why); len(got) > 0 {
				t.Fatalf("%s or the precheck's why holds %q:\n%s\n%s", precheckFile, got, res.Why, report)
			}
		})
	}
}

// precheck.md is the writer's evidence of what failed (its fix turns name
// it): the output keeps what a failure prints for the writer to act on, an
// email, a host, an address and its port, and masks only the secrets. A
// spec that expected "a@example.com" and got "A@example.com" reads so, not
// "<email>" twice.
func TestPrecheckMdKeepsTheEmailsAndHostsAFailurePrints(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12417", "bug")
	r.fix(task)
	r.adds = map[string]string{"bin/specs": `expected "a@example.com", got "A@example.com"` + "\n" +
		"Mysql2::Error::ConnectionError: Can't connect to MySQL server on '127.0.0.1:3306'\n" +
		"Net::OpenTimeout: shop.example.com\n" +
		"DB_PASSWORD=" + leakSecrets[0]}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	report := r.readTask(task.ID, precheckFile)
	if res.Passed {
		t.Fatalf("precheck %+v, want the red specs to fail it", res)
	}
	for _, want := range []string{`expected "a@example.com", got "A@example.com"`, "'127.0.0.1:3306'", "shop.example.com",
		"DB_PASSWORD=<redacted>"} {
		if !strings.Contains(report, want) {
			t.Errorf("%s does not hold %q:\n%s", precheckFile, want, report)
		}
	}
	if got := leaks(report); len(got) > 0 {
		t.Fatalf("%s holds %q:\n%s", precheckFile, got, report)
	}
}

// The lint's offenses are read from the kept output, whose scrub keeps file
// names: a changed file named as a host is (webpack.config.dev.js, which a
// pass that masks hosts would read as <host>.js) keeps its name, and an
// offense on a changed line of it fails the precheck. What precheck.md
// names of it holds no secret.
func TestTheScrubbedLintOutputKeepsAFileNamedLikeAHost(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12414", "bug")
	r.fix(task)
	const file = "app/javascript/webpack.config.dev.js"
	r.commit("PR-12414 the dev config", map[string]string{file: "module.exports = {}\n"})
	r.lint = func([]string) string {
		return file + ":1:1: error: no-undef, DB_PASSWORD=" + leakSecrets[0] + " (eslint)\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || !strings.Contains(res.Why, "lint: 1 offense on the changed lines") {
		t.Fatalf("precheck %+v; want the offense on the changed line of %s to fail it", res, file)
	}
	report := r.readTask(task.ID, precheckFile)
	if !strings.Contains(report, file+":1:1: error: no-undef") || len(leaks(report)) > 0 {
		t.Fatalf("%s names the offense in %s %q, with the secret %q:\n%s", precheckFile, file, "no-undef", leaks(report), report)
	}
}

// factory-proof.json is kept, and the pr-body agent reads it too: the
// output of the proof's spec runs, on the base and at the head, has its
// secrets masked.
func TestFactoryProofJSONKeepsNoSecretASpecRunPrinted(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12413", "bug")
	r.fix(task)
	r.adds = map[string]string{"bin/specs": leakProbe}
	p, err := r.e.runProof(r.ctx, task, r.cp)
	if err != nil {
		t.Fatalf("runProof: %v", err)
	}
	saved := r.readTask(task.ID, factoryProofFile)
	var kept Proof
	if err := json.Unmarshal([]byte(saved), &kept); err != nil || !strings.Contains(kept.Base.Output, "PGPASSWORD=<redacted>") ||
		!strings.Contains(kept.Head.Output, "DB_PASSWORD=<redacted>") {
		t.Fatalf("%s (%v) keeps base %q, head %q; want both runs' output, scrubbed", factoryProofFile, err, kept.Base.Output,
			kept.Head.Output)
	}
	if got := leaks(saved + p.Base.Output + p.Head.Output); len(got) > 0 {
		t.Fatalf("%s holds %q:\n%s", factoryProofFile, got, saved)
	}
}

// failures reads a spec run's output after the scrub as it read it before:
// the same failing examples, in the same files and places, and the same
// keys at the head and on the base, so a spec red on the base too is still
// not the change's. An example's words keep their email, host and address,
// and a secret in them is masked alike in both runs.
func TestTheScrubbedSpecOutputNamesTheSameFailures(t *testing.T) {
	out := leakProbe + "\n\nFailed examples:\n\n" +
		"rspec ./spec/models/credit_spec.rb:12 # Credit mails alice@example.com from shop.example.com\n" +
		"rspec ./spec/models/user_spec.rb[1:2:3] # User with DB_PASSWORD=hunter2 logs in\n" +
		"rspec spec/requests/api_spec.rb:40 # API answers 10.0.4.12 with a token: abc\n"
	raw := failures(out)
	head := failures(outputOf(execx.Result{Stdout: []byte(out)}))
	base := failures(outputOf(execx.Result{Stdout: []byte(out)}))
	if len(raw) != 3 || len(head) != len(raw) {
		t.Fatalf("the scrubbed output names %d failures, the raw %d; want the 3 of the raw: %+v", len(head), len(raw), head)
	}
	place := func(f specFailure) string { return strings.Fields(f.line)[1] }
	for i := range raw {
		if head[i].file != raw[i].file || place(head[i]) != place(raw[i]) || head[i].key != base[i].key {
			t.Errorf("failure %d: scrubbed %+v, raw %+v, base %+v; want its file and place, and one key at the head and on the base",
				i, head[i], raw[i], base[i])
		}
	}
	lines := failureLines(head)
	if !strings.Contains(lines, "mails alice@example.com from shop.example.com") || !strings.Contains(lines, "answers 10.0.4.12") ||
		strings.Contains(lines, "DB_PASSWORD=hunter2") {
		t.Fatalf("the failing examples' lines read:\n%s\nwant their email, host and address as printed, the password masked", lines)
	}
}
