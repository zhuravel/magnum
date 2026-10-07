package agents

import (
	"strings"
	"testing"
	"time"
)

// Codex's safety warnings are classified refused: its cybersecurity refusal
// as the TUI shows it (wrapped too), the words of that message on their own
// (cyber permissive safeguards, Daybreak access), and the policy forms
// (usage policy violations, abuse, threat) inside Codex's own error and
// warning lines; the same words in a review's text about the code are no
// refusal.
func TestClassifyCodexSafetyWarningsAsRefused(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	refused := map[string]string{
		"codex tui": "• Reading app/controllers/sessions_controller.rb\n\n" +
			"■ This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. " +
			"If you’re doing authorized security work that requires more cyber permissive safeguards, apply for Daybreak access via " +
			"https://platform.openai.com/settings/organization/status-and-access before retrying.\n\n› Implement {feature}\n\n  100% context left · ? for shortcuts",
		"wrapped":           "■ This content was flagged for possible\n  cybersecurity risk. If this seems wrong, try rephrasing your request.",
		"safeguards":        "■ This request needs more cyber permissive safeguards than your account has.",
		"daybreak":          "⚠ Apply for Daybreak access to continue this work.",
		"usage policy":      "■ This request may violate our usage policies.",
		"abuse":             "■ Your organization was flagged for potential abuse.",
		"threat":            "⚠ This conversation was flagged as a possible security threat.",
		"exec error line":   "ERROR: This content was flagged for possible cybersecurity risk.",
		"after usage limit": "■ You've hit your usage limit. Try again at 3:45 PM.\n› continue\n■ This content was flagged for possible cybersecurity risk.",
	}
	for name, text := range refused {
		t.Run(name, func(t *testing.T) {
			h := ClassifyAt(text, now)
			if h.Kind != HealthRefused || h.Detail == "" {
				t.Fatalf("%q: %+v, want refused with a detail", text, h)
			}
		})
	}
	if h := ClassifyAt(refused["codex tui"], now); !strings.Contains(h.Detail, "flagged for possible cybersecurity risk") {
		t.Errorf("detail = %q, want the refusal line", h.Detail)
	}
	for _, text := range []string{
		"P1 app/controllers/sessions_controller.rb:42 - a stolen cookie lets an attacker abuse the session; the threat model needs a rotation.",
		"• The token check can be abused by a replay (threat: session fixation); it may violate the usage policy of the API.",
		"  └ grep -n threat app/models/session.rb",
		"■ You've hit your usage limit. Try again later.",
	} {
		if h := ClassifyAt(text, now); h.Kind == HealthRefused {
			t.Errorf("%q classified refused: %+v", text, h)
		}
	}
}
