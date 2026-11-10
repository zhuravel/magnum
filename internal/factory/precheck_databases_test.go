package factory

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The migrations commands of a multi-database application: one database's
// task each ({database}), up, down and up again.
var databaseMigrations = []string{
	"bin/rails db:migrate:up:{database} VERSION={version}",
	"bin/rails db:migrate:down:{database} VERSION={version}",
	"bin/rails db:migrate:up:{database} VERSION={version}",
}

// multiDatabase sets the rig's repository up as a multi-database
// application: the primary database's folder, the reporting one's and the
// shards', each with its databases, and databaseMigrations.
func (r *codingRig) multiDatabase() {
	pc := &r.cfg.Factory.Repos[0].Precheck
	pc.Migrations = databaseMigrations
	pc.MigrationDatabases = map[string][]string{
		"db/migrate":           {"primary"},
		"db/reporting_migrate": {"reporting"},
		"db/sharded_migrate":   {"shard_1", "shard_1001"},
	}
}

// migrationText is a plain migration of class that adds a column to table.
func migrationText(class, table string) string {
	return "class " + class + " < ActiveRecord::Migration[7.1]\n  def change\n    add_column :" + table + ", :x, :integer\n  end\nend\n"
}

// railsScripts are the bin/rails scripts the rig ran, in order.
func (r *codingRig) railsScripts() []string {
	var out []string
	for _, s := range r.ran("bin/rails") {
		out = append(out, s.Script)
	}
	return out
}

// migrationCommands are the commands of the task's migration checks, in
// order.
func (r *codingRig) migrationCommands(id int64) []string {
	r.t.Helper()
	cs, err := r.reg.Checks(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		if c.Kind == registry.CheckMigration {
			out = append(out, c.Command)
		}
	}
	return out
}

// A migration of the shards' folder runs up, down and up on each shard its
// folder names, with {database} filled in, the shards in their order and
// each shard's three commands before the next shard's; its checks and
// precheck.md name the database of each run.
func TestAShardMigrationRunsUpDownAndUpOnEachShard(t *testing.T) {
	r := newCodingRig(t)
	r.multiDatabase()
	task := r.codingTask("PR-12480", "feature")
	r.commit("PR-12480 an index on the shards", map[string]string{
		"db/sharded_migrate/20261111090000_add_x_to_events.rb": migrationText("AddXToEvents", "events"),
	})
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v; want passed", res, err)
	}
	want := []string{
		"bin/rails db:migrate:up:shard_1 VERSION=20261111090000", "bin/rails db:migrate:down:shard_1 VERSION=20261111090000",
		"bin/rails db:migrate:up:shard_1 VERSION=20261111090000",
		"bin/rails db:migrate:up:shard_1001 VERSION=20261111090000", "bin/rails db:migrate:down:shard_1001 VERSION=20261111090000",
		"bin/rails db:migrate:up:shard_1001 VERSION=20261111090000",
	}
	if got := r.railsScripts(); !slices.Equal(got, want) {
		t.Fatalf("migration scripts %q, want %q", got, want)
	}
	if got := r.migrationCommands(task.ID); !slices.Equal(got, want) {
		t.Fatalf("migration checks of %q, want one for each run, naming its database: %q", got, want)
	}
	if got := r.checksOf(task.ID, registry.CheckMigration); !slices.Equal(got, []string{"pass", "pass", "pass", "pass", "pass", "pass"}) {
		t.Fatalf("migration checks %q, want six passed", got)
	}
	report := r.readTask(task.ID, precheckFile)
	for _, db := range []string{"shard_1", "shard_1001"} {
		if !strings.Contains(report, "db/sharded_migrate/20261111090000_add_x_to_events.rb on "+db+": passed") {
			t.Errorf("%s does not name the run on %s:\n%s", precheckFile, db, report)
		}
	}

	// A down that fails on the first shard fails the precheck naming the
	// migration and the shard; the next shard still runs.
	r.mu.Lock()
	r.scripts = nil
	r.mu.Unlock()
	r.migrate = func(_, script string) int {
		if strings.Contains(script, "down:shard_1 ") {
			return 1
		}
		return 0
	}
	res, err = r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !strings.Contains(res.Why, "db/sharded_migrate/20261111090000_add_x_to_events.rb on shard_1:") {
		t.Fatalf("precheck %+v, %v; want a failure naming the migration and the shard whose down failed", res, err)
	}
	want = []string{
		"bin/rails db:migrate:up:shard_1 VERSION=20261111090000", "bin/rails db:migrate:down:shard_1 VERSION=20261111090000",
		"bin/rails db:migrate:up:shard_1001 VERSION=20261111090000", "bin/rails db:migrate:down:shard_1001 VERSION=20261111090000",
		"bin/rails db:migrate:up:shard_1001 VERSION=20261111090000",
	}
	if got := r.railsScripts(); !slices.Equal(got, want) {
		t.Fatalf("migration scripts %q, want shard_1's to stop at its failed down and shard_1001's to run: %q", got, want)
	}
}

