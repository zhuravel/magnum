package config

import "testing"

// delta_check is on by default; [daemon] turns it off and a [[watch]]
// overrides the daemon's either way.
func TestDeltaCheckKeyDefaultsOnAndAWatchMayOverrideIt(t *testing.T) {
	if !Defaults().Daemon.DeltaCheck {
		t.Fatal("Defaults(): delta_check is off")
	}
	if cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig}); !cfg.Daemon.DeltaCheck || cfg.Watches[0].DeltaCheck != nil {
		t.Fatalf("unset: daemon %v, watch %v", cfg.Daemon.DeltaCheck, cfg.Watches[0].DeltaCheck)
	}
	cfg := mustLoad(t, map[string]string{"config.toml": `
[daemon]
delta_check = false

[[identity]]
name = "z"
kind = "gh"
login = "z"

[[watch]]
owner = "acme"
include = ["*"]
identity = "z"
delta_check = true

[[watch]]
owner = "example"
include = ["*"]
identity = "z"
`})
	if cfg.Daemon.DeltaCheck {
		t.Error("[daemon] delta_check = false did not load")
	}
	if !cfg.ThrottleFor(&cfg.Watches[0]).DeltaCheck || cfg.ThrottleFor(&cfg.Watches[1]).DeltaCheck {
		t.Errorf("ThrottleFor: acme %v, example %v; want the watch's true, the daemon's false",
			cfg.ThrottleFor(&cfg.Watches[0]).DeltaCheck, cfg.ThrottleFor(&cfg.Watches[1]).DeltaCheck)
	}
}
