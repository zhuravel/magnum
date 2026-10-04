package store

import (
	"context"
	"encoding/json"
	"fmt"
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
// findings it posted by priority, the simplifications it suggested, how
// the earlier findings stood, and its verdict. A repository whose policy
// only comments still has a verdict here: what the review would have
// decided.
type ReviewSummary struct {
	RunID           string    `json:"run_id"`
	SHA             string    `json:"sha"`   // the reviewed head
	Event           string    `json:"event"` // what was posted: APPROVE, REQUEST_CHANGES or COMMENT
	URL             string    `json:"url,omitempty"`
	At              time.Time `json:"at"`
	Counts          [4]int    `json:"counts"`          // P0..P3 posted this round
	Simplifications int       `json:"simplifications"` // optional suggestions posted this round
	Fixed           int       `json:"fixed"`           // earlier findings fixed (a re-review)
	Open            int       `json:"open"`            // earlier findings still open
	Answered        int       `json:"answered"`        // earlier findings answered with a reason
	Verdict         string    `json:"verdict"`         // VerdictBlocking, VerdictNonBlocking or VerdictClean
}

// Findings is the number of findings posted this round.
func (s ReviewSummary) Findings() int { return s.Counts[0] + s.Counts[1] + s.Counts[2] + s.Counts[3] }

// LastReviewSummaries returns the ReviewSummary of each PR's latest posted
// round, for the PRs that have one. A result file that does not parse is
// skipped (the round still counts as posted elsewhere).
func (s *Store) LastReviewSummaries(ctx context.Context, prIDs []int64) (map[int64]ReviewSummary, error) {
	out := map[int64]ReviewSummary{}
	if len(prIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT pr_id, id, target_sha, coalesce(review_event, ''), coalesce(review_url, ''),
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
		if err := rows.Scan(&prID, &sum.RunID, &sum.SHA, &sum.Event, &sum.URL, &at, &payload); err != nil {
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

// ParseReviewResult fills sum from a judge result file (the skill's section
// 8 JSON): findings by priority, simplifications suggested (the candidates'
// `suggested`), earlier findings, the posted event and the verdict. A result
// without a verdict (written before the skill had one) gets it from the
// counts: P0 or P1 blocks, other findings or still-open earlier ones
// comment, none is clean. It reports false when data is not a result.
func ParseReviewResult(data []byte, sum *ReviewSummary) bool {
	var r struct {
		Event    string         `json:"event"`
		Verdict  string         `json:"verdict"`
		Findings map[string]int `json:"findings"`
		Previous struct {
			Fixed    int `json:"fixed"`
			Open     int `json:"open"`
			Answered int `json:"answered"`
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
	sum.Fixed, sum.Open, sum.Answered = r.Previous.Fixed, r.Previous.Open, r.Previous.Answered
	if sum.Event == "" {
		sum.Event = r.Event
	}
	switch r.Verdict {
	case VerdictBlocking, VerdictNonBlocking, VerdictClean:
		sum.Verdict = r.Verdict
	default:
		switch {
		case sum.Counts[0]+sum.Counts[1] > 0:
			sum.Verdict = VerdictBlocking
		case sum.Findings() > 0 || sum.Open > 0:
			sum.Verdict = VerdictNonBlocking
		default:
			sum.Verdict = VerdictClean
		}
	}
	return true
}

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
