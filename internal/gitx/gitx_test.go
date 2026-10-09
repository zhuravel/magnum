package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

const (
	clone = "/repos/talkable"
	slot  = "/repos/talkable.review1"
	sha1  = "0123456789abcdef0123456789abcdef01234567"
	sha2  = "89abcdef0123456789abcdef0123456789abcdef"
)

func newFake(rules ...execx.Rule) (*Client, *execx.Fake) {
	f := &execx.Fake{Rules: rules}
	return New(f), f
}

func okRule(prefix ...string) execx.Rule { return execx.Rule{Prefix: prefix} }

func outRule(stdout string, prefix ...string) execx.Rule {
	return execx.Rule{Prefix: prefix, Result: execx.Result{Stdout: []byte(stdout)}}
}

func exitRule(code int, stderr string, prefix ...string) execx.Rule {
	return execx.Rule{Prefix: prefix, Result: execx.Result{Code: code, Stderr: []byte(stderr)}}
}

func argv(c execx.Cmd) []string { return append([]string{c.Name}, c.Args...) }

// wantCall asserts the i-th recorded call has exactly this argv and mutability.
func wantCall(t *testing.T, f *execx.Fake, i int, mutates bool, want ...string) execx.Cmd {
	t.Helper()
	if i >= len(f.Calls) {
		t.Fatalf("call %d missing, only %d recorded: %v", i, len(f.Calls), f.Calls)
	}
	c := f.Calls[i]
	if got := argv(c); !slices.Equal(got, want) {
		t.Fatalf("call %d argv\n got: %q\nwant: %q", i, got, want)
	}
	if c.Mutates != mutates {
		t.Fatalf("call %d (%s): Mutates=%v want %v", i, c.String(), c.Mutates, mutates)
	}
	if c.Label == "" {
		t.Fatalf("call %d (%s): empty Label", i, c.String())
	}
	return c
}

func TestFetchPRArgvAndSha(t *testing.T) {
	c, f := newFake(
		okRule("git", "-C", clone, "fetch"),
		outRule(sha1+"\n", "git", "-C", clone, "rev-parse"),
	)
	got, err := c.FetchPR(context.Background(), clone, 42)
	if err != nil || got != sha1 {
		t.Fatalf("FetchPR = %q, %v", got, err)
	}
	fetch := wantCall(t, f, 0, true, "git", "-C", clone, "fetch", "--no-tags", "origin", "+refs/pull/42/head:refs/magnum/pr/42")
	if fetch.Dir != clone {
		t.Errorf("Dir = %q, want %q", fetch.Dir, clone)
	}
	if fetch.Env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("fetch must disable credential prompts, env=%v", fetch.Env)
	}
	if fetch.Timeout < time.Minute {
		t.Errorf("fetch timeout %s too small for a big repo", fetch.Timeout)
	}
	wantCall(t, f, 1, false, "git", "-C", clone, "rev-parse", "--verify", "--quiet", "refs/magnum/pr/42^{commit}")
}

func TestFetchPRErrors(t *testing.T) {
	c, _ := newFake()
	if _, err := c.FetchPR(context.Background(), clone, 0); err == nil {
		t.Error("PR number 0 must be rejected")
	}
	if _, err := c.FetchPR(context.Background(), "", 3); err == nil {
		t.Error("empty clone dir must be rejected")
	}

	c, _ = newFake(exitRule(128, "fatal: couldn't find remote ref refs/pull/9/head", "git", "-C", clone, "fetch"))
	_, err := c.FetchPR(context.Background(), clone, 9)
	var ee *execx.ExitError
	if !errors.As(err, &ee) || ee.Code != 128 || !strings.Contains(err.Error(), "PR 9") {
		t.Fatalf("want wrapped ExitError mentioning PR 9, got %v", err)
	}
}

func TestFetchBranchArgv(t *testing.T) {
	c, f := newFake(okRule("git"))
	if err := c.FetchBranch(context.Background(), clone, "master"); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, 0, true, "git", "-C", clone, "fetch", "--no-tags", "origin", "+refs/heads/master:refs/remotes/origin/master")
	for _, bad := range []string{"", "-x", "a b", "a:b", "a..b", "--upload-pack=x"} {
		if err := c.FetchBranch(context.Background(), clone, bad); err == nil {
			t.Errorf("branch %q must be rejected", bad)
		}
	}
}

func TestFetchesAreSerializedPerClone(t *testing.T) {
	var inflight, peak atomic.Int32
	slow := execx.Rule{Prefix: []string{"git", "-C", clone, "fetch"}, Fn: func(execx.Cmd) (execx.Result, error) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		inflight.Add(-1)
		return execx.Result{}, nil
	}}
	c, _ := newFake(slow)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if err := c.FetchBranch(context.Background(), clone, "master"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("fetches on one clone overlapped (peak %d)", peak.Load())
	}
}

