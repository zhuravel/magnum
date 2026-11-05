package factory

// The startup's recovery (spec section 10): what a daemon that stopped,
// drained or crashed left is settled before the first tick, in one order,
// each step under its own guard and in the event log. A copy a base run
// left detached is restored first (resumeDetached), then the servers a
// stopped daemon left are ended (Copies.Reconcile), a stop or a publish cut
// short is finished and its copy released, a claim a crash cut has its
// begin ended (its next claim runs its steps again), the sessions are bound
// to the panes herdr shows again and the workspaces no row names are
// adopted or closed (the runtime's Recover and SettleWorkspaces; what
// fails runs again after a backoff, on a tick herdr answers), a local round
// a restart cut is said to run again, each prompt of unknown delivery is
// read once (a yes is watched; any other is sent to nobody, its task waits
// on it, and a no is proven only by a later read, which sends it once),
// the effects in flight are named (their stage runs each one's guard again
// before any step), and last the heavy stages that still run take their
// slots back (adoptHeavy).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/textx"
)

// startup runs once before the first tick: the recovery steps of the
// file's comment, in their order. Only a registry it cannot read stops the
// daemon; every other step records its own failure and the daemon starts.
func (e *Engine) startup(ctx context.Context) error {
	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		return fmt.Errorf("factory startup: %w", err)
	}
	for _, t := range tasks {
		err := e.safely(ctx, t, func(ctx context.Context) error { return e.resumeDetached(ctx, t) })
		if err != nil && !errors.Is(err, ErrHeld) && !errors.Is(err, errPanicked) {
			e.log.Warn("restore a copy a base run left detached", "task", t.ID, "err", err)
		}
	}
	if e.d.Copies != nil {
		if err := e.d.Copies.Reconcile(ctx); err != nil {
			e.log.Warn("reconcile the copies' servers", "err", err)
			e.event(ctx, 0, "server", registry.PhaseFail, "reconcile: "+err.Error())
		}
	}
	e.finishEnds(ctx, tasks)
	e.closeCutClaims(ctx, tasks)
	e.recoverAgents(ctx, tasks)
	e.cutRounds(ctx, tasks)
	e.reconcileDeliveries(ctx)
	e.openEffects(ctx)
	return e.adoptHeavy(ctx)
}

// finishEnds finishes what a stop, a drop or a publish began when the
// daemon stopped in its middle (endTask, finishPublish; their release of
// the copy comes last): a stopped or done task that still holds a copy
// (Copies.Reserved) has its open attempts end failed, and every task that
// frees its copy has its agents parked and its copy released now
// (payReleaseDebts), not one tick later.
func (e *Engine) finishEnds(ctx context.Context, tasks []registry.Task) {
	if e.d.Copies == nil {
		return
	}
	res, err := e.d.Copies.Reserved(ctx)
	if err != nil {
		e.log.Warn("read the copies reserved at the startup", "err", err)
		return
	}
	held := map[int64]bool{}
	for _, r := range res {
		held[r.TaskID] = true
	}
	for _, t := range tasks {
		if taskEnded(t) && held[t.ID] {
			e.failOpenAttempts(ctx, t, "its task "+t.State)
		}
	}
	e.payReleaseDebts(ctx, tasks)
}

// failOpenAttempts ends task t's attempts that launch, run or ended
// unverified as failed, because.
func (e *Engine) failOpenAttempts(ctx context.Context, t registry.Task, because string) {
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		e.log.Warn("read an ended task's attempts", "task", t.ID, "err", err)
		return
	}
	for _, a := range as {
		switch a.State {
		case registry.AttemptLaunching, registry.AttemptRunning, registry.AttemptEnded:
			if err := e.reg.TransitionAttempt(ctx, a.ID, []string{a.State}, registry.AttemptFailed, func(u *registry.AttemptUpdate) {
				u.Set("error", because)
			}); err != nil {
				e.log.Warn("end an ended task's attempt", "attempt", a.ID, "err", err)
			}
		}
	}
}

// claimEventsRead bounds the events read for a claim a crash cut.
const claimEventsRead = 200

