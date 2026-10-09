package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The skill landing's sequence on a real repository (internal/engine
// learn_land.go): a candidate branch whose base the checkout's branch moved
// past is rebased in its worktree (the branch follows), the checkout's
// branch fast-forwards to it, the merge base then says it landed, and the
// worktree, then the branch (merged, so a plain delete), go.
func TestALandingRebasesTheBranchFastForwardsAndDropsIt(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	const branch = "learn/20261009-070000"
	dir := learnWorktree(t, fx, branch)
	fx.write(dir, "skills/SKILL.md", "When two writers share a row, check the lock.\n")
	cand, err := fx.c.CommitAll(ctx, dir, "The judge checks the lock.\n", "skills/SKILL.md")
	if err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	moved := fx.commit(fx.clone, "docs/notes.md", "the operator's commit\n", "c3 on main")
	if base, err := fx.c.MergeBase(ctx, fx.clone, cand, moved); err != nil || base != fx.base {
		t.Fatalf("merge base = %s, %v, want the old main %s", base, err, fx.base)
	}

	if err := fx.c.RebaseOnto(ctx, dir, moved, fx.base); err != nil {
		t.Fatalf("RebaseOnto: %v", err)
	}
	head, err := fx.c.RevParse(ctx, dir, "HEAD")
	if err != nil || head == cand {
		t.Fatalf("the worktree's HEAD = %s, %v after the rebase", head, err)
	}
	// The landing knows the rebased branch for the candidate: a single
	// commit on main with the candidate's patch id.
	if parent, err := fx.c.RevParse(ctx, fx.clone, head+"^"); err != nil || parent != moved {
		t.Fatalf("the rebased commit's parent = %s, %v, want %s", parent, err, moved)
	}
	if _, err := fx.c.RevParse(ctx, fx.clone, head+"^2"); !errors.Is(err, ErrNoSuchRef) {
		t.Fatalf("the rebased commit's second parent: %v, want ErrNoSuchRef", err)
	}
	want, err := fx.c.PatchID(ctx, fx.clone, cand)
	if got, gerr := fx.c.PatchID(ctx, fx.clone, head); err != nil || gerr != nil || got != want {
		t.Fatalf("patch ids = %s (%v), %s (%v)", want, err, got, gerr)
	}
	if got, _ := fx.c.RevParse(ctx, fx.clone, "refs/heads/"+branch); got != head {
		t.Fatalf("the branch = %s, want the rebased %s", got, head)
	}
	if base, err := fx.c.MergeBase(ctx, fx.clone, head, moved); err != nil || base != moved {
		t.Fatalf("merge base after the rebase = %s, %v", base, err)
	}
	if dirty, err := fx.c.DirtyPaths(ctx, fx.clone, "skills/SKILL.md"); err != nil || len(dirty) != 0 {
		t.Fatalf("dirty = %q, %v", dirty, err)
	}

	if err := fx.c.MergeFFOnly(ctx, fx.clone, "main", branch); err != nil {
		t.Fatalf("MergeFFOnly: %v", err)
	}
	if got := fx.git(fx.clone, "rev-parse", "main"); got != head {
		t.Fatalf("main = %s, want %s", got, head)
	}
	if b, err := os.ReadFile(filepath.Join(fx.clone, "skills", "SKILL.md")); err != nil || len(b) == 0 {
		t.Fatalf("the checkout's skill after the landing: %q, %v", b, err)
	}
	if base, err := fx.c.MergeBase(ctx, fx.clone, head, "main"); err != nil || base != head {
		t.Fatalf("a landed branch's merge base = %s, %v, want itself", base, err)
	}

	if err := fx.c.WorktreeDiscard(ctx, fx.clone, dir); err != nil {
		t.Fatalf("WorktreeDiscard: %v", err)
	}
	if err := fx.c.BranchDelete(ctx, fx.clone, branch, false); err != nil {
		t.Fatalf("BranchDelete of the landed branch: %v", err)
	}
	if _, err := fx.c.RevParse(ctx, fx.clone, "refs/heads/"+branch); err == nil {
		t.Fatal("the branch is still there")
	}
}
