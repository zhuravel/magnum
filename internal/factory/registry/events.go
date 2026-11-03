package registry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Event phases: begin, ok and fail bracket a destructive step; info is
// everything else.
const (
	PhaseBegin = "begin"
	PhaseOK    = "ok"
	PhaseFail  = "fail"
	PhaseInfo  = "info"
)

// Event is one row of the audit log.
type Event struct {
	ID     int64
	At     time.Time
	TaskID int64 // 0 for the daemon's own events
	Kind   string
	Phase  string
	Text   string
}

// AppendEvent writes an audit row at the clock's time. taskID 0 is an event of
// the daemon itself; any other must be an existing task (ErrNotFound). Kind is
// required and phase must be begin, ok, fail or info. Callers redact secrets
// (execx.Redact) before they put command output into text.
func (r *Registry) AppendEvent(ctx context.Context, taskID int64, kind, phase, text string) error {
	if kind == "" {
		return errors.New("append event: kind is required")
	}
	switch phase {
	case PhaseBegin, PhaseOK, PhaseFail, PhaseInfo:
	default:
		return fmt.Errorf("append event %s: unknown phase %q", kind, phase)
	}
	var task any
	if taskID != 0 {
		task = taskID
	}
	_, err := r.db.ExecContext(ctx, "INSERT INTO events (at, task_id, kind, phase, text) VALUES (?, ?, ?, ?, ?)",
		store.FormatTime(r.now()), task, kind, phase, text)
	if err != nil {
		return fmt.Errorf("append event %s: %w", kind, mapErr(err))
	}
	return nil
}

// Events returns a task's events oldest first (taskID 0: the daemon's own);
// limit > 0 keeps only the newest limit rows.
func (r *Registry) Events(ctx context.Context, taskID int64, limit int) ([]Event, error) {
	var task any
	if taskID != 0 {
		task = taskID
	}
	q := "SELECT id, at, task_id, kind, phase, text FROM events WHERE task_id IS ? ORDER BY id DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := r.db.QueryContext(ctx, q, task)
	if err != nil {
		return nil, fmt.Errorf("events of task %d: %w", taskID, err)
	}
	evs, err := collect(rows, func(sc scanner) (Event, error) {
		var e Event
		var tid *int64
		err := sc.Scan(&e.ID, timeCol{&e.At}, &tid, &e.Kind, &e.Phase, &e.Text)
		if tid != nil {
			e.TaskID = *tid
		}
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("events of task %d: %w", taskID, err)
	}
	slices.Reverse(evs)
	return evs, nil
}
