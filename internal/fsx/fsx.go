// Package fsx holds the file helpers several packages share: an atomic file
// write, plain or confined to an os.Root, an existence check and a canonical
// path. It imports only the standard library.
package fsx

import (
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
