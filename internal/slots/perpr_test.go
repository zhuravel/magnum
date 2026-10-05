package slots

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

type perPRFixture struct {
	h       *harness
	watch   config.Watch
	repo    store.Repo
	cloneRt string
	main    string
	sha7    string
	pr      store.PR
}

func newPerPR(t *testing.T, clone bool) *perPRFixture {
	t.Helper()
	h := newHarness(t)
	fx := fixtures(t).widget
	f := &perPRFixture{h: h, sha7: fx.pr7}
	f.cloneRt = filepath.Join(h.root, "Projects")
	if err := os.MkdirAll(f.cloneRt, 0o755); err != nil {
		t.Fatal(err)
	}
	f.main = filepath.Join(f.cloneRt, "widget")
	if clone {
		copyRepo(t, fx.main, f.main)
	}
	f.watch = config.Watch{Owner: "zhuravel", Include: []string{"*"}, Identity: "zhuravel", CloneRoot: f.cloneRt}
	var err error
	f.repo, err = h.st.UpsertRepo(h.ctx, store.Repo{NodeID: "R_widget", Owner: "zhuravel", Name: "widget",
		WatchOwner: "zhuravel", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	f.pr = h.pr(f.repo.ID, 7, f.sha7, store.PRClaiming)
	return f
}

// ownOrigin moves the test onto a private, writable copy of the widget
// origin (the shared one is read-only) and returns a scratch clone that can
// push to it.
func (f *perPRFixture) ownOrigin(t *testing.T) (work string) {
	t.Helper()
	fx := fixtures(t).widget
	origin := filepath.Join(f.h.root, "remotes", "zhuravel", "widget.git")
	work = filepath.Join(f.h.root, "work-widget")
	copyRepo(t, fx.origin, origin)
	copyRepo(t, fx.work, work)
	repointOrigin(t, work, fx.origin, origin)
	if fsx.Exists(f.main) {
		repointOrigin(t, f.main, fx.origin, origin)
	}
	return work
}

// httpsClone is the start of a clone through gh's credential helper; an SSH
// clone starts with "git clone".
var httpsClone = []string{"git", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential", "clone"}

func isClone(c execx.Cmd) bool {
	return hasArgs(c, httpsClone[1:]) || hasArgs(c, []string{"clone"})
}

func hasArgs(c execx.Cmd, prefix []string) bool {
	return len(c.Args) >= len(prefix) && slices.Equal(c.Args[:len(prefix)], prefix)
}

// cloneCalls returns the recorded clone commands of either transport.
func (h *harness) cloneCalls() []execx.Cmd {
	return slices.Concat(h.fake.CallsWithPrefix(httpsClone...), h.fake.CallsWithPrefix("git", "clone"))
}

// fakeClone answers the code's `git clone` (either transport) with a copy of
// the widget main clone at the requested destination.
func (f *perPRFixture) fakeClone(t *testing.T) {
	t.Helper()
	fx := fixtures(t).widget
	f.h.run.fakeGit = isClone
	clone := func(c execx.Cmd) (execx.Result, error) {
		err := copyTree(fx.main, c.Args[len(c.Args)-1])
		if err != nil {
			t.Errorf("fake clone: %v", err)
		}
		return execx.Result{}, err
	}
	f.h.fake.Rules = append(f.h.fake.Rules,
		execx.Rule{Prefix: httpsClone, Fn: clone},
		execx.Rule{Prefix: []string{"git", "clone"}, Fn: clone})
}

func TestCreatePRWorktree(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatalf("CreatePRWorktree: %v", err)
	}
	wantPath := filepath.Join(f.cloneRt, "widget__worktrees", "pr-7")
	if sl.Kind != store.SlotKindPerPR || sl.State != store.SlotClaimed || sl.Path != wantPath || sl.MainClone != f.main {
		t.Fatalf("slot = %+v", sl)
	}
	if store.Deref(sl.PRID) != f.pr.ID || store.Deref(sl.CheckedOutSHA) != f.sha7 || sl.RepoFullName != "zhuravel/widget" {
		t.Fatalf("slot pr=%v sha=%v repo=%s", sl.PRID, store.Deref(sl.CheckedOutSHA), sl.RepoFullName)
	}
	if head := gitT(t, wantPath, "rev-parse", "HEAD"); head != f.sha7 {
		t.Fatalf("HEAD = %s", head)
	}
	add := h.run.gitCalls("worktree", "add")
	if len(add) != 1 || !add[0].Mutates || !slices.Equal(add[0].Args, []string{"-C", f.main, "worktree", "add", "--quiet", "--detach", wantPath, gitx.PRRef(7)}) {
		t.Fatalf("worktree add = %+v", add)
	}
	a, err := h.openAssignment(f.pr.ID)
	if err != nil || a.SlotID != sl.ID || store.Deref(a.HeadSHA) != f.sha7 {
		t.Fatalf("assignment = %+v, %v", a, err)
	}
	// No pool side effects for per-PR repos.
	if n := len(h.scriptCalls("")); n != 0 {
		t.Fatalf("ran %d mise commands", n)
	}
	// Idempotent.
	again, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil || again.ID != sl.ID {
		t.Fatalf("second CreatePRWorktree = %+v, %v", again, err)
	}
}

func TestCreatePRWorktreeOriginMismatch(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	// <clone_root>/gadget exists but its origin is zhuravel/widget.
	if err := os.Rename(f.main, filepath.Join(f.cloneRt, "gadget")); err != nil {
		t.Fatal(err)
	}
	_, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/gadget", f.pr, f.sha7)
	if !errors.Is(err, ErrOriginMismatch) {
		t.Fatalf("err = %v, want ErrOriginMismatch", err)
	}
	if n := len(h.run.gitCalls("worktree", "add")); n != 0 {
		t.Fatal("worktree added despite the mismatch")
	}
}

func TestCreatePRWorktreeClonesMissingRepo(t *testing.T) {
	f := newPerPR(t, false)
	h := f.h
	f.fakeClone(t)
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatalf("CreatePRWorktree: %v", err)
	}
	clones := h.cloneCalls()
	want := slices.Concat(httpsClone[1:], []string{"--quiet",
		"--config", "credential.helper=", "--config", "credential.helper=!gh auth git-credential",
		"--", "https://github.com/zhuravel/widget.git", f.main})
	if len(clones) != 1 || !clones[0].Mutates || !slices.Equal(clones[0].Args, want) {
		t.Fatalf("clone calls = %+v\nwant args %q", clones, want)
	}
	if sl.State != store.SlotClaimed {
		t.Fatalf("state = %s", sl.State)
	}
}

// siblingClone makes <clone_root>/<dir> a git repository whose origin is url.
func (f *perPRFixture) siblingClone(t *testing.T, dir, url string) {
	t.Helper()
	p := filepath.Join(f.cloneRt, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, p, "init", "--quiet")
	gitT(t, p, "remote", "add", "origin", url)
}

// A user whose clones of the owner go over SSH has working keys for it: a
// new clone follows them instead of switching that owner to gh.
func TestCreatePRWorktreeClonesOverSSHWhenTheOwnersClonesDo(t *testing.T) {
	f := newPerPR(t, false)
	h := f.h
	f.siblingClone(t, "gadget", "git@github.com:zhuravel/gadget.git")
	f.fakeClone(t)
	if _, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7); err != nil {
		t.Fatalf("CreatePRWorktree: %v", err)
	}
	clones := h.cloneCalls()
	want := []string{"clone", "--quiet", "--", "git@github.com:zhuravel/widget.git", f.main}
	if len(clones) != 1 || !slices.Equal(clones[0].Args, want) {
		t.Fatalf("clone calls = %+v, want args %q", clones, want)
	}
}

