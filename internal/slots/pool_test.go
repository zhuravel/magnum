package slots

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
)

func TestProvisionPool(t *testing.T) {
	h := newHarness(t)
	h.repo()
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); err != nil {
		t.Fatalf("ProvisionPool: %v", err)
	}
	sl := h.slot("review1")
	path := h.pool.Path(1)
	if sl.State != store.SlotFree || sl.Kind != store.SlotKindPool || sl.Path != path || sl.MainClone != h.main {
		t.Fatalf("slot = %+v", sl)
	}
	if store.Deref(sl.PlaceholderBranch) != "review1" || store.Deref(sl.DBSlug) != "review1" || sl.RepoID == nil {
		t.Fatalf("placeholder=%v slug=%v repo=%v", store.Deref(sl.PlaceholderBranch), store.Deref(sl.DBSlug), sl.RepoID)
	}
	if sl.LockSHA == nil || *sl.LockSHA == "" || sl.LastError != nil {
		t.Fatalf("lock_sha=%v last_error=%v", sl.LockSHA, store.Deref(sl.LastError))
	}

	// Worktree on the placeholder branch at origin/master, without upstream.
	if b := gitT(t, path, "branch", "--show-current"); b != "review1" {
		t.Fatalf("branch = %q", b)
	}
	if head := gitT(t, path, "rev-parse", "HEAD"); head != h.shaMaster {
		t.Fatalf("HEAD = %s, want %s", head, h.shaMaster)
	}
	add := h.run.gitCalls("worktree", "add")
	if len(add) != 1 || !add[0].Mutates {
		t.Fatalf("worktree add calls = %v", add)
	}
	if want := []string{"-C", h.main, "worktree", "add", "--quiet", "--no-track", "-b", "review1", path, "origin/master"}; !slices.Equal(add[0].Args, want) {
		t.Fatalf("worktree add argv = %q, want %q", add[0].Args, want)
	}

	// .mise.local.toml rendered: secret stripped, WT_BRANCH set; then trusted.
	mise := readFile(t, filepath.Join(path, ".mise.local.toml"))
	if strings.Contains(mise, "GITHUB_PERSONAL_ACCESS_TOKEN") || !strings.Contains(mise, `WT_BRANCH = "review1"`) ||
		!strings.Contains(mise, `FAKE_AWS = "1"`) {
		t.Fatalf(".mise.local.toml =\n%s", mise)
	}
	if fi, err := os.Stat(filepath.Join(path, ".mise.local.toml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mise file mode: %v %v", fi, err)
	}
	trust := h.fake.CallsWithPrefix("mise", "-C", path, "trust")
	if len(trust) != 1 || !trust[0].Mutates || !slices.Equal(trust[0].Args, []string{"-C", path, "trust", filepath.Join(path, ".mise.local.toml")}) {
		t.Fatalf("trust calls = %+v", trust)
	}

	// Setup through mise exec with the slot env, 45m, Mutates, logged.
	setup := h.scriptCalls("bin/worktree-setup")
	if len(setup) != 1 {
		t.Fatalf("setup calls = %d", len(setup))
	}
	c := setup[0].Cmd
	wantArgs := []string{"-C", path, "exec", "--", "env", "CONDUCTOR_WORKSPACE_NAME=", "WT_BRANCH=review1", "/bin/sh", "-c", "bin/worktree-setup"}
	if c.Name != "mise" || !slices.Equal(c.Args, wantArgs) {
		t.Fatalf("setup argv = %s %q, want mise %q", c.Name, c.Args, wantArgs)
	}
	if c.Timeout != SetupTimeout || SetupTimeout != 45*time.Minute || !c.Mutates || c.Dir != path || c.Label == "" {
		t.Fatalf("setup cmd = %+v", c)
	}
	logText := readFile(t, filepath.Join(h.layout.Logs(), "provision-review1.log"))
	if !strings.Contains(logText, "setup ok") || strings.Contains(logText, "ghp_") || !strings.Contains(logText, "bin/worktree-setup") {
		t.Fatalf("provision log =\n%s", logText)
	}

	// Databases recorded for the slot.
	dbs, err := h.st.ListSlotDatabases(h.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range dbs {
		if d.SlotID == nil || *d.SlotID != sl.ID {
			t.Fatalf("db %s slot_id = %v", d.DBName, d.SlotID)
		}
		names = append(names, d.DBName)
	}
	if !slices.Equal(names, []string{"talkable_development__review1", "talkable_test__review1"}) {
		t.Fatalf("slot databases = %v", names)
	}

	// Provisioning an already free slot is a no-op.
	h.run.reset()
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); err != nil {
		t.Fatal(err)
	}
	if len(h.run.calls) != 0 {
		t.Fatalf("re-provision ran commands: %v", h.run.calls)
	}
}

