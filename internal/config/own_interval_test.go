package config

import (
	"testing"
	"time"
)

// own_min_rereview_interval is off ("0": the normal interval) by default;
// [daemon] sets it and a [[watch]] overrides the daemon's, a zero keeping it.
func TestOwnMinRereviewIntervalLoadsAndAWatchMayOverrideIt(t *testing.T) {
	if d := Defaults().Daemon.OwnMinRereviewInterval.Duration; d != 0 {
		t.Fatalf("Defaults(): own_min_rereview_interval = %s, want 0", d)
	}
	cfg := mustLoad(t, map[string]string{"config.toml": `
[daemon]
own_min_rereview_interval = "2h"

[[identity]]
name = "z"
kind = "gh"
login = "z"

[[watch]]
owner = "acme"
include = ["*"]
identity = "z"
own_min_rereview_interval = "4h"

[[watch]]
owner = "example"
include = ["*"]
identity = "z"
`})
	if got := cfg.Daemon.OwnMinRereviewInterval.Duration; got != 2*time.Hour {
		t.Fatalf("[daemon] own_min_rereview_interval = %s", got)
	}
	if got := cfg.ThrottleFor(&cfg.Watches[0]).OwnMinRereviewInterval.Duration; got != 4*time.Hour {
		t.Errorf("ThrottleFor(acme) = %s, want the watch's 4h", got)
	}
	if got := cfg.ThrottleFor(&cfg.Watches[1]).OwnMinRereviewInterval.Duration; got != 2*time.Hour {
		t.Errorf("ThrottleFor(example) = %s, want the daemon's 2h", got)
	}
}

// A negative own interval is refused, in [daemon] and in a [[watch]].
func TestOwnMinRereviewIntervalMustNotBeNegative(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Daemon.OwnMinRereviewInterval = Duration{-time.Minute}
	wantError(t, cfg.Validate(), "daemon.own_min_rereview_interval must not be negative")

	cfg = validPipelineConfig()
	cfg.Watches[0].OwnMinRereviewInterval = Duration{-time.Minute}
	wantError(t, cfg.Validate(), "watch "+cfg.Watches[0].Owner+": own_min_rereview_interval must not be negative")
}
