package staging

import (
	"slices"
	"testing"
)

// A migration is any file a merge adds under a db/<...migrate...> directory,
// at the root or under an engine: Jenkins runs the sharded and the data
// migrations as it runs db/migrate's, so the deploy's y/N must name them and
// a merge that now brings one must be refused. talkable#12203 adds
// db/sharded_migrate/20261105011921_add_metric_indexes_to_custom_activities.rb,
// which a db/migrate-only rule missed.
func TestAMigrationIsAnyFileUnderADbMigrateDirectory(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"db/migrate/20261108_add_flag.rb", true},
		{"db/sharded_migrate/20261105011921_add_metric_indexes_to_custom_activities.rb", true},
		{"db/data_migrate/20261101_backfill.rb", true},
		{"engines/billing/db/migrate/20261101_create_invoices.rb", true},
		{"engines/billing/db/sharded_migrate/20261101_index.rb", true},
		{"db/schema.rb", false},
		{"db/structure.sql", false},
		{"db/seeds.rb", false},
		{"db/migrate", false},
		{"app/models/migrate/thing.rb", false},
		{"lib/tasks/db_migrate.rake", false},
		{"spec/db/migrate_spec.rb", false},
	} {
		if got := isMigration(tc.path); got != tc.want {
			t.Errorf("isMigration(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// The plan of a merge that adds only a sharded migration names it, so the
// deploy's y/N shows it and a deploy whose question did not name it is
// refused (the plan's Migrations feed both).
func TestThePlanNamesAShardedMigration(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	p := w.plan(c, 9)
	if len(p.Envs) != 1 || !p.Envs[0].Clean {
		t.Fatalf("plan: %+v, want one clean env", p.Envs)
	}
	if want := []string{"db/sharded_migrate/20261105011921_add_metric_indexes.rb"}; !slices.Equal(p.Envs[0].Migrations, want) {
		t.Fatalf("migrations: %q, want %q", p.Envs[0].Migrations, want)
	}
}
