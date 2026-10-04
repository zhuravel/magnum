package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/store"
)

// closeGrace hands closed PRs past their grace (and releases a previous
// apply left half done) to the heavy worker: cleanup's default plan parks
// their sessions, releases or removes their slots and moves them to
// released.
func (e *Engine) closeGrace(ctx context.Context) {
	if e.d.Cleanup == nil {
		return
	}
	due, err := e.st.ClosedPastGrace(ctx, e.now())
	if err != nil {
		e.log.Warn("closed past grace", "err", err)
		return
	}
	releasing, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRReleasing}})
	if err != nil {
		e.log.Warn("releasing PRs", "err", err)
		return
	}
	now := e.now()
	if e.roundOnAny(append(due, releasing...)) {
		e.log.Debug("close grace waits for a round of a closed PR to end")
		return
	}
	ready := false
	pending := map[int64]bool{}
	for _, pr := range append(due, releasing...) {
		pending[pr.ID] = true
	}
	for id := range e.cleanupTried {
		if !pending[id] {
			delete(e.cleanupTried, id)
		}
	}
	for _, pr := range append(due, releasing...) {
		if t, ok := e.cleanupTried[pr.ID]; !ok || now.Sub(t) >= cleanupRetry {
			ready = true
		}
	}
	if !ready {
		return
	}
	if e.enqueueHeavy("cleanup:default", e.defaultCleanup) {
		for _, pr := range append(due, releasing...) {
			e.cleanupTried[pr.ID] = now
		}
	}
}

// cleanupRetry spaces out release attempts of a PR whose release failed (each
// attempt scans the inventory).
const cleanupRetry = 5 * time.Minute

// defaultCleanup plans and applies cleanup's default plan (closed PRs past
// grace plus allowlisted orphan databases).
func (e *Engine) defaultCleanup(ctx context.Context) error {
	if e.closedRoundsRunning(ctx) {
		return nil
	}
	return e.applyCleanup(ctx, cleanup.Options{}, "default", nil)
}

// closedRoundsRunning reports whether a closed PR that cleanup's default plan
// would release (past its grace, or being released) still has a round or a
// restore running: its slot may be mid checkout or deps install, so the
// cleanup waits (closeGrace queues it again once the round ended). A closed
// PR never gets a new round, so the answer cannot flip back while cleanup
// runs.
func (e *Engine) closedRoundsRunning(ctx context.Context) bool {
	due, err := e.st.ClosedPastGrace(ctx, e.now())
	if err != nil {
		e.log.Warn("closed past grace", "err", err)
		return true
	}
	releasing, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRReleasing}})
	if err != nil {
		e.log.Warn("releasing PRs", "err", err)
		return true
	}
	return e.roundOnAny(append(due, releasing...))
}

// roundOnAny reports whether any of prs has a round or an open running.
func (e *Engine) roundOnAny(prs []store.PR) bool {
	return slices.ContainsFunc(prs, func(pr store.PR) bool { return e.roundActive(pr.ID) })
}

// applyCleanup plans opts (over inv when the caller already scanned, else
// with a fresh scan) and applies the plan without typed confirmation.
func (e *Engine) applyCleanup(ctx context.Context, opts cleanup.Options, what string, inv *inventory.Inventory) error {
	opts.DryRun = opts.DryRun || e.d.DryRun
	var plan cleanup.Plan
	var err error
	if inv != nil {
		plan, err = e.d.Cleanup.PlanFrom(ctx, opts, *inv)
	} else {
		plan, err = e.d.Cleanup.Plan(ctx, opts)
	}
	if err != nil {
		return fmt.Errorf("cleanup plan (%s): %w", what, err)
	}
	if len(plan.Actions) == 0 {
		return nil
	}
	rep, err := e.d.Cleanup.Apply(ctx, plan, false)
	if opts.DryRun {
		for _, a := range plan.Actions {
			e.rec.Record(ctx, a.Subject, "cleanup."+a.Kind, a.Why)
		}
		return nil
	}
	e.log.Info("cleanup applied", "plan", what, "done", rep.Done, "failed", rep.Failed, "unconfirmed", rep.Unconfirmed)
	if err != nil {
		return fmt.Errorf("cleanup (%s): %w", what, err)
	}
	return nil
}

// maybeReconcile queues a reconcile every daemon.reconcile_interval.
func (e *Engine) maybeReconcile(ctx context.Context) {
	iv := e.cfg.Daemon.ReconcileInterval.Duration
	if iv <= 0 {
		return
	}
	now := e.now()
	if !e.lastReconcile.IsZero() && now.Sub(e.lastReconcile) < iv {
		return
	}
	e.notePromptChanges(ctx)
	if e.enqueueHeavy("reconcile", e.reconcile) {
		e.lastReconcile = now
	}
}