// A migration of the reporting folder runs on the reporting database only,
// and one of db/migrate on the primary only.
func TestAReportingMigrationRunsOnReportingOnly(t *testing.T) {
	r := newCodingRig(t)
	r.multiDatabase()
	task := r.codingTask("PR-12481", "feature")
	r.commit("PR-12481 columns", map[string]string{
		"db/migrate/20261111090000_add_x_to_credits.rb":           migrationText("AddXToCredits", "credits"),
		"db/reporting_migrate/20261111090100_add_x_to_reports.rb": migrationText("AddXToReports", "reports"),
	})
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v; want passed", res, err)
	}
	want := []string{
		"bin/rails db:migrate:up:primary VERSION=20261111090000", "bin/rails db:migrate:down:primary VERSION=20261111090000",
		"bin/rails db:migrate:up:primary VERSION=20261111090000",
		"bin/rails db:migrate:up:reporting VERSION=20261111090100", "bin/rails db:migrate:down:reporting VERSION=20261111090100",
		"bin/rails db:migrate:up:reporting VERSION=20261111090100",
	}
	if got := r.railsScripts(); !slices.Equal(got, want) {
		t.Fatalf("migration scripts %q, want %q", got, want)
	}
	if len(res.Migrations) != 2 || res.Migrations[1].Path != "db/reporting_migrate/20261111090100_add_x_to_reports.rb" ||
		!slices.Equal(res.Migrations[1].Tables, []string{"reports"}) {
		t.Fatalf("migrations %+v, want both named with their flags", res.Migrations)
	}
}

// A migration of a folder with no databases is not run, and never in
// silence: precheck.md says why, naming its folder, its check is skipped,
// the precheck passes, the ready card's FLAGS line lists it as a migration
// with the same words, and the PR body's Known list names it in the
// factory's own sentence.
func TestAMigrationOfAFolderWithNoDatabasesIsNotRunAndNamed(t *testing.T) {
	r := newCodingRig(t)
	r.multiDatabase()
	task := r.codingTask("PR-12482", "feature")
	const backfill = "db/data_migrate/20261111090000_backfill_points.rb"
	head := r.commit("PR-12482 a backfill", map[string]string{
		backfill: "class BackfillPoints < ActiveRecord::Migration[7.1]\n  def up\n    execute 'UPDATE credits SET x = 1'\n  end\n\n" +
			"  def down\n  end\nend\n",
		"db/migrate/20261111090100_add_x_to_credits.rb": migrationText("AddXToCredits", "credits"),
	})
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v; want passed: a migration it does not run fails nothing", res, err)
	}
	for _, s := range r.railsScripts() {
		if strings.Contains(s, "20261111090000") {
			t.Fatalf("migration scripts %q run the backfill, whose folder has no databases", r.railsScripts())
		}
	}
	if n := len(r.railsScripts()); n != 3 {
		t.Fatalf("migration scripts %q, want db/migrate's three on primary", r.railsScripts())
	}
	if got := r.checksOf(task.ID, registry.CheckMigration); !slices.Equal(got, []string{"skipped", "pass", "pass", "pass"}) {
		t.Fatalf("migration checks %q, want the backfill skipped, then the primary's three", got)
	}
	const words = "not run: no databases are set for db/data_migrate"
	if report := r.readTask(task.ID, precheckFile); !strings.Contains(report, backfill) || !strings.Contains(report, words) {
		t.Fatalf("%s does not say the backfill is not run, and why:\n%s", precheckFile, report)
	}

	f, err := r.e.flags(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(f.Lines(), func(l string) bool { return strings.HasPrefix(l, "migration "+backfill) && strings.Contains(l, words) }) {
		t.Fatalf("FLAGS %q, want the backfill listed as a migration, not run", f.Lines())
	}

	r.taskFile(task.ID, prBodyFile, "## Summary\n\nThe change.\n")
	task.HeadSHA = head
	if err := r.e.appendKnown(r.ctx, task); err != nil {
		t.Fatal(err)
	}
	const sentence = "The factory's precheck did not run 1 migration, as no databases are set for db/data_migrate: `" + backfill + "`."
	if body := r.readTask(task.ID, prBodyFile); strings.Count(body, sentence) != 1 {
		t.Fatalf("pr-body.md does not name the migration it did not run once:\n%s", body)
	}
	task.HeadSHA = "0123456789abcdef0123456789abcdef01234567" // another head
	if err := r.e.appendKnown(r.ctx, task); err != nil {
		t.Fatal(err)
	}
	if body := r.readTask(task.ID, prBodyFile); strings.Contains(body, "did not run") {
		t.Fatalf("pr-body.md names the migrations of another head's precheck:\n%s", body)
	}
}

