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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
)

// RedisDBsPerCopy is how many Redis databases a copy has: copy n's start at
// n × RedisDBsPerCopy (REDIS_DB_OFFSET).
const RedisDBsPerCopy = 16

// RedisOffsetEnv is the variable a copy's Redis configuration reads its
// first database from. A repository whose env names it gets each copy's own
// offset there, whatever value the env gives it.
const RedisOffsetEnv = "REDIS_DB_OFFSET"

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
	owner func(context.Context, registry.Task) (store.PR, error)

	// mu makes claims and releases one at a time: a claim counts the copies
	// and adds the next one; srv guards the servers' records.
	mu, srv sync.Mutex

	// Seams the tests replace: the process table, the signal to a server's
	// process group, the start of a server, the wait between a stop's looks
	// and the clock.
	processes func(ctx context.Context) ([]execx.Process, error)
	kill      func(pgid int, sig syscall.Signal) error
	spawn     func(ctx context.Context, dir string, env map[string]string, command, log string) (int, error)
	sleep     func(ctx context.Context, d time.Duration) error
	now       func() time.Time
}

// The copies the daemon wires are the engine's.
var _ Copies = (*copySet)(nil)

// NewCopies returns the factory's copies: sm is magnum's slot manager over
// st, the factory's agents store (Layout.FactoryAgentsDB, magnum's schema),
// git and run run the claim protocol's commands, my reads a copy's
// development database (nil: no reset_db at a claim), and owner is the
// runtime's Owner, the task's row in st a copy is claimed for. A claim adds
// copy n on demand with sm.ProvisionPool (FactoryRepo.Pool with the copy's
// env), up to max_copies.
func NewCopies(cfg *config.Config, sm *slots.Manager, st *store.Store, git *gitx.Client, run execx.Runner, my slots.MySQL,
	owner func(context.Context, registry.Task) (store.PR, error)) Copies {
	c := &copySet{cfg: cfg, slots: sm, st: st, git: git, run: run, my: my, owner: owner,
		kill: func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) },
		now:  time.Now}
	c.processes = func(ctx context.Context) ([]execx.Process, error) { return execx.ProcessTable(ctx, run) }
	c.spawn = c.spawnServer
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

