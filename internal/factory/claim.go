package factory

// A stage's claim of its task's copy (spec 7.1). A claim can run a copy's
// setup, a database reset and the installs for most of an hour, and the
// loop is sequential: the claim runs off the loop, in a goroutine of its
// own on the engine's context, while the task waits at no cost, "copy:
// preparing <copy> (setup <elapsed>)", and a later tick picks the copy up.
// A task claims its copy once and keeps it until its release: a claim of
// the copy it holds would run the claim protocol again, an install in the
// middle of its work among it. Every claim's begin event ends with an ok or
// a fail one. A stop of the task cancels its claim (its commands end with
// the context) and the copy the claim reserved is released once it ended;
// a drain cancels every claim and waits for them, and closes their begins.
// A daemon killed during a claim leaves the task waiting on the copy, a
// claim event that began and never ended, and the claim's steps of the
// agents store as far as they got (each resumes after its guard, a done one
// never runs again): the next claim of the task picks up from there, and
// the startup's recovery reads that state.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/textx"
)

// claimRun is a claim of a copy for a task that runs off the loop: when it
// started, done closed when it ended, cancel to end it, the wait it keeps
// while it runs (a retry's: the reason of the claim before it; "" for a
// first claim, which waits "preparing"), the waits before it, and its
// result (written before done closes).
type claimRun struct {
	started  time.Time
	done     chan struct{}
	cancel   context.CancelFunc
	waitWhat string
	tries    int
	copy     Copy
	err      error
}

// ended reports whether the claim's goroutine has ended.
func (r *claimRun) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// claimWait is a task's claim that ended without a copy and costs it
// nothing (no copy free, a guard's hold, a factory pane in the copy): its
// reason, how many such waits came in a row, when the next claim may run
// (claimBackoff), and the held error the stage gets until then.
type claimWait struct {
	what  string
	tries int
	next  time.Time
	err   error
}

// The tries of a claim that waits: after 1, 2 and 4 minutes, then every 5.
const (
	claimRetryFirst = time.Minute
	claimRetryMax   = 5 * time.Minute
	claimRetryShift = 3 // 1m<<3 is past claimRetryMax
)

// claimBackoff is the wait after a claim's tries waits in a row.
func claimBackoff(tries int) time.Duration {
	return min(claimRetryFirst<<min(max(tries-1, 0), claimRetryShift), claimRetryMax)
}

// drainWait bounds how long a drain waits for the work off the loop to end
// once it is cancelled (execx kills a process group within its grace).
const drainWait = 30 * time.Second

// claimCopy is how a stage gets its task's copy. The copy the task holds
// comes at once. Otherwise the first call starts a claim off the loop
// (never while the daemon drains or a hold of the machine holds launches)
// and every call while it runs returns a held error: the task waits
// "copy: preparing <copy> (setup <elapsed>)". The call after the claim
// ended takes its result: the copy, noted on the task (its copy wait
// cleared), or the claim's error. No free copy ("copy: all 3 busy"), a
// guard that holds the copy (your agent, console or changes in it:
// "copy: held: <reason>: <detail>") and a factory pane in it are waits that
// cost the task nothing: the task keeps that wait, and the next claim runs
// after claimBackoff (1, 2, 4, then every 5 minutes), keeping the wait while
// it runs. Any other error is the task's failure.
func (e *Engine) claimCopy(ctx context.Context, t registry.Task) (Copy, error) {
	e.claimMu.Lock()
	if c, ok := e.held[t.ID]; ok {
		// A task read before its claim was noted has no copy yet: it is
		// the same task, and its copy is the one held.
		if t.Copy == "" || c.Name == t.Copy {
			e.claimMu.Unlock()
			return c, nil
		}
		delete(e.held, t.ID) // another copy was noted on the task meanwhile
	}
	run := e.claims[t.ID]
	ended := run != nil && run.ended()
	if ended {
		delete(e.claims, t.ID)
	}
	w := e.claimWaits[t.ID]
	e.claimMu.Unlock()
	switch {
	case run != nil && !ended:
		return Copy{}, e.preparing(ctx, t, run)
	case run != nil:
		return e.claimed(ctx, t, run)
	case w != nil && e.now().Before(w.next):
		e.waitOn(ctx, t, waitCopy, w.what)
		return Copy{}, w.err
	}
	return Copy{}, e.beginClaim(ctx, t, w)
}

