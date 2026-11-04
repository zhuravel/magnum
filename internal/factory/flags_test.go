package factory

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// planReport is a report.md whose Files section names files.
func planReport(files ...string) string {
	return "## Architecture decisions\n\nnone\n\n## Helpers\n\nnone\n\n## Files\n\n- " + strings.Join(files, "\n- ") + "\n"
}

// The FLAGS line names the migrations the branch adds, the files of
// [factory] sensitive it changes (Gemfile.lock, a workflow, a config file)
// and the files the plan did not name; a directory of the plan names the
// files under it. The sensitive files are the configuration's.
func TestFlagsNameMigrationsLockfilesWorkflowsAndFilesOutsideThePlan(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12370", "bug")
	const migration = "db/migrate/20261104090000_add_points_to_credits.rb"
	r.commit("PR-12370 copies the reward rules", map[string]string{
		"app/credit.rb":                     strings.Replace(creditRB, "    0\n", "    10\n", 1),
		"spec/models/credit_points_spec.rb": "expect: 10 in app/credit.rb\n",
		"spec/factories/reward_rules.rb":    "factory :reward_rule\n",
		"Gemfile.lock":                      "GEM\n  specs:\n",
		".github/workflows/ci.yml":          "name: CI\non: [pull_request]\njobs:\n  lint:\n    steps:\n      - run: bin/lint .\n",
		"config/settings.yml":               "points: 25\n",
		migration:                           "class AddPointsToCredits < ActiveRecord::Migration[7.1]\n  def change\n    add_column :credits, :points, :integer\n  end\nend\n",
	})
	r.taskFile(task.ID, reportFile, planReport("app/credit.rb", migration, "spec/models/"))

	f, err := r.e.flags(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Migrations) != 1 || f.Migrations[0].Path != migration || !slices.Equal(f.Migrations[0].Tables, []string{"credits"}) {
		t.Errorf("migrations %+v, want %s on credits", f.Migrations, migration)
	}
	if want := []string{".github/workflows/ci.yml", "Gemfile.lock", "config/settings.yml"}; !slices.Equal(f.Sensitive, want) {
		t.Errorf("sensitive %q, want %q", f.Sensitive, want)
	}
	if want := []string{".github/workflows/ci.yml", "Gemfile.lock", "config/settings.yml", "spec/factories/reward_rules.rb"}; !slices.Equal(f.NotInPlan, want) {
		t.Errorf("not in the plan %q, want %q", f.NotInPlan, want)
	}
	if len(f.Conflicts) != 0 {
		t.Errorf("conflicts %q with a base that did not move", f.Conflicts)
	}
	lines := strings.Join(f.Lines(), "\n")
	for _, want := range []string{"migration " + migration, "sensitive: .github/workflows/ci.yml, Gemfile.lock", "not in the plan: ", "reward_rules.rb"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the FLAGS lines lack %q:\n%s", want, lines)
		}
	}

	r.cfg.Factory.Sensitive = []string{"db/**"}
	if f, err := r.e.flags(r.ctx, task, r.cp); err != nil || !slices.Equal(f.Sensitive, []string{migration}) {
		t.Fatalf("with sensitive = [\"db/**\"] the sensitive files are %q (%v), want the migration", f.Sensitive, err)
	}
}

// commitOld commits path (text) on master in the work clone, authored and
// committed at at, and pushes it.
func (r *codingRig) commitOld(path, text string, at time.Time) {
	r.t.Helper()
	r.write(r.work, path, text)
	r.git(r.work, "add", "--", path)
	stamp := at.Format(time.RFC3339)
	if _, err := r.real.Run(r.ctx, execx.Cmd{Name: "git", Args: []string{"-C", r.work, "commit", "--quiet", "-m", "guard"},
		Env: map[string]string{"GIT_AUTHOR_DATE": stamp, "GIT_COMMITTER_DATE": stamp}}); err != nil {
		r.t.Fatal(err)
	}
	r.git(r.work, "push", "--quiet", "origin", "master")
	r.git(r.main, "fetch", "--quiet", "origin")
}

