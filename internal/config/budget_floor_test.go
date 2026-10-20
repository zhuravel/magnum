package config

import (
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
)

// [github] budget_floor is 500 points by default, in Defaults() and in
// config.defaults.toml loaded alone; a user layer overrides it.
func TestBudgetFloorDefaultsTo500(t *testing.T) {
	if got := Defaults().GitHub.BudgetFloor; got != 500 {
		t.Fatalf("Defaults budget_floor = %d, want 500", got)
	}
	root := repoRoot(t)
	standalone, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.defaults.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatalf("config.defaults.toml does not load standalone: %v", err)
	}
	if got := standalone.GitHub.BudgetFloor; got != 500 {
		t.Fatalf("config.defaults.toml alone has budget_floor %d, want 500", got)
	}
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+"\n[github]\nbudget_floor = 0\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.BudgetFloor != 0 || cfg.GitHub.Transport != "gh" {
		t.Fatalf("overlay: %+v, want budget_floor 0 and the transport kept", cfg.GitHub)
	}
}

// budget_floor is 0 (off) to 5000 points (a user's whole hourly budget).
func TestValidateBudgetFloor(t *testing.T) {
	for _, tc := range []struct {
		floor int
		want  string
	}{
		{floor: 0},
		{floor: 500},
		{floor: 5000},
		{floor: -1, want: "github.budget_floor must be between 0 (off) and 5000 points, got -1"},
		{floor: 5001, want: "github.budget_floor must be between 0 (off) and 5000 points, got 5001"},
	} {
		cfg := validPipelineConfig()
		cfg.GitHub.BudgetFloor = tc.floor
		err := cfg.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("budget_floor %d: %v", tc.floor, err)
			}
			continue
		}
		wantError(t, err, tc.want)
	}
}
