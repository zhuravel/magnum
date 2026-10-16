package store

import (
	"context"
	"maps"
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

// SetKVIf and DeleteKVIf act only while the key still holds the value the
// caller read: a writer that lost the race (the key changed or went) learns
// it and changes nothing.
func TestSetKVIfAndDeleteKVIfCompareTheValue(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	if ok, err := st.SetKVIf(ctx, "pr.7.fix", "a", "b"); ok || err != nil {
		t.Fatalf("SetKVIf on a missing key = %v, %v", ok, err)
	}
	if err := st.SetKV(ctx, "pr.7.fix", "a"); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SetKVIf(ctx, "pr.7.fix", "x", "b"); ok || err != nil {
		t.Fatalf("SetKVIf from a stale value = %v, %v", ok, err)
	}
	if ok, err := st.SetKVIf(ctx, "pr.7.fix", "a", "b"); !ok || err != nil {
		t.Fatalf("SetKVIf from the current value = %v, %v", ok, err)
	}
	if v, _, _ := st.GetKV(ctx, "pr.7.fix"); v != "b" {
		t.Fatalf("value = %q, want b", v)
	}
	if ok, err := st.DeleteKVIf(ctx, "pr.7.fix", "a"); ok || err != nil {
		t.Fatalf("DeleteKVIf of a stale value = %v, %v", ok, err)
	}
	if _, found, _ := st.GetKV(ctx, "pr.7.fix"); !found {
		t.Fatal("a stale DeleteKVIf removed the key")
	}
	if ok, err := st.DeleteKVIf(ctx, "pr.7.fix", "b"); !ok || err != nil {
		t.Fatalf("DeleteKVIf of the current value = %v, %v", ok, err)
	}
	if _, found, _ := st.GetKV(ctx, "pr.7.fix"); found {
		t.Fatal("the key outlived DeleteKVIf")
	}
}

// KVValues reads the keys a glob matches with their values in one query
// (the board's pr.*.star): a key the glob does not match, a near miss
// included, stays out, and a glob that matches nothing reads an empty map.
func TestKVValuesReadsTheMatchingKeysWithTheirValues(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	for k, v := range map[string]string{
		KVPRStar(7): `{"at":"2026-10-16T12:00:00Z","by":"the board"}`, KVPRStar(12): `{"by":"magnum star"}`,
		"pr.7.star_x": "1", "pr.7.fix": "a", "board.hide_skipped": "1",
	} {
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.KVValues(ctx, "pr.*.star")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"pr.7.star": `{"at":"2026-10-16T12:00:00Z","by":"the board"}`, "pr.12.star": `{"by":"magnum star"}`}
	if !maps.Equal(got, want) {
		t.Fatalf("KVValues = %v, want %v", got, want)
	}
	if none, err := st.KVValues(ctx, "pr.*.nothing"); err != nil || len(none) != 0 {
		t.Fatalf("a glob that matches nothing = %v, %v", none, err)
	}
}
