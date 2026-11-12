package factory

// What a local round leaves in the task's copy. A round's roles work in the
// copy itself, and its result is read only on a copy as the round found it
// (roundLeft): the task's branch at the head the round reviewed, no tracked
// change and no untracked file the copy did not have before the round. What
// a round leaves there (a reviewer's `bundle check` adds a line to
// Gemfile.lock, a reviewer's probe file) the factory saves in the task's
// directory (review-<n>-left.diff, review-<n>-left/) and puts back by
// itself only when nothing leaves a doubt that it is the round's alone
// (leftDoubt), an event and an alert saying so, and the round's result
// stands (putBackRound). Any doubt (the copy moved, a turn in flight, you
// in a pane or at work since the round began, a runtime that cannot tell)
// asks you one question instead (askLeft): you put the copy back yourself
// and the task goes on, or it stops. The factory never moves a branch,
// never puts back under a doubt and never runs an answer over a turn. A
// round stopped before its end has the same done before it runs again in
// its cycle (startRound). A save or a put-back that fails needs you, with
// why and the steps that put the copy back (leftWait). No step here
// discards a change it did not save first.

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
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/scrub"
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

// leftDiffExt ends the name of the diff of the tracked changes a round
// left, saved in the task's directory (review-<n>-left.diff).
const leftDiffExt = ".diff"

// roundLeftovers is what a round left in a task's copy that keeps its
// result from being read (roundLeft): tracked changes, the untracked files,
// one by one, that the listing taken before the round lacks, and moved, the
// copy off the task's branch or the branch at another head than the round
// reviewed ("" for neither). Of the tracked changes, unstage are the paths
// HEAD lacks, which the round staged: a file of yours from before the round
// may be among them (listed, or ignored), so only the index goes back for
// them, and the file goes only when it is proven the round's (putBackLeft);
// unindexed are the files the round took out of the index and left in the
// work tree (git's patch of them is a deletion: their files are saved too).
// keepStaged are the paths of unstage the work tree changed or deleted
// since the round staged them: the index holds the only copy of what was
// staged (git's patch has the work tree's), so they stay staged. before is
// the listing taken before the cycle's first run, nil without one (nothing
// then tells a new file from yours, and no file is removed).
type roundLeftovers struct {
	tracked, added     []string
	unstage, unindexed []string
	keepStaged         []string
	moved              string
	before             *beforeListing
}

// unstaged are the paths of unstage whose index goes back: all but
// keepStaged.
func (l roundLeftovers) unstaged() []string {
	return slices.DeleteFunc(slices.Clone(l.unstage), func(p string) bool {
		_, keep := slices.BinarySearch(l.keepStaged, p)
		return keep
	})
}

// staged are the paths of left.unstage the listing before the round does
// not name: files the round staged that may be its own, or files of yours
// git ignores (all of them without a listing).
func (l roundLeftovers) staged() []string {
	return slices.DeleteFunc(slices.Clone(l.unstage), func(p string) bool { return l.before != nil && l.before.names(p) })
}

// fullRestore are the tracked paths that go back as HEAD has them, in the
// index and the work tree: all but unstage.
func (l roundLeftovers) fullRestore() []string {
	return slices.DeleteFunc(slices.Clone(l.tracked), func(p string) bool {
		_, found := slices.BinarySearch(l.unstage, p)
		return found
	})
}

