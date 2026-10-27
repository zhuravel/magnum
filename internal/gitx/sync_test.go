package gitx

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// syncFixture is the fixture with the clone detached at PR 7's head (the
// fixer's start head, fx.pr) and main moved on in origin by a commit that
// touches another file; it returns origin's new main.
func syncFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	fx := newFixture(t)
	fx.git(fx.clone, "switch", "--quiet", "--detach", fx.pr)
	main := fx.commit(fx.origin, "docs/main.md", "main moves\n", "main moves elsewhere")
	return fx, main
}

// mergeMainIntoPR7 merges origin's main into origin's pr7, as GitHub's
// "Update branch" does (the branch's head is the first parent), and
// fetches both into the clone; it returns the merge.
func mergeMainIntoPR7(fx *fixture) string {
	fx.t.Helper()
	fx.git(fx.origin, "switch", "--quiet", "pr7")
	fx.git(fx.origin, "merge", "--quiet", "--no-edit", "main")
	merge := fx.git(fx.origin, "rev-parse", "HEAD")
	fx.git(fx.origin, "switch", "--quiet", "main")
	fx.fetchSync()
	return merge
}

// fetchSync fetches origin's pr7 and main into the clone's remote refs.
func (fx *fixture) fetchSync() {
	fx.t.Helper()
	for _, b := range []string{"pr7", "main"} {
		if err := fx.c.FetchBranch(context.Background(), fx.clone, b); err != nil {
			fx.t.Fatal(err)
		}
	}
}

// What a branch gained since the fixer's start head is read on its
// first-parent chain: a merge of the base is one commit with two parents
// there, while the plain list holds the base's commits it brought in too.
func TestCommitsAfterReadsAMergeOfTheBaseAsOneCommit(t *testing.T) {
	fx, main := syncFixture(t)
	ctx := context.Background()
	merge := mergeMainIntoPR7(fx)
	got, err := fx.c.CommitsAfter(ctx, fx.clone, fx.pr, merge, true)
	want := []ListedCommit{{SHA: merge, Parents: []string{fx.pr, main}}}
	if err != nil || !slices.EqualFunc(got, want, func(a, b ListedCommit) bool { return a.SHA == b.SHA && slices.Equal(a.Parents, b.Parents) }) {
		t.Fatalf("CommitsAfter first parent = %+v, %v; want %+v", got, err, want)
	}
	if !got[0].Merge() {
		t.Error("a merge does not read as one")
	}
	all, err := fx.c.CommitsAfter(ctx, fx.clone, fx.pr, merge, false)
	if err != nil || len(all) != 2 || all[0].SHA != merge || all[1].SHA != main || all[1].Merge() {
		t.Fatalf("CommitsAfter = %+v, %v; want the merge, then main's commit", all, err)
	}
	if got, err := fx.c.CommitsAfter(ctx, fx.clone, merge, merge, true); err != nil || len(got) != 0 {
		t.Fatalf("nothing gained = %+v, %v", got, err)
	}
	if _, err := fx.c.CommitsAfter(ctx, fx.clone, "--all", merge, true); err == nil {
		t.Fatal("an option as the start was taken")
	}
}

