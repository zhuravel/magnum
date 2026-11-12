package factory

// The factory's side of `magnum pause` and `magnum drain` (docs/superpowers/
// specs/2026-11-12-control-commands-design.md). Both are files beside
// drain.json, since the CLI cannot migrate the factory's registry under a
// running daemon.
//
// A pause (pause.json, Pause) holds new automatic work: launches of a
// stage's roles, claims of a copy, new local review rounds, checks of a head
// and QA runs (startsHeld, mayLaunch). What you ask for still runs: a start
// queues its task, an answer, a retry and a talk run, and a push (an effect,
// drainsHeld) goes out. Each tick reads it into a hold of the machine
// (HoldPause), and the tick after its end lifts it.
//
// A drain for a reboot (reboot-drain.json, engine.RebootDrain) holds what a
// restart's drain holds (drainsHeld) and new prompts to the agents at work
// (MayPrompt, which the runtime's Send asks). Each tick acknowledges it in
// work.json (WorkRecord.RebootDrain): the work a reboot would cut (the
// record's work, a repair aside, and every agent's turn in flight, the
// liaisons' too), quiet once none runs, and once quiet the idle agents of
// every task parked (Runtime.Park). An hour quiet with no reboot raises a
// NEEDS YOU blocker, with its one alert. The first start on another boot of
// the Mac ends a drain until "boot"; `magnum resume` removes the file.

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
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
)

// The holds of the controls (Hold.Kind): `magnum pause`'s and `magnum
// drain`'s.
const (
	HoldPause  = "pause"
	HoldReboot = "reboot"
)

// rebootWhat is why nothing new starts under a drain for a reboot, as a
// held start says it.
const rebootWhat = "the factory drains for a reboot (`magnum resume` ends it)"

// Pause is a pause `magnum pause` asks of the factory (pause.json): the
// command's generation, since when, until when (zero: until `magnum
// resume`) and why.
type Pause struct {
	Generation int64     `json:"generation"`
	Since      time.Time `json:"since"`
	Until      time.Time `json:"until,omitzero"`
	Reason     string    `json:"reason,omitempty"`
}

// Value is p as pause.json holds it.
func (p Pause) Value() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// ParsePause reads pause.json's value (Value); false for anything else.
func ParsePause(v string) (Pause, bool) {
	var p Pause
	if strings.TrimSpace(v) == "" || json.Unmarshal([]byte(v), &p) != nil || p.Since.IsZero() {
		return Pause{}, false
	}
	return p, true
}

// Words say the pause as the holds and the status lines do: "paused by you
// until Nov 12 15:30 (lunch)".
func (p Pause) Words() string {
	s := "paused by you"
	if !p.Until.IsZero() {
		s += " until " + p.Until.Local().Format("Jan 2 15:04")
	}
	if p.Reason != "" {
		s += " (" + p.Reason + ")"
	}
	return s
}

// ReadPause is the pause asked of the factory daemon of layout l ("" for
// none).
func ReadPause(l paths.Layout) (string, error) { return readControl(l.FactoryPause()) }

// WritePause asks the factory daemon of layout l for a pause (Pause.Value),
// replacing any pause asked before.
func WritePause(l paths.Layout, value string) error { return writeControl(l.FactoryPause(), value) }

// LiftPause removes the pause asked of the factory daemon of layout l when
// it is value ("": whatever it is); another, or none, is left alone.
func LiftPause(l paths.Layout, value string) error { return liftControl(l.FactoryPause(), value) }

// ReadRebootDrain is the drain for a reboot asked of the factory daemon of
// layout l ("" for none).
func ReadRebootDrain(l paths.Layout) (string, error) { return readControl(l.FactoryRebootDrain()) }

// WriteRebootDrain asks the factory daemon of layout l for a drain for a
// reboot (engine.RebootDrain.Value), replacing any asked before.
func WriteRebootDrain(l paths.Layout, value string) error {
	return writeControl(l.FactoryRebootDrain(), value)
}

// LiftRebootDrain removes the drain for a reboot asked of the factory daemon
// of layout l when it is value ("": whatever it is).
func LiftRebootDrain(l paths.Layout, value string) error {
	return liftControl(l.FactoryRebootDrain(), value)
}

