package factory

// What the factory's checks keep for you in a copy. No git step the factory
// runs in a copy discards a change it did not make, so a check that cannot
// vouch for a change (restoreOwn: somebody else could have made it since the
// check began), or that finds one it did not make in its way (a base run, a
// look after a wait), leaves it, names it for you and records it in
// kept.json. Those changes never go to the writer as its change to commit or
// undo: once the task goes on, each goes back under restoreOwn's guard, or
// the task needs you again for it (settleKept). And a copy a base run left
// detached with such changes is looked at again only when it can matter
// (keptDue).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/textx"
)

// keptFile is kept.json in a task's directory (keptRecord).
const keptFile = "kept.json"

// keptRecheck is how long the sweep leaves a copy kept for you (a base run
// left it detached with changes the factory did not make) before it looks
// at it again when none of its kept paths changed.
const keptRecheck = 10 * time.Minute

// keptCopy is a copy a base run left detached that resumeDetached keeps for
// you: what it said, when it looked, the copy, the paths it kept and their
// marks then (pathMarks).
type keptCopy struct {
	what  string
	at    time.Time
	copy  string
	paths []string
	marks string
}

// keptDue reports whether the copy of task id is due a look by the sweep:
// none is kept for you, one of its kept paths changed since the last look
// (you undid or moved it), or keptRecheck went by.
func (e *Engine) keptDue(id int64) bool {
	k, ok := e.keptCopies[id]
	return !ok || !e.now().Before(k.at.Add(keptRecheck)) || pathMarks(k.copy, k.paths) != k.marks
}

// pathMarks is the state of paths in the work tree at dir, without git:
// each one's size, time of change and mode, or that it is gone.
func pathMarks(dir string, paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		fi, err := os.Lstat(filepath.Join(dir, p))
		if err != nil {
			fmt.Fprintf(&b, "%s -\n", p)
			continue
		}
		fmt.Fprintf(&b, "%s %d %d %v\n", p, fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
	}
	return b.String()
}

// keptRecord is kept.json: the tracked changes a check of the factory left
// in task's copy for you, each path with its content as it left it (sha256,
// "" for a path gone), when the check began, whether its own commands may
// have made them (Maybe: restoreOwn could not tell them from others'; false:
// the factory did not make them), and why, as the task was told.
type keptRecord struct {
	Copy     string            `json:"copy"`
	CopyName string            `json:"copy_name"`
	Since    time.Time         `json:"since"`
	Maybe    bool              `json:"maybe_ours"`
	Paths    map[string]string `json:"paths"`
	Why      string            `json:"why"`
	At       time.Time         `json:"at"`
}

// keptUnwritten is a keptRecord the factory could not write to kept.json,
// held in memory with why (writeKept's error) until a later write works.
type keptUnwritten struct {
	rec keptRecord
	err error
}

// keepFor records paths, tracked changes a check of task t that began at
// since left in copy c for you (or could not put back), in kept.json
// (keptRecord); none records nothing. A write that fails keeps the record
// in memory (keptUnwritten, written again by settleKept), so the paths are
// never handed on for want of it.
func (e *Engine) keepFor(t registry.Task, c Copy, since time.Time, paths []string, maybe bool, why string) {
	if len(paths) == 0 {
		return
	}
	rec := keptRecord{Copy: c.Path, CopyName: c.Name, Since: since, Maybe: maybe, Paths: map[string]string{}, Why: why, At: e.now()}
	for _, p := range paths {
		rec.Paths[p] = contentSum(c.Path, p)
	}
	err := e.writeKept(t.ID, rec)
	e.keptMu.Lock()
	defer e.keptMu.Unlock()
	if err != nil {
		e.log.Warn("record the changes a check left for you; the record is held in memory", "task", t.ID, "err", err)
		e.keptMem[t.ID] = keptUnwritten{rec, err}
		return
	}
	delete(e.keptMem, t.ID)
}

// writeKept writes rec to task id's kept.json (the keptWrite seam, else an
// atomic write).
func (e *Engine) writeKept(id int64, rec keptRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	dir := e.d.Layout.FactoryTaskDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path, b := filepath.Join(dir, keptFile), append(b, '\n')
	if e.keptWrite != nil {
		return e.keptWrite(path, b)
	}
	return fsx.WriteFileAtomic(path, b, 0o600)
}

// keptRecorded reports whether task id has a record of changes a check left
// in its copy: kept.json is there (readable or not), or a record the factory
// could not write is held in memory.
func (e *Engine) keptRecorded(id int64) bool {
	e.keptMu.Lock()
	_, mem := e.keptMem[id]
	e.keptMu.Unlock()
	_, err := os.Lstat(filepath.Join(e.d.Layout.FactoryTaskDir(id), keptFile))
	return mem || err == nil
}

