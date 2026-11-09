package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// bigText is a file of 2000 lines, the same for every word but its line
// 1000: a pack deltifies one version of it against another.
func bigText(word string) string {
	var b strings.Builder
	for i := 1; i <= 2000; i++ {
		if i == 1000 {
			b.WriteString(word + "\n")
			continue
		}
		fmt.Fprintf(&b, "line %d of a file a pack deltifies\n", i)
	}
	return b.String()
}

// deletedConflict makes PR 3, which conflicts with staging-void where
// merge-tree reads no blob: staging-void gains app/big.rb and app/wide.rb,
// PR 3 (made on that tip) changes both, then staging-void deletes both
// (modify/delete). The treeless mirror never holds a version of either, so
// the thin pack a fetch of a resolved merge gets from a full clone holds
// deltas whose bases the mirror lacks. It returns staging-void's tip.
func (w *deployWorld) deletedConflict() string {
	w.t.Helper()
	on := w.coworker(map[string]string{"app/big.rb": bigText("base"), "app/wide.rb": bigText("wide base")})
	w.prOn(3, "pr3", on, map[string]string{"app/big.rb": bigText("theirs"), "app/wide.rb": bigText("wide theirs")})
	return w.coworker(nil, "app/big.rb", "app/wide.rb")
}

// resolvedWorld is a deploy world whose deploy of PR 3, conflicting as
// deletedConflict makes it, waits for the resolver's job.
type resolvedWorld struct {
	*deployWorld
	res *fakeResolver
	c   *Client
	id  EffectID
	tip string // staging-void's tip the deploy merges on
}

func newResolvedWorld(t *testing.T) *resolvedWorld {
	t.Helper()
	w := newDeployWorld(t)
	tip := w.deletedConflict()
	res := w.withResolver()
	c := w.client()
	t.Cleanup(c.Close)
	id := w.deploy(c, 3)
	if e := w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 || ended(e) }); ended(e) {
		t.Fatalf("before the resolver's job: %+v", e)
	}
	return &resolvedWorld{deployWorld: w, res: res, c: c, id: id, tip: tip}
}

// resolve answers the job with the merge [tip, head] that edit's
// resolution makes in a full clone, and returns the clone and the merge.
func (w *resolvedWorld) resolve(edit func(dir string)) (string, string) {
	w.t.Helper()
	dir, merge := w.resolverRepoWith(3, []string{w.tip, w.heads[3]}, edit)
	w.res.answer(ResolveResult{State: ResolveDone, MergeSHA: merge, Dir: dir})
	return dir, merge
}

