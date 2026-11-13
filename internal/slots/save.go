package slots

// Saves before a discard: no change in a checkout goes without a trace.
// Before a step drops the tracked changes of a slot's tree (a switch or a
// reset with --discard-changes, the abort of an operation left in
// progress) or removes the tree with what it holds (a worktree's removal
// with --force), saveChanges writes what the tree holds to a folder of its
// own, in the folder of the PR the slot holds (savesFolder):
//   - changes.diff, the binary patch of the tracked changes against HEAD
//     (gitx.DiffHeadTo), which `git apply` puts back on a checkout at that
//     HEAD;
//   - untracked/, each untracked file git does not ignore at its path, but
//     the local mise file magnum rendered;
//   - saved.txt: what was saved, from where and before what, how to put it
//     back, and what the save left out, past its cap (SaveCap, over the
//     patch and the files together) or no regular file.
//
// Ignored files are never saved: a work tree's caches, logs and builds,
// which a discard keeps anyway, but for a removal's.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// SaveCap bounds what one save keeps, the patch and the untracked files
// together: past it, a path's patch or a file is left out and named.
const SaveCap = 50 << 20

// The parts of a save, in its folder.
const (
	saveDiff  = "changes.diff"
	saveFiles = "untracked"
	saveNote  = "saved.txt"
)

// SetSaveDir names where the saves of a slot's changes go (saveChanges):
// dir answers the folder for slot sl and the PR row that holds it (the
// zero PR for none), "" for this package's own (savesFolder). The factory's
// copies set theirs, a task's folder or a PR work's; call it before the
// manager's first discard.
func (m *Manager) SetSaveDir(dir func(sl store.Slot, pr store.PR) string) {
	m.saveDir.Store(&dir)
}

// savesFolder is where a save of slot sl's changes goes: the folder
// SetSaveDir's function names, else the review slots' own, PRSaves of the
// PR the slot holds (or that a per-PR worktree is for), else SlotSaves by
// the slot's name.
func (m *Manager) savesFolder(ctx context.Context, sl store.Slot) (string, error) {
	pr, err := m.guardPR(ctx, sl)
	if err != nil {
		return "", err
	}
	if f := m.saveDir.Load(); f != nil {
		if dir := (*f)(sl, pr); dir != "" {
			return dir, nil
		}
	}
	if !m.d.Layout.Valid() {
		return "", fmt.Errorf("slots: no folder for the saves of %s", sl.Name)
	}
	if pr.ID != 0 && pr.Number > 0 {
		repo, err := m.d.Store.RepoByID(ctx, pr.RepoID)
		switch {
		case err == nil:
			return m.d.Layout.PRSaves(repo.Owner, repo.Name, pr.Number), nil
		case !errors.Is(err, store.ErrNotFound):
			return "", fmt.Errorf("slots: the repository of %s's PR: %w", sl.Name, err)
		}
	}
	return m.d.Layout.SlotSaves(pathElement(sl.Name)), nil
}

