// Package storetest gives tests a registry without migrating one for each of
// them: the first call in a test binary migrates a template database, and
// every call writes a copy of its bytes, which store.Open then finds current.
// It also holds the settable clock the store's users share in their tests.
// Only _test.go files import it.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

var template = sync.OnceValues(migrate)

// migrate opens a fresh database in a temporary directory, which applies
// every migration, and returns its bytes once SQLite has checkpointed the
// WAL into the main file and closed it.
func migrate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "magnum-storetest-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "magnum.db")
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := st.DB().ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		st.Close()
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	if err := st.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Seed writes a migrated database to path, creating its directory (0700),
// for code under test that opens the registry itself. A file already at path
// is left as it is.
func Seed(t testing.TB, path string) {
	t.Helper()
	data, err := template()
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
	if _, err := f.Write(data); err != nil {
		f.Close()
		t.Fatalf("storetest: write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("storetest: %v", err)
	}
}

// Open seeds path (see Seed) and opens it with store.Open; the store is
// closed when the test ends.
func Open(t testing.TB, path string) *store.Store {
	t.Helper()
	Seed(t, path)
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
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
