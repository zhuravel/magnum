package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// realEnv is a hermetic git environment: no user or system config, fixed
// identity, English messages.
func realEnv(t *testing.T) *execx.Real {
	t.Helper()
	return &execx.Real{BaseEnv: []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"LC_ALL=C",
	}}
}

// isExit1 reports whether err is git answering "no" (exit status 1).
func isExit1(err error) bool {
	var ee *execx.ExitError
	return errors.As(err, &ee) && ee.Code == 1
}

// fixture is a throwaway origin repository and a clone of it.
type fixture struct {
	t      *testing.T
	r      *execx.Real
	c      *Client
	origin string
	clone  string
	base   string // c1, the initial commit on main
	pr     string // c2, head of PR 7 (adds db/schema.rb and app/a.rb)
}

func (fx *fixture) git(dir string, args ...string) string {
	fx.t.Helper()
	res, err := fx.r.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)})
	if err != nil {
		fx.t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return res.Out()
}

// status counts dir's changes through gitx.
func (fx *fixture) status(dir string) Status {
	fx.t.Helper()
	st, err := fx.c.Status(context.Background(), dir)
	if err != nil {
		fx.t.Fatalf("Status(%s): %v", dir, err)
	}
	return st
}

func (fx *fixture) write(dir, rel, content string) {
	fx.t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) commit(dir, rel, content, msg string) string {
	fx.t.Helper()
	fx.write(dir, rel, content)
	fx.git(dir, "add", "--", rel)
	fx.git(dir, "commit", "--quiet", "-m", msg)
	return fx.git(dir, "rev-parse", "HEAD")
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	r := realEnv(t)
	if _, err := r.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"--version"}}); err != nil {
		t.Skipf("git not available: %v", err)
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved // git reports real paths; macOS temp dirs are symlinks
	}
	fx := &fixture{t: t, r: r, c: New(r), origin: filepath.Join(root, "origin"), clone: filepath.Join(root, "clone")}

	if err := os.MkdirAll(fx.origin, 0o755); err != nil {
		t.Fatal(err)
	}
	fx.git(fx.origin, "init", "--quiet", "-b", "main")
	fx.base = fx.commit(fx.origin, "README.md", "hello\n", "c1")

	fx.git(fx.origin, "checkout", "--quiet", "-b", "pr7")
	fx.write(fx.origin, "app/a.rb", "a = 1\n")
	fx.write(fx.origin, "db/schema.rb", "version 1\n")
	fx.git(fx.origin, "add", ".")
	fx.git(fx.origin, "commit", "--quiet", "-m", "c2 pr7")
	fx.pr = fx.git(fx.origin, "rev-parse", "HEAD")
	fx.git(fx.origin, "update-ref", "refs/pull/7/head", fx.pr)
	fx.git(fx.origin, "checkout", "--quiet", "main")

	fx.git(root, "clone", "--quiet", fx.origin, fx.clone)
	return fx
}

func (fx *fixture) fetchPR7() {
	fx.t.Helper()
	got, err := fx.c.FetchPR(context.Background(), fx.clone, 7)
	if err != nil || got != fx.pr {
		fx.t.Fatalf("FetchPR = %q, %v; want %s", got, err, fx.pr)
	}
}

