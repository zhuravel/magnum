package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CreateSlot inserts a slot row (created_at/updated_at = now) and returns it.
// A duplicate name or path is ErrConflict.
func (s *Store) CreateSlot(ctx context.Context, sl Slot) (Slot, error) {
	if sl.Name == "" || sl.Path == "" || sl.Kind == "" || sl.State == "" || sl.RepoFullName == "" {
		return Slot{}, fmt.Errorf("create slot %q: name, repo_full_name, kind, path and state are required", sl.Name)
	}
	now := FormatTime(s.now())
	res, err := s.db.ExecContext(ctx, `
INSERT INTO slots (name, repo_id, repo_full_name, kind, path, main_clone, placeholder_branch, db_slug, state,
  pr_id, pinned, dirty_schema, checked_out_sha, hold_reason, lock_sha, last_used_at, last_error, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sl.Name, sl.RepoID, sl.RepoFullName, sl.Kind, sl.Path, sl.MainClone, sl.PlaceholderBranch, sl.DBSlug,
		sl.State, sl.PRID, boolInt(sl.Pinned), boolInt(sl.DirtySchema), sl.CheckedOutSHA, sl.HoldReason,
		sl.LockSHA, mustDB(sl.LastUsedAt), sl.LastError, now, now)
	if err != nil {
		return Slot{}, fmt.Errorf("create slot %s: %w", sl.Name, mapErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Slot{}, fmt.Errorf("create slot %s: %w", sl.Name, err)
	}
	return s.SlotByID(ctx, id)
}

// SlotByID looks up a slot by id.
func (s *Store) SlotByID(ctx context.Context, id int64) (Slot, error) {
	sl, err := scanSlot(s.db.QueryRowContext(ctx, "SELECT "+cols("", slotColumns)+" FROM slots WHERE id = ?", id))
	if err != nil {
		return Slot{}, notFound(err, "slot", id)
	}
	return sl, nil
}

// SlotByName looks up a slot by name ("review3").
func (s *Store) SlotByName(ctx context.Context, name string) (Slot, error) {
	sl, err := scanSlot(s.db.QueryRowContext(ctx, "SELECT "+cols("", slotColumns)+" FROM slots WHERE name = ?", name))
	if err != nil {
		return Slot{}, notFound(err, "slot", name)
	}
	return sl, nil
}

// SlotByPR returns the PR's current slot: the slot row (pool or per-PR, any
// state but removed) whose pr_id is prID, or ErrNotFound. The unique index
// slots_pr allows at most one row per PR, so there is never a choice to make.
func (s *Store) SlotByPR(ctx context.Context, prID int64) (Slot, error) {
	sl, err := scanSlot(s.db.QueryRowContext(ctx, "SELECT "+cols("", slotColumns)+
		" FROM slots WHERE pr_id = ? AND state <> ?", prID, SlotRemoved))
	if err != nil {
		return Slot{}, notFound(err, "slot of pr", prID)
	}
	return sl, nil
}

// SlotFilter selects slots for ListSlots. Zero values match everything.
type SlotFilter struct {
	RepoFullName string // case-insensitive
	Kind         string
	States       []string
}

// ListSlots returns slots matching f ordered by id.
func (s *Store) ListSlots(ctx context.Context, f SlotFilter) ([]Slot, error) {
	var where []string
	var args []any
	if f.RepoFullName != "" {
		where = append(where, "repo_full_name = ? COLLATE NOCASE")
		args = append(args, f.RepoFullName)
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	if len(f.States) > 0 {
		where = append(where, "state IN ("+placeholders(len(f.States))+")")
		args = append(args, anys(f.States)...)
	}
	q := "SELECT " + cols("", slotColumns) + " FROM slots"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY id", args...)
	if err != nil {
		return nil, fmt.Errorf("list slots: %w", err)
	}
	return collect(rows, scanSlot)
}

// ClaimSlot atomically hands a free slot to a PR: the PR moves from a
// ClaimableStates state to claiming, the slot from free (unpinned, no
// hold_reason) to claimed with pr_id and last_used_at set, and an open
// assignment is inserted (path, db_slug from the slot; head_sha from the PR;
// dbNames recorded as db_names_json). Any lost race is ErrConflict and
// leaves every row untouched.
func (s *Store) ClaimSlot(ctx context.Context, prID, slotID int64, dbNames ...string) (Assignment, error) {
	return s.claimSlot(ctx, prID, slotID, true, dbNames)
}

// AssignSlot is ClaimSlot without the PR transition: the slot moves from
// free (unpinned, no hold_reason) to claimed for prID and an open assignment
// is inserted, whatever state the PR is in (magnum open restores a parked
// PR's checkout for a human). Lost races are ErrConflict.
func (s *Store) AssignSlot(ctx context.Context, prID, slotID int64, dbNames ...string) (Assignment, error) {
	return s.claimSlot(ctx, prID, slotID, false, dbNames)
}

func (s *Store) claimSlot(ctx context.Context, prID, slotID int64, movePR bool, dbNames []string) (Assignment, error) {
	var a Assignment
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if movePR {
			pu := &PRUpdate{Update{table: prTable}}
			if err := s.apply(ctx, tx, prTable, prID, ClaimableStates, PRClaiming, &pu.Update); err != nil {
				return fmt.Errorf("pr: %w", err)
			}
		}
		now := FormatTime(s.now())
		res, err := tx.ExecContext(ctx, `
UPDATE slots SET state = ?, pr_id = ?, last_used_at = ?, updated_at = ?
WHERE id = ? AND state = ? AND pinned = 0 AND hold_reason IS NULL`,
			SlotClaimed, prID, now, now, slotID, SlotFree)
		if err != nil {
			return fmt.Errorf("slot: %w", mapErr(err))
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var state string
			err := tx.QueryRowContext(ctx, "SELECT state FROM slots WHERE id = ?", slotID).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("slot: %w", ErrNotFound)
			}
			if err != nil {
				return err
			}
			return fmt.Errorf("slot: %w: not a free, unpinned, unheld slot (state %s)", ErrConflict, state)
		}
		id, err := insertAssignment(ctx, tx, `
INSERT INTO assignments (pr_id, slot_id, path, db_slug, db_names_json, head_sha, started_at)
SELECT p.id, s.id, s.path, s.db_slug, ?, p.head_sha, ? FROM prs p, slots s WHERE p.id = ? AND s.id = ?`,
			mustDB(nonNil(dbNames)), now, prID, slotID)
		if err != nil {
			return err
		}
		a, err = scanAssignment(tx.QueryRowContext(ctx, "SELECT "+cols("", assignmentColumns)+" FROM assignments WHERE id = ?", id))
		return err
	})
	if err != nil {
		return Assignment{}, fmt.Errorf("claim slot %d for pr %d: %w", slotID, prID, err)
	}
	return a, nil
}

// ReleaseSlot atomically moves slot slotID from one of from to state to,
// clears its pr_id and closes its open assignment (if any) with reason.
func (s *Store) ReleaseSlot(ctx context.Context, slotID int64, from []string, to, reason string) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		u := &SlotUpdate{Update{table: slotTable}}
		u.Set("pr_id", nil)
		if err := s.apply(ctx, tx, slotTable, slotID, from, to, &u.Update); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE assignments SET ended_at = ?, end_reason = ? WHERE slot_id = ? AND ended_at IS NULL",
			FormatTime(s.now()), reason, slotID)
		return err
	})
	if err != nil {
		return fmt.Errorf("release slot %d %v -> %s: %w", slotID, from, to, err)
	}
	return nil
}

// OpenAssignment inserts an open assignment (for flows other than ClaimSlot,
// e.g. per-PR worktrees). StartedAt defaults to now. A second open assignment
// for the same slot or PR is ErrConflict.
func (s *Store) OpenAssignment(ctx context.Context, a Assignment) (Assignment, error) {
	started := a.StartedAt
	if started.IsZero() {
		started = s.now()
	}
	id, err := insertAssignment(ctx, s.db, `
INSERT INTO assignments (pr_id, slot_id, path, db_slug, db_names_json, head_sha, started_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, a.PRID, a.SlotID, a.Path, a.DBSlug, mustDB(nonNil(a.DBNames)), a.HeadSHA, FormatTime(started))
	if err != nil {
		return Assignment{}, fmt.Errorf("open assignment pr %d slot %d: %w", a.PRID, a.SlotID, err)
	}
	return s.assignmentWhere(ctx, "id = ?", id)
}

func insertAssignment(ctx context.Context, q querier, query string, args ...any) (int64, error) {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("assignment: %w", mapErr(err))
	}
	return res.LastInsertId()
}

