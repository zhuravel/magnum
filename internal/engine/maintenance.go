package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
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
		e.warnUnlessStopped(ctx, err, "closed past grace")
		return
	}
	releasing, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRReleasing}})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "releasing PRs")
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
// PR gets a new round only from a post-merge review request, which moves it
// out of closed first; apply re-reads each PR and moves it closed →
// releasing (compare-and-set) before its first side effect, so a request
// that comes meanwhile either stops the release or is refused.
func (e *Engine) closedRoundsRunning(ctx context.Context) bool {
	due, err := e.st.ClosedPastGrace(ctx, e.now())
	if err != nil {
		e.warnUnlessStopped(ctx, err, "closed past grace")
		return true
	}
	releasing, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRReleasing}})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "releasing PRs")
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

// maybeReconcile queues a reconcile every daemon.reconcile_interval, after
// noting prompt files and a binary changed on disk.
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
	e.noteBuild(ctx)
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
	e.syncNotes(ctx, false)
	if err := e.prune(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// prune applies [daemon] keep_events and keep_requests (store.Prune): the
// audit events and handled requests older than them are deleted, once per
// reconcile; so are the retro's run directories and the notes curations'
// scratch directories past their 30 days (pruneRetro, pruneCurations).
// Nothing else is pruned: the notes' versions and proposals, the findings,
// the misses and the judges' results stay in the registry. A dry run keeps
// everything.
func (e *Engine) prune(ctx context.Context) error {
	if e.d.DryRun {
		return nil
	}
	e.pruneRetro()
	e.pruneCurations()
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
	due := e.driftDue(ctx, inv.Drift)
	for _, f := range inv.Drift {
		if !f.Safe {
			if due[driftKey(f)] {
				e.log.Info("drift", "kind", f.Kind, "subject", f.Subject, "message", f.Message)
			}
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

func driftKey(f inventory.Finding) string { return f.Kind + " " + f.Subject }

// driftDue says which of the unsafe drift findings (those the reconcile only
// reports) are to be logged now, by driftKey: each once per local day while it
// persists (the same orphan databases were logged by every reconcile). What
// was logged is kept in the registry (kvDriftLogged), reduced to the findings
// of this scan, so a finding that went away and came back is logged again, and
// a restart does not repeat a day's lines. The scan itself still reports every
// finding to status and doctor. A stopping daemon (ctx ended) logs and records
// nothing.
func (e *Engine) driftDue(ctx context.Context, drift []inventory.Finding) map[string]bool {
	raw, _ := e.getKV(ctx, kvDriftLogged)
	if ctx.Err() != nil {
		return nil
	}
	var was map[string]string
	_ = json.Unmarshal([]byte(raw), &was) // a damaged value is a day nothing was logged
	day := store.DayKey(e.now())
	now := map[string]string{}
	due := map[string]bool{}
	for _, f := range drift {
		if f.Safe {
			continue
		}
		now[driftKey(f)] = day
		if was[driftKey(f)] != day {
			due[driftKey(f)] = true
		}
	}
	if !maps.Equal(was, now) {
		if b, err := json.Marshal(now); err == nil {
			e.setKV(ctx, kvDriftLogged, string(b))
		}
	}
	return due
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

// reclassifyIneligible applies the watches' filters, as configured now, at
// startup: baseline PRs a filter rejects become ineligible (skipBaseline),
// ineligible ones skipBaseline moved go back to baseline when no filter
// rejects them any more (restoreBaseline), and the other ineligible PRs are
// classified again, since the config may have relaxed a filter while the
// poller only re-classifies a PR when GitHub reports a change. Waiting PRs a
// filter now rejects become ineligible here too (reclassify, which dispatch
// also applies), since dispatch may be held for days (the Codex soft cap
// holds first reviews) while the board shows them as queued.
func (e *Engine) reclassifyIneligible(ctx context.Context) {
	e.skipBaseline(ctx)
	e.reclassifyWaiting(ctx)
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
		if e.restoreBaseline(ctx, repo, *w, pr) {
			continue
		}
		if err := e.onSeenPR(ctx, repo, *w, pr, store.PRUpsert{PR: pr, Changed: true}, e.now()); err != nil {
			e.log.Warn("reclassify ineligible PR", "subject", prSubject(repo, pr.Number), "err", err)
		}
	}
}

// reclassifyWaiting applies the watches' current filters to the PRs waiting
// for a round (queued, rereview_pending): one a filter now rejects becomes
// ineligible (reclassify keeps forced, paused and detail-less PRs).
func (e *Engine) reclassifyWaiting(ctx context.Context) {
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRQueued, store.PRRereviewPending}})
	if err != nil {
		e.log.Warn("waiting PRs", "err", err)
		return
	}
	now := e.now()
	for _, pr := range prs {
		if pr.GHState != store.GHOpen {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		e.reclassify(ctx, pr, e.cfg.WatchFor(repo.FullName()), now)
	}
}

// KVPRSkippedBaseline marks a baseline PR skipBaseline made ineligible: its
// value is the head it was skipped on.
func KVPRSkippedBaseline(prID int64) string { return fmt.Sprintf("pr.%d.skipped_baseline", prID) }

// skipBaseline makes the baseline PRs (open before magnum watched their
// repository, never reviewed) that the configuration skips read as skipped:
// ineligible with the rule's reason, instead of "not reviewed". Only a push
// reclassifies a baseline PR otherwise, so without this a bot's PR from
// before the first sync looked like any other.
func (e *Engine) skipBaseline(ctx context.Context) {
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRBaseline}})
	if err != nil {
		e.log.Warn("baseline PRs", "err", err)
		return
	}
	now := e.now()
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
		dec := e.classify(ctx, *w, pr, now)
		if dec.Eligible {
			continue
		}
		if err := e.markIneligible(ctx, pr, []string{store.PRBaseline}, dec.Reason, false, now); err != nil {
			e.log.Info("skip baseline PR", "subject", prSubject(repo, pr.Number), "err", err)
			continue
		}
		e.setKV(ctx, KVPRSkippedBaseline(pr.ID), pr.HeadSHA)
	}
}

