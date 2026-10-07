package slots

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Claim hands a free pool slot to pr: store.FreeSlots (the slot that last
// held the PR first, then least recently used) and store.ClaimSlot for each
// candidate until one succeeds (PR → claiming, slot → claimed, open
// assignment with the pool's database names). A lost race moves on to the
// next slot; a PR that is not claimable is ErrConflict; no slot left is
// ErrNoFreeSlot. The returned row is the claimed slot.
func (m *Manager) Claim(ctx context.Context, pr store.PR, pool config.Pool) (store.Slot, error) {
	free, err := m.d.Store.FreeSlots(ctx, pool.Repo, pr.ID)
	if err != nil {
		return store.Slot{}, fmt.Errorf("slots: claim for PR #%d: %w", pr.Number, err)
	}
	if m.d.DryRun {
		if len(free) == 0 {
			return store.Slot{}, fmt.Errorf("slots: claim for PR #%d: %w", pr.Number, ErrNoFreeSlot)
		}
		m.dryRun("claim %s for PR #%d", free[0].Name, pr.Number)
		return free[0], nil
	}
	for _, sl := range free {
		_, err := m.d.Store.ClaimSlot(ctx, pr.ID, sl.ID, pool.DBNames(SlotDBSlug(sl))...)
		if err == nil {
			return m.reload(ctx, sl)
		}
		if !errors.Is(err, store.ErrConflict) {
			return store.Slot{}, fmt.Errorf("slots: claim %s for PR #%d: %w", sl.Name, pr.Number, err)
		}
		cur, perr := m.d.Store.PRByID(ctx, pr.ID)
		if perr != nil {
			return store.Slot{}, fmt.Errorf("slots: claim for PR #%d: %w", pr.Number, perr)
		}
		if !slices.Contains(store.ClaimableStates, cur.State) {
			return store.Slot{}, fmt.Errorf("slots: claim for PR #%d in state %s: %w", pr.Number, cur.State, err)
		}
	}
	return store.Slot{}, fmt.Errorf("slots: claim for PR #%d in %s: %w", pr.Number, pool.Repo, ErrNoFreeSlot)
}

// Reserve hands a free pool slot to pr without touching the PR's state, for
// a human who wants the PR's checkout back (magnum open on a parked PR):
// like Claim but through store.AssignSlot (slot → claimed with pr_id, open
// assignment with the pool's database names). A claimed or held slot the PR
// already has is returned as is; no free slot left is ErrNoFreeSlot.
func (m *Manager) Reserve(ctx context.Context, pr store.PR, pool config.Pool) (store.Slot, error) {
	if sl, err := m.d.Store.SlotByPR(ctx, pr.ID); err == nil && slices.Contains([]string{store.SlotClaimed, store.SlotHeld}, sl.State) {
		return sl, nil
	}
	free, err := m.d.Store.FreeSlots(ctx, pool.Repo, pr.ID)
	if err != nil {
		return store.Slot{}, fmt.Errorf("slots: reserve for PR #%d: %w", pr.Number, err)
	}
	if m.d.DryRun {
		if len(free) == 0 {
			return store.Slot{}, fmt.Errorf("slots: reserve for PR #%d: %w", pr.Number, ErrNoFreeSlot)
		}
		m.dryRun("reserve %s for PR #%d", free[0].Name, pr.Number)
		return free[0], nil
	}
	for _, sl := range free {
		_, err := m.d.Store.AssignSlot(ctx, pr.ID, sl.ID, pool.DBNames(SlotDBSlug(sl))...)
		if err == nil {
			return m.reload(ctx, sl)
		}
		if !errors.Is(err, store.ErrConflict) {
			return store.Slot{}, fmt.Errorf("slots: reserve %s for PR #%d: %w", sl.Name, pr.Number, err)
		}
	}
	return store.Slot{}, fmt.Errorf("slots: reserve for PR #%d in %s: %w", pr.Number, pool.Repo, ErrNoFreeSlot)
}

// Checkout puts pr's head into a claimed or held slot that belongs to pr.
// Steps (subject "slot:<name>:pr:<N>:<sha7>", a new generation on every
// call; each step is idempotent): fetch (refs/pull/N/head →
// refs/magnum/pr/N, plus origin/<base> for pool slots; when the fetched head
// differs from targetSHA the fetched head is used and a slot.head_moved event
// is written), guard, switch (detached at refs/magnum/pr/N, discarding
// tracked changes the guard took for magnum's residue after a slot.discarded
// event names them; the target commit is written to the kv store first and
// checked_out_sha right after, so a retry after a failure in a later step
// recognizes HEAD as magnum's own switch, not a human's commit), and for
// pool slots render_mise, deps (pool.post_checkout
// through mise exec, 30m, when the Gemfile.lock/pnpm-lock.yaml hash differs
// from lock_sha) and schema (dirty_schema = 1 when the PR changes
// pool.schema_paths since its merge base with origin/<base>), for per-PR
// slots render_mise alone (preparePerPR, as at the worktree's creation: the
// main clone's .mise.local.toml with the [[repo]] strip_env and env as the
// daemon has them now, its copy_files); finally verify
// (HEAD == fetched head → checked_out_sha, last_used_at). The slot state is
// left as it was: moving claimed → busy after the prompt is acked is the
// engine's job. Read the slot again for the sha actually checked out.
// pool is ignored for per-PR slots.
func (m *Manager) Checkout(ctx context.Context, slot store.Slot, pr store.PR, pool config.Pool, targetSHA string) error {
	return m.checkout(ctx, slot, pr, pool, targetSHA, "")
}

// CheckoutFetched is Checkout of the head Fetch brought into the slot's
// main clone (fetched), without fetching again: its fetch step only checks
// that refs/magnum/pr/N still holds that commit, and fails when a fetch
// since moved it (the caller read fetched, not the newer head).
func (m *Manager) CheckoutFetched(ctx context.Context, slot store.Slot, pr store.PR, pool config.Pool, targetSHA, fetched string) error {
	if fetched == "" {
		return fmt.Errorf("slots: checkout PR #%d: no fetched head", pr.Number)
	}
	return m.checkout(ctx, slot, pr, pool, targetSHA, fetched)
}

// Fetch is Checkout's fetch alone, for a caller that reads the head before
// it checks it out (the engine compares the project config a running agent
// reloads, before the checkout moves under it) and then checks it out with
// CheckoutFetched: refs/pull/N/head → refs/magnum/pr/N in the slot's main
// clone, plus origin/<base> for a pool slot. It returns the commit fetched,
// which may be newer than the radar's head. The slot must belong to pr;
// its state does not matter, as nothing in its work tree changes. pool is
// ignored for per-PR slots.
func (m *Manager) Fetch(ctx context.Context, slot store.Slot, pr store.PR, pool config.Pool) (string, error) {
	if m.d.DryRun {
		m.dryRun("fetch PR #%d into %s", pr.Number, slot.MainClone)
		return "", fmt.Errorf("slots: fetch PR #%d: a dry run fetches nothing", pr.Number)
	}
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return "", err
	}
	if sl.PRID == nil || *sl.PRID != pr.ID {
		return "", fmt.Errorf("slots: fetch PR #%d: slot %s is not assigned to it: %w", pr.Number, sl.Name, store.ErrConflict)
	}
	return m.fetchHead(ctx, sl, pr, pool)
}

