package factory

// The holds of the machine (spec section 10.1). An infrastructure hold
// (SSH, DNS or TLS, a MySQL ping, or the same failure on two tasks) and a
// herdr-down hold stop every stage at no cost to the tasks; a disk or a
// memory hold stops launches, parks the idle agents of the tasks that wait
// on you and stops idle servers. A hold that lasts, and a disk below half
// its floor, become a NEEDS YOU blocker of the home screen with one alert.
// The heavy stages (a local review round, local QA, a copy's server) run at
// most [factory] heavy_stages at once across tasks.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/textx"
)

// The kinds of Hold.
const (
	HoldInfra  = "infra"
	HoldHerdr  = "herdr"
	HoldDisk   = "disk"
	HoldMemory = "memory"
	// holdRestart is the drain's reason in a heldError; it is no Hold and
	// no task waits on it (the daemon exits).
	holdRestart = "restart"
)

const (
	// infraBackoffFirst is the wait before an infrastructure hold's first
	// try; each failed try doubles it up to infraBackoffMax (2, 4, 8, 16,
	// then every 30 minutes), as the review daemon's pause waits.
	infraBackoffFirst = 2 * time.Minute
	infraBackoffMax   = 30 * time.Minute
	// holdNeedsYou is how long an infrastructure or herdr hold lasts before
	// it is a NEEDS YOU blocker.
	holdNeedsYou = 30 * time.Minute
	// herdrRevealGap keeps the reveals of a herdr hold at most 2 an hour.
	herdrRevealGap = 30 * time.Minute
	drainWhat      = "the factory daemon is restarting"
)

// Hold is a stop of the machine's: its kind (HoldInfra, HoldHerdr,
// HoldDisk, HoldMemory), its reason without the kind ("SSH key refused"),
// since when it holds, when it tries again (an infrastructure hold's next
// try, a herdr hold's next reveal) and how many tries ran.
type Hold struct {
	Kind, Reason string
	Since, Next  time.Time
	Tries        int
}

// ErrHeld marks a launch or an effect that the engine held back: the daemon
// drains, or a hold of the machine is on. It costs the task nothing.
var ErrHeld = errors.New("held")

// heldError is an ErrHeld with who holds it and why, as a wait says it.
type heldError struct{ who, what string }

func (h heldError) Error() string        { return h.who + ": " + h.what }
func (h heldError) Is(target error) bool { return target == ErrHeld }

// heldWait reports whether a task's wait is a hold's (launch set it).
func heldWait(who string) bool {
	switch who {
	case HoldInfra, HoldHerdr, HoldDisk, HoldMemory:
		return true
	}
	return false
}

// hold weighs a tick's failures for the infrastructure hold and returns the
// hold in force. Without one, an infrastructure failure (infraCause) or the
// same failure on two tasks within 10 minutes starts it: every stage holds
// until its first try, 2 minutes later. A tick at or after the hold's Next
// that advanced the tasks (Tick's tried) is a try: another infrastructure
// failure doubles the wait (up to 30 minutes), none lifts the hold. Failures
// between tries, and a tick that tried nothing (herdr down, the first tick
// after a wake), change nothing.
func (e *Engine) hold(ctx context.Context, failures []error) (Hold, bool) {
	now := e.tickAt
	if now.IsZero() {
		now = e.now()
	}
	cause := ""
	for _, f := range failures {
		c := infraCause(f)
		if tf, ok := errors.AsType[taskFailure](f); ok && c == "" {
			if sig, same := e.sameOnTwoTasks(tf, now); same {
				c = "the same failure on 2 tasks: " + sig
			}
		}
		if c != "" && cause == "" {
			cause = c
		}
	}
	h := e.holds[HoldInfra]
	switch {
	case h == nil && cause == "":
		return Hold{}, false
	case h == nil:
		h = &Hold{Kind: HoldInfra, Reason: cause, Since: now, Next: now.Add(infraBackoffFirst)}
		e.holds[HoldInfra] = h
		e.log.Warn("infrastructure hold: every stage holds, at no cost to the tasks", "cause", cause, "next", h.Next)
		e.event(ctx, 0, "hold", registry.PhaseBegin, fmt.Sprintf("infra: %s; every stage holds; tries again at %s",
			cause, h.Next.Local().Format("15:04")))
		return *h, true
	case now.Before(h.Next) || !e.tried:
		return *h, true
	case cause != "":
		h.Tries++
		h.Reason = cause
		h.Next = now.Add(min(infraBackoffFirst<<h.Tries, infraBackoffMax))
		e.event(ctx, 0, "hold", registry.PhaseInfo, fmt.Sprintf("infra: try %d failed (%s); tries again at %s",
			h.Tries, cause, h.Next.Local().Format("15:04")))
		return *h, true
	}
	e.lift(ctx, HoldInfra, now)
	return Hold{}, false
}