// restoreBaseline undoes skipBaseline for a PR the configuration no longer
// skips and nobody pushed to since: it goes back to baseline ("not
// reviewed"), not in line for a review, so a configuration change never
// starts reviews of old PRs. It reports whether it handled pr.
func (e *Engine) restoreBaseline(ctx context.Context, repo store.Repo, w config.Watch, pr store.PR) bool {
	head, ok := e.getKV(ctx, KVPRSkippedBaseline(pr.ID))
	if !ok {
		return false
	}
	if head != pr.HeadSHA || deref(pr.ReviewedSHA) != "" { // pushed to since: the normal rules apply
		e.delKV(ctx, KVPRSkippedBaseline(pr.ID))
		return false
	}
	if !e.classify(ctx, w, pr, e.now()).Eligible {
		return true // still skipped
	}
	if err := e.st.TransitionPR(ctx, pr.ID, []string{store.PRIneligible}, store.PRBaseline, func(u *store.PRUpdate) {
		u.Set("skip_reason", nil)
	}); err != nil {
		e.log.Info("restore baseline PR", "subject", prSubject(repo, pr.Number), "err", err)
		return true
	}
	e.delKV(ctx, KVPRSkippedBaseline(pr.ID))
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.baseline", "ineligible → baseline: the configuration no longer skips it", nil)
	return true
}

// interruptedPhase says where the round of pr, whose judge was not prompted,
// was when the daemon restarted: before it started (claiming), before it
// prompted any agent, or while its reviewers ran (a run created since the
// round's start sent its prompt). Each goes back in line.
func (e *Engine) interruptedPhase(ctx context.Context, pr store.PR) string {
	if pr.State == store.PRClaiming {
		return "daemon restarted before the round started"
	}
	if runs, err := e.st.RunsByPR(ctx, pr.ID); err == nil && pr.LastRoundStartedAt != nil {
		for _, r := range runs {
			if r.SubmittedAt != nil && !r.CreatedAt.Before(*pr.LastRoundStartedAt) {
				return "daemon restarted while the reviewers ran; the round starts again"
			}
		}
	}
	return "daemon restarted before the round prompted its agents; the round starts again"
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
		why := e.interruptedPhase(ctx, pr)
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
