package config

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func anyMatch(res []*regexp.Regexp, s string) bool {
	return slices.ContainsFunc(res, func(re *regexp.Regexp) bool { return re.MatchString(s) })
}

// A model's own cap ("You've reached your Fable limit") is a model_limit, an
// account-wide cap is a usage_limit, and no pane text is both: the engine
// checks model_limit first, so a usage pattern that also matched a model name
// would pause the whole kind instead of switching the session's model.
func TestResetModelLimitPatternsSeparateModelFromAccountLimits(t *testing.T) {
	h, err := DefaultHealthPatterns().Compile()
	if err != nil {
		t.Fatal(err)
	}
	if len(DefaultHealthPatterns().ModelLimit) != 3 {
		t.Fatalf("default model_limit patterns = %d, want 3", len(DefaultHealthPatterns().ModelLimit))
	}
	if len(h.ModelLimit) != 3 {
		t.Fatalf("compiled model_limit = %d, want 3", len(h.ModelLimit))
	}
	model := map[string]string{
		"You've reached your Fable limit. /model to switch models.": "",
		"You've reached your Fable limit":                           "fable",
		"You’ve reached your Fable limit":                           "fable", // curly apostrophe
		"Opus limit reached":                                        "opus",
		"You've hit your Opus limit · resets 3:45pm":                "opus",
		"you've hit your sonnet weekly limit":                       "sonnet",
		"Press /model to switch models":                             "",
	}
	for text := range model {
		if !anyMatch(h.ModelLimit, text) {
			t.Errorf("model_limit does not match %q", text)
		}
		if anyMatch(h.UsageLimit, text) {
			t.Errorf("usage_limit also matches the per-model text %q", text)
		}
	}
	account := []string{
		"You've hit your limit · resets 3pm",
		"You've hit your usage limit",
		"You've reached your weekly limit",
		"5-hour limit reached",
		"Weekly limit reached",
		"Usage limit reached",
	}
	for _, text := range account {
		if !anyMatch(h.UsageLimit, text) {
			t.Errorf("usage_limit does not match %q", text)
		}
		if anyMatch(h.ModelLimit, text) {
			t.Errorf("model_limit also matches the account-wide text %q", text)
		}
	}
	// Unrelated pane text matches neither.
	for _, text := range []string{"Thinking about the diff", "model: opus", "limit"} {
		if anyMatch(h.ModelLimit, text) || anyMatch(h.UsageLimit, text) {
			t.Errorf("%q must not be a limit", text)
		}
	}
}

// The first two default model_limit patterns name the model in a (?P<model>)
// group: the health check reads it to learn which model is limited.
func TestResetModelLimitPatternsCaptureTheModel(t *testing.T) {
	h, err := DefaultHealthPatterns().Compile()
	if err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]string{
		"You've reached your Fable limit. /model to switch models.": "fable",
		"You've hit your Opus limit · resets 3:45pm":                "opus",
		"Sonnet limit reached":                                      "sonnet",
	} {
		var got string
		for _, re := range h.ModelLimit {
			if i := re.SubexpIndex("model"); i >= 0 {
				if m := re.FindStringSubmatch(text); m != nil {
					got = strings.ToLower(m[i])
					break
				}
			}
		}
		if got != want {
			t.Errorf("model captured from %q = %q, want %q", text, got, want)
		}
	}
}

func TestBuiltInKindsSwitchModels(t *testing.T) {
	kinds := DefaultKinds()
	claude := kinds[KindClaude]
	if claude.SwitchModel != "/model {model}" || !slices.Equal(claude.FallbackModels, []string{"opus", "sonnet"}) || claude.ResetModel != "default" {
		t.Fatalf("claude = %+v", claude)
	}
	if got := claude.SwitchModelCommand("opus"); got != "/model opus" {
		t.Errorf("SwitchModelCommand(opus) = %q", got)
	}
	if got := claude.SwitchModelCommand(claude.ResetModel); got != "/model default" {
		t.Errorf("SwitchModelCommand(default) = %q", got)
	}
	if got := claude.SwitchModelCommand(""); got != "" {
		t.Errorf("SwitchModelCommand(\"\") = %q, want nothing to type", got)
	}
	for _, name := range []string{KindCodex, KindDroid, KindOMP} {
		k, ok := kinds[name]
		if !ok {
			t.Fatalf("no built-in kind %q", name)
		}
		if k.SwitchModel != "" || len(k.FallbackModels) != 0 || k.ResetModel != "" {
			t.Errorf("%s must not switch models in-session: %+v", name, k)
		}
		if got := k.SwitchModelCommand("opus"); got != "" {
			t.Errorf("%s SwitchModelCommand = %q, want empty", name, got)
		}
	}
	// Every call returns fresh copies: editing one kind's fallbacks leaves
	// the next call's defaults alone.
	kinds[KindClaude].FallbackModels[0] = "mutated"
	if got := DefaultKinds()[KindClaude].FallbackModels[0]; got != "opus" {
		t.Fatalf("DefaultKinds shares FallbackModels: first = %q", got)
	}
	// The built-in kinds validate as they are.
	cfg := validPipelineConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if got := cfg.Daemon.ModelLimitCooldown.Duration; got != 5*time.Hour {
		t.Fatalf("default model_limit_cooldown = %s, want 5h", got)
	}
}

