package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// `magnum attention --json` printed "since": "0001-01-01T00:00:00Z" for an
// item with no time: omitempty never omits a time.Time. A zero Since is
// left out; a set one is kept.
func TestAttentionJSONOmitsAZeroSince(t *testing.T) {
	b, err := json.Marshal(attentionItem{Kind: "done", PR: "talkable#5"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "since") || strings.Contains(string(b), "0001-01-01") {
		t.Fatalf("zero since printed: %s", b)
	}
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if b, _ = json.Marshal(attentionItem{Kind: "done", PR: "talkable#5", Since: at}); !strings.Contains(string(b), `"since":"2026-10-09T12:00:00Z"`) {
		t.Fatalf("set since lost: %s", b)
	}
}