// filesIn is every entry under dir with its size, mode and time: a write
// there changes one, and a new file changes its directory's.
func filesIn(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out[p] = fmt.Sprintf("%d %s %d", fi.Size(), fi.Mode(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// onlyReadIn fails the test unless every command run in the resolver's
// git dir src, or on its work tree dir (checkResolverDir tries both), only
// read it (cat-file, or checkResolverDir's rev-parse, with no hook, no lazy
// fetch and no optional lock), and no other command named dir (a fetch
// from it).
func (w *deployWorld) onlyReadIn(src, dir string) {
	w.t.Helper()
	reads := 0
	for _, c := range w.run.started() {
		switch sub := subcommand(c.Args); {
		case slices.Contains(c.Args, "--git-dir="+src) || slices.Contains(c.Args, "--git-dir="+dir):
			reads++
			if sub != "cat-file" && sub != "rev-parse" || c.Mutates || c.Env["GIT_NO_LAZY_FETCH"] != "1" ||
				c.Env["GIT_OPTIONAL_LOCKS"] != "0" || !slices.Contains(c.Args, "core.hooksPath=/dev/null") {
				w.t.Errorf("a command in the resolver's repository that may write it: %q (env %v)", c.Args, c.Env)
			}
		case slices.ContainsFunc(c.Args, func(a string) bool { return strings.Contains(a, dir) }):
			w.t.Errorf("a command names the resolver's repository: %q", c.Args)
		}
	}
	if reads == 0 {
		w.t.Error("no command read the resolver's repository")
	}
}

// A resolved merge enters the treeless mirror without a fetch. PR 3's
// conflicts are modify/delete (deletedConflict), so the mirror holds no
// version of either file, and a fetch from the resolver's full clone got a
// thin pack the mirror could not index (the first real resolver run failed
// so: "could not fetch <blob> from promisor remote" there, "unresolved
// deltas" in this world). The hand-off writes the resolver's blobs and its
// commit into the mirror instead: the deploy pushes the resolver's merge,
// the same sha, and the resolver's repository is only read.
func TestAResolvedMergeEntersTheTreelessMirrorWithoutAFetch(t *testing.T) {
	t.Parallel()
	w := newResolvedWorld(t)
	g := w.deps.Git.(deployGit)
	for _, p := range []string{"app/big.rb", "app/wide.rb"} {
		blob := gitIn(t, w.r, w.origin, "rev-parse", w.heads[3]+":"+p)
		if typ, err := g.objectType(context.Background(), w.mirror, blob); err != nil || typ != "" {
			t.Fatalf("the mirror holds the head's %s (%q, %v): the world does not reproduce the bug", p, typ, err)
		}
	}
	dir, merge := w.resolve(func(dir string) {
		writeIn(t, dir, "app/big.rb", bigText("theirs, resolved"))
		writeIn(t, dir, "app/wide.rb", bigText("wide theirs, resolved"))
	})
	src := filepath.Join(dir, ".git")
	before := filesIn(t, src)
	e := w.finish(w.c, w.id)
	if e.State != EffectDone || e.Merge != merge {
		t.Fatalf("finished: %+v, want done with the resolver's merge %s", e, short(merge))
	}
	if got := w.originTip(); got != merge {
		t.Errorf("origin's staging-void is %s, want the resolver's merge %s", short(got), short(merge))
	}
	if got := w.run.pushed(); got != 1 {
		t.Errorf("pushes: %d, want 1", got)
	}
	if after := filesIn(t, src); !maps.Equal(before, after) {
		t.Error("the resolver's repository changed during the hand-off")
	}
	w.onlyReadIn(src, dir)
}

// A resolver's merge that also changes a path no conflict names (app/a.rb)
// is refused at its hand-off: merge-tree's tree with the resolver's
// conflicted paths put in is not its tree. The deploy fails at merge,
// saying so, the merge commit never enters the mirror, and nothing is
// pushed.
func TestAResolvedMergeThatChangesAnUnconflictedPathIsRefused(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	res := w.withResolver()
	c := w.client()
	defer c.Close()
	id := w.deploy(c, 2)
	w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 || ended(e) })
	dir, merge := w.resolverRepo(2, []string{w.tip, w.heads[2]}, map[string]string{"app/s.rb": "resolved\n", "app/a.rb": "sneaked in\n"})
	res.answer(ResolveResult{State: ResolveDone, MergeSHA: merge, Dir: dir})
	e := w.drive(c, id, ended)
	if e.State != EffectFailed || e.Step != StepMerge || !strings.Contains(e.Why, "beyond the conflicted paths") ||
		!strings.Contains(e.Why, "nothing was pushed") {
		t.Errorf("ended: %+v, want failed at merge, a change beyond the conflicted paths, nothing pushed", e)
	}
	w.noPushSince(0, w.tip)
	if typ, err := w.deps.Git.(deployGit).objectType(context.Background(), w.mirror, merge); err != nil || typ != "" {
		t.Errorf("the refused merge is in the mirror: %q, %v", typ, err)
	}
	if got := gitIn(t, w.r, w.mirror, "for-each-ref", resolvedPrefix); got != "" {
		t.Errorf("resolved refs left in the mirror: %s", got)
	}
}

// The rebuilt tree is the resolver's whatever it made of a conflicted
// path: a path it removed is gone, an executable file and a symlink keep
// their modes. The deploy pushes the resolver's merge.
func TestTheRebuiltTreeTakesTheResolversRemovalsAndModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, dir string)
		want map[string]string // path: its mode and type in the merge, "" absent
	}{
		{"a removed path", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Remove(filepath.Join(dir, "app/big.rb")); err != nil {
				t.Fatal(err)
			}
			writeIn(t, dir, "app/wide.rb", bigText("wide theirs, resolved"))
		}, map[string]string{"app/big.rb": "", "app/wide.rb": "100644 blob"}},
		{"an executable and a symlink", func(t *testing.T, dir string) {
			t.Helper()
			big, wide := filepath.Join(dir, "app/big.rb"), filepath.Join(dir, "app/wide.rb")
			writeIn(t, dir, "app/big.rb", bigText("theirs, resolved"))
			if err := os.Chmod(big, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(wide); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("big.rb", wide); err != nil {
				t.Fatal(err)
			}
		}, map[string]string{"app/big.rb": "100755 blob", "app/wide.rb": "120000 blob"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newResolvedWorld(t)
			_, merge := w.resolve(func(dir string) { tc.edit(t, dir) })
			e := w.finish(w.c, w.id)
			if e.State != EffectDone || e.Merge != merge {
				t.Fatalf("finished: %+v, want done with the resolver's merge %s", e, short(merge))
			}
			for p, want := range tc.want {
				got := ""
				if f := strings.Fields(gitIn(t, w.r, w.origin, "ls-tree", merge, "--", p)); len(f) >= 2 {
					got = f[0] + " " + f[1]
				}
				if got != want {
					t.Errorf("%s in the pushed merge: %q, want %q", p, got, want)
				}
			}
		})
	}
}