// closeCutClaims ends with a fail event the claim a crash cut: the newest
// claim event of a task at work or waiting on you is a begin (a drain ends
// every claim it cancels, stopBackground; a crash ends none). The task's
// next claim runs the claim's steps again after their guards; a task that
// ended has its copy released (payReleaseDebts).
func (e *Engine) closeCutClaims(ctx context.Context, tasks []registry.Task) {
	for _, t := range tasks {
		if !isLive(t) && t.State != registry.TaskNeedsYou {
			continue
		}
		evs, err := e.reg.Events(ctx, t.ID, claimEventsRead)
		if err != nil {
			e.log.Warn("read a task's claim events at the startup", "task", t.ID, "err", err)
			continue
		}
		for _, ev := range slices.Backward(evs) { // the newest claim event
			if ev.Kind != "claim" {
				continue
			}
			if ev.Phase == registry.PhaseBegin {
				e.event(ctx, t.ID, "claim", registry.PhaseFail, "the daemon stopped during the claim (a crash): the next claim runs its steps again")
			}
			break
		}
	}
}

// agentsRecovery is what the agents' recovery has left to do after a
// failure (herdr that does not answer, a store that refuses): the tasks
// Recover failed for, whether SettleWorkspaces runs again, when the next try
// runs (claimBackoff after each failure in a row, and only on a tick herdr
// answers), and the cause last told (each distinct one is one fail event).
// The loop's own.
type agentsRecovery struct {
	tasks  map[int64]bool
	settle bool
	next   time.Time
	tries  int
	why    string
}

// recoverAgents binds the sessions of every task whose agents may live (at
// work, waiting, or waiting on you) to the panes herdr shows again
// (Runtime.Recover), then settles the workspaces a crash left that no row
// names (Runtime.SettleWorkspaces): one its attempt's launch adopts, the
// others closed. Each is in the event log. What fails runs again later, and
// only that (retryRecovery).
func (e *Engine) recoverAgents(ctx context.Context, tasks []registry.Task) {
	e.runRecovery(ctx, tasks, nil)
}

// retryRecovery runs again, once its try is due, what the agents' recovery
// failed for; the caller runs it on a tick herdr answers.
func (e *Engine) retryRecovery(ctx context.Context, tasks []registry.Task) {
	if r := e.recovery; r != nil && !e.now().Before(r.next) {
		e.runRecovery(ctx, tasks, r)
	}
}

// runRecovery is recoverAgents over every task (left nil) or over what left
// says failed before.
func (e *Engine) runRecovery(ctx context.Context, tasks []registry.Task, left *agentsRecovery) {
	if e.d.Runtime == nil {
		return
	}
	failed := map[int64]bool{}
	var cause error
	for _, t := range tasks {
		if (!isLive(t) && t.State != registry.TaskNeedsYou) || (left != nil && !left.tasks[t.ID]) {
			continue
		}
		changed, err := e.d.Runtime.Recover(ctx, t)
		if err != nil {
			failed[t.ID], cause = true, cmpErr(cause, err)
			continue
		}
		if len(changed) > 0 {
			e.event(ctx, t.ID, "recover", registry.PhaseInfo, "the agents after the restart: "+strings.Join(changed, ", "))
		}
	}
	settleFailed := false
	if left == nil || left.settle {
		orphans, err := e.d.Runtime.SettleWorkspaces(ctx)
		if err != nil {
			settleFailed, cause = true, cmpErr(cause, err)
		}
		for _, o := range orphans {
			if o.Adopted {
				e.event(ctx, o.TaskID, "workspace", registry.PhaseInfo, fmt.Sprintf("workspace %q, which no row names (a launch the daemon "+
					"stopped in), is adopted by its attempt's launch", o.Label))
				continue
			}
			e.event(ctx, o.TaskID, "workspace", registry.PhaseOK, fmt.Sprintf("closed workspace %q, which no row names: %s", o.Label, o.Why))
		}
	}
	if cause == nil {
		if left != nil {
			e.event(ctx, 0, "recover", registry.PhaseOK, "the agents' recovery went through")
		}
		e.recovery = nil
		return
	}
	r := left
	if r == nil {
		r = &agentsRecovery{}
	}
	r.tasks, r.settle = failed, settleFailed
	r.tries++
	r.next = e.now().Add(claimBackoff(r.tries))
	e.recovery = r
	if why := textx.Clip(textx.FirstLine(execx.Redact(cause.Error())), 300); why != r.why {
		r.why = why
		e.log.Warn("the agents' recovery failed; what failed runs again", "err", cause)
		e.event(ctx, 0, "recover", registry.PhaseFail, fmt.Sprintf("the agents' recovery runs again at %s for what failed (once herdr "+
			"answers): %s", r.next.Local().Format("15:04"), why))
	}
}

