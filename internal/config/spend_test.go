package config

import (
	"os"
	"path/filepath"
	"testing"

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