func TestFetchLockHonorsContext(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	block := execx.Rule{Prefix: []string{"git", "-C", clone, "fetch"}, Fn: func(execx.Cmd) (execx.Result, error) {
		close(started)
		<-release
		return execx.Result{}, nil
	}}
	c, _ := newFake(block)
	done := make(chan error, 1)
	go func() { done <- c.FetchBranch(context.Background(), clone, "master") }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := c.FetchBranch(ctx, clone, "other")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline while waiting for the clone lock, got %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRevParse(t *testing.T) {
	c, f := newFake(outRule(sha1+"\n", "git", "-C", slot, "rev-parse"))
	got, err := c.RevParse(context.Background(), slot, "HEAD")
	if err != nil || got != sha1 {
		t.Fatalf("RevParse = %q, %v", got, err)
	}
	wantCall(t, f, 0, false, "git", "-C", slot, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")

	c, _ = newFake(exitRule(1, "", "git", "-C", slot, "rev-parse"))
	if _, err := c.RevParse(context.Background(), slot, "refs/magnum/pr/7"); !errors.Is(err, ErrNoSuchRef) {
		t.Fatalf("exit 1 must map to ErrNoSuchRef, got %v", err)
	}

	c, _ = newFake(exitRule(128, "fatal: not a git repository", "git", "-C", slot, "rev-parse"))
	if _, err := c.RevParse(context.Background(), slot, "HEAD"); err == nil || errors.Is(err, ErrNoSuchRef) {
		t.Fatalf("exit 128 is a real failure, got %v", err)
	}

	c, _ = newFake(outRule("not-a-sha\n", "git", "-C", slot, "rev-parse"))
	if _, err := c.RevParse(context.Background(), slot, "HEAD"); err == nil {
		t.Fatal("non-oid output must be rejected")
	}
	if _, err := c.RevParse(context.Background(), slot, "--all"); err == nil {
		t.Fatal("option-looking ref must be rejected")
	}
}

func TestSwitchDetachArgv(t *testing.T) {
	c, f := newFake(okRule("git"))
	if err := c.SwitchDetach(context.Background(), slot, "refs/magnum/pr/42"); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, 0, true, "git", "-C", slot, "switch", "--quiet", "--discard-changes", "--detach", "refs/magnum/pr/42")
	if err := c.SwitchDetach(context.Background(), slot, "-f"); err == nil {
		t.Error("option-looking ref must be rejected")
	}
}

func TestResetPlaceholderArgv(t *testing.T) {
	// No upstream configured (config --get exits 1): nothing to unset.
	c, f := newFake(
		exitRule(1, "", "git", "-C", slot, "config"),
		okRule("git"),
	)
	if err := c.ResetPlaceholder(context.Background(), slot, "review1", "master"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("calls = %d, want the reset and the upstream check only: %v", len(f.Calls), f.Calls)
	}
	wantCall(t, f, 0, true, "git", "-C", slot, "switch", "--quiet", "--discard-changes", "--no-track", "-C", "review1", "origin/master")
	wantCall(t, f, 1, false, "git", "-C", slot, "config", "--get", "branch.review1.merge")

	// An upstream survived the reset: it is unset.
	c, f = newFake(
		outRule("refs/heads/master\n", "git", "-C", slot, "config"),
		okRule("git"),
	)
	if err := c.ResetPlaceholder(context.Background(), slot, "review1", "master"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 3 {
		t.Fatalf("calls = %d, want reset, check and unset: %v", len(f.Calls), f.Calls)
	}
	wantCall(t, f, 2, true, "git", "-C", slot, "branch", "--unset-upstream", "review1")

	// An empty value names no upstream: nothing to unset.
	c, f = newFake(okRule("git"))
	if err := c.ResetPlaceholder(context.Background(), slot, "review1", "master"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("calls = %d, want the reset and the upstream check only: %v", len(f.Calls), f.Calls)
	}

	// Any other failure of the check is an error, not a skipped unset.
	c, _ = newFake(exitRule(128, "fatal: bad config", "git", "-C", slot, "config"), okRule("git"))
	if err := c.ResetPlaceholder(context.Background(), slot, "review1", "master"); err == nil {
		t.Error("a failing upstream check must surface")
	}

	c, f = newFake(okRule("git"))
	if err := c.ResetPlaceholder(context.Background(), slot, "-C", "master"); err == nil {
		t.Error("option-looking branch must be rejected")
	}
	if err := c.ResetPlaceholder(context.Background(), slot, "review1", ""); err == nil {
		t.Error("empty base must be rejected")
	}
	if len(f.Calls) != 0 {
		t.Errorf("rejected calls must not reach git, %d calls", len(f.Calls))
	}
}

func TestWorktreeAddVariants(t *testing.T) {
	wt := "/repos/talkable__worktrees/pr-42"
	c, f := newFake(okRule("git"))
	ctx := context.Background()

	if err := c.WorktreeAdd(ctx, clone, wt, "refs/magnum/pr/42", true, ""); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, 0, true, "git", "-C", clone, "worktree", "add", "--quiet", "--detach", wt, "refs/magnum/pr/42")

	if err := c.WorktreeAdd(ctx, clone, "/repos/talkable.review7", "origin/master", false, "review7"); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, 1, true, "git", "-C", clone, "worktree", "add", "--quiet", "--no-track", "-b", "review7", "/repos/talkable.review7", "origin/master")

	if err := c.WorktreeAdd(ctx, clone, wt, "refs/magnum/pr/42", false, ""); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, 2, true, "git", "-C", clone, "worktree", "add", "--quiet", wt, "refs/magnum/pr/42")

	if f.Calls[0].Timeout < time.Minute {
		t.Errorf("worktree add timeout %s too small for a checkout of a big tree", f.Calls[0].Timeout)
	}

	n := len(f.Calls)
	for name, err := range map[string]error{
		"detach+branch": c.WorktreeAdd(ctx, clone, wt, "HEAD", true, "x"),
		"relative path": c.WorktreeAdd(ctx, clone, "rel/pr-1", "HEAD", true, ""),
		"empty ref":     c.WorktreeAdd(ctx, clone, wt, "", true, ""),
		"bad branch":    c.WorktreeAdd(ctx, clone, wt, "HEAD", false, "-b"),
		"dash path":     c.WorktreeAdd(ctx, clone, "-f", "HEAD", true, ""),
	} {
		if err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
	if len(f.Calls) != n {
		t.Errorf("rejected calls must not reach git, %d extra", len(f.Calls)-n)
	}
}

func TestWorktreeRemoveAndPrune(t *testing.T) {
	wt := "/repos/talkable__worktrees/pr-42"
	c, f := newFake(okRule("git"))
	ctx := context.Background()
	if err := c.WorktreeRemove(ctx, clone, wt, false); err != nil {
		t.Fatal(err)
	}
	if err := c.WorktreeRemove(ctx, clone, wt, true); err != nil {
		t.Fatal(err)
	}
	if err := c.WorktreePrune(ctx, clone); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, 0, true, "git", "-C", clone, "worktree", "remove", wt)
	wantCall(t, f, 1, true, "git", "-C", clone, "worktree", "remove", "--force", wt)
	wantCall(t, f, 2, true, "git", "-C", clone, "worktree", "prune")
	if err := c.WorktreeRemove(ctx, clone, "relative", false); err == nil {
		t.Error("relative worktree path must be rejected")
	}
}

const porcelainFixture = `worktree /Users/b/Projects/talkable
HEAD 79e2bd17ceeed77639afd4a476a4a69b2db6ff89
branch refs/heads/feature/some-work

worktree /Users/b/Projects/talkable.review1
HEAD 8c714b151cf3b65ce6a45006de8422a467d7967d
branch refs/heads/review1

worktree /Users/b/Projects/talkable__worktrees/pr-42
HEAD 3cdf82c3f06b32e07dcee9396ae32e904ff33ae3
detached

worktree /Users/b/Projects/with space/pr-43
HEAD 1111111111111111111111111111111111111111
detached
locked

worktree /Users/b/Projects/locked-reason
HEAD 2222222222222222222222222222222222222222
branch refs/heads/x
locked in use by magnum

worktree /Users/b/Projects/gone
HEAD 3333333333333333333333333333333333333333
detached
prunable gitdir file points to non-existent location

worktree /Users/b/Projects/bare.git
bare
`

func TestWorktreeListParsing(t *testing.T) {
	c, f := newFake(outRule(porcelainFixture, "git", "-C", clone, "worktree", "list"))
	got, err := c.WorktreeList(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, 0, false, "git", "-C", clone, "worktree", "list", "--porcelain")
	want := []Worktree{
		{Path: "/Users/b/Projects/talkable", Head: "79e2bd17ceeed77639afd4a476a4a69b2db6ff89", Branch: "feature/some-work"},
		{Path: "/Users/b/Projects/talkable.review1", Head: "8c714b151cf3b65ce6a45006de8422a467d7967d", Branch: "review1"},
		{Path: "/Users/b/Projects/talkable__worktrees/pr-42", Head: "3cdf82c3f06b32e07dcee9396ae32e904ff33ae3", Detached: true},
		{Path: "/Users/b/Projects/with space/pr-43", Head: "1111111111111111111111111111111111111111", Detached: true, Locked: true},
		{Path: "/Users/b/Projects/locked-reason", Head: "2222222222222222222222222222222222222222", Branch: "x", Locked: true, LockReason: "in use by magnum"},
		{Path: "/Users/b/Projects/gone", Head: "3333333333333333333333333333333333333333", Detached: true, Prunable: true},
		{Path: "/Users/b/Projects/bare.git", Bare: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d worktrees, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("worktree %d\n got: %+v\nwant: %+v", i, got[i], want[i])
		}
	}
}

