package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Tracking is where a task's Jira issue stands as the factory daemon last
// read it (migrations/0006_tracking.sql, and 0008 for who moved it): the facts of the last read that
// worked, of the key JiraKey, and the last try, which a failure moves with
// its cause while the facts stay.
type Tracking struct {
	TaskID  int64
	JiraKey string
	// Status is the status's name, StatusID its id, Category Jira's
	// category of it (new, indeterminate, done; "" unknown), Since when the
	// issue moved into it (zero: unknown) and Searched whether the changelog
	// was searched for that (jira.IssueStatus's).
	Status, StatusID, Category string
	Since                      time.Time
	Searched                   bool
	// Assigned says the issue has an assignee, AssigneeYou that it is you.
	Assigned, AssigneeYou bool
	// MovedBy is who moved the issue into Status: "you", "other", or ""
	// when unknown (jira.IssueStatus's); never a name or an id.
	MovedBy string
	// ReadAt is the read the facts are from, zero before one worked;
	// TriedAt the last try, never zero; LastError that try's failure, ""
	// after a read that worked: a class of failure, never ticket text.
	ReadAt, TriedAt time.Time
	LastError       string
}

const trackingColumns = "task_id, jira_key, jira_status, jira_status_id, jira_category, jira_since, since_searched, assigned, " +
	"assignee_you, read_at, tried_at, last_error, moved_by"

func scanTracking(sc scanner) (Tracking, error) {
	var t Tracking
	err := sc.Scan(&t.TaskID, &t.JiraKey, &t.Status, &t.StatusID, &t.Category, timeCol{&t.Since}, &t.Searched, &t.Assigned,
		&t.AssigneeYou, timeCol{&t.ReadAt}, timeCol{&t.TriedAt}, &t.LastError, &t.MovedBy)
	return t, err
}

// SetTracking writes task t.TaskID's tracking row whole, replacing the one
// it had. A row needs its try's time; a missing task is ErrNotFound.
func (r *Registry) SetTracking(ctx context.Context, t Tracking) error {
	if t.TriedAt.IsZero() {
		return errors.New("tracking: a row needs the time of its try")
	}
	_, err := r.db.ExecContext(ctx, "INSERT OR REPLACE INTO tracking ("+trackingColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.TaskID, t.JiraKey, t.Status, t.StatusID, t.Category, timeArg(t.Since), t.Searched, t.Assigned, t.AssigneeYou && t.Assigned,
		timeArg(t.ReadAt), store.FormatTime(t.TriedAt), t.LastError, t.MovedBy)
	if err != nil {
		return fmt.Errorf("tracking of task %d: %w", t.TaskID, mapErr(err))
	}
	return nil
}

// Tracking is task id's tracking row; ErrNotFound before its first try.
func (r *Registry) Tracking(ctx context.Context, id int64) (Tracking, error) {
	t, err := scanTracking(r.db.QueryRowContext(ctx, "SELECT "+trackingColumns+" FROM tracking WHERE task_id = ?", id))
	if err != nil {
		return Tracking{}, notFound(err, "tracking of task", id)
	}
	return t, nil
}

// Trackings are every task's tracking row, by task id.
func (r *Registry) Trackings(ctx context.Context) (map[int64]Tracking, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+trackingColumns+" FROM tracking")
	if err != nil {
		return nil, fmt.Errorf("trackings: %w", err)
	}
	ts, err := collect(rows, scanTracking)
	if err != nil {
		return nil, fmt.Errorf("trackings: %w", err)
	}
	out := make(map[int64]Tracking, len(ts))
	for _, t := range ts {
		out[t.TaskID] = t
	}
	return out, nil
}
