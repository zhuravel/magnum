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
// release is refused; the tries go on. The checkouts of your PRs' fixes
// (fix_copies.go) are let go the same way once their fix work ended
// (payFixDebts).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
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
	e.oweIn(ctx, e.debts, t.ID, debtOwner{task: t.ID, kind: "release", who: TaskKey(t)}, copyName, err)
}

// debtOwner is whose a release debt is, as its events and its alert say:
// the task its events go to (0: the daemon's own), their kind, and its
// name.
type debtOwner struct {
	task      int64
	kind, who string
}

// oweIn records a release of copy copyName that failed with err in debts,
// under key id, for owner: each distinct failure is a fail event, the next
// try comes after claimBackoff, and after releaseAlertAfter one alert
// names the copy and why.
func (e *Engine) oweIn(ctx context.Context, debts map[int64]*releaseDebt, id int64, owner debtOwner, copyName string, err error) {
	now := e.now()
	e.claimMu.Lock()
	d := debts[id]
	if d == nil {
		d = &releaseDebt{since: now}
		debts[id] = d
	}
	e.claimMu.Unlock()
	d.tries++
	d.next = now.Add(claimBackoff(d.tries))
	why := textx.Clip(textx.FirstLine(err.Error()), panicRunes)
	if why != d.why {
		d.why = why
		e.log.Warn("a copy's release was refused; it is tried again", "task", owner.task, "of", owner.who, "copy", copyName, "err", err)
		e.event(ctx, owner.task, owner.kind, registry.PhaseFail, fmt.Sprintf("%s: %s; tried again at %s",
			cmp.Or(copyName, "its copy"), why, d.next.Local().Format("15:04")))
	}
	if !d.alerted && now.Sub(d.since) >= releaseAlertAfter && copyName != "" {
		d.alerted = true
		e.alert(ctx, owner.task, fmt.Sprintf("%s: you: %s is still reserved for it, its release refused for %s: %s",
			owner.who, copyName, shortAge(now.Sub(d.since)), why))
	}
}

// payReleaseDebts tries the releases due of the copies that ended tasks
// still hold (Copies.Reserved): a task that stopped, is done or is
// published (freesCopy), whose claim does not run (sweepClaims takes
// those), its agents parked first; a published task's copy comes off the
// task once released. A
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
		if e.d.Runtime != nil {
			// A stop or a publish the daemon stopped in the middle of may have
			// left its agents in the copy, where a release is refused for as
			// long as they live; Park does nothing to agents parked already.
			if err := e.d.Runtime.Park(ctx, t); err != nil {
				e.log.Warn("park an ended task's agents before its copy's release", "task", t.ID, "err", err)
			}
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

// fixIdleEnd is how long a PR's fix work stays idle before it has ended for
// good: a loop that ended (done, or your fix off) and that no new verdict
// queued again since. Its checkout then goes (payFixDebts).
const fixIdleEnd = 10 * time.Minute

// fixEnd is PR work whose fix ended, and why, in a release's words.
type fixEnd struct {
	w   registry.PRWork
	why string
}

// payFixDebts lets go the checkouts the PRs' fix work holds
// (Copies.FixReserved) once that work ended (fixEnded): a fix copy goes
// back to its pool clean, a per-PR checkout is removed, each with a
// fix.release event. A claim that still runs is cancelled first, and its
// checkout let go once it ended. A release refused (a pane or a guard
// holds the checkout) or cut is tried again after claimBackoff (oweFix),
// and after 30 minutes one alert names the checkout and why; the tries go
// on. The debt is the stores', not the daemon's memory: a restart finds it.
// A checkout of work that has not ended stays, whatever a restart did.
func (e *Engine) payFixDebts(ctx context.Context) {
	if e.d.Copies == nil {
		return
	}
	res, err := e.d.Copies.FixReserved(ctx)
	if err != nil {
		e.log.Warn("read the fix checkouts reserved", "err", err)
		return
	}
	ended := e.fixEnded(ctx, res)
	now := e.now()
	for _, r := range res {
		end, ok := ended[r.WorkID]
		if !ok {
			continue
		}
		e.claimMu.Lock()
		d := e.fixDebts[r.WorkID]
		e.claimMu.Unlock()
		if d != nil && now.Before(d.next) {
			continue
		}
		if !e.releaseFix(ctx, end.w, r.Checkout, end.why) {
			continue
		}
		e.claimMu.Lock()
		delete(e.fixDebts, r.WorkID)
		e.claimMu.Unlock()
		verb := "released"
		if r.PerPR {
			verb = "removed"
		}
		e.event(ctx, end.w.TaskID, "fix.release", registry.PhaseOK, fmt.Sprintf("%s %s of %s: %s", verb, r.Checkout, workKey(end.w), end.why))
	}
	e.claimMu.Lock()
	for id := range e.fixDebts {
		if _, ok := ended[id]; !ok {
			delete(e.fixDebts, id) // its checkout is free, or its work goes on
		}
	}
	e.claimMu.Unlock()
}

// fixEnded is the PR work among res whose fix ended, by id: its row is
// gone, its work went back to idle for good (fixIdleEnd), or the PR merged
// or closed, as magnum's registry says (Magnum.PRStates; a failed read
// says nothing of it). Work the registry cannot read now is left for the
// next tick.
func (e *Engine) fixEnded(ctx context.Context, res []FixReservation) map[int64]fixEnd {
	out := map[int64]fixEnd{}
	open := map[int64]registry.PRWork{}
	var refs []PRRef
	now := e.now()
	for _, r := range res {
		w, err := e.reg.PRWorkByID(ctx, r.WorkID)
		switch {
		case errors.Is(err, registry.ErrNotFound):
			out[r.WorkID] = fixEnd{w: registry.PRWork{ID: r.WorkID}, why: "its PR's fix work is gone"}
			continue
		case err != nil:
			e.log.Warn("read the PR work of a fix checkout", "work", r.WorkID, "err", err)
			continue
		case w.Phase == registry.PRWorkIdle && !now.Before(w.PhaseAt.Add(fixIdleEnd)):
			out[w.ID] = fixEnd{w: w, why: "its fix work went back to idle"}
			continue
		}
		open[w.ID] = w
		refs = append(refs, PRRef{Repo: w.Repo, Number: w.Number})
	}
	if len(refs) == 0 || e.d.Magnum == nil {
		return out
	}
	states, err := e.d.Magnum.PRStates(ctx, refs)
	if err != nil {
		e.log.Warn("read the PRs of the fix checkouts in magnum's registry", "err", err)
		return out
	}
	for id, w := range open {
		switch states[PRRef{Repo: w.Repo, Number: w.Number}].GHState {
		case store.GHMerged:
			out[id] = fixEnd{w: w, why: "the PR merged"}
		case store.GHClosed:
			out[id] = fixEnd{w: w, why: "the PR closed"}
		}
	}
	return out
}

// oweFix records a release of PR work w's checkout that failed with err
// (oweIn): the next try comes after claimBackoff.
func (e *Engine) oweFix(ctx context.Context, w registry.PRWork, checkout string, err error) {
	e.oweIn(ctx, e.fixDebts, w.ID, debtOwner{task: w.TaskID, kind: "fix.release", who: workKey(w)}, checkout, err)
}
