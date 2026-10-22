package config

import (
	"fmt"
	"time"
)

// validateOpenHold checks [daemon] open_hold: 0 (a lasting pin) or a
// positive duration.
func (d Daemon) validateOpenHold() []error {
	if d.OpenHold.Duration < 0 {
		return []error{fmt.Errorf("daemon.open_hold must be 0 (a lasting pin) or positive, got %s", d.OpenHold.Duration)}
	}
	return nil
}

// OpenHoldEnd is when a hold `magnum open` set ends: until (the open plus
// open_hold), or the end of human_cooldown after humanAt (the PR's
// human_active_at, the last input in its panes) when that is later.
func (d Daemon) OpenHoldEnd(until time.Time, humanAt *time.Time) time.Time {
	if c := d.CooldownUntil(humanAt); c.After(until) {
		return c
	}
	return until
}
