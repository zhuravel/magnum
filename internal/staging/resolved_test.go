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

	"github.com/zhuravel/magnum/internal/execx"
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

// handOffCmds are the git subcommands the hand-off runs in the mirror,
// merge-tree's aside: every one runs with lazy fetching off.
var handOffCmds = []string{"read-tree", "update-index", "write-tree", "hash-object", "ls-tree", "update-ref"}

// noLazyFetchInMirror fails the test unless every hand-off command run in
// the mirror carries GIT_NO_LAZY_FETCH=1, and some ran.
func (w *deployWorld) noLazyFetchInMirror() {
	w.t.Helper()
	seen := map[string]bool{}
	for _, c := range w.run.started() {
		sub := subcommand(c.Args)
		if !slices.Contains(c.Args, "--git-dir="+w.mirror) || !slices.Contains(handOffCmds, sub) {
			continue
		}
		seen[sub] = true
		if c.Env["GIT_NO_LAZY_FETCH"] != "1" {
			w.t.Errorf("%s ran in the mirror with lazy fetching on: %q", sub, c.Args)
		}
	}
	for _, sub := range []string{"read-tree", "update-index", "write-tree", "hash-object"} {
		if !seen[sub] {
			w.t.Errorf("no %s ran in the mirror", sub)
		}
	}
}

// A resolved merge enters the treeless mirror without a fetch. PR 3's
// conflicts are modify/delete (deletedConflict), so the mirror holds no
// version of either file, and a fetch from the resolver's full clone got a
// thin pack the mirror could not index (the first real resolver run failed
// so: "could not fetch <blob> from promisor remote" there, "unresolved
// deltas" in this world). The hand-off writes the resolver's blobs and its
// commit into the mirror instead: the deploy pushes the resolver's merge,
// the same sha, the resolver's repository is only read, and no command of
// the hand-off in the mirror (merge-tree's aside) may fetch lazily.
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
	w.noLazyFetchInMirror()
}

// A resolver's merge that also changes a path no conflict names (app/a.rb)
// is refused at its hand-off: merge-tree's tree with the resolver's
// conflicted paths put in is not its tree. The deploy fails at merge,
// naming the path, the merge commit never enters the mirror, and nothing
// is pushed.
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
	if e.State != EffectFailed || e.Step != StepMerge || !strings.Contains(e.Why, "beyond the conflicted paths: app/a.rb") ||
		!strings.Contains(e.Why, "nothing was pushed") {
		t.Errorf("ended: %+v, want failed at merge, a change beyond the conflicted paths naming app/a.rb, nothing pushed", e)
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

// A resolver's merge whose parents are not [tip, head] is refused at its
// hand-off, before anything is written into the mirror, even when its tree
// is the right one: [tip, X], X a commit of the head's tree that only the
// resolver's repository holds, and [head, tip]. The deploy fails at merge,
// naming the parents; the commit never enters the mirror (whose gc and
// fetches would trip on a parent it lacks), and nothing is pushed.
func TestAResolvedMergeWithOtherParentsIsRefusedAtTheHandOff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		parents func(w *deployWorld, dir string) []string
	}{
		{"another second parent of the head's tree", func(w *deployWorld, dir string) []string {
			x := gitIn(w.t, w.r, dir, "commit-tree", w.heads[2]+"^{tree}", "-p", w.c1, "-m", "not the head")
			return []string{w.tip, x}
		}},
		{"the parents reversed", func(w *deployWorld, _ string) []string { return []string{w.heads[2], w.tip} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			res := w.withResolver()
			c := w.client()
			defer c.Close()
			id := w.deploy(c, 2)
			w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 || ended(e) })
			dir, right := w.resolverRepo(2, []string{w.tip, w.heads[2]}, map[string]string{"app/s.rb": "resolved\n"})
			args := []string{"commit-tree", right + "^{tree}", "-m", "Merge"}
			for _, p := range tc.parents(w, dir) {
				args = append(args, "-p", p)
			}
			merge := gitIn(t, w.r, dir, args...)
			res.answer(ResolveResult{State: ResolveDone, MergeSHA: merge, Dir: dir})
			e := w.drive(c, id, ended)
			if e.State != EffectFailed || e.Step != StepMerge || !strings.Contains(e.Why, "the resolver's merge's parents are") ||
				!strings.Contains(e.Why, "nothing was pushed") {
				t.Errorf("ended: %+v, want failed at merge, naming the parents, nothing pushed", e)
			}
			w.noPushSince(0, w.tip)
			if typ, err := w.deps.Git.(deployGit).objectType(context.Background(), w.mirror, merge); err != nil || typ != "" {
				t.Errorf("the refused merge is in the mirror: %q, %v", typ, err)
			}
		})
	}
}