// fetchHead fetches pr's head into sl's main clone (and a pool slot's base
// branch) and returns the commit fetched.
func (m *Manager) fetchHead(ctx context.Context, sl store.Slot, pr store.PR, pool config.Pool) (string, error) {
	sha, err := m.git.FetchPR(ctx, sl.MainClone, pr.Number)
	if err != nil {
		return "", err
	}
	if sl.Kind == store.SlotKindPool {
		if err := m.git.FetchBranch(ctx, sl.MainClone, pool.Base); err != nil {
			return "", err
		}
	}
	return sha, nil
}

// checkout is Checkout, or CheckoutFetched with fetched set.
func (m *Manager) checkout(ctx context.Context, slot store.Slot, pr store.PR, pool config.Pool, targetSHA, fetched string) error {
	if len(targetSHA) < 7 {
		return fmt.Errorf("slots: checkout PR #%d: invalid target sha %q", pr.Number, targetSHA)
	}
	if m.d.DryRun {
		m.dryRun("check out PR #%d at %s in %s", pr.Number, textx.ShortSHA(targetSHA), slot.Name)
		return nil
	}
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	if sl.PRID == nil || *sl.PRID != pr.ID {
		return fmt.Errorf("slots: checkout PR #%d: slot %s is not assigned to it: %w", pr.Number, sl.Name, store.ErrConflict)
	}
	if !slices.Contains([]string{store.SlotClaimed, store.SlotHeld}, sl.State) {
		return fmt.Errorf("slots: checkout PR #%d: slot %s is %s: %w", pr.Number, sl.Name, sl.State, store.ErrConflict)
	}
	subject := fmt.Sprintf("slot:%s:pr:%d:%s", sl.Name, pr.Number, textx.ShortSHA(targetSHA))
	if err := steps.ResetSubject(ctx, m.d.Store, subject); err != nil {
		return err
	}
	err = m.checkoutSteps(ctx, subject, sl, pr, pool, targetSHA, fetched)
	if err != nil {
		m.setLastError(ctx, sl.ID, err)
	}
	return err
}

