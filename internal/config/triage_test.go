package config

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

// Triage is off unless asked for, and its defaults are the committed
// config.defaults.toml's: loading the file and a config without the section
// give the same values.
func TestTriageDefaults(t *testing.T) {
	want := Triage{
		MaxLines: 120,
		Command:  []string{"claude", "-p", "--model", "haiku", "--tools", "", "--no-session-persistence"},
		Timeout:  Duration{2 * time.Minute},
		Prompt:   "triage.md",
	}
	if got := Defaults().Triage; !reflect.DeepEqual(got, want) {
		t.Fatalf("built-in triage = %+v, want %+v", got, want)
	}
	root := repoRoot(t)
	explicit, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.defaults.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(explicit.Triage, want) {
		t.Fatalf("config.defaults.toml triage = %+v, want %+v", explicit.Triage, want)
	}
}

// The built-in reviewers carry a summary, the judge none: it is never a
// candidate for removal, and neither is a role without a summary.
func TestBuiltinRolesTriageSummaries(t *testing.T) {
	root := repoRoot(t)
	cfg, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.defaults.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	var removable []string
	for _, r := range cfg.Roles {
		if r.Removable() {
			removable = append(removable, r.Name)
		}
		if r.Judge && r.Summary != "" {
			t.Errorf("the judge has a summary: %q", r.Summary)
		}
	}
	if want := []string{"claude-review", "codex-review", "claude-simplify"}; !slices.Equal(removable, want) {
		t.Fatalf("removable roles = %v, want %v", removable, want)
	}
	for i, r := range Defaults().Roles { // the committed file writes the built-in roles out
		if r.Summary != cfg.Roles[i].Summary {
			t.Errorf("built-in %s summary = %q, config.defaults.toml has %q", r.Name, r.Summary, cfg.Roles[i].Summary)
		}
	}
	if (Role{Name: "x", Judge: true, Summary: "reads everything"}).Removable() || (Role{Name: "x", Summary: "  "}).Removable() {
		t.Fatal("a judge or a role with a blank summary is removable")
	}
}

func TestValidateTriage(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Triage)
		want string // "" = valid
	}{
		{name: "defaults", edit: func(*Triage) {}},
		{name: "enabled defaults", edit: func(tr *Triage) { tr.Enabled = true }},
		{name: "zero max_lines", edit: func(tr *Triage) { tr.Enabled, tr.MaxLines = true, 0 }, want: "triage.max_lines must be positive, got 0"},
		{name: "negative max_lines", edit: func(tr *Triage) { tr.Enabled, tr.MaxLines = true, -5 }, want: "triage.max_lines must be positive, got -5"},
		{name: "no command", edit: func(tr *Triage) { tr.Enabled, tr.Command = true, nil }, want: "triage.command must name the model's CLI"},
		{name: "blank command", edit: func(tr *Triage) { tr.Enabled, tr.Command = true, []string{" "} }, want: "triage.command must name the model's CLI"},
		{name: "zero timeout", edit: func(tr *Triage) { tr.Enabled, tr.Timeout = true, Duration{} }, want: "triage.timeout must be positive, got 0s"},
		{name: "disabled ignores the limits", edit: func(tr *Triage) { tr.MaxLines, tr.Command, tr.Timeout = 0, nil, Duration{} }},
		{name: "missing prompt", edit: func(tr *Triage) { tr.Prompt = "nope.md" }, want: "triage.prompt: prompt nope.md: prompt not found"},
		{name: "no prompt", edit: func(tr *Triage) { tr.Prompt = "" }, want: "triage.prompt must name a prompt file"},
		{name: "prompt with a path", edit: func(tr *Triage) { tr.Prompt = "../triage.md" }, want: "triage.prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(&cfg.Triage)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid: %v", err)
				}
				return
			}
			wantError(t, err, tc.want)
		})
	}
}

// A user config turns triage on key by key (the rest keeps its default) and
// gives a role a summary, which makes it a candidate for removal.
func TestTriageOverlayAndRoleSummary(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+`
[triage]
enabled = true
max_lines = 40
command = ["codex", "exec", "--model", "cheap", "-"]

[[role]]
name = "claude-review"
summary = "a skeptical read of the logic"

[[role]]
name = "lint"
kind = "shell"
command = "make lint"
`)
	if err != nil {
		t.Fatal(err)
	}
	want := Triage{Enabled: true, MaxLines: 40, Command: []string{"codex", "exec", "--model", "cheap", "-"},
		Timeout: Duration{2 * time.Minute}, Prompt: "triage.md"}
	if !reflect.DeepEqual(cfg.Triage, want) {
		t.Fatalf("triage = %+v, want %+v", cfg.Triage, want)
	}
	for name, removable := range map[string]bool{"claude-review": true, "codex-review": true, "lint": false, "codex-judge": false} {
		r, ok := cfg.RoleByNameOrAlias(nil, name)
		if !ok || r.Removable() != removable {
			t.Errorf("role %s: found %v, removable %v, want %v (%+v)", name, ok, r.Removable(), removable, r)
		}
	}
	if r, _ := cfg.RoleByNameOrAlias(nil, "claude-review"); r.Summary != "a skeptical read of the logic" {
		t.Fatalf("claude-review summary = %q", r.Summary)
	}
}
