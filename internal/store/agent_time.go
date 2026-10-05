package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
)

// PRAgentTime is what one PR's runs cost over a window (AgentTimeSince).
type PRAgentTime struct {
	PRID   int64
	Repo   string // owner/name
	Number int
	Time   time.Duration // the sum of its runs' durations
	Rounds int           // the distinct rounds those runs belong to
}

// AgentTimeSince sums, per PR, the durations of the runs created at or after
// since and counts the distinct rounds they belong to; a PR without such a
// run has no entry. A run lasts from its submission (its creation when it was
// never submitted) to its end; one still going (pending, submitted, working)
// lasts to now, and a finished run that never got an end stops where it was
// last seen (verified, else working), or lasts nothing. A duration is never
// negative. prIDs limits the PRs (none: every PR). The result is the PR with
// the most agent time first, ties by PR id.
//
// The durations are summed here, not in SQL: the runs' timestamps are text.
func (s *Store) AgentTimeSince(ctx context.Context, since, now time.Time, prIDs ...int64) ([]PRAgentTime, error) {
	q := "SELECT r.pr_id, r.round, r.state, r.created_at, r.submitted_at, r.working_seen_at, r.ended_at, r.verified_at," +
		" rp.owner || '/' || rp.name, p.number" +
		" FROM runs r JOIN prs p ON p.id = r.pr_id JOIN repos rp ON rp.id = p.repo_id WHERE r.created_at >= ?"
	args := []any{FormatTime(since)}
	if len(prIDs) > 0 {
		q += " AND r.pr_id IN (" + placeholders(len(prIDs)) + ")"
		for _, id := range prIDs {
			args = append(args, id)
		}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("agent time since %s: %w", FormatTime(since), err)
	}
	type roundKey struct {
		pr    int64
		round int
	}
	byPR := map[int64]*PRAgentTime{}
	rounds := map[roundKey]bool{}
	_, err = collect(rows, func(sc scanner) (struct{}, error) {
		var (
			prID                                int64
			round, number                       int
			state, repo                         string
			created                             time.Time
			submitted, working, ended, verified *time.Time
		)
		if err := sc.Scan(&prID, &round, &state, timeCol(&created), nullTime(&submitted), nullTime(&working), nullTime(&ended),
			nullTime(&verified), &repo, &number); err != nil {
			return struct{}{}, err
		}
		p := byPR[prID]
		if p == nil {
			p = &PRAgentTime{PRID: prID, Repo: repo, Number: number}
			byPR[prID] = p
		}
		p.Time += runDuration(state, created, submitted, working, ended, verified, now)
		if k := (roundKey{prID, round}); !rounds[k] {
			rounds[k] = true
			p.Rounds++
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("agent time since %s: %w", FormatTime(since), err)
	}
	out := make([]PRAgentTime, 0, len(byPR))
	for _, p := range byPR {
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b PRAgentTime) int {
		return cmp.Or(cmp.Compare(b.Time, a.Time), cmp.Compare(a.PRID, b.PRID))
	})
	return out, nil
}

// runDuration is how long a run kept its agent busy, as AgentTimeSince
// counts it.
func runDuration(state string, created time.Time, submitted, working, ended, verified *time.Time, now time.Time) time.Duration {
	start := created
	if submitted != nil {
		start = *submitted
	}
	var end time.Time
	switch {
	case ended != nil:
		end = *ended
	case slices.Contains(activeRunStates, state):
		end = now
	case verified != nil:
		end = *verified
	case working != nil:
		end = *working
	default:
		return 0
	}
	return max(end.Sub(start), 0)
}