// checkoutSteps runs Checkout's steps; fetched, when set, is the head Fetch
// brought, which the fetch step checks instead of fetching.
func (m *Manager) checkoutSteps(ctx context.Context, subject string, sl store.Slot, pr store.PR, pool config.Pool, target, fetched string) error {
	isPool := sl.Kind == store.SlotKindPool
	sha := ""
	if err := m.step(ctx, subject, "fetch", func(ctx context.Context) error {
		var err error
		if fetched == "" {
			if sha, err = m.fetchHead(ctx, sl, pr, pool); err != nil {
				return err
			}
		} else {
			if sha, err = m.git.RevParse(ctx, sl.MainClone, gitx.PRRef(pr.Number)); err != nil {
				return err
			}
			if sha != fetched {
				return fmt.Errorf("slots: PR #%d: %s moved from %s to %s since the fetch", pr.Number, gitx.PRRef(pr.Number),
					textx.ShortSHA(fetched), textx.ShortSHA(sha))
			}
		}
		if sha != target {
			m.event(ctx, subject, "warn", "slot.head_moved",
				fmt.Sprintf("PR #%d head moved: wanted %s, fetched %s; checking out %s", pr.Number, textx.ShortSHA(target), textx.ShortSHA(sha), textx.ShortSHA(sha)))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "guard", func(ctx context.Context) error {
		return m.Guard(ctx, sl)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "switch", func(ctx context.Context) error {
		m.discarding(ctx, sl)
		return m.switchTo(ctx, sl, gitx.PRRef(pr.Number), sha)
	}); err != nil {
		return err
	}
	if isPool {
		if err := m.step(ctx, subject, "render_mise", func(ctx context.Context) error {
			return m.renderMise(ctx, sl, pool)
		}); err != nil {
			return err
		}
		if err := m.step(ctx, subject, "deps", func(ctx context.Context) error {
			return m.deps(ctx, sl, pool)
		}); err != nil {
			return err
		}
		if err := m.step(ctx, subject, "schema", func(ctx context.Context) error {
			return m.markSchema(ctx, sl, pool)
		}); err != nil {
			return err
		}
	} else if err := m.step(ctx, subject, "render_mise", func(ctx context.Context) error {
		// What the worktree's creation rendered, with the strip and env
		// rules of the [[repo]] block as the daemon has it now. A wt.toml
		// that cannot be planned concerns the hooks only (planPerPR fills
		// the env, strip and copy lists first).
		plan, _ := m.planPerPR(sl, pr.Number, store.Deref(pr.BaseRef))
		return m.preparePerPR(ctx, sl, plan)
	}); err != nil {
		return err
	}
	return m.step(ctx, subject, "verify", func(ctx context.Context) error {
		head, err := m.git.RevParse(ctx, sl.Path, "HEAD")
		if err != nil {
			return err
		}
		if head != sha {
			return fmt.Errorf("%w: %s HEAD is %s, want %s", ErrVerify, sl.Name, textx.ShortSHA(head), textx.ShortSHA(sha))
		}
		if err := m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) {
			u.Set("checked_out_sha", sha)
			u.Set("last_used_at", m.now())
			u.Set("last_error", nil)
		}); err != nil {
			return err
		}
		// The assignment records the head the PR's code is at in this slot.
		if a, err := m.d.Store.OpenAssignmentBySlot(ctx, sl.ID); err == nil && a.PRID == pr.ID && store.Deref(a.HeadSHA) != sha {
			return m.d.Store.UpdateAssignmentHead(ctx, a.ID, sha)
		}
		return nil
	})
}

// discarding records the tracked changes a switch or reset with
// --discard-changes is about to drop (noteDiscard); untracked files stay. A
// failure to list them is logged, never blocks the step.
func (m *Manager) discarding(ctx context.Context, sl store.Slot) {
	if _, err := m.noteDiscard(ctx, sl, false); err != nil {
		m.logf("slots: list the changes in %s before discarding them: %v", sl.Path, err)
	}
}

// switchTo detaches the slot at ref, whose commit is sha, and records the
// resulting HEAD as checked_out_sha. sha is written to the kv store before
// the switch (switchingKey) so that Guard, after a crash between the switch
// and the record, takes HEAD == sha as magnum's own switch.
func (m *Manager) switchTo(ctx context.Context, sl store.Slot, ref, sha string) error {
	if err := m.d.Store.SetKV(ctx, switchingKey(sl.ID), sha); err != nil {
		return err
	}
	if err := m.git.SwitchDetach(ctx, sl.Path, ref); err != nil {
		return err
	}
	head, err := m.git.RevParse(ctx, sl.Path, "HEAD")
	if err != nil {
		return err
	}
	return m.recordSwitch(ctx, sl, head)
}

// deps reruns pool.post_checkout when the lockfile hash differs from lock_sha.
func (m *Manager) deps(ctx context.Context, slot store.Slot, pool config.Pool) error {
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	lock, err := lockHash(sl.Path)
	if err != nil {
		return err
	}
	if sl.LockSHA != nil && *sl.LockSHA == lock {
		return nil
	}
	if err := m.runScripts(ctx, sl, pool.SlotEnv(sl.Name), pool.PostCheckout, "deps", "slot-"+sl.Name+".log", DepsTimeout); err != nil {
		return err
	}
	return m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("lock_sha", lock) })
}

