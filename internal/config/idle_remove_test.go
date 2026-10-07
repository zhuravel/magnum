package config

import (
	"strings"
	"testing"
	"time"
)

// A pool without idle_remove_after keeps a surplus free slot for a week
// (DefaultIdleRemoveAfter) instead of removing it at the next reconcile:
// each removal is about 1 GB written again by the next provision. An
// explicit value is kept.
func TestPoolIdleRemoveAfterDefaultsToAWeek(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig)
	if err != nil {
		t.Fatal(err)
	}
	if DefaultIdleRemoveAfter != 168*time.Hour {
		t.Fatalf("DefaultIdleRemoveAfter = %s, want 168h", DefaultIdleRemoveAfter)
	}
	if got := cfg.Pools[0].IdleRemoveAfter.Duration; got != DefaultIdleRemoveAfter {
		t.Fatalf("default idle_remove_after = %s, want 168h", got)
	}
	cfg, err = loadCommittedWithLocal(t, strings.Replace(testLocalConfig, "min = 1\n", "min = 1\nidle_remove_after = \"36h\"\n", 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Pools[0].IdleRemoveAfter.Duration; got != 36*time.Hour {
		t.Fatalf("explicit idle_remove_after = %s, want 36h", got)
	}
}

// A negative idle_remove_after loaded and removed every surplus free slot at
// the next reconcile; it is refused, naming the pool and the key.
func TestPoolRefusesANegativeIdleRemoveAfter(t *testing.T) {
	_, err := loadCommittedWithLocal(t, strings.Replace(testLocalConfig, "min = 1\n", "min = 1\nidle_remove_after = \"-1h\"\n", 1))
	if err == nil || !strings.Contains(err.Error(), "pool talkable/talkable: idle_remove_after must not be negative") {
		t.Fatalf("err = %v, want one naming the pool's idle_remove_after", err)
	}
}
