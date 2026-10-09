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

// gitCmd is `git -C dir args...` for a command a test expects to fail.
func gitCmd(dir string, args ...string) execx.Cmd {
	return execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)}
}

// learnWorktree adds a worktree of the fixture's clone on a new branch
// from main, as the skill editor's run does.
func learnWorktree(t *testing.T, fx *fixture, branch string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(fx.clone), "wt", strings.ReplaceAll(branch, "/", "-"))
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fx.c.WorktreeAdd(context.Background(), fx.clone, dir, "main", false, branch); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	return dir
}

// The editor's commit: on its own branch in its worktree, only the paths
// it names, unsigned whatever the repository asks, and master untouched.
// Discarding the worktree removes it and its registration, dirty or gone.
func TestWorktreeAddAndCommitAllOnABranch(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	// A repository that signs every commit with a program that fails:
	// CommitAll signs nothing.
	fx.git(fx.clone, "config", "commit.gpgsign", "true")
	fx.git(fx.clone, "config", "gpg.program", "false")
	dir := learnWorktree(t, fx, "learn/20261009-070000")
	if b, err := fx.c.CurrentBranch(ctx, dir); err != nil || b != "learn/20261009-070000" {
		t.Fatalf("CurrentBranch = %q, %v", b, err)
	}
	fx.write(dir, "README.md", "hello, edited\n")
	fx.write(dir, "docs/new.md", "a new file\n")
	fx.write(dir, "changes.json", "{}\n") // the editor's answer: never committed
	sha, err := fx.c.CommitAll(ctx, dir, "Edit the readme.\n\nNo new dependencies.\n", "README.md", "docs/new.md")
	if err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	if got := fx.git(dir, "rev-parse", "learn/20261009-070000"); got != sha {
		t.Fatalf("branch = %s, want the commit %s", got, sha)
	}
	if got := fx.git(fx.clone, "rev-parse", "main"); got != fx.base {
		t.Fatalf("main moved to %s", got)
	}
	if msg := fx.git(dir, "log", "-1", "--format=%B"); msg != "Edit the readme.\n\nNo new dependencies." {
		t.Fatalf("message = %q", msg)
	}
	if files := fx.git(dir, "show", "--name-only", "--format=", sha); files != "README.md\ndocs/new.md" {
		t.Fatalf("committed = %q", files)
	}
	if sig := fx.git(dir, "log", "-1", "--format=%G?"); sig != "N" {
		t.Fatalf("signature status = %q, want none", sig)
	}
	if dirty, err := fx.c.DirtyPaths(ctx, dir, "changes.json", "README.md"); err != nil || !slices.Equal(dirty, []string{"changes.json"}) {
		t.Fatalf("after the commit, dirty = %q, %v", dirty, err)
	}
	if _, err := fx.c.CommitAll(ctx, dir, "nothing", "README.md"); err == nil {
		t.Fatal("a commit with nothing to commit succeeded")
	}
	if _, err := fx.c.CommitAll(ctx, dir, "no paths"); err == nil {
		t.Fatal("a commit without paths succeeded")
	}

	if err := fx.c.WorktreeDiscard(ctx, fx.clone, dir); err != nil {
		t.Fatalf("WorktreeDiscard: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worktree is still there: %v", err)
	}
	if list, _ := fx.c.WorktreeList(ctx, fx.clone); len(list) != 1 {
		t.Fatalf("worktrees after the discard = %+v", list)
	}
	// One whose directory someone removed is forgotten too, and a second
	// discard is fine.
	gone := learnWorktree(t, fx, "learn/gone")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := fx.c.WorktreeDiscard(ctx, fx.clone, gone); err != nil {
			t.Fatalf("WorktreeDiscard of a removed directory: %v", err)
		}
	}
	if list, _ := fx.c.WorktreeList(ctx, fx.clone); len(list) != 1 {
		t.Fatalf("worktrees after discarding a removed one = %+v", list)
	}
}