// A hand-off killed in its tree's build leaves its index and the index's
// lock in the mirror; when the effect then ends without another hand-off
// (a cancel), both go with it.
func TestAnIndexAKilledHandOffLeftGoesWhenTheEffectEnds(t *testing.T) {
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
	w.resolve(func(dir string) {
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
	if err := second.Cancel(context.Background(), w.id); err != nil {
		t.Fatal(err)
	}
	if e := w.effect(second, w.id); e.State != EffectCancelled {
		t.Fatalf("after the cancel: %+v", e)
	}
	if left, _ := filepath.Glob(filepath.Join(w.mirror, resolvedIndexPrefix+"*")); len(left) > 0 {
		t.Errorf("left in the mirror after the effect ended: %q", left)
	}
}

// A git before 2.51, which lacks cat-file's %(objectmode), cannot read the
// resolver's entries: the deploy fails at once, saying it needs git 2.51
// or later, rather than trying again for 15 minutes; nothing is pushed.
// The version (`git version`, here a fake 2.50.1) decides before any read;
// when it cannot be read, cat-file's own failure does, in the words of
// git 2.42 to 2.44 and of git 2.45 to 2.50.
func TestAGitWithoutObjectmodeFailsTheHandOffAtOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, version, stderr string
	}{
		{"git 2.50 by its version", "git version 2.50.1", ""},
		{"git 2.44's words, the version unreadable", "", "fatal: unknown format element: objectmode\n"},
		{"git 2.50's words, the version unreadable", "", "fatal: bad cat-file format: %(objectmode)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newResolvedWorld(t)
			w.run.mu.Lock()
			if tc.version != "" {
				w.run.version = tc.version
			} else {
				w.run.fail = map[string]bool{"version": true}
				w.run.failArg, w.run.failStderr = "%(objectmode)", tc.stderr
			}
			w.run.mu.Unlock()
			w.resolve(func(dir string) {
				writeIn(t, dir, "app/big.rb", bigText("theirs, resolved"))
				writeIn(t, dir, "app/wide.rb", bigText("wide theirs, resolved"))
			})
			if err := w.c.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			e := w.effect(w.c, w.id)
			if e.State != EffectFailed || e.Step != StepMerge || !strings.Contains(e.Why, "git 2.51 or later") ||
				!strings.Contains(e.Why, "nothing was pushed") {
				t.Errorf("after one Step: %+v, want failed at merge, needing git 2.51 or later", e)
			}
			w.noPushSince(0, w.tip)
			if tc.version != "" && slices.ContainsFunc(w.run.started(), func(c execx.Cmd) bool { return slices.Contains(c.Args, "-Z") }) {
				t.Error("the batch check ran on a git its version says is too old")
			}
		})
	}
}

