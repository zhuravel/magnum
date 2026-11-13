package config

import (
	"slices"
	"strings"
	"testing"
)

// fixerOf is the factory's fixer role of cfg, to edit in place.
func fixerOf(t *testing.T, cfg *Config) *Role {
	t.Helper()
	i := slices.IndexFunc(cfg.Factory.Roles, func(r Role) bool { return r.Name == FactoryFixerRole })
	if i < 0 {
		t.Fatalf("no factory role %s", FactoryFixerRole)
	}
	return &cfg.Factory.Roles[i]
}

// Only the factory's fixer (fx-fixer) may carry an identity: it pushes and
// answers threads as the fix identity of the PR's watch, as magnum's fix
// role did, while every other factory role acts as you. So no other fx role
// takes an identity, not even a dedicated fixer App, none of them may be a
// fix role, and the fixer itself takes no review identity (the App a watch
// reviews, polls or approves as: its replies would read as magnum's own)
// and no identity the config does not declare.
func TestOnlyTheFixerRoleMayCarryAnIdentity(t *testing.T) {
	base := func() *Config {
		cfg := validFactoryConfig()
		cfg.Identities = append(cfg.Identities, fixerApp("fixer"), reviewApp())
		return cfg
	}

	cfg := base()
	fixerOf(t, cfg).Identity = "fixer"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the fixer as a dedicated fixer App: %v", err)
	}
	cfg = base()
	fixerOf(t, cfg).Identity = "me" // a gh identity: the operator's own account
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the fixer as a gh identity: %v", err)
	}

	for _, r := range DefaultFactoryRoles() {
		if r.Name == FactoryFixerRole {
			continue
		}
		t.Run(r.Name, func(t *testing.T) {
			cfg := base()
			i := slices.IndexFunc(cfg.Factory.Roles, func(x Role) bool { return x.Name == r.Name })
			cfg.Factory.Roles[i].Identity = "fixer"
			wantError(t, cfg.Validate(), "factory.role "+r.Name, "identity is not used by factory roles", FactoryFixerRole)

			cfg = base()
			cfg.Factory.Roles[i].Fix = true
			wantError(t, cfg.Validate(), "factory.role "+r.Name, "cannot be a fix role", FactoryFixerRole)
		})
	}

	for name, tc := range map[string]struct {
		edit func(*Config)
		want []string
	}{
		"the App a watch reviews as": {func(c *Config) {
			c.Watches[0].Identity = "review-app"
			fixerOf(t, c).Identity = "review-app"
		}, []string{"factory.role " + FactoryFixerRole, "identity review-app is the App watch acme posts its reviews as"}},
		"an undeclared identity": {func(c *Config) { fixerOf(t, c).Identity = "ghost" },
			[]string{"factory.role " + FactoryFixerRole, `unknown identity "ghost"`}},
		"no fix role": {func(c *Config) { fixerOf(t, c).Fix = false },
			[]string{"factory.role " + FactoryFixerRole, "must be a fix role"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			tc.edit(cfg)
			wantError(t, cfg.Validate(), tc.want...)
		})
	}

	// From the files: your [[factory.role]] blocks merge by name.
	if _, err := loadCommittedWithLocal(t, testLocalConfig+"\n[[factory.role]]\nname = \"fx-fixer\"\nidentity = \"me\"\n"); err != nil {
		t.Errorf("fx-fixer as your gh identity: %v", err)
	}
	_, err := loadCommittedWithLocal(t, testLocalConfig+"\n[[factory.role]]\nname = \"fx-writer\"\nidentity = \"me\"\n")
	wantError(t, err, "factory.role fx-writer", "identity is not used by factory roles")
}

// No stage runs the fixer: the fix lane starts it on a PR, with the fix
// job's prompt data, never a task's.
func TestNoStageNamesTheFixer(t *testing.T) {
	cfg := validFactoryConfig()
	editKind(cfg, "bug", func(stages []FactoryStage) []FactoryStage {
		stages[0].Roles = append(slices.Clone(stages[0].Roles), FactoryFixerRole)
		return stages
	})
	wantError(t, cfg.Validate(), "factory kind bug", `stage "investigate"`, "names fx-fixer, the fixer of your PRs, which no stage runs")
}

