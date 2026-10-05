package cleanup

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// Apply executes plan in order. A dry-run plan executes nothing (every result
// is planned); an action that needs typed confirmation runs only when
// confirmed is true (otherwise unconfirmed, not an error). Each action is
// re-checked against the current registry first, so a stale plan fails that
// action (wrapping store.ErrConflict) instead of touching something that
// changed. Apply keeps going past failed actions, writes one audit event per
// executed action and returns the report plus the failures joined (nil when
// none failed).
func (p *Planner) Apply(ctx context.Context, plan Plan, confirmed bool) (Report, error) {
	if err := p.check(); err != nil {
		return Report{}, err
	}
	rep := Report{Results: make([]Result, 0, len(plan.Actions))}
	var errs []error
	var ext *inventory.Inventory
	for _, a := range plan.Actions {
		r := Result{Action: a}
		switch {
		case plan.DryRun:
			r.Status = StatusPlanned
			rep.Planned++
		case a.Confirm && !confirmed:
			r.Status, r.Error = StatusUnconfirmed, ErrUnconfirmed.Error()
			rep.Unconfirmed++
		case ctx.Err() != nil:
			err := ctx.Err()
			r.Status, r.Error = StatusFailed, err.Error()
			rep.Failed++
			errs = append(errs, fmt.Errorf("%s %s: %w", a.Kind, a.Subject, err))
		default:
			dropped, err := p.applyOne(ctx, a, &ext)
			rep.DroppedDBs = append(rep.DroppedDBs, dropped...)
			if err != nil {
				r.Status, r.Error = StatusFailed, execx.Redact(err.Error())
				rep.Failed++
				errs = append(errs, fmt.Errorf("%s %s: %w", a.Kind, a.Subject, err))
			} else {
				r.Status = StatusDone
				rep.Done++
				rep.DiskBytes += a.Bytes
				rep.MySQLBytes += a.DBBytes
			}
			p.event(ctx, logSubject(a), "cleanup."+a.Kind, a, err)
		}
		rep.Results = append(rep.Results, r)
	}
	return rep, errors.Join(errs...)
}

func (p *Planner) applyOne(ctx context.Context, a Action, ext **inventory.Inventory) ([]string, error) {
	switch a.Kind {
	case KindRelease, KindRemoveSlot, KindRemoveWorktree:
		return nil, p.applySlot(ctx, a)
	case KindDropDBs:
		return p.applyDrop(ctx, a)
	case KindResetExternal:
		return nil, p.applyReset(ctx, a, ext)
	}
	return nil, fmt.Errorf("%w: unknown action kind %q", ErrOptions, a.Kind)
}

func stale(format string, args ...any) error {
	return fmt.Errorf("cleanup: "+format+": %w", append(args, store.ErrConflict)...)
}

