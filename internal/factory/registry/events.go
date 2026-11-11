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
// required and phase must be begin, ok, fail or info. Callers scrub text
// (scrub.Text: secrets, emails and hosts) before they put it into an event.
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
	evs, err := collect(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("events of task %d: %w", taskID, err)
	}
	slices.Reverse(evs)
	return evs, nil
}

// TaskEvents returns task taskID's events of kind, oldest first; limit > 0
// keeps only the newest limit of them. A read that a screen repeats, or a
// check of a task's own stage events, stays bounded by its kind and limit
// however long the task's log grows.
func (r *Registry) TaskEvents(ctx context.Context, taskID int64, kind string, limit int) ([]Event, error) {
	q := "SELECT id, at, task_id, kind, phase, text FROM events WHERE task_id = ? AND kind = ? ORDER BY id DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := r.db.QueryContext(ctx, q, taskID, kind)
	if err != nil {
		return nil, fmt.Errorf("%s events of task %d: %w", kind, taskID, err)
	}
	evs, err := collect(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("%s events of task %d: %w", kind, taskID, err)
	}
	slices.Reverse(evs)
	return evs, nil
}

// RecentEvents returns the newest limit events of the given kinds, of
// every task and of the daemon itself, newest first (limit <= 0: all).
// No kind is no event.
func (r *Registry) RecentEvents(ctx context.Context, limit int, kinds ...string) ([]Event, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	q := "SELECT id, at, task_id, kind, phase, text FROM events WHERE kind IN (" + placeholders(len(kinds)) + ") ORDER BY id DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("recent events: %w", err)
	}
	evs, err := collect(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("recent events: %w", err)
	}
	return evs, nil
}

// EventsAfter returns the events of the given kinds written after event
// after (an id; 0: from the first), of every task and of the daemon
// itself, oldest first, at most limit of them (limit <= 0: all). No kind is
// no event.
func (r *Registry) EventsAfter(ctx context.Context, after int64, limit int, kinds ...string) ([]Event, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(kinds)+1)
	for _, k := range kinds {
		args = append(args, k)
	}
	q := "SELECT id, at, task_id, kind, phase, text FROM events WHERE kind IN (" + placeholders(len(kinds)) + ") AND id > ? ORDER BY id"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := r.db.QueryContext(ctx, q, append(args, after)...)
	if err != nil {
		return nil, fmt.Errorf("events after %d: %w", after, err)
	}
	evs, err := collect(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("events after %d: %w", after, err)
	}
	return evs, nil
}

func scanEvent(sc scanner) (Event, error) {
	var e Event
	var tid *int64
	err := sc.Scan(&e.ID, timeCol{&e.At}, &tid, &e.Kind, &e.Phase, &e.Text)
	if tid != nil {
		e.TaskID = *tid
	}
	return e, err
}
