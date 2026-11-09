package config

import (
	"testing"
	"time"
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
	"fx-points-codex":  {"", "high"},
	"fx-scope":         {"", "high"},
	"fx-critic":        {"", "xhigh"},
	"fx-critic-narrow": {"", "high"},
	"fx-critic-claude": {"", "high"},
	"fx-judge":         {"opus", "xhigh"},
	"fx-writer":        {"opus", "high"},
	"fx-qa":            {"", "high"},
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
// factory role that is not built in keeps its own effort, and fx-history
// keeps its medium (a history summary gains little from xhigh).
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
		{SpendHigh, map[string]string{"fx-repro": "xhigh", "fx-history": "medium", "fx-cause": "xhigh", "fx-judge": "xhigh",
			"fx-writer": "xhigh", "fx-liaison": "high", "fx-extra": "medium", "fx-critic-narrow": "xhigh", "fx-points-codex": "xhigh"}},
		{SpendLow, map[string]string{"fx-repro": "medium", "fx-history": "medium", "fx-critic": "high", "fx-judge": "high",
			"fx-writer": "medium", "fx-liaison": "high", "fx-extra": "medium", "fx-critic-narrow": "medium", "fx-points-codex": "medium"}},
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

// The stronger models of the factory's roles live in the operator's own
// levels, not in the built-in defaults (which name no Codex model and no
// burn level): a user [spend.burn] that names fx-judge and every factory
// role on Codex, the critics, the cause finder and the points twin, gives
// them its models at xhigh under spend = "burn", whichever kind's level
// applies; the user's [spend.high], which names the review's Codex roles
// only, leaves the factory's Codex roles on Codex's own model, at the high
// notch's xhigh, and the Claude roles on their built-in models.
func TestAUserBurnLevelRaisesTheFactoryJudgeAndItsCodexRoles(t *testing.T) {
	const tables = `
[spend.high]
codex-judge = { model = "gpt-lighter" }
codex-review = { model = "gpt-lighter" }

[spend.burn]
codex-judge = { model = "gpt-strongest", effort = "xhigh", rereview_effort = "xhigh" }
fx-judge = { model = "fable", effort = "xhigh" }
fx-critic = { model = "gpt-strongest", effort = "xhigh" }
fx-critic-narrow = { model = "gpt-strongest", effort = "xhigh" }
fx-cause = { model = "gpt-strongest", effort = "xhigh" }
fx-points-codex = { model = "gpt-strongest", effort = "xhigh" }
`
	codexRoles := []string{"fx-critic", "fx-critic-narrow", "fx-cause", "fx-points-codex"}
	for _, extra := range []string{"", "\n[kinds.codex]\nspend = \"burn\"\n\n[kinds.claude]\nspend = \"burn\"\n"} {
		cfg, _, err := loadUser(t, spendUser("burn", tables+extra))
		if err != nil {
			t.Fatal(err)
		}
		if r := factoryRoleOf(cfg, "fx-judge"); r.Model != "fable" || r.Effort != "xhigh" {
			t.Errorf("burn: fx-judge %q at %q, want fable at xhigh", r.Model, r.Effort)
		}
		for _, name := range codexRoles {
			if r := factoryRoleOf(cfg, name); r.Model != "gpt-strongest" || r.Effort != "xhigh" {
				t.Errorf("burn: %s %q at %q, want the level's Codex model at xhigh", name, r.Model, r.Effort)
			}
		}
		if r := factoryRoleOf(cfg, "fx-writer"); r.Model != "opus" || r.Effort != "high" {
			t.Errorf("burn: fx-writer %q at %q, want its built-in opus at high (burn starts from medium)", r.Model, r.Effort)
		}
	}

	cfg, _, err := loadUser(t, spendUser(SpendHigh, tables))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range codexRoles {
		if r := factoryRoleOf(cfg, name); r.Model != "" || r.Effort != "xhigh" {
			t.Errorf("high: %s %q at %q, want Codex's own model at the notch's xhigh", name, r.Model, r.Effort)
		}
	}
	if r := factoryRoleOf(cfg, "fx-judge"); r.Model != "opus" || r.Effort != "xhigh" {
		t.Errorf("high: fx-judge %q at %q, want its built-in opus at xhigh", r.Model, r.Effort)
	}
	if r := roleOf(cfg, RoleCodexJudge); r.Model != "gpt-lighter" {
		t.Errorf("high: codex-judge model %q, want the high table's", r.Model)
	}
}

// fx-history stays at its built-in effort under every built-in level and an
// operator's own: the notch leaves it alone. A level's table still sets it,
// as it sets any role.
func TestTheNotchLeavesTheHistoryRolesEffort(t *testing.T) {
	for _, level := range []string{SpendLow, SpendMedium, SpendHigh, "burn"} {
		cfg, _, err := loadUser(t, spendUser(level, "\n[spend.burn]\nclaude-review = { effort = \"xhigh\" }\n"))
		if err != nil {
			t.Fatalf("spend %s: %v", level, err)
		}
		if r := factoryRoleOf(cfg, "fx-history"); r.Effort != "medium" {
			t.Errorf("spend %s: fx-history effort %q, want its medium", level, r.Effort)
		}
	}
	cfg, _, err := loadUser(t, spendUser(SpendHigh, "\n[spend.high]\nfx-history = { effort = \"high\" }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r := factoryRoleOf(cfg, "fx-history"); r.Effort != "high" {
		t.Errorf("fx-history effort %q under a table that names it, want the table's high", r.Effort)
	}
}

// typicalFactoryRun is the mean wall time, in minutes, of each built-in
// factory role's verified runs in the factory's registry (attempt_roles,
// 2026-11-07), at the efforts config.defaults.toml writes. fx-scope,
// fx-cause-claude and fx-critic-claude have no runs yet; fx-judge and
// fx-critic already run at xhigh, and fx-history keeps medium.
var typicalFactoryRun = map[string]float64{"fx-repro": 3.6, "fx-cause": 4.8, "fx-points": 18.9, "fx-writer": 11.4}

// A role the high notch raises to xhigh keeps room in its timeout for a
// typical xhigh run, taken as twice its typical run at its built-in effort:
// past the timeout the watchdog treats 10 minutes without progress as a hung
// run, so the timeout must not fall inside a typical turn.
func TestARaisedFactoryRolesTimeoutFitsAnXhighRun(t *testing.T) {
	for name, typical := range typicalFactoryRun {
		r := factoryRoleOf(Defaults(), name)
		need := time.Duration(2*typical*float64(time.Minute)) + 10*time.Minute
		if r.Timeout.Duration < need {
			t.Errorf("%s timeout %s, want at least %s (twice its typical %.1f-minute run, then the watchdog's 10 minutes)",
				name, r.Timeout.Duration, need.Round(time.Minute), typical)
		}
	}
}