// ignoreFile reports whether path p is a git ignore file, whose change shows
// or hides other files.
func ignoreFile(p string) bool { return path.Base(p) == ".gitignore" }

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
// the copy is back as the round found it). The factory puts it back by
// itself only when nothing leaves a doubt that it is the round's alone
// (leftDoubt): the copy on the task's branch at the head the round
// reviewed, no agent's turn in flight in the copy and you in none of its
// panes (Runtime.AtWork answers, naming nobody), and no prompt to one of
// its agents and no input of yours in its panes since the round began
// (Runtime.TouchedSince answers "no"), asked before the save and again
// before the put-back. It saves the tracked changes and the new files first
// in the task's directory (saveLeft: review-<n>-left.diff, mode 0600, and
// review-<n>-left/), then puts the tracked paths back as HEAD has them (the
// index only, for a file from before the round) and removes the new files
// (putBackLeft), with begin, ok and fail events that name the counts and
// the saved files, and looks at the copy again (roundLeft): back as the
// round found it, whatever git answered, an info event and an alert say
// so, and the round's result stands.
//
// In every other case, a "cannot tell" too, it saves the same and asks you
// one question (askLeft): you put the copy back yourself and the task goes
// on, or it stops. The factory never moves a branch, never puts anything
// back under a doubt, and never runs an answer over a turn: your "go on"
// only makes it look at the copy again, and a copy that still has what
// the round left asks again. A save or a put-back that fails needs you,
// with why and the steps that put the copy back (leftWait). It runs
// whatever ended the pass (ctx's end does not stop it).
func (e *Engine) putBackRound(ctx context.Context, t registry.Task, c Copy, cy registry.Cycle, round int, left roundLeftovers) (string,
	error) {
	ctx = context.WithoutCancel(ctx)
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	stops, err := readStops(dir)
	if err != nil {
		return "", err
	}
	if waits, goOns := stops.leftAsked(cy, round); waits || goOns > 0 { // asked already: never by itself after a question
		return e.askLeft(ctx, t, c, cy, round, left, "", stops, goOns)
	}
	if doubt := e.leftDoubt(ctx, t, cy, left); doubt != "" {
		return e.askLeft(ctx, t, c, cy, round, left, doubt, stops, 0)
	}
	name := roundName(round, cy.N)
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
		line := clipEnd(textx.FirstLine(scrub.Text(err.Error())), 240)
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("save what %s left in %s: %s", name, c.Name, line))
		return e.leftWait(t, c, cy, round, left, "", "the factory could not save it, so it put none of it back: "+line)
	}
	if doubt := e.leftDoubt(ctx, t, cy, left); doubt != "" {
		undo() // the question saves it again, under the next free name
		return e.askLeft(ctx, t, c, cy, round, left, doubt, stops, 0)
	}
	perr := putBackLeft(ctx, e.d.Git, c.Path, left, cy.Started)
	line := ""
	if perr != nil {
		line = clipEnd(textx.FirstLine(scrub.Text(perr.Error())), 240)
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("put back what %s left in %s: %s", name, c.Name, line))
	}
	now, err := e.roundLeft(ctx, t, c, cy.HeadBefore, cy.N)
	switch {
	case err != nil && perr != nil:
		return e.leftWait(t, c, cy, round, left, base, "the factory saved it to "+saved+" but could not put it back: "+line)
	case err != nil:
		return "", err
	case !now.none() && perr != nil:
		return e.leftWait(t, c, cy, round, now, base, "the factory saved it to "+saved+" but could not put it back: "+line)
	case !now.none():
		e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("after the put-back of what %s left, %s still has %s", name, c.Name, now))
		return e.leftWait(t, c, cy, round, now, base, "the factory saved what it found to "+saved+" and put it back, and the copy still has this")
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("put back what %s left in %s as the round found it, saved to %s", name,
		c.Name, saved))
	e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("%s of the local review left %s; saved to %s, "+
		"the copy is back as the round found it", name, left, saved))
	e.alert(ctx, t.ID, fmt.Sprintf("%s: %swhat %s of the local review left in the copy (%s), saved to %s; the round's result stands",
		TaskKey(t), putBackAlert, name, left, saved))
	return "", nil
}

