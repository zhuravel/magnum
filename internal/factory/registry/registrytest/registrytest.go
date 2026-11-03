// Package registrytest gives tests a factory registry without migrating one
// for each of them: the first call in a test binary migrates a template
// database, and every call writes a copy of its bytes, which registry.Open
// then finds current. Only _test.go files import it.
package registrytest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

var template = sync.OnceValues(migrate)

// migrate opens a fresh database in a temporary directory, which applies
// every migration, and returns its bytes once SQLite has checkpointed the
// WAL into the main file and closed it.
func migrate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "magnum-registrytest-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "factory.db")
	r, err := registry.Open(path, registry.Options{})
	if err != nil {
		return nil, err
	}
	// Closing the last connection checkpoints the WAL into the main file and
	// removes it; a WAL left behind would mean the copy lacks the schema.
	if err := r.Close(); err != nil {
		return nil, err
	}
	if st, err := os.Stat(path + "-wal"); err == nil && st.Size() > 0 {
		return nil, fmt.Errorf("the template database kept a %d-byte WAL after Close", st.Size())
	}
	return os.ReadFile(path)
}

// Seed writes a migrated database to path, creating its directory (0700), for
// code under test that opens the registry itself. A file already at path is
// left as it is.
func Seed(tb testing.TB, path string) {
	tb.Helper()
	data, err := template()
	if err != nil {
		tb.Fatalf("registrytest: migrate the template: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		tb.Fatalf("registrytest: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return
	}
	if err != nil {
		tb.Fatalf("registrytest: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		tb.Fatalf("registrytest: write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		tb.Fatalf("registrytest: %v", err)
	}
}

// Open seeds a database in the test's temporary directory and opens it; the
// registry is closed when the test ends.
func Open(tb testing.TB) *registry.Registry {
	tb.Helper()
	return OpenWith(tb, registry.Options{})
}

// OpenWith is Open with options, for a test that moves the registry's clock.
func OpenWith(tb testing.TB, opts registry.Options) *registry.Registry {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "factory.db")
	Seed(tb, path)
	r, err := registry.Open(path, opts)
	if err != nil {
		tb.Fatalf("registry.Open: %v", err)
	}
	tb.Cleanup(func() { r.Close() })
	return r
}