func TestValidateModelLimitKeys(t *testing.T) {
	edit := func(kind string, f func(*Kind)) func(*Config) {
		return func(c *Config) {
			k := c.Kinds[kind]
			f(&k)
			c.Kinds[kind] = k
		}
	}
	cases := map[string]struct {
		edit func(*Config)
		want string // "" = valid
	}{
		"switch_model without placeholder":   {edit("claude", func(k *Kind) { k.SwitchModel = "/model" }), "switch_model"},
		"switch_model placeholder":           {edit("claude", func(k *Kind) { k.SwitchModel = "/model {model}" }), ""},
		"fallbacks without switch_model":     {edit("codex", func(k *Kind) { k.FallbackModels = []string{"o3"} }), ""},
		"switch_model alone is valid":        {edit("codex", func(k *Kind) { k.SwitchModel = "/model {model}" }), ""},
		"fallback with a space":              {edit("claude", func(k *Kind) { k.FallbackModels = []string{"opus 4"} }), "must be one word"},
		"fallback with a newline":            {edit("claude", func(k *Kind) { k.FallbackModels = []string{"opus\n/exit"} }), "must be one word"},
		"fallback with a tab":                {edit("claude", func(k *Kind) { k.FallbackModels = []string{"so\tnnet"} }), "must be one word"},
		"fallback with an escape":            {edit("claude", func(k *Kind) { k.FallbackModels = []string{"opus\x1b"} }), "must be one word"},
		"empty fallback":                     {edit("claude", func(k *Kind) { k.FallbackModels = []string{"opus", ""} }), "must be one word"},
		"duplicate fallback":                 {edit("claude", func(k *Kind) { k.FallbackModels = []string{"opus", "sonnet", "opus"} }), `lists "opus" twice`},
		"duplicate fallback, other case":     {edit("claude", func(k *Kind) { k.FallbackModels = []string{"opus", "OPUS"} }), `lists "OPUS" twice`},
		"model ids with dashes and brackets": {edit("claude", func(k *Kind) { k.FallbackModels = []string{"claude-opus-4-1", "sonnet[1m]"} }), ""},
		"reset_model with a space":           {edit("claude", func(k *Kind) { k.ResetModel = "the default" }), "reset_model"},
		"reset_model with a control char":    {edit("claude", func(k *Kind) { k.ResetModel = "def\x1bault" }), "reset_model"},
		"reset_model one word":               {edit("claude", func(k *Kind) { k.ResetModel = "sonnet" }), ""},
		"no fallbacks at all":                {edit("claude", func(k *Kind) { k.FallbackModels = nil; k.ResetModel = "" }), ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(cfg)
			cfg.Normalize()
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want valid", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
			if name != "switch_model without placeholder" && !strings.Contains(err.Error(), "kinds.") {
				t.Errorf("error %q does not name the kind", err)
			}
		})
	}
}

func TestValidateModelLimitCooldown(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "model_limit_cooldown must be at least 1m"},
		{-time.Hour, "model_limit_cooldown must be at least 1m"},
		{30 * time.Second, "model_limit_cooldown must be at least 1m"},
		{time.Minute - time.Nanosecond, "model_limit_cooldown must be at least 1m"},
		{time.Minute, ""},
		{5 * time.Hour, ""},
		{48 * time.Hour, ""},
	} {
		cfg := validPipelineConfig()
		cfg.Daemon.ModelLimitCooldown = Duration{tc.d}
		err := cfg.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("cooldown %s: %v", tc.d, err)
			}
			continue
		}
		wantError(t, err, "daemon.", tc.want)
	}
}

