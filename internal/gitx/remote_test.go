package gitx

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
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

// OriginURLs are where git sends a fetch and a push of origin, as git
// itself resolves them: a url.<base>.insteadOf rewrites both, a
// pushInsteadOf and remote.origin.pushurl the push alone, a second
// remote.origin.url is listed too, and a URL's credentials are dropped.
// The factory's publish checks them against the repository's GitHub URL
// before it pushes.
func TestOriginURLsAreWhereGitSendsAFetchAndAPush(t *testing.T) {
	fx, bare := pushFixture(t)
	ctx := context.Background()
	elsewhere := filepath.Join(filepath.Dir(bare), "elsewhere.git")
	want := func(what string, fetch, push []string) {
		t.Helper()
		gotFetch, err := fx.c.OriginURLs(ctx, fx.clone, false)
		if err != nil || !slices.Equal(gotFetch, fetch) {
			t.Fatalf("%s: the fetch URLs are %q (%v), want %q", what, gotFetch, err, fetch)
		}
		gotPush, err := fx.c.OriginURLs(ctx, fx.clone, true)
		if err != nil || !slices.Equal(gotPush, push) {
			t.Fatalf("%s: the push URLs are %q (%v), want %q", what, gotPush, err, push)
		}
	}
	want("as cloned", []string{bare}, []string{bare})

	fx.git(fx.clone, "config", "url."+elsewhere+".pushInsteadOf", bare)
	want("a pushInsteadOf", []string{bare}, []string{elsewhere})
	fx.git(fx.clone, "config", "--unset", "url."+elsewhere+".pushInsteadOf")

	fx.git(fx.clone, "config", "remote.origin.pushurl", elsewhere)
	want("a pushurl", []string{bare}, []string{elsewhere})
	fx.git(fx.clone, "config", "--unset", "remote.origin.pushurl")

	fx.git(fx.clone, "config", "url."+elsewhere+".insteadOf", bare)
	want("an insteadOf", []string{elsewhere}, []string{elsewhere})
	fx.git(fx.clone, "config", "--unset", "url."+elsewhere+".insteadOf")

	fx.git(fx.clone, "remote", "set-url", "--add", "origin", elsewhere)
	want("a second URL", []string{bare, elsewhere}, []string{bare, elsewhere})

	fx.git(fx.clone, "config", "--unset-all", "remote.origin.url")
	fx.git(fx.clone, "config", "remote.origin.url", "https://x-access-token:a-secret@github.com/talkable/widget.git")
	want("credentials", []string{"https://github.com/talkable/widget.git"}, []string{"https://github.com/talkable/widget.git"})

	fx.git(fx.clone, "remote", "remove", "origin")
	if got, err := fx.c.OriginURLs(ctx, fx.clone, true); err == nil {
		t.Fatalf("a clone with no origin: %q, want an error", got)
	}
}

// PushLeaseApart pushes to the URL its caller checked, from a repository
// of its own, so no rewrite in the clone's config (a pushInsteadOf, an
// insteadOf of that URL) sends the push anywhere else; its lease holds as
// PushLease's does, and a URL git could read as an option, or one with
// credentials, is refused.
func TestPushLeaseApartGoesToTheURLItWasGiven(t *testing.T) {
	fx, bare := pushFixture(t)
	ctx := context.Background()
	elsewhere := filepath.Join(filepath.Dir(bare), "elsewhere.git")
	fx.git(filepath.Dir(bare), "init", "--quiet", "--bare", elsewhere)
	fx.git(fx.clone, "config", "url."+elsewhere+".pushInsteadOf", bare)
	fx.git(fx.clone, "config", "url."+elsewhere+".insteadOf", bare)
	const branch = "zhuravel-PR-12430-copy"
	fx.git(fx.clone, "switch", "--quiet", "-c", branch)
	head := fx.commit(fx.clone, "app/credit.rb", "points = 10\n", "PR-12430 counts the points")

	if err := fx.c.PushLeaseApart(ctx, fx.clone, bare, head, branch, ""); err != nil {
		t.Fatalf("PushLeaseApart = %v", err)
	}
	if got := fx.git(bare, "rev-parse", "refs/heads/"+branch); got != head {
		t.Fatalf("the remote's %s is at %q, want %s", branch, got, head)
	}
	if got := fx.git(elsewhere, "for-each-ref", "--format=%(refname)"); got != "" {
		t.Fatalf("the clone's rewrite got %q: the push followed the clone's config", got)
	}
	next := fx.commit(fx.clone, "app/credit.rb", "points = 25\n", "PR-12430 counts the copies")
	if err := fx.c.PushLeaseApart(ctx, fx.clone, bare, next, branch, ""); err == nil {
		t.Fatal("a lease for a new branch pushed over one that exists")
	}
	for _, bad := range []string{"", "-oProxyCommand=x", "https://user:pw@github.com/talkable/widget.git"} {
		if err := fx.c.PushLeaseApart(ctx, fx.clone, bad, next, branch, head); err == nil {
			t.Errorf("PushLeaseApart to %q went through", bad)
		}
	}
}

// With HTTPSFetch a github.com SSH URL is pushed over HTTPS with gh as
// git's only credential helper, as PushLease reaches such an origin, and
// from a repository of its own (--git-dir), never from the clone.
func TestPushLeaseApartGoesOverHTTPSAsFetchesDo(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule(filepath.Join(clone, ".git")+"\n", "git", "-C", clone, "rev-parse"), okRule("git"))
	c.HTTPSFetch = true
	sha := strings.Repeat("ab", 20)
	if err := c.PushLeaseApart(ctx, clone, "git@github.com:talkable/widget.git", sha, "b", ""); err != nil {
		t.Fatal(err)
	}
	var push []string
	for _, call := range f.Calls {
		if slices.Contains(call.Args, "push") {
			push = call.Args
		}
	}
	if len(push) == 0 || !strings.HasPrefix(push[0], "--git-dir=") ||
		!slices.Contains(push, "credential.helper=!gh auth git-credential") || !slices.Contains(push, "https://github.com/talkable/widget.git") {
		t.Fatalf("the push ran as %q, want it from a repository of its own to the HTTPS URL with gh's helper", push)
	}
}
