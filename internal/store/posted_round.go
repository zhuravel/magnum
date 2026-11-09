package store

import (
	"context"
	"fmt"
	"time"
)

// FirstPostedReview returns the first round of PR prID that posted a review
// and began at or after since (its first run, a reviewer's before the
// judge's, created then; the rounds in the order they began), with the
// findings it posted, every priority, in the judge's order: the round as
// ReviewedPRs reads a posted review (its run is the earliest of the round
// that carries the review, Summary what that run's result says, OK false
// for none that parses), its findings the posted rows of that round
// (findings.verdict posted). A round that began before since does not
// count, however late it posted. One query; false when no such round
// exists yet.
func (s *Store) FirstPostedReview(ctx context.Context, prID int64, since time.Time) (PostedReview, []Finding, bool, error) {
	rows, err := s.db.QueryContext(ctx, `WITH r AS (
  SELECT id, round, target_sha, review_id, outcome, result_json, created_at, rowid AS seq,
         MIN(created_at) OVER (PARTITION BY round) AS round_start,
         FIRST_VALUE(target_sha) OVER (PARTITION BY round ORDER BY created_at, rowid) AS round_target
  FROM runs WHERE pr_id = ?
), x AS (
  SELECT * FROM r WHERE outcome = 'posted' AND review_id IS NOT NULL AND round_start >= ?
  ORDER BY round_start, created_at, seq LIMIT 1
)
SELECT COALESCE(f.id, 0), COALESCE(f.run_id, ''), COALESCE(f.pr_id, 0), COALESCE(f.round, 0), COALESCE(f.finding_id, ''), f.title,
  f.severity, f.path, f.line, COALESCE(f.sources_json, '[]'), COALESCE(f.verdict, ''), f.reason_code, COALESCE(f.nearby, 0),
  COALESCE(f.created_at, x.created_at),
  x.id, x.round, x.target_sha, x.review_id, x.created_at, x.round_start, x.round_target, COALESCE(x.result_json, '')
FROM x LEFT JOIN findings f ON f.pr_id = ? AND f.round = x.round AND f.verdict = ?
ORDER BY f.created_at, f.id`, prID, FormatTime(since), prID, FindingPosted)
	if err != nil {
		return PostedReview{}, nil, false, fmt.Errorf("first posted review of pr %d: %w", prID, err)
	}
	defer rows.Close()
	var (
		pr     PostedReview
		result string
		found  bool
		out    []Finding
	)
	for rows.Next() {
		var f Finding
		if err := scanFindingInto(rows, &f, &pr.RunID, &pr.Round, &pr.SHA, &pr.ReviewID, timeCol(&pr.CreatedAt), timeCol(&pr.RoundStart),
			&pr.RoundTarget, &result); err != nil {
			return PostedReview{}, nil, false, fmt.Errorf("first posted review of pr %d: %w", prID, err)
		}
		found = true
		if f.ID != 0 { // a round that posted no finding is one row with none
			out = append(out, f)
		}
	}
	if err := rows.Err(); err != nil {
		return PostedReview{}, nil, false, fmt.Errorf("first posted review of pr %d: %w", prID, err)
	}
	if !found {
		return PostedReview{}, nil, false, nil
	}
	pr.Summary = ReviewSummary{RunID: pr.RunID, ReviewID: pr.ReviewID, SHA: pr.SHA}
	pr.OK = result != "" && ParseReviewResult([]byte(result), &pr.Summary)
	return pr, out, true, nil
}
