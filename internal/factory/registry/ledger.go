package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Delivery verdicts of a ledger entry. magnum sends a prompt before it saves
// that the prompt was sent, so a crash between the two leaves the entry
// unknown until recovery reads the agent's transcript for the prompt's hash.
const (
	DeliveredUnknown = "unknown"
	DeliveredYes     = "yes"
	DeliveredNo      = "no"
)

// LedgerEntry is one prompt the factory sends (or is about to send) to an
// agent: the scrubbed text and its hash, written before the prompt leaves.
type LedgerEntry struct {
	ID        int64
	TaskID    int64
	AttemptID string
	Role      string
	RunID     string
	Hash      string
	Text      string
	Delivered string
	Sent      time.Time
}

const ledgerColumns = "id, task_id, attempt_id, role, run_id, hash, text, delivered, sent_at"

func scanLedger(sc scanner) (LedgerEntry, error) {
	var e LedgerEntry
	err := sc.Scan(&e.ID, &e.TaskID, &e.AttemptID, &e.Role, &e.RunID, &e.Hash, &e.Text, &e.Delivered, timeCol{&e.Sent})
	return e, err
}

// AppendLedger writes e before its prompt is sent and returns the stored
// entry. TaskID, AttemptID (an existing attempt, ErrNotFound if there is
// none), Role, Hash and Text are required, and TaskID must be the task of the
// attempt (checked in the same transaction as the insert); Delivered starts
// unknown, and Sent defaults to the clock.
func (r *Registry) AppendLedger(ctx context.Context, e LedgerEntry) (LedgerEntry, error) {
	if e.AttemptID == "" || e.Role == "" || e.Hash == "" || e.Text == "" {
		return LedgerEntry{}, errors.New("append ledger: attempt, role, hash and text are required")
	}
	if e.Delivered != "" && e.Delivered != DeliveredUnknown {
		return LedgerEntry{}, fmt.Errorf("append ledger: a new entry is %s, not %q", DeliveredUnknown, e.Delivered)
	}
	if e.Sent.IsZero() {
		e.Sent = r.now()
	}
	err := r.tx(ctx, func(tx *sql.Tx) error {
		var owner int64
		if err := tx.QueryRowContext(ctx, "SELECT task_id FROM attempts WHERE id = ?", e.AttemptID).Scan(&owner); err != nil {
			return notFound(err, "attempt", e.AttemptID)
		}
		if owner != e.TaskID {
			return fmt.Errorf("attempt %s belongs to task %d, not task %d", e.AttemptID, owner, e.TaskID)
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO ledger (task_id, attempt_id, role, run_id, hash, text, delivered, sent_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			e.TaskID, e.AttemptID, e.Role, e.RunID, e.Hash, e.Text, DeliveredUnknown, store.FormatTime(e.Sent))
		if err != nil {
			return mapErr(err)
		}
		e.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("append ledger for attempt %s: %w", e.AttemptID, err)
	}
	e.Delivered, e.Sent = DeliveredUnknown, e.Sent.UTC()
	return e, nil
}

// MarkDelivered settles an unknown entry as yes (the agent got the prompt) or
// no (it did not). Settling an entry with the verdict it already has is not an
// error; a different verdict is ErrConflict, and a missing entry ErrNotFound.
func (r *Registry) MarkDelivered(ctx context.Context, id int64, delivered string) error {
	if delivered != DeliveredYes && delivered != DeliveredNo {
		return fmt.Errorf("mark delivered %d: verdict must be %s or %s, got %q", id, DeliveredYes, DeliveredNo, delivered)
	}
	res, err := r.db.ExecContext(ctx, "UPDATE ledger SET delivered = ? WHERE id = ? AND delivered = ?", delivered, id, DeliveredUnknown)
	if err != nil {
		return fmt.Errorf("mark delivered %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var current string
	if err := r.db.QueryRowContext(ctx, "SELECT delivered FROM ledger WHERE id = ?", id).Scan(&current); err != nil {
		return notFound(err, "ledger entry", id)
	}
	if current == delivered {
		return nil
	}
	return fmt.Errorf("mark delivered %d: %w: already %s", id, ErrConflict, current)
}

// Undelivered returns the entries whose delivery is still unknown, oldest
// first: the prompts recovery has to settle after a restart.
func (r *Registry) Undelivered(ctx context.Context) ([]LedgerEntry, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+ledgerColumns+" FROM ledger WHERE delivered = ? ORDER BY id", DeliveredUnknown)
	if err != nil {
		return nil, fmt.Errorf("undelivered prompts: %w", err)
	}
	es, err := collect(rows, scanLedger)
	if err != nil {
		return nil, fmt.Errorf("undelivered prompts: %w", err)
	}
	return es, nil
}

// LedgerOf returns the task's entries in the order they were written: the
// prompts its agents were sent, or were about to be.
func (r *Registry) LedgerOf(ctx context.Context, taskID int64) ([]LedgerEntry, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+ledgerColumns+" FROM ledger WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, fmt.Errorf("ledger of task %d: %w", taskID, err)
	}
	es, err := collect(rows, scanLedger)
	if err != nil {
		return nil, fmt.Errorf("ledger of task %d: %w", taskID, err)
	}
	return es, nil
}
