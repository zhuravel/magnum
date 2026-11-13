package factory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
)

// fakeFixes is what the fake copies' fix claims and releases do: a claim
// counts itself and answers claimed, or claimErr, after block (when set)
// is closed or its context ends; a release records its PR work and answers
// releaseErr; FixReserved answers reserved.
type fakeFixes struct {
	claims     int
	claimed    FixCheckout
	claimErr   error
	block      chan struct{}
	released   []int64
	releaseErr error
	reserved   []FixReservation
}

func (f *fakeCopies) ClaimFix(ctx context.Context, _ registry.PRWork) (FixCheckout, error) {
	f.mu.Lock()
	f.fix.claims++
	block := f.fix.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return FixCheckout{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fix.claimErr != nil {
		return FixCheckout{}, f.fix.claimErr
	}
	return f.fix.claimed, nil
}

func (f *fakeCopies) ReleaseFix(_ context.Context, w registry.PRWork, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fix.released = append(f.fix.released, w.ID)
	return f.fix.releaseErr
}

func (f *fakeCopies) FixReserved(context.Context) ([]FixReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fix.reserved), nil
}

func (f *fakeCopies) fixClaims() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fix.claims
}

// gitCmd is `git -C dir args…`.
func gitCmd(dir string, args ...string) execx.Cmd {
	return execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)}
}

// newFixRig is the copies rig of talkable/talkable with maxCopies task
// copies and fixCopies fix copies.
func newFixRig(t *testing.T, maxCopies, fixCopies int) *copiesRig {
	t.Helper()
	r := newCopiesRig(t, maxCopies)
	r.cfg.Factory.Repos[0].FixCopies = fixCopies
	return r
}

// pushPR makes PR number's head on origin, a commit on master's tip that
// writes file with content, pushed as refs/pull/<number>/head (a later
// push moves it), and returns that commit.
func (r *copiesRig) pushPR(origin string, number int, file, content string) string {
	r.t.Helper()
	work := filepath.Join(r.root, "author-"+filepath.Base(origin))
	if _, err := os.Stat(work); err != nil {
		r.gitIn(r.root, "clone", "--quiet", "--template=", origin, work)
	}
	branch := "pr" + strconv.Itoa(number)
	if _, err := r.real.Run(r.ctx, gitCmd(work, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)); err != nil {
		r.gitIn(work, "switch", "--quiet", "-c", branch, "origin/master")
	} else {
		r.gitIn(work, "switch", "--quiet", branch)
	}
	r.write(filepath.Join(work, file), content)
	r.gitIn(work, "add", "-A")
	r.gitIn(work, "commit", "--quiet", "-m", "pr "+strconv.Itoa(number))
	r.gitIn(work, "push", "--quiet", "--force", "origin", "HEAD:refs/pull/"+strconv.Itoa(number)+"/head")
	return r.gitIn(work, "rev-parse", "HEAD")
}

// prWork is the queued fix work of PR number of repo, cycle 1.
func (r *copiesRig) prWork(repo string, number int) registry.PRWork {
	r.t.Helper()
	w, err := r.reg.EnsurePRWork(r.ctx, repo, number)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.reg.UpdatePRWork(r.ctx, w.ID, nil, registry.PRWorkQueued, func(u *registry.PRWorkUpdate) { u.Set("cycle", 1) }); err != nil {
		r.t.Fatal(err)
	}
	return r.work(w.ID)
}

