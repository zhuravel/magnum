package gitx

import (
	"context"
	"strings"
	"testing"
)

// heldMergeFixture is a PR branch and its base that both change
// .github/workflows/ci.yml, merged with the conflict resolved by hand, and
// a later merge of the base whose workflow files merged without a
// conflict: the merges magnum holds for the operator's y
// (engine.HeldMerge).
type heldMergeFixture struct {
	*fixture
	head, resolved, clean string
}

func newHeldMergeFixture(t *testing.T) heldMergeFixture {
	t.Helper()
	fx := newFixture(t)
	dir := fx.origin
	fx.write(dir, ".github/workflows/ci.yml", "on: push\njobs: a\n")
	fx.write(dir, ".github/workflows/lint.yml", "on: push\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "workflows")
	fx.git(dir, "checkout", "--quiet", "-b", "feature")
	fx.write(dir, ".github/workflows/ci.yml", "on: push\njobs: pr\n")
	fx.write(dir, "app/a.rb", "a = 2\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "the PR's own")
	head := fx.git(dir, "rev-parse", "HEAD")
	fx.git(dir, "checkout", "--quiet", "main")
	fx.write(dir, ".github/workflows/ci.yml", "on: push\njobs: base\n")
	fx.write(dir, ".github/workflows/lint.yml", "on: pull_request\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "the base's")
	fx.git(dir, "checkout", "--quiet", "feature")
	if _, err := fx.r.Run(context.Background(), gitCmd(dir, "merge", "--quiet", "--no-edit", "main")); err == nil {
		t.Fatal("the merge did not conflict")
	}
	fx.write(dir, ".github/workflows/ci.yml", "on: push\njobs: resolved\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "--no-edit")
	resolved := fx.git(dir, "rev-parse", "HEAD")

	fx.git(dir, "checkout", "--quiet", "-b", "clean", resolved)
	fx.write(dir, "app/a.rb", "a = 3\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "no workflow file")
	fx.git(dir, "checkout", "--quiet", "main")
	fx.write(dir, ".github/workflows/lint.yml", "on: [push, pull_request]\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "the base's lint")
	fx.git(dir, "checkout", "--quiet", "clean")
	fx.git(dir, "merge", "--quiet", "--no-edit", "main")
	return heldMergeFixture{fixture: fx, head: head, resolved: resolved, clean: fx.git(dir, "rev-parse", "HEAD")}
}

// The operator reads what a merge magnum holds does to the workflow files
// before its y: RemergeDiff shows how the merge resolved its conflicts in
// the paths asked (git's own merge, conflict markers and all, against the
// merge as committed) and nothing outside them, with no commit header; a
// merge that resolved no conflict there shows nothing.
func TestRemergeDiffShowsHowAMergeResolvedItsConflictsInThePathsAsked(t *testing.T) {
	fx := newHeldMergeFixture(t)
	ctx := context.Background()
	got, err := fx.c.RemergeDiff(ctx, fx.origin, fx.resolved, ".github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml", "remerge CONFLICT (content)",
		"-<<<<<<<", "-jobs: pr", "-jobs: base", "+jobs: resolved"} {
		if !strings.Contains(got, want) {
			t.Errorf("RemergeDiff = %q, want it to hold %q", got, want)
		}
	}
	for _, not := range []string{"lint.yml", "app/a.rb", "Author:", "commit "} {
		if strings.Contains(got, not) {
			t.Errorf("RemergeDiff = %q, want no %q", got, not)
		}
	}
	if got, err := fx.c.RemergeDiff(ctx, fx.origin, fx.clean, ".github/workflows"); err != nil || got != "" {
		t.Errorf("RemergeDiff of a merge with no conflict in the workflows = %q, %v; want none", got, err)
	}
	if _, err := fx.c.RemergeDiff(ctx, fx.origin, "-x", ".github/workflows"); err == nil {
		t.Error("an option as the merge was taken")
	}
	if _, err := fx.c.RemergeDiff(ctx, fx.origin, fx.resolved); err == nil {
		t.Error("RemergeDiff of no path was taken")
	}
}

// PathsDiff is everything one commit changes against another in the
// paths asked, and nothing outside them: the workflow files the push of a
// held merge changes against the PR's head.
func TestPathsDiffIsTheDiffOfTwoCommitsInThePathsAsked(t *testing.T) {
	fx := newHeldMergeFixture(t)
	ctx := context.Background()
	got, err := fx.c.PathsDiff(ctx, fx.origin, fx.head, fx.resolved, ".github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"diff --git a/.github/workflows/ci.yml", "-jobs: pr", "+jobs: resolved",
		"diff --git a/.github/workflows/lint.yml", "-on: push", "+on: pull_request"} {
		if !strings.Contains(got, want) {
			t.Errorf("PathsDiff = %q, want it to hold %q", got, want)
		}
	}
	if strings.Contains(got, "<<<<<<<") || strings.Contains(got, "app/a.rb") {
		t.Errorf("PathsDiff = %q, want the two commits' workflow files alone", got)
	}
	if got, err := fx.c.PathsDiff(ctx, fx.origin, fx.head, fx.head, ".github/workflows"); err != nil || got != "" {
		t.Errorf("PathsDiff of one commit = %q, %v; want none", got, err)
	}
	if _, err := fx.c.PathsDiff(ctx, fx.origin, fx.head, fx.resolved); err == nil {
		t.Error("PathsDiff of no path was taken")
	}
	if _, err := fx.c.PathsDiff(ctx, fx.origin, "-x", fx.resolved, ".github"); err == nil {
		t.Error("an option as a revision was taken")
	}
}
