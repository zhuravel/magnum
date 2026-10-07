package cleanup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

var now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

var dbTemplates = []string{
	"talkable_development__{slug}", "talkable_development_reporting__{slug}",
	"talkable_development_shard_1__{slug}", "talkable_development_shard_1001__{slug}",
	"talkable_test__{slug}", "talkable_test_reporting__{slug}",
	"talkable_test_shard_1__{slug}", "talkable_test_shard_1001__{slug}",
}

// fakeSlots records every call; errs is keyed "op slot" (e.g. "release review5").
// evidence answers HumanEvidence by slot name (errs["evidence <slot>"] fails it);
// those calls are not recorded.
type fakeSlots struct {
	calls    []string
	errs     map[string]error
	pools    []config.Pool
	evidence map[string]string
}

func (f *fakeSlots) record(op, name string, extra string) error {
	f.calls = append(f.calls, strings.TrimSpace(op+" "+name+" "+extra))
	return f.errs[op+" "+name]
}

func (f *fakeSlots) Release(ctx context.Context, slot store.Slot, pool config.Pool, reason string) error {
	f.pools = append(f.pools, pool)
	return f.record("release", slot.Name, reason)
}

func (f *fakeSlots) Remove(ctx context.Context, slot store.Slot, pool config.Pool, force bool) error {
	f.pools = append(f.pools, pool)
	return f.record("remove", slot.Name, fmt.Sprintf("force=%v", force))
}

func (f *fakeSlots) RemovePRWorktree(ctx context.Context, slot store.Slot, force bool) error {
	return f.record("remove_pr", slot.Name, fmt.Sprintf("force=%v", force))
}

func (f *fakeSlots) GuardLive(ctx context.Context, slot store.Slot) error {
	return f.record("guard_live", slot.Name, "")
}

func (f *fakeSlots) HumanEvidence(ctx context.Context, slot store.Slot) (string, error) {
	return f.evidence[slot.Name], f.errs["evidence "+slot.Name]
}

type fakeInventory struct {
	inv   inventory.Inventory
	err   error
	calls []inventory.Options
}

func (f *fakeInventory) Scan(ctx context.Context, opts inventory.Options) (inventory.Inventory, error) {
	f.calls = append(f.calls, opts)
	return f.inv, f.err
}

type fakeMySQL struct {
	calls  [][]string
	guards []mysqlx.Guard
	fail   map[string]error
}

func (f *fakeMySQL) DropAll(ctx context.Context, names []string, g mysqlx.Guard) []mysqlx.DropResult {
	f.calls = append(f.calls, append([]string(nil), names...))
	f.guards = append(f.guards, g)
	out := make([]mysqlx.DropResult, 0, len(names))
	for _, n := range names {
		err := g.Check(n)
		if err == nil {
			err = f.fail[n]
		}
		out = append(out, mysqlx.DropResult{Name: n, Err: err})
	}
	return out
}

type fakeGit struct {
	status   map[string]gitx.Status
	paths    map[string][]gitx.StatusEntry // StatusPaths by dir
	unpushed map[string]int
	branches map[string]int   // "dir refs/heads/<b>" -> its commits on no remote; absent: no such branch
	errs     map[string]error // keyed "op dir"
	calls    []string
}

func (f *fakeGit) err(op, dir string) error { return f.errs[op+" "+dir] }

func (f *fakeGit) FetchBranch(ctx context.Context, mainClone, base string) error {
	f.calls = append(f.calls, "fetch "+mainClone+" "+base)
	return f.err("fetch", mainClone)
}

func (f *fakeGit) ResetPlaceholder(ctx context.Context, dir, branch, base string) error {
	f.calls = append(f.calls, "reset "+dir+" "+branch+" "+base)
	return f.err("reset", dir)
}

func (f *fakeGit) Status(ctx context.Context, dir string) (gitx.Status, error) {
	return f.status[dir], f.err("status", dir)
}

func (f *fakeGit) StatusPaths(ctx context.Context, dir string) ([]gitx.StatusEntry, error) {
	return f.paths[dir], f.err("status", dir)
}