// markSchema sets dirty_schema when HEAD changes pool.schema_paths relative to
// its merge base with origin/<base> (unrelated histories count as dirty). It
// never clears the flag: the release does. Whether a round reloads the
// databases is CheckSchema's call (LazySchema), not this flag's.
func (m *Manager) markSchema(ctx context.Context, sl store.Slot, pool config.Pool) error {
	if len(pool.SchemaPaths) == 0 {
		return nil
	}
	dirty := false
	base, err := m.git.MergeBase(ctx, sl.Path, "origin/"+pool.Base, "HEAD")
	switch {
	case errors.Is(err, gitx.ErrNoMergeBase):
		dirty = true
	case err != nil:
		return err
	default:
		changed, err := m.git.ChangedPaths(ctx, sl.Path, base, "HEAD", pool.SchemaPaths...)
		if err != nil {
			return err
		}
		dirty = len(changed) > 0
	}
	if !dirty {
		return nil
	}
	return m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("dirty_schema", true) })
}

// Release hands a claimed or held pool slot back to the pool. Steps (subject
// "slot:<name>:release"): guard (Guard: pins, holds, a human's agent or
// process, HEAD drift, tracked changes a human made), then the slot moves to
// releasing, fetch_base, reset (placeholder branch → origin/<base>, tracked
// changes discarded after a slot.discarded event, checked_out_sha cleared;
// origin/<base>'s commit is written to the kv store first, as Checkout's
// switch does, so a release interrupted after the reset is no head drift),
// delete_ref (refs/magnum/pr/N), render_mise, deps, schema_check
// (resetSchema: a LazySchema pool keeps the databases unless
// MAX(schema_migrations.version) of the development database differs from
// the version of the schema recorded for them; otherwise when dirty_schema
// is set or that version differs from db/schema.rb at HEAD: the slot moves
// to dirty_schema and pool.reset_db runs through mise exec, 30m; a second
// failure in a row moves the slot to broken and returns ErrBroken) and
// mark_free (store.ReleaseSlot: free, pr_id cleared, assignment closed with
// reason). A releasing or dirty_schema slot resumes after its last completed
// step, after Guard ran again: whatever a human did since the last attempt
// refuses the release before its next destructive step. Per-PR slots are
// removed instead (RemovePRWorktree semantics).
func (m *Manager) Release(ctx context.Context, slot store.Slot, pool config.Pool, reason string) error {
	if m.d.DryRun {
		m.dryRun("release %s (%s)", slot.Name, reason)
		return nil
	}
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	if sl.Kind == store.SlotKindPerPR {
		return m.removePR(ctx, sl, false, reason)
	}
	subject := "slot:" + sl.Name + ":release"
	switch {
	case slices.Contains([]string{store.SlotClaimed, store.SlotHeld}, sl.State):
		if err := steps.ResetSubject(ctx, m.d.Store, subject); err != nil {
			return err
		}
		if err := m.step(ctx, subject, "guard", func(ctx context.Context) error {
			return m.Guard(ctx, sl)
		}); err != nil {
			return err
		}
		if err := m.d.Store.TransitionSlot(ctx, sl.ID, []string{store.SlotClaimed, store.SlotHeld}, store.SlotReleasing, nil); err != nil {
			return fmt.Errorf("slots: release %s: %w", sl.Name, err)
		}
	case slices.Contains([]string{store.SlotReleasing, store.SlotDirtySchema}, sl.State):
		if err := m.Guard(ctx, sl); err != nil {
			return err
		}
	default:
		return fmt.Errorf("slots: release %s in state %s: %w", sl.Name, sl.State, store.ErrConflict)
	}
	err = m.releaseSteps(ctx, subject, sl, pool, reason, "", nil)
	if err != nil && !errors.Is(err, ErrBroken) {
		m.setLastError(ctx, sl.ID, err)
	}
	return err
}

