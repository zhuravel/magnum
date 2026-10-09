package slots

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// DefaultCloneRoot is used when a watch has no clone_root.
const DefaultCloneRoot = "~/Projects"

// CloneRoot is the watch's expanded clone_root (DefaultCloneRoot when unset).
func CloneRoot(watch config.Watch) string {
	root := watch.CloneRoot
	if root == "" {
		root = DefaultCloneRoot
	}
	return paths.Expand(root)
}

// PRWorktreePath is the per-PR worktree of a main clone:
// <clone>__worktrees/pr-<N>, next to the clone whatever it is named
// (~/Projects/<owner>-<name>__worktrees/pr-7).
func PRWorktreePath(mainClone string, number int) string {
	return filepath.Join(filepath.Clean(mainClone)+gitx.WorktreesSuffix, "pr-"+strconv.Itoa(number))
}

// PRWorktreePaths returns the default layout for repo "owner/name" when no
// clone exists yet: the main clone <clone_root>/<name> and its per-PR
// worktree. CreatePRWorktree finds an existing clone first (FindClone).
func PRWorktreePaths(watch config.Watch, repo string, number int) (mainClone, path string) {
	_, name, _ := strings.Cut(repo, "/")
	mainClone = filepath.Join(CloneRoot(watch), name)
	return mainClone, PRWorktreePath(mainClone, number)
}

// MainClonePath is where repo "owner/name" lives under the watch's
// clone_root: the clone gitx FindClone discovers (<name>, <owner>-<name>,
// <owner>_<name>, else any directory whose origin is the repository) or,
// when there is none (gitx.ErrNoClone), the directory a clone goes into:
// <root>/<name>, or <root>/<owner>-<name> when a folder named <name> exists
// but is not a git repository. found reports an existing clone.
func (m *Manager) MainClonePath(ctx context.Context, watch config.Watch, repo string) (mainClone string, found bool, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", false, fmt.Errorf("slots: invalid repository %q", repo)
	}
	root := CloneRoot(watch)
	dir, err := m.git.FindClone(ctx, root, owner, name)
	if err == nil {
		return dir, true, nil
	}
	if !errors.Is(err, gitx.ErrNoClone) {
		return "", false, fmt.Errorf("slots: find the clone of %s: %w", repo, err)
	}
	dest := filepath.Join(root, name)
	if fsx.Exists(dest) && !gitx.IsRepo(dest) {
		if alt := filepath.Join(root, owner+"-"+name); !fsx.Exists(alt) {
			return alt, false, nil
		}
	}
	return dest, false, nil
}

// PRSlotName is the slot name of a per-PR worktree: "owner/name#N".
func PRSlotName(repo string, number int) string { return repo + "#" + strconv.Itoa(number) }

// perPRNumber is the PR number in a PRSlotName (0 when there is none).
func perPRNumber(name string) int {
	i := strings.LastIndexByte(name, '#')
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(name[i+1:])
	return n
}