// Only the same owner's clones count: SSH clones of other owners, or an
// https clone of the owner, leave the new clone on https through gh.
func TestCreatePRWorktreeClonesOverHTTPSUnlessTheOwnerUsesSSH(t *testing.T) {
	f := newPerPR(t, false)
	h := f.h
	f.siblingClone(t, "other", "git@github.com:example/other.git")
	f.siblingClone(t, "gadget", "https://github.com/zhuravel/gadget.git")
	f.fakeClone(t)
	if _, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7); err != nil {
		t.Fatalf("CreatePRWorktree: %v", err)
	}
	clones := h.cloneCalls()
	if len(clones) != 1 || !hasArgs(clones[0], httpsClone[1:]) || !slices.Contains(clones[0].Args, "https://github.com/zhuravel/widget.git") {
		t.Fatalf("clone calls = %+v; want one https clone through gh", clones)
	}
}

func TestPerPRCheckoutAndRemove(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	work := f.ownOrigin(t)
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	// Re-review on a new head: Checkout works for per-PR slots without pool steps.
	gitT(t, work, "commit", "--quiet", "--allow-empty", "-m", "more")
	gitT(t, work, "push", "--quiet", "--force", "origin", "HEAD:refs/pull/7/head")
	newSHA := gitT(t, work, "rev-parse", "HEAD")
	if err := h.m.Checkout(h.ctx, sl, f.pr, config.Pool{}, newSHA); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != newSHA {
		t.Fatalf("HEAD = %s", head)
	}
	if a, err := h.openAssignment(f.pr.ID); err != nil || store.Deref(a.HeadSHA) != newSHA {
		t.Fatalf("assignment head = %v, %v; want the new checkout %s", a.HeadSHA, err, newSHA)
	}
	if n := len(h.scriptCalls("")); n != 0 {
		t.Fatalf("per-PR checkout ran mise (%d)", n)
	}

	// Untracked files are never deleted when a human typed into the PR's
	// panes since the checkout: held.
	notes := filepath.Join(sl.Path, "notes.txt")
	writeFile(t, notes, "scratch\n")
	h.humanTyped(f.pr.ID, h.now.Add(time.Minute))
	wantHold(t, h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false), HoldDirtyWorktree)
	if !fsx.Exists(notes) || len(h.run.gitCalls("worktree", "remove")) != 0 {
		t.Fatal("an untracked file was removed")
	}
	if got := h.slot(sl.Name); got.State != store.SlotClaimed || store.Deref(got.HoldReason) != HoldDirtyWorktree {
		t.Fatalf("state %s, hold %v; want claimed and held", got.State, got.HoldReason)
	}
	// Once the file is gone and the slot unpinned, a plain remove runs.
	if err := os.Remove(notes); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Unpin(h.ctx, h.slot(sl.Name)); err != nil {
		t.Fatal(err)
	}
	if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false); err != nil {
		t.Fatalf("RemovePRWorktree: %v", err)
	}
	rm := h.run.gitCalls("worktree", "remove")
	if len(rm) != 1 || slices.Contains(rm[0].Args, "--force") {
		t.Fatalf("worktree remove = %+v", rm)
	}
	if _, err := os.Stat(sl.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree dir still there: %v", err)
	}
	if _, err := gitx.New(h.run).RevParse(h.ctx, f.main, gitx.PRRef(7)); !errors.Is(err, gitx.ErrNoSuchRef) {
		t.Fatalf("PR ref not deleted: %v", err)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotRemoved || got.PRID != nil {
		t.Fatalf("slot = %+v", got)
	}
	if _, err := h.openAssignment(f.pr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("assignment still open: %v", err)
	}

	// Reopened PR: the removed row is reused.
	again, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, newSHA)
	if err != nil {
		t.Fatalf("re-create: %v", err)
	}
	if again.ID != sl.ID || again.State != store.SlotClaimed {
		t.Fatalf("re-created = %+v", again)
	}
	// Release of a per-PR slot removes it.
	if err := h.m.Release(h.ctx, again, config.Pool{}, "pr_closed"); err != nil {
		t.Fatalf("Release per-PR: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoved {
		t.Fatalf("state = %s", got.State)
	}
}

