package factory

// What a local round leaves in the task's copy. A round's roles work in the
// copy itself, and its result is read only on a copy as the round found it
// (roundLeft): the task's branch at the head the round reviewed, no tracked
// change and no untracked file the copy did not have before the round. What
// a round leaves on that branch at that head (a reviewer's `bundle check`
// adds a line to Gemfile.lock, a reviewer's probe file) the factory saves in
// the task's directory and then puts back, but only while nobody else could
// have made it (putBackRound): nobody at work in the copy now, and no prompt
// to an agent of the task and no input of yours in its panes since the round
// began (othersIn). Anything else needs you, with why and the steps that put
// the copy back (leftWait); a copy off the branch or at another head is
// always yours: the factory never moves a branch or HEAD here. No step here
// discards a change it did not save first.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

// leftFileLimit caps a new file a round left that the factory saves: a
// larger one is yours to look at.
const leftFileLimit = 64 << 20

// roundLeftovers is what a round left in a task's copy that keeps its
// result from being read (roundLeft): tracked changes, the untracked paths
// the listing taken before the round lacks (git status's: a wholly new
// directory once, ending in /), and moved, the copy off the task's branch
// or the branch at another head than the round reviewed ("" for neither).
type roundLeftovers struct {
	tracked, added []string
	moved          string
}

// none reports whether the round left the copy as it found it.
func (l roundLeftovers) none() bool { return len(l.tracked) == 0 && len(l.added) == 0 && l.moved == "" }

// String names what the round left, "" for nothing.
func (l roundLeftovers) String() string {
	var parts []string
	if len(l.tracked) > 0 {
		parts = append(parts, fmt.Sprintf("%s in the copy (%s)", plural(len(l.tracked), "tracked change"), clipList(l.tracked)))
	}
	if l.moved != "" {
		parts = append(parts, l.moved)
	}
	if len(l.added) > 0 {
		parts = append(parts, fmt.Sprintf("%s in the copy (%s)", plural(len(l.added), "new untracked file"), clipList(l.added)))
	}
	return strings.Join(parts, " and ")
}

// putBackRound puts back what round `round` of the loop (cycle cy) left in
// task t's copy c (left), and says why the task needs you instead ("" when
// the copy is back as the round found it). Only tracked changes and new
// untracked files on the task's branch at the head the round reviewed go
// back, and only while nobody else could have made them: nobody at work in
// the copy and nobody in the task's panes since the cycle began (othersIn;
// a cycle with no start fails the guard). Each is saved first in the task's
// directory (saveLeft: round-<n>-left.patch, round-<n>-left/), then the
// tracked paths go back as HEAD has them and the new files are removed
// (putBackLeft), with begin, ok and fail events that name the counts and the
// saved files, and the copy is looked at again (roundLeft): back as the
// round found it, an info event says so. A save that fails puts nothing
// back. It runs whatever ended the pass (ctx's end does not stop it).
func (e *Engine) putBackRound(ctx context.Context, t registry.Task, c Copy, cy registry.Cycle, round int, left roundLeftovers) (string,
	error) {
	ctx = context.WithoutCancel(ctx)
	if left.moved != "" {
		return e.leftWait(ctx, t, c, cy, round, left, "")
	}
	files, err := e.newFiles(ctx, c, left)
	if err != nil {
		return "", err
	}
	who := "the factory does not know when the round began, so it cannot tell the round's changes from others'"
	if !cy.Started.IsZero() {
		who = e.othersIn(ctx, t, cy.Started)
	}
	if who != "" {
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("left what round %d left in %s as it is: %s", round, c.Name, who))
		return e.leftWait(ctx, t, c, cy, round, left, "the factory put none of it back: "+who)
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	name := leftName(dir, cy.N)
	saved := savedAs(name, left.tracked, files)
	var counts []string
	if len(left.tracked) > 0 {
		counts = append(counts, plural(len(left.tracked), "tracked change"))
	}
	if len(files) > 0 {
		counts = append(counts, plural(len(files), "new file"))
	}
	e.event(ctx, t.ID, "review", registry.PhaseBegin, fmt.Sprintf("puts back what round %d left in %s (%s), saved to %s first", round, c.Name,
		strings.Join(counts, ", "), saved))
	if err := saveLeft(ctx, e.d.Git, c.Path, dir, name, left.tracked, files); err != nil {
		line := clipEnd(textx.FirstLine(execx.Redact(err.Error())), 240)
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("save what round %d left in %s: %s", round, c.Name, line))
		return e.leftWait(ctx, t, c, cy, round, left, "the factory could not save it, so it put none of it back: "+line)
	}
	if err := putBackLeft(ctx, e.d.Git, c.Path, left.tracked, files, left.added); err != nil {
		line := clipEnd(textx.FirstLine(execx.Redact(err.Error())), 240)
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("put back what round %d left in %s: %s", round, c.Name, line))
		if now, lerr := e.roundLeft(ctx, t, c, cy.HeadBefore, cy.N); lerr == nil && !now.none() {
			left = now
		}
		return e.leftWait(ctx, t, c, cy, round, left, "the factory saved it to "+saved+" but could not put it back: "+line)
	}
	now, err := e.roundLeft(ctx, t, c, cy.HeadBefore, cy.N)
	if err != nil {
		return "", err
	}
	if !now.none() {
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("after the put-back of what round %d left, %s still has %s", round,
			c.Name, now))
		return e.leftWait(ctx, t, c, cy, round, now, "the factory saved what it found to "+saved+" and put it back, and the copy still has this")
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("put back what round %d left in %s as the round found it, saved to %s", round,
		c.Name, saved))
	e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("round %d of the local review left %s; saved to %s, "+
		"the copy is back as the round found it", round, left, saved))
	return "", nil
}

