package gitx

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// commonDir is the absolute git common directory of dir.
func (fx *fixture) commonDir(dir string) string {
	fx.t.Helper()
	return fx.git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

// forgeCommitGraph writes dir's commit-graph and patches it to say that
// commit has tree and, as its first parent, parent: git reads a commit's
// tree and parents from the graph and never checks the graph's checksum.
func (fx *fixture) forgeCommitGraph(dir, commit, tree, parent string) {
	t := fx.t
	t.Helper()
	fx.git(dir, "commit-graph", "write", "--reachable")
	path := filepath.Join(fx.commonDir(dir), "objects", "info", "commit-graph")
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 8 || string(data[:4]) != "CGPH" {
		t.Fatalf("the commit-graph: %v", err)
	}
	chunks := map[string]int{}
	for i := range int(data[6]) {
		e := data[8+12*i:]
		chunks[string(e[:4])] = int(binary.BigEndian.Uint64(e[4:12]))
	}
	count := int(binary.BigEndian.Uint32(data[chunks["OIDF"]+255*4:]))
	pos := func(oid string) int {
		raw, _ := hex.DecodeString(oid)
		for i := range count {
			at := chunks["OIDL"] + 20*i
			if bytes.Equal(data[at:at+20], raw) {
				return i
			}
		}
		t.Fatalf("the commit-graph has no %s", oid)
		return 0
	}
	row := chunks["CDAT"] + pos(commit)*(20+16)
	raw, _ := hex.DecodeString(tree)
	copy(data[row:row+20], raw)
	binary.BigEndian.PutUint32(data[row+20:], uint32(pos(parent)))
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// forgeLoose overwrites the loose object oid in dir with an object of
// typ whose content is body: git reads a loose object as it is and never
// hashes it again. It returns a func that puts the real one back.
func (fx *fixture) forgeLoose(dir, oid, typ string, body []byte) (restore func()) {
	t := fx.t
	t.Helper()
	path := filepath.Join(fx.commonDir(dir), "objects", oid[:2], oid[2:])
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no loose object %s: %v", oid, err)
	}
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_, _ = w.Write(append([]byte(typ+" "+strconv.Itoa(len(body))+"\x00"), body...))
	_ = w.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.WriteFile(path, old, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// gitRaw is fx.git's output as git wrote it, binary content and all.
func (fx *fixture) gitRaw(dir string, args ...string) []byte {
	fx.t.Helper()
	res, err := fx.r.Run(context.Background(), gitCmd(dir, args...))
	if err != nil {
		fx.t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return res.Stdout
}

// The review's bug (K1): a commit-graph the fixer's pane forged (git
// ignores its checksum) gave gitx's reads another tree and other parents
// for a merge than the commit object has, while a push sends the commit
// object: DifferingPaths of a merge whose workflow files are the PR's own
// said none differ, and the daemon pushed it with the Workflows token.
// gitx reads no commit-graph now.
func TestGitxReadsACommitsTreeAndParentsFromTheCommitNotTheCommitGraph(t *testing.T) {
	fx := newHeldMergeFixture(t)
	ctx := context.Background()
	dir := fx.origin
	tip := fx.git(dir, "rev-parse", "main")
	fx.forgeCommitGraph(dir, fx.resolved, fx.git(dir, "rev-parse", tip+"^{tree}"), fx.base)
	if got := fx.git(dir, "log", "-1", "--format=%T %P", fx.resolved); !strings.HasPrefix(got, fx.git(dir, "rev-parse", tip+"^{tree}")+" "+fx.base) {
		t.Fatalf("git without gitx's settings reads %q: the forged graph is not read, so this test shows nothing", got)
	}
	if got, err := fx.c.DifferingPaths(ctx, dir, fx.resolved, tip, ".github/workflows"); err != nil || !slices.Contains(got, ".github/workflows/ci.yml") {
		t.Errorf("DifferingPaths(merge, tip) = %q, %v; want ci.yml, as the merge's own tree has it", got, err)
	}
	if got, err := fx.c.PathsDiff(ctx, dir, fx.head, fx.resolved, ".github/workflows"); err != nil || !strings.Contains(got, "+jobs: resolved") {
		t.Errorf("PathsDiff(head, merge) = %q, %v; want the merge's own resolution", got, err)
	}
	if ps, err := fx.c.Parents(ctx, dir, fx.resolved); err != nil || len(ps) != 2 || ps[0] != fx.head {
		t.Errorf("Parents(merge) = %q, %v; want the head first, as the commit has it", ps, err)
	}
}

// The review's bug (K1): a loose object whose bytes are not what its name
// says (git does not hash an object again when it reads it) showed a
// benign workflow file in held-merge.diff while GitHub, which has the
// real one, kept it. VerifyObjects hashes again, in Go, every object a
// read of the workflow files goes through: each commit, its root tree,
// the trees down to the path and everything under it.
func TestVerifyObjectsFindsAnObjectWhoseContentIsNotItsName(t *testing.T) {
	fx := newHeldMergeFixture(t)
	ctx := context.Background()
	dir := fx.origin
	tip := fx.git(dir, "rev-parse", "main")
	if err := fx.c.VerifyObjects(ctx, dir, ".github/workflows", fx.head, tip, fx.resolved, fx.base); err != nil {
		t.Fatalf("VerifyObjects of honest objects = %v", err)
	}
	blob := fx.git(dir, "rev-parse", fx.resolved+":.github/workflows/ci.yml")
	tree := fx.git(dir, "rev-parse", fx.resolved+":.github/workflows")
	forged := []struct {
		name, oid, typ string
		body           []byte
	}{
		{"the merge's commit", fx.resolved, "commit", []byte(fx.git(dir, "cat-file", "commit", fx.resolved) + "\nmore\n")},
		{"the workflows tree", tree, "tree", fx.gitRaw(dir, "cat-file", "tree", fx.head+":.github/workflows")},
		{"a workflow file", blob, "blob", []byte("on: push\njobs: harmless\n")},
	}
	for _, f := range forged {
		restore := fx.forgeLoose(dir, f.oid, f.typ, f.body)
		err := fx.c.VerifyObjects(ctx, dir, ".github/workflows", fx.head, tip, fx.resolved)
		if !errors.Is(err, ErrObjectMismatch) || !strings.Contains(err.Error(), f.oid) {
			t.Errorf("%s forged: VerifyObjects = %v, want ErrObjectMismatch naming %s", f.name, err, f.oid)
		}
		restore()
	}
	if err := fx.c.VerifyObjects(ctx, dir, ".github/workflows", "-x"); err == nil {
		t.Error("an option as a commit was taken")
	}
}

// The review's bug (K2): RemergeDiff ran in the clone, so a merge driver
// the clone's attributes and config named (merge=x and merge.x.driver) ran
// inside the daemon, and its answer, the merge as committed, made the
// remerge diff say that no conflict was resolved. In a repository of its
// own (Isolated) no attribute, config or driver of the clone's applies.
func TestIsolatedReadsRunNoMergeDriverOrAttributeOfTheClones(t *testing.T) {
	fx := newHeldMergeFixture(t)
	ctx := context.Background()
	dir := fx.origin
	marker := filepath.Join(t.TempDir(), "driver-ran")
	if err := os.WriteFile(filepath.Join(fx.commonDir(dir), "info", "attributes"), []byte(".github/workflows/* merge=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.git(dir, "config", "merge.evil.driver", "touch "+marker+"; printf 'on: push\\njobs: resolved\\n' > %A")
	if got, err := fx.c.RemergeDiff(ctx, dir, fx.resolved, ".github/workflows"); err != nil || got != "" {
		t.Fatalf("RemergeDiff in the clone = %q, %v: the clone's driver did not answer, so this test shows nothing", got, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("the clone's driver did not run: %v", err)
	}
	var remerge, plain string
	var differ, changed []string
	err := fx.c.Isolated(ctx, dir, func(iso string) error {
		var err error
		if remerge, err = fx.c.RemergeDiff(ctx, iso, fx.resolved, ".github/workflows"); err != nil {
			return err
		}
		if plain, err = fx.c.PathsDiff(ctx, iso, fx.head, fx.resolved, ".github/workflows"); err != nil {
			return err
		}
		if differ, err = fx.c.DifferingPaths(ctx, iso, fx.resolved, fx.head, ".github/workflows"); err != nil {
			return err
		}
		changed, err = fx.c.ChangedPaths(ctx, iso, fx.head, fx.resolved, ".github/workflows")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remerge, "remerge CONFLICT") || !strings.Contains(remerge, "+jobs: resolved") {
		t.Errorf("RemergeDiff in Isolated = %q, want the conflict and its resolution", remerge)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the clone's merge driver ran in Isolated")
	}
	if !strings.Contains(plain, "+jobs: resolved") || len(differ) == 0 || len(changed) == 0 {
		t.Errorf("PathsDiff %q, DifferingPaths %q, ChangedPaths %q in Isolated; want the merge's workflow files", plain, differ, changed)
	}
}

// The review's bug (K4): the daemon's push ran `git -C <its bare
// repository>`, which a global safe.bareRepository=explicit (git 3.0's
// default) refuses, so every merge of the parent and every y failed. It
// names the repository with --git-dir, as Isolated's reads do.
func TestTheIsolatedRepositoryWorksWhenTheGlobalConfigWantsExplicitBareRepositories(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[safe]\n\tbareRepository = explicit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := slices.DeleteFunc(slices.Clone(fx.r.BaseEnv), func(kv string) bool { return strings.HasPrefix(kv, "GIT_CONFIG_GLOBAL=") })
	c := New(&execx.Real{BaseEnv: append(env, "GIT_CONFIG_GLOBAL="+global)})
	fx.git(fx.clone, "switch", "--quiet", "--detach", fx.pr)
	head := fx.commit(fx.clone, "app/a.rb", "a = 3\n", "fix")
	if err := c.Isolated(ctx, fx.clone, func(iso string) error {
		_, err := c.ChangedPaths(ctx, iso, fx.pr, head)
		return err
	}); err != nil {
		t.Errorf("a read in Isolated = %v", err)
	}
	if err := c.pushIsolated(ctx, fx.clone, fx.origin, head, "pr7", fx.pr, nil); err != nil {
		t.Fatalf("pushIsolated = %v", err)
	}
	if got := fx.git(fx.origin, "rev-parse", "refs/heads/pr7"); got != head {
		t.Errorf("origin's pr7 = %s, want the pushed %s", got, head)
	}
}

// The review's bug (K3): copyShallow checked the path, then opened it, so
// a link swapped in between could name a FIFO, whose open waits for a
// writer forever, or any file. It opens with no link followed and without
// waiting, takes a regular file alone, and a size a shallow list has.
func TestCopyShallowTakesARegularFileAloneAndNeverWaits(t *testing.T) {
	t.Parallel()
	copyOf := func(lay func(path string) error) (string, error) {
		common, to := t.TempDir(), t.TempDir()
		if err := lay(filepath.Join(common, "shallow")); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- copyShallow(common, to) }()
		select {
		case err := <-done:
			got, _ := os.ReadFile(filepath.Join(to, "shallow"))
			return string(got), err
		case <-time.After(10 * time.Second):
			t.Fatal("copyShallow waits")
			return "", nil
		}
	}
	line := strings.Repeat("a", 40) + "\n"
	if got, err := copyOf(func(p string) error { return os.WriteFile(p, []byte(line), 0o644) }); err != nil || got != line {
		t.Errorf("a regular file: %q, %v", got, err)
	}
	if got, err := copyOf(func(string) error { return nil }); err != nil || got != "" {
		t.Errorf("no file: %q, %v; want nothing copied", got, err)
	}
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, lay := range map[string]func(string) error{
		"a link":      func(p string) error { return os.Symlink(target, p) },
		"a FIFO":      func(p string) error { return syscall.Mkfifo(p, 0o600) },
		"a directory": func(p string) error { return os.Mkdir(p, 0o700) },
		"a huge file": func(p string) error {
			return os.WriteFile(p, bytes.Repeat([]byte(line), shallowMax/len(line)+1), 0o644)
		},
		"a link, FIFO": func(p string) error { f := p + ".fifo"; return errors.Join(syscall.Mkfifo(f, 0o600), os.Symlink(f, p)) },
	} {
		if got, err := copyOf(lay); err == nil || got != "" {
			t.Errorf("%s: copied %d bytes, %v; want it refused", name, len(got), err)
		}
	}
}
