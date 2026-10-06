package cli

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/tui"
)

var jsonKeyRe = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// walkJSON calls fn with the path and key of every object member, at any depth.
func walkJSON(path string, v any, fn func(path, key string, val any)) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			fn(path, k, val)
			walkJSON(path+"."+k, val, fn)
		}
	case []any:
		for _, val := range x {
			walkJSON(path+"[]", val, fn)
		}
	}
}

// fullBoardRow is a PR with every field of the board row set.
func fullBoardRow() tui.PRBoardRow {
	at := time.Date(2026, 10, 5, 14, 30, 0, 0, time.UTC)
	req := tui.RequestInfo{To: "rev-ann", By: "alice", At: at.Add(-time.Hour), Mine: true}
	return tui.PRBoardRow{
		Ref: "talkable/example#7", Owner: "talkable", Repo: "example", Number: 7,
		Title: "Add a thing", Author: "alice", URL: "https://github.com/talkable/example/pull/7",
		Issue: "PS-1", IssueURL: "https://example.atlassian.net/browse/PS-1", Draft: true,
		Labels: []string{"Flagged"}, Assignees: []string{"bob-rev"},
		State: "reviewed", SkipReason: "bot author",
		Badges:  []tui.Badge{{Label: "Flagged", Text: "!", Color: "red"}},
		GHState: "OPEN", UpdatedAt: at, HeadSHA: "abc1234",
		LastReview: &tui.ReviewInfo{Login: "zhuravel", Event: "APPROVED", SubmittedAt: at, CommitSHA: "abc1234", Stale: true, Mine: true},
		Findings: &tui.FindingsInfo{Counts: [4]int{1, 2, 3, 4}, Simplifications: 5, Fixed: 6, Open: 7, Answered: 8,
			Verdict: "blocking", Posted: "REQUEST_CHANGES", SHA: "abc1234"},
		CI: &tui.CIInfo{State: "failed", Total: 9, Passed: 5, Failed: 2, Pending: 1, Skipped: 1, Failing: []string{"CI / rspec"},
			Workflows:      []tui.WorkflowCI{{Name: "CI", State: "failed", Passed: 5, Failed: 2, Pending: 1, Total: 8}},
			Required:       []tui.CheckState{{Name: "workflow:CI", Label: "CI", State: "failed", Done: 7, Total: 8}},
			RequiredSource: "github", Stale: true},
		Reviewers:   []tui.ReviewerInfo{{Login: "rev-ann", Verdict: "approved", SubmittedAt: at, CommitSHA: "abc1234", Stale: true, Requested: true, Mine: true}},
		SinceReview: &tui.ReviewDelta{Base: "reviewed", BaseSHA: "def5678", Commits: 2, Files: 3, Additions: 40, Deletions: 5, Truncated: true},
		Slot:        "review3", Pinned: true, Muted: true, Notes: true, NextEligibleAt: at.Add(time.Hour),
		LastError: "boom", ErrorFix: "run magnum doctor", ErrorDetail: []string{"line one"}, RoundsToday: 3,
		LastRound: &tui.RoundTimings{Round: 2, Kind: "rereview", Total: 754 * time.Second, Running: true, Stages: []tui.StageTiming{
			{Name: "fetch/checkout", Duration: 12 * time.Second}, {Name: "claude-review", Duration: 18*time.Minute + 4*time.Second, Running: true, Failed: true}}},
		Progress: &tui.RoundProgress{StartedAt: at.Add(-20 * time.Minute), Roles: []tui.RoleProgress{
			{Role: "claude-simplify", Label: "simplify", Started: at.Add(-19 * time.Minute), Working: true},
			{Role: "codex-judge", Label: "judge", Judge: true}}},
		Wait: "re-review · quiet", WaitDetail: "waiting for a quiet period", Note: "comment-only push skipped",
		RequestedToMe: &req, LastRequest: &req, Requests: []tui.RequestInfo{req},
		ClosedAt: at.Add(2 * time.Hour), Recent: true, MergedUnreviewed: true,
	}
}

func marshalPRsJSON(t *testing.T, rows ...tui.PRBoardRow) []map[string]any {
	t.Helper()
	var b strings.Builder
	if err := writeJSON(&b, prsJSONRows(rows)); err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(b.String()), &out); err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	return out
}

