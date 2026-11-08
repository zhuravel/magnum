package config

import (
	"testing"
	"time"
)

// The review daemon observes herdr on a clock of its own, every
// observe_interval (5 s by default), between the GitHub poll's ticks: a
// turn's end is seen within seconds instead of the 72 s the tick took
// (median, 2026-11-07). "0" turns the clock off (the tick observes alone,
// as before); anything else is 2 s or more, so the observe pass's own cost
// stays small, and no longer than the poll interval, which would make the
// clock slower than the tick.
func TestObserveIntervalIsOffOrBetweenTwoSecondsAndThePollInterval(t *testing.T) {
	if got := Defaults().Daemon.ObserveInterval.Duration; got != 5*time.Second {
		t.Fatalf("default observe_interval = %s, want 5s", got)
	}
	cases := []struct {
		name string
		d    time.Duration
		want string // "" = valid
	}{
		{"off", 0, ""},
		{"the floor", 2 * time.Second, ""},
		{"the default", 5 * time.Second, ""},
		{"the poll interval", 30 * time.Second, ""},
		{"below the floor", time.Second, "daemon.observe_interval must be 0 (off) or between 2s and poll_interval (30s), got 1s"},
		{"above the poll interval", 31 * time.Second, "daemon.observe_interval must be 0 (off) or between 2s and poll_interval (30s), got 31s"},
		{"negative", -5 * time.Second, "daemon.observe_interval must be 0 (off) or between 2s and poll_interval (30s), got -5s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Daemon.ObserveInterval = Duration{tc.d}
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

// The key in a user config is read and checked like the others.
func TestLoadReadsObserveInterval(t *testing.T) {
	cfg, err := loadFiles(t, t.TempDir(), map[string]string{
		"config.toml": minimalConfig,
		"user.toml":   "[daemon]\nobserve_interval = \"10s\"\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Daemon.ObserveInterval.Duration; got != 10*time.Second {
		t.Fatalf("observe_interval = %s, want 10s", got)
	}
	_, err = loadFiles(t, t.TempDir(), map[string]string{
		"config.toml": minimalConfig,
		"user.toml":   "[daemon]\nobserve_interval = \"1m\"\n",
	})
	wantError(t, err, "daemon.observe_interval")
}