// reconcile compares the registry with disk, MySQL and herdr: records the
// MySQL listing, applies safe fixes (lost slots; allowlisted orphan
// databases through cleanup), keeps each pool between min and max,
// re-checks unhealthy identities and prunes old events and requests. The inventory is scanned once; the
// default cleanup and every pool's shrink plan over that scan (Apply
// re-checks each action against the registry). After a failed scan the
// cleanups are skipped until the next reconcile.
func (e *Engine) reconcile(ctx context.Context) error {
	var errs []error
	e.setKV(ctx, kvLastReconcile, store.FormatTime(e.now()))
	var inv *inventory.Inventory
	if e.d.Inventory != nil {
		scanned, err := e.d.Inventory.Scan(ctx, inventory.Options{})
		if err != nil {
			errs = append(errs, fmt.Errorf("inventory: %w", err))
		} else {
			e.applyInventory(ctx, scanned)
			inv = &scanned
		}
	}
	// Without a scanner of its own the planner scans; after a failed scan
	// the cleanups wait for the next reconcile.
	clean := e.d.Cleanup != nil && (inv != nil || e.d.Inventory == nil)
	if clean && !e.closedRoundsRunning(ctx) {
		if err := e.applyCleanup(ctx, cleanup.Options{}, "default", inv); err != nil {
			errs = append(errs, err)
		}
	}
	for _, pool := range e.cfg.Pools {
		if err := e.maintainPool(ctx, pool.Repo, inv, clean); err != nil {
			errs = append(errs, err)
		}
	}
	e.warmIdentities(ctx, false)
	if err := e.prune(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// prune applies [daemon] keep_events and keep_requests (store.Prune): the
// audit events and handled requests older than them are deleted, once per
// reconcile. A dry run keeps everything.
func (e *Engine) prune(ctx context.Context) error {
	if e.d.DryRun {
		return nil
	}
	keepEvents, keepRequests := e.cfg.Daemon.KeepEvents.Duration, e.cfg.Daemon.KeepRequests.Duration
	if keepEvents <= 0 && keepRequests <= 0 {
		return nil
	}
	res, err := e.st.Prune(ctx, keepEvents, keepRequests)
	if err != nil {
		return err
	}
	if res.Events > 0 || res.Requests > 0 {
		e.log.Info("pruned old rows", "events", res.Events, "requests", res.Requests,
			"keep_events", keepEvents.String(), "keep_requests", keepRequests.String())
	}
	return nil
}

// applyInventory records the scan and fixes what is magnum's to fix.
func (e *Engine) applyInventory(ctx context.Context, inv inventory.Inventory) {
	for _, w := range inv.Warnings {
		e.log.Info("inventory warning", "warning", w)
	}
	if inv.DatabasesListed && !e.d.DryRun {
		if res, err := e.d.Inventory.UpsertSlotDatabases(ctx, inv); err != nil {
			e.log.Warn("record databases", "err", err)
		} else if len(res.Dropped) > 0 {
			e.event(ctx, "info", "", "reconcile.databases_gone", fmt.Sprintf("%d databases no longer in MySQL: %s", len(res.Dropped), strings.Join(res.Dropped, ", ")), nil)
		}
	}
	for _, f := range inv.Drift {
		if !f.Safe {
			e.log.Info("drift", "kind", f.Kind, "subject", f.Subject, "message", f.Message)
			continue
		}
		switch f.Kind {
		case inventory.KindLostSlot:
			name := strings.TrimPrefix(f.Subject, "slot:")
			sl, err := e.st.SlotByName(ctx, name)
			if err != nil || slices.Contains([]string{store.SlotLost, store.SlotRemoved, store.SlotBusy}, sl.State) {
				continue
			}
			if e.d.DryRun {
				e.rec.Record(ctx, f.Subject, "mark_lost", f.Message)
				continue
			}
			err = e.st.TransitionSlot(ctx, sl.ID, []string{sl.State}, store.SlotLost, func(u *store.SlotUpdate) {
				u.Set("last_error", f.Message)
			})
			if err != nil {
				e.log.Warn("mark slot lost", "slot", name, "err", err)
				continue
			}
			e.event(ctx, "warn", f.Subject, "reconcile.slot_lost", f.Message, nil)
		case inventory.KindOrphanDB:
			// Dropped by cleanup's default plan (same allowlist guard).
		}
	}
}

// maintainPool provisions below pool.min and, with clean, removes free
// slots idle longer than pool.idle_remove_after above it (cleanup Shrink
// with Idle, planned over the reconcile's inventory when there is one).
func (e *Engine) maintainPool(ctx context.Context, repo string, inv *inventory.Inventory, clean bool) error {
	pool := e.cfg.PoolFor(repo)
	if pool == nil || e.d.Slots == nil {
		return nil
	}
	sls, err := e.st.ListSlots(ctx, store.SlotFilter{RepoFullName: pool.Repo, Kind: store.SlotKindPool})
	if err != nil {
		return err
	}
	provisioning := slices.ContainsFunc(sls, func(s store.Slot) bool { return s.State == store.SlotProvisioning })
	if active := activeSlotCount(sls); provisioning || active < pool.Min {
		if e.d.DryRun {
			e.rec.Record(ctx, "pool:"+pool.Repo, "provision", fmt.Sprintf("%d of min %d slots", active, pool.Min))
		} else if err := e.provisionJob(*pool)(ctx); err != nil {
			return fmt.Errorf("provision %s: %w", pool.Repo, err)
		}
	}
	if clean {
		zero := 0
		if err := e.applyCleanup(ctx, cleanup.Options{Shrink: &zero, Idle: true}, "pool "+pool.Repo, inv); err != nil {
			return err
		}
	}
	return nil
}

// reclassifyIneligible applies the watches' filters, as configured now, to
// the open ineligible PRs: the config is read at startup and may have
// relaxed a filter since they were classified, while the poller only
// re-classifies a PR when GitHub reports a change. (Waiting PRs a filter now
// rejects are caught at dispatch: reclassify.)
func (e *Engine) reclassifyIneligible(ctx context.Context) {
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRIneligible}})
	if err != nil {
		e.log.Warn("ineligible PRs", "err", err)
		return
	}
	for _, pr := range prs {
		if pr.GHState != store.GHOpen || pr.DetailsAt == nil {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		w := e.cfg.WatchFor(repo.FullName())
		if w == nil {
			continue
		}
		if err := e.onSeenPR(ctx, repo, *w, pr, store.PRUpsert{PR: pr, Changed: true}, e.now()); err != nil {
			e.log.Warn("reclassify ineligible PR", "subject", prSubject(repo, pr.Number), "err", err)
		}
	}
}

