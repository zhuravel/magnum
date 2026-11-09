package staging

import "testing"

func TestAPRThatChangesAMigrationNeedsAStagingDeployBeforeItsMerge(t *testing.T) {
	env := func(name string, s State) EnvStatus {
		return EnvStatus{Env: Env{Name: name, Branch: "staging-" + name}, State: s}
	}
	paths := []string{"app/models/user.rb", "db/migrate/20261109_b.rb", "db/migrate/20261109_a.rb"}
	for _, tc := range []struct {
		name      string
		paths     []string
		envs      []EnvStatus
		migration string
		needed    bool
	}{
		{"nowhere on a staging", paths, []EnvStatus{env("void", StateNone), env("bastion", StateNone), env("bart", StateOutdated)}, "db/migrate/20261109_a.rb", true},
		{"live on one staging", paths, []EnvStatus{env("void", StateNone), env("bastion", StateLatest), env("bart", StateNone)}, "db/migrate/20261109_a.rb", false},
		{"pushed to one staging, the deploy not confirmed yet", paths, []EnvStatus{env("void", StatePushed), env("bastion", StateNone), env("bart", StateNone)}, "db/migrate/20261109_a.rb", false},
		{"one staging not read: it may hold the head", paths, []EnvStatus{env("void", StateUnknown), env("bastion", StateNone), env("bart", StateNone)}, "db/migrate/20261109_a.rb", false},
		{"no staging read", paths, []EnvStatus{env("void", StateUnknown), env("bastion", StateUnknown)}, "db/migrate/20261109_a.rb", false},
		{"no staging configured", paths, nil, "db/migrate/20261109_a.rb", false},
		{"no migration", []string{"app/models/user.rb", "db/schema.rb"}, []EnvStatus{env("void", StateNone)}, "", false},
		{"an engine's sharded migration", []string{"engines/billing/db/sharded_migrate/20261101_index.rb"}, []EnvStatus{env("void", StateNone)}, "engines/billing/db/sharded_migrate/20261101_index.rb", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migration, needed := DeployBeforeMerge(tc.paths, tc.envs)
			if migration != tc.migration || needed != tc.needed {
				t.Errorf("DeployBeforeMerge = %q, %v; want %q, %v", migration, needed, tc.migration, tc.needed)
			}
		})
	}
}

func TestTheDeployBeforeMergeWordsNameTheMigration(t *testing.T) {
	if got, want := DeployBeforeMergeWords("db/migrate/20261109_a.rb"), "deploy to a staging before the merge (it changes db/migrate/20261109_a.rb)"; got != want {
		t.Errorf("DeployBeforeMergeWords = %q, want %q", got, want)
	}
}