func (f *fakeGit) Unpushed(ctx context.Context, dir string) (int, error) {
	return f.unpushed[dir], f.err("unpushed", dir)
}

// RevParse answers HEAD with headSHA and a branch in branches with
// branchSHA; any other ref does not exist.
func (f *fakeGit) RevParse(ctx context.Context, dir, ref string) (string, error) {
	if ref == "HEAD" {
		return headSHA, f.err("rev-parse", dir)
	}
	if _, ok := f.branches[dir+" "+ref]; !ok {
		return "", fmt.Errorf("fake: %s: %w", ref, gitx.ErrNoSuchRef)
	}
	return branchSHA, f.err("rev-parse", dir)
}

func (f *fakeGit) UnpushedRef(ctx context.Context, dir, ref string) (int, error) {
	return f.branches[dir+" "+ref], f.err("unpushed-ref", dir)
}

var (
	headSHA   = strings.Repeat("b", 40)
	branchSHA = strings.Repeat("a", 40)
)

type fixture struct {
	t        *testing.T
	ctx      context.Context
	root     string
	st       *store.Store
	cfg      *config.Config
	talkable store.Repo
	foo      store.Repo
	slots    *fakeSlots
	inv      *fakeInventory
	mysql    *fakeMySQL
	git      *fakeGit
	run      *execx.Fake
	parked   []int64
	parkErr  error
	parkHook func(store.PR) // runs inside Park, as something racing the release would
	p        *Planner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state", "magnum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Clock = func() time.Time { return now }
	pool := config.Pool{
		Repo: "talkable/talkable", MainClone: filepath.Join(root, "talkable"),
		SlotName: "review{n}", SlotPath: filepath.Join(root, "talkable.review{n}"),
		Base: "master", Min: 2, Max: 4, IdleRemoveAfter: config.Duration{Duration: 48 * time.Hour},
		Databases: dbTemplates,
	}
	cfg := &config.Config{
		Pools: []config.Pool{pool},
		Watches: []config.Watch{
			{Owner: "talkable", Include: []string{"talkable"}, Identity: "talkable-app", CloneRoot: root},
			{Owner: "zhuravel", Include: []string{"*"}, Identity: "zhuravel", CloneRoot: root},
		},
	}
	f := &fixture{
		t: t, ctx: context.Background(), root: root, st: st, cfg: cfg,
		slots: &fakeSlots{errs: map[string]error{}, evidence: map[string]string{}},
		inv:   &fakeInventory{inv: inventory.Inventory{DatabasesListed: true, AgentsListed: true, ClonesListed: true, OrphansKnown: true}},
		mysql: &fakeMySQL{fail: map[string]error{}},
		git: &fakeGit{status: map[string]gitx.Status{}, paths: map[string][]gitx.StatusEntry{}, unpushed: map[string]int{},
			branches: map[string]int{}, errs: map[string]error{}},
		run: &execx.Fake{},
	}
	f.talkable = f.repo("talkable", "talkable", store.RepoModePool)
	f.foo = f.repo("zhuravel", "foo", store.RepoModePerPR)
	f.run.Rules = []execx.Rule{{Prefix: []string{"du", "-sk"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		// 1 KiB per character of the base name keeps sizes deterministic.
		return execx.Result{Stdout: []byte(fmt.Sprintf("%d\t%s\n", 1024*len(filepath.Base(c.Args[1])), c.Args[1]))}, nil
	}}}
	f.p = &Planner{
		Store: st, Slots: f.slots, Inventory: f.inv, MySQL: f.mysql, Git: f.git, Runner: f.run,
		Config: cfg, Clock: func() time.Time { return now },
		Park: func(ctx context.Context, pr store.PR) error {
			f.parked = append(f.parked, pr.ID)
			if f.parkHook != nil {
				f.parkHook(pr)
			}
			return f.parkErr
		},
	}
	return f
}

