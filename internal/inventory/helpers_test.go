package inventory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// Compile-time proof that the real clients satisfy the scanner's interfaces.
var (
	_ Git    = (*gitx.Client)(nil)
	_ MySQL  = (*mysqlx.Client)(nil)
	_ Herdr  = (*herdr.Client)(nil)
	_ GitHub = (*github.Client)(nil)
)

type fakeGit struct {
	mu        sync.Mutex
	lists     map[string][]gitx.Worktree
	listErr   map[string]error
	status    map[string]gitx.Status
	statusErr map[string]error
	prs       map[string]int    // branch -> PR number (git config branch.<b>.pr)
	clones    map[string]string // owner/name -> clone FindClone reports
	findErr   error             // FindClone fails with this (not ErrNoClone)
	calls     []string
}

func newFakeGit() *fakeGit {
	return &fakeGit{
		lists: map[string][]gitx.Worktree{}, listErr: map[string]error{},
		status: map[string]gitx.Status{}, statusErr: map[string]error{}, prs: map[string]int{},
		clones: map[string]string{},
	}
}

func (g *fakeGit) record(s string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, s)
}

func (g *fakeGit) WorktreeList(_ context.Context, mainClone string) ([]gitx.Worktree, error) {
	g.record("worktree-list " + mainClone)
	if err := g.listErr[mainClone]; err != nil {
		return nil, err
	}
	list, ok := g.lists[mainClone]
	if !ok {
		return nil, fmt.Errorf("fakeGit: no worktree list for %s", mainClone)
	}
	return slices.Clone(list), nil
}

func (g *fakeGit) Status(_ context.Context, dir string) (gitx.Status, error) {
	g.record("status " + dir)
	if err := g.statusErr[dir]; err != nil {
		return gitx.Status{}, err
	}
	return g.status[dir], nil
}

func (g *fakeGit) BranchPR(_ context.Context, dir, branch string) (int, bool, error) {
	g.record("branch-pr " + branch)
	n, ok := g.prs[branch]
	return n, ok, nil
}

func (g *fakeGit) FindClone(_ context.Context, root, owner, name string) (string, error) {
	g.record("find-clone " + root + " " + owner + "/" + name)
	if g.findErr != nil {
		return "", g.findErr
	}
	if p, ok := g.clones[owner+"/"+name]; ok {
		return p, nil
	}
	return "", fmt.Errorf("%w: %s/%s", gitx.ErrNoClone, owner, name)
}

// fakeMySQL lists by name prefix, as (*mysqlx.Client).ListPrefixed does.
type fakeMySQL struct {
	dbs      []mysqlx.Database
	err      error
	prefixes [][]string // every ListPrefixed call
}

func (m *fakeMySQL) ListPrefixed(_ context.Context, prefixes []string) ([]mysqlx.Database, error) {
	m.prefixes = append(m.prefixes, slices.Clone(prefixes))
	if m.err != nil {
		return nil, m.err
	}
	var out []mysqlx.Database
	for _, d := range m.dbs {
		if slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(d.Name, p) }) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *fakeMySQL) add(slug string, sizeMB float64, names ...string) {
	for _, n := range names {
		m.dbs = append(m.dbs, mysqlx.Database{Name: n + "__" + slug, Slug: slug, SizeMB: sizeMB})
	}
}

func (m *fakeMySQL) remove(name string) {
	m.dbs = slices.DeleteFunc(m.dbs, func(d mysqlx.Database) bool { return d.Name == name })
}

type fakeHerdr struct {
	snap herdr.Snapshot
	err  error
}

func (h *fakeHerdr) Snapshot(context.Context) (herdr.Snapshot, error) {
	if h.err != nil {
		return herdr.Snapshot{}, h.err
	}
	return h.snap, nil
}

type confirmCall struct {
	Owner, Repo string
	Numbers     []int
}

type fakeGitHub struct {
	mu       sync.Mutex
	states   map[int]github.PRState
	notFound []int
	err      error
	calls    []confirmCall
}

func (f *fakeGitHub) ConfirmStates(_ context.Context, owner, repo string, numbers []int) (map[int]github.PRState, []int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, confirmCall{owner, repo, slices.Clone(numbers)})
	f.mu.Unlock()
	if f.err != nil {
		return nil, nil, f.err
	}
	out := map[int]github.PRState{}
	var missing []int
	for _, n := range numbers {
		if st, ok := f.states[n]; ok {
			out[n] = st
		} else if slices.Contains(f.notFound, n) {
			missing = append(missing, n)
		}
	}
	return out, missing, nil
}

var talkableDBs = []string{
	"talkable_development", "talkable_development_reporting",
	"talkable_development_shard_1", "talkable_development_shard_1001",
	"talkable_test", "talkable_test_reporting",
	"talkable_test_shard_1", "talkable_test_shard_1001",
}

func dbTemplates() []string {
	out := make([]string, 0, len(talkableDBs))
	for _, n := range talkableDBs {
		out = append(out, n+"__{slug}")
	}
	return out
}

type fixture struct {
	t     *testing.T
	root  string
	main  string
	st    *store.Store
	git   *fakeGit
	db    *fakeMySQL
	hd    *fakeHerdr
	gh    *fakeGitHub
	cfg   *config.Config
	repo  store.Repo
	now   time.Time
	slots map[string]store.Slot
}

