package store

import (
	"context"
	"testing"
	"time"
)

// SetKV with the value a key already holds writes nothing (the daemon sets
// the same gate, wait and pause keys every tick); a new value, such as the
// daemon.last_tick heartbeat's, is written with its time.
func TestSetKVSkipsAnUnchangedValue(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	updatedAt := func(key string) string {
		t.Helper()
		var at string
		if err := st.db.QueryRowContext(ctx, "SELECT updated_at FROM kv WHERE key = ?", key).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if err := st.SetKV(ctx, "pr.7.gate", "slot"); err != nil {
		t.Fatal(err)
	}
	first := updatedAt("pr.7.gate")
	clk.Add(time.Minute)
	if err := st.SetKV(ctx, "pr.7.gate", "slot"); err != nil {
		t.Fatal(err)
	}
	if got := updatedAt("pr.7.gate"); got != first {
		t.Fatalf("an unchanged value was written again: updated_at %s, was %s", got, first)
	}

	if err := st.SetKV(ctx, KVDaemonLastTick, FormatTime(clk.Now())); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	if err := st.SetKV(ctx, KVDaemonLastTick, FormatTime(clk.Now())); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := st.GetKV(ctx, KVDaemonLastTick); err != nil || !ok || v != FormatTime(clk.Now()) {
		t.Fatalf("heartbeat = %q, %v, %v; want the new tick", v, ok, err)
	}
	if got := updatedAt(KVDaemonLastTick); got != FormatTime(clk.Now()) {
		t.Fatalf("heartbeat updated_at = %s, want %s", got, FormatTime(clk.Now()))
	}
	if err := st.SetKV(ctx, "pr.7.gate", "human"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := st.GetKV(ctx, "pr.7.gate"); v != "human" || updatedAt("pr.7.gate") != FormatTime(clk.Now()) {
		t.Fatalf("a changed value = %q at %s", v, updatedAt("pr.7.gate"))
	}
}
