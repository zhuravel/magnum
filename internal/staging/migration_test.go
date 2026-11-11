package staging

import (
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// A migration is a versioned file a merge adds under a db/<...migrate...>
// directory, at the root or under an engine: Jenkins runs the sharded and the
// data migrations as it runs db/migrate's, so the deploy's y/N must name them
// and a merge that now brings one must be refused. talkable#12203 adds
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

// staging's isMigration is a copy of config.MigrationFolder's rule (staging
// does not import config), and the two give the same answer for every path:
// a versioned .rb at any depth in a db/*migrate* folder, the root's or an
// engine's, never under spec or test. The copy took every file in such a
// folder before: talkable #12160 and #12220 listed the specs of
// spec/db/reporting_migrate next to their migrations, which the deploy-first
// nudge, the deploy plan's migrations and the stop on unseen ones all read.
func TestStagingsMigrationRuleIsTheConfigs(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"db/migrate/20261108090000_add_flag_to_credits.rb", true},
		{"db/sharded_migrate/20261105011921_add_metric_indexes.rb", true},
		{"engines/billing/db/migrate/20261101090000_create_invoices.rb", true},
		{"db/migrate/archive/20250101090000_old.rb", true},
		{"spec/db/reporting_migrate/20261101090000_add_rollup_spec.rb", false},
		{"spec/db/reporting_migrate/add_rollup_spec.rb", false},
		{"test/db/migrate/20261101090000_x.rb", false},
		{"db/migrate/README.md", false},
		{"db/migrate/support/helper.rb", false},
		{"db/structure.sql", false},
		{"db/reporting_structure.sql", false},
	} {
		_, inConfig := config.MigrationFolder(tc.path)
		if got := isMigration(tc.path); got != tc.want || inConfig != tc.want {
			t.Errorf("%s: staging's isMigration %v, config.MigrationFolder %v; want both %v", tc.path, got, inConfig, tc.want)
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
