package config

import "testing"

// The retro's classifying agent is a role like any other, so a [learn] kind
// that cannot run a role fails the load the way the same [[role]] would: a kind
// without model args cannot be handed the model.
func TestValidateLearnHoldsTheRoleToTheRoleChecks(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Learn.Kind, cfg.Learn.Model = "droid", "sonnet"
	wantError(t, cfg.Validate(), "learn:", "kind droid has no model args", `model "sonnet" cannot be passed`)

	// The same fault in a [[role]] gives the same message, under the role's name.
	cfg = validPipelineConfig()
	cfg.Roles = append(cfg.Roles, Role{Name: "retro-copy", Kind: "droid", Model: "sonnet", Prompt: "claude-review.md"})
	wantError(t, cfg.Validate(), "role retro-copy: kind droid has no model args")

	// A fault of the role is reported once, not once as a role and again as [learn].
	cfg = validPipelineConfig()
	cfg.Learn.Kind, cfg.Learn.Model = "droid", "sonnet"
	if got := len(validateErrs(cfg.Validate())); got != 1 {
		t.Fatalf("%d errors for one bad kind: %v", got, cfg.Validate())
	}

	// The prompt and the timeout keep their own messages and are not repeated by the role checks.
	cfg = validPipelineConfig()
	cfg.Learn.Timeout, cfg.Learn.Prompt = Duration{-1}, "nope.md"
	if got := len(validateErrs(cfg.Validate())); got != 2 {
		t.Fatalf("%d errors for a bad timeout and prompt: %v", got, cfg.Validate())
	}

	// A kind that is not declared is reported by the [learn] check alone.
	cfg = validPipelineConfig()
	cfg.Learn.Kind = "nope"
	if got := len(validateErrs(cfg.Validate())); got != 1 {
		t.Fatalf("%d errors for an undeclared kind: %v", got, cfg.Validate())
	}
}

// The default model "sonnet" is a Claude model name: it is the default of
// the claude kind only. Another kind named without a model runs on its own
// default, because `codex --model sonnet` would fail the retro on every PR.
func TestLearnDefaultModelFollowsTheKind(t *testing.T) {
	cases := []struct {
		name, learn string
		wantKind    string
		wantModel   string
		wantErr     string
	}{
		{"no [learn] section", "", "claude", "sonnet", ""},
		{"claude named", "kind = \"claude\"\n", "claude", "sonnet", ""},
		{"only a model", "model = \"haiku\"\n", "claude", "haiku", ""},
		{"claude with no model of its own", "kind = \"claude\"\nmodel = \"\"\n", "claude", "", ""},
		{"codex without a model", "kind = \"codex\"\n", "codex", "", ""},
		{"codex with its own model", "kind = \"codex\"\nmodel = \"gpt-5\"\n", "codex", "gpt-5", ""},
		{"codex with sonnet written out", "kind = \"codex\"\nmodel = \"sonnet\"\n", "codex", "sonnet", ""},
		{"droid without a model", "kind = \"droid\"\n", "droid", "", ""},
		{"droid with a model", "kind = \"droid\"\nmodel = \"sonnet\"\n", "", "", "kind droid has no model args"},
	}
	for _, tc := range cases {
		for _, layer := range []string{"user config", "base file"} {
			t.Run(tc.name+"/"+layer, func(t *testing.T) {
				section := ""
				if tc.learn != "" {
					section = "[learn]\n" + tc.learn
				}
				files := map[string]string{"config.toml": minimalConfig}
				if layer == "user config" {
					files["user.toml"] = section
				} else {
					files["config.toml"] = minimalConfig + section
				}
				cfg, err := loadFiles(t, t.TempDir(), files)
				if tc.wantErr != "" {
					wantError(t, err, tc.wantErr)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Learn.Kind != tc.wantKind || cfg.Learn.Model != tc.wantModel {
					t.Fatalf("learn kind, model = %q, %q; want %q, %q", cfg.Learn.Kind, cfg.Learn.Model, tc.wantKind, tc.wantModel)
				}
				if got := cfg.LearnRole().Model; got != tc.wantModel {
					t.Fatalf("the retro role's model = %q, want %q", got, tc.wantModel)
				}
			})
		}
	}
}

// A later layer that moves the kind back to claude gets claude's default
// again, so the model never depends on which layer set what.
func TestLearnDefaultModelIsRecomputedWhenALaterLayerChangesTheKind(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"config.toml": minimalConfig + "[learn]\nkind = \"codex\"\n",
		"user.toml":   "[learn]\nkind = \"claude\"\n",
	})
	if cfg.Learn.Kind != "claude" || cfg.Learn.Model != "sonnet" {
		t.Fatalf("learn kind, model = %q, %q; want claude, sonnet", cfg.Learn.Kind, cfg.Learn.Model)
	}
	// A model the base chose stays when the user layer leaves the kind alone.
	cfg = mustLoad(t, map[string]string{
		"config.toml": minimalConfig + "[learn]\nkind = \"codex\"\nmodel = \"gpt-5\"\n",
		"user.toml":   "[learn]\nenabled = true\n",
	})
	if cfg.Learn.Kind != "codex" || cfg.Learn.Model != "gpt-5" {
		t.Fatalf("learn kind, model = %q, %q; want codex, gpt-5", cfg.Learn.Kind, cfg.Learn.Model)
	}
}

// DefaultLearnModel is what the section's documentation says: sonnet for
// claude, nothing (the kind's own default) for any other kind.
func TestDefaultLearnModel(t *testing.T) {
	for kind, want := range map[string]string{KindClaude: "sonnet", KindCodex: "", "droid": "", "": ""} {
		if got := DefaultLearnModel(kind); got != want {
			t.Errorf("DefaultLearnModel(%q) = %q, want %q", kind, got, want)
		}
	}
	if got := DefaultLearn().Model; got != DefaultLearnModel(KindClaude) {
		t.Errorf("DefaultLearn().Model = %q", got)
	}
}