func TestRealFetchPRAndBranch(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetchPR7()

	if got, err := fx.c.RevParse(ctx, fx.clone, PRRef(7)); err != nil || got != fx.pr {
		t.Fatalf("refs/magnum/pr/7 = %q, %v", got, err)
	}

	// Force-push the PR: replace its head with a commit that is NOT a
	// descendant of the old head. A re-fetch must still move the ref (the
	// refspec has '+'); without it git would refuse the non-fast-forward.
	fx.git(fx.origin, "checkout", "--quiet", "pr7")
	fx.git(fx.origin, "reset", "--quiet", "--hard", fx.base)
	amended := fx.commit(fx.origin, "app/a.rb", "a = 2\n", "c2b force-pushed")
	fx.git(fx.origin, "update-ref", "refs/pull/7/head", amended)
	fx.git(fx.origin, "checkout", "--quiet", "main")
	if _, err := fx.r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", fx.origin, "merge-base", "--is-ancestor", fx.pr, amended}}); !isExit1(err) {
		t.Fatalf("the replacement head must not descend from the old one (merge-base --is-ancestor: %v)", err)
	}
	if got, err := fx.c.FetchPR(ctx, fx.clone, 7); err != nil || got != amended {
		t.Fatalf("re-fetch = %q, %v; want %s", got, err, amended)
	}
	if _, err := fx.r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", fx.clone, "merge-base", "--is-ancestor", fx.pr, PRRef(7)}}); !isExit1(err) {
		t.Fatalf("the ref moved, but not by a fast-forward (merge-base --is-ancestor: %v)", err)
	}

	// Base branch moves on origin; FetchBranch updates origin/main.
	c3 := fx.commit(fx.origin, "db/other.rb", "x\n", "c3 main moves")
	if err := fx.c.FetchBranch(ctx, fx.clone, "main"); err != nil {
		t.Fatal(err)
	}
	if got, _ := fx.c.RevParse(ctx, fx.clone, "origin/main"); got != c3 {
		t.Fatalf("origin/main = %s, want %s", got, c3)
	}

	// Unknown PR fails with context.
	if _, err := fx.c.FetchPR(ctx, fx.clone, 99); err == nil || !strings.Contains(err.Error(), "PR 99") {
		t.Fatalf("missing PR ref: %v", err)
	}

	// Ref cleanup is idempotent.
	for i := 0; i < 2; i++ {
		if err := fx.c.UpdateRefDelete(ctx, fx.clone, PRRef(7)); err != nil {
			t.Fatalf("UpdateRefDelete #%d: %v", i, err)
		}
	}
	if _, err := fx.c.RevParse(ctx, fx.clone, PRRef(7)); !errors.Is(err, ErrNoSuchRef) {
		t.Fatalf("deleted ref must be ErrNoSuchRef, got %v", err)
	}
}

func TestRealMergeBaseAndChangedPaths(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetchPR7()
	// main moves on and touches db/ too; that must NOT show up as a PR change.
	fx.commit(fx.origin, "db/other.rb", "x\n", "c3 main moves")
	if err := fx.c.FetchBranch(ctx, fx.clone, "main"); err != nil {
		t.Fatal(err)
	}

	mb, err := fx.c.MergeBase(ctx, fx.clone, "origin/main", PRRef(7))
	if err != nil || mb != fx.base {
		t.Fatalf("MergeBase = %q, %v; want %s", mb, err, fx.base)
	}

	got, err := fx.c.ChangedPaths(ctx, fx.clone, "origin/main", PRRef(7), "db/")
	if err != nil || !slices.Equal(got, []string{"db/schema.rb"}) {
		t.Fatalf("ChangedPaths(db/) = %q, %v", got, err)
	}
	got, err = fx.c.ChangedPaths(ctx, fx.clone, "origin/main", PRRef(7))
	if err != nil || !slices.Equal(got, []string{"app/a.rb", "db/schema.rb"}) {
		t.Fatalf("ChangedPaths() = %q, %v", got, err)
	}
	got, err = fx.c.ChangedPaths(ctx, fx.clone, "origin/main", PRRef(7), "nothing/")
	if err != nil || len(got) != 0 {
		t.Fatalf("ChangedPaths(nothing/) = %q, %v", got, err)
	}
	// Exclude pathspecs work (argv-safe pathspec magic).
	got, err = fx.c.ChangedPaths(ctx, fx.clone, "origin/main", PRRef(7), ".", ":(exclude)db")
	if err != nil || !slices.Equal(got, []string{"app/a.rb"}) {
		t.Fatalf("ChangedPaths(exclude) = %q, %v", got, err)
	}

	// Unrelated histories have no merge base.
	fx.git(fx.clone, "checkout", "--quiet", "--orphan", "orphan")
	fx.git(fx.clone, "rm", "-rf", "--quiet", ".")
	orphan := fx.commit(fx.clone, "z.txt", "z\n", "orphan")
	if _, err := fx.c.MergeBase(ctx, fx.clone, orphan, PRRef(7)); !errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("want ErrNoMergeBase, got %v", err)
	}
}