// lift ends the hold of kind, its blocker with it.
func (e *Engine) lift(ctx context.Context, kind string, now time.Time) {
	h := e.holds[kind]
	if h == nil {
		return
	}
	delete(e.holds, kind)
	e.log.Info("hold lifted", "kind", kind, "after", now.Sub(h.Since).Round(time.Second))
	e.event(ctx, 0, "hold", registry.PhaseOK, fmt.Sprintf("%s: lifted after %s (%s)", kind,
		textx.DurationWords(now.Sub(h.Since)), h.Reason))
	e.clearBlocker(ctx, kind)
}

// escalate makes an infrastructure or herdr hold that has lasted 30 minutes
// a NEEDS YOU blocker, with one alert.
func (e *Engine) escalate(ctx context.Context, now time.Time) {
	for _, kind := range []string{HoldInfra, HoldHerdr} {
		h := e.holds[kind]
		if h == nil || now.Sub(h.Since) < holdNeedsYou {
			continue
		}
		e.raiseBlocker(ctx, kind, fmt.Sprintf("%s: %s, held since %s", kind, h.Reason, h.Since.Local().Format("15:04")))
	}
}

// raiseBlocker records a NEEDS YOU blocker of the factory under key and
// alerts once; a blocker raised before keeps its first line.
func (e *Engine) raiseBlocker(ctx context.Context, key, line string) {
	if _, raised := e.blockers[key]; raised {
		return
	}
	e.blockers[key] = line
	e.event(ctx, 0, "blocker", registry.PhaseFail, "you: "+line)
	e.alert(ctx, 0, "factory: you: "+line)
}

func (e *Engine) clearBlocker(ctx context.Context, key string) {
	line, raised := e.blockers[key]
	if !raised {
		return
	}
	delete(e.blockers, key)
	e.event(ctx, 0, "blocker", registry.PhaseOK, "cleared: "+line)
}

// Blocking is the factory's NEEDS YOU blockers of the home screen, one line
// each ("infra: SSH key refused, held since 14:02"), sorted.
func (e *Engine) Blocking() []string {
	return slices.Sorted(maps.Values(e.blockers))
}

// checkHerdr asks the factory's herdr session whether it answers. While it
// does not, a herdr hold stops every stage, and Reveal opens `herdr
// --session <herdr_session>` in a terminal, at most twice an hour
// (herdrRevealGap apart). It reports whether herdr is up.
func (e *Engine) checkHerdr(ctx context.Context, now time.Time) bool {
	if e.d.HerdrUp == nil {
		return true
	}
	err := e.d.HerdrUp(ctx)
	h := e.holds[HoldHerdr]
	if err == nil {
		e.lift(ctx, HoldHerdr, now)
		return true
	}
	if h == nil {
		h = &Hold{Kind: HoldHerdr, Reason: fmt.Sprintf("the %s session does not answer", e.cfg.Factory.HerdrSession), Since: now}
		e.holds[HoldHerdr] = h
		e.log.Warn("herdr hold: every stage holds", "session", e.cfg.Factory.HerdrSession, "err", err)
		e.event(ctx, 0, "hold", registry.PhaseBegin, "herdr: "+h.Reason+": "+err.Error())
	}
	if e.d.Reveal != nil && (e.lastReveal.IsZero() || now.Sub(e.lastReveal) >= herdrRevealGap) {
		e.lastReveal = now
		h.Tries++
		phase, text := registry.PhaseInfo, "herdr: opened `herdr --session "+e.cfg.Factory.HerdrSession+"`"
		if rerr := e.d.Reveal(ctx); rerr != nil {
			phase, text = registry.PhaseFail, text+": "+rerr.Error()
		}
		e.event(ctx, 0, "hold", phase, text)
	}
	if !e.lastReveal.IsZero() {
		h.Next = e.lastReveal.Add(herdrRevealGap)
	}
	return false
}