func TestProvisionPoolWritesRowFirstAndResumes(t *testing.T) {
	h := newHarness(t)
	h.failScript["bin/worktree-setup"] = 1
	err := h.m.ProvisionPool(h.ctx, h.pool, 2)
	if err == nil {
		t.Fatal("ProvisionPool succeeded with a failing setup")
	}
	sl := h.slot("review2")
	if sl.State != store.SlotProvisioning || sl.LastError == nil || strings.Contains(*sl.LastError, "ghp_") {
		t.Fatalf("after failure: state=%s last_error=%v", sl.State, store.Deref(sl.LastError))
	}
	if done, _ := steps.Done(h.ctx, h.st, "slot:review2", "worktree_add"); !done {
		t.Fatal("worktree_add not recorded as done")
	}

	// Retry resumes: no second worktree add, setup runs again, slot ends free.
	h.run.reset()
	if err := h.m.ProvisionPool(h.ctx, h.pool, 2); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(h.run.gitCalls("worktree", "add")); n != 0 {
		t.Fatalf("resume re-ran worktree add (%d)", n)
	}
	if n := len(h.scriptCalls("bin/worktree-setup")); n != 2 {
		t.Fatalf("setup ran %d times, want 2", n)
	}
	if got := h.slot("review2"); got.State != store.SlotFree || got.LastError != nil {
		t.Fatalf("after resume: %s %v", got.State, store.Deref(got.LastError))
	}
}

func TestProvisionPoolCrashResume(t *testing.T) {
	h := newHarness(t)
	crash := errors.New("crash")
	ctx := steps.WithFailpoint(h.ctx, func(subject, name string, at steps.Point) error {
		if name == "setup" && at == steps.AfterRun {
			return crash
		}
		return nil
	})
	if err := h.m.ProvisionPool(ctx, h.pool, 1); !errors.Is(err, crash) {
		t.Fatalf("err = %v, want crash", err)
	}
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); err != nil {
		t.Fatal(err)
	}
	// setup had no ok row, so it ran again (at-least-once); everything else once.
	if n := len(h.scriptCalls("bin/worktree-setup")); n != 2 {
		t.Fatalf("setup ran %d times", n)
	}
	if n := len(h.run.gitCalls("worktree", "add")); n != 1 {
		t.Fatalf("worktree add ran %d times", n)
	}
	if got := h.slot("review1"); got.State != store.SlotFree {
		t.Fatalf("state = %s", got.State)
	}
}

func TestProvisionPoolVerifyFailuresBreakSlot(t *testing.T) {
	t.Run("marker", func(t *testing.T) {
		h := newHarness(t)
		h.setupSlug = func(string) string { return "repo1" }
		if err := h.m.ProvisionPool(h.ctx, h.pool, 1); !errors.Is(err, ErrVerify) {
			t.Fatalf("err = %v, want ErrVerify", err)
		}
		if got := h.slot("review1"); got.State != store.SlotBroken || got.LastError == nil {
			t.Fatalf("state=%s last_error=%v", got.State, store.Deref(got.LastError))
		}
	})
	t.Run("databases", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Rules = append([]execx.Rule{{Prefix: []string{"mise", "-C", h.pool.Path(1), "exec"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			writeFile(t, filepath.Join(h.pool.Path(1), "tmp", ".worktree-db-slug"), "review1")
			return execx.Result{}, nil // creates no databases
		}}}, h.fake.Rules...)
		if err := h.m.ProvisionPool(h.ctx, h.pool, 1); !errors.Is(err, ErrVerify) {
			t.Fatalf("err = %v, want ErrVerify", err)
		}
		if got := h.slot("review1"); got.State != store.SlotBroken {
			t.Fatalf("state = %s", got.State)
		}
	})
}