// beginClaim starts task t's claim off the loop (Copies.Claim), after the
// wait w of the claim before it (nil for a first claim): at most [factory]
// max_active claims run at once, as no more tasks are at work. The claim
// runs on a context of its own under the engine's (a stop of the task or
// a drain ends it).
func (e *Engine) beginClaim(ctx context.Context, t registry.Task, w *claimWait) error {
	if who, what, ok := e.mayLaunch(); !ok {
		if who != holdRestart {
			e.waitOn(ctx, t, who, what)
		}
		return heldError{who: who, what: what}
	}
	if e.d.Copies == nil {
		return heldError{who: waitCopy, what: "this build has no copies yet"}
	}
	cctx, cancel := context.WithCancel(e.bgCtx)
	run := &claimRun{started: e.now(), done: make(chan struct{}), cancel: cancel}
	text := "claims a copy, off the loop"
	if w != nil {
		run.waitWhat, run.tries = w.what, w.tries
		text = fmt.Sprintf("claims a copy again (try %d after: %s)", w.tries+1, w.what)
	}
	e.claimMu.Lock()
	if n := len(e.claims); n >= max(1, e.cfg.Factory.MaxActive) {
		e.claimMu.Unlock()
		cancel()
		what := fmt.Sprintf("%d claims run, at most max_active", n)
		e.waitOn(ctx, t, waitCopy, what)
		return heldError{who: waitCopy, what: what}
	}
	e.claims[t.ID] = run
	e.claimMu.Unlock()
	e.event(ctx, t.ID, "claim", registry.PhaseBegin, text)
	e.bg.Go(func() {
		defer close(run.done)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				e.log.Error("panic in a claim; the daemon goes on", "task", t.ID, "panic", fmt.Sprint(r),
					"stack", execx.Redact(string(debug.Stack())))
				run.copy, run.err = Copy{}, fmt.Errorf("the claim panicked: %v", r)
			}
		}()
		run.copy, run.err = e.d.Copies.Claim(cctx, t)
	})
	return e.preparing(ctx, t, run)
}

// preparing is the wait of a task whose claim runs: a retry keeps the
// reason the claim before it waited on, a first claim says it prepares.
func (e *Engine) preparing(ctx context.Context, t registry.Task, run *claimRun) error {
	what := run.waitWhat
	if what == "" {
		what = fmt.Sprintf("preparing %s (setup %s)", cmp.Or(t.Copy, "a copy"), setupAge(e.now().Sub(run.started)))
	}
	e.waitOn(ctx, t, waitCopy, what)
	return heldError{who: waitCopy, what: what}
}

// setupAge is how long a claim ran, in whole minutes ("<1m", "3m", "1h5m"),
// so the wait changes at most once a minute.
func setupAge(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	return shortAge(d.Truncate(time.Minute))
}

// claimed takes the result of task t's claim that ended.
func (e *Engine) claimed(ctx context.Context, t registry.Task, run *claimRun) (Copy, error) {
	if run.err != nil {
		return Copy{}, e.claimFailed(ctx, t, run)
	}
	c := run.copy
	if t.Copy != c.Name || t.WaitsWho == waitCopy {
		if err := e.reg.TransitionTask(ctx, t.ID, []string{t.State}, "", func(u *registry.TaskUpdate) {
			u.Set("copy", c.Name)
			if t.WaitsWho == waitCopy {
				u.Set("waits_who", "")
				u.Set("waits_what", "")
			}
		}); err != nil {
			// The copy stays the claim's: the next call takes it again.
			e.claimMu.Lock()
			e.claims[t.ID] = run
			e.claimMu.Unlock()
			return Copy{}, fmt.Errorf("note copy %s on %s: %w", c.Name, TaskKey(t), err)
		}
	}
	e.claimMu.Lock()
	e.held[t.ID] = c
	delete(e.claimWaits, t.ID)
	e.claimMu.Unlock()
	e.event(ctx, t.ID, "claim", registry.PhaseOK, fmt.Sprintf("claimed %s in %s", c.Name, setupAge(e.now().Sub(run.started))))
	return c, nil
}

// claimFailed maps the error of a claim that ended: the waits that cost
// nothing (no copy free, a guard's hold, a factory pane in the copy), which
// the task keeps until the next claim after claimBackoff, and the task's
// failure. Either ends the claim's begin with a fail event.
func (e *Engine) claimFailed(ctx context.Context, t registry.Task, run *claimRun) error {
	err := run.err
	what, held := "", err
	if nc, ok := errors.AsType[noCopy](err); ok {
		what = nc.what
	} else if h, ok := slots.AsHold(err); ok {
		what = "held: " + h.Reason
		if h.Detail != "" {
			what += ": " + h.Detail
		}
	} else if errors.Is(err, ErrPaneInCopy) {
		what = "a pane runs in it"
		if _, detail, ok := strings.Cut(err.Error(), ErrPaneInCopy.Error()+": "); ok {
			what += ": " + detail
		}
	}
	if what == "" {
		e.claimMu.Lock()
		delete(e.claimWaits, t.ID)
		e.claimMu.Unlock()
		e.event(ctx, t.ID, "claim", registry.PhaseFail, err.Error())
		return err
	}
	if !errors.Is(held, ErrHeld) {
		held = heldError{who: waitCopy, what: what}
	}
	w := &claimWait{what: what, tries: run.tries + 1, err: held}
	w.next = e.now().Add(claimBackoff(w.tries))
	e.claimMu.Lock()
	e.claimWaits[t.ID] = w
	e.claimMu.Unlock()
	e.event(ctx, t.ID, "claim", registry.PhaseFail, fmt.Sprintf("no copy yet: %s; claims again at %s", what, w.next.Local().Format("15:04")))
	e.waitOn(ctx, t, waitCopy, what)
	return held
}