func TestRealWorktreeLifecycle(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetchPR7()
	wt := filepath.Join(filepath.Dir(fx.clone), "clone__worktrees", "pr-7") // parent dir does not exist yet

	if err := fx.c.WorktreeAdd(ctx, fx.clone, wt, PRRef(7), true, ""); err != nil {
		t.Fatal(err)
	}
	list, err := fx.c.WorktreeList(ctx, fx.clone)
	if err != nil || len(list) != 2 {
		t.Fatalf("WorktreeList = %+v, %v", list, err)
	}
	if list[0].Path != fx.clone || list[0].Branch != "main" || list[0].Detached || list[0].Head != fx.base {
		t.Errorf("main worktree parsed wrong: %+v", list[0])
	}
	got, ok := FindWorktree(list, wt)
	if !ok || !got.Detached || got.Branch != "" || got.Head != fx.pr || got.Locked || got.Prunable || got.Bare {
		t.Errorf("per-PR worktree parsed wrong: %+v ok=%v", got, ok)
	}
	// FindWorktree tolerates a non-canonical spelling of the same path.
	if _, ok := FindWorktree(list, wt+"/../pr-7"); !ok {
		t.Error("FindWorktree should clean the path")
	}

	// A second add of the same path fails (callers skip listed paths).
	if err := fx.c.WorktreeAdd(ctx, fx.clone, wt, PRRef(7), true, ""); err == nil {
		t.Error("adding an existing worktree path must fail")
	}

	// Clean tree, then dirt of each kind.
	if st := fx.status(wt); st.Dirty() {
		t.Fatalf("fresh worktree dirty: %+v", st)
	}
	fx.write(wt, "scratch/notes.txt", "n\n")
	if st := fx.status(wt); !st.Dirty() || !st.UntrackedOnly() {
		t.Fatalf("untracked file must make the tree dirty, untracked-only: %+v", st)
	}
	fx.write(wt, "app/a.rb", "changed\n")
	if st := fx.status(wt); st.Tracked != 1 || st.Untracked != 1 || st.UntrackedOnly() {
		t.Fatalf("Status = %+v", st)
	}

	// Non-forced removal refuses a dirty tree.
	if err := fx.c.WorktreeRemove(ctx, fx.clone, wt, false); err == nil {
		t.Fatal("remove of a dirty worktree without force must fail")
	}

	// SwitchDetach discards tracked changes and moves HEAD (keeps untracked).
	if err := fx.c.SwitchDetach(ctx, wt, "origin/main"); err != nil {
		t.Fatal(err)
	}
	if head, _ := fx.c.RevParse(ctx, wt, "HEAD"); head != fx.base {
		t.Fatalf("HEAD = %s, want %s", head, fx.base)
	}
	if st := fx.status(wt); st.Tracked != 0 || st.Untracked != 1 {
		t.Fatalf("after switch: %+v", st)
	}
	if b := fx.git(wt, "branch", "--show-current"); b != "" {
		t.Fatalf("detached HEAD has branch %q", b)
	}
	if err := fx.c.SwitchDetach(ctx, wt, PRRef(7)); err != nil {
		t.Fatal(err)
	}
	if head, _ := fx.c.RevParse(ctx, wt, "HEAD"); head != fx.pr {
		t.Fatalf("HEAD = %s, want PR head %s", head, fx.pr)
	}

	// Untracked-only dirt: plain remove fails, forced remove works.
	if err := fx.c.WorktreeRemove(ctx, fx.clone, wt, false); err == nil {
		t.Fatal("untracked-only worktree must need force")
	}
	if err := fx.c.WorktreeRemove(ctx, fx.clone, wt, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists: %v", err)
	}
	if list, _ := fx.c.WorktreeList(ctx, fx.clone); len(list) != 1 {
		t.Fatalf("worktree still listed: %+v", list)
	}
}

func TestRealWorktreeListLockedAndPrunable(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	root := filepath.Dir(fx.clone)
	locked := filepath.Join(root, "locked")
	gone := filepath.Join(root, "gone")
	for _, p := range []string{locked, gone} {
		if err := fx.c.WorktreeAdd(ctx, fx.clone, p, "origin/main", true, ""); err != nil {
			t.Fatal(err)
		}
	}
	fx.git(fx.clone, "worktree", "lock", "--reason", "being used", locked)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	list, err := fx.c.WorktreeList(ctx, fx.clone)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := FindWorktree(list, locked)
	if !ok || !l.Locked || l.LockReason != "being used" || l.Prunable {
		t.Errorf("locked: %+v ok=%v", l, ok)
	}
	g, ok := FindWorktree(list, gone)
	if !ok {
		t.Fatalf("missing-dir worktree must still be listed: %+v", list)
	}
	if !g.Prunable {
		t.Errorf("missing directory should be prunable: %+v", g)
	}

	if err := fx.c.WorktreePrune(ctx, fx.clone); err != nil {
		t.Fatal(err)
	}
	list, _ = fx.c.WorktreeList(ctx, fx.clone)
	if _, ok := FindWorktree(list, gone); ok {
		t.Error("prune must drop the stale entry")
	}
	if _, ok := FindWorktree(list, locked); !ok {
		t.Error("prune must keep live worktrees")
	}
}

