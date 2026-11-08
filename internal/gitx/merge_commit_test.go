package gitx

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// A parent merge without an agent: MergeTree gives the tree of a clean
// merge of the base's tip into the PR's head (`git merge-tree
// --write-tree`), or the conflicting paths and no tree, and CommitTree
// writes the merge commit of that tree with the PR's head as its first
// parent and the tip as its second, as who (author and committer alike,
// whatever the environment says), unsigned. Neither moves a ref, the
// checkout's HEAD, its work tree or its index.
func TestMergeTreeAndCommitTreeMakeTheParentMergeAndMoveNothing(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetchPR7()
	fx.commit(fx.origin, "docs/main.md", "main moves\n", "main moves elsewhere")
	if err := fx.c.FetchBranch(ctx, fx.clone, "main"); err != nil {
		t.Fatal(err)
	}
	tip := fx.git(fx.clone, "rev-parse", "refs/remotes/origin/main")
	head := fx.git(fx.clone, "rev-parse", "HEAD")

	tree, conflicts, err := fx.c.MergeTree(ctx, fx.clone, fx.pr, tip)
	if err != nil || conflicts != nil || !isOID(tree) {
		t.Fatalf("MergeTree of a clean merge = %q, %v, %v; want a tree and no conflict", tree, conflicts, err)
	}
	who := Signature{Name: "fixer-app[bot]", Email: "7+fixer-app[bot]@users.noreply.github.com"}
	merge, err := fx.c.CommitTree(ctx, fx.clone, tree, []string{fx.pr, tip}, "Merge main into pr7", who)
	if err != nil || !isOID(merge) {
		t.Fatalf("CommitTree = %q, %v", merge, err)
	}
	if got := fx.git(fx.clone, "log", "-1", "--format=%P%x00%T%x00%an%x00%ae%x00%cn%x00%ce%x00%s%x00%G?", merge); got !=
		strings.Join([]string{fx.pr + " " + tip, tree, who.Name, who.Email, who.Name, who.Email, "Merge main into pr7", "N"}, "\x00") {
		t.Errorf("the merge commit = %q", got)
	}
	if got := fx.git(fx.clone, "show", merge+":docs/main.md"); got != "main moves" {
		t.Errorf("the merge's docs/main.md = %q, want the base's", got)
	}

	fx.commit(fx.origin, "app/a.rb", "a = 2\n", "main adds a.rb too")
	if err := fx.c.FetchBranch(ctx, fx.clone, "main"); err != nil {
		t.Fatal(err)
	}
	tip = fx.git(fx.clone, "rev-parse", "refs/remotes/origin/main")
	if tree, conflicts, err := fx.c.MergeTree(ctx, fx.clone, fx.pr, tip); err != nil || tree != "" || !slices.Equal(conflicts, []string{"app/a.rb"}) {
		t.Errorf("MergeTree of a conflict = %q, %v, %v; want no tree and app/a.rb", tree, conflicts, err)
	}

	if got := fx.git(fx.clone, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s", got)
	}
	if got := fx.git(fx.clone, "status", "--porcelain"); got != "" {
		t.Errorf("the checkout after the merge: %q", got)
	}
	for _, bad := range [][]string{{"-x"}, {fx.pr, "--stdin"}, {}} {
		if _, err := fx.c.CommitTree(ctx, fx.clone, tree, bad, "m", who); err == nil {
			t.Errorf("CommitTree with parents %q was taken", bad)
		}
	}
	if _, err := fx.c.CommitTree(ctx, fx.clone, tree, []string{fx.pr}, "m", Signature{Name: "x", Email: ""}); err == nil {
		t.Error("CommitTree without an email was taken")
	}
	if _, _, err := fx.c.MergeTree(ctx, fx.clone, "-x", tip); err == nil {
		t.Error("an option as the head was taken")
	}
}

// envRunner records the environment of each git command it runs.
type envRunner struct {
	inner execx.Runner
	mu    sync.Mutex
	envs  map[string]map[string]string // by the command's first git subcommand after -C dir
}

func (r *envRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	r.mu.Lock()
	if r.envs == nil {
		r.envs = map[string]map[string]string{}
	}
	for _, a := range c.Args[2:] {
		if !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
			r.envs[a] = c.Env
			break
		}
	}
	r.mu.Unlock()
	return r.inner.Run(ctx, c)
}

// PushLeaseViaAs is PushLeaseVia with the caller's environment over the
// push (a daemon push as a fixer App: its GH_CONFIG_DIR, which gh, git's
// only credential helper, answers from), never over gitx's own keys.
func TestPushLeaseViaAsPushesWithTheCallersEnvironment(t *testing.T) {
	fx := newFixture(t)
	r := &envRunner{inner: fx.r}
	c := New(r)
	ctx := context.Background()
	fx.git(fx.clone, "switch", "--quiet", "--detach", fx.pr)
	head := fx.commit(fx.clone, "app/a.rb", "a = 2\n", "fix")
	env := map[string]string{"GH_CONFIG_DIR": "/state/gh/fixer-app", "GH_TOKEN": "", "GIT_TERMINAL_PROMPT": "1"}
	if err := c.PushLeaseViaAs(ctx, fx.clone, fx.origin, head, "pr7", fx.base, env); err == nil {
		t.Error("a stale lease pushed")
	}
	if err := c.PushLeaseViaAs(ctx, fx.clone, fx.origin, head, "pr7", fx.pr, env); err != nil {
		t.Fatalf("PushLeaseViaAs = %v", err)
	}
	if got := fx.git(fx.origin, "rev-parse", "refs/heads/pr7"); got != head {
		t.Errorf("origin's pr7 = %s, want %s", got, head)
	}
	got := r.envs["push"]
	if got["GH_CONFIG_DIR"] != "/state/gh/fixer-app" || got["GH_TOKEN"] != "" || got["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("the push's environment = %v; want the caller's GH_CONFIG_DIR and gitx's own GIT_TERMINAL_PROMPT", got)
	}
	if _, ok := got["GH_TOKEN"]; !ok {
		t.Error("the caller's blank GH_TOKEN was dropped")
	}
}