// The fixer acts as the fix identity of the PR's watch, as magnum's fix role
// does (FixRoleFor): the watch's fix_identity, else the identity the role
// names, else the watch's poll identity.
func TestTheFactoryFixerActsAsTheWatchsFixIdentity(t *testing.T) {
	cfg := validFactoryConfig()
	cfg.Identities = append(cfg.Identities, fixerApp("fixer"))
	w := &cfg.Watches[0]
	if r, ok := cfg.FactoryFixerFor(w); !ok || r.Identity != w.PollIdentity || !r.Fix {
		t.Fatalf("FactoryFixerFor without a fix_identity = %+v, %v; want the watch's poll identity %s", r, ok, w.PollIdentity)
	}
	fixerOf(t, cfg).Identity = "me"
	if r, _ := cfg.FactoryFixerFor(w); r.Identity != "me" {
		t.Errorf("FactoryFixerFor with the role's identity = %s, want me", r.Identity)
	}
	w.FixIdentity = "fixer"
	if r, _ := cfg.FactoryFixerFor(w); r.Identity != "fixer" {
		t.Errorf("FactoryFixerFor with the watch's fix_identity = %s, want fixer", r.Identity)
	}
	if r, _ := cfg.FactoryFixerFor(nil); r.Identity != "me" {
		t.Errorf("FactoryFixerFor(nil) = %s, want the role's own", r.Identity)
	}
	cfg.Factory.Roles = slices.DeleteFunc(cfg.Factory.Roles, func(r Role) bool { return r.Name == FactoryFixerRole })
	if _, ok := cfg.FactoryFixerFor(w); ok {
		t.Error("FactoryFixerFor found a fixer in a config without fx-fixer")
	}
}

// The fixer is magnum's fix role moved into the factory: claude, the fix
// prompt and the fix skill, in the Go defaults and the embedded file alike.
func TestTheFactoryFixerIsClaudeWithTheFixPromptAndSkill(t *testing.T) {
	for _, cfg := range []*Config{Defaults(), mustLoadDefaults(t)} {
		r, ok := cfg.FactoryRole(FactoryFixerRole)
		if !ok || !r.Fix || r.Kind != KindClaude || r.Prompt != DefaultFixPrompt || r.Skill != DefaultFixSkill || r.Identity != "" {
			t.Errorf("%s = %+v, %v; want a claude fix role with %s and %s, and no identity", FactoryFixerRole, r, ok, DefaultFixPrompt, DefaultFixSkill)
		}
	}
}

// [factory] fix is "off" by default, in the Go defaults and the embedded
// file: the factory does not fix until the switch day. It takes "on" or
// "off" alone, naming the key otherwise.
func TestFactoryFixIsOffByDefaultAndTakesOnOrOff(t *testing.T) {
	for _, f := range []Factory{Defaults().Factory, mustLoadDefaults(t).Factory} {
		if f.Fix != FactoryFixOff || f.Fixes() {
			t.Errorf("fix = %q by default, want off", f.Fix)
		}
	}
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml+"\n[factory]\nfix = \"on\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Factory.Fixes() || cfg.Factory.MaxActive != 3 {
		t.Errorf("factory = %+v, want fix on and the built-in max_active", cfg.Factory)
	}
	for _, bad := range []string{`"yes"`, `true`, `""`} {
		_, err := loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml+"\n[factory]\nfix = "+bad+"\n")
		if err == nil || !strings.Contains(err.Error(), "fix") {
			t.Errorf("fix = %s: err = %v, want one naming the key", bad, err)
		}
	}
}

// fix_max_active is 2 by default (the first day's cap of concurrent fix
// loops) and at least 1; fix_local_review is on by default and takes a
// boolean alone; a repository's fix_copies is 0 by default and 0 to 10.
func TestFactoryFixLimitsDefaultAndBounds(t *testing.T) {
	for _, f := range []Factory{Defaults().Factory, mustLoadDefaults(t).Factory} {
		if f.FixMaxActive != 2 || !f.FixLocalReview {
			t.Errorf("fix_max_active = %d, fix_local_review = %v by default, want 2 and true", f.FixMaxActive, f.FixLocalReview)
		}
	}
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml+"fix_copies = 2\n\n[factory]\nfix_max_active = 4\nfix_local_review = false\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Factory.FixMaxActive != 4 || cfg.Factory.FixLocalReview || cfg.Factory.Repos[0].FixCopies != 2 {
		t.Errorf("factory = %+v, want fix_max_active 4, fix_local_review off and fix_copies 2", cfg.Factory)
	}
	cfg, err = loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml)
	if err != nil {
		t.Fatal(err)
	}
	if n := cfg.Factory.Repos[0].FixCopies; n != 0 {
		t.Errorf("fix_copies = %d by default, want 0", n)
	}
	for key, bads := range map[string][]string{
		"fix_max_active":   {"0", "-1", `"two"`},
		"fix_local_review": {`"no"`, "0"},
	} {
		for _, bad := range bads {
			_, err := loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml+"\n[factory]\n"+key+" = "+bad+"\n")
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s = %s: err = %v, want one naming the key", key, bad, err)
			}
		}
	}
	for _, bad := range []string{"-1", "11", `"two"`} {
		_, err := loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml+"fix_copies = "+bad+"\n")
		if err == nil || !strings.Contains(err.Error(), "fix_copies") {
			t.Errorf("fix_copies = %s: err = %v, want one naming the key", bad, err)
		}
	}
}
