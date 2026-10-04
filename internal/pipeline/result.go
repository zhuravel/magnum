package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/store"
)

// judgeResult is the judge's result (codex-judge.json, or the MAGNUM_RESULT line):
// {status, run_id, review_id, review_url, event, findings{P0..P3},
// provenance[{id, severity, path, line, sources, verdict, reason_code}],
// environment_failures[{cmd, error}], blocker, …}.
type judgeResult struct {
	Status    string
	RunID     string
	ReviewID  int64
	ReviewURL string
	Event     string
	Findings  map[string]int
	Blocker   string
	// EnvironmentFailures are checks the review machine itself broke (a
	// missing table, a deadlock, the wrong Ruby): kept out of the posted
	// review by the skill, reported to the operator by the round.
	EnvironmentFailures []envFailure
	// Provenance is every finding the judge judged: its sources, whether it
	// was posted and why it was rejected (nil in a result file written
	// before the skill asked for it).
	Provenance []findingRecord
	Raw        string // the JSON as written
	Source     string // file | pane
}

// findingRecord is one entry of the result's provenance list.
type findingRecord struct {
	ID       string   // unique within the result
	Severity string   // P0..P3
	Path     string   // "" for a finding in the review body
	Line     int      // 0 = none
	Sources  []string // reviewer roles that raised it; "judge" for the judge's own pass
	Verdict  string   // store.FindingPosted | store.FindingRejected
	// ReasonCode says why a finding was rejected, as the judge wrote it
	// (normalized): duplicate, not_reproducible, outside_diff, pre_existing,
	// style_only, speculative or environment (SKILL.md section 3).
	ReasonCode string
}

// parseProvenance reads the result's provenance list leniently: an entry
// that is not an object or has no posted/rejected verdict is skipped; an id
// that is missing or repeated becomes "#<position>"; severities are upper
// case, sources lower case without repeats, reason codes lower case with
// underscores (a posted finding has none).
func parseProvenance(m json.RawMessage) []findingRecord {
	var items []json.RawMessage
	if json.Unmarshal(m, &items) != nil {
		return nil
	}
	var out []findingRecord
	seen := map[string]bool{}
	for i, it := range items {
		var o map[string]json.RawMessage
		if json.Unmarshal(it, &o) != nil || o == nil {
			continue
		}
		f := findingRecord{
			ID:         strings.TrimSpace(jsonString(o["id"])),
			Severity:   strings.ToUpper(strings.TrimSpace(jsonString(o["severity"]))),
			Path:       strings.TrimSpace(jsonString(o["path"])),
			Line:       int(max(jsonInt(o["line"]), 0)),
			Sources:    parseSources(o["sources"]),
			ReasonCode: reasonCode(jsonString(o["reason_code"])),
		}
		switch strings.ToLower(strings.TrimSpace(jsonString(o["verdict"]))) {
		case store.FindingPosted, "accepted":
			f.Verdict, f.ReasonCode = store.FindingPosted, ""
		case store.FindingRejected, "dropped":
			f.Verdict = store.FindingRejected
		default:
			continue
		}
		if f.ID == "" {
			if n := jsonInt(o["id"]); n != 0 {
				f.ID = strconv.FormatInt(n, 10)
			}
		}
		if f.ID == "" || seen[f.ID] {
			f.ID = "#" + strconv.Itoa(i+1)
		}
		seen[f.ID] = true
		out = append(out, f)
	}
	return out
}

// parseSources reads a list of source names (or one name as a string).
func parseSources(m json.RawMessage) []string {
	var list []string
	if json.Unmarshal(m, &list) != nil {
		if s := jsonString(m); s != "" {
			list = []string{s}
		}
	}
	out := []string{}
	for _, s := range list {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// reasonCode normalizes a reason code: "Not reproducible" -> not_reproducible.
func reasonCode(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == '\t'
	}), "_")
}

// envFailure is one entry of the result's environment_failures: an object
// {cmd, error} or a plain string (Error only).
type envFailure struct {
	Cmd   string `json:"cmd,omitempty"`
	Error string `json:"error"`
}

// parseEnvFailures reads environment_failures leniently: malformed entries
// are skipped.
func parseEnvFailures(m json.RawMessage) []envFailure {
	var items []json.RawMessage
	if json.Unmarshal(m, &items) != nil {
		return nil
	}
	var out []envFailure
	for _, it := range items {
		if s := strings.TrimSpace(jsonString(it)); s != "" {
			out = append(out, envFailure{Error: s})
			continue
		}
		var o map[string]json.RawMessage
		if json.Unmarshal(it, &o) != nil {
			continue
		}
		f := envFailure{Cmd: strings.TrimSpace(jsonString(o["cmd"])), Error: strings.TrimSpace(jsonString(o["error"]))}
		if f.Error == "" {
			f.Error = strings.TrimSpace(jsonString(o["result"]))
		}
		if f.Cmd != "" || f.Error != "" {
			out = append(out, f)
		}
	}
	return out
}

