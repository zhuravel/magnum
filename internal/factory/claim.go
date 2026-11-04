package factory

// A stage's claim of its task's copy (spec 7.1). A claim can run a copy's
// setup, a database reset and the installs for most of an hour, and the
// loop is sequential: the claim runs off the loop, in a goroutine of its
// own, while the task waits at no cost, "copy: preparing <copy> (setup
// <elapsed>)", and a later tick picks the copy up. A task claims its copy
// once and keeps it until its release: a claim of the copy it holds would
// run the claim protocol again, an install in the middle of its work among
// it. A daemon that stops during a claim leaves the task waiting on the
// copy with a claim event that began and never ended, and the claim's
// steps of the agents store as far as they got (each step resumes after
// its guard, a done one never runs again); the next claim of the task
// picks up from there, and the startup's recovery reads that state.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/slots"
)

// claimRun is a claim of a copy for a task that runs off the loop: when it
// started, done closed when it ended, and its result (written before done
// closes).
type claimRun struct {
	started time.Time
	done    chan struct{}
	copy    Copy
	err     error
}

// claimCopy is how a stage gets its task's copy. The copy the task holds
// comes at once. Otherwise the first call starts a claim off the loop
// (never while the daemon drains or a hold of the machine holds launches)
// and every call while it runs returns a held error: the task waits
// "copy: preparing <copy> (setup <elapsed>)". The call after the claim
// ended takes its result: the copy, noted on the task (its copy wait
// cleared), or the claim's error. No free copy ("copy: all 3 busy"), a
// guard that holds the copy (your agent, console or changes in it:
// "copy: held: <reason>: <detail>") and a factory pane in it are waits that
// cost the task nothing, and the next call claims again; any other error is
// the task's failure.
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
	ended := false
	if run != nil {
		select {
		case <-run.done:
			ended = true
			delete(e.claims, t.ID)
		default:
		}
	}
	e.claimMu.Unlock()
	switch {
	case run == nil:
		return Copy{}, e.beginClaim(ctx, t)
	case !ended:
		return Copy{}, e.preparing(ctx, t, run)
	}
	return e.claimed(ctx, t, run)
}

// beginClaim starts task t's claim off the loop (Copies.Claim): at most
// [factory] max_active claims run at once, as no more tasks are at work.
// The claim runs on ctx, the daemon's: it ends with the daemon.
func (e *Engine) beginClaim(ctx context.Context, t registry.Task) error {
	if who, what, ok := e.mayLaunch(); !ok {
		if who != holdRestart {
			e.waitOn(ctx, t, who, what)
		}
		return heldError{who: who, what: what}
	}
	if e.d.Copies == nil {
		return heldError{who: waitCopy, what: "this build has no copies yet"}
	}
	run := &claimRun{started: e.now(), done: make(chan struct{})}
	e.claimMu.Lock()
	if n := len(e.claims); n >= max(1, e.cfg.Factory.MaxActive) {
		e.claimMu.Unlock()
		what := fmt.Sprintf("%d claims run, at most max_active", n)
		e.waitOn(ctx, t, waitCopy, what)
		return heldError{who: waitCopy, what: what}
	}
	e.claims[t.ID] = run
	e.claimMu.Unlock()
	e.event(ctx, t.ID, "claim", registry.PhaseBegin, "claims a copy, off the loop")
	e.claimWG.Go(func() {
		defer close(run.done)
		defer func() {
			if r := recover(); r != nil {
				run.copy, run.err = Copy{}, fmt.Errorf("the claim panicked: %v", r)
			}
		}()
		run.copy, run.err = e.d.Copies.Claim(ctx, t)
	})
	return e.preparing(ctx, t, run)
}

// preparing is the wait of a task whose claim runs.
func (e *Engine) preparing(ctx context.Context, t registry.Task, run *claimRun) error {
	what := fmt.Sprintf("preparing %s (setup %s)", cmp.Or(t.Copy, "a copy"), setupAge(e.now().Sub(run.started)))
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
		return Copy{}, e.claimFailed(ctx, t, run.err)
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
	e.claimMu.Unlock()
	e.event(ctx, t.ID, "claim", registry.PhaseOK, fmt.Sprintf("claimed %s in %s", c.Name, setupAge(e.now().Sub(run.started))))
	return c, nil
}

// claimFailed maps a claim's error: the waits that cost nothing (no copy
// free, a guard's hold, a factory pane in the copy) and the task's failure.
func (e *Engine) claimFailed(ctx context.Context, t registry.Task, err error) error {
	if nc, ok := errors.AsType[noCopy](err); ok {
		e.waitOn(ctx, t, waitCopy, nc.what)
		return err
	}
	what := ""
	if h, ok := slots.AsHold(err); ok {
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
	if what != "" {
		if t.WaitsWhat != what {
			e.event(ctx, t.ID, "claim", registry.PhaseInfo, "waits: "+what)
		}
		e.waitOn(ctx, t, waitCopy, what)
		return heldError{who: waitCopy, what: what}
	}
	e.event(ctx, t.ID, "claim", registry.PhaseFail, err.Error())
	return err
}

// heldCopy is the copy task id holds, as its claim handed it over.
func (e *Engine) heldCopy(id int64) (Copy, bool) {
	e.claimMu.Lock()
	defer e.claimMu.Unlock()
	c, ok := e.held[id]
	return c, ok
}

// forgetCopy forgets the copy of task id, which released it.
func (e *Engine) forgetCopy(id int64) {
	e.claimMu.Lock()
	defer e.claimMu.Unlock()
	delete(e.held, id)
}

// waitClaims waits for the claims that run to end (a test's; the daemon
// never waits for one).
func (e *Engine) waitClaims() { e.claimWG.Wait() }

// sweepClaims releases the copy a claim handed to a task that ended
// meanwhile (it stopped or is done, or is gone): nothing else would. A task
// that waits on you keeps its claim for when it goes on.
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
		select {
		case <-run.done:
		default:
			continue // it ends first
		}
		delete(e.claims, id)
		if !ok {
			t = registry.Task{ID: id}
		}
		gone = append(gone, ended{t, run})
	}
	e.claimMu.Unlock()
	for _, g := range gone {
		if g.run.err != nil || e.d.Copies == nil {
			continue
		}
		t := g.t
		t.Copy = g.run.copy.Name
		if err := e.d.Copies.Release(ctx, t, "its task ended during the claim"); err != nil {
			e.log.Warn("release the copy of a task that ended during its claim", "task", t.ID, "copy", t.Copy, "err", err)
			e.event(ctx, t.ID, "release", registry.PhaseFail, err.Error())
			continue
		}
		e.event(ctx, t.ID, "claim", registry.PhaseInfo, "released "+t.Copy+": the task ended during the claim")
	}
}