// CloseAssignment ends open assignment id. Closing a closed or missing
// assignment is ErrConflict.
func (s *Store) CloseAssignment(ctx context.Context, id int64, reason string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE assignments SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL",
		FormatTime(s.now()), reason, id)
	if err != nil {
		return fmt.Errorf("close assignment %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("close assignment %d: %w: not open", id, ErrConflict)
	}
	return nil
}

// OpenAssignmentBySlot returns the slot's open assignment.
func (s *Store) OpenAssignmentBySlot(ctx context.Context, slotID int64) (Assignment, error) {
	return s.assignmentWhere(ctx, "slot_id = ? AND ended_at IS NULL", slotID)
}

// OpenAssignmentByPR returns the PR's open assignment.
func (s *Store) OpenAssignmentByPR(ctx context.Context, prID int64) (Assignment, error) {
	return s.assignmentWhere(ctx, "pr_id = ? AND ended_at IS NULL", prID)
}

// UpdateAssignmentHead records the commit an open assignment's checkout now
// holds (a later round checked out a newer head in the same slot). An
// unknown id is ErrNotFound; an ended assignment is ErrConflict.
func (s *Store) UpdateAssignmentHead(ctx context.Context, assignmentID int64, sha string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE assignments SET head_sha = ? WHERE id = ? AND ended_at IS NULL", sha, assignmentID)
	if err != nil {
		return fmt.Errorf("assignment %d head: %w", assignmentID, mapErr(err))
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var ended sql.NullString
	err = s.db.QueryRowContext(ctx, "SELECT ended_at FROM assignments WHERE id = ?", assignmentID).Scan(&ended)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("assignment %d: %w", assignmentID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("assignment %d head: %w", assignmentID, err)
	}
	return fmt.Errorf("assignment %d has ended: %w", assignmentID, ErrConflict)
}

// AssignmentsByPR returns the PR's assignment history, oldest first.
func (s *Store) AssignmentsByPR(ctx context.Context, prID int64) ([]Assignment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("", assignmentColumns)+" FROM assignments WHERE pr_id = ? ORDER BY id", prID)
	if err != nil {
		return nil, fmt.Errorf("assignments of pr %d: %w", prID, err)
	}
	return collect(rows, scanAssignment)
}

