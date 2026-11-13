package slots

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

// The files magnum writes into a slot's worktree besides the local mise
// file it renders (which its marker tells apart: withoutRenderedMiseLocal)
// are magnum's too, never a person's: the copy_files of its [[repo]] or
// [[pool]], the main clone's own files (secrets among them), and whatever
// the setup wrote. A removal saves none of them (saveChanges), but a file
// below a copied folder that is no longer the main clone's file there
// (sortOwn); the worktree keeps a record of them in the kv store
// (ownFilesKey): the copy_files each render_mise copies, and the untracked
// files the first setup over a worktree its provisioning made wrote (there
// after it, not before it: recordingSetup). A worktree made before the
// record existed has the copy_files of its next checkout alone.

// ownFilesKey is the kv key of the record of the files magnum wrote into
// slot id: a JSON list of paths relative to the worktree, a folder's with
// a trailing "/".
func ownFilesKey(id int64) string { return fmt.Sprintf("slot.%d.own_files", id) }

// ownFiles is slot sl's record of the files magnum wrote into it (nil: no
// record, or one that cannot be read, which the save then treats as a
// person's files).
func (m *Manager) ownFiles(ctx context.Context, sl store.Slot) []string {
	if sl.ID == 0 {
		return nil
	}
	v, ok, err := m.d.Store.GetKV(ctx, ownFilesKey(sl.ID))
	if err != nil || !ok {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(v), &out) != nil {
		return nil
	}
	return out
}

// recordOwnFiles adds paths to slot sl's record of the files magnum wrote
// into it (ownFiles), or with replace makes them the whole record, never
// either spelling of the local mise file, whose marker decides. Best
// effort: a record that fails is logged, and a removal then saves those
// files as a person's.
func (m *Manager) recordOwnFiles(ctx context.Context, sl store.Slot, paths []string, replace bool) {
	if sl.ID == 0 || m.d.DryRun {
		return
	}
	var all []string
	if !replace {
		all = m.ownFiles(ctx, sl)
	}
	for _, p := range paths {
		if base := strings.TrimSuffix(p, "/"); base != MiseLocal && base != MiseLocalDotted && p != "" {
			all = append(all, p)
		}
	}
	slices.Sort(all)
	all = slices.Compact(all)
	b, err := json.Marshal(all)
	if err == nil {
		err = m.d.Store.SetKV(ctx, ownFilesKey(sl.ID), string(b))
	}
	if err != nil {
		m.logErr(ctx, err, "slots: record the files magnum wrote into %s (a removal saves them as a person's): %v", sl.Name, err)
	}
}

// copiedFiles are the copy_files entries of files that the main clone
// holds, as recordOwnFiles names them: a file by its path, a folder with a
// trailing "/" (the files below it that the main clone holds: withoutOwn),
// never the local mise file.
func copiedFiles(mainClone string, files []string) []string {
	var out []string
	for _, f := range files {
		c := filepath.ToSlash(filepath.Clean(f))
		fi, err := os.Stat(filepath.Join(mainClone, c))
		switch {
		case err != nil:
		case fi.IsDir():
			out = append(out, c+"/")
		default:
			out = append(out, c)
		}
	}
	return out
}

// treeAddedKey is the kv key that says the worktree_add step of slot id's
// provisioning made its worktree, rather than kept one git still listed
// from an earlier attempt (noteTreeAdded), and that no setup ran over it
// since (takeTreeAdded).
func treeAddedKey(id int64) string { return fmt.Sprintf("slot.%d.tree_added", id) }

// noteTreeAdded records, as the worktree_add step of slot sl's
// provisioning ends, whether it made the worktree (added) or kept one
// listed from an earlier attempt, for the first setup over it
// (recordingSetup). A record that fails fails the step, whose retry finds
// the worktree listed and keeps it.
func (m *Manager) noteTreeAdded(ctx context.Context, sl store.Slot, added bool) error {
	if sl.ID == 0 || m.d.DryRun {
		return nil
	}
	var err error
	if added {
		err = m.d.Store.SetKV(ctx, treeAddedKey(sl.ID), "1")
	} else {
		err = m.d.Store.DeleteKV(ctx, treeAddedKey(sl.ID))
	}
	if err != nil {
		return fmt.Errorf("slots: record whether the worktree of %s is new: %w", sl.Name, err)
	}
	return nil
}