func TestRealPlaceholderReset(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	slotPath := filepath.Join(filepath.Dir(fx.clone), "clone.review1")

	if err := fx.c.WorktreeAdd(ctx, fx.clone, slotPath, "origin/main", false, "review1"); err != nil {
		t.Fatal(err)
	}
	list, _ := fx.c.WorktreeList(ctx, fx.clone)
	if w, ok := FindWorktree(list, slotPath); !ok || w.Branch != "review1" || w.Detached {
		t.Fatalf("slot worktree: %+v ok=%v", w, ok)
	}
	// -b with --no-track: no upstream configured for the placeholder.
	if res, err := fx.r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", fx.clone, "config", "--get", "branch.review1.remote"}}); err == nil {
		t.Fatalf("placeholder must not track, got remote %q", res.Out())
	}

	// Review a PR in the slot, leave a stray local commit and dirt, then release.
	fx.fetchPR7()
	if err := fx.c.SwitchDetach(ctx, slotPath, PRRef(7)); err != nil {
		t.Fatal(err)
	}
	if n, err := fx.c.Unpushed(ctx, slotPath); err != nil || n != 0 {
		t.Fatalf("HEAD at the fetched PR ref is not unpushed: %d, %v", n, err)
	}
	fx.commit(slotPath, "stray.txt", "s\n", "human commit on top")
	if n, err := fx.c.Unpushed(ctx, slotPath); err != nil || n != 1 {
		t.Fatalf("Unpushed = %d, %v; want 1", n, err)
	}
	fx.write(slotPath, "app/a.rb", "dirty\n")

	c3 := fx.commit(fx.origin, "db/other.rb", "x\n", "c3 main moves")
	if err := fx.c.FetchBranch(ctx, fx.clone, "main"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // repeating must be harmless (crash-safe step)
		if err := fx.c.ResetPlaceholder(ctx, slotPath, "review1", "main"); err != nil {
			t.Fatalf("ResetPlaceholder #%d: %v", i, err)
		}
	}
	if b := fx.git(slotPath, "branch", "--show-current"); b != "review1" {
		t.Fatalf("branch = %q", b)
	}
	if head, _ := fx.c.RevParse(ctx, slotPath, "HEAD"); head != c3 {
		t.Fatalf("HEAD = %s want origin/main %s", head, c3)
	}
	if st := fx.status(slotPath); st.Tracked != 0 {
		t.Fatalf("reset must discard tracked changes: %+v", st)
	}
	if n, _ := fx.c.Unpushed(ctx, slotPath); n != 0 {
		t.Fatalf("Unpushed after reset = %d", n)
	}
	if res, err := fx.r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", fx.clone, "config", "--get", "branch.review1.remote"}}); err == nil {
		t.Fatalf("--no-track must leave no upstream, got remote %q", res.Out())
	}

	// Branch deletion: refused while checked out, then fine, then idempotent.
	if err := fx.c.BranchDelete(ctx, fx.clone, "review1", true); err == nil {
		t.Fatal("deleting a checked-out branch must fail")
	}
	if err := fx.c.SwitchDetach(ctx, slotPath, "HEAD"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := fx.c.BranchDelete(ctx, fx.clone, "review1", true); err != nil {
			t.Fatalf("BranchDelete #%d: %v", i, err)
		}
	}
	if _, err := fx.c.RevParse(ctx, fx.clone, "refs/heads/review1"); !errors.Is(err, ErrNoSuchRef) {
		t.Fatalf("branch should be gone: %v", err)
	}
}

func TestRealBranchPR(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.git(fx.clone, "config", "branch.bohdan/PR-1.fix.pr", "11483")
	if n, ok, err := fx.c.BranchPR(ctx, fx.clone, "bohdan/PR-1.fix"); err != nil || !ok || n != 11483 {
		t.Fatalf("BranchPR = %d %v %v", n, ok, err)
	}
	if n, ok, err := fx.c.BranchPR(ctx, fx.clone, "main"); err != nil || ok || n != 0 {
		t.Fatalf("unset: %d %v %v", n, ok, err)
	}
}

