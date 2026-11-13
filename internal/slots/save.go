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
//     rendered, the copy_files, what setup wrote; of a copied folder, the
//     main clone's files as it holds them: sortOwn): a switch, a reset and
//     an abort keep them;
//   - saved.txt: what was saved, from where and before what, how to put it
//     back, the untracked files a switch keeps in the tree, and what the
//     save left out: past its cap (SaveCap, over the patch and the files
//     together), no regular file or symlink, a file it could not read, or
//     a file a copy_files entry names that a person edited (editedOwn). A
//     write into the save that fails fails the save.
//
// Ignored files are never saved: a work tree's caches, logs and builds,
// which a discard keeps anyway, but for a removal's.

import (
	"cmp"
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
	"syscall"
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
// save was the slot's last one again, which dir names (lastSave). unsaved
// is what the save does not hold if it fails at that point, which a
// forced removal deletes unsaved; failed, the error of such a save, kept
// in part (keepPartial), and noNote why its saved.txt could not be
// written either.
type saved struct {
	dir                string
	tracked, untracked int
	skipped, kept      []string
	same               bool
	unsaved            []string
	failed, noNote     string
}

// note is what a slot.discarded event adds about the save: where it is,
// and the first maxListed of what it left out.
func (s saved) note() string {
	out := "; saved to " + s.dir
	switch {
	case s.same:
		out = "; unchanged since the save " + s.dir + ", which holds them"
	case s.noNote != "":
		out = "; saved in part to " + s.dir + ", with no saved.txt (" + s.noNote + ")"
	case s.failed != "":
		out = "; saved in part to " + s.dir + ", whose saved.txt names all it does not hold"
	}
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
// names those files, which the step keeps. A file that cannot be read is
// left out and named, never a failure; a write into the save that fails
// is one. The files magnum wrote into the tree (the mise file it
// rendered, the copy_files and what setup wrote: ownFiles) are neither
// saved nor named, but a copy a person edited, which a removal names
// (editedOwn), and a save that would hold nothing else is not kept
// (s.dir is ""); nor is one that copied no file and repeats the slot's
// last save (repeatedSave: s.same, s.dir that save). Saves past SaveKeep
// in the folder go first (pruneSaveFolder). A save
// that fails (its folder, the patch, the listing of the untracked files,
// saved.txt) leaves nothing behind, and the step that called it discards
// nothing; but with force, a forced removal's, which goes on over it, the
// save keeps what it copied, with a saved.txt that names all it does not
// hold (keepPartial), and s is that save.
func (m *Manager) saveChanges(ctx context.Context, sl store.Slot, ch changes, untracked, force bool, what string) (s saved, err error) {
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
	head := ""
	defer func() {
		if err == nil {
			return
		}
		err = fmt.Errorf("slots: save the changes in %s: %w", sl.Path, err)
		if force {
			s = m.keepPartial(sl, s, head, what, err)
			return
		}
		_ = os.RemoveAll(s.dir)
		s = saved{}
	}()
	own := m.ownFiles(ctx, sl)
	listed := withoutRenderedMiseLocal(sl.Path, slices.Clone(ch.untracked))
	s.unsaved = slices.Concat(ch.tracked, listed) // magnum's own files with them, until they are sorted out
	if listed, err = withoutOwn(sl.Path, sl.MainClone, listed, own, m.sameMainOr()); err != nil {
		return s, err
	}
	if !untracked { // the step keeps them
		s.kept, listed = listed, nil
	}
	s.unsaved = slices.Concat(ch.tracked, listed)
	if head, err = m.git.RevParse(ctx, sl.Path, "HEAD"); err != nil {
		return s, err
	}
	budget := m.saveCapOr()
	if len(ch.tracked) > 0 {
		used, err := m.savePatch(ctx, sl.Path, s.dir, ch.patchPaths(), budget, &s)
		if err != nil {
			s.dropPatch()
			return s, err
		}
		budget -= used
	}
	s.unsaved = listed
	if untracked {
		if err := m.saveRemoved(ctx, sl, own, budget, &s); err != nil {
			return s, err
		}
	}
	s.unsaved = nil
	return m.settleSave(folder, sl, s, head, what)
}

// dropPatch removes what a patch that failed wrote into the save (a path's
// patch may be cut short), so a save kept in part holds none of it.
func (s *saved) dropPatch() {
	_ = os.Remove(filepath.Join(s.dir, saveDiff))
	_ = os.Remove(filepath.Join(s.dir, saveDiff+".part"))
	s.tracked, s.skipped = 0, nil
}

// keepPartial keeps save s of slot sl, which failed with err before a
// forced removal (HEAD head, "" when it failed first, before what): the
// removal goes on, so what the save copied stays, and saved.txt says it
// failed part way and names, as not saved, all it does not hold
// (s.unsaved), past the event's maxListed too. A saved.txt that cannot be
// written either leaves what was copied, which the event says (noNote);
// a save that then holds nothing goes.
func (m *Manager) keepPartial(sl store.Slot, s saved, head, what string, err error) saved {
	s.failed = textx.FirstLine(execx.Redact(err.Error())) // whole: failureLine clips the cause past a long path
	for _, p := range s.unsaved {
		s.skipped = append(s.skipped, p+" (not saved: the save failed)")
	}
	s.unsaved = nil
	werr := s.writeNote(sl, cmp.Or(head, "unknown"), what, m.saveCapOr(), m.now())
	if werr == nil {
		return s
	}
	if s.tracked == 0 && s.untracked == 0 {
		_ = os.RemoveAll(s.dir)
		return saved{}
	}
	s.noNote = failureLine(werr)
	return s
}

// settleSave keeps save s of slot sl, in folder, with its saved.txt (HEAD
// head, before what), and returns what the slot.discarded event names:
// itself, nothing for a save that holds nothing (magnum's own files alone),
// or the slot's last save when s copied no file and repeats it
// (repeatedSave), which then stands for it; s is removed in both.
func (m *Manager) settleSave(folder string, sl store.Slot, s saved, head, what string) (saved, error) {
	last := ""
	if !s.empty() && s.untracked == 0 {
		last = repeatedSave(folder, sl.Name, s, m.now())
	}
	if !s.empty() && last == "" {
		return s, s.writeNote(sl, head, what, m.saveCapOr(), m.now())
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return s, err
	}
	if last == "" {
		return saved{}, nil
	}
	return saved{dir: last, same: true, skipped: s.skipped}, nil
}

// saveRemoved saves the untracked files of slot sl that a removal deletes
// (UntrackedFiles, one by one), but the files magnum wrote (the rendered
// mise file, own), within budget, into s, and names those of own a person
// edited (editedOwn). A listing past the runner's output saves none and
// says so; any other failure to list is an error.
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
	theirs, mine, err := sortOwn(sl.Path, sl.MainClone, withoutRenderedMiseLocal(sl.Path, files), own, m.sameMainOr())
	if err != nil {
		return err
	}
	edited, err := editedOwn(sl.Path, sl.MainClone, mine, m.sameMainOr())
	if err != nil {
		return err
	}
	s.skipped = append(s.skipped, edited...)
	if err := m.saveUntracked(sl.Path, s.dir, theirs, budget, s); err != nil {
		return err
	}
	return nil
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
// read (unreadable, gone), with the error. untracked/ is made at the first
// file saved, so a save that copies none has no such folder. A write into
// the save that fails (a full disk, a quota, a name its folder refuses) is
// an error, the save's as a whole: it is no reason about the file, and the
// removal must not delete what the save does not hold. So is a read that
// fails for the whole process (processWide: too many open files, no
// memory), whose file the next attempt reads.
func (m *Manager) saveUntracked(tree, dir string, files []string, budget int64, s *saved) error {
	if len(files) == 0 {
		return nil
	}
	src, err := os.OpenRoot(tree)
	if err != nil {
		return err
	}
	defer src.Close()
	u := &untrackedSave{src: src, dir: filepath.Join(dir, saveFiles), budget: budget, read: m.readTree}
	if u.read == nil {
		u.read = fsx.ReadRegularIn
	}
	defer u.close()
	for i, f := range files {
		why, err := u.save(f)
		if err != nil {
			s.unsaved = files[i:]
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
	read   func(root *os.Root, name string, limit int64) ([]byte, error) // fsx.ReadRegularIn
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
// says why it does not ("" when it did): a file it cannot read is left out
// with the error, as a file past the cap is. The error is the save's own
// (place, the write, a read that fails for the whole process: cannotRead).
func (u *untrackedSave) save(f string) (string, error) {
	if strings.HasSuffix(f, "/") {
		return "a repository inside the work tree", nil
	}
	fi, err := u.src.Lstat(f)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "gone before the save", nil
	case err != nil:
		return cannotRead(err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return u.link(f)
	case !fi.Mode().IsRegular():
		return "not a regular file", nil
	case fi.Size() > u.budget:
		return sizeWord(fi.Size()) + ", past the cap", nil
	}
	b, err := u.read(u.src, f, u.budget)
	switch {
	case errors.Is(err, fsx.ErrTooLarge):
		return "grew past the cap", nil
	case errors.Is(err, fsx.ErrNotRegular):
		return "not a regular file", nil
	case errors.Is(err, fs.ErrNotExist):
		return "gone before the save", nil
	case err != nil:
		return cannotRead(err)
	}
	dst, err := u.place(f)
	if err != nil {
		return "", err
	}
	// The owner's bits (an executable script stays one when `cp -R` puts
	// it back), never the group's or others'.
	if err := writeSaved(dst, f, b, fi.Mode().Perm()&0o700|0o600); err != nil {
		return "", fmt.Errorf("copy into the save: %w", err)
	}
	u.budget -= int64(len(b))
	return "", nil
}

// place makes the save's untracked/ and the folder of f in it, and returns
// untracked/: an error there is the save's own (its disk full, a folder it
// cannot write), never a reason about f.
func (u *untrackedSave) place(f string) (*os.Root, error) {
	dst, err := u.folder()
	if err != nil {
		return nil, err
	}
	if d := path.Dir(f); d != "." {
		if err := dst.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("copy into the save: %w", err)
		}
	}
	return dst, nil
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
		return cannotRead(err)
	case int64(len(target)) > u.budget:
		return "a symlink to " + target + ", past the cap", nil
	}
	dst, err := u.place(f)
	if err != nil {
		return "", err
	}
	if err := dst.Symlink(target, f); err != nil {
		return "", fmt.Errorf("copy into the save: %w", err)
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
	return err.Error()
}

// cannotRead is save's answer to err, a read of a file of the tree that
// failed: the reason "cannot be read: <cause>", but for an error of the
// whole process (processWide), which fails the save.
func cannotRead(err error) (string, error) {
	if processWide(err) {
		return "", err
	}
	return "cannot be read: " + errText(err), nil
}

// processWide reports whether err is a failure of the whole process, not
// of the file it read: too many open files in the process or the system,
// or no memory. The file is readable, and the next attempt saves it.
func processWide(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOMEM)
}

// keptListed caps the untracked files saved.txt names as kept in the tree
// (git status names a wholly untracked folder once).
const keptListed = 1000

// The lines of saved.txt that start its two lists, each item "- <path>"
// on a line of its own up to the next empty line (savedLists reads them
// back): the untracked files the step keeps in the tree, and what the
// save left out.
const (
	keptHeading    = "Untracked, kept in the tree by "
	leftOutHeading = "Left out ("
)

// keptLines is how saved.txt lists the untracked files kept in the tree:
// the first keptListed, then how many more.
func keptLines(kept []string) []string {
	out := slices.Clone(kept[:min(len(kept), keptListed)])
	if more := len(kept) - keptListed; more > 0 {
		out = append(out, fmt.Sprintf("(+%d more)", more))
	}
	return out
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
		// Not -p: it would also set the tree's own folders to the save's 0700.
		fmt.Fprintf(&b, "- %d untracked file(s) under %s/, each with its owner's bits: `cp -R %s %s` puts them back\n",
			s.untracked, saveFiles, execx.ShellQuote(filepath.Join(s.dir, saveFiles)+"/."), quoted)
	}
	if s.failed != "" {
		fmt.Fprintf(&b, "\nThe save failed part way (%s), and the forced removal went on: it deleted, unsaved, all this save does not hold, named below as not saved.\n",
			s.failed)
	}
	if len(s.kept) > 0 {
		fmt.Fprintf(&b, "\n%s%s and not saved (%d):\n", keptHeading, what, len(s.kept))
		for _, p := range keptLines(s.kept) {
			b.WriteString("- " + p + "\n")
		}
	}
	if len(s.skipped) > 0 {
		fmt.Fprintf(&b, "\n%sa save keeps %s at most, only regular files and symlinks it can read, and never a file git ignores or magnum wrote):\n",
			leftOutHeading, sizeWord(limit))
		for _, p := range s.skipped {
			b.WriteString("- " + p + "\n")
		}
	}
	return fsx.WriteFileAtomic(filepath.Join(s.dir, saveNote), []byte(b.String()), 0o600)
}
