package factory

// The record of the work a restart would cut, and the drain `magnum factory
// install --drain` asks for. A restart of the factory daemon cuts the work
// that runs off the loop: a round of the local review (its agents' turns
// are lost and it runs again from its start), a check of a task's head (the
// proof and the precheck run again), a claim of a copy (its steps run
// again) and a talk with a liaison (it starts again). The daemon keeps that
// work in work.json, under its pid, from its start to its stop, and install
// reads it first: it refuses while anything runs, unless --now. With
// --drain, install writes drain.json (engine.Drain: since when, and its
// pid). The tick that takes it starts nothing new while it holds (no
// launch, effect, round, check, claim or talk, as a stop's drain), the loop
// goes on and collects what ends, and the record says the drain holds:
// install restarts the daemon only once the record says so and nothing
// runs. A drain whose command is gone (engine.DrainerAlive) is lifted, and
// the daemon that starts ends the drain it was waited for.

import (
	"bytes"
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

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
)

// The kinds of WorkItem.
const (
	WorkRound = "round" // a round of the local review
	WorkCheck = "check" // the factory's proof and precheck of a head
	WorkClaim = "claim" // a claim of a copy
	WorkTalk  = "talk"  // a talk with a task's liaison
)

// WorkItem is one piece of work a restart would cut: its kind (Work*), its
// task, what it is in words ("local review round 2 of PR-12345") and since
// when it runs.
type WorkItem struct {
	Kind  string    `json:"kind"`
	Task  int64     `json:"task"`
	What  string    `json:"what"`
	Since time.Time `json:"since"`
}

// WorkRecord is work.json (paths.Layout.FactoryWork): the pid of the
// daemon that wrote it, the drain it holds (drain.json's value it took; ""
// for none) and the work it runs that a restart would cut, oldest first.
type WorkRecord struct {
	PID   int        `json:"pid"`
	Drain string     `json:"drain,omitempty"`
	Work  []WorkItem `json:"work"`
}

// ReadWork reads the record the factory daemon of layout l keeps; ok is
// false when there is none (no daemon of this build runs, or it stopped).
func ReadWork(l paths.Layout) (WorkRecord, bool, error) {
	var r WorkRecord
	b, err := os.ReadFile(l.FactoryWork())
	if errors.Is(err, fs.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return WorkRecord{}, false, fmt.Errorf("%s: %w", filepath.Base(l.FactoryWork()), err)
	}
	return r, true, nil
}

// WriteDrain asks the factory daemon of layout l for a drain: value is
// engine.Drain.Value (since when, and the asking command's pid), and it
// replaces any drain asked before.
func WriteDrain(l paths.Layout, value string) error {
	path := l.FactoryDrain()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, []byte(value+"\n"), 0o600)
}

