package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/paths"
)

// efforts maps each role of cfg to its effort and rereview_effort.
func efforts(cfg *Config) map[string][2]string {
	out := map[string][2]string{}
	for _, r := range cfg.AllRoles() {
		out[r.Name] = [2]string{r.Effort, r.RereviewEffort}
	}
	return out
}

// spendUser is a user config over the built-in defaults that sets [pipeline]
// spend (none when spend is empty).
func spendUser(spend, extra string) string {
	body := minimalConfig + extra
	if spend != "" {
		body = "[pipeline]\nspend = \"" + spend + "\"\n" + body
	}
	return body
}

// [pipeline] spend sets the effort and rereview_effort of every built-in
// role: medium (and no key) is config.defaults.toml as written, high is xhigh
// everywhere, low one notch below medium with medium as the floor and an
// empty value left empty.
func TestTheSpendProfileSetsTheBuiltInEfforts(t *testing.T) {
	medium := map[string][2]string{
		RoleCodexJudge:       {"xhigh", "high"},
		RoleClaudeReview:     {"high", "medium"},
		RoleCodexReview:      {"high", ""},
		RoleClaudeSimplify:   {"", ""},
		RoleClaudeJudge:      {"xhigh", "high"},
		RoleClaudeFix:        {"high", ""},
		RoleClaudeWhiteboard: {"medium", ""},
	}
	high := map[string][2]string{}
	for name := range medium {
		high[name] = [2]string{"xhigh", "xhigh"}
	}
	low := map[string][2]string{
		RoleCodexJudge:       {"high", "medium"},
		RoleClaudeReview:     {"medium", "medium"},
		RoleCodexReview:      {"medium", ""},
		RoleClaudeSimplify:   {"", ""},
		RoleClaudeJudge:      {"high", "medium"},
		RoleClaudeFix:        {"medium", ""},
		RoleClaudeWhiteboard: {"medium", ""},
	}
	for _, tc := range []struct {
		spend string
		want  map[string][2]string
	}{{"", medium}, {SpendMedium, medium}, {SpendHigh, high}, {SpendLow, low}} {
		cfg, _, err := loadUser(t, spendUser(tc.spend, ""))
		if err != nil {
			t.Fatalf("spend %q: %v", tc.spend, err)
		}
		got := efforts(cfg)
		if len(got) != len(tc.want) {
			t.Errorf("spend %q: roles %v, want the %d built-in roles", tc.spend, got, len(tc.want))
		}
		for name, want := range tc.want {
			if got[name] != want {
				t.Errorf("spend %q: %s effort, rereview_effort = %q, want %q", tc.spend, name, got[name], want)
			}
		}
		// The re-review effort a round asks for (EffortFor) follows the profile too.
		if j := cfg.JudgeFor(nil); j.EffortFor(true) != tc.want[RoleCodexJudge][1] {
			t.Errorf("spend %q: the judge's re-review effort = %q, want %q", tc.spend, j.EffortFor(true), tc.want[RoleCodexJudge][1])
		}
	}
	// The low column's roles are the medium column's one notch down, which the
	// floor stops at medium; the check above pins the result, this one the rule.
	if got := spendEffort(SpendLow, "medium"); got != "medium" {
		t.Errorf(`spendEffort(low, "medium") = %q, want medium (the floor)`, got)
	}
}