// Claim hands task t a copy of its repository for its owner row: the copy
// it holds, else a free one, the copy it used last first (slots.Reserve:
// the owner row's state stays the runtime's), else a new one while the
// repository has fewer than max_copies (provision). None left is ErrNoCopy,
// "copy: all 3 busy". The claim protocol runs once per claim (prepare).
func (c *copySet) Claim(ctx context.Context, t registry.Task) (Copy, error) {
	repo, ok := c.cfg.FactoryRepo(t.Repo)
	if !ok {
		return Copy{}, fmt.Errorf("claim a copy for %s: no [[factory.repo]] for %q", TaskKey(t), t.Repo)
	}
	pr, err := c.owner(ctx, t)
	if err != nil {
		return Copy{}, fmt.Errorf("claim a copy for %s: %w", TaskKey(t), err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sl, err := c.reserve(ctx, repo, pr)
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
	if err := c.prepare(ctx, t, repo, sl, cp); err != nil {
		return Copy{}, fmt.Errorf("claim %s for %s: %w", cp.Name, TaskKey(t), err)
	}
	return cp, nil
}

// reserve is the slot of the copy task row pr gets: the one it holds or a
// free one (slots.Reserve), else a copy added for it (nextCopy, provision).
// A release a crash cut short ends first: the row keeps that copy until its
// release is done, and no other one can be its.
func (c *copySet) reserve(ctx context.Context, repo config.FactoryRepo, pr store.PR) (store.Slot, error) {
	if sl, err := c.st.SlotByPR(ctx, pr.ID); err == nil && slices.Contains([]string{store.SlotReleasing, store.SlotDirtySchema}, sl.State) {
		n, ok := copyNumber(repo, sl.Name)
		if !ok {
			return store.Slot{}, fmt.Errorf("%s is no copy of %s", sl.Name, repo.Repo)
		}
		if err := c.slots.Release(ctx, sl, c.pool(repo, n), "released"); err != nil {
			return store.Slot{}, fmt.Errorf("end the release of %s: %w", sl.Name, err)
		}
	}
	// Each pass adds a copy or says why none: max_copies bounds them.
	for range repo.MaxCopies + 2 {
		sl, err := c.slots.Reserve(ctx, pr, repo.Pool())
		if !errors.Is(err, slots.ErrNoFreeSlot) {
			return sl, err
		}
		n, wait, err := c.nextCopy(ctx, repo)
		if err != nil {
			return store.Slot{}, err
		}
		if wait != "" {
			return store.Slot{}, noCopy{what: wait}
		}
		if err := c.provision(ctx, repo, n); err != nil {
			return store.Slot{}, err
		}
	}
	return store.Slot{}, fmt.Errorf("no copy of %s became free after adding %d", repo.Repo, repo.MaxCopies)
}

// nextCopy is the number of the copy a claim adds: one a crash left
// provisioning first, else the lowest number free up to max_copies; wait
// says why there is none ("all 3 busy", "2 busy, 1 broken"). A broken copy
// counts toward max_copies and is never provisioned again by a claim (only
// a repair brings it back).
func (c *copySet) nextCopy(ctx context.Context, repo config.FactoryRepo) (n int, wait string, err error) {
	list, err := c.st.ListSlots(ctx, store.SlotFilter{RepoFullName: repo.Repo, Kind: store.SlotKindPool})
	if err != nil {
		return 0, "", err
	}
	live, broken := 0, 0
	names := map[string]bool{}
	for _, sl := range list {
		switch sl.State {
		case store.SlotRemoved:
			continue
		case store.SlotProvisioning:
			if n, ok := copyNumber(repo, sl.Name); ok {
				return n, "", nil
			}
		case store.SlotBroken:
			broken++
		}
		live++
		names[sl.Name] = true
	}
	pool := repo.Pool()
	if live < repo.MaxCopies {
		if n, err = c.slots.NextSlotNumber(ctx, pool); err != nil {
			return 0, "", err
		}
		if n <= repo.MaxCopies {
			return n, "", nil
		}
	}
	busy := live - broken
	wait = fmt.Sprintf("%d busy", busy)
	if busy >= repo.MaxCopies {
		wait = fmt.Sprintf("all %d busy", busy)
	}
	if broken > 0 {
		wait += fmt.Sprintf(", %d broken", broken)
	}
	for n := 1; live < repo.MaxCopies && n <= repo.MaxCopies; n++ {
		if !names[pool.Slot(n)] {
			// NextSlotNumber skipped it: its path exists, and no copy is there.
			wait += fmt.Sprintf(", and %s is a directory that is no copy", pool.Path(n))
			break
		}
	}
	return 0, wait, nil
}

// provision adds copy n (slots.ProvisionPool with the copy's env). A failed
// setup (the setup step or its verification: setupFailed) runs once more;
// a second failure marks the copy broken (markBroken), and the claim goes on
// to another copy. Too little disk is a wait; any other failure (a fetch
// that cannot reach origin) is the claim's error, and the copy stays
// provisioning for the next claim.
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
			return c.markBroken(ctx, name, err)
		}
		err = c.slots.ProvisionPool(ctx, pool, n)
	}
	return nil
}

// setupFailed reports whether err, a failed provisioning of copy name, is
// its setup's: the setup step failed after the worktree was added, or the
// verification of what the setup makes (the slug marker, the databases).
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
	return derr == nil && !setup
}

// markBroken moves copy name to broken with why its setup failed twice
// (a verification failure has moved it there already) and records a
// factory.copy_broken event; it returns nil, so the claim goes on.
func (c *copySet) markBroken(ctx context.Context, name string, cause error) error {
	sl, err := c.st.SlotByName(ctx, name)
	if err != nil {
		return err
	}
	why := execx.Redact("setup failed twice: " + cause.Error())
	if sl.State == store.SlotProvisioning {
		if err := c.st.TransitionSlot(context.WithoutCancel(ctx), sl.ID, []string{store.SlotProvisioning}, store.SlotBroken,
			func(u *store.SlotUpdate) { u.Set("last_error", why) }); err != nil {
			return fmt.Errorf("mark %s broken: %w", name, err)
		}
	}
	c.event(ctx, name, "warn", "factory.copy_broken", why+"; no claim takes it until it is repaired")
	return nil
}