// work reads PR work id again.
func (r *copiesRig) work(id int64) registry.PRWork {
	r.t.Helper()
	w, err := r.reg.PRWorkByID(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

// claimFix claims w's fix checkout and fails the test on error.
func (r *copiesRig) claimFix(w registry.PRWork) FixCheckout {
	r.t.Helper()
	r.clock.Add(time.Minute)
	fc, err := r.c.ClaimFix(r.ctx, w)
	if err != nil {
		r.t.Fatalf("ClaimFix for %s: %v", workKey(w), err)
	}
	return fc
}

// releaseFix releases w's fix checkout and fails the test on error.
func (r *copiesRig) releaseFix(w registry.PRWork) {
	r.t.Helper()
	r.clock.Add(time.Minute)
	if err := r.c.ReleaseFix(r.ctx, w, "released"); err != nil {
		r.t.Fatalf("ReleaseFix for %s: %v", workKey(w), err)
	}
}

// head is the commit HEAD is at in dir, and branch the branch it is on
// ("" when detached).
func (r *copiesRig) head(dir string) (head, branch string) {
	r.t.Helper()
	return r.gitIn(dir, "rev-parse", "HEAD"), r.gitIn(dir, "branch", "--show-current")
}

// stepBegins counts the begin rows of step name under the subjects that
// start with prefix.
func (r *copiesRig) stepBegins(prefix, name string) int {
	r.t.Helper()
	evs, err := r.st.EventsMatching(r.ctx, []store.SubjectMatch{{Prefix: prefix}}, 0, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == "step" && store.Deref(ev.Step) == name && store.Deref(ev.Phase) == store.PhaseBegin {
			n++
		}
	}
	return n
}

// ref is the commit ref names in dir ("" when there is none).
func (r *copiesRig) ref(dir, ref string) string {
	r.t.Helper()
	res, err := r.real.Run(r.ctx, gitCmd(dir, "rev-parse", "--verify", "--quiet", ref))
	if err != nil {
		return ""
	}
	return res.Out()
}

// addShop adds example/shop to the rig: a bare origin, its clone under the
// rig's root (which clone discovery finds by its GitHub origin) and a
// [[watch]] whose clone_root is the root, and no [[factory.repo]] or
// [[pool]]. It returns the shop's origin.
func (r *copiesRig) addShop() string {
	r.t.Helper()
	origin, clone := filepath.Join(r.root, "shop-origin.git"), filepath.Join(r.root, "shop")
	work := filepath.Join(r.root, "shop-work")
	for _, d := range []string{origin, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	r.gitIn(origin, "init", "--quiet", "--bare", "--template=", "-b", "master")
	r.gitIn(work, "init", "--quiet", "--template=", "-b", "master")
	r.write(filepath.Join(work, "README"), "shop\n")
	r.write(filepath.Join(work, "db", "schema.rb"), schemaRB(baseSchema))
	r.gitIn(work, "add", "-A")
	r.gitIn(work, "commit", "--quiet", "-m", "init")
	r.gitIn(work, "remote", "add", "origin", origin)
	r.gitIn(work, "push", "--quiet", "origin", "master")
	r.gitIn(r.root, "clone", "--quiet", "--template=", origin, clone)
	r.origins[clone] = "https://github.com/example/shop.git"
	r.cfg.Watches = append(r.cfg.Watches, config.Watch{Owner: "example", Include: []string{"shop"}, CloneRoot: r.root})
	return origin
}

// A fix claims one of its repository's fix copies, numbered past
// max_copies and set up as a task's copy is, and checks the PR's remote
// head out in it, detached, through a ref of the factory's own: the
// review's refs/magnum/pr/N stays as it was. The claim protocol's steps
// run with begin and ok rows, and the PR's phase stays queued. A later
// claim at a newer head switches the copy to it.
func TestAFixClaimsAFixCopyAtThePRsRemoteHead(t *testing.T) {
	r := newFixRig(t, 2, 1)
	head := r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)

	fc := r.claimFix(w)
	if fc.Name != "factory3" || fc.Number != 3 || fc.PerPR() || fc.Path != filepath.Join(r.root, "talkable.factory3") || fc.Head != head {
		t.Fatalf("ClaimFix = %+v, want fix copy factory3 at %s", fc, head)
	}
	if got, branch := r.head(fc.Path); got != head || branch != "" {
		t.Fatalf("the fix copy is at %s on %q, want the PR's head %s, detached", got, branch, head)
	}
	if got := r.ref(r.main, fixRef(7)); got != head {
		t.Fatalf("%s = %q in the main clone, want the PR's head", fixRef(7), got)
	}
	if got := r.ref(r.main, gitx.PRRef(7)); got != "" {
		t.Fatalf("the fix moved the review's %s to %s", gitx.PRRef(7), got)
	}
	setup := r.calls("bin/worktree-setup", fc.Path)
	if len(setup) != 1 || setup[0].Env["WT_BRANCH"] != "factory3" || setup[0].Env["REDIS_DB_OFFSET"] != "48" || fc.Env["PORT"] != "3030" {
		t.Fatalf("the fix copy's setup ran %+v with env %v, want once, as copy 3", setup, fc.Env)
	}
	if n := len(r.calls("bundle install", fc.Path)); n != 1 {
		t.Fatalf("the claim installed %d times, want once: a fix copy is readied as a task's", n)
	}
	prefix := "slot:factory3:fix:"
	for _, name := range []string{"switch", "install Gemfile.lock", "flush_redis"} {
		if n := r.stepBegins(prefix, name); n != 1 {
			t.Errorf("step %s began %d times, want once", name, n)
		}
	}
	if got := r.work(w.ID); got.Phase != registry.PRWorkQueued {
		t.Fatalf("the claim moved the PR's phase to %s; the fix lane moves it", got.Phase)
	}
	res, err := r.c.FixReserved(r.ctx)
	if err != nil || !slices.Equal(res, []FixReservation{{WorkID: w.ID, Checkout: "factory3"}}) {
		t.Fatalf("FixReserved = %+v, %v, want factory3 for the PR", res, err)
	}

	newer := r.pushPR(r.origin, 7, "fix.txt", "v2\n")
	if again := r.claimFix(r.work(w.ID)); again.Name != "factory3" || again.Head != newer {
		t.Fatalf("the claim at the newer head = %+v, want factory3 at %s", again, newer)
	}
	if got, _ := r.head(fc.Path); got != newer {
		t.Fatalf("the fix copy is at %s, want the newer head %s", got, newer)
	}
	if n := len(r.calls("bin/worktree-setup", "")); n != 1 {
		t.Fatalf("the setup ran %d times, want once: the PR keeps its copy", n)
	}
}

// The fix copies are apart from the tasks': a fix takes one while every
// task copy is busy, a task's claim and Free never count one, and a fix
// waits for a fix copy (fix_copies bounds them), never for max_copies. In
// the engine a fix's claim off the loop is no task's claim: a task's
// claim and a queued task's start go on beside it under max_active = 1.
func TestFixesDoNotCountTowardMaxActive(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	r.pushPR(r.origin, 8, "other.txt", "v1\n")
	if n := r.free(); n != 1 {
		t.Fatalf("Free = %d before any claim, want the 1 task copy a claim may add", n)
	}
	fix := r.prWork("talkable/talkable", 7)
	if fc := r.claimFix(fix); fc.Name != "factory2" {
		t.Fatalf("the fix claimed %s, want its fix copy factory2", fc.Name)
	}
	if n := r.free(); n != 1 {
		t.Fatalf("Free = %d while the fix holds its copy, want the 1 task copy still to come", n)
	}
	r.releaseFix(fix)
	task := r.task("PR-1")
	if c := r.claim(task); c.Name != "factory1" {
		t.Fatalf("the task claimed %s, want factory1", c.Name)
	}
	if fc := r.claimFix(r.work(fix.ID)); fc.Name != "factory2" {
		t.Fatalf("the fix claimed %s while every task copy is busy, want its fix copy factory2", fc.Name)
	}
	if n := r.free(); n != 0 {
		t.Fatalf("Free = %d with the task copy busy, want 0: a fix copy is no task's", n)
	}
	r.release(task)
	if n := r.free(); n != 1 {
		t.Fatalf("Free = %d once the task let its copy go, want 1 while the fix holds its own", n)
	}
	_, err := r.c.ClaimFix(r.ctx, r.prWork("talkable/talkable", 8))
	if !errors.Is(err, ErrNoCopy) || !errors.Is(err, ErrHeld) || err.Error() != "copy: all 1 busy" {
		t.Fatalf("a second fix's claim = %v, want a wait for the one fix copy, copy: all 1 busy", err)
	}

	h := newHarness(t)
	h.cfg.Factory.MaxActive = 1
	h.copies.fix.block = make(chan struct{})
	h.copies.fix.claimed = FixCheckout{Name: "factory2", Path: "/copies/factory2", Number: 2}
	fix, err = h.reg.EnsurePRWork(h.ctx, "example/shop", 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.claimFix(h.ctx, fix); !errors.Is(err, ErrHeld) || err.Error() != "copy: preparing a fix checkout (setup <1m)" {
		t.Fatalf("claimFix while its claim runs = %v, want copy: preparing a fix checkout (setup <1m)", err)
	}
	waitFor(t, "the fix's claim to start", func() bool { return h.copies.fixClaims() == 1 })
	h.copies.claimed = Copy{Name: "factory1", Path: "/copies/factory1", Number: 1}
	busy := h.task("PR-2", registry.TaskActive, "investigate")
	if _, err := h.e.claimCopy(h.ctx, busy); !errors.Is(err, ErrHeld) || strings.Contains(err.Error(), "max_active") {
		t.Fatalf("the task's claimCopy beside the fix's claim = %v, want its own claim to run", err)
	}
	waitFor(t, "the task's claim to start beside the fix's", func() bool { return h.copies.claimCount() == 1 })
	close(h.copies.fix.block)
	h.e.waitBackground()
	if c, err := h.e.claimCopy(h.ctx, h.reload(busy.ID)); err != nil || c.Name != "factory1" {
		t.Fatalf("the task's claim = %+v, %v, want factory1", c, err)
	}
	if fc, err := h.e.claimFix(h.ctx, fix); err != nil || fc.Name != "factory2" {
		t.Fatalf("claimFix once its claim ended = %+v, %v, want factory2", fc, err)
	}
	if who, what := h.e.startGate(memoryGate{}, []registry.Task{h.reload(busy.ID)}, 0); who != waitCopy || what != "all 1 busy" {
		t.Fatalf("the start gate with the task at work and the fix holding its checkout = %q %q, want copy: all 1 busy for the task alone", who, what)
	}
	if who, _ := h.e.startGate(memoryGate{}, nil, 0); who != "" {
		t.Fatalf("the start gate with only the fix at work holds a task (%q): the fix counts toward max_active", who)
	}
}

// A repository with a [[watch]] and no [[factory.repo]] gets a per-PR
// checkout: a detached worktree of the clone magnum's review uses, at the
// PR's remote head, in the factory's checkouts folder, named for the
// repository and the PR, with no setup, no install, no database and no
// Redis flush. So does a [[factory.repo]] without fix copies.
func TestARepoWithoutCopiesGetsAPerPRCheckoutWithoutDatabases(t *testing.T) {
	r := newFixRig(t, 2, 0)
	shop := r.addShop()
	head := r.pushPR(shop, 5, "fix.txt", "v1\n")
	w := r.prWork("example/shop", 5)

	fc := r.claimFix(w)
	want := r.cfg.Layout.FactoryCheckout("example/shop", 5)
	if !fc.PerPR() || fc.Name != "fix-example/shop#5" || fc.Path != want || fc.Head != head || fc.Env != nil || fc.Port != 0 {
		t.Fatalf("ClaimFix = %+v, want the per-PR checkout fix-example/shop#5 at %s, no env", fc, want)
	}
	if got, branch := r.head(fc.Path); got != head || branch != "" {
		t.Fatalf("the checkout is at %s on %q, want the PR's head %s, detached", got, branch, head)
	}
	if listed := r.gitIn(filepath.Join(r.root, "shop"), "worktree", "list", "--porcelain"); !strings.Contains(listed, "worktree "+want) {
		t.Fatalf("the shop's clone lists no worktree at %s:\n%s", want, listed)
	}
	if got := r.ref(filepath.Join(r.root, "shop"), gitx.PRRef(5)); got != "" {
		t.Fatalf("the fix moved the review's %s to %s", gitx.PRRef(5), got)
	}
	r.mu.Lock()
	scripts := slices.Clone(r.scripts)
	r.mu.Unlock()
	if len(scripts) != 0 || len(r.flushes()) != 0 {
		t.Fatalf("the per-PR checkout ran %+v and flushed %v, want no setup, install or flush", scripts, r.flushes())
	}
	if dbs, _ := r.dbs.ListPrefixed(r.ctx, []string{""}); len(dbs) != 0 {
		t.Fatalf("databases %v exist, want none", dbs)
	}
	sl := r.slot(fc.Name)
	if sl.Kind != store.SlotKindPerPR || sl.State != store.SlotClaimed || sl.DBSlug != nil || store.Deref(sl.CheckedOutSHA) != head {
		t.Fatalf("the checkout's row = %+v, want a claimed per-PR row with no database, at the head", sl)
	}
	res, err := r.c.FixReserved(r.ctx)
	if err != nil || !slices.Equal(res, []FixReservation{{WorkID: w.ID, Checkout: fc.Name, PerPR: true}}) {
		t.Fatalf("FixReserved = %+v, %v, want the per-PR checkout", res, err)
	}
	if tasks, err := r.c.Reserved(r.ctx); err != nil || len(tasks) != 0 {
		t.Fatalf("Reserved = %+v, %v: a fix's checkout is no task's", tasks, err)
	}

	own := r.pushPR(r.origin, 9, "fix.txt", "v1\n")
	if fc := r.claimFix(r.prWork("talkable/talkable", 9)); !fc.PerPR() || fc.Head != own ||
		fc.Path != r.cfg.Layout.FactoryCheckout("talkable/talkable", 9) {
		t.Fatalf("a fix in a [[factory.repo]] without fix copies = %+v, want its per-PR checkout", fc)
	}
	if n := len(r.calls("bin/worktree-setup", "")); n != 0 {
		t.Fatalf("the setup ran %d times, want none", n)
	}
}

// A per-PR checkout lives in the factory's own folder of magnum's data
// directory, factory/checkouts/<owner>-<name>/pr-<N>: one folder per
// repository, one per PR in it. It went in the folder of the first
// [[factory.repo]]'s copies, which belongs to another repository (the
// operator's external volume), whatever repository the PR was of.
func TestAPerPRCheckoutLivesUnderItsOwnRepositorysFolder(t *testing.T) {
	r := newFixRig(t, 2, 0)
	shop := r.addShop()
	r.pushPR(shop, 5, "fix.txt", "v1\n")
	r.pushPR(shop, 6, "other.txt", "v1\n")
	r.pushPR(r.origin, 9, "fix.txt", "v1\n")
	checkouts := filepath.Join(r.cfg.Layout.Data(), "factory", "checkouts")
	copies := filepath.Dir(r.cfg.Factory.Repos[0].Pool().Path(1))
	for _, c := range []struct {
		repo   string
		number int
		want   string
	}{
		{"example/shop", 5, filepath.Join(checkouts, "example-shop", "pr-5")},
		{"example/shop", 6, filepath.Join(checkouts, "example-shop", "pr-6")},
		{"talkable/talkable", 9, filepath.Join(checkouts, "talkable-talkable", "pr-9")},
	} {
		fc := r.claimFix(r.prWork(c.repo, c.number))
		if !fc.PerPR() || fc.Path != c.want {
			t.Fatalf("the per-PR checkout of %s#%d is at %s, want %s", c.repo, c.number, fc.Path, c.want)
		}
		if _, err := os.Stat(filepath.Join(c.want, "README")); err != nil {
			t.Fatalf("the checkout of %s#%d at %s has no work tree: %v", c.repo, c.number, c.want, err)
		}
		if sl := r.slot(fc.Name); sl.Path != c.want {
			t.Fatalf("the row of %s is at %s, want %s", fc.Name, sl.Path, c.want)
		}
	}
	entries, err := os.ReadDir(copies)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "fix-pr") || strings.HasPrefix(e.Name(), "pr-") {
			t.Fatalf("the copies' folder %s holds %s, a per-PR checkout", copies, e.Name())
		}
	}
}

// A task never takes a fix copy, and a fix never takes a task's copy: each
// adds one of its own, or waits, while a copy of the other kind is free.
func TestATasksCopyIsNeverAFixCopy(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	r.pushPR(r.origin, 8, "other.txt", "v1\n")
	fix := r.prWork("talkable/talkable", 7)
	if fc := r.claimFix(fix); fc.Name != "factory2" {
		t.Fatalf("the fix claimed %s, want its fix copy factory2", fc.Name)
	}
	r.releaseFix(fix)
	if sl := r.slot("factory2"); sl.State != store.SlotFree {
		t.Fatalf("factory2 = %s after the fix's release, want free", sl.State)
	}
	if c := r.claim(r.task("PR-1")); c.Name != "factory1" {
		t.Fatalf("a task claimed %s while the fix copy factory2 is free, want a copy of its own, factory1", c.Name)
	}
	if _, err := r.c.Claim(r.ctx, r.task("PR-2")); !errors.Is(err, ErrNoCopy) || err.Error() != "copy: all 1 busy" {
		t.Fatalf("a second task's claim = %v, want a wait (copy: all 1 busy), never the free fix copy", err)
	}
	if sl := r.slot("factory2"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("factory2 = %s for %v, want free: no task's", sl.State, sl.PRID)
	}

	r2 := newFixRig(t, 1, 1)
	r2.pushPR(r2.origin, 8, "other.txt", "v1\n")
	task := r2.task("PR-3")
	r2.claim(task)
	r2.release(task)
	if fc := r2.claimFix(r2.prWork("talkable/talkable", 8)); fc.Name != "factory2" {
		t.Fatalf("a fix claimed %s while the task copy factory1 is free, want its own fix copy factory2", fc.Name)
	}
	if sl := r2.slot("factory1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("factory1 = %s for %v, want free: no fix's", sl.State, sl.PRID)
	}
}

// A fix takes the fix copy its PR used last when it is free, before the
// copy that waited longest.
func TestAFixPrefersThePRsLastCopy(t *testing.T) {
	r := newFixRig(t, 1, 2)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	r.pushPR(r.origin, 8, "other.txt", "v1\n")
	a, b := r.prWork("talkable/talkable", 7), r.prWork("talkable/talkable", 8)
	if fc := r.claimFix(a); fc.Name != "factory2" {
		t.Fatalf("a claimed %s, want factory2", fc.Name)
	}
	if fc := r.claimFix(b); fc.Name != "factory3" {
		t.Fatalf("b claimed %s, want factory3", fc.Name)
	}
	r.releaseFix(a)
	r.releaseFix(b)
	if fc := r.claimFix(r.work(b.ID)); fc.Name != "factory3" {
		t.Fatalf("b claimed %s again, want factory3, the copy it used last (factory2 waited longer)", fc.Name)
	}
	if fc := r.claimFix(r.work(a.ID)); fc.Name != "factory2" {
		t.Fatalf("a claimed %s again, want factory2", fc.Name)
	}
}

// A restart keeps a fix's claim and its copy: the copy stays the PR's, the
// next claim of the same head runs only the steps a cut left undone, and a
// tick lets go no checkout of fix work that goes on. In the engine a stop
// during the claim ends its begin, and the next start's claim resumes it.
func TestAFixClaimSurvivesAFactoryRestart(t *testing.T) {
	r := newFixRig(t, 1, 1)
	head := r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	cut := steps.WithFailpoint(r.ctx, func(subject, name string, at steps.Point) error {
		if strings.HasPrefix(subject, "slot:factory2:fix:") && name == "install Gemfile.lock" && at == steps.BeforeRun {
			return errors.New("the daemon stopped")
		}
		return nil
	})
	if _, err := r.c.ClaimFix(cut, w); err == nil {
		t.Fatal("the cut claim succeeded")
	}
	r.c = r.newCopies() // the restart
	res, err := r.c.FixReserved(r.ctx)
	if err != nil || !slices.Equal(res, []FixReservation{{WorkID: w.ID, Checkout: "factory2"}}) {
		t.Fatalf("FixReserved after the restart = %+v, %v, want factory2 still the PR's", res, err)
	}
	fc := r.claimFix(r.work(w.ID))
	if fc.Name != "factory2" || fc.Head != head {
		t.Fatalf("the claim after the restart = %+v, want factory2 at %s", fc, head)
	}
	if n := r.stepBegins("slot:factory2:fix:", "switch"); n != 1 {
		t.Fatalf("the switch began %d times, want once: the resumed claim never runs a done step again", n)
	}
	if n := len(r.calls("bundle install", fc.Path)); n != 1 {
		t.Fatalf("the install ran %d times, want once, in the resumed claim", n)
	}
	if n := len(r.calls("bin/worktree-setup", "")); n != 1 {
		t.Fatalf("the setup ran %d times, want once: the restart kept the copy", n)
	}

	// The engine: the daemon stops while the claim installs.
	block := make(chan struct{})
	r.mu.Lock()
	r.blocks["bundle install factory2"] = block
	r.mu.Unlock()
	r.write(filepath.Join(r.root, "author-origin.git", "Gemfile.lock"), "GEM v2\n")
	newer := r.pushPR(r.origin, 7, "fix.txt", "v2\n")
	h := newHarness(t, func(d *Deps) {
		d.Registry, d.Copies, d.Clock = r.reg, r.c, r.clock.Now
		d.Magnum = &fakeMagnum{}
	})
	if _, err := h.e.claimFix(h.ctx, r.work(w.ID)); !errors.Is(err, ErrHeld) {
		t.Fatalf("claimFix = %v, want a wait while its claim runs", err)
	}
	select {
	case got := <-r.entered:
		if got != "bundle install factory2" {
			t.Fatalf("the claim blocked in %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the claim never reached its install")
	}
	h.deps.Copies = r.newCopies()
	stopped := h.e.bgCtx
	go func() { <-stopped.Done(); close(block) }() // the install ends with the stop's cancel, as its process group does
	h.restart()
	if evs := h.events(0, "fix.claim"); len(evs) != 2 || evs[1].Phase != registry.PhaseFail ||
		!strings.Contains(evs[1].Text, "the daemon stopped before the fix took it: the next claim resumes it") {
		t.Fatalf("fix.claim events %+v, want the begin closed by the stop", evs)
	}
	h.tick()
	if res, err := h.deps.Copies.FixReserved(h.ctx); err != nil || len(res) != 1 {
		t.Fatalf("FixReserved after the restart's tick = %+v, %v, want the PR's copy kept", res, err)
	}
	if _, err := h.e.claimFix(h.ctx, r.work(w.ID)); !errors.Is(err, ErrHeld) {
		t.Fatalf("claimFix after the restart = %v, want its claim to run again", err)
	}
	h.e.waitBackground()
	fc, err = h.e.claimFix(h.ctx, r.work(w.ID))
	if err != nil || fc.Name != "factory2" || fc.Head != newer {
		t.Fatalf("claimFix once the resumed claim ended = %+v, %v, want factory2 at %s", fc, err, newer)
	}
	if got, _ := r.head(fc.Path); got != newer {
		t.Fatalf("the copy is at %s, want %s", got, newer)
	}
	if n := r.stepBegins("slot:factory2:fix:", "switch"); n != 2 {
		t.Fatalf("the switch began %d times, want twice: once per head", n)
	}

	// The same cycle keeps its checkout; the next cycle claims the PR's head
	// again, in the copy the PR holds.
	newest := r.pushPR(r.origin, 7, "fix.txt", "v3\n")
	if again, err := h.e.claimFix(h.ctx, r.work(w.ID)); err != nil || again.Head != newer {
		t.Fatalf("claimFix in the same cycle = %+v, %v, want the checkout it holds at %s", again, err, newer)
	}
	if err := r.reg.UpdatePRWork(r.ctx, w.ID, nil, "", func(u *registry.PRWorkUpdate) { u.Set("cycle", 2) }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.claimFix(h.ctx, r.work(w.ID)); !errors.Is(err, ErrHeld) {
		t.Fatalf("claimFix in the next cycle = %v, want its claim to run", err)
	}
	h.e.waitBackground()
	if fc, err := h.e.claimFix(h.ctx, r.work(w.ID)); err != nil || fc.Name != "factory2" || fc.Head != newest {
		t.Fatalf("claimFix in the next cycle = %+v, %v, want factory2 at %s", fc, err, newest)
	}
}

// A per-PR checkout goes once the PR's fix work ends: at once when the PR
// merges or closes, once its work stayed idle for fixIdleEnd, never while
// the work goes on. Its worktree, its fix ref and its row's hold on it go,
// with a fix.release event. A commit of the fixer's on no remote holds it
// until the fixer pushes it.
func TestAPerPRCheckoutIsRemovedWhenThePRsWorkEnds(t *testing.T) {
	r := newFixRig(t, 1, 0)
	shop := r.addShop()
	clone := filepath.Join(r.root, "shop")
	works := map[int]registry.PRWork{}
	paths := map[int]string{}
	for _, n := range []int{4, 5, 6} {
		r.pushPR(shop, n, "fix"+strconv.Itoa(n)+".txt", "v1\n")
		works[n] = r.prWork("example/shop", n)
		paths[n] = r.claimFix(works[n]).Path
	}
	if err := r.reg.UpdatePRWork(r.ctx, works[5].ID, nil, registry.PRWorkFixing, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.reg.UpdatePRWork(r.ctx, works[4].ID, nil, registry.PRWorkReviewing, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.reg.UpdatePRWork(r.ctx, works[6].ID, nil, registry.PRWorkIdle, nil); err != nil {
		t.Fatal(err)
	}
	mag := &fakeMagnum{prs: map[PRRef]PRState{{Repo: "example/shop", Number: 5}: {GHState: store.GHMerged}}}
	h := newHarness(t, func(d *Deps) {
		d.Registry, d.Copies, d.Clock, d.Magnum = r.reg, r.c, r.clock.Now, mag
	})
	removed := func(n int) bool {
		_, err := os.Stat(paths[n])
		return errors.Is(err, os.ErrNotExist) && r.slot("fix-example/shop#"+strconv.Itoa(n)).State == store.SlotRemoved
	}

	// The fixer's commit, not pushed yet, holds the checkout of the merged PR.
	r.write(filepath.Join(paths[5], "fix5.txt"), "v2\n")
	r.gitIn(paths[5], "commit", "--quiet", "-am", "the fixer's fix")
	h.tick()
	if removed(5) {
		t.Fatal("the removal lost the fixer's commit on no remote")
	}
	if evs := h.events(0, "fix.release"); len(evs) != 1 || evs[0].Phase != registry.PhaseFail || !strings.Contains(evs[0].Text, "unpushed_commits") {
		t.Fatalf("fix.release events %+v, want the removal held over the commit on no remote", evs)
	}
	r.gitIn(paths[5], "push", "--quiet", "--force", "origin", "HEAD:refs/pull/5/head") // the fixer pushes it
	r.clock.Add(claimBackoff(1))
	h.tick()
	if !removed(5) {
		t.Fatalf("the checkout of the merged PR is still there (%s)", r.slot("fix-example/shop#5").State)
	}
	if got := r.ref(clone, fixRef(5)); got != "" {
		t.Fatalf("%s is still %s after its checkout went", fixRef(5), got)
	}
	if listed := r.gitIn(clone, "worktree", "list", "--porcelain"); strings.Contains(listed, paths[5]) {
		t.Fatalf("the clone still lists %s:\n%s", paths[5], listed)
	}
	if removed(6) || removed(4) {
		t.Fatal("a checkout went before its work ended for good")
	}
	evs := h.events(0, "fix.release")
	if len(evs) != 2 || !strings.Contains(evs[1].Text, "removed fix-example/shop#5 of example/shop#5: the PR merged") {
		t.Fatalf("fix.release events %+v, want the merged PR's removal once its commit was pushed", evs)
	}

	r.clock.Add(fixIdleEnd)
	h.tick()
	if !removed(6) {
		t.Fatal("the checkout of work idle for 10 minutes is still there")
	}
	if removed(4) {
		t.Fatal("the checkout of work that waits for its re-review went")
	}
	res, err := r.c.FixReserved(r.ctx)
	if err != nil || !slices.Equal(res, []FixReservation{{WorkID: works[4].ID, Checkout: "fix-example/shop#4", PerPR: true}}) {
		t.Fatalf("FixReserved = %+v, %v, want only the checkout of the work that goes on", res, err)
	}

	// The work comes back: the PR gets a checkout again.
	if fc := r.claimFix(r.prWork("example/shop", 6)); fc.Path != paths[6] {
		t.Fatalf("the claim of work that came back = %+v, want its checkout at %s again", fc, paths[6])
	}
}