func TestWorktreeListEmptyAndNoTrailingBlank(t *testing.T) {
	c, _ := newFake(outRule("", "git"))
	got, err := c.WorktreeList(context.Background(), clone)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty output: %v %v", got, err)
	}
	c, _ = newFake(outRule("worktree /a\r\nHEAD "+sha1+"\r\nbranch refs/heads/m", "git"))
	got, err = c.WorktreeList(context.Background(), clone)
	if err != nil || len(got) != 1 || got[0].Path != "/a" || got[0].Branch != "m" {
		t.Fatalf("CRLF/no trailing blank: %+v %v", got, err)
	}
}

func TestFindWorktree(t *testing.T) {
	list := []Worktree{{Path: "/a/b"}, {Path: "/a/c/", Branch: "x"}}
	if w, ok := FindWorktree(list, "/a/c"); !ok || w.Branch != "x" {
		t.Errorf("trailing slash must match: %+v %v", w, ok)
	}
	if _, ok := FindWorktree(list, "/a/d"); ok {
		t.Error("unexpected match")
	}
	if _, ok := FindWorktree(list, ""); ok {
		t.Error("empty path must not match")
	}
}

func TestBranchDelete(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(okRule("git", "-C", clone, "branch"))
	if err := c.BranchDelete(ctx, clone, "review1", false); err != nil {
		t.Fatal(err)
	}
	if err := c.BranchDelete(ctx, clone, "review1", true); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, 0, true, "git", "-C", clone, "branch", "-d", "review1")
	wantCall(t, f, 1, true, "git", "-C", clone, "branch", "-D", "review1")

	// Already gone counts as success.
	c, _ = newFake(
		exitRule(1, "error: branch 'review1' not found.", "git", "-C", clone, "branch"),
		exitRule(1, "", "git", "-C", clone, "rev-parse"),
	)
	if err := c.BranchDelete(ctx, clone, "review1", true); err != nil {
		t.Fatalf("missing branch must be a no-op, got %v", err)
	}

	// Exists but git refused (checked out, unmerged): surface the error.
	c, _ = newFake(
		exitRule(1, "error: cannot delete branch 'review1' used by worktree", "git", "-C", clone, "branch"),
		outRule(sha1, "git", "-C", clone, "rev-parse"),
	)
	if err := c.BranchDelete(ctx, clone, "review1", true); err == nil {
		t.Fatal("refused delete of an existing branch must error")
	}
	if err := c.BranchDelete(ctx, clone, "-D", false); err == nil {
		t.Error("option-looking branch must be rejected")
	}
}

func TestUpdateRefDelete(t *testing.T) {
	ctx := context.Background()
	// Not a symbolic ref (symbolic-ref -q exits 1): deleted without following.
	c, f := newFake(exitRule(1, "", "git", "-C", clone, "symbolic-ref"), okRule("git"))
	if err := c.UpdateRefDelete(ctx, clone, PRRef(42)); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, 0, false, "git", "-C", clone, "symbolic-ref", "-q", "refs/magnum/pr/42")
	wantCall(t, f, 1, true, "git", "-C", clone, "update-ref", "-d", "--no-deref", "refs/magnum/pr/42")

	// A symbolic ref inside refs/magnum/ is fine; one leaving it is refused
	// and nothing is deleted.
	c, f = newFake(outRule("refs/magnum/pr/1\n", "git", "-C", clone, "symbolic-ref"), okRule("git"))
	if err := c.UpdateRefDelete(ctx, clone, PRRef(42)); err != nil {
		t.Fatalf("symbolic ref within refs/magnum/: %v", err)
	}
	wantCall(t, f, 1, true, "git", "-C", clone, "update-ref", "-d", "--no-deref", "refs/magnum/pr/42")
	c, f = newFake(outRule("refs/heads/main\n", "git", "-C", clone, "symbolic-ref"), okRule("git"))
	if err := c.UpdateRefDelete(ctx, clone, PRRef(42)); err == nil || !strings.Contains(err.Error(), "refs/heads/main") {
		t.Fatalf("symbolic ref to a branch must be refused, got %v", err)
	}
	if len(f.CallsWithPrefix("git", "-C", clone, "update-ref")) != 0 {
		t.Fatal("update-ref must not run for a refused ref")
	}

	// symbolic-ref failing for another reason is surfaced, not ignored.
	c, f = newFake(exitRule(128, "fatal: bad ref", "git", "-C", clone, "symbolic-ref"), okRule("git"))
	if err := c.UpdateRefDelete(ctx, clone, PRRef(42)); err == nil {
		t.Fatal("a failing symbolic-ref check must surface")
	}
	if len(f.CallsWithPrefix("git", "-C", clone, "update-ref")) != 0 {
		t.Fatal("update-ref must not run when the check failed")
	}

	c, f = newFake(okRule("git"))
	for _, bad := range []string{"", "refs/heads/master", "refs/tags/v1", "HEAD", "refs/remotes/origin/master", "refs/magnum/../heads/master", "refs/magnum/"} {
		if err := c.UpdateRefDelete(ctx, clone, bad); err == nil {
			t.Errorf("%q must be refused (only refs/magnum/* may be deleted)", bad)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("refused refs must not reach git: %v", f.Calls)
	}
}

func TestStatusCounts(t *testing.T) {
	statusRule := func(out string) execx.Rule { return outRule(out, "git", "-C", slot, "status") }
	ctx := context.Background()

	c, f := newFake(statusRule(""))
	st, err := c.Status(ctx, slot)
	if err != nil || st.Dirty() {
		t.Fatalf("clean tree: %+v %v", st, err)
	}
	cmd := wantCall(t, f, 0, false, "git", "-C", slot, "status", "--porcelain", "--untracked-files=normal")
	if cmd.Env["GIT_OPTIONAL_LOCKS"] != "0" {
		t.Errorf("status must not take the index lock, env=%v", cmd.Env)
	}
	if st.UntrackedOnly() {
		t.Error("clean tree is not 'untracked only'")
	}

	c, _ = newFake(statusRule(" M app/a.rb\n?? tmp/x\n"))
	st, err = c.Status(ctx, slot)
	if err != nil || st.Tracked != 1 || st.Untracked != 1 || !st.Dirty() || st.UntrackedOnly() {
		t.Fatalf("mixed: %+v %v", st, err)
	}

	c, _ = newFake(statusRule("?? a\n?? b\n!! ignored\n"))
	st, _ = c.Status(ctx, slot)
	if st.Tracked != 0 || st.Untracked != 2 || !st.UntrackedOnly() || !st.Dirty() {
		t.Fatalf("untracked only: %+v", st)
	}

	c, _ = newFake(statusRule("UU conflict.rb\nA  new.rb\nR  a -> b\n"))
	st, _ = c.Status(ctx, slot)
	if st.Tracked != 3 || st.Untracked != 0 || st.UntrackedOnly() {
		t.Fatalf("tracked kinds: %+v", st)
	}
}

