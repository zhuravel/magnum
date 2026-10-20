package config

import "testing"

// BaseRole is a role as the layers set it, before any spend level, and
// RolesAtLevel every role under a level the roles do not follow, as the
// loader would resolve it: what the settings screen shows for a value the
// user file leaves to the layers under it.
func TestBaseRoleAndRolesAtLevelResolveAsTheLoaderDoes(t *testing.T) {
	cfg, _, err := loadUser(t, "[pipeline]\nspend = \"low\"\n\n[spend.high]\nclaude-review = { model = \"table-model\" }\n\n"+
		"[[role]]\nname = \"claude-review\"\nmodel = \"block-model\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := cfg.BaseRole("claude-review"); !ok || r.Model != "block-model" {
		t.Errorf("BaseRole = %q, %v", r.Model, ok)
	}
	if r, ok := cfg.BaseRole("claude-judge"); !ok || r.Model != "opus" {
		t.Errorf("BaseRole(claude-judge) = %q, %v", r.Model, ok)
	}
	byName := func(roles []Role, name string) Role {
		t.Helper()
		for _, r := range roles {
			if r.Name == name {
				return r
			}
		}
		t.Fatalf("no role %s", name)
		return Role{}
	}
	if r := byName(cfg.RolesAtLevel(SpendHigh), "claude-review"); r.Model != "table-model" || r.Effort != "xhigh" {
		t.Errorf("claude-review at high = %q %q", r.Model, r.Effort)
	}
	low, running := byName(cfg.RolesAtLevel(SpendLow), "claude-review"), byName(cfg.Roles, "claude-review")
	if low.Model != running.Model || low.Effort != running.Effort || low.RereviewEffort != running.RereviewEffort {
		t.Errorf("claude-review at low = %q %q %q, want the running roles' %q %q %q", low.Model, low.Effort, low.RereviewEffort,
			running.Model, running.Effort, running.RereviewEffort)
	}
	if running.Model != "block-model" {
		t.Errorf("RolesAtLevel changed the running roles: %q", running.Model)
	}
}
