// Package registry is the factory's own SQLite registry: tasks, the stage
// attempts that run them, questions and the operator's decisions, the prompt
// ledger, external effects, the event log and the CLI's requests. It also
// keeps the evidence records of the factory's design (section 11.1) from the
// first task on, because evidence that was not written at the time cannot be
// read later: the spans of a task's states, the facts of each role of an
// attempt, the operator's inputs, ratings, the rounds and findings of the
// local review, artifacts, the PR link, checks and suspensions.
//
// It is a file of its own with its own migration list (migrations/NNNN_*.sql,
// tracked by PRAGMA user_version and applied by the store package's runner),
// so a factory migration never touches magnum's review registry. It follows
// that registry's conventions: WAL with busy_timeout 5 s and foreign keys on,
// one connection per process, BEGIN IMMEDIATE transactions, timestamps as
// fixed-width RFC3339 UTC text, and compare-and-set state changes that return
// ErrConflict or ErrNotFound.
//
// Callers scrub text before they put it into an event or an error column
// (scrub.Text: secrets, emails and hosts) or into a request's result, the
// answer you read (scrub.Secrets: its hosts stay): the registry stores what
// it is given.
package registry

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/zhuravel/magnum/internal/store"
)

// ErrNotFound is returned (wrapped) when a looked-up row, or a row that
// another row refers to, does not exist.
var ErrNotFound = errors.New("factory registry: not found")

// ErrConflict is returned (wrapped) when a compare-and-set update matched no
// row because the state moved on, or when a write hits a uniqueness rule.
var ErrConflict = errors.New("factory registry: conflict")

// ErrStaleRevision is returned (wrapped) when a question revision is not the
// newest one the task has: answers to older questions are never taken.
var ErrStaleRevision = errors.New("factory registry: stale question revision")

// Registry is the factory's registry handle. It is safe for concurrent use.
type Registry struct {
	db    *sql.DB
	clock func() time.Time
}

// Options tune Open.
type Options struct {
	// Clock returns the current time; nil means time.Now. Tests replace it.
	Clock func() time.Time
	// BeforeMigrate, when set, runs before pending migrations are applied,
	// with the file's schema version and the one this binary migrates to; an
	// error from it aborts Open and leaves the file untouched. The CLI sets it
	// to refuse while a factory daemon runs on the older schema, and so does
	// the daemon's builder, which writes its own pidfile only later.
	BeforeMigrate func(from, to int) error
}

// Open creates the parent directory (0700), opens the database at path with
// WAL, busy_timeout=5000, foreign_keys=ON, synchronous=NORMAL and
// temp_store=MEMORY, and applies pending migrations after opts.BeforeMigrate
// agrees. It refuses a database whose schema version is newer than this
// binary knows. The database, its -wal and its -shm file are made 0600
// whatever the umask.
func Open(path string, opts Options) (*Registry, error) {
	if path == "" {
		return nil, errors.New("factory registry: empty database path")
	}
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("factory registry: database path must not contain '?': %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("factory registry: create dir: %w", err)
	}
	// Create the file private from the start: SQLite gives the -wal and -shm
	// files the main file's mode. An existing file is left alone (chmod below).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("factory registry: create %s: %w", path, err)
	}
	f.Close()
	// temp_store(MEMORY), as store.Open: sorts never spill to $TMPDIR.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)" +
		"&_pragma=temp_store(MEMORY)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("factory registry: open %s: %w", path, err)
	}
	// One connection per process: SQLite has a single writer anyway, and it
	// keeps transactions and pragmas trivially consistent.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if _, _, err := store.ApplyMigrations(context.Background(), db, migrations, opts.BeforeMigrate); err != nil {
		db.Close()
		return nil, fmt.Errorf("factory registry: %s: %w", path, err)
	}
	// The -wal and -shm files hold the same data as the database and exist
	// while this connection is open; a missing one is not an error.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("factory registry: chmod %s: %w", p, err)
		}
	}
	return &Registry{db: db, clock: opts.Clock}, nil
}

// OpenDB is Open for a database the caller opened: the in-memory registries
// of tests (registrytest, sqlitetest.Open). Production opens files with
// Open, and OpenDB refuses to run outside a test binary (testing.Testing).
// It pins db to the one connection Open keeps, applies pending migrations
// after opts.BeforeMigrate agrees and refuses a schema newer than this
// binary, as Open does. It closes db when it fails.
func OpenDB(db *sql.DB, opts Options) (*Registry, error) { return openDB(db, opts, testing.Testing()) }

