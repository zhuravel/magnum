package eval

import (
	"maps"
	"strings"
	"testing"
)

const dryRunResult = `{
  "status": "dry_run",
  "unknown_field": {"nested": [1, 2, 3]},
  "event": "REQUEST_CHANGES",
  "findings": {"P0": 0, "P1": 1, "P2": 2, "P3": 0},
  "blocker": null,
  "planned_review": {
    "commit_id": "` + sha + `",
    "body": "Overall: the OAuth flow needs work.",
    "event": "COMMENT",
    "comments": [
      {"path": "app/controllers/oauth/callback.rb", "line": 42, "start_line": 40, "side": "RIGHT", "body": "**P1** HMAC is never verified"},
      {"path": "app/models/user.rb", "line": 7, "side": "RIGHT", "body": "Simplification (optional): inline this"},
      {"path": "app/models/user.rb", "line": 9, "start_line": 9, "body": "[P2] Missing index"}
    ]
  },
  "provenance": [
    {"id": "F1", "severity": "P0", "path": "app/controllers/oauth/callback.rb", "line": 42, "sources": ["claude-review", "judge"], "verdict": "posted", "reason_code": "ok"}
  ]
}`

func TestParseResultReadsADryRun(t *testing.T) {
	r, err := ParseResult([]byte(dryRunResult))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "dry_run" || !r.Planned {
		t.Errorf("status = %q planned = %v", r.Status, r.Planned)
	}
	if r.Event != "REQUEST_CHANGES" {
		t.Errorf("event = %q, want the result's own event to win over the planned review's", r.Event)
	}
	if want := map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 0}; !maps.Equal(r.Counts, want) {
		t.Errorf("counts = %v, want %v", r.Counts, want)
	}
	if len(r.Findings) != 4 {
		t.Fatalf("findings = %d, want 3 inline + the review body", len(r.Findings))
	}
	f := r.Findings[0]
	if f.Path != "app/controllers/oauth/callback.rb" || f.Line != 42 || f.StartLine != 40 || f.InBody {
		t.Errorf("first finding = %+v", f)
	}
	if f.Severity != "P0" {
		t.Errorf("severity = %q, want provenance P0 over the body's P1", f.Severity)
	}
	if r.Findings[1].Simplification != true || r.Findings[1].Severity != "" {
		t.Errorf("simplification finding = %+v", r.Findings[1])
	}
	if r.Findings[2].StartLine != 0 {
		t.Errorf("start_line equal to line = %d, want 0 (single line)", r.Findings[2].StartLine)
	}
	body := r.Findings[3]
	if !body.InBody || body.Body != "Overall: the OAuth flow needs work." || body.Path != "" || body.Severity != "" || body.Simplification {
		t.Errorf("body finding = %+v", body)
	}
}

func TestParseResultSeverityOfAComment(t *testing.T) {
	long := strings.Repeat("x", 80) + " P1"
	tests := []struct {
		name string
		body string
		prov string // provenance array
		want string
	}{
		{"bold", "**P1** Title", "[]", "P1"},
		{"bracket", "[P2] Title", "[]", "P2"},
		{"colon", "P3: Title", "[]", "P3"},
		{"p zero", "**P0** Title", "[]", "P0"},
		{"first token wins", "P2 and also P0", "[]", "P2"},
		{"lower case is not a severity", "p1 title", "[]", ""},
		{"out of range", "P4 title", "[]", ""},
		{"longer number", "P10 title", "[]", ""},
		{"inside a word", "UP1 title", "[]", ""},
		{"no token", "Title only", "[]", ""},
		{"token beyond 80 characters", long, "[]", ""},
		{"token at the 80 character edge", strings.Repeat("x", 77) + " P1", "[]", "P1"},
		{"provenance beats the body", "**P3** Title", `[{"path":"a.rb","line":5,"severity":"P0","verdict":"posted"}]`, "P0"},
		{"provenance with no verdict counts", "Title", `[{"path":"a.rb","line":5,"severity":"P2"}]`, "P2"},
		{"rejected provenance is ignored", "**P3** Title", `[{"path":"a.rb","line":5,"severity":"P0","verdict":"rejected"}]`, "P3"},
		{"provenance on another line is ignored", "**P3** Title", `[{"path":"a.rb","line":6,"severity":"P0","verdict":"posted"}]`, "P3"},
		{"provenance on another path is ignored", "**P3** Title", `[{"path":"b.rb","line":5,"severity":"P0","verdict":"posted"}]`, "P3"},
		{"provenance without a valid severity falls back", "**P3** Title", `[{"path":"a.rb","line":5,"severity":"high","verdict":"posted"}]`, "P3"},
		{"first usable provenance wins", "Title", `[{"path":"a.rb","line":5,"severity":"P1","verdict":"rejected"},{"path":"a.rb","line":5,"severity":"P2","verdict":"posted"},{"path":"a.rb","line":5,"severity":"P0","verdict":"posted"}]`, "P2"},
		{"provenance with null line", "**P3** Title", `[{"path":"a.rb","line":null,"severity":"P0","verdict":"posted"}]`, "P3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := `{"planned_review":{"comments":[{"path":"a.rb","line":5,"body":` + jsonString(tt.body) + `}]},"provenance":` + tt.prov + `}`
			r, err := ParseResult([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Findings) != 1 || r.Findings[0].Severity != tt.want {
				t.Errorf("findings = %+v, want severity %q", r.Findings, tt.want)
			}
		})
	}
}