// A guard the change rewrites is flagged when the base had its line for
// more than 6 months, with the reason the history agent found: a younger
// guard, a guard the change leaves as it is, one in a file the change does
// not touch and a line a findings file names without a why are not.
func TestFlagsNameAnOldGuardWithItsReason(t *testing.T) {
	r := newCodingRig(t)
	guarded := "class Credit\n  def points\n    return 0 if refunded?\n    return 0 if frozen?\n    0\n  end\nend\n"
	r.commitOld("app/credit.rb", guarded, time.Date(2023, 5, 1, 10, 0, 0, 0, time.UTC))
	recent := strings.Replace(guarded, "frozen?", "archived?", 1)
	r.commitOld("app/credit.rb", recent, t0.AddDate(0, -1, 0))
	task := r.codingTask("PR-12371", "bug")
	r.commit("PR-12371 counts partial refunds", map[string]string{
		"app/credit.rb": strings.NewReplacer("return 0 if refunded?", "return 0 if refunded? && !partial?",
			"return 0 if archived?", "return 0 if archived? || frozen?").Replace(recent),
	})
	r.taskFile(task.ID, findingsFile("fx-history"), "# History\n\n"+
		"- `app/credit.rb:3` · a1b2c3d 2023-05-01 · why: a refund once paid its points twice · breaks: refunds count again\n"+
		"- `app/credit.rb:4` · d4e5f6a 2026-10-04 · why: archived campaigns stopped counting · breaks: archived ones count\n"+
		"- `app/models/user.rb:10` · 0a0b0c0 2022-01-01 · why: users without an email · breaks: the sign-up\n")
	r.taskFile(task.ID, findingsFile("fx-cause"), "# Cause\n\n- `app/credit.rb:5` sums the points without the refunds\n")

	f, err := r.e.flags(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.OldGuards) != 1 {
		t.Fatalf("old guards %+v, want the refund guard alone", f.OldGuards)
	}
	g := f.OldGuards[0]
	if g.Path != "app/credit.rb" || g.Line != 3 || g.Why != "a refund once paid its points twice" || g.Age < 365*24*time.Hour {
		t.Fatalf("old guard %+v, want app/credit.rb:3 of 2023 with the history's why", g)
	}
	if lines := strings.Join(f.Lines(), "\n"); !strings.Contains(lines, "an old guard at app/credit.rb:3 (3 years): a refund once paid its points twice") {
		t.Fatalf("the FLAGS lines:\n%s", lines)
	}
}

// The CI jobs that did not run locally are the jobs of the workflows that
// run on a pull request whose commands the precheck did not run: a job
// that runs the precheck's lint or jest is covered, the full rspec suite
// and a docker build are not, and a workflow of pushes to master only is
// no PR's.
func TestFlagsNameTheCIJobsNotRunLocally(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12372", "bug")
	r.commit("PR-12372 counts the points", map[string]string{
		"app/credit.rb": strings.Replace(creditRB, "    0\n", "    10\n", 1),
		".github/workflows/ci.yml": `name: CI
on:
  pull_request:
  push:
    branches: [master]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Lint
        run: bin/lint app spec
  rspec:
    name: RSpec
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: |
          bundle exec rails db:create
          bundle exec rspec
  jest:
    runs-on: ubuntu-latest
    steps:
      - run: bin/jest --ci
  docker:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/build-push-action@v6
`,
		".github/workflows/deploy.yaml": "name: Deploy\non:\n  push:\n    branches: [master]\njobs:\n  deploy:\n    steps:\n      - run: bin/deploy\n",
	})
	r.taskFile(task.ID, reportFile, planReport("app/credit.rb"))

	f, err := r.e.flags(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ci: RSpec", "ci: docker"}; !slices.Equal(f.CIJobsNotRun, want) {
		t.Fatalf("CI jobs not run locally %q, want %q", f.CIJobsNotRun, want)
	}
	if lines := strings.Join(f.Lines(), "\n"); !strings.Contains(lines, "CI jobs not run here: ci: RSpec, ci: docker") {
		t.Fatalf("the FLAGS lines:\n%s", lines)
	}
}
