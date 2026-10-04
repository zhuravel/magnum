package eval

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Finding is one thing the review reported: an inline comment, or the review body as a whole.
type Finding struct {
	Path           string `json:"path,omitempty"`
	Line           int    `json:"line,omitempty"`
	StartLine      int    `json:"start_line,omitempty"` // 0 = single line
	Body           string `json:"body"`
	Severity       string `json:"severity,omitempty"`       // P0..P3 or ""
	Simplification bool   `json:"simplification,omitempty"` // an optional suggestion, never a defect
	InBody         bool   `json:"in_body,omitempty"`        // the review body rather than an inline comment
}

// Result is what a judge result file says about one round.
type Result struct {
	Status   string
	Event    string
	Counts   map[string]int // the "findings" object: severity -> number
	Findings []Finding      // inline comments in order, then the review body when it is not empty
	Planned  bool           // planned_review was present
}

// resultFile is the part of the judge's JSON result file that scoring reads. Everything may be
// missing or null; unknown fields are ignored.
type resultFile struct {
	Status        string            `json:"status"`
	Event         string            `json:"event"`
	Findings      json.RawMessage   `json:"findings"`
	PlannedReview *plannedReview    `json:"planned_review"`
	Provenance    []provenanceEntry `json:"provenance"`
}

// plannedReview is the body of GitHub's POST /repos/{o}/{r}/pulls/{n}/reviews.
type plannedReview struct {
	Body     string           `json:"body"`
	Event    string           `json:"event"`
	Comments []plannedComment `json:"comments"`
}

type plannedComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"start_line"`
	Body      string `json:"body"`
}

type provenanceEntry struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Verdict  string `json:"verdict"`
}

var (
	severityToken        = regexp.MustCompile(`\bP[0-3]\b`)
	severityWindow       = 80  // characters of a comment body searched for a severity
	simplificationWindow = 120 // characters of a comment body searched for the simplification marker
)

const simplificationMarker = "simplification (optional)"

// ParseResult reads the judge's result file. The inline comments of planned_review become findings
// in order, followed by one InBody finding holding the review body when it is not empty.
//
// The severity of a comment comes from the provenance entry that was posted (or has no verdict) on
// the same path and line, else from the first P0..P3 token in the first 80 characters of its body.
func ParseResult(data []byte) (Result, error) {
	var f resultFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Result{}, fmt.Errorf("eval: parse result: %w", err)
	}
	r := Result{Status: f.Status, Event: f.Event}
	// The counts are informational; a shape this code does not know is not worth failing a score.
	_ = json.Unmarshal(f.Findings, &r.Counts)
	pr := f.PlannedReview
	if pr == nil {
		return r, nil
	}
	r.Planned = true
	if r.Event == "" {
		r.Event = pr.Event
	}
	for _, c := range pr.Comments {
		fd := Finding{Path: c.Path, Line: c.Line, Body: c.Body}
		if c.StartLine > 0 && c.StartLine < c.Line {
			fd.StartLine = c.StartLine
		}
		fd.Severity = commentSeverity(c, f.Provenance)
		fd.Simplification = isSimplification(c.Body)
		r.Findings = append(r.Findings, fd)
	}
	if strings.TrimSpace(pr.Body) != "" {
		r.Findings = append(r.Findings, Finding{Body: pr.Body, InBody: true})
	}
	return r, nil
}

func commentSeverity(c plannedComment, prov []provenanceEntry) string {
	for _, p := range prov {
		if p.Path != c.Path || p.Line != c.Line || (p.Verdict != "" && p.Verdict != "posted") {
			continue
		}
		if s := strings.ToUpper(strings.TrimSpace(p.Severity)); validSeverity(s) {
			return s
		}
	}
	return severityToken.FindString(head(c.Body, severityWindow))
}

func isSimplification(body string) bool {
	return strings.Contains(strings.ToLower(head(body, simplificationWindow)), simplificationMarker)
}

// head returns the first n characters (runes) of s.
func head(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}
