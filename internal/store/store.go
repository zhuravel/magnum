// Package store is magnum's SQLite registry: repos, PRs, slots, assignments,
// slot databases, agent sessions, review runs, CLI requests, the audit event
// log, small key/value state and notification dedup.
//
// The daemon and the CLI open the same file (WAL mode, busy_timeout 5 s, one
// connection per process, BEGIN IMMEDIATE transactions). The schema lives in
// migrations/NNNN_*.sql and is applied in order, tracked by PRAGMA
// user_version; a database newer than this binary is refused at Open, and
// SchemaChanged tells a long-running process that another binary migrated the
// file after it opened. Prune is the retention pass for the events and
// requests tables.
//
// State changes are compare-and-set: TransitionPR, TransitionSlot,
// TransitionSession and TransitionRun issue UPDATE … WHERE id=? AND state IN
// (…) and return ErrConflict when another writer got there first. Timestamps
// are stored as fixed-width RFC3339 UTC text with a 9-digit fraction (see
// TimeFormat and FormatTime), so string comparison in SQL is chronological
// comparison.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// ErrNotFound is returned (wrapped) when a looked-up row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned (wrapped) when a compare-and-set update matched no
// row because the state moved on, or when an insert hits a uniqueness rule
// (a second open assignment, a second live session for a role, …).
var ErrConflict = errors.New("store: conflict")

// Store is the registry handle. It is safe for concurrent use.
type Store struct {
	// Clock returns the current time; nil means time.Now. Tests replace it.
	Clock func() time.Time

	db *sql.DB
	// openVersion is PRAGMA user_version as Open left it; SchemaChanged
	// compares the live value with it.
	openVersion int
}

// Open creates the parent directory (0700), opens the database at path with
// WAL, busy_timeout=5000, foreign_keys=ON and synchronous=NORMAL, and applies
// pending migrations. It refuses a database whose schema version is newer
// than this binary knows. The database, its -wal and its -shm file are made
// 0600 whatever the umask.
func Open(path string) (*Store, error) { return OpenWith(path, Options{}) }

// Options tune OpenWith.
type Options struct {
	// BeforeMigrate, when set, runs before pending migrations are applied,
	// with the file's schema version and this binary's. An error refuses the
	// migration: OpenWith fails with it and leaves the file untouched. The
	// CLI uses it so a newer build never migrates the registry under a
	// running older daemon (which exits when the schema changes under it).
	BeforeMigrate func(from, to int) error
}

// OpenWith is Open with options.
func OpenWith(path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("store: database path must not contain '?': %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	// Create the file private from the start: SQLite gives the -wal and -shm
	// files the main file's mode. An existing file is left alone (chmod below).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create %s: %w", path, err)
	}
	f.Close()
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One connection per process: SQLite has a single writer anyway, and it
	// keeps transactions and pragmas trivially consistent.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db}
	ctx := context.Background()
	if err := s.migrate(ctx, opts.BeforeMigrate); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: %s: %w", path, err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.openVersion = v
	// The -wal and -shm files hold the same data as the database and exist
	// while this connection is open; a missing one is not an error.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("store: chmod %s: %w", p, err)
		}
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for ad hoc read-only queries (status
// views, tests). Writes should go through the typed methods.
func (s *Store) DB() *sql.DB { return s.db }

// SchemaVersion returns PRAGMA user_version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read user_version: %w", err)
	}
	return v, nil
}

// SchemaChanged reports whether PRAGMA user_version differs from the value
// this Store saw at Open, i.e. whether another binary migrated the database
// underneath it. A long-running process (the daemon) calls it once per tick
// and restarts when it returns true. It is one cheap read, no write lock.
func (s *Store) SchemaChanged(ctx context.Context) (bool, error) {
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		return false, err
	}
	return v != s.openVersion, nil
}

// LatestSchemaVersion is the highest migration version embedded in this binary.
func LatestSchemaVersion() int { return len(migrations) }

func (s *Store) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

var migrations = mustLoadMigrations()

// mustLoadMigrations reads migrations/NNNN_name.sql; versions must run 1..N
// without gaps. A malformed embed is a build defect, hence the panic.
func mustLoadMigrations() []migration {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		panic(fmt.Sprintf("store: read embedded migrations: %v", err))
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if err != nil || v < 1 {
			panic(fmt.Sprintf("store: migration %s: name must start with a positive number", e.Name()))
		}
		body, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			panic(fmt.Sprintf("store: read migration %s: %v", e.Name(), err))
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			panic(fmt.Sprintf("store: migration %s: expected version %d", m.name, i+1))
		}
	}
	return out
}

// migrate applies every migration above the current user_version, after
// before (Options.BeforeMigrate) agrees. A schema
// that is already current is detected with a plain read, so opening it never
// takes the write lock. Otherwise the migrations run inside one immediate
// transaction that re-reads user_version first, so a concurrent opener waits
// and then sees the result instead of applying them twice.
func (s *Store) migrate(ctx context.Context, before func(from, to int) error) error {
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if err := checkSchemaVersion(v); err != nil {
		return err
	}
	if v == len(migrations) {
		return nil
	}
	if before != nil {
		if err := before(v, len(migrations)); err != nil {
			return err
		}
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
			return fmt.Errorf("read user_version: %w", err)
		}
		if err := checkSchemaVersion(v); err != nil {
			return err
		}
		for _, m := range migrations[v:] {
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %s: %w", m.name, err)
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
				return fmt.Errorf("migration %s: set user_version: %w", m.name, err)
			}
		}
		return nil
	})
}

// checkSchemaVersion refuses a database newer than this binary.
func checkSchemaVersion(v int) error {
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); upgrade magnum", v, len(migrations))
	}
	return nil
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// tx runs fn in a transaction (BEGIN IMMEDIATE via the DSN) and commits it
// when fn returns nil. Inside fn only tx may be used: the pool has a single
// connection, so touching s.db there would deadlock. The deferred Rollback is
// a no-op after Commit; it releases the only connection when fn returns an
// error or panics.
func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// SQLite extended result codes for uniqueness violations.
const (
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

// mapErr turns uniqueness violations into ErrConflict and keeps the rest.
func mapErr(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		if c := se.Code(); c == sqliteConstraintUnique || c == sqliteConstraintPrimaryKey {
			return fmt.Errorf("%w: %w", ErrConflict, err)
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

// TimeFormat is RFC3339 with a fixed nine-digit fraction. Unlike
// time.RFC3339Nano it never trims zeros, so lexical order equals time order.
const TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime renders t in UTC with TimeFormat. Every timestamp written to the
// database must go through it.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime parses any RFC3339 timestamp (with or without a fraction) and
// returns it in UTC.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parse time %q: %w", s, err)
	}
	return t.UTC(), nil
}

// DayKey is the local calendar day of t ("2006-01-02"), the value stored in
// prs.rounds_day next to rounds_today.
func DayKey(t time.Time) string { return t.Local().Format("2006-01-02") }

// Ptr returns a pointer to v (for nullable fields).
func Ptr[T any](v T) *T { return &v }

// Deref returns *p, or the zero value when p is nil.
func Deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