// newFiles are the new untracked files of left one by one, a new
// directory's each (gitx.UntrackedFiles); none without new paths.
func (e *Engine) newFiles(ctx context.Context, c Copy, left roundLeftovers) ([]string, error) {
	if len(left.added) == 0 {
		return nil, nil
	}
	return e.d.Git.UntrackedFiles(ctx, c.Path, left.added...)
}

// leftWait is the wait for you over what round `round` (cycle cy) left in
// task t's copy c (left) that the factory did not put back, and why not:
// the steps that put the copy back (the tracked paths restored as HEAD has
// them, the new files removed), then the retry. A copy off the task's branch
// or at another head is yours to put back as the round found it.
func (e *Engine) leftWait(ctx context.Context, t registry.Task, c Copy, cy registry.Cycle, round int, left roundLeftovers, why string) (string,
	error) {
	what := fmt.Sprintf("round %d of the local review left %s", round, left)
	if left.moved != "" {
		return fmt.Sprintf("%s: the factory changes none of it; put the copy back as the round found it (%s at %s, no tracked change, "+
			"no new untracked file), then let the task go on", what, t.Branch, short(cy.HeadBefore)), nil
	}
	files, err := e.newFiles(ctx, c, left)
	if err != nil {
		return "", err
	}
	var steps []string
	if len(left.tracked) > 0 {
		steps = append(steps, "`git -C "+execx.ShellQuote(c.Path)+" restore --staged --worktree -- "+shellArgs("", left.tracked)+"`")
	}
	if len(files) > 0 {
		steps = append(steps, "`rm -- "+shellArgs(c.Path, files)+"`")
	}
	return fmt.Sprintf("%s, and %s. Put the copy back as the round found it (%s at %s): %s, then `magnum factory retry %d`",
		what, why, t.Branch, short(cy.HeadBefore), strings.Join(steps, " and "), t.ID), nil
}

// shellArgs is paths under dir ("" for as they are), each quoted for a
// shell, joined by spaces.
func shellArgs(dir string, paths []string) string {
	out := make([]string, len(paths))
	for i, p := range paths {
		if dir != "" {
			p = filepath.Join(dir, filepath.FromSlash(p))
		}
		out[i] = execx.ShellQuote(p)
	}
	return strings.Join(out, " ")
}

// leftName is the free name in dir of what round n left:
// round-<n>-left, else round-<n>-left-<k> (the round ran again in its cycle
// and left something again).
func leftName(dir string, n int) string {
	name := fmt.Sprintf("round-%d-left", n)
	for k := 2; fsx.Exists(filepath.Join(dir, name+".patch")) || fsx.Exists(filepath.Join(dir, name)); k++ {
		name = fmt.Sprintf("round-%d-left-%d", n, k)
	}
	return name
}