func readControl(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

func writeControl(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, []byte(value+"\n"), 0o600)
}

func liftControl(path, value string) error {
	cur, err := readControl(path)
	if err != nil || cur == "" || (value != "" && cur != value) {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// controlState is what the engine keeps of the controls: the pause and the
// drain for a reboot in force (read without a lock by the start gates), and
// the loop's own record of the drain it tends: its generation, since when
// the daemon is quiet under it, the tasks whose agents it parked, the last
// warnings about files it could not read, and the Mac's boot time as the
// start read it. ack is the acknowledgement work.json carries, under
// workMu.
type controlState struct {
	pause  atomic.Pointer[Pause]
	reboot atomic.Pointer[engine.RebootDrain]

	gen        int64
	quietSince time.Time
	parked     map[int64]bool
	done       bool
	warned     map[string]string
	boot       time.Time

	ack *engine.DrainAck
}

// drainsHeld reports whether nothing new may start at all: the daemon stops
// (a stop's drain), holds the drain install asked for, or drains for a
// reboot. Effects and talks wait on it alone, so a pause lets your push and
// your talk through.
func (e *Engine) drainsHeld() bool {
	return e.draining.Load() || e.heldDrain() != "" || e.ctl.reboot.Load() != nil
}

// MayPrompt is the runtime's hold of new prompts (runtime.Deps.MayPrompt): a
// drain for a reboot holds them, as a reboot would cut their turns; a
// restart's drain lets them go, since the turns outlive a restart.
func (e *Engine) MayPrompt() (who, what string, ok bool) {
	if e.ctl.reboot.Load() != nil {
		return holdRestart, rebootWhat, false
	}
	return "", "", true
}

// heldWhat is why nothing new starts under drainsHeld, in a held start's
// words.
func (e *Engine) heldWhat() string {
	if !e.draining.Load() && e.heldDrain() == "" && e.ctl.reboot.Load() != nil {
		return rebootWhat
	}
	return drainWhat
}

// takeControls reads, at the start of a tick, the pause and the drain for a
// reboot asked of the daemon and holds them as holds of the machine
// (HoldPause, HoldReboot), saying so in an event when one begins or ends. A
// pause past its end is lifted. A file it cannot read is said once and
// holds as it did; a drain this build cannot parse holds, unacknowledged,
// until `magnum resume` removes it.
func (e *Engine) takeControls(ctx context.Context) {
	now := e.now()
	l := e.d.Layout
	if v, err := ReadPause(l); err != nil {
		e.warnControl("pause", err.Error())
	} else {
		p, ok := ParsePause(v)
		why := "the pause was lifted: work starts again"
		switch {
		case v != "" && !ok:
			e.warnControl("pause", "not a pause `magnum pause` writes")
		case ok && !p.Until.IsZero() && !now.Before(p.Until):
			if err := LiftPause(l, v); err != nil {
				e.log.Warn("lift a pause past its end", "err", err)
			}
			ok, why = false, "the pause ended at its time: work starts again"
		}
		e.holdPause(ctx, p, ok, why)
	}
	v, err := ReadRebootDrain(l)
	if err != nil {
		e.warnControl("reboot", err.Error())
		return
	}
	d, ok := engine.ParseRebootDrain(v)
	if v != "" && !ok {
		e.warnControl("reboot", "not a drain `magnum drain` writes: it holds until `magnum resume`")
		d, ok = engine.RebootDrain{Since: now}, true
	}
	e.holdReboot(ctx, d, ok)
}

// warnControl logs, once while it lasts, that a control's file cannot be
// read (why).
func (e *Engine) warnControl(kind, why string) {
	if e.ctl.warned == nil {
		e.ctl.warned = map[string]string{}
	}
	if e.ctl.warned[kind] == why {
		return
	}
	e.ctl.warned[kind] = why
	e.log.Warn("a control's file cannot be read", "kind", kind, "why", why)
}

// holdPause holds pause p (ok) or lets go of the one held, saying why.
func (e *Engine) holdPause(ctx context.Context, p Pause, ok bool, why string) {
	prev := e.ctl.pause.Load()
	if !ok {
		e.ctl.pause.Store(nil)
		delete(e.holds, HoldPause)
		if prev != nil {
			e.event(ctx, 0, "pause", registry.PhaseInfo, why)
		}
		return
	}
	e.ctl.pause.Store(&p)
	e.holds[HoldPause] = &Hold{Kind: HoldPause, Reason: p.Words(), Since: p.Since.Round(0)}
	if prev == nil || prev.Generation != p.Generation || !prev.Until.Equal(p.Until) || prev.Reason != p.Reason {
		e.event(ctx, 0, "pause", registry.PhaseInfo, p.Words()+": no launch, claim, local review round, check or QA run starts; "+
			"your start, answer, retry, talk and push still run")
	}
}

// holdReboot holds drain d (ok) or lets go of the one held.
func (e *Engine) holdReboot(ctx context.Context, d engine.RebootDrain, ok bool) {
	prev := e.ctl.reboot.Load()
	if !ok {
		e.ctl.reboot.Store(nil)
		delete(e.holds, HoldReboot)
		if prev != nil {
			e.event(ctx, 0, "drain", registry.PhaseInfo, "the drain for a reboot was lifted: work starts again")
		}
		return
	}
	e.ctl.reboot.Store(&d)
	e.holds[HoldReboot] = &Hold{Kind: HoldReboot, Reason: "draining for a reboot since " + d.Since.Local().Format("Jan 2 15:04") +
		": nothing new starts; `magnum resume` ends it", Since: d.Since.Round(0)}
	if prev == nil || prev.Generation != d.Generation {
		e.event(ctx, 0, "drain", registry.PhaseInfo, "drain for a reboot held: nothing new starts (no launch, prompt, effect, "+
			"local review round, check, claim, talk or QA run); the work that runs ends, then the idle agents park; it lasts "+d.Words())
	}
}

// readBootTime reads the Mac's boot time (Deps.BootTime); zero when it
// cannot.
func (e *Engine) readBootTime(ctx context.Context) time.Time {
	if e.d.BootTime == nil {
		return time.Time{}
	}
	t, err := e.d.BootTime(ctx)
	if err != nil {
		e.log.Warn("read the Mac's boot time: a drain for a reboot lasts until `magnum resume`", "err", err)
		return time.Time{}
	}
	return t
}

// endRebootDrainAtStart ends, as the daemon starts, a drain for a reboot
// until "boot" that began on another boot of the Mac. A crash restart, an
// install or any start on the same boot keeps it.
func (e *Engine) endRebootDrainAtStart(ctx context.Context) {
	v, err := ReadRebootDrain(e.d.Layout)
	if err != nil || v == "" {
		return
	}
	e.ctl.boot = e.readBootTime(ctx)
	d, ok := engine.ParseRebootDrain(v)
	if !ok || !d.EndsAtStart(e.ctl.boot) {
		return
	}
	if err := LiftRebootDrain(e.d.Layout, v); err != nil {
		e.log.Warn("end the drain for a reboot at the start", "err", err)
		return
	}
	e.event(ctx, 0, "drain", registry.PhaseInfo, "the drain for a reboot that began at "+d.Since.Local().Format("Jan 2 15:04")+
		" ended: the factory daemon started after the Mac rebooted")
}

// tendRebootDrain acknowledges the drain for a reboot that holds, after
// takeControls: the work a reboot would cut (rebootDrainWork), quiet once
// none runs, and once quiet the idle agents of every task parked
// (parkForRebootDrain); an hour quiet raises a NEEDS YOU blocker. With none
// it drops its acknowledgement and the blocker.
func (e *Engine) tendRebootDrain(ctx context.Context, tasks []registry.Task) {
	st := &e.ctl
	d := st.reboot.Load()
	if d == nil || d.Generation == 0 {
		if st.gen != 0 {
			st.gen, st.quietSince, st.parked, st.done = 0, time.Time{}, nil, false
			e.clearBlocker(ctx, HoldReboot)
		}
		e.setDrainAck(nil)
		return
	}
	if st.gen != d.Generation {
		st.gen, st.quietSince, st.parked, st.done = d.Generation, time.Time{}, map[int64]bool{}, false
		e.clearBlocker(ctx, HoldReboot)
	}
	now := e.now()
	work := e.rebootDrainWork(ctx, tasks)
	quiet := len(work) == 0
	switch {
	case !quiet:
		st.quietSince = time.Time{}
	case st.quietSince.IsZero():
		st.quietSince = now
	}
	parked := quiet && e.parkForRebootDrain(ctx, tasks)
	if parked && !st.done {
		st.done = true
		e.event(ctx, 0, "drain", registry.PhaseInfo, "drained for a reboot: nothing runs and the idle agents are parked")
	}
	e.setDrainAck(&engine.DrainAck{Generation: d.Generation, PID: os.Getpid(), Quiet: quiet, QuietSince: st.quietSince,
		Parked: parked, Work: work})
	if quiet && now.Sub(st.quietSince) >= engine.DrainQuietToastAfter {
		e.raiseBlocker(ctx, HoldReboot, "the drain for a reboot still holds, quiet since "+st.quietSince.Local().Format("15:04")+
			": `magnum resume` ends it")
	}
}

// setDrainAck writes the acknowledgement into work.json (nil: none) when it
// changed.
func (e *Engine) setDrainAck(a *engine.DrainAck) {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if (a == nil) == (e.ctl.ack == nil) && (a == nil || a.Value() == e.ctl.ack.Value()) {
		return
	}
	e.ctl.ack = a
	e.saveWorkLocked()
}

// turnReader lists the agents' turns in flight (runtime.Runtime's).
type turnReader interface {
	TurnsInFlight(ctx context.Context) ([]runtime.TurnInFlight, error)
}

// rebootDrainWork is what a reboot would cut, in words: the work the record
// names (a backoff repair aside: the copy stays broken as it was and is
// repaired again) and every agent's turn in flight ("fx-writer turn of
// PR-12360", "liaison turn of PR-12360"). A runtime that cannot list its
// turns, or a read that fails, counts as a turn in flight.
func (e *Engine) rebootDrainWork(ctx context.Context, tasks []registry.Task) []string {
	e.workMu.Lock()
	items := make([]WorkItem, 0, len(e.work))
	for _, k := range slices.Sorted(maps.Keys(e.work)) {
		items = append(items, e.work[k])
	}
	e.workMu.Unlock()
	var out []string
	for _, w := range items {
		if w.Kind != WorkRepair {
			out = append(out, w.What)
		}
	}
	if e.d.Runtime == nil {
		return out
	}
	tr, ok := e.d.Runtime.(turnReader)
	if !ok {
		return append(out, "the agents' turns (this runtime cannot list them)")
	}
	turns, err := tr.TurnsInFlight(ctx)
	if err != nil {
		return append(out, "the agents' turns (unreadable: "+err.Error()+")")
	}
	keys := map[int64]string{}
	for _, t := range tasks {
		keys[t.ID] = TaskKey(t)
	}
	for _, tu := range turns {
		key := keys[tu.TaskID]
		if key == "" {
			key = fmt.Sprintf("task %d", tu.TaskID)
		}
		who := tu.Role
		if tu.Liaison {
			who = "liaison"
		}
		out = append(out, who+" turn of "+key)
	}
	return out
}

// parkForRebootDrain parks the idle agents of every task that has any (all
// but a queued one) once the daemon is quiet under the drain, each task once
// per drain; a park that fails is tried again at the next tick. It reports
// whether every park went through.
func (e *Engine) parkForRebootDrain(ctx context.Context, tasks []registry.Task) bool {
	if e.d.Runtime == nil {
		return true
	}
	all := true
	for _, t := range tasks {
		if t.State == registry.TaskQueued || e.ctl.parked[t.ID] {
			continue
		}
		if err := e.d.Runtime.Park(ctx, t); err != nil {
			e.log.Warn("park the agents of a task for the drain for a reboot", "task", t.ID, "err", err)
			e.event(ctx, t.ID, "park", registry.PhaseFail, err.Error())
			all = false
			continue
		}
		e.ctl.parked[t.ID] = true
	}
	return all
}