// applySlot releases or removes a slot, moving a closed PR to releasing and
// then released around it. A slot-less release (SlotID 0) only parks the
// PR's sessions and finishes that move.
func (p *Planner) applySlot(ctx context.Context, a Action) error {
	if a.SlotID == 0 && (a.PRID == 0 || a.Kind != KindRelease || !closing(a.PRState)) {
		return fmt.Errorf("%w: %s %s without a slot", ErrOptions, a.Kind, a.Subject)
	}
	var sl store.Slot
	var err error
	if a.SlotID != 0 {
		if sl, err = p.Store.SlotByID(ctx, a.SlotID); err != nil {
			return fmt.Errorf("cleanup: %w", err)
		}
	}
	switch {
	case a.SlotID == 0:
	case sl.Name != a.Slot:
		return stale("slot %d is now %s, not %s", sl.ID, sl.Name, a.Slot)
	case a.PRID != 0 && sl.PRID == nil && sl.Kind == store.SlotKindPerPR:
		// The PR's failed per-PR worktree (never claimed): checked by name below.
	case a.PRID != 0 && (sl.PRID == nil || *sl.PRID != a.PRID):
		return stale("slot %s no longer holds the PR", sl.Name)
	case a.PRID == 0 && sl.PRID != nil:
		return stale("slot %s now holds a PR", sl.Name)
	case sl.Pinned:
		return stale("slot %s was pinned since the plan", sl.Name)
	case sl.HoldReason != nil:
		return stale("slot %s is held (%s)", sl.Name, *sl.HoldReason)
	case sl.State == store.SlotBusy:
		return stale("slot %s is busy (active round)", sl.Name)
	}
	var pool config.Pool
	if a.SlotID != 0 && sl.Kind == store.SlotKindPool {
		pp := p.Config.PoolFor(sl.RepoFullName)
		if pp == nil {
			return fmt.Errorf("cleanup: no [[pool]] for %s", sl.RepoFullName)
		}
		pool = *pp
	}

	var pr store.PR
	if a.PRID != 0 {
		if pr, err = p.Store.PRByID(ctx, a.PRID); err != nil {
			return fmt.Errorf("cleanup: %w", err)
		}
		if closing(a.PRState) && !closing(pr.State) {
			return stale("PR was reopened (now %s)", pr.State)
		}
		if a.SlotID != 0 && sl.PRID == nil && sl.Name != slots.PRSlotName(a.Repo, pr.Number) {
			return stale("slot %s does not belong to the PR", sl.Name)
		}
		if pr.Pinned {
			return stale("PR was pinned since the plan")
		}
		if detail, err := p.activeDetail(ctx, pr); err != nil {
			return err
		} else if detail != "" {
			return stale("active round: %s", detail)
		}
	}

	// A forced removal skips the slots guard entirely; keep its live checks
	// (a human's agent or process in the slot) and accept only what force is
	// for: a moved HEAD, unpushed commits or a dirty tree.
	if a.SlotID != 0 && a.Force && a.Kind != KindRelease {
		if err := p.Slots.Guard(ctx, sl); err != nil {
			h, ok := slots.AsHold(err)
			if !ok || !slices.Contains([]string{slots.HoldHeadDrift, slots.HoldUnpushed, slots.HoldDirtyWorktree}, h.Reason) {
				return err
			}
			p.logf("cleanup: %s: forcing past %s (%s)", sl.Name, h.Reason, h.Detail)
		}
	}
	// A closed PR moves to releasing before the first side effect: whatever
	// races the release for it (a reopen, or a post-merge review request,
	// which moves it closed → rereview_pending) loses this compare-and-set,
	// or wins it and the release stops here, before anything was parked.
	// A failed park puts the PR back in closed.
	closingPR := a.PRID != 0 && closing(pr.State)
	toReleasing := closingPR && pr.State == store.PRClosed
	if toReleasing {
		if err := p.Store.TransitionPR(ctx, pr.ID, []string{store.PRClosed}, store.PRReleasing, nil); err != nil {
			return fmt.Errorf("cleanup: PR to releasing: %w", err)
		}
	}
	if a.PRID != 0 && p.Park != nil {
		if err := p.Park(ctx, pr); err != nil {
			if toReleasing {
				if rerr := p.Store.TransitionPR(ctx, pr.ID, []string{store.PRReleasing}, store.PRClosed, nil); rerr != nil {
					err = errors.Join(err, fmt.Errorf("PR back to closed: %w", rerr))
				}
			}
			return fmt.Errorf("cleanup: park sessions: %w", err)
		}
	}
	switch {
	case a.SlotID == 0:
	case a.Kind == KindRelease:
		err = p.Slots.Release(ctx, sl, pool, cmp.Or(a.Reason, "cleanup"))
	case a.Kind == KindRemoveWorktree:
		err = p.Slots.RemovePRWorktree(ctx, sl, a.Force)
	case a.Kind == KindRemoveSlot:
		err = p.Slots.Remove(ctx, sl, pool, a.Force)
	}
	if err != nil {
		return err
	}
	p.logf("cleanup: %s %s done", a.Kind, a.Subject)
	if closingPR {
		if err := p.Store.TransitionPR(ctx, pr.ID, []string{store.PRReleasing}, store.PRReleased, nil); err != nil {
			return fmt.Errorf("cleanup: PR to released: %w", err)
		}
		if a.SlotID != 0 {
			p.event(ctx, prSubject(pr.ID), "cleanup.pr_released", a, nil)
		}
	}
	return nil
}