func TestParseStatusZ(t *testing.T) {
	const nul = "\x00"
	in := " M app/a.rb" + nul +
		"?? notes with space.txt" + nul +
		"?? line\nbreak.txt" + nul +
		"R  new name.rb" + nul + "old name.rb" + nul +
		"A  -dash.rb" + nul +
		"MR renamed in tree.rb" + nul + "src.rb" + nul +
		"C  copy.rb" + nul + "orig.rb" + nul +
		"UU conflict.rb" + nul +
		"!! tmp/cache" + nul +
		"?? ünï/čödé.rb" + nul
	want := []StatusEntry{
		{X: ' ', Y: 'M', Path: "app/a.rb"},
		{X: '?', Y: '?', Path: "notes with space.txt"},
		{X: '?', Y: '?', Path: "line\nbreak.txt"},
		{X: 'R', Y: ' ', Path: "new name.rb", OrigPath: "old name.rb"},
		{X: 'A', Y: ' ', Path: "-dash.rb"},
		{X: 'M', Y: 'R', Path: "renamed in tree.rb", OrigPath: "src.rb"},
		{X: 'C', Y: ' ', Path: "copy.rb", OrigPath: "orig.rb"},
		{X: 'U', Y: 'U', Path: "conflict.rb"},
		{X: '!', Y: '!', Path: "tmp/cache"},
		{X: '?', Y: '?', Path: "ünï/čödé.rb"},
	}
	got := parseStatusZ([]byte(in))
	if !slices.Equal(got, want) {
		t.Fatalf("parseStatusZ\n got: %+v\nwant: %+v", got, want)
	}
	if !got[1].Untracked() || got[0].Untracked() || !got[8].Ignored() || got[1].Ignored() {
		t.Error("Untracked/Ignored predicates")
	}
	for _, empty := range []string{"", nul, "garbage"} {
		if got := parseStatusZ([]byte(empty)); len(got) != 0 {
			t.Errorf("parseStatusZ(%q) = %+v, want nothing", empty, got)
		}
	}
	// A rename whose source field is missing (truncated output) keeps the entry.
	if got := parseStatusZ([]byte("R  only-new.rb" + nul)); len(got) != 1 || got[0].Path != "only-new.rb" || got[0].OrigPath != "" {
		t.Errorf("truncated rename: %+v", got)
	}
}

func TestStatusPathsArgv(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule(" M a.rb\x00?? b c.txt\x00", "git", "-C", slot, "status"))
	got, err := c.StatusPaths(ctx, slot)
	if err != nil || !slices.Equal(got, []StatusEntry{{X: ' ', Y: 'M', Path: "a.rb"}, {X: '?', Y: '?', Path: "b c.txt"}}) {
		t.Fatalf("StatusPaths = %+v, %v", got, err)
	}
	cmd := wantCall(t, f, 0, false, "git", "-C", slot, "status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if cmd.Env["GIT_OPTIONAL_LOCKS"] != "0" {
		t.Errorf("status must not take the index lock, env=%v", cmd.Env)
	}

	c, _ = newFake(exitRule(128, "fatal: not a git repository", "git"))
	if _, err := c.StatusPaths(ctx, slot); err == nil {
		t.Error("git failure must surface")
	}
	if _, err := c.StatusPaths(ctx, ""); err == nil {
		t.Error("empty dir must be rejected")
	}
}

func TestMergeBase(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule(sha1+"\n", "git", "-C", slot, "merge-base"))
	got, err := c.MergeBase(ctx, slot, "origin/master", "HEAD")
	if err != nil || got != sha1 {
		t.Fatalf("MergeBase = %q, %v", got, err)
	}
	wantCall(t, f, 0, false, "git", "-C", slot, "merge-base", "origin/master", "HEAD")

	c, _ = newFake(exitRule(1, "", "git", "-C", slot, "merge-base"))
	if _, err := c.MergeBase(ctx, slot, "a", "b"); !errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("want ErrNoMergeBase, got %v", err)
	}
	if _, err := c.MergeBase(ctx, slot, "--all", "b"); err == nil {
		t.Error("option-looking rev must be rejected")
	}
}

// FileDiff diffs one file the way GitHub shows a PR's patch, whatever the
// user's diff settings, with the path taken literally from the top.
func TestFileDiff(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule("@@ -1 +1 @@\n-a\n+b\n", "git", "-C", ".", "diff"))
	got, err := c.FileDiff(ctx, ".", sha1, "HEAD", ":(exclude)app/x.rb")
	if err != nil || got != "@@ -1 +1 @@\n-a\n+b\n" {
		t.Fatalf("FileDiff = %q, %v", got, err)
	}
	wantCall(t, f, 0, false, "git", "-C", ".", "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--unified=3",
		"--inter-hunk-context=0", sha1, "HEAD", "--", ":(top,literal):(exclude)app/x.rb")
	if _, err := c.FileDiff(ctx, ".", "--output=x", "HEAD", "a"); err == nil {
		t.Error("option-looking rev must be rejected")
	}
	if _, err := c.FileDiff(ctx, ".", sha1, "HEAD", ""); err == nil {
		t.Error("an empty path must be rejected")
	}
}

func TestChangedPaths(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule("db/migrate/1_a.rb\x00db/schema.rb\x00", "git", "-C", slot, "diff"))
	got, err := c.ChangedPaths(ctx, slot, "origin/master", "HEAD", "db/")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"db/migrate/1_a.rb", "db/schema.rb"}; !slices.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	cmd := wantCall(t, f, 0, false, "git", "-C", slot, "diff", "--name-only", "-z", "--no-renames", "origin/master...HEAD", "--", "db/")
	if cmd.Env["GIT_OPTIONAL_LOCKS"] != "0" {
		t.Errorf("env=%v", cmd.Env)
	}

	c, f = newFake(outRule("", "git"))
	got, err = c.ChangedPaths(ctx, slot, "origin/master", "HEAD")
	if err != nil || len(got) != 0 {
		t.Fatalf("no changes: %q %v", got, err)
	}
	wantCall(t, f, 0, false, "git", "-C", slot, "diff", "--name-only", "-z", "--no-renames", "origin/master...HEAD", "--")

	c, f = newFake(outRule("a b/ü.rb\x00", "git"))
	got, _ = c.ChangedPaths(ctx, slot, "x", "y", "db/", ":(exclude)db/seeds")
	if !slices.Equal(got, []string{"a b/ü.rb"}) {
		t.Fatalf("NUL parsing must keep odd names intact: %q", got)
	}
	wantCall(t, f, 0, false, "git", "-C", slot, "diff", "--name-only", "-z", "--no-renames", "x...y", "--", "db/", ":(exclude)db/seeds")

	if _, err := c.ChangedPaths(ctx, slot, "-x", "y"); err == nil {
		t.Error("option-looking base must be rejected")
	}
	if _, err := c.ChangedPaths(ctx, slot, "a...b", "y"); err == nil {
		t.Error("range syntax inside base must be rejected")
	}
}

// ModifiedPaths is ChangedPaths without the files the head adds: those have
// no history on the base.
func TestModifiedPaths(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule("app/a b.rb\x00db/old.rb\x00", "git", "-C", slot, "diff"))
	got, err := c.ModifiedPaths(ctx, slot, sha1, "HEAD")
	if err != nil || !slices.Equal(got, []string{"app/a b.rb", "db/old.rb"}) {
		t.Fatalf("ModifiedPaths = %q, %v", got, err)
	}
	cmd := wantCall(t, f, 0, false, "git", "-C", slot, "diff", "--name-only", "-z", "--no-renames", "--diff-filter=a", sha1+"...HEAD", "--")
	if cmd.Env["GIT_OPTIONAL_LOCKS"] != "0" {
		t.Errorf("env=%v", cmd.Env)
	}
	for _, bad := range [][2]string{{"-x", "y"}, {"a..b", "y"}, {"x", ""}} {
		if _, err := c.ModifiedPaths(ctx, slot, bad[0], bad[1]); err == nil {
			t.Errorf("ModifiedPaths(%q, %q) must be refused", bad[0], bad[1])
		}
	}
}

