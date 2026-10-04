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
	q := "SELECT " + cols("r", runColumns) + " FROM runs r WHERE r.round = (SELECT MAX(x.round) FROM runs x WHERE x.pr_id = r.pr_id)"
	args := make([]any, 0, len(prIDs))
	if len(prIDs) > 0 {
		q += " AND r.pr_id IN (" + placeholders(len(prIDs)) + ")"
		for _, id := range prIDs {
			args = append(args, id)
		}
	}
	q += " ORDER BY r.pr_id, r.created_at, r.rowid"
	rows, err := s.db.QueryContext(ctx, q, args...)
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