func prSubject(id int64) string { return "pr:" + strconv.FormatInt(id, 10) }

// activeDetail explains why pr has a round in flight now ("" when none).
func (p *Planner) activeDetail(ctx context.Context, pr store.PR) (string, error) {
	if slices.Contains(activePRStates, pr.State) {
		return "PR is " + pr.State, nil
	}
	runs, err := p.Store.ActiveRuns(ctx)
	if err != nil {
		return "", fmt.Errorf("cleanup: %w", err)
	}
	for _, r := range runs {
		if r.PRID == pr.ID {
			return "run " + r.ID + " is " + r.State, nil
		}
	}
	return "", nil
}

// applyDrop drops one slug's orphan databases through the matching guard and
// records each drop in slot_databases. Ownership is scanned again right
// before: every planned database must still be an orphan of a.Slug on
// complete facts, and no registry slot may claim the slug.
func (p *Planner) applyDrop(ctx context.Context, a Action) ([]string, error) {
	if p.MySQL == nil {
		return nil, errors.New("cleanup: Planner.MySQL is required to drop databases")
	}
	all, err := p.Store.ListSlots(ctx, store.SlotFilter{})
	if err != nil {
		return nil, fmt.Errorf("cleanup: %w", err)
	}
	for _, sl := range all {
		if sl.State != store.SlotRemoved && slots.SlotDBSlug(sl) == a.Slug {
			return nil, stale("slug %s now belongs to slot %s", a.Slug, sl.Name)
		}
	}
	if err := p.stillOrphaned(ctx, a); err != nil {
		return nil, err
	}
	guard, ok := autoGuard(p.Config, a.Slug)
	if a.Confirm {
		guard, ok = manualGuard(p.Config, a.Slug, a.DBNames)
	}
	if !ok {
		return nil, fmt.Errorf("cleanup: no drop guard allows slug %s", a.Slug)
	}
	var dropped []string
	var errs []error
	for _, r := range p.MySQL.DropAll(ctx, a.DBNames, guard) {
		if r.Err != nil {
			errs = append(errs, fmt.Errorf("drop %s: %w", r.Name, r.Err))
			continue
		}
		dropped = append(dropped, r.Name)
		if err := p.markDropped(ctx, r.Name, a.Slug); err != nil {
			errs = append(errs, err)
		}
	}
	return dropped, errors.Join(errs...)
}

// stillOrphaned scans the inventory again and refuses the drop unless every
// database of a carries a.Slug and is still listed as an orphan: a worktree
// created since the plan (or a source that cannot be read now) stops it.
func (p *Planner) stillOrphaned(ctx context.Context, a Action) error {
	if len(a.DBNames) == 0 {
		return fmt.Errorf("%w: drop_dbs %s names no database", ErrOptions, a.Slug)
	}
	inv, err := p.Inventory.Scan(ctx, inventory.Options{})
	if err != nil {
		return fmt.Errorf("cleanup: inventory: %w", err)
	}
	if !orphansKnown(inv) {
		return fmt.Errorf("cleanup: %s: %s", SkipIncomplete, orphansUnknown(inv))
	}
	for _, n := range a.DBNames {
		if slug, ok := mysqlx.Slug(n); !ok || slug != a.Slug {
			return fmt.Errorf("%w: database %s does not carry slug %s", ErrOptions, n, a.Slug)
		}
		if !slices.ContainsFunc(inv.OrphanDBs, func(d mysqlx.Database) bool { return d.Name == n }) {
			return stale("database %s is no longer an orphan", n)
		}
	}
	return nil
}