// takeTreeAdded reports whether the worktree_add step of slot sl's
// provisioning made its worktree (noteTreeAdded), and deletes that record,
// so only the first setup over the worktree finds it: a setup run again
// (a failed one resumed or retried) runs over a worktree a person may be
// at work in. No record is a worktree kept. A record that cannot be read
// or deleted is an error, logged, which fails the setup step before its
// setup runs: run, the setup would leave the record standing, and a later
// run of it would take the record as the first; the step's retry takes it
// with no setup run between.
func (m *Manager) takeTreeAdded(ctx context.Context, sl store.Slot) (bool, error) {
	_, ok, err := m.d.Store.GetKV(ctx, treeAddedKey(sl.ID))
	if err != nil {
		m.logErr(ctx, err, "slots: read the record that the provisioning of %s made its worktree (its setup runs once it reads): %v",
			sl.Name, err)
		return false, fmt.Errorf("slots: read the record that the provisioning of %s made its worktree: %w", sl.Name, err)
	}
	if !ok {
		return false, nil
	}
	if err := m.d.Store.DeleteKV(ctx, treeAddedKey(sl.ID)); err != nil {
		m.logErr(ctx, err, "slots: take the record that the provisioning of %s made its worktree (its setup runs once it takes it): %v",
			sl.Name, err)
		return false, fmt.Errorf("slots: take the record that the provisioning of %s made its worktree: %w", sl.Name, err)
	}
	return true, nil
}

// recordingSetup runs setup, the setup step of slot sl, then makes what it
// wrote, with copied (copiedFiles of the copy_files the step's render
// copied), the slot's record of the files magnum wrote into it: the
// untracked files there after it that were not before it, at the first
// setup over a worktree the provisioning's worktree_add made, which takes
// that step's record before it runs (takeTreeAdded). Setup also runs over
// a worktree kept from an earlier attempt (a per-PR row whose setup
// failed, revived; a pool slot repaired over its listed worktree) and runs
// again over one this provisioning made (a failed setup resumed or
// retried), where a person may be at work: a file new after the setup may
// be theirs, written while it ran, so the record is copied alone, and a
// removal saves what the setup wrote there as a person's. A file the
// setup rewrote over one that was there stays theirs in both. A record a
// removed row of the same name left goes. Best effort, as recordOwnFiles:
// a listing that fails records copied alone and is logged, and a removal
// then saves what the setup wrote as a person's. A record of the
// worktree_add step that cannot be taken fails the step before setup
// runs (takeTreeAdded).
func (m *Manager) recordingSetup(ctx context.Context, sl store.Slot, copied []string, setup func() error) error {
	if sl.ID == 0 || m.d.DryRun {
		return setup()
	}
	added, err := m.takeTreeAdded(ctx, sl)
	if err != nil {
		return err
	}
	var before []string
	listed := false
	if added {
		before, listed = m.untrackedForRecord(ctx, sl, "before")
	}
	if err := setup(); err != nil {
		return err
	}
	var wrote []string
	if listed {
		if after, ok := m.untrackedForRecord(ctx, sl, "after"); ok {
			had := map[string]bool{}
			for _, p := range before {
				had[p] = true
			}
			wrote = slices.DeleteFunc(after, func(p string) bool { return had[p] })
		}
	}
	m.recordOwnFiles(ctx, sl, append(wrote, copied...), true)
	return nil
}

// untrackedForRecord lists the untracked files of slot sl, when (before or
// after) its setup runs, for recordingSetup; false for a listing that
// fails, which is logged.
func (m *Manager) untrackedForRecord(ctx context.Context, sl store.Slot, when string) ([]string, bool) {
	files, err := m.git.UntrackedFiles(ctx, sl.Path)
	if errors.Is(err, gitx.ErrTreeTooLarge) {
		err = fmt.Errorf("more than the runner's output of names: %w", err)
	}
	if err != nil {
		m.logErr(ctx, err, "slots: list the untracked files of %s %s its setup (a removal saves what the setup wrote as a person's): %v",
			sl.Name, when, err)
		return nil, false
	}
	return files, true
}

// sortOwn splits untracked, the untracked paths of the tree at tree, into
// a person's (theirs) and the paths own names (ownFiles, mine): a file a
// copy_files entry names, what the setup wrote, a copy_files folder git
// status names whole. A path below a folder own names (a copy_files
// folder) is in neither when it is the main clone's regular file there
// (at mainClone), byte for byte (compare: sameAsMain), as the copy of such
// a folder writes those alone (copyTreeIntoCheckout); one the main clone
// holds none of, or one that differs, which a person wrote or edited
// there, is theirs, and so is one that cannot be compared (the save then
// reads it itself), but for an error of the whole process (processWide),
// which fails the save.
func sortOwn(tree, mainClone string, untracked, own []string, compare sameFunc) (theirs, mine []string, err error) {
	if len(own) == 0 {
		return untracked, nil, nil
	}
	c := copies{tree: tree, main: mainFiles{dir: mainClone}, compare: compare}
	defer c.close()
	for _, p := range untracked {
		switch {
		case slices.Contains(own, p):
			mine = append(mine, p)
		case !slices.ContainsFunc(own, func(o string) bool { return strings.HasSuffix(o, "/") && strings.HasPrefix(p, o) }):
			theirs = append(theirs, p)
		default:
			copied, err := c.copied(p)
			if err != nil {
				return nil, nil, err
			}
			if !copied {
				theirs = append(theirs, p)
			}
		}
	}
	return theirs, mine, nil
}

