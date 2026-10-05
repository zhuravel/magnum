package config

import (
	"testing"
	"time"
)

func TestRoundSpeedDefaults(t *testing.T) {
	cfg := Defaults()
	d := cfg.Daemon
	if d.MaxRoundRestarts != 2 || d.BurstQuietPeriod.Duration != 15*time.Minute || d.BurstPushes != 3 || d.BurstWindow.Duration != 30*time.Minute {
		t.Fatalf("daemon = restarts %d burst %s/%d/%s", d.MaxRoundRestarts, d.BurstQuietPeriod, d.BurstPushes, d.BurstWindow)
	}
	j := cfg.JudgeFor(nil)
	if j.RereviewEffort != "high" || j.EffortFor(true) != "high" || j.EffortFor(false) != "xhigh" {
		t.Fatalf("judge effort %q rereview %q", j.Effort, j.RereviewEffort)
	}
	if j.PromptFile(PromptRestart) != "" {
		t.Errorf("the judge has a restart prompt: %q", j.PromptFile(PromptRestart))
	}
	cr, _ := cfg.RoleByNameOrAlias(nil, "claude")
	if cr.PromptFile(PromptRestart) != "claude-restart.md" || cr.EffortFor(true) != "medium" {
		t.Errorf("claude-review restart %q rereview effort %q", cr.PromptFile(PromptRestart), cr.EffortFor(true))
	}
	for _, name := range []string{"codex-review", "claude-simplify"} {
		if r, _ := cfg.RoleByNameOrAlias(nil, name); r.PromptFile(PromptRestart) != "" {
			t.Errorf("%s restart prompt = %q, want none (it reruns its own prompt)", name, r.PromptFile(PromptRestart))
		}
	}
}

func TestRestartPromptDefaultsToNameRestartMD(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"config.toml": minimalConfig + `
[[role]]
name = "codex-judge"
[[role]]
name = "droid-review"
kind = "droid"
rereview_effort = "low"
`,
		"prompts/droid-review.md":         "review {{.URL}}",
		"prompts/droid-review-restart.md": "head moved from {{.RestartedFrom}} to {{.HeadSHA}}",
	})
	r, ok := cfg.RoleByNameOrAlias(nil, "droid-review")
	if !ok || r.PromptFile(PromptRestart) != "droid-review-restart.md" || r.RereviewEffort != "low" || r.EffortFor(false) != "" {
		t.Fatalf("droid-review = %+v", r)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestThrottleForAppliesWatchBurstOverrides(t *testing.T) {
	cfg := Defaults()
	if got := cfg.ThrottleFor(nil); got.BurstPushes != 3 {
		t.Fatalf("ThrottleFor(nil) = %+v", got)
	}
	off := 0
	w := Watch{BurstQuietPeriod: Duration{25 * time.Minute}, BurstPushes: &off}
	got := cfg.ThrottleFor(&w)
	if got.BurstQuietPeriod.Duration != 25*time.Minute || got.BurstPushes != 0 || got.BurstWindow.Duration != 30*time.Minute ||
		got.PushQuietPeriod != cfg.Daemon.PushQuietPeriod {
		t.Fatalf("ThrottleFor(watch) = %+v", got)
	}
	if cfg.Daemon.BurstQuietPeriod.Duration != 15*time.Minute || cfg.Daemon.BurstPushes != 3 {
		t.Fatalf("ThrottleFor changed the daemon section: %+v", cfg.Daemon)
	}
}

func TestRoundSpeedKeysLoad(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"config.toml": `
[daemon]
max_round_restarts = 1
burst_quiet_period = "20m"
burst_pushes = 4
burst_window = "1h"
` + minimalConfig + `burst_pushes = 0
burst_window = "45m"
`,
		"config.local.toml": `
[[role]]
name = "codex-judge"
rereview_effort = "medium"
`,
	})
	d := cfg.Daemon
	if d.MaxRoundRestarts != 1 || d.BurstQuietPeriod.Duration != 20*time.Minute || d.BurstPushes != 4 || d.BurstWindow.Duration != time.Hour {
		t.Fatalf("daemon = %+v", d)
	}
	w := cfg.Watches[0]
	if w.BurstPushes == nil || *w.BurstPushes != 0 || w.BurstWindow.Duration != 45*time.Minute || w.BurstQuietPeriod.Duration != 0 {
		t.Fatalf("watch burst = %v %v %v", w.BurstPushes, w.BurstWindow, w.BurstQuietPeriod)
	}
	if got := cfg.ThrottleFor(&w); got.BurstPushes != 0 || got.BurstWindow.Duration != 45*time.Minute || got.BurstQuietPeriod.Duration != 20*time.Minute {
		t.Fatalf("ThrottleFor = %+v", got)
	}
	if j := cfg.JudgeFor(nil); j.RereviewEffort != "medium" || j.Effort != "xhigh" {
		t.Fatalf("judge = effort %q rereview %q", j.Effort, j.RereviewEffort)
	}
}

func TestValidateRoundSpeedKeys(t *testing.T) {
	neg := -1
	for _, tc := range []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"restarts", func(c *Config) { c.Daemon.MaxRoundRestarts = -1 }, "daemon.max_round_restarts must be >= 0"},
		{"daemon pushes", func(c *Config) { c.Daemon.BurstPushes = -1 }, "daemon.burst_pushes must be >= 0"},
		{"daemon quiet", func(c *Config) { c.Daemon.BurstQuietPeriod.Duration = -time.Minute }, "daemon.burst_quiet_period must not be negative"},
		{"watch window", func(c *Config) { c.Watches[0].BurstWindow.Duration = -time.Minute }, "watch acme: burst_window must not be negative"},
		{"watch pushes", func(c *Config) { c.Watches[0].BurstPushes = &neg }, "watch acme: burst_pushes must be >= 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.mod(cfg)
			wantError(t, cfg.Validate(), tc.want)
		})
	}
	if err := validPipelineConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}