// releaseSteps are the release's steps after its guard. With hold "" they
// are a round's release, from releasing (or dirty_schema) to free. With a
// hold they are ReleaseHeld's: the slot stays held with that hold_reason,
// which the daemon never touches, until mark_free frees it in one
// compare-and-set; delete_ref also deletes refs (the check's own refs).
func (m *Manager) releaseSteps(ctx context.Context, subject string, sl store.Slot, pool config.Pool, reason, hold string, refs []string) error {
	if err := m.step(ctx, subject, "fetch_base", func(ctx context.Context) error {
		return m.git.FetchBranch(ctx, sl.MainClone, pool.Base)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "reset", func(ctx context.Context) error {
		// As switchTo: the commit the reset moves HEAD to is written first, so
		// the guard of a resumed release takes HEAD there as magnum's own
		// reset when something fails before checked_out_sha is cleared.
		base, err := m.git.RevParse(ctx, sl.MainClone, "origin/"+pool.Base)
		if err != nil {
			return err
		}
		if err := m.d.Store.SetKV(ctx, switchingKey(sl.ID), base); err != nil {
			return err
		}
		m.discarding(ctx, sl)
		if err := m.git.ResetPlaceholder(ctx, sl.Path, placeholder(sl), pool.Base); err != nil {
			return err
		}
		if err := m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", nil) }); err != nil {
			return err
		}
		return m.d.Store.DeleteKV(ctx, switchingKey(sl.ID))
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "delete_ref", func(ctx context.Context) error {
		for _, ref := range refs {
			if err := m.git.UpdateRefDelete(ctx, sl.MainClone, ref); err != nil {
				return err
			}
		}
		pr, ok, err := m.slotPR(ctx, sl)
		if err != nil || !ok || pr.Number <= 0 {
			return err
		}
		return m.git.UpdateRefDelete(ctx, sl.MainClone, gitx.PRRef(pr.Number))
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "render_mise", func(ctx context.Context) error {
		return m.renderMise(ctx, sl, pool)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "deps", func(ctx context.Context) error {
		return m.deps(ctx, sl, pool)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "schema_check", func(ctx context.Context) error {
		return m.resetSchema(ctx, sl, pool, hold)
	}); err != nil {
		return err
	}
	return m.step(ctx, subject, "mark_free", func(ctx context.Context) error {
		if hold != "" {
			err := m.d.Store.TransitionSlot(ctx, sl.ID, []string{store.SlotHeld}, store.SlotFree, func(u *store.SlotUpdate) {
				u.Where("hold_reason", hold)
				u.Where("pr_id", nil)
				u.Set("hold_reason", nil)
				u.Set("last_error", nil)
			})
			if err != nil {
				return fmt.Errorf("slots: release %s held for %s: %w", sl.Name, hold, err)
			}
			return nil
		}
		if err := m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("last_error", nil) }); err != nil {
			return err
		}
		return m.d.Store.ReleaseSlot(ctx, sl.ID, []string{store.SlotReleasing, store.SlotDirtySchema}, store.SlotFree, reason)
	})
}

// resetSchema is the release's schema step. A LazySchema pool keeps the
// slot's databases, whatever schema they carry, unless the development
// database drifted from the recorded schema (keptSchemaDrifted): the next
// round reloads them only when its checkout needs another (CheckSchema).
// Otherwise, and on a drift, it reloads them with the base schema when the
// PR touched the schema or the dev database is not at db/schema.rb's
// version, and records the base schema as theirs. With a hold (ReleaseHeld)
// the slot stays held during the reload instead of moving to dirty_schema,
// and two failed reloads move it from held to broken without the hold.
func (m *Manager) resetSchema(ctx context.Context, slot store.Slot, pool config.Pool, hold string) error {
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	need := sl.DirtySchema
	why := "dirty_schema"
	if LazySchema(pool) {
		if why, err = m.keptSchemaDrifted(ctx, sl, pool); err != nil {
			return err
		}
		if why == "" {
			return m.keepSchema(ctx, sl)
		}
		need = true
	}
	if !need && m.d.MySQL != nil {
		want, ok, err := schemaVersion(sl.Path)
		if err != nil {
			return err
		}
		if db := devDatabase(pool, SlotDBSlug(sl)); ok && db != "" {
			have, err := m.d.MySQL.SchemaMigrationsMax(ctx, db)
			switch {
			case errors.Is(err, mysqlx.ErrNotFound):
				need, why = true, db+" has no schema_migrations"
			case err != nil:
				return fmt.Errorf("slots: schema version of %s: %w", db, err)
			case have != want:
				need, why = true, fmt.Sprintf("%s is at %q, %s says %q", db, have, SchemaFile, want)
			}
		}
	}
	if !need {
		return nil
	}
	if len(pool.ResetDB) == 0 {
		return m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("dirty_schema", false) })
	}
	// The databases' schema is unknown until the reload succeeds.
	switch {
	case hold != "":
		if err := m.d.Store.TransitionSlot(ctx, sl.ID, []string{store.SlotHeld}, "", func(u *store.SlotUpdate) {
			u.Where("hold_reason", hold)
			setSchema(u, SchemaCheck{})
		}); err != nil {
			return fmt.Errorf("slots: release %s: %w", sl.Name, err)
		}
	case sl.State != store.SlotDirtySchema:
		if err := m.d.Store.TransitionSlot(ctx, sl.ID, []string{store.SlotReleasing}, store.SlotDirtySchema,
			func(u *store.SlotUpdate) { setSchema(u, SchemaCheck{}) }); err != nil {
			return fmt.Errorf("slots: release %s: %w", sl.Name, err)
		}
	}
	m.event(ctx, "slot:"+sl.Name+":release", "info", "slot.schema_reset", "reloading schema: "+why)
	var runErr error
	for attempt := 1; attempt <= 2; attempt++ {
		runErr = m.runScripts(ctx, sl, pool.SlotEnv(sl.Name), pool.ResetDB, "reset_db", "slot-"+sl.Name+".log", ResetDBTimeout)
		if runErr == nil {
			base := m.loadedSchema(ctx, sl, pool)
			return m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) {
				u.Set("dirty_schema", false)
				setSchema(u, base)
			})
		}
		if ctx.Err() != nil {
			return runErr
		}
	}
	broken := fmt.Errorf("%w: %s schema reset failed twice: %w", ErrBroken, sl.Name, runErr)
	from := []string{store.SlotDirtySchema}
	if hold != "" {
		from = []string{store.SlotHeld}
	}
	terr := m.d.Store.TransitionSlot(context.WithoutCancel(ctx), sl.ID, from, store.SlotBroken,
		func(u *store.SlotUpdate) {
			u.Set("last_error", redactErr(broken))
			if hold != "" { // so `magnum slots repair` takes it
				u.Where("hold_reason", hold)
				u.Set("hold_reason", nil)
			}
		})
	if terr != nil {
		return errors.Join(broken, terr)
	}
	return broken
}