// pressure reads the disk and the memory. Under pressure a disk or memory
// hold stops launches (mayLaunch), the idle agents of the tasks that wait on
// you are parked, once per spell (Runtime.Park), and the servers of idle
// tasks stop (Copies.StopServer). A disk below half its floor is a NEEDS
// YOU blocker.
func (e *Engine) pressure(ctx context.Context, now time.Time, tasks []registry.Task) {
	disk, half := e.diskPressure()
	mem := e.memoryPressure(ctx)
	e.setHold(ctx, HoldDisk, disk, now)
	e.setHold(ctx, HoldMemory, mem, now)
	if half != "" {
		e.raiseBlocker(ctx, HoldDisk, half)
	} else {
		e.clearBlocker(ctx, HoldDisk)
	}
	if disk == "" && mem == "" {
		clear(e.parked)
		return
	}
	for _, t := range tasks {
		if !waitsOnYou(t) || e.parked[t.ID] || e.d.Runtime == nil {
			continue
		}
		e.parked[t.ID] = true
		if err := e.d.Runtime.Park(ctx, t); err != nil {
			e.log.Warn("park the agents of a task that waits on you", "task", t.ID, "err", err)
			e.event(ctx, t.ID, "park", registry.PhaseFail, err.Error())
			continue
		}
		e.event(ctx, t.ID, "park", registry.PhaseOK, "parked its idle agents: the Mac is short on disk or memory")
	}
	for _, t := range tasks {
		if s, ok := e.servers[t.ID]; ok && idle(t) {
			e.stopServer(ctx, t, s)
		}
	}
}

// waitsOnYou reports whether a task that has not ended waits on the
// operator.
func waitsOnYou(t registry.Task) bool {
	if t.State == registry.TaskDone || t.State == registry.TaskStopped {
		return false
	}
	return t.State == registry.TaskNeedsYou || t.WaitsWho == waitsYou
}

// idle reports whether a task's server serves nothing now: the task is not
// at work.
func idle(t registry.Task) bool { return t.State != registry.TaskActive || waitsOnYou(t) }

// setHold starts, updates or lifts (an empty reason) a launch hold.
func (e *Engine) setHold(ctx context.Context, kind, reason string, now time.Time) {
	h := e.holds[kind]
	switch {
	case reason == "":
		e.lift(ctx, kind, now)
	case h == nil:
		e.holds[kind] = &Hold{Kind: kind, Reason: reason, Since: now}
		e.log.Warn("launches hold", "kind", kind, "reason", reason)
		e.event(ctx, 0, "hold", registry.PhaseBegin, kind+": "+reason+"; launches hold")
	default:
		h.Reason = reason
	}
}

