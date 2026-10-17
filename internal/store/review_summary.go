package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Verdicts: what a review concluded, whatever its repository lets it post
// (ReviewSummary.Verdict).
const (
	VerdictBlocking    = "blocking"     // at least one P0 or P1: request changes
	VerdictNonBlocking = "non_blocking" // only P2 and P3: comment
	VerdictClean       = "clean"        // nothing to fix: approve
)

// ReviewSummary is what a PR's latest posted review round concluded, read
// from the judge's result file the round stored (runs.result_json): the
// new findings it posted by priority, the simplifications it suggested, how
// the earlier findings stood (those still open by priority, when the result
// gives them), and its verdict. A repository whose policy only comments
// still has a verdict here: what the review would have decided.
type ReviewSummary struct {
	RunID           string    `json:"run_id"`
	ReviewID        int64     `json:"review_id"` // the review the round posted
	SHA             string    `json:"sha"`       // the reviewed head
	Event           string    `json:"event"`     // what was posted: APPROVE, REQUEST_CHANGES or COMMENT
	URL             string    `json:"url,omitempty"`
	At              time.Time `json:"at"`
	Counts          [4]int    `json:"counts"`          // P0..P3 posted as new this round
	Simplifications int       `json:"simplifications"` // optional suggestions posted this round
	Fixed           int       `json:"fixed"`           // earlier findings fixed (a re-review)
	Open            int       `json:"open"`            // earlier findings still open
	// OpenCounts is Open by priority, P0..P3 (the result's
	// previous_findings.open as an object); nil when the result gives only
	// their number (written before the skill split them), so their
	// priorities are unknown.
	OpenCounts *[4]int `json:"open_counts,omitempty"`
	Answered   int     `json:"answered"` // earlier findings answered with a reason
	Verdict    string  `json:"verdict"`  // VerdictBlocking, VerdictNonBlocking or VerdictClean
}

// Findings is the number of findings posted this round.
func (s ReviewSummary) Findings() int { return s.Counts[0] + s.Counts[1] + s.Counts[2] + s.Counts[3] }

// OpenProblems is how many earlier findings the review keeps open that its
// verdict counts as problems to fix before merging: the P0, P1 and P2 of
// OpenCounts (an open P3 stays optional), or all of Open when the result
// gave only their number.
func (s ReviewSummary) OpenProblems() int {
	if s.OpenCounts == nil {
		return s.Open
	}
	return s.OpenCounts[0] + s.OpenCounts[1] + s.OpenCounts[2]
}

// VerdictWords is the review's verdict line in a few words, rebuilt from
// the summary as the review skill writes it (section 7), without its
// period, its LGTM or a re-review's lead: "Blocking: 2 problems must be
// fixed before merging" (a P0 or P1, new or still open), "Fix 1 problem
// before merging" (P2), "No blocking problems" (optional ones only) or "No
// problems found", the optional ones (P3s, new or still open, and
// simplifications) counted after a comma ("Fix 1 problem before merging, 4
// optional"). The PR status comment's posted line carries it; it names no
// score and no event.
func (s ReviewSummary) VerdictWords() string {
	problems := s.Counts[0] + s.Counts[1] + s.Counts[2] + s.OpenProblems()
	optional := s.Counts[3] + s.Simplifications
	severe := s.Counts[0] + s.Counts[1]
	if s.OpenCounts != nil {
		optional += s.OpenCounts[3]
		severe += s.OpenCounts[0] + s.OpenCounts[1]
	}
	plural := func(n int) string {
		if n == 1 {
			return "1 problem"
		}
		return strconv.Itoa(n) + " problems"
	}
	var words string
	switch {
	case (severe > 0 || s.Verdict == VerdictBlocking) && problems > 0:
		words = "Blocking: " + plural(problems) + " must be fixed before merging"
	case s.Verdict == VerdictBlocking:
		words = "Blocking: problems must be fixed before merging"
	case problems > 0:
		words = "Fix " + plural(problems) + " before merging"
	case optional > 0 || s.Verdict == VerdictNonBlocking:
		words = "No blocking problems"
	default:
		return "No problems found"
	}
	if optional > 0 {
		words += ", " + strconv.Itoa(optional) + " optional"
	}
	return words
}