// loadKept is task t's record of the changes a check left in its copy: ok
// false for none. A record held in memory is written again first. A record
// the factory cannot read or write is never acted on: why says so, for you
// (the task waits on you, and nothing is handed to the writer).
func (e *Engine) loadKept(t registry.Task) (rec keptRecord, ok bool, why string) {
	file := filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), keptFile)
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		e.keptMu.Lock()
		u, mem := e.keptMem[t.ID]
		e.keptMu.Unlock()
		if !mem {
			return rec, false, ""
		}
		if werr := e.writeKept(t.ID, u.rec); werr != nil {
			return rec, false, fmt.Sprintf("%s has changes a check of the factory left for you (%s), and the factory cannot record them "+
				"in %s (%s), so it hands the copy to nobody: commit or undo them, then let the task go on",
				cmpJoin(nonEmpty(u.rec.CopyName), u.rec.Copy), clipList(slices.Sorted(maps.Keys(u.rec.Paths))), file,
				textx.Clip(textx.FirstLine(werr.Error()), 200))
		}
		e.keptMu.Lock()
		delete(e.keptMem, t.ID)
		e.keptMu.Unlock()
		return u.rec, true, ""
	}
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if err == nil && rec.Copy == "" {
		err = errors.New("it names no copy")
	}
	if err != nil {
		return rec, false, fmt.Sprintf("the factory cannot read %s (%s), its record of changes a check left in the task's copy for you, "+
			"so it hands the copy to nobody: commit or undo what git status shows there, remove the file, then let the task go on",
			file, textx.Clip(textx.FirstLine(err.Error()), 200))
	}
	return rec, true, ""
}

// refusal reads err as git refusing to switch or move a copy because a file
// it does not track is in the way, an ignored one included
// (--no-overwrite-ignore): the files git named (its tab-indented lines, or
// the quoted name of its one-line form) and its first line; ok false for
// any other error. It reads git's English messages: a refusal in another
// language is an error as any other (nothing is written over either way,
// and the look before a switch, lostBy, names the cases it knows first).
func refusal(err error) (files []string, line string, ok bool) {
	ee, found := errors.AsType[*execx.ExitError](err)
	if !found || (!strings.Contains(ee.Stderr, "would be overwritten by") && !strings.Contains(ee.Stderr, "would lose untracked files")) {
		return nil, "", false
	}
	line = textx.Clip(textx.FirstLine(strings.TrimPrefix(strings.TrimSpace(ee.Stderr), "error: ")), 200)
	for l := range strings.SplitSeq(ee.Stderr, "\n") {
		if f, tab := strings.CutPrefix(l, "\t"); tab && strings.TrimSpace(f) != "" {
			files = append(files, strings.TrimSpace(f))
		}
	}
	if len(files) == 0 {
		if _, rest, quoted := strings.Cut(line, "'"); quoted {
			if name, _, closed := strings.Cut(rest, "'"); closed && name != "" {
				files = []string{name}
			}
		}
	}
	return files, line, true
}

// refusedWhat is the wait of a task whose copy (name) git refused to switch
// over files it does not track (files, else git's line): yours, never
// written over. A caller that sends the task to you ends it with the retry
// (retryHint).
func refusedWhat(name string, files []string, line string) string {
	if len(files) == 0 {
		return fmt.Sprintf("git refused to switch %s (%s): move or remove what is in the way", name, line)
	}
	return fmt.Sprintf("git refused to switch %s: it would write over %s, which git does not track there (an untracked or "+
		"ignored file of yours): move or remove it", name, clipList(files))
}

// contentSum is the sha256 of the file at p in dir, "" when there is none.
func contentSum(dir, p string) string {
	b, err := os.ReadFile(filepath.Join(dir, p))
	if err != nil {
		return ""
	}
	return sha256Hex(b)
}

// checkHeld reports whether task id has a check of its head, running or
// ended (its result not yet taken).
func (e *Engine) checkHeld(id int64) bool {
	e.checksMu.Lock()
	defer e.checksMu.Unlock()
	return e.checks[id] != nil
}

