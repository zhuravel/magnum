package config

import (
	"testing"
	"time"
)

// A zero or negative poll or reconcile interval used to load clean and then
// disable the guard (the engine patched the poll interval in three places, and
// a reconcile interval of 0 skipped every reconcile, the retention with it); a
// negative wait or quiet period released a slot, or reviewed a push, at once.
func TestValidateDaemonTimings(t *testing.T) {
	minute := func(n int) Duration { return Duration{time.Duration(n) * time.Minute} }
	cases := []struct {
		name string
		edit func(*Daemon)
		want string // "" = valid
	}{
		{"defaults", func(*Daemon) {}, ""},
		{"zero poll_interval", func(d *Daemon) { d.PollInterval = Duration{} }, "daemon.poll_interval must be positive, got 0s"},
		{"negative poll_interval", func(d *Daemon) { d.PollInterval = Duration{-time.Second} }, "daemon.poll_interval must be positive, got -1s"},
		{"zero reconcile_interval", func(d *Daemon) { d.ReconcileInterval = Duration{} }, "daemon.reconcile_interval must be positive, got 0s"},
		{"negative reconcile_interval", func(d *Daemon) { d.ReconcileInterval = minute(-5) }, "daemon.reconcile_interval must be positive, got -5m0s"},
		{"zero close_grace", func(d *Daemon) { d.CloseGrace = Duration{} }, ""},
		{"negative close_grace", func(d *Daemon) { d.CloseGrace = Duration{-time.Hour} }, "daemon.close_grace must not be negative, got -1h0m0s"},
		{"zero push_quiet_period", func(d *Daemon) { d.PushQuietPeriod = Duration{} }, ""},
		{"negative push_quiet_period", func(d *Daemon) { d.PushQuietPeriod = minute(-5) }, "daemon.push_quiet_period must not be negative, got -5m0s"},
		{"zero min_rereview_interval", func(d *Daemon) { d.MinRereviewInterval = Duration{} }, ""},
		{"negative min_rereview_interval", func(d *Daemon) { d.MinRereviewInterval = minute(-1) }, "daemon.min_rereview_interval must not be negative"},
		{"negative draft_min_rereview_interval", func(d *Daemon) { d.DraftMinRereviewInterval = minute(-1) }, "daemon.draft_min_rereview_interval must not be negative"},
		{"negative agent_start_stagger", func(d *Daemon) { d.AgentStartStagger = Duration{-time.Second} }, "daemon.agent_start_stagger must not be negative"},
		{"negative min_warm", func(d *Daemon) { d.MinWarm = minute(-1) }, "daemon.min_warm must not be negative"},
		{"negative human_cooldown", func(d *Daemon) { d.HumanCooldown = minute(-1) }, "daemon.human_cooldown must not be negative"},
		{"negative reviewer_timeout", func(d *Daemon) { d.ReviewerTimeout = minute(-1) }, "daemon.reviewer_timeout must not be negative"},
		{"negative judge_timeout", func(d *Daemon) { d.JudgeTimeout = minute(-1) }, "daemon.judge_timeout must not be negative"},
		{"zero min_free_disk_gb", func(d *Daemon) { d.MinFreeDiskGB = 0 }, ""},
		{"negative min_free_disk_gb", func(d *Daemon) { d.MinFreeDiskGB = -3 }, "daemon.min_free_disk_gb must be >= 0, got -3"},
		{"default_repo unset", func(d *Daemon) { d.DefaultRepo = "" }, ""},
		{"default_repo owner/name", func(d *Daemon) { d.DefaultRepo = "talkable/magnum" }, ""},
		{"default_repo without owner", func(d *Daemon) { d.DefaultRepo = "justaname" }, `daemon.default_repo "justaname" must be owner/name`},
		{"default_repo without name", func(d *Daemon) { d.DefaultRepo = "talkable/" }, `daemon.default_repo "talkable/" must be owner/name`},
		{"default_repo without owner name", func(d *Daemon) { d.DefaultRepo = "/magnum" }, `daemon.default_repo "/magnum" must be owner/name`},
		{"default_repo with a path", func(d *Daemon) { d.DefaultRepo = "talkable/magnum/pulls" }, "must be owner/name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(&cfg.Daemon)
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

// The same keys set in a user config fail the load and name the key, so a typo
// in ~/.config/magnum/config.toml is refused rather than half applied.
func TestLoadRefusesBadDaemonTimings(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{`poll_interval = "0s"`, "daemon.poll_interval"},
		{`reconcile_interval = "-5m"`, "daemon.reconcile_interval"},
		{`close_grace = "-1h"`, "daemon.close_grace"},
		{`push_quiet_period = "-5m"`, "daemon.push_quiet_period"},
		{`min_free_disk_gb = -3`, "daemon.min_free_disk_gb"},
		{`default_repo = "justaname"`, "daemon.default_repo"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			_, err := loadFiles(t, t.TempDir(), map[string]string{
				"config.toml":       minimalConfig,
				"config.local.toml": "[daemon]\n" + tc.line + "\n",
			})
			wantError(t, err, tc.want)
		})
	}
}
