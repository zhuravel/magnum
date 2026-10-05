package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/usage"
)

const paceWeek = 7 * 24 * time.Hour

// paceReport gathers the Codex budget the daemon would have recorded:
// percent of a weekly window that is share percent elapsed at the fixture's
// now (window 0 and no reset when share is negative).
func paceReport(t *testing.T, percent string, share float64) (statusReport, time.Time) {
	t.Helper()
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	kv := map[string]string{engine.KVUsageCodexPercent: percent, engine.KVUsageCodexPlan: "pro"}
	if share >= 0 {
		kv[engine.KVUsageCodexWindow] = "10080"
		kv[engine.KVUsageCodexResetsAt] = store.FormatTime(now.Add(time.Duration((100 - share) / 100 * float64(paceWeek))))
	}
	for k, v := range kv {
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Codex == nil {
		t.Fatal("no codex usage in the report")
	}
	return r, now
}

// TestStatusCodexPaceNamesWhenTheCapsAreReached: 51% of the weekly budget
// used with 18% of the window elapsed is 2.8x the sustainable rate; the line
// says when codex_soft and codex_hard come, both before the reset.
func TestStatusCodexPaceNamesWhenTheCapsAreReached(t *testing.T) {
	r, now := paceReport(t, "51", 18)
	u := r.Codex
	pace, _ := usage.PaceOf(51, 10080, *u.ResetsAt, now)
	soft, _ := pace.Reach(80)
	hard, _ := pace.Reach(95)
	if u.Pace != 2.83 || u.SoftAt == nil || !u.SoftAt.Equal(soft) || u.HardAt == nil || !u.HardAt.Equal(hard) {
		t.Fatalf("codex = %+v (soft %v, hard %v)", u, soft, hard)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	want := "codex:    51% used of the weekly limit (pro), resets " + inspClock(now, *u.ResetsAt) + " (" + inspAgo(now, u.ResetsAt) + "); caps 80%/95%; pace 2.8x: 80% " +
		inspClock(now, soft) + ", 95% " + inspClock(now, hard) + "\n"
	if !strings.Contains(b.String(), want) {
		t.Fatalf("status lacks\n%s\nin\n%s", want, b.String())
	}

	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["pace"] != 2.83 || got["soft_at"] != soft.Format(time.RFC3339Nano) || got["hard_at"] != hard.Format(time.RFC3339Nano) {
		t.Fatalf("json = %s", raw)
	}
}

// TestStatusCodexPaceWithinTheWindowWhenNoCapIsAhead: a budget spent slower
// than the window lasts reaches neither cap before the reset; the line says
// so and the JSON leaves soft_at and hard_at out.
func TestStatusCodexPaceWithinTheWindowWhenNoCapIsAhead(t *testing.T) {
	r, now := paceReport(t, "20", 50)
	u := r.Codex
	if u.Pace != 0.4 || u.SoftAt != nil || u.HardAt != nil {
		t.Fatalf("codex = %+v", u)
	}
	if got := statusCodexText(*u, now); !strings.HasSuffix(got, "; caps 80%/95%; pace 0.4x: within the window") {
		t.Fatalf("text = %q", got)
	}
	raw, _ := json.Marshal(u)
	for _, key := range []string{`"soft_at"`, `"hard_at"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("json has %s: %s", key, raw)
		}
	}
	if !strings.Contains(string(raw), `"pace":0.4`) {
		t.Errorf("json lacks the pace: %s", raw)
	}
}

// TestStatusCodexPaceSkipsACapAlreadyReached: at the soft cap only the hard
// cap is projected; at the hard cap there is nothing to project.
func TestStatusCodexPaceSkipsACapAlreadyReached(t *testing.T) {
	r, now := paceReport(t, "85", 50)
	u := r.Codex
	if u.SoftAt != nil || u.HardAt == nil {
		t.Fatalf("codex = %+v", u)
	}
	text := statusCodexText(*u, now)
	if want := "at the soft cap 80%: first reviews wait; pace 1.7x: 95% " + inspClock(now, *u.HardAt); !strings.HasSuffix(text, want) {
		t.Fatalf("text = %q, want suffix %q", text, want)
	}

	r, now = paceReport(t, "96", 50)
	text = statusCodexText(*r.Codex, now)
	if strings.Contains(text, "pace") || !strings.HasSuffix(text, "at the hard cap 95%: rounds that need Codex pause") {
		t.Fatalf("text at the hard cap = %q", text)
	}
	if r.Codex.SoftAt != nil || r.Codex.HardAt != nil {
		t.Fatalf("codex at the hard cap = %+v", r.Codex)
	}
}

// TestStatusCodexPaceIsLeftOutWhenUnknown: nothing used, no window length or
// reset known, or a window that already ended give no pace.
func TestStatusCodexPaceIsLeftOutWhenUnknown(t *testing.T) {
	tests := []struct {
		name    string
		percent string
		share   float64 // negative: no window, no reset
	}{
		{"nothing used", "0", 40},
		{"no window", "51", -1},
		{"window already reset", "51", 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, now := paceReport(t, tt.percent, tt.share)
			u := r.Codex
			if u.Pace != 0 || u.SoftAt != nil || u.HardAt != nil {
				t.Fatalf("codex = %+v", u)
			}
			if text := statusCodexText(*u, now); strings.Contains(text, "pace") {
				t.Fatalf("text = %q", text)
			}
			raw, _ := json.Marshal(u)
			for _, key := range []string{`"pace"`, `"soft_at"`, `"hard_at"`} {
				if strings.Contains(string(raw), key) {
					t.Errorf("json has %s: %s", key, raw)
				}
			}
		})
	}
}
