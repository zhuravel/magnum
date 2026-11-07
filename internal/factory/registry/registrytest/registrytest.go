// Package registrytest gives tests a factory registry without migrating one
// for each of them: the first call in a test binary migrates a template
// database in memory, and every call starts from a copy of its bytes. Open
// and OpenWith keep that copy in memory, so the test writes no database
// file; Seed, OpenFile and OpenPair write it to a file, which registry.Open
// then finds current. Only _test.go files import it.
package registrytest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/sqlitetest"
)

var template = sync.OnceValues(migrate)

// migrate applies every migration to an in-memory database and returns its
// image (sqlitetest.Image).
func migrate() ([]byte, error) {
	db, err := sqlitetest.Open(nil, sqlitetest.Params)
	if err != nil {
		return nil, err
	}
	r, err := registry.OpenDB(db, registry.Options{})
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return sqlitetest.Image(context.Background(), db)
}

// Seed writes a migrated database to path, creating its directory (0700), for
// code under test that opens the registry itself. A file already at path is
// left as it is.
func Seed(tb testing.TB, path string) {
	tb.Helper()
	image, err := template()
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
	if _, err := f.Write(sqlitetest.FileImage(image)); err != nil {
		f.Close()
		tb.Fatalf("registrytest: write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		tb.Fatalf("registrytest: %v", err)
	}
}

// Open gives the test a migrated registry in memory, private to it; the
// registry is closed when the test ends.
func Open(tb testing.TB) *registry.Registry {
	tb.Helper()
	return OpenWith(tb, registry.Options{})
}

// OpenWith is Open with options, for a test that moves the registry's clock.
func OpenWith(tb testing.TB, opts registry.Options) *registry.Registry {
	tb.Helper()
	image, err := template()
	if err != nil {
		tb.Fatalf("registrytest: migrate the template: %v", err)
	}
	db, err := sqlitetest.Open(image, sqlitetest.Params)
	if err != nil {
		tb.Fatalf("registrytest: %v", err)
	}
	r, err := registry.OpenDB(db, opts)
	if err != nil {
		tb.Fatalf("registry.OpenDB: %v", err)
	}
	tb.Cleanup(func() { r.Close() })
	return r
}

// OpenFile seeds path (see Seed) and opens it with registry.Open; the
// registry is closed when the test ends. It is for a test whose code under
// test opens the registry by path, such as the CLI's commands.
func OpenFile(tb testing.TB, path string, opts registry.Options) *registry.Registry {
	tb.Helper()
	Seed(tb, path)
	r, err := registry.Open(path, opts)
	if err != nil {
		tb.Fatalf("registry.Open: %v", err)
	}
	tb.Cleanup(func() { r.Close() })
	return r
}

// OpenPair opens two registries on one database file, the way the daemon and
// the CLI each hold one in production; both are closed when the test ends.
func OpenPair(tb testing.TB) (first, second *registry.Registry) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "factory.db")
	Seed(tb, path)
	for _, r := range []**registry.Registry{&first, &second} {
		opened, err := registry.Open(path, registry.Options{})
		if err != nil {
			tb.Fatalf("registry.Open: %v", err)
		}
		tb.Cleanup(func() { opened.Close() })
		*r = opened
	}
	return first, second
}
