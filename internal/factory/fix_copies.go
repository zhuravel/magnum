package factory

// The checkouts the fixes of your PRs work in (DECISIONS "The factory's fix
// copies and per-PR checkouts"). A PR's fix work, its pr_work row, claims a
// checkout at the PR's remote head, apart from the tasks' copies. A
// repository whose [[factory.repo]] sets fix_copies has that many fix
// copies, numbered past its max_copies (isFixCopy) and set up, claimed,
// repaired and released as the tasks' copies are, with the same guards: a
// task never takes one, a fix never takes a task's, and max_active never
// counts them. Any other watched repository gets a per-PR checkout: a
// detached worktree of its main clone at the PR's head, in the factory's
// checkouts folder of magnum's data directory (fixCheckoutPath), with no
// setup, no databases and no services, removed once the PR's fix work
// ends. The checkout's reservation is the fix work's
// owner row in the agents store (fixOwner), so a restart finds the claim
// and its checkout; every step that changes a checkout is a step of the
// agents store (begin, ok or fail rows), which a cut claim resumes.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// The owner rows of the PRs' fix work in the agents store: a repository of
// their own, factory-fix/fix (no task's or liaison's), each row numbered by
// its pr_work id, its node id factory:fix:<id>. A slot release leaves the
// review's refs/magnum/pr/N alone for a row of another repository than the
// slot's, as it does for a task's row.
const (
	fixOwnerRepo       = "factory-fix"
	fixOwnerName       = "fix"
	fixOwnerNodePrefix = "factory:fix:"
	fixOwnerIdentity   = "factory"
)

// fixRefPrefix holds the PR heads a fix claim fetches (fixRef), apart from
// the review's refs/magnum/pr/N in a main clone the two daemons share: a
// fix's fetch never moves the review's ref, and its release never deletes
// it.
const fixRefPrefix = "refs/magnum/factory-fix/"

// fixRef is where a fix claim fetches PR number's head in its main clone.
func fixRef(number int) string { return fixRefPrefix + strconv.Itoa(number) }

func fixOwnerNode(id int64) string { return fixOwnerNodePrefix + strconv.FormatInt(id, 10) }

// fixOwnerWork is the pr_work row an owner row's node id names (fixOwner),
// false for a row that is no fix work's.
func fixOwnerWork(nodeID string) (int64, bool) {
	rest, ok := strings.CutPrefix(nodeID, fixOwnerNodePrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}

// fixOwnerPR is the PR an owner row stands for, owner/name and its number,
// read from the URL fixOwner gives it (https://github.com/<owner>/<name>/
// pull/<N>); "" and 0 for a URL of another form.
func fixOwnerPR(owner store.PR) (string, int) {
	rest, ok := strings.CutPrefix(owner.URL, "https://github.com/")
	repo, n, cut := strings.Cut(rest, "/pull/")
	number, err := strconv.Atoi(n)
	if !ok || !cut || err != nil || number < 1 || strings.Count(repo, "/") != 1 {
		return "", 0
	}
	return repo, number
}

// workKey names PR work w as the events do: owner/name#N.
func workKey(w registry.PRWork) string { return w.Repo + "#" + strconv.Itoa(w.Number) }

// FixCheckout is the checkout a PR's fix works in: a fix copy of its
// repository (Number, the copy's, with the copy's Port and Env) or its
// per-PR checkout (Number 0, no port, no env), at Head, the PR's remote
// head the claim fetched.
type FixCheckout struct {
	Name, Path string
	Number     int
	Head       string
	Port       int
	Env        map[string]string
}

// PerPR reports whether the checkout is a per-PR checkout, not a fix copy.
func (f FixCheckout) PerPR() bool { return f.Number == 0 }

// FixReservation is a checkout the fix work of a PR (its pr_work row)
// holds, from its claim until its release: the checkout's name, whether it
// is a per-PR checkout, and the PR as the checkout's owner row names it
// (Repo, owner/name, and Number), which a release of work whose row is
// gone reads.
type FixReservation struct {
	WorkID   int64
	Checkout string
	PerPR    bool
	Repo     string
	Number   int
}

// fixOwner upserts the owner row of PR work w in the agents store: number
// w's id, URL the PR's page, head the PR's remote head the claim fetched.
func (c *copySet) fixOwner(ctx context.Context, w registry.PRWork, head string) (store.PR, error) {
	repo, err := c.st.UpsertRepo(ctx, store.Repo{NodeID: fixOwnerRepo, Owner: fixOwnerRepo, Name: fixOwnerName,
		WatchOwner: fixOwnerRepo, Mode: store.RepoModePerPR})
	if err != nil {
		return store.PR{}, fmt.Errorf("the owner row of %s: %w", workKey(w), err)
	}
	title := workKey(w)
	up, err := c.st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: fixOwnerNode(w.ID), Number: int(w.ID),
		URL: "https://github.com/" + w.Repo + "/pull/" + strconv.Itoa(w.Number), HeadSHA: head, Title: &title,
		GHState: store.GHOpen, InitialState: store.PRClaiming, Identity: fixOwnerIdentity})
	if err != nil {
		return store.PR{}, fmt.Errorf("the owner row of %s: %w", workKey(w), err)
	}
	return up.PR, nil
}

