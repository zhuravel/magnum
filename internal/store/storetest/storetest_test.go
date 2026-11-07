package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

func TestOpenGivesEachTestItsOwnCurrentRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := Open(t, filepath.Join(t.TempDir(), "state", "magnum.db"))
	b := Open(t, filepath.Join(t.TempDir(), "state", "magnum.db"))
	for _, st := range []*store.Store{a, b} {
		v, err := st.SchemaVersion(ctx)
		if err != nil || v != store.LatestSchemaVersion() {
			t.Fatalf("schema version = %d, %v, want %d", v, err, store.LatestSchemaVersion())
		}
	}
	if err := a.SetKV(ctx, "probe", "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := b.GetKV(ctx, "probe"); err != nil || ok {
		t.Fatalf("a write to one copy shows in another: ok=%v, %v", ok, err)
	}
}

func TestOpenKeepsTheRegistryOffTheDisk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "magnum.db")
	st := Open(t, path)
	if err := st.SetKV(ctx, "probe", "a"); err != nil {
		t.Fatal(err)
	}
	if v, _, err := st.GetKV(ctx, "probe"); err != nil || v != "a" {
		t.Fatalf("probe = %q, %v, want a", v, err)
	}
	var seq int
	var name, file string
	if err := st.DB().QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &name, &file); err != nil || file != "" {
		t.Fatalf("database_list file = %q, %v, want none", file, err)
	}
	for _, p := range []string{path + "-wal", path + "-shm", path + "-journal"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s: %v, want no such file", filepath.Base(p), err)
		}
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != placeholder {
		t.Fatalf("the path holds %q, %v, want the placeholder", data, err)
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, %v, want 0700", fi.Mode().Perm(), err)
	}
	if other, err := store.Open(path); err == nil || !strings.Contains(err.Error(), "not a database") {
		if other != nil {
			other.Close()
		}
		t.Fatalf("store.Open of the path = %v, want \"not a database\"", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != placeholder {
		t.Fatalf("after store.Open the path holds %q, %v, want the placeholder", data, err)
	}
}

// fatalTB records Fatalf and ends the goroutine, as testing.T's Fatalf does,
// for a test of a helper that must fail its test.
type fatalTB struct {
	testing.TB
	fatal string
}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func TestOpenFailsWhenThePathHoldsAFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "magnum.db")
	if err := os.WriteFile(path, []byte("a registry another fixture wrote"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &fatalTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Open(rec, path)
	}()
	<-done
	if !strings.Contains(rec.fatal, path+" exists") || !strings.Contains(rec.fatal, "storetest.OpenFile") {
		t.Fatalf("Fatalf = %q, want it to name the path and storetest.OpenFile", rec.fatal)
	}
	if data, _ := os.ReadFile(path); string(data) != "a registry another fixture wrote" {
		t.Fatalf("the file now holds %q", data)
	}
}

// TestOpenAndOpenFileHaveTheSameConnectionSettings pins sqlitetest.Params to
// store.Open's DSN: only the journal differs (memory has no WAL).
func TestOpenAndOpenFileHaveTheSameConnectionSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := Open(t, filepath.Join(t.TempDir(), "magnum.db"))
	file := OpenFile(t, filepath.Join(t.TempDir(), "magnum.db"))
	read := func(db *sql.DB, pragma string) string {
		t.Helper()
		var v string
		if err := db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&v); err != nil {
			t.Fatalf("PRAGMA %s: %v", pragma, err)
		}
		return v
	}
	for _, pragma := range []string{"foreign_keys", "busy_timeout", "temp_store", "query_only"} {
		if m, f := read(mem.DB(), pragma), read(file.DB(), pragma); m != f {
			t.Errorf("%s = %s in memory, %s on a file", pragma, m, f)
		}
	}
	if m, f := read(mem.DB(), "journal_mode"), read(file.DB(), "journal_mode"); m != "memory" || f != "wal" {
		t.Errorf("journal_mode = %s in memory, %s on a file; want memory, wal", m, f)
	}
	for _, st := range []*store.Store{mem, file} {
		if n := st.DB().Stats().MaxOpenConnections; n != 1 {
			t.Errorf("MaxOpenConnections = %d, want 1", n)
		}
	}
}

func TestSeedWritesARegistryStoreOpenFindsCurrent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "magnum.db")
	Seed(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if data[18] != 2 || data[19] != 2 {
		t.Fatalf("header bytes 18, 19 = %d, %d, want 2, 2 (a WAL database, as store.Open leaves one)", data[18], data[19])
	}
	called := false
	st, err := store.OpenWith(path, store.Options{BeforeMigrate: func(int, int) error { called = true; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, err := st.SchemaVersion(context.Background()); err != nil || v != store.LatestSchemaVersion() || called {
		t.Fatalf("schema version = %d, %v (migrated: %v), want %d with nothing to migrate", v, err, called, store.LatestSchemaVersion())
	}
}

func TestSeedLeavesAnExistingRegistryAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "magnum.db")
	st := OpenFile(t, path)
	if err := st.SetKV(ctx, "probe", "kept"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	Seed(t, path)
	again := OpenFile(t, path)
	if v, _, err := again.GetKV(ctx, "probe"); err != nil || v != "kept" {
		t.Fatalf("probe = %q, %v after a second Seed, want kept", v, err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v, want 0600", fi.Mode().Perm(), err)
	}
}

func TestParallelRunsATestInParallelOnceAndNeverAfterSerial(t *testing.T) {
	var order []string
	t.Cleanup(func() {
		if want := []string{"serial", "after serial", "after parallel", "parallel"}; !slices.Equal(order, want) {
			t.Errorf("order = %q, want %q", order, want)
		}
	})
	t.Run("serial", func(t *testing.T) {
		Serial(t)
		Parallel(t)
		order = append(order, "serial")
	})
	order = append(order, "after serial")
	t.Run("parallel", func(t *testing.T) {
		Parallel(t)
		Parallel(t) // a second fixture in the same test
		order = append(order, "parallel")
	})
	order = append(order, "after parallel")
}

func TestClockMovesOnlyWhenTold(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	c := NewClock(t0)
	if !c.Now().Equal(t0) {
		t.Fatalf("Now = %v, want %v", c.Now(), t0)
	}
	c.Add(time.Minute)
	if want := t0.Add(time.Minute); !c.Now().Equal(want) {
		t.Fatalf("after Add: %v, want %v", c.Now(), want)
	}
	c.Set(t0)
	if !c.Now().Equal(t0) {
		t.Fatalf("after Set: %v, want %v", c.Now(), t0)
	}
}
