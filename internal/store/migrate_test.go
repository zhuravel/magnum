package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// openRawDB opens a bare database the way OpenWith does (one connection,
// BEGIN IMMEDIATE), without running any migration.
func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "raw.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	return n == 1
}

var threeMigrations = []Migration{
	{Version: 1, Name: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, note TEXT NOT NULL);"},
	{Version: 2, Name: "0002_b.sql", SQL: "CREATE TABLE b (id INTEGER PRIMARY KEY);"},
	{Version: 3, Name: "0003_c.sql", SQL: "CREATE TABLE c (id INTEGER PRIMARY KEY);"},
}

func TestLoadMigrationsRefusesAGap(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"migrations/0001_a.sql": {Data: []byte("CREATE TABLE a (id INTEGER);")},
		"migrations/0003_c.sql": {Data: []byte("CREATE TABLE c (id INTEGER);")},
	}
	got, err := LoadMigrations(fsys, "migrations")
	if err == nil {
		t.Fatalf("LoadMigrations = %+v, nil; want an error for the missing version 2", got)
	}
	if !strings.Contains(err.Error(), "0003_c.sql") || !strings.Contains(err.Error(), "expected version 2") {
		t.Fatalf("error = %q, want it to name version 2 and the file that sits in its place", err)
	}
}

func TestApplyMigrationsRunsOnlyNewOnes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawDB(t)
	if from, to, err := ApplyMigrations(ctx, db, threeMigrations[:1], nil); err != nil || from != 0 || to != 1 {
		t.Fatalf("first ApplyMigrations = %d, %d, %v; want 0, 1, nil", from, to, err)
	}
	if _, err := db.Exec("INSERT INTO a (id, note) VALUES (1, 'kept')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Migration 1 would fail if it ran again (the table exists), so a pass proves it was skipped.
	from, to, err := ApplyMigrations(ctx, db, threeMigrations, nil)
	if err != nil || from != 1 || to != 3 {
		t.Fatalf("ApplyMigrations at version 1 = %d, %d, %v; want 1, 3, nil", from, to, err)
	}
	if v := userVersion(t, db); v != 3 {
		t.Fatalf("user_version = %d, want 3", v)
	}
	for _, name := range []string{"b", "c"} {
		if !tableExists(t, db, name) {
			t.Errorf("table %s missing after the run", name)
		}
	}
	var note string
	if err := db.QueryRow("SELECT note FROM a WHERE id = 1").Scan(&note); err != nil || note != "kept" {
		t.Fatalf("row of migration 1's table = %q, %v; want it untouched", note, err)
	}

	// A current schema is read only: before is not asked and nothing runs.
	asked := false
	from, to, err = ApplyMigrations(ctx, db, threeMigrations, func(int, int) error { asked = true; return nil })
	if err != nil || from != 3 || to != 3 || asked {
		t.Fatalf("ApplyMigrations at version 3 = %d, %d, %v (before asked: %v); want 3, 3, nil, not asked", from, to, err, asked)
	}
}

func TestApplyMigrationsAsksBeforeMigrating(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawDB(t)
	if _, _, err := ApplyMigrations(ctx, db, threeMigrations[:1], nil); err != nil {
		t.Fatalf("first ApplyMigrations: %v", err)
	}
	refusal := errors.New("a daemon runs the older schema")
	var gotFrom, gotTo int
	_, _, err := ApplyMigrations(ctx, db, threeMigrations, func(from, to int) error {
		gotFrom, gotTo = from, to
		return refusal
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("ApplyMigrations error = %v, want the refusal of before", err)
	}
	if gotFrom != 1 || gotTo != 3 {
		t.Errorf("before asked with %d, %d; want 1, 3", gotFrom, gotTo)
	}
	if v := userVersion(t, db); v != 1 {
		t.Errorf("user_version = %d after a refused migration, want 1", v)
	}
	for _, name := range []string{"b", "c"} {
		if tableExists(t, db, name) {
			t.Errorf("table %s exists after a refused migration", name)
		}
	}
}