func TestLoadModelLimitKeysMergeKeyByKey(t *testing.T) {
	// Setting fallback_models alone keeps the built-in switch_model and
	// reset_model: the kinds merge key by key.
	cfg := mustLoad(t, map[string]string{
		"config.toml": minimalConfig + "[kinds.claude]\nfallback_models = [\"sonnet\"]\n",
	})
	claude, ok := cfg.KindSpec("claude")
	if !ok {
		t.Fatal("no claude kind")
	}
	if !slices.Equal(claude.FallbackModels, []string{"sonnet"}) || claude.SwitchModel != "/model {model}" || claude.ResetModel != "default" {
		t.Fatalf("claude = %+v", claude)
	}
	if got := claude.SwitchModelCommand("sonnet"); got != "/model sonnet" {
		t.Fatalf("SwitchModelCommand = %q", got)
	}
	// Other kinds are untouched.
	if codex, _ := cfg.KindSpec("codex"); codex.SwitchModel != "" || len(codex.FallbackModels) != 0 {
		t.Fatalf("codex = %+v", codex)
	}
	// And the committed health patterns stay (nothing replaced them).
	if !slices.Equal(claude.HealthPatterns.ModelLimit, DefaultHealthPatterns().ModelLimit) {
		t.Fatalf("claude model_limit patterns = %q", claude.HealthPatterns.ModelLimit)
	}

	// The overlay overrides the committed file key by key too.
	cfg = mustLoad(t, map[string]string{
		"config.toml":       minimalConfig + "[kinds.claude]\nfallback_models = [\"sonnet\"]\nreset_model = \"opus\"\n",
		"config.local.toml": "[kinds.claude]\nfallback_models = [\"haiku\", \"sonnet\"]\n",
	})
	claude, _ = cfg.KindSpec("claude")
	if !slices.Equal(claude.FallbackModels, []string{"haiku", "sonnet"}) || claude.ResetModel != "opus" || claude.SwitchModel != "/model {model}" {
		t.Fatalf("claude with overlay = %+v", claude)
	}

	// An explicit empty list or string disables a built-in default.
	cfg = mustLoad(t, map[string]string{
		"config.toml": minimalConfig + "[kinds.claude]\nswitch_model = \"\"\nfallback_models = []\nreset_model = \"\"\n",
	})
	claude, _ = cfg.KindSpec("claude")
	if claude.SwitchModel != "" || len(claude.FallbackModels) != 0 || claude.ResetModel != "" {
		t.Fatalf("claude with the switch disabled = %+v", claude)
	}
	if got := claude.SwitchModelCommand("opus"); got != "" {
		t.Fatalf("SwitchModelCommand on a disabled switch = %q", got)
	}

	// A new kind can declare its own switch command and fallbacks.
	cfg = mustLoad(t, map[string]string{
		"config.toml": minimalConfig + "[kinds.pi]\nswitch_model = \"/m {model}\"\nfallback_models = [\"big\", \"small\"]\n",
	})
	if pi, _ := cfg.KindSpec("pi"); pi.SwitchModelCommand("small") != "/m small" || !slices.Equal(pi.FallbackModels, []string{"big", "small"}) {
		t.Fatalf("pi = %+v", pi)
	}

	// A custom model_limit pattern list replaces that list only.
	cfg = mustLoad(t, map[string]string{
		"config.toml": minimalConfig + "[kinds.claude.health_patterns]\nmodel_limit = [\"custom model cap\"]\n",
	})
	claude, _ = cfg.KindSpec("claude")
	if !slices.Equal(claude.HealthPatterns.ModelLimit, []string{"custom model cap"}) ||
		!slices.Equal(claude.HealthPatterns.UsageLimit, DefaultHealthPatterns().UsageLimit) {
		t.Fatalf("claude health = %+v", claude.HealthPatterns)
	}
	h, err := claude.HealthPatterns.Compile()
	if err != nil || len(h.ModelLimit) != 1 || !h.ModelLimit[0].MatchString("A CUSTOM MODEL CAP hit") {
		t.Fatalf("compile: %v %+v", err, h)
	}
}

func TestLoadRejectsBadModelLimitKinds(t *testing.T) {
	for name, tc := range map[string]struct{ toml, want string }{
		"no placeholder":  {"[kinds.claude]\nswitch_model = \"/model\"\n", "switch_model"},
		"spaced fallback": {"[kinds.claude]\nfallback_models = [\"opus 4\"]\n", "must be one word"},
		"duplicate":       {"[kinds.claude]\nfallback_models = [\"opus\", \"Opus\"]\n", "twice"},
		"spaced default":  {"[kinds.claude]\nreset_model = \"the default\"\n", "reset_model"},
		"unknown key":     {"[kinds.claude]\nfallback_model = [\"opus\"]\n", "unknown keys"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + tc.toml})
			wantError(t, err, tc.want)
		})
	}
}