// FileLog reads a file's last commits as NUL-separated fields, so a subject
// keeps any character but NUL and an empty subject keeps its place; the path
// is taken literally from the repository's top.
func TestFileLog(t *testing.T) {
	ctx := context.Background()
	out := "abc1234\x002026-09-30\x00Fix flaky mock generation; see \"x\" (#11391)\x00" +
		"def5678\x002026-09-01\x00\x00"
	c, f := newFake(outRule(out, "git", "-C", slot, "log"))
	got, err := c.FileLog(ctx, slot, "origin/master", "spec/support/*.rb", 8)
	if err != nil {
		t.Fatal(err)
	}
	want := []Commit{
		{SHA: "abc1234", Date: "2026-09-30", Subject: "Fix flaky mock generation; see \"x\" (#11391)"},
		{SHA: "def5678", Date: "2026-09-01"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("FileLog = %+v, want %+v", got, want)
	}
	cmd := wantCall(t, f, 0, false, "git", "-C", slot, "log", "-n", "8", "-z", "--no-color", "--no-show-signature",
		"--format=%h%x00%cs%x00%s", "origin/master", "--", ":(top,literal)spec/support/*.rb")
	if cmd.Env["GIT_OPTIONAL_LOCKS"] != "0" {
		t.Errorf("env=%v", cmd.Env)
	}

	c, _ = newFake(outRule("", "git"))
	if got, err := c.FileLog(ctx, slot, sha1, "app/new.rb", 8); err != nil || len(got) != 0 {
		t.Fatalf("a file without commits = %+v, %v", got, err)
	}
	c, _ = newFake(outRule("abc1234\x002026-09-30\x00", "git"))
	if _, err := c.FileLog(ctx, slot, sha1, "a.rb", 8); err == nil {
		t.Error("a record without its subject must be an error")
	}
	for _, bad := range []struct {
		rev, path string
		n         int
	}{{"--all", "a.rb", 8}, {"a..b", "a.rb", 8}, {sha1, "", 8}, {sha1, "a.rb", 0}} {
		if _, err := c.FileLog(ctx, slot, bad.rev, bad.path, bad.n); err == nil {
			t.Errorf("FileLog(%q, %q, %d) must be refused", bad.rev, bad.path, bad.n)
		}
	}
}

func TestBranchPR(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule("11483\n", "git", "-C", slot, "config"))
	n, ok, err := c.BranchPR(ctx, slot, "bohdan/PR-26788-campaign-snapshots")
	if err != nil || !ok || n != 11483 {
		t.Fatalf("BranchPR = %d %v %v", n, ok, err)
	}
	wantCall(t, f, 0, false, "git", "-C", slot, "config", "--get", "branch.bohdan/PR-26788-campaign-snapshots.pr")

	c, _ = newFake(exitRule(1, "", "git", "-C", slot, "config"))
	if n, ok, err := c.BranchPR(ctx, slot, "main"); err != nil || ok || n != 0 {
		t.Fatalf("unset key: %d %v %v", n, ok, err)
	}

	c, _ = newFake(outRule("https://github.com/talkable/talkable/pull/777\n", "git", "-C", slot, "config"))
	if n, ok, err := c.BranchPR(ctx, slot, "x"); err != nil || !ok || n != 777 {
		t.Fatalf("URL value: %d %v %v", n, ok, err)
	}

	c, _ = newFake(outRule("garbage\n", "git", "-C", slot, "config"))
	if _, ok, err := c.BranchPR(ctx, slot, "x"); err == nil || ok {
		t.Fatalf("unparsable value must error: ok=%v err=%v", ok, err)
	}

	c, _ = newFake(exitRule(128, "fatal: bad config", "git", "-C", slot, "config"))
	if _, _, err := c.BranchPR(ctx, slot, "x"); err == nil {
		t.Fatal("real failure must propagate")
	}
	if _, _, err := c.BranchPR(ctx, slot, ""); err == nil {
		t.Error("empty branch must be rejected")
	}
}

func TestUnpushed(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule("3\n", "git", "-C", slot, "rev-list"))
	n, err := c.Unpushed(ctx, slot)
	if err != nil || n != 3 {
		t.Fatalf("Unpushed = %d %v", n, err)
	}
	wantCall(t, f, 0, false, "git", "-C", slot, "rev-list", "--count", "HEAD", "--not", "--remotes", "--glob=refs/magnum/*")
	c, _ = newFake(outRule("x\n", "git"))
	if _, err := c.Unpushed(ctx, slot); err == nil {
		t.Error("non-numeric count must error")
	}
}

func TestUnpushedRefArgv(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule("2\n", "git", "-C", slot, "rev-list"))
	n, err := c.UnpushedRef(ctx, slot, "review1")
	if err != nil || n != 2 {
		t.Fatalf("UnpushedRef = %d %v", n, err)
	}
	wantCall(t, f, 0, false, "git", "-C", slot, "rev-list", "--count", "review1", "--not", "--remotes", "--glob=refs/magnum/*")
	for _, bad := range []string{"", "--all", "a b"} {
		if _, err := c.UnpushedRef(ctx, slot, bad); err == nil {
			t.Errorf("ref %q must be rejected", bad)
		}
	}
	if len(f.Calls) != 1 {
		t.Errorf("rejected refs must not reach git: %d calls", len(f.Calls))
	}
	c, _ = newFake(outRule("-1\n", "git"))
	if _, err := c.UnpushedRef(ctx, slot, "x"); err == nil {
		t.Error("negative count must error")
	}
}

func TestRemoteURLAndClone(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(outRule("git@github.com:talkable/talkable.git\n", "git", "-C", clone, "remote"))
	got, err := c.RemoteURL(ctx, clone)
	if err != nil || got != "git@github.com:talkable/talkable.git" {
		t.Fatalf("RemoteURL = %q %v", got, err)
	}
	wantCall(t, f, 0, false, "git", "-C", clone, "remote", "get-url", "origin")

	c, _ = newFake(outRule("https://ghp_secretsecret123@github.com/o/r.git\n", "git", "-C", clone, "remote"))
	if got, err := c.RemoteURL(ctx, clone); err != nil || got != "https://github.com/o/r.git" {
		t.Fatalf("RemoteURL must strip credentials, got %q %v", got, err)
	}

	c, f = newFake(okRule("git"))
	if err := c.Clone(ctx, "https://github.com/zhuravel/app.git", "/repos/app"); err != nil {
		t.Fatal(err)
	}
	cmd := wantCall(t, f, 0, true, "git", "clone", "--quiet", "--", "https://github.com/zhuravel/app.git", "/repos/app")
	if cmd.Timeout < 10*time.Minute {
		t.Errorf("clone timeout %s too small", cmd.Timeout)
	}
	err = c.Clone(ctx, "https://ghp_secretsecret123@github.com/o/r.git", "/repos/r")
	if err == nil || strings.Contains(err.Error(), "ghp_") {
		t.Fatalf("credentialed URL must be refused without echoing the secret: %v", err)
	}
	if err := c.Clone(ctx, "https://github.com/o/r.git", "rel/dest"); err == nil {
		t.Error("relative dest must be rejected")
	}
	if len(f.Calls) != 1 {
		t.Errorf("rejected clones must not reach git: %d calls", len(f.Calls))
	}
}

