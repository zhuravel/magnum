package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Request states.
const (
	RequestPending = "pending"
	RequestDone    = "done"
	RequestFailed  = "failed"
)

// Request is a CLI to daemon work item.
type Request struct {
	ID      int64
	Kind    string
	Payload json.RawMessage
	State   string
	Result  string
	Created time.Time
	Handled time.Time // the zero time while the request is pending
}

const requestColumns = "id, kind, payload_json, state, result, created_at, handled_at"

func scanRequest(sc scanner) (Request, error) {
	var q Request
	var payload string
	err := sc.Scan(&q.ID, &q.Kind, &payload, &q.State, &q.Result, timeCol{&q.Created}, timeCol{&q.Handled})
	q.Payload = json.RawMessage(payload)
	return q, err
}

// EnqueueRequest inserts a pending request with payload marshalled as JSON
// (nil = {}) and returns its id.
func (r *Registry) EnqueueRequest(ctx context.Context, kind string, payload any) (int64, error) {
	if kind == "" {
		return 0, errors.New("enqueue request: empty kind")
	}
	body := []byte("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("enqueue request %s: %w", kind, err)
		}
		body = b
	}
	res, err := r.db.ExecContext(ctx, "INSERT INTO requests (kind, payload_json, state, created_at) VALUES (?, ?, ?, ?)",
		kind, string(body), RequestPending, store.FormatTime(r.now()))
	if err != nil {
		return 0, fmt.Errorf("enqueue request %s: %w", kind, err)
	}
	return res.LastInsertId()
}

// PendingRequests returns up to limit pending requests, oldest first
// (limit <= 0 = all), so a consumer can skip a request it already handed to
// a background worker.
func (r *Registry) PendingRequests(ctx context.Context, limit int) ([]Request, error) {
	q := "SELECT " + requestColumns + " FROM requests WHERE state = ? ORDER BY id"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := r.db.QueryContext(ctx, q, RequestPending)
	if err != nil {
		return nil, fmt.Errorf("pending requests: %w", err)
	}
	reqs, err := collect(rows, scanRequest)
	if err != nil {
		return nil, fmt.Errorf("pending requests: %w", err)
	}
	return reqs, nil
}

// RequestByID looks up a request; a missing one is ErrNotFound.
func (r *Registry) RequestByID(ctx context.Context, id int64) (Request, error) {
	q, err := scanRequest(r.db.QueryRowContext(ctx, "SELECT "+requestColumns+" FROM requests WHERE id = ?", id))
	if err != nil {
		return Request{}, notFound(err, "request", id)
	}
	return q, nil
}

// CompleteRequest marks a pending request done or failed with a result
// message. Completing a request twice is ErrConflict.
func (r *Registry) CompleteRequest(ctx context.Context, id int64, state, result string) error {
	if state != RequestDone && state != RequestFailed {
		return fmt.Errorf("complete request %d: state must be done or failed, got %q", id, state)
	}
	res, err := r.db.ExecContext(ctx, "UPDATE requests SET state = ?, result = ?, handled_at = ? WHERE id = ? AND state = ?",
		state, result, store.FormatTime(r.now()), id, RequestPending)
	if err != nil {
		return fmt.Errorf("complete request %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("complete request %d: %w: not pending", id, ErrConflict)
	}
	return nil
}