func TestParseResultSimplificationMarker(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"exact", "Simplification (optional): use a map", true},
		{"lower case", "simplification (optional) use a map", true},
		{"after a severity", "**P3** Simplification (optional): use a map", true},
		{"at the 120 character edge", strings.Repeat("x", 120-len("Simplification (optional)")) + "Simplification (optional)", true},
		{"beyond 120 characters", strings.Repeat("x", 100) + " and then a Simplification (optional) suggestion", false},
		{"missing parenthesis", "Simplification: use a map", false},
		{"not a suggestion", "The code is not simple", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := ParseResult([]byte(`{"planned_review":{"comments":[{"path":"a.rb","line":1,"body":` + jsonString(tt.body) + `}]}}`))
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Findings[0].Simplification; got != tt.want {
				t.Errorf("Simplification = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseResultToleratesMissingAndNullFields(t *testing.T) {
	tests := []struct {
		name         string
		data         string
		wantPlanned  bool
		wantFindings int
	}{
		{"empty object", `{}`, false, 0},
		{"status only", `{"status":"error","error":"boom"}`, false, 0},
		{"null planned review", `{"status":"dry_run","planned_review":null}`, false, 0},
		{"null comments", `{"planned_review":{"comments":null,"body":null}}`, true, 0},
		{"null findings and provenance", `{"findings":null,"provenance":null,"planned_review":{"comments":[{"body":"x"}]}}`, true, 1},
		{"comment without path and line", `{"planned_review":{"comments":[{"body":"x"}]}}`, true, 1},
		{"comment with null line", `{"planned_review":{"comments":[{"path":"a.rb","line":null,"start_line":null,"body":"x"}]}}`, true, 1},
		{"whitespace body is no finding", `{"planned_review":{"body":"  \n ","comments":[]}}`, true, 0},
		{"findings of an unknown shape", `{"findings":[{"id":"F1"}],"planned_review":{}}`, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := ParseResult([]byte(tt.data))
			if err != nil {
				t.Fatal(err)
			}
			if r.Planned != tt.wantPlanned || len(r.Findings) != tt.wantFindings {
				t.Errorf("planned = %v findings = %d, want %v and %d", r.Planned, len(r.Findings), tt.wantPlanned, tt.wantFindings)
			}
		})
	}
}

func TestParseResultFallsBackToThePlannedEvent(t *testing.T) {
	r, err := ParseResult([]byte(`{"planned_review":{"event":"COMMENT"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Event != "COMMENT" {
		t.Errorf("event = %q, want the planned review's", r.Event)
	}
}

func TestParseResultKeepsInlineOrderAndPutsTheBodyLast(t *testing.T) {
	r, err := ParseResult([]byte(`{"planned_review":{"body":"summary","comments":[{"path":"b.rb","line":1,"body":"one"},{"path":"a.rb","line":2,"body":"two"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range r.Findings {
		got = append(got, f.Body)
	}
	if strings.Join(got, ",") != "one,two,summary" {
		t.Errorf("order = %v, want one,two,summary", got)
	}
}

func TestParseResultStartLineNeverPointsPastTheLine(t *testing.T) {
	r, err := ParseResult([]byte(`{"planned_review":{"comments":[{"path":"a.rb","line":5,"start_line":9,"body":"x"},{"path":"a.rb","line":5,"start_line":3,"body":"y"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Findings[0].StartLine != 0 || r.Findings[1].StartLine != 3 {
		t.Errorf("start lines = %d, %d, want 0 and 3", r.Findings[0].StartLine, r.Findings[1].StartLine)
	}
}

func TestParseResultRejectsBrokenJSON(t *testing.T) {
	for _, data := range []string{"", "not json", `{"status":`, `{"planned_review":"text"}`} {
		if _, err := ParseResult([]byte(data)); err == nil || !strings.HasPrefix(err.Error(), "eval: parse result") {
			t.Errorf("ParseResult(%q) error = %v, want an eval: parse result error", data, err)
		}
	}
}

// jsonString quotes s as a JSON string literal.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
