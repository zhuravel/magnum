package factory

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
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
	return r.addWatchedAt(r.root, "example", "shop")
}

// addWatched adds owner/name to the rig as addShop adds example/shop, under
// a clone_root of its own, and returns its origin.
func (r *copiesRig) addWatched(owner, name string) string {
	r.t.Helper()
	return r.addWatchedAt(filepath.Join(r.root, "watched-"+owner), owner, name)
}

// addWatchedAt adds owner/name to the rig: a bare origin
// <root>/<name>-origin.git, its clone <root>/<name> (which clone discovery
// finds by its GitHub origin) and a [[watch]] of the repository alone whose
// clone_root is root, and no [[factory.repo]] or [[pool]]. It returns the
// origin.
func (r *copiesRig) addWatchedAt(root, owner, name string) string {
	r.t.Helper()
	origin, clone := filepath.Join(root, name+"-origin.git"), filepath.Join(root, name)
	work := filepath.Join(root, name+"-work")
	for _, d := range []string{origin, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	r.gitIn(origin, "init", "--quiet", "--bare", "--template=", "-b", "master")
	r.gitIn(work, "init", "--quiet", "--template=", "-b", "master")
	r.write(filepath.Join(work, "README"), name+"\n")
	r.write(filepath.Join(work, "db", "schema.rb"), schemaRB(baseSchema))
	r.gitIn(work, "add", "-A")
	r.gitIn(work, "commit", "--quiet", "-m", "init")
	r.gitIn(work, "remote", "add", "origin", origin)
	r.gitIn(work, "push", "--quiet", "origin", "master")
	r.gitIn(root, "clone", "--quiet", "--template=", origin, clone)
	r.origins[clone] = "https://github.com/" + owner + "/" + name + ".git"
	r.cfg.Watches = append(r.cfg.Watches, config.Watch{Owner: owner, Include: []string{name}, CloneRoot: root})
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
	if err != nil || !slices.Equal(res, []FixReservation{{WorkID: w.ID, Checkout: "factory3", Repo: "talkable/talkable", Number: 7}}) {
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
	if err != nil || !slices.Equal(res, []FixReservation{{WorkID: w.ID, Checkout: fc.Name, PerPR: true, Repo: "example/shop", Number: 5}}) {
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
// directory, factory/checkouts/<owner>/<name>/pr-<N>: a folder per owner
// and per repository, one per PR in it. It went in the folder of the first
// [[factory.repo]]'s copies, which belongs to another repository (the
// operator's external volume), whatever repository the PR was of. Owners
// and names may hold a "-", so example-web/app and example/web-app, joined
// by one, would share a path (slots.path is unique): nested folders cannot.
func TestAPerPRCheckoutLivesUnderItsOwnRepositorysFolder(t *testing.T) {
	r := newFixRig(t, 2, 0)
	shop := r.addShop()
	r.pushPR(shop, 5, "fix.txt", "v1\n")
	r.pushPR(shop, 6, "other.txt", "v1\n")
	r.pushPR(r.origin, 9, "fix.txt", "v1\n")
	r.pushPR(r.addWatched("example-web", "app"), 5, "fix.txt", "v1\n")
	r.pushPR(r.addWatched("example", "web-app"), 5, "fix.txt", "v1\n")
	checkouts := filepath.Join(r.cfg.Layout.Data(), "factory", "checkouts")
	copies := filepath.Dir(r.cfg.Factory.Repos[0].Pool().Path(1))
	for _, c := range []struct {
		repo   string
		number int
		want   string
	}{
		{"example/shop", 5, filepath.Join(checkouts, "example", "shop", "pr-5")},
		{"example/shop", 6, filepath.Join(checkouts, "example", "shop", "pr-6")},
		{"talkable/talkable", 9, filepath.Join(checkouts, "talkable", "talkable", "pr-9")},
		{"example-web/app", 5, filepath.Join(checkouts, "example-web", "app", "pr-5")},
		{"example/web-app", 5, filepath.Join(checkouts, "example", "web-app", "pr-5")},
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
	if err != nil || !slices.Equal(res, []FixReservation{{WorkID: w.ID, Checkout: "factory2", Repo: "talkable/talkable", Number: 7}}) {
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

// setTyped records that you typed in PR work w's fix panes at the rig's
// time: its owner row's human_active_at, as the observe pass sets it.
func (r *copiesRig) setTyped(w registry.PRWork) {
	r.t.Helper()
	owner, err := r.c.fixOwnerOf(r.ctx, w.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.UpdatePR(r.ctx, owner.ID, func(u *store.PRUpdate) { u.Set("human_active_at", r.clock.Now()) }); err != nil {
		r.t.Fatal(err)
	}
}

// The release takes a HEAD equal to the PR's head it fetched as the
// checkout's own whatever you typed in the fix's panes: all of it is on
// GitHub. It held it as head_drift for good once you typed (R1 on T3b):
// the typing kept fixOwnHead from recording it, and the slot guard
// compared it with the owner row's head, the claim's.
func TestAReleaseTakesThePushedHeadWhateverYouTyped(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	fc := r.claimFix(w)
	r.clock.Add(time.Minute)
	r.setTyped(w) // you answer the fixer in its pane
	r.write(filepath.Join(fc.Path, "fix.txt"), "v2\n")
	r.gitIn(fc.Path, "commit", "--quiet", "-am", "the fixer's fix")
	r.gitIn(fc.Path, "push", "--quiet", "--force", "origin", "HEAD:refs/pull/7/head") // the fixer pushes it
	r.releaseFix(w)
	sl := r.slot(fc.Name)
	if sl.State != store.SlotFree || sl.HoldReason != nil || sl.PRID != nil {
		t.Fatalf("%s after the release = %s, hold %v, pr %v, want free with no hold", fc.Name, sl.State, store.Deref(sl.HoldReason), sl.PRID)
	}
	if got := r.ref(r.main, fixRef(7)); got != "" {
		t.Fatalf("%s is still %s after the release", fixRef(7), got)
	}

	// So does a per-PR checkout's removal.
	shop := r.addShop()
	r.pushPR(shop, 5, "fix.txt", "v1\n")
	ws := r.prWork("example/shop", 5)
	pc := r.claimFix(ws)
	r.clock.Add(time.Minute)
	r.setTyped(ws)
	r.write(filepath.Join(pc.Path, "fix.txt"), "v2\n")
	r.gitIn(pc.Path, "commit", "--quiet", "-am", "the fixer's fix")
	r.gitIn(pc.Path, "push", "--quiet", "--force", "origin", "HEAD:refs/pull/5/head")
	r.releaseFix(ws)
	if sl := r.slot(pc.Name); sl.State != store.SlotRemoved || sl.HoldReason != nil || fsx.Exists(pc.Path) {
		t.Fatalf("%s after the release = %s, hold %v, want removed with no hold", pc.Name, sl.State, store.Deref(sl.HoldReason))
	}
}

// commit commits every change in dir as the fixer and returns the commit.
func (r *copiesRig) commit(dir, file, content string) string {
	r.t.Helper()
	r.write(filepath.Join(dir, file), content)
	r.gitIn(dir, "add", "-A")
	r.gitIn(dir, "commit", "--quiet", "-m", "the fixer's fix")
	return r.gitIn(dir, "rev-parse", "HEAD")
}

// A claim's guard takes a HEAD the fixer moved as the checkout's own, with
// a factory.fix_head event, when it is the PR's head the claim fetched (the
// fixer pushed it) or a descendant of the checkout (the fixer's commits on
// it), and the claim goes on. No test reached the claim's guard with a
// moved HEAD (R10 on T3b). It does so whatever you typed in the fix's
// panes after the checkout: a commit after your typing is kept under
// refs/magnum/factory-kept/<N>/<sha> before the switch, as any commit on no
// remote, and held the checkout as head_drift for good before the
// engine owner's one rule for a fix checkout's commits (C1, C2 and C7 on
// T3b).
func TestAClaimTakesTheFixersMovedHeadWhateverYouTyped(t *testing.T) {
	r := newFixRig(t, 1, 2)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w7 := r.prWork("talkable/talkable", 7)
	fc := r.claimFix(w7)
	pushed := r.commit(fc.Path, "fix.txt", "v2\n")
	r.gitIn(fc.Path, "push", "--quiet", "--force", "origin", "HEAD:refs/pull/7/head")
	if again := r.claimFix(w7); again.Name != fc.Name || again.Head != pushed {
		t.Fatalf("the claim at the head the fixer pushed = %+v, want %s at %s", again, fc.Name, pushed)
	}
	if !r.eventsSay(fc.Name, "factory.fix_head", "HEAD "+textx.ShortSHA(pushed)) {
		t.Fatalf("no factory.fix_head event for the pushed HEAD %s", pushed)
	}

	r.pushPR(r.origin, 8, "other.txt", "v1\n")
	w8 := r.prWork("talkable/talkable", 8)
	fc8 := r.claimFix(w8)
	mine := r.commit(fc8.Path, "other.txt", "v2\n")
	newer := r.pushPR(r.origin, 8, "other.txt", "v3\n")
	if again := r.claimFix(w8); again.Head != newer {
		t.Fatalf("the claim at the PR's newer head = %+v, want %s", again, newer)
	}
	if got, _ := r.head(fc8.Path); got != newer {
		t.Fatalf("%s is at %s, want the PR's newer head %s", fc8.Name, got, newer)
	}
	if !r.eventsSay(fc8.Name, "factory.fix_head", "HEAD "+textx.ShortSHA(mine)) {
		t.Fatalf("no factory.fix_head event for the fixer's commit %s", mine)
	}

	yours := r.commit(fc8.Path, "other.txt", "v4\n")
	r.clock.Add(time.Minute)
	r.setTyped(w8)
	newest := r.pushPR(r.origin, 8, "other.txt", "v5\n")
	if again := r.claimFix(w8); again.Head != newest {
		t.Fatalf("the claim over a commit after your typing = %+v, want %s at the PR's newest head", again, newest)
	}
	if got, _ := r.head(fc8.Path); got != newest {
		t.Fatalf("%s is at %s, want the PR's newest head %s", fc8.Name, got, newest)
	}
	if sl := r.slot(fc8.Name); sl.HoldReason != nil {
		t.Fatalf("%s's hold = %q, want none", fc8.Name, store.Deref(sl.HoldReason))
	}
	ref := fixKeptRef(8, yours)
	if got := r.ref(r.main, ref); got != yours {
		t.Fatalf("%s = %q, want the commit after your typing %s kept", ref, got, yours)
	}
	if !r.eventsSay(fc8.Name, "factory.fix_kept", ref) {
		t.Fatalf("no factory.fix_kept event names %s", ref)
	}
}

// A claim at a newer head of the PR keeps the fixer's commits on no remote
// before it switches away from them: under refs/magnum/factory-kept/<N>/
// <sha> of the main clone, with a factory.fix_kept event that names the
// ref, so the removal of a per-PR checkout, which deletes its reflog, loses
// none. They were left on no ref, and the removal lost them (R2 and R18 on
// T3b). A head a force-push of the PR left behind is the PR's, not the
// fixer's: nothing keeps it.
func TestAClaimKeepsTheFixersUnpushedCommitsBeforeItSwitches(t *testing.T) {
	r := newFixRig(t, 1, 0)
	shop := r.addShop()
	clone := filepath.Join(r.root, "shop")
	kept := func() string {
		return r.gitIn(clone, "for-each-ref", "--format=%(refname) %(objectname)", "refs/magnum/factory-kept/")
	}
	r.pushPR(shop, 5, "fix.txt", "v1\n")
	w := r.prWork("example/shop", 5)
	pc := r.claimFix(w)
	mine := r.commit(pc.Path, "fix.txt", "v2\n")
	newer := r.pushPR(shop, 5, "fix.txt", "v3\n")
	if got := r.claimFix(w); got.Head != newer {
		t.Fatalf("the claim at the PR's newer head = %+v, want %s", got, newer)
	}
	ref := "refs/magnum/factory-kept/5/" + mine
	if got := kept(); got != ref+" "+mine {
		t.Fatalf("the kept refs = %q, want %s at the fixer's commit", got, ref)
	}
	if !r.eventsSay(pc.Name, "factory.fix_kept", ref) {
		t.Fatalf("no factory.fix_kept event names %s", ref)
	}
	r.releaseFix(w)
	if fsx.Exists(pc.Path) {
		t.Fatalf("%s is still there after its release", pc.Path)
	}
	if got := r.gitIn(clone, "rev-parse", "--verify", ref+"^{commit}"); got != mine {
		t.Fatalf("%s = %q after the removal, want the fixer's commit %s", ref, got, mine)
	}

	r.pushPR(shop, 6, "other.txt", "v1\n")
	w6 := r.prWork("example/shop", 6)
	r.claimFix(w6)
	author := filepath.Join(r.root, "author-"+filepath.Base(shop))
	r.gitIn(author, "reset", "--quiet", "--hard", "origin/master")
	rebased := r.pushPR(shop, 6, "rebased.txt", "v1\n")
	if got := r.claimFix(w6); got.Head != rebased {
		t.Fatalf("the claim after a force-push = %+v, want %s", got, rebased)
	}
	if got := kept(); got != ref+" "+mine {
		t.Fatalf("the kept refs = %q after a force-push of PR 6, want only PR 5's", got)
	}
}

// A PR whose repository's fix_copies changed since it claimed its checkout
// lets that checkout go and claims one of the other kind: from a fix copy
// to a per-PR checkout when fix_copies drops to 0, and back. The release of
// the fix copy deleted the fix ref the same claim had just fetched, so the
// per-PR checkout's worktree add failed the fix (R3 and R7 on T3b), and no
// test changed fix_copies between two claims (R13).
func TestAChangeOfFixCopiesMovesThePRsCheckout(t *testing.T) {
	r := newFixRig(t, 1, 1)
	head := r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	if fc := r.claimFix(w); fc.Name != "factory2" {
		t.Fatalf("the claim = %+v, want the fix copy factory2", fc)
	}

	r.cfg.Factory.Repos[0].FixCopies = 0
	r.c = r.newCopies() // the restart on the new config
	pc := r.claimFix(w)
	if !pc.PerPR() || pc.Head != head {
		t.Fatalf("the claim with fix_copies = 0 = %+v, want a per-PR checkout at %s", pc, head)
	}
	if got, _ := r.head(pc.Path); got != head {
		t.Fatalf("the per-PR checkout is at %s, want %s", got, head)
	}
	if sl := r.slot("factory2"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("factory2 = %s (pr %v), want free once the PR moved", sl.State, sl.PRID)
	}
	if got := r.ref(r.main, fixRef(7)); got != head {
		t.Fatalf("%s = %q, want the claimed head %s", fixRef(7), got, head)
	}

	r.cfg.Factory.Repos[0].FixCopies = 1
	r.c = r.newCopies()
	if fc := r.claimFix(w); fc.Name != "factory2" || fc.Head != head {
		t.Fatalf("the claim with fix_copies = 1 again = %+v, want factory2 at %s", fc, head)
	}
	if sl := r.slot(pc.Name); sl.State != store.SlotRemoved || fsx.Exists(pc.Path) {
		t.Fatalf("the per-PR checkout = %s, want removed once the PR moved", sl.State)
	}
	res, err := r.c.FixReserved(r.ctx)
	if err != nil || !slices.Equal(res, []FixReservation{{WorkID: w.ID, Checkout: "factory2", Repo: "talkable/talkable", Number: 7}}) {
		t.Fatalf("FixReserved = %+v, %v, want only factory2", res, err)
	}
}

// A fix checkout goes when its PR closes, as when it merges, and when its
// PR work's row is gone: the release of the gone row's checkout knows the
// PR from the checkout's owner row, so it fetches the PR's head (what the
// fixer pushed counts as pushed) and deletes that PR's fix ref. It ran as
// PR number 0: it skipped the fetch, held a pushed commit as unpushed and
// deleted refs/magnum/factory-fix/0 (R16 on T3b). Nothing deletes a row
// today; a claim for work with no row stands for one that went.
func TestAFixCheckoutGoesWhenItsPRClosesOrItsWorkIsGone(t *testing.T) {
	r := newFixRig(t, 1, 0)
	shop := r.addShop()
	clone := filepath.Join(r.root, "shop")
	r.pushPR(shop, 5, "fix.txt", "v1\n")
	r.pushPR(shop, 6, "other.txt", "v1\n")
	closed := r.prWork("example/shop", 5)
	if err := r.reg.UpdatePRWork(r.ctx, closed.ID, nil, registry.PRWorkFixing, nil); err != nil {
		t.Fatal(err)
	}
	gone := registry.PRWork{ID: closed.ID + 100, Repo: "example/shop", Number: 6}
	pc5, pc6 := r.claimFix(closed), r.claimFix(gone)
	r.commit(pc6.Path, "other.txt", "v2\n")
	r.gitIn(pc6.Path, "push", "--quiet", "--force", "origin", "HEAD:refs/pull/6/head") // the fixer pushed it
	mag := &fakeMagnum{prs: map[PRRef]PRState{{Repo: "example/shop", Number: 5}: {GHState: store.GHClosed}}}
	h := newHarness(t, func(d *Deps) {
		d.Registry, d.Copies, d.Clock, d.Magnum = r.reg, r.c, r.clock.Now, mag
	})
	h.tick()
	for _, c := range []struct {
		fc     FixCheckout
		number int
		why    string
	}{{pc5, 5, "the PR closed"}, {pc6, 6, "its PR's fix work is gone"}} {
		if sl := r.slot(c.fc.Name); sl.State != store.SlotRemoved || fsx.Exists(c.fc.Path) {
			t.Fatalf("%s = %s, want removed: %s", c.fc.Name, sl.State, c.why)
		}
		if got := r.ref(clone, fixRef(c.number)); got != "" {
			t.Fatalf("%s is still %s after its checkout went", fixRef(c.number), got)
		}
		want := fmt.Sprintf("removed %s of example/shop#%d: %s", c.fc.Name, c.number, c.why)
		if !slices.ContainsFunc(h.events(0, "fix.release"), func(ev registry.Event) bool { return ev.Text == want }) {
			t.Fatalf("fix.release events %+v, want %q", h.events(0, "fix.release"), want)
		}
	}
}

// A fix copy's release, as a per-PR checkout's removal, keeps the fixer's
// commit on no remote under refs/magnum/factory-kept/<N>/<sha> and frees
// the copy; a commit the fixer pushed is kept nowhere: the release fetches
// the PR's head first, so it counts as pushed, and is no head_drift. Only
// the per-PR checkout's removal had this test (R11 on T3b); the release
// held on the commit until it was pushed before the engine owner's one
// rule for a fix checkout's commits (C1, C2 and C7 on T3b).
func TestAFixCopysReleaseKeepsTheFixersCommitAndFreesTheCopy(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	fc := r.claimFix(w)
	mine := r.commit(fc.Path, "fix.txt", "v2\n")
	r.releaseFix(w)
	if sl := r.slot(fc.Name); sl.State != store.SlotFree || sl.HoldReason != nil || sl.PRID != nil {
		t.Fatalf("%s after the release = %s, hold %q, pr %v, want free", fc.Name, sl.State, store.Deref(sl.HoldReason), sl.PRID)
	}
	ref := fixKeptRef(7, mine)
	if got := r.keptRefs(r.main); got != ref+" "+mine {
		t.Fatalf("the kept refs = %q, want %s at the fixer's commit", got, ref)
	}
	if got := r.ref(r.main, fixRef(7)); got != "" {
		t.Fatalf("%s is still %s after the release", fixRef(7), got)
	}

	fc = r.claimFix(w)
	pushed := r.commit(fc.Path, "fix.txt", "v3\n")
	r.gitIn(fc.Path, "push", "--quiet", "--force", "origin", "HEAD:refs/pull/7/head") // the fixer pushes it
	r.releaseFix(w)
	if sl := r.slot(fc.Name); sl.State != store.SlotFree || sl.HoldReason != nil {
		t.Fatalf("%s after the release of a pushed commit = %s, hold %q, want free", fc.Name, sl.State, store.Deref(sl.HoldReason))
	}
	if got := r.ref(r.main, fixKeptRef(7, pushed)); got != "" {
		t.Fatalf("the release kept the pushed commit %s as %s", pushed, fixKeptRef(7, pushed))
	}
}

// The removal of a per-PR checkout holds changes your typing in the fix's
// panes since the checkout explains (dirty_worktree, the checkout and the
// file kept), and removes the fixer's residue otherwise, named in a
// slot.discarded event. No test left a change in a per-PR checkout (R13 on
// T3b); the slot guard holds a tracked change you made, so an untracked
// file is the case the removal itself decides.
func TestAPerPRCheckoutsRemovalHoldsYourChangesAndDropsTheFixers(t *testing.T) {
	r := newFixRig(t, 1, 0)
	shop := r.addShop()
	r.pushPR(shop, 5, "fix.txt", "v1\n")
	r.pushPR(shop, 6, "other.txt", "v1\n")
	yours := r.prWork("example/shop", 5)
	pc := r.claimFix(yours)
	r.write(filepath.Join(pc.Path, "notes.txt"), "mine\n")
	r.clock.Add(time.Minute)
	r.setTyped(yours)
	err := r.c.ReleaseFix(r.ctx, yours, "released")
	if hold, ok := errors.AsType[slots.ErrHold](err); !ok || hold.Reason != slots.HoldDirtyWorktree || !strings.Contains(hold.Detail, "notes.txt") {
		t.Fatalf("the removal over your file = %v, want held: dirty_worktree naming notes.txt", err)
	}
	if !fsx.Exists(filepath.Join(pc.Path, "notes.txt")) {
		t.Fatal("the removal lost your file")
	}

	fixers := r.prWork("example/shop", 6)
	pc6 := r.claimFix(fixers)
	r.write(filepath.Join(pc6.Path, "residue.txt"), "the fixer's\n")
	r.releaseFix(fixers)
	if sl := r.slot(pc6.Name); sl.State != store.SlotRemoved || fsx.Exists(pc6.Path) {
		t.Fatalf("%s = %s, want removed with the fixer's residue", pc6.Name, sl.State)
	}
	if !r.eventsSay(pc6.Name, "slot.discarded", "residue.txt") {
		t.Fatal("no slot.discarded event names the fixer's residue.txt")
	}
}

// A force-push of the PR between the claim and the release keeps nothing:
// the head the claim switched to counts as pushed, though the release's
// fetch moved the fix ref off it, so it is the PR's and not kept. Its
// commits counted as unpushed_commits, and every later try fetched and
// held again (R5 on T3b). A commit of the fixer's on it is kept under
// refs/magnum/factory-kept/<N>/<sha>, alone, and the copy is freed.
func TestAForcePushOfThePRDoesNotHoldItsFixCheckout(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	fc := r.claimFix(w)
	forcePush := func(content string) {
		author := filepath.Join(r.root, "author-"+filepath.Base(r.origin))
		r.gitIn(author, "reset", "--quiet", "--hard", "origin/master")
		r.pushPR(r.origin, 7, "rebased.txt", content) // the author rebases and force-pushes
	}
	forcePush("v1\n")
	r.releaseFix(w)
	if sl := r.slot(fc.Name); sl.State != store.SlotFree || sl.HoldReason != nil {
		t.Fatalf("%s after a force-push and the release = %s, hold %v, want free", fc.Name, sl.State, store.Deref(sl.HoldReason))
	}
	if got := r.keptRefs(r.main); got != "" {
		t.Fatalf("the release kept %q, want nothing: the old head is the PR's", got)
	}

	// The fixer's own commit on the claimed head is kept, alone.
	fc = r.claimFix(w)
	mine := r.commit(fc.Path, "fix.txt", "v2\n")
	forcePush("v2\n")
	r.releaseFix(w)
	if sl := r.slot(fc.Name); sl.State != store.SlotFree || sl.HoldReason != nil {
		t.Fatalf("%s after the release over the fixer's commit = %s, hold %v, want free", fc.Name, sl.State, store.Deref(sl.HoldReason))
	}
	if got, ref := r.keptRefs(r.main), fixKeptRef(7, mine); got != ref+" "+mine {
		t.Fatalf("the kept refs = %q, want only %s at the fixer's commit", got, ref)
	}
}

// keptRefs lists the refs under refs/magnum/factory-kept/ of clone, each
// as "<ref> <commit>", one per line ("" for none).
func (r *copiesRig) keptRefs(clone string) string {
	r.t.Helper()
	return r.gitIn(clone, "for-each-ref", "--format=%(refname) %(objectname)", fixKeptPrefix)
}

// The release frees the checkout when the PR moved past a HEAD the fixer
// pushed after you typed in the fix's panes (an "Update branch" merge, a
// suggestion commit, the author's push): HEAD has no commit on no ref, so
// it is the checkout's own whatever you typed. It held it as head_drift for
// good (C1 on T3b): only a HEAD equal to the fetched head was taken, and
// the slot guard compared it with the owner row's head, the claim's.
func TestAReleaseFreesTheCheckoutTheFixerPushedAfterYouTypedOnceThePRMovedPastIt(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	fc := r.claimFix(w)
	r.clock.Add(time.Minute)
	r.setTyped(w) // you answer the fixer in its pane
	pushed := r.commit(fc.Path, "fix.txt", "v2\n")
	r.gitIn(fc.Path, "push", "--quiet", "--force", "origin", "HEAD:refs/pull/7/head") // the fixer pushes it
	author := filepath.Join(r.root, "author-"+filepath.Base(r.origin))
	r.gitIn(author, "fetch", "--quiet", "origin", "refs/pull/7/head")
	r.gitIn(author, "reset", "--quiet", "--hard", "FETCH_HEAD")
	past := r.pushPR(r.origin, 7, "suggestion.txt", "v1\n") // a commit on top of the fixer's push
	if got := r.gitIn(author, "rev-parse", past+"^"); got != pushed {
		t.Fatalf("the PR's new head %s has parent %s, want the fixer's push %s", past, got, pushed)
	}
	r.releaseFix(w)
	if sl := r.slot(fc.Name); sl.State != store.SlotFree || sl.HoldReason != nil || sl.PRID != nil {
		t.Fatalf("%s after the release = %s, hold %q, pr %v, want free with no hold", fc.Name, sl.State, store.Deref(sl.HoldReason), sl.PRID)
	}
	if got := r.keptRefs(r.main); got != "" {
		t.Fatalf("the release kept %q, want nothing: every commit of HEAD is on GitHub", got)
	}
}

// The release frees the checkout when the author force-pushed a rebase
// over the head the fixer pushed: that head is on no ref once the release's
// fetch moved the fix ref off it, so the release keeps it under
// refs/magnum/factory-kept/<N>/<sha>, with a factory.fix_kept event, and
// goes on. It held the release as unpushed_commits on every try (C2 on
// T3b): only the claimed head counted as pushed.
func TestAReleaseKeepsTheFixersPushAForcePushLeftBehindAndFreesTheCheckout(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	fc := r.claimFix(w)
	pushed := r.commit(fc.Path, "fix.txt", "v2\n")
	r.gitIn(fc.Path, "push", "--quiet", "--force", "origin", "HEAD:refs/pull/7/head") // the fixer pushes it
	author := filepath.Join(r.root, "author-"+filepath.Base(r.origin))
	r.gitIn(author, "reset", "--quiet", "--hard", "origin/master")
	r.pushPR(r.origin, 7, "rebased.txt", "v1\n") // the author rebases and force-pushes
	r.releaseFix(w)
	if sl := r.slot(fc.Name); sl.State != store.SlotFree || sl.HoldReason != nil || sl.PRID != nil {
		t.Fatalf("%s after the release = %s, hold %q, pr %v, want free with no hold", fc.Name, sl.State, store.Deref(sl.HoldReason), sl.PRID)
	}
	ref := fixKeptRef(7, pushed)
	if got := r.keptRefs(r.main); got != ref+" "+pushed {
		t.Fatalf("the kept refs = %q, want %s at the fixer's push", got, ref)
	}
	if !r.eventsSay(fc.Name, "factory.fix_kept", ref) {
		t.Fatalf("no factory.fix_kept event names %s", ref)
	}
}

// A claim lets go of the checkout of the other kind the PR holds (its
// repository's fix_copies changed) over the fixer's commit on no remote in
// it: the commit is kept under refs/magnum/factory-kept/<N>/<sha>, with a
// factory.fix_kept event, and the claim takes the new checkout. The
// release of the other checkout held on the commit, so the claim waited
// for good: no fixer could run to push it (C7 on T3b).
func TestAChangeOfFixCopiesKeepsTheFixersCommitAndClaims(t *testing.T) {
	r := newFixRig(t, 1, 0)
	head := r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	pc := r.claimFix(w)
	if !pc.PerPR() {
		t.Fatalf("the claim with fix_copies = 0 = %+v, want a per-PR checkout", pc)
	}
	mine := r.commit(pc.Path, "fix.txt", "v2\n")

	r.cfg.Factory.Repos[0].FixCopies = 1
	r.c = r.newCopies() // the restart on the new config
	fc, err := r.c.ClaimFix(r.ctx, w)
	if err != nil || fc.Name != "factory2" || fc.Head != head {
		t.Fatalf("the claim with fix_copies = 1 = %+v, %v, want the fix copy factory2 at %s", fc, err, head)
	}
	if sl := r.slot(pc.Name); sl.State != store.SlotRemoved || fsx.Exists(pc.Path) {
		t.Fatalf("the per-PR checkout = %s, want removed once the PR moved", sl.State)
	}
	ref := fixKeptRef(7, mine)
	if got := r.keptRefs(r.main); got != ref+" "+mine {
		t.Fatalf("the kept refs = %q, want %s at the fixer's commit", got, ref)
	}
	if !r.eventsSay(pc.Name, "factory.fix_kept", ref) {
		t.Fatalf("no factory.fix_kept event names %s", ref)
	}
}

// A claim's switch records the newer head as its assignment's before it
// moves the checkout there, so a switch cut once the checkout moved still
// leaves the newer head as the claim's: a release after a force-push of the
// PR then counts that head, the PR's, as pushed and keeps nothing. The
// switch recorded it last, so the cut left the older head as the claim's,
// and the release took the PR's own head for commits on no remote (C4 on
// T3b). The cut comes inside slots.Resync, once its git switch ran and
// before it records the checkout, whatever switchFix reads after it (the
// third check's word on C4's test).
func TestACutSwitchLeavesTheNewerHeadAsTheClaims(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	fc := r.claimFix(w)
	newer := r.pushPR(r.origin, 7, "fix.txt", "v2\n")
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	r.onGit(func(c execx.Cmd) {
		if len(c.Args) > 2 && c.Args[1] == fc.Path && c.Args[2] == "switch" && slices.Contains(c.Args, "--detach") {
			cancel() // the daemon goes down once Resync's switch moved the checkout, before it records it
		}
	})
	r.clock.Add(time.Minute)
	if _, err := r.c.ClaimFix(ctx, w); err == nil {
		t.Fatal("the claim cut once its checkout moved succeeded")
	}
	r.onGit(nil)
	if got, _ := r.head(fc.Path); got != newer {
		t.Fatalf("%s is at %s after the cut switch, want the newer head %s", fc.Name, got, newer)
	}
	a, err := r.st.OpenAssignmentBySlot(r.ctx, r.slot(fc.Name).ID)
	if err != nil || store.Deref(a.HeadSHA) != newer {
		t.Fatalf("the claim's head after the cut switch = %q, %v, want the newer head %s", store.Deref(a.HeadSHA), err, newer)
	}

	r.c = r.newCopies() // the restart
	author := filepath.Join(r.root, "author-"+filepath.Base(r.origin))
	r.gitIn(author, "reset", "--quiet", "--hard", "origin/master")
	r.pushPR(r.origin, 7, "rebased.txt", "v1\n") // the author rebases and force-pushes
	r.releaseFix(w)
	if sl := r.slot(fc.Name); sl.State != store.SlotFree || sl.HoldReason != nil {
		t.Fatalf("%s after the release = %s, hold %q, want free", fc.Name, sl.State, store.Deref(sl.HoldReason))
	}
	if got := r.keptRefs(r.main); got != "" {
		t.Fatalf("the release kept %q, want nothing: the head the cut switch moved to is the PR's", got)
	}
}

// A claim's switch cut after it recorded the newer head as its
// assignment's and before it moved the checkout there leaves HEAD at the
// older head, the checkout's recorded one: once a force-push of the PR took
// that head off every ref, the release counts it as pushed, as every head
// the checkout records was pushed or kept, and keeps nothing. Only the
// assignment's head counted, so the release kept the PR's own old head as
// the fixer's commits (C2 of T3b's third check).
func TestASwitchCutBeforeTheCheckoutMovedKeepsNothingOfThePRsOldHead(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	fc := r.claimFix(w)
	forcePush := func(content string) string {
		author := filepath.Join(r.root, "author-"+filepath.Base(r.origin))
		r.gitIn(author, "reset", "--quiet", "--hard", "origin/master")
		return r.pushPR(r.origin, 7, "rebased.txt", content) // the author rebases and force-pushes
	}
	newer := forcePush("v1\n")
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	r.onGit(func(c execx.Cmd) {
		if len(c.Args) > 2 && c.Args[1] == fc.Path && slices.Contains(c.Args, newer+"^{commit}") {
			cancel() // the daemon goes down once Resync looked the newer head up, before it moves the checkout
		}
	})
	r.clock.Add(time.Minute)
	if _, err := r.c.ClaimFix(ctx, w); err == nil {
		t.Fatal("the claim cut before its checkout moved succeeded")
	}
	r.onGit(nil)
	if got, _ := r.head(fc.Path); got != fc.Head {
		t.Fatalf("%s is at %s after the cut switch, want the older head %s", fc.Name, got, fc.Head)
	}
	if a, err := r.st.OpenAssignmentBySlot(r.ctx, r.slot(fc.Name).ID); err != nil || store.Deref(a.HeadSHA) != newer {
		t.Fatalf("the claim's head after the cut switch = %q, %v, want the newer head %s", store.Deref(a.HeadSHA), err, newer)
	}

	r.c = r.newCopies() // the restart
	forcePush("v2\n")
	r.releaseFix(w)
	if sl := r.slot(fc.Name); sl.State != store.SlotFree || sl.HoldReason != nil {
		t.Fatalf("%s after the release = %s, hold %q, want free", fc.Name, sl.State, store.Deref(sl.HoldReason))
	}
	if got := r.keptRefs(r.main); got != "" {
		t.Fatalf("the release kept %q, want nothing: the head the checkout stayed at is the PR's", got)
	}
}

// stopRebase leaves a replay of the fixer's commits in progress in checkout
// dir, claimed at PR number's head claimed, as `magnum fix-sync` killed
// mid-replay does: the fixer commits p1 and p2, the PR's author pushes a
// change of fix.txt on claimed to origin, and `git rebase --onto <new head>
// <claimed>` replays p1 and stops on p2, which changes fix.txt too. It
// returns HEAD, the replayed p1, and the rebase's original tip, p2.
func (r *copiesRig) stopRebase(origin string, number int, dir, claimed string) (replayed, orig string) {
	r.t.Helper()
	r.commit(dir, "fixer.txt", "p1\n")
	orig = r.commit(dir, "fix.txt", "the fixer's\n")
	head := r.pushPR(origin, number, "fix.txt", "the author's after "+textx.ShortSHA(claimed)+"\n")
	r.gitIn(dir, "fetch", "--quiet", "origin", "refs/pull/"+strconv.Itoa(number)+"/head")
	if _, err := r.real.Run(r.ctx, gitCmd(dir, "rebase", "--onto", head, claimed)); err == nil {
		r.t.Fatal("the replay of the fixer's commits did not stop")
	}
	replayed = r.gitIn(dir, "rev-parse", "HEAD")
	if r.gitIn(dir, "rev-parse", replayed+"^") != head {
		r.t.Fatalf("HEAD %s during the replay is no replayed commit on the PR's new head %s", replayed, head)
	}
	return replayed, orig
}

// kept reports whether clone keeps commit sha of PR number's fix checkout
// under its own ref (fixKeptRef).
func (r *copiesRig) kept(clone string, number int, sha string) bool {
	r.t.Helper()
	return r.ref(clone, fixKeptRef(number, sha)) == sha
}

// A replay left in progress (`magnum fix-sync` killed after it replayed
// one commit onto the PR's new head and stopped on the next) holds the
// fixer's commits in two places: HEAD, the replayed commit, and the
// rebase's original tip, which only the rebase's state and the reflog name.
// The release of a fix copy (whose abort puts HEAD back on that tip, then
// whose reset moves it to the base), the removal of a per-PR checkout
// (which deletes the rebase's state and the reflog with the worktree) and a
// claim's switch (whose abort runs after its guard) keep both under
// refs/magnum/factory-kept/<N>/<sha>. Only HEAD was kept, so the original
// tip, with the commit that stopped the replay, was lost (C1 of T3b's
// third check).
func TestARebaseLeftInProgressKeepsItsReplayedAndItsOriginalTip(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	fc := r.claimFix(w)
	replayed, orig := r.stopRebase(r.origin, 7, fc.Path, fc.Head)
	r.releaseFix(w)
	if sl := r.slot(fc.Name); sl.State != store.SlotFree || sl.HoldReason != nil {
		t.Fatalf("%s after the release = %s, hold %q, want free", fc.Name, sl.State, store.Deref(sl.HoldReason))
	}
	if !r.kept(r.main, 7, replayed) || !r.kept(r.main, 7, orig) {
		t.Fatalf("the fix copy's release kept %q, want the replayed %s and the original tip %s", r.keptRefs(r.main), replayed, orig)
	}
	if !r.eventsSay(fc.Name, "factory.fix_kept", fixKeptRef(7, orig)) {
		t.Fatalf("no factory.fix_kept event names %s", fixKeptRef(7, orig))
	}

	// A per-PR checkout's removal.
	shop := r.addShop()
	r.pushPR(shop, 5, "fix.txt", "v1\n")
	ws := r.prWork("example/shop", 5)
	pc := r.claimFix(ws)
	replayed, orig = r.stopRebase(shop, 5, pc.Path, pc.Head)
	r.releaseFix(ws)
	if sl := r.slot(pc.Name); sl.State != store.SlotRemoved || fsx.Exists(pc.Path) {
		t.Fatalf("%s after the release = %s, want removed", pc.Name, sl.State)
	}
	clone := filepath.Join(r.root, "shop")
	if !r.kept(clone, 5, replayed) || !r.kept(clone, 5, orig) {
		t.Fatalf("the per-PR checkout's removal kept %q, want the replayed %s and the original tip %s", r.keptRefs(clone), replayed, orig)
	}

	// A claim's switch to the PR's newer head.
	fc = r.claimFix(r.work(w.ID))
	replayed, orig = r.stopRebase(r.origin, 7, fc.Path, fc.Head)
	newer := r.pushPR(r.origin, 7, "other.txt", "v1\n")
	if again := r.claimFix(r.work(w.ID)); again.Name != fc.Name || again.Head != newer {
		t.Fatalf("the claim at the newer head = %+v, want %s at %s", again, fc.Name, newer)
	}
	if got, _ := r.head(fc.Path); got != newer {
		t.Fatalf("%s is at %s after the claim, want the newer head %s", fc.Name, got, newer)
	}
	if !r.kept(r.main, 7, replayed) || !r.kept(r.main, 7, orig) {
		t.Fatalf("the claim's switch kept %q, want the replayed %s and the original tip %s", r.keptRefs(r.main), replayed, orig)
	}
}

// A per-PR checkout goes once the PR's fix work ends: at once when the PR
// merges or closes, once its work stayed idle for fixIdleEnd, never while
// the work goes on. Its worktree, its fix ref and its row's hold on it go,
// with a fix.release event. A change your typing in the fix's panes
// explains holds it until it is gone, and the removal is tried again after
// claimBackoff; a commit no longer holds it (C1, C2 and C7 on T3b).
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

	// Your file, after you typed in the fix's panes, holds the checkout of
	// the merged PR.
	notes := filepath.Join(paths[5], "notes.txt")
	r.write(notes, "mine\n")
	r.setTyped(works[5])
	h.tick()
	if removed(5) {
		t.Fatal("the removal lost your file")
	}
	if evs := h.events(0, "fix.release"); len(evs) != 1 || evs[0].Phase != registry.PhaseFail || !strings.Contains(evs[0].Text, "dirty_worktree") {
		t.Fatalf("fix.release events %+v, want the removal held over your file", evs)
	}
	if err := os.Remove(notes); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("fix.release events %+v, want the merged PR's removal once your file was gone", evs)
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
	if err != nil || !slices.Equal(res, []FixReservation{{WorkID: works[4].ID, Checkout: "fix-example/shop#4", PerPR: true, Repo: "example/shop", Number: 4}}) {
		t.Fatalf("FixReserved = %+v, %v, want only the checkout of the work that goes on", res, err)
	}

	// The work comes back: the PR gets a checkout again.
	if fc := r.claimFix(r.prWork("example/shop", 6)); fc.Path != paths[6] {
		t.Fatalf("the claim of work that came back = %+v, want its checkout at %s again", fc, paths[6])
	}
}

// harnessWork is the queued fix work of PR number of talkable/talkable in
// the harness's registry, cycle 1.
func harnessWork(h *harness, number int) registry.PRWork {
	h.t.Helper()
	w, err := h.reg.EnsurePRWork(h.ctx, "talkable/talkable", number)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.reg.UpdatePRWork(h.ctx, w.ID, nil, registry.PRWorkQueued, func(u *registry.PRWorkUpdate) { u.Set("cycle", 1) }); err != nil {
		h.t.Fatal(err)
	}
	if w, err = h.reg.PRWorkByID(h.ctx, w.ID); err != nil {
		h.t.Fatal(err)
	}
	return w
}

// A fix's claim that finds no checkout free is a wait that costs the fix
// nothing: the PR's fix is told "copy: all 2 busy", with a fix.claim fail
// event that says when it claims again, and no claim runs before
// claimBackoff; the next one runs after it. Any other error of a claim is
// the fix's failure, returned as it is, and the claim after it runs at
// once. No test set the fake's claim error (R12 on T3b).
func TestAFixClaimWaitsWhileNoCheckoutIsFreeAndFailsOnAnyOtherError(t *testing.T) {
	h := newHarness(t)
	w := harnessWork(h, 7)
	setClaimErr := func(err error) {
		h.copies.mu.Lock()
		h.copies.fix.claimErr = err
		h.copies.mu.Unlock()
	}
	claim := func() error {
		t.Helper()
		_, err := h.e.claimFix(h.ctx, w)
		return err
	}
	setClaimErr(noCopy{what: "all 2 busy"})
	if err := claim(); !errors.Is(err, ErrHeld) {
		t.Fatalf("the first claimFix = %v, want a wait while its claim runs", err)
	}
	h.e.waitBackground()
	if err := claim(); !errors.Is(err, ErrHeld) || err.Error() != "copy: all 2 busy" {
		t.Fatalf("claimFix once the claim found no checkout = %v, want the wait copy: all 2 busy", err)
	}
	evs := h.events(0, "fix.claim")
	if len(evs) != 2 || evs[1].Phase != registry.PhaseFail || !strings.Contains(evs[1].Text, "no checkout for talkable/talkable#7 yet: all 2 busy; claims again at") {
		t.Fatalf("fix.claim events %+v, want the begin and the wait", evs)
	}
	h.clock.Add(claimBackoff(1) - time.Second)
	if err := claim(); err == nil || err.Error() != "copy: all 2 busy" {
		t.Fatalf("claimFix inside the backoff = %v, want the wait", err)
	}
	h.e.waitBackground()
	if n := h.copies.fixClaims(); n != 1 {
		t.Fatalf("%d claims inside the backoff, want no new one", n)
	}

	setClaimErr(errors.New("the fetch of refs/pull/7/head failed"))
	h.clock.Add(time.Second)
	if err := claim(); !errors.Is(err, ErrHeld) {
		t.Fatalf("claimFix after the backoff = %v, want a wait while a second claim runs", err)
	}
	h.e.waitBackground()
	if n := h.copies.fixClaims(); n != 2 {
		t.Fatalf("%d claims after the backoff, want a second one", n)
	}
	if err := claim(); err == nil || errors.Is(err, ErrHeld) || err.Error() != "the fetch of refs/pull/7/head failed" {
		t.Fatalf("claimFix once the claim failed = %v, want its error as it is", err)
	}
	if evs := h.events(0, "fix.claim"); evs[len(evs)-1].Phase != registry.PhaseFail || evs[len(evs)-1].Text != "the fetch of refs/pull/7/head failed" {
		t.Fatalf("the last fix.claim event = %+v, want the failure", evs[len(evs)-1])
	}
	if err := claim(); !errors.Is(err, ErrHeld) {
		t.Fatalf("claimFix after a failure = %v, want a new claim at once", err)
	}
	h.e.waitBackground()
	if n := h.copies.fixClaims(); n != 3 {
		t.Fatalf("%d claims after the failure, want a third one at once", n)
	}
}

// The checkout of fix work idle since 9m59s stays, and goes at 10 minutes:
// fixIdleEnd is the 10 minutes README and DECISIONS name. A refused release
// is tried again after claimBackoff, never on the ticks before it. The test
// of the idle end moved the clock by fixIdleEnd itself and never ticked
// inside the backoff (R15 on T3b).
func TestAFixCheckoutOfIdleWorkGoesAtTenMinutesAndARefusedReleaseWaits(t *testing.T) {
	h := newHarness(t)
	w := harnessWork(h, 7)
	if err := h.reg.UpdatePRWork(h.ctx, w.ID, nil, registry.PRWorkIdle, nil); err != nil {
		t.Fatal(err)
	}
	released := func() int {
		h.copies.mu.Lock()
		defer h.copies.mu.Unlock()
		return len(h.copies.fix.released)
	}
	h.copies.mu.Lock()
	h.copies.fix.reserved = []FixReservation{{WorkID: w.ID, Checkout: "fix-talkable/talkable#7", PerPR: true, Repo: "talkable/talkable", Number: 7}}
	h.copies.fix.releaseErr = slots.ErrHold{Reason: slots.HoldDirtyWorktree, Detail: "notes.txt, and someone typed into PR #7's magnum panes"}
	h.copies.mu.Unlock()
	h.clock.Add(9*time.Minute + 59*time.Second)
	h.tick()
	if n := released(); n != 0 {
		t.Fatalf("%d releases of work idle for 9m59s, want none", n)
	}
	h.clock.Add(time.Second)
	h.tick()
	if n := released(); n != 1 {
		t.Fatalf("%d releases of work idle for 10 minutes, want one", n)
	}
	h.clock.Add(claimBackoff(1) - time.Second)
	h.tick()
	if n := released(); n != 1 {
		t.Fatalf("%d releases on a tick inside the backoff, want still one", n)
	}
	h.clock.Add(time.Second)
	h.tick()
	if n := released(); n != 2 {
		t.Fatalf("%d releases once the backoff passed, want two", n)
	}
}

// The fix copies are kept apart in repair and outage, as in claims: a fix
// claim that finds no fix copy free repairs its broken fix copy at the
// start, never a broken task copy it cannot take, and a broken fix copy
// neither makes the tasks' outage nor keeps one from them. No test broke a
// fix copy (R17 on T3b).
func TestAFixCopyIsKeptApartInRepairAndOutage(t *testing.T) {
	r := newFixRig(t, 1, 1)
	r.pushPR(r.origin, 7, "fix.txt", "v1\n")
	w := r.prWork("talkable/talkable", 7)
	r.breakAll("factory1")
	r.failSetup["factory2"] = 2
	if _, err := r.c.ClaimFix(r.ctx, w); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("the fix claim with its copy's setup failing = %v, want a wait", err)
	}
	if sl := r.slot("factory2"); sl.State != store.SlotBroken {
		t.Fatalf("factory2 = %s, want broken", sl.State)
	}
	setups := func(n int) int {
		return len(r.calls("bin/worktree-setup", filepath.Join(r.root, "talkable.factory"+strconv.Itoa(n))))
	}
	task, fix := setups(1), setups(2)
	r.c = r.newCopies() // a restart: each broken copy's start repair is owed
	if fc := r.claimFix(w); fc.Name != "factory2" {
		t.Fatalf("the fix claim after the restart took %s, want its repaired fix copy factory2", fc.Name)
	}
	if got := setups(2); got != fix+1 {
		t.Fatalf("factory2's setup ran %d more times, want once: its repair", got-fix)
	}
	if got := setups(1); got != task {
		t.Fatalf("the fix claim repaired the task copy factory1 (%d more setups), which it cannot take", got-task)
	}
	if sl := r.slot("factory1"); sl.State != store.SlotBroken {
		t.Fatalf("factory1 = %s, want still broken: its repair is a task claim's", sl.State)
	}

	o := newFixRig(t, 2, 1)
	o.setupStderr = "redis-server: command not found\n"
	o.pushPR(o.origin, 7, "fix.txt", "v1\n")
	o.breakAll("factory1", "factory2")
	o.failSetup["factory3"] = 2
	o.stderrFor["factory3"] = "createdb: could not connect to the server"
	if _, err := o.c.ClaimFix(o.ctx, o.prWork("talkable/talkable", 7)); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("the fix claim with its copy's setup failing = %v, want a wait", err)
	}
	outs, err := o.c.Outages(o.ctx)
	if err != nil || len(outs) != 1 || !strings.Contains(outs[0].Cause, "redis-server: command not found") {
		t.Fatalf("Outages with both task copies broken one way and the fix copy another = %+v, %v; want the tasks' outage", outs, err)
	}
}
