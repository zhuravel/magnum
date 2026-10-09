// Package fsx holds the file helpers several packages share: an atomic file
// write, plain or confined to an os.Root, a size-limited read of a regular
// file, an existence check and a canonical path. It imports only the
// standard library.
package fsx

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

// WriteFileAtomic replaces path with data, mode exactly perm whatever the
// umask: a temporary file with a unique name in path's directory is written,
// synced and renamed over path, then the directory is synced, so a reader (or
// a crash) sees the old content or the new, never part of a file. A symlink
// at path is replaced, not followed. The directory must exist; the
// temporary file is removed on failure.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	return WriteFileAtomicIn(root, filepath.Base(path), data, perm)
}

// WriteFileAtomicIn is WriteFileAtomic for name inside root: a name that is
// absolute or escapes root (.., or a symlink in a parent pointing outside) is
// an error and nothing is written outside root. name's directory is resolved
// once, so the temporary file and the rename land in the same one.
func WriteFileAtomicIn(root *os.Root, name string, data []byte, perm fs.FileMode) (err error) {
	dir, base := filepath.Split(name)
	parent := root
	if dir != "" {
		if parent, err = root.OpenRoot(dir); err != nil {
			return err
		}
		defer parent.Close()
	}
	var (
		f   *os.File
		tmp string
	)
	for range 10 {
		tmp = "." + base + ".tmp-" + rand.Text()[:12]
		f, err = parent.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = parent.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm.Perm()); err == nil {
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = parent.Rename(tmp, base); err != nil {
		return err
	}
	// Best effort: the file is in place; the sync only makes the rename
	// itself survive a crash.
	if d, derr := parent.Open("."); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ReadRegular's errors, each inside an *fs.PathError naming the path.
var (
	// ErrNotRegular: the path is a symbolic link, a directory, a device, a
	// FIFO or a socket.
	ErrNotRegular = errors.New("not a regular file (no symbolic links or special files)")
	// ErrTooLarge: the file holds more than the bytes the caller allows.
	ErrTooLarge = errors.New("file too large")
)

// ReadRegular reads the regular file at path, at most limit bytes: the read
// for a file a pull request controls (a checkout's lockfiles, its schema,
// the marker its setup writes, its project config). A symbolic link at path
// is refused, not followed, so a file a checkout commits as a symlink never
// leads the daemon out of the checkout, to /dev/zero or to anyone's file;
// a directory, a device, a FIFO or a socket is refused before it is opened,
// so no read runs without end and no open waits for a FIFO's writer. Both
// are ErrNotRegular. Only path's last element is checked: its parent
// directories resolve as usual, and what they lead to is held to the same
// rules. A file over limit bytes, by its size or by what the read finds
// (it may grow), is ErrTooLarge, and none of it is returned. A missing file
// is the error os.Lstat returns, unwrapped (errors.Is fs.ErrNotExist).
func ReadRegular(path string, limit int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	notRegular := &fs.PathError{Op: "read", Path: path, Err: ErrNotRegular}
	tooLarge := &fs.PathError{Op: "read", Path: path, Err: fmt.Errorf("%w: over %d bytes", ErrTooLarge, limit)}
	switch {
	case !fi.Mode().IsRegular():
		return nil, notRegular
	case fi.Size() > limit:
		return nil, tooLarge
	}
	// A symlink or a FIFO put in its place since the Lstat is neither
	// followed (O_NOFOLLOW) nor waited on (O_NONBLOCK; a regular file's
	// reads ignore it), and SameFile refuses whatever replaced the file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, notRegular
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(fi, st) {
		return nil, notRegular
	}
	var buf bytes.Buffer
	buf.Grow(int(fi.Size()) + bytes.MinRead)
	if _, err := buf.ReadFrom(io.LimitReader(f, min(limit, math.MaxInt64-1)+1)); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > limit {
		return nil, tooLarge
	}
	return buf.Bytes(), nil
}

// Exists reports whether path names something os.Stat can see (a symlink
// counts by its target).
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Canon cleans p and resolves its symlinks when it exists, so the /var and
// /private/var spellings of one directory compare equal; "" stays "".
func Canon(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