func TestRealRemoteAndClone(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	if got, err := fx.c.RemoteURL(ctx, fx.clone); err != nil || got != fx.origin {
		t.Fatalf("RemoteURL = %q, %v; want %s", got, err, fx.origin)
	}
	dest := filepath.Join(filepath.Dir(fx.clone), "second", "clone2")
	if err := fx.c.Clone(ctx, fx.origin, dest); err != nil {
		t.Fatal(err)
	}
	if head, err := fx.c.RevParse(ctx, dest, "HEAD"); err != nil || head != fx.base {
		t.Fatalf("clone HEAD = %q, %v", head, err)
	}
}

func TestRealStatusIgnoresIgnoredFiles(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.commit(fx.clone, ".gitignore", ".mise.local.toml\ntmp/\n", "ignore")
	fx.write(fx.clone, ".mise.local.toml", "[env]\n")
	fx.write(fx.clone, "tmp/.worktree-db-slug", "x\n")
	if st := fx.status(fx.clone); st.Dirty() {
		t.Fatalf("ignored files must not count as dirt: %+v", st)
	}
	if entries, err := fx.c.StatusPaths(ctx, fx.clone); err != nil || len(entries) != 0 {
		t.Fatalf("StatusPaths must not list ignored files: %+v, %v", entries, err)
	}
}

// A symbolic ref under refs/magnum/ must never lead to a deletion of the ref
// it points at.
func TestRealUpdateRefDeleteSymbolicRef(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetchPR7()

	// Pointing outside refs/magnum/: refused, main survives.
	fx.git(fx.clone, "symbolic-ref", PRRef(70), "refs/heads/main")
	if err := fx.c.UpdateRefDelete(ctx, fx.clone, PRRef(70)); err == nil {
		t.Fatal("a symbolic ref to a branch must be refused")
	}
	if got, err := fx.c.RevParse(ctx, fx.clone, "refs/heads/main"); err != nil || got != fx.base {
		t.Fatalf("main = %q, %v; want %s: the branch must survive", got, err, fx.base)
	}

	// Pointing inside refs/magnum/: only the symbolic ref goes.
	fx.git(fx.clone, "symbolic-ref", PRRef(71), PRRef(7))
	if err := fx.c.UpdateRefDelete(ctx, fx.clone, PRRef(71)); err != nil {
		t.Fatalf("symbolic ref within refs/magnum/: %v", err)
	}
	if got, err := fx.c.RevParse(ctx, fx.clone, PRRef(7)); err != nil || got != fx.pr {
		t.Fatalf("the target %s = %q, %v must survive", PRRef(7), got, err)
	}
	if _, err := fx.r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", fx.clone, "symbolic-ref", "-q", PRRef(71)}}); err == nil {
		t.Fatal("the symbolic ref itself should be gone")
	}

	// A dangling symbolic ref inside refs/magnum/ is removable too.
	fx.git(fx.clone, "symbolic-ref", PRRef(72), "refs/magnum/pr/never-created")
	if err := fx.c.UpdateRefDelete(ctx, fx.clone, PRRef(72)); err != nil {
		t.Fatalf("dangling symbolic ref: %v", err)
	}
}

// A placeholder that tracks a remote branch loses the upstream on reset
// (switch --no-track only stops git from creating one).
func TestRealPlaceholderResetUnsetsUpstream(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	slotPath := filepath.Join(filepath.Dir(fx.clone), "clone.review1")
	if err := fx.c.WorktreeAdd(ctx, fx.clone, slotPath, "origin/main", false, "review1"); err != nil {
		t.Fatal(err)
	}
	fx.git(fx.clone, "branch", "--set-upstream-to=origin/main", "review1")
	if got := fx.git(fx.clone, "config", "--get", "branch.review1.merge"); got != "refs/heads/main" {
		t.Fatalf("setup: merge = %q", got)
	}

	for i := 0; i < 2; i++ { // idempotent: the second run has nothing to unset
		if err := fx.c.ResetPlaceholder(ctx, slotPath, "review1", "main"); err != nil {
			t.Fatalf("ResetPlaceholder #%d: %v", i, err)
		}
		for _, key := range []string{"merge", "remote"} {
			if res, err := fx.r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", fx.clone, "config", "--get", "branch.review1." + key}}); err == nil {
				t.Fatalf("run #%d: branch.review1.%s = %q must be unset", i, key, res.Out())
			}
		}
	}
	if head, _ := fx.c.RevParse(ctx, slotPath, "HEAD"); head != fx.base {
		t.Fatalf("HEAD = %s, want origin/main %s", head, fx.base)
	}
}