func (f *fixture) repo(owner, name, mode string) store.Repo {
	f.t.Helper()
	r, err := f.st.UpsertRepo(f.ctx, store.Repo{NodeID: "R_" + owner + "_" + name, Owner: owner, Name: name, Mode: mode, LastSeenAt: now})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// pr creates a PR in state with gh_state gh; set adjusts automation columns.
func (f *fixture) pr(repo store.Repo, number int, state, gh string, set func(*store.PRUpdate)) store.PR {
	f.t.Helper()
	g := store.GitHubPR{
		RepoID: repo.ID, NodeID: fmt.Sprintf("PR_%s_%d", repo.Name, number), Number: number,
		URL:     fmt.Sprintf("https://github.com/%s/pull/%d", repo.FullName(), number),
		HeadSHA: fmt.Sprintf("%040d", number), GHState: gh, InitialState: store.PRReviewed, Identity: "zhuravel",
	}
	switch gh {
	case store.GHMerged:
		g.MergedAt = store.Ptr(now.Add(-14 * time.Minute))
		g.ClosedAt = g.MergedAt
	case store.GHClosed:
		g.ClosedAt = store.Ptr(now.Add(-2 * time.Hour))
	}
	up, err := f.st.UpsertPRFromGitHub(f.ctx, g)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.TransitionPR(f.ctx, up.PR.ID, nil, state, set); err != nil {
		f.t.Fatal(err)
	}
	pr, err := f.st.PRByID(f.ctx, up.PR.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return pr
}

// closedPR is a PR closed by the poller with release_after at now+graceLeft.
func (f *fixture) closedPR(repo store.Repo, number int, gh string, graceLeft time.Duration) store.PR {
	return f.pr(repo, number, store.PRClosed, gh, func(u *store.PRUpdate) {
		u.Set("prev_state", store.PRReviewed)
		u.Set("release_after", now.Add(graceLeft))
	})
}

// poolSlot registers pool slot name in state; dir creates its directory.
func (f *fixture) poolSlot(name, state string, prID int64, dir bool, set func(*store.Slot)) store.Slot {
	f.t.Helper()
	sl := store.Slot{
		Name: name, RepoID: &f.talkable.ID, RepoFullName: "talkable/talkable", Kind: store.SlotKindPool,
		Path: filepath.Join(f.root, "talkable."+name), MainClone: filepath.Join(f.root, "talkable"),
		PlaceholderBranch: store.Ptr(name), DBSlug: store.Ptr(name), State: state,
	}
	if prID != 0 {
		sl.PRID = &prID
	}
	if set != nil {
		set(&sl)
	}
	return f.createSlot(sl, dir)
}

func (f *fixture) prSlot(pr store.PR, state string, dir bool) store.Slot {
	f.t.Helper()
	sl := store.Slot{
		Name: slots.PRSlotName(f.foo.FullName(), pr.Number), RepoID: &f.foo.ID, RepoFullName: f.foo.FullName(),
		Kind: store.SlotKindPerPR, Path: filepath.Join(f.root, "foo__worktrees", fmt.Sprintf("pr-%d", pr.Number)),
		MainClone: filepath.Join(f.root, "foo"), State: state, PRID: &pr.ID,
	}
	return f.createSlot(sl, dir)
}

func (f *fixture) createSlot(sl store.Slot, dir bool) store.Slot {
	f.t.Helper()
	got, err := f.st.CreateSlot(f.ctx, sl)
	if err != nil {
		f.t.Fatal(err)
	}
	if dir {
		if err := os.MkdirAll(got.Path, 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	f.inv.inv.Slots = append(f.inv.inv.Slots, inventory.SlotView{Slot: got, Exists: dir, Slug: store.Deref(got.DBSlug)})
	return got
}

// view returns the inventory view of slot name for tweaking.
func (f *fixture) view(name string) *inventory.SlotView {
	f.t.Helper()
	for i := range f.inv.inv.Slots {
		if f.inv.inv.Slots[i].Slot.Name == name {
			return &f.inv.inv.Slots[i]
		}
	}
	f.t.Fatalf("no view for %s", name)
	return nil
}

func (f *fixture) dirty(name string, tracked, untracked int) {
	v := f.view(name)
	d := tracked+untracked > 0
	v.Dirty, v.Tracked, v.Untracked = &d, tracked, untracked
}

func (f *fixture) agent(name string, a inventory.AgentView) {
	v := f.view(name)
	v.Agents = append(v.Agents, a)
}

// slotDBs gives a slot view its eight present databases of sizeMB each.
func (f *fixture) slotDBs(name string, sizeMB float64) {
	v := f.view(name)
	for _, tmpl := range dbTemplates {
		n := strings.ReplaceAll(tmpl, "{slug}", v.Slug)
		v.Databases = append(v.Databases, inventory.DBView{Name: n, SizeMB: sizeMB, Present: true, Expected: true})
		f.inv.inv.Databases = append(f.inv.inv.Databases, mysqlx.Database{Name: n, Slug: v.Slug, SizeMB: sizeMB})
	}
}

// orphan adds the eight databases of slug as orphans.
func (f *fixture) orphan(slug string, sizeMB float64) []string {
	var names []string
	for _, tmpl := range dbTemplates {
		n := strings.ReplaceAll(tmpl, "{slug}", slug)
		d := mysqlx.Database{Name: n, Slug: slug, SizeMB: sizeMB}
		f.inv.inv.Databases = append(f.inv.inv.Databases, d)
		f.inv.inv.OrphanDBs = append(f.inv.inv.OrphanDBs, d)
		names = append(names, n)
	}
	return names
}

func (f *fixture) external(name string, set func(*inventory.ExternalView)) inventory.ExternalView {
	ev := inventory.ExternalView{
		Path: filepath.Join(f.root, "talkable."+name), MainClone: filepath.Join(f.root, "talkable"),
		Repo: "talkable/talkable", Exists: true, Branch: name,
	}
	if set != nil {
		set(&ev)
	}
	f.inv.inv.External = append(f.inv.inv.External, ev)
	return ev
}

func (f *fixture) activeRun(pr store.PR, state string) store.Run {
	f.t.Helper()
	r, err := f.st.CreateRun(f.ctx, store.Run{
		PRID: pr.ID, Round: 1, Role: store.RoleJudge, Kind: store.RunInitial, TargetSHA: pr.HeadSHA,
		Identity: "zhuravel", ReviewerLogin: "zhuravel", State: state, PromptText: "x",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) plan(opts Options) Plan {
	f.t.Helper()
	p, err := f.p.Plan(f.ctx, opts)
	if err != nil {
		f.t.Fatalf("Plan(%+v): %v", opts, err)
	}
	return p
}

func (f *fixture) prState(id int64) string {
	f.t.Helper()
	pr, err := f.st.PRByID(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return pr.State
}

func (f *fixture) events(subject string) []store.Event {
	f.t.Helper()
	evs, err := f.st.EventsBySubject(f.ctx, subject, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return evs
}

func actionsOf(p Plan, kind string) []Action {
	var out []Action
	for _, a := range p.Actions {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

func skipFor(p Plan, subject string) (Skip, bool) {
	for _, s := range p.Skipped {
		if s.Subject == subject {
			return s, true
		}
	}
	return Skip{}, false
}

func mustSkip(t *testing.T, p Plan, subject, reason string) Skip {
	t.Helper()
	s, ok := skipFor(p, subject)
	if !ok {
		t.Fatalf("no skip for %s; skipped = %+v; actions = %+v", subject, p.Skipped, p.Actions)
	}
	if s.Reason != reason {
		t.Fatalf("skip %s reason = %q (%s), want %q", subject, s.Reason, s.Detail, reason)
	}
	return s
}

var (
	errBoom           = errors.New("boom")
	_                 = herdr.StatusWorking
	_       Slots     = (*slots.Manager)(nil)
	_       Git       = (*gitx.Client)(nil)
	_       MySQL     = (*mysqlx.Client)(nil)
	_       Inventory = (*inventory.Scanner)(nil)
)