// cutRounds names the local rounds a restart cut: a task at its review
// stage whose newest cycle is open with no result (local-review-<n>.json)
// runs that round again in the cycle (goOnCycle), as the same round of the
// loop, so with the same synthetic PR number, and the round's start closes
// the workspace the cut one left by its label (evalrun.RunLocal) before
// anything else runs in the copy.
func (e *Engine) cutRounds(ctx context.Context, tasks []registry.Task) {
	for _, t := range tasks {
		if st, ok := e.stageOf(t, t.Stage); !ok || st.Type != config.FactoryStageReview || (!isLive(t) && t.State != registry.TaskNeedsYou) {
			continue
		}
		cycles, err := e.reg.Cycles(ctx, t.ID)
		if err != nil || len(cycles) == 0 {
			continue
		}
		cy := cycles[len(cycles)-1]
		if !cy.Ended.IsZero() {
			continue
		}
		if _, err := os.Stat(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reviewRoundFile(cy.N))); err == nil {
			continue // it ended before the restart: its result is read
		}
		round := len(loopCycles(cycles))
		e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("round %d (cycle %d) was cut by the restart: it runs again in "+
			"its cycle, and its start closes the workspace the cut one left by its label (synthetic PR #%d)", round, cy.N,
			evalrun.LocalNumber(t.ID, round)))
	}
}

// reconcileDeliveries reads each prompt of unknown delivery once
// (Runtime.Reconcile): one its transcript shows arrived is delivered and
// its run watched, never sent again; any other is sent to nobody, and its
// task, at work, waits on it ("agent: prompt delivery unknown") until a
// later read settles it or you let it go. This first read never proves that
// a prompt never arrived: the proof needs a second read RedeliverAfter later
// (the runtime keeps the first in memory), which the observe pass makes and
// says (EventUndelivered); this read begins it (Runtime.Proving). Then the
// wait on you of each task read is settled from its ledger (settleUnknown):
// none while a proof is under way.
func (e *Engine) reconcileDeliveries(ctx context.Context) {
	if e.d.Runtime == nil {
		return
	}
	es, err := e.reg.Undelivered(ctx)
	if err != nil {
		e.log.Warn("read the prompts of unknown delivery at the startup", "err", err)
		return
	}
	unknown := map[int64][]string{}
	var order []int64
	for _, en := range es {
		if _, ok := unknown[en.TaskID]; !ok {
			order = append(order, en.TaskID)
			unknown[en.TaskID] = nil
		}
		v, err := e.d.Runtime.Reconcile(ctx, en)
		if err != nil {
			e.log.Warn("read a prompt's delivery at the startup", "entry", en.ID, "err", err)
			v = registry.DeliveredUnknown
		}
		switch v {
		case registry.DeliveredYes:
			e.event(ctx, en.TaskID, "prompt", registry.PhaseOK, fmt.Sprintf("prompt %d to %s arrived before the restart (its transcript "+
				"holds it): its run is watched, never sent again", en.ID, en.Role))
		case registry.DeliveredUnknown:
			if !slices.Contains(unknown[en.TaskID], en.Role) {
				unknown[en.TaskID] = append(unknown[en.TaskID], en.Role)
			}
		}
	}
	for _, id := range order {
		t, err := e.reg.TaskByID(ctx, id)
		if err != nil {
			continue
		}
		if roles := unknown[id]; len(roles) > 0 {
			who := strings.Join(roles, ", ")
			e.event(ctx, id, "prompt", registry.PhaseInfo, "the delivery of a prompt to "+who+" is unknown: nothing is sent to "+who+
				" until a read of its transcript proves it never arrived, or you let it go "+
				fmt.Sprintf("(`magnum factory let-go %d --role <role>`)", id))
			if isLive(t) {
				e.waitOn(ctx, t, waitAgent, unknownWait(roles))
				if t, err = e.reg.TaskByID(ctx, id); err != nil {
					continue
				}
			}
		}
		if err := e.settleUnknown(ctx, t, "the restart's read of its prompts", false); err != nil {
			e.log.Warn("settle a task's wait on its unknown deliveries", "task", id, "err", err)
		}
	}
}

