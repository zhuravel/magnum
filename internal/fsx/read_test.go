package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readRegularSoon is ReadRegular that fails the test when the read does not
// return within a few seconds (a FIFO would block an open for good).
func readRegularSoon(t *testing.T, path string, limit int64) ([]byte, error) {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := ReadRegular(path, limit)
		done <- result{b, err}
	}()
	select {
	case r := <-done:
		return r.b, r.err
	case <-time.After(5 * time.Second):
		t.Fatalf("ReadRegular(%s) blocked", path)
		return nil, nil
	}
}

func TestReadRegularReadsAFileOfAtMostMaxBytes(t *testing.T) {
	dir := t.TempDir()
	for _, text := range []string{"", "x", "12345"} {
		path := filepath.Join(dir, "f")
		writeTestFile(t, path, text)
		b, err := ReadRegular(path, 5)
		if err != nil || string(b) != text {
			t.Errorf("%q: got %q, %v", text, b, err)
		}
	}
}

func TestReadRegularRefusesAFileOverMaxAndReturnsNoneOfIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Gemfile.lock")
	writeTestFile(t, path, "123456")
	b, err := ReadRegular(path, 5)
	if !errors.Is(err, ErrTooLarge) || b != nil {
		t.Fatalf("got %q, %v; want ErrTooLarge", b, err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != path {
		t.Errorf("error %v does not name %s", err, path)
	}
}

// A file a checkout commits as a symlink is refused even when it points to
// a plain file: nothing outside the checkout is read through it.
func TestReadRegularRefusesASymlinkEvenToARegularFile(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret")
	writeTestFile(t, outside, "outside the checkout")
	link := filepath.Join(t.TempDir(), "Gemfile.lock")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	b, err := ReadRegular(link, 1<<20)
	if !errors.Is(err, ErrNotRegular) || b != nil {
		t.Fatalf("got %q, %v; want ErrNotRegular", b, err)
	}
}

// /dev/zero (directly or through a committed symlink), a FIFO and a
// directory are refused at once: nothing is read without end and no open
// waits for a writer.
func TestReadRegularRefusesSpecialFilesWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	zeroLink := filepath.Join(dir, "schema.rb")
	if err := os.Symlink("/dev/zero", zeroLink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, ".worktree-db-slug")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "config.toml")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{zeroLink, "/dev/zero", fifo, sub} {
		b, err := readRegularSoon(t, path, 1<<20)
		if !errors.Is(err, ErrNotRegular) || b != nil {
			t.Errorf("%s: got %d bytes, %v; want ErrNotRegular", path, len(b), err)
		}
	}
}

// Callers switch on a missing file: its error is the one Lstat returns.
func TestReadRegularKeepsAMissingFilesError(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadRegular(filepath.Join(dir, "gone"), 10); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v, want fs.ErrNotExist", err)
	}
	file := filepath.Join(dir, "file")
	writeTestFile(t, file, "x")
	if _, err := ReadRegular(filepath.Join(file, "below"), 10); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("below a file: %v, want ENOTDIR", err)
	}
}