// CreatePRWorktree checks pr out into its own detached worktree for a repo
// without a pool. The main clone is found with MainClonePath (an existing
// clone under any of the usual names, else the directory to clone into) and
// the worktree goes next to it (<clone>__worktrees/pr-N). The slot row (kind
// per_pr, state provisioning) is written first; then steps of subject
// "slot:owner/name#N": clone (only when no clone was found, see ensureClone;
// an existing directory's origin must be owner/name, else
// ErrOriginMismatch), fetch (refs/pull/N/head →
// refs/magnum/pr/N; a head different from targetSHA is used and reported as
// slot.head_moved), worktree_add (--detach at refs/magnum/pr/N, skipped when
// listed at the fetched head; a listed worktree at another commit, left by
// an earlier attempt, is switched to it after Guard), verify (HEAD ==
// fetched head), render_mise (the main clone's
// .mise.local.toml rendered with the per-PR env, [[repo]] copy_files),
// setup (the [[repo]] setup commands or the main clone's .config/wt.toml
// [post-create]/[post-start] hooks as WT_BRANCH=magnum-pr-<N>, see
// hooks.go; a failure moves the slot to broken and returns ErrBroken) and
// claim (slot → claimed with pr_id, checked_out_sha and db_slug when setup
// ran, then its open assignment). The PR's state is not changed: move it to
// claiming before calling. Calling it again for a slot that already holds pr
// returns that slot (at its recorded paths), opening its assignment if a
// crash between the claim's two writes left none; a removed, broken or lost
// row for the same PR is reused (its old assignment closed) and its steps
// start over.
func (m *Manager) CreatePRWorktree(ctx context.Context, watch config.Watch, repo string, pr store.PR, targetSHA string) (store.Slot, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return store.Slot{}, fmt.Errorf("slots: invalid repository %q", repo)
	}
	slotName := PRSlotName(repo, pr.Number)
	if m.d.DryRun { // no git at all: the default layout stands in for discovery
		mainClone, path := PRWorktreePaths(watch, repo, pr.Number)
		m.dryRun("create worktree %s (or next to an existing clone) for %s#%d at %s", path, repo, pr.Number, textx.ShortSHA(targetSHA))
		return store.Slot{Name: slotName, RepoFullName: repo, Kind: store.SlotKindPerPR, Path: path,
			MainClone: mainClone, State: store.SlotClaimed}, nil
	}
	mainClone, _, err := m.MainClonePath(ctx, watch, repo)
	if err != nil {
		return store.Slot{}, err
	}
	path := PRWorktreePath(mainClone, pr.Number)
	sl, fresh, err := m.perPRRow(ctx, repo, pr, slotName, mainClone, path)
	if err != nil {
		return store.Slot{}, err
	}
	if sl.State != store.SlotProvisioning { // already holds pr
		if err := m.ensureAssignment(ctx, sl, pr); err != nil {
			return store.Slot{}, err
		}
		return sl, nil
	}
	subject := "slot:" + slotName
	if fresh {
		if err := steps.ResetSubject(ctx, m.d.Store, subject); err != nil {
			return store.Slot{}, err
		}
	}
	if err := m.perPRSteps(ctx, subject, sl, repo, pr, targetSHA); err != nil {
		if errors.Is(err, ErrBroken) && ctx.Err() == nil {
			if terr := m.d.Store.TransitionSlot(context.WithoutCancel(ctx), sl.ID, []string{store.SlotProvisioning}, store.SlotBroken,
				func(u *store.SlotUpdate) { u.Set("last_error", redactErr(err)) }); terr != nil {
				m.logErr(ctx, terr, "slots: mark %s broken: %v", sl.Name, terr)
			}
			return store.Slot{}, err
		}
		m.setLastError(ctx, sl.ID, err)
		return store.Slot{}, err
	}
	return m.reload(ctx, sl)
}