// savedAs names what saveLeft writes under name, relative to the task's
// directory: the patch of the tracked changes, the directory of the new
// files.
func savedAs(name string, tracked, files []string) string {
	var out []string
	if len(tracked) > 0 {
		out = append(out, name+".patch")
	}
	if len(files) > 0 {
		out = append(out, name+"/")
	}
	return strings.Join(out, " and ")
}

// saveLeft saves what a round left in the copy at copyDir before it goes
// back, in the task's directory dir: the tracked paths' changes as one
// patch against HEAD (gitx.DiffHead) in <name>.patch, and each new
// untracked file (files), a regular file only, at its path under <name>/,
// read through an os.Root of the copy. Every write is atomic. A save that
// fails removes what it wrote: the copy still has all of it.
func saveLeft(ctx context.Context, git *gitx.Client, copyDir, dir, name string, tracked, files []string) (err error) {
	defer func() {
		if err != nil {
			_ = os.Remove(filepath.Join(dir, name+".patch"))
			_ = os.RemoveAll(filepath.Join(dir, name))
		}
	}()
	if len(tracked) > 0 {
		patch, err := git.DiffHead(ctx, copyDir, tracked...)
		if err != nil {
			return err
		}
		if err := fsx.WriteFileAtomic(filepath.Join(dir, name+".patch"), patch, 0o600); err != nil {
			return err
		}
	}
	if len(files) == 0 {
		return nil
	}
	src, err := os.OpenRoot(copyDir)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
		return err
	}
	dst, err := os.OpenRoot(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer dst.Close()
	for _, f := range files {
		b, err := readRegularIn(src, f, leftFileLimit)
		if err != nil {
			return err
		}
		if d := path.Dir(f); d != "." {
			if err := dst.MkdirAll(d, 0o700); err != nil {
				return err
			}
		}
		if err := fsx.WriteFileAtomicIn(dst, f, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// readRegularIn reads the regular file name in root, at most limit bytes:
// a symlink, a directory or a special file is refused and never followed,
// and so is a file put in its place since it was looked at.
func readRegularIn(root *os.Root, name string, limit int64) ([]byte, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	notRegular := func(what string) error {
		return fmt.Errorf("%s is not a regular file (%s), and the factory saves only those", name, what)
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, notRegular("a symlink")
	case fi.IsDir():
		return nil, notRegular("a directory")
	case !fi.Mode().IsRegular():
		return nil, notRegular("a special file")
	case fi.Size() > limit:
		return nil, fmt.Errorf("%s holds over %d bytes", name, limit)
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !os.SameFile(fi, st) {
		return nil, cmp.Or(err, notRegular("replaced since the factory looked at it"))
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s holds over %d bytes", name, limit)
	}
	return b, nil
}

// putBackLeft puts the copy at copyDir back as a round found it: the
// tracked paths as HEAD has them (gitx.RestoreTracked), and the new
// untracked files removed through an os.Root of the copy, with each
// directory of a new one (entries: git status's paths, a new directory
// ending in /) that this empties. A directory that still holds anything (a
// file git ignores) stays.
func putBackLeft(ctx context.Context, git *gitx.Client, copyDir string, tracked, files, entries []string) error {
	if len(tracked) > 0 {
		if err := git.RestoreTracked(ctx, copyDir, tracked...); err != nil {
			return err
		}
	}
	if len(files) == 0 {
		return nil
	}
	root, err := os.OpenRoot(copyDir)
	if err != nil {
		return err
	}
	defer root.Close()
	inNew := func(d string) bool {
		return slices.ContainsFunc(entries, func(e string) bool { return strings.HasSuffix(e, "/") && strings.HasPrefix(d+"/", e) })
	}
	dirs := map[string]bool{}
	for _, f := range files {
		if err := root.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for d := path.Dir(f); d != "." && inNew(d); d = path.Dir(d) {
			dirs[d] = true
		}
	}
	// Deepest first, so a directory's emptied ones go before it.
	for _, d := range slices.SortedFunc(maps.Keys(dirs), func(a, b string) int {
		return cmp.Or(cmp.Compare(strings.Count(b, "/"), strings.Count(a, "/")), cmp.Compare(a, b))
	}) {
		_ = root.Remove(d) // one that is not empty stays
	}
	return nil
}