// prepare is the claim protocol, once per claim of the copy (its open
// assignment, recorded under claimKey when done), in order: the task's
// branch checked out when it has one (a later claim of a task that left
// another copy), reset_db when the development database holds a migration
// the task's code lacks, each installs entry whose lockfile differs from the
// copy's last install, and a flush of the copy's Redis databases. A step
// that fails fails the claim, which keeps the copy: the task's next claim
// runs the protocol again.
func (c *copySet) prepare(ctx context.Context, t registry.Task, repo config.FactoryRepo, sl store.Slot, cp Copy) error {
	a, err := c.st.OpenAssignmentBySlot(ctx, sl.ID)
	if err != nil {
		return err
	}
	key, id := claimKey(cp.Name), strconv.FormatInt(a.ID, 10)
	if done, ok, err := c.st.GetKV(ctx, key); err != nil || (ok && done == id) {
		return err
	}
	for _, step := range []func() error{
		func() error { return c.switchToBranch(ctx, t, cp) },
		func() error { return c.resetForeignSchema(ctx, repo, sl, cp) },
		func() error { return c.install(ctx, repo, cp) },
		func() error { return c.flushRedis(ctx, cp) },
	} {
		if err := step(); err != nil {
			return err
		}
	}
	return c.st.SetKV(ctx, key, id)
}

// claimKey is the kv key that holds the assignment whose claim protocol ran
// in copy name.
func claimKey(name string) string { return "factory.copy." + name + ".claim" }

// installKey is the kv key that holds the hash of the lockfile lock copy
// name last installed.
func installKey(name, lock string) string { return "factory.copy." + name + ".install." + lock }

// switchToBranch checks the task's branch out in the copy when the task has
// one and the copy is not on it: the release of the copy it left kept it.
// A task without a branch yet stays on the copy's placeholder, at
// origin/<base>.
func (c *copySet) switchToBranch(ctx context.Context, t registry.Task, cp Copy) error {
	if t.Branch == "" {
		return nil
	}
	if _, err := c.git.RevParse(ctx, cp.Path, "refs/heads/"+t.Branch); errors.Is(err, gitx.ErrNoSuchRef) {
		return nil
	} else if err != nil {
		return err
	}
	if b, err := c.git.CurrentBranch(ctx, cp.Path); err != nil || b == t.Branch {
		return err
	}
	return c.git.Switch(ctx, cp.Path, t.Branch)
}

