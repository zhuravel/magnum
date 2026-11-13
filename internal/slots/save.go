package slots

// Saves before a discard: no change in a checkout goes without a trace.
// Before a step drops the tracked changes of a slot's tree (a switch or a
// reset with --discard-changes, the abort of an operation left in
// progress) or removes the tree with what it holds (a worktree's removal
// with --force), saveChanges writes what the tree holds to a folder of its
// own, in the folder of the PR the slot holds (savesFolder):
//   - changes.diff, the binary patch of the tracked changes against HEAD
//     (gitx.DiffHeadWrite), which `git apply` puts back on a checkout at that
//     HEAD;
//   - untracked/, for a removal alone, each untracked file git does not
//     ignore at its path (a symlink as the link, never what it points to),
//     but the files magnum wrote (ownFiles: the local mise file it
//     rendered, the copy_files, what setup wrote): a switch, a reset and an
//     abort keep them;
//   - saved.txt: what was saved, from where and before what, how to put it
//     back, the untracked files a switch keeps in the tree, and what the
//     save left out: past its cap (SaveCap, over the patch and the files
//     together), no regular file or symlink, or a file it could not read
//     or write.
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
// holds and how many untracked files it saved, what it left out, each
// "<path> (<why>)", and the untracked files the step keeps in the tree (a
// switch's, a reset's, an abort's), which it names only. same says the
// save was the slot's last one again, which dir names (lastSave).
type saved struct {
	dir                string
	tracked, untracked int
	skipped, kept      []string
	same               bool
}