// newFixture builds a canonical temp root with a main clone dir, one pool
// config for talkable/talkable and an empty store.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t: t, root: root, main: filepath.Join(root, "talkable"),
		git: newFakeGit(), db: &fakeMySQL{}, hd: &fakeHerdr{}, gh: &fakeGitHub{states: map[int]github.PRState{}},
		now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), slots: map[string]store.Slot{},
	}
	f.mkdir("talkable")
	f.cfg = &config.Config{
		Pools: []config.Pool{{
			Repo: "talkable/talkable", MainClone: f.main, SlotName: "review{n}",
			SlotPath: filepath.Join(root, "talkable.review{n}"), Base: "master", Databases: dbTemplates(),
		}},
		Watches: []config.Watch{{Owner: "talkable", Include: []string{"talkable"}, CloneRoot: root}},
	}
	f.st = storetest.Open(t, filepath.Join(t.TempDir(), "magnum.db"))
	f.repo, err = f.st.UpsertRepo(context.Background(), store.Repo{
		NodeID: "R_talkable", Owner: "talkable", Name: "talkable", Mode: store.RepoModePool, ClonePath: new(f.main),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.git.lists[f.main] = []gitx.Worktree{{Path: f.main, Head: sha("main"), Branch: "master"}}
	return f
}

func (f *fixture) scanner() *Scanner {
	return &Scanner{
		Store: f.st, Git: f.git, MySQL: f.db, Herdr: f.hd, GitHub: f.gh, Config: f.cfg,
		Now: func() time.Time { return f.now },
	}
}

func (f *fixture) scan(opts Options) Inventory {
	f.t.Helper()
	inv, err := f.scanner().Scan(context.Background(), opts)
	if err != nil {
		f.t.Fatalf("Scan: %v", err)
	}
	return inv
}

func (f *fixture) path(rel string) string { return filepath.Join(f.root, rel) }

func (f *fixture) mkdir(rel string) string {
	f.t.Helper()
	p := f.path(rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// mkrepo creates a directory that looks like a git clone (a .git entry).
func (f *fixture) mkrepo(rel string) string {
	f.t.Helper()
	p := f.mkdir(rel)
	f.mkdir(filepath.Join(rel, ".git"))
	return p
}

func (f *fixture) writeSlug(rel, slug string) {
	f.t.Helper()
	dir := f.mkdir(filepath.Join(rel, "tmp"))
	if err := os.WriteFile(filepath.Join(dir, ".worktree-db-slug"), []byte(slug+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// worktree appends a worktree of the main clone to the fake git list.
func (f *fixture) worktree(wt gitx.Worktree) {
	f.git.lists[f.main] = append(f.git.lists[f.main], wt)
}

// slot registers a pool slot (state given) at talkable.<name>; dir created when exists.
func (f *fixture) slot(name, state string, exists bool) store.Slot {
	f.t.Helper()
	if exists {
		f.mkdir("talkable." + name)
	}
	return f.slotAt(name, f.path("talkable."+name), state)
}

// slotAt registers a pool slot at path without touching the filesystem.
func (f *fixture) slotAt(name, path, state string) store.Slot {
	f.t.Helper()
	sl, err := f.st.CreateSlot(context.Background(), store.Slot{
		Name: name, RepoID: new(f.repo.ID), RepoFullName: "talkable/talkable", Kind: store.SlotKindPool,
		Path: path, MainClone: f.main, PlaceholderBranch: new(name), DBSlug: new(name), State: state,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.slots[name] = sl
	return sl
}

func (f *fixture) pr(number int, ghState, head string) store.PR {
	f.t.Helper()
	res, err := f.st.UpsertPRFromGitHub(context.Background(), store.GitHubPR{
		RepoID: f.repo.ID, NodeID: fmt.Sprintf("PR_%d", number), Number: number,
		URL: fmt.Sprintf("https://github.com/talkable/talkable/pull/%d", number), HeadSHA: head,
		GHState: ghState, InitialState: store.PRQueued, Identity: "talkable-app",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return res.PR
}

func (f *fixture) claim(prID int64, slot string) store.Assignment {
	f.t.Helper()
	a, err := f.st.ClaimSlot(context.Background(), prID, f.slots[slot].ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

// unreadable makes dir inaccessible (mode 000) until the test ends. Root
// ignores permissions, so the test is skipped there.
func unreadable(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions are not enforced")
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

func hasWarning(inv Inventory, prefix, substr string) bool {
	return slices.ContainsFunc(inv.Warnings, func(w string) bool {
		return strings.HasPrefix(w, prefix) && strings.Contains(w, substr)
	})
}

func dbNames(dbs []mysqlx.Database) []string {
	out := []string{}
	for _, d := range dbs {
		out = append(out, d.Name)
	}
	return out
}

func sha(seed string) string {
	s := fmt.Sprintf("%x", seed)
	for len(s) < 40 {
		s += "0"
	}
	return s[:40]
}

func findingsOf(inv Inventory, kind string) []Finding {
	var out []Finding
	for _, d := range inv.Drift {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

func slotView(t *testing.T, inv Inventory, name string) SlotView {
	t.Helper()
	for _, s := range inv.Slots {
		if s.Slot.Name == name {
			return s
		}
	}
	t.Fatalf("slot %s not in inventory", name)
	return SlotView{}
}

func externalView(t *testing.T, inv Inventory, path string) ExternalView {
	t.Helper()
	for _, e := range inv.External {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("external %s not in inventory (have %d)", path, len(inv.External))
	return ExternalView{}
}
