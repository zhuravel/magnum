package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// seedModelLimits writes the kv rows the agents package keeps for a kind's
// per-model limits: one row per model (the end of the limit) and the
// comma-separated index status reads them through. extraIndex names models
// that have an index entry but no row.
func seedModelLimits(t *testing.T, st *store.Store, kind string, limits map[string]time.Time, extraIndex ...string) {
	t.Helper()
	ctx := context.Background()
	index := slices.Sorted(maps.Keys(limits))
	index = append(index, extraIndex...)
	for model, until := range limits {
		if err := st.SetKV(ctx, agents.KVModelLimited(kind, model), store.FormatTime(until)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetKV(ctx, agents.KVModelLimits(kind), strings.Join(index, ",")); err != nil {
		t.Fatal(err)
	}
}

// modelPauses are the pauses whose scope is kind and whose reason reads
// "<model> limited", by model.
func modelPauses(r statusReport, kind string) map[string]statusPause {
	out := map[string]statusPause{}
	for _, p := range r.Pauses {
		if model, ok := strings.CutSuffix(p.Reason, " limited"); ok && p.Scope == kind {
			out[model] = p
		}
	}
	return out
}

func TestStatusShowsAModelLimitAndTheFallbackInUse(t *testing.T) {
	_, st, d, now := statusFixture(t)
	until := now.Add(2 * time.Hour)
	seedModelLimits(t, st, "claude", map[string]time.Time{"fable": until})

	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var claude []statusPause
	for _, p := range r.Pauses {
		if p.Scope == "claude" {
			claude = append(claude, p)
		}
	}
	if len(claude) != 1 {
		t.Fatalf("claude pauses = %+v, want only the fable limit", claude)
	}
	p := claude[0]
	if p.Scope != "claude" || p.Reason != "fable limited" || p.Using != "opus" || p.Until == nil || !p.Until.Equal(until) {
		t.Fatalf("pause = %+v, want fable limited until %v, using opus", p, until)
	}
	if p.Fix != "" || p.Detail != "" {
		t.Errorf("a model limit needs no fix (magnum switches models by itself): %+v", p)
	}
	// It does not displace the other pauses.
	scopes := map[string]bool{}
	for _, q := range r.Pauses {
		scopes[q.Scope] = true
	}
	for _, s := range []string{"daemon", "codex", "watch:talkable", "identity:talkable-app"} {
		if !scopes[s] {
			t.Errorf("pause %s lost: %+v", s, r.Pauses)
		}
	}

	var b bytes.Buffer
	statusRender(&b, r)
	out := b.String()
	for _, want := range []string{"claude: fable limited until ", " (in 2h)", ", using opus"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q\n%s", want, out)
		}
	}
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "fable limited") {
			line = l
		}
	}
	if strings.Index(line, "until ") > strings.Index(line, ", using opus") {
		t.Errorf("the end comes before the fallback: %q", line)
	}
	if strings.Contains(out, "claude: usage_limit") || strings.Contains(out, "magnum resume --tool claude") {
		t.Errorf("a model limit is shown as a paused kind:\n%s", out)
	}

	// The JSON report carries the fallback too.
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"scope":"claude"`, `"reason":"fable limited"`, `"using":"opus"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("JSON lacks %s: %s", want, raw)
		}
	}

	// And the dashboard rows read the same text.
	var dash string
	for _, tp := range statusDashData(r, "talkable/talkable").Pauses {
		if tp.Key == "claude" {
			dash = tp.Reason
		}
	}
	if !strings.HasPrefix(dash, "fable limited until ") || !strings.HasSuffix(dash, ", using opus") {
		t.Errorf("dashboard pause = %q", dash)
	}
}

func TestStatusOmitsExpiredModelLimits(t *testing.T) {
	_, st, d, now := statusFixture(t)
	seedModelLimits(t, st, "claude", map[string]time.Time{
		"fable":  now.Add(-time.Minute),
		"opus":   now.Add(-3 * time.Hour),
		"sonnet": now, // ends exactly now: over
	}, "ghost") // an index entry without a row
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := modelPauses(r, "claude"); len(got) != 0 {
		t.Fatalf("expired limits listed: %+v", got)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if strings.Contains(b.String(), "limited") {
		t.Fatalf("status shows an expired limit:\n%s", b.String())
	}

	// One active limit among expired ones is the only one listed, and the
	// expired models count as available fallbacks.
	seedModelLimits(t, st, "claude", map[string]time.Time{
		"fable": now.Add(time.Hour),
		"opus":  now.Add(-time.Hour),
	})
	r, err = statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := modelPauses(r, "claude")
	if len(got) != 1 || got["fable"].Using != "opus" {
		t.Fatalf("limits = %+v, want fable only, using opus (opus's limit is over)", got)
	}
}

func TestStatusFallbackSkipsLimitedModels(t *testing.T) {
	_, st, d, now := statusFixture(t)
	seedModelLimits(t, st, "claude", map[string]time.Time{
		"fable": now.Add(2 * time.Hour),
		"opus":  now.Add(3 * time.Hour),
	})
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := modelPauses(r, "claude")
	if len(got) != 2 || got["fable"].Using != "sonnet" || got["opus"].Using != "sonnet" {
		t.Fatalf("limits = %+v, want fable and opus both on sonnet", got)
	}
	if !got["fable"].Until.Equal(now.Add(2*time.Hour)) || !got["opus"].Until.Equal(now.Add(3*time.Hour)) {
		t.Errorf("ends = %v, %v", got["fable"].Until, got["opus"].Until)
	}
	// Listed in model order.
	var order []string
	for _, p := range r.Pauses {
		if p.Scope == "claude" {
			order = append(order, p.Reason)
		}
	}
	if !slices.Equal(order, []string{"fable limited", "opus limited"}) {
		t.Errorf("order = %v", order)
	}

	// With every fallback limited there is nothing to run on: no ", using".
	seedModelLimits(t, st, "claude", map[string]time.Time{
		"fable":  now.Add(2 * time.Hour),
		"opus":   now.Add(3 * time.Hour),
		"sonnet": now.Add(4 * time.Hour),
	})
	r, err = statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got = modelPauses(r, "claude")
	if len(got) != 3 || got["fable"].Using != "" || got["sonnet"].Using != "" {
		t.Fatalf("limits = %+v, want three limits with no fallback", got)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if strings.Contains(b.String(), "using") {
		t.Errorf("status names a fallback that is limited:\n%s", b.String())
	}
}

func TestStatusModelLimitOfAKindThatCannotSwitch(t *testing.T) {
	_, st, d, now := statusFixture(t)
	// codex has no switch_model: its fallback list is empty, so the limit is
	// listed without a model to run on.
	seedModelLimits(t, st, "codex", map[string]time.Time{"opus": now.Add(time.Hour)})
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := modelPauses(r, "codex")
	if len(got) != 1 || got["opus"].Using != "" {
		t.Fatalf("codex limits = %+v", got)
	}
	// Claude's limits are not read from codex's rows.
	if claude := modelPauses(r, "claude"); len(claude) != 0 {
		t.Fatalf("claude limits = %+v", claude)
	}
}
