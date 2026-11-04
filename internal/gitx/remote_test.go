package gitx

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// pushFixture is a clone whose origin is a bare repository: the factory's
// copies push to such a remote.
func pushFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	fx := newFixture(t)
	bare := filepath.Join(filepath.Dir(fx.origin), "remote.git")
	fx.git(filepath.Dir(fx.origin), "clone", "--quiet", "--bare", fx.origin, bare)
	fx.git(fx.clone, "remote", "set-url", "origin", bare)
	return fx, bare
}

// remoteTip is branch's head on the bare remote, "" when it has none.
func remoteTip(t *testing.T, fx *fixture, bare, branch string) string {
	t.Helper()
	out, err := fx.c.LsRemote(context.Background(), fx.clone, 0, "refs/heads/"+branch)
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		return ""
	}
	sha, _, _ := strings.Cut(out, "\t")
	return sha
}

// A push with a lease goes through only while the remote branch is where
// the caller last saw it: a new branch must not exist yet (expect ""), an
// existing one must still be at expect. A branch someone moved since is
// left as they left it.
func TestPushLeaseRefusesWhenTheRemoteBranchMoved(t *testing.T) {
	fx, bare := pushFixture(t)
	ctx := context.Background()
	const branch = "zhuravel-PR-12360-copy"
	fx.git(fx.clone, "switch", "--quiet", "-c", branch)
	first := fx.commit(fx.clone, "app/credit.rb", "points = 10\n", "PR-12360 counts the points")

	if err := fx.c.PushLease(ctx, fx.clone, first, branch, ""); err != nil {
		t.Fatalf("PushLease of a new branch: %v", err)
	}
	if got := remoteTip(t, fx, bare, branch); got != first {
		t.Fatalf("remote %s at %q, want %s", branch, got, first)
	}
	// A new branch's lease (expect "") refuses one that exists.
	second := fx.commit(fx.clone, "app/credit.rb", "points = 25\n", "PR-12360 counts the copies")
	if err := fx.c.PushLease(ctx, fx.clone, second, branch, ""); err == nil {
		t.Fatal("a lease for a new branch pushed over one that exists")
	}

	// Somebody pushes the branch on: the lease at first is refused.
	other := filepath.Join(filepath.Dir(fx.clone), "other")
	fx.git(filepath.Dir(fx.clone), "clone", "--quiet", "--branch", branch, bare, other)
	moved := fx.commit(other, "app/notes.rb", "note = 1\n", "a colleague's commit")
	fx.git(other, "push", "--quiet", "origin", branch)
	fx.git(fx.clone, "fetch", "--quiet", "origin")
	third := fx.commit(fx.clone, "app/credit.rb", "points = 30\n", "PR-12360 rounds the points")
	if err := fx.c.PushLease(ctx, fx.clone, third, branch, first); err == nil {
		t.Fatal("PushLease went through after the remote branch moved")
	}
	if got := remoteTip(t, fx, bare, branch); got != moved {
		t.Fatalf("remote %s at %s after the refused push, want the colleague's %s", branch, got, moved)
	}

	// At the head it expects, the lease goes through.
	if err := fx.c.PushLease(ctx, fx.clone, second, branch, moved); err == nil {
		t.Fatal("PushLease pushed a commit that does not descend from the remote head")
	}
	fx.git(fx.clone, "merge", "--quiet", "--no-edit", "origin/"+branch)
	merged := fx.git(fx.clone, "rev-parse", "HEAD")
	if err := fx.c.PushLease(ctx, fx.clone, merged, branch, moved); err != nil {
		t.Fatalf("PushLease at the expected head: %v", err)
	}
	if got := remoteTip(t, fx, bare, branch); got != merged {
		t.Fatalf("remote %s at %s, want %s", branch, got, merged)
	}
}

// The lease never rewrites history: a commit that does not descend from
// the remote head it expects is refused before anything is pushed, and the
// remote branch keeps its commits. A sha that is no full commit id, a bad
// branch name or an expect that is not one is refused too.
func TestPushLeaseNeverRewritesHistory(t *testing.T) {
	fx, bare := pushFixture(t)
	ctx := context.Background()
	const branch = "zhuravel-PR-12361-copy"
	fx.git(fx.clone, "switch", "--quiet", "-c", branch)
	pushed := fx.commit(fx.clone, "app/credit.rb", "points = 10\n", "PR-12361 counts the points")
	if err := fx.c.PushLease(ctx, fx.clone, pushed, branch, ""); err != nil {
		t.Fatal(err)
	}
	fx.git(fx.clone, "reset", "--quiet", "--hard", fx.base)
	rewritten := fx.commit(fx.clone, "app/credit.rb", "points = 11\n", "PR-12361 counts the points, again")

	err := fx.c.PushLease(ctx, fx.clone, rewritten, branch, pushed)
	if err == nil || !errors.Is(err, ErrNotDescendant) {
		t.Fatalf("PushLease of a rewritten branch = %v, want ErrNotDescendant", err)
	}
	if got := remoteTip(t, fx, bare, branch); got != pushed {
		t.Fatalf("remote %s at %s, want it kept at %s", branch, got, pushed)
	}
	for _, bad := range []struct{ sha, branch, expect string }{
		{"HEAD", branch, pushed},
		{rewritten, "-delete", pushed},
		{rewritten, "a..b", pushed},
		{rewritten, branch, "origin/" + branch},
	} {
		if err := fx.c.PushLease(ctx, fx.clone, bad.sha, bad.branch, bad.expect); err == nil {
			t.Errorf("PushLease(%q, %q, %q) went through", bad.sha, bad.branch, bad.expect)
		}
	}
	if got := remoteTip(t, fx, bare, branch); got != pushed {
		t.Fatalf("remote %s at %s, want it kept at %s", branch, got, pushed)
	}
}