// The PR's own diff against its base (from their merge base to the head,
// as GitHub's base...head shows the PR) keeps its patch id across a merge
// of the base that changes other files, and loses it when the merge also
// changes a line of the PR's own, or brings a branch the base lacks.
func TestOwnDiffIDKeepsThePRsDiffAcrossAMergeOfTheBase(t *testing.T) {
	fx, _ := syncFixture(t)
	ctx := context.Background()
	merge := mergeMainIntoPR7(fx)
	base := "refs/remotes/origin/main"
	before, err := fx.c.OwnDiffID(ctx, fx.clone, base, fx.pr)
	if err != nil || len(before) != 40 {
		t.Fatalf("OwnDiffID(start head) = %q, %v", before, err)
	}
	if after, err := fx.c.OwnDiffID(ctx, fx.clone, base, merge); err != nil || after != before {
		t.Fatalf("OwnDiffID(merge of the base) = %q, %v; want the start head's %s", after, err, before)
	}

	// A change slipped into the merge (`git merge --no-commit`, an edit, a commit).
	fx.commit(fx.origin, "docs/other.md", "more\n", "main moves on")
	fx.git(fx.origin, "switch", "--quiet", "pr7")
	fx.git(fx.origin, "merge", "--quiet", "--no-commit", "main")
	fx.write(fx.origin, "app/a.rb", "a = 1\nb = 2\n")
	fx.git(fx.origin, "add", "app/a.rb")
	fx.git(fx.origin, "commit", "--quiet", "--no-edit")
	evil := fx.git(fx.origin, "rev-parse", "HEAD")
	fx.git(fx.origin, "switch", "--quiet", "main")
	fx.fetchSync()
	if got, err := fx.c.OwnDiffID(ctx, fx.clone, base, evil); err != nil || got == before {
		t.Fatalf("OwnDiffID(a merge that changed the PR's line) = %q, %v; want other than %s", got, err, before)
	}

	// A merge of a branch the base lacks.
	fx.git(fx.origin, "switch", "--quiet", "-c", "other", fx.base)
	fx.commit(fx.origin, "lib/other.rb", "o = 1\n", "another branch's change")
	fx.git(fx.origin, "switch", "--quiet", "-c", "pr7-other", merge)
	fx.git(fx.origin, "merge", "--quiet", "--no-edit", "other")
	other := fx.git(fx.origin, "rev-parse", "HEAD")
	fx.git(fx.origin, "switch", "--quiet", "main")
	if err := fx.c.FetchBranch(ctx, fx.clone, "pr7-other"); err != nil {
		t.Fatal(err)
	}
	if got, err := fx.c.OwnDiffID(ctx, fx.clone, base, other); err != nil || got == before {
		t.Fatalf("OwnDiffID(a merge of a branch the base lacks) = %q, %v; want other than %s", got, err, before)
	}

	// The base against itself changes nothing.
	if got, err := fx.c.OwnDiffID(ctx, fx.clone, base, base); err != nil || got != "" {
		t.Fatalf("OwnDiffID(the base) = %q, %v; want none", got, err)
	}
	if _, err := fx.c.OwnDiffID(ctx, fx.clone, "-p", merge); err == nil {
		t.Fatal("an option as the base was taken")
	}
}

// ReplayOnto replays the fixer's commits (from..HEAD, the detached HEAD)
// onto the branch's new tip; a replay that conflicts is aborted with the
// conflicting paths, and HEAD, the index and the work tree are as they
// were.
func TestReplayOntoReplaysTheCommitsAfterTheStartAndAbortsAConflict(t *testing.T) {
	fx, _ := syncFixture(t)
	ctx := context.Background()
	fixed := fx.commit(fx.clone, "app/b.rb", "b = 1\n", "the fixer's fix")
	merge := mergeMainIntoPR7(fx)
	paths, err := fx.c.ReplayOnto(ctx, fx.clone, merge, fx.pr)
	if err != nil || paths != nil {
		t.Fatalf("ReplayOnto = %v, %v", paths, err)
	}
	if got := fx.git(fx.clone, "rev-parse", "HEAD~1"); got != merge {
		t.Fatalf("HEAD~1 = %s, want the merge %s", got, merge)
	}
	if got := fx.git(fx.clone, "show", "HEAD:app/b.rb"); got != "b = 1" {
		t.Fatalf("the replayed commit's file = %q", got)
	}
	if got := fx.git(fx.clone, "rev-parse", "HEAD"); got == fixed {
		t.Fatal("HEAD is still the commit before the replay")
	}
	if b, err := fx.c.CurrentBranch(ctx, fx.clone); err != nil || b != "" {
		t.Fatalf("after the replay the checkout is on %q, %v; want the detached HEAD", b, err)
	}

	// The branch gains a commit that changes the line the fixer changes.
	start := fx.git(fx.clone, "rev-parse", "HEAD")
	head := fx.commit(fx.clone, "app/a.rb", "a = 2\n", "the fixer changes a")
	fx.git(fx.origin, "switch", "--quiet", "pr7")
	fx.git(fx.origin, "reset", "--quiet", "--hard", merge)
	fx.commit(fx.origin, "app/b.rb", "b = 1\n", "the same fix")
	fx.commit(fx.origin, "app/a.rb", "a = 3\n", "someone changes a")
	fx.git(fx.origin, "switch", "--quiet", "main")
	fx.fetchSync()
	tip := fx.git(fx.clone, "rev-parse", "refs/remotes/origin/pr7")
	paths, err = fx.c.ReplayOnto(ctx, fx.clone, tip, start)
	if !errors.Is(err, ErrRebaseConflict) || !slices.Equal(paths, []string{"app/a.rb"}) {
		t.Fatalf("a conflicting replay = %v, %v; want ErrRebaseConflict in app/a.rb", paths, err)
	}
	if got := fx.git(fx.clone, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %s after the abort, want %s", got, head)
	}
	if p, err := fx.c.InProgress(ctx, fx.clone); err != nil || p != "" {
		t.Fatalf("in progress after the abort = %q, %v", p, err)
	}
	if st := fx.status(fx.clone); st.Dirty() {
		t.Fatalf("the checkout after the abort: %+v", st)
	}
	if _, err := fx.c.ReplayOnto(ctx, fx.clone, "-x", start); err == nil {
		t.Fatal("an option as the target was taken")
	}
}