// pathElement is name as one path element: its separators as "-", "slot"
// for a name that is none.
func pathElement(name string) string {
	name = strings.NewReplacer("/", "-", `\`, "-").Replace(name)
	if name == "" || name == "." || name == ".." {
		return "slot"
	}
	return name
}

// saved is what saveChanges kept: its folder, how many paths its patch
// holds and how many untracked files it saved, and what it left out, each
// "<path> (<why>)".
type saved struct {
	dir                string
	tracked, untracked int
	skipped            []string
}

// note is what a slot.discarded event adds about the save: where it is,
// and the first maxListed of what it left out.
func (s saved) note() string {
	out := "; saved to " + s.dir
	if len(s.skipped) > 0 {
		out += fmt.Sprintf("; the save left out %d: %s", len(s.skipped), clipNames(s.skipped))
	}
	return out
}

// clipNames joins the first maxListed of names, and how many more.
func clipNames(names []string) string {
	out := strings.Join(names[:min(len(names), maxListed)], ", ")
	if more := len(names) - maxListed; more > 0 {
		out += fmt.Sprintf(" (+%d more)", more)
	}
	return out
}

// saveChanges saves the tree of slot sl, whose changes are ch, before what
// discards them (the step's words, "the switch to …"), in a folder of its
// own under savesFolder: the patch of the tracked changes, the untracked
// files git does not ignore and saved.txt, within the cap (saveCap). A save
// that fails leaves nothing behind, and the step that called it discards
// nothing.
func (m *Manager) saveChanges(ctx context.Context, sl store.Slot, ch changes, what string) (s saved, err error) {
	folder, err := m.savesFolder(ctx, sl)
	if err != nil {
		return saved{}, err
	}
	if s.dir, err = newSaveDir(folder, m.now(), sl.Name); err != nil {
		return saved{}, fmt.Errorf("slots: save the changes in %s: %w", sl.Path, err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(s.dir)
			err = fmt.Errorf("slots: save the changes in %s: %w", sl.Path, err)
		}
	}()
	head, err := m.git.RevParse(ctx, sl.Path, "HEAD")
	if err != nil {
		return s, err
	}
	budget := m.saveCapOr()
	if len(ch.tracked) > 0 {
		used, err := m.savePatch(ctx, sl.Path, s.dir, ch.patchPaths(), budget, &s)
		if err != nil {
			return s, err
		}
		budget -= used
	}
	files, err := m.git.UntrackedFiles(ctx, sl.Path)
	if errors.Is(err, gitx.ErrTreeTooLarge) {
		// Their names alone pass the runner's output: far past the cap.
		s.skipped = append(s.skipped, "every untracked file (git lists more than "+sizeWord(execx.MaxOutput)+" of their names)")
		files, err = nil, nil
	}
	if err != nil {
		return s, err
	}
	if err := saveUntracked(sl.Path, s.dir, withoutRenderedMiseLocal(sl.Path, files), budget, &s); err != nil {
		return s, err
	}
	return s, s.writeNote(sl, head, what, m.saveCapOr(), m.now())
}

// saveCapOr is the cap of a save: saveCap, a test's smaller one, else
// SaveCap.
func (m *Manager) saveCapOr() int64 {
	if m.saveCap > 0 {
		return m.saveCap
	}
	return SaveCap
}

// newSaveDir makes the folder of a save in folder, private, named by the
// time and the slot: <UTC time>-<slot>, else with -2, -3… when a save of
// the same second has that name.
func newSaveDir(folder string, now time.Time, slot string) (string, error) {
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return "", err
	}
	base := now.UTC().Format("20060102T150405Z") + "-" + pathElement(slot)
	for k := 1; k <= 100; k++ {
		name := base
		if k > 1 {
			name = fmt.Sprintf("%s-%d", base, k)
		}
		dir := filepath.Join(folder, name)
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free name for a save in %s", folder)
}

// patchPaths are the paths whose patch a save holds: the tracked changes',
// a rename's source too, sorted.
func (c changes) patchPaths() []string {
	out := slices.Concat(c.tracked, c.sources)
	slices.Sort(out)
	return slices.Compact(out)
}

// savePatch writes the patch of the tracked changes of the tree at tree
// against HEAD to the save's changes.diff, at most budget bytes, and
// returns its size. A patch past budget is written path by path (paths),
// each that still fits, and the paths left out are named in s.
func (m *Manager) savePatch(ctx context.Context, tree, dir string, paths []string, budget int64, s *saved) (int64, error) {
	part, final := filepath.Join(dir, saveDiff+".part"), filepath.Join(dir, saveDiff)
	size, err := m.diffTo(ctx, tree, part)
	if err != nil {
		return 0, err
	}
	if size <= budget {
		s.tracked = len(paths)
		return size, os.Rename(part, final)
	}
	out, err := os.OpenFile(final, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	var used int64
	for _, p := range paths {
		size, err := m.diffTo(ctx, tree, part, p)
		switch {
		case err != nil:
			return 0, err
		case size == 0:
			continue
		case used+size > budget:
			s.skipped = append(s.skipped, fmt.Sprintf("%s (%s of patch, past the cap)", p, sizeWord(size)))
			continue
		}
		if err := appendFile(out, part); err != nil {
			return 0, err
		}
		used += size
		s.tracked++
	}
	if err := os.Remove(part); err != nil {
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	if used == 0 { // no path's patch fit: no patch
		return 0, os.Remove(final)
	}
	return used, nil
}

// diffTo writes the patch of paths (none: every tracked change) of the
// tree at tree against HEAD to file, private, and returns its size.
func (m *Manager) diffTo(ctx context.Context, tree, file string, paths ...string) (int64, error) {
	// git keeps the mode of the file it truncates.
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := m.git.DiffHeadTo(ctx, tree, file, paths...); err != nil {
		return 0, err
	}
	fi, err := os.Stat(file)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// appendFile appends the file at name to out.
func appendFile(out *os.File, name string) error {
	in, err := os.Open(name)
	if err != nil {
		return err
	}
	defer in.Close()
	_, err = io.Copy(out, in)
	return err
}

// saveUntracked saves the untracked files of the tree at tree (files, git's
// listing of those it does not ignore) under the save's untracked/, each a
// regular file read through an os.Root of the tree and written private, at
// most budget bytes together; the others are named in s: a file past what
// is left, a symlink with its target, a repository inside the tree, and any
// other file that is no regular file.
func saveUntracked(tree, dir string, files []string, budget int64, s *saved) error {
	if len(files) == 0 {
		return nil
	}
	src, err := os.OpenRoot(tree)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.Mkdir(filepath.Join(dir, saveFiles), 0o700); err != nil {
		return err
	}
	dst, err := os.OpenRoot(filepath.Join(dir, saveFiles))
	if err != nil {
		return err
	}
	defer dst.Close()
	for _, f := range files {
		why, err := saveFile(src, dst, f, &budget)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		if why != "" {
			s.skipped = append(s.skipped, f+" ("+why+")")
			continue
		}
		s.untracked++
	}
	return nil
}

// saveFile copies untracked file f of src to dst and takes its size from
// budget, or says why it does not ("" when it did).
func saveFile(src, dst *os.Root, f string, budget *int64) (string, error) {
	if strings.HasSuffix(f, "/") {
		return "a repository inside the work tree", nil
	}
	fi, err := src.Lstat(f)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "gone before the save", nil
	case err != nil:
		return "", err
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := src.Readlink(f)
		if err != nil {
			return "", err
		}
		return "a symlink to " + target, nil
	case !fi.Mode().IsRegular():
		return "not a regular file", nil
	case fi.Size() > *budget:
		return sizeWord(fi.Size()) + ", past the cap", nil
	}
	b, err := fsx.ReadRegularIn(src, f, *budget)
	switch {
	case errors.Is(err, fsx.ErrTooLarge):
		return "grew past the cap", nil
	case errors.Is(err, fsx.ErrNotRegular):
		return "not a regular file", nil
	case err != nil:
		return "", err
	}
	if d := path.Dir(f); d != "." {
		if err := dst.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
	}
	if err := fsx.WriteFileAtomicIn(dst, f, b, 0o600); err != nil {
		return "", err
	}
	*budget -= int64(len(b))
	return "", nil
}

// sizeWord is n bytes for a person: bytes, KB or MB.
func sizeWord(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// writeNote writes the save's saved.txt: what it holds, from slot sl at
// head, before what, at now, how to put it back, and what it left out.
func (s saved) writeNote(sl store.Slot, head, what string, limit int64, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "magnum saved what %s held before %s discarded it.\n\n", sl.Path, what)
	fmt.Fprintf(&b, "- checkout: %s (%s)\n- HEAD: %s\n- at: %s\n", sl.Name, sl.Path, head, now.UTC().Format(time.RFC3339))
	quoted := execx.ShellQuote(sl.Path)
	if s.tracked > 0 {
		fmt.Fprintf(&b, "- %d tracked path(s) in %s, the patch against HEAD: on a checkout at %s, `git -C %s apply %s` puts them back\n",
			s.tracked, saveDiff, textx.ShortSHA(head), quoted, execx.ShellQuote(filepath.Join(s.dir, saveDiff)))
	}
	if s.untracked > 0 {
		fmt.Fprintf(&b, "- %d untracked file(s) under %s/: `cp -Rp %s %s` puts them back\n", s.untracked, saveFiles,
			execx.ShellQuote(filepath.Join(s.dir, saveFiles)+"/."), quoted)
	}
	if len(s.skipped) > 0 {
		fmt.Fprintf(&b, "\nLeft out (a save keeps %s at most, and only regular files; files git ignores are never saved):\n",
			sizeWord(limit))
		for _, p := range s.skipped {
			b.WriteString("- " + p + "\n")
		}
	}
	return fsx.WriteFileAtomic(filepath.Join(s.dir, saveNote), []byte(b.String()), 0o600)
}