// fixOwnerOf is PR work id's owner row as it is (store.ErrNotFound before
// its first claim).
func (c *copySet) fixOwnerOf(ctx context.Context, id int64) (store.PR, error) {
	repo, err := c.st.RepoByFullName(ctx, fixOwnerRepo+"/"+fixOwnerName)
	if err != nil {
		return store.PR{}, err
	}
	pr, err := c.st.PRByRepoNumber(ctx, repo.ID, int(id))
	if err != nil {
		return store.PR{}, err
	}
	if pr.NodeID != fixOwnerNode(id) {
		return store.PR{}, fmt.Errorf("the owner row of pr work %d: %w", id, store.ErrNotFound)
	}
	return pr, nil
}

// fixPlace is where a PR's fix works: its repository's fix copies (copies:
// a [[factory.repo]] that sets fix_copies), else a per-PR checkout of the
// repository's main clone, clone (the [[factory.repo]]'s, else the
// [[pool]]'s, else the clone magnum's review found under its [[watch]]'s
// clone_root). The factory never clones a repository.
type fixPlace struct {
	repo   config.FactoryRepo
	copies bool
	clone  string
}

// fixPlaceOf is where PR work w's fix works (fixPlace). A watched
// repository with no clone yet is a wait: magnum's review clones it.
func (c *copySet) fixPlaceOf(ctx context.Context, w registry.PRWork) (fixPlace, error) {
	if repo, ok := c.cfg.FactoryRepo(w.Repo); ok {
		return fixPlace{repo: repo, copies: repo.FixCopies > 0, clone: repo.MainClone}, nil
	}
	if p := c.cfg.PoolFor(w.Repo); p != nil {
		return fixPlace{clone: p.MainClone}, nil
	}
	watch := c.cfg.WatchFor(w.Repo)
	if watch == nil {
		return fixPlace{}, fmt.Errorf("no [[watch]] covers %s", w.Repo)
	}
	clone, found, err := c.slots.MainClonePath(ctx, *watch, w.Repo)
	if err != nil {
		return fixPlace{}, err
	}
	if !found {
		return fixPlace{}, noCopy{what: fmt.Sprintf("no clone of %s under %s yet (magnum's review clones it)", w.Repo, slots.CloneRoot(*watch))}
	}
	return fixPlace{clone: clone}, nil
}

// ClaimFix hands PR work w a checkout at the PR's remote head: it fetches
// the head into the main clone (fixRef: a step of subject fix:<repo>#<N>,
// a new generation each claim), then takes the checkout the PR holds, else
// a fix copy (reserve: the PR's last copy first, else a free one, else one
// added up to fix_copies; none left is ErrNoCopy, "copy: all 2 busy", a
// wait that costs the PR nothing) or its per-PR checkout (perPRCheckout),
// and runs the fix claim's protocol (prepareFix) under the checkout's own
// lock. A checkout of the other kind the PR still holds (the configuration
// changed) is released first, before the fetch: its release deletes the
// fix ref, which the per-PR checkout's worktree add then reads.
func (c *copySet) ClaimFix(ctx context.Context, w registry.PRWork) (FixCheckout, error) {
	place, err := c.fixPlaceOf(ctx, w)
	if _, wait := errors.AsType[noCopy](err); wait {
		return FixCheckout{}, err
	}
	if err != nil {
		return FixCheckout{}, fmt.Errorf("claim a fix checkout for %s: %w", workKey(w), err)
	}
	if err := c.dropOtherCheckout(ctx, w, place.copies); err != nil {
		return FixCheckout{}, fmt.Errorf("claim a fix checkout for %s: %w", workKey(w), err)
	}
	head, err := c.fetchFix(ctx, w, place.clone)
	if err != nil {
		return FixCheckout{}, fmt.Errorf("claim a fix checkout for %s: %w", workKey(w), err)
	}
	owner, err := c.fixOwner(ctx, w, head)
	if err != nil {
		return FixCheckout{}, fmt.Errorf("claim a fix checkout for %s: %w", workKey(w), err)
	}
	if place.copies {
		return c.claimFixCopy(ctx, w, owner, place.repo, head)
	}
	return c.claimPerPR(ctx, w, owner, place.clone, head)
}

