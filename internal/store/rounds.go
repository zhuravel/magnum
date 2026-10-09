package store

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// LatestRoundRuns returns, per PR, the runs of its highest round, oldest
// first. prIDs limits the PRs; empty means every PR with a run. A PR
// without runs has no entry.
func (s *Store) LatestRoundRuns(ctx context.Context, prIDs ...int64) (map[int64][]Run, error) {
	args := make([]any, 0, len(prIDs))
	for _, id := range prIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, latestRoundRunsQuery(len(prIDs)), args...)
	if err != nil {
		return nil, fmt.Errorf("latest round runs: %w", err)
	}
	runs, err := collect(rows, scanRun)
	if err != nil {
		return nil, fmt.Errorf("latest round runs: %w", err)
	}
	out := map[int64][]Run{}
	for _, r := range runs {
		out[r.PRID] = append(out[r.PRID], r)
	}
	return out, nil
}

// latestRoundRunsQuery is LatestRoundRuns' query for n PR ids (0 = all):
// the PRs' highest rounds grouped once, joined back to their runs (a
// correlated MAX would read a PR's runs again for each of them).
func latestRoundRunsQuery(n int) string {
	latest := "SELECT pr_id, MAX(round) AS round FROM runs"
	if n > 0 {
		latest += " WHERE pr_id IN (" + placeholders(n) + ")"
	}
	return "SELECT " + cols("r", runColumns) + " FROM (" + latest + " GROUP BY pr_id) m" +
		" JOIN runs r ON r.pr_id = m.pr_id AND r.round = m.round ORDER BY r.pr_id, r.created_at, r.rowid"
}

// CheckoutSteps returns the step events (KindStep and KindStepReset) of the
// PR's checkouts, oldest first, at most limit of the newest (0 = all): the
// subjects "slot:<slot>:pr:<N>:<sha7>" of every slot the PR was assigned to
// and its per-PR worktree's own "slot:<owner>/<name>#<N>".
func (s *Store) CheckoutSteps(ctx context.Context, prID int64, limit int) ([]Event, error) {
	var owner, name string
	var number int
	err := s.db.QueryRowContext(ctx,
		"SELECT r.owner, r.name, p.number FROM prs p JOIN repos r ON r.id = p.repo_id WHERE p.id = ?", prID).Scan(&owner, &name, &number)
	if err != nil {
		return nil, notFound(err, "pr", prID)
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT DISTINCT s.name FROM assignments a JOIN slots s ON s.id = a.slot_id WHERE a.pr_id = ? ORDER BY s.name", prID)
	if err != nil {
		return nil, fmt.Errorf("checkout steps of pr %d: %w", prID, err)
	}
	slots, err := collect(rows, func(sc scanner) (string, error) {
		var n string
		return n, sc.Scan(&n)
	})
	if err != nil {
		return nil, fmt.Errorf("checkout steps of pr %d: %w", prID, err)
	}
	perPR := owner + "/" + name + "#" + strconv.Itoa(number)
	if !slices.Contains(slots, perPR) {
		slots = append(slots, perPR)
	}
	conds := []string{"subject = ?"}
	args := []any{"slot:" + perPR}
	for _, sl := range slots {
		conds = append(conds, "subject GLOB ?")
		args = append(args, globLiteral(fmt.Sprintf("slot:%s:pr:%d:", sl, number))+"*")
	}
	q := "SELECT " + cols("", eventColumns) + " FROM events WHERE kind IN (?, ?) AND (" + strings.Join(conds, " OR ") + ") ORDER BY id DESC"
	args = append([]any{KindStep, KindStepReset}, args...)
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	evRows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("checkout steps of pr %d: %w", prID, err)
	}
	evs, err := collect(evRows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("checkout steps of pr %d: %w", prID, err)
	}
	slices.Reverse(evs)
	return evs, nil
}