func TestMergeFFOnlyRefusesADivergedMaster(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	dir := learnWorktree(t, fx, "learn/a")
	fx.commit(dir, "README.md", "hello, a\n", "a")
	if err := fx.c.MergeFFOnly(ctx, fx.clone, "learn/a"); err != nil {
		t.Fatalf("a fast-forward: %v", err)
	}
	if fx.git(fx.clone, "rev-parse", "main") != fx.git(dir, "rev-parse", "HEAD") {
		t.Fatal("main did not move to the branch")
	}
	if b, err := fx.c.CurrentBranch(ctx, fx.clone); err != nil || b != "main" {
		t.Fatalf("CurrentBranch = %q, %v", b, err)
	}

	other := learnWorktree(t, fx, "learn/b")
	fx.commit(other, "docs/b.md", "b\n", "b")
	head := fx.commit(fx.clone, "docs/main.md", "main moves\n", "main moves")
	err := fx.c.MergeFFOnly(ctx, fx.clone, "learn/b")
	if !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("a diverged main = %v, want ErrNotFastForward", err)
	}
	if fx.git(fx.clone, "rev-parse", "main") != head {
		t.Fatal("the refused merge moved main")
	}
	if p, err := fx.c.InProgress(ctx, fx.clone); err != nil || p != "" {
		t.Fatalf("after the refusal, in progress = %q, %v", p, err)
	}
	if err := fx.c.MergeFFOnly(ctx, fx.clone, "learn/none"); err == nil || errors.Is(err, ErrNotFastForward) {
		t.Fatalf("an unknown branch = %v", err)
	}
}

func TestRebaseOntoReportsAConflictAndAborts(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	dir := learnWorktree(t, fx, "learn/a")
	fx.commit(dir, "docs/a.md", "a\n", "a")
	fx.commit(fx.clone, "docs/main.md", "main moves\n", "main moves")
	if err := fx.c.RebaseOnto(ctx, dir, "main", fx.base); err != nil {
		t.Fatalf("a clean rebase: %v", err)
	}
	if fx.git(dir, "rev-parse", "HEAD~1") != fx.git(fx.clone, "rev-parse", "main") {
		t.Fatal("the branch is not on main after the rebase")
	}

	upstream := fx.git(fx.clone, "rev-parse", "main")
	before := fx.commit(dir, "README.md", "hello, branch\n", "branch edits the readme")
	fx.commit(fx.clone, "README.md", "hello, main\n", "main edits the readme")
	err := fx.c.RebaseOnto(ctx, dir, "main", upstream)
	if !errors.Is(err, ErrRebaseConflict) {
		t.Fatalf("a conflicting rebase = %v, want ErrRebaseConflict", err)
	}
	if p, err := fx.c.InProgress(ctx, dir); err != nil || p != "" {
		t.Fatalf("after the conflict, in progress = %q, %v (aborted)", p, err)
	}
	if got := fx.git(dir, "rev-parse", "HEAD"); got != before {
		t.Fatalf("HEAD = %s, want the branch as it was (%s)", got, before)
	}
	if b, _ := fx.c.CurrentBranch(ctx, dir); b != "learn/a" {
		t.Fatalf("branch after the abort = %q", b)
	}
	if err := fx.c.RebaseOnto(ctx, dir, "main", "-x"); err == nil {
		t.Fatal("an option as the upstream was taken")
	}
}