// leftDoubt is why what a round of cycle cy left in task t's copy (left)
// may not be the round's alone, so the factory puts none of it back by
// itself ("" for no doubt): the copy off the task's branch or at another
// head, an agent's turn in flight in the copy or you in one of its panes
// (Runtime.AtWork), a prompt to one of its agents or your input in its
// panes since the round began (Runtime.TouchedSince), or a runtime that
// cannot tell either, or a cycle with no start.
func (e *Engine) leftDoubt(ctx context.Context, t registry.Task, cy registry.Cycle, left roundLeftovers) string {
	switch {
	case left.moved != "":
		return "the factory never moves a branch or HEAD"
	case e.d.Runtime == nil:
		return ""
	case cy.Started.IsZero():
		return "the factory does not know when the round began"
	}
	role, you, err := e.d.Runtime.AtWork(ctx, t)
	switch {
	case err != nil:
		return "the factory cannot tell who works in the copy (" + textx.Clip(textx.FirstLine(err.Error()), 200) + ")"
	case role != "" && you:
		return "you are in the " + role + " pane"
	case role != "":
		return "the " + role + " agent works in the copy"
	}
	who, err := e.d.Runtime.TouchedSince(ctx, t, cy.Started)
	switch {
	case err != nil:
		return "the factory cannot tell who changed it since the round began (" + textx.Clip(textx.FirstLine(err.Error()), 200) + ")"
	case who != "":
		return who + " since the round began"
	}
	return ""
}