func (m *Manager) perPRRow(ctx context.Context, repo string, pr store.PR, slotName, mainClone, path string) (store.Slot, bool, error) {
	repoID := pr.RepoID
	sl, err := m.d.Store.CreateSlot(ctx, store.Slot{
		Name: slotName, RepoID: &repoID, RepoFullName: repo, Kind: store.SlotKindPerPR, Path: path,
		MainClone: mainClone, State: store.SlotProvisioning,
	})
	if err == nil {
		return sl, true, nil
	}
	if !errors.Is(err, store.ErrConflict) {
		return store.Slot{}, false, fmt.Errorf("slots: worktree %s: %w", slotName, err)
	}
	sl, err = m.d.Store.SlotByName(ctx, slotName)
	if errors.Is(err, store.ErrNotFound) {
		return store.Slot{}, false, fmt.Errorf("slots: worktree %s: path %s belongs to another slot: %w", slotName, path, store.ErrConflict)
	}
	if err != nil {
		return store.Slot{}, false, fmt.Errorf("slots: worktree %s: %w", slotName, err)
	}
	switch {
	case sl.State == store.SlotProvisioning && sl.MainClone != mainClone && !fsx.Exists(sl.Path) &&
		(!fsx.Exists(sl.MainClone) || !gitx.IsRepo(sl.MainClone)):
		// An interrupted row that pointed at a folder that is not the clone
		// (before clone discovery): nothing was created yet, so it moves to
		// the clone found now and its steps start over.
		err := m.d.Store.TransitionSlot(ctx, sl.ID, []string{store.SlotProvisioning}, store.SlotProvisioning, func(u *store.SlotUpdate) {
			u.Set("path", path)
			u.Set("main_clone", mainClone)
			u.Set("last_error", nil)
		})
		if err != nil {
			return store.Slot{}, false, fmt.Errorf("slots: worktree %s: %w", slotName, err)
		}
		sl, err = m.reload(ctx, sl)
		return sl, true, err
	case sl.State == store.SlotProvisioning:
		return sl, false, nil
	case slices.Contains([]string{store.SlotRemoved, store.SlotBroken, store.SlotLost}, sl.State):
		err := m.reviveSlot(ctx, sl, "slot:"+slotName, store.SlotProvisioning, "reprovisioned", func(u *store.SlotUpdate) {
			u.Set("path", path)
			u.Set("main_clone", mainClone)
		})
		if err != nil {
			return store.Slot{}, false, fmt.Errorf("slots: worktree %s: %w", slotName, err)
		}
		sl, err = m.reload(ctx, sl)
		return sl, false, err
	case slices.Contains([]string{store.SlotClaimed, store.SlotBusy, store.SlotHeld}, sl.State) && sl.PRID != nil && *sl.PRID == pr.ID:
		return sl, false, nil
	default:
		return store.Slot{}, false, fmt.Errorf("slots: worktree %s is %s: %w", slotName, sl.State, store.ErrConflict)
	}
}