// openDB is OpenDB; inTest says whether this is a test binary, so a test can
// see the refusal a production binary gets.
func openDB(db *sql.DB, opts Options, inTest bool) (*Registry, error) {
	if !inTest {
		db.Close()
		return nil, errors.New("factory registry: OpenDB is for tests; open the registry file with Open")
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if _, _, err := store.ApplyMigrations(context.Background(), db, migrations, opts.BeforeMigrate); err != nil {
		db.Close()
		return nil, fmt.Errorf("factory registry: %w", err)
	}
	return &Registry{db: db, clock: opts.Clock}, nil
}

// Close closes the database.
func (r *Registry) Close() error { return r.db.Close() }

// SchemaVersion returns PRAGMA user_version.
func (r *Registry) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := r.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("factory registry: read user_version: %w", err)
	}
	return v, nil
}

// LatestSchemaVersion is the highest migration version embedded in this binary.
func LatestSchemaVersion() int { return len(migrations) }

func (r *Registry) now() time.Time {
	if r.clock != nil {
		return r.clock().UTC()
	}
	return time.Now().UTC()
}

//go:embed migrations/*.sql
var migrationFS embed.FS

var migrations = mustLoadMigrations()

// mustLoadMigrations loads the migrations embedded in this binary. A malformed
// embed is a build defect, hence the panic.
func mustLoadMigrations() []store.Migration {
	out, err := store.LoadMigrations(migrationFS, "migrations")
	if err != nil {
		panic(fmt.Sprintf("factory registry: embedded migrations: %v", err))
	}
	return out
}

// tx runs fn in a transaction (BEGIN IMMEDIATE via the DSN) and commits it
// when fn returns nil. Inside fn only tx may be used: the pool has a single
// connection, so touching r.db there would deadlock. The deferred Rollback is
// a no-op after Commit; it releases the only connection when fn returns an
// error or panics.
func (r *Registry) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SQLite extended result codes for the constraints the registry maps.
const (
	sqliteConstraintForeignKey = 787
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

// mapErr turns a uniqueness violation into ErrConflict and a foreign-key
// violation (a row that names a task or an attempt that does not exist) into
// ErrNotFound, and keeps the rest.
func mapErr(err error) error {
	if se, ok := errors.AsType[*sqlite.Error](err); ok {
		switch se.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return fmt.Errorf("%w: %w", ErrConflict, err)
		case sqliteConstraintForeignKey:
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		}
	}
	return err
}