func TestStripUserinfo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://ghp_secretsecret123@github.com/o/r.git", "https://github.com/o/r.git"},
		{"https://user:pw@host:8443/p?q=1#f", "https://host:8443/p?q=1#f"},
		{"HTTPS://tok@github.com/o/r", "HTTPS://github.com/o/r"},
		{"http://user:pw@example.com", "http://example.com"},
		{"ssh://user:secret@host/path", "ssh://user@host/path"},
		{"ssh://user:se@cret@host/x", "ssh://user@host/x"},
		{"git://user:pw@host/p", "git://user@host/p"},
		// url.Parse rejects the stray percent sign; the secret must go anyway.
		{"https://user:pa%ss@host/x", "https://host/x"},
		{"ssh://user:pa%ss@host/x", "ssh://user@host/x"},
		{"https://a@b@host/p", "https://host/p"},
		// Nothing secret: returned untouched.
		{"https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"ssh://git@github.com/o/r.git", "ssh://git@github.com/o/r.git"},
		{"git@github.com:o/r.git", "git@github.com:o/r.git"},
		{"https://github.com/o/r@v1", "https://github.com/o/r@v1"},
		{"/local/path@v1/repo", "/local/path@v1/repo"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripUserinfo(tc.in); got != tc.want {
			t.Errorf("stripUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	c, _ := newFake(outRule("ssh://user:secret@host/path\n", "git", "-C", clone, "remote"))
	if got, err := c.RemoteURL(context.Background(), clone); err != nil || strings.Contains(got, "secret") {
		t.Fatalf("RemoteURL must not return an ssh password: %q, %v", got, err)
	}
	// Clone refuses a password in any scheme but still takes a bare ssh user.
	c, f := newFake(okRule("git"))
	for _, bad := range []string{"ssh://user:secret@host/path", "https://tok@github.com/o/r.git", "https://u:p%ss@host/x"} {
		if err := c.Clone(context.Background(), bad, "/repos/x"); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("Clone(%q) must be refused without echoing the secret: %v", bad, err)
		}
	}
	if err := c.Clone(context.Background(), "ssh://git@github.com/o/r.git", "/repos/x"); err != nil {
		t.Errorf("a plain ssh user is not a credential: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Errorf("%d clones reached git, want 1", len(f.Calls))
	}
}

func TestParseRemote(t *testing.T) {
	cases := []struct {
		in    string
		want  Remote
		valid bool
	}{
		{"https://github.com/talkable/talkable.git", Remote{"github.com", "talkable", "talkable"}, true},
		{"https://github.com/talkable/talkable", Remote{"github.com", "talkable", "talkable"}, true},
		{"https://github.com/talkable/talkable/", Remote{"github.com", "talkable", "talkable"}, true},
		{"https://user:tok@github.com/o/r.git", Remote{"github.com", "o", "r"}, true},
		{"git@github.com:zhuravel/app.git", Remote{"github.com", "zhuravel", "app"}, true},
		{"ssh://git@github.com/o/r.git", Remote{"github.com", "o", "r"}, true},
		{"git://github.com/o/r.git", Remote{"github.com", "o", "r"}, true},
		{"HTTPS://GitHub.com/o/R.git", Remote{"github.com", "o", "R"}, true},
		{"/local/path/repo", Remote{}, false},
		{"https://github.com/onlyowner", Remote{}, false},
		{"", Remote{}, false},
	}
	for _, tc := range cases {
		got, ok := ParseRemote(tc.in)
		if ok != tc.valid || got != tc.want {
			t.Errorf("ParseRemote(%q) = %+v %v, want %+v %v", tc.in, got, ok, tc.want, tc.valid)
		}
	}
	if (Remote{Owner: "o", Repo: "r"}).FullName() != "o/r" {
		t.Error("FullName")
	}
}

func TestPRRef(t *testing.T) {
	if PRRef(7) != "refs/magnum/pr/7" {
		t.Fatal(PRRef(7))
	}
}

// op is one Client operation, for the tests that must cover every git call.
type op struct {
	name string
	// mutates: the operation plans at least one mutating command under --dry-run.
	mutates bool
	// reads: it also runs at least one read-only command for real.
	reads bool
	do    func(c *Client) error
}

// allOps lists every Client operation that runs git. Fake answers "0\n" to
// everything, so only the commands issued matter, not the results.
func allOps(ctx context.Context) []op {
	wt := "/repos/talkable__worktrees/pr-1"
	return []op{
		{"FetchPR", true, true, func(c *Client) error { _, err := c.FetchPR(ctx, clone, 1); return err }},
		{"FetchBranch", true, false, func(c *Client) error { return c.FetchBranch(ctx, clone, "master") }},
		{"SwitchDetach", true, false, func(c *Client) error { return c.SwitchDetach(ctx, slot, "HEAD") }},
		{"ResetPlaceholder", true, true, func(c *Client) error { return c.ResetPlaceholder(ctx, slot, "review1", "master") }},
		{"BranchDelete", true, false, func(c *Client) error { return c.BranchDelete(ctx, clone, "review1", true) }},
		{"WorktreeAdd", true, false, func(c *Client) error { return c.WorktreeAdd(ctx, clone, wt, "HEAD", true, "") }},
		{"WorktreeRemove", true, false, func(c *Client) error { return c.WorktreeRemove(ctx, clone, wt, true) }},
		{"WorktreePrune", true, false, func(c *Client) error { return c.WorktreePrune(ctx, clone) }},
		{"UpdateRefDelete", true, true, func(c *Client) error { return c.UpdateRefDelete(ctx, clone, PRRef(1)) }},
		{"Clone", true, false, func(c *Client) error { return c.Clone(ctx, "https://github.com/o/r.git", "/repos/r") }},
		{"RevParse", false, true, func(c *Client) error { _, err := c.RevParse(ctx, slot, "HEAD"); return err }},
		{"WorktreeList", false, true, func(c *Client) error { _, err := c.WorktreeList(ctx, clone); return err }},
		{"Status", false, true, func(c *Client) error { _, err := c.Status(ctx, slot); return err }},
		{"StatusPaths", false, true, func(c *Client) error { _, err := c.StatusPaths(ctx, slot); return err }},
		{"MergeBase", false, true, func(c *Client) error { _, err := c.MergeBase(ctx, slot, "a", "b"); return err }},
		{"ChangedPaths", false, true, func(c *Client) error { _, err := c.ChangedPaths(ctx, slot, "a", "b"); return err }},
		{"ChangedUnder", false, true, func(c *Client) error { _, err := c.ChangedUnder(ctx, slot, "a", "b", ".claude"); return err }},
		{"BranchPR", false, true, func(c *Client) error { _, _, err := c.BranchPR(ctx, slot, "b"); return err }},
		{"Unpushed", false, true, func(c *Client) error { _, err := c.Unpushed(ctx, slot); return err }},
		{"UnpushedRef", false, true, func(c *Client) error { _, err := c.UnpushedRef(ctx, slot, "review1"); return err }},
		{"RemoteURL", false, true, func(c *Client) error { _, err := c.RemoteURL(ctx, clone); return err }},
	}
}

// Every operation must declare whether it mutates, so --dry-run is safe: read
// operations reach the inner runner, mutating ones are only planned. FetchPR,
// ResetPlaceholder and UpdateRefDelete are both: the fetch, unset-upstream or
// delete is planned, while the rev-parse, upstream check or symbolic-ref look
// at the real repository.
func TestDryRunClassification(t *testing.T) {
	for _, o := range allOps(context.Background()) {
		t.Run(o.name, func(t *testing.T) {
			inner := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}, Fn: func(c execx.Cmd) (execx.Result, error) {
				if slices.Contains(c.Args, "symbolic-ref") {
					return execx.Result{}, &execx.ExitError{Cmd: c, Code: 1} // a plain ref
				}
				return execx.Result{Stdout: []byte("0\n")}, nil
			}}}}
			dry := &execx.DryRun{Inner: inner}
			_ = o.do(New(dry)) // output shapes do not matter here, only routing
			if got := len(dry.Planned) > 0; got != o.mutates {
				t.Fatalf("planned %d mutating commands, want mutating=%v", len(dry.Planned), o.mutates)
			}
			if got := len(inner.Calls) > 0; got != o.reads {
				t.Fatalf("%d commands reached the real runner, want reads=%v", len(inner.Calls), o.reads)
			}
		})
	}
}

