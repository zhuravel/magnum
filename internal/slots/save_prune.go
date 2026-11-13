package slots

// How long saves stay. A save is kept SaveKeep (14 days, by the UTC time
// its folder's name starts with), then pruned: by the next save in its
// folder (saveChanges) and by each daemon's prune of every folder of saves
// (PruneSaves over paths.Layout.ReviewSaveFolders or FactorySaveFolders),
// for the PRs that never save again. A save whose changes.diff is the same,
// byte for byte, as the newest save of the same slot in its folder, and
// that copied no untracked file, is not kept (lastSave, samePatch):
// post_checkout's residue saves once, not at every round.

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// SaveKeep is how long a save before a discard is kept.
const SaveKeep = 14 * 24 * time.Hour

// saveTimeLayout is the UTC time a save's folder's name starts with
// (newSaveDir), before "-<slot>".
const saveTimeLayout = "20060102T150405Z"

// saveTime is the time a save's folder's name starts with; false for a name
// that is no save's.
func saveTime(name string) (time.Time, bool) {
	n := len(saveTimeLayout)
	if len(name) < n+2 || name[n] != '-' {
		return time.Time{}, false
	}
	at, err := time.Parse(saveTimeLayout, name[:n])
	return at, err == nil
}

// PruneSaves removes the saves older than SaveKeep at now in every folder
// of saves folders names (a path whose "*" elements stand for any folder;
// nothing else in it is special) and returns how many it removed. A folder
// whose name is no save's is left alone, and so is the folder of saves
// itself (a save may be making its own folder in it). A save it cannot
// remove is an error, after the others went.
func PruneSaves(folders []string, now time.Time) (int, error) {
	var (
		n    int
		errs []error
	)
	for _, pattern := range folders {
		for _, folder := range expandFolders(pattern) {
			k, err := pruneSaveFolder(folder, now)
			n += k
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	return n, errors.Join(errs...)
}

// expandFolders is the folders that exist that pattern names, each "*"
// element any folder (never a symlink to one).
func expandFolders(pattern string) []string {
	elems := strings.Split(filepath.ToSlash(filepath.Clean(pattern)), "/")
	i := slices.Index(elems, "*")
	if i < 0 {
		if fi, err := os.Lstat(pattern); err == nil && fi.IsDir() {
			return []string{pattern}
		}
		return nil
	}
	base := filepath.FromSlash(strings.Join(elems[:i], "/"))
	if base == "" {
		base = string(filepath.Separator)
	}
	ents, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, expandFolders(filepath.Join(append([]string{base, e.Name()}, elems[i+1:]...)...))...)
		}
	}
	return out
}

// pruneSaveFolder removes the saves in folder older than SaveKeep at now
// and returns how many.
func pruneSaveFolder(folder string, now time.Time) (int, error) {
	ents, err := os.ReadDir(folder)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-SaveKeep)
	var (
		n    int
		errs []error
	)
	for _, e := range ents {
		at, ok := saveTime(e.Name())
		if !e.IsDir() || !ok || !at.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(folder, e.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// lastSave is the newest save of slot in folder but the one at dir (by its
// folder's name, which starts with its time; its saved.txt names the
// slot), "" for none.
func lastSave(folder, dir, slot string) string {
	ents, err := os.ReadDir(folder)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range ents {
		if _, ok := saveTime(e.Name()); ok && e.IsDir() && e.Name() != filepath.Base(dir) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	for _, name := range slices.Backward(names) {
		if save := filepath.Join(folder, name); savedSlot(save) == slot {
			return save
		}
	}
	return ""
}

// savedSlot is the checkout the saved.txt of the save at dir names ("" for
// none): its "- checkout: <name> (<path>)" line, among its first lines.
func savedSlot(dir string) string {
	f, err := os.Open(filepath.Join(dir, saveNote))
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 4<<10)
	n, _ := io.ReadFull(f, head)
	for line := range strings.Lines(string(head[:n])) {
		if after, ok := strings.CutPrefix(line, "- checkout: "); ok {
			name, _, _ := strings.Cut(after, " (")
			return name
		}
	}
	return ""
}

// atEnd reports whether err, of io.ReadFull, is the end of the file.
func atEnd(err error) bool { return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) }

// samePatch reports whether the saves at a and b hold the same
// changes.diff, byte for byte, or neither holds one.
func samePatch(a, b string) bool {
	fa, errA := os.Open(filepath.Join(a, saveDiff))
	fb, errB := os.Open(filepath.Join(b, saveDiff))
	if errA == nil {
		defer fa.Close()
	}
	if errB == nil {
		defer fb.Close()
	}
	switch {
	case errors.Is(errA, fs.ErrNotExist) && errors.Is(errB, fs.ErrNotExist):
		return true
	case errA != nil || errB != nil:
		return false
	}
	ia, errA := fa.Stat()
	ib, errB := fb.Stat()
	if errA != nil || errB != nil || ia.Size() != ib.Size() {
		return false
	}
	ba, bb := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, errA := io.ReadFull(fa, ba)
		nb, errB := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false
		}
		if errA != nil || errB != nil { // the end of both (same size), or a read that failed
			return atEnd(errA) && atEnd(errB)
		}
	}
}
