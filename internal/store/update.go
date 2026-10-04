package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Update collects column assignments for one row, and optional extra guards
// on the same UPDATE (Where). Column names are the schema's; id, state,
// created_at and updated_at cannot be set (state changes go through the
// Transition* `to` argument, updated_at is maintained by the store). The first
// invalid call is remembered and returned by the store method that applies
// the update.
//
// Accepted values: nil (NULL), string, bool, int, int64, float64, time.Time
// (the zero time stores NULL), pointers to those (nil pointer = NULL),
// []string, map[string]string, []LatestReview, SinceReview and *SinceReview
// (stored as JSON; a nil *SinceReview stores NULL) and json.RawMessage.
type Update struct {
	table    *tableSpec
	sets     []string
	args     []any
	conds    []string // extra WHERE terms from Where, ANDed after the state guard
	condCols []string // the columns named by conds, for error messages
	condArgs []any
	touched  map[string]bool
	err      error
}

// PRUpdate is an Update on the prs table.
type PRUpdate struct{ Update }

// SlotUpdate is an Update on the slots table.
type SlotUpdate struct{ Update }

// SessionUpdate is an Update on the sessions table.
type SessionUpdate struct{ Update }

// RunUpdate is an Update on the runs table.
type RunUpdate struct{ Update }

// Set assigns v to column col.
func (u *Update) Set(col string, v any) {
	if !u.check(col) {
		return
	}
	dv, err := dbValue(v)
	if err != nil {
		u.fail(fmt.Errorf("%s.%s: %w", u.table.name, col, err))
		return
	}
	u.sets = append(u.sets, col+" = ?")
	u.args = append(u.args, dv)
}

// Inc adds n to the integer column col.
func (u *Update) Inc(col string, n int) {
	if !u.check(col) {
		return
	}
	u.sets = append(u.sets, fmt.Sprintf("%s = %s + ?", col, col))
	u.args = append(u.args, n)
}

// Copy sets column dst to the row's current value of src (read before the
// update, so Copy("prev_state", "state") records the state being left).
func (u *Update) Copy(dst, src string) {
	if !u.check(dst) {
		return
	}
	if !u.table.columns[src] {
		u.fail(fmt.Errorf("%s: unknown column %q", u.table.name, src))
		return
	}
	u.sets = append(u.sets, dst+" = "+src)
}

// Where requires column col to equal v (a nil v requires NULL) for the UPDATE
// to match, in the same statement as the id and state guard. When the row
// exists but the condition fails, the store method returns ErrConflict, like
// a state mismatch, so a caller can require that the row still describes what
// it decided on (for example head_sha == the target sha). col may be any
// column of the table; conditions combine with AND.
func (u *Update) Where(col string, v any) {
	if u.err != nil {
		return
	}
	if !u.table.columns[col] {
		u.fail(fmt.Errorf("%s: unknown column %q", u.table.name, col))
		return
	}
	dv, err := dbValue(v)
	if err != nil {
		u.fail(fmt.Errorf("%s.%s: %w", u.table.name, col, err))
		return
	}
	u.condCols = append(u.condCols, col)
	if dv == nil {
		u.conds = append(u.conds, col+" IS NULL")
		return
	}
	u.conds = append(u.conds, col+" = ?")
	u.condArgs = append(u.condArgs, dv)
}

func (u *Update) check(col string) bool {
	if u.err != nil {
		return false
	}
	if !u.table.settable[col] {
		u.fail(fmt.Errorf("%s: column %q cannot be set", u.table.name, col))
		return false
	}
	if u.touched[col] {
		u.fail(fmt.Errorf("%s: column %q set twice", u.table.name, col))
		return false
	}
	if u.touched == nil {
		u.touched = map[string]bool{}
	}
	u.touched[col] = true
	return true
}

func (u *Update) fail(err error) {
	if u.err == nil {
		u.err = err
	}
}

// dbValue converts a Go value into a database/sql argument.
func dbValue(v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string, int, int64, float64:
		return x, nil
	case bool:
		return boolInt(x), nil
	case time.Time:
		if x.IsZero() {
			return nil, nil
		}
		return FormatTime(x), nil
	case *string:
		return derefAny(x), nil
	case *int:
		return derefAny(x), nil
	case *int64:
		return derefAny(x), nil
	case *float64:
		return derefAny(x), nil
	case *bool:
		if x == nil {
			return nil, nil
		}
		return boolInt(*x), nil
	case *time.Time:
		if x == nil || x.IsZero() {
			return nil, nil
		}
		return FormatTime(*x), nil
	case json.RawMessage:
		if x == nil {
			return nil, nil
		}
		return string(x), nil
	case []string:
		if x == nil {
			x = []string{}
		}
		b, err := json.Marshal(x)
		return string(b), err
	case map[string]string:
		if x == nil {
			x = map[string]string{}
		}
		b, err := json.Marshal(x)
		return string(b), err
	case []LatestReview:
		if x == nil {
			x = []LatestReview{}
		}
		b, err := json.Marshal(x)
		return string(b), err
	case SinceReview:
		b, err := json.Marshal(x)
		return string(b), err
	case *SinceReview:
		if x == nil {
			return nil, nil
		}
		b, err := json.Marshal(x)
		return string(b), err
	case *CIStatus:
		if x == nil {
			return nil, nil
		}
		b, err := json.Marshal(x)
		return string(b), err
	default:
		return nil, fmt.Errorf("unsupported value type %T", v)
	}
}