// The commands of FetchPR in a dry run, in order: the fetch is planned, the
// rev-parse that follows is a real read.
func TestDryRunFetchPRSequence(t *testing.T) {
	inner := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}, Fn: func(execx.Cmd) (execx.Result, error) {
		return execx.Result{Stdout: []byte(sha1 + "\n")}, nil
	}}}}
	dry := &execx.DryRun{Inner: inner}
	got, err := New(dry).FetchPR(context.Background(), clone, 5)
	if err != nil || got != sha1 {
		t.Fatalf("FetchPR = %q, %v (the stale ref is what a dry run reports)", got, err)
	}
	if len(dry.Planned) != 1 || dry.Planned[0].Args[2] != "fetch" {
		t.Fatalf("planned = %v, want the fetch only", dry.Planned)
	}
	if len(inner.Calls) != 1 || inner.Calls[0].Args[2] != "rev-parse" {
		t.Fatalf("real calls = %v, want the rev-parse only", inner.Calls)
	}
}

// Every git command gitx runs drops the variables that redirect git, so an
// exported GIT_DIR cannot point `git -C <slot>` at another repository.
func TestEveryCallUnsetsRedirectingEnv(t *testing.T) {
	want := []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR", "GIT_NAMESPACE"}
	if got := ScrubbedEnv(); !slices.Equal(got, want) {
		t.Fatalf("ScrubbedEnv = %v, want %v", got, want)
	}
	ScrubbedEnv()[0] = "MUTATED" // a caller's copy must not corrupt the list
	if got := ScrubbedEnv(); !slices.Equal(got, want) {
		t.Fatalf("ScrubbedEnv shares its backing array: %v", got)
	}
	for _, o := range allOps(context.Background()) {
		t.Run(o.name, func(t *testing.T) {
			f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}, Result: execx.Result{Stdout: []byte("0\n")}}}}
			_ = o.do(New(f))
			if len(f.Calls) == 0 {
				t.Fatal("no git command was run")
			}
			for _, call := range f.Calls {
				if !slices.Equal(call.Unset, want) {
					t.Errorf("%s: Unset = %v, want %v", call.String(), call.Unset, want)
				}
			}
		})
	}
	// FindClone reaches git through RemoteURL.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "r", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}, Result: execx.Result{Stdout: []byte("git@github.com:o/r.git\n")}}}}
	if _, err := New(f).FindClone(context.Background(), root, "o", "r"); err != nil || len(f.Calls) == 0 {
		t.Fatalf("FindClone = %v, %d calls", err, len(f.Calls))
	}
	for _, call := range f.Calls {
		if !slices.Equal(call.Unset, want) {
			t.Errorf("FindClone: %s: Unset = %v", call.String(), call.Unset)
		}
	}
}

