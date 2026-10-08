package config

import (
	"testing"
	"time"
)

// CooldownUntil is the one human-cooldown rule the agents' Submit, RunShell
// and observer and the engine's dispatch, parking, flag release and setup
// retry share: no human_active_at, or a human_cooldown of 0, holds nothing,
// and the cooldown ends exactly at human_active_at + human_cooldown.
func TestCooldownUntil(t *testing.T) {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	d := Daemon{HumanCooldown: Duration{15 * time.Minute}}
	end := at.Add(15 * time.Minute)

	if got := d.CooldownUntil(nil); !got.IsZero() {
		t.Errorf("nil human_active_at: %v, want zero (no cooldown)", got)
	}
	if got := d.CooldownUntil(&at); !got.Equal(end) {
		t.Fatalf("CooldownUntil = %v, want %v", got, end)
	}
	if inside := end.Add(-time.Second); !inside.Before(d.CooldownUntil(&at)) {
		t.Errorf("%v, inside the cooldown, is not before its end", inside)
	}
	if end.Before(d.CooldownUntil(&at)) {
		t.Errorf("at the end (%v) the cooldown still runs", end)
	}
	none := Daemon{}
	if got := none.CooldownUntil(&at); !got.IsZero() {
		t.Errorf("human_cooldown 0: %v, want zero (no cooldown)", got)
	}
	if at.Add(-time.Hour).Before(none.CooldownUntil(&at)) {
		t.Error("human_cooldown 0 holds a time before human_active_at")
	}
}