// A [[role]] block that sets effort or rereview_effort keeps that value under
// every profile, whichever file it is in (the user config over the built-in
// defaults, or a base configuration of its own), while a key the block leaves
// out follows the profile; a role that is not built in and sets no effort
// stays without one.
func TestARolesOwnEffortWinsOverTheSpendProfile(t *testing.T) {
	const roles = `
[[role]]
name = "codex-judge"
effort = "medium"

[[role]]
name = "claude-review"
rereview_effort = ""

[[role]]
name = "droid-lint"
kind = "droid"
runs = "manual"
prompt = "claude-review.md"
`
	cfg, _, err := loadUser(t, spendUser(SpendHigh, roles))
	if err != nil {
		t.Fatal(err)
	}
	got := efforts(cfg)
	for name, want := range map[string][2]string{
		RoleCodexJudge:   {"medium", "xhigh"},
		RoleClaudeReview: {"xhigh", ""},
		RoleCodexReview:  {"xhigh", "xhigh"},
		"droid-lint":     {"", ""},
	} {
		if got[name] != want {
			t.Errorf("user config, spend high: %s effort, rereview_effort = %q, want %q", name, got[name], want)
		}
	}
	cfg, _, err = loadUser(t, spendUser(SpendLow, roles))
	if err != nil {
		t.Fatal(err)
	}
	if got := efforts(cfg)[RoleCodexJudge]; got != [2]string{"medium", "medium"} {
		t.Errorf("user config, spend low: codex-judge = %q, want its own medium and the profile's medium", got)
	}

	// A base configuration (--config) is the operator's file too: its keys win,
	// the ones its blocks leave out are the built-in role's and follow the
	// profile, and a built-in role it does not name is gone from the list.
	home := t.TempDir()
	base := filepath.Join(home, "config.toml")
	if err := os.WriteFile(base, []byte(spendUser(SpendHigh, roles)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadWithOptions(paths.Layout{Home: home}, base, LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	got = efforts(cfg)
	for name, want := range map[string][2]string{
		RoleCodexJudge:   {"medium", "xhigh"},
		RoleClaudeReview: {"xhigh", ""},
		"droid-lint":     {"", ""},
	} {
		if got[name] != want {
			t.Errorf("base config, spend high: %s effort, rereview_effort = %q, want %q", name, got[name], want)
		}
	}
	if _, ok := got[RoleCodexReview]; ok {
		t.Errorf("base config: roles %v, want only the three it declares", got)
	}
}

// spend takes low, medium or high; anything else is refused with a sentence
// that names the three.
func TestAnUnknownSpendValueIsRefused(t *testing.T) {
	for _, v := range []string{"max", "xhigh", "High", "none"} {
		_, _, err := loadUser(t, spendUser(v, ""))
		wantError(t, err, `pipeline: spend must be "low", "medium" or "high", got "`+v+`"`)
	}
	cfg := validPipelineConfig()
	cfg.Pipeline.Spend = "turbo"
	wantError(t, cfg.Validate(), `pipeline: spend must be "low", "medium" or "high", got "turbo"`)
	if d := Defaults().Pipeline.Spend; d != SpendMedium {
		t.Errorf("Defaults(): spend = %q, want medium", d)
	}
}

// models maps each role of cfg to its model.
func models(cfg *Config) map[string]string {
	out := map[string]string{}
	for _, r := range cfg.AllRoles() {
		out[r.Name] = r.Model
	}
	return out
}

// roleOf is cfg's role called name (the zero Role when there is none).
func roleOf(cfg *Config, name string) Role {
	for _, r := range cfg.AllRoles() {
		if r.Name == name {
			return r
		}
	}
	return Role{}
}

// spendTables are the README's example tables: a high level on the astra
// model and fable, a low one on the cheaper models.
const spendTables = `
[spend.high]
codex-judge = { model = "gpt-6.1-astra", effort = "xhigh", rereview_effort = "xhigh" }
codex-review = { model = "gpt-6.1-astra" }
claude-review = { model = "fable", effort = "xhigh" }

[spend.low]
codex-judge = { model = "gpt-6.1-sol", effort = "high", rereview_effort = "medium" }
claude-review = { model = "sonnet", effort = "medium", rereview_effort = "medium" }
claude-simplify = { model = "sonnet", effort = "low" }
`

// spendWant is a role's model and its effort and rereview_effort.
type spendWant struct {
	model   string
	efforts [2]string
}

func checkSpend(t *testing.T, label string, cfg *Config, want map[string]spendWant) {
	t.Helper()
	got, mdl := efforts(cfg), models(cfg)
	for name, w := range want {
		if mdl[name] != w.model || got[name] != w.efforts {
			t.Errorf("%s: %s model %q, efforts %q; want %q, %q", label, name, mdl[name], got[name], w.model, w.efforts)
		}
	}
}

// The active level's [spend.<level>] table sets the models and efforts of
// the roles it names; a role it does not name keeps the level's notch
// mapping, and a table of a level that is not active changes nothing.
func TestASpendLevelTableSetsModelsAndEfforts(t *testing.T) {
	cfg, _, err := loadUser(t, spendUser(SpendHigh, spendTables))
	if err != nil {
		t.Fatal(err)
	}
	checkSpend(t, "spend high", cfg, map[string]spendWant{
		RoleCodexJudge:     {"gpt-6.1-astra", [2]string{"xhigh", "xhigh"}},
		RoleClaudeReview:   {"fable", [2]string{"xhigh", "xhigh"}}, // rereview_effort: the notch mapping's
		RoleCodexReview:    {"gpt-6.1-astra", [2]string{"xhigh", "xhigh"}},
		RoleClaudeFix:      {"opus", [2]string{"xhigh", "xhigh"}}, // not in the table
		RoleClaudeSimplify: {"", [2]string{"xhigh", "xhigh"}},     // in the low table only
	})

	cfg, _, err = loadUser(t, spendUser(SpendLow, spendTables))
	if err != nil {
		t.Fatal(err)
	}
	checkSpend(t, "spend low", cfg, map[string]spendWant{
		RoleCodexJudge:     {"gpt-6.1-sol", [2]string{"high", "medium"}},
		RoleClaudeReview:   {"sonnet", [2]string{"medium", "medium"}},
		RoleClaudeSimplify: {"sonnet", [2]string{"low", ""}},
		RoleCodexReview:    {"", [2]string{"medium", ""}}, // the notch mapping, no model
		RoleClaudeJudge:    {"opus", [2]string{"high", "medium"}},
	})

	// A base configuration's tables and the user config's merge level by
	// level, role by role and key by key, the user config's key winning.
	home := t.TempDir()
	base := filepath.Join(home, "base.toml")
	baseBody := string(magnum.DefaultConfig) + `
[spend.high]
codex-judge = { model = "gpt-6.1-sol", max_subagents = 4 }
claude-fix = { model = "sonnet" }
`
	if err := os.WriteFile(base, []byte(baseBody), 0o600); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(home, "user.toml")
	if err := os.WriteFile(user, []byte(spendUser(SpendHigh, spendTables)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(paths.Layout{Home: home, UserConfig: user}, base)
	if err != nil {
		t.Fatal(err)
	}
	judge := roleOf(cfg, RoleCodexJudge)
	if judge.Model != "gpt-6.1-astra" || judge.MaxSubagents == nil || *judge.MaxSubagents != 4 {
		t.Errorf("base and user tables: codex-judge model %q, max_subagents %v; want the user's astra and the base's 4", judge.Model, judge.MaxSubagents)
	}
	if fix := roleOf(cfg, RoleClaudeFix); fix.Model != "sonnet" {
		t.Errorf("base and user tables: claude-fix model %q, want the base table's sonnet", fix.Model)
	}
}

// A level's table is the more specific intent: it wins over a [[role]]
// block's own model and effort while its level is active, and the block's
// keys hold under any other level.
func TestASpendLevelTableWinsOverTheRoleBlock(t *testing.T) {
	const body = `
[[role]]
name = "claude-review"
model = "opus"
effort = "medium"

[spend.high]
claude-review = { model = "fable", effort = "xhigh" }
`
	for _, tc := range []struct {
		spend, model, effort string
	}{{SpendHigh, "fable", "xhigh"}, {SpendMedium, "opus", "medium"}, {SpendLow, "opus", "medium"}} {
		cfg, _, err := loadUser(t, spendUser(tc.spend, body))
		if err != nil {
			t.Fatalf("spend %s: %v", tc.spend, err)
		}
		if r := roleOf(cfg, RoleClaudeReview); r.Model != tc.model || r.Effort != tc.effort {
			t.Errorf("spend %s: claude-review %q at %q, want %q at %q", tc.spend, r.Model, r.Effort, tc.model, tc.effort)
		}
	}
}

// A level that is not low, medium or high is the operator's own: it starts
// from medium (the efforts as config.defaults.toml writes them) and applies
// its table, max_subagents included.
func TestAnOperatorsOwnSpendLevelStartsFromMedium(t *testing.T) {
	const body = `
[spend.burn]
codex-judge = { model = "gpt-6.1-astra", rereview_effort = "xhigh", max_subagents = 0 }
claude-review = { effort = "xhigh" }
`
	cfg, _, err := loadUser(t, spendUser("burn", body))
	if err != nil {
		t.Fatal(err)
	}
	medium, _, err := loadUser(t, spendUser(SpendMedium, ""))
	if err != nil {
		t.Fatal(err)
	}
	got, want := efforts(cfg), efforts(medium)
	want[RoleCodexJudge] = [2]string{"xhigh", "xhigh"}
	want[RoleClaudeReview] = [2]string{"xhigh", "medium"}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("spend burn: %s efforts %q, want %q", name, got[name], w)
		}
	}
	judge := roleOf(cfg, RoleCodexJudge)
	if judge.Model != "gpt-6.1-astra" || judge.MaxSubagents == nil || *judge.MaxSubagents != 0 {
		t.Errorf("spend burn: codex-judge model %q, max_subagents %v; want astra and 0", judge.Model, judge.MaxSubagents)
	}
	if r := roleOf(cfg, RoleClaudeReview); r.Model != "" {
		t.Errorf("spend burn: claude-review model %q, want none (the table sets no model)", r.Model)
	}
	if r := roleOf(cfg, RoleClaudeFix); r.Model != "opus" {
		t.Errorf("spend burn: claude-fix model %q, want the built-in opus", r.Model)
	}
}

// A table may name only a built-in role or a declared [[role]], and only the
// four keys; a level's name is short and lower case.
func TestASpendTableNamingAnUnknownRoleOrKeyIsRefused(t *testing.T) {
	_, _, err := loadUser(t, spendUser(SpendHigh, "\n[spend.high]\nclaude-reviewer = { effort = \"xhigh\" }\n"))
	wantError(t, err, `spend.high: unknown role "claude-reviewer"`)
	_, _, err = loadUser(t, spendUser(SpendHigh, "\n[spend.low]\nclaude-reviewer = { effort = \"xhigh\" }\n"))
	wantError(t, err, `spend.low: unknown role "claude-reviewer"`) // an inactive level's table too
	_, _, err = loadUser(t, spendUser(SpendHigh, "\n[spend.high]\nclaude-review = { models = \"fable\" }\n"))
	wantError(t, err, "unknown keys", "spend.high.claude-review.models")
	_, _, err = loadUser(t, spendUser(SpendHigh, "\n[spend.high]\nclaude-review = { timeout = \"10m\" }\n"))
	wantError(t, err, "unknown keys", "spend.high.claude-review.timeout")
	_, _, err = loadUser(t, spendUser("", "\n[spend.Burn]\nclaude-review = { effort = \"xhigh\" }\n"))
	wantError(t, err, `spend: level name "Burn" must match ^[a-z][a-z0-9_-]{0,15}$`)
	_, _, err = loadUser(t, spendUser("", "\n[spend.a-very-long-level-name]\nclaude-review = { effort = \"xhigh\" }\n"))
	wantError(t, err, `spend: level name "a-very-long-level-name" must match`)

	// A declared [[role]] may be named, and so may a built-in role the
	// watch does not run.
	const droid = `
[[role]]
name = "droid-lint"
kind = "droid"
runs = "manual"
prompt = "claude-review.md"

[spend.high]
droid-lint = { effort = "high" }
claude-judge = { effort = "max" }
`
	cfg, _, err := loadUser(t, spendUser(SpendHigh, droid))
	if err != nil {
		t.Fatal(err)
	}
	if r := roleOf(cfg, "droid-lint"); r.Effort != "high" {
		t.Errorf("droid-lint effort %q, want the table's high", r.Effort)
	}
	if r := roleOf(cfg, RoleClaudeJudge); r.Effort != "max" {
		t.Errorf("claude-judge effort %q, want the table's max", r.Effort)
	}
}

// spend names low, medium, high or a level a [spend.<level>] table declares;
// any other name is refused with the declared names, and a table for a
// built-in level no spend names is declared for later.
func TestSpendNamingAnUndeclaredLevelIsRefused(t *testing.T) {
	const tables = `
[spend.burn]
claude-review = { effort = "xhigh" }

[spend.tight]
claude-review = { model = "sonnet" }

[spend.high]
claude-review = { model = "fable" }
`
	_, _, err := loadUser(t, spendUser("turbo", tables))
	wantError(t, err, `pipeline: spend must be "low", "medium", "high", "burn" or "tight", got "turbo"`)
	for _, v := range []string{"", SpendLow, SpendMedium, "burn", "tight"} {
		cfg, _, err := loadUser(t, spendUser(v, tables))
		if err != nil {
			t.Fatalf("spend %q: %v", v, err)
		}
		if r := roleOf(cfg, RoleClaudeReview); r.Model == "fable" {
			t.Errorf("spend %q: claude-review runs the high table's fable", v)
		}
	}
}

// A shell role takes a model now (codex-review passes it as -c model=...),
// but only through {{.Model}} in its command or full-line template: a model
// its command does not use is refused rather than dropped.
func TestAShellRolesModelNeedsItsCommandToUseIt(t *testing.T) {
	cfg, _, err := loadUser(t, spendUser("", "\n[[role]]\nname = \"codex-review\"\nmodel = \"gpt-6.1-sol\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r := roleOf(cfg, RoleCodexReview); r.Model != "gpt-6.1-sol" {
		t.Errorf("codex-review model %q, want gpt-6.1-sol", r.Model)
	}
	const own = "\n[[role]]\nname = \"codex-review\"\ncommand = \"codex review --base {{.BaseRef}}\"\n"
	_, _, err = loadUser(t, spendUser("", own+"model = \"gpt-6.1-sol\"\n"))
	wantError(t, err, `role codex-review`, `model "gpt-6.1-sol" reaches a shell role only as {{.Model}} in its command, which does not use it`)
	_, _, err = loadUser(t, spendUser(SpendHigh, own+"\n[spend.high]\ncodex-review = { model = \"gpt-6.1-astra\" }\n"))
	wantError(t, err, `model "gpt-6.1-astra" reaches a shell role only as {{.Model}}`)
	if _, _, err = loadUser(t, spendUser("", "\n[[role]]\nname = \"codex-review\"\ncommand = \"\"\nprompt = \"codex-review.sh\"\nmodel = \"gpt-6.1-sol\"\n")); err != nil {
		t.Fatalf("codex-review.sh with a model: %v", err)
	}
}