// ReadDrain is the drain asked of the factory daemon of layout l ("" for
// none).
func ReadDrain(l paths.Layout) (string, error) {
	b, err := os.ReadFile(l.FactoryDrain())
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

// LiftDrain removes the drain asked of the factory daemon of layout l when
// it is value; a drain asked since by another command, or none, is left
// alone.
func LiftDrain(l paths.Layout, value string) error {
	cur, err := ReadDrain(l)
	if err != nil || cur != value {
		return err
	}
	if err := os.Remove(l.FactoryDrain()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// beginWork notes work a restart would cut, of kind (Work*) and task id,
// in the record, and returns its end, which takes it out again; the work's
// goroutine calls it as it ends.
func (e *Engine) beginWork(kind string, id int64, what string) (end func()) {
	e.workMu.Lock()
	e.workSeq++
	seq := e.workSeq
	e.work[seq] = WorkItem{Kind: kind, Task: id, What: what, Since: e.now().Round(0)}
	e.saveWorkLocked()
	e.workMu.Unlock()
	return func() {
		e.workMu.Lock()
		defer e.workMu.Unlock()
		delete(e.work, seq)
		e.saveWorkLocked()
	}
}

// saveWork writes the record now (saveWorkLocked).
func (e *Engine) saveWork() {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	e.saveWorkLocked()
}

// saveWorkLocked writes the record (work.json) when it changed, under
// workMu; a write that fails is logged, and install reads the older record.
// After closeWork it writes nothing.
func (e *Engine) saveWorkLocked() {
	if e.workClosed {
		return
	}
	rec := WorkRecord{PID: os.Getpid(), Drain: e.drainHeld, Work: []WorkItem{}}
	for _, k := range slices.Sorted(maps.Keys(e.work)) {
		rec.Work = append(rec.Work, e.work[k])
	}
	b, err := json.Marshal(rec)
	if err != nil || (e.workSaved != nil && bytes.Equal(b, e.workSaved)) {
		return
	}
	path := e.d.Layout.FactoryWork()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.log.Warn("write the record of the work a restart would cut", "err", err)
		return
	}
	if err := fsx.WriteFileAtomic(path, append(b, '\n'), 0o600); err != nil {
		e.log.Warn("write the record of the work a restart would cut", "err", err)
		return
	}
	e.workSaved = b
}

// closeWork removes the record as the daemon stops: no daemon keeps it.
func (e *Engine) closeWork() {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	e.workClosed = true
	if err := os.Remove(e.d.Layout.FactoryWork()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.log.Warn("remove the record of the work a restart would cut", "err", err)
	}
}

// heldDrain is the drain the daemon holds ("" for none).
func (e *Engine) heldDrain() string {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	return e.drainHeld
}

// startsHeld reports whether nothing new may start: the daemon stops (a
// stop's drain) or holds the drain install asked for.
func (e *Engine) startsHeld() bool { return e.draining.Load() || e.heldDrain() != "" }

// takeDrain reads, at the start of a tick, the drain install asks for and
// holds it, or lets go of the one it held, and writes the record that says
// so: every work that started in an earlier tick is in it, and none starts
// in this one once it holds. A drain whose command is gone is lifted. A
// drain.json it cannot read changes nothing.
func (e *Engine) takeDrain(ctx context.Context) {
	value, err := ReadDrain(e.d.Layout)
	if err != nil {
		e.log.Warn("read the drain install asked for", "err", err)
		return
	}
	gone := false
	if d, ok := engine.ParseDrain(value); ok && d.PID > 0 && !e.drainerAlive(d.PID) {
		if err := LiftDrain(e.d.Layout, value); err != nil {
			e.log.Warn("lift a drain whose command is gone", "err", err)
		}
		e.event(ctx, 0, "drain", registry.PhaseInfo, fmt.Sprintf("drain lifted: the command that started it at %s (pid %d) is gone, "+
			"so no restart follows; work starts again", d.Since.Local().Format("15:04:05"), d.PID))
		value, gone = "", true
	}
	e.workMu.Lock()
	prev := e.drainHeld
	e.drainHeld = value
	e.saveWorkLocked()
	e.workMu.Unlock()
	switch {
	case value == prev:
	case value != "":
		e.log.Info("draining for `magnum factory install --drain`: nothing new starts until the restart", "drain", value)
		e.event(ctx, 0, "drain", registry.PhaseInfo, "drain held for "+drainOwner(value)+": nothing new starts (no launch, effect, "+
			"local review round, check, claim or talk) until the restart; the work that runs ends first")
	case !gone:
		e.log.Info("the drain was lifted; work starts again")
		e.event(ctx, 0, "drain", registry.PhaseInfo, "drain lifted by its command: no restart followed; work starts again")
	}
}

// drainOwner names the command a drain value came from.
func drainOwner(value string) string {
	d, ok := engine.ParseDrain(value)
	switch {
	case !ok:
		return "a command"
	case d.PID > 0:
		return fmt.Sprintf("`magnum factory install --drain` (pid %d, since %s)", d.PID, d.Since.Local().Format("15:04:05"))
	}
	return fmt.Sprintf("`magnum factory install --drain` (since %s)", d.Since.Local().Format("15:04:05"))
}

// endDrainAtStart ends, as the daemon starts, the drain it was waited
// for: install restarted the daemon once the drain held and nothing ran.
func (e *Engine) endDrainAtStart(ctx context.Context) {
	value, err := ReadDrain(e.d.Layout)
	if err != nil || value == "" {
		return
	}
	if err := LiftDrain(e.d.Layout, value); err != nil {
		e.log.Warn("end the drain at the start", "err", err)
		return
	}
	e.event(ctx, 0, "drain", registry.PhaseInfo, "drain ended: the factory daemon restarted ("+drainOwner(value)+")")
}