// A restart between the blob writes and the ref: the hand-off stops there
// as a killed process would, its tree's index and the index's lock left
// as a kill in the tree's build leaves them. The next client's first Step
// hands the merge off again (no try fails on the lock left): it is held at
// its one ref, no index is left in the mirror, the merge step's ok is
// recorded once, and the deploy pushes the merge once.
func TestAHandOffCutAfterItsBlobsResumesAndLeavesNoIndex(t *testing.T) {
	t.Parallel()
	w := newResolvedWorld(t)
	index := resolvedIndex(w.mirror, w.id)
	hit := false
	w.c.hook = func(_ EffectID, step, point string) error {
		if step != StepMerge || point != "blobs written" || hit {
			return nil
		}
		hit = true
		for _, f := range []string{index, index + ".lock"} {
			if err := os.WriteFile(f, []byte("cut\n"), 0o644); err != nil {
				t.Error(err)
			}
		}
		return errHook
	}
	_, merge := w.resolve(func(dir string) {
		writeIn(t, dir, "app/big.rb", bigText("theirs, resolved"))
		writeIn(t, dir, "app/wide.rb", bigText("wide theirs, resolved"))
	})
	for i := 0; !hit && i < 5; i++ {
		if err := w.c.Step(context.Background()); err != nil && !errors.Is(err, errHook) {
			t.Fatalf("Step: %v", err)
		}
	}
	if !hit {
		t.Fatal("the hook never stopped the hand-off after its blobs")
	}
	kill(w.c)
	second := w.client()
	defer second.Close()
	if err := second.Step(context.Background()); err != nil {
		t.Fatalf("Step: %v", err)
	}
	e := w.effect(second, w.id)
	if e.State != EffectRunning || e.Step != StepVerify || e.Merge != merge {
		t.Fatalf("after one Step: %+v, want at verify with the resolver's merge %s", e, short(merge))
	}
	if got, want := gitIn(t, w.r, w.mirror, "for-each-ref", "--format=%(objectname) %(refname)", "refs/magnum/"), merge+" "+resolvedRef(w.id); got != want {
		t.Errorf("the effect's refs: %q, want %q", got, want)
	}
	if left, _ := filepath.Glob(filepath.Join(w.mirror, resolvedIndexPrefix+"*")); len(left) > 0 {
		t.Errorf("left in the mirror: %q", left)
	}
	e = w.finish(second, w.id)
	if e.State != EffectDone || e.Merge != merge {
		t.Fatalf("finished: %+v, want done with the resolver's merge", e)
	}
	if got := w.run.pushed(); got != 1 {
		t.Errorf("pushes: %d, want 1", got)
	}
	w.mu.Lock()
	oks := 0
	for _, ev := range w.events {
		if strings.HasPrefix(ev, StepMerge+" "+PhaseOK+" ") {
			oks++
		}
	}
	w.mu.Unlock()
	if oks != 1 {
		t.Errorf("merge ok events: %d, want 1", oks)
	}
}
