package config

import (
	"strings"
	"testing"
	"time"
)

func TestUsageAndParkIdleDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Usage.CodexSoft != 80 || cfg.Usage.CodexHard != 95 || cfg.Usage.CodexHome != "" {
		t.Fatalf("usage defaults: %+v", cfg.Usage)
	}
	if cfg.Daemon.ParkIdleAfter.Duration != 2*time.Hour {
		t.Fatalf("park_idle_after default: %v", cfg.Daemon.ParkIdleAfter)
	}
}

func TestValidateUsageAndParkIdle(t *testing.T) {
	for _, tc := range []struct {
		name       string
		soft, hard float64
		park       time.Duration
		want       string
	}{
		{name: "defaults", soft: 80, hard: 95, park: 2 * time.Hour},
		{name: "both off", soft: 0, hard: 0, park: 0},
		{name: "only hard", soft: 0, hard: 90, park: time.Minute},
		{name: "only soft", soft: 70, hard: 0, park: time.Hour},
		{name: "soft above hard", soft: 96, hard: 95, park: time.Hour, want: "usage.codex_soft (96) must be below usage.codex_hard (95)"},
		{name: "negative", soft: -1, hard: 95, park: time.Hour, want: "usage.codex_soft must be a percentage between 0 (off) and 100, got -1"},
		{name: "over 100", soft: 80, hard: 120, park: time.Hour, want: "usage.codex_hard must be a percentage between 0 (off) and 100, got 120"},
		{name: "park too short", soft: 80, hard: 95, park: 30 * time.Second, want: "daemon.park_idle_after must be 0 (never) or at least 1m, got 30s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Usage.CodexSoft, cfg.Usage.CodexHard = tc.soft, tc.hard
			cfg.Daemon.ParkIdleAfter.Duration = tc.park
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

func TestUsageOverlayAndHomeExpansion(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+"\n[usage]\ncodex_soft = 60\ncodex_home = \"~/.codex-work\"\n\n[daemon]\npark_idle_after = \"0\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Usage.CodexSoft != 60 || cfg.Usage.CodexHard != 95 {
		t.Fatalf("overlay keeps codex_hard and overrides codex_soft: %+v", cfg.Usage)
	}
	if strings.HasPrefix(cfg.Usage.CodexHome, "~") || !strings.HasSuffix(cfg.Usage.CodexHome, "/.codex-work") {
		t.Fatalf("codex_home not expanded: %q", cfg.Usage.CodexHome)
	}
	if cfg.Daemon.ParkIdleAfter.Duration != 0 {
		t.Fatalf("park_idle_after overlay: %v", cfg.Daemon.ParkIdleAfter)
	}
}
