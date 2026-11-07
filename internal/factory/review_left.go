package factory

// What a local round leaves in the task's copy. A round's roles work in the
// copy itself, and its result is read only on a copy as the round found it
// (roundLeft): the task's branch at the head the round reviewed, no tracked
// change and no untracked file the copy did not have before the round. What
// a round leaves on that branch at that head (a reviewer's `bundle check`
// adds a line to Gemfile.lock, a reviewer's probe file) the factory saves in
// the task's directory and then puts back, but only while nobody else could
// have made it (putBackRound): nobody at work in the copy now, and no prompt
// to an agent of the task and no input of yours in its panes since the
// round's cycle began (othersIn), asked before the save and again before the
// put-back. A round stopped before its end has the same done before it runs
// again in its cycle (startRound). Anything else needs you, with why and the
// steps that put the copy back (leftWait); a copy off the branch or at
// another head is always yours: the factory never moves a branch or HEAD
// here. No step here discards a change it did not save first.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

// The bounds of a save of what a round left (saveLeft): a new file over
// leftFileLimit, more than leftMaxFiles new files or more than leftMaxBytes
// of them are yours to look at.
const (
	leftFileLimit = 64 << 20
	leftMaxFiles  = 1000
	leftMaxBytes  = 256 << 20
)

// leftStepPaths is how many paths a step of the wait for you names (leftWait).
const leftStepPaths = 10

// roundLeftovers is what a round left in a task's copy that keeps its
// result from being read (roundLeft): tracked changes, the untracked files,
// one by one, that the listing taken before the round lacks, and moved, the
// copy off the task's branch or the branch at another head than the round
// reviewed ("" for neither).
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

// roundName names round `round` of the loop, cycle n of the task, as the
// local review's texts do: "round 1", and its cycle too when the two differ
// (a later loop), as the saved files carry the cycle's number.
func roundName(round, n int) string {
	if round == n {
		return fmt.Sprintf("round %d", round)
	}
	return fmt.Sprintf("round %d (cycle %d)", round, n)
}

// putBackRound puts back what round `round` of the loop (cycle cy) left in
// task t's copy c (left), and says why the task needs you instead ("" when
// the copy is back as the round found it). Only tracked changes and new
// untracked files on the task's branch at the head the round reviewed go
// back, and only while nobody else could have made them: nobody at work in
// the copy and nobody in the task's panes since the cycle began (othersIn;
// a cycle with no start fails the guard), asked before the save and again
// before the put-back. Each is saved first in the task's directory
// (saveLeft: round-<n>-left.patch, round-<n>-left/), then the tracked paths
// go back as HEAD has them and the new files are removed (putBackLeft), with
// begin, ok and fail events that name the counts and the saved files, and
// the copy is looked at again (roundLeft): back as the round found it,
// whatever git answered, an info event says so. A save that fails, or a
// guard that fails after it, puts nothing back and keeps no save. It runs
// whatever ended the pass (ctx's end does not stop it).
func (e *Engine) putBackRound(ctx context.Context, t registry.Task, c Copy, cy registry.Cycle, round int, left roundLeftovers) (string,
	error) {
	ctx = context.WithoutCancel(ctx)
	if left.moved != "" {
		return e.leftWait(ctx, t, c, cy, round, left, "")
	}
	name := roundName(round, cy.N)
	guard := func() string {
		if cy.Started.IsZero() {
			return "the factory does not know when the round began, so it cannot tell the round's changes from others'"
		}
		return e.othersIn(ctx, t, cy.Started)
	}
	if who := guard(); who != "" {
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("left what %s left in %s as it is: %s", name, c.Name, who))
		return e.leftWait(ctx, t, c, cy, round, left, "the factory put none of it back: "+who)
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	base := leftName(dir, cy.N)
	saved := savedAs(base, left)
	var counts []string
	if len(left.tracked) > 0 {
		counts = append(counts, plural(len(left.tracked), "tracked change"))
	}
	if len(left.added) > 0 {
		counts = append(counts, plural(len(left.added), "new file"))
	}
	e.event(ctx, t.ID, "review", registry.PhaseBegin, fmt.Sprintf("puts back what %s left in %s (%s), saved to %s first", name, c.Name,
		strings.Join(counts, ", "), saved))
	undo, err := saveLeft(ctx, e.d.Git, c.Path, dir, base, left)
	if err != nil {
		line := clipEnd(textx.FirstLine(execx.Redact(err.Error())), 240)
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("save what %s left in %s: %s", name, c.Name, line))
		return e.leftWait(ctx, t, c, cy, round, left, "the factory could not save it, so it put none of it back: "+line)
	}
	if who := guard(); who != "" {
		undo() // the copy still has all of it
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("left what %s left in %s as it is: %s", name, c.Name, who))
		return e.leftWait(ctx, t, c, cy, round, left, "the factory put none of it back: "+who)
	}
	perr := putBackLeft(ctx, e.d.Git, c.Path, left)
	line := ""
	if perr != nil {
		line = clipEnd(textx.FirstLine(execx.Redact(perr.Error())), 240)
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("put back what %s left in %s: %s", name, c.Name, line))
	}
	now, err := e.roundLeft(ctx, t, c, cy.HeadBefore, cy.N)
	switch {
	case err != nil && perr != nil:
		return e.leftWait(ctx, t, c, cy, round, left, "the factory saved it to "+saved+" but could not put it back: "+line)
	case err != nil:
		return "", err
	case !now.none() && perr != nil:
		return e.leftWait(ctx, t, c, cy, round, now, "the factory saved it to "+saved+" but could not put it back: "+line)
	case !now.none():
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("after the put-back of what %s left, %s still has %s", name, c.Name, now))
		return e.leftWait(ctx, t, c, cy, round, now, "the factory saved what it found to "+saved+" and put it back, and the copy still has this")
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("put back what %s left in %s as the round found it, saved to %s", name,
		c.Name, saved))
	e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("%s of the local review left %s; saved to %s, "+
		"the copy is back as the round found it", name, left, saved))
	return "", nil
}

