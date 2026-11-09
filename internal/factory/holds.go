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
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/textx"
)

// The kinds of Hold.
const (
	HoldInfra  = "infra"
	HoldHerdr  = "herdr"
	HoldDisk   = "disk"
	HoldMemory = "memory"
	// HoldDrain is the drain `magnum factory install --drain` asks for:
	// nothing new starts until the restart.
	HoldDrain = "drain"
	// holdRestart is the drain's reason in a heldError (a stop's drain, or
	// the one `magnum factory install --drain` asks for); it is no Hold and
	// no task waits on it (the daemon exits, or install restarts it).
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

// infraBackoffShifts caps the doubling: 2m<<4 is past infraBackoffMax, and
// an uncapped shift overflows after 27 tries (12 hours of an outage), when
// the wait turns negative and every tick becomes a try.
const infraBackoffShifts = 4

// infraBackoff is the wait after an infrastructure hold's tries failed
// tries times: 2, 4, 8, 16, then 30 minutes however long the outage lasts.
func infraBackoff(tries int) time.Duration {
	return min(infraBackoffFirst<<min(max(tries, 0), infraBackoffShifts), infraBackoffMax)
}

// Hold is a stop of the machine's: its kind (HoldInfra, HoldHerdr,
// HoldDisk, HoldMemory), its reason without the kind ("SSH key refused"),
// the repository it holds ("" for all: only an infrastructure hold of a
// repository whose every copy broke the same way has one, copyHolds), since
// when it holds, when it tries again (an infrastructure hold's next try, a
// herdr hold's next reveal) and how many tries ran.
type Hold struct {
	Kind, Reason string
	Repo         string `json:",omitempty"`
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

// heldWait reports whether a task's wait is a hold's or a runtime wait's
// (launch set it): a launch that goes through clears it.
func heldWait(who string) bool {
	switch who {
	case HoldInfra, HoldHerdr, HoldDisk, HoldMemory, waitUsage, waitLogin, waitOverload:
		return true
	}
	return false
}

// The waits of a launch the runtime holds back (runtime.WaitError.Who):
// Codex's budget or a CLI's usage limit, a logged-out CLI, an overload.
const (
	waitUsage    = "usage"
	waitLogin    = "login"
	waitOverload = "overload"
	// waitCopy is a task's wait for a copy (claimCopy).
	waitCopy = "copy"
)

// hold weighs a tick's failures for the infrastructure hold and returns the
// hold in force. Without one, an infrastructure failure (infraCause) or the
// same failure on two tasks within 10 minutes starts it: every stage holds
// until its first try, 2 minutes later. A tick at or after the hold's Next
// that advanced the tasks (Tick's tried) is a try: another infrastructure
// failure doubles the wait (up to 30 minutes), none lifts the hold. Failures
// between tries, and a tick that tried nothing (herdr down, the first tick
// after a wake), change nothing. A try whose checks run again after they
// crashed on the environment (holdTry) ends with them.
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
		h = &Hold{Kind: HoldInfra, Reason: cause, Since: now, Next: now.Add(infraBackoff(0))}
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
		h.Next = now.Add(infraBackoff(h.Tries))
		e.event(ctx, 0, "hold", registry.PhaseInfo, fmt.Sprintf("infra: try %d failed (%s); tries again at %s",
			h.Tries, cause, h.Next.Local().Format("15:04")))
		return *h, true
	case e.holdTry: // checks that crashed on the environment run again as its try: it ends with them
		return *h, true
	}
	e.lift(ctx, HoldInfra, now)
	return Hold{}, false
}

// lift ends the hold under key (its kind, or a repository's, repoHoldKey),
// its blocker with it.
func (e *Engine) lift(ctx context.Context, key string, now time.Time) {
	h := e.holds[key]
	if h == nil {
		return
	}
	delete(e.holds, key)
	attrs := []any{"kind", h.Kind, "after", now.Sub(h.Since).Round(time.Second)}
	if h.Repo != "" {
		attrs = append(attrs, "repo", h.Repo)
	}
	e.log.Info("hold lifted", attrs...)
	e.event(ctx, 0, "hold", registry.PhaseOK, fmt.Sprintf("%s: lifted after %s (%s)", h.Kind,
		textx.DurationWords(now.Sub(h.Since)), h.Reason))
	e.clearBlocker(ctx, key)
}