func (p *Planner) markDropped(ctx context.Context, name, slug string) error {
	err := p.Store.MarkSlotDatabaseDropped(ctx, name, DroppedBy)
	if errors.Is(err, store.ErrNotFound) {
		if _, err = p.Store.UpsertSlotDatabase(ctx, store.SlotDatabase{DBName: name, Slug: slug}); err == nil {
			err = p.Store.MarkSlotDatabaseDropped(ctx, name, DroppedBy)
		}
	}
	if err != nil {
		return fmt.Errorf("cleanup: record drop of %s: %w", name, err)
	}
	return nil
}

// applyReset resets a manual worktree after re-checking it with a fresh
// inventory scan (scanned once per Apply).
func (p *Planner) applyReset(ctx context.Context, a Action, ext **inventory.Inventory) error {
	if p.Git == nil {
		return errors.New("cleanup: Planner.Git is required to reset a manual worktree")
	}
	if canon(a.Path) == canon(a.MainClone) {
		return fmt.Errorf("cleanup: %s is the main clone: %w", a.Path, ErrOptions)
	}
	if *ext == nil {
		inv, err := p.Inventory.Scan(ctx, inventory.Options{External: true})
		if err != nil {
			return fmt.Errorf("cleanup: inventory: %w", err)
		}
		*ext = &inv
	}
	i := slices.IndexFunc((*ext).External, func(ev inventory.ExternalView) bool { return canon(ev.Path) == canon(a.Path) })
	if i < 0 || !(*ext).External[i].Exists {
		return stale("manual worktree %s not found", a.Path)
	}
	if reason, detail := externalBlock(ctx, p.Git, (*ext).AgentsListed, (*ext).External[i], a.Branch, a.Force); reason != "" {
		return stale("%s: %s", reason, detail)
	}
	if err := p.Git.FetchBranch(ctx, a.MainClone, a.Base); err != nil {
		return err
	}
	if err := p.Git.ResetPlaceholder(ctx, a.Path, a.Branch, a.Base); err != nil {
		return err
	}
	// The main clone's .mise.local.toml (it holds secrets: its permissions
	// are kept), written inside the worktree only.
	return slots.CopyIntoCheckout(filepath.Join(a.MainClone, slots.MiseLocal), a.Path, slots.MiseLocal)
}

// logSubject is the events.subject of an action: slot:<name> for slot
// operations (`magnum logs <slot>`), pr:<id> for a slot-less release,
// slug:<slug> for drops, path:<dir> for external resets.
func logSubject(a Action) string {
	switch {
	case a.SlotID == 0 && a.PRID != 0:
		return prSubject(a.PRID)
	case a.Kind == KindDropDBs:
		return "slug:" + a.Slug
	case a.Kind == KindResetExternal:
		return "path:" + a.Path
	}
	return "slot:" + a.Slot
}

func (p *Planner) event(ctx context.Context, subject, kind string, a Action, err error) {
	level, msg := "info", fmt.Sprintf("%s %s (%s): ok", a.Kind, a.Subject, a.Why)
	if kind == "cleanup.pr_released" {
		msg = fmt.Sprintf("PR released after %s of %s", a.Kind, a.Slot)
	}
	if err != nil {
		level, msg = "error", fmt.Sprintf("%s %s (%s) failed: %v", a.Kind, a.Subject, a.Why, err)
	}
	data, _ := json.Marshal(a)
	_, e := p.Store.AppendEvent(context.WithoutCancel(ctx), store.Event{
		Level: level, Subject: &subject, Kind: kind, Message: execx.Redact(msg), Data: data,
	})
	if e != nil {
		p.logf("cleanup: event %s %s: %v", subject, kind, e)
	}
}