func TestProvisionPoolExistingPlaceholderBranch(t *testing.T) {
	h := newHarness(t)
	gitT(t, h.main, "branch", "review1", h.shaMaster)
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); err != nil {
		t.Fatal(err)
	}
	path := h.pool.Path(1)
	add := h.run.gitCalls("worktree", "add")
	if len(add) != 1 || !slices.Contains(add[0].Args, "--detach") {
		t.Fatalf("worktree add = %v", add)
	}
	if b := gitT(t, path, "branch", "--show-current"); b != "review1" {
		t.Fatalf("branch = %q", b)
	}
}

func TestProvisionPoolLowDisk(t *testing.T) {
	h := newHarness(t)
	h.freeDisk = 3 << 30
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); !errors.Is(err, ErrLowDisk) {
		t.Fatalf("err = %v, want ErrLowDisk", err)
	}
	if n := len(h.run.gitCalls("worktree", "add")); n != 0 {
		t.Fatal("worktree added despite low disk")
	}
}

func TestNextSlotNumber(t *testing.T) {
	h := newHarness(t)
	n, err := h.m.NextSlotNumber(h.ctx, h.pool)
	if err != nil || n != 1 {
		t.Fatalf("NextSlotNumber = %d, %v", n, err)
	}
	h.provisioned(1)
	if err := os.MkdirAll(h.pool.Path(2), 0o755); err != nil { // unregistered leftover dir
		t.Fatal(err)
	}
	if n, _ := h.m.NextSlotNumber(h.ctx, h.pool); n != 3 {
		t.Fatalf("NextSlotNumber = %d, want 3", n)
	}
}

// TestHeavyOperationsAreSerialized blocks the first setup script until the
// two other provisions are parked on the heavy-command lock (seen in their
// goroutines' stacks), then checks that no second script started meanwhile.
func TestHeavyOperationsAreSerialized(t *testing.T) {
	h := newHarness(t)
	var (
		mu      sync.Mutex
		running int
		peak    int
		started []string // slot of every setup, in start order
	)
	first := make(chan string, 1)
	release := make(chan struct{})
	h.fake.Rules = append([]execx.Rule{{Prefix: []string{"mise"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		mc, ok := parseMiseExec(c)
		if !ok || mc.Script != "bin/worktree-setup" {
			return h.mise(c)
		}
		mu.Lock()
		running++
		peak = max(peak, running)
		started = append(started, mc.Env["WT_BRANCH"])
		isFirst := len(started) == 1
		mu.Unlock()
		defer func() {
			mu.Lock()
			running--
			mu.Unlock()
		}()
		if isFirst {
			first <- mc.Env["WT_BRANCH"]
			<-release
		}
		return h.mise(c)
	}}}, h.fake.Rules...)
	type atSetup struct {
		subject string
		gid     int64
	}
	setups := make(chan atSetup, 3)
	ctx := steps.WithFailpoint(h.ctx, func(subject, name string, at steps.Point) error {
		if name == "setup" && at == steps.BeforeRun {
			setups <- atSetup{subject, goroutineID()}
		}
		return nil
	})

	var wg sync.WaitGroup
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer func() { unblock(); wg.Wait() }() // also on a failure below
	errs := make([]error, 3)
	for i := range 3 {
		wg.Go(func() { errs[i] = h.m.ProvisionPool(ctx, h.pool, i+1) })
	}
	var running1 string
	select {
	case running1 = <-first:
	case <-time.After(gitGuard):
		t.Fatal("no setup started")
	}
	var others []int64
	for len(others) < 2 {
		select {
		case s := <-setups:
			if s.subject != "slot:"+running1 {
				others = append(others, s.gid)
			}
		case <-time.After(gitGuard):
			t.Fatalf("only %d other provisions reached their setup step", len(others))
		}
	}
	startedNow := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(started)
	}
	deadline := time.Now().Add(gitGuard)
	for !waitingOnHeavyLock(others...) {
		if n := startedNow(); n != 1 {
			t.Fatalf("%d setups started while %s's was running", n, running1)
		}
		if time.Now().After(deadline) {
			t.Fatal("the other provisions never waited on the heavy-command lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := startedNow(); n != 1 {
		t.Fatalf("%d setups started while %s's was running", n, running1)
	}
	unblock()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("provision %d: %v", i+1, err)
		}
	}
	if peak != 1 || len(started) != 3 {
		t.Fatalf("peak concurrent setups = %d (of %d), want 1", peak, len(started))
	}
}

