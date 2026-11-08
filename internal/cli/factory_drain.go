package cli

// `magnum factory install`'s guard of the work the factory daemon runs: a
// restart cuts the work it runs off its loop (a round of the local review,
// a check of a head, a claim of a copy, a talk with a liaison, a QA run), so install
// reads the record the daemon keeps of it (factory.ReadWork) and refuses
// while anything runs, unless --now; with --drain it asks the daemon for a
// drain (factory.WriteDrain) and waits until the daemon holds it and
// nothing runs, as daemon-restart and install do for the review daemon
// (daemon_drain.go).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory"
)

// factoryRounds counts the local review rounds in flight (inFlight.count).
const factoryRounds = "local review round(s)"

// factoryDrainTimeout is factory install's default --timeout: longer than a
// round of the local review may run (factory.LocalRoundCap).
const factoryDrainTimeout = factory.LocalRoundCap + 30*time.Minute

// errNoFactoryRecord is a factory daemon that keeps no record of its work:
// a build before the record, or one that could not write it.
var errNoFactoryRecord = errors.New("no record of the factory daemon's work")

// factoryDaemonPID is the pid of the running factory daemon, 0 for none: a
// pidfile whose process ps shows as something else is stale (its pid was
// reused); one ps cannot read counts as the daemon.
func factoryDaemonPID(ctx context.Context, c *Context, run execx.Runner) (int, error) {
	pid, err := daemonSys.factoryPID(c.Layout)
	if err != nil || pid == 0 {
		return 0, err
	}
	comm, args, err := engine.ProcessCommand(ctx, run, pid)
	if err == nil && !factory.LooksLikeFactoryDaemon(comm, args) {
		return 0, nil
	}
	return pid, nil
}

// factoryInFlight reads what the factory daemon pid runs that a restart
// would cut, from its record: the local review rounds and the other work
// ("precheck of PR-12345 (since 15:04)"), and the record itself (its
// drain). A record that is missing or another daemon's is
// errNoFactoryRecord: the caller cannot tell, and fails closed.
func factoryInFlight(c *Context, pid int) (inFlight, factory.WorkRecord, error) {
	rec, ok, err := factory.ReadWork(c.Layout)
	switch {
	case err != nil:
		return inFlight{}, rec, err
	case !ok || rec.PID != pid:
		return inFlight{}, rec, errNoFactoryRecord
	}
	now := time.Now()
	var out inFlight
	for _, w := range rec.Work {
		what := w.What
		if !w.Since.IsZero() {
			what += " (since " + inspClock(now, w.Since) + ")"
		}
		if w.Kind == factory.WorkRound {
			out.rounds = append(out.rounds, what)
		} else {
			out.work = append(out.work, what)
		}
	}
	return out, rec, nil
}

// factoryCutShort says what restarting the factory daemon now does to the
// work in flight, by its kinds.
func factoryCutShort(work []factory.WorkItem) string {
	var parts []string
	for _, k := range []struct{ kind, words string }{
		{factory.WorkRound, "a local review round runs again from its start (its agents' turns are lost)"},
		{factory.WorkCheck, "a proof or a precheck runs again"},
		{factory.WorkClaim, "a claim of a copy runs its steps again"},
		{factory.WorkTalk, "a talk with a liaison starts again"},
		{factory.WorkQA, "a QA run stops its server and its browser and starts again in a fresh folder"},
	} {
		if slices.ContainsFunc(work, func(w factory.WorkItem) bool { return w.Kind == k.kind }) {
			parts = append(parts, k.words)
		}
	}
	if len(parts) == 0 {
		return "stopping the factory daemon now cuts them short"
	}
	return "stopping the factory daemon now cuts them short: " + strings.Join(parts, ", ")
}

// factoryCannotTell says that what the factory daemon pid runs cannot be
// told (err: factoryInFlight's), and how to go on.
func factoryCannotTell(c *Context, pid int, err error) {
	const cut = "stopping it now may cut a local review round, a check, a claim of a copy, a talk or a QA run short"
	if errors.Is(err, errNoFactoryRecord) {
		fmt.Fprintf(c.Stderr, "magnum factory install: cannot tell what the factory daemon (pid %d) runs: it keeps no record of its work in %s "+
			"(a build before this one keeps none and takes no drain; one that could not write it removed it)\n%s\n"+
			"fix: `magnum factory status` shows what each task waits on (a round of the local review, a precheck, a copy being prepared), "+
			"then pass --now to restart it anyway\n", pid, inspTilde(c.Layout.FactoryWork()), cut)
		return
	}
	fmt.Fprintf(c.Stderr, "magnum factory install: cannot tell what the factory daemon (pid %d) runs (reading %s: %v)\n%s\n"+
		"fix: retry in a moment, or pass --now to restart it anyway\n", pid, inspTilde(c.Layout.FactoryWork()), err, cut)
}