func TestRealUnpushedRef(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetchPR7()
	if n, err := fx.c.UnpushedRef(ctx, fx.clone, "main"); err != nil || n != 0 {
		t.Fatalf("main is on origin: %d, %v", n, err)
	}
	if n, err := fx.c.UnpushedRef(ctx, fx.clone, PRRef(7)); err != nil || n != 0 {
		t.Fatalf("a fetched PR ref is not unpushed: %d, %v", n, err)
	}
	fx.git(fx.clone, "checkout", "--quiet", "-b", "wip")
	fx.commit(fx.clone, "wip1.txt", "1\n", "wip 1")
	fx.commit(fx.clone, "wip2.txt", "2\n", "wip 2")
	fx.git(fx.clone, "checkout", "--quiet", "main")
	if n, err := fx.c.UnpushedRef(ctx, fx.clone, "wip"); err != nil || n != 2 {
		t.Fatalf("UnpushedRef(wip) = %d, %v; want 2", n, err)
	}
	if n, err := fx.c.UnpushedRef(ctx, fx.clone, "wip~1"); err != nil || n != 1 {
		t.Fatalf("UnpushedRef(wip~1) = %d, %v; want 1", n, err)
	}
	// HEAD (main) is on origin even though wip is not: the refs are independent.
	if n, err := fx.c.Unpushed(ctx, fx.clone); err != nil || n != 0 {
		t.Fatalf("Unpushed = %d, %v", n, err)
	}
	if _, err := fx.c.UnpushedRef(ctx, fx.clone, "no-such-branch"); err == nil {
		t.Fatal("an unknown ref must be an error, not zero")
	}
}

func TestRealStatusPaths(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	if entries, err := fx.c.StatusPaths(ctx, fx.clone); err != nil || len(entries) != 0 {
		t.Fatalf("clean tree: %+v, %v", entries, err)
	}
	fx.git(fx.clone, "mv", "README.md", "new name.md") // staged rename, a space in the name
	fx.write(fx.clone, "plain.txt", "p\n")
	fx.write(fx.clone, "has space.txt", "s\n")
	fx.write(fx.clone, "line\nbreak.txt", "n\n")
	fx.write(fx.clone, "ünï/čödé.rb", "u\n")
	fx.write(fx.clone, "ignored.log", "i\n")
	fx.write(fx.clone, ".git/info/exclude", "*.log\n")

	entries, err := fx.c.StatusPaths(ctx, fx.clone)
	if err != nil {
		t.Fatal(err)
	}
	want := []StatusEntry{
		{X: '?', Y: '?', Path: "has space.txt"},
		{X: '?', Y: '?', Path: "line\nbreak.txt"},
		{X: '?', Y: '?', Path: "plain.txt"},
		{X: 'R', Y: ' ', Path: "new name.md", OrigPath: "README.md"},
		{X: '?', Y: '?', Path: "ünï/"}, // a wholly untracked directory is one entry
	}
	sortEntries := func(es []StatusEntry) {
		slices.SortFunc(es, func(a, b StatusEntry) int { return strings.Compare(a.Path, b.Path) })
	}
	sortEntries(entries)
	sortEntries(want)
	if !slices.Equal(entries, want) {
		t.Fatalf("StatusPaths\n got: %+v\nwant: %+v", entries, want)
	}
	// Paths are relative to the top level even when asked from a subdirectory.
	sub, err := fx.c.StatusPaths(ctx, filepath.Join(fx.clone, "ünï"))
	if err != nil {
		t.Fatal(err)
	}
	sortEntries(sub)
	if !slices.Equal(sub, want) {
		t.Fatalf("StatusPaths from a subdirectory\n got: %+v\nwant: %+v", sub, want)
	}
}