func TestLoadModelLimitCooldown(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig})
	if got := cfg.Daemon.ModelLimitCooldown.Duration; got != 5*time.Hour {
		t.Fatalf("default model_limit_cooldown = %s, want 5h", got)
	}

	cfg = mustLoad(t, map[string]string{"config.toml": minimalConfig + "[daemon]\nmodel_limit_cooldown = \"2h\"\n"})
	if got := cfg.Daemon.ModelLimitCooldown.Duration; got != 2*time.Hour {
		t.Fatalf("model_limit_cooldown = %s, want 2h", got)
	}
	// The setting sits next to the other daemon keys, which keep their defaults.
	if got := cfg.Daemon.BurstWindow.Duration; got != 30*time.Minute {
		t.Fatalf("burst_window = %s, want its default", got)
	}

	// The overlay overrides the committed value.
	cfg = mustLoad(t, map[string]string{
		"config.toml":       minimalConfig + "[daemon]\nmodel_limit_cooldown = \"2h\"\n",
		"config.local.toml": "[daemon]\nmodel_limit_cooldown = \"90m\"\n",
	})
	if got := cfg.Daemon.ModelLimitCooldown.Duration; got != 90*time.Minute {
		t.Fatalf("overlay model_limit_cooldown = %s, want 90m", got)
	}
	// An overlay that leaves it out keeps the committed one.
	cfg = mustLoad(t, map[string]string{
		"config.toml":       minimalConfig + "[daemon]\nmodel_limit_cooldown = \"2h\"\n",
		"config.local.toml": "[daemon]\nburst_pushes = 4\n",
	})
	if got := cfg.Daemon.ModelLimitCooldown.Duration; got != 2*time.Hour {
		t.Fatalf("model_limit_cooldown with an unrelated overlay = %s, want 2h", got)
	}

	for _, v := range []string{"30s", "0s", "59s"} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + "[daemon]\nmodel_limit_cooldown = \"" + v + "\"\n"})
		wantError(t, err, "model_limit_cooldown must be at least 1m")
	}
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + "[daemon]\nmodel_limit_cooldown = \"soon\"\n"})
	if err == nil {
		t.Fatal("model_limit_cooldown = \"soon\" must not load")
	}
}

// switch_model = "" alone turns in-session switching off: the default
// fallback_models stay, unused, and the config loads.
func TestLoadSwitchModelClearedTurnsSwitchingOff(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + "[kinds.claude]\nswitch_model = \"\"\n"})
	claude, _ := cfg.KindSpec("claude")
	if claude.SwitchModel != "" || claude.SwitchModelCommand("opus") != "" {
		t.Fatalf("claude switch_model = %q, want none", claude.SwitchModel)
	}
}

// A kind's default_model is the model of its roles without their own,
// passed through the kind's model args.
func TestKindDefaultModel(t *testing.T) {
	k := Kind{Model: []string{"--model", PlaceholderModel}, DefaultModel: "gpt-6.1-sol"}
	if got := k.Argv(LaunchArgs{}); !slices.Equal(got, []string{"--model", "gpt-6.1-sol"}) {
		t.Fatalf("argv = %q, want the kind's default model", got)
	}
	if got := k.Argv(LaunchArgs{Model: "o3"}); !slices.Equal(got, []string{"--model", "o3"}) {
		t.Fatalf("argv = %q, want the role's own model", got)
	}

	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + "[kinds.codex]\ndefault_model = \"gpt-6.1-sol\"\n"})
	judge := cfg.JudgeFor(nil)
	if got := cfg.RoleModel(judge); got != "gpt-6.1-sol" {
		t.Fatalf("judge model = %q, want the codex default", got)
	}
	own := judge
	own.Model = "o3"
	if got := cfg.RoleModel(own); got != "o3" {
		t.Fatalf("judge with its own model = %q", got)
	}
	if review, ok := cfg.RoleByNameOrAlias(nil, RoleCodexReview); !ok || cfg.RoleModel(review) != "" {
		t.Fatalf("a shell role keeps its own model only (codex review takes -c model=...): %q", cfg.RoleModel(review))
	}
	if claude, _ := cfg.KindSpec(KindClaude); claude.DefaultModel != "" {
		t.Fatalf("claude default_model = %q, want none (the CLI's default)", claude.DefaultModel)
	}
}

func TestLoadRejectsBadKindDefaultModel(t *testing.T) {
	for name, tc := range map[string]struct{ toml, want string }{
		"no model args": {"[kinds.droid]\ndefault_model = \"glm-5\"\n", "needs model args"},
		"two words":     {"[kinds.codex]\ndefault_model = \"gpt 6\"\n", "must be one word"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + tc.toml})
			wantError(t, err, tc.want)
		})
	}
}