// The git version is read once per client (its Git), with the mirror's
// scrubbed env and a bound of its own, and parsed by its numbers: Apple's
// and Windows' suffixes, 2.51 and later pass, earlier ones do not, and a
// version that cannot be read is no answer.
func TestTheGitVersionIsReadOnceAndComparedByItsNumbers(t *testing.T) {
	t.Parallel()
	run := &countingRunner{inner: hermetic(t)}
	g := newGit(run, func(string) string { return "file:///origin" }).(*execGit)
	mirror := filepath.Join(t.TempDir(), "acme", "app.git")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatal(err)
	}
	first := g.gitVersion(context.Background(), mirror)
	if !strings.HasPrefix(first, "git version 2.") || g.gitVersion(context.Background(), mirror) != first {
		t.Fatalf("version %q, then again", first)
	}
	var reads []execx.Cmd
	for _, c := range run.started() {
		if subcommand(c.Args) == "version" {
			reads = append(reads, c)
		}
	}
	if len(reads) != 1 || reads[0].Timeout == 0 || reads[0].Env["GIT_CONFIG_GLOBAL"] != "/dev/null" || !slices.Contains(reads[0].Unset, "GIT_DIR") {
		t.Errorf("version reads: %+v, want one, bounded, with the scrubbed env", reads)
	}
	for v, want := range map[string]int{
		"git version 2.51.0": 1, "git version 2.56.0": 1, "git version 3.0.0": 1, "git version 2.54.0 (Apple Git-154)": 1,
		"git version 2.50.1": -1, "git version 2.45.1.windows.1": -1, "git version 1.9.5": -1,
		"": 0, "git version": 0, "git version x.y": 0, "hub version 2.14.2": 0,
	} {
		if got := gitAtLeast(v, 2, 51); got != want {
			t.Errorf("gitAtLeast(%q, 2.51) = %d, want %d", v, got, want)
		}
	}
}

// The resolver's tree is listed from its root, whatever the repository's
// own config says of its work tree: a core.worktree above the git dir's
// directory makes ls-tree list a subdirectory without --full-tree.
func TestTheResolversTreeIsListedFromItsRootWhateverItsWorkTree(t *testing.T) {
	t.Parallel()
	r := hermetic(t)
	wt := t.TempDir()
	writeIn(t, wt, "top.rb", "top\n")
	writeIn(t, wt, "sub/in.rb", "in\n")
	gitIn(t, r, wt, "init", "-q", "-b", "master")
	gitIn(t, r, wt, "add", "-A")
	gitIn(t, r, wt, "commit", "-qm", "c")
	tree := gitIn(t, r, wt, "rev-parse", "HEAD^{tree}")
	src := filepath.Join(wt, "sub", ".git")
	if err := os.Rename(filepath.Join(wt, ".git"), src); err != nil {
		t.Fatal(err)
	}
	gitIn(t, r, filepath.Join(wt, "sub"), "config", "core.worktree", wt)
	g := newGit(r, func(string) string { return "file:///origin" }).(*execGit)
	entries, err := g.srcTree(context.Background(), src, filepath.Join(t.TempDir(), "acme", "app.git"), tree)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Path)
	}
	if !slices.Equal(names, []string{"sub", "top.rb"}) {
		t.Errorf("the resolver's root tree lists %q, want [sub top.rb]", names)
	}
}

// A conflicted path whose entry the resolver's merge holds but whose content
// its repository lacks (cat-file answers "missing", as for a removed path) is
// named as such, not as a path beyond the conflicts.
func TestAConflictedPathWhoseContentTheResolverLacksIsNamedSo(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	res := w.withResolver()
	c := w.client()
	defer c.Close()
	id := w.deploy(c, 2)
	w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 || ended(e) })
	dir, right := w.resolverRepo(2, []string{w.tip, w.heads[2]}, map[string]string{"app/s.rb": "resolved\n"})
	index := filepath.Join(t.TempDir(), "index")
	in := func(args ...string) string {
		t.Helper()
		res, err := w.r.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...), Env: map[string]string{"GIT_INDEX_FILE": index}})
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(res.Out())
	}
	in("read-tree", right+"^{tree}")
	in("update-index", "--cacheinfo", "100644,"+strings.Repeat("e", 40)+",app/s.rb")
	merge := in("commit-tree", in("write-tree", "--missing-ok"), "-p", w.tip, "-p", w.heads[2], "-m", "Merge")
	res.answer(ResolveResult{State: ResolveDone, MergeSHA: merge, Dir: dir})
	e := w.drive(c, id, ended)
	if e.State != EffectFailed || e.Step != StepMerge || !strings.Contains(e.Why, "lacks the content of the conflicted paths its merge names: app/s.rb") ||
		strings.Contains(e.Why, "beyond") {
		t.Errorf("ended: %+v, want failed at merge, app/s.rb named as content the resolver lacks", e)
	}
	w.noPushSince(0, w.tip)
}