// settleKept acts on task t's record of the changes a check left in its
// copy (loadKept) once the task goes on, with no check of it held (a check's
// own result says what it kept first; provenHead takes an ended one before
// it settles). The changes it names never go to the writer: a path no longer
// a tracked change is settled; one still as the check left it, which the
// check's own commands may have made, goes back under restoreOwn's guard
// (nobody at work in the copy now, and nobody who could have changed it
// since the check began: othersIn) with begin and ok or fail events; any
// other stays as it is, with a fail event. The record goes once nothing in
// it is left. why is what the task then needs you for (the paths kept, a
// put-back that failed, a record the factory cannot read or write); err a
// wait (somebody at work in the copy) or a failure to read it. A copy the
// task no longer holds drops the record (an info event).
func (e *Engine) settleKept(ctx context.Context, t registry.Task) (why string, err error) {
	if e.d.Git == nil || e.checkHeld(t.ID) || !e.keptRecorded(t.ID) {
		return "", nil
	}
	rec, ok, why := e.loadKept(t)
	if !ok {
		return why, nil
	}
	file := filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), keptFile)
	name := cmpJoin(nonEmpty(rec.CopyName), rec.Copy)
	drop := func(phase, text string) (string, error) {
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		e.event(ctx, t.ID, "check", phase, text)
		return "", nil
	}
	switch holds, known, err := e.holdsCopy(ctx, t, rec.CopyName); {
	case err != nil:
		return "", err
	case !known:
		return "", nil
	case !holds:
		return drop(registry.PhaseInfo, "the record of the changes left for you in "+name+" is dropped: "+TaskKey(t)+" no longer holds the copy")
	}
	dirty, err := trackedChanges(ctx, e.d.Git, rec.Copy)
	if err != nil {
		return "", err
	}
	var left []string
	for _, p := range slices.Sorted(maps.Keys(rec.Paths)) {
		if slices.Contains(dirty, p) {
			left = append(left, p)
		}
	}
	if len(left) == 0 {
		return drop(registry.PhaseOK, "the changes left for you in "+name+" are no longer there")
	}
	if err := e.copyIdle(ctx, t); err != nil {
		if h, ok := errors.AsType[heldError](err); ok {
			what := name + " has changes a check left for you; they are settled once nobody works in it (" + h.what + ")"
			e.waitOn(ctx, t, waitCopy, what)
			return "", heldError{who: waitCopy, what: what}
		}
		return "", err
	}
	var back, keep []string
	switch {
	case !rec.Maybe:
		why, keep = "the factory did not make them", left
	default:
		if who := e.othersIn(ctx, t, rec.Since); who != "" {
			why, keep = who+" since the check began", left
			break
		}
		for _, p := range left {
			if contentSum(rec.Copy, p) == rec.Paths[p] {
				back = append(back, p)
			} else {
				keep = append(keep, p)
			}
		}
		why = "changed since the check left them"
	}
	if len(back) > 0 {
		list := clipList(back)
		e.event(ctx, t.ID, "check", registry.PhaseBegin, "puts back "+list+" in "+name+": a check left them, and nobody else could have "+
			"changed them since it began")
		if err := e.d.Git.RestoreTracked(ctx, rec.Copy, back...); err != nil {
			line := clipEnd(textx.FirstLine(execx.Redact(err.Error())), 240) // git says what went wrong at the end
			e.event(ctx, t.ID, "check", registry.PhaseFail, "put back "+list+" in "+name+": "+line)
			return fmt.Sprintf("%s still has %s, which the factory could not put back (%s): commit or undo them, or let the task go "+
				"on once git works there again; the factory never hands them to the writer", name, list, line), nil
		}
		e.event(ctx, t.ID, "check", registry.PhaseOK, "put back "+list+" in "+name+" as the branch has it")
	}
	if len(keep) == 0 {
		return drop(registry.PhaseOK, "the changes left for you in "+name+" are settled")
	}
	list := clipList(keep)
	e.event(ctx, t.ID, "check", registry.PhaseFail, "left "+list+" in "+name+" as it is: "+why)
	return fmt.Sprintf("%s still has %s, which the factory's checks left for you (%s): commit or undo them, then let the task go on; "+
		"the factory never hands them to the writer", name, list, why), nil
}

// afterWait looks at task t's copy c again after a wait of the check what,
// begun at since (the machine's spec lock), before any of its commands runs:
// nobody may be at work in it now (copyIdle: else a wait), and it may hold
// no tracked change, since the check began on a clean tree and ran nothing
// since: whatever is there is not the factory's (a keptChanges naming it and
// who touched the copy meanwhile, TouchedSince, with a fail event of kind
// and kept.json), and no command runs over it. It returns the check's new
// start, taken before it looked at a clean tree: a change after it is the
// check's own, or somebody else's (othersIn).
func (e *Engine) afterWait(ctx context.Context, t registry.Task, c Copy, since time.Time, kind, what string) (time.Time, error) {
	at := e.now()
	if err := e.copyIdle(ctx, t); err != nil {
		return since, err
	}
	dirty, err := trackedChanges(ctx, e.d.Git, c.Path)
	if err != nil || len(dirty) == 0 {
		return at, err
	}
	who := ""
	if w := e.othersIn(ctx, t, since); w != "" {
		who = " (" + w + ")"
	}
	why := fmt.Sprintf("%s has changes the factory did not make (%s), made while %s waited for the machine's spec lock%s, so it ran "+
		"nothing over them: keep or undo them, then let the task go on", c.Name, clipList(dirty), what, who)
	e.event(ctx, t.ID, kind, registry.PhaseFail, why)
	e.keepFor(t, c, since, dirty, false, why)
	return since, keptChanges{why, dirty}
}