// resetForeignSchema runs the repository's reset_db in the copy when its
// development database's newest migration (MAX(schema_migrations.version))
// is none the task's code has: neither a file of db/migrate nor the
// version db/schema.rb declares. Another task migrated it. A database at a
// version the code has is kept (the task migrates it further itself).
func (c *copySet) resetForeignSchema(ctx context.Context, repo config.FactoryRepo, sl store.Slot, cp Copy) error {
	db := devDatabase(repo.Pool().DBNames(slots.SlotDBSlug(sl)))
	if c.my == nil || db == "" || len(repo.ResetDB) == 0 {
		return nil
	}
	have, err := c.my.SchemaMigrationsMax(ctx, db)
	switch {
	case errors.Is(err, mysqlx.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("the schema version of %s: %w", db, err)
	case have == "":
		return nil
	}
	known, err := codeVersions(cp.Path)
	if err != nil {
		return err
	}
	if known[have] {
		return nil
	}
	c.event(ctx, cp.Name, "info", "factory.copy_reset_db",
		fmt.Sprintf("%s holds migration %s, which the task's code does not have: reset_db", db, have))
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

// install runs each installs entry of the repository whose lockfile, in the
// copy, differs from the one the copy last installed (installKey; a copy
// that never installed runs them all), and records the lockfile's hash once
// its run passed. A checkout without the lockfile runs nothing for it.
func (c *copySet) install(ctx context.Context, repo config.FactoryRepo, cp Copy) error {
	for _, in := range repo.Installs {
		if !filepath.IsLocal(in.Lock) {
			return fmt.Errorf("installs lock %q is not a path inside the checkout", in.Lock)
		}
		b, err := fsx.ReadRegular(filepath.Join(cp.Path, in.Lock), lockFileMax)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", in.Lock, err)
		}
		h := sha256.Sum256(b)
		sum := hex.EncodeToString(h[:])
		key := installKey(cp.Name, in.Lock)
		if last, ok, err := c.st.GetKV(ctx, key); err != nil || (ok && last == sum) {
			if err != nil {
				return err
			}
			continue
		}
		if err := c.script(ctx, cp, in.Run, slots.DepsTimeout); err != nil {
			return err
		}
		if err := c.st.SetKV(ctx, key, sum); err != nil {
			return err
		}
	}
	return nil
}

// redisFlushTimeout bounds one FLUSHDB.
const redisFlushTimeout = time.Minute

// flushRedis empties the copy's RedisDBsPerCopy Redis databases from its
// offset (`redis-cli -n <db> FLUSHDB`), when its env names one.
func (c *copySet) flushRedis(ctx context.Context, cp Copy) error {
	v, ok := cp.Env[RedisOffsetEnv]
	if !ok {
		return nil
	}
	offset, err := strconv.Atoi(v)
	if err != nil || offset < 0 {
		return fmt.Errorf("%s of %s is %q, no database number", RedisOffsetEnv, cp.Name, v)
	}
	for db := offset; db < offset+RedisDBsPerCopy; db++ {
		n := strconv.Itoa(db)
		if _, err := c.run.Run(ctx, execx.Cmd{Name: "redis-cli", Args: []string{"-n", n, "FLUSHDB"}, Unset: c.unset(),
			Timeout: redisFlushTimeout, Mutates: true, Label: "redis-cli flushdb " + n + " of " + cp.Name}); err != nil {
			return fmt.Errorf("flush Redis database %d of %s: %w", db, cp.Name, err)
		}
	}
	return nil
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
func (c *copySet) unset() []string {
	keys := slices.Concat(gitx.ScrubbedEnv(), slots.DefaultStripEnv)
	for _, id := range c.cfg.Identities {
		if id.PrivateKeyEnv != "" {
			keys = append(keys, id.PrivateKeyEnv)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// Release hands task t's copy back: refused while one of the factory's
// sessions works in it (ErrPaneInCopy), its server stopped, then magnum's
// slot release (slots.Release, its guard first) puts the copy back on its
// placeholder at origin/<base>. The task's branch stays, its commits on it,
// pushed or not: HEAD at the tip of the task's own branch is recorded as the
// factory's checkout first, so the guard takes it for the factory's own work
// (no head drift, no unpushed hold) while it still holds a copy left on
// another branch or on no branch, and a person's changes. A released copy
// leaves handoff.md in the task's directory (writeHandoff). A task that
// holds no copy has nothing to release.
func (c *copySet) Release(ctx context.Context, t registry.Task, reason string) error {
	repo, ok := c.cfg.FactoryRepo(t.Repo)
	if !ok {
		return fmt.Errorf("release the copy of %s: no [[factory.repo]] for %q", TaskKey(t), t.Repo)
	}
	pr, err := c.owner(ctx, t)
	if err != nil {
		return fmt.Errorf("release the copy of %s: %w", TaskKey(t), err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sl, err := c.st.SlotByPR(ctx, pr.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release the copy of %s: %w", TaskKey(t), err)
	}
	n, ok := copyNumber(repo, sl.Name)
	if !ok {
		return fmt.Errorf("release the copy of %s: %s is no copy of %s", TaskKey(t), sl.Name, repo.Repo)
	}
	cp := copyOf(repo, sl, n)
	if err := c.paneIn(ctx, cp); err != nil {
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
// working directory is the copy or below it).
func (c *copySet) paneIn(ctx context.Context, cp Copy) error {
	live, err := c.st.LiveSessions(ctx)
	if err != nil {
		return err
	}
	root := fsx.Canon(cp.Path)
	for _, s := range live {
		cwd := fsx.Canon(store.Deref(s.Cwd))
		if cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator)) {
			return fmt.Errorf("%w: the %s session works in %s", ErrPaneInCopy, s.Role, cwd)
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
	if l.Head, err = c.git.RevParse(ctx, cp.Path, "HEAD"); err != nil {
		return l, err
	}
	if l.Branch, err = c.git.CurrentBranch(ctx, cp.Path); err != nil {
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
	if t.Branch != "" && l.Branch == t.Branch && store.Deref(sl.CheckedOutSHA) != l.Head {
		if err := c.st.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", l.Head) }); err != nil {
			return l, err
		}
	}
	return l, nil
}

// handoffListed caps the changed paths handoff.md names.
const handoffListed = 20

// writeHandoff writes handoff.md in task t's directory: the copy it left
// and why, its branch and head, its server, the changes the release
// discarded, and what the next claimer needs. It replaces the last one.
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
		fmt.Fprintf(&b, "- uncommitted: %d tracked change(s), discarded by the release: %s\n", len(l.Changed), listed)
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

// Free is how many copies a claim could take now, over the factory's
// repositories: the free ones and those max_copies still lets a claim add.
func (c *copySet) Free(ctx context.Context) (int, error) {
	total := 0
	for _, repo := range c.cfg.Factory.Repos {
		list, err := c.st.ListSlots(ctx, store.SlotFilter{RepoFullName: repo.Repo, Kind: store.SlotKindPool})
		if err != nil {
			return 0, err
		}
		live, free := 0, 0
		for _, sl := range list {
			if sl.State == store.SlotRemoved {
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
		Message: execx.Redact(msg)})
}
