package store

import (
	"context"
	"fmt"
	"time"
)

// CandidateParams are the throttle settings Candidates applies (mirroring
// config.Daemon). Zero durations and a zero cap disable that gate.
type CandidateParams struct {
	Now              time.Time
	QuietPeriod      time.Duration // pending_since + QuietPeriod <= Now
	MinInterval      time.Duration // last_round_started_at + MinInterval <= Now
	DraftMinInterval time.Duration // the same for drafts; 0 = MinInterval
	MaxRoundsPerDay  int           // rounds_today < cap when rounds_day == Day
	Day              string        // DayKey(Now) when empty
}

// Candidates returns the PRs the dispatcher may start a round for, in
// dispatch order: forced first, then PRs where the watched user is a requested
// reviewer (review_requested), then oldest GitHub activity (gh_updated_at,
// falling back to created_at), then id.
//
// Every candidate is OPEN on GitHub, or MERGED and forced (a post-merge
// review: `magnum review` of a PR merged before magnum reviewed its last
// push), not muted (unless forced) and past its retry backoff
// (next_attempt_at). Within that:
//   - queued PRs qualify when forced or next_eligible_at is unset or due;
//   - rereview_pending PRs qualify when forced, or when next_eligible_at is
//     unset or due AND the push quiet period, the (draft) minimum interval and
//     the daily round cap all allow it.
func (s *Store) Candidates(ctx context.Context, p CandidateParams) ([]PR, error) {
	now := p.Now
	if now.IsZero() {
		now = s.now()
	}
	day := p.Day
	if day == "" {
		day = DayKey(now)
	}
	draftInterval := p.DraftMinInterval
	if draftInterval == 0 {
		draftInterval = p.MinInterval
	}
	nowS := FormatTime(now)
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("", prColumns)+` FROM prs
WHERE (gh_state = ? OR (gh_state = ? AND forced = 1))
  AND (muted = 0 OR forced = 1)
  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
  AND (
    (state = ? AND (forced = 1 OR next_eligible_at IS NULL OR next_eligible_at <= ?))
    OR (state = ? AND (forced = 1 OR (
          (next_eligible_at IS NULL OR next_eligible_at <= ?)
      AND (pending_since IS NULL OR pending_since <= ?)
      AND (last_round_started_at IS NULL OR last_round_started_at <= CASE WHEN is_draft = 1 THEN ? ELSE ? END)
      AND (? = 0 OR rounds_day IS NULL OR rounds_day <> ? OR rounds_today < ?)
    )))
  )
ORDER BY forced DESC, review_requested DESC, COALESCE(gh_updated_at, created_at) ASC, id ASC`,
		GHOpen, GHMerged, nowS,
		PRQueued, nowS,
		PRRereviewPending, nowS, FormatTime(now.Add(-p.QuietPeriod)),
		FormatTime(now.Add(-draftInterval)), FormatTime(now.Add(-p.MinInterval)),
		p.MaxRoundsPerDay, day, p.MaxRoundsPerDay)
	if err != nil {
		return nil, fmt.Errorf("candidates: %w", err)
	}
	return collect(rows, scanPR)
}

// ClosedPastGrace returns closed PRs whose release_after has passed (an
// unset release_after counts as passed) and that have no active run, oldest
// deadline first.
func (s *Store) ClosedPastGrace(ctx context.Context, now time.Time) ([]PR, error) {
	args := []any{PRClosed, FormatTime(now)}
	args = append(args, anys(activeRunStates)...)
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("p", prColumns)+` FROM prs p
WHERE p.state = ? AND (p.release_after IS NULL OR p.release_after <= ?)
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.pr_id = p.id AND r.state IN (`+placeholders(len(activeRunStates))+`))
ORDER BY COALESCE(p.release_after, ''), p.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("closed past grace: %w", err)
	}
	return collect(rows, scanPR)
}
