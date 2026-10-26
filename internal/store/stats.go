package store

import (
	"context"
	"fmt"
	"time"
)

// Reads for `magnum stats`: the runs of the rounds in a window, the events
// of a few kinds, the reviews of the PRs that ended in it (convergence) and
// its merges (approvals). FindingsSince (findings.go), AutoApprovalsByPR
// (auto_approvals.go) and Misses (learn.go) are the others.

// RoundRun is a run with its PR's repository (owner/name) and number.
type RoundRun struct {
	Run
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// RunsOfRoundsSince returns every run of each round (pr_id, round) that has a
// run created at or after since, oldest first, with its PR's repository and
// number. A round that began before since and went on after it comes whole,
// so the caller can tell when it began.
func (s *Store) RunsOfRoundsSince(ctx context.Context, since time.Time) ([]RoundRun, error) {
	q := "SELECT " + cols("r", runColumns) + ", rp.owner || '/' || rp.name, p.number" +
		" FROM runs r JOIN prs p ON p.id = r.pr_id JOIN repos rp ON rp.id = p.repo_id" +
		" WHERE (r.pr_id, r.round) IN (SELECT pr_id, round FROM runs WHERE created_at >= ?)" +
		" ORDER BY r.created_at, r.rowid"
	rows, err := s.db.QueryContext(ctx, q, FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("runs since %s: %w", FormatTime(since), err)
	}
	out, err := collect(rows, func(sc scanner) (RoundRun, error) {
		var rr RoundRun
		r, err := scanRun(extraScanner{sc, []any{&rr.Repo, &rr.Number}})
		rr.Run = r
		return rr, err
	})
	if err != nil {
		return nil, fmt.Errorf("runs since %s: %w", FormatTime(since), err)
	}
	return out, nil
}

// EventsOfKindsSince returns the events of the given kinds at or after since,
// oldest first; no kinds returns nil.
func (s *Store) EventsOfKindsSince(ctx context.Context, since time.Time, kinds ...string) ([]Event, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	q := "SELECT " + cols("", eventColumns) + " FROM events WHERE kind IN (" + placeholders(len(kinds)) + ") AND at >= ? ORDER BY id"
	args := append(anys(kinds), FormatTime(since))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events since %s: %w", FormatTime(since), err)
	}
	evs, err := collect(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("events since %s: %w", FormatTime(since), err)
	}
	return evs, nil
}

// PostedReview is one review a PR's round posted, read from the earliest
// run that carries its review id (outcome posted): a nudge, a continue or a
// recovery that posted the same review adds nothing.
type PostedReview struct {
	RunID    string
	Round    int
	SHA      string // the reviewed head (target_sha)
	ReviewID int64
	// CreatedAt is when the run was created, RoundStart when the first run
	// of its round was (a reviewer's, before the judge's), and RoundTarget
	// that first run's head: the head the round started on, which SHA is
	// not when a push restarted the round on a newer one.
	CreatedAt   time.Time
	RoundStart  time.Time
	RoundTarget string
	// Summary is what the judge's result says (ParseReviewResult); OK is
	// false when the run stored no result that parses.
	Summary ReviewSummary
	OK      bool
}

// ReviewedPR is a PR with every review its rounds posted, oldest first.
type ReviewedPR struct {
	ID       int64
	Repo     string // owner/name
	Number   int
	Author   string // "" for a ghost
	GHState  string // GHOpen, GHMerged or GHClosed
	MergedAt *time.Time
	ClosedAt *time.Time
	Reviews  []PostedReview
}

// End is when the PR ended: merged, else closed; nil while it is neither.
func (p ReviewedPR) End() *time.Time {
	if p.MergedAt != nil {
		return p.MergedAt
	}
	return p.ClosedAt
}