func (s *Store) assignmentWhere(ctx context.Context, where string, arg any) (Assignment, error) {
	a, err := scanAssignment(s.db.QueryRowContext(ctx, "SELECT "+cols("", assignmentColumns)+" FROM assignments WHERE "+where, arg))
	if err != nil {
		return Assignment{}, notFound(err, "assignment", arg)
	}
	return a, nil
}

func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// UpsertSlotDatabase records that MySQL holds d.DBName: first_seen_at is set
// once, last_seen_at = now, a nil SlotID or SizeMB keeps the stored value,
// and a database seen again after a drop is no longer marked dropped.
func (s *Store) UpsertSlotDatabase(ctx context.Context, d SlotDatabase) (SlotDatabase, error) {
	if d.DBName == "" || d.Slug == "" {
		return SlotDatabase{}, errors.New("upsert slot database: db_name and slug are required")
	}
	now := FormatTime(s.now())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO slot_databases (slot_id, db_name, slug, size_mb, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(db_name) DO UPDATE SET
  slot_id = COALESCE(excluded.slot_id, slot_databases.slot_id), slug = excluded.slug,
  size_mb = COALESCE(excluded.size_mb, slot_databases.size_mb), last_seen_at = excluded.last_seen_at,
  dropped_at = NULL, dropped_by = NULL`,
		d.SlotID, d.DBName, d.Slug, d.SizeMB, now, now)
	if err != nil {
		return SlotDatabase{}, fmt.Errorf("upsert slot database %s: %w", d.DBName, err)
	}
	out, err := scanSlotDatabase(s.db.QueryRowContext(ctx, "SELECT "+cols("", slotDatabaseColumns)+
		" FROM slot_databases WHERE db_name = ?", d.DBName))
	if err != nil {
		return SlotDatabase{}, notFound(err, "slot database", d.DBName)
	}
	return out, nil
}

// ListSlotDatabases returns known databases ordered by name, dropped ones
// only when includeDropped.
func (s *Store) ListSlotDatabases(ctx context.Context, includeDropped bool) ([]SlotDatabase, error) {
	q := "SELECT " + cols("", slotDatabaseColumns) + " FROM slot_databases"
	if !includeDropped {
		q += " WHERE dropped_at IS NULL"
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY db_name")
	if err != nil {
		return nil, fmt.Errorf("list slot databases: %w", err)
	}
	return collect(rows, scanSlotDatabase)
}

// MarkSlotDatabaseDropped records that dbName was dropped (by = who: a
// cleanup plan id, "cleanup", "reconcile", …).
func (s *Store) MarkSlotDatabaseDropped(ctx context.Context, dbName, by string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE slot_databases SET dropped_at = ?, dropped_by = ? WHERE db_name = ?",
		FormatTime(s.now()), by, dbName)
	if err != nil {
		return fmt.Errorf("mark %s dropped: %w", dbName, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("mark %s dropped: %w", dbName, ErrNotFound)
	}
	return nil
}

// FreeSlots returns the repository's claimable pool slots (state free, not
// pinned, no hold_reason): the slot that last held preferPRID first (0 = no
// preference), then least recently used.
func (s *Store) FreeSlots(ctx context.Context, repoFullName string, preferPRID int64) ([]Slot, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("s", slotColumns)+` FROM slots s
WHERE s.state = ? AND s.kind = ? AND s.pinned = 0 AND s.hold_reason IS NULL AND s.repo_full_name = ? COLLATE NOCASE
ORDER BY s.id = COALESCE((SELECT a.slot_id FROM assignments a WHERE a.pr_id = ? ORDER BY a.id DESC LIMIT 1), -1) DESC,
  COALESCE(s.last_used_at, '') ASC, s.id ASC`, SlotFree, SlotKindPool, repoFullName, preferPRID)
	if err != nil {
		return nil, fmt.Errorf("free slots %s: %w", repoFullName, err)
	}
	return collect(rows, scanSlot)
}

// EvictableSlots returns the repository's held pool slots that may be taken
// back, least recently used first: not pinned, no hold_reason, unused for at
// least minWarm, PR not pinned and no human activity within minWarm, no
// active run, and no live session that is working, blocked or was prompted
// (or started) within minWarm.
func (s *Store) EvictableSlots(ctx context.Context, repoFullName string, now time.Time, minWarm time.Duration) ([]Slot, error) {
	cut := FormatTime(now.Add(-minWarm))
	args := []any{SlotHeld, SlotKindPool, repoFullName, cut, cut}
	args = append(args, anys(activeRunStates)...)
	args = append(args, anys(liveSessionStates)...)
	args = append(args, cut)
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("s", slotColumns)+` FROM slots s
LEFT JOIN prs p ON p.id = s.pr_id
WHERE s.state = ? AND s.kind = ? AND s.pinned = 0 AND s.hold_reason IS NULL AND s.repo_full_name = ? COLLATE NOCASE
  AND COALESCE(s.last_used_at, s.updated_at) <= ?
  AND (p.id IS NULL OR (p.pinned = 0 AND (p.human_active_at IS NULL OR p.human_active_at <= ?)))
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.pr_id = s.pr_id AND r.state IN (`+placeholders(len(activeRunStates))+`))
  AND NOT EXISTS (SELECT 1 FROM sessions x WHERE x.pr_id = s.pr_id AND x.state IN (`+placeholders(len(liveSessionStates))+`)
    AND (x.agent_status IN ('working', 'blocked') OR COALESCE(x.last_prompt_at, x.started_at) > ?))
ORDER BY COALESCE(s.last_used_at, s.updated_at) ASC, s.id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("evictable slots %s: %w", repoFullName, err)
	}
	return collect(rows, scanSlot)
}
