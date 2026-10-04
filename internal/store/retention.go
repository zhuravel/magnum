package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PruneResult counts the rows Prune deleted.
type PruneResult struct {
	Events   int64
	Requests int64
}

// Prune deletes events older than keepEvents and handled requests (state
// done or failed, never pending) created more than keepRequests ago, both
// measured from the store's clock, in one transaction. A keep of zero or less
// disables pruning of that table. It is meant for the daemon's maintenance
// pass: nothing else ever removes these rows.
//
// Step rows of a subject's current generation (kind step, and the step.reset
// row that opened it) are kept whatever their age, because StepDone reads
// them to resume a sequence; only superseded generations are pruned.
func (s *Store) Prune(ctx context.Context, keepEvents, keepRequests time.Duration) (PruneResult, error) {
	var out PruneResult
	now := s.now()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if keepEvents > 0 {
			res, err := tx.ExecContext(ctx, `
DELETE FROM events
WHERE at < ?
  AND NOT (kind IN (?, ?) AND id >= COALESCE(
    (SELECT MAX(r.id) FROM events r WHERE r.subject = events.subject AND r.kind = ?), 0))`,
				FormatTime(now.Add(-keepEvents)), KindStep, KindStepReset, KindStepReset)
			if err != nil {
				return fmt.Errorf("events: %w", err)
			}
			if out.Events, err = res.RowsAffected(); err != nil {
				return fmt.Errorf("events: %w", err)
			}
		}
		if keepRequests > 0 {
			res, err := tx.ExecContext(ctx, "DELETE FROM requests WHERE state IN (?, ?) AND created_at < ?",
				RequestDone, RequestFailed, FormatTime(now.Add(-keepRequests)))
			if err != nil {
				return fmt.Errorf("requests: %w", err)
			}
			if out.Requests, err = res.RowsAffected(); err != nil {
				return fmt.Errorf("requests: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return PruneResult{}, fmt.Errorf("prune: %w", err)
	}
	return out, nil
}