// diskPressure checks the filesystem of each repository's copies against
// its min_free_disk_gb: the first that is short gives the hold's reason,
// and the first below half its floor the blocker's line ("" for none).
func (e *Engine) diskPressure() (reason, half string) {
	if e.d.Disk == nil {
		return "", ""
	}
	floors := map[string]int{}
	var dirs []string
	for _, r := range e.cfg.Factory.Repos {
		if r.MinFreeDiskGB <= 0 {
			continue
		}
		dir := existingDir(copiesDir(r))
		if dir == "" {
			continue
		}
		if _, seen := floors[dir]; !seen {
			dirs = append(dirs, dir)
		}
		floors[dir] = max(floors[dir], r.MinFreeDiskGB)
	}
	for _, dir := range dirs {
		free, err := e.d.Disk(dir)
		if err != nil {
			e.log.Warn("disk check", "dir", dir, "err", err)
			continue
		}
		floor, freeGB := float64(floors[dir]), float64(free)/(1<<30)
		if freeGB < floor && reason == "" {
			reason = fmt.Sprintf("%.1f GB free under %s, needs %d GB", freeGB, dir, floors[dir])
		}
		if freeGB < floor/2 && half == "" {
			half = fmt.Sprintf("disk: %.1f GB free under %s, below half of %d GB", freeGB, dir, floors[dir])
		}
	}
	return reason, half
}

// copiesDir is the directory a repository's copies live in.
func copiesDir(r config.FactoryRepo) string {
	if r.Copies != "" {
		return filepath.Dir(paths.Expand(r.Copies))
	}
	return filepath.Dir(r.MainClone)
}