// withoutOwn is untracked less the files of own: sortOwn's theirs.
func withoutOwn(tree, mainClone string, untracked, own []string, compare sameFunc) ([]string, error) {
	theirs, _, err := sortOwn(tree, mainClone, untracked, own, compare)
	return theirs, err
}

// copies compares the files of a tree below a copy_files folder with the
// main clone's, the tree read through an os.Root opened at the first
// comparison.
type copies struct {
	tree    string
	src     *os.Root
	err     error
	main    mainFiles
	compare sameFunc
}

// copied reports whether file p of the tree is the main clone's regular
// file there, byte for byte: false when the main clone holds none, and
// for one that cannot be compared, but an error of the whole process
// (processWide).
func (c *copies) copied(p string) (bool, error) {
	if !c.main.holds(p) {
		return false, nil
	}
	if c.src == nil && c.err == nil {
		c.src, c.err = os.OpenRoot(c.tree)
	}
	same, err := false, c.err
	if err == nil {
		same, err = c.compare(c.src, c.main.dir, p)
	}
	if err != nil && processWide(err) {
		return false, err
	}
	return err == nil && same, nil
}

func (c *copies) close() {
	c.main.close()
	if c.src != nil {
		_ = c.src.Close()
	}
}

// editedOwn names the files of mine (sortOwn's, in the tree at tree) that
// differ from the main clone's copy at mainClone (compare: sameAsMain),
// which a person edited since magnum copied it, each "<path> (<why>)": a
// removal saves none of magnum's files, which hold the main clone's
// secrets, but names those. A file the main clone holds none of, what the
// setup wrote, is not compared; nor is one gone from the tree. A
// comparison that fails is named with its error, but an error of the
// whole process (processWide), which fails the save.
func editedOwn(tree, mainClone string, mine []string, compare sameFunc) ([]string, error) {
	if len(mine) == 0 {
		return nil, nil
	}
	src, err := os.OpenRoot(tree)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	var out []string
	for _, p := range mine {
		same, err := compare(src, mainClone, p)
		switch {
		case err != nil && processWide(err):
			return nil, err
		case err != nil:
			out = append(out, p+" (magnum's own, not saved, cannot be compared with the main clone's: "+errText(err)+")")
		case !same:
			out = append(out, p+" (magnum's own, edited, not saved)")
		}
	}
	return out, nil
}

// sameFunc compares file p of the tree at src with the main clone's at
// mainClone: sameAsMain, or a test's stand-in (Manager.sameMain).
type sameFunc func(src *os.Root, mainClone, p string) (bool, error)

// sameMainOr is the comparison of a removal's files with the main clone's:
// a test's stand-in, else sameAsMain.
func (m *Manager) sameMainOr() sameFunc {
	if m.sameMain != nil {
		return m.sameMain
	}
	return sameAsMain
}

// sameAsMain reports whether file p of the tree at src is the same as the
// main clone's at mainClone, which the copy read through its links
// (CopyIntoCheckout): true when the main clone holds no regular file of
// that path or the tree holds no file there; false for a tree's file that
// is no regular file (magnum wrote one).
func sameAsMain(src *os.Root, mainClone, p string) (bool, error) {
	name := filepath.FromSlash(p)
	mainPath := filepath.Join(mainClone, name)
	mfi, err := os.Stat(mainPath)
	if err != nil || !mfi.Mode().IsRegular() {
		return true, nil
	}
	fi, err := src.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, err
	case !fi.Mode().IsRegular():
		return false, nil
	case fi.Size() != mfi.Size():
		return false, nil
	}
	a, err := src.Open(name)
	if err != nil {
		return false, err
	}
	defer a.Close()
	b, err := os.Open(mainPath)
	if err != nil {
		return false, err
	}
	defer b.Close()
	return sameContent(a, b)
}

// sameContent reports whether a and b read the same bytes to their ends.
func sameContent(a, b io.Reader) (bool, error) {
	ba, bb := make([]byte, 32<<10), make([]byte, 32<<10)
	for {
		na, ea := io.ReadFull(a, ba)
		nb, eb := io.ReadFull(b, bb)
		if err := cmp.Or(readErr(ea), readErr(eb)); err != nil {
			return false, err
		}
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		if ea != nil || eb != nil {
			return ea != nil && eb != nil, nil
		}
	}
}

// readErr is err of a read to the end, nil at the end.
func readErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}

// mainFiles reads the main clone at dir, through an os.Root opened at the
// first read.
type mainFiles struct {
	dir  string
	root *os.Root
	err  error
}

// holds reports whether the main clone holds p, a slash-separated path
// relative to it, as a regular file; false when it cannot be read.
func (f *mainFiles) holds(p string) bool {
	if f.root == nil && f.err == nil {
		f.root, f.err = os.OpenRoot(f.dir)
	}
	if f.err != nil {
		return false
	}
	fi, err := f.root.Lstat(filepath.FromSlash(p))
	return err == nil && fi.Mode().IsRegular()
}

func (f *mainFiles) close() {
	if f.root != nil {
		_ = f.root.Close()
	}
}
