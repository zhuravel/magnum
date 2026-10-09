package slots

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// The files below belong to the pull request: its commit (Gemfile.lock,
// db/schema.rb) or the code its setup runs (tmp/.worktree-db-slug). A file
// it commits as a symlink is refused, even one to a plain file outside the
// checkout; /dev/zero and a FIFO are refused before anything is read or an
// open waits; a file past its cap is refused without being read.

// refusedKinds are the files a checkout may leave that magnum does not read.
var refusedKinds = []string{"symlink outside", "symlink to /dev/zero", "fifo", "over the cap"}

// plantRefused makes path a file of kind: a symlink to a plain file outside
// the checkout holding text, a symlink to /dev/zero, a FIFO, or a sparse
// file of limit+1 bytes.
func plantRefused(t *testing.T, kind, path, text string, limit int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var err error
	switch kind {
	case "symlink outside":
		outside := filepath.Join(t.TempDir(), "outside")
		writeFile(t, outside, text)
		err = os.Symlink(outside, path)
	case "symlink to /dev/zero":
		err = os.Symlink("/dev/zero", path)
	case "fifo":
		err = syscall.Mkfifo(path, 0o600)
	case "over the cap":
		sparseFile(t, path, limit+1)
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// sparseFile makes path a file of size bytes without writing them.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	writeFile(t, path, "")
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

// refused reports whether err is fsx.ReadRegular's refusal.
func refused(err error) bool {
	return errors.Is(err, fsx.ErrNotRegular) || errors.Is(err, fsx.ErrTooLarge)
}

// soon fails the test when fn does not return within a few seconds.
func soon(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked", what)
	}
}

func TestLockHashRefusesALockfileThePRCommitsAsASymlinkOrPastItsCap(t *testing.T) {
	t.Parallel()
	for _, kind := range refusedKinds {
		for _, name := range LockFiles {
			t.Run(kind+" "+name, func(t *testing.T) {
				dir := t.TempDir()
				plantRefused(t, kind, filepath.Join(dir, name), "GEM\n", lockFileMax)
				var err error
				soon(t, "lockHash", func() { _, err = lockHash(dir) })
				if !refused(err) {
					t.Fatalf("lockHash = %v, want a refusal", err)
				}
			})
		}
	}
	// A lockfile at its cap is still hashed.
	dir := t.TempDir()
	sparseFile(t, filepath.Join(dir, "pnpm-lock.yaml"), lockFileMax)
	if _, err := lockHash(dir); err != nil {
		t.Fatalf("lockHash at the cap: %v", err)
	}
}

// A checkout whose Gemfile.lock is a symlink to /dev/zero fails its deps
// step at once: post_checkout does not run against it and lock_sha keeps
// the hash it had.
func TestDepsRunsNothingForALockfileItRefuses(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	before := store.Deref(sl.LockSHA)
	lock := filepath.Join(sl.Path, "Gemfile.lock")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	plantRefused(t, "symlink to /dev/zero", lock, "", 0)
	h.clearScripts()
	var err error
	soon(t, "deps", func() { err = h.m.deps(h.ctx, sl, h.pool) })
	if !errors.Is(err, fsx.ErrNotRegular) {
		t.Fatalf("deps = %v, want fsx.ErrNotRegular", err)
	}
	if n := len(h.scriptCalls(h.pool.PostCheckout[0])); n != 0 {
		t.Errorf("post_checkout ran %d times", n)
	}
	if got := store.Deref(h.slot(sl.Name).LockSHA); got != before {
		t.Errorf("lock_sha = %q, want %q", got, before)
	}
}

func TestSchemaVersionRefusesASchemaThePRCommitsAsASymlinkOrPastItsCap(t *testing.T) {
	t.Parallel()
	for _, kind := range refusedKinds {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			plantRefused(t, kind, filepath.Join(dir, SchemaFile), schemaRB("2026_01_01_000000"), schemaFileMax)
			var (
				v   string
				ok  bool
				err error
			)
			soon(t, "schemaVersion", func() { v, ok, err = schemaVersion(dir) })
			if !refused(err) || ok || v != "" {
				t.Fatalf("schemaVersion = %q, %v, %v; want a refusal", v, ok, err)
			}
		})
	}
	if v, ok, err := schemaVersion(t.TempDir()); v != "" || ok || err != nil {
		t.Errorf("no schema: %q, %v, %v", v, ok, err)
	}
}

func TestTheSlugMarkerIsReadOnlyFromARegularFileOfItsCheckout(t *testing.T) {
	t.Parallel()
	for _, kind := range refusedKinds {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			plantRefused(t, kind, filepath.Join(dir, MarkerFile), "review1\n", markerMax)
			var err, verr error
			var slug, perPR string
			soon(t, "the marker's readers", func() {
				slug, err = ReadMarker(dir)
				verr = verifyMarker(dir, "review1")
				perPR = perPRDBSlug(store.Slot{Path: dir}, perPRPlan{slug: PRSlug(5), setup: []Hook{{Type: HookSetup, Command: "bin/setup"}}})
			})
			if slug != "" || !refused(err) {
				t.Errorf("ReadMarker = %q, %v; want a refusal", slug, err)
			}
			if !errors.Is(verr, ErrVerify) || !refused(verr) {
				t.Errorf("verifyMarker = %v, want ErrVerify for a refused marker", verr)
			}
			if want := DBSlug(PRSlug(5)); perPR != want {
				t.Errorf("perPRDBSlug = %q, want the default %q", perPR, want)
			}
		})
	}
	dir := t.TempDir()
	if _, err := ReadMarker(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing marker: %v, want fs.ErrNotExist", err)
	}
	writeFile(t, filepath.Join(dir, MarkerFile), " review1\n")
	if slug, err := ReadMarker(dir); slug != "review1" || err != nil {
		t.Errorf("ReadMarker = %q, %v", slug, err)
	}
	if err := verifyMarker(dir, "review1"); err != nil {
		t.Errorf("verifyMarker: %v", err)
	}
}