// Tracked changes after a human typed into the PR's panes hold a removal.
func TestRemovePRWorktreeRefusesTrackedChanges(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	h.m = h.newManager(Deps{Repos: []config.Repo{{Repo: f.repoName(), Teardown: []string{"bin/teardown"}}}})
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(sl.Path, "README")
	writeFile(t, readme, "edited\n")
	h.humanTyped(f.pr.ID, h.now.Add(time.Minute))
	h.run.reset()
	wantHold(t, h.m.RemovePRWorktree(h.ctx, sl, false), HoldDirtyWorktree)
	if got := h.slot(sl.Name); got.State != store.SlotClaimed {
		t.Fatalf("state = %s", got.State)
	}
	if !fsx.Exists(sl.Path) || readFile(t, readme) != "edited\n" {
		t.Fatal("the worktree or its change is gone")
	}
	if rm := h.run.gitCalls("worktree", "remove"); len(rm) != 0 {
		t.Fatalf("worktree remove ran: %v", rm)
	}
	if n := len(h.scriptCalls("bin/teardown")); n != 0 {
		t.Fatalf("teardown ran %d times before the guard", n)
	}
	if err := h.m.RemovePRWorktree(h.ctx, sl, true); err != nil {
		t.Fatalf("forced: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoved || fsx.Exists(sl.Path) {
		t.Fatalf("state = %s, dir exists %v", got.State, fsx.Exists(sl.Path))
	}
	if n := len(h.scriptCalls("bin/teardown")); n != 1 {
		t.Fatalf("forced removal ran teardown %d times, want 1", n)
	}
	h.wantDiscarded(t, sl.Name, "1 tracked change(s)", "README")
}

// The slot.discarded event counts everything and names the first maxListed
// paths.
func TestDiscardedEventListsTheFirstPaths(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, "README"), "residue\n")
	for i := range maxListed + 5 {
		writeFile(t, filepath.Join(sl.Path, fmt.Sprintf("log-%02d.txt", i)), "x\n")
	}
	if err := h.m.RemovePRWorktree(h.ctx, sl, false); err != nil {
		t.Fatalf("RemovePRWorktree over residue: %v", err)
	}
	h.wantDiscarded(t, sl.Name, "1 tracked change(s) and 25 untracked file(s)", "README", "log-00.txt", "log-18.txt", "(+6 more)")
	if msg := h.discarded(t, sl.Name)[0].Message; strings.Contains(msg, "log-19.txt") {
		t.Fatalf("event names more than %d paths: %q", maxListed, msg)
	}
}

