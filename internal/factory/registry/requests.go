package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// LastDone is the newest request of kinds the daemon answered done at or
// after since (false for none): the status rule's news, read typed.
func (r *Registry) LastDone(ctx context.Context, since time.Time, kinds ...string) (Request, bool, error) {
	if len(kinds) == 0 {
		return Request{}, false, nil
	}
	args := []any{RequestDone, store.FormatTime(since)}
	for _, k := range kinds {
		args = append(args, k)
	}
	q, err := scanRequest(r.db.QueryRowContext(ctx, "SELECT "+requestColumns+" FROM requests WHERE state = ? AND handled_at >= ? AND kind IN ("+
		placeholders(len(kinds))+") ORDER BY handled_at DESC, id DESC LIMIT 1", args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, fmt.Errorf("last done request: %w", err)
	}
	return q, true, nil
}

// ForgetPayload empties the payload of request id once it is done or
// failed (a pending one is ErrConflict: the daemon still reads it), so a
// request that carried your draft (a preview) keeps none of it.
func (r *Registry) ForgetPayload(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, "UPDATE requests SET payload_json = '{}' WHERE id = ? AND state <> ?", id, RequestPending)
	if err != nil {
		return fmt.Errorf("forget the payload of request %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("forget the payload of request %d: %w: pending or missing", id, ErrConflict)
	}
	return nil
}

// PruneRequests deletes the requests of kinds the daemon answered before
// before, and the `request` events of their answers ("request <id>
// (<kind>): ..."), and returns how many requests went. A pending request
// stays.
func (r *Registry) PruneRequests(ctx context.Context, before time.Time, kinds ...string) (int, error) {
	if len(kinds) == 0 {
		return 0, nil
	}
	args := []any{RequestPending, store.FormatTime(before)}
	for _, k := range kinds {
		args = append(args, k)
	}
	n := 0
	err := r.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT "+requestColumns+" FROM requests WHERE state <> ? AND handled_at < ? AND kind IN ("+
			placeholders(len(kinds))+")", args...)
		if err != nil {
			return err
		}
		gone, err := collect(rows, scanRequest)
		if err != nil {
			return err
		}
		like := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
		for _, q := range gone {
			if _, err := tx.ExecContext(ctx, "DELETE FROM requests WHERE id = ?", q.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM events WHERE kind = 'request' AND task_id IS NULL AND text LIKE ? ESCAPE '\\'",
				fmt.Sprintf("request %d (%s): %%", q.ID, like.Replace(q.Kind))); err != nil {
				return err
			}
		}
		n = len(gone)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prune requests: %w", err)
	}
	return n, nil
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
