package relay

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite" // registers the "sqlite" database/sql driver; its Error gives the error classes
)

// schemaVersion is the journal's PRAGMA user_version.
const schemaVersion = 1

// schema is the journal: one row per delivery, holding the Event fields and never a body, and meta's one row
// with the journal's epoch, the highest sequence number retention deleted, and the counters the health check
// reads (the events kept, kept up by insert and prune, and when the newest arrived).
const schema = `
CREATE TABLE meta (
	id               INTEGER PRIMARY KEY CHECK (id = 1),
	epoch            TEXT    NOT NULL,
	pruned_through   INTEGER NOT NULL DEFAULT 0,
	event_count      INTEGER NOT NULL DEFAULT 0,
	last_received_at INTEGER
);
CREATE TABLE events (
	seq              INTEGER PRIMARY KEY AUTOINCREMENT,
	delivery_id      TEXT    NOT NULL UNIQUE,
	event            TEXT    NOT NULL,
	action           TEXT    NOT NULL,
	installation_id  INTEGER NOT NULL,
	repository_id    INTEGER NOT NULL,
	repository       TEXT    NOT NULL,
	pr_numbers       TEXT    NOT NULL,
	head_sha         TEXT    NOT NULL,
	base_ref         TEXT    NOT NULL,
	check_name       TEXT,
	check_conclusion TEXT,
	received_at      INTEGER NOT NULL
);
CREATE INDEX events_received_at ON events (received_at);
`

// eventColumns are the columns scanEvent reads, in its order.
const eventColumns = `seq, delivery_id, event, action, installation_id, repository_id, repository, pr_numbers,
	head_sha, base_ref, check_name, check_conclusion, received_at`

// errGap is a cursor the journal cannot continue: its events were pruned, or it is ahead of the journal.
var errGap = errors.New("gap")

type journal struct {
	db    *sql.DB
	epoch string
}

// bounds are the journal's cursors at one moment: pruned is the highest sequence retention deleted (the
// oldest cursor), high the highest ever assigned.
type bounds struct {
	pruned, high int64
}

func openJournal(ctx context.Context, path string) (*journal, error) {
	if path == "" {
		return nil, errors.New("relay: a journal path is required")
	}
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("relay: journal path must not contain '?' or '#': %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("relay: create the journal directory: %w", err)
	}
	// Create the file private from the start: SQLite gives the -wal and -shm files the main file's mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("relay: create the journal: %w", err)
	}
	f.Close()
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=temp_store(MEMORY)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("relay: open the journal: %w", err)
	}
	// A few connections: WAL readers (the listings, the health check) never wait on the writer, and the
	// writes are single statements or IMMEDIATE transactions, which take turns under busy_timeout.
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(0)
	j := &journal{db: db}
	if err := j.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("relay: %s: %w", path, err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("relay: chmod %s: %w", p, err)
		}
	}
	return j, nil
}

// migrate creates the schema and the epoch of a new journal, and refuses one of a newer relay.
func (j *journal) migrate(ctx context.Context) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // after a Commit it does nothing
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	switch {
	case version > schemaVersion:
		return fmt.Errorf("journal schema %d is newer than this relay's %d", version, schemaVersion)
	case version == 0:
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta (id, epoch) VALUES (1, ?)`, hex.EncodeToString(b)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
			return err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT epoch FROM meta WHERE id = 1`).Scan(&j.epoch); err != nil {
		return err
	}
	return tx.Commit()
}

func (j *journal) close() error { return j.db.Close() }