// guardFactoryRestart is what factory install does about the work in flight
// before it reloads the job, which restarts a running factory daemon:
// refuse while anything runs (default), go on (--now) or drain (--drain).
// It returns ok false when the command stops, and lift, which lifts a drain
// it started; the caller lifts it only when the install fails, since the
// restarted daemon ends the drain it was waited for. Without a running
// factory daemon there is nothing to cut.
func guardFactoryRestart(ctx context.Context, c *Context, run execx.Runner, f restartFlags) (lift func(), ok bool) {
	noLift := func() {}
	if f.now {
		return noLift, true
	}
	pid, err := factoryDaemonPID(ctx, c, run)
	switch {
	case err != nil:
		fmt.Fprintf(c.Stderr, "magnum factory install: cannot tell whether the factory daemon runs (reading %s: %v)\n"+
			"fix: retry in a moment, or pass --now to restart it anyway\n", inspTilde(c.Layout.FactoryPid()), err)
		return noLift, false
	case pid == 0:
		return noLift, true
	case f.drain:
		return drainFactory(ctx, c, run, pid, f.timeout)
	}
	running, rec, err := factoryInFlight(c, pid)
	switch {
	case err != nil:
		factoryCannotTell(c, pid, err)
		return noLift, false
	case running.none():
		return noLift, true
	}
	fmt.Fprintf(c.Stderr, "magnum factory install: %s in flight: %s\n%s\nfix: `magnum factory install --drain` starts nothing new and "+
		"waits for these, --now interrupts them\n", running.count(factoryRounds), running, factoryCutShort(rec.Work))
	return noLift, false
}

// drainFactory asks the factory daemon pid for a drain (this command's pid
// and start in it) and wakes it: the tick that takes the drain starts
// nothing new (no launch, effect, round, check, claim or talk) and says so
// in the record. It waits until the record says the daemon holds this
// drain and nothing runs,
// printing a progress line every drainProgressEvery, for at most timeout,
// and asks again for a drain a restarted daemon ended meanwhile; a daemon
// that stopped has nothing left to cut. It returns the drain's lift, and
// false when it gave up (timeout, interrupt, a record it cannot read), with
// the drain already lifted.
func drainFactory(ctx context.Context, c *Context, run execx.Runner, pid int, timeout time.Duration) (func(), bool) {
	noLift := func() {}
	running, _, err := factoryInFlight(c, pid)
	if err != nil {
		factoryCannotTell(c, pid, err)
		return noLift, false
	}
	// The start tells this command from a later process that gets its pid.
	start, _ := execx.ProcessStart(ctx, run, os.Getpid())
	value := factory.Drain{Since: time.Now(), PID: os.Getpid(), Start: start}.Value()
	if err := factory.WriteDrain(c.Layout, value); err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory install: cannot start the drain (writing %s: %v)\nfix: retry, or pass --now to restart anyway\n",
			inspTilde(c.Layout.FactoryDrain()), err)
		return noLift, false
	}
	// The daemon takes the drain at its next tick; a wake makes that now.
	kick := func() { _, _ = daemonSys.factoryKick(c.Layout) }
	kick()
	lift := func() {
		if err := factory.LiftDrain(c.Layout, value); err != nil {
			fmt.Fprintf(c.Stderr, "magnum factory install: warning: could not lift the drain (%v); the factory daemon lifts it once this "+
				"command is gone\n", err)
		}
		kick()
	}
	out := c.Stdout
	if running.none() {
		fmt.Fprintf(out, "draining (pid %d): nothing new starts in the factory; waiting for the factory daemon to hold the drain (at most %s)\n",
			os.Getpid(), timeout)
	} else {
		fmt.Fprintf(out, "draining (pid %d): nothing new starts in the factory; waiting for %s in flight (at most %s): %s\n",
			os.Getpid(), running.count(factoryRounds), timeout, running)
	}
	var waited, sinceProgress time.Duration
	interrupted := func() (func(), bool) {
		lift()
		fmt.Fprintf(c.Stderr, "magnum factory install: interrupted after %s; the drain is lifted and the factory daemon was not restarted\n", waited)
		return noLift, false
	}
	for {
		if ctx.Err() != nil {
			return interrupted()
		}
		if waited >= timeout {
			lift()
			still := fmt.Sprintf("the factory daemon (pid %d) did not hold the drain within %s", pid, timeout)
			if !running.none() {
				still = fmt.Sprintf("%s still in flight after %s: %s", running.count(factoryRounds), timeout, running)
			}
			fmt.Fprintf(c.Stderr, "magnum factory install: %s\nthe drain is lifted and the factory daemon was not restarted\n"+
				"fix: retry with a longer --timeout, or pass --now to restart anyway (what runs starts over)\n", still)
			return noLift, false
		}
		if daemonSys.Sleep(ctx, drainPollEvery) != nil {
			return interrupted()
		}
		waited += drainPollEvery
		sinceProgress += drainPollEvery
		now, err := factoryDaemonPID(ctx, c, run)
		if err != nil {
			continue // an unreadable pidfile: try again on the next poll
		}
		if now == 0 {
			fmt.Fprintf(out, "the factory daemon stopped during the drain (after %s): nothing in flight\n", waited)
			return lift, true
		}
		pid = now
		// A daemon that started meanwhile ended the drain as the one it was
		// waited for: it is asked again.
		if v, err := factory.ReadDrain(c.Layout); err == nil && v == "" && factory.WriteDrain(c.Layout, value) == nil {
			kick()
		}
		r, rec, err := factoryInFlight(c, pid)
		if err != nil {
			continue // a record being replaced, or not yet written: the next poll
		}
		running = r
		if rec.Drain == value && running.none() {
			fmt.Fprintf(out, "drained after %s: nothing in flight\n", waited)
			return lift, true
		}
		if sinceProgress >= drainProgressEvery {
			sinceProgress = 0
			if running.none() {
				fmt.Fprintf(out, "draining: waiting for the factory daemon to hold the drain after %s\n", waited)
			} else {
				fmt.Fprintf(out, "draining: %s in flight after %s: %s\n", running.count(factoryRounds), waited, running)
			}
		}
	}
}
