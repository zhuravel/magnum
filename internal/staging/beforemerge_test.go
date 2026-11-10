package staging

import (
	"slices"
	"testing"
)

func TestAPRThatChangesAMigrationNeedsAStagingDeployBeforeItsMerge(t *testing.T) {
	env := func(name string, s State) EnvStatus {
		return EnvStatus{Env: Env{Name: name, Branch: "staging-" + name}, State: s}
	}
	paths := []string{"app/models/user.rb", "db/migrate/20261109_b.rb", "db/migrate/20261109_a.rb"}
	for _, tc := range []struct {
		name       string
		paths      []string
		envs       []EnvStatus
		migrations []string
		needed     bool
	}{
		{"nowhere on a staging", paths, []EnvStatus{env("void", StateNone), env("bastion", StateNone), env("bart", StateOutdated)}, []string{"db/migrate/20261109_a.rb", "db/migrate/20261109_b.rb"}, true},
		{"live on one staging", paths, []EnvStatus{env("void", StateNone), env("bastion", StateLatest), env("bart", StateNone)}, []string{"db/migrate/20261109_a.rb", "db/migrate/20261109_b.rb"}, false},
		{"pushed to one staging, the deploy not confirmed yet", paths, []EnvStatus{env("void", StatePushed), env("bastion", StateNone), env("bart", StateNone)}, []string{"db/migrate/20261109_a.rb", "db/migrate/20261109_b.rb"}, false},
		{"one staging not read: it may hold the head", paths, []EnvStatus{env("void", StateUnknown), env("bastion", StateNone), env("bart", StateNone)}, []string{"db/migrate/20261109_a.rb", "db/migrate/20261109_b.rb"}, false},
		{"no staging read", paths, []EnvStatus{env("void", StateUnknown), env("bastion", StateUnknown)}, []string{"db/migrate/20261109_a.rb", "db/migrate/20261109_b.rb"}, false},
		{"no staging configured", paths, nil, []string{"db/migrate/20261109_a.rb", "db/migrate/20261109_b.rb"}, false},
		{"no migration", []string{"app/models/user.rb", "db/schema.rb"}, []EnvStatus{env("void", StateNone)}, nil, false},
		{"an engine's sharded migration", []string{"engines/billing/db/sharded_migrate/20261101_index.rb"}, []EnvStatus{env("void", StateNone)}, []string{"engines/billing/db/sharded_migrate/20261101_index.rb"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migrations, needed := DeployBeforeMerge(tc.paths, tc.envs)
			if !slices.Equal(migrations, tc.migrations) || (migrations == nil) != (tc.migrations == nil) || needed != tc.needed {
				t.Errorf("DeployBeforeMerge = %q, %v; want %q, %v", migrations, needed, tc.migrations, tc.needed)
			}
		})
	}
}

// The words name the first migration, in byte order, and count the rest:
// the first clause stays the same however many there are (the board stores
// the words, and the factory's short form cuts at the first " ("). No
// migration is no line: "" is what the board and the factory use for it, so
// a caller that forgets its own test of the list shows no nudge.
func TestTheDeployBeforeMergeWordsNameTheFirstMigrationAndCountTheRest(t *testing.T) {
	const a, b, c = "db/migrate/20261109_a.rb", "db/migrate/20261109_b.rb", "engines/billing/db/sharded_migrate/20261101_c.rb"
	for _, tc := range []struct {
		migrations []string
		want       string
	}{
		{[]string{a}, "deploy to a staging before the merge (it changes db/migrate/20261109_a.rb)"},
		{[]string{a, b}, "deploy to a staging before the merge (it changes db/migrate/20261109_a.rb and 1 more migration)"},
		{[]string{a, b, c}, "deploy to a staging before the merge (it changes db/migrate/20261109_a.rb and 2 more migrations)"},
		{nil, ""},
		{[]string{}, ""},
	} {
		if got := DeployBeforeMergeWords(tc.migrations); got != tc.want {
			t.Errorf("DeployBeforeMergeWords(%q) = %q, want %q", tc.migrations, got, tc.want)
		}
	}
}

// The rule lists every migration of the PR in byte order, nil for none,
// whatever order the paths come in.
func TestDeployBeforeMergeListsEveryMigrationInByteOrder(t *testing.T) {
	none := []EnvStatus{{Env: Env{Name: "void"}, State: StateNone}}
	got, needed := DeployBeforeMerge([]string{"db/migrate/2_b.rb", "app/a.rb", "engines/x/db/sharded_migrate/1_c.rb", "db/migrate/1_a.rb", "db/schema.rb"}, none)
	if want := []string{"db/migrate/1_a.rb", "db/migrate/2_b.rb", "engines/x/db/sharded_migrate/1_c.rb"}; !slices.Equal(got, want) || !needed {
		t.Errorf("DeployBeforeMerge = %q, %v; want %q, true", got, needed, want)
	}
	if got, needed := DeployBeforeMerge([]string{"app/a.rb", "db/schema.rb"}, none); got != nil || needed {
		t.Errorf("DeployBeforeMerge with no migration = %q, %v; want nil, false", got, needed)
	}
	if got, needed := DeployBeforeMerge([]string{"db/migrate/1_a.rb", "db/migrate/1_a.rb"}, none); !slices.Equal(got, []string{"db/migrate/1_a.rb"}) || !needed {
		t.Errorf("DeployBeforeMerge with a path twice = %q, %v; want it once", got, needed)
	}
}

// The count words are the rule's own, for any line that names the
// migrations a PR changes: the first in byte order, then how many more.
func TestMigrationWordsNameTheFirstMigrationAndCountTheRest(t *testing.T) {
	for _, tc := range []struct {
		migrations []string
		want       string
	}{
		{[]string{"db/migrate/a.rb"}, "it changes db/migrate/a.rb"},
		{[]string{"db/migrate/a.rb", "db/migrate/b.rb"}, "it changes db/migrate/a.rb and 1 more migration"},
		{[]string{"db/migrate/a.rb", "db/migrate/b.rb", "db/migrate/c.rb"}, "it changes db/migrate/a.rb and 2 more migrations"},
		{nil, ""},
	} {
		if got := MigrationWords(tc.migrations); got != tc.want {
			t.Errorf("MigrationWords(%q) = %q, want %q", tc.migrations, got, tc.want)
		}
	}
}