// heldCopy is the copy task id holds, as its claim handed it over.
func (e *Engine) heldCopy(id int64) (Copy, bool) {
	e.claimMu.Lock()
	defer e.claimMu.Unlock()
	c, ok := e.held[id]
	return c, ok
}

// releaseCopy releases the copy task t holds, or the one a claim of it
// reserved (Copies.Release finds it by the task's owner row: a claim that
// failed or waits keeps its reservation for the task's next claim, and the
// task's row names no copy; none is no error), and forgets the task's
// claims. A claim that still runs is cancelled instead (its commands end
// with it): sweepClaims releases its copy once it ended.
func (e *Engine) releaseCopy(ctx context.Context, t registry.Task, reason string) {
	e.claimMu.Lock()
	run := e.claims[t.ID]
	running := run != nil && !run.ended()
	if !running {
		delete(e.claims, t.ID)
	}
	delete(e.held, t.ID)
	delete(e.claimWaits, t.ID)
	e.claimMu.Unlock()
	if running {
		run.cancel()
		return
	}
	if run != nil { // it ended, and nobody took its answer
		e.endClaim(ctx, t, run, "the task left first")
	}
	if e.d.Copies == nil {
		return
	}
	if err := e.d.Copies.Release(ctx, t, reason); err != nil {
		e.log.Warn("release a task's copy", "task", t.ID, "err", err)
		e.event(ctx, t.ID, "release", registry.PhaseFail, err.Error())
	}
}

// endClaim ends the begin of a claim whose answer no stage took, saying
// why.
func (e *Engine) endClaim(ctx context.Context, t registry.Task, run *claimRun, why string) {
	if run.err == nil {
		e.event(ctx, t.ID, "claim", registry.PhaseOK, fmt.Sprintf("claimed %s, but %s", run.copy.Name, why))
		return
	}
	e.event(ctx, t.ID, "claim", registry.PhaseFail, fmt.Sprintf("ended (%s), and %s",
		textx.Clip(textx.FirstLine(run.err.Error()), panicRunes), why))
}

// waitBackground waits for the work off the loop to end (a test's; the
// daemon waits only in stopBackground).
func (e *Engine) waitBackground() { e.bg.Wait() }

// goBackground runs fn off the loop on the engine's context, which a drain
// cancels and waits for; a panic in fn is logged, never the daemon's.
func (e *Engine) goBackground(what string, fn func(ctx context.Context)) {
	e.bg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				e.log.Error("panic off the loop; the daemon goes on", "work", what, "panic", fmt.Sprint(r),
					"stack", execx.Redact(string(debug.Stack())))
			}
		}()
		fn(e.bgCtx)
	})
}

// stopBackground ends the work off the loop: it cancels the engine's
// context (a claim's commands end with it), waits up to drainWait for the
// work to end, and ends the begin of each claim no stage took the answer
// of. Run calls it as it returns; a second call does nothing new.
func (e *Engine) stopBackground() {
	e.bgCancel()
	done := make(chan struct{})
	go func() {
		e.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drainWait):
		e.log.Warn("work off the loop did not end within the drain's wait; the daemon stops anyway", "wait", drainWait)
	}
	e.claimMu.Lock()
	runs := e.claims
	e.claims = map[int64]*claimRun{}
	e.claimMu.Unlock()
	ctx := context.Background() // the engine's is cancelled
	for id, run := range runs {
		if !run.ended() {
			e.event(ctx, id, "claim", registry.PhaseFail, "the daemon stopped during the claim: the next claim resumes its steps")
			continue
		}
		e.endClaim(ctx, registry.Task{ID: id}, run, "the daemon stopped before a stage took it: the next claim resumes it")
	}
}

// sweepClaims releases the copy a claim reserved for a task that ended
// meanwhile (it stopped or is done, or is gone), whatever the claim's
// result, once the claim has ended: nothing else would. A task that waits
// on you keeps its claim for when it goes on.
func (e *Engine) sweepClaims(ctx context.Context, tasks []registry.Task) {
	byID := make(map[int64]registry.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	type ended struct {
		t   registry.Task
		run *claimRun
	}
	var gone []ended
	e.claimMu.Lock()
	for id, run := range e.claims {
		t, ok := byID[id]
		if ok && t.State != registry.TaskDone && t.State != registry.TaskStopped {
			continue
		}
		if !run.ended() {
			continue // it ends first
		}
		delete(e.claims, id)
		delete(e.claimWaits, id)
		if !ok {
			t = registry.Task{ID: id}
		}
		gone = append(gone, ended{t, run})
	}
	e.claimMu.Unlock()
	for _, g := range gone {
		e.endClaim(ctx, g.t, g.run, "its task ended meanwhile")
		if e.d.Copies == nil {
			continue
		}
		if err := e.d.Copies.Release(ctx, g.t, "its task ended during the claim"); err != nil {
			e.log.Warn("release the copy of a task that ended during its claim", "task", g.t.ID, "err", err)
			e.event(ctx, g.t.ID, "release", registry.PhaseFail, err.Error())
			continue
		}
		e.event(ctx, g.t.ID, "claim", registry.PhaseInfo, "released what the claim reserved: the task ended during it")
	}
}