// recoverRows re-evaluates rows a previous daemon left mid-flight, before
// anything starts: sessions are rebound to the panes herdr restored (by
// session id), PRs whose judge turn is not over (judgePrompted, from the run
// rows) pause and continue that turn later (its runs are observed, never
// re-sent), the rest go back in line, and busy or claimed slots become held
// (no round or restore runs yet, and a held slot can be evicted).
func (e *Engine) recoverRows(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: store.InFlightStates})
	if err != nil {
		e.log.Warn("recovery: PRs", "err", err)
		return
	}
	if e.d.Agents != nil {
		ids := map[int64]bool{}
		for _, pr := range prs {
			ids[pr.ID] = true
		}
		if live, err := e.st.LiveSessions(ctx); err == nil {
			for _, s := range live {
				ids[s.PRID] = true
			}
		}
		for id := range ids {
			pr, err := e.st.PRByID(ctx, id)
			if err != nil {
				continue
			}
			rec, err := e.d.Agents.Recover(ctx, pr)
			if err != nil {
				e.log.Warn("recovery: sessions", "pr", id, "err", err)
				continue
			}
			for _, r := range rec {
				if r.Action != "ok" {
					e.log.Info("recovery: session", "pr", id, "role", r.Role, "action", r.Action)
				}
			}
		}
	}
	for _, pr := range prs {
		repo, _ := e.st.RepoByID(ctx, pr.RepoID)
		subject := prSubject(repo, pr.Number)
		to := claimableState(pr)
		why := "daemon restarted before the round started"
		if e.judgePrompted(ctx, pr) {
			to = store.PRPaused
			why = "daemon restarted during the judge's turn; it continues when idle"
		}
		err := e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, to, func(u *store.PRUpdate) {
			u.Set("last_error", why)
			u.Set("next_attempt_at", nil)
		})
		if err != nil {
			e.log.Warn("recovery: PR", "subject", subject, "err", err)
			continue
		}
		e.event(ctx, "info", subject, "engine.recovered", fmt.Sprintf("%s → %s: %s", pr.State, to, why), nil)
	}
	settle := []string{store.SlotBusy, store.SlotClaimed}
	stuck, err := e.st.ListSlots(ctx, store.SlotFilter{States: settle})
	if err != nil {
		e.log.Warn("recovery: slots", "err", err)
		return
	}
	for _, sl := range stuck {
		if err := e.st.TransitionSlot(ctx, sl.ID, settle, store.SlotHeld, nil); err != nil {
			e.log.Warn("recovery: slot", "slot", sl.Name, "err", err)
		}
	}
}