// An exported GIT_DIR (dotfile shells, hook environments) must not redirect
// gitx to another repository, not even for commands that rewrite HEAD.
func TestRealIgnoresRedirectingEnvironment(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetchPR7()
	wt := filepath.Join(filepath.Dir(fx.clone), "wt")
	if err := fx.c.WorktreeAdd(ctx, fx.clone, wt, "origin/main", true, ""); err != nil {
		t.Fatal(err)
	}

	decoy := filepath.Join(filepath.Dir(fx.clone), "decoy")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	fx.git(decoy, "init", "--quiet", "-b", "main")
	decoyHead := fx.commit(decoy, "decoy.txt", "d\n", "decoy")
	decoyTree := fx.git(decoy, "status", "--porcelain")

	// From here on gitx inherits the environment, as the daemon and CLI do.
	for k, v := range map[string]string{
		"HOME": t.TempDir(), "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null", "LC_ALL": "C",
		"GIT_DIR": filepath.Join(decoy, ".git"), "GIT_WORK_TREE": decoy,
		"GIT_INDEX_FILE": filepath.Join(decoy, ".git", "index"),
		"GIT_COMMON_DIR": filepath.Join(decoy, ".git"), "GIT_NAMESPACE": "decoy",
		"GIT_OBJECT_DIRECTORY": filepath.Join(decoy, ".git", "objects"),
	} {
		t.Setenv(k, v)
	}
	c := New(&execx.Real{})

	// Sanity: the redirect works for a git that is not scrubbed.
	res, err := (&execx.Real{}).Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", fx.clone, "rev-parse", "HEAD"}})
	if err != nil || res.Out() != decoyHead {
		t.Skipf("GIT_DIR did not redirect an unscrubbed git (%q, %v); nothing to prove", res.Out(), err)
	}

	if got, err := c.RevParse(ctx, fx.clone, "HEAD"); err != nil || got != fx.base {
		t.Fatalf("RevParse(clone) = %q, %v; want the clone's HEAD %s, not the decoy's", got, err, fx.base)
	}
	if err := c.SwitchDetach(ctx, wt, PRRef(7)); err != nil {
		t.Fatalf("SwitchDetach: %v", err)
	}
	if got, err := c.RevParse(ctx, wt, "HEAD"); err != nil || got != fx.pr {
		t.Fatalf("worktree HEAD = %q, %v; want %s", got, err, fx.pr)
	}
	if st, err := c.Status(ctx, wt); err != nil || st.Dirty() {
		t.Fatalf("Status(worktree) = %+v, %v", st, err)
	}
	if got, err := c.RemoteURL(ctx, fx.clone); err != nil || got != fx.origin {
		t.Fatalf("RemoteURL = %q, %v; want %s", got, err, fx.origin)
	}
	if err := c.UpdateRefDelete(ctx, fx.clone, PRRef(7)); err != nil {
		t.Fatalf("UpdateRefDelete: %v", err)
	}
	dest := filepath.Join(filepath.Dir(fx.clone), "cloned")
	if err := c.Clone(ctx, fx.origin, dest); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if got, err := c.RevParse(ctx, dest, "HEAD"); err != nil || got != fx.base {
		t.Fatalf("cloned HEAD = %q, %v", got, err)
	}

	// The decoy repository was never touched (fx.git has its own hermetic
	// environment, so the exported GIT_DIR does not affect these checks).
	if head := fx.git(decoy, "rev-parse", "HEAD"); head != decoyHead {
		t.Fatalf("decoy HEAD moved to %s", head)
	}
	if st := fx.git(decoy, "status", "--porcelain"); st != decoyTree {
		t.Fatalf("decoy tree changed: %q", st)
	}
}

