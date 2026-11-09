package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// AppendEvent writes an audit row and returns its id. At defaults to now and
// Level to "info"; Kind and Message are required. Callers must redact
// secrets (execx.Redact) before putting command output into Message.
func (s *Store) AppendEvent(ctx context.Context, e Event) (int64, error) {
	return s.appendEvent(ctx, s.db, e)
}

// appendEvent is AppendEvent on q.
func (s *Store) appendEvent(ctx context.Context, q querier, e Event) (int64, error) {
	if e.Kind == "" || e.Message == "" {
		return 0, errors.New("append event: kind and message are required")
	}
	at := e.At
	if at.IsZero() {
		at = s.now()
	}
	if e.Level == "" {
		e.Level = "info"
	}
	res, err := q.ExecContext(ctx, "INSERT INTO events (at, level, subject, kind, step, phase, message, data_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		FormatTime(at), e.Level, e.Subject, e.Kind, e.Step, e.Phase, e.Message, mustDB(e.Data))
	if err != nil {
		return 0, fmt.Errorf("append event %s: %w", e.Kind, err)
	}
	return res.LastInsertId()
}

// EventsBySubject returns a subject's events oldest first; limit > 0 keeps
// only the newest limit rows.
func (s *Store) EventsBySubject(ctx context.Context, subject string, limit int) ([]Event, error) {
	q := "SELECT " + cols("", eventColumns) + " FROM events WHERE subject = ? ORDER BY id DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, subject)
	if err != nil {
		return nil, fmt.Errorf("events of %s: %w", subject, err)
	}
	evs, err := collect(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("events of %s: %w", subject, err)
	}
	slices.Reverse(evs)
	return evs, nil
}

// SubjectMatch selects events by subject for EventsMatching. A non-empty
// Exact matches a subject equal to it; a non-empty Prefix matches a subject
// starting with it. Both are case-sensitive and compare bytes, so
// "slot:review3:" never matches "slot:Review3:x", and no character in them is
// a wildcard. Set both to match either; an empty SubjectMatch matches nothing.
type SubjectMatch struct {
	Exact  string
	Prefix string
}

// EventsMatching returns the events with id > after whose subject satisfies
// any of matches, oldest first; limit > 0 keeps only the newest limit rows.
// Without any usable match it returns nil.
func (s *Store) EventsMatching(ctx context.Context, matches []SubjectMatch, after int64, limit int) ([]Event, error) {
	var conds []string
	var args []any
	for _, m := range matches {
		if m.Exact != "" {
			conds = append(conds, "subject = ?")
			args = append(args, m.Exact)
		}
		if m.Prefix != "" {
			conds = append(conds, "subject GLOB ?")
			args = append(args, globLiteral(m.Prefix)+"*")
		}
	}
	if len(conds) == 0 {
		return nil, nil
	}
	q := "SELECT " + cols("", eventColumns) + " FROM events WHERE (" + strings.Join(conds, " OR ") + ") AND id > ? ORDER BY id"
	args = append(args, after)
	if limit > 0 {
		q += fmt.Sprintf(" DESC LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events matching subjects: %w", err)
	}
	evs, err := collect(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("events matching subjects: %w", err)
	}
	if limit > 0 {
		slices.Reverse(evs)
	}
	return evs, nil
}

// globLiteral escapes the GLOB wildcards in s so it matches itself. GLOB (not
// LIKE) because it is case-sensitive and, on the indexed subject column, a
// literal prefix is served by events_subject_id.
func globLiteral(s string) string {
	return strings.NewReplacer("*", "[*]", "?", "[?]", "[", "[[]").Replace(s)
}

// StepDone reports whether a step event with phase ok exists for (subject,
// step) after the subject's latest KindStepReset row (its current step
// generation). See package steps.
func (s *Store) StepDone(ctx context.Context, subject, step string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM events
WHERE subject = ? AND kind = ? AND step = ? AND phase = ?
  AND id > COALESCE((SELECT MAX(id) FROM events WHERE subject = ? AND kind = ?), 0)`,
		subject, KindStep, step, PhaseOK, subject, KindStepReset).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("step %s/%s: %w", subject, step, err)
	}
	return n > 0, nil
}

// AppendEventOnce is AppendEvent unless an event of e's subject, kind, step
// and phase already carries key, in its data's "key" (set here, beside e's
// own data fields): a delivery that comes at least once is written once. It
// reports whether it wrote e.
func (s *Store) AppendEventOnce(ctx context.Context, e Event, key string) (bool, error) {
	if key == "" {
		return false, errors.New("append event once: the key is required")
	}
	data := map[string]any{}
	if len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return false, fmt.Errorf("append event once %s: its data is no object: %w", e.Kind, err)
		}
	}
	data["key"] = key
	b, err := json.Marshal(data)
	if err != nil {
		return false, fmt.Errorf("append event once %s: %w", e.Kind, err)
	}
	e.Data = b
	added := false
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		// The subject's index (events_subject_id) bounds the scan to its events.
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE subject IS ? AND kind = ? AND step IS ? AND phase IS ?
  AND json_extract(data_json, '$.key') = ?`, e.Subject, e.Kind, e.Step, e.Phase, key).Scan(&n); err != nil {
			return fmt.Errorf("append event once %s: %w", e.Kind, err)
		}
		if n > 0 {
			return nil
		}
		if _, err := s.appendEvent(ctx, tx, e); err != nil {
			return err
		}
		added = true
		return nil
	})
	return added, err
}