// fakeOrigins answers `git remote get-url origin` for the given directories
// (the fixture's real origins are local paths, which FindClone never
// matches) and passes every other git command to the real runner.
func fakeOrigins(h *harness, urls map[string]string) {
	h.run.fakeGit = func(c execx.Cmd) bool {
		return len(c.Args) >= 4 && c.Args[0] == "-C" && c.Args[2] == "remote" && c.Args[3] == "get-url" && urls[c.Args[1]] != ""
	}
	h.fake.Rules = append(h.fake.Rules, execx.Rule{Prefix: []string{"git", "-C"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{Stdout: []byte(urls[c.Args[1]] + "\n")}, nil
	}})
}

func TestCreatePRWorktreeFindsCloneUnderAnotherName(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	// The user's clone is ~/Projects/zhuravel-widget; ~/Projects/widget is
	// an unrelated folder that is not a git repository.
	renamed := filepath.Join(f.cloneRt, "zhuravel-widget")
	if err := os.Rename(f.main, renamed); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.cloneRt, "widget", "notes.txt"), "not a repo\n")
	fakeOrigins(h, map[string]string{renamed: "git@github.com:zhuravel/widget.git"})

	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatalf("CreatePRWorktree: %v", err)
	}
	wantPath := filepath.Join(f.cloneRt, "zhuravel-widget__worktrees", "pr-7")
	if sl.MainClone != renamed || sl.Path != wantPath || sl.State != store.SlotClaimed {
		t.Fatalf("slot = %+v", sl)
	}
	if head := gitT(t, wantPath, "rev-parse", "HEAD"); head != f.sha7 {
		t.Fatalf("HEAD = %s", head)
	}
	if n := len(h.cloneCalls()); n != 0 {
		t.Fatalf("cloned %d times although a clone exists", n)
	}
	for _, c := range h.run.calls {
		if c.Name == "git" && len(c.Args) > 1 && c.Args[1] == filepath.Join(f.cloneRt, "widget") {
			t.Fatalf("git ran in the non-repository folder: %v", c.Args)
		}
	}
}