func (m *Manager) perPRSteps(ctx context.Context, subject string, sl store.Slot, repo string, pr store.PR, target string) error {
	if err := m.step(ctx, subject, "clone", func(ctx context.Context) error {
		return m.ensureClone(ctx, repo, sl.MainClone)
	}); err != nil {
		return err
	}
	sha := ""
	if err := m.step(ctx, subject, "fetch", func(ctx context.Context) error {
		var err error
		if sha, err = m.git.FetchPR(ctx, sl.MainClone, pr.Number); err != nil {
			return err
		}
		if sha != target {
			m.event(ctx, subject, "warn", "slot.head_moved",
				fmt.Sprintf("PR #%d head moved: wanted %s, fetched %s; checking out %s", pr.Number, textx.ShortSHA(target), textx.ShortSHA(sha), textx.ShortSHA(sha)))
		}
		return nil
	}); err != nil {
		return err
	}
	if sha == "" { // fetch completed before a crash
		var err error
		if sha, err = m.git.RevParse(ctx, sl.MainClone, gitx.PRRef(pr.Number)); err != nil {
			return err
		}
	}
	if err := m.step(ctx, subject, "worktree_add", func(ctx context.Context) error {
		w, ok, err := m.listedWorktree(ctx, sl)
		switch {
		case err != nil:
			return err
		case !ok:
			return m.git.WorktreeAdd(ctx, sl.MainClone, sl.Path, gitx.PRRef(pr.Number), true, "")
		case w.Head == sha:
			return nil
		}
		// Left by an earlier attempt (setup failed, then the PR moved on):
		// bring it to the fetched head unless a human has been at work there.
		if err := m.Guard(ctx, sl); err != nil {
			return err
		}
		m.discarding(ctx, sl)
		return m.git.SwitchDetach(ctx, sl.Path, gitx.PRRef(pr.Number))
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "verify", func(ctx context.Context) error {
		head, err := m.git.RevParse(ctx, sl.Path, "HEAD")
		if err != nil {
			return err
		}
		if head != sha {
			return fmt.Errorf("%w: %s HEAD is %s, want %s", ErrVerify, sl.Path, textx.ShortSHA(head), textx.ShortSHA(sha))
		}
		return nil
	}); err != nil {
		return err
	}
	plan, planErr := m.planPerPR(sl, pr.Number, store.Deref(pr.BaseRef))
	if err := m.step(ctx, subject, "render_mise", func(ctx context.Context) error {
		return m.preparePerPR(ctx, sl, plan)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "setup", func(ctx context.Context) error {
		if planErr != nil {
			return planErr
		}
		return m.runHooks(ctx, sl, plan, plan.setup, SetupTimeout, false)
	}); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w: %s setup: %w", ErrBroken, sl.Name, err)
	}
	return m.step(ctx, subject, "claim", func(ctx context.Context) error {
		var dbSlug any
		if s := perPRDBSlug(sl, plan); s != "" {
			dbSlug = s
		}
		err := m.d.Store.TransitionSlot(ctx, sl.ID, []string{store.SlotProvisioning}, store.SlotClaimed, func(u *store.SlotUpdate) {
			u.Set("pr_id", pr.ID)
			u.Set("checked_out_sha", sha)
			u.Set("db_slug", dbSlug)
			u.Set("last_used_at", m.now())
			u.Set("last_error", nil)
		})
		if err != nil {
			return fmt.Errorf("slots: claim %s: %w", sl.Name, err)
		}
		cur, err := m.reload(ctx, sl)
		if err != nil {
			return err
		}
		return m.ensureAssignment(ctx, cur, pr)
	})
}

// ensureAssignment opens the open assignment of per-PR slot sl, which holds
// pr (path, db_slug and checked_out_sha from the row), unless it has one.
// The claim makes two writes (slot, then assignment); this makes the second
// one resumable.
func (m *Manager) ensureAssignment(ctx context.Context, sl store.Slot, pr store.PR) error {
	a, err := m.d.Store.OpenAssignmentBySlot(ctx, sl.ID)
	switch {
	case err == nil && a.PRID == pr.ID:
		return nil
	case err == nil:
		return fmt.Errorf("slots: %s has an open assignment for PR id %d: %w", sl.Name, a.PRID, store.ErrConflict)
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	_, err = m.d.Store.OpenAssignment(ctx, store.Assignment{PRID: pr.ID, SlotID: sl.ID, Path: sl.Path,
		DBSlug: sl.DBSlug, HeadSHA: sl.CheckedOutSHA})
	if err != nil {
		return fmt.Errorf("slots: open the assignment of %s: %w", sl.Name, err)
	}
	return nil
}

// listedWorktree returns git's entry for the slot's worktree. A prunable
// entry (its directory is gone) is pruned and reported as not listed.
func (m *Manager) listedWorktree(ctx context.Context, sl store.Slot) (gitx.Worktree, bool, error) {
	list, err := m.git.WorktreeList(ctx, sl.MainClone)
	if err != nil {
		return gitx.Worktree{}, false, err
	}
	w, ok := gitx.FindWorktree(list, sl.Path)
	if !ok || !w.Prunable {
		return w, ok, nil
	}
	return gitx.Worktree{}, false, m.git.WorktreePrune(ctx, sl.MainClone)
}

// ensureClone clones repo into mainClone when missing, else checks its origin.
// A new clone uses https://github.com/<owner>/<name>.git with gh as its only
// credential helper (for the clone and, written into its config, for every
// later fetch), so it needs no SSH key; it uses SSH only when another clone
// of the same owner next to it already does (that user's keys work).
func (m *Manager) ensureClone(ctx context.Context, repo, mainClone string) error {
	_, err := os.Stat(mainClone)
	if errors.Is(err, fs.ErrNotExist) {
		owner, name, _ := strings.Cut(repo, "/")
		ssh, err := m.git.OwnerUsesSSH(ctx, filepath.Dir(mainClone), owner)
		if err != nil {
			return fmt.Errorf("slots: clone %s: %w", repo, err)
		}
		if ssh {
			return m.git.Clone(ctx, gitx.GitHubSSHURL(owner, name), mainClone)
		}
		return m.git.CloneWith(ctx, gitx.GitHubHTTPSURL(owner, name), mainClone,
			gitx.CloneOptions{CredentialHelper: gitx.GHCredentialHelper})
	}
	if err != nil {
		return fmt.Errorf("slots: %s: %w", mainClone, err)
	}
	if !gitx.IsRepo(mainClone) {
		return fmt.Errorf("%w: %s exists but is not a git clone (move it away or clone %s next to it as %s)",
			ErrOriginMismatch, mainClone, repo, strings.ReplaceAll(repo, "/", "-"))
	}
	url, err := m.git.RemoteURL(ctx, mainClone)
	if err != nil {
		return err
	}
	if owner, name, _ := strings.Cut(repo, "/"); !gitx.OriginMatches(url, owner, name) {
		return fmt.Errorf("%w: %s origin is %q, want github.com/%s", ErrOriginMismatch, mainClone, url, repo)
	}
	return nil
}

// RemovePRWorktree removes a per-PR worktree: steps of subject
// "slot:owner/name#N:remove" are guard (skipped with force: Guard, untracked
// files counted as changes), teardown (the [[repo]] teardown commands or the
// main clone's .config/wt.toml [pre-remove] hooks as
// WT_BRANCH=magnum-pr-<N>; skipped when the directory is gone, failures
// logged and tolerated), worktree_remove (--force with force or over
// magnum's residue, see removeWorktree),
// delete_ref (refs/magnum/pr/N), worktree_prune and mark_removed (pr_id
// cleared, assignment closed). RemovePRStates can be removed (a busy slot is
// ErrConflict); a removing one resumes after the guard ran again; a removed
// one is a no-op.
func (m *Manager) RemovePRWorktree(ctx context.Context, slot store.Slot, force bool) error {
	if m.d.DryRun {
		m.dryRun("remove worktree %s (force=%v)", slot.Path, force)
		return nil
	}
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	if sl.Kind != store.SlotKindPerPR {
		return fmt.Errorf("slots: %s is not a per-PR worktree: %w", sl.Name, store.ErrConflict)
	}
	return m.removePR(ctx, sl, force, "removed")
}

func (m *Manager) removePR(ctx context.Context, sl store.Slot, force bool, reason string) error {
	if sl.State == store.SlotRemoved {
		return nil
	}
	subject := "slot:" + sl.Name + ":remove"
	if err := m.beginRemove(ctx, sl, subject, RemovePRStates, force); err != nil {
		return err
	}
	pr, _, err := m.slotPR(ctx, sl)
	if err != nil {
		return err
	}
	number := pr.Number
	if number == 0 {
		number = perPRNumber(sl.Name)
	}
	err = m.removePRSteps(ctx, subject, sl, force, number, store.Deref(pr.BaseRef), reason)
	if err != nil {
		m.setLastError(ctx, sl.ID, err)
	}
	return err
}

func (m *Manager) removePRSteps(ctx context.Context, subject string, sl store.Slot, force bool, number int, base, reason string) error {
	if err := m.step(ctx, subject, "teardown", func(ctx context.Context) error {
		return m.teardownPerPR(ctx, sl, number, base)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "worktree_remove", func(ctx context.Context) error {
		return m.removeWorktree(ctx, sl, force)
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "delete_ref", func(ctx context.Context) error {
		if number <= 0 || !fsx.Exists(sl.MainClone) {
			return nil
		}
		return m.git.UpdateRefDelete(ctx, sl.MainClone, gitx.PRRef(number))
	}); err != nil {
		return err
	}
	if err := m.step(ctx, subject, "worktree_prune", func(ctx context.Context) error {
		if !fsx.Exists(sl.MainClone) {
			return nil
		}
		// Plain prune: see removeSteps for the unmounted-volume caveat.
		return m.git.WorktreePrune(ctx, sl.MainClone)
	}); err != nil {
		return err
	}
	return m.step(ctx, subject, "mark_removed", func(ctx context.Context) error {
		return m.markRemoved(ctx, sl, reason)
	})
}