// `magnum prs --json` marshalled tui.PRBoardRow, which has no json tags: keys
// came out PascalCase ("Ref", "GHState"), unset times as 0001-01-01, durations
// in nanoseconds, while every other --json output is snake_case. The output
// has its own shape now: snake_case keys at every depth, times RFC3339 and
// left out when unset, durations in seconds.
func TestPRsJSONKeysAreSnakeCase(t *testing.T) {
	rows := marshalPRsJSON(t, fullBoardRow())
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	walkJSON("", rows[0], func(path, key string, _ any) {
		if !jsonKeyRe.MatchString(key) {
			t.Errorf("key %q at %q is not snake_case", key, path)
		}
	})
	for _, key := range []string{"ref", "owner", "repo", "number", "title", "author", "url", "gh_state", "updated_at", "head_sha",
		"last_review", "since_review", "next_eligible_at", "rounds_today", "last_round", "requested_to_me", "merged_unreviewed", "closed_at"} {
		if _, ok := rows[0][key]; !ok {
			t.Errorf("no %q key in %v", key, keysOf(rows[0]))
		}
	}
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPRsJSONTimesAreRFC3339AndOmittedWhenUnset(t *testing.T) {
	full := marshalPRsJSON(t, fullBoardRow())[0]
	for _, key := range []string{"updated_at", "next_eligible_at", "closed_at"} {
		s, ok := full[key].(string)
		if !ok {
			t.Fatalf("%s = %v, want an RFC3339 string", key, full[key])
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			t.Errorf("%s = %q: %v", key, s, err)
		}
	}
	if got := full["updated_at"]; got != "2026-10-05T14:30:00Z" {
		t.Errorf("updated_at = %v", got)
	}
	review := full["last_review"].(map[string]any)
	if got := review["submitted_at"]; got != "2026-10-05T14:30:00Z" {
		t.Errorf("last_review.submitted_at = %v", got)
	}
	progress := full["progress"].(map[string]any)
	roles := progress["roles"].([]any)
	if got := progress["started_at"]; got != "2026-10-05T14:10:00Z" || len(roles) != 2 {
		t.Errorf("progress = %v", progress)
	} else if _, ok := roles[1].(map[string]any)["started_at"]; ok {
		t.Errorf("a role not started yet has a started_at: %v", roles[1])
	}

	// An open PR nobody scheduled, with a reviewer whose time is not known: no zero times.
	bare := fullBoardRow()
	bare.UpdatedAt, bare.NextEligibleAt, bare.ClosedAt = time.Time{}, time.Time{}, time.Time{}
	bare.LastReview.SubmittedAt = time.Time{}
	bare.Reviewers[0].SubmittedAt = time.Time{}
	bare.RequestedToMe.At = time.Time{}
	out := marshalPRsJSON(t, bare)[0]
	for _, key := range []string{"updated_at", "next_eligible_at", "closed_at"} {
		if v, ok := out[key]; ok {
			t.Errorf("unset %s is %v, want the key left out", key, v)
		}
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "0001-01-01") {
		t.Errorf("a zero time leaked: %s", raw)
	}
}

func TestPRsJSONDurationsAreSeconds(t *testing.T) {
	round := marshalPRsJSON(t, fullBoardRow())[0]["last_round"].(map[string]any)
	if got := round["total_seconds"]; got != float64(754) {
		t.Errorf("last_round.total_seconds = %v, want 754", got)
	}
	stages := round["stages"].([]any)
	if len(stages) != 2 {
		t.Fatalf("stages %v", stages)
	}
	if got := stages[0].(map[string]any)["duration_seconds"]; got != float64(12) {
		t.Errorf("stage 0 duration_seconds = %v, want 12", got)
	}
	if got := stages[1].(map[string]any)["duration_seconds"]; got != float64(1084) {
		t.Errorf("stage 1 duration_seconds = %v, want 1084", got)
	}
	for _, k := range []string{"total", "duration", "Total", "Duration"} {
		if _, ok := round[k]; ok {
			t.Errorf("last_round still has a nanosecond %q", k)
		}
	}
}

// Optional parts are null and lists are empty arrays, never missing or null,
// so a consumer indexes without checking the key first.
func TestPRsJSONOptionalPartsAreNullAndListsAreEmpty(t *testing.T) {
	out := marshalPRsJSON(t, tui.PRBoardRow{Ref: "talkable/example#1", Number: 1})[0]
	for _, key := range []string{"last_review", "findings", "ci", "since_review", "last_round", "progress", "requested_to_me", "last_request"} {
		if v, ok := out[key]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want null", key, v, ok)
		}
	}
	for _, key := range []string{"labels", "assignees", "badges", "reviewers", "requests", "error_detail"} {
		if v, ok := out[key].([]any); !ok || len(v) != 0 {
			t.Errorf("%s = %#v, want []", key, out[key])
		}
	}
	full := marshalPRsJSON(t, fullBoardRow())[0]
	ci := full["ci"].(map[string]any)
	if v, ok := ci["failing"].([]any); !ok || len(v) != 1 {
		t.Errorf("ci.failing = %#v", ci["failing"])
	}
	if got := full["findings"].(map[string]any)["counts"]; !reflect.DeepEqual(got, []any{1.0, 2.0, 3.0, 4.0}) {
		t.Errorf("findings.counts = %v, want [p0 p1 p2 p3]", got)
	}
	empty := marshalPRsJSON(t, tui.PRBoardRow{CI: &tui.CIInfo{}})[0]["ci"].(map[string]any)
	for _, key := range []string{"failing", "workflows", "required"} {
		if v, ok := empty[key].([]any); !ok || len(v) != 0 {
			t.Errorf("ci.%s = %#v, want []", key, empty[key])
		}
	}
}