// leftWait is the wait for you over what round `round` (cycle cy) left in
// task t's copy c (left) that the factory did not put back, and why not:
// the steps that put the copy back (the tracked paths restored as HEAD has
// them, every tracked change of the copy past leftStepPaths, and the new
// files removed), then the retry. A step names at most leftStepPaths new
// files and how many more; an event of the review then names them all. A
// copy off the task's branch or at another head is yours to put back as the
// round found it.
func (e *Engine) leftWait(ctx context.Context, t registry.Task, c Copy, cy registry.Cycle, round int, left roundLeftovers, why string) (string,
	error) {
	if left.moved != "" {
		return fmt.Sprintf("round %d of the local review left %s: the factory changes none of it; put the copy back as the round found it "+
			"(%s at %s, no tracked change, no new untracked file), then let the task go on", round, left, t.Branch, short(cy.HeadBefore)), nil
	}
	var steps []string
	if n := len(left.tracked); n > 0 {
		paths := shellArgs("", left.tracked)
		if n > leftStepPaths {
			paths = ":/" // every tracked change in the copy is one the round left
		}
		steps = append(steps, "`git -C "+execx.ShellQuote(c.Path)+" restore --staged --worktree -- "+paths+"`")
	}
	if n := len(left.added); n > 0 {
		rm := "rm --"
		if slices.ContainsFunc(left.added, func(p string) bool { return strings.HasSuffix(p, "/") }) {
			rm = "rm -r --" // a repository inside the copy
		}
		step := "`" + rm + " " + shellArgs(c.Path, left.added[:min(n, leftStepPaths)]) + "`"
		if n > leftStepPaths {
			step += fmt.Sprintf(" and %d more (the review's events name them all)", n-leftStepPaths)
			e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("the %s %s left in %s: %s", plural(n, "new untracked file"),
				roundName(round, cy.N), c.Name, strings.Join(left.added, ", ")))
		}
		steps = append(steps, step)
	}
	return fmt.Sprintf("%s of the local review left %s, and %s. Put the copy back as the round found it (%s at %s): %s, then "+
		"`magnum factory retry %d`", roundName(round, cy.N), left, why, t.Branch, short(cy.HeadBefore), strings.Join(steps, " and "), t.ID), nil
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

// leftName is the free name in dir of what cycle n's round left:
// round-<n>-left, else round-<n>-left-<k> (the round ran again in its cycle
// and left something again, or a save was not put back). A name is taken
// by anything there, a dangling symlink too, or by what cannot be looked at.
func leftName(dir string, n int) string {
	taken := func(p string) bool {
		_, err := os.Lstat(p)
		return !errors.Is(err, fs.ErrNotExist)
	}
	name := fmt.Sprintf("round-%d-left", n)
	for k := 2; taken(filepath.Join(dir, name+".patch")) || taken(filepath.Join(dir, name)); k++ {
		name = fmt.Sprintf("round-%d-left-%d", n, k)
	}
	return name
}

