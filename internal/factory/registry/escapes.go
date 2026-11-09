package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// EscapeCheck is the one check of a published task's local review against
// magnum's first round on the task's PR that posted a review and began
// after the publish (migrations/0007_escapes.sql): magnum's run that posted
// the review (RunID, runs.id) and its Round, the head that round reviewed,
// how many P0 to P2 findings it posted, how many of the task's local
// findings were placed at a head comparable with it, and when the check
// ran. SameHeadOnly counts the files of the posted findings the check
// compared at the same head only, as git could not compare them at two
// heads (none means every comparison ran); Unrecorded the P0 to P2
// findings the round's result counts that magnum recorded no row of (above
// zero, the escapes are unknown, not none).
type EscapeCheck struct {
	TaskID                   int64
	RunID                    string
	Round                    int
	HeadSHA                  string
	Posted, Comparable       int
	SameHeadOnly, Unrecorded int
	At                       time.Time
}

// Why no local finding caught a posted one (Escape.Reason): none on its
// path at a comparable head, the nearest one more than 3 lines away, or
// the posted finding has no path or no line to place.
const (
	EscapeNoLocal  = "no_local"
	EscapeFar      = "far"
	EscapeUnplaced = "unplaced"
)

var escapeReasons = map[string]bool{EscapeNoLocal: true, EscapeFar: true, EscapeUnplaced: true}

// escapePriorities are the priorities an escape is counted at: P3 is
// optional, so a P3 the local review let pass is no escape.
var escapePriorities = map[string]bool{"P0": true, "P1": true, "P2": true}

// Escape is one P0 to P2 finding of magnum's posted round that no finding of
// the task's local review caught: magnum's finding (FindingID, findings.id),
// its path and lines, its priority, the hash of its title (the factory's
// own, as Finding.TitleHash; never the title), why (Reason), and for
// EscapeFar only the nearest local finding (NearestID, review_findings.id)
// and how many lines lie between them. At is the check's time.
type Escape struct {
	ID, TaskID                  int64
	FindingID                   int64
	Path                        string
	StartLine, EndLine          int
	Priority, TitleHash, Reason string
	NearestID                   int64
	Distance                    int
	At                          time.Time
}

// check refuses an escape of the wrong shape: an unknown reason, a
// priority below P2, a nearest finding and a distance on anything but a
// far escape, or a far one without them.
func (e Escape) check() error {
	switch far := e.Reason == EscapeFar; {
	case !escapeReasons[e.Reason]:
		return fmt.Errorf("escape of finding %d: unknown reason %q", e.FindingID, e.Reason)
	case !escapePriorities[e.Priority]:
		return fmt.Errorf("escape of finding %d: priority %q is not P0 to P2", e.FindingID, e.Priority)
	case far && (e.NearestID <= 0 || e.Distance <= 0):
		return fmt.Errorf("escape of finding %d: a far escape needs its nearest local finding and its distance", e.FindingID)
	case !far && (e.NearestID != 0 || e.Distance != 0):
		return fmt.Errorf("escape of finding %d: only a far escape has a nearest local finding", e.FindingID)
	}
	return nil
}

const escapeCheckColumns = "task_id, run_id, round, head_sha, posted, comparable, same_head_only, unrecorded, checked_at"

func scanEscapeCheck(sc scanner) (EscapeCheck, error) {
	var c EscapeCheck
	err := sc.Scan(&c.TaskID, &c.RunID, &c.Round, &c.HeadSHA, &c.Posted, &c.Comparable, &c.SameHeadOnly, &c.Unrecorded, timeCol{&c.At})
	return c, err
}

const escapeColumns = "id, task_id, finding_id, path, start_line, end_line, priority, title_hash, reason, " +
	"COALESCE(nearest_id, 0), COALESCE(distance, 0), found_at"

func scanEscape(sc scanner) (Escape, error) {
	var e Escape
	err := sc.Scan(&e.ID, &e.TaskID, &e.FindingID, &e.Path, &e.StartLine, &e.EndLine, &e.Priority, &e.TitleHash, &e.Reason,
		&e.NearestID, &e.Distance, timeCol{&e.At})
	return e, err
}

