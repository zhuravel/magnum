package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
)

// In [webhooks] shadow mode `magnum status` has a webhooks line: when the
// relay last answered a page, the events since the oldest hour the daemon
// keeps (with those no poll change followed), the poll changes and how they
// came out, and the stream's health, or its cause when it is not healthy;
// --json has the numbers. With webhooks off there is no line.
func TestStatusShowsTheWebhooksLineInShadowMode(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	render := func() (string, map[string]any) {
		t.Helper()
		r, err := statusGather(ctx, d, statusOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var b, js bytes.Buffer
		statusRender(&b, r)
		if err := writeJSON(&js, r); err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		w, _ := doc["webhooks"].(map[string]any)
		for line := range strings.SplitSeq(b.String(), "\n") {
			if v, ok := strings.CutPrefix(line, "webhooks: "); ok {
				return v, w
			}
		}
		return "", w
	}
	set := func(key string, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if err := st.SetKV(ctx, key, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	since := now.Add(-2 * time.Hour).Truncate(time.Hour)
	set(engine.KVWebhooksHealth, engine.WebhooksHealth{PageAt: new(now.Add(-8 * time.Second))})
	set(engine.KVWebhooksHours, []engine.WebhooksHour{
		{Hour: since, Events: 100, Changes: 15, Announced: 15, Early: 9},
		{Hour: since.Add(time.Hour), Events: 40, Changes: 8, Announced: 7, Missed: 1},
	})
	if line, w := render(); line != "" || w != nil {
		t.Fatalf("webhooks off: line %q, json %v", line, w)
	}

	d.Config.Webhooks.Mode = config.WebhooksShadow
	line, w := render()
	want := "shadow · last page 8s ago · since " + since.Local().Format("15:04") +
		" 140 events (9 early or noise) · poll changes 23: 22 announced, 1 missed · healthy"
	if line != want {
		t.Fatalf("webhooks line = %q\nwant              %q", line, want)
	}
	for key, n := range map[string]float64{"events": 140, "poll_changes": 23, "announced": 22, "missed": 1, "early_or_noise": 9, "gaps": 0} {
		if w[key] != n {
			t.Errorf("json %s = %v, want %v (%v)", key, w[key], n, w)
		}
	}
	if w["healthy"] != true || w["mode"] != "shadow" || w["last_page_at"] == nil {
		t.Errorf("json = %v", w)
	}

	// A refused token, then a stream that went quiet past stale_after.
	set(engine.KVWebhooksHealth, engine.WebhooksHealth{PageAt: new(now.Add(-time.Minute)), Error: "the relay refused the token (HTTP 401)",
		ErrorAt: new(now.Add(-10 * time.Second))})
	if line, w := render(); !strings.HasSuffix(line, " · unhealthy: the relay refused the token (HTTP 401)") || w["healthy"] != false ||
		w["cause"] != "the relay refused the token (HTTP 401)" {
		t.Errorf("after a 401: %q, %v", line, w)
	}
	set(engine.KVWebhooksHealth, engine.WebhooksHealth{PageAt: new(now.Add(-2 * time.Minute))})
	if line, _ := render(); !strings.Contains(line, "last page 2m ago") || !strings.HasSuffix(line, "unhealthy: no page from the relay for 2m0s") {
		t.Errorf("stale: %q", line)
	}
	if err := st.DeleteKV(ctx, engine.KVWebhooksHealth); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteKV(ctx, engine.KVWebhooksHours); err != nil {
		t.Fatal(err)
	}
	if line, _ := render(); line != "shadow · no page yet · poll changes 0: 0 announced, 0 missed · unhealthy: no page from the relay yet" {
		t.Errorf("nothing recorded: %q", line)
	}
}
