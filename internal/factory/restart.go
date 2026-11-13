package factory

// The record of the work a restart would cut, and the drain `magnum factory
// install --drain` asks for. A restart of the factory daemon cuts the work
// that runs off the loop: a round of the local review (its agents' turns
// are lost and it runs again from its start), a check of a task's head (the
// proof and the precheck run again), a claim of a copy (its steps run
// again), a talk with a liaison (it starts again) and a QA run (its server
// and its browser stop, and a fresh run starts; a drain ends one whose walk
// has not begun at once). The daemon keeps that
// work in work.json, under its pid, from its start to its stop, and install
// reads it first: it refuses while anything runs, unless --now. A check
// stays in the record until a tick took its result, which lives only in
// the daemon's memory. With --drain, install writes drain.json (Drain:
// since when, and its pid and start). The tick that takes it starts nothing
// new while it holds (no launch, effect, round, check, claim or talk, as a
// stop's drain), shows it as a hold of the machine (HoldDrain), and the
// loop goes on and collects what ends; the record says the drain holds:
// install restarts the daemon only once the record says so and nothing
// runs. A drain whose command is gone (liveDrainer) is lifted, and the
// daemon that starts ends the drain it was waited for.

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
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// The kinds of WorkItem.
const (
	WorkRound  = "round"  // a round of the local review
	WorkCheck  = "check"  // the factory's proof and precheck of a head
	WorkClaim  = "claim"  // a claim of a copy
	WorkTalk   = "talk"   // a talk with a task's liaison
	WorkQA     = "qa"     // a QA run: its server, its browser and its walk
	WorkRepair = "repair" // a broken copy's repair on its backoff
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
// for none) and the work it runs that a restart would cut, oldest first;
// the controls its build knows (engine.ControlsVersion: pause.json and the
// drain for a reboot; 0 for an older build) and the build itself, and its
// acknowledgement of the drain for a reboot that holds (control.go; nil
// for none).
type WorkRecord struct {
	PID         int              `json:"pid"`
	Drain       string           `json:"drain,omitempty"`
	Work        []WorkItem       `json:"work"`
	Controls    int              `json:"controls,omitempty"`
	Build       string           `json:"build,omitempty"`
	RebootDrain *engine.DrainAck `json:"reboot_drain,omitempty"`
}

// Drain is a drain install asks for (drain.json): when it started, and the
// pid and the start (ps's lstart: a pid another process took since has
// another) of the command that asks.
type Drain struct {
	Since time.Time
	PID   int
	Start string
}

// Value is d as drain.json holds it.
func (d Drain) Value() string {
	b, _ := json.Marshal(struct {
		Since string `json:"since"`
		PID   int    `json:"pid,omitempty"`
		Start string `json:"start,omitempty"`
	}{store.FormatTime(d.Since), d.PID, d.Start})
	return string(b)
}

// ParseDrain reads a drain.json value (Drain.Value); false for anything
// else.
func ParseDrain(v string) (Drain, bool) {
	var raw struct {
		Since string `json:"since"`
		PID   int    `json:"pid"`
		Start string `json:"start"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(v)), &raw) != nil {
		return Drain{}, false
	}
	t, err := store.ParseTime(raw.Since)
	if err != nil {
		return Drain{}, false
	}
	return Drain{Since: t, PID: raw.PID, Start: raw.Start}, true
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
// Drain.Value (since when, and the asking command's pid and start), and it
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
// workMu, with fsx.ReplaceFile: a record lost in a crash is no record, and
// install then fails closed. A write that fails removes the record for the
// same reason (a full disk still lets a file go), and the next change
// writes it again. After closeWork it writes nothing.
func (e *Engine) saveWorkLocked() {
	if e.workClosed {
		return
	}
	rec := WorkRecord{PID: os.Getpid(), Drain: e.heldDrain(), Work: []WorkItem{}, Controls: engine.ControlsVersion, Build: e.d.Build,
		RebootDrain: e.ctl.ack}
	for _, k := range slices.Sorted(maps.Keys(e.work)) {
		rec.Work = append(rec.Work, e.work[k])
	}
	b, err := json.Marshal(rec)
	if err != nil || (e.workSaved != nil && bytes.Equal(b, e.workSaved)) {
		return
	}
	path := e.d.Layout.FactoryWork()
	write := e.workWrite
	if write == nil {
		write = func(path string, b []byte) error { return fsx.ReplaceFile(path, b, 0o600) }
	}
	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = write(path, append(b, '\n'))
	}
	if err != nil {
		e.log.Warn("write the record of the work a restart would cut; removed it, so install cannot trust an old one", "err", err)
		e.workSaved = nil
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			e.log.Warn("remove the record of the work a restart would cut", "err", err)
		}
		return
	}
	e.workSaved = b
}

// closeWork removes the record as the daemon stops: no daemon keeps it. A
// stop whose wait for the work off the loop ran out keeps it, naming that
// work (control.go: workCut): it may still run, its agents in herdr, and a
// drain read after the exit must not take it for a clean stop.
func (e *Engine) closeWork() {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	e.workClosed = true
	if e.ctl.workCut.Load() {
		e.log.Warn("kept the record of the work a restart would cut: work off the loop did not end before the stop")
		return
	}
	if err := os.Remove(e.d.Layout.FactoryWork()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.log.Warn("remove the record of the work a restart would cut", "err", err)
	}
}

// heldDrain is the drain the daemon holds ("" for none).
func (e *Engine) heldDrain() string {
	v, _ := e.drainHeld.Load().(string)
	return v
}

// startsHeld reports whether no new automatic work may start: nothing new
// starts at all (drainsHeld: the daemon stops, holds the drain install asked
// for, or drains for a reboot), or `magnum pause` holds the factory
// (control.go): the rounds, the checks and the QA runs wait on it.
func (e *Engine) startsHeld() bool { return e.drainsHeld() || e.ctl.pause.Load() != nil }

// processStart is when process pid started (execx.ProcessStart, with the
// real ps): Engine.procStart's default.
func processStart(pid int) (string, error) {
	return execx.ProcessStart(context.Background(), &execx.Real{}, pid)
}

// liveDrainer reports whether the command that asked for drain d still
// runs: the process at its pid started when the command did (procStart),
// so a pid another process took since is not the command. A drain that
// names no start falls back on engine.DrainerAlive (a magnum process holds
// the pid). A pid whose start ps cannot read counts as the command unless
// no process has it.
func (e *Engine) liveDrainer(d Drain) bool {
	if d.Start == "" {
		return engine.DrainerAlive(d.PID)
	}
	start, err := e.procStart(d.PID)
	if err != nil {
		return !errors.Is(syscall.Kill(d.PID, 0), syscall.ESRCH)
	}
	return start == d.Start
}

// takeDrain reads, at the start of a tick, the drain install asks for and
// holds it, or lets go of the one it held, and writes the record that says
// so: every work that started in an earlier tick is in it, and none starts
// in this one once it holds. While it holds, the drain is a hold of the
// machine (HoldDrain), which holds.json shows the screen. A drain whose
// command is gone is lifted. A drain.json it cannot read is no drain, said
// once.
func (e *Engine) takeDrain(ctx context.Context) {
	value, err := ReadDrain(e.d.Layout)
	d, ok := ParseDrain(value)
	switch {
	case err != nil:
		e.warnDrain(err.Error())
		value = ""
	case value != "" && !ok:
		e.warnDrain("not a drain install writes")
		value = ""
	default:
		e.drainWarned = ""
	}
	gone := false
	if value != "" && d.PID > 0 && !e.drainerAlive(d) {
		if err := LiftDrain(e.d.Layout, value); err != nil {
			e.log.Warn("lift a drain whose command is gone", "err", err)
		}
		e.event(ctx, 0, "drain", registry.PhaseInfo, fmt.Sprintf("drain lifted: the command that started it at %s (pid %d) is gone, "+
			"so no restart follows; work starts again", d.Since.Local().Format("15:04:05"), d.PID))
		value, gone = "", true
	}
	prev := e.heldDrain()
	e.drainHeld.Store(value)
	if value == "" {
		delete(e.holds, HoldDrain)
	} else if h := e.holds[HoldDrain]; h == nil || value != prev {
		e.holds[HoldDrain] = &Hold{Kind: HoldDrain, Reason: drainOwner(value) + ": " + drainWords,
			Since: d.Since.Round(0)}
	}
	e.saveWork()
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

// warnDrain logs, once while it lasts, that drain.json cannot be read
// (why): no drain holds.
func (e *Engine) warnDrain(why string) {
	if why == e.drainWarned {
		return
	}
	e.drainWarned = why
	e.log.Warn("the drain install asked for cannot be read; no drain holds", "file", filepath.Base(e.d.Layout.FactoryDrain()), "why", why)
}

// drainWords is what the install drain's hold says after whose it is: the
// wait `magnum factory status` gives each active task with no wait of its
// own (StoppingHold), which cannot see whether its agent is mid-turn. The
// drain holds back what would start (a launch, an effect, a round, a check,
// a claim, a talk), never a turn or a round in flight, nor the prompts, so
// it says what holds for any such task. A reboot's drain holds the prompts
// too, and says that nothing new starts.
const drainWords = "no new start: work in flight finishes"

// drainOwner names the command a drain value came from.
func drainOwner(value string) string {
	d, ok := ParseDrain(value)
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