// escalate makes an infrastructure or herdr hold that has lasted 30 minutes
// a NEEDS YOU blocker, with one alert: a repository's infrastructure hold
// too.
func (e *Engine) escalate(ctx context.Context, now time.Time) {
	for _, key := range append([]string{HoldInfra, HoldHerdr}, e.repoHolds()...) {
		h := e.holds[key]
		if h == nil || now.Sub(h.Since) < holdNeedsYou {
			continue
		}
		e.raiseBlocker(ctx, key, fmt.Sprintf("%s: %s, held since %s", h.Kind, h.Reason, h.Since.Local().Format("15:04")))
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

// holdsOrder is the order the screen names the holds in: the ones that stop
// every stage first.
var holdsOrder = []string{HoldHerdr, HoldInfra, HoldDrain, HoldDisk, HoldMemory}

// saveHolds writes the holds in force to holds.json (Layout.FactoryHolds),
// in holdsOrder, the repositories' infrastructure holds after the
// machine's, when they changed since the last write of this daemon (and on
// its first tick, so a restart clears what the last run left): the screen,
// another process, draws them. A failed write is logged and tried again on
// the next tick.
func (e *Engine) saveHolds() {
	var hs []Hold
	for _, kind := range holdsOrder {
		if h := e.holds[kind]; h != nil {
			hs = append(hs, *h)
		}
		if kind == HoldInfra {
			for _, key := range e.repoHolds() {
				hs = append(hs, *e.holds[key])
			}
		}
	}
	b, err := json.Marshal(append([]Hold{}, hs...))
	if err != nil || (e.holdsSaved && bytes.Equal(b, e.holdsWritten)) {
		return
	}
	path := e.d.Layout.FactoryHolds()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.log.Warn("save the holds for the screen", "err", err)
		return
	}
	if err := fsx.WriteFileAtomic(path, append(b, '\n'), 0o600); err != nil {
		e.log.Warn("save the holds for the screen", "err", err)
		return
	}
	e.holdsSaved, e.holdsWritten = true, b
}

// ReadHolds are the holds of the machine in force as the factory daemon
// last saved them (holds.json in layout l); none before its first tick.
func ReadHolds(l paths.Layout) ([]Hold, error) {
	b, err := os.ReadFile(l.FactoryHolds())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var hs []Hold
	if err := json.Unmarshal(b, &hs); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(l.FactoryHolds()), err)
	}
	return hs, nil
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
		if h != nil {
			e.restartQuits() // no stage ran under the hold: its quits count from the lift
		}
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
	mem := e.memoryPressure(ctx, now)
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
		if s, ok := e.servers[t.ID]; ok && idle(t) && !now.Before(s.next) { // a stop that failed waits its backoff here too
			_ = e.stopServer(ctx, t, s, "it was idle and the Mac is short on disk or memory") // its fail event says so; a later tick tries again
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

// memoryCritical starts the memory hold's reason at the kernel's critical
// pressure level, which stops a QA run (qaBounded).
const memoryCritical = "critical pressure"

// memoryPressure is the memory hold's reason ("" for none): the kernel's
// pressure level at critical, or the swap at [factory] memory_gate_swap
// percent or more, then the largest processes and the Codex brokers with
// their ages (memoryCulprits). The warn level only staggers launches (the
// runtime's).
func (e *Engine) memoryPressure(ctx context.Context, now time.Time) string {
	reason := ""
	if e.d.Memory != nil {
		s := e.d.Memory(ctx)
		if s.Level == engine.MemoryCritical {
			reason = memoryCritical + ", " + s.SwapText()
		} else if gate := e.cfg.Factory.MemoryGateSwap; gate > 0 && s.Swap != nil && s.Swap.TotalBytes > 0 {
			if pct := int(s.Swap.UsedBytes * 100 / s.Swap.TotalBytes); pct >= gate {
				reason = fmt.Sprintf("swap %d%% used, launches wait below %d%%", pct, gate)
			}
		}
	}
	if reason == "" {
		e.culprits, e.culpritsAt = "", time.Time{} // the next hold reads the table afresh
		return ""
	}
	return reason + e.memoryCulprits(ctx, now)
}

// mayLaunch reports whether a launch may start now; who and what are the
// hold that stops it, as the task's wait says it.
func (e *Engine) mayLaunch() (who, what string, ok bool) {
	if e.startsHeld() {
		return holdRestart, drainWhat, false
	}
	for _, kind := range []string{HoldHerdr, HoldInfra, HoldDisk, HoldMemory} {
		if h := e.holds[kind]; h != nil {
			return kind, h.Reason, false
		}
	}
	return "", "", true
}

// server is a copy's server the engine started for a task, the heavy slot
// it holds, and its stops that failed in a row (fails) with when the next
// try of retryServerStops is due (next), kept in memory with it.
type server struct {
	copy    Copy
	release func()
	fails   int
	next    time.Time
}

// serverLeftWords start the alert a server stop's second failure sends.
const serverLeftWords = "a QA server may still be running in "

// heavyServer is the heavy use of a task's server: bound to no stage, it
// ends when the server stops.
const heavyServer = "server"

// startServer starts a task's copy's server (Copies.StartServer): a heavy
// stage, and a launch, so it waits for both.
func (e *Engine) startServer(ctx context.Context, t registry.Task, c Copy) error {
	if who, what, ok := e.mayLaunch(); !ok {
		return heldError{who: who, what: what}
	}
	if e.d.Copies == nil {
		return heldError{who: waitCopy, what: "this build has no copies yet"}
	}
	release, ok := e.takeHeavy(ctx, t, heavyServer, "")
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

// stopServer stops a server startServer started and frees its heavy use,
// on a context of its own (a tick's end or the daemon's shutdown never cuts
// a stop). A stop that found the copy's port held by a process outside the
// copy (portHeld) stopped and forgot the factory's server all the same: it
// counts as stopped, and its event names the holder. A stop that failed
// keeps the server, which the next stop tries again: retryServerStops
// after a backoff of 1, 2, 4, 8, then 15 minutes (alertBackoff of its
// fails), and its second failure in a row sends one alert, naming the copy.
func (e *Engine) stopServer(ctx context.Context, t registry.Task, s server, why string) error {
	ctx = context.WithoutCancel(ctx)
	err := e.d.Copies.StopServer(ctx, s.copy)
	held, isHeld := errors.AsType[portHeld](err)
	if err != nil && !isHeld {
		e.log.Warn("stop a server", "task", t.ID, "copy", s.copy.Name, "err", err)
		e.event(ctx, t.ID, "server", registry.PhaseFail, "stop "+s.copy.Name+": "+err.Error())
		s.fails++
		s.next = e.now().Add(alertBackoff(s.fails))
		if _, ok := e.servers[t.ID]; ok {
			e.servers[t.ID] = s
		}
		if s.fails == 2 {
			e.alert(ctx, t.ID, fmt.Sprintf("%s: %s%s: its stop failed twice; the factory tries it again, every 15 minutes at most",
				TaskKey(t), serverLeftWords, s.copy.Name))
		}
		return fmt.Errorf("stop %s's server: %w", s.copy.Name, err)
	}
	delete(e.servers, t.ID)
	s.release()
	text := "stopped " + s.copy.Name + "'s server: " + why
	if isHeld {
		text += "; " + held.Error()
	}
	e.event(ctx, t.ID, "server", registry.PhaseOK, text)
	return nil
}

// heavySlot is a task's hold on the heavy stages: its key and the use the
// others' waits name, its uses (what runs, by the stage it was taken in; ""
// for a use no stage binds, a server), whether startup adopted it from an
// attempt that still runs, and its order. A task's uses share one slot, so
// its QA and its server count once.
type heavySlot struct {
	key, what string
	uses      map[string]string
	adopted   bool
	seq       int
}

// heavy takes a slot of the heavy stages for task t's what (a local review
// round, local QA) in its current stage: at most [factory] heavy_stages
// tasks hold one at once. A repeat call for the same use is the same hold,
// so a stage may ask on every tick. When no slot is free it returns false
// and the task waits "heavy: <key>'s <what> runs", naming the oldest
// holder. release frees the use; a use the stage never releases is freed
// when the task leaves the stage or stops being live (sweepHeavy).
func (e *Engine) heavy(ctx context.Context, t registry.Task, what string) (release func(), ok bool) {
	return e.takeHeavy(ctx, t, what, t.Stage)
}

// takeHeavy is heavy for a use bound to stage ("" = to none).
func (e *Engine) takeHeavy(ctx context.Context, t registry.Task, what, stage string) (release func(), ok bool) {
	e.heavyMu.Lock()
	s := e.heavyHeld[t.ID]
	switch {
	case s != nil && s.adopted: // the attempt that still ran takes its slot back
		s.adopted, s.what, s.uses = false, what, map[string]string{what: stage}
	case s != nil:
		s.uses[what] = stage
	case len(e.heavyHeld) >= max(1, e.cfg.Factory.HeavyStages):
		holder := e.oldestHeavy()
		e.heavyMu.Unlock()
		e.waitOn(ctx, t, "heavy", holder.key+"'s "+holder.what+" runs")
		return nil, false
	default:
		e.heavySeq++
		e.heavyHeld[t.ID] = &heavySlot{key: TaskKey(t), what: what, uses: map[string]string{what: stage}, seq: e.heavySeq}
	}
	e.heavyMu.Unlock()
	if t.WaitsWho == "heavy" {
		e.waitOn(ctx, t, "", "")
	}
	var once sync.Once
	return func() { once.Do(func() { e.releaseHeavy(t.ID, what) }) }, true
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

// releaseHeavy frees task id's use what; the slot goes with its last use.
func (e *Engine) releaseHeavy(id int64, what string) {
	e.heavyMu.Lock()
	defer e.heavyMu.Unlock()
	if s := e.heavyHeld[id]; s != nil {
		delete(s.uses, what)
		if len(s.uses) == 0 {
			delete(e.heavyHeld, id)
		}
	}
}

// freeHeavy frees everything heavy of task t, which is no longer live (it
// needs you, it stopped, it ended): its QA session ends (endQASession: its
// browser and its server stop, the Mac's QA lock goes), its server stops
// and its slot goes.
func (e *Engine) freeHeavy(ctx context.Context, t registry.Task, why string) {
	e.endQASessionLogged(ctx, t, why)
	if s, ok := e.servers[t.ID]; ok {
		_ = e.stopServer(ctx, t, s, why) // its fail event says so; the server stays for the next stop
	}
	e.heavyMu.Lock()
	delete(e.heavyHeld, t.ID)
	e.heavyMu.Unlock()
}

// sweepHeavy frees the heavy uses nothing will release: every use of a task
// that is not live (a panic sent it to you, it stopped or ended, or it is
// gone), and a live task's uses bound to a stage it has left. A stage that
// failed between its heavy and its release, or a restart that adopted an
// attempt whose stage never came back, would otherwise hold the slot for
// good, and every other task would wait on it.
func (e *Engine) sweepHeavy(ctx context.Context, tasks []registry.Task) {
	byID := make(map[int64]registry.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	var gone []registry.Task
	e.heavyMu.Lock()
	for id, s := range e.heavyHeld {
		t, ok := byID[id]
		if !ok || !isLive(t) {
			if !ok {
				t = registry.Task{ID: id}
			}
			gone = append(gone, t)
			continue
		}
		for what, stage := range s.uses {
			if stage != "" && stage != t.Stage {
				delete(s.uses, what)
			}
		}
		if len(s.uses) == 0 {
			delete(e.heavyHeld, id)
		}
	}
	e.heavyMu.Unlock()
	for _, t := range gone {
		e.freeHeavy(ctx, t, "its task is not at work")
	}
}

// isLive reports whether the loop advances t (liveStates).
func isLive(t registry.Task) bool { return slices.Contains(liveStates, t.State) }

// adoptHeavy is startup's count of the heavy stages that still run: each
// live task with a launching or running attempt of a heavy stage, or, in a
// local review, a round whose cycle is open with no result (its agents may
// still work in magnum's herdr), holds its slot until that stage takes it
// back (heavy) and releases it. A local review's attempts are its writer's
// fix turns, never heavy; a qa stage's attempts belong to a run the restart
// cut (cutQA), whose fresh run takes its slot anew. A task that is not live
// (a panic sent it to you with its attempt left running) holds none: nothing
// would release it.
func (e *Engine) adoptHeavy(ctx context.Context) error {
	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		return fmt.Errorf("factory startup: %w", err)
	}
	for _, t := range tasks {
		if !isLive(t) {
			continue
		}
		as, err := e.reg.Attempts(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("factory startup: %w", err)
		}
		for _, a := range as {
			st, ok := e.stageOf(t, a.Stage)
			if !ok || !heavyStage(st) || st.Type == config.FactoryStageReview || st.Type == config.FactoryStageQA ||
				(a.State != registry.AttemptLaunching && a.State != registry.AttemptRunning) {
				continue
			}
			e.adoptSlot(t, st)
		}
		if st, ok := e.stageOf(t, t.Stage); ok && st.Type == config.FactoryStageReview {
			cs, err := e.reg.Cycles(ctx, t.ID)
			if err != nil {
				return fmt.Errorf("factory startup: %w", err)
			}
			if n := len(cs); n > 0 && cs[n-1].Ended.IsZero() && !fsx.Exists(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reviewRoundFile(cs[n-1].N))) {
				e.adoptSlot(t, st)
			}
		}
	}
	return nil
}

// adoptSlot holds a heavy slot for task t's stage st at startup (adoptHeavy).
func (e *Engine) adoptSlot(t registry.Task, st config.FactoryStage) {
	e.heavyMu.Lock()
	defer e.heavyMu.Unlock()
	if e.heavyHeld[t.ID] == nil {
		e.heavySeq++
		what := stageWhat(st.Name)
		e.heavyHeld[t.ID] = &heavySlot{key: TaskKey(t), what: what, uses: map[string]string{what: st.Name}, adopted: true, seq: e.heavySeq}
	}
}

// heavyStage reports whether a stage is heavy: the local review loop, and
// a qa stage (its server, its browser and its walker).
func heavyStage(st config.FactoryStage) bool {
	return st.Type == config.FactoryStageReview || st.Type == config.FactoryStageQA
}