// TreeFiles lists the files of a commit that pathspecs match, with the same
// pathspec rules as ChangedPaths (a directory, a glob, pathspec magic), each
// with its blob id: what a pool slot's schema fingerprint is made of.
func TestRealTreeFilesMatchesLikeChangedPaths(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetchPR7()
	blob := fx.git(fx.clone, "rev-parse", fx.pr+":db/schema.rb")

	got, err := fx.c.TreeFiles(ctx, fx.clone, PRRef(7), "db/")
	if want := []string{"100644 " + blob + " db/schema.rb"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("TreeFiles(db/) = %q, %v; want %q", got, err, want)
	}
	for _, spec := range []string{"db/*.rb", "*.rb", "db/schema.rb"} {
		got, err := fx.c.TreeFiles(ctx, fx.clone, PRRef(7), spec)
		changed, cerr := fx.c.ChangedPaths(ctx, fx.clone, fx.base, PRRef(7), spec)
		var paths []string
		for _, f := range got {
			paths = append(paths, f[strings.LastIndexByte(f, ' ')+1:])
		}
		if err != nil || cerr != nil || !slices.Equal(paths, changed) {
			t.Fatalf("TreeFiles(%s) = %q, %v; ChangedPaths = %q, %v", spec, got, err, changed, cerr)
		}
	}
	if got, err := fx.c.TreeFiles(ctx, fx.clone, fx.base, "db/"); err != nil || len(got) != 0 {
		t.Fatalf("TreeFiles at a commit without db/ = %q, %v", got, err)
	}
	if _, err := fx.c.TreeFiles(ctx, fx.clone, "refs/magnum/pr/99", "db/"); !errors.Is(err, ErrNoSuchRef) {
		t.Fatalf("TreeFiles of a missing ref: err = %v, want ErrNoSuchRef", err)
	}
}

// FileLog lists the commits of a revision that changed one file, newest
// first and at most n, never one that only another branch or a later base
// has, and takes the path literally; ModifiedPaths leaves out the files the
// head adds, which have no history on the base.
func TestRealFileLogAndModifiedPaths(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	dir := fx.origin
	fx.commit(dir, "lib/x.rb", "1\n", "Add x")
	fx.commit(dir, "lib/x.rb", "2\n", "Fix flaky x (#12)")
	fx.commit(dir, "lib/y.rb", "y\n", "Add y")
	tune := fx.commit(dir, "lib/x.rb", "3\n", "Tune x")
	fx.git(dir, "checkout", "--quiet", "-b", "feature")
	fx.commit(dir, "lib/x.rb", "4\n", "Feature changes x")
	fx.commit(dir, "lib/new.rb", "n\n", "Feature adds new")
	fx.git(dir, "rm", "--quiet", "lib/y.rb")
	fx.git(dir, "commit", "--quiet", "-m", "Feature drops y")
	fx.git(dir, "checkout", "--quiet", "main")
	fx.commit(dir, "lib/x.rb", "5\n", "Later fix (#20)")

	got, err := fx.c.ModifiedPaths(ctx, dir, "main", "feature")
	if err != nil || !slices.Equal(got, []string{"lib/x.rb", "lib/y.rb"}) {
		t.Fatalf("ModifiedPaths = %q, %v; want lib/x.rb and lib/y.rb, not the added lib/new.rb", got, err)
	}
	subjects := func(cs []Commit) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Subject)
		}
		return out
	}
	log, err := fx.c.FileLog(ctx, dir, "main", "lib/x.rb", 2)
	if err != nil || !slices.Equal(subjects(log), []string{"Later fix (#20)", "Tune x"}) {
		t.Fatalf("FileLog(main, 2) = %+v, %v", log, err)
	}
	if c := log[1]; !strings.HasPrefix(tune, c.SHA) || len(c.SHA) < 7 || len(c.Date) != len("2026-10-06") || c.Date[4] != '-' {
		t.Fatalf("commit = %+v, want an abbreviation of %s and a YYYY-MM-DD date", c, tune)
	}
	log, err = fx.c.FileLog(ctx, dir, tune, "lib/x.rb", 8)
	if want := []string{"Tune x", "Fix flaky x (#12)", "Add x"}; err != nil || !slices.Equal(subjects(log), want) {
		t.Fatalf("FileLog(base, 8) = %q, %v; want %q", subjects(log), err, want)
	}
	if log, err := fx.c.FileLog(ctx, dir, "main", "lib/*.rb", 8); err != nil || len(log) != 0 {
		t.Fatalf("FileLog of the literal lib/*.rb = %+v, %v; want none", log, err)
	}
	// Each record ends in NUL, so an empty last subject keeps its place.
	fx.write(dir, "lib/z.rb", "z\n")
	fx.git(dir, "add", "--", "lib/z.rb")
	fx.git(dir, "commit", "--quiet", "--allow-empty-message", "-m", "")
	if log, err := fx.c.FileLog(ctx, dir, "main", "lib/z.rb", 8); err != nil || len(log) != 1 || log[0].Subject != "" || log[0].SHA == "" {
		t.Fatalf("FileLog of a commit without a message = %+v, %v", log, err)
	}
}
