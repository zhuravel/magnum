package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// EnqueueRequest inserts a pending request with payload marshalled as JSON
// (nil = {}) and returns its id.
func (s *Store) EnqueueRequest(ctx context.Context, kind string, payload any) (int64, error) {
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
	res, err := s.db.ExecContext(ctx, "INSERT INTO requests (kind, payload_json, state, created_at) VALUES (?, ?, ?, ?)",
		kind, string(body), RequestPending, FormatTime(s.now()))
	if err != nil {
		return 0, fmt.Errorf("enqueue request %s: %w", kind, err)
	}
	return res.LastInsertId()
}

// PendingRequests returns up to limit pending requests, oldest first
// (limit <= 0 = all), so a consumer can skip a request it already handed to
// a background worker.
func (s *Store) PendingRequests(ctx context.Context, limit int) ([]Request, error) {
	q := "SELECT " + cols("", requestColumns) + " FROM requests WHERE state = ? ORDER BY id"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, RequestPending)
	if err != nil {
		return nil, fmt.Errorf("pending requests: %w", err)
	}
	reqs, err := collect(rows, scanRequest)
	if err != nil {
		return nil, fmt.Errorf("pending requests: %w", err)
	}
	return reqs, nil
}

// RequestByID looks up a request by id.
func (s *Store) RequestByID(ctx context.Context, id int64) (Request, error) {
	r, err := scanRequest(s.db.QueryRowContext(ctx, "SELECT "+cols("", requestColumns)+" FROM requests WHERE id = ?", id))
	if err != nil {
		return Request{}, notFound(err, "request", id)
	}
	return r, nil
}

// CompleteRequest marks a pending request done or failed with a result
// message. Completing a request twice is ErrConflict.
func (s *Store) CompleteRequest(ctx context.Context, id int64, state, result string) error {
	if state != RequestDone && state != RequestFailed {
		return fmt.Errorf("complete request %d: state must be done or failed, got %q", id, state)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE requests SET state = ?, result = ?, handled_at = ? WHERE id = ? AND state = ?",
		state, result, FormatTime(s.now()), id, RequestPending)
	if err != nil {
		return fmt.Errorf("complete request %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("complete request %d: %w: not pending", id, ErrConflict)
	}
	return nil
}
