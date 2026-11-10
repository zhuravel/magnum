package execx

import (
	"strings"
	"testing"
)

// Scrub masks what Redact masks, then the token shapes and the secret
// pairs the factory's scrub masks (AWS, Slack, OpenAI and Anthropic, GitLab,
// Stripe and Google keys; the values of password=, token: and the like,
// the key kept) and emails; host names stay, since a CI failure's host is
// what a fixer needs.
func TestScrubMasksWhatRuntimeScrubMasked(t *testing.T) {
	for in, want := range map[string]string{
		"push with ghp_abcdefghijklmnopqrstuvwxyz0123456789": "push with <redacted>",
		"key AKIAABCDEFGHIJKLMNOP in the env":                "key <redacted> in the env",
		"slack xoxb-1234567890-abcdefghij":                   "slack <redacted>",
		"OPENAI sk-proj-abcdefghijklmnopqrstuvwxyz":          "OPENAI <redacted>",
		"gitlab glpat-abcdefghijklmnop12":                    "gitlab <redacted>",
		"stripe sk_live_abcdefghijklmnop12":                  "stripe <redacted>",
		"google AIzaSyA0123456789abcdefghijklmnopqrstuv":     "google <redacted>",
		`DB password="hunter2" and api_key: abc123`:          `DB password=<redacted> and api_key: <redacted>`,
		"mail rev-ann@example.com failed":                    "mail <email> failed",
		"connect to db.example.com:5432 refused":             "connect to db.example.com:5432 refused",
		"Authorization: Bearer abc.def":                      "Authorization: <redacted>",
		"expected 3, got 4 in spec/models/offer_spec.rb:12":  "expected 3, got 4 in spec/models/offer_spec.rb:12",
	} {
		if got := Scrub(in); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
	}
	key := "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nAAAA\n-----END RSA PRIVATE KEY-----\nafter"
	if got := Scrub(key); strings.Contains(got, "MIIEow") || !strings.HasSuffix(got, "after") {
		t.Errorf("Scrub of a key block = %q", got)
	}
}