// Without migration_databases only db/migrate's migrations run, with the
// commands as they are (no database); a migration of another folder is
// listed and not run, naming its folder.
func TestWithoutMigrationDatabasesOnlyDbMigrateRuns(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12483", "feature")
	r.commit("PR-12483 columns", map[string]string{
		"db/migrate/20261111090000_add_x_to_credits.rb":         migrationText("AddXToCredits", "credits"),
		"db/sharded_migrate/20261111090100_add_x_to_events.rb":  migrationText("AddXToEvents", "events"),
		"engines/billing/db/migrate/20261111090200_add_x.rb":    migrationText("AddX", "invoices"),
		"db/migrate/archive/20261111090300_add_y_to_credits.rb": migrationText("AddYToCredits", "credits"),
	})
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v; want passed", res, err)
	}
	want := []string{
		"bin/rails db:migrate:up VERSION=20261111090000", "bin/rails db:migrate:down VERSION=20261111090000",
		"bin/rails db:migrate:up VERSION=20261111090300", "bin/rails db:migrate:down VERSION=20261111090300",
	}
	if got := r.railsScripts(); !slices.Equal(got, want) {
		t.Fatalf("migration scripts %q, want db/migrate's only, as before: %q", got, want)
	}
	if len(res.Migrations) != 4 {
		t.Fatalf("migrations %+v, want all four named", res.Migrations)
	}
	report := r.readTask(task.ID, precheckFile)
	for _, words := range []string{"not run: no databases are set for db/sharded_migrate", "not run: no databases are set for engines/billing/db/migrate"} {
		if !strings.Contains(report, words) {
			t.Errorf("%s does not say %q:\n%s", precheckFile, words, report)
		}
	}
	if rec := r.readPrecheckRecord(task.ID); !slices.Equal(rec.MigrationsNotRun,
		[]string{"db/sharded_migrate/20261111090100_add_x_to_events.rb", "engines/billing/db/migrate/20261111090200_add_x.rb"}) {
		t.Fatalf("precheck.json records %q as not run, want the shard's and the engine's", rec.MigrationsNotRun)
	}
}

// The FLAGS line lists a migration the branch adds in every migration
// folder (a db directory's child whose name holds "migrate", the root's or
// an engine's), and no other file under db.
func TestFlagsListAMigrationOfEveryFolder(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12484", "feature")
	migrations := []string{
		"db/data_migrate/20261111090000_backfill.rb",
		"db/migrate/20261111090100_add_x_to_credits.rb",
		"db/reporting_migrate/20261111090200_add_x_to_reports.rb",
		"db/sharded_migrate/20261111090300_add_x_to_events.rb",
		"engines/billing/db/migrate/20261111090400_add_x_to_invoices.rb",
	}
	files := map[string]string{
		"db/seeds.rb":            "Credit.create!\n",
		"db/migrate/README.md":   "how to migrate\n",
		"db/reporting_schema.rb": "ActiveRecord::Schema.define(version: 2026_11_11_090200) do\nend\n",
	}
	for _, m := range migrations {
		files[m] = migrationText("AddX", "credits")
	}
	r.commit("PR-12484 migrations everywhere", files)
	f, err := r.e.flags(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range f.Migrations {
		got = append(got, m.Path)
	}
	if !slices.Equal(got, migrations) {
		t.Fatalf("FLAGS migrations %q, want one of each folder: %q", got, migrations)
	}
	lines := f.Lines()
	for _, m := range migrations {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "migration "+m) }) {
			t.Errorf("FLAGS %q do not list %s", lines, m)
		}
	}
}

// What the precheck runs with counts migration_databases: a retry after you
// changed a folder's databases is no retry that changed nothing. A
// repository without the key keeps the print it had (its commands alone).
func TestTheChecksPrintCountsMigrationDatabases(t *testing.T) {
	h := newHarness(t)
	pc := config.FactoryPrecheck{Lint: []string{"bin/lint {files}"}, Migrations: databaseMigrations,
		MigrationDatabases: map[string][]string{"db/sharded_migrate": {"shard_1"}}}
	before, _ := h.e.checksPrint(config.FactoryRepo{Precheck: pc})
	pc.MigrationDatabases = map[string][]string{"db/sharded_migrate": {"shard_1", "shard_1001"}}
	if after, _ := h.e.checksPrint(config.FactoryRepo{Precheck: pc}); after == before {
		t.Fatal("the commands' print is the same after a shard joined migration_databases")
	}
	pc.MigrationDatabases = nil
	got, _ := h.e.checksPrint(config.FactoryRepo{Precheck: pc})
	if want := sha256Hex([]byte(strings.Join(append([]string{"bin/lint {files}", "", ""}, databaseMigrations...), "\x00"))); got != want {
		t.Fatalf("the commands' print without migration_databases is %s, want the commands' alone, %s", got, want)
	}
}