// unknownWait is the wait of a task whose prompts to roles have a delivery
// nobody can tell yet.
func unknownWait(roles []string) string {
	return "prompt delivery unknown (" + strings.Join(roles, ", ") + ")"
}

// openEffects names each effect a stop or a crash left begun: its stage
// runs it again, its own guard first, and never repeats a step it finds
// done (publish: the remote branch and the PRs of the branch are read
// before any step).
func (e *Engine) openEffects(ctx context.Context) {
	es, err := e.reg.OpenEffects(ctx)
	if err != nil {
		e.log.Warn("read the effects in flight at the startup", "err", err)
		return
	}
	for _, ef := range es {
		guard := "its guard runs again before any step"
		if ef.Kind == "publish" {
			guard = "the remote branch and the PRs of the branch are read before any step, so nothing is pushed or opened twice"
		}
		e.event(ctx, ef.TaskID, "effect", registry.PhaseInfo, fmt.Sprintf("%s was in flight when the daemon stopped: its stage runs "+
			"it again, and %s", ef.Key, guard))
	}
}

// unknownRoles are the roles of the ledger entries es whose delivery is
// unknown, in the ledger's order: the ones a stage holds.
func unknownRoles(es []registry.LedgerEntry) []string {
	var roles []string
	for _, en := range es {
		if en.Delivered == registry.DeliveredUnknown && !slices.Contains(roles, en.Role) {
			roles = append(roles, en.Role)
		}
	}
	return roles
}

// unknownToYou are the roles of the ledger entries es whose prompt you are
// to look at: its delivery unknown for runtime.UnknownAfter or longer, and
// no proof under way that it never arrived (Runtime.Proving: its read
// after RedeliverAfter settles it), in the ledger's order. Every wait on
// you for prompts of unknown delivery is these roles' (settleUnknown,
// unknownHeld).
func (e *Engine) unknownToYou(es []registry.LedgerEntry) []string {
	now := e.now()
	var roles []string
	for _, en := range es {
		if en.Delivered != registry.DeliveredUnknown || now.Sub(en.Sent) < runtime.UnknownAfter || slices.Contains(roles, en.Role) ||
			(e.d.Runtime != nil && e.d.Runtime.Proving(en.ID)) {
			continue
		}
		roles = append(roles, en.Role)
	}
	return roles
}

// settleUnknown is the one place that decides, from task t's ledger as it
// is now (unknownToYou), what t waits on you for about its prompts of
// unknown delivery: a live task with such a prompt goes to NEEDS YOU naming
// every one; a task at NEEDS YOU for them (runtime.UnknownReason's wait)
// names the ones left, with an alert when alert says one came, or, none
// left (you let them go, a read settled them, their proof is under way),
// goes back to work, where its stage holds a role whose prompt is still
// unknown and sends the others what they are due. because says what
// changed. A task that waits on you for anything else is left alone.
func (e *Engine) settleUnknown(ctx context.Context, t registry.Task, because string, alert bool) error {
	live, ours := isLive(t), t.State == registry.TaskNeedsYou && runtime.IsUnknownReason(t.WaitsWhat)
	if !live && !ours {
		return nil
	}
	es, err := e.reg.LedgerOf(ctx, t.ID)
	if err != nil {
		return err
	}
	left := e.unknownToYou(es)
	switch {
	case live && len(left) > 0:
		e.toOperator(ctx, t, runtime.UnknownReason(t.ID, left...), because)
	case live:
	case len(left) == 0:
		return e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskNeedsYou}, registry.TaskActive, func(u *registry.TaskUpdate) {
			u.Set("waits_who", "")
			u.Set("waits_what", "")
			u.Because(because)
		})
	case t.WaitsWhat != runtime.UnknownReason(t.ID, left...):
		what := runtime.UnknownReason(t.ID, left...)
		if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskNeedsYou}, "", func(u *registry.TaskUpdate) {
			u.Set("waits_what", what)
			u.Because(because)
		}); err != nil {
			return err
		}
		if alert {
			e.alert(ctx, t.ID, TaskKey(t)+": you: "+what)
		}
	}
	return nil
}