// Judge result statuses (skills/magnum-review/SKILL.md section 8).
const (
	statusPosted        = "posted"
	statusDryRun        = "dry_run"
	statusBlocked       = "blocked"
	statusIdentityError = "identity_error"
	statusClosed        = "closed"
	statusStopped       = "stopped"
	statusError         = "error"
)

// resultMarker prefixes the judge's final line.
const resultMarker = "MAGNUM_RESULT"

// parseResult decodes a result object leniently: numbers may be strings and
// unknown or malformed fields are ignored. ok is false unless it is a JSON
// object with a status.
func parseResult(b []byte) (judgeResult, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return judgeResult{}, false
	}
	r := judgeResult{
		Status:    jsonString(raw["status"]),
		RunID:     jsonString(raw["run_id"]),
		ReviewID:  jsonInt(raw["review_id"]),
		ReviewURL: jsonString(raw["review_url"]),
		Event:     jsonString(raw["event"]),
		Blocker:   jsonString(raw["blocker"]),
		Raw:       strings.TrimSpace(string(b)),
	}
	if f, ok := raw["environment_failures"]; ok {
		r.EnvironmentFailures = parseEnvFailures(f)
	}
	if f, ok := raw["provenance"]; ok {
		r.Provenance = parseProvenance(f)
	}
	if f, ok := raw["findings"]; ok {
		var m map[string]json.RawMessage
		if json.Unmarshal(f, &m) == nil {
			r.Findings = map[string]int{}
			for k, v := range m {
				r.Findings[k] = int(jsonInt(v))
			}
		}
	}
	r.Status = strings.ToLower(strings.TrimSpace(r.Status))
	return r, r.Status != ""
}

// readResultFile reads the judge's result file.
func readResultFile(path string) (judgeResult, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return judgeResult{}, false
	}
	r, ok := parseResult(b)
	r.Source = "file"
	return r, ok
}

// parseResultLine finds the last `MAGNUM_RESULT <json>` in pane text whose
// run_id is one of ids (an unattributed line may belong to an earlier round
// of the same session).
func parseResultLine(text string, ids map[string]bool) (judgeResult, bool) {
	for end := len(text); end > 0; {
		i := strings.LastIndex(text[:end], resultMarker)
		if i < 0 {
			break
		}
		end = i
		rest := strings.TrimLeft(text[i+len(resultMarker):], " \t:")
		var raw json.RawMessage
		if err := json.NewDecoder(strings.NewReader(rest)).Decode(&raw); err != nil {
			continue
		}
		r, ok := parseResult(raw)
		if !ok || !ids[r.RunID] {
			continue
		}
		r.Source = "pane"
		return r, true
	}
	return judgeResult{}, false
}

func jsonString(m json.RawMessage) string {
	var s string
	if json.Unmarshal(m, &s) == nil {
		return s
	}
	return ""
}

func jsonInt(m json.RawMessage) int64 {
	var n json.Number
	if json.Unmarshal(m, &n) == nil {
		if v, err := n.Int64(); err == nil {
			return v
		}
		if f, err := n.Float64(); err == nil {
			return int64(f)
		}
	}
	if s := jsonString(m); s != "" {
		if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return v
		}
	}
	return 0
}

// normalizeEvent maps review events and states onto GitHub's review state
// names: APPROVED, COMMENTED, CHANGES_REQUESTED.
func normalizeEvent(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	switch s {
	case "APPROVE", "APPROVED":
		return "APPROVED"
	case "COMMENT", "COMMENTED":
		return "COMMENTED"
	case "REQUEST_CHANGES", "CHANGES_REQUESTED":
		return "CHANGES_REQUESTED"
	}
	return s
}

func isChangesRequested(event string) bool { return normalizeEvent(event) == "CHANGES_REQUESTED" }

// recordFindings stores the provenance of a posted round's result on the
// judge's run (store.RecordFindings) for `magnum stats`. A result without
// provenance records nothing; a failure is logged, never the round's.
func (rd *round) recordFindings(ctx context.Context, runID string, res *judgeResult) {
	if res == nil || len(res.Provenance) == 0 {
		return
	}
	fs := make([]store.Finding, 0, len(res.Provenance))
	for _, f := range res.Provenance {
		fs = append(fs, store.Finding{FindingID: f.ID, Severity: f.Severity, Path: f.Path, Line: f.Line,
			Sources: f.Sources, Verdict: f.Verdict, ReasonCode: f.ReasonCode})
	}
	if err := rd.r.Store.RecordFindings(context.WithoutCancel(ctx), runID, rd.pr.ID, rd.in.Round, fs); err != nil {
		rd.logf("pipeline: record findings: %v", err)
	}
}
