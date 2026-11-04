package factory

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// migrationRepo is a repository whose migrations name their gh-ost
// environments with a generator, and whose table sizes are in a stats file.
func migrationRepo() config.FactoryRepo {
	return config.FactoryRepo{Repo: "example/shop", GhostMarker: "GhostCommandGenerator", TableStats: "config/table_stats.json",
		LargeTableRows: 10_000_000}
}

// writeMigrations writes the stats file and each migration (name -> its
// text) into a copy, and returns the copy and the migrations' paths.
func writeMigrations(t *testing.T, stats string, files map[string]string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, text string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config/table_stats.json", stats)
	var paths []string
	for _, name := range slices.Sorted(maps.Keys(files)) {
		rel := "db/migrate/" + name
		write(rel, files[name])
		paths = append(paths, rel)
	}
	return dir, paths
}

const tableStats = `{"credits": {"rows": 25000000, "data_mb": 9000}, "notes": 1200, "users": {"table_rows": 40000000}}`

func scan(t *testing.T, stats string, files map[string]string) map[string]MigrationFlag {
	t.Helper()
	dir, paths := writeMigrations(t, stats, files)
	flags, err := ScanMigrations(dir, paths, migrationRepo())
	if err != nil {
		t.Fatalf("ScanMigrations: %v", err)
	}
	if len(flags) != len(paths) {
		t.Fatalf("ScanMigrations named %d of %d migrations: %+v", len(flags), len(paths), flags)
	}
	out := map[string]MigrationFlag{}
	for _, f := range flags {
		out[strings.TrimSuffix(filepath.Base(f.Path), ".rb")] = f
	}
	return out
}

// A migration that includes the generator names the environments where a
// person changes the table with gh-ost; its down refuses by the generator's
// design, which is no irreversibility of the change.
func TestScanMigrationsNamesTheGhostEnvironments(t *testing.T) {
	flags := scan(t, tableStats, map[string]string{
		"20261104090000_add_points_to_credits.rb": `class AddPointsToCredits < ActiveRecord::Migration[7.1]
  include GhostCommandGenerator[:production, :bastion]

  def up
    add_column :credits, :points, :integer
  end

  def down
    raise ActiveRecord::IrreversibleMigration
  end
end
`,
		"20261104090100_add_flag_to_users.rb": `class AddFlagToUsers < ActiveRecord::Migration[7.1]
  include GhostCommandGenerator[%i[production]]

  def change
    add_column :users, :flag, :boolean
  end
end
`,
		"20261104090200_add_title_to_notes.rb": `class AddTitleToNotes < ActiveRecord::Migration[7.1]
  def change
    add_column :notes, :title, :string
  end
end
`,
	})
	credits := flags["20261104090000_add_points_to_credits"]
	if !credits.Ghost || !slices.Equal(credits.Envs, []string{"production", "bastion"}) || !slices.Equal(credits.Tables, []string{"credits"}) ||
		credits.Irreversible || credits.TwoTablesInGhost || credits.Path != "db/migrate/20261104090000_add_points_to_credits.rb" {
		t.Errorf("the gh-ost migration of credits: %+v", credits)
	}
	if len(credits.Problems()) != 0 {
		t.Errorf("a gh-ost migration of one large table has problems: %q", credits.Problems())
	}
	if users := flags["20261104090100_add_flag_to_users"]; !users.Ghost || !slices.Equal(users.Envs, []string{"production"}) {
		t.Errorf("the %%i[] form: %+v", users)
	}
	if notes := flags["20261104090200_add_title_to_notes"]; notes.Ghost || len(notes.Envs) != 0 || !slices.Equal(notes.Tables, []string{"notes"}) {
		t.Errorf("a plain migration: %+v", notes)
	}

	// No marker configured: nothing is a gh-ost migration.
	dir, paths := writeMigrations(t, tableStats, map[string]string{"20261104090000_x.rb": "include GhostCommandGenerator[:production]\n"})
	repo := migrationRepo()
	repo.GhostMarker = ""
	got, err := ScanMigrations(dir, paths, repo)
	if err != nil || len(got) != 1 || got[0].Ghost {
		t.Fatalf("without ghost_marker: %+v, %v", got, err)
	}
}

