package slots

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/fsx"
)

// copyTreeIntoCheckout copies the tree of dir, a copy_files entry that names
// a directory of the main clone (generated fixtures git ignores), to the
// same path inside checkout. Both ends go through an os.Root: a path that
// leaves the main clone (dir reached through a link out of it) or the
// checkout (a tracked link in the destination's parents) is an error, and
// nothing is read or written outside them. Only regular files are copied,
// each with its permission bits: a link in the tree is neither followed nor
// copied, and a .git is skipped. A file is written only when the checkout
// lacks it or holds an older copy (other than a regular file, of another
// size, or older than the source), so a checkout does not rewrite hundreds
// of files each round; each write replaces the file at once, without the
// disk flushes a durable write makes (the next copy puts back a lost one).
// A missing dir is skipped, as a missing file is; the clone itself (".") is
// no directory to copy.
func copyTreeIntoCheckout(mainClone, checkout, dir string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("slots: copy %s into %s: %w", dir, checkout, err)
		}
	}()
	if !filepath.IsLocal(dir) || dir == "." {
		return errors.New("not a directory inside the main clone")
	}
	src, err := os.OpenRoot(mainClone)
	if err != nil {
		return err
	}
	defer src.Close()
	switch fi, err := src.Stat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case !fi.IsDir():
		return errors.New("not a directory")
	}
	dst, err := os.OpenRoot(checkout)
	if err != nil {
		return err
	}
	defer dst.Close()
	tree := src.FS()
	return fs.WalkDir(tree, filepath.ToSlash(dir), func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.Name() == ".git":
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		case !d.Type().IsRegular():
			return nil // a directory is walked; a link or a device is not copied
		}
		return copyTreeFile(tree, dst, p, d)
	})
}

// copyTreeFile copies the regular file p of tree to the same path in dst
// unless dst holds a current copy (copyTreeIntoCheckout).
func copyTreeFile(tree fs.FS, dst *os.Root, p string, d fs.DirEntry) error {
	fi, err := d.Info()
	if err != nil {
		return err
	}
	name := filepath.FromSlash(p)
	if cur, err := dst.Lstat(name); err == nil && cur.Mode().IsRegular() && cur.Size() == fi.Size() && !cur.ModTime().Before(fi.ModTime()) {
		return nil
	}
	b, err := fs.ReadFile(tree, p)
	if err != nil {
		return err
	}
	if err := dst.MkdirAll(filepath.FromSlash(path.Dir(p)), 0o755); err != nil {
		return err
	}
	return fsx.ReplaceFileIn(dst, name, b, fi.Mode().Perm())
}