// LastReviewSummaries returns the ReviewSummary of each PR's latest posted
// round, for the PRs that have one. A result file that does not parse is
// skipped (the round still counts as posted elsewhere).
func (s *Store) LastReviewSummaries(ctx context.Context, prIDs []int64) (map[int64]ReviewSummary, error) {
	out := map[int64]ReviewSummary{}
	if len(prIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT pr_id, id, review_id, target_sha, coalesce(review_event, ''), coalesce(review_url, ''),
  coalesce(verified_at, ended_at, created_at), result_json
FROM runs
WHERE pr_id IN (`+placeholders(len(prIDs))+`) AND outcome = 'posted' AND review_id IS NOT NULL AND result_json IS NOT NULL
ORDER BY pr_id, created_at DESC`, int64Args(prIDs)...)
	if err != nil {
		return nil, fmt.Errorf("review summaries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			prID        int64
			sum         ReviewSummary
			at, payload string
		)
		if err := rows.Scan(&prID, &sum.RunID, &sum.ReviewID, &sum.SHA, &sum.Event, &sum.URL, &at, &payload); err != nil {
			return nil, fmt.Errorf("review summaries: %w", err)
		}
		if _, seen := out[prID]; seen {
			continue // an older round
		}
		if t, err := ParseTime(at); err == nil {
			sum.At = t
		}
		if ParseReviewResult([]byte(payload), &sum) {
			out[prID] = sum
		}
	}
	return out, rows.Err()
}

// PostedPromptAt is when the judge prompt behind the PR's latest review
// posted on target went out (runs.submitted_at): that of the first run of
// its round posted with the review, the judge's run of the round (in a
// two-prompt round its candidates prompt), whose prompt carried the head's
// failing checks; a nudge or a model fallback continues that turn later.
// ok is false when no review posted on target has a prompt time.
func (s *Store) PostedPromptAt(ctx context.Context, prID int64, target string) (time.Time, bool, error) {
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT r.submitted_at FROM runs r
JOIN (SELECT round, review_id FROM runs WHERE pr_id = ? AND target_sha = ? AND outcome = 'posted' AND review_id IS NOT NULL
      ORDER BY created_at DESC, rowid DESC LIMIT 1) p ON r.round = p.round AND r.review_id = p.review_id
WHERE r.pr_id = ? AND r.outcome = 'posted' AND r.submitted_at IS NOT NULL
ORDER BY r.created_at, r.rowid LIMIT 1`, prID, target, prID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("posted prompt of pr %d: %w", prID, err)
	}
	t, err := ParseTime(at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("posted prompt of pr %d: %w", prID, err)
	}
	return t, true, nil
}

// ParseReviewResult fills sum from a judge result file (the skill's section
// 8 JSON): new findings by priority, simplifications suggested (the
// candidates' `suggested`), earlier findings (previous_findings.open by
// priority, or, in an older result, their number), the posted event and the
// verdict. A result without a verdict (written before the skill had one)
// gets it from the counts: a P0 or P1, new or still open, blocks, other
// findings or still-open earlier ones comment, none is clean. It reports
// false when data is not a result.
func ParseReviewResult(data []byte, sum *ReviewSummary) bool {
	var r struct {
		Event    string         `json:"event"`
		Verdict  string         `json:"verdict"`
		Findings map[string]int `json:"findings"`
		Previous struct {
			Fixed    int             `json:"fixed"`
			Open     json.RawMessage `json:"open"`
			Answered int             `json:"answered"`
		} `json:"previous_findings"`
		Candidates map[string]struct {
			Suggested int `json:"suggested"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.Findings == nil {
		return false
	}
	for i, k := range []string{"P0", "P1", "P2", "P3"} {
		sum.Counts[i] = max(r.Findings[k], 0)
	}
	for _, c := range r.Candidates {
		sum.Simplifications += max(c.Suggested, 0)
	}
	sum.Fixed, sum.Answered = r.Previous.Fixed, r.Previous.Answered
	sum.Open, sum.OpenCounts = parseOpen(r.Previous.Open)
	if sum.Event == "" {
		sum.Event = r.Event
	}
	switch r.Verdict {
	case VerdictBlocking, VerdictNonBlocking, VerdictClean:
		sum.Verdict = r.Verdict
	default:
		switch {
		case sum.Counts[0]+sum.Counts[1] > 0, sum.OpenCounts != nil && sum.OpenCounts[0]+sum.OpenCounts[1] > 0:
			sum.Verdict = VerdictBlocking
		case sum.Findings() > 0 || sum.Open > 0:
			sum.Verdict = VerdictNonBlocking
		default:
			sum.Verdict = VerdictClean
		}
	}
	return true
}

// parseOpen reads previous_findings.open: counts by priority ({"P0":0,
// "P1":2,...}; a missing priority or a negative count is 0) with their sum,
// or, in a result written before the skill split them, a plain number and
// no priorities. Anything else is no open finding.
func parseOpen(m json.RawMessage) (int, *[4]int) {
	var n int
	if json.Unmarshal(m, &n) == nil {
		return max(n, 0), nil
	}
	var byPriority map[string]int
	if json.Unmarshal(m, &byPriority) != nil || byPriority == nil {
		return 0, nil
	}
	var counts [4]int
	total := 0
	for i, k := range []string{"P0", "P1", "P2", "P3"} {
		counts[i] = max(byPriority[k], 0)
		total += counts[i]
	}
	return total, &counts
}

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
