package gitx

import "testing"

// EnvConfig reads one setting of the GIT_CONFIG_COUNT pairs of an
// environment, as git reads them: only the first COUNT pairs, the last
// pair of the key winning, the key compared without case. fix-sync reads
// the pane's commit.gpgsign so, since gitx replaces the pairs with its own.
func TestEnvConfigReadsOnePairOfTheEnvironment(t *testing.T) {
	env := []string{"PATH=/bin", "GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=commit.gpgsign", "GIT_CONFIG_VALUE_0=true",
		"GIT_CONFIG_KEY_1=url./x/.pushInsteadOf", "GIT_CONFIG_VALUE_1=https://github.com/",
		"GIT_CONFIG_KEY_2=Commit.GpgSign", "GIT_CONFIG_VALUE_2=false",
		"GIT_CONFIG_KEY_3=core.hooksPath", "GIT_CONFIG_VALUE_3=/dev/null"}
	for _, tc := range []struct {
		env      []string
		key      string
		want     string
		wantSeen bool
	}{
		{env, "commit.gpgsign", "false", true},
		{env, "core.hooksPath", "", false}, // past GIT_CONFIG_COUNT
		{env, "user.email", "", false},
		{[]string{"GIT_CONFIG_KEY_0=commit.gpgsign", "GIT_CONFIG_VALUE_0=false"}, "commit.gpgsign", "", false}, // no count
		{[]string{"GIT_CONFIG_COUNT=x", "GIT_CONFIG_KEY_0=commit.gpgsign", "GIT_CONFIG_VALUE_0=false"}, "commit.gpgsign", "", false},
	} {
		if got, ok := EnvConfig(tc.env, tc.key); got != tc.want || ok != tc.wantSeen {
			t.Errorf("EnvConfig(%q, %s) = %q, %v; want %q, %v", tc.env, tc.key, got, ok, tc.want, tc.wantSeen)
		}
	}
}