// treeChanges names each path whose entry differs between two trees and
// nothing else: a changed file deep in a subtree, a file that took a
// directory's place (the file and each path the directory held), a
// directory that went with its last file (that file alone, which a
// conflict may name), and a new directory's files; it reads only the
// subtrees whose ids differ and stops at its limit.
func TestTreeChangesNamesEachDifferingPath(t *testing.T) {
	t.Parallel()
	oid := func(c byte) string { return strings.Repeat(string(c), 40) }
	blob := func(name string, c byte) treeEntry {
		return treeEntry{Mode: "100644", Type: "blob", OID: oid(c), Path: name}
	}
	dir := func(name string, c byte) treeEntry {
		return treeEntry{Mode: "040000", Type: "tree", OID: oid(c), Path: name}
	}
	trees := map[string][]treeEntry{
		// a: app/{s.rb, deep/x.rb}, lib/only.rb, same/ (untouched), d/k.rb
		oid('1'): {dir("app", '2'), dir("lib", '3'), dir("same", '4'), dir("d", '5')},
		oid('2'): {blob("s.rb", 'a'), dir("deep", '6')},
		oid('6'): {blob("x.rb", 'b')},
		oid('3'): {blob("only.rb", 'c')},
		oid('5'): {blob("k.rb", 'd')},
		// b: app/{s.rb changed, deep/x.rb}, no lib/, same/, d a file, new/n.rb
		oid('9'): {dir("app", '7'), dir("same", '4'), blob("d", 'e'), dir("new", '8')},
		oid('7'): {blob("s.rb", 'f'), dir("deep", '6')},
		oid('8'): {blob("n.rb", '0')},
	}
	var read []string
	list := func(_ context.Context, tree string) ([]treeEntry, error) {
		read = append(read, tree)
		entries, ok := trees[tree]
		if !ok {
			return nil, fmt.Errorf("no tree %s", short(tree))
		}
		return entries, nil
	}
	got, cut, err := treeChanges(context.Background(), list, list, oid('1'), oid('9'), 100, 0)
	if err != nil || cut {
		t.Fatal(err, cut)
	}
	if want := []string{"app/s.rb", "d", "d/k.rb", "lib/only.rb", "new/n.rb"}; !slices.Equal(got, want) {
		t.Errorf("changes: %q, want %q", got, want)
	}
	if slices.Contains(read, oid('4')) || slices.Contains(read, oid('6')) {
		t.Errorf("read a subtree the same on both sides: %q", read)
	}
	if got, _, err := treeChanges(context.Background(), list, list, oid('1'), oid('9'), 2, 0); err != nil || len(got) != 2 {
		t.Errorf("with a limit of 2: %q, %v", got, err)
	}
}

// The walk of two trees makes at most budget lists: a tree tampered with
// to hold hundreds of empty subtrees on one side names nothing, and the
// walk stops at its budget, saying it was cut, rather than holding the
// mirror's flock for a list each.
func TestTreeChangesStopsAtItsBudgetOfLists(t *testing.T) {
	t.Parallel()
	oid := func(i int) string { return fmt.Sprintf("%040x", i) }
	empty := oid(0)
	wide := make([]treeEntry, 400)
	for i := range wide {
		wide[i] = treeEntry{Mode: "040000", Type: "tree", OID: empty, Path: fmt.Sprintf("d%03d", i)}
	}
	trees := map[string][]treeEntry{oid(1): nil, oid(2): wide, empty: nil}
	reads := 0
	list := func(_ context.Context, tree string) ([]treeEntry, error) {
		reads++
		return trees[tree], nil
	}
	got, cut, err := treeChanges(context.Background(), list, list, oid(1), oid(2), maxListed+1, maxTreeReads)
	if err != nil || len(got) != 0 || !cut || reads > maxTreeReads {
		t.Errorf("walk: %q, cut %v, %v after %d lists; want nothing named, cut, at most %d lists", got, cut, err, reads, maxTreeReads)
	}
	reads = 0
	if _, cut, err := treeChanges(context.Background(), list, list, oid(1), oid(2), maxListed+1, 0); err != nil || cut || reads != 402 {
		t.Errorf("without a budget: cut %v, %v after %d lists; want all 402", cut, err, reads)
	}
}