// A change to a table at or over large_table_rows needs gh-ost or an online
// algorithm: a migration with neither is flagged, one with either is not,
// and a small table needs neither.
func TestScanMigrationsFlagsALargeTableWithoutGhostOrAnOnlineAlgorithm(t *testing.T) {
	flags := scan(t, tableStats, map[string]string{
		"20261104090000_index_credits.rb": `class IndexCredits < ActiveRecord::Migration[7.1]
  def change
    add_index :credits, :points
  end
end
`,
		"20261104090100_index_credits_inplace.rb": `class IndexCreditsInplace < ActiveRecord::Migration[7.1]
  def change
    add_index :credits, :points, algorithm: :inplace
  end
end
`,
		"20261104090200_alter_users_instant.rb": `class AlterUsersInstant < ActiveRecord::Migration[7.1]
  def up
    execute "ALTER TABLE ` + "`users`" + ` ADD COLUMN x INT, ALGORITHM=INSTANT"
  end

  def down
    execute "ALTER TABLE users DROP COLUMN x"
  end
end
`,
		"20261104090300_add_title_to_notes.rb": `class AddTitleToNotes < ActiveRecord::Migration[7.1]
  def change
    add_column :notes, :title, :string
  end
end
`,
	})
	index := flags["20261104090000_index_credits"]
	if !slices.Equal(index.Large, []string{"credits"}) || index.Online || index.Ghost {
		t.Fatalf("an index on credits: %+v", index)
	}
	if ps := index.Problems(); len(ps) != 1 || !strings.Contains(ps[0], "credits") || !strings.Contains(ps[0], "25000000 rows") ||
		!strings.Contains(ps[0], "gh-ost") || !strings.Contains(ps[0], "online algorithm") {
		t.Fatalf("the problems of an index on credits: %q", ps)
	}
	if inplace := flags["20261104090100_index_credits_inplace"]; !inplace.Online || len(inplace.Problems()) != 0 {
		t.Errorf("algorithm: :inplace on credits: %+v, %q", inplace, inplace.Problems())
	}
	if instant := flags["20261104090200_alter_users_instant"]; !instant.Online || !slices.Equal(instant.Tables, []string{"users"}) ||
		!slices.Equal(instant.Large, []string{"users"}) || len(instant.Problems()) != 0 {
		t.Errorf("ALGORITHM=INSTANT on users: %+v, %q", instant, instant.Problems())
	}
	if notes := flags["20261104090300_add_title_to_notes"]; len(notes.Large) != 0 || len(notes.Problems()) != 0 {
		t.Errorf("a small table: %+v, %q", notes, notes.Problems())
	}

	// A stats file that is not there, or not JSON, is an error: the sizes
	// cannot be told.
	dir, paths := writeMigrations(t, "not json", map[string]string{"20261104090000_x.rb": "add_index :credits, :points\n"})
	if _, err := ScanMigrations(dir, paths, migrationRepo()); err == nil || !strings.Contains(err.Error(), "config/table_stats.json") {
		t.Fatalf("a stats file that is not JSON: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "config/table_stats.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanMigrations(dir, paths, migrationRepo()); err == nil {
		t.Fatal("a missing stats file passed")
	}
}

// A gh-ost migration changes one table only (spec 8.4).
func TestScanMigrationsFlagsTwoTablesInOneGhostMigration(t *testing.T) {
	flags := scan(t, tableStats, map[string]string{
		"20261104090000_points_everywhere.rb": `class PointsEverywhere < ActiveRecord::Migration[7.1]
  include GhostCommandGenerator[:production]

  def up
    add_column :credits, :points, :integer
    add_column "users", :points, :integer
  end
end
`,
	})
	f := flags["20261104090000_points_everywhere"]
	if !f.TwoTablesInGhost || !slices.Equal(f.Tables, []string{"credits", "users"}) {
		t.Fatalf("a gh-ost migration of two tables: %+v", f)
	}
	if ps := f.Problems(); len(ps) != 1 || !strings.Contains(ps[0], "two tables") || !strings.Contains(ps[0], "credits, users") {
		t.Fatalf("its problems: %q", ps)
	}
}

// A migration without a way back is flagged: only an up, a down that
// raises, or a change whose steps Rails cannot reverse.
func TestScanMigrationsFlagsAnIrreversibleMigration(t *testing.T) {
	flags := scan(t, tableStats, map[string]string{
		"20261104090000_only_up.rb": `class OnlyUp < ActiveRecord::Migration[7.1]
  def up
    add_column :notes, :body, :text
  end
end
`,
		"20261104090100_down_raises.rb": `class DownRaises < ActiveRecord::Migration[7.1]
  def up
    remove_column :notes, :body
  end

  def down
    raise ActiveRecord::IrreversibleMigration
  end
end
`,
		"20261104090200_change_execute.rb": `class ChangeExecute < ActiveRecord::Migration[7.1]
  def change
    execute "UPDATE notes SET body = ''"
  end
end
`,
		"20261104090300_reversible.rb": `class Reversible < ActiveRecord::Migration[7.1]
  def change
    add_column :notes, :body, :text
    reversible do |dir|
      dir.up { execute "UPDATE notes SET body = ''" }
    end
  end
end
`,
		"20261104090400_up_and_down.rb": `class UpAndDown < ActiveRecord::Migration[7.1]
  def up
    add_column :notes, :body, :text
  end

  def down
    remove_column :notes, :body
  end
end
`,
	})
	for _, name := range []string{"20261104090000_only_up", "20261104090100_down_raises", "20261104090200_change_execute"} {
		f := flags[name]
		if !f.Irreversible || !slices.ContainsFunc(f.Problems(), func(p string) bool { return strings.Contains(p, "no way back") }) {
			t.Errorf("%s: %+v, %q; want irreversible", name, f, f.Problems())
		}
	}
	for _, name := range []string{"20261104090300_reversible", "20261104090400_up_and_down"} {
		if f := flags[name]; f.Irreversible || len(f.Problems()) != 0 {
			t.Errorf("%s: %+v, %q; want reversible", name, f, f.Problems())
		}
	}
}
