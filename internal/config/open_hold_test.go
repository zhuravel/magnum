package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

// [daemon] open_hold is 30m by default, in Defaults() and in
// config.defaults.toml loaded alone, and a user config may set it to "0"
// (a lasting pin).
func TestOpenHoldIsThirtyMinutesByDefault(t *testing.T) {
	if got := Defaults().Daemon.OpenHold.Duration; got != 30*time.Minute {
		t.Fatalf("open_hold default: %v, want 30m", got)
	}
	root := repoRoot(t)
	standalone, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.defaults.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatalf("config.defaults.toml does not load standalone: %v", err)
	}
	if got := standalone.Daemon.OpenHold.Duration; got != 30*time.Minute {
		t.Fatalf("config.defaults.toml alone holds an open for %v, want 30m", got)
	}
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+"\n[daemon]\nopen_hold = \"0\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.OpenHold.Duration != 0 {
		t.Fatalf("open_hold overlay: %v, want 0", cfg.Daemon.OpenHold)
	}
}

// open_hold is 0 (a lasting pin) or positive; a negative one is refused.
func TestValidateOpenHold(t *testing.T) {
	for _, tc := range []struct {
		hold time.Duration
		want string
	}{
		{hold: 30 * time.Minute},
		{hold: 0},
		{hold: time.Second},
		{hold: -time.Minute, want: "daemon.open_hold must be 0 (a lasting pin) or positive, got -1m0s"},
	} {
		t.Run(tc.hold.String(), func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Daemon.OpenHold.Duration = tc.hold
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

// A hold ends at the open plus open_hold, or at human_cooldown after the last
// input in the PR's panes when that is later.
func TestAnOpenHoldEndsAfterTheLastInput(t *testing.T) {
	d := Defaults().Daemon // human_cooldown 15m
	until := time.Date(2026, 10, 22, 3, 21, 0, 0, time.UTC)
	early, late := until.Add(-20*time.Minute), until.Add(-5*time.Minute)
	for _, tc := range []struct {
		name    string
		humanAt *time.Time
		want    time.Time
	}{
		{"no input", nil, until},
		{"input long before", &early, until},
		{"input near the end", &late, late.Add(15 * time.Minute)},
	} {
		if got := d.OpenHoldEnd(until, tc.humanAt); !got.Equal(tc.want) {
			t.Errorf("%s: end %v, want %v", tc.name, got, tc.want)
		}
	}
}