// fetchFix fetches PR work w's head into its fix ref in clone and returns
// it: a step of subject fix:<repo>#<N>, a new generation each claim.
func (c *copySet) fetchFix(ctx context.Context, w registry.PRWork, clone string) (string, error) {
	subject := "fix:" + workKey(w)
	if err := steps.ResetSubject(ctx, c.st, subject); err != nil {
		return "", err
	}
	head := ""
	err := steps.Step(ctx, c.st, subject, "fetch", func(ctx context.Context) error {
		var err error
		head, err = c.git.FetchPRInto(ctx, clone, w.Number, fixRef(w.Number))
		return err
	})
	return head, err
}

// dropOtherCheckout releases the checkout PR work w's owner row holds when
// it is of the other kind than the claim's (copies: a per-PR checkout, else
// a fix copy): the repository's fix_copies changed since the PR claimed it.
// Work with no owner row yet holds none.
func (c *copySet) dropOtherCheckout(ctx context.Context, w registry.PRWork, copies bool) error {
	owner, err := c.fixOwnerOf(ctx, w.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	sl, err := c.st.SlotByPR(ctx, owner.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	case (sl.Kind == store.SlotKindPerPR) != copies:
		return nil
	}
	unlock := c.lockCopy(sl.Name)
	defer unlock()
	return c.releaseFixSlot(ctx, w, owner, sl, "its repository's fix_copies changed")
}

// claimFixCopy is ClaimFix in a fix copy of repo.
func (c *copySet) claimFixCopy(ctx context.Context, w registry.PRWork, owner store.PR, repo config.FactoryRepo, head string) (FixCheckout, error) {
	sl, err := c.reserve(ctx, repo, owner, true)
	if _, wait := errors.AsType[noCopy](err); wait {
		return FixCheckout{}, err
	}
	if err != nil {
		return FixCheckout{}, fmt.Errorf("claim a fix copy for %s: %w", workKey(w), err)
	}
	n, ok := copyNumber(repo, sl.Name)
	if !ok {
		return FixCheckout{}, fmt.Errorf("claim a fix copy for %s: %s is no copy of %s (copy_name %q)", workKey(w), sl.Name, repo.Repo, repo.CopyName)
	}
	cp := copyOf(repo, sl, n)
	unlock := c.lockCopy(cp.Name)
	defer unlock()
	data := func(step claimStep) error { return c.readyData(ctx, repo, sl, cp, step) }
	if err := c.prepareFix(ctx, w, owner, sl, head, data); err != nil {
		return FixCheckout{}, fmt.Errorf("claim %s for %s: %w", cp.Name, workKey(w), err)
	}
	return FixCheckout{Name: cp.Name, Path: cp.Path, Number: n, Head: head, Port: cp.Port, Env: cp.Env}, nil
}

// claimPerPR is ClaimFix in PR work w's per-PR checkout of clone.
func (c *copySet) claimPerPR(ctx context.Context, w registry.PRWork, owner store.PR, clone, head string) (FixCheckout, error) {
	name := fixCheckoutName(w)
	unlock := c.lockCopy(name)
	defer unlock()
	sl, err := c.perPRCheckout(ctx, w, owner, clone)
	if err != nil {
		return FixCheckout{}, fmt.Errorf("claim %s for %s: %w", name, workKey(w), err)
	}
	if err := c.prepareFix(ctx, w, owner, sl, head, nil); err != nil {
		return FixCheckout{}, fmt.Errorf("claim %s for %s: %w", name, workKey(w), err)
	}
	return FixCheckout{Name: sl.Name, Path: sl.Path, Head: head}, nil
}

// fixCheckoutName is the slot name of PR work w's per-PR checkout:
// fix-<owner>/<name>#<N>.
func fixCheckoutName(w registry.PRWork) string { return "fix-" + workKey(w) }

// fixCheckoutPath is where PR work w's per-PR checkout goes: the factory's
// own folder of magnum's data directory, factory/checkouts/<owner>/<name>/
// pr-<N> (paths.Layout.FactoryCheckout), never the copies' folder of
// another repository. It holds no database, so the internal disk does.
func (c *copySet) fixCheckoutPath(w registry.PRWork) string {
	return c.cfg.Layout.FactoryCheckout(w.Repo, w.Number)
}

// perPRCheckout is PR work w's per-PR checkout of clone, reserved for owner
// (its row's pr_id from its creation on, so a removal finds even one a
// crash left half made): its row created, or revived after a removal, and
// its worktree added when it has none, as steps of subject slot:<name>:
// worktree_add (detached at the fix ref; a worktree an earlier try added
// stays) and claim (claimed, its HEAD recorded as its checkout, its
// assignment opened). A removal a crash cut short ends first. A directory
// at its path that no row of the factory's names is never taken. The
// caller holds the checkout's lock.
func (c *copySet) perPRCheckout(ctx context.Context, w registry.PRWork, owner store.PR, clone string) (store.Slot, error) {
	name, path := fixCheckoutName(w), c.fixCheckoutPath(w)
	sl, err := c.st.SlotByName(ctx, name)
	fresh := false
	switch {
	case errors.Is(err, store.ErrNotFound):
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return store.Slot{}, fmt.Errorf("%s already exists, and no fix checkout of the factory's is there: move it away", path)
		}
		id := owner.ID
		if sl, err = c.st.CreateSlot(ctx, store.Slot{Name: name, RepoFullName: w.Repo, Kind: store.SlotKindPerPR, Path: path,
			MainClone: clone, State: store.SlotProvisioning, PRID: &id}); err != nil {
			return store.Slot{}, err
		}
		fresh = true
	case err != nil:
		return store.Slot{}, err
	}
	if sl.State == store.SlotRemoving {
		if err := c.removePerPR(ctx, w, owner, sl, "its fix work came back"); err != nil {
			return store.Slot{}, err
		}
		if sl, err = c.st.SlotByID(ctx, sl.ID); err != nil {
			return store.Slot{}, err
		}
	}
	switch {
	case sl.State == store.SlotRemoved:
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return store.Slot{}, fmt.Errorf("%s already exists, and no fix checkout of the factory's is there: move it away", path)
		}
		if err := c.st.TransitionSlot(ctx, sl.ID, []string{store.SlotRemoved}, store.SlotProvisioning, func(u *store.SlotUpdate) {
			u.Set("pr_id", owner.ID)
			u.Set("path", path)
			u.Set("main_clone", clone)
			u.Set("last_error", nil)
		}); err != nil {
			return store.Slot{}, err
		}
		if sl, err = c.st.SlotByID(ctx, sl.ID); err != nil {
			return store.Slot{}, err
		}
		fresh = true
	case sl.PRID == nil || *sl.PRID != owner.ID:
		return store.Slot{}, fmt.Errorf("%s is %s and not this PR's: %w", name, sl.State, store.ErrConflict)
	case sl.State != store.SlotProvisioning:
		return sl, nil // the PR's, made before
	}
	return c.makePerPR(ctx, w, owner, sl, fresh)
}

// makePerPR runs the steps of per-PR checkout sl's making (perPRCheckout);
// fresh starts them over.
func (c *copySet) makePerPR(ctx context.Context, w registry.PRWork, owner store.PR, sl store.Slot, fresh bool) (store.Slot, error) {
	subject := "slot:" + sl.Name
	if fresh {
		if err := steps.ResetSubject(ctx, c.st, subject); err != nil {
			return store.Slot{}, err
		}
	}
	if err := steps.Step(ctx, c.st, subject, "worktree_add", func(ctx context.Context) error {
		list, err := c.git.WorktreeList(ctx, sl.MainClone)
		if err != nil {
			return err
		}
		if wt, ok := gitx.FindWorktree(list, sl.Path); ok {
			if !wt.Prunable {
				return nil // an earlier try added it
			}
			if err := c.git.WorktreePrune(ctx, sl.MainClone); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(sl.Path), 0o750); err != nil {
			return err
		}
		return c.git.WorktreeAdd(ctx, sl.MainClone, sl.Path, fixRef(w.Number), true, "")
	}); err != nil {
		return store.Slot{}, err
	}
	if err := steps.Step(ctx, c.st, subject, "claim", func(ctx context.Context) error {
		return c.claimMade(ctx, owner, sl)
	}); err != nil {
		return store.Slot{}, err
	}
	return c.st.SlotByID(ctx, sl.ID)
}

// claimMade claims per-PR checkout sl, made for owner: its HEAD recorded
// as its checkout, then its assignment opened, each once (a crash between
// the two runs the step again).
func (c *copySet) claimMade(ctx context.Context, owner store.PR, sl store.Slot) error {
	cur, err := c.st.SlotByID(ctx, sl.ID)
	if err != nil {
		return err
	}
	head, err := c.git.RevParse(ctx, sl.Path, "HEAD")
	if err != nil {
		return err
	}
	if cur.State == store.SlotProvisioning {
		if err := c.st.TransitionSlot(ctx, sl.ID, []string{store.SlotProvisioning}, store.SlotClaimed, func(u *store.SlotUpdate) {
			u.Set("checked_out_sha", head)
			u.Set("last_used_at", c.now())
			u.Set("last_error", nil)
		}); err != nil {
			return err
		}
	}
	switch a, err := c.st.OpenAssignmentBySlot(ctx, sl.ID); {
	case err == nil && a.PRID == owner.ID:
		return nil
	case err == nil:
		return fmt.Errorf("%s has an open assignment of another row: %w", sl.Name, store.ErrConflict)
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	_, err = c.st.OpenAssignment(ctx, store.Assignment{PRID: owner.ID, SlotID: sl.ID, Path: sl.Path, HeadSHA: &head})
	return err
}

// prepareFix is the fix claim's protocol in checkout sl, in order: the
// switch to head, the PR's remote head the claim fetched (slots.Resync:
// detached, an operation left in progress aborted, its checkout recorded;
// nothing to do when the checkout is there already), then data's steps (a
// fix copy's readyData: reset_db, installs, the Redis flush; none for a
// per-PR checkout, which has no databases). Each is a step of subject
// slot:<name>:fix:<assignment>:<head>, with begin and ok or fail rows, and
// runs the checkout's guard (fixGuard) first. A claim cut short runs again
// the steps not done for that head, each after its guard, and never one
// that is done: a later claim at the same head keeps what the fixer did in
// the checkout, and one at a newer head switches to it.
func (c *copySet) prepareFix(ctx context.Context, w registry.PRWork, owner store.PR, sl store.Slot, head string,
	data func(step claimStep) error) error {
	a, err := c.st.OpenAssignmentBySlot(ctx, sl.ID)
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("slot:%s:fix:%d:%s", sl.Name, a.ID, textx.ShortSHA(head))
	step := func(name string, fn func(ctx context.Context) error) error {
		return steps.Step(ctx, c.st, subject, name, func(ctx context.Context) error {
			if err := c.fixGuard(ctx, w, owner, sl, head); err != nil {
				return err
			}
			return fn(ctx)
		})
	}
	if err := step("switch", func(ctx context.Context) error { return c.switchFix(ctx, w, sl, a, head) }); err != nil {
		return err
	}
	if data == nil {
		return nil
	}
	return data(step)
}

// switchFix switches checkout sl to head (slots.Resync) and records it as
// the checkout of the claim's assignment a, with the time, which a person's
// typing in the PR's fix panes is read against (the guard). The fixer's
// commits on no remote that HEAD carries are kept first (keepUnpushed).
func (c *copySet) switchFix(ctx context.Context, w registry.PRWork, sl store.Slot, a store.Assignment, head string) error {
	if err := c.keepUnpushed(ctx, w, sl, head); err != nil {
		return err
	}
	if err := c.slots.Resync(ctx, sl, head); err != nil {
		return err
	}
	if err := c.st.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("last_used_at", c.now()) }); err != nil {
		return err
	}
	if store.Deref(a.HeadSHA) == head {
		return nil
	}
	return c.st.UpdateAssignmentHead(ctx, a.ID, head)
}

// fixKeptPrefix holds the fixer's commits a claim's switch would leave on
// no ref (keepUnpushed): fixKeptRef, apart from the fix refs
// (refs/magnum/factory-fix/<N>, which a release deletes) and the review's
// refs/magnum/pr/<N>. Nothing deletes them: they are yours to look at.
const fixKeptPrefix = "refs/magnum/factory-kept/"

// fixKeptRef is where a claim keeps commit sha of PR number's fixer.
func fixKeptRef(number int, sha string) string {
	return fixKeptPrefix + strconv.Itoa(number) + "/" + sha
}

// keepUnpushed keeps checkout sl's HEAD under fixKeptRef in its main clone,
// with a factory.fix_kept event that names the ref, when the claim of PR
// work w is about to switch it to head and it carries commits on no remote
// or magnum ref past the head the PR's last claim switched to
// (unpushedPastClaim): the switch leaves them on no ref, and the removal of
// a per-PR checkout deletes the reflog that still names them. A HEAD at
// head, or with no such commit (the PR's own old head after a force-push
// among them), keeps nothing. Once kept, the commits count as pushed, so a
// switch run again keeps nothing twice.
func (c *copySet) keepUnpushed(ctx context.Context, w registry.PRWork, sl store.Slot, head string) error {
	if !fsx.Exists(sl.Path) {
		return nil
	}
	got, err := c.git.RevParse(ctx, sl.Path, "HEAD")
	if err != nil || got == head {
		return err
	}
	n, err := c.unpushedPastClaim(ctx, sl)
	if err != nil || n == 0 {
		return err
	}
	ref := fixKeptRef(w.Number, got)
	if err := c.git.KeepCommit(ctx, sl.Path, ref, got); err != nil {
		return err
	}
	c.event(ctx, sl.Name, "info", "factory.fix_kept", fmt.Sprintf("HEAD %s of %s carries %d commit(s) of the fixer's on no remote: kept as %s before the switch to the PR's head %s",
		textx.ShortSHA(got), sl.Name, n, ref, textx.ShortSHA(head)))
	return nil
}

// fixGuard is what each step of PR work w's claim runs before it changes
// checkout sl: no factory pane works in it (paneInPath: the PR's own fix
// sessions too, as yet), HEAD the fixer moved is the factory's own
// (fixOwnHead, with head the PR's remote head), and magnum's slot guard
// lets it go (a person's agent or process in it, their changes, a HEAD
// that moved otherwise).
func (c *copySet) fixGuard(ctx context.Context, w registry.PRWork, owner store.PR, sl store.Slot, head string) error {
	if err := c.paneInPath(ctx, sl.Path, owner, nil, workKey(w)); err != nil {
		return err
	}
	if err := c.fixOwnHead(ctx, owner, sl, head, false); err != nil {
		return err
	}
	return c.slots.Guard(ctx, sl)
}

// fixOwnHead records a HEAD that moved off checkout sl's recorded checkout
// as the checkout's own (checked_out_sha), as ownHead does for a task's
// branch. A HEAD at head, the PR's remote head the claim or the release
// fetched just now, is all on GitHub: it is the checkout's own whatever
// you typed. Any other is the checkout's own only when nobody typed into
// the PR's fix panes since the checkout (owner's human_active_at) and it is
// the fixer's: at a claim a descendant of the checkout (the fixer's commits
// on it), at a release (release) any HEAD with no commit on no remote or
// magnum ref. Any other HEAD is left to the guard, which holds the checkout
// over it (head_drift). At a release a HEAD with a commit on no remote or
// magnum ref holds the checkout (unpushed_commits), wherever it is, so the
// release loses no commit of the fixer's.
func (c *copySet) fixOwnHead(ctx context.Context, owner store.PR, sl store.Slot, head string, release bool) error {
	cur, err := c.st.SlotByID(ctx, sl.ID)
	if err != nil || !fsx.Exists(cur.Path) {
		return err
	}
	if release {
		n, err := c.unpushedPastClaim(ctx, cur)
		if err != nil {
			return err
		}
		if n > 0 {
			return slots.ErrHold{Reason: slots.HoldUnpushed, Detail: fmt.Sprintf("HEAD of %s has %d commit(s) on no remote, which its release would lose", cur.Name, n)}
		}
	}
	want := store.Deref(cur.CheckedOutSHA)
	if want == "" {
		return nil
	}
	got, err := c.git.RevParse(ctx, cur.Path, "HEAD")
	if err != nil || got == want {
		return err
	}
	pr, err := c.st.PRByID(ctx, owner.ID)
	if err != nil {
		return err
	}
	pushed := head != "" && got == head
	if !pushed && pr.HumanActiveAt != nil && (cur.LastUsedAt == nil || pr.HumanActiveAt.After(*cur.LastUsedAt)) {
		return nil
	}
	own := release || pushed
	if !own {
		if own, err = c.git.IsAncestor(ctx, cur.Path, want, got); err != nil {
			return err
		}
	}
	if !own {
		return nil
	}
	if err := c.st.UpdateSlotFields(ctx, cur.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", got) }); err != nil {
		return err
	}
	c.event(ctx, cur.Name, "info", "factory.fix_head", fmt.Sprintf("HEAD %s, past the checkout %s, is the fixer's own: recorded as %s's checkout",
		textx.ShortSHA(got), textx.ShortSHA(want), cur.Name))
	return nil
}

// unpushedPastClaim counts the commits of checkout sl's HEAD on no remote
// or magnum ref and past the head its claim switched to (its open
// assignment's): that head was the PR's remote head, so a force-push of the
// PR, which moves the fix ref off it at the release's fetch, leaves it
// pushed.
func (c *copySet) unpushedPastClaim(ctx context.Context, sl store.Slot) (int, error) {
	var claimed []string
	switch a, err := c.st.OpenAssignmentBySlot(ctx, sl.ID); {
	case err == nil && store.Deref(a.HeadSHA) != "":
		claimed = append(claimed, *a.HeadSHA)
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return 0, err
	}
	return c.git.UnpushedPast(ctx, sl.Path, claimed...)
}

// ReleaseFix hands PR work w's checkout back: a fix copy goes back to its
// pool clean, on its placeholder at origin/<base>, as a task's copy does
// (releaseFixSlot); a per-PR checkout is removed (removePerPR). Either is
// refused while one of the factory's sessions works in it (ErrPaneInCopy)
// or its guard holds it, and resumes where a crash cut it. PR work that
// holds no checkout has nothing to release. It takes only the checkout's
// own lock.
func (c *copySet) ReleaseFix(ctx context.Context, w registry.PRWork, reason string) error {
	owner, err := c.fixOwnerOf(ctx, w.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release the fix checkout of %s: %w", workKey(w), err)
	}
	sl, err := c.st.SlotByPR(ctx, owner.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release the fix checkout of %s: %w", workKey(w), err)
	}
	unlock := c.lockCopy(sl.Name)
	defer unlock()
	return c.releaseFixSlot(ctx, w, owner, sl, reason)
}

// releaseFixSlot is ReleaseFix of checkout sl, which owner holds; the
// caller holds the checkout's lock.
func (c *copySet) releaseFixSlot(ctx context.Context, w registry.PRWork, owner store.PR, sl store.Slot, reason string) error {
	if sl.Kind == store.SlotKindPerPR {
		if err := c.removePerPR(ctx, w, owner, sl, reason); err != nil {
			return fmt.Errorf("remove %s of %s: %w", sl.Name, workKey(w), err)
		}
		return nil
	}
	repo, n, ok := c.repoOf(sl.Name)
	if !ok {
		return fmt.Errorf("release %s of %s: no [[factory.repo]] has that copy", sl.Name, workKey(w))
	}
	cp := copyOf(repo, sl, n)
	if err := c.paneInPath(ctx, cp.Path, store.PR{}, nil, workKey(w)); err != nil {
		return err
	}
	if _, err := c.stopForRelease(ctx, cp); err != nil {
		return fmt.Errorf("release %s of %s: %w", cp.Name, workKey(w), err)
	}
	if err := c.releaseHead(ctx, w, owner, sl, repo.MainClone); err != nil {
		return fmt.Errorf("release %s of %s: %w", cp.Name, workKey(w), err)
	}
	if err := c.slots.Release(ctx, sl, c.pool(repo, n), reason); err != nil {
		return fmt.Errorf("release %s of %s: %w", cp.Name, workKey(w), err)
	}
	return c.dropFixRef(ctx, w, repo.MainClone)
}

// releaseHead is a release's look at checkout sl's HEAD (fixOwnHead): it
// fetches PR work w's head into its fix ref in clone first, so the commits
// the fixer pushed since the claim count as pushed and only those on no
// remote hold the release, and a HEAD at that head is the checkout's own
// whatever you typed in the fix's panes.
func (c *copySet) releaseHead(ctx context.Context, w registry.PRWork, owner store.PR, sl store.Slot, clone string) error {
	head := ""
	if w.Number > 0 && fsx.Exists(clone) && fsx.Exists(sl.Path) {
		var err error
		if head, err = c.git.FetchPRInto(ctx, clone, w.Number, fixRef(w.Number)); err != nil {
			return err
		}
	}
	return c.fixOwnHead(ctx, owner, sl, head, true)
}

// dropFixRef deletes PR work w's fix ref in clone once its checkout is
// released: a step of subject fix:<repo>#<N>, once per claim.
func (c *copySet) dropFixRef(ctx context.Context, w registry.PRWork, clone string) error {
	if !fsx.Exists(clone) {
		return nil
	}
	return steps.Step(ctx, c.st, "fix:"+workKey(w), "delete_ref", func(ctx context.Context) error {
		return c.git.UpdateRefDelete(ctx, clone, fixRef(w.Number))
	})
}

// removePerPR removes per-PR checkout sl of PR work w, which owner holds:
// steps of subject slot:<name>:remove, guard (no factory pane in it, a
// HEAD with commits on no remote held and the fixer's other HEAD taken as
// the factory's own: releaseHead, magnum's slot guard), then the checkout
// moves to removing, worktree_remove (the
// tracked and untracked changes a person's typing in the PR's fix panes
// since the checkout explains are held, dirty_worktree; others are the
// fixer's residue, named in a slot.discarded event and removed), delete_ref
// (its fix ref), worktree_prune and mark_removed (its PR and assignment
// let go). A removing checkout resumes after its guard ran again; a
// removed one is done. No teardown and no database: it has none.
func (c *copySet) removePerPR(ctx context.Context, w registry.PRWork, owner store.PR, sl store.Slot, reason string) error {
	subject := "slot:" + sl.Name + ":remove"
	guard := func(ctx context.Context) error {
		if err := c.paneInPath(ctx, sl.Path, store.PR{}, nil, workKey(w)); err != nil {
			return err
		}
		if err := c.releaseHead(ctx, w, owner, sl, sl.MainClone); err != nil {
			return err
		}
		return c.slots.Guard(ctx, sl)
	}
	switch sl.State {
	case store.SlotRemoved:
		return nil
	case store.SlotRemoving:
		if err := guard(ctx); err != nil {
			return err
		}
	default:
		if err := steps.ResetSubject(ctx, c.st, subject); err != nil {
			return err
		}
		if err := steps.Step(ctx, c.st, subject, "guard", guard); err != nil {
			return err
		}
		if err := c.st.TransitionSlot(ctx, sl.ID, []string{sl.State}, store.SlotRemoving, nil); err != nil {
			return err
		}
	}
	for _, s := range []struct {
		name string
		fn   func(ctx context.Context) error
	}{
		{"worktree_remove", func(ctx context.Context) error { return c.removeWorktree(ctx, sl) }},
		{"delete_ref", func(ctx context.Context) error {
			if !fsx.Exists(sl.MainClone) {
				return nil
			}
			return c.git.UpdateRefDelete(ctx, sl.MainClone, fixRef(w.Number))
		}},
		{"worktree_prune", func(ctx context.Context) error {
			if !fsx.Exists(sl.MainClone) {
				return nil
			}
			return c.git.WorktreePrune(ctx, sl.MainClone)
		}},
		{"mark_removed", func(ctx context.Context) error {
			if err := c.st.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) {
				u.Set("checked_out_sha", nil)
				u.Set("last_error", nil)
			}); err != nil {
				return err
			}
			return c.st.ReleaseSlot(ctx, sl.ID, []string{store.SlotRemoving}, store.SlotRemoved, reason)
		}},
	} {
		if err := steps.Step(ctx, c.st, subject, s.name, s.fn); err != nil {
			return err
		}
	}
	return nil
}

// removeWorktree removes per-PR checkout sl's worktree (removePerPR): none
// listed is gone already, a listed one whose directory is gone is pruned.
// Changes in it that a person's typing explains (slots.HumanEvidence) hold
// it, as dirty_worktree; any others are named in a slot.discarded event
// and go with it.
func (c *copySet) removeWorktree(ctx context.Context, sl store.Slot) error {
	if !fsx.Exists(sl.MainClone) {
		return nil
	}
	list, err := c.git.WorktreeList(ctx, sl.MainClone)
	if err != nil {
		return err
	}
	wt, ok := gitx.FindWorktree(list, sl.Path)
	switch {
	case !ok:
		return nil
	case wt.Prunable:
		return c.git.WorktreePrune(ctx, sl.MainClone)
	}
	entries, err := c.git.StatusPaths(ctx, sl.Path)
	if err != nil {
		return err
	}
	var changed []string
	for _, e := range entries {
		if !e.Ignored() {
			changed = append(changed, e.Path)
		}
	}
	if len(changed) > 0 {
		why, err := c.slots.HumanEvidence(ctx, sl)
		if err != nil {
			return err
		}
		listed := strings.Join(changed[:min(len(changed), handoffListed)], ", ")
		if why != "" {
			return slots.ErrHold{Reason: slots.HoldDirtyWorktree, Detail: fmt.Sprintf("%s holds %d change(s) (%s), and %s", sl.Path, len(changed), listed, why)}
		}
		c.event(ctx, sl.Name, "info", "slot.discarded", fmt.Sprintf("%s: its removal discards %d change(s): %s", sl.Name, len(changed), listed))
	}
	return c.git.WorktreeRemove(ctx, sl.MainClone, sl.Path, len(changed) > 0)
}

// FixReserved lists the checkouts the PRs' fix work holds (FixReservation),
// from the agents store: a claim reserves one (one that got it, waits on a
// guard or failed, a per-PR checkout from its making on) and the release
// lets it go.
func (c *copySet) FixReserved(ctx context.Context) ([]FixReservation, error) {
	var out []FixReservation
	err := c.eachReserved(ctx, func(sl store.Slot, owner store.PR) {
		if id, ok := fixOwnerWork(owner.NodeID); ok {
			repo, number := fixOwnerPR(owner)
			out = append(out, FixReservation{WorkID: id, Checkout: sl.Name, PerPR: sl.Kind == store.SlotKindPerPR, Repo: repo, Number: number})
		}
	})
	if err != nil {
		return nil, fmt.Errorf("the fix checkouts reserved: %w", err)
	}
	return out, nil
}