// savedAs names what saveLeft writes under base, relative to the task's
// directory: the patch of the tracked changes, the directory of the new
// files.
func savedAs(base string, left roundLeftovers) string {
	var out []string
	if len(left.tracked) > 0 {
		out = append(out, base+".patch")
	}
	if len(left.added) > 0 {
		out = append(out, base+"/")
	}
	return strings.Join(out, " and ")
}

// saveLeft saves what a round left in the copy at copyDir (left) before it
// goes back, in the task's directory dir: the tracked paths' changes as one
// patch against HEAD (gitx.DiffHead) in <base>.patch, and each new untracked
// file, a regular file only (fsx.ReadRegularIn: read through an os.Root of
// the copy), at its path under <base>/, within leftFileLimit,
// leftMaxFiles and leftMaxBytes. Every write is atomic. undo removes what it
// wrote, and a save that fails removes it itself: the copy still has all of
// it.
func saveLeft(ctx context.Context, git *gitx.Client, copyDir, dir, base string, left roundLeftovers) (undo func(), err error) {
	var madePatch, madeDir bool
	remove := func() {
		if madePatch {
			_ = os.Remove(filepath.Join(dir, base+".patch"))
		}
		if madeDir {
			_ = os.RemoveAll(filepath.Join(dir, base))
		}
	}
	defer func() {
		if err != nil {
			remove()
		}
	}()
	if len(left.added) > leftMaxFiles {
		return nil, fmt.Errorf("%d new files, more than the %d the factory saves", len(left.added), leftMaxFiles)
	}
	if len(left.tracked) > 0 {
		patch, err := git.DiffHead(ctx, copyDir, left.tracked...)
		if err != nil {
			return nil, err
		}
		if err := fsx.WriteFileAtomic(filepath.Join(dir, base+".patch"), patch, 0o600); err != nil {
			return nil, err
		}
		madePatch = true
	}
	if len(left.added) == 0 {
		return remove, nil
	}
	src, err := os.OpenRoot(copyDir)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if err := os.Mkdir(filepath.Join(dir, base), 0o700); err != nil {
		return nil, err
	}
	madeDir = true
	dst, err := os.OpenRoot(filepath.Join(dir, base))
	if err != nil {
		return nil, err
	}
	defer dst.Close()
	var total int64
	for _, f := range left.added {
		limit := min(leftFileLimit, leftMaxBytes-total)
		b, err := fsx.ReadRegularIn(src, f, limit)
		switch {
		case errors.Is(err, fsx.ErrTooLarge) && limit < leftFileLimit:
			return nil, fmt.Errorf("the new files hold more than the %d bytes the factory saves", leftMaxBytes)
		case err != nil:
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		total += int64(len(b))
		if d := path.Dir(f); d != "." {
			if err := dst.MkdirAll(d, 0o700); err != nil {
				return nil, err
			}
		}
		if err := fsx.WriteFileAtomicIn(dst, f, b, 0o600); err != nil {
			return nil, err
		}
	}
	return remove, nil
}

// putBackLeft puts the copy at copyDir back as a round found it (left): the
// tracked paths as HEAD has them (gitx.RestoreTracked), then the new
// untracked files removed through an os.Root of the copy, with each of their
// directories this empties, deepest first. A directory that still holds
// anything (a file git ignores, a tracked file) stays.
func putBackLeft(ctx context.Context, git *gitx.Client, copyDir string, left roundLeftovers) error {
	if len(left.tracked) > 0 {
		if err := git.RestoreTracked(ctx, copyDir, left.tracked...); err != nil {
			return err
		}
	}
	if len(left.added) == 0 {
		return nil
	}
	root, err := os.OpenRoot(copyDir)
	if err != nil {
		return err
	}
	defer root.Close()
	dirs := map[string]bool{}
	for _, f := range left.added {
		if err := root.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for d := path.Dir(f); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for _, d := range slices.SortedFunc(maps.Keys(dirs), func(a, b string) int {
		return cmp.Or(cmp.Compare(strings.Count(b, "/"), strings.Count(a, "/")), cmp.Compare(a, b))
	}) {
		_ = root.Remove(d) // one that is not empty stays
	}
	return nil
}