// nullID is id for a nullable column: NULL for 0.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// RecordEscapeCheck stores task c.TaskID's escape check and its escapes es
// (each of the task, at the check's time), in one transaction, once: a task
// checked before writes nothing more, whatever c and es hold, and false says
// so. The check needs magnum's run and the head; each escape is of a P0 to P2
// finding, a known reason, and a nearest finding and a distance for far only
// (anything else is refused, and nothing is written). At defaults to the
// clock; a task that does not exist is ErrNotFound.
func (r *Registry) RecordEscapeCheck(ctx context.Context, c EscapeCheck, es []Escape) (bool, error) {
	if c.RunID == "" || c.HeadSHA == "" {
		return false, fmt.Errorf("escape check of task %d: magnum's run and the head are required", c.TaskID)
	}
	for _, e := range es {
		if err := e.check(); err != nil {
			return false, fmt.Errorf("escape check of task %d: %w", c.TaskID, err)
		}
	}
	if c.At.IsZero() {
		c.At = r.now()
	}
	at := store.FormatTime(c.At)
	recorded := false
	err := r.tx(ctx, func(tx *sql.Tx) error {
		if err := taskExists(ctx, tx, c.TaskID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO escape_checks ("+escapeCheckColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (task_id) DO NOTHING",
			c.TaskID, c.RunID, c.Round, c.HeadSHA, c.Posted, c.Comparable, c.SameHeadOnly, c.Unrecorded, at)
		if err != nil {
			return mapErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // checked before: its escapes stand
		}
		for _, e := range es {
			_, err := tx.ExecContext(ctx, `INSERT INTO escapes (task_id, finding_id, path, start_line, end_line, priority, title_hash, reason,
					nearest_id, distance, found_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				c.TaskID, e.FindingID, e.Path, e.StartLine, e.EndLine, e.Priority, e.TitleHash, e.Reason, nullID(e.NearestID),
				nullID(int64(e.Distance)), at)
			if err != nil {
				return fmt.Errorf("escape of finding %d: %w", e.FindingID, mapErr(err))
			}
		}
		recorded = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("escape check of task %d: %w", c.TaskID, err)
	}
	return recorded, nil
}

// EscapeCheck is task id's escape check; ErrNotFound before it ran.
func (r *Registry) EscapeCheck(ctx context.Context, id int64) (EscapeCheck, error) {
	c, err := scanEscapeCheck(r.db.QueryRowContext(ctx, "SELECT "+escapeCheckColumns+" FROM escape_checks WHERE task_id = ?", id))
	if err != nil {
		return EscapeCheck{}, notFound(err, "escape check of task", id)
	}
	return c, nil
}

// Escapes are task id's escapes in the order they were stored (magnum's
// findings' order); none before its check, or after one that found none.
func (r *Registry) Escapes(ctx context.Context, id int64) ([]Escape, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+escapeColumns+" FROM escapes WHERE task_id = ? ORDER BY id", id)
	if err != nil {
		return nil, fmt.Errorf("escapes of task %d: %w", id, err)
	}
	es, err := collect(rows, scanEscape)
	if err != nil {
		return nil, fmt.Errorf("escapes of task %d: %w", id, err)
	}
	return es, nil
}

// RecordGitHubCI stores GitHub's CI on task taskID's PR at head as a check
// of kind ci from source github with result (a pass or a failure; cause, a
// failure's only, as Check.Cause), once per task, head and result: the same
// again writes nothing, and false says so (the look and the write are one
// transaction, which takes the registry's write lock first, so two writers
// cannot both write it). A head that failed and then passed after a rerun
// has both rows. A task that does not exist is ErrNotFound.
func (r *Registry) RecordGitHubCI(ctx context.Context, taskID int64, head, result, cause string) (bool, error) {
	switch {
	case head == "":
		return false, fmt.Errorf("record CI of task %d: a head is required", taskID)
	case result != CheckPass && result != CheckFail:
		return false, fmt.Errorf("record CI of task %d: result %q is not a pass or a failure", taskID, result)
	case !checkCauses[cause]:
		return false, fmt.Errorf("record CI of task %d: unknown cause %q", taskID, cause)
	case cause != "" && result != CheckFail:
		return false, fmt.Errorf("record CI of task %d: a pass has no cause (got %q)", taskID, cause)
	}
	recorded := false
	err := r.tx(ctx, func(tx *sql.Tx) error {
		if err := taskExists(ctx, tx, taskID); err != nil {
			return err
		}
		var one int
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM checks WHERE task_id = ? AND kind = ? AND source = 'github' AND head_sha = ? AND result = ? LIMIT 1",
			taskID, CheckCI, head, result).Scan(&one)
		switch {
		case err == nil:
			return nil // recorded before
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO checks (task_id, head_sha, kind, result, cause, source, at) VALUES (?, ?, ?, ?, ?, 'github', ?)",
			taskID, head, CheckCI, result, cause, store.FormatTime(r.now())); err != nil {
			return mapErr(err)
		}
		recorded = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("record CI of task %d: %w", taskID, err)
	}
	return recorded, nil
}
