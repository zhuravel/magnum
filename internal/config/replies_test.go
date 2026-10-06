package config

import (
	"strings"
	"testing"
	"time"
)

// Replies on magnum's threads start a judge-only round 3 minutes after the
// last one by default ("0" turns that off), at most one per PR and head
// every 2 hours; negative values are refused.
func TestReplyDebounceDefaultsTo3MinutesAndAtMostOneRoundPerHeadEvery2Hours(t *testing.T) {
	d := Defaults().Daemon
	if d.ReplyDebounce.Duration != 3*time.Minute || d.ReplyMinInterval.Duration != 2*time.Hour {
		t.Fatalf("Defaults(): reply_debounce %v, reply_min_interval %v; want 3m and 2h", d.ReplyDebounce, d.ReplyMinInterval)
	}
	if d := mustLoad(t, map[string]string{"config.toml": minimalConfig}).Daemon; d.ReplyDebounce.Duration != 3*time.Minute || d.ReplyMinInterval.Duration != 2*time.Hour {
		t.Fatalf("unset: reply_debounce %v, reply_min_interval %v", d.ReplyDebounce, d.ReplyMinInterval)
	}
	off := mustLoad(t, map[string]string{"config.toml": "[daemon]\nreply_debounce = \"0\"\nreply_min_interval = \"30m\"\n" + minimalConfig}).Daemon
	if off.ReplyDebounce.Duration != 0 || off.ReplyMinInterval.Duration != 30*time.Minute {
		t.Fatalf(`reply_debounce = "0", reply_min_interval = "30m": %v, %v`, off.ReplyDebounce, off.ReplyMinInterval)
	}
	for _, key := range []string{"reply_debounce", "reply_min_interval"} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[daemon]\n" + key + " = \"-1m\"\n" + minimalConfig})
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("a negative %s: err = %v, want one naming it", key, err)
		}
	}
}