// notFound converts sql.ErrNoRows into a wrapped ErrNotFound.
func notFound(err error, what string, key any) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s %v: %w", what, key, ErrNotFound)
	}
	return fmt.Errorf("%s %v: %w", what, key, err)
}

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// collect reads every row of rows with scan and closes rows.
func collect[T any](rows *sql.Rows, scan func(scanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// timeArg is the column value for t: NULL for the zero time, else the
// fixed-width UTC text.
func timeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return store.FormatTime(t)
}

// timeCol scans a timestamp column (NULL is the zero time) into p.
type timeCol struct{ p *time.Time }

// Scan implements sql.Scanner.
func (c timeCol) Scan(v any) error {
	var s string
	switch x := v.(type) {
	case nil:
		*c.p = time.Time{}
		return nil
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return fmt.Errorf("factory registry: timestamp column holds a %T", v)
	}
	if s == "" {
		*c.p = time.Time{}
		return nil
	}
	t, err := store.ParseTime(s)
	if err != nil {
		return err
	}
	*c.p = t
	return nil
}

// update collects column assignments for one row. Only the columns of the
// table's settable list can be assigned (state moves through the Transition
// methods' `to`, and the registry keeps the timestamps); the first invalid
// call is remembered and returned by the Transition method that applies it.
type update struct {
	settable map[string]bool
	sets     []string
	args     []any
	touched  map[string]bool
	err      error
}

// Set assigns v to column col. v is nil (NULL), a string, an int, an int64, a
// bool or a time.Time (the zero time stores NULL).
func (u *update) Set(col string, v any) {
	if !u.check(col) {
		return
	}
	dv, err := dbValue(v)
	if err != nil {
		u.fail(fmt.Errorf("column %q: %w", col, err))
		return
	}
	u.sets = append(u.sets, col+" = ?")
	u.args = append(u.args, dv)
}

// Inc adds n to the integer column col.
func (u *update) Inc(col string, n int) {
	if !u.check(col) {
		return
	}
	u.sets = append(u.sets, col+" = "+col+" + ?")
	u.args = append(u.args, n)
}

// raw adds an assignment the registry itself maintains (updated_at, ended_at).
func (u *update) raw(assign string, args ...any) {
	u.sets = append(u.sets, assign)
	u.args = append(u.args, args...)
}

func (u *update) check(col string) bool {
	if u.err != nil {
		return false
	}
	if !u.settable[col] {
		u.fail(fmt.Errorf("column %q cannot be set", col))
		return false
	}
	if u.touched[col] {
		u.fail(fmt.Errorf("column %q set twice", col))
		return false
	}
	if u.touched == nil {
		u.touched = map[string]bool{}
	}
	u.touched[col] = true
	return true
}

func (u *update) fail(err error) {
	if u.err == nil {
		u.err = err
	}
}

// dbValue converts the values Update.Set accepts to a driver value.
func dbValue(v any) (any, error) {
	switch x := v.(type) {
	case nil, string, int, int64:
		return x, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case time.Time:
		return timeArg(x), nil
	default:
		return nil, fmt.Errorf("unsupported value type %T", v)
	}
}

// moveHooks run inside the transaction of transition: before just ahead of
// the UPDATE (the row may not exist), after when the UPDATE matched a row.
// Either may be nil.
type moveHooks struct {
	before func(tx *sql.Tx) error
	after  func(tx *sql.Tx) error
}

// transition is the shared body of the Transition methods: UPDATE table SET
// the update's assignments (and state = to, unless to is empty) WHERE the key
// matches and, when from is not nil, the state is one of from. When no row
// matched it tells a missing row (ErrNotFound) from a moved one (ErrConflict).
func (r *Registry) transition(ctx context.Context, table, keyCol, what string, key any, validStates map[string]bool, from []string, to string, u *update, hooks moveHooks) error {
	if u.err != nil {
		return fmt.Errorf("update %s %v: %w", what, key, u.err)
	}
	if to != "" && !validStates[to] {
		return fmt.Errorf("transition %s %v: unknown state %q", what, key, to)
	}
	sets, args := u.sets, u.args
	if to == "" && len(sets) == 0 {
		return fmt.Errorf("update %s %v: nothing to set", what, key)
	}
	if to != "" {
		sets = append([]string{"state = ?"}, sets...)
		args = append([]any{to}, args...)
	}
	q := "UPDATE " + table + " SET " + strings.Join(sets, ", ") + " WHERE " + keyCol + " = ?"
	args = append(args, key)
	if from != nil {
		q += " AND state IN (" + placeholders(len(from)) + ")"
		for _, s := range from {
			args = append(args, s)
		}
	}
	return r.tx(ctx, func(tx *sql.Tx) error {
		if hooks.before != nil {
			if err := hooks.before(tx); err != nil {
				return fmt.Errorf("transition %s %v: %w", what, key, err)
			}
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("transition %s %v %v -> %s: %w", what, key, from, to, mapErr(err))
		}
		if n, _ := res.RowsAffected(); n > 0 {
			if hooks.after != nil {
				if err := hooks.after(tx); err != nil {
					return fmt.Errorf("transition %s %v: %w", what, key, err)
				}
			}
			return nil
		}
		var state string
		err = tx.QueryRowContext(ctx, "SELECT state FROM "+table+" WHERE "+keyCol+" = ?", key).Scan(&state)
		if err != nil {
			return notFound(err, what, key)
		}
		return fmt.Errorf("transition %s %v -> %s: %w: state is %s, want one of %v", what, key, to, ErrConflict, state, from)
	})
}

// placeholders returns n comma-separated question marks.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// boolInt is the column value of a bool.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// taskExists returns a wrapped ErrNotFound unless task id exists, for a write
// that has to read the task's rows before it can insert.
func taskExists(ctx context.Context, q querier, id int64) error {
	var one int
	if err := q.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE id = ?", id).Scan(&one); err != nil {
		return notFound(err, "task", id)
	}
	return nil
}
