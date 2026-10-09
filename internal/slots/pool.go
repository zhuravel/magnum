package slots

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
)

// ProvisionPool provisions pool slot number n (name pool.Slot(n), path
// pool.Path(n)). The slot row (state provisioning, kind pool, db_slug and
// placeholder_branch = name) is written first, then the steps of subject
// "slot:<name>" run: worktree_add (fetch origin/<base>, `git worktree add
// --no-track -b <name> <path> origin/<base>`, skipped when the path is
// already a worktree), render_mise, setup (each pool.setup command through
// mise exec, 45m, logged to logs/provision-<name>.log), verify_marker
// (tmp/.worktree-db-slug == name), verify_dbs (every pool database exists)
// and mark_free (provisioning → free, lock_sha, slot_databases).
//
// A slot that is already provisioned is left alone (nil). A provisioning
// slot resumes at the first step without an ok row; a removed or broken one
// starts over. A new or removed slot whose path already exists is refused
// (ErrConflict): that directory is not magnum's (adopt it instead). Failures leave the slot provisioning with last_error, except
// failed verifications, which move it to broken (ErrVerify). Below
// pool.min_free_disk_gb nothing is added (ErrLowDisk).
func (m *Manager) ProvisionPool(ctx context.Context, pool config.Pool, n int) error {
	if n <= 0 {
		return fmt.Errorf("slots: invalid slot number %d", n)
	}
	name, path := pool.Slot(n), pool.Path(n)
	if m.d.DryRun {
		m.dryRun("provision %s at %s (worktree add, render %s, %s)", name, path, MiseLocal, strings.Join(pool.Setup, "; "))
		return nil
	}
	sl, fresh, err := m.poolRow(ctx, pool, name, path)
	if err != nil {
		return err
	}
	if sl.State != store.SlotProvisioning {
		return nil
	}
	return m.provision(ctx, sl, pool, fresh)
}

// poolRow creates (or revives) the provisioning row for a pool slot. fresh
// reports whether the step history must start over.
func (m *Manager) poolRow(ctx context.Context, pool config.Pool, name, path string) (store.Slot, bool, error) {
	var repoID *int64
	if r, err := m.d.Store.RepoByFullName(ctx, pool.Repo); err == nil {
		repoID = &r.ID
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Slot{}, false, fmt.Errorf("slots: provision %s: %w", name, err)
	}
	if _, err := m.d.Store.SlotByName(ctx, name); errors.Is(err, store.ErrNotFound) && !missing(path) {
		return store.Slot{}, false, errPathTaken(name, path)
	}
	sl, err := m.d.Store.CreateSlot(ctx, store.Slot{
		Name: name, RepoID: repoID, RepoFullName: pool.Repo, Kind: store.SlotKindPool, Path: path,
		MainClone: pool.MainClone, PlaceholderBranch: &name, DBSlug: &name, State: store.SlotProvisioning,
	})
	if err == nil {
		return sl, true, nil
	}
	if !errors.Is(err, store.ErrConflict) {
		return store.Slot{}, false, fmt.Errorf("slots: provision %s: %w", name, err)
	}
	sl, err = m.d.Store.SlotByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Slot{}, false, fmt.Errorf("slots: provision %s: path %s belongs to another slot: %w", name, path, store.ErrConflict)
	}
	if err != nil {
		return store.Slot{}, false, fmt.Errorf("slots: provision %s: %w", name, err)
	}
	switch sl.State {
	case store.SlotProvisioning:
		return sl, false, nil
	case store.SlotRemoved, store.SlotBroken:
		if sl.State == store.SlotRemoved && !missing(path) {
			return store.Slot{}, false, errPathTaken(name, path)
		}
		err := m.reviveSlot(ctx, sl, "slot:"+name, store.SlotProvisioning, "reprovisioned", func(u *store.SlotUpdate) {
			u.Set("repo_id", repoID)
			u.Set("path", path)
			u.Set("main_clone", pool.MainClone)
			u.Set("placeholder_branch", name)
			u.Set("db_slug", name)
		})
		if err != nil {
			return store.Slot{}, false, fmt.Errorf("slots: provision %s: %w", name, err)
		}
		sl, err = m.reload(ctx, sl)
		return sl, false, err
	default:
		return sl, false, nil
	}
}

