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
// One kind of old event is kept: the step rows of a subject's current
// generation (kind step, and the step.reset row that opened it), because
// StepDone reads them to resume a sequence, and only while the subject is in
// use, that is while it has an event of any kind inside the window. A subject
// quiet for the whole window is not mid-sequence (subjects carry the head's
// sha, so a finished checkout never comes back), and its step rows are pruned
// like any other; a sequence that did run again would repeat its steps, which
// package steps already requires to be idempotent. Superseded generations are
// pruned by age as before.
func (s *Store) Prune(ctx context.Context, keepEvents, keepRequests time.Duration) (PruneResult, error) {
	var out PruneResult
	now := s.now()
	cutoff := now.Add(-keepEvents)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if keepEvents > 0 {
			res, err := tx.ExecContext(ctx, `
DELETE FROM events
WHERE at < ?
  AND NOT (kind IN (?, ?)
    AND id >= COALESCE((SELECT MAX(r.id) FROM events r WHERE r.subject = events.subject AND r.kind = ?), 0)
    AND EXISTS (SELECT 1 FROM events a WHERE a.subject = events.subject AND a.at >= ?))`,
				FormatTime(cutoff), KindStep, KindStepReset, KindStepReset, FormatTime(cutoff))
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