func derefAny[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// tableSpec describes a table that supports Update/Transition.
type tableSpec struct {
	name       string
	columns    map[string]bool
	settable   map[string]bool
	hasUpdated bool // maintain updated_at
}

func newTableSpec(name string, columns []string) *tableSpec {
	t := &tableSpec{name: name, columns: map[string]bool{}, settable: map[string]bool{}}
	for _, c := range columns {
		t.columns[c] = true
		switch c {
		case "id", "state", "created_at":
		case "updated_at":
			t.hasUpdated = true
		default:
			t.settable[c] = true
		}
	}
	return t
}

var (
	prTable      = newTableSpec("prs", prColumns)
	slotTable    = newTableSpec("slots", slotColumns)
	sessionTable = newTableSpec("sessions", sessionColumns)
	runTable     = newTableSpec("runs", runColumns)
)

// apply runs one compare-and-set UPDATE on t: assignments from u, state = to
// (when to != ""), updated_at = now (when the table has it), guarded by
// id = ?, when from is non-empty state IN (from), and u's Where conditions.
// Zero matched rows is ErrNotFound when the row is missing and ErrConflict
// otherwise.
func (s *Store) apply(ctx context.Context, q querier, t *tableSpec, id any, from []string, to string, u *Update) error {
	if u.err != nil {
		return u.err
	}
	sets := append([]string(nil), u.sets...)
	args := append([]any(nil), u.args...)
	if to != "" {
		sets = append(sets, "state = ?")
		args = append(args, to)
	}
	if t.hasUpdated {
		sets = append(sets, "updated_at = ?")
		args = append(args, FormatTime(s.now()))
	}
	if len(sets) == 0 {
		sets = append(sets, "id = id") // still verify existence and the state guard
	}
	query := "UPDATE " + t.name + " SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	args = append(args, id)
	if len(from) > 0 {
		query += " AND state IN (" + placeholders(len(from)) + ")"
		args = append(args, anys(from)...)
	}
	for _, c := range u.conds {
		query += " AND " + c
	}
	args = append(args, u.condArgs...)
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	var state string
	err = q.QueryRowContext(ctx, "SELECT state FROM "+t.name+" WHERE id = ?", id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if len(u.conds) > 0 {
		return fmt.Errorf("%w: state is %s, want one of %v (or a guard on %s failed)", ErrConflict, state, from, strings.Join(u.condCols, ", "))
	}
	return fmt.Errorf("%w: state is %s, want one of %v", ErrConflict, state, from)
}

// transition is the shared body of the public Transition*/Update* methods.
func (s *Store) transition(ctx context.Context, t *tableSpec, id any, from []string, to string, u *Update) error {
	if err := s.apply(ctx, s.db, t, id, from, to, u); err != nil {
		if to == "" {
			return fmt.Errorf("update %s %v: %w", t.name, id, err)
		}
		return fmt.Errorf("transition %s %v %v -> %s: %w", t.name, id, from, to, err)
	}
	return nil
}

// TransitionPR moves PR id to state `to` only if its current state is one of
// from (nil from = any state; empty to = keep the state), applying set's
// assignments in the same UPDATE. It returns ErrConflict when the state or a
// condition added with Where did not match and ErrNotFound when the PR does
// not exist.
func (s *Store) TransitionPR(ctx context.Context, id int64, from []string, to string, set func(*PRUpdate)) error {
	u := &PRUpdate{Update{table: prTable}}
	if set != nil {
		set(u)
	}
	return s.transition(ctx, prTable, id, from, to, &u.Update)
}

// UpdatePR changes non-state columns of PR id.
func (s *Store) UpdatePR(ctx context.Context, id int64, set func(*PRUpdate)) error {
	return s.TransitionPR(ctx, id, nil, "", set)
}

// TransitionSlot is TransitionPR for slots.
func (s *Store) TransitionSlot(ctx context.Context, id int64, from []string, to string, set func(*SlotUpdate)) error {
	u := &SlotUpdate{Update{table: slotTable}}
	if set != nil {
		set(u)
	}
	return s.transition(ctx, slotTable, id, from, to, &u.Update)
}

// UpdateSlotFields changes non-state columns of slot id.
func (s *Store) UpdateSlotFields(ctx context.Context, id int64, set func(*SlotUpdate)) error {
	return s.TransitionSlot(ctx, id, nil, "", set)
}

// TransitionSession is TransitionPR for sessions.
func (s *Store) TransitionSession(ctx context.Context, id int64, from []string, to string, set func(*SessionUpdate)) error {
	u := &SessionUpdate{Update{table: sessionTable}}
	if set != nil {
		set(u)
	}
	return s.transition(ctx, sessionTable, id, from, to, &u.Update)
}

// UpdateSession changes non-state columns of session id.
func (s *Store) UpdateSession(ctx context.Context, id int64, set func(*SessionUpdate)) error {
	return s.TransitionSession(ctx, id, nil, "", set)
}

// TransitionRun is TransitionPR for runs.
func (s *Store) TransitionRun(ctx context.Context, id string, from []string, to string, set func(*RunUpdate)) error {
	u := &RunUpdate{Update{table: runTable}}
	if set != nil {
		set(u)
	}
	return s.transition(ctx, runTable, id, from, to, &u.Update)
}

// UpdateRun changes non-state columns of run id.
func (s *Store) UpdateRun(ctx context.Context, id string, set func(*RunUpdate)) error {
	return s.TransitionRun(ctx, id, nil, "", set)
}
