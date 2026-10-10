package engine

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"
)

// TestFixStateRoundTrips: the fix loop's row is a PR's KVPRFix JSON: absent
// until the job starts, read back as it was written (the times by instant),
// replaced by the next write and gone once cleared. The stored fields are
// the ones the CLI reads, in snake case.
func TestFixStateRoundTrips(t *testing.T) {
	h := newHarness(t)
	const id = int64(7)

	if _, ok, err := h.e.fixState(h.ctx, id); ok || err != nil {
		t.Fatalf("fixState of a PR with no loop = %v, %v", ok, err)
	}

	started := time.Date(2026, 10, 10, 9, 30, 15, 0, time.FixedZone("EEST", 3*3600))
	want := FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Since: "aaa1111", Head: "bbb2222", By: "magnum fix",
		Report: "/state/reports/fix-1.md", StartedAt: started}
	if err := h.e.setFixState(h.ctx, id, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := h.e.fixState(h.ctx, id)
	if !ok || err != nil {
		t.Fatalf("fixState after a write = %v, %v", ok, err)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %s, want %s", got.StartedAt, started)
	}
	got.StartedAt = want.StartedAt
	if got != want {
		t.Errorf("fixState = %+v, want %+v", got, want)
	}

	raw, _ := h.e.getKV(h.ctx, KVPRFix(id))
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("the stored value %q is no JSON object: %v", raw, err)
	}
	if keys := slices.Sorted(maps.Keys(fields)); !slices.Equal(keys,
		[]string{"by", "cap", "cycle", "head", "phase", "report", "since", "started_at"}) {
		t.Errorf("stored fields = %v in %s", keys, raw)
	}
	if fields["phase"] != "fixing" || fields["started_at"] != "2026-10-10T09:30:15+03:00" {
		t.Errorf("stored phase %v, started_at %v: want fixing and an RFC 3339 time", fields["phase"], fields["started_at"])
	}

	want.Cycle, want.Phase, want.Head, want.By = 2, FixPhaseReviewing, "ccc3333", "autofix"
	if err := h.e.setFixState(h.ctx, id, want); err != nil {
		t.Fatal(err)
	}
	if got, _, _ = h.e.fixState(h.ctx, id); got.Cycle != 2 || got.Phase != "reviewing" || got.Head != "ccc3333" || got.By != "autofix" || got.Since != "aaa1111" {
		t.Errorf("fixState after a second write = %+v", got)
	}
	if _, ok, _ := h.e.fixState(h.ctx, id+1); ok {
		t.Error("another PR reads a loop it never started")
	}

	if err := h.e.clearFixState(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.e.fixState(h.ctx, id); ok || err != nil {
		t.Errorf("fixState after clearing = %v, %v", ok, err)
	}
	if err := h.e.clearFixState(h.ctx, id); err != nil {
		t.Errorf("clearing a loop that is gone = %v", err)
	}
}

// TestAnUnreadableFixStateIsAnError: a row that is no fix state is not a PR
// without a loop (the caller would start another over a running one), so
// fixState says so instead of answering "no loop".
func TestAnUnreadableFixStateIsAnError(t *testing.T) {
	h := newHarness(t)
	h.e.setKV(h.ctx, KVPRFix(7), "{not json")
	if _, ok, err := h.e.fixState(h.ctx, 7); ok || err == nil {
		t.Fatalf("fixState of an unreadable row = %v, %v: want an error", ok, err)
	}
}

// TestFixOffRoundTrips: the opt-out row of `magnum fix --off` is a PR's
// KVPRFixOff JSON of the time it was set and who set it, kept apart from the
// loop's own row, so a PR can be switched off while a loop runs and clearing
// the loop leaves the opt-out.
func TestFixOffRoundTrips(t *testing.T) {
	h := newHarness(t)
	const id = int64(7)

	if _, ok, err := h.e.fixOff(h.ctx, id); ok || err != nil {
		t.Fatalf("fixOff of a PR that is not switched off = %v, %v", ok, err)
	}
	since := h.clock.Now()
	if err := h.e.setFixOff(h.ctx, id, FixOff{Since: since, By: "magnum fix --off"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := h.e.fixOff(h.ctx, id)
	if !ok || err != nil || !got.Since.Equal(since) || got.By != "magnum fix --off" {
		t.Fatalf("fixOff after a write = %+v, %v, %v", got, ok, err)
	}
	raw, _ := h.e.getKV(h.ctx, KVPRFixOff(id))
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("the stored value %q is no JSON object: %v", raw, err)
	}
	if keys := slices.Sorted(maps.Keys(fields)); !slices.Equal(keys, []string{"by", "since"}) || fields["by"] != "magnum fix --off" {
		t.Errorf("stored value = %s, want since and by", raw)
	}
	if s, _ := fields["since"].(string); s == "" {
		t.Errorf("stored since = %v, want an RFC 3339 time", fields["since"])
	} else if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		t.Errorf("stored since %q is no RFC 3339 time: %v", s, err)
	}

	if _, ok, _ := h.e.fixState(h.ctx, id); ok {
		t.Error("the opt-out started a loop")
	}
	if err := h.e.setFixState(h.ctx, id, FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing}); err != nil {
		t.Fatal(err)
	}
	if err := h.e.clearFixState(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.e.fixOff(h.ctx, id); !ok {
		t.Error("clearing the loop cleared the opt-out")
	}

	if err := h.e.clearFixOff(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.e.fixOff(h.ctx, id); ok || err != nil {
		t.Errorf("fixOff after clearing = %v, %v", ok, err)
	}
	if err := h.e.clearFixOff(h.ctx, id); err != nil {
		t.Errorf("clearing an opt-out that is gone = %v", err)
	}
}
