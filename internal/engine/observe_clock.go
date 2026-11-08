package engine

import (
	"context"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Log messages of the observe passes.
const (
	observePassMsg  = "observe pass"
	observeSlowMsg  = "an observe pass took longer than observe_interval"
	observeWaitsMsg = "observe pass waits for the one in flight"
)

// observeMinWait is the shortest wait of the observe clock before the
// second idle reading a pass found due (Observation.SecondReadingAt): a
// pass slower than the gap never makes it read herdr back to back.
const observeMinWait = time.Second

// observeSlowEvery is how often a slow observe pass warns at most.
const observeSlowEvery = time.Minute

// observeClockEvery is the observe clock's interval ([daemon]
// observe_interval), 0 when it does not run: the key is "0", a dry run (it
// changes nothing outside the store, and its tick observes nothing either),
// or an engine without herdr or agents.
func (e *Engine) observeClockEvery() time.Duration {
	if e.d.DryRun || e.d.Herdr == nil || e.d.Agents == nil {
		return 0
	}
	return e.cfg.Daemon.ObserveInterval.Duration
}

// goObserveClock starts the observe clock (observeClock) on a goroutine of
// its own when it runs (observeClockEvery) and returns a channel closed
// once it stopped, with ctx.
func (e *Engine) goObserveClock(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	every := e.observeClockEvery()
	if every <= 0 {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		e.observeClock(ctx, every)
	}()
	return done
}

// observeClock is the review daemon's observe clock: until ctx ends it waits
// every (Deps.ObserveSleep) and runs an observe pass (observeRuns), outside
// the tick, whose GitHub poll paced the observation before (a turn's end
// was seen 72 s after the agent stopped, median). After a pass that found a
// turn's second idle reading due sooner, it waits only until then.
func (e *Engine) observeClock(ctx context.Context, every time.Duration) {
	sleep := e.d.ObserveSleep
	if sleep == nil {
		sleep = sleepCtx
	}
	wait := every
	for {
		if err := sleep(ctx, wait); err != nil {
			return
		}
		wait = every
		e.safely(ctx, "observe clock", "", func() { wait = e.observeRuns(ctx, every) }, nil)
	}
}

// observeRuns is one pass of the observe clock and returns how long the
// clock waits for the next: a herdr snapshot fed to the agents' observation
// and its observations handled as the tick handles its own (onObservation),
// under the observe lock the tick takes too. With no run in flight there is
// no turn to end and the pass reads nothing, so an idle daemon makes no
// socket call for it; herdr's being up or down is the tick's to say. Each
// pass logs its duration at debug level, and a pass that took longer than
// every warns, at most once per observeSlowEvery.
func (e *Engine) observeRuns(ctx context.Context, every time.Duration) time.Duration {
	if !e.runsInFlight(ctx) {
		return every
	}
	defer e.lockObserve("the observe clock")()
	start := e.now()
	snap, err := e.d.Herdr.Snapshot(ctx)
	if err != nil {
		e.log.Debug("observe clock: no herdr snapshot; the tick says whether herdr is down", "err", err)
		return every
	}
	obs, err := e.d.Agents.ObserveSnapshotAt(ctx, snap, start)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "observe agents")
	}
	for _, o := range obs {
		e.onObservation(ctx, o)
	}
	end := e.now()
	next := every
	for _, o := range obs {
		if !o.SecondReadingAt.IsZero() {
			next = min(next, max(o.SecondReadingAt.Sub(end), observeMinWait))
		}
	}
	took := end.Sub(start)
	e.log.Debug(observePassMsg, "took", took, "sessions", len(obs), "next", next)
	if took > every && (e.observeSlowAt.IsZero() || end.Sub(e.observeSlowAt) >= observeSlowEvery) {
		e.observeSlowAt = end
		e.log.Warn(observeSlowMsg, "took", took, "observe_interval", every)
	}
	return next
}

// runsInFlight reports whether a run of a session is submitted or at
// work: what an observe pass can end. A failed read says no: the tick
// still observes, and says what failed.
func (e *Engine) runsInFlight(ctx context.Context) bool {
	runs, err := e.st.ActiveRuns(ctx)
	if err != nil {
		e.log.Debug("observe clock: no runs read; the pass waits for the next", "err", err)
		return false
	}
	for _, r := range runs {
		if r.SessionID != nil && (r.State == store.RunSubmitted || r.State == store.RunWorking) {
			return true
		}
	}
	return false
}

// lockObserve takes the observe lock (observeMu) for pass, the tick or the
// observe clock, saying at debug level when it waits for the other's pass
// in flight, and returns its unlock. One snapshot is applied at a time, in
// the order they were taken: a pass that read herdr before another but
// applied it after would record a status older than the row's.
func (e *Engine) lockObserve(pass string) func() {
	if !e.observeMu.TryLock() {
		e.log.Debug(observeWaitsMsg, "pass", pass)
		e.observeMu.Lock()
	}
	return e.observeMu.Unlock
}