// errPathTaken refuses to provision over a directory magnum did not create.
func errPathTaken(name, path string) error {
	return fmt.Errorf("slots: provision %s: %s already exists and is not a registered slot; adopt it (magnum slots adopt) or move it away: %w",
		name, path, store.ErrConflict)
}

// reviveSlot starts a new life for slot row sl (removed, broken, lost, or a
// slot being repaired) in state to. The step generation of subject (the
// provisioning steps) is reset first, so a crash after the transition never
// resumes the previous life's steps. The per-use columns are reset (set
// applied on top) while sl keeps its state, then store.ReleaseSlot moves it
// to `to`, clearing pr_id and closing any open assignment with reason in
// one transaction: a slot never loses its PR but keeps the assignment.
func (m *Manager) reviveSlot(ctx context.Context, sl store.Slot, subject, to, reason string, set func(*store.SlotUpdate)) error {
	if err := steps.ResetSubject(ctx, m.d.Store, subject); err != nil {
		return err
	}
	err := m.d.Store.TransitionSlot(ctx, sl.ID, []string{sl.State}, sl.State, func(u *store.SlotUpdate) {
		resetSlotFields(u)
		if set != nil {
			set(u)
		}
	})
	if err != nil {
		return err
	}
	return m.d.Store.ReleaseSlot(ctx, sl.ID, []string{sl.State}, to, reason)
}

// resetSlotFields clears the per-use columns of a slot being (re)provisioned.
// pr_id is cleared by store.ReleaseSlot with the assignment (reviveSlot).
func resetSlotFields(u *store.SlotUpdate) {
	u.Set("pinned", false)
	u.Set("dirty_schema", false)
	u.Set("checked_out_sha", nil)
	u.Set("hold_reason", nil)
	u.Set("lock_sha", nil)
	u.Set("last_error", nil)
	setSchema(u, SchemaCheck{})
}

// provision runs the provisioning steps for a slot in state provisioning.
func (m *Manager) provision(ctx context.Context, sl store.Slot, pool config.Pool, fresh bool) error {
	subject := "slot:" + sl.Name
	if fresh {
		if err := steps.ResetSubject(ctx, m.d.Store, subject); err != nil {
			return err
		}
	}
	err := m.provisionSteps(ctx, subject, sl, pool)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrVerify) && ctx.Err() == nil {
		if terr := m.d.Store.TransitionSlot(context.WithoutCancel(ctx), sl.ID, []string{store.SlotProvisioning}, store.SlotBroken,
			func(u *store.SlotUpdate) { u.Set("last_error", redactErr(err)) }); terr != nil {
			m.logErr(ctx, terr, "slots: mark %s broken: %v", sl.Name, terr)
		}
		return err
	}
	m.setLastError(ctx, sl.ID, err)
	return err
}