// insert stores ev under its delivery id and returns its sequence number, or dup for a delivery stored
// already (which stores nothing).
func (j *journal) insert(ctx context.Context, ev Event) (seq int64, dup bool, err error) {
	prs, err := json.Marshal(ev.PRNumbers)
	if err != nil {
		return 0, false, err
	}
	var name, conclusion sql.NullString
	if ev.Check != nil {
		name = sql.NullString{String: ev.Check.Name, Valid: true}
		conclusion = sql.NullString{String: ev.Check.Conclusion, Valid: true}
	}
	tx, err := j.db.BeginTx(ctx, nil) // IMMEDIATE: the row and the counters, in one write
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }() // after a Commit it does nothing
	res, err := tx.ExecContext(ctx, `INSERT INTO events (delivery_id, event, action, installation_id, repository_id,
		repository, pr_numbers, head_sha, base_ref, check_name, check_conclusion, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (delivery_id) DO NOTHING`,
		ev.DeliveryID, ev.Event, ev.Action, ev.InstallationID, ev.RepositoryID, ev.Repository, string(prs),
		ev.HeadSHA, ev.BaseRef, name, conclusion, ev.ReceivedAt.Unix())
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		return 0, true, nil
	}
	if seq, err = res.LastInsertId(); err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET event_count = event_count + 1,
		last_received_at = MAX(COALESCE(last_received_at, 0), ?) WHERE id = 1`, ev.ReceivedAt.Unix()); err != nil {
		return 0, false, err
	}
	return seq, false, tx.Commit()
}

// list returns up to limit events after the cursor's sequence (after < 0: from the oldest kept event) and
// the journal's bounds, all from one snapshot. A cursor below the oldest or above the high water is errGap,
// returned with the bounds.
func (j *journal) list(ctx context.Context, after int64, limit int) ([]Event, bounds, error) {
	tx, err := j.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, bounds{}, err
	}
	defer func() { _ = tx.Rollback() }() // after a Commit it does nothing
	b, err := readBounds(ctx, tx)
	if err != nil {
		return nil, bounds{}, err
	}
	if after < 0 {
		after = b.pruned
	}
	if after < b.pruned || after > b.high {
		return nil, b, errGap
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+eventColumns+` FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, b, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, b, err
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, b, err
	}
	return events, b, tx.Commit()
}

// readBounds reads the oldest cursor and the high water inside tx.
func readBounds(ctx context.Context, tx *sql.Tx) (bounds, error) {
	var b bounds
	err := tx.QueryRowContext(ctx, `SELECT pruned_through,
		COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'events'), 0) FROM meta WHERE id = 1`).Scan(&b.pruned, &b.high)
	return b, err
}

// bounds reads the oldest cursor and the high water.
func (j *journal) bounds(ctx context.Context) (bounds, error) {
	tx, err := j.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return bounds{}, err
	}
	defer func() { _ = tx.Rollback() }() // after a Commit it does nothing
	b, err := readBounds(ctx, tx)
	if err != nil {
		return bounds{}, err
	}
	return b, tx.Commit()
}

func scanEvent(rows *sql.Rows) (Event, error) {
	var (
		ev               Event
		prs              string
		name, conclusion sql.NullString
		received         int64
	)
	if err := rows.Scan(&ev.Seq, &ev.DeliveryID, &ev.Event, &ev.Action, &ev.InstallationID, &ev.RepositoryID,
		&ev.Repository, &prs, &ev.HeadSHA, &ev.BaseRef, &name, &conclusion, &received); err != nil {
		return Event{}, err
	}
	if err := json.Unmarshal([]byte(prs), &ev.PRNumbers); err != nil {
		return Event{}, err
	}
	if ev.PRNumbers == nil {
		ev.PRNumbers = []int{}
	}
	if name.Valid {
		ev.Check = &Check{Name: name.String, Conclusion: conclusion.String}
	}
	ev.ReceivedAt = time.Unix(received, 0).UTC()
	return ev, nil
}

// prune deletes the events received before cutoff. It deletes a prefix of the journal (every event up to
// the newest one received before cutoff), so the oldest cursor (pruned_through) only moves forward and
// names every deleted sequence number.
func (j *journal) prune(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }() // after a Commit it does nothing
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET pruned_through = MAX(pruned_through,
		COALESCE((SELECT MAX(seq) FROM events WHERE received_at < ?), 0)) WHERE id = 1`, cutoff.Unix()); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE seq <= (SELECT pruned_through FROM meta WHERE id = 1)`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// Count again here, once an hour, rather than subtract: the counter then never drifts.
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET event_count = (SELECT COUNT(*) FROM events) WHERE id = 1`); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// stats is what the health check tells: the events kept and when the newest arrived, from meta's counters
// (one row: no scan of the events, however many the journal keeps).
func (j *journal) stats(ctx context.Context) (count int64, last *time.Time, err error) {
	var newest sql.NullInt64
	if err := j.db.QueryRowContext(ctx, `SELECT event_count, last_received_at FROM meta WHERE id = 1`).Scan(&count, &newest); err != nil {
		return 0, nil, err
	}
	if newest.Valid {
		t := time.Unix(newest.Int64, 0).UTC()
		last = &t
	}
	return count, last, nil
}

// errorClass names an error for the logs without its text, which may quote what it failed on.
func errorClass(err error) string {
	var se *sqlite.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, sql.ErrConnDone), errors.Is(err, sql.ErrTxDone):
		return "closed"
	case errors.As(err, &se):
		return fmt.Sprintf("sqlite %d", se.Code())
	}
	if err.Error() == "sql: database is closed" {
		return "closed"
	}
	return "other"
}