// askLeft asks you about what round `round` of cycle cy left in task t's
// copy c (left), which the factory put none of back because of doubt
// (leftDoubt): it saves it first (saveLeft) and asks one question
// (leftQuestion), you put the copy back yourself and the task goes on, or
// it stops, naming the saved diff, the paths, the reviewed head and, for a
// copy that moved, the branch's head now; the task waits on you, a held
// pass. A question that waits for your answer already waits on, nothing
// saved again. After goOns answers "go on" that found the copy still not
// as the round found it, the question is asked again. A save that fails
// needs you with the steps, as before (leftWait).
func (e *Engine) askLeft(ctx context.Context, t registry.Task, c Copy, cy registry.Cycle, round int, left roundLeftovers, doubt string,
	stops stopsRecord, goOns int) (string, error) {
	key := leftKey(cy, round)
	if stops.pending(stopLeft, key) >= 0 { // asked: askStop keeps its text, nothing saved again
		q := leftQuestion(t, cy, round, left, leftAsk{doubt: doubt, goOns: goOns})
		if err := e.askStop(ctx, t, q); err != nil {
			return "", err
		}
		return "", heldError{who: waitsYou, what: q.ask.Why}
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	base, saved := leftName(dir, cy.N), ""
	if len(left.tracked)+len(left.added)+len(left.unindexed) > 0 {
		if _, err := saveLeft(ctx, e.d.Git, c.Path, dir, base, left); err != nil {
			line := clipEnd(textx.FirstLine(scrub.Text(err.Error())), 240)
			return e.leftWait(t, c, cy, round, left, "", "the factory could not save it, so it put none of it back: "+line)
		}
		saved = savedAs(base, left)
	}
	branchAt := ""
	if left.moved != "" {
		if at, err := e.d.Git.RevParse(ctx, c.Path, "refs/heads/"+t.Branch); err == nil {
			branchAt = at
		}
	}
	diff, _ := os.ReadFile(filepath.Join(dir, base+leftDiffExt))
	q := leftQuestion(t, cy, round, left, leftAsk{doubt: doubt, saved: saved, diff: scrub.Text(string(diff)), branchAt: branchAt,
		steps: e.leftSteps(t, c, cy, left, base), goOns: goOns})
	if err := e.askStop(ctx, t, q); err != nil {
		return "", err
	}
	return "", heldError{who: waitsYou, what: q.ask.Why}
}

// leftWait is the wait for you over what round `round` (cycle cy) left in
// task t's copy c (left) that the factory did not put back, and why not:
// the steps that put the copy back (the tracked paths restored as HEAD has
// them, the index only for a path HEAD lacks, and the new files removed),
// then the retry. No step drops from the index a version the work tree
// lacks (keepStaged), and the rm step names only the new files saved in
// the task's directory under saved ("" for nothing saved) that are not
// older than the round, and none after a round that changed an ignore file
// (git's listing of new files is then the round's rules'); a file the round
// staged is never in it (it may be yours, git may ignore it). The wait
// names what it leaves out. A step over more than leftStepPaths paths takes
// them from a file in the task's directory that lists exactly them
// (round-<n>-<step>.paths, NUL-separated: git's --pathspec-from-file, xargs
// -0); one that cannot be written names leftStepPaths of them and how many
// more. A copy off the task's branch or at another head is yours to put back
// as the round found it.
func (e *Engine) leftWait(t registry.Task, c Copy, cy registry.Cycle, round int, left roundLeftovers, saved, why string) (string, error) {
	if left.moved != "" {
		return fmt.Sprintf("%s of the local review left %s, and %s. Put the copy back as the round found it (%s at %s, no tracked "+
			"change, no new untracked file); then `magnum factory retry %d`", roundName(round, cy.N), left, why, t.Branch,
			short(cy.HeadBefore), t.ID), nil
	}
	return fmt.Sprintf("%s of the local review left %s, and %s. Put the copy back as the round found it (%s at %s): %s; then "+
		"`magnum factory retry %d`", roundName(round, cy.N), left, why, t.Branch, short(cy.HeadBefore), e.leftSteps(t, c, cy, left, saved),
		t.ID), nil
}

// leftSteps are the steps that put back what a round of cycle cy left in
// task t's copy c (left), the new files saved under saved in the task's
// directory ("" for nothing saved), as leftWait and the question
// (askLeft) give them; "" for a copy off the task's branch or at another
// head, which only you can put back.
func (e *Engine) leftSteps(t registry.Task, c Copy, cy registry.Cycle, left roundLeftovers, saved string) string {
	if left.moved != "" {
		return ""
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	quoted := execx.ShellQuote(c.Path)
	more := func(n int) string {
		return fmt.Sprintf(" and %d more (`git -C %s status` names them)", n-leftStepPaths, quoted)
	}
	var steps, notes []string
	restore := func(opts string, paths []string, list string) {
		cmd := "git -C " + quoted + " --literal-pathspecs restore " + opts
		switch file, err := pathsFile(dir, cy.N, list, paths); {
		case len(paths) == 0:
		case len(paths) <= leftStepPaths:
			steps = append(steps, "`"+cmd+" -- "+shellArgs("", paths)+"`")
		case err == nil:
			steps = append(steps, "`"+cmd+" --pathspec-from-file="+execx.ShellQuote(file)+" --pathspec-file-nul`")
		default:
			steps = append(steps, "`"+cmd+" -- "+shellArgs("", paths[:leftStepPaths])+"`"+more(len(paths)))
		}
	}
	restore("--staged --worktree", left.fullRestore(), "restore")
	if n := len(left.keepStaged); n > 0 {
		notes = append(notes, fmt.Sprintf("the round staged and then changed or deleted %s (%s): the index holds a version the work "+
			"tree does not, so they stay staged (`git -C %s restore --worktree -- <path>` writes that version back to the file, "+
			"`git -C %s restore --staged -- <path>` then drops it from the index)", plural(n, "file"), clipList(left.keepStaged), quoted, quoted))
	}
	unstage := left.unstaged()
	restore("--staged", unstage, "unstage")
	if staged := slices.DeleteFunc(left.staged(), func(p string) bool {
		_, keep := slices.BinarySearch(left.keepStaged, p)
		return keep
	}); len(staged) > 0 {
		notes = append(notes, fmt.Sprintf("the round staged %s the copy did not list before (%s): they stay in the copy once "+
			"their index goes back, as they may be yours (git may ignore them); remove those the round made", plural(len(staged), "file"),
			clipList(staged)))
	}
	switch {
	case len(left.added) == 0:
	case slices.ContainsFunc(slices.Concat(left.tracked, left.added), ignoreFile):
		notes = append(notes, fmt.Sprintf("the round changed the ignore rules, so the factory names no file to remove: once the "+
			"rest is back, `git -C %s ls-files --others --exclude-standard` lists the new files; remove those the round made", quoted))
	default:
		var abs, out []string
		for _, p := range left.added {
			fi, err := os.Lstat(filepath.Join(dir, saved, filepath.FromSlash(p)))
			mine, merr := os.Lstat(filepath.Join(c.Path, filepath.FromSlash(p)))
			if saved == "" || err != nil || !fi.Mode().IsRegular() || merr != nil || mine.ModTime().Before(cy.Started) {
				out = append(out, p)
				continue
			}
			abs = append(abs, filepath.Join(c.Path, filepath.FromSlash(p)))
		}
		if len(out) > 0 {
			notes = append(notes, fmt.Sprintf("no step removes the new files the factory did not save or that are older than the round "+
				"(%s), as they may be yours: remove those the round made", clipList(out)))
		}
		rm := "rm --"
		switch file, err := pathsFile(dir, cy.N, "remove", abs); {
		case len(abs) == 0:
		case len(abs) <= leftStepPaths:
			steps = append(steps, "`"+rm+" "+shellArgs("", abs)+"`")
		case err == nil:
			steps = append(steps, "`xargs -0 "+rm+" < "+execx.ShellQuote(file)+"`")
		default:
			steps = append(steps, "`"+rm+" "+shellArgs("", abs[:leftStepPaths])+"`"+more(len(abs)))
		}
	}
	what := strings.Join(slices.Concat([]string{strings.Join(steps, " and ")}, notes), "; ")
	return strings.TrimPrefix(what, "; ")
}

// pathsFile writes paths, NUL-separated, to round-<n>-<list>.paths in the
// task's directory dir when there are more than leftStepPaths of them (a
// step of the wait then reads them from it), and returns its path; "" for
// fewer.
func pathsFile(dir string, n int, list string, paths []string) (string, error) {
	if len(paths) <= leftStepPaths {
		return "", nil
	}
	file := filepath.Join(dir, fmt.Sprintf("round-%d-%s.paths", n, list))
	return file, fsx.WriteFileAtomic(file, []byte(strings.Join(paths, "\x00")+"\x00"), 0o600)
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
// review-<n>-left, else review-<n>-left-<k> (the round ran again in its
// cycle and left something again, or a save was not put back). A name is
// taken by anything there, a dangling symlink too, or by what cannot be
// looked at.
func leftName(dir string, n int) string {
	taken := func(p string) bool {
		_, err := os.Lstat(p)
		return !errors.Is(err, fs.ErrNotExist)
	}
	name := fmt.Sprintf("review-%d-left", n)
	for k := 2; taken(filepath.Join(dir, name+leftDiffExt)) || taken(filepath.Join(dir, name)); k++ {
		name = fmt.Sprintf("review-%d-left-%d", n, k)
	}
	return name
}

// savedAs names what saveLeft writes under base, relative to the task's
// directory: the patch of the tracked changes, the directory of the new
// files.
func savedAs(base string, left roundLeftovers) string {
	var out []string
	if len(left.tracked) > 0 {
		out = append(out, base+leftDiffExt)
	}
	if len(left.added)+len(left.unindexed) > 0 {
		out = append(out, base+"/")
	}
	return strings.Join(out, " and ")
}

// saveLeft saves what a round left in the copy at copyDir (left) before it
// goes back, in the task's directory dir: the tracked paths' changes as one
// patch against HEAD (gitx.DiffHead) in <base>.diff, and each new untracked
// file and each file the round took out of the index, a regular file only
// (fsx.ReadRegularIn: read through an os.Root of the copy), at its path
// under <base>/, within leftFileLimit,
// leftMaxFiles and leftMaxBytes. Every write is atomic. undo removes what it
// wrote, and a save that fails removes it itself: the copy still has all of
// it.
func saveLeft(ctx context.Context, git *gitx.Client, copyDir, dir, base string, left roundLeftovers) (undo func(), err error) {
	var madePatch, madeDir bool
	remove := func() {
		if madePatch {
			_ = os.Remove(filepath.Join(dir, base+leftDiffExt))
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
	files := slices.Concat(left.added, left.unindexed)
	if len(files) > leftMaxFiles {
		return nil, fmt.Errorf("%d new files, more than the %d the factory saves", len(files), leftMaxFiles)
	}
	if len(left.tracked) > 0 {
		patch, err := git.DiffHead(ctx, copyDir, left.tracked...)
		if err != nil {
			return nil, err
		}
		if err := fsx.WriteFileAtomic(filepath.Join(dir, base+leftDiffExt), patch, 0o600); err != nil {
			return nil, err
		}
		madePatch = true
	}
	if len(files) == 0 {
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
	for _, f := range files {
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

// putBackLeft puts the copy at copyDir back as a round found it (left):
//  1. the tracked paths HEAD has, as HEAD has them (gitx.RestoreTracked),
//     which brings back an ignore file the round changed;
//  2. the paths HEAD lacks out of the index only (gitx.RestoreIndex), their
//     files left in the work tree, but for keepStaged, whose staged version
//     only the index holds: they stay staged, and the look after the
//     put-back finds them;
//  3. with a listing before the round (none tells no new file from yours),
//     the round's new ignore files, then the files git lists as untracked
//     once the rest is back, of the new ones and of those the round staged
//     that the listing does not name: a file of yours git ignores again is
//     not listed, and stays. A file older than since, the round's start, is
//     never the round's, and stays too, whatever ignore rule showed it.
//
// The files go through an os.Root of the copy, with each of their
// directories this empties, deepest first; a directory that still holds
// anything (a file git ignores, a tracked file) stays.
func putBackLeft(ctx context.Context, git *gitx.Client, copyDir string, left roundLeftovers, since time.Time) error {
	if full := left.fullRestore(); len(full) > 0 {
		if err := git.RestoreTracked(ctx, copyDir, full...); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(copyDir)
	if err != nil {
		return err
	}
	defer root.Close()
	unstage := left.unstaged()
	if len(unstage) > 0 {
		if err := git.RestoreIndex(ctx, copyDir, unstage...); err != nil {
			return err
		}
	}
	if left.before == nil {
		return nil
	}
	maybe := slices.Concat(left.added, slices.DeleteFunc(left.staged(), func(p string) bool {
		_, ok := slices.BinarySearch(unstage, p)
		return !ok
	}))
	if len(maybe) == 0 {
		return nil
	}
	dirs := map[string]bool{}
	// remove removes the files of maybe that which takes and git lists as
	// untracked now, and says whether it removed any.
	remove := func(which func(string) bool) (bool, error) {
		still, err := git.UntrackedFiles(ctx, copyDir)
		if err != nil {
			return false, err
		}
		removed := false
		for _, f := range maybe {
			if _, listed := slices.BinarySearch(still, f); !listed || !which(f) {
				continue
			}
			if fi, err := root.Lstat(f); err != nil || fi.ModTime().Before(since) {
				continue // older than the round: not the round's
			}
			if err := root.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, err
			}
			removed = true
			for d := path.Dir(f); d != "."; d = path.Dir(d) {
				dirs[d] = true
			}
		}
		return removed, nil
	}
	// The round's new ignore files first: they may show files of yours that
	// git ignored, which a listing taken with them in place would name.
	if _, err := remove(ignoreFile); err != nil {
		return err
	}
	if _, err := remove(func(p string) bool { return !ignoreFile(p) }); err != nil {
		return err
	}
	for _, d := range slices.SortedFunc(maps.Keys(dirs), func(a, b string) int {
		return cmp.Or(cmp.Compare(strings.Count(b, "/"), strings.Count(a, "/")), cmp.Compare(a, b))
	}) {
		_ = root.Remove(d) // one that is not empty stays
	}
	return nil
}
