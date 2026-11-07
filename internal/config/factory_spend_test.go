package config

import (
	"testing"
)

// factoryRoleOf is cfg's factory role called name (the zero Role when there
// is none).
func factoryRoleOf(cfg *Config, name string) Role {
	r, _ := cfg.FactoryRole(name)
	return r
}

// builtinFactoryModels are the model and effort of every built-in factory
// role as config.defaults.toml writes them: opus for fx-judge and fx-writer,
// the CLI's default model for the rest.
var builtinFactoryModels = map[string][2]string{
	"fx-repro":         {"", "high"},
	"fx-cause":         {"", "high"},
	"fx-cause-claude":  {"", "high"},
	"fx-history":       {"", "medium"},
	"fx-points":        {"", "high"},
	"fx-scope":         {"", "high"},
	"fx-critic":        {"", "xhigh"},
	"fx-critic-claude": {"", "high"},
	"fx-judge":         {"opus", "xhigh"},
	"fx-writer":        {"opus", "high"},
	"fx-liaison":       {"", "high"},
}

// Under the default config every factory role resolves to the model and the
// effort it ran before [spend] reached the factory roles: the embedded
// defaults alone, a user config that names no level, and one that names
// medium. No built-in level whose table names no factory role moves a
// factory role's model either: the notch changes efforts only.
func TestTheDefaultConfigKeepsEveryFactoryRolesModel(t *testing.T) {
	builtin := mustLoadDefaults(t)
	none, _, err := loadUser(t, spendUser("", ""))
	if err != nil {
		t.Fatal(err)
	}
	medium, _, err := loadUser(t, spendUser(SpendMedium, ""))
	if err != nil {
		t.Fatal(err)
	}
	for label, cfg := range map[string]*Config{"the embedded defaults": builtin, "no spend": none, "spend medium": medium} {
		if len(cfg.Factory.Roles) != len(builtinFactoryModels) {
			t.Errorf("%s: %d factory roles, want the %d built-in ones", label, len(cfg.Factory.Roles), len(builtinFactoryModels))
		}
		for name, want := range builtinFactoryModels {
			if r := factoryRoleOf(cfg, name); r.Model != want[0] || r.Effort != want[1] {
				t.Errorf("%s: %s model %q at %q, want %q at %q", label, name, r.Model, r.Effort, want[0], want[1])
			}
		}
	}
	for _, level := range []string{SpendLow, SpendHigh} {
		cfg, _, err := loadUser(t, spendUser(level, ""))
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range builtinFactoryModels {
			if r := factoryRoleOf(cfg, name); r.Model != want[0] {
				t.Errorf("spend %s: %s model %q, want %q (no table names it)", level, name, r.Model, want[0])
			}
		}
	}
}

// [pipeline] spend's notch reaches the built-in factory roles as it reaches
// the built-in review roles: high is xhigh, low one notch down with medium
// the floor, and a [[factory.role]] block's own effort wins over it; a
// factory role that is not built in keeps its own effort.
func TestTheSpendLevelsNotchReachesTheFactoryRoles(t *testing.T) {
	const roles = `
[[factory.role]]
name = "fx-liaison"
effort = "high"

[[factory.role]]
name = "fx-extra"
kind = "claude"
effort = "medium"
prompt = "factory-judge.md"
why = "look at the extra things"
doing = "looking at extras"
`
	for _, tc := range []struct {
		spend string
		want  map[string]string
	}{
		{SpendHigh, map[string]string{"fx-repro": "xhigh", "fx-history": "xhigh", "fx-cause": "xhigh", "fx-judge": "xhigh",
			"fx-writer": "xhigh", "fx-liaison": "high", "fx-extra": "medium"}},
		{SpendLow, map[string]string{"fx-repro": "medium", "fx-history": "medium", "fx-critic": "high", "fx-judge": "high",
			"fx-writer": "medium", "fx-liaison": "high", "fx-extra": "medium"}},
	} {
		cfg, _, err := loadUser(t, spendUser(tc.spend, roles))
		if err != nil {
			t.Fatalf("spend %s: %v", tc.spend, err)
		}
		for name, want := range tc.want {
			if r := factoryRoleOf(cfg, name); r.Effort != want {
				t.Errorf("spend %s: %s effort %q, want %q", tc.spend, name, r.Effort, want)
			}
		}
	}
}

// A [spend.<level>] table names a factory role as it names a review role:
// the active level's table sets its model and effort over the notch, a
// kind's own level picks the table for that kind's factory roles, and a
// role no table names keeps its model. A declared [[factory.role]] may be
// named; a name that is no role of either kind is still refused.
func TestASpendLevelTableSetsAFactoryRolesModel(t *testing.T) {
	const body = `
[spend.high]
fx-judge = { model = "fable" }
fx-history = { model = "sonnet", effort = "low" }
fx-extra = { model = "sonnet" }

[spend.burn]
fx-critic = { model = "gpt-6-astra" }

[kinds.codex]
spend = "burn"

[[factory.role]]
name = "fx-extra"
kind = "claude"
prompt = "factory-judge.md"
why = "look at the extra things"
doing = "looking at extras"
`
	cfg, _, err := loadUser(t, spendUser(SpendHigh, body))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][2]string{
		"fx-judge":   {"fable", "xhigh"},
		"fx-history": {"sonnet", "low"},
		"fx-writer":  {"opus", "xhigh"},        // no table names it
		"fx-critic":  {"gpt-6-astra", "xhigh"}, // codex: burn, an own level, starts from medium
		"fx-cause":   {"", "high"},
		"fx-extra":   {"sonnet", ""},
	} {
		if r := factoryRoleOf(cfg, name); r.Model != want[0] || r.Effort != want[1] {
			t.Errorf("%s model %q at %q, want %q at %q", name, r.Model, r.Effort, want[0], want[1])
		}
	}
	if r := roleOf(cfg, RoleClaudeReview); r.Model != "" || r.Effort != "xhigh" {
		t.Errorf("claude-review model %q at %q, want the notch alone", r.Model, r.Effort)
	}

	_, _, err = loadUser(t, spendUser(SpendHigh, "\n[spend.high]\nfx-nobody = { model = \"fable\" }\n"))
	wantError(t, err, `spend.high: unknown role "fx-nobody"`, "[[factory.role]]")
}
