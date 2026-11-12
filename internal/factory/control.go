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

// parkTries is how many ticks in a row a task's park may fail under a drain
// for a reboot before the drain leaves it (its idle panes close with the
// reboot) and goes on: a park that keeps failing must not hold the drain
// for good.
const parkTries = 3

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
	parkFails  map[int64]int
	left       map[int64]string
	done       bool
	warned     map[string]string
	boot       engine.BootID

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
		e.warnControl(ctx, "pause", err.Error())
	} else {
		p, ok := ParsePause(v)
		why := "the pause was lifted: work starts again"
		switch {
		case v != "" && !ok:
			e.warnControl(ctx, "pause", "not a pause `magnum pause` writes")
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
		e.warnControl(ctx, "reboot", err.Error())
		return
	}
	d, ok := engine.ParseRebootDrain(v)
	if v != "" && !ok {
		e.warnControl(ctx, "reboot", "not a drain `magnum drain` writes")
		d, ok = engine.RebootDrain{Since: now}, true
	}
	e.holdReboot(ctx, d, ok)
}

// warnControl says, once while it lasts, that a control's file cannot be
// read (why), in the log and in a warn event with what to do: an unreadable
// pause.json holds no pause, an unreadable reboot-drain.json holds until
// `magnum resume` removes it (`magnum doctor` says it too).
func (e *Engine) warnControl(ctx context.Context, kind, why string) {
	if e.ctl.warned == nil {
		e.ctl.warned = map[string]string{}
	}
	if e.ctl.warned[kind] == why {
		return
	}
	e.ctl.warned[kind] = why
	e.log.Warn("a control's file cannot be read", "kind", kind, "why", why)
	text := "pause.json cannot be read (" + why + "): no pause holds; `magnum pause` writes it again, `magnum resume` removes it"
	evKind := "pause"
	if kind == "reboot" {
		text = "reboot-drain.json cannot be read (" + why + "): the drain holds until `magnum resume` removes it"
		evKind = "drain"
	}
	e.event(ctx, 0, evKind, registry.PhaseFail, text)
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

// pausedNote is what the answer to your retry or your answers adds while
// `magnum pause` holds the factory: the task goes on, but its next launch,
// claim or check waits for the pause ("" with none).
func (e *Engine) pausedNote() string {
	p := e.ctl.pause.Load()
	if p == nil {
		return ""
	}
	return "; the factory is " + p.Words() + ": its next launch, claim or check waits for `magnum resume`"
}

// readBoot reads the Mac's boot (Deps.Boot); unknown when it cannot.
func (e *Engine) readBoot(ctx context.Context) engine.BootID {
	if e.d.Boot == nil {
		return engine.BootID{}
	}
	b, err := e.d.Boot(ctx)
	if err != nil {
		e.log.Warn("read the Mac's boot: a drain for a reboot lasts until `magnum resume`", "err", err)
		return engine.BootID{}
	}
	return b
}

// endRebootDrainAtStart ends, as the daemon starts, a drain for a reboot
// until "boot" that began on another boot of the Mac. A crash restart, an
// install or any start on the same boot keeps it.
func (e *Engine) endRebootDrainAtStart(ctx context.Context) {
	v, err := ReadRebootDrain(e.d.Layout)
	if err != nil || v == "" {
		return
	}
	e.ctl.boot = e.readBoot(ctx)
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
			st.gen, st.quietSince, st.parked, st.parkFails, st.left, st.done = 0, time.Time{}, nil, nil, nil, false
			e.clearBlocker(ctx, HoldReboot)
		}
		e.setDrainAck(nil)
		return
	}
	if st.gen != d.Generation {
		st.gen, st.quietSince, st.done = d.Generation, time.Time{}, false
		st.parked, st.parkFails, st.left = map[int64]bool{}, map[int64]int{}, map[int64]string{}
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
	var left []string
	for _, id := range slices.Sorted(maps.Keys(st.left)) {
		left = append(left, st.left[id])
	}
	e.setDrainAck(&engine.DrainAck{Generation: d.Generation, PID: os.Getpid(), Quiet: quiet, QuietSince: st.quietSince,
		Parked: parked, Work: work, Left: left})
	if quiet && now.Sub(st.quietSince) >= engine.DrainQuietToastAfter {
		e.raiseBlocker(ctx, HoldReboot, "the drain for a reboot still holds, quiet since "+st.quietSince.Local().Format("15:04")+
			": `magnum resume` ends it")
	}
}

// setDrainAck writes the acknowledgement into work.json (nil: none). The
// record's own check skips a write of what it holds already
// (saveWorkLocked), so one that failed is written again at the next tick.
func (e *Engine) setDrainAck(a *engine.DrainAck) {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	e.ctl.ack = a
	e.saveWorkLocked()
}

// liveReader says whether a task has a live agent (runtime.Runtime's
// LiveAgents); a runtime without it parks every task it may.
type liveReader interface {
	LiveAgents(ctx context.Context, t registry.Task) (bool, error)
}

// parkable reports whether the drain parks task t's agents: a task at work,
// waiting, needing you or published (a queued one has none yet, an ended
// one had them parked as it ended) with a live agent, the liaison's too.
func (e *Engine) parkable(ctx context.Context, t registry.Task) bool {
	switch t.State {
	case registry.TaskActive, registry.TaskWaiting, registry.TaskNeedsYou, registry.TaskPublished:
	default:
		return false
	}
	lr, ok := e.d.Runtime.(liveReader)
	if !ok {
		return true
	}
	live, err := lr.LiveAgents(ctx, t)
	return err != nil || live // one it cannot read is parked: the park says what holds it
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

// parkForRebootDrain parks the idle agents of every task that has live
// ones (parkable) once the daemon is quiet under the drain, each task once
// per drain. A park that fails is tried again at the next tick; after
// parkTries failures in a row the drain leaves the task as it is, says so
// once (the reboot closes its idle panes, and the factory brings its agents
// back after it), names it in the acknowledgement's Left, and goes on. It
// reports whether every park went through or was left.
func (e *Engine) parkForRebootDrain(ctx context.Context, tasks []registry.Task) bool {
	if e.d.Runtime == nil {
		return true
	}
	st := &e.ctl
	all := true
	for _, t := range tasks {
		if st.parked[t.ID] || st.left[t.ID] != "" || !e.parkable(ctx, t) {
			continue
		}
		err := e.d.Runtime.Park(ctx, t)
		if err == nil {
			st.parked[t.ID] = true
			continue
		}
		st.parkFails[t.ID]++
		e.log.Warn("park the agents of a task for the drain for a reboot", "task", t.ID, "try", st.parkFails[t.ID], "err", err)
		if st.parkFails[t.ID] < parkTries {
			all = false
			continue
		}
		why := TaskKey(t) + ": its agents could not be parked (" + err.Error() + "): the reboot closes their idle panes, and the factory " +
			"brings them back after it"
		st.left[t.ID] = why
		e.event(ctx, t.ID, "park", registry.PhaseFail, "left for the drain for a reboot: "+why)
	}
	return all
}