// note is what a slot.discarded event adds about the save: where it is,
// and the first maxListed of what it left out.
func (s saved) note() string {
	if s.same {
		return "; unchanged since the save " + s.dir + ", which holds them"
	}
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
// own under savesFolder: the patch of the tracked changes and saved.txt,
// and with untracked (a removal, which deletes them) the untracked files
// git does not ignore, within the cap (saveCap); without it saved.txt only
// names those files, which the step keeps. A file that cannot be copied is
// left out and named, never a failure. The files magnum wrote into the
// tree (the mise file it rendered, the copy_files and what setup wrote:
// ownFiles) are neither saved nor named, and a save that would hold
// nothing else is not kept (s.dir is ""); nor is one whose patch is the
// slot's last save's and that copied no file (s.same, s.dir that save).
// Saves past SaveKeep in the folder go first (pruneSaveFolder). A save
// that fails (its folder, the patch, the listing of the untracked files,
// saved.txt) leaves nothing behind, and the step that called it discards
// nothing.
func (m *Manager) saveChanges(ctx context.Context, sl store.Slot, ch changes, untracked bool, what string) (s saved, err error) {
	folder, err := m.savesFolder(ctx, sl)
	if err != nil {
		return saved{}, err
	}
	if _, err := pruneSaveFolder(folder, m.now()); err != nil {
		m.logErr(ctx, err, "slots: prune the saves past %s in %s: %v", SaveKeep, folder, err)
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
	own := m.ownFiles(ctx, sl)
	if !untracked {
		s.kept = withoutOwn(withoutRenderedMiseLocal(sl.Path, slices.Clone(ch.untracked)), own)
	} else if err := m.saveRemoved(ctx, sl, own, budget, &s); err != nil {
		return s, err
	}
	return m.settleSave(folder, sl, s, head, what)
}

// settleSave keeps save s of slot sl, in folder, with its saved.txt (HEAD
// head, before what), and returns what the slot.discarded event names:
// itself, nothing for a save that holds nothing (magnum's own files alone),
// or the slot's last save (lastSave) when s copied no file and its patch is
// that save's (samePatch), which then stands for it; s is removed in both.
func (m *Manager) settleSave(folder string, sl store.Slot, s saved, head, what string) (saved, error) {
	last := ""
	if !s.empty() && s.untracked == 0 {
		last = lastSave(folder, s.dir, sl.Name)
	}
	if !s.empty() && (last == "" || !samePatch(last, s.dir)) {
		return s, s.writeNote(sl, head, what, m.saveCapOr(), m.now())
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return s, err
	}
	if last == "" {
		return saved{}, nil
	}
	return saved{dir: last, same: true}, nil
}

// saveRemoved saves the untracked files of slot sl that a removal deletes
// (UntrackedFiles, one by one), but the files magnum wrote (the rendered
// mise file, own), within budget, into s. A listing past the runner's
// output saves none and says so; any other failure to list is an error.
func (m *Manager) saveRemoved(ctx context.Context, sl store.Slot, own []string, budget int64, s *saved) error {
	files, err := m.git.UntrackedFiles(ctx, sl.Path)
	if errors.Is(err, gitx.ErrTreeTooLarge) {
		// Their names alone pass the runner's output: far past the cap.
		s.skipped = append(s.skipped, "every untracked file (git lists more than "+sizeWord(execx.MaxOutput)+" of their names)")
		return nil
	}
	if err != nil {
		return err
	}
	return saveUntracked(sl.Path, s.dir, withoutOwn(withoutRenderedMiseLocal(sl.Path, files), own), budget, s)
}

// empty reports whether the save holds nothing and names nothing.
func (s saved) empty() bool {
	return s.tracked == 0 && s.untracked == 0 && len(s.skipped) == 0 && len(s.kept) == 0
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
// returns its size; git is stopped at the cap (diffTo), so no more than
// budget bytes of it are written. A patch past budget is written path by
// path (paths), each that still fits, and the paths left out are named in
// s.
func (m *Manager) savePatch(ctx context.Context, tree, dir string, paths []string, budget int64, s *saved) (int64, error) {
	part, final := filepath.Join(dir, saveDiff+".part"), filepath.Join(dir, saveDiff)
	size, over, err := m.diffTo(ctx, tree, part, budget)
	switch {
	case err != nil:
		return 0, err
	case over:
	case size == 0: // a submodule's untracked content: a change git diff writes nothing for
		return 0, os.Remove(part)
	default:
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
		left := budget - used
		if left < int64(2*len(p)+len("diff --git a/ b/\n")) { // not even its patch's first line fits
			s.skipped = append(s.skipped, p+" (past the cap)")
			continue
		}
		size, over, err := m.diffTo(ctx, tree, part, left, p)
		switch {
		case err != nil:
			return 0, err
		case over:
			s.skipped = append(s.skipped, fmt.Sprintf("%s (over %s of patch, past the cap)", p, sizeWord(left)))
			continue
		case size == 0:
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
// tree at tree against HEAD to file, private, at most limit bytes, and
// returns its size: git writes it into a pipe (gitx.DiffHeadWrite), which
// this reads into file. Past limit it stops git and says over (file then
// holds no patch). A write to file that fails (a full disk) is an error,
// so a patch cut short never counts as whole: git's own --output ignored
// its write errors and exited 0.
func (m *Manager) diffTo(ctx context.Context, tree, file string, limit int64, paths ...string) (size int64, over bool, err error) {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, false, err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	w := &cappedWriter{w: f, limit: limit, stop: stop}
	err = m.git.DiffHeadWrite(ctx, tree, w, paths...)
	if cerr := f.Close(); w.err == nil {
		w.err = cerr
	}
	switch {
	case w.err != nil:
		return 0, false, w.err
	case w.over:
		return 0, true, nil
	case err != nil:
		return 0, false, err
	}
	return w.n, false, nil
}

// cappedWriter writes to w at most limit bytes. Past them, or at w's first
// failed write, it writes nothing more, says why (over, err) and calls stop,
// which ends the writer's source (git). It takes every write, so its
// source never blocks on it.
type cappedWriter struct {
	w     io.Writer
	limit int64
	stop  func()
	n     int64
	over  bool
	err   error
	done  bool // over, or err: nothing more is written
}

// Write writes p to w, or nothing once the writer is done; it never
// fails its caller, whose output the writer keeps or drops (over, err).
func (c *cappedWriter) Write(p []byte) (int, error) {
	switch {
	case c.done:
	case c.n+int64(len(p)) > c.limit:
		c.over, c.done = true, true
		c.stop()
	default:
		n, err := c.w.Write(p)
		c.n += int64(n)
		if err != nil {
			c.err, c.done = err, true
			c.stop()
		}
	}
	return len(p), nil
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
// regular file read through an os.Root of the tree and written private,
// or a symlink saved as the link, at most budget bytes together; the
// others are named in s: a file past what is left, a repository inside the
// tree, any other file that is no regular file, and a file it could not
// read or write (unreadable, gone, a name the save's folder refuses), with
// the error. untracked/ is made at the first file saved, so a save that copies
// none has no such folder. Only that folder failing is an error.
func saveUntracked(tree, dir string, files []string, budget int64, s *saved) error {
	if len(files) == 0 {
		return nil
	}
	src, err := os.OpenRoot(tree)
	if err != nil {
		return err
	}
	defer src.Close()
	u := &untrackedSave{src: src, dir: filepath.Join(dir, saveFiles), budget: budget}
	defer u.close()
	for _, f := range files {
		why, err := u.save(f)
		if err != nil {
			return err
		}
		if why != "" {
			s.skipped = append(s.skipped, f+" ("+why+")")
			continue
		}
		s.untracked++
	}
	return nil
}

// untrackedSave copies a removal's untracked files from src, the tree, to
// dir, the save's untracked/, within budget bytes.
type untrackedSave struct {
	src    *os.Root
	dir    string
	dst    *os.Root // dir, made and opened at the first file saved
	budget int64
}

// folder is the save's untracked/, made at the first call.
func (u *untrackedSave) folder() (*os.Root, error) {
	if u.dst != nil {
		return u.dst, nil
	}
	if err := os.Mkdir(u.dir, 0o700); err != nil {
		return nil, err
	}
	dst, err := os.OpenRoot(u.dir)
	if err != nil {
		return nil, err
	}
	u.dst = dst
	return dst, nil
}

func (u *untrackedSave) close() {
	if u.dst != nil {
		_ = u.dst.Close()
	}
}

// save copies untracked file f and takes its size from the budget, or
// says why it does not ("" when it did): a file it cannot read or write is
// left out with the error, as a file past the cap is. The error is the
// save's folder's alone.
func (u *untrackedSave) save(f string) (string, error) {
	if strings.HasSuffix(f, "/") {
		return "a repository inside the work tree", nil
	}
	fi, err := u.src.Lstat(f)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "gone before the save", nil
	case err != nil:
		return "cannot be read: " + errText(err), nil
	case fi.Mode()&fs.ModeSymlink != 0:
		return u.link(f)
	case !fi.Mode().IsRegular():
		return "not a regular file", nil
	case fi.Size() > u.budget:
		return sizeWord(fi.Size()) + ", past the cap", nil
	}
	b, err := fsx.ReadRegularIn(u.src, f, u.budget)
	switch {
	case errors.Is(err, fsx.ErrTooLarge):
		return "grew past the cap", nil
	case errors.Is(err, fsx.ErrNotRegular):
		return "not a regular file", nil
	case errors.Is(err, fs.ErrNotExist):
		return "gone before the save", nil
	case err != nil:
		return "cannot be read: " + errText(err), nil
	}
	dst, err := u.folder()
	if err != nil {
		return "", err
	}
	if d := path.Dir(f); d != "." {
		if err := dst.MkdirAll(d, 0o700); err != nil {
			return "cannot be written: " + errText(err), nil
		}
	}
	// The owner's bits (an executable script stays one when `cp -R` puts
	// it back), never the group's or others'.
	if err := writeSaved(dst, f, b, fi.Mode().Perm()&0o700|0o600); err != nil {
		return "cannot be written: " + errText(err), nil
	}
	u.budget -= int64(len(b))
	return "", nil
}

// link saves symlink f as a symlink to the same target, never what it
// points to (a link to a folder of keys copies no key), its target's
// length taken from the budget; or says why it does not, as save does.
func (u *untrackedSave) link(f string) (string, error) {
	target, err := u.src.Readlink(f)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "gone before the save", nil
	case err != nil:
		return "cannot be read: " + errText(err), nil
	case int64(len(target)) > u.budget:
		return "a symlink to " + target + ", past the cap", nil
	}
	dst, err := u.folder()
	if err != nil {
		return "", err
	}
	if d := path.Dir(f); d != "." {
		if err := dst.MkdirAll(d, 0o700); err != nil {
			return "cannot be written: " + errText(err), nil
		}
	}
	if err := dst.Symlink(target, f); err != nil {
		return "cannot be written: " + errText(err), nil
	}
	u.budget -= int64(len(target))
	return "", nil
}

// writeSaved writes b to name in dst, the save's own folder, which nothing
// else writes, mode perm whatever the umask: created there, with no
// temporary file (whose longer name a file name near 255 bytes leaves no
// room for) and no disk flush, which an atomic write pays twice per file.
// saved.txt, written last and durably, marks the save whole; a file cut
// short is removed.
func writeSaved(dst *os.Root, name string, b []byte, perm fs.FileMode) (err error) {
	f, err := dst.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = dst.Remove(name)
		}
	}()
	if err = f.Chmod(perm); err == nil {
		_, err = f.Write(b)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// errText is err's cause without the path a file operation's error names
// (the file is named beside it): "permission denied".
func errText(err error) string {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err.Error()
	}
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		return le.Err.Error()
	}
	return err.Error()
}

// keptListed caps the untracked files saved.txt names as kept in the tree
// (git status names a wholly untracked folder once).
const keptListed = 1000

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
		// Not -p: it would also set the tree's own folders to the save's 0700.
		fmt.Fprintf(&b, "- %d untracked file(s) under %s/, each with its owner's bits: `cp -R %s %s` puts them back\n",
			s.untracked, saveFiles, execx.ShellQuote(filepath.Join(s.dir, saveFiles)+"/."), quoted)
	}
	if len(s.kept) > 0 {
		fmt.Fprintf(&b, "\nUntracked, kept in the tree by %s and not saved (%d):\n", what, len(s.kept))
		for _, p := range s.kept[:min(len(s.kept), keptListed)] {
			b.WriteString("- " + p + "\n")
		}
		if more := len(s.kept) - keptListed; more > 0 {
			fmt.Fprintf(&b, "- (+%d more)\n", more)
		}
	}
	if len(s.skipped) > 0 {
		fmt.Fprintf(&b, "\nLeft out (a save keeps %s at most, only regular files and symlinks it can read and write, and never a file git ignores or magnum wrote):\n",
			sizeWord(limit))
		for _, p := range s.skipped {
			b.WriteString("- " + p + "\n")
		}
	}
	return fsx.WriteFileAtomic(filepath.Join(s.dir, saveNote), []byte(b.String()), 0o600)
}
