package slots

// Work in a pool slot outside a review round (`magnum merge-check`): the
// command holds a free slot for itself (HoldFree), checks commits out in it
// (CheckoutHeld) and hands it back (ReleaseHeld), and the slot stays in
// state held with the command's hold_reason the whole time. The daemon
// never claims, evicts, resumes, cleans up or releases a slot that has a
// hold_reason (FreeSlots, EvictableSlots, stalledRelease's states, the
// cleanup planner and Guard all leave it alone), so the work can run next
// to a live daemon: taking the slot is the compare-and-set a round's
// ClaimSlot is (free, unpinned, no hold_reason, no PR), and the slot is
// free again only in the release's last compare-and-set. A slot left held
// (a crash, --keep) shows the reason in `magnum slots` and `magnum status`;
// `magnum slots unpin` hands it to the daemon, whose next eviction releases
// it.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// HoldFree holds a free slot of pool for hold (a hold_reason such as
// "merge-check"): the slot named name, or else each of store.FreeSlots in
// turn (least recently used first). One compare-and-set moves it from free,
// unpinned, without hold_reason and without a PR to held with hold_reason =
// hold; a lost race moves on. Then the guard runs on it (guardHeld): a
// human's agent or process there hands the slot back (free again) and the
// next one is tried; a hold the guard persists (their changes, a moved HEAD,
// unpushed commits) keeps it held for them. A named slot that is pinned,
// held, holds a PR or is not free is refused (ErrHold or store.ErrConflict);
// none left is ErrNoFreeSlot. It returns the held slot.
func (m *Manager) HoldFree(ctx context.Context, pool config.Pool, name, hold string) (store.Slot, error) {
	if hold == "" {
		return store.Slot{}, errors.New("slots: hold a slot: no hold reason")
	}
	var cands []store.Slot
	if name != "" {
		sl, err := m.d.Store.SlotByName(ctx, name)
		if err != nil {
			return store.Slot{}, fmt.Errorf("slots: hold %s: %w", name, err)
		}
		if err := holdable(sl, pool); err != nil {
			return store.Slot{}, err
		}
		cands = []store.Slot{sl}
	} else {
		free, err := m.d.Store.FreeSlots(ctx, pool.Repo, 0)
		if err != nil {
			return store.Slot{}, fmt.Errorf("slots: hold a slot of %s: %w", pool.Repo, err)
		}
		cands = free
	}
	if m.d.DryRun {
		if len(cands) == 0 {
			return store.Slot{}, fmt.Errorf("slots: hold a slot of %s for %s: %w", pool.Repo, hold, ErrNoFreeSlot)
		}
		m.dryRun("hold %s for %s", cands[0].Name, hold)
		return cands[0], nil
	}
	var refused []error
	for _, sl := range cands {
		err := m.d.Store.TransitionSlot(ctx, sl.ID, []string{store.SlotFree}, store.SlotHeld, func(u *store.SlotUpdate) {
			u.Where("pinned", false)
			u.Where("hold_reason", nil)
			u.Where("pr_id", nil)
			u.Set("hold_reason", hold)
			u.Set("last_used_at", m.now())
		})
		if errors.Is(err, store.ErrConflict) {
			refused = append(refused, fmt.Errorf("%s was taken meanwhile", sl.Name))
			continue
		}
		if err != nil {
			return store.Slot{}, fmt.Errorf("slots: hold %s for %s: %w", sl.Name, hold, err)
		}
		m.event(ctx, "slot:"+sl.Name, "info", "slot.held", fmt.Sprintf("%s held for %s", sl.Name, hold))
		gerr := m.guardHeld(ctx, sl, hold)
		if gerr == nil {
			return m.reload(ctx, sl)
		}
		// Nothing in the slot changed yet: hand it back unless the guard
		// held it for someone (its hold_reason is no longer hold).
		back := m.handBack(ctx, sl, hold)
		if _, isHold := AsHold(gerr); !isHold {
			return store.Slot{}, errors.Join(gerr, back)
		}
		refused = append(refused, gerr)
	}
	if name != "" && len(refused) > 0 {
		return store.Slot{}, refused[0]
	}
	return store.Slot{}, fmt.Errorf("slots: hold a slot of %s for %s: %w", pool.Repo, hold,
		errors.Join(append([]error{ErrNoFreeSlot}, refused...)...))
}

// holdable refuses a slot HoldFree may not take: another pool's, pinned,
// held, holding a PR or not free.
func holdable(sl store.Slot, pool config.Pool) error {
	switch {
	case sl.Kind != store.SlotKindPool || !strings.EqualFold(sl.RepoFullName, pool.Repo):
		return fmt.Errorf("slots: %s is not a slot of the %s pool: %w", sl.Name, pool.Repo, store.ErrConflict)
	case sl.Pinned:
		return ErrHold{Reason: HoldPinned, Detail: sl.Name + " is pinned"}
	case store.Deref(sl.HoldReason) != "":
		return ErrHold{Reason: *sl.HoldReason, Detail: sl.Name + " is held"}
	case sl.PRID != nil:
		return fmt.Errorf("slots: %s holds a PR: %w", sl.Name, store.ErrConflict)
	case sl.State != store.SlotFree:
		return fmt.Errorf("slots: %s is %s, not free: %w", sl.Name, sl.State, store.ErrConflict)
	}
	return nil
}