// ReviewedPRs returns the PRs with at least one posted review that are
// open, merged or closed at or after since, or have a finding recorded at or
// after since, by id, each with every review its rounds posted (whenever
// they did) in the order their runs were created.
func (s *Store) ReviewedPRs(ctx context.Context, since time.Time) ([]ReviewedPR, error) {
	at := FormatTime(since)
	rows, err := s.db.QueryContext(ctx, `WITH scope AS (
  SELECT p.id FROM prs p
  WHERE p.gh_state = ? OR COALESCE(p.merged_at, p.closed_at) >= ?
     OR EXISTS (SELECT 1 FROM findings f WHERE f.pr_id = p.id AND f.created_at >= ?)
), r AS (
  SELECT id, pr_id, round, target_sha, review_id, outcome, result_json, created_at, rowid AS seq,
         MIN(created_at) OVER (PARTITION BY pr_id, round) AS round_start,
         FIRST_VALUE(target_sha) OVER (PARTITION BY pr_id, round ORDER BY created_at, rowid) AS round_target
  FROM runs WHERE pr_id IN (SELECT id FROM scope)
), posted AS (
  SELECT *, ROW_NUMBER() OVER (PARTITION BY pr_id, review_id ORDER BY created_at, seq) AS n
  FROM r WHERE outcome = 'posted' AND review_id IS NOT NULL
)
SELECT p.id, rp.owner || '/' || rp.name, p.number, COALESCE(p.author_login, ''), p.gh_state, p.merged_at, p.closed_at,
  x.id, x.round, x.target_sha, x.review_id, x.created_at, x.round_start, x.round_target, COALESCE(x.result_json, '')
FROM posted x JOIN prs p ON p.id = x.pr_id JOIN repos rp ON rp.id = p.repo_id
WHERE x.n = 1
ORDER BY p.id, x.created_at, x.seq`, GHOpen, at, at)
	if err != nil {
		return nil, fmt.Errorf("reviewed prs: %w", err)
	}
	defer rows.Close()
	var out []ReviewedPR
	for rows.Next() {
		var (
			p      ReviewedPR
			r      PostedReview
			result string
		)
		if err := rows.Scan(&p.ID, &p.Repo, &p.Number, &p.Author, &p.GHState, nullTime(&p.MergedAt), nullTime(&p.ClosedAt),
			&r.RunID, &r.Round, &r.SHA, &r.ReviewID, timeCol(&r.CreatedAt), timeCol(&r.RoundStart), &r.RoundTarget, &result); err != nil {
			return nil, fmt.Errorf("reviewed prs: %w", err)
		}
		r.Summary = ReviewSummary{RunID: r.RunID, ReviewID: r.ReviewID, SHA: r.SHA}
		r.OK = result != "" && ParseReviewResult([]byte(result), &r.Summary)
		if n := len(out); n > 0 && out[n-1].ID == p.ID {
			out[n-1].Reviews = append(out[n-1].Reviews, r)
			continue
		}
		p.Reviews = []PostedReview{r}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reviewed prs: %w", err)
	}
	return out, nil
}

// MergedPRsSince lists the PRs merged at or after since, with their
// repository, by repository and number.
func (s *Store) MergedPRsSince(ctx context.Context, since time.Time) ([]RepoPR, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("p", prColumns)+`, r.owner || '/' || r.name
FROM prs p JOIN repos r ON r.id = p.repo_id
WHERE p.gh_state = ? AND p.merged_at >= ?
ORDER BY r.owner, r.name, p.number`, GHMerged, FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("merged prs: %w", err)
	}
	list, err := collect(rows, func(sc scanner) (RepoPR, error) {
		var c RepoPR
		p, err := scanPR(rowWithTail{sc, &c.Repo})
		c.PR = p
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("merged prs: %w", err)
	}
	return list, nil
}

// extraScanner scans a row's leading columns with the caller's destinations
// and the trailing ones into extra.
type extraScanner struct {
	sc    scanner
	extra []any
}

func (e extraScanner) Scan(dest ...any) error { return e.sc.Scan(append(dest, e.extra...)...) }
