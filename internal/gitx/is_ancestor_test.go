package gitx

import (
	"context"
	"strings"
	"testing"
)

// A related PR is stacked on the PR under review when one head is in the
// other's history (pipeline's related step): IsAncestor asks `git merge-base
// --is-ancestor` in the checkout. Its exit 1 is the answer no, logged at
// Debug; a commit the checkout lacks is an error, never a no; a commit is
// its own ancestor, as git says.
func TestRealIsAncestorAnswersWhetherOneHeadIsInTheOthersHistory(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetchPR7()
	var log levelLines
	fx.r.Log = &log
	for _, tc := range []struct {
		name, ancestor, rev string
		want                bool
	}{
		{"the base under the PR", fx.base, fx.pr, true},
		{"the PR over the base", fx.pr, fx.base, false},
		{"the same commit", fx.pr, fx.pr, true},
	} {
		got, err := fx.c.IsAncestor(ctx, fx.clone, tc.ancestor, tc.rev)
		if err != nil || got != tc.want {
			t.Errorf("%s: IsAncestor = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	no := log.find("merge-base --is-ancestor "+fx.pr+" "+fx.base, "dir="+fx.clone+" ")
	if len(no) != 1 || !strings.HasPrefix(no[0], "DEBUG ") {
		t.Errorf("the answer no logged %q, want one line at DEBUG", no)
	}
	const unknown = "ffffffffffffffffffffffffffffffffffffffff"
	if got, err := fx.c.IsAncestor(ctx, fx.clone, unknown, fx.pr); err == nil {
		t.Errorf("IsAncestor of a commit the clone lacks = %v, nil; want an error", got)
	}
}

// IsAncestor refuses a revision git would read as an option, without
// running git.
func TestIsAncestorRefusesAnOptionForARevision(t *testing.T) {
	c, f := newFake()
	if _, err := c.IsAncestor(context.Background(), slot, "--all", sha1); err == nil {
		t.Fatal("IsAncestor(--all) = nil error")
	}
	if _, err := c.IsAncestor(context.Background(), slot, sha1, "-x"); err == nil {
		t.Fatal("IsAncestor(-x) = nil error")
	}
	if len(f.Calls) != 0 {
		t.Fatalf("calls = %v, want none", f.Calls)
	}
}