// A rebase replays only the commits after upstream: when the operator
// dropped a commit from main after the branch was cut on it (`git reset
// --hard HEAD~1`), the branch rebased onto main with its base as upstream
// lands without it, where `git rebase main` would bring it back.
func TestRebaseOntoLeavesOutACommitMainDropped(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	dropped := fx.commit(fx.clone, "docs/dropped.md", "a commit the operator drops\n", "dropped")
	dir := learnWorktree(t, fx, "learn/a")
	fx.commit(dir, "docs/a.md", "the candidate\n", "the candidate")
	fx.git(fx.clone, "reset", "--quiet", "--hard", "HEAD~1")
	if err := fx.c.RebaseOnto(ctx, dir, "main", dropped); err != nil {
		t.Fatalf("RebaseOnto: %v", err)
	}
	if fx.git(dir, "rev-parse", "HEAD~1") != fx.git(fx.clone, "rev-parse", "main") {
		t.Fatal("the branch is not a single commit on main after the rebase")
	}
	if err := fx.c.MergeFFOnly(ctx, fx.clone, "learn/a"); err != nil {
		t.Fatalf("MergeFFOnly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fx.clone, "docs", "dropped.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the dropped commit came back with the landing: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(fx.clone, "docs", "a.md")); err != nil || string(b) != "the candidate\n" {
		t.Fatalf("the candidate's file = %q, %v", b, err)
	}
}

// A candidate rebased onto a main that changed a line near its own keeps
// its patch id (no context lines count); a commit that changes anything
// else has another.
func TestPatchIDIsTheSameForTheCandidateRebased(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	const text = "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"
	base := fx.commit(fx.clone, "docs/skill.md", text, "the skill")
	dir := learnWorktree(t, fx, "learn/a")
	cand := fx.commit(dir, "docs/skill.md", strings.Replace(text, "five", "FIVE", 1), "the candidate")
	fx.commit(fx.clone, "docs/skill.md", strings.Replace(text, "two", "TWO", 1), "main edits a line of the candidate's context")
	if err := fx.c.RebaseOnto(ctx, dir, "main", base); err != nil {
		t.Fatalf("RebaseOnto: %v", err)
	}
	rebased := fx.git(dir, "rev-parse", "HEAD")
	id, err := fx.c.PatchID(ctx, dir, cand)
	if err != nil || len(id) != 40 {
		t.Fatalf("PatchID(candidate) = %q, %v", id, err)
	}
	if got, err := fx.c.PatchID(ctx, dir, rebased); err != nil || got != id || rebased == cand {
		t.Fatalf("PatchID(rebased %s) = %q, %v; want the candidate's %s", rebased, got, err, id)
	}
	more := fx.commit(dir, "docs/skill.md", strings.Replace(text, "five", "FIVE, and more", 1), "a commit made in the worktree")
	if got, err := fx.c.PatchID(ctx, dir, more); err != nil || got == id {
		t.Fatalf("PatchID(another change) = %q, %v; want other than %s", got, err, id)
	}
	if _, err := fx.c.PatchID(ctx, dir, "-x"); err == nil {
		t.Fatal("an option as the commit was taken")
	}
}

func TestDirtyPathsNamesOnlyTheDirtyOnes(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.commit(fx.clone, "docs/kept.md", "kept\n", "kept")
	fx.write(fx.clone, "README.md", "hello, edited by hand\n")
	fx.write(fx.clone, "docs/untracked.md", "new\n")
	fx.write(fx.clone, "elsewhere.txt", "not asked about\n")
	got, err := fx.c.DirtyPaths(ctx, fx.clone, "README.md", "docs/kept.md", "docs/untracked.md", "missing.md")
	if err != nil || !slices.Equal(got, []string{"README.md", "docs/untracked.md"}) {
		t.Fatalf("DirtyPaths = %q, %v", got, err)
	}
	if got, err := fx.c.DirtyPaths(ctx, fx.clone, "docs/kept.md"); err != nil || got != nil {
		t.Fatalf("a clean path = %q, %v", got, err)
	}
	if _, err := fx.c.DirtyPaths(ctx, fx.clone); err == nil {
		t.Fatal("DirtyPaths without a path answered")
	}
}

func TestInProgressSeesARebase(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	if p, err := fx.c.InProgress(ctx, fx.clone); err != nil || p != "" {
		t.Fatalf("a quiet clone = %q, %v", p, err)
	}
	fx.git(fx.clone, "checkout", "--quiet", "-b", "side")
	fx.commit(fx.clone, "README.md", "hello, side\n", "side")
	fx.git(fx.clone, "checkout", "--quiet", "main")
	fx.commit(fx.clone, "README.md", "hello, main\n", "main")

	// A rebase that stopped on a conflict, as the operator might leave it.
	fx.git(fx.clone, "checkout", "--quiet", "side")
	if _, err := fx.r.Run(ctx, gitCmd(fx.clone, "rebase", "main")); err == nil {
		t.Fatal("the rebase did not stop")
	}
	if p, err := fx.c.InProgress(ctx, fx.clone); err != nil || p != "rebase" {
		t.Fatalf("during a rebase = %q, %v", p, err)
	}
	if b, err := fx.c.CurrentBranch(ctx, fx.clone); err != nil || b != "" {
		t.Fatalf("CurrentBranch during a rebase = %q, %v; want detached", b, err)
	}
	fx.git(fx.clone, "rebase", "--abort")

	// A merge that stopped.
	if _, err := fx.r.Run(ctx, gitCmd(fx.clone, "merge", "main")); err == nil {
		t.Fatal("the merge did not stop")
	}
	if p, err := fx.c.InProgress(ctx, fx.clone); err != nil || p != "merge" {
		t.Fatalf("during a merge = %q, %v", p, err)
	}
	fx.git(fx.clone, "merge", "--abort")
	if p, err := fx.c.InProgress(ctx, fx.clone); err != nil || p != "" {
		t.Fatalf("after the abort = %q, %v", p, err)
	}
}

// The skill editor's orphan cleanup lists its own branches: the local
// branches under a prefix, by their short names, sorted; a branch whose
// name only starts like the prefix, a remote one and a tag are not listed.
func TestBranchesUnderListsTheLocalBranchesOfAPrefix(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	for _, b := range []string{"learn/20261010-070000", "learn/20261009-070000", "learnt", "feature/x"} {
		fx.git(fx.clone, "branch", b, "main")
	}
	fx.git(fx.clone, "tag", "learn/v1", "main")
	fx.git(fx.clone, "update-ref", "refs/remotes/origin/learn/20261011-070000", "main")
	got, err := fx.c.BranchesUnder(ctx, fx.clone, "learn/")
	if err != nil || !slices.Equal(got, []string{"learn/20261009-070000", "learn/20261010-070000"}) {
		t.Fatalf("BranchesUnder = %q, %v", got, err)
	}
	if got, err := fx.c.BranchesUnder(ctx, fx.clone, "none/"); err != nil || len(got) != 0 {
		t.Fatalf("an empty prefix's branches = %q, %v", got, err)
	}
	for _, bad := range []string{"", "learn", "-x/", "a b/"} {
		if _, err := fx.c.BranchesUnder(ctx, fx.clone, bad); err == nil {
			t.Errorf("prefix %q was taken", bad)
		}
	}
}

// A revert of a skill version restores its files in a worktree as an
// earlier commit had them: a file it changed gets the earlier text, a file
// the earlier commit did not have goes, and a commit of those paths
// records exactly that; nothing else in the worktree changes.
func TestRestoreFromBringsBackTheEarlierText(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	before := fx.commit(fx.clone, "skills/SKILL.md", "Prove five facts.\n", "the skill")
	fx.write(fx.clone, "prompts/judge-new.md", "Review it.\n")
	fx.git(fx.clone, "add", "--", "prompts/judge-new.md")
	fx.commit(fx.clone, "skills/SKILL.md", "Prove five facts.\nCheck the lock.\n", "version 1")
	const branch = "learn/20261010-070000"
	dir := learnWorktree(t, fx, branch)
	fx.write(dir, "README.md", "an edit the revert leaves alone\n")

	if err := fx.c.RestoreFrom(ctx, dir, before, "skills/SKILL.md", "prompts/judge-new.md"); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "skills", "SKILL.md")); err != nil || string(b) != "Prove five facts.\n" {
		t.Fatalf("the restored skill = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "prompts", "judge-new.md")); !os.IsNotExist(err) {
		t.Fatalf("a file the earlier commit lacks is still there: %v", err)
	}
	sha, err := fx.c.CommitAll(ctx, dir, "Reverts version 1.\n", "skills/SKILL.md", "prompts/judge-new.md")
	if err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	if got := fx.git(dir, "show", "--name-status", "--format=", sha); got != "D\tprompts/judge-new.md\nM\tskills/SKILL.md" {
		t.Fatalf("the revert commit changes %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(b) != "an edit the revert leaves alone\n" {
		t.Fatalf("README.md = %q", b)
	}
	if err := fx.c.RestoreFrom(ctx, dir, "-x", "skills/SKILL.md"); err == nil {
		t.Fatal("an option as the source was taken")
	}
	if err := fx.c.RestoreFrom(ctx, dir, before); err == nil {
		t.Fatal("a restore of no path was taken")
	}
}