// goroutineID is the id of the calling goroutine, from its stack header.
func goroutineID() int64 {
	var buf [64]byte
	b := bytes.TrimPrefix(buf[:runtime.Stack(buf[:], false)], []byte("goroutine "))
	id, _, _ := bytes.Cut(b, []byte(" "))
	n, err := strconv.ParseInt(string(id), 10, 64)
	if err != nil {
		panic("goroutineID: " + string(buf[:]))
	}
	return n
}

// waitingOnHeavyLock reports whether every goroutine in gids is blocked in
// Manager.acquireHeavy's select.
func waitingOnHeavyLock(gids ...int64) bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	blocked := map[int64]bool{}
	for _, g := range strings.Split(string(buf), "\n\n") {
		for _, id := range gids {
			if strings.HasPrefix(g, fmt.Sprintf("goroutine %d [select", id)) && strings.Contains(g, ".(*Manager).acquireHeavy(") {
				blocked[id] = true
			}
		}
	}
	return len(blocked) == len(gids)
}

// TestHeavyLockHonorsContext holds the heavy-command lock itself, so the
// only way out of a wait for it is the context.
func TestHeavyLockHonorsContext(t *testing.T) {
	h := newHarness(t)
	release, err := h.m.acquireHeavy(h.ctx) // another heavy command is running
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(h.ctx, 20*time.Millisecond)
	defer cancel()
	err = h.m.runHeavy(ctx, h.root, nil, "bin/worktree-setup", "setup x", "", SetupTimeout)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "heavy-command lock") {
		t.Fatalf("err = %v, want a deadline at the lock", err)
	}
	if n := len(h.scriptCalls("")); n != 0 {
		t.Fatalf("ran %d commands while the lock was held", n)
	}

	// A provision cancelled while it waits for the lock (the context is
	// cancelled as its setup step begins) stays provisioning (resumable),
	// not broken.
	ctx2, cancel2 := context.WithCancel(h.ctx)
	defer cancel2()
	ctx2 = steps.WithFailpoint(ctx2, func(subject, name string, at steps.Point) error {
		if name == "setup" && at == steps.BeforeRun {
			cancel2()
		}
		return nil
	})
	err = h.m.ProvisionPool(ctx2, h.pool, 1)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "heavy-command lock") {
		t.Fatalf("ProvisionPool err = %v, want cancelled at the lock", err)
	}
	if n := len(h.scriptCalls("")); n != 0 {
		t.Fatalf("ran %d commands while the lock was held", n)
	}
	if got := h.slot("review1"); got.State != store.SlotProvisioning {
		t.Fatalf("cancelled provision state = %s, want provisioning", got.State)
	}
	if done, err := steps.Done(h.ctx, h.st, "slot:review1", "setup"); err != nil || done {
		t.Fatalf("setup done = %v, %v; want it pending", done, err)
	}

	// Once the lock is free the provision resumes and completes.
	release()
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := h.slot("review1"); got.State != store.SlotFree {
		t.Fatalf("state = %s", got.State)
	}
	if n := len(h.scriptCalls("bin/worktree-setup")); n != 1 {
		t.Fatalf("setup ran %d times, want 1", n)
	}
}

