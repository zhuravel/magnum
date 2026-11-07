package registry_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/sqlitetest"
)

func memoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitetest.Open(nil, sqlitetest.Params)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenDBMigratesAMemoryDatabaseOnOneConnection(t *testing.T) {
	t.Parallel()
	r, err := registry.OpenDB(memoryDB(t), registry.Options{})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer r.Close()
	if v, err := r.SchemaVersion(t.Context()); err != nil || v != registry.LatestSchemaVersion() {
		t.Fatalf("SchemaVersion = %d, %v, want %d", v, err, registry.LatestSchemaVersion())
	}
	if n := r.DBForTest().Stats().MaxOpenConnections; n != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1, as Open keeps", n)
	}
	createTask(t, r, "PR-12345")
}

func TestOpenDBRefusesADatabaseNewerThanTheBinary(t *testing.T) {
	t.Parallel()
	db := memoryDB(t)
	if _, err := db.ExecContext(t.Context(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.OpenDB(db, registry.Options{}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("OpenDB of a newer database = %v, want a refusal that says newer", err)
	}
	if err := db.PingContext(t.Context()); err == nil {
		t.Error("OpenDB left the refused database open")
	}
}

// TestOpenDBAsksBeforeMigrating: BeforeMigrate sees the database's version and
// this binary's, and its refusal fails OpenDB before any migration runs. The
// database is a file only so the test can look at it after OpenDB closed it.
func TestOpenDBAsksBeforeMigrating(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "raw.db")
	db, err := sql.Open("sqlite", path+"?"+sqlitetest.FastFile)
	if err != nil {
		t.Fatal(err)
	}
	var from, to int
	refusal := errors.New("a factory daemon runs the older schema")
	if _, err := registry.OpenDB(db, registry.Options{BeforeMigrate: func(f, t int) error { from, to = f, t; return refusal }}); !errors.Is(err, refusal) {
		t.Fatalf("OpenDB = %v, want the refusal", err)
	}
	if from != 0 || to != registry.LatestSchemaVersion() {
		t.Errorf("BeforeMigrate(%d, %d), want (0, %d)", from, to, registry.LatestSchemaVersion())
	}
	again, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var v int
	if err := again.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&v); err != nil || v != 0 {
		t.Errorf("user_version = %d, %v after a refused migration, want 0", v, err)
	}
}

// TestOpenDBRefusesOutsideATestBinary: OpenDB passes testing.Testing(); this
// test cannot be outside a test binary, so it calls the refusal's path with
// false, as a production binary would.
func TestOpenDBRefusesOutsideATestBinary(t *testing.T) {
	t.Parallel()
	db := memoryDB(t)
	if _, err := registry.OpenDBOutsideATestBinary(db, registry.Options{}); err == nil || !strings.Contains(err.Error(), "tests") {
		t.Fatalf("OpenDB outside a test binary = %v, want a refusal", err)
	}
	if err := db.PingContext(t.Context()); err == nil {
		t.Error("the refused database stayed open")
	}
}

func pragmaOf(t *testing.T, r *registry.Registry, name string) string {
	t.Helper()
	var v string
	if err := r.DBForTest().QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

// TestRegistrytestOpenIsInMemoryWithOpensSettings pins sqlitetest.Params to
// registry.Open's DSN: only the journal differs (memory has no WAL).
func TestRegistrytestOpenIsInMemoryWithOpensSettings(t *testing.T) {
	t.Parallel()
	mem := registrytest.Open(t)
	file := registrytest.OpenFile(t, filepath.Join(t.TempDir(), "state", "factory.db"), registry.Options{})
	for _, name := range []string{"foreign_keys", "busy_timeout", "temp_store", "query_only"} {
		if m, f := pragmaOf(t, mem, name), pragmaOf(t, file, name); m != f {
			t.Errorf("%s = %s in memory, %s on a file", name, m, f)
		}
	}
	if m, f := pragmaOf(t, mem, "journal_mode"), pragmaOf(t, file, "journal_mode"); m != "memory" || f != "wal" {
		t.Errorf("journal_mode = %s in memory, %s on a file; want memory, wal", m, f)
	}
	var seq int
	var name, path string
	if err := mem.DBForTest().QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&seq, &name, &path); err != nil || path != "" {
		t.Errorf("registrytest.Open's database_list file = %q, %v, want none", path, err)
	}
}

func TestRegistrytestSeedWritesARegistryOpenFindsCurrent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "factory.db")
	registrytest.Seed(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if data[18] != 2 || data[19] != 2 {
		t.Fatalf("header bytes 18, 19 = %d, %d, want 2, 2 (a WAL database, as registry.Open leaves one)", data[18], data[19])
	}
	called := false
	r, err := registry.Open(path, registry.Options{BeforeMigrate: func(int, int) error { called = true; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if v, err := r.SchemaVersion(context.Background()); err != nil || v != registry.LatestSchemaVersion() || called {
		t.Fatalf("schema version = %d, %v (migrated: %v), want %d with nothing to migrate", v, err, called, registry.LatestSchemaVersion())
	}
}