// A field added to the board row, or to a type it holds, must get a place in
// the JSON: each output type has as many fields as the type it mirrors.
func TestPRsJSONMirrorsEveryBoardRowField(t *testing.T) {
	for _, p := range []struct{ board, out any }{
		{tui.PRBoardRow{}, prsJSONRow{}},
		{tui.Badge{}, prsJSONBadge{}},
		{tui.ReviewInfo{}, prsJSONReview{}},
		{tui.FindingsInfo{}, prsJSONFindings{}},
		{tui.CIInfo{}, prsJSONCI{}},
		{tui.WorkflowCI{}, prsJSONWorkflow{}},
		{tui.CheckState{}, prsJSONCheck{}},
		{tui.ReviewerInfo{}, prsJSONReviewer{}},
		{tui.ReviewDelta{}, prsJSONDelta{}},
		{tui.RoundTimings{}, prsJSONRound{}},
		{tui.StageTiming{}, prsJSONStage{}},
		{tui.RequestInfo{}, prsJSONRequest{}},
		{tui.RoundWhy{}, prsJSONRoundWhy{}},
		{tui.RoleRerun{}, prsJSONRoleRerun{}},
		{tui.RoundProgress{}, prsJSONProgress{}},
		{tui.RoleProgress{}, prsJSONRoleProgress{}},
		{tui.SpendInfo{}, prsJSONSpend{}},
	} {
		b, o := reflect.TypeOf(p.board), reflect.TypeOf(p.out)
		if b.NumField() != o.NumField() {
			t.Errorf("%s has %d fields, %s has %d: map the new field", b, b.NumField(), o, o.NumField())
		}
		for i := 0; i < o.NumField(); i++ {
			if tag := o.Field(i).Tag.Get("json"); tag == "" || tag == "-" {
				t.Errorf("%s.%s has no json tag", o, o.Field(i).Name)
			}
		}
	}
}

// The command itself prints that shape: the keys a script reads are
// snake_case, no time is the zero one, and no row is a bare null.
func TestPRsJSONCommandPrintsTheSnakeCaseShape(t *testing.T) {
	f := newInspFixture(t)
	prsSeed(t, f)
	if code := f.run("prs", "--all", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		walkJSON("", r, func(path, key string, _ any) {
			if !jsonKeyRe.MatchString(key) {
				t.Errorf("%v: key %q at %q is not snake_case", r["ref"], key, path)
			}
		})
		for _, key := range []string{"ref", "gh_state", "updated_at", "head_sha", "rounds_today"} {
			if _, ok := r[key]; !ok {
				t.Errorf("%v: no %q key (have %v)", r["ref"], key, keysOf(r))
			}
		}
	}
	if strings.Contains(f.Out.String(), "0001-01-01") {
		t.Errorf("a zero time in the output:\n%s", f.Out.String())
	}
}
