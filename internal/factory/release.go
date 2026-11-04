package factory

// The release debt: a copy a task that ended (it stopped or is done) or
// was published still holds. A release can be refused (the copy's guard
// holds it: your changes or your console in it) or cut (the daemon
// stopped), and such a task is never advanced again, so each tick reads
// the copies' reservations
// (Copies.Reserved, from the factory's agents store) against the registry
// and tries the release again, after 1, 2, 4 and then every 5 minutes,
// until the copy is free. The debt is the stores', not the daemon's memory:
// a restart finds it. After 30 minutes one alert names the copy and why its
// release is refused; the tries go on.

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/textx"
)

// releaseAlertAfter is how long a release debt lasts before it alerts you.
const releaseAlertAfter = 30 * time.Minute

// releaseDebt is a task's copy whose release did not go through: since
// when, how many tries failed, when the next try runs, the last failure
// (each distinct one is a fail event) and whether it alerted; the loop's
// own (a restart tries at once and counts again).
type releaseDebt struct {
	since, next time.Time
	tries       int
	why         string
	alerted     bool
}

// taskEnded reports whether a task has ended: stopped or done.
func taskEnded(t registry.Task) bool {
	return t.State == registry.TaskStopped || t.State == registry.TaskDone
}

// freesCopy reports whether a task holds no copy: it ended, or it is
// published (its PR is open; milestone 1a does no more work on it).
func freesCopy(t registry.Task) bool { return taskEnded(t) || t.State == registry.TaskPublished }

// owe records a release of task t's copy that failed with err: the next
// try comes after claimBackoff.
func (e *Engine) owe(ctx context.Context, t registry.Task, copyName string, err error) {
	now := e.now()
	e.claimMu.Lock()
	d := e.debts[t.ID]
	if d == nil {
		d = &releaseDebt{since: now}
		e.debts[t.ID] = d
	}
	e.claimMu.Unlock()
	d.tries++
	d.next = now.Add(claimBackoff(d.tries))
	why := textx.Clip(textx.FirstLine(err.Error()), panicRunes)
	if why != d.why {
		d.why = why
		e.log.Warn("a copy's release was refused; it is tried again", "task", t.ID, "copy", copyName, "err", err)
		e.event(ctx, t.ID, "release", registry.PhaseFail, fmt.Sprintf("%s: %s; tried again at %s",
			cmp.Or(copyName, "its copy"), why, d.next.Local().Format("15:04")))
	}
	if !d.alerted && now.Sub(d.since) >= releaseAlertAfter && copyName != "" {
		d.alerted = true
		e.alert(ctx, t.ID, fmt.Sprintf("%s: you: %s is still reserved for it, its release refused for %s: %s",
			TaskKey(t), copyName, shortAge(now.Sub(d.since)), why))
	}
}

// payReleaseDebts tries the releases due of the copies that ended tasks
// still hold (Copies.Reserved): a task that stopped, is done or is
// published (freesCopy), whose claim does not run (sweepClaims takes
// those); a published task's copy comes off the task once released. A
// task at work or that waits on you keeps its copy, and a reservation of a
// task the registry does not have is left alone. A debt whose copy is free
// is forgotten.
func (e *Engine) payReleaseDebts(ctx context.Context, tasks []registry.Task) {
	if e.d.Copies == nil {
		return
	}
	res, err := e.d.Copies.Reserved(ctx)
	if err != nil {
		e.log.Warn("read the copies reserved", "err", err)
		return
	}
	byID := make(map[int64]registry.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	now := e.now()
	owing := map[int64]bool{}
	for _, r := range res {
		t, ok := byID[r.TaskID]
		if !ok || !freesCopy(t) {
			continue
		}
		owing[t.ID] = true
		e.claimMu.Lock()
		run := e.claims[t.ID]
		d := e.debts[t.ID]
		e.claimMu.Unlock()
		if run != nil || (d != nil && now.Before(d.next)) {
			continue
		}
		if err := e.d.Copies.Release(ctx, t, "its task "+t.State); err != nil {
			e.owe(ctx, t, r.Copy, err)
			continue
		}
		e.claimMu.Lock()
		delete(e.debts, t.ID)
		e.claimMu.Unlock()
		if t.State == registry.TaskPublished {
			e.copyReleased(ctx, t.ID)
		}
		e.event(ctx, t.ID, "release", registry.PhaseOK, fmt.Sprintf("released %s, which the %s task still held", r.Copy, t.State))
	}
	e.claimMu.Lock()
	for id := range e.debts {
		if !owing[id] {
			delete(e.debts, id) // its copy is free
		}
	}
	e.claimMu.Unlock()
}