func TestRepair(t *testing.T) {
	h := newHarness(t)
	h.setupSlug = func(string) string { return "wrong" }
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); !errors.Is(err, ErrVerify) {
		t.Fatalf("err = %v", err)
	}
	h.setupSlug = nil
	sl := h.slot("review1")
	if err := h.m.Repair(h.ctx, sl, h.pool); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if got := h.slot("review1"); got.State != store.SlotFree || got.LastError != nil {
		t.Fatalf("after repair: %s %v", got.State, store.Deref(got.LastError))
	}
	if n := len(h.scriptCalls("bin/worktree-setup")); n != 2 {
		t.Fatalf("setup ran %d times, want 2", n)
	}
	if n := len(h.run.gitCalls("worktree", "add")); n != 1 {
		t.Fatalf("repair re-added the worktree")
	}
}

func TestRemove(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	path := sl.Path
	h.run.reset()
	h.clearScripts()

	if err := h.m.Remove(h.ctx, sl, h.pool, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Teardown via mise exec with the slot env.
	td := h.scriptCalls("bin/worktree-archive")
	if len(td) != 1 || td[0].Env["WT_BRANCH"] != "review1" || !td[0].Cmd.Mutates || td[0].Cmd.Timeout != TeardownTimeout {
		t.Fatalf("teardown = %+v", td)
	}
	// The archive script left talkable_test__review1 behind; magnum dropped it with the pool guard.
	if !slices.Equal(h.my.drops, []string{"talkable_test__review1"}) {
		t.Fatalf("drops = %v", h.my.drops)
	}
	g := h.my.guards[0]
	if g.Prefix != "talkable_" || g.AllowRegexp == nil || g.AllowRegexp.String() != "^review[0-9]+$" {
		t.Fatalf("guard = %+v", g)
	}
	if h.my.has("talkable_development__review1") || h.my.has("talkable_test__review1") {
		t.Fatal("databases left behind")
	}
	dbs, _ := h.st.ListSlotDatabases(h.ctx, false)
	if len(dbs) != 0 {
		t.Fatalf("slot_databases not marked dropped: %+v", dbs)
	}
	// Worktree, placeholder branch and admin entry gone.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("slot dir still exists: %v", err)
	}
	rm := h.run.gitCalls("worktree", "remove")
	if len(rm) != 1 || !rm[0].Mutates || slices.Contains(rm[0].Args, "--force") { // clean (ignored files only)
		t.Fatalf("worktree remove = %v", rm)
	}
	if out := gitT(t, h.main, "branch", "--list", "review1"); out != "" {
		t.Fatalf("placeholder branch still exists: %q", out)
	}
	if len(h.run.gitCalls("worktree", "prune")) != 1 {
		t.Fatal("no worktree prune")
	}
	got := h.slot("review1")
	if got.State != store.SlotRemoved {
		t.Fatalf("state = %s", got.State)
	}
	// Removing again is a no-op; re-provisioning reuses the row.
	if err := h.m.Remove(h.ctx, got, h.pool, false); err != nil {
		t.Fatal(err)
	}
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	if again := h.slot("review1"); again.State != store.SlotFree || again.ID != got.ID {
		t.Fatalf("re-provisioned slot = %+v", again)
	}
}

func TestRemoveRefusals(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	if err := h.m.Remove(h.ctx, sl, h.pool, false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Remove of a claimed slot: err = %v, want ErrConflict", err)
	}
	// Changes a human (here their agent) made block a non-forced remove of a
	// free slot.
	if err := h.m.Release(h.ctx, sl, h.pool, "test"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, "README"), "dirty\n")
	h.foreignAgent(sl.Path)
	err := h.m.Remove(h.ctx, h.slot(sl.Name), h.pool, false)
	wantHold(t, err, HoldDirtyWorktree)
	if got := h.slot(sl.Name); got.State != store.SlotFree {
		t.Fatalf("state = %s, want free (checked before teardown)", got.State)
	}
	if len(h.scriptCalls("bin/worktree-archive")) != 0 {
		t.Fatal("teardown ran before the dirty check")
	}
	// force discards it.
	if err := h.m.Remove(h.ctx, h.slot(sl.Name), h.pool, true); err != nil {
		t.Fatalf("forced Remove: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoved {
		t.Fatalf("state = %s", got.State)
	}
}