func TestFetchRetriesLostRefLockOnce(t *testing.T) {
	old := lockRetryDelay
	lockRetryDelay = time.Millisecond
	t.Cleanup(func() { lockRetryDelay = old })
	ctx := context.Background()
	lost := "error: cannot lock ref 'refs/magnum/pr/7': is at " + sha2 + " but expected " + sha1

	// Another process held the lock once: the retry succeeds.
	var n int
	c, f := newFake(
		execx.Rule{Prefix: []string{"git", "-C", clone, "fetch"}, Fn: func(cmd execx.Cmd) (execx.Result, error) {
			if n++; n == 1 {
				return execx.Result{Code: 1, Stderr: []byte(lost)}, &execx.ExitError{Cmd: cmd, Code: 1, Stderr: lost}
			}
			return execx.Result{}, nil
		}},
		outRule(sha1+"\n", "git", "-C", clone, "rev-parse"),
	)
	if got, err := c.FetchPR(ctx, clone, 7); err != nil || got != sha1 {
		t.Fatalf("FetchPR = %q, %v", got, err)
	}
	if got := len(f.CallsWithPrefix("git", "-C", clone, "fetch")); got != 2 {
		t.Fatalf("fetch ran %d times, want 2", got)
	}

	// Still locked: the second failure is returned, no third attempt.
	c, f = newFake(exitRule(1, lost, "git", "-C", clone, "fetch"))
	err := c.FetchBranch(ctx, clone, "master")
	var ee *execx.ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Stderr, "cannot lock ref") {
		t.Fatalf("want the lock failure, got %v", err)
	}
	if got := len(f.Calls); got != 2 {
		t.Fatalf("fetch ran %d times, want 2", got)
	}

	// Any other failure is not retried.
	c, f = newFake(exitRule(128, "fatal: couldn't find remote ref refs/pull/9/head", "git", "-C", clone, "fetch"))
	if _, err := c.FetchPR(ctx, clone, 9); err == nil {
		t.Fatal("want an error")
	}
	if got := len(f.Calls); got != 1 {
		t.Fatalf("fetch ran %d times, want 1", got)
	}

	// A cancelled context ends the wait for the retry.
	lockRetryDelay = time.Hour
	cctx, cancel := context.WithCancel(ctx)
	c, _ = newFake(execx.Rule{Prefix: []string{"git"}, Fn: func(cmd execx.Cmd) (execx.Result, error) {
		cancel()
		return execx.Result{Code: 1, Stderr: []byte(lost)}, &execx.ExitError{Cmd: cmd, Code: 1, Stderr: lost}
	}})
	if err := c.FetchBranch(cctx, clone, "master"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestErrorsWrapContext(t *testing.T) {
	c, _ := newFake(exitRule(128, "fatal: boom", "git"))
	err := c.SwitchDetach(context.Background(), slot, "refs/magnum/pr/1")
	var ee *execx.ExitError
	if !errors.As(err, &ee) || !strings.Contains(err.Error(), "gitx") || !strings.Contains(err.Error(), "refs/magnum/pr/1") {
		t.Fatalf("error must wrap ExitError with context, got %v", err)
	}
	if fmt.Sprint(err) == ee.Error() {
		t.Error("error should add context beyond the raw exit error")
	}
}

// With HTTPSFetch, a clone whose origin is a github.com SSH URL is fetched
// over HTTPS with gh as the only credential helper; other origins are not.
func TestHTTPSFetchForSSHOrigins(t *testing.T) {
	for _, tc := range []struct {
		origin string
		want   []string
	}{
		{"git@github.com:talkable/talkable.git", []string{"-C", "/repos/talkable", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
			"fetch", "--no-tags", "https://github.com/talkable/talkable.git", "+refs/pull/42/head:refs/magnum/pr/42"}},
		{"https://github.com/talkable/talkable.git", []string{"-C", "/repos/talkable", "fetch", "--no-tags", "origin", "+refs/pull/42/head:refs/magnum/pr/42"}},
	} {
		run := &execx.Fake{Rules: []execx.Rule{
			{Prefix: []string{"git", "-C", "/repos/talkable", "remote", "get-url"}, Result: execx.Result{Stdout: []byte(tc.origin + "\n")}},
			{Prefix: []string{"git", "-C", "/repos/talkable", "rev-parse"}, Result: execx.Result{Stdout: []byte("0123456789abcdef0123456789abcdef01234567\n")}},
			{Prefix: []string{"git", "-C", "/repos/talkable", "-c"}},
			{Prefix: []string{"git", "-C", "/repos/talkable", "fetch"}},
		}}
		c := New(run)
		c.HTTPSFetch = true
		if _, err := c.FetchPR(context.Background(), "/repos/talkable", 42); err != nil {
			t.Fatal(err)
		}
		var fetch []string
		for _, call := range run.Calls {
			if slices.Contains(call.Args, "fetch") {
				fetch = call.Args
			}
		}
		if !slices.Equal(fetch, tc.want) {
			t.Fatalf("origin %s: fetch argv %q, want %q", tc.origin, fetch, tc.want)
		}
	}
}

// TestLsRemoteReachesOriginLikeFetch: the reachability probe (doctor, the
// infrastructure-pause probe) goes where fetches go, so a github.com SSH
// origin is probed over HTTPS through gh and a locked ssh-agent cannot fail
// it; other origins, and a client without HTTPSFetch, name origin.
func TestLsRemoteReachesOriginLikeFetch(t *testing.T) {
	const dir = "/repos/talkable"
	for _, tc := range []struct {
		origin string
		https  bool
		want   []string
	}{
		{"git@github.com:talkable/talkable.git", true, []string{"-C", dir, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
			"ls-remote", "https://github.com/talkable/talkable.git", "HEAD"}},
		{"ssh://git@github.com/talkable/talkable.git", true, []string{"-C", dir, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
			"ls-remote", "https://github.com/talkable/talkable.git", "HEAD"}},
		{"git@git.example.com:talkable/talkable.git", true, []string{"-C", dir, "ls-remote", "origin", "HEAD"}},
		{"https://github.com/talkable/talkable.git", true, []string{"-C", dir, "ls-remote", "origin", "HEAD"}},
		{"git@github.com:talkable/talkable.git", false, []string{"-C", dir, "ls-remote", "origin", "HEAD"}},
	} {
		run := &execx.Fake{Rules: []execx.Rule{
			{Prefix: []string{"git", "-C", dir, "remote", "get-url"}, Result: execx.Result{Stdout: []byte(tc.origin + "\n")}},
			{Prefix: []string{"git", "-C", dir}, Result: execx.Result{Stdout: []byte("0123456789abcdef0123456789abcdef01234567\tHEAD\n")}},
		}}
		c := New(run)
		c.HTTPSFetch = tc.https
		out, err := c.LsRemote(context.Background(), dir, time.Second, "HEAD")
		if err != nil || !strings.Contains(out, "HEAD") {
			t.Fatalf("origin %s: %q, %v", tc.origin, out, err)
		}
		last := run.Calls[len(run.Calls)-1]
		if !slices.Equal(last.Args, tc.want) || !last.Probe {
			t.Errorf("origin %s https=%v: argv %q probe=%v, want %q", tc.origin, tc.https, last.Args, last.Probe, tc.want)
		}
	}
}

// TestFetchCommitKeepsOffTheDaemonsRefs: magnum eval makes a pinned commit
// local without writing refs/magnum/pr/*: nothing is fetched when the commit
// is there, the id is tried first, and PR's head is the fallback.
func TestFetchCommitKeepsOffTheDaemonsRefs(t *testing.T) {
	const dir = "/repos/talkable"
	sha := strings.Repeat("ab", 20)
	for _, tc := range []struct {
		name     string
		present  bool
		byIDOK   bool
		afterPR  bool
		wantErr  bool
		wantRefs []string
	}{
		{"present", true, false, false, false, nil},
		{"by id", false, true, false, false, []string{"+" + sha + ":refs/magnum/eval/" + sha}},
		{"via PR head", false, false, true, false, []string{"+" + sha + ":refs/magnum/eval/" + sha, "+refs/pull/42/head:refs/magnum/eval/pr-42"}},
		{"gone", false, false, false, true, []string{"+" + sha + ":refs/magnum/eval/" + sha, "+refs/pull/42/head:refs/magnum/eval/pr-42"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revParses := 0
			run := &execx.Fake{Rules: []execx.Rule{
				{Prefix: []string{"git", "-C", dir, "rev-parse"}, Fn: func(execx.Cmd) (execx.Result, error) {
					revParses++
					if tc.present || (tc.afterPR && revParses > 1) {
						return execx.Result{Stdout: []byte(sha + "\n")}, nil
					}
					return execx.Result{Code: 1}, &execx.ExitError{Code: 1}
				}},
				{Prefix: []string{"git", "-C", dir, "fetch", "--no-tags", "origin", "+" + sha + ":refs/magnum/eval/" + sha}, Fn: func(c execx.Cmd) (execx.Result, error) {
					if tc.byIDOK {
						return execx.Result{}, nil
					}
					return execx.Result{Code: 128}, &execx.ExitError{Cmd: c, Code: 128, Stderr: "fatal: remote error: upload-pack: not our ref"}
				}},
				{Prefix: []string{"git", "-C", dir, "fetch"}},
			}}
			err := New(run).FetchCommit(context.Background(), dir, sha, 42)
			if (err != nil) != tc.wantErr {
				t.Fatalf("FetchCommit: %v", err)
			}
			var refs []string
			for _, c := range run.Calls {
				if slices.Contains(c.Args, "fetch") {
					refs = append(refs, c.Args[len(c.Args)-1])
				}
				for _, a := range c.Args {
					if strings.Contains(a, "refs/magnum/pr/") {
						t.Fatalf("touched the daemon's refs: %q", c.Args)
					}
				}
			}
			if !slices.Equal(refs, tc.wantRefs) {
				t.Fatalf("fetched %q, want %q", refs, tc.wantRefs)
			}
		})
	}
}

// A base branch name reaches the judge's git commands unquoted when the
// merge base is unknown: only letters, digits, '.', '_', '/' and '-' pass,
// and never ".." or a leading '-', whatever git itself allows.
func TestShellSafeRefPassesOnlyPlainBranchNames(t *testing.T) {
	for _, ref := range []string{"master", "main", "origin/master", "release/2026.10", "feature/a_b-c", "v1.2.3", "refs/heads/x"} {
		if !ShellSafeRef(ref) {
			t.Errorf("ShellSafeRef(%q) = false, want true", ref)
		}
	}
	for _, ref := range []string{"", "main$(x)", "a|b", "a&b", "a;b", "(x)", "a`id`", "a b", "it's", "a\"b", "a\nb",
		"a..b", "-x", "--upload-pack=x", "a>b", "a<b", "a*b", "maïn", "a\\b", "a{b}", "a@{1}"} {
		if ShellSafeRef(ref) {
			t.Errorf("ShellSafeRef(%q) = true, want false", ref)
		}
	}
}