func (m *Manager) provisionSteps(ctx context.Context, subject string, sl store.Slot, pool config.Pool) error {
	if done, err := steps.Done(ctx, m.d.Store, subject, "worktree_add"); err != nil {
		return err
	} else if !done {
		if err := m.checkDisk(sl.Path, pool.MinFreeDiskGB); err != nil {
			return err
		}
	}
	if err := m.step(ctx, subject, "worktree_add", func(ctx context.Context) error {
		return m.ensurePoolWorktree(ctx, sl, pool)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "render_mise", func(ctx context.Context) error {
		return m.renderMise(ctx, sl, pool)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "setup", func(ctx context.Context) error {
		return m.runScripts(ctx, sl, pool.SlotEnv(sl.Name), pool.Setup, "setup", "provision-"+sl.Name+".log", SetupTimeout)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "verify_marker", func(ctx context.Context) error {
		return verifyMarker(sl.Path, SlotDBSlug(sl))
	}); err != nil {
		return err
	}
	var dbs []mysqlx.Database
	if err := m.step(ctx, subject, "verify_dbs", func(ctx context.Context) error {
		var err error
		dbs, err = m.verifyDatabases(ctx, pool, SlotDBSlug(sl))
		return err
	}); err != nil {
		return err
	}
	return m.step(ctx, subject, "mark_free", func(ctx context.Context) error {
		lock, err := lockHash(sl.Path)
		if err != nil {
			return err
		}
		// Databases first: once the slot is free, ProvisionPool no longer resumes it.
		if err := m.recordDatabases(ctx, sl, pool, dbs); err != nil {
			return err
		}
		// The setup loaded the checkout's schema, as the release trusted
		// before: a first round of a PR with the same schema does not reload.
		loaded := m.loadedSchema(ctx, sl, pool)
		err = m.d.Store.TransitionSlot(ctx, sl.ID, []string{store.SlotProvisioning}, store.SlotFree, func(u *store.SlotUpdate) {
			u.Set("lock_sha", lock)
			u.Set("last_error", nil)
			setSchema(u, loaded)
		})
		if err != nil {
			return fmt.Errorf("slots: mark %s free: %w", sl.Name, err)
		}
		return nil
	})
}

// ensurePoolWorktree adds the slot's worktree on its placeholder branch at
// origin/<base> unless git already lists the path.
func (m *Manager) ensurePoolWorktree(ctx context.Context, sl store.Slot, pool config.Pool) error {
	if _, ok, err := m.listedWorktree(ctx, sl); err != nil || ok {
		return err
	}
	if err := m.git.FetchBranch(ctx, sl.MainClone, pool.Base); err != nil {
		return err
	}
	branch := placeholder(sl)
	base := "origin/" + pool.Base
	if _, err := m.git.RevParse(ctx, sl.MainClone, "refs/heads/"+branch); errors.Is(err, gitx.ErrNoSuchRef) {
		return m.git.WorktreeAdd(ctx, sl.MainClone, sl.Path, base, false, branch)
	} else if err != nil {
		return err
	}
	// The placeholder branch survived an earlier removal: add detached, then
	// point the branch at origin/<base> (refused by git if another worktree
	// has it checked out).
	list, err := m.git.WorktreeList(ctx, sl.MainClone)
	if err != nil {
		return err
	}
	for _, w := range list {
		if w.Branch == branch {
			return fmt.Errorf("slots: branch %s is checked out in %s: %w", branch, w.Path, store.ErrConflict)
		}
	}
	if err := m.git.WorktreeAdd(ctx, sl.MainClone, sl.Path, base, true, ""); err != nil {
		return err
	}
	return m.git.ResetPlaceholder(ctx, sl.Path, branch, pool.Base)
}

func verifyMarker(dir, want string) error {
	got, err := ReadMarker(dir)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrVerify, MarkerFile, err)
	}
	if got != want {
		return fmt.Errorf("%w: %s says %q, want %q", ErrVerify, MarkerFile, got, want)
	}
	return nil
}

// verifyDatabases checks that every pool database of slug exists and returns them.
func (m *Manager) verifyDatabases(ctx context.Context, pool config.Pool, slug string) ([]mysqlx.Database, error) {
	all, err := m.listDatabases(ctx, pool)
	if err != nil {
		return nil, err
	}
	byName := map[string]mysqlx.Database{}
	for _, d := range all {
		byName[d.Name] = d
	}
	var out []mysqlx.Database
	var missing []string
	for _, n := range pool.DBNames(slug) {
		d, ok := byName[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		out = append(out, d)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: databases missing: %s", ErrVerify, strings.Join(missing, ", "))
	}
	return out, nil
}

// recordDatabases upserts the slot's databases into slot_databases.
func (m *Manager) recordDatabases(ctx context.Context, sl store.Slot, pool config.Pool, dbs []mysqlx.Database) error {
	if dbs == nil {
		var err error
		if dbs, err = m.verifyDatabases(ctx, pool, SlotDBSlug(sl)); err != nil {
			return err
		}
	}
	for _, d := range dbs {
		size := d.SizeMB
		_, err := m.d.Store.UpsertSlotDatabase(ctx, store.SlotDatabase{SlotID: &sl.ID, DBName: d.Name, Slug: d.Slug, SizeMB: &size})
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) checkDisk(path string, minGB int) error {
	if minGB <= 0 {
		return nil
	}
	free, err := m.d.FreeDiskBytes(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("slots: disk check: %w", err)
	}
	if need := uint64(minGB) << 30; free < need {
		return fmt.Errorf("%w: %.1f GB free under %s, need %d GB", ErrLowDisk, float64(free)/(1<<30), filepath.Dir(path), minGB)
	}
	return nil
}

// NextSlotNumber returns the lowest slot number whose name is not used by a
// non-removed slot and whose path does not exist (an unregistered directory,
// or a removed slot's path that is back, is a human's, not magnum's to take
// over). It does not check pool.max.
func (m *Manager) NextSlotNumber(ctx context.Context, pool config.Pool) (int, error) {
	list, err := m.d.Store.ListSlots(ctx, store.SlotFilter{RepoFullName: pool.Repo, Kind: store.SlotKindPool})
	if err != nil {
		return 0, fmt.Errorf("slots: next slot: %w", err)
	}
	state := map[string]string{}
	for _, sl := range list {
		state[sl.Name] = sl.State
	}
	for n := 1; ; n++ {
		st, known := state[pool.Slot(n)]
		switch {
		case known && st != store.SlotRemoved:
			continue
		case !missing(pool.Path(n)):
			continue
		}
		return n, nil
	}
}

// Repair re-runs provisioning (render, setup, verification) for a free,
// broken, provisioning or lost slot; a missing worktree is added again. The
// slot ends free, its PR and open assignment released (reason "repaired").
// A pinned or held slot is refused (ErrHold): Unpin it first.
func (m *Manager) Repair(ctx context.Context, slot store.Slot, pool config.Pool) error {
	if m.d.DryRun {
		m.dryRun("repair %s (%s)", slot.Name, strings.Join(pool.Setup, "; "))
		return nil
	}
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	if sl.Kind != store.SlotKindPool {
		return fmt.Errorf("slots: repair %s: only pool slots can be repaired: %w", sl.Name, store.ErrConflict)
	}
	if !slices.Contains([]string{store.SlotFree, store.SlotBroken, store.SlotProvisioning, store.SlotLost}, sl.State) {
		return fmt.Errorf("slots: repair %s in state %s: %w", sl.Name, sl.State, store.ErrConflict)
	}
	if sl.Pinned {
		return ErrHold{Reason: HoldPinned, Detail: sl.Name + " is pinned; unpin it before a repair"}
	}
	if r := store.Deref(sl.HoldReason); r != "" {
		return ErrHold{Reason: r, Detail: sl.Name + " is held; unpin it before a repair"}
	}
	if err := m.reviveSlot(ctx, sl, "slot:"+sl.Name, store.SlotProvisioning, "repaired", nil); err != nil {
		return fmt.Errorf("slots: repair %s: %w", sl.Name, err)
	}
	if sl, err = m.reload(ctx, sl); err != nil {
		return err
	}
	return m.provision(ctx, sl, pool, false)
}

// Adopt registers an existing checkout at path as a free pool slot: path must
// be pool.Path(n) for some n, a worktree of the main clone, its
// tmp/.worktree-db-slug must say pool.Slot(n), and all its databases must
// exist (ErrVerify otherwise). An already registered live slot is ErrConflict;
// a removed, lost or broken row with that name is revived (its PR and open
// assignment released with reason "adopted").
func (m *Manager) Adopt(ctx context.Context, pool config.Pool, path string) (store.Slot, error) {
	path = filepath.Clean(paths.Expand(path))
	n, ok := slotNumberForPath(pool, path)
	if !ok {
		return store.Slot{}, fmt.Errorf("slots: adopt %s: not a path of the %s pool (%s)", path, pool.Repo, pool.SlotPath)
	}
	name := pool.Slot(n)
	if m.d.DryRun {
		m.dryRun("adopt %s as %s", path, name)
		return store.Slot{Name: name, Path: path, Kind: store.SlotKindPool, State: store.SlotFree}, nil
	}
	list, err := m.git.WorktreeList(ctx, pool.MainClone)
	if err != nil {
		return store.Slot{}, err
	}
	if _, ok := gitx.FindWorktree(list, path); !ok {
		return store.Slot{}, fmt.Errorf("%w: %s is not a worktree of %s", ErrVerify, path, pool.MainClone)
	}
	if err := verifyMarker(path, name); err != nil {
		return store.Slot{}, err
	}
	dbs, err := m.verifyDatabases(ctx, pool, name)
	if err != nil {
		return store.Slot{}, err
	}
	lock, err := lockHash(path)
	if err != nil {
		return store.Slot{}, err
	}
	var repoID *int64
	if r, err := m.d.Store.RepoByFullName(ctx, pool.Repo); err == nil {
		repoID = &r.ID
	}
	sl, err := m.d.Store.CreateSlot(ctx, store.Slot{
		Name: name, RepoID: repoID, RepoFullName: pool.Repo, Kind: store.SlotKindPool, Path: path,
		MainClone: pool.MainClone, PlaceholderBranch: &name, DBSlug: &name, State: store.SlotFree, LockSHA: &lock,
	})
	if errors.Is(err, store.ErrConflict) {
		existing, gerr := m.d.Store.SlotByName(ctx, name)
		if gerr != nil || !slices.Contains([]string{store.SlotRemoved, store.SlotLost, store.SlotBroken}, existing.State) {
			return store.Slot{}, fmt.Errorf("slots: adopt %s: %s is already registered: %w", path, name, store.ErrConflict)
		}
		err = m.reviveSlot(ctx, existing, "slot:"+name, store.SlotFree, "adopted", func(u *store.SlotUpdate) {
			u.Set("path", path)
			u.Set("main_clone", pool.MainClone)
			u.Set("placeholder_branch", name)
			u.Set("db_slug", name)
		})
		if err == nil { // after the revive: it clears lock_sha with the other per-use columns
			err = m.d.Store.UpdateSlotFields(ctx, existing.ID, func(u *store.SlotUpdate) { u.Set("lock_sha", lock) })
		}
		if err == nil {
			sl, err = m.reload(ctx, existing)
		}
	}
	if err != nil {
		return store.Slot{}, fmt.Errorf("slots: adopt %s: %w", path, err)
	}
	if err := m.recordDatabases(ctx, sl, pool, dbs); err != nil {
		return store.Slot{}, err
	}
	m.event(ctx, "slot:"+name, "info", "slot.adopted", "adopted "+path)
	return sl, nil
}

// Remove tears a pool slot down: steps of subject "slot:<name>:remove" are
// guard (skipped with force: Guard, untracked files counted as changes since
// the removal deletes them), teardown (pool.teardown through mise exec,
// skipped when the directory is gone), verify_dbs_gone (leftover pool
// databases dropped with the pool's drop guard, slot_databases marked
// dropped), worktree_remove (--force with force or over magnum's residue,
// see removeWorktree), branch_delete
// (placeholder, -D), worktree_prune and mark_removed. RemoveStates can be
// removed; with force also ForceRemoveStates (never busy). A removing slot
// resumes after the guard ran again; a removed one is a no-op. Per-PR slots
// are handed to RemovePRWorktree.
func (m *Manager) Remove(ctx context.Context, slot store.Slot, pool config.Pool, force bool) error {
	if m.d.DryRun {
		m.dryRun("remove %s at %s (force=%v)", slot.Name, slot.Path, force)
		return nil
	}
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	if sl.Kind == store.SlotKindPerPR {
		return m.removePR(ctx, sl, force, "removed")
	}
	if sl.State == store.SlotRemoved {
		return nil
	}
	from := RemoveStates
	if force {
		from = slices.Concat(RemoveStates, ForceRemoveStates)
	}
	subject := "slot:" + sl.Name + ":remove"
	if err := m.beginRemove(ctx, sl, subject, from, force); err != nil {
		return err
	}
	err = m.removeSteps(ctx, subject, sl, pool, force)
	if err != nil {
		m.setLastError(ctx, sl.ID, err)
	}
	return err
}

// beginRemove starts or resumes the removal of sl. A slot in one of from
// starts a new step generation, passes the removal guard (a step) and moves
// to removing; a removing slot resumes, after the removal guard ran again
// (whatever happened since the last attempt is checked before the next
// destructive step). force skips the guard.
func (m *Manager) beginRemove(ctx context.Context, sl store.Slot, subject string, from []string, force bool) error {
	switch {
	case sl.State == store.SlotRemoving:
		return m.removeGuard(ctx, sl, force)
	case !slices.Contains(from, sl.State):
		return fmt.Errorf("slots: remove %s in state %s: %w", sl.Name, sl.State, store.ErrConflict)
	}
	if err := steps.ResetSubject(ctx, m.d.Store, subject); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "guard", func(ctx context.Context) error {
		return m.removeGuard(ctx, sl, force)
	}); err != nil {
		return err
	}
	if err := m.d.Store.TransitionSlot(ctx, sl.ID, from, store.SlotRemoving, nil); err != nil {
		return fmt.Errorf("slots: remove %s: %w", sl.Name, err)
	}
	return nil
}

// removeGuard is Guard with untracked files counted as changes: a removal
// deletes the whole directory. force skips it.
func (m *Manager) removeGuard(ctx context.Context, sl store.Slot, force bool) error {
	if force {
		return nil
	}
	return m.guard(ctx, sl, true)
}

func (m *Manager) removeSteps(ctx context.Context, subject string, sl store.Slot, pool config.Pool, force bool) error {
	if err := m.step(ctx, subject, "teardown", func(ctx context.Context) error {
		if !fsx.Exists(sl.Path) {
			return nil
		}
		return m.runScripts(ctx, sl, pool.SlotEnv(sl.Name), pool.Teardown, "teardown", "slot-"+sl.Name+".log", TeardownTimeout)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "verify_dbs_gone", func(ctx context.Context) error {
		return m.dropLeftovers(ctx, sl, pool)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "worktree_remove", func(ctx context.Context) error {
		return m.removeWorktree(ctx, sl, force)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "branch_delete", func(ctx context.Context) error {
		if missing(sl.MainClone) {
			return nil
		}
		return m.git.BranchDelete(ctx, sl.MainClone, placeholder(sl), true)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "worktree_prune", func(ctx context.Context) error {
		if missing(sl.MainClone) {
			return nil
		}
		// Plain prune only forgets worktrees whose directory is gone. Caveat:
		// a worktree of the main clone on a volume that is not mounted right
		// now looks gone too and loses its registration (git worktree repair
		// restores it).
		return m.git.WorktreePrune(ctx, sl.MainClone)
	}); err != nil {
		return err
	}
	return m.step(ctx, subject, "mark_removed", func(ctx context.Context) error {
		return m.markRemoved(ctx, sl, "removed")
	})
}

// markRemoved moves a removing slot to removed, clearing its PR and closing
// any open assignment.
func (m *Manager) markRemoved(ctx context.Context, sl store.Slot, reason string) error {
	if err := m.d.Store.DeleteKV(ctx, switchingKey(sl.ID)); err != nil {
		return err
	}
	err := m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) {
		u.Set("checked_out_sha", nil)
		u.Set("dirty_schema", false)
		u.Set("hold_reason", nil)
		u.Set("pinned", false)
		u.Set("last_error", nil)
		setSchema(u, SchemaCheck{})
	})
	if err != nil {
		return err
	}
	return m.d.Store.ReleaseSlot(ctx, sl.ID, []string{store.SlotRemoving}, store.SlotRemoved, reason)
}

// dropLeftovers drops the pool databases of the slot that still exist (after
// teardown) through the pool's drop guard and records them as dropped.
func (m *Manager) dropLeftovers(ctx context.Context, sl store.Slot, pool config.Pool) error {
	if m.d.MySQL == nil {
		return errors.New("slots: no MySQL client configured")
	}
	want := pool.DBNames(SlotDBSlug(sl))
	present, err := m.presentDatabases(ctx, pool, want)
	if err != nil {
		return err
	}
	var errs []error
	if len(present) > 0 {
		for _, r := range m.d.MySQL.DropAll(ctx, present, DropGuard(pool)) {
			if r.Err != nil {
				errs = append(errs, fmt.Errorf("drop %s: %w", r.Name, r.Err))
			}
		}
	}
	if left, err := m.presentDatabases(ctx, pool, want); err != nil {
		errs = append(errs, err)
	} else if len(left) > 0 {
		errs = append(errs, fmt.Errorf("%w: databases still present: %s", ErrVerify, strings.Join(left, ", ")))
	}
	if len(errs) > 0 {
		return fmt.Errorf("slots: drop databases of %s: %w", sl.Name, errors.Join(errs...))
	}
	for _, n := range want {
		if err := m.d.Store.MarkSlotDatabaseDropped(ctx, n, "slots.remove"); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// listDatabases lists the pool's databases (ListPoolDatabases); templates
// that cannot be listed are logged.
func (m *Manager) listDatabases(ctx context.Context, pool config.Pool) ([]mysqlx.Database, error) {
	if m.d.MySQL == nil {
		return nil, errors.New("slots: no MySQL client configured")
	}
	all, bad, err := ListPoolDatabases(ctx, m.d.MySQL, pool)
	for _, t := range bad {
		m.logf("slots: %s: database template %q does not end in __{slug}; its databases cannot be listed or dropped", pool.Repo, t)
	}
	if err != nil {
		return nil, fmt.Errorf("slots: list databases: %w", err)
	}
	return all, nil
}

func (m *Manager) presentDatabases(ctx context.Context, pool config.Pool, names []string) ([]string, error) {
	all, err := m.listDatabases(ctx, pool)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, d := range all {
		have[d.Name] = true
	}
	var out []string
	for _, n := range names {
		if have[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

// removeWorktree removes the slot's worktree when git still lists it. A
// checkout whose main clone is gone too is already removed. Without force,
// changes (tracked or untracked) a human may have made since the checkout
// (HumanEvidence; the removal guard checked herdr) are a HoldDirtyWorktree;
// anything else left in the tree is magnum's residue, removed with --force
// after a slot.discarded event names it.
func (m *Manager) removeWorktree(ctx context.Context, sl store.Slot, force bool) error {
	if missing(sl.Path) && missing(sl.MainClone) {
		return nil
	}
	list, err := m.git.WorktreeList(ctx, sl.MainClone)
	if err != nil {
		return err
	}
	w, ok := gitx.FindWorktree(list, sl.Path)
	if !ok {
		return nil
	}
	if w.Prunable { // directory already gone: drop the stale entry so the branch can be deleted
		return m.git.WorktreePrune(ctx, sl.MainClone)
	}
	if !force {
		why, err := m.HumanEvidence(ctx, sl)
		if err != nil {
			return err
		}
		if why != "" {
			if err := m.holdChanges(ctx, sl, true, why); err != nil {
				return err
			}
		}
	}
	dirty, err := m.noteDiscard(ctx, sl, true)
	switch {
	case err != nil && !force:
		return err
	case err != nil:
		m.logErr(ctx, err, "slots: list the changes in %s before a forced removal: %v", sl.Path, err)
	}
	return m.git.WorktreeRemove(ctx, sl.MainClone, sl.Path, force || dirty)
}

// missing reports whether path definitely does not exist (an unreadable
// path is not missing).
func missing(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func redactErr(err error) string {
	if err == nil {
		return ""
	}
	return execx.Redact(err.Error())
}
