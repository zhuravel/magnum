package gitx

import (
	"context"
	"slices"
	"testing"
)

// A merge of the base carries the base's workflow files as the base has
// them, which is how magnum tells them from the PR's own: DifferingPaths
// names, among the paths asked (a directory or a file, taken literally),
// those whose content or presence differs between two commits, whatever
// the history between them, and nothing outside them.
func TestDifferingPathsNamesOnlyTheAskedPathsThatDifferBetweenTwoCommits(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	dir := fx.origin
	fx.write(dir, ".github/workflows/ci.yml", "on: push\n")
	fx.write(dir, ".github/workflows/lint.yml", "on: pull_request\n")
	fx.write(dir, "app/a.rb", "a\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "Base")
	base := fx.git(dir, "rev-parse", "HEAD")

	fx.write(dir, ".github/workflows/ci.yml", "on: [push, workflow_dispatch]\n")
	fx.write(dir, ".github/workflows/new.yml", "on: push\n")
	fx.write(dir, "app/a.rb", "a2\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "Head")
	head := fx.git(dir, "rev-parse", "HEAD")

	got, err := fx.c.DifferingPaths(ctx, dir, base, head, ".github/workflows")
	if want := []string{".github/workflows/ci.yml", ".github/workflows/new.yml"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("DifferingPaths of the workflows = %q, %v; want %q", got, err, want)
	}
	if got, err := fx.c.DifferingPaths(ctx, dir, base, head, ".github/workflows/lint.yml"); err != nil || got != nil {
		t.Errorf("DifferingPaths of a file both have alike = %q, %v; want nil", got, err)
	}
	if got, err := fx.c.DifferingPaths(ctx, dir, head, head, ".github/workflows"); err != nil || got != nil {
		t.Errorf("DifferingPaths of one commit = %q, %v; want nil", got, err)
	}
	if got, err := fx.c.DifferingPaths(ctx, dir, base, head, ".github/workflows/*"); err != nil || got != nil {
		t.Errorf("a glob was taken as one: %q, %v", got, err)
	}
	for _, bad := range [][]string{nil, {""}} {
		if _, err := fx.c.DifferingPaths(ctx, dir, base, head, bad...); err == nil {
			t.Errorf("DifferingPaths of paths %q was taken", bad)
		}
	}
	if _, err := fx.c.DifferingPaths(ctx, dir, "-x", head, ".github"); err == nil {
		t.Error("an option as a revision was taken")
	}
}