// existingDir is dir or its nearest ancestor that exists ("" for none): a
// copies template can name a directory per copy that is not made yet.
func existingDir(dir string) string {
	for dir != "" && dir != "." {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// memoryPressure is the memory hold's reason ("" for none): the kernel's
// pressure level at critical, or the swap at [factory] memory_gate_swap
// percent or more. The warn level only staggers launches (the runtime's).
func (e *Engine) memoryPressure(ctx context.Context) string {
	if e.d.Memory == nil {
		return ""
	}
	s := e.d.Memory(ctx)
	if s.Level == engine.MemoryCritical {
		return "critical pressure, " + s.SwapText()
	}
	if gate := e.cfg.Factory.MemoryGateSwap; gate > 0 && s.Swap != nil && s.Swap.TotalBytes > 0 {
		if pct := int(s.Swap.UsedBytes * 100 / s.Swap.TotalBytes); pct >= gate {
			return fmt.Sprintf("swap %d%% used, launches wait below %d%%", pct, gate)
		}
	}
	return ""
}

// mayLaunch reports whether a launch may start now; who and what are the
// hold that stops it, as the task's wait says it.
func (e *Engine) mayLaunch() (who, what string, ok bool) {
	if e.draining.Load() {
		return holdRestart, drainWhat, false
	}
	for _, kind := range []string{HoldHerdr, HoldInfra, HoldDisk, HoldMemory} {
		if h := e.holds[kind]; h != nil {
			return kind, h.Reason, false
		}
	}
	return "", "", true
}

// server is a copy's server the engine started for a task, and the heavy
// slot it holds.
type server struct {
	copy    Copy
	release func()
}

// startServer starts a task's copy's server (Copies.StartServer): a heavy
// stage, and a launch, so it waits for both.
func (e *Engine) startServer(ctx context.Context, t registry.Task, c Copy) error {
	if who, what, ok := e.mayLaunch(); !ok {
		return heldError{who: who, what: what}
	}
	if e.d.Copies == nil {
		return heldError{who: "copy", what: "this build has no copies yet"}
	}
	release, ok := e.heavy(ctx, t, "server")
	if !ok {
		return heldError{who: "heavy", what: "another heavy stage runs"}
	}
	if err := e.d.Copies.StartServer(ctx, c); err != nil {
		release()
		return err
	}
	e.servers[t.ID] = server{copy: c, release: release}
	return nil
}

// stopServer stops a server startServer started and frees its heavy slot.
func (e *Engine) stopServer(ctx context.Context, t registry.Task, s server) {
	if err := e.d.Copies.StopServer(ctx, s.copy); err != nil {
		e.log.Warn("stop an idle server", "task", t.ID, "copy", s.copy.Name, "err", err)
		e.event(ctx, t.ID, "server", registry.PhaseFail, "stop "+s.copy.Name+": "+err.Error())
		return
	}
	delete(e.servers, t.ID)
	s.release()
	e.event(ctx, t.ID, "server", registry.PhaseOK, "stopped "+s.copy.Name+"'s idle server: the Mac is short on disk or memory")
}

// heavySlot is a task's hold on a heavy stage: what runs (for the wait of
// the others), how many of the task's heavy uses share it (a task's QA and
// its server count once), whether startup adopted it from an attempt that
// still runs, and its order.
type heavySlot struct {
	key, what string
	refs      int
	adopted   bool
	seq       int
}

// heavy takes a slot of the heavy stages for task t's what (a local review
// round, local QA, a server): at most [factory] heavy_stages tasks hold one
// at once, and a task's own uses share its slot. When none is free it
// returns false and the task waits "heavy: <key>'s <what> runs", naming the
// oldest holder. release frees the use, once however often it is called.
func (e *Engine) heavy(ctx context.Context, t registry.Task, what string) (release func(), ok bool) {
	e.heavyMu.Lock()
	s := e.heavyHeld[t.ID]
	switch {
	case s != nil && s.adopted:
		s.adopted, s.what = false, what // the attempt that still ran takes its slot back
	case s != nil:
		s.refs++
	case len(e.heavyHeld) >= max(1, e.cfg.Factory.HeavyStages):
		holder := e.oldestHeavy()
		e.heavyMu.Unlock()
		e.waitOn(ctx, t, "heavy", holder.key+"'s "+holder.what+" runs")
		return nil, false
	default:
		e.heavySeq++
		e.heavyHeld[t.ID] = &heavySlot{key: TaskKey(t), what: what, refs: 1, seq: e.heavySeq}
	}
	e.heavyMu.Unlock()
	if t.WaitsWho == "heavy" {
		e.waitOn(ctx, t, "", "")
	}
	var once sync.Once
	return func() { once.Do(func() { e.releaseHeavy(t.ID, false) }) }, true
}

// oldestHeavy is the slot taken first; heavyMu is held.
func (e *Engine) oldestHeavy() *heavySlot {
	var old *heavySlot
	for _, s := range e.heavyHeld {
		if old == nil || s.seq < old.seq {
			old = s
		}
	}
	return old
}

// releaseHeavy frees one use of task id's slot, or the whole slot (all).
func (e *Engine) releaseHeavy(id int64, all bool) {
	e.heavyMu.Lock()
	defer e.heavyMu.Unlock()
	s := e.heavyHeld[id]
	if s == nil {
		return
	}
	if s.refs--; all || s.refs <= 0 {
		delete(e.heavyHeld, id)
	}
}

// adoptHeavy is startup's count of the heavy stages that still run: each
// task with a launching or running attempt of a heavy stage (a local
// review) holds its slot until that stage takes it back (heavy) and
// releases it.
func (e *Engine) adoptHeavy(ctx context.Context) error {
	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		return fmt.Errorf("factory startup: %w", err)
	}
	for _, t := range tasks {
		switch t.State {
		case registry.TaskDone, registry.TaskStopped:
			continue
		}
		as, err := e.reg.Attempts(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("factory startup: %w", err)
		}
		for _, a := range as {
			st, ok := e.stageOf(t, a.Stage)
			if !ok || !heavyStage(st) || (a.State != registry.AttemptLaunching && a.State != registry.AttemptRunning) {
				continue
			}
			e.heavyMu.Lock()
			if e.heavyHeld[t.ID] == nil {
				e.heavySeq++
				e.heavyHeld[t.ID] = &heavySlot{key: TaskKey(t), what: stageWhat(st.Name), refs: 1, adopted: true, seq: e.heavySeq}
			}
			e.heavyMu.Unlock()
		}
	}
	return nil
}

// heavyStage reports whether a stage is heavy: the local review loop.
func heavyStage(st config.FactoryStage) bool { return st.Type == config.FactoryStageReview }