func TestRemoveLostSlotSkipsTeardown(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	if err := os.RemoveAll(sl.Path); err != nil {
		t.Fatal(err)
	}
	h.clearScripts()
	if err := h.m.Remove(h.ctx, sl, h.pool, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if n := len(h.scriptCalls("")); n != 0 {
		t.Fatalf("ran %d mise commands for a missing dir", n)
	}
	if len(h.my.drops) != 2 {
		t.Fatalf("drops = %v, want both databases dropped by magnum", h.my.drops)
	}
	if got := h.slot("review1"); got.State != store.SlotRemoved {
		t.Fatalf("state = %s", got.State)
	}
}

func TestAdopt(t *testing.T) {
	h := newHarness(t)
	path := h.pool.Path(3)
	gitT(t, h.main, "worktree", "add", "--quiet", "--no-track", "-b", "review3", path, "origin/master")
	writeFile(t, filepath.Join(path, "tmp", ".worktree-db-slug"), "review3")

	if _, err := h.m.Adopt(h.ctx, h.pool, path); !errors.Is(err, ErrVerify) {
		t.Fatalf("Adopt without databases: err = %v, want ErrVerify", err)
	}
	h.my.add("20260914085954", h.pool.DBNames("review3")...)
	sl, err := h.m.Adopt(h.ctx, h.pool, path)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if sl.Name != "review3" || sl.State != store.SlotFree || sl.Kind != store.SlotKindPool || store.Deref(sl.DBSlug) != "review3" {
		t.Fatalf("adopted = %+v", sl)
	}
	if _, err := h.m.Adopt(h.ctx, h.pool, path); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second Adopt: err = %v, want ErrConflict", err)
	}

	other := filepath.Join(h.root, "elsewhere")
	if _, err := h.m.Adopt(h.ctx, h.pool, other); err == nil {
		t.Fatal("Adopt accepted a path outside the pool template")
	}
	// Marker mismatch.
	path4 := h.pool.Path(4)
	gitT(t, h.main, "worktree", "add", "--quiet", "--detach", path4, "origin/master")
	writeFile(t, filepath.Join(path4, "tmp", ".worktree-db-slug"), "repo4")
	h.my.add("1", h.pool.DBNames("review4")...)
	if _, err := h.m.Adopt(h.ctx, h.pool, path4); !errors.Is(err, ErrVerify) {
		t.Fatalf("Adopt with a wrong marker: err = %v", err)
	}
}

func TestPoolGuard(t *testing.T) {
	h := newHarness(t)
	g := DropGuard(h.pool)
	for name, ok := range map[string]bool{
		"talkable_test__review12":       true,
		"talkable_development__review1": true,
		"talkable_test__repo1":          false,
		"talkable_test__review":         false,
		"talkable_test":                 false,
		"other_test__review1":           false,
	} {
		err := g.Check(name)
		if (err == nil) != ok {
			t.Errorf("Check(%s) = %v, want ok=%v", name, err, ok)
		}
		if err != nil && !errors.Is(err, mysqlx.ErrGuard) {
			t.Errorf("Check(%s) err = %v, want ErrGuard", name, err)
		}
	}
}

func TestDryRunHasNoSideEffects(t *testing.T) {
	h := newHarness(t)
	m := h.newManager(Deps{DryRun: true})
	if err := m.ProvisionPool(h.ctx, h.pool, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.SlotByName(h.ctx, "review1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dry-run created a slot row: %v", err)
	}
	if len(h.run.calls) != 0 {
		t.Fatalf("dry-run ran %v", h.run.calls)
	}
}
