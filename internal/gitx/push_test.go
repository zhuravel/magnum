package gitx

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// FirstParentIdentities lists the commits of to that from lacks on to's
// first-parent chain, newest first, each with its parents and its author's
// and committer's emails as written in the commit (no .mailmap): a merge
// is one entry, without the commits it brought in. magnum push checks that
// each is the fixer's.
func TestFirstParentIdentitiesListsTheFirstParentChainsAuthors(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.write(fx.clone, ".mailmap", "Someone <mapped@example.com> <bot@example.com>\n")
	fx.git(fx.clone, "add", ".mailmap")
	fx.git(fx.clone, "-c", "user.name=Bot", "-c", "user.email=bot@example.com", "commit", "--quiet", "--author=Bot <bot@example.com>", "-m", "mailmap")
	one := fx.git(fx.clone, "rev-parse", "HEAD")
	side := fx.commit(fx.origin, "side.txt", "side\n", "on main") // on origin's main
	fx.git(fx.clone, "fetch", "--quiet", "origin", "main")
	fx.git(fx.clone, "merge", "--quiet", "--no-edit", "origin/main")
	merge := fx.git(fx.clone, "rev-parse", "HEAD")

	got, err := fx.c.FirstParentIdentities(ctx, fx.clone, fx.base, merge)
	if err != nil {
		t.Fatal(err)
	}
	want := []CommitIdentity{
		{SHA: merge, Parents: []string{one, side}, AuthorEmail: "test@example.com", CommitterEmail: "test@example.com"},
		{SHA: one, Parents: []string{fx.base}, AuthorEmail: "bot@example.com", CommitterEmail: "test@example.com"},
	}
	if !slices.EqualFunc(got, want, func(a, b CommitIdentity) bool {
		return a.SHA == b.SHA && slices.Equal(a.Parents, b.Parents) && a.AuthorEmail == b.AuthorEmail && a.CommitterEmail == b.CommitterEmail
	}) {
		t.Errorf("FirstParentIdentities = %+v\nwant %+v", got, want)
	}
	if got, err := fx.c.FirstParentIdentities(ctx, fx.clone, merge, merge); err != nil || len(got) != 0 {
		t.Errorf("FirstParentIdentities of nothing = %+v, %v", got, err)
	}
	if _, err := fx.c.FirstParentIdentities(ctx, fx.clone, "-x", merge); err == nil {
		t.Error("an option as a revision was accepted")
	}
}

// PushLeaseVia pushes a commit to a URL's branch with a lease on the head
// the caller read, through gh as git's only credential helper, and keeps
// PushLease's guards: a commit that does not descend from the leased head
// is refused before anything is pushed, and a branch that moved since the
// read fails the push.
func TestPushLeaseViaLeasesTheHeadItWasGiven(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.git(fx.clone, "switch", "--quiet", "--detach", fx.pr)
	head := fx.commit(fx.clone, "app/a.rb", "a = 2\n", "fix")

	out, err := fx.c.LsRemoteVia(ctx, fx.clone, fx.origin, pushTimeout, "refs/heads/pr7")
	if err != nil || out != fx.pr+"\trefs/heads/pr7" {
		t.Fatalf("LsRemoteVia = %q, %v", out, err)
	}
	if err := fx.c.PushLeaseVia(ctx, fx.clone, fx.origin, fx.base, "pr7", fx.pr); !errors.Is(err, ErrNotDescendant) {
		t.Errorf("a push that rewrites history = %v, want ErrNotDescendant", err)
	}
	if err := fx.c.PushLeaseVia(ctx, fx.clone, fx.origin, head, "pr7", fx.base); err == nil {
		t.Error("a stale lease pushed")
	}
	if err := fx.c.PushLeaseVia(ctx, fx.clone, fx.origin, head, "pr7", fx.pr); err != nil {
		t.Fatalf("PushLeaseVia = %v", err)
	}
	if got := fx.git(fx.origin, "rev-parse", "refs/heads/pr7"); got != head {
		t.Errorf("origin's pr7 = %s, want %s", got, head)
	}
}
