package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/textx"
)

// statusWebhooks is the webhooks line in [webhooks] shadow mode: when the
// relay last answered a page, the totals of the hours the daemon keeps
// (engine.WebhooksHour, since the oldest), and the stream's health
// (engine.WebhooksHealth.State at the report's time). The relay is read with
// curl, not through the GitHub client: its pages spend no GitHub budget and
// are not on the github lines' meter.
type statusWebhooks struct {
	Mode       string     `json:"mode"`
	LastPageAt *time.Time `json:"last_page_at,omitempty"`
	Since      *time.Time `json:"since,omitempty"`
	Events     int        `json:"events"`
	Changes    int        `json:"poll_changes"`
	Announced  int        `json:"announced"`
	Missed     int        `json:"missed"`
	Early      int        `json:"early_or_noise"`
	Gaps       int        `json:"gaps"`
	Healthy    bool       `json:"healthy"`
	Cause      string     `json:"cause,omitempty"`
	// HintsDropped and LastDropped are what the consumer's bounds let go
	// (engine.WebhooksHour), in --json only: the line stays as it is.
	HintsDropped int `json:"hints_dropped"`
	LastDropped  int `json:"last_events_dropped"`
}

// statusGatherWebhooks fills the webhooks line from what the daemon's
// consumer recorded, in shadow mode only.
func statusGatherWebhooks(ctx context.Context, d statusDeps, r *statusReport) {
	if d.Config == nil || !d.Config.Webhooks.Shadow() {
		return
	}
	health, hours := engine.ReadWebhooks(ctx, d.Store)
	w := &statusWebhooks{Mode: d.Config.Webhooks.Mode, LastPageAt: health.PageAt}
	for _, h := range hours {
		w.Events += h.Events
		w.Changes += h.Changes
		w.Announced += h.Announced
		w.Missed += h.Missed
		w.Early += h.Early
		w.Gaps += h.Gaps
		w.HintsDropped += h.HintsDropped
		w.LastDropped += h.LastDropped
	}
	if len(hours) > 0 {
		w.Since = new(hours[0].Hour)
	}
	w.Healthy, w.Cause = health.State(r.GeneratedAt, d.Config.Webhooks.StaleAfter.Duration)
	r.Webhooks = w
}

// statusWebhooksText is the webhooks line's value: "shadow · last page 8s
// ago · since 16:00 140 events (9 early or noise) · poll changes 23: 22
// announced, 1 missed · healthy", or the cause when the stream is not.
func statusWebhooksText(w statusWebhooks, now time.Time) string {
	parts := []string{statusSafe(w.Mode, 0)}
	if w.LastPageAt != nil {
		parts = append(parts, "last page "+inspAgo(now, w.LastPageAt))
	} else {
		parts = append(parts, "no page yet")
	}
	if w.Since != nil {
		events := fmt.Sprintf("since %s %s", inspClock(now, *w.Since), textx.Count(w.Events, "event", "events"))
		if w.Early > 0 {
			events += fmt.Sprintf(" (%d early or noise)", w.Early)
		}
		parts = append(parts, events)
	}
	parts = append(parts, fmt.Sprintf("poll changes %d: %d announced, %d missed", w.Changes, w.Announced, w.Missed))
	if w.Gaps > 0 {
		parts = append(parts, textx.Count(w.Gaps, "gap", "gaps"))
	}
	if w.Healthy {
		parts = append(parts, "healthy")
	} else {
		parts = append(parts, "unhealthy: "+statusSafe(w.Cause, 0))
	}
	return strings.Join(parts, " · ")
}