// handBack frees a slot HoldFree took before anything in it changed: one
// compare-and-set from held with hold_reason = hold and no PR.
func (m *Manager) handBack(ctx context.Context, sl store.Slot, hold string) error {
	err := m.d.Store.TransitionSlot(context.WithoutCancel(ctx), sl.ID, []string{store.SlotHeld}, store.SlotFree, func(u *store.SlotUpdate) {
		u.Where("hold_reason", hold)
		u.Where("pr_id", nil)
		u.Set("hold_reason", nil)
	})
	switch {
	case errors.Is(err, store.ErrConflict):
		return nil // the guard held it for someone: it stays held
	case err != nil:
		return fmt.Errorf("slots: hand %s back: %w", sl.Name, err)
	}
	m.event(ctx, "slot:"+sl.Name, "info", "slot.unheld", fmt.Sprintf("%s handed back: not used for %s", sl.Name, hold))
	return nil
}

// heldFor refuses a slot that is not a pool slot held for hold.
func heldFor(sl store.Slot, hold string) error {
	if sl.Kind != store.SlotKindPool || sl.State != store.SlotHeld || store.Deref(sl.HoldReason) != hold {
		return fmt.Errorf("slots: %s is %s (hold %q), not held for %s: %w", sl.Name, sl.State, store.Deref(sl.HoldReason), hold, store.ErrConflict)
	}
	return nil
}

// guardHeld is Guard for a slot held for hold: it must still be held for
// hold and unpinned, and the rest of Guard runs as for any slot (a human's
// agent or process, HEAD drift, unpushed commits). The slot holds no PR, so
// no human activity of a PR counts.
func (m *Manager) guardHeld(ctx context.Context, slot store.Slot, hold string) error {
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	if err := heldFor(sl, hold); err != nil {
		return err
	}
	if sl.Pinned {
		return ErrHold{Reason: HoldPinned, Detail: sl.Name + " is pinned"}
	}
	return m.guardChecks(ctx, sl, store.PR{}, false)
}

// CheckoutHeld puts commit sha (present in the slot's main clone) into a
// slot held for hold, as Checkout puts a PR's head into a claimed one. Steps
// (subject "slot:<name>:<hold>:<sha7>", a new generation each call): guard
// (guardHeld), switch (detached at sha, tracked changes discarded after a
// slot.discarded event, sha written to the kv store first), render_mise,
// deps, schema (dirty_schema when sha changes pool.schema_paths since its
// merge base with origin/<base>) and verify (HEAD == sha → checked_out_sha,
// last_used_at). Whether the databases need the checkout's schema is
// EnsureSchema's call, as for a person's checkout.
func (m *Manager) CheckoutHeld(ctx context.Context, slot store.Slot, pool config.Pool, hold, sha string) error {
	if len(sha) < 7 {
		return fmt.Errorf("slots: check out %q in %s: not a commit id", sha, slot.Name)
	}
	if m.d.DryRun {
		m.dryRun("check out %s in %s", textx.ShortSHA(sha), slot.Name)
		return nil
	}
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	if err := heldFor(sl, hold); err != nil {
		return err
	}
	subject := fmt.Sprintf("slot:%s:%s:%s", sl.Name, hold, textx.ShortSHA(sha))
	if err := steps.ResetSubject(ctx, m.d.Store, subject); err != nil {
		return err
	}
	err = m.checkoutHeldSteps(ctx, subject, sl, pool, hold, sha)
	if err != nil {
		m.setLastError(ctx, sl.ID, err)
	}
	return err
}

func (m *Manager) checkoutHeldSteps(ctx context.Context, subject string, sl store.Slot, pool config.Pool, hold, sha string) error {
	if err := m.step(ctx, subject, "guard", func(ctx context.Context) error {
		return m.guardHeld(ctx, sl, hold)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "switch", func(ctx context.Context) error {
		m.discarding(ctx, sl)
		return m.switchTo(ctx, sl, sha, sha)
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
	if err := m.step(ctx, subject, "schema", func(ctx context.Context) error {
		return m.markSchema(ctx, sl, pool)
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
		return m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) {
			u.Set("checked_out_sha", sha)
			u.Set("last_used_at", m.now())
			u.Set("last_error", nil)
		})
	})
}

// ReleaseHeld hands a slot held for hold back to the pool with Release's
// steps (subject "slot:<name>:release": guard, fetch_base, reset to the
// placeholder at origin/<base>, delete_ref, render_mise, deps,
// schema_check, mark_free), deleting refs (under gitx.MergeCheckRefPrefix,
// in the main clone) in delete_ref. The slot stays held for hold until
// mark_free, one compare-and-set from held with that hold_reason to free
// with none, so the daemon cannot resume the release meanwhile. A failure
// leaves it held for hold, with last_error; two failed schema reloads move
// it to broken (ErrBroken). A guard that finds a human holds it for them.
func (m *Manager) ReleaseHeld(ctx context.Context, slot store.Slot, pool config.Pool, hold string, refs ...string) error {
	for _, ref := range refs {
		if !strings.HasPrefix(ref, gitx.MergeCheckRefPrefix) {
			return fmt.Errorf("slots: release %s: ref %q is not under %s", slot.Name, ref, gitx.MergeCheckRefPrefix)
		}
	}
	if m.d.DryRun {
		m.dryRun("release %s held for %s", slot.Name, hold)
		return nil
	}
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	if err := heldFor(sl, hold); err != nil {
		return err
	}
	subject := "slot:" + sl.Name + ":release"
	if err := steps.ResetSubject(ctx, m.d.Store, subject); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "guard", func(ctx context.Context) error {
		return m.guardHeld(ctx, sl, hold)
	}); err != nil {
		return err
	}
	err = m.releaseSteps(ctx, subject, sl, pool, hold, hold, refs)
	if err != nil && !errors.Is(err, ErrBroken) {
		m.setLastError(ctx, sl.ID, err)
	}
	return err
}
