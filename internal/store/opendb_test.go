package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/sqlitetest"
)

func memoryDB(t *testing.T, image []byte) *sql.DB {
	t.Helper()
	db, err := sqlitetest.Open(image, sqlitetest.Params)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenDBMigratesAMemoryDatabaseOnOneConnection(t *testing.T) {
	t.Parallel()
	st, err := OpenDB(memoryDB(t, nil), Options{})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if v, err := st.SchemaVersion(ctx); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("SchemaVersion = %d, %v, want %d", v, err, LatestSchemaVersion())
	}
	if changed, err := st.SchemaChanged(ctx); err != nil || changed {
		t.Fatalf("SchemaChanged right after OpenDB = %v, %v", changed, err)
	}
	if n := st.DB().Stats().MaxOpenConnections; n != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1, as OpenWith keeps", n)
	}
	if _, err := st.UpsertRepo(ctx, Repo{NodeID: "R_1", Owner: "o", Name: "n", WatchOwner: "o", Mode: RepoModePerPR}); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
}

func TestOpenDBRefusesNewerSchema(t *testing.T) {
	t.Parallel()
	db := memoryDB(t, nil)
	if _, err := db.ExecContext(context.Background(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDB(db, Options{}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("OpenDB of a newer schema = %v, want a refusal that says newer", err)
	}
	if err := db.PingContext(context.Background()); err == nil {
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
	refusal := errors.New("a daemon runs the older schema")
	if _, err := OpenDB(db, Options{BeforeMigrate: func(f, t int) error { from, to = f, t; return refusal }}); !errors.Is(err, refusal) {
		t.Fatalf("OpenDB = %v, want the refusal", err)
	}
	if from != 0 || to != LatestSchemaVersion() {
		t.Errorf("BeforeMigrate(%d, %d), want (0, %d)", from, to, LatestSchemaVersion())
	}
	again, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if v := userVersion(t, again); v != 0 {
		t.Errorf("user_version = %d after a refused migration, want 0", v)
	}
}

func TestOpenDBRefusesQueryOnly(t *testing.T) {
	t.Parallel()
	if _, err := OpenDB(memoryDB(t, nil), Options{QueryOnly: true}); err == nil || !strings.Contains(err.Error(), "query-only") {
		t.Fatalf("OpenDB with QueryOnly = %v, want a refusal", err)
	}
}

// TestOpenDBRefusesOutsideATestBinary: OpenDB passes testing.Testing() as
// inTest; this test cannot be outside a test binary, so it calls openDB with
// false, as a production binary would.
func TestOpenDBRefusesOutsideATestBinary(t *testing.T) {
	t.Parallel()
	db := memoryDB(t, nil)
	if _, err := openDB(db, Options{}, false); err == nil || !strings.Contains(err.Error(), "tests") {
		t.Fatalf("openDB outside a test binary = %v, want a refusal", err)
	}
	if err := db.PingContext(context.Background()); err == nil {
		t.Error("the refused database stayed open")
	}
}