func TestCreatePRWorktreeClonesBesideAFolderThatIsNotARepo(t *testing.T) {
	f := newPerPR(t, false)
	h := f.h
	writeFile(t, filepath.Join(f.cloneRt, "widget", "notes.txt"), "not a repo\n")
	f.fakeClone(t)
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatalf("CreatePRWorktree: %v", err)
	}
	dest := filepath.Join(f.cloneRt, "zhuravel-widget")
	clones := h.cloneCalls()
	if len(clones) != 1 || clones[0].Args[len(clones[0].Args)-1] != dest {
		t.Fatalf("clone calls = %+v, want one into %s", clones, dest)
	}
	if sl.MainClone != dest || sl.Path != filepath.Join(f.cloneRt, "zhuravel-widget__worktrees", "pr-7") {
		t.Fatalf("slot = %+v", sl)
	}
}

func TestMainClonePathAndPRWorktreePath(t *testing.T) {
	f := newPerPR(t, false)
	h := f.h
	got, found, err := h.m.MainClonePath(h.ctx, f.watch, "zhuravel/widget")
	if err != nil || found || got != filepath.Join(f.cloneRt, "widget") {
		t.Fatalf("no clone: %q %v %v", got, found, err)
	}
	if _, _, err := h.m.MainClonePath(h.ctx, f.watch, "widget"); err == nil {
		t.Fatal("a repository without owner must be refused")
	}
	if p := PRWorktreePath("/p/zhuravel-widgets", 7); p != "/p/zhuravel-widgets__worktrees/pr-7" {
		t.Fatalf("PRWorktreePath = %s", p)
	}
	mc, p := PRWorktreePaths(config.Watch{CloneRoot: "/p"}, "zhuravel/widgets", 3)
	if mc != "/p/widgets" || p != "/p/widgets__worktrees/pr-3" {
		t.Fatalf("PRWorktreePaths = %s %s", mc, p)
	}
}

func TestCreatePRWorktreeMovesAStaleRowToTheFoundClone(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	renamed := filepath.Join(f.cloneRt, "zhuravel-widget")
	if err := os.Rename(f.main, renamed); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.cloneRt, "widget", "notes.txt"), "not a repo\n")
	fakeOrigins(h, map[string]string{renamed: "git@github.com:zhuravel/widget.git"})
	// A row an earlier version left behind, pointing at the folder.
	repoID := f.repo.ID
	if _, err := h.st.CreateSlot(h.ctx, store.Slot{Name: PRSlotName("zhuravel/widget", 7), RepoID: &repoID, RepoFullName: "zhuravel/widget",
		Kind: store.SlotKindPerPR, Path: filepath.Join(f.cloneRt, "widget__worktrees", "pr-7"), MainClone: filepath.Join(f.cloneRt, "widget"),
		State: store.SlotProvisioning, LastError: store.Ptr("slots: clone origin does not match")}); err != nil {
		t.Fatal(err)
	}
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatalf("CreatePRWorktree: %v", err)
	}
	if sl.MainClone != renamed || sl.Path != filepath.Join(f.cloneRt, "zhuravel-widget__worktrees", "pr-7") || sl.State != store.SlotClaimed {
		t.Fatalf("slot = %+v", sl)
	}
}
