// Package storetest gives tests a registry without migrating one for each of
// them: the first call in a test binary migrates a template database in
// memory, and every call starts from a copy of its bytes. Open keeps that
// copy in memory, so the test writes no database file; OpenFile and Seed
// write it to a file, which store.Open then finds current, for code under
// test that opens the registry by path. It also holds the settable clock the
// store's users share in their tests. Only _test.go files import it.
package storetest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/sqlitetest"
	"github.com/zhuravel/magnum/internal/store"
)

var template = sync.OnceValues(migrate)

// migrate applies every migration to an in-memory database and returns its
// image (sqlitetest.Image).
func migrate() ([]byte, error) {
	db, err := sqlitetest.Open(nil, sqlitetest.Params)
	if err != nil {
		return nil, err
	}
	st, err := store.OpenDB(db, store.Options{})
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return sqlitetest.Image(context.Background(), st.DB())
}

// placeholder is what Open leaves at the path it is given: a file that
// SQLite refuses ("file is not a database"), so code under test that opens
// the path gets an error at once instead of migrating a registry of its own
// beside the test's (Open says where that error fails no test).
const placeholder = "storetest: this registry is in memory (storetest.Open); a test whose code opens it by path uses storetest.OpenFile\n"

// Open gives the test a migrated registry in memory (Memory) and closes it
// when the test ends. It creates path's directory (0700), as store.Open
// would, and leaves a placeholder at path that SQLite refuses ("file is not
// a database"), so code that opens the path gets an error instead of
// finding or making a registry of its own there; a file already at path
// fails the test. That error fails the test only where the code under test
// passes it on: the cli's notes command (notesOpenStore), shell completion
// (completeQuery) and daemon-restart's event writer (writeRestartEvent)
// swallow it and go on without the registry, so a test can pass without
// ever reaching it. A test whose code under test opens the registry by
// path, or that checks the file (WAL, modes, a reopen, a second handle, a
// read-only open), uses OpenFile.
func Open(tb testing.TB, path string) *store.Store {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		tb.Fatalf("storetest: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		tb.Fatalf("storetest.Open: %s exists: a registry file that is shared or reopened needs storetest.OpenFile", path)
	}
	if err != nil {
		tb.Fatalf("storetest: %v", err)
	}
	if _, err := f.WriteString(placeholder); err != nil {
		f.Close()
		tb.Fatalf("storetest: write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		tb.Fatalf("storetest: %v", err)
	}
	return Memory(tb)
}

// Memory gives the test a migrated registry that lives in this process's
// memory, private to it, and closes it when the test ends. Nothing but the
// returned store reaches it.
func Memory(tb testing.TB) *store.Store {
	tb.Helper()
	image, err := template()
	if err != nil {
		tb.Fatalf("storetest: migrate the template: %v", err)
	}
	db, err := sqlitetest.Open(image, sqlitetest.Params)
	if err != nil {
		tb.Fatalf("storetest: %v", err)
	}
	st, err := store.OpenDB(db, store.Options{})
	if err != nil {
		tb.Fatalf("store.OpenDB: %v", err)
	}
	tb.Cleanup(func() { st.Close() })
	return st
}

// Seed writes a migrated database to path, creating its directory (0700),
// for code under test that opens the registry itself. A file already at path
// is left as it is.
func Seed(t testing.TB, path string) {
	t.Helper()
	image, err := template()
	if err != nil {
		t.Fatalf("storetest: migrate the template: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("storetest: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return
	}
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	if _, err := f.Write(sqlitetest.FileImage(image)); err != nil {
		f.Close()
		t.Fatalf("storetest: write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("storetest: %v", err)
	}
}

// OpenFile seeds path (see Seed) and opens it with store.Open; the store is
// closed when the test ends. It is for a test whose code under test opens
// the registry by path, or that checks the file.
func OpenFile(tb testing.TB, path string) *store.Store {
	tb.Helper()
	Seed(tb, path)
	st, err := store.Open(path)
	if err != nil {
		tb.Fatalf("store.Open: %v", err)
	}
	tb.Cleanup(func() { st.Close() })
	return st
}

// marked holds the tests Parallel already ran in parallel or Serial keeps
// serial.
var marked sync.Map

// Parallel runs t in parallel with the other tests (t.Parallel) the first
// time it is called for t, so a fixture helper can call it and a test that
// builds two fixtures does not call t.Parallel twice (which panics). A test
// that called Serial first stays serial.
func Parallel(t *testing.T) {
	t.Helper()
	if _, done := marked.LoadOrStore(t, true); !done {
		t.Cleanup(func() { marked.Delete(t) })
		t.Parallel()
	}
}

// Serial keeps t serial when its fixture helpers call Parallel: for a test
// that swaps a package variable or signals the test binary. Go runs every
// serial test before it releases the parallel ones, so none of them sees
// the swap. Call it before the helpers.
func Serial(t *testing.T) {
	t.Helper()
	if _, done := marked.LoadOrStore(t, true); !done {
		t.Cleanup(func() { marked.Delete(t) })
	}
}

// Clock is a settable clock that is safe for concurrent use. Its Now is what
// tests hand to store.Store.Clock and the other components' clocks.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a Clock that reads t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) { c.mu.Lock(); c.now = t; c.mu.Unlock() }

// Add moves the clock forward by d.
func (c *Clock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }
