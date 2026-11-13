package factory

// The copies of a repository the factory's tasks work in (spec 7.1): magnum's
// slot code (internal/slots) over the factory's own agents store, so the
// review daemon's slots never see a copy, and the copies' own block of the
// configuration ([[factory.repo]], never a second [[pool]]). A claim hands a
// task a copy for its owner row (the runtime's Owner), the copy it used last
// first, and adds one on demand up to max_copies; the claim protocol then
// readies it for the task. A release keeps the task's branch and leaves a
// handoff.md in the task's directory. The copies' servers are in server.go.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/scrub"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// RedisDBsPerCopy is how many Redis databases a copy has: copy n's start at
// n × RedisDBsPerCopy (REDIS_DB_OFFSET).
const RedisDBsPerCopy = config.RedisDBsPerCopy

// RedisOffsetEnv is the variable a copy's Redis configuration reads its
// first database from. A repository whose env names it gets each copy's own
// offset there, whatever value the env gives it.
const RedisOffsetEnv = config.RedisOffsetEnv

// HandoffFile is the file of a task's directory a release writes: how the
// task left its copy, for whoever claims a copy for it next.
const HandoffFile = "handoff.md"

var (
	// ErrNoCopy is a claim no copy is free for: every copy the repository
	// may have is busy or broken. It costs the task nothing (it is ErrHeld
	// too): the task waits with the reason the error gives, "copy: all 3
	// busy" (claimCopy).
	ErrNoCopy = errors.New("factory: no free copy")
	// ErrPaneInCopy is a release refused while one of the factory's
	// sessions works in the copy.
	ErrPaneInCopy = errors.New("factory: a pane runs in the copy")
)

// noCopy is ErrNoCopy with what the task waits for ("all 3 busy").
type noCopy struct{ what string }

func (n noCopy) Error() string { return waitCopy + ": " + n.what }

func (n noCopy) Is(target error) bool { return target == ErrNoCopy || target == ErrHeld }

// copySet is the Copies NewCopies builds.
type copySet struct {
	cfg   *config.Config
	slots *slots.Manager
	st    *store.Store
	git   *gitx.Client
	run   execx.Runner
	my    slots.MySQL
	panes func(context.Context) (herdr.Snapshot, error)
	owner func(context.Context, registry.Task) (store.PR, error)
	// atWork is the runtime's AtWork: who of a task is at work in its copy
	// (a run in flight, or you in one of its panes); nil: nobody can say.
	atWork func(context.Context, registry.Task) (role string, you bool, err error)

	// mu guards the copies' bookkeeping, held only while a claim counts the
	// copies, reserves one or picks the one it adds or repairs: adding names
	// the copies claims add and the copies a repair works on now (their
	// setup runs without mu), and locks are the copies' own locks
	// (lockCopy). repairs are the broken copies' repairs under this daemon
	// start (repair.go: the start's repair, the backoff's, the cause). srv
	// guards the servers' records, brw the browsers'.
	mu, srv, brw sync.Mutex
	adding       map[string]bool
	repairs      map[string]*copyRepairs
	locks        map[string]*sync.Mutex

	// Seams the tests replace: the process table, the signal to a server's
	// process group and to one process, the start of a server, the TCP
	// listeners (lsof) and a connection to an address (nil: something
	// answers), the start of a browser and whether a file exists, the wait
	// between a stop's looks and the clock.
	processes    func(ctx context.Context) ([]execx.Process, error)
	kill         func(pgid int, sig syscall.Signal) error
	signal       func(pid int, sig syscall.Signal) error
	spawn        func(ctx context.Context, dir string, env map[string]string, command, log, pidfile string) (int, error)
	listeners    func(ctx context.Context) ([]listener, error)
	dial         func(ctx context.Context, addr string) error
	spawnBrowser func(ctx context.Context, bin string, args []string, log string) (pid int, err error)
	exists       func(path string) bool
	sleep        func(ctx context.Context, d time.Duration) error
	now          func() time.Time
}

// The copies the daemon wires are the engine's.
var _ Copies = (*copySet)(nil)

// NewCopies returns the factory's copies: sm is magnum's slot manager over
// st, the factory's agents store (Layout.FactoryAgentsDB, magnum's schema),
// whose guard sees herdr.GuardView; git and run run the claim protocol's
// commands, my reads a copy's development database (nil: no reset_db at a
// claim), panes is the factory's herdr session's snapshot (a pane in a copy
// that no session row names holds it; nil: no such check), owner is the
// runtime's Owner, the task's row in st a copy is claimed for, and atWork
// the runtime's AtWork, who of the claiming task is at work in the copy
// (its own sessions hold a claim only then; nil: they always do). A claim
// adds copy n on demand with sm.ProvisionPool (FactoryRepo.Pool with the
// copy's env), up to max_copies. sm saves what a copy's or a fix checkout's
// tree held before it discards it in the factory's folders (saveDir).
func NewCopies(cfg *config.Config, sm *slots.Manager, st *store.Store, git *gitx.Client, run execx.Runner, my slots.MySQL,
	panes func(context.Context) (herdr.Snapshot, error), owner func(context.Context, registry.Task) (store.PR, error),
	atWork func(context.Context, registry.Task) (role string, you bool, err error)) Copies {
	sm.SetSaveDir(func(_ store.Slot, pr store.PR) string { return saveDir(cfg.Layout, pr) })
	c := &copySet{cfg: cfg, slots: sm, st: st, git: git, run: run, my: my, panes: panes, owner: owner, atWork: atWork,
		adding: map[string]bool{}, repairs: map[string]*copyRepairs{}, locks: map[string]*sync.Mutex{},
		kill:   func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) },
		signal: syscall.Kill, exists: isFile, dial: dialTCP,
		now: time.Now}
	c.processes = func(ctx context.Context) ([]execx.Process, error) { return execx.ProcessTable(ctx, run) }
	c.spawn = c.spawnServer
	c.listeners = c.lsofListeners
	c.spawnBrowser = c.spawnChrome
	c.sleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	return c
}

// saveDir is where magnum's slot code saves what a checkout of the
// factory's held before a step discards it (slots.Manager.SetSaveDir), by
// the owner row that holds it: a task's copy in the task's folder
// (FactoryTaskSaves), a fix checkout in its PR work's
// (FactoryPRWorkSaves); "" for any other row, the slot code's own folder.
// The daemon prunes them past 14 days (pruneSaves).
func saveDir(l paths.Layout, owner store.PR) string {
	if id, ok := runtime.OwnerTask(owner.NodeID); ok {
		return l.FactoryTaskSaves(id)
	}
	if _, ok := fixOwnerWork(owner.NodeID); ok {
		if repo, number := fixOwnerPR(owner); repo != "" {
			return l.FactoryPRWorkSaves(repo, number)
		}
	}
	return ""
}

// Claim hands task t a copy of its repository for its owner row: the copy
// it holds, else a free one, the copy it used last first (slots.Reserve:
// the owner row's state stays the runtime's), else a new one while the
// repository has fewer than max_copies (provision). None left is ErrNoCopy,
// "copy: all 3 busy". Every claim runs the claim protocol (prepare), under
// the copy's own lock: claims of different copies go on side by side, and a
// release of another copy never waits for one. So a claim of the copy a
// task holds runs it again, and an install step can run in the middle of
// the task's work: a stage claims its task's copy once, off the loop, and
// keeps it (Engine.claimCopy).
func (c *copySet) Claim(ctx context.Context, t registry.Task) (Copy, error) {
	repo, ok := c.cfg.FactoryRepo(t.Repo)
	if !ok {
		return Copy{}, fmt.Errorf("claim a copy for %s: no [[factory.repo]] for %q", TaskKey(t), t.Repo)
	}
	pr, err := c.owner(ctx, t)
	if err != nil {
		return Copy{}, fmt.Errorf("claim a copy for %s: %w", TaskKey(t), err)
	}
	sl, err := c.reserve(ctx, repo, pr, false)
	if _, wait := errors.AsType[noCopy](err); wait {
		return Copy{}, err
	}
	if err != nil {
		return Copy{}, fmt.Errorf("claim a copy for %s: %w", TaskKey(t), err)
	}
	n, ok := copyNumber(repo, sl.Name)
	if !ok {
		return Copy{}, fmt.Errorf("claim a copy for %s: %s is no copy of %s (copy_name %q)", TaskKey(t), sl.Name, repo.Repo, repo.CopyName)
	}
	cp := copyOf(repo, sl, n)
	unlock := c.lockCopy(cp.Name)
	defer unlock()
	if err := c.prepare(ctx, t, pr, repo, sl, cp); err != nil {
		return Copy{}, fmt.Errorf("claim %s for %s: %w", cp.Name, TaskKey(t), err)
	}
	return cp, nil
}

// lockCopy takes copy name's own lock: its claim protocol and its release
// run one at a time.
func (c *copySet) lockCopy(name string) (unlock func()) {
	c.mu.Lock()
	l := c.locks[name]
	if l == nil {
		l = &sync.Mutex{}
		c.locks[name] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// isFixCopy reports whether copy n of repo is one of its fix copies: the
// fix_copies numbered past max_copies (max_copies+1 to
// max_copies+fix_copies), set apart for the fixes of your PRs. Every other
// copy is the tasks'.
func isFixCopy(repo config.FactoryRepo, n int) bool {
	return n > repo.MaxCopies && n <= repo.MaxCopies+repo.FixCopies
}

// ownsCopy reports whether a claim of its kind (fix: a fix's, else a
// task's) may take slot name of repo: a copy of repo (copyNumber) among
// the fix copies for a fix's, among the others for a task's.
func ownsCopy(repo config.FactoryRepo, name string, fix bool) bool {
	n, ok := copyNumber(repo, name)
	return ok && isFixCopy(repo, n) == fix
}

// copyRange is where a claim of its kind numbers the copies it adds (from
// first, at most limit of them): the tasks' from 1, max_copies of them; the
// fixes' from max_copies+1, fix_copies of them.
func copyRange(repo config.FactoryRepo, fix bool) (first, limit int) {
	if fix {
		return repo.MaxCopies + 1, repo.FixCopies
	}
	return 1, repo.MaxCopies
}

// reserve is the slot of the copy owner row pr gets, among the copies a
// claim of its kind may take (fix: the fix copies, else the tasks'): the
// one it holds or a free one (reserveFree), else a copy added for it
// (nextCopy, provision). mu is held only to reserve and to pick the copy to
// add (noted in adding); the setup runs without it. A copy another claim
// takes while this one adds it is lost to that claim, and the next pass
// adds or reserves another. A release a crash cut short ends first: the
// row keeps that copy until its release is done, and no other one can be
// its.
func (c *copySet) reserve(ctx context.Context, repo config.FactoryRepo, pr store.PR, fix bool) (store.Slot, error) {
	if sl, err := c.st.SlotByPR(ctx, pr.ID); err == nil && slices.Contains([]string{store.SlotReleasing, store.SlotDirtySchema}, sl.State) {
		n, ok := copyNumber(repo, sl.Name)
		if !ok {
			return store.Slot{}, fmt.Errorf("%s is no copy of %s", sl.Name, repo.Repo)
		}
		unlock := c.lockCopy(sl.Name)
		err := c.slots.Release(ctx, sl, c.pool(repo, n), "released")
		unlock()
		if err != nil {
			return store.Slot{}, fmt.Errorf("end the release of %s: %w", sl.Name, err)
		}
	}
	// Each pass adds a copy or says why none: max_copies (fix_copies) bounds
	// them.
	_, limit := copyRange(repo, fix)
	for range limit + 2 {
		c.mu.Lock()
		sl, err := c.reserveFree(ctx, repo, pr, fix)
		if !errors.Is(err, slots.ErrNoFreeSlot) {
			c.mu.Unlock()
			return sl, err
		}
		n, wait, err := c.nextCopy(ctx, repo, fix)
		name := repo.Pool().Slot(n)
		if err == nil && wait == "" {
			c.adding[name] = true
		}
		if err != nil {
			c.mu.Unlock()
			return store.Slot{}, err
		}
		if wait != "" {
			broken, ok, err := c.repairable(ctx, repo, fix)
			c.mu.Unlock()
			if err != nil {
				return store.Slot{}, err
			}
			if !ok {
				return store.Slot{}, noCopy{what: wait}
			}
			err = c.repair(ctx, repo, broken, false)
			c.endRepair(broken.Name)
			if _, broke := errors.AsType[copyBroke](err); err != nil && !broke { // broken again: the claim goes on
				return store.Slot{}, err
			}
			continue
		}
		c.mu.Unlock()
		err = c.provision(ctx, repo, n)
		c.mu.Lock()
		delete(c.adding, name)
		c.mu.Unlock()
		if err != nil {
			return store.Slot{}, err
		}
	}
	return store.Slot{}, fmt.Errorf("no copy of %s became free after adding %d", repo.Repo, limit)
}

// reserveFree is slots.Reserve over the copies a claim of its kind may
// take (ownsCopy): the copy owner row pr holds, else a free one, the copy
// it used last first (store.FreeSlots); slots.ErrNoFreeSlot when none of
// them is free. So a task never takes a fix copy, and a fix never a task's.
func (c *copySet) reserveFree(ctx context.Context, repo config.FactoryRepo, pr store.PR, fix bool) (store.Slot, error) {
	switch sl, err := c.st.SlotByPR(ctx, pr.ID); {
	case err == nil && slices.Contains([]string{store.SlotClaimed, store.SlotHeld}, sl.State):
		return sl, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		// A store failure is not "no copy": a second copy for the row would
		// follow.
		return store.Slot{}, fmt.Errorf("the copy it holds: %w", err)
	}
	free, err := c.st.FreeSlots(ctx, repo.Repo, pr.ID)
	if err != nil {
		return store.Slot{}, err
	}
	pool := repo.Pool()
	for _, sl := range free {
		if !ownsCopy(repo, sl.Name, fix) {
			continue
		}
		_, err := c.st.AssignSlot(ctx, pr.ID, sl.ID, pool.DBNames(slots.SlotDBSlug(sl))...)
		if err == nil {
			return c.st.SlotByID(ctx, sl.ID)
		}
		if !errors.Is(err, store.ErrConflict) {
			return store.Slot{}, fmt.Errorf("reserve %s: %w", sl.Name, err)
		}
	}
	return store.Slot{}, slots.ErrNoFreeSlot
}

// nextCopy is the number of the copy a claim of its kind adds (fix: a fix
// copy, else a task's; copyRange): one a crash left provisioning first,
// else the lowest number of its range with no copy and no directory; wait
// says why there is none ("all 3 busy", "2 busy, 1 broken (the next repair
// at 15:40, or a restart: magnum factory install)"). A broken copy counts
// toward its range's limit (max_copies, fix_copies) and is never
// provisioned again by a claim: only a repair brings it back, the start's
// (repairable), the backoff's (repair.go) or yours. A copy another claim
// adds or a repair works on now (adding) is busy. A row that is no copy of
// the repository counts as the tasks'. The caller holds mu.
func (c *copySet) nextCopy(ctx context.Context, repo config.FactoryRepo, fix bool) (n int, wait string, err error) {
	list, err := c.st.ListSlots(ctx, store.SlotFilter{RepoFullName: repo.Repo, Kind: store.SlotKindPool})
	if err != nil {
		return 0, "", err
	}
	first, limit := copyRange(repo, fix)
	mine := func(name string) bool {
		if _, ok := copyNumber(repo, name); !ok {
			return !fix
		}
		return ownsCopy(repo, name, fix)
	}
	list = slices.DeleteFunc(list, func(sl store.Slot) bool { return !mine(sl.Name) })
	live, broken := 0, 0
	names := map[string]bool{}
	for _, sl := range list {
		switch sl.State {
		case store.SlotRemoved:
			continue
		case store.SlotProvisioning:
			if n, ok := copyNumber(repo, sl.Name); ok && !c.adding[sl.Name] {
				return n, "", nil
			}
		case store.SlotBroken:
			broken++
		}
		live++
		names[sl.Name] = true
	}
	for name := range c.adding {
		if ownsCopy(repo, name, fix) && !names[name] {
			live++ // its row comes with its provisioning
			names[name] = true
		}
	}
	pool := repo.Pool()
	last := first + limit - 1
	for n := first; live < limit && n <= last; n++ {
		if names[pool.Slot(n)] {
			continue
		}
		if _, err := os.Lstat(pool.Path(n)); errors.Is(err, fs.ErrNotExist) {
			return n, "", nil
		}
	}
	busy := live - broken
	wait = fmt.Sprintf("%d busy", busy)
	if busy >= limit {
		wait = fmt.Sprintf("all %d busy", busy)
	}
	if broken > 0 {
		wait += fmt.Sprintf(", %d broken (%s)", broken, c.nextRepairWords(list))
	}
	for n := first; live < limit && n <= last; n++ {
		if !names[pool.Slot(n)] {
			// Its path exists, and no copy is there.
			wait += fmt.Sprintf(", and %s is a directory that is no copy", pool.Path(n))
			break
		}
	}
	return 0, wait, nil
}

// provision adds copy n (slots.ProvisionPool with the copy's env). A failed
// setup (the setup step, its verification or the post_checkout after them:
// setupFailed) runs once more; a second failure marks the copy broken
// (markBroken), and the claim goes on to another copy. Too little disk is a
// wait; any other failure (a fetch that cannot reach origin) is the claim's
// error, and the copy stays provisioning for the next claim.
func (c *copySet) provision(ctx context.Context, repo config.FactoryRepo, n int) error {
	pool := c.pool(repo, n)
	name := pool.Slot(n)
	err := c.slots.ProvisionPool(ctx, pool, n)
	for try := 1; err != nil; try++ {
		switch {
		case errors.Is(err, slots.ErrLowDisk):
			return noCopy{what: "too little disk for another copy (" + strings.TrimPrefix(err.Error(), "slots: ") + ")"}
		case ctx.Err() != nil || !c.setupFailed(ctx, name, err):
			return err
		case try == 2:
			_, err := c.markBroken(ctx, name, whatSetupTwice, err, false)
			return err
		}
		err = c.slots.ProvisionPool(ctx, pool, n)
	}
	return nil
}

// setupFailed reports whether err, a failed provisioning of copy name, is
// its setup's: the setup step failed after the worktree was added, the
// verification of what the setup makes (the slug marker, the databases)
// failed, or the post_checkout step after that verification did. A check of
// the databases that could not run (MySQL down: no ErrVerify) is not.
func (c *copySet) setupFailed(ctx context.Context, name string, err error) bool {
	if errors.Is(err, slots.ErrVerify) {
		return true
	}
	subject := "slot:" + name
	added, derr := steps.Done(ctx, c.st, subject, "worktree_add")
	if derr != nil || !added {
		return false
	}
	setup, derr := steps.Done(ctx, c.st, subject, "setup")
	switch {
	case derr != nil:
		return false
	case !setup:
		return true
	}
	verified, derr := steps.Done(ctx, c.st, subject, "verify_dbs")
	if derr != nil || !verified {
		return false
	}
	installed, derr := steps.Done(ctx, c.st, subject, "post_checkout")
	return derr == nil && !installed
}

// The words a broken copy's reason starts with: what failed.
const (
	whatSetupTwice   = "setup failed twice"
	whatRepairFailed = "its repair failed"
)

// markBroken moves copy name to broken with why its setup failed (what:
// whatSetupTwice, whatRepairFailed; a verification failure has moved it
// there already), schedules its next repair (a break and a start's repair
// that failed: 10 minutes on; a backoff repair that failed: 30, then every
// hour) and records a factory.copy_broken warning, once per cause: a copy
// that broke the way it last broke (copySignature) says it no more. A claim
// takes it no more until a repair works. Why names the step that failed and
// the first line of its stderr that says why (brokenWhy), and the setup's
// log, which has the rest. It returns the break (the claim goes on).
func (c *copySet) markBroken(ctx context.Context, name, what string, cause error, backoff bool) (copyBroke, error) {
	sl, err := c.st.SlotByName(ctx, name)
	if err != nil {
		return copyBroke{}, err
	}
	why := scrub.Text(what + ": " + brokenWhy(cause) + " (" + setupLogWords +
		filepath.Join(c.cfg.Layout.Logs(), slots.ProvisionLog(name)) + ")")
	stored := why
	if sl.State != store.SlotProvisioning { // a verification failure: the slot code said why
		stored = store.Deref(sl.LastError)
	}
	// The cause is the whole line's, its own and changing words taken out
	// before the cut (a reason keeps outputLineRunes runes): kept beside the
	// reason, so the copies broken by one cause read the same.
	repo, _, _ := c.repoOf(name)
	sig := copySignature(repo, sl, what+": "+brokenWhyMax(cause, wholeLineRunes))
	c.saveCause(ctx, name, stored, sig)
	b := c.broke(sl, sig, backoff)
	b.why = why
	if sl.State == store.SlotProvisioning {
		if err := c.st.TransitionSlot(context.WithoutCancel(ctx), sl.ID, []string{store.SlotProvisioning}, store.SlotBroken,
			func(u *store.SlotUpdate) { u.Set("last_error", why) }); err != nil {
			return b, fmt.Errorf("mark %s broken: %w", name, err)
		}
	}
	if !b.same {
		c.event(ctx, name, "warn", "factory.copy_broken", why+"; no claim takes it until a repair works: "+repairAtWords(b.next, c.now()))
	}
	return b, nil
}

// brokenWhy is why a copy's setup failed, in one line: the command that
// exited (its label) and the first line of its stderr that says why, past
// mise's warnings that come first (a copy's untrusted config: they named
// no failure); else the error's first line (a verification's).
func brokenWhy(cause error) string { return brokenWhyMax(cause, outputLineRunes) }

// wholeLineRunes bounds the line a broken copy's cause is read from
// (brokenWhyMax), far past the cut of its reason.
const wholeLineRunes = 4000

// brokenWhyMax is brokenWhy with its line at most n runes long.
func brokenWhyMax(cause error, n int) string {
	if ee, ok := errors.AsType[*execx.ExitError](cause); ok {
		why := fmt.Sprintf("%s exited %d", cmp.Or(ee.Cmd.Label, firstWord(ee.Cmd.String())), ee.Code)
		if line := firstLineMax(ee.Stderr, n); line != "" {
			why += ": " + line
		}
		return why
	}
	return cleanLineMax(textx.FirstLine(cause.Error()), n)
}

// repairable is a broken copy of repo, among those a claim of its kind may
// take (ownsCopy), whose start's repair is still owed (this daemon start
// neither broke it nor repaired it yet) and that no claim or repair works
// on, noted in repairs and adding so that no other claim repairs it too;
// false when there is none. A restart is when a fix of a setup's cause (the
// configuration, the daemon's PATH) takes effect, so each start repairs
// each broken copy once in the claim that finds no copy (repair); the
// backoff repairs it again while the daemon runs (repair.go). The caller
// holds mu.
func (c *copySet) repairable(ctx context.Context, repo config.FactoryRepo, fix bool) (store.Slot, bool, error) {
	list, err := c.st.ListSlots(ctx, store.SlotFilter{RepoFullName: repo.Repo, Kind: store.SlotKindPool})
	if err != nil {
		return store.Slot{}, false, err
	}
	for _, sl := range list {
		r := c.repairs[sl.Name]
		if sl.State != store.SlotBroken || (r != nil && (r.started || r.running)) || c.adding[sl.Name] {
			continue
		}
		if ownsCopy(repo, sl.Name, fix) {
			c.startRepair(ctx, repo, sl)
			return sl, true, nil
		}
	}
	return store.Slot{}, false, nil
}

// repair runs broken copy sl's provisioning again (slots.Repair: render,
// setup, verification, post_checkout), the start's (in a claim) or the
// backoff's, with a factory.copy_repair begin and an ok or a fail event. It
// ends free, or broken again when its setup fails (setupFailed; markBroken:
// a copyBroke, after which a claim goes on). Any other failure of the start's repair (a fetch that
// cannot reach origin) is the claim's error, and the copy stays
// provisioning for the next claim; a backoff repair that could not run to
// its end leaves the copy as it was (unrepaired). The caller marked it in
// repairs and adding (startRepair) and ends that after (endRepair).
func (c *copySet) repair(ctx context.Context, repo config.FactoryRepo, sl store.Slot, backoff bool) error {
	n, _ := copyNumber(repo, sl.Name)
	c.phase(ctx, sl.Name, "factory.copy_repair", store.PhaseBegin, c.repairBeginWords(sl.Name, backoff))
	err := c.slots.Repair(ctx, sl, c.pool(repo, n))
	switch {
	case err == nil:
		c.repaired(sl.Name)
		c.phase(ctx, sl.Name, "factory.copy_repair", store.PhaseOK, "repaired: it is back in the pool")
		return nil
	case ctx.Err() == nil && c.setupFailed(ctx, sl.Name, err):
		b, merr := c.markBroken(ctx, sl.Name, whatRepairFailed, err, backoff)
		c.repairFailed(ctx, sl.Name, b, brokenWhy(err)+"; "+repairAtWords(b.next, c.now()))
		if merr != nil {
			return merr
		}
		return b
	case backoff:
		return c.unrepaired(ctx, sl, err)
	}
	c.phase(ctx, sl.Name, "factory.copy_repair", store.PhaseFail, "its repair did not run to its end: "+textx.FirstLine(err.Error()))
	return errors.Join(err, c.keepReason(ctx, sl))
}

// prepare is the claim protocol, in order: the task's branch created at
// origin/<base> when the task has a branch name and no branch yet (its
// first claim), or checked out when it has one (a later claim of a task
// that left another copy), reset_db when the development database holds a migration the task's code
// lacks, each installs entry whose lockfile differs from the copy's last
// install, and a flush of the copy's Redis databases. Each one that has
// work to do is a step (steps.Step) of the claim's subject,
// slot:<copy>:claim:<assignment>, with begin and ok or fail rows, and runs
// the copy's guard (guard) first. A step that fails fails the claim, which
// keeps the copy: the task's next claim runs the steps not done yet, each
// after its guard again, and never one that is done.
func (c *copySet) prepare(ctx context.Context, t registry.Task, pr store.PR, repo config.FactoryRepo, sl store.Slot, cp Copy) error {
	a, err := c.st.OpenAssignmentBySlot(ctx, sl.ID)
	if err != nil {
		return err
	}
	subject := "slot:" + cp.Name + ":claim:" + strconv.FormatInt(a.ID, 10)
	step := func(name string, fn func(ctx context.Context) error) error {
		return steps.Step(ctx, c.st, subject, name, func(ctx context.Context) error {
			if err := c.guard(ctx, t, pr, sl, cp); err != nil {
				return err
			}
			return fn(ctx)
		})
	}
	switch {
	case c.mustCreate(ctx, t, cp):
		if err := step("create_branch", func(ctx context.Context) error { return c.createBranch(ctx, t, repo, cp) }); err != nil {
			return err
		}
	case c.mustSwitch(ctx, t, cp):
		if err := step("switch_branch", func(ctx context.Context) error { return c.git.Switch(ctx, cp.Path, t.Branch) }); err != nil {
			return err
		}
	}
	return c.readyData(ctx, repo, sl, cp, step)
}

// claimStep runs one step of a claim protocol (steps.Step of its subject,
// the copy's guard first).
type claimStep func(name string, fn func(ctx context.Context) error) error

// readyData is the part of a claim protocol that readies copy cp's data
// for the code checked out, after the checkout, in order: reset_db when the
// development database holds a migration that code lacks, each installs
// entry whose lockfile differs from the copy's last install, and a flush of
// the copy's Redis databases, each one that has work to do a step (step).
func (c *copySet) readyData(ctx context.Context, repo config.FactoryRepo, sl store.Slot, cp Copy, step claimStep) error {
	if why, err := c.foreignSchema(ctx, repo, sl, cp); err != nil {
		return err
	} else if why != "" {
		if err := step("reset_db", func(ctx context.Context) error { return c.resetDB(ctx, repo, cp, why) }); err != nil {
			return err
		}
	}
	for _, in := range repo.Installs {
		sum, err := c.installNeeded(ctx, cp, in)
		if err != nil {
			return err
		}
		if sum == "" {
			continue
		}
		if err := step("install "+in.Lock, func(ctx context.Context) error { return c.install(ctx, cp, in, sum) }); err != nil {
			return err
		}
	}
	if _, ok := cp.Env[RedisOffsetEnv]; ok {
		return step("flush_redis", func(ctx context.Context) error { return c.flushRedis(ctx, cp) })
	}
	return nil
}

// guard is what each claim step of task t (its owner row pr) runs before
// it changes the copy or its data: no factory pane works in the copy
// (paneIn: the task's own sessions only while one of them is at work), and
// magnum's slot guard lets it go (a person's agent or process in it, their
// changes, a HEAD that moved). HEAD at the tip of the task's own branch is
// the factory's own (ownHead).
func (c *copySet) guard(ctx context.Context, t registry.Task, pr store.PR, sl store.Slot, cp Copy) error {
	if err := c.paneIn(ctx, cp, pr, t); err != nil {
		return err
	}
	if _, _, err := c.ownHead(ctx, t, sl, cp); err != nil {
		return err
	}
	return c.slots.Guard(ctx, sl)
}

// ownHead reads the copy's HEAD and branch and, when HEAD is the tip of the
// task's own branch, records it as the copy's checkout (checked_out_sha):
// the factory's work, as magnum's checkouts are magnum's, which the branch
// keeps whatever the copy does next.
func (c *copySet) ownHead(ctx context.Context, t registry.Task, sl store.Slot, cp Copy) (head, branch string, err error) {
	if head, err = c.git.RevParse(ctx, cp.Path, "HEAD"); err != nil {
		return "", "", err
	}
	if branch, err = c.git.CurrentBranch(ctx, cp.Path); err != nil {
		return "", "", err
	}
	if t.Branch == "" || branch != t.Branch {
		return head, branch, nil
	}
	cur, err := c.st.SlotByID(ctx, sl.ID)
	if err != nil {
		return "", "", err
	}
	if store.Deref(cur.CheckedOutSHA) != head {
		if err := c.st.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", head) }); err != nil {
			return "", "", err
		}
	}
	return head, branch, nil
}

// installKey is the kv key that holds the hash of the lockfile lock copy
// name last installed.
func installKey(name, lock string) string { return "factory.copy." + name + ".install." + lock }

// mustSwitch reports whether the claim checks the task's branch out in the
// copy: the task has one (the release of the copy it left kept it) and the
// copy is not on it. A task without a branch yet stays on the copy's
// placeholder, at origin/<base>. A git failure here says yes: the switch's
// step reports it.
func (c *copySet) mustSwitch(ctx context.Context, t registry.Task, cp Copy) bool {
	if t.Branch == "" {
		return false
	}
	if _, err := c.git.RevParse(ctx, cp.Path, "refs/heads/"+t.Branch); errors.Is(err, gitx.ErrNoSuchRef) {
		return false
	}
	b, err := c.git.CurrentBranch(ctx, cp.Path)
	return err != nil || b != t.Branch
}

// mustCreate reports whether the claim creates the task's branch: the task
// has a branch name (intake gave it one) and the repository no branch of
// it yet.
func (c *copySet) mustCreate(ctx context.Context, t registry.Task, cp Copy) bool {
	if t.Branch == "" {
		return false
	}
	_, err := c.git.RevParse(ctx, cp.Path, "refs/heads/"+t.Branch)
	return errors.Is(err, gitx.ErrNoSuchRef)
}

// createBranch creates the task's branch at origin/<base> in the copy and
// checks it out, tracking nothing (gitx.SwitchCreate); a branch made since
// the check (a step resumed after the switch) is checked out.
func (c *copySet) createBranch(ctx context.Context, t registry.Task, repo config.FactoryRepo, cp Copy) error {
	err := c.git.SwitchCreate(ctx, cp.Path, t.Branch, "origin/"+cmp.Or(repo.Base, "master"))
	if errors.Is(err, gitx.ErrBranchExists) {
		return c.git.Switch(ctx, cp.Path, t.Branch)
	}
	return err
}

// foreignSchema says why the copy's databases need reset_db ("" when they
// do not): the development database's newest migration
// (MAX(schema_migrations.version)) is none the code checked out (a task's,
// a PR's) has, neither a file of db/migrate nor the version db/schema.rb
// declares. Another task or PR migrated it. A database at a version the
// code has is kept (the task migrates it further itself).
func (c *copySet) foreignSchema(ctx context.Context, repo config.FactoryRepo, sl store.Slot, cp Copy) (string, error) {
	db := devDatabase(repo.Pool().DBNames(slots.SlotDBSlug(sl)))
	if c.my == nil || db == "" || len(repo.ResetDB) == 0 {
		return "", nil
	}
	have, err := c.my.SchemaMigrationsMax(ctx, db)
	switch {
	case errors.Is(err, mysqlx.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("the schema version of %s: %w", db, err)
	case have == "":
		return "", nil
	}
	known, err := codeVersions(cp.Path)
	if err != nil || known[have] {
		return "", err
	}
	return fmt.Sprintf("%s holds migration %s, which the code checked out does not have", db, have), nil
}

// resetDB runs the repository's reset_db commands in the copy.
func (c *copySet) resetDB(ctx context.Context, repo config.FactoryRepo, cp Copy, why string) error {
	c.event(ctx, cp.Name, "info", "factory.copy_reset_db", why+": reset_db")
	for _, s := range repo.ResetDB {
		if err := c.script(ctx, cp, s, slots.ResetDBTimeout); err != nil {
			return err
		}
	}
	return nil
}

// devDatabase is the database whose schema_migrations a claim reads: the
// first name with "development" in it, else the first.
func devDatabase(names []string) string {
	if i := slices.IndexFunc(names, func(n string) bool { return strings.Contains(n, "development") }); i >= 0 {
		return names[i]
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// The checkout files codeVersions reads: a migration's version leads its
// file name, and db/schema.rb declares the version it is at.
var (
	migrationRe = regexp.MustCompile(`^([0-9]+)_[^/]*\.rb$`)
	schemaRe    = regexp.MustCompile(`define\(\s*version:\s*([0-9_]+)\s*\)`)
)

// schemaFileMax caps the db/schema.rb a claim reads (fsx.ReadRegular).
const schemaFileMax = 16 << 20

// codeVersions are the migration versions the checkout at dir has, as
// schema_migrations keeps them: those of db/migrate's files and the one
// db/schema.rb declares (underscores removed).
func codeVersions(dir string) (map[string]bool, error) {
	known := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(dir, "db", "migrate"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if m := migrationRe.FindStringSubmatch(e.Name()); m != nil && e.Type().IsRegular() {
			known[m[1]] = true
		}
	}
	b, err := fsx.ReadRegular(filepath.Join(dir, slots.SchemaFile), schemaFileMax)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", slots.SchemaFile, err)
	default:
		if m := schemaRe.FindSubmatch(b); m != nil {
			known[strings.ReplaceAll(string(m[1]), "_", "")] = true
		}
	}
	return known, nil
}

// lockFileMax caps a lockfile an install hashes.
const lockFileMax = 16 << 20

// installNeeded is the hash of installs entry in's lockfile in the copy
// when it differs from the one the copy last installed (installKey; a copy
// that never installed needs them all), "" when the entry has nothing to
// do: the same lockfile, or none in the checkout.
func (c *copySet) installNeeded(ctx context.Context, cp Copy, in config.FactoryInstall) (string, error) {
	if !filepath.IsLocal(in.Lock) {
		return "", fmt.Errorf("installs lock %q is not a path inside the checkout", in.Lock)
	}
	b, err := fsx.ReadRegular(filepath.Join(cp.Path, in.Lock), lockFileMax)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", in.Lock, err)
	}
	h := sha256.Sum256(b)
	sum := hex.EncodeToString(h[:])
	last, ok, err := c.st.GetKV(ctx, installKey(cp.Name, in.Lock))
	if err != nil || (ok && last == sum) {
		return "", err
	}
	return sum, nil
}

// install runs installs entry in's command in the copy and records sum, its
// lockfile's hash, as the copy's last install of it.
func (c *copySet) install(ctx context.Context, cp Copy, in config.FactoryInstall, sum string) error {
	if err := c.script(ctx, cp, in.Run, slots.DepsTimeout); err != nil {
		return err
	}
	return c.st.SetKV(ctx, installKey(cp.Name, in.Lock), sum)
}

// redisFlushTimeout bounds one FLUSHDB.
const redisFlushTimeout = time.Minute

// flushRedis empties the copy's RedisDBsPerCopy Redis databases from its
// offset (`redis-cli -h <host> -p <port> -n <db> FLUSHDB`), when its env
// names one, on the Redis the copy's env names (redisOf): never on the
// daemon's own default, which a copy pointed at another Redis does not use.
// A Redis the env does not name is flushed nowhere: the step fails, naming
// what to set. A database flushed twice (a failed flush's next run) is as
// empty.
func (c *copySet) flushRedis(ctx context.Context, cp Copy) error {
	v, ok := cp.Env[RedisOffsetEnv]
	if !ok {
		return nil
	}
	offset, err := strconv.Atoi(v)
	if err != nil || offset < 0 {
		return fmt.Errorf("%s of %s is %q, no database number", RedisOffsetEnv, cp.Name, v)
	}
	ep, err := redisOf(cp.Env)
	if err != nil {
		return fmt.Errorf("flush the Redis databases of %s: %w", cp.Name, err)
	}
	for db := offset; db < offset+RedisDBsPerCopy; db++ {
		n := strconv.Itoa(db)
		cmd := ep.cli(redisFlushTimeout, "-n", n, "FLUSHDB")
		cmd.Unset, cmd.Mutates, cmd.Label = c.unset(), true, "redis-cli flushdb "+n+" of "+cp.Name+" on "+ep.addr()
		if _, err := c.run.Run(ctx, cmd); err != nil {
			return fmt.Errorf("flush Redis database %d of %s on %s: %w", db, cp.Name, ep.addr(), err)
		}
	}
	return nil
}

// RedisDatabasesProbe is the read-only command that asks the Redis of
// repo's copies how many databases it has (`redis-cli -h <host> -p <port>
// CONFIG GET databases`, which answers "databases" and the number on two
// lines), and that Redis as host:port. It is the Redis the env of repo's
// highest copy names (redisOf), as that copy's claims flush it; an env that
// names none is an error.
func RedisDatabasesProbe(repo config.FactoryRepo, timeout time.Duration) (execx.Cmd, string, error) {
	ep, err := redisOf(copyEnv(repo, repo.LastCopy()))
	if err != nil {
		return execx.Cmd{}, "", err
	}
	cmd := ep.cli(timeout, "CONFIG", "GET", "databases")
	cmd.Probe, cmd.Label = true, "redis-cli config get databases on "+ep.addr()
	return cmd, ep.addr(), nil
}

// redisEndpoint is a Redis server: its host and port, the user and the
// password a URL gives, and whether it takes TLS (rediss://).
type redisEndpoint struct {
	host, port, user, pass string
	tls                    bool
}

// addr is the Redis as host:port.
func (ep redisEndpoint) addr() string { return ep.host + ":" + ep.port }

// cli is `redis-cli -h <host> -p <port> [--tls] [--user <user>] args…` on
// the Redis, bounded by timeout, its password in REDISCLI_AUTH, never on
// the command line.
func (ep redisEndpoint) cli(timeout time.Duration, args ...string) execx.Cmd {
	argv := []string{"-h", ep.host, "-p", ep.port}
	if ep.tls {
		argv = append(argv, "--tls")
	}
	if ep.user != "" {
		argv = append(argv, "--user", ep.user)
	}
	var env map[string]string
	if ep.pass != "" {
		env = map[string]string{"REDISCLI_AUTH": ep.pass}
	}
	return execx.Cmd{Name: "redis-cli", Args: append(argv, args...), Env: env, Timeout: timeout}
}

// redisOf is the Redis the copy's env names: REDIS_URL (redis:// or
// rediss://, its host and port, 6379 by default, and its user and
// password), else REDIS_HOST and REDIS_PORT (one of them at least; the
// other is 127.0.0.1 or 6379). An env that names none, or a value that does
// not parse, is an error: a flush of a Redis the factory cannot name could
// empty the databases of another Redis's tenants, magnum's review slots
// among them.
func redisOf(env map[string]string) (redisEndpoint, error) {
	port := func(p string) (string, error) {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("%q is no port", p)
		}
		return p, nil
	}
	if raw := strings.TrimSpace(env["REDIS_URL"]); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" {
			return redisEndpoint{}, errors.New("REDIS_URL in the copy's env is not a redis:// or rediss:// URL with a host")
		}
		ep := redisEndpoint{host: u.Hostname(), port: "6379", tls: u.Scheme == "rediss"}
		if p := u.Port(); p != "" {
			if ep.port, err = port(p); err != nil {
				return redisEndpoint{}, fmt.Errorf("REDIS_URL in the copy's env: %w", err)
			}
		}
		if u.User != nil {
			ep.user = u.User.Username()
			ep.pass, _ = u.User.Password()
		}
		return ep, nil
	}
	host, p := strings.TrimSpace(env["REDIS_HOST"]), strings.TrimSpace(env["REDIS_PORT"])
	if host == "" && p == "" {
		return redisEndpoint{}, errors.New("the copy's env names no Redis: set REDIS_URL (or REDIS_HOST and REDIS_PORT) in its " +
			"[[factory.repo]] env, the Redis its databases are on")
	}
	ep := redisEndpoint{host: cmp.Or(host, "127.0.0.1"), port: "6379"}
	if p != "" {
		var err error
		if ep.port, err = port(p); err != nil {
			return redisEndpoint{}, fmt.Errorf("REDIS_PORT in the copy's env: %w", err)
		}
	}
	return ep, nil
}

// script runs one of the repository's commands in the copy as magnum's
// pool scripts run (`mise -C <copy> exec -- env <copy env> /bin/sh -c`),
// without the daemon's secrets (unset).
func (c *copySet) script(ctx context.Context, cp Copy, script string, timeout time.Duration) error {
	if _, err := c.run.Run(ctx, execx.Cmd{Name: "mise", Args: slots.MiseExecArgs(cp.Path, cp.Env, script), Dir: cp.Path,
		Unset: c.unset(), Timeout: timeout, Mutates: true, Label: script + " in " + cp.Name}); err != nil {
		return fmt.Errorf("%s in %s: %w", script, cp.Name, err)
	}
	return nil
}

// unset is what the copies' commands never inherit from the daemon: the
// variables that point git elsewhere, the GitHub tokens and the App
// identities' private keys, as magnum's heavy commands.
func (c *copySet) unset() []string { return unsetEnv(c.cfg) }

// Release hands task t's copy back: refused while one of the factory's
// sessions works in it (ErrPaneInCopy), its server stopped, then magnum's
// slot release (slots.Release, its guard first) puts the copy back on its
// placeholder at origin/<base>. The task's branch stays, its commits on it,
// pushed or not: HEAD at the tip of the task's own branch is recorded as the
// factory's checkout first, so the guard takes it for the factory's own work
// (no head drift, no unpushed hold) while it still holds a copy left on
// another branch or on no branch, and a person's changes. A released copy
// leaves handoff.md in the task's directory (writeHandoff). A task that
// holds no copy has nothing to release. It takes only the copy's own lock,
// never waiting for a claim of another copy.
func (c *copySet) Release(ctx context.Context, t registry.Task, reason string) error {
	repo, ok := c.cfg.FactoryRepo(t.Repo)
	if !ok {
		return fmt.Errorf("release the copy of %s: no [[factory.repo]] for %q", TaskKey(t), t.Repo)
	}
	pr, err := c.owner(ctx, t)
	if err != nil {
		return fmt.Errorf("release the copy of %s: %w", TaskKey(t), err)
	}
	sl, err := c.st.SlotByPR(ctx, pr.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release the copy of %s: %w", TaskKey(t), err)
	}
	unlock := c.lockCopy(sl.Name)
	defer unlock()
	n, ok := copyNumber(repo, sl.Name)
	if !ok {
		return fmt.Errorf("release the copy of %s: %s is no copy of %s", TaskKey(t), sl.Name, repo.Repo)
	}
	cp := copyOf(repo, sl, n)
	if err := c.paneIn(ctx, cp, store.PR{}, t); err != nil {
		return err
	}
	server, err := c.stopForRelease(ctx, cp)
	if err != nil {
		return fmt.Errorf("release %s of %s: %w", cp.Name, TaskKey(t), err)
	}
	left, err := c.leave(ctx, t, sl, cp)
	if err != nil {
		return fmt.Errorf("release %s of %s: %w", cp.Name, TaskKey(t), err)
	}
	if err := c.slots.Release(ctx, sl, c.pool(repo, n), reason); err != nil {
		return fmt.Errorf("release %s of %s: %w", cp.Name, TaskKey(t), err)
	}
	return c.writeHandoff(t, repo, cp, reason, left, server)
}

// paneIn refuses a copy one of the factory's live sessions works in (its
// working directory is the copy or below it), and a copy a pane of the
// factory's herdr session works in that no session row names (someone
// opened it there; the shell a parked session left is the factory's). The
// sessions of own, the owner row of task t that claims the copy (none at a
// release), refuse it only while somebody of t is at work, as the runtime's
// AtWork says for the factory's other changes of the copy (a run in
// flight, or you in one of its panes): one that waits there (its stage
// passed, its turn ended) waits for the factory's next prompt, which the
// claim comes before. Without AtWork they refuse it as any session does.
func (c *copySet) paneIn(ctx context.Context, cp Copy, own store.PR, t registry.Task) error {
	var atWork ownAtWork
	if c.atWork != nil {
		atWork = func(ctx context.Context) (string, bool, error) { return c.atWork(ctx, t) }
	}
	return c.paneInPath(ctx, cp.Path, own, atWork, TaskKey(t))
}

// ownAtWork says who of the work that claims a checkout is at work in it
// (the runtime's AtWork for a task): a role with a run in flight, or you in
// one of its panes.
type ownAtWork func(ctx context.Context) (role string, you bool, err error)

// paneInPath is paneIn of the checkout at path, for own, the owner row of
// the work that claims it (whose words name it), and atWork, who of that
// work is at work (nil: its sessions refuse it as any session does).
func (c *copySet) paneInPath(ctx context.Context, path string, own store.PR, atWork ownAtWork, whose string) error {
	live, err := c.st.LiveSessions(ctx)
	if err != nil {
		return err
	}
	root := fsx.Canon(path)
	in := func(dir string) bool {
		dir = fsx.Canon(dir)
		return dir != "" && (dir == root || strings.HasPrefix(dir, root+string(filepath.Separator)))
	}
	ownLooked := false
	for _, s := range live {
		cwd := store.Deref(s.Cwd)
		switch {
		case !in(cwd):
			continue
		case own.ID == 0 || s.PRID != own.ID || atWork == nil:
			return fmt.Errorf("%w: the %s session works in %s", ErrPaneInCopy, s.Role, cwd)
		case ownLooked:
			continue
		}
		ownLooked = true
		switch role, you, err := atWork(ctx); {
		case err != nil:
			return err
		case role != "" && you:
			return fmt.Errorf("%w: you're in the %s pane of %s (%s)", ErrPaneInCopy, role, whose, cwd)
		case role != "":
			return fmt.Errorf("%w: the %s agent works in it (%s)", ErrPaneInCopy, role, cwd)
		}
	}
	if c.panes == nil {
		return nil
	}
	snap, err := c.panes(ctx)
	if err != nil {
		return fmt.Errorf("the factory's herdr session: %w", err)
	}
	for _, p := range snap.Panes {
		if !in(p.Cwd) && !in(p.ForegroundCwd) {
			continue
		}
		prs, err := c.st.PRsWithPanes(ctx, []string{p.ID})
		if err != nil {
			return err
		}
		if len(prs) == 0 {
			return fmt.Errorf("%w: pane %s of the factory's herdr session works in %s, and no session of the factory's is in it",
				ErrPaneInCopy, p.ID, path)
		}
	}
	return nil
}

// leftCopy is how a task left its copy: the branch checked out ("" for a
// detached HEAD), HEAD, and the tracked changes the release discards.
type leftCopy struct {
	Branch, Head string
	Changed      []string
}

// leave reads how task t leaves copy cp and, when HEAD is the tip of the
// task's own branch, records it as the copy's checkout (checked_out_sha),
// the factory's own as magnum's checkouts are magnum's.
func (c *copySet) leave(ctx context.Context, t registry.Task, sl store.Slot, cp Copy) (leftCopy, error) {
	var l leftCopy
	var err error
	if l.Head, l.Branch, err = c.ownHead(ctx, t, sl, cp); err != nil {
		return l, err
	}
	entries, err := c.git.StatusPaths(ctx, cp.Path)
	if err != nil {
		return l, err
	}
	for _, e := range entries {
		if !e.Untracked() && !e.Ignored() {
			l.Changed = append(l.Changed, e.Path)
		}
	}
	return l, nil
}

// handoffListed caps the changed paths handoff.md names.
const handoffListed = 20

// writeHandoff writes handoff.md in task t's directory: the copy it left
// and why, its branch and head, its server, the changes the release
// discarded and where it saved them first, and what the next claimer
// needs. It replaces the last one.
func (c *copySet) writeHandoff(t registry.Task, repo config.FactoryRepo, cp Copy, reason string, l leftCopy, server string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s left %s\n\n", TaskKey(t), cp.Name)
	fmt.Fprintf(&b, "- left: %s, %s\n", c.now().UTC().Format(time.RFC3339), reason)
	fmt.Fprintf(&b, "- copy: %s (%s), back on its placeholder at origin/%s\n", cp.Name, cp.Path, repo.Pool().Base)
	if l.Branch == "" {
		b.WriteString("- branch: none, HEAD was detached\n")
	} else {
		fmt.Fprintf(&b, "- branch: %s, kept\n", l.Branch)
	}
	fmt.Fprintf(&b, "- head: %s\n", l.Head)
	fmt.Fprintf(&b, "- server: %s\n", server)
	if len(l.Changed) == 0 {
		b.WriteString("- uncommitted: none\n")
	} else {
		listed := strings.Join(l.Changed[:min(len(l.Changed), handoffListed)], ", ")
		if more := len(l.Changed) - handoffListed; more > 0 {
			listed += fmt.Sprintf(" (+%d more)", more)
		}
		fmt.Fprintf(&b, "- uncommitted: %d tracked change(s), saved under %s before the release discarded them (the copy's "+
			"slot.discarded event names the save): %s\n", len(l.Changed), c.cfg.Layout.FactoryTaskSaves(t.ID), listed)
	}
	b.WriteString("\n## For the next claimer\n\n")
	if l.Branch != "" && l.Branch == t.Branch {
		fmt.Fprintf(&b, "- `git switch %s` in the copy you claim (a claim does it): the work is on that branch, at %s.\n", l.Branch, l.Head)
	} else {
		b.WriteString("- The task had no branch of its own in the copy: start it from origin/" + repo.Pool().Base + ".\n")
	}
	b.WriteString("- The copy's databases carry the base schema again if the task migrated them: migrate them again after the switch.\n")
	b.WriteString("- Start the server again if a stage needs it.\n")
	dir := c.cfg.Layout.FactoryTaskDir(t.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("handoff of %s: %w", TaskKey(t), err)
	}
	if err := fsx.WriteFileAtomic(filepath.Join(dir, HandoffFile), []byte(execx.Redact(b.String())), 0o600); err != nil {
		return fmt.Errorf("handoff of %s: %w", TaskKey(t), err)
	}
	return nil
}

// Reserved lists the copies the tasks' owner rows hold, by slot: a claim
// reserves a copy (one that got it, waits on a guard or failed) and the
// release frees it, so the factory's agents store says which tasks hold one
// whatever the daemon remembers.
func (c *copySet) Reserved(ctx context.Context) ([]Reservation, error) {
	var out []Reservation
	err := c.eachReserved(ctx, func(sl store.Slot, owner store.PR) {
		if id, ok := runtime.OwnerTask(owner.NodeID); ok {
			out = append(out, Reservation{TaskID: id, Copy: sl.Name})
		}
	})
	if err != nil {
		return nil, fmt.Errorf("the copies reserved: %w", err)
	}
	return out, nil
}

// eachReserved calls fn with each slot of the agents store an owner row
// holds (not removed) and that row, by slot; a row gone since is skipped.
func (c *copySet) eachReserved(ctx context.Context, fn func(sl store.Slot, owner store.PR)) error {
	list, err := c.st.ListSlots(ctx, store.SlotFilter{})
	if err != nil {
		return err
	}
	for _, sl := range list {
		if sl.PRID == nil || sl.State == store.SlotRemoved {
			continue
		}
		pr, err := c.st.PRByID(ctx, *sl.PRID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		fn(sl, pr)
	}
	return nil
}

// Free is how many copies a task's claim could take now, over the
// factory's repositories: the free ones and those max_copies still lets a
// claim add. The fix copies are never a task's, and count for nothing here.
func (c *copySet) Free(ctx context.Context) (int, error) {
	total := 0
	for _, repo := range c.cfg.Factory.Repos {
		list, err := c.st.ListSlots(ctx, store.SlotFilter{RepoFullName: repo.Repo, Kind: store.SlotKindPool})
		if err != nil {
			return 0, err
		}
		live, free := 0, 0
		for _, sl := range list {
			if sl.State == store.SlotRemoved || ownsCopy(repo, sl.Name, true) {
				continue
			}
			live++
			if sl.State == store.SlotFree && !sl.Pinned && sl.HoldReason == nil {
				free++
			}
		}
		total += free + max(repo.MaxCopies-live, 0)
	}
	return total, nil
}

// pool is the repository's copies as the Pool the slot code provisions and
// releases copy n with: its env rendered for copy n (copyEnv), so the
// setup and the release's commands see the copy's name and Redis offset.
func (c *copySet) pool(repo config.FactoryRepo, n int) config.Pool {
	p := repo.Pool()
	p.Env = copyEnv(repo, n)
	return p
}

// copyEnv is copy n's environment: the repository's env with {copy} and
// {slot} as the copy's name, {n} as its number and {n*16} as its Redis
// offset (n × RedisDBsPerCopy), and REDIS_DB_OFFSET, when the env names it,
// that offset whatever the env's value.
func copyEnv(repo config.FactoryRepo, n int) map[string]string {
	name := repo.Pool().Slot(n)
	offset := strconv.Itoa(n * RedisDBsPerCopy)
	r := strings.NewReplacer("{copy}", name, "{slot}", name, "{n*16}", offset, "{n}", strconv.Itoa(n))
	env := make(map[string]string, len(repo.Env))
	for k, v := range repo.Env {
		env[k] = r.Replace(v)
	}
	if _, ok := env[RedisOffsetEnv]; ok {
		env[RedisOffsetEnv] = offset
	}
	return env
}

// copyOf is slot sl, copy n of repo, as a Copy: its port is the copy's
// PORT when its env names one.
func copyOf(repo config.FactoryRepo, sl store.Slot, n int) Copy {
	env := copyEnv(repo, n)
	port, _ := strconv.Atoi(env["PORT"])
	return Copy{Name: sl.Name, Path: sl.Path, Number: n, Port: port, Env: env}
}

// copyNumber is n such that repo's copy_name renders name for copy n.
func copyNumber(repo config.FactoryRepo, name string) (int, bool) {
	pre, _, ok := strings.Cut(repo.CopyName, "{n}")
	if !ok {
		return 0, false
	}
	digits, ok := strings.CutPrefix(name, pre)
	if !ok {
		return 0, false
	}
	if i := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = digits[:i]
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 || repo.Pool().Slot(n) != name {
		return 0, false
	}
	return n, true
}

// LastCopyMade is the highest number of repo's copies among names, the
// names of its pool slots in the factory's agents store (0 for none): the
// copies its claims made. A lower max_copies or fix_copies retires none of
// them, and a task's claim takes one numbered past both as its own
// (ownsCopy), so its Redis offset still counts.
func LastCopyMade(repo config.FactoryRepo, names []string) int {
	last := 0
	for _, name := range names {
		if n, ok := copyNumber(repo, name); ok {
			last = max(last, n)
		}
	}
	return last
}

// repoOf is the repository copy name is a copy of, and its number.
func (c *copySet) repoOf(name string) (config.FactoryRepo, int, bool) {
	for _, r := range c.cfg.Factory.Repos {
		if n, ok := copyNumber(r, name); ok {
			return r, n, true
		}
	}
	return config.FactoryRepo{}, 0, false
}

// event records an event of copy name (subject slot:<name>, as magnum's slot
// code records its own) in the agents store, redacted. It is the record of a
// step that happened: a failure to write it changes nothing of the step's
// result.
func (c *copySet) event(ctx context.Context, name, level, kind, msg string) {
	subject := "slot:" + name
	_, _ = c.st.AppendEvent(context.WithoutCancel(ctx), store.Event{Level: level, Subject: &subject, Kind: kind,
		Message: scrub.Text(msg)})
}

// phase records the begin, ok or fail event of a destructive step of copy
// name (a server's or a browser's start and stop, a repair) as event does;
// a failure is a warning.
func (c *copySet) phase(ctx context.Context, name, kind, phase, msg string) {
	level := "info"
	if phase == store.PhaseFail {
		level = "warn"
	}
	c.phaseAt(ctx, name, kind, phase, level, msg)
}

// phaseAt is phase at level.
func (c *copySet) phaseAt(ctx context.Context, name, kind, phase, level, msg string) {
	subject := "slot:" + name
	_, _ = c.st.AppendEvent(context.WithoutCancel(ctx), store.Event{Level: level, Subject: &subject, Kind: kind, Phase: &phase,
		Message: scrub.Text(msg)})
}
