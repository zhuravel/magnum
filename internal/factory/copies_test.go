package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The base's schema: db/schema.rb's version and its migration's file.
const (
	baseSchema    = "2026_09_14_085954"
	baseMigration = "20260914085954"
)

// copiesRig is the factory's copies over real git in temp directories (a
// bare origin and its main clone; the copies are its worktrees), the
// factory's agents store with magnum's slot code over it, a fake MySQL, and
// fakes for mise, redis-cli, the process table and kill: no real process
// but git, and no git outside t.TempDir.
type copiesRig struct {
	t            *testing.T
	ctx          context.Context
	root         string
	origin, main string
	clock        *storetest.Clock
	st           *store.Store
	reg          *registry.Registry
	cfg          *config.Config
	dbs          *copyDBs
	real         *execx.Real
	git          *gitx.Client
	sm           *slots.Manager
	c            *copySet
	// factoryHerdr is the factory's herdr session, yourHerdr yours: each
	// numbers its own workspaces and panes from w1.
	factoryHerdr, yourHerdr *fakeHerdrView

	mu        sync.Mutex
	scripts   []miseCall     // every mise exec, in order
	failSetup map[string]int // copy name -> setup failures left
	failRedis int            // FLUSHDB failures left
	// blocks hold a mise exec of "<script> <copy>" until its channel closes;
	// entered gets "<script> <copy>" as one starts to wait.
	blocks  map[string]chan struct{}
	entered chan string
	flushed []string // the Redis databases flushed, "<db>"
	table   []execx.Process
	kills   []string // "<pid> <signal>"
	spawned []spawnCall
	nextPID int
}

// fakeHerdrView is one herdr session as the copies' guard reads it.
type fakeHerdrView struct {
	mu    sync.Mutex
	snap  herdr.Snapshot
	procs map[string]herdr.ProcessInfo // pane -> its processes; default an idle shell
}

func (f *fakeHerdrView) Snapshot(context.Context) (herdr.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return herdr.Snapshot{Workspaces: slices.Clone(f.snap.Workspaces), Panes: slices.Clone(f.snap.Panes),
		Agents: slices.Clone(f.snap.Agents)}, nil
}

func (f *fakeHerdrView) PaneProcessInfo(_ context.Context, pane string) (herdr.ProcessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.snap.Panes {
		if p.ID == pane {
			if pi, ok := f.procs[pane]; ok {
				return pi, nil
			}
			return herdr.ProcessInfo{PaneID: pane, ShellPID: 10, ForegroundPGID: 10}, nil
		}
	}
	return herdr.ProcessInfo{}, fmt.Errorf("pane %s: %w", pane, errNoPane)
}

var errNoPane = errors.New("no such pane")

// pane shows a pane (and an agent in it, when agent is set) in the session.
func (f *fakeHerdrView) pane(ws, id, cwd, agent string, fg ...herdr.Process) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap.Panes = append(f.snap.Panes, herdr.Pane{ID: id, WorkspaceID: ws, Cwd: cwd, Agent: agent})
	if agent != "" {
		f.snap.Agents = append(f.snap.Agents, herdr.AgentInfo{PaneID: id, WorkspaceID: ws, Name: agent, Agent: agent, Cwd: cwd})
	}
	if len(fg) > 0 {
		if f.procs == nil {
			f.procs = map[string]herdr.ProcessInfo{}
		}
		f.procs[id] = herdr.ProcessInfo{PaneID: id, ShellPID: 10, ForegroundPGID: fg[0].PID, Foreground: fg}
	}
}

// clear empties the session.
func (f *fakeHerdrView) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap, f.procs = herdr.Snapshot{}, nil
}

// miseCall is one `mise -C <dir> exec -- env K=V… /bin/sh -c <script>`.
type miseCall struct {
	Dir, Script string
	Env         map[string]string
}

// spawnCall is one server the copies started.
type spawnCall struct {
	Dir, Command string
	Env          map[string]string
	PID          int
}

// newCopiesRig builds the rig for talkable/talkable with at most maxCopies copies
// (copy {n} is talkable.factory{n}, named factory{n}), and runs t in
// parallel.
func newCopiesRig(t *testing.T, maxCopies int) *copiesRig {
	t.Helper()
	storetest.Parallel(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &copiesRig{t: t, ctx: t.Context(), root: root, clock: storetest.NewClock(t0), dbs: &copyDBs{version: map[string]string{}},
		failSetup: map[string]int{}, blocks: map[string]chan struct{}{}, entered: make(chan string, 8), nextPID: 4242,
		factoryHerdr: &fakeHerdrView{}, yourHerdr: &fakeHerdrView{}}
	r.real = &execx.Real{BaseEnv: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"LC_ALL=C",
	}}
	r.buildRepos()

	r.st = storetest.Open(t, filepath.Join(root, "home", "agents.db"))
	r.st.Clock = r.clock.Now
	r.reg = registrytest.OpenWith(t, registry.Options{Clock: r.clock.Now})
	r.cfg = config.Defaults()
	r.cfg.Layout = paths.Layout{Home: filepath.Join(root, "home")}
	r.cfg.Factory.Repos = []config.FactoryRepo{{
		Repo: "talkable/talkable", Base: "master", MainClone: r.main,
		Copies: filepath.Join(root, "talkable.factory{n}"), CopyName: "factory{n}", MaxCopies: maxCopies,
		Setup: []string{"bin/worktree-setup"}, ResetDB: []string{"bin/rails db:schema:load"},
		Installs:  []config.FactoryInstall{{Lock: "Gemfile.lock", Run: "bundle install"}},
		Server:    "bin/worktree-run",
		Env:       map[string]string{"WT_BRANCH": "{copy}", "REDIS_DB_OFFSET": "{n*16}", "PORT": "30{n}0"},
		Databases: []string{"talkable_development__{slug}", "talkable_test__{slug}"},
	}}
	fake := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"mise"}, Fn: r.mise},
		{Prefix: []string{"redis-cli"}, Fn: r.redis},
	}}
	run := routeRunner{real: r.real, fake: fake}
	r.git = gitx.New(run)
	view := GuardPanes{Factory: r.factoryHerdr, Yours: r.yourHerdr}
	r.sm = slots.New(slots.Deps{Store: r.st, Run: run, Git: r.git, MySQL: r.dbs, Layout: r.cfg.Layout,
		Snapshot: view.Snapshot, ProcessInfo: view.ProcessInfo,
		FreeDiskBytes: func(string) (uint64, error) { return 100 << 30, nil }, Now: r.clock.Now,
		LookPath: func(file string) (string, error) { return "/fake/bin/" + file, nil }})
	r.c = r.newCopies()
	return r
}

// newCopies builds the rig's copies afresh over the same store and git, as
// a daemon restart does, with the rig's process seams.
func (r *copiesRig) newCopies() *copySet {
	c, ok := NewCopies(r.cfg, r.sm, r.st, r.git, routeRunner{real: r.real, fake: &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"mise"}, Fn: r.mise},
		{Prefix: []string{"redis-cli"}, Fn: r.redis},
	}}}, r.dbs, r.factoryHerdr.Snapshot, r.owner).(*copySet)
	if !ok {
		r.t.Fatal("NewCopies is not the copies' set")
	}
	c.processes = r.processTable
	c.kill = r.killGroup
	c.spawn = r.spawn
	c.sleep = func(context.Context, time.Duration) error { return nil }
	c.now = r.clock.Now
	return c
}

// routeRunner sends git to the real runner (the rig's temp repositories)
// and every other command to the fakes.
type routeRunner struct{ real, fake execx.Runner }

func (r routeRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	if c.Name == "git" {
		return r.real.Run(ctx, c)
	}
	return r.fake.Run(ctx, c)
}

func (r *copiesRig) gitIn(dir string, args ...string) string {
	r.t.Helper()
	res, err := r.real.Run(r.ctx, execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)})
	if err != nil {
		r.t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return res.Out()
}

func (r *copiesRig) write(path, content string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// buildRepos makes a bare origin with master (a README, a Gemfile.lock, the
// base schema and its migration) and the main clone of it.
func (r *copiesRig) buildRepos() {
	r.t.Helper()
	r.origin, r.main = filepath.Join(r.root, "origin.git"), filepath.Join(r.root, "talkable")
	work := filepath.Join(r.root, "work")
	for _, d := range []string{r.origin, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	r.gitIn(r.origin, "init", "--quiet", "--bare", "--template=", "-b", "master")
	r.gitIn(work, "init", "--quiet", "--template=", "-b", "master")
	r.write(filepath.Join(work, "README"), "hello\n")
	r.write(filepath.Join(work, ".gitignore"), "tmp/\n.mise.local.toml\n")
	r.write(filepath.Join(work, "Gemfile.lock"), "GEM v1\n")
	r.write(filepath.Join(work, "db", "schema.rb"), schemaRB(baseSchema))
	r.write(filepath.Join(work, "db", "migrate", baseMigration+"_init.rb"), "class Init; end\n")
	r.gitIn(work, "add", "-A")
	r.gitIn(work, "commit", "--quiet", "-m", "init")
	r.gitIn(work, "remote", "add", "origin", r.origin)
	r.gitIn(work, "push", "--quiet", "origin", "master")
	r.gitIn(r.root, "clone", "--quiet", "--template=", r.origin, r.main)
}

func schemaRB(v string) string {
	return "ActiveRecord::Schema[8.1].define(version: " + v + ") do\nend\n"
}

// owner is the task's owner row in the agents store, as the runtime's Owner
// keeps it: repository factory/talkable, number the task's id.
func (r *copiesRig) owner(ctx context.Context, t registry.Task) (store.PR, error) {
	repo, err := r.st.UpsertRepo(ctx, store.Repo{NodeID: "factory:talkable", Owner: "factory", Name: "talkable",
		WatchOwner: "factory", Mode: store.RepoModePerPR})
	if err != nil {
		return store.PR{}, err
	}
	id := strconv.FormatInt(t.ID, 10)
	up, err := r.st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "factory:task:" + id, Number: int(t.ID),
		URL: "file:///tasks/" + id, HeadSHA: "t" + id, GHState: store.GHOpen, InitialState: store.PRClaiming, Identity: "factory"})
	if err != nil {
		return store.PR{}, err
	}
	return up.PR, nil
}

// task creates an active bug task of talkable/talkable.
func (r *copiesRig) task(key string) registry.Task {
	r.t.Helper()
	t, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: key, Kind: "bug", Repo: "talkable/talkable",
		State: registry.TaskActive, Stage: "investigate"})
	if err != nil {
		r.t.Fatal(err)
	}
	return t
}

// claim claims a copy for t and fails the test on error.
func (r *copiesRig) claim(t registry.Task) Copy {
	r.t.Helper()
	r.clock.Add(time.Minute)
	c, err := r.c.Claim(r.ctx, t)
	if err != nil {
		r.t.Fatalf("Claim for %s: %v", TaskKey(t), err)
	}
	return c
}

// release releases t's copy and fails the test on error.
func (r *copiesRig) release(t registry.Task) {
	r.t.Helper()
	r.clock.Add(time.Minute)
	if err := r.c.Release(r.ctx, t, "released"); err != nil {
		r.t.Fatalf("Release for %s: %v", TaskKey(t), err)
	}
}

func (r *copiesRig) slot(name string) store.Slot {
	r.t.Helper()
	sl, err := r.st.SlotByName(r.ctx, name)
	if err != nil {
		r.t.Fatalf("SlotByName %s: %v", name, err)
	}
	return sl
}

func (r *copiesRig) slotNames() []string {
	r.t.Helper()
	list, err := r.st.ListSlots(r.ctx, store.SlotFilter{})
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, sl := range list {
		out = append(out, sl.Name)
	}
	return out
}

func (r *copiesRig) free() int {
	r.t.Helper()
	n, err := r.c.Free(r.ctx)
	if err != nil {
		r.t.Fatalf("Free: %v", err)
	}
	return n
}

// calls are the mise execs of script in dir ("" = any), in order.
func (r *copiesRig) calls(script, dir string) []miseCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []miseCall
	for _, c := range r.scripts {
		if c.Script == script && (dir == "" || c.Dir == dir) {
			out = append(out, c)
		}
	}
	return out
}

func (r *copiesRig) flushes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.flushed)
}

var versionRe = regexp.MustCompile(`define\(version: ([0-9_]+)\)`)

// schemaAt is the version dir's db/schema.rb declares, as schema_migrations
// keeps it (no underscores).
func schemaAt(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "db", "schema.rb"))
	if err != nil {
		return ""
	}
	if m := versionRe.FindSubmatch(b); m != nil {
		return strings.ReplaceAll(string(m[1]), "_", "")
	}
	return ""
}

// mise simulates `mise trust` and `mise exec` of the copies' commands: the
// setup writes the copy's slug marker and creates its databases at the
// checkout's schema, reset_db loads that schema again.
func (r *copiesRig) mise(c execx.Cmd) (execx.Result, error) {
	a := c.Args
	if len(a) >= 3 && a[0] == "-C" && a[2] == "trust" {
		return execx.Result{}, nil
	}
	if len(a) < 7 || a[0] != "-C" || a[2] != "exec" || a[3] != "--" || a[4] != "env" {
		return execx.Result{}, fmt.Errorf("unexpected mise call %v", a)
	}
	mc := miseCall{Dir: a[1], Env: map[string]string{}}
	i := 5
	for ; i < len(a) && strings.Contains(a[i], "=") && !strings.HasPrefix(a[i], "/"); i++ {
		k, v, _ := strings.Cut(a[i], "=")
		mc.Env[k] = v
	}
	if len(a)-i != 3 || a[i] != "/bin/sh" || a[i+1] != "-c" {
		return execx.Result{}, fmt.Errorf("unexpected mise call %v", a)
	}
	mc.Script = a[i+2]
	r.mu.Lock()
	r.scripts = append(r.scripts, mc)
	slot := mc.Env["WT_BRANCH"]
	fail := mc.Script == "bin/worktree-setup" && r.failSetup[slot] > 0
	if fail {
		r.failSetup[slot]--
	}
	block := r.blocks[mc.Script+" "+slot]
	r.mu.Unlock()
	if block != nil {
		r.entered <- mc.Script + " " + slot
		select {
		case <-block:
		case <-time.After(time.Minute):
			return execx.Result{}, errors.New("blocked for a minute: the test never let it go")
		}
	}
	if fail {
		return execx.Result{Code: 1, Stderr: []byte("setup failed")}, &execx.ExitError{Cmd: c, Code: 1, Stderr: "setup failed"}
	}
	dbs := []string{"talkable_development__" + slot, "talkable_test__" + slot}
	switch mc.Script {
	case "bin/worktree-setup":
		r.write(filepath.Join(mc.Dir, "tmp", ".worktree-db-slug"), slot)
		r.dbs.set(schemaAt(mc.Dir), dbs...)
	case "bin/rails db:schema:load":
		r.dbs.set(schemaAt(mc.Dir), dbs...)
	}
	return execx.Result{}, nil
}

// redis records `redis-cli -n <db> FLUSHDB`.
func (r *copiesRig) redis(c execx.Cmd) (execx.Result, error) {
	if len(c.Args) != 3 || c.Args[0] != "-n" || c.Args[2] != "FLUSHDB" {
		return execx.Result{}, fmt.Errorf("unexpected redis-cli call %v", c.Args)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failRedis > 0 {
		r.failRedis--
		return execx.Result{Code: 1, Stderr: []byte("Could not connect to Redis")},
			&execx.ExitError{Cmd: c, Code: 1, Stderr: "Could not connect to Redis"}
	}
	r.flushed = append(r.flushed, c.Args[1])
	return execx.Result{Stdout: []byte("OK\n")}, nil
}

func (r *copiesRig) processTable(context.Context) ([]execx.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.table), nil
}

// killGroup records the signal; a SIGTERM or SIGKILL ends the group's
// processes in the table.
func (r *copiesRig) killGroup(pgid int, sig syscall.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kills = append(r.kills, fmt.Sprintf("%d %s", pgid, sig))
	r.table = slices.DeleteFunc(r.table, func(p execx.Process) bool { return p.PGID == pgid })
	return nil
}

// spawn starts a fake server: a new pid in the table, started now.
func (r *copiesRig) spawn(_ context.Context, dir string, env map[string]string, command, _ string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pid := r.nextPID
	r.nextPID++
	r.spawned = append(r.spawned, spawnCall{Dir: dir, Command: command, Env: env, PID: pid})
	r.table = append(r.table, execx.Process{PID: pid, PPID: 1, PGID: pid, Start: lstart(r.clock.Now()), Name: "ruby"})
	return pid, nil
}

func lstart(t time.Time) string { return t.Format("Mon Jan _2 15:04:05 2006") }

// copyDBs is the MySQL server of the copies' databases: each database's
// MAX(schema_migrations.version).
type copyDBs struct {
	mu      sync.Mutex
	version map[string]string
}

func (d *copyDBs) set(version string, names ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, n := range names {
		d.version[n] = version
	}
}

func (d *copyDBs) get(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.version[name]
}

func (d *copyDBs) ListPrefixed(_ context.Context, prefixes []string) ([]mysqlx.Database, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []mysqlx.Database
	for n := range d.version {
		slug, ok := mysqlx.Slug(n)
		if ok && slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(n, p) }) {
			out = append(out, mysqlx.Database{Name: n, Slug: slug, SizeMB: 1})
		}
	}
	slices.SortFunc(out, func(a, b mysqlx.Database) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (d *copyDBs) SchemaMigrationsMax(_ context.Context, db string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.version[db]
	if !ok {
		return "", fmt.Errorf("fake: %s: %w", db, mysqlx.ErrNotFound)
	}
	return v, nil
}

func (d *copyDBs) DropAll(_ context.Context, names []string, _ mysqlx.Guard) []mysqlx.DropResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []mysqlx.DropResult
	for _, n := range names {
		delete(d.version, n)
		out = append(out, mysqlx.DropResult{Name: n})
	}
	return out
}

// No copy exists before a task needs one, and a claim adds one only while
// every copy is busy, up to max_copies: the fourth task of three copies
// gets none, and nothing is provisioned past the third.
func TestCopiesProvisionOnDemandUpToMaxCopies(t *testing.T) {
	r := newCopiesRig(t, 3)
	if names := r.slotNames(); len(names) != 0 {
		t.Fatalf("copies %v before any claim", names)
	}
	if n := r.free(); n != 3 {
		t.Fatalf("Free = %d before any claim, want the 3 a claim may add", n)
	}
	for i := 1; i <= 3; i++ {
		c := r.claim(r.task("PR-" + strconv.Itoa(i)))
		if want := "factory" + strconv.Itoa(i); c.Name != want || c.Number != i ||
			c.Path != filepath.Join(r.root, "talkable.factory"+strconv.Itoa(i)) {
			t.Fatalf("claim %d = %+v, want copy %s", i, c, want)
		}
		if got := len(r.calls("bin/worktree-setup", "")); got != i {
			t.Fatalf("after claim %d the setup ran %d times, want %d: one copy per claim", i, got, i)
		}
		if b := r.gitIn(c.Path, "branch", "--show-current"); b != c.Name {
			t.Fatalf("copy %s is on %q, want its placeholder branch", c.Name, b)
		}
		if n := r.free(); n != 3-i {
			t.Fatalf("Free = %d after claim %d, want %d", n, i, 3-i)
		}
	}
	if _, err := r.c.Claim(r.ctx, r.task("PR-4")); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("the fourth claim of three copies = %v, want ErrNoCopy", err)
	}
	if names := r.slotNames(); !slices.Equal(names, []string{"factory1", "factory2", "factory3"}) {
		t.Fatalf("copies %v, want three", names)
	}
	if got := len(r.calls("bin/worktree-setup", "")); got != 3 {
		t.Fatalf("the setup ran %d times, want 3", got)
	}
}

// A task no copy is free for waits at no cost (ErrHeld) with the copies'
// reason, "copy: all 3 busy", and the engine records that wait on the task.
func TestCopiesQueueATaskWhenNoCopyIsFree(t *testing.T) {
	r := newCopiesRig(t, 3)
	for i := 1; i <= 3; i++ {
		r.claim(r.task("PR-" + strconv.Itoa(i)))
	}
	_, err := r.c.Claim(r.ctx, r.task("PR-4"))
	if !errors.Is(err, ErrNoCopy) || !errors.Is(err, ErrHeld) || err.Error() != "copy: all 3 busy" {
		t.Fatalf("Claim = %v, want ErrNoCopy, held, saying copy: all 3 busy", err)
	}

	h := newHarness(t)
	h.copies.claimErr = err
	task := h.task("PR-4", registry.TaskActive, "investigate")
	h.claimNow(task) // the claim runs off the loop
	if _, err := h.e.claimCopy(h.ctx, h.reload(task.ID)); !errors.Is(err, ErrHeld) {
		t.Fatalf("claimCopy = %v, want a hold that costs the task nothing", err)
	}
	if got := h.reload(task.ID); got.WaitsWho != "copy" || got.WaitsWhat != "all 3 busy" || got.Copy != "" {
		t.Fatalf("task waits %q: %q with copy %q, want copy: all 3 busy and none", got.WaitsWho, got.WaitsWhat, got.Copy)
	}
	h.copies.mu.Lock()
	h.copies.claimErr = nil
	h.copies.claimed = Copy{Name: "factory2", Path: "/copies/factory2", Number: 2}
	h.copies.mu.Unlock()
	h.clock.Add(time.Minute) // a waiting claim tries again a minute later
	h.claimNow(h.reload(task.ID))
	if c, err := h.e.claimCopy(h.ctx, h.reload(task.ID)); err != nil || c.Name != "factory2" {
		t.Fatalf("claimCopy = %+v, %v", c, err)
	}
	if got := h.reload(task.ID); got.WaitsWho != "" || got.WaitsWhat != "" || got.Copy != "factory2" {
		t.Fatalf("after a claim the task waits %q: %q with copy %q, want no wait and factory2", got.WaitsWho, got.WaitsWhat, got.Copy)
	}
}

// Copy {n} gets Redis databases of its own, 16 from n*16, through
// REDIS_DB_OFFSET when the repository's env names it: the setup and the
// claim see it, and {copy} and {n} in the env render the copy's name and
// number. A repository whose env does not name it gets no offset and no
// flush.
func TestCopiesSetARedisOffsetPerCopy(t *testing.T) {
	r := newCopiesRig(t, 3)
	for i := 1; i <= 3; i++ {
		c := r.claim(r.task("PR-" + strconv.Itoa(i)))
		want := map[string]string{"WT_BRANCH": "factory" + strconv.Itoa(i), "REDIS_DB_OFFSET": strconv.Itoa(16 * i),
			"PORT": "30" + strconv.Itoa(i) + "0"}
		for k, v := range want {
			if c.Env[k] != v {
				t.Errorf("copy %s: %s = %q, want %q", c.Name, k, c.Env[k], v)
			}
		}
		if c.Port != 3000+10*i {
			t.Errorf("copy %s: port %d, want %d", c.Name, c.Port, 3000+10*i)
		}
		setup := r.calls("bin/worktree-setup", c.Path)
		if len(setup) != 1 || setup[0].Env["REDIS_DB_OFFSET"] != want["REDIS_DB_OFFSET"] || setup[0].Env["WT_BRANCH"] != c.Name {
			t.Errorf("copy %s's setup ran with %+v, want its own name and offset", c.Name, setup)
		}
	}

	// The env names the variable: the copy's offset it is, whatever its value.
	fixed := newCopiesRig(t, 2)
	fixed.cfg.Factory.Repos[0].Env = map[string]string{"WT_BRANCH": "{copy}", "REDIS_DB_OFFSET": "0"}
	fixed.claim(fixed.task("PR-8"))
	if c := fixed.claim(fixed.task("PR-9")); c.Env["REDIS_DB_OFFSET"] != "32" {
		t.Fatalf("copy %s: REDIS_DB_OFFSET = %q with the env's 0, want its own 32", c.Name, c.Env["REDIS_DB_OFFSET"])
	}

	plain := newCopiesRig(t, 1)
	plain.cfg.Factory.Repos[0].Env = map[string]string{"WT_BRANCH": "{copy}"}
	c := plain.claim(plain.task("PR-9"))
	if _, ok := c.Env["REDIS_DB_OFFSET"]; ok {
		t.Fatalf("copy env %v: an offset the repository's env does not name", c.Env)
	}
	if f := plain.flushes(); len(f) != 0 {
		t.Fatalf("flushed %v without a Redis offset", f)
	}
}

// A claim takes the copy the task used last when it is free, before the
// copy that waited longest.
func TestClaimPrefersTheCopyTheTaskUsedLast(t *testing.T) {
	r := newCopiesRig(t, 3)
	a, b := r.task("PR-1"), r.task("PR-2")
	if c := r.claim(b); c.Name != "factory1" {
		t.Fatalf("b claimed %s, want factory1", c.Name)
	}
	if c := r.claim(a); c.Name != "factory2" {
		t.Fatalf("a claimed %s, want factory2", c.Name)
	}
	r.release(b)
	r.release(a)
	if c := r.claim(a); c.Name != "factory2" {
		t.Fatalf("a claimed %s again, want factory2, the copy it used last (factory1 waited longer)", c.Name)
	}
	if c := r.claim(b); c.Name != "factory1" {
		t.Fatalf("b claimed %s again, want factory1", c.Name)
	}
}

// A release a crash cut short keeps the task's copy in releasing, where no
// claim takes it and no other copy can be the task's: the task's next claim
// ends that release first and gets the copy back.
func TestClaimEndsTheReleaseACrashCutShort(t *testing.T) {
	r := newCopiesRig(t, 2)
	a := r.task("PR-1")
	c := r.claim(a)
	sl := r.slot(c.Name)
	if err := r.st.TransitionSlot(r.ctx, sl.ID, []string{store.SlotClaimed}, store.SlotReleasing, nil); err != nil {
		t.Fatal(err)
	}
	if got := r.claim(a); got.Name != c.Name {
		t.Fatalf("claimed %s, want %s back once its release ended", got.Name, c.Name)
	}
	if sl := r.slot(c.Name); sl.State != store.SlotClaimed {
		t.Fatalf("%s = %s, want claimed again", c.Name, sl.State)
	}
	if names := r.slotNames(); len(names) != 1 {
		t.Fatalf("copies %v, want no second one", names)
	}
}

// A copy whose development database holds a migration the task's code does
// not have (another task migrated it) is reset with reset_db before the
// task gets it; a database at a version the code has is kept.
func TestClaimResetsADatabaseThatHoldsAnotherTasksMigrations(t *testing.T) {
	r := newCopiesRig(t, 1)
	a := r.task("PR-1")
	c := r.claim(a)
	r.release(a)
	dev := "talkable_development__factory1"
	if v := r.dbs.get(dev); v != baseMigration {
		t.Fatalf("precondition: %s at %q, want the base %s", dev, v, baseMigration)
	}
	r.dbs.set("20261101000000", dev) // the migration of another task's branch

	r.claim(r.task("PR-2"))
	resets := r.calls("bin/rails db:schema:load", c.Path)
	if len(resets) != 1 || resets[0].Env["WT_BRANCH"] != "factory1" || resets[0].Env["REDIS_DB_OFFSET"] != "16" {
		t.Fatalf("reset_db ran %+v, want once in factory1 with its env", resets)
	}
	if v := r.dbs.get(dev); v != baseMigration {
		t.Fatalf("%s at %q after the claim, want the base schema %s", dev, v, baseMigration)
	}

	r.release(r.reload(2))
	r.claim(r.task("PR-3"))
	if resets := r.calls("bin/rails db:schema:load", c.Path); len(resets) != 1 {
		t.Fatalf("reset_db ran %d times, want no second run for a database at the code's version", len(resets))
	}
}

// reload is task id as the registry has it.
func (r *copiesRig) reload(id int64) registry.Task {
	r.t.Helper()
	t, err := r.reg.TaskByID(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return t
}

// A claim runs an installs entry when its lockfile differs from the one the
// copy last installed: once for a copy that never installed, never for the
// same lockfile, and again for a task whose branch changes it. A task that
// has a branch gets its copy on that branch.
func TestClaimInstallsOnlyWhenALockfileDiffers(t *testing.T) {
	r := newCopiesRig(t, 1)
	a := r.task("PR-1")
	c := r.claim(a)
	if got := len(r.calls("bundle install", c.Path)); got != 1 {
		t.Fatalf("bundle install ran %d times in a copy that never installed, want 1", got)
	}
	r.release(a)
	b := r.task("PR-2")
	r.claim(b)
	if got := len(r.calls("bundle install", c.Path)); got != 1 {
		t.Fatalf("bundle install ran %d times, want no second run for the same Gemfile.lock", got)
	}
	r.release(b)

	r.gitIn(r.main, "switch", "--quiet", "-c", "task-PR-3")
	r.write(filepath.Join(r.main, "Gemfile.lock"), "GEM v2\n")
	r.gitIn(r.main, "commit", "--quiet", "-am", "a gem upgrade")
	r.gitIn(r.main, "switch", "--quiet", "master")
	d := r.task("PR-3")
	if err := r.reg.TransitionTask(r.ctx, d.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("branch", "task-PR-3") }); err != nil {
		t.Fatal(err)
	}
	r.claim(r.reload(d.ID))
	if b := r.gitIn(c.Path, "branch", "--show-current"); b != "task-PR-3" {
		t.Fatalf("the copy is on %q, want the task's branch", b)
	}
	if got := len(r.calls("bundle install", c.Path)); got != 2 {
		t.Fatalf("bundle install ran %d times, want a second run for the branch's Gemfile.lock", got)
	}
}

// A task that has a branch name and no branch yet gets it at its claim,
// from origin/<base>, as a step of the claim with its guard and events: the
// copy is on the task's branch when the claim hands it over. A later claim
// switches to the branch it has.
func TestClaimCreatesTheTasksBranchFromTheBase(t *testing.T) {
	r := newCopiesRig(t, 1)
	task := r.task("PR-12345")
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, "", func(u *registry.TaskUpdate) {
		u.Set("branch", "zhuravel-PR-12345-credit-expiry")
	}); err != nil {
		t.Fatal(err)
	}
	c := r.claim(r.reload(task.ID))
	if b := r.gitIn(c.Path, "branch", "--show-current"); b != "zhuravel-PR-12345-credit-expiry" {
		t.Fatalf("the copy is on %q, want the task's new branch", b)
	}
	if head, base := r.gitIn(c.Path, "rev-parse", "HEAD"), r.gitIn(r.main, "rev-parse", "origin/master"); head != base {
		t.Fatalf("the branch starts at %s, want origin/master %s", head, base)
	}
	if up := r.gitIn(c.Path, "for-each-ref", "--format=%(upstream)", "refs/heads/zhuravel-PR-12345-credit-expiry"); up != "" {
		t.Fatalf("the branch tracks %q, want nothing", up)
	}
	var created bool
	evs, err := r.st.EventsMatching(r.ctx, []store.SubjectMatch{{Prefix: "slot:factory1:claim:"}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if store.Deref(ev.Step) == "create_branch" && store.Deref(ev.Phase) == store.PhaseOK {
			created = true
		}
	}
	if !created {
		t.Fatalf("no ok step create_branch among %+v", evs)
	}
	r.release(r.reload(task.ID))
	c = r.claim(r.reload(task.ID))
	if b := r.gitIn(c.Path, "branch", "--show-current"); b != "zhuravel-PR-12345-credit-expiry" {
		t.Fatalf("a second claim left the copy on %q", b)
	}
}

// A claim never writes over your ignored file where the task's branch has a
// file: a first claim's new branch (create_branch) and a later claim's
// switch to the branch it has (switch_branch) are refused, the file stays,
// and the claim's error names it as git's refusal (refusal), which the
// engine turns into a wait on you.
func TestAClaimsSwitchOverAnIgnoredFileIsRefusedNamingIt(t *testing.T) {
	seed := func(r *copiesRig, dir string) {
		r.write(filepath.Join(dir, "tmp", "seed.txt"), "seed\n")
		r.gitIn(dir, "add", "-f", "--", "tmp/seed.txt")
		r.gitIn(dir, "commit", "--quiet", "-m", "tracks a seed")
	}
	named := func(r *copiesRig, task registry.Task, branch string) registry.Task {
		if err := r.reg.TransitionTask(r.ctx, task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("branch", branch) }); err != nil {
			r.t.Fatal(err)
		}
		return r.reload(task.ID)
	}
	refused := func(r *copiesRig, task registry.Task, copyPath string) {
		r.t.Helper()
		r.clock.Add(time.Minute)
		_, err := r.c.Claim(r.ctx, task)
		if files, _, ok := refusal(err); !ok || !slices.Contains(files, "tmp/seed.txt") {
			r.t.Fatalf("Claim = %v; want git's refusal naming tmp/seed.txt", err)
		}
		b, rerr := os.ReadFile(filepath.Join(copyPath, "tmp", "seed.txt"))
		if rerr != nil || string(b) != "your notes\n" {
			r.t.Fatalf("tmp/seed.txt = %q, %v; want your ignored file kept", b, rerr)
		}
	}
	t.Run("a first claim's new branch", func(t *testing.T) {
		r := newCopiesRig(t, 1)
		a := named(r, r.task("PR-12425"), "zhuravel-PR-12425-a")
		c := r.claim(a)
		r.release(r.reload(a.ID))
		work := filepath.Join(r.root, "work")
		seed(r, work)
		r.gitIn(work, "push", "--quiet", "origin", "master")
		r.gitIn(r.main, "fetch", "--quiet", "origin")
		r.write(filepath.Join(c.Path, "tmp", "seed.txt"), "your notes\n")
		refused(r, named(r, r.task("PR-12426"), "zhuravel-PR-12426-b"), c.Path)
	})
	t.Run("a later claim's switch", func(t *testing.T) {
		r := newCopiesRig(t, 1)
		a := named(r, r.task("PR-12427"), "zhuravel-PR-12427-a")
		c := r.claim(a)
		seed(r, c.Path)
		r.release(r.reload(a.ID))
		b := named(r, r.task("PR-12428"), "zhuravel-PR-12428-b")
		r.claim(b)
		r.release(r.reload(b.ID))
		r.write(filepath.Join(c.Path, "tmp", "seed.txt"), "your notes\n")
		refused(r, r.reload(a.ID), c.Path)
	})
}

// A claim flushes the copy's 16 Redis databases from its offset, once per
// claim: a task that claims the copy it holds again keeps its data.
func TestClaimFlushesTheCopysRedisDatabases(t *testing.T) {
	r := newCopiesRig(t, 2)
	r.claim(r.task("PR-1"))
	b := r.task("PR-2")
	r.claim(b)
	var want []string
	for db := 16; db < 48; db++ {
		want = append(want, strconv.Itoa(db))
	}
	if got := r.flushes(); !slices.Equal(got, want) {
		t.Fatalf("flushed %v, want 16-31 for factory1 and 32-47 for factory2", got)
	}
	r.claim(b)
	if got := r.flushes(); len(got) != 32 {
		t.Fatalf("flushed %d databases, want no flush for a claim of the copy the task holds", len(got))
	}
	r.release(b)
	r.claim(r.task("PR-3"))
	if got := r.flushes(); len(got) != 48 || got[32] != "32" || got[47] != "47" {
		t.Fatalf("flushed %v, want factory2's 32-47 again for its next task", got)
	}
}

// A copy whose setup fails runs it once more; a second failure marks the
// copy broken (a slot state and an event) and the task claims another. A
// broken copy is claimed by nobody until the operator repairs it.
func TestASetupFailureRunsOnceMoreThenMarksTheCopyBroken(t *testing.T) {
	once := newCopiesRig(t, 1)
	once.failSetup["factory1"] = 1
	if c := once.claim(once.task("PR-1")); c.Name != "factory1" {
		t.Fatalf("claimed %s after one failed setup, want factory1 on its second run", c.Name)
	}
	if got := len(once.calls("bin/worktree-setup", "")); got != 2 {
		t.Fatalf("the setup ran %d times, want 2", got)
	}

	r := newCopiesRig(t, 3)
	r.failSetup["factory1"] = 2
	if c := r.claim(r.task("PR-1")); c.Name != "factory2" {
		t.Fatalf("claimed %s, want factory2 after factory1 broke", c.Name)
	}
	one := filepath.Join(r.root, "talkable.factory1")
	if got := len(r.calls("bin/worktree-setup", one)); got != 2 {
		t.Fatalf("factory1's setup ran %d times, want 2", got)
	}
	if sl := r.slot("factory1"); sl.State != store.SlotBroken || !strings.Contains(store.Deref(sl.LastError), "setup failed") {
		t.Fatalf("factory1 = %s (%s), want broken with the setup's failure", sl.State, store.Deref(sl.LastError))
	}
	evs, err := r.st.EventsBySubject(r.ctx, "slot:factory1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(evs, func(e store.Event) bool { return e.Kind == "factory.copy_broken" && e.Level == "warn" }) {
		t.Fatalf("events of factory1 %+v, want factory.copy_broken", evs)
	}

	if c := r.claim(r.task("PR-2")); c.Name != "factory3" {
		t.Fatalf("the next task claimed %s, want factory3: a broken copy is never claimed", c.Name)
	}
	c3 := r.task("PR-3")
	if _, err := r.c.Claim(r.ctx, c3); !errors.Is(err, ErrNoCopy) || err.Error() != "copy: 2 busy, 1 broken" {
		t.Fatalf("Claim with two busy and one broken = %v, want copy: 2 busy, 1 broken", err)
	}
	if err := r.sm.Repair(r.ctx, r.slot("factory1"), r.c.pool(r.cfg.Factory.Repos[0], 1)); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if c := r.claim(c3); c.Name != "factory1" {
		t.Fatalf("claimed %s after the repair, want factory1", c.Name)
	}
}

// A task leaves its copy with a handoff.md in its directory (the branch, the
// head, the server, what the next claimer needs), and its branch stays: its
// commits, pushed or not, are the factory's own on a branch of its own, so
// the guard holds nothing, and the copy goes back to its placeholder.
// Reserved names the copies the tasks' owner rows hold and for which task,
// from the claim to the release, so a release that failed can be told from
// the stores after a restart.
func TestReservedNamesTheCopiesTasksHold(t *testing.T) {
	r := newCopiesRig(t, 2)
	a, b := r.task("PR-12348"), r.task("PR-12349")
	r.claim(a)
	r.claim(b)
	got, err := r.c.Reserved(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Reservation{{TaskID: a.ID, Copy: "factory1"}, {TaskID: b.ID, Copy: "factory2"}}
	if !slices.Equal(got, want) {
		t.Fatalf("Reserved = %+v, want %+v", got, want)
	}
	r.release(a)
	if got, _ := r.c.Reserved(r.ctx); !slices.Equal(got, want[1:]) {
		t.Fatalf("after a release Reserved = %+v, want %+v", got, want[1:])
	}
}

func TestReleaseWritesHandoffAndKeepsTheBranch(t *testing.T) {
	r := newCopiesRig(t, 1)
	task := r.task("PR-12345")
	c := r.claim(task)
	if err := r.git.SwitchCreate(r.ctx, c.Path, "task-PR-12345", "origin/master"); err != nil {
		t.Fatal(err)
	}
	r.write(filepath.Join(c.Path, "app", "fix.rb"), "fixed = true\n")
	r.gitIn(c.Path, "add", "app/fix.rb")
	r.gitIn(c.Path, "commit", "--quiet", "-m", "the fix")
	head := r.gitIn(c.Path, "rev-parse", "HEAD")
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("branch", "task-PR-12345") }); err != nil {
		t.Fatal(err)
	}

	if err := r.c.Release(r.ctx, r.reload(task.ID), "stopped"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	sl := r.slot("factory1")
	if sl.State != store.SlotFree || sl.HoldReason != nil || sl.PRID != nil {
		t.Fatalf("factory1 = %s, hold %q, pr %v; want free", sl.State, store.Deref(sl.HoldReason), sl.PRID)
	}
	if got := r.gitIn(r.main, "rev-parse", "refs/heads/task-PR-12345"); got != head {
		t.Fatalf("the task's branch is at %s, want its head %s", got, head)
	}
	if b := r.gitIn(c.Path, "branch", "--show-current"); b != "factory1" {
		t.Fatalf("the copy is on %q, want its placeholder", b)
	}
	b, err := os.ReadFile(filepath.Join(r.cfg.Layout.FactoryTaskDir(task.ID), "handoff.md"))
	if err != nil {
		t.Fatalf("handoff.md: %v", err)
	}
	for _, want := range []string{"factory1", "task-PR-12345", head, "stopped", "server: not running", "git switch task-PR-12345"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("handoff.md does not say %q:\n%s", want, b)
		}
	}
}

// The guard still protects work on no branch: a copy left on a detached
// commit nothing else has is held, not reset.
func TestReleaseHoldsACopyWhoseCommitsAreOnNoBranch(t *testing.T) {
	r := newCopiesRig(t, 1)
	task := r.task("PR-1")
	c := r.claim(task)
	r.gitIn(c.Path, "switch", "--quiet", "--detach")
	r.write(filepath.Join(c.Path, "app", "lost.rb"), "x\n")
	r.gitIn(c.Path, "add", "app/lost.rb")
	r.gitIn(c.Path, "commit", "--quiet", "-m", "on no branch")
	err := r.c.Release(r.ctx, task, "stopped")
	if h, ok := slots.AsHold(err); !ok || h.Reason != slots.HoldUnpushed {
		t.Fatalf("Release = %v, want the guard's unpushed hold", err)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.Layout.FactoryTaskDir(task.ID), "handoff.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused release wrote handoff.md: %v", err)
	}
}

// A release is refused while one of the factory's sessions works in the
// copy (ErrPaneInCopy), and the copy stays the task's.
func TestReleaseRefusesWhileAPaneRunsInTheCopy(t *testing.T) {
	r := newCopiesRig(t, 1)
	task := r.task("PR-1")
	c := r.claim(task)
	pr, err := r.owner(r.ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.st.CreateSession(r.ctx, store.Session{PRID: pr.ID, Role: "fx-writer", State: store.SessionLive,
		Cwd: new(filepath.Join(c.Path, "app"))})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.Release(r.ctx, task, "stopped"); !errors.Is(err, ErrPaneInCopy) || !strings.Contains(err.Error(), "fx-writer") {
		t.Fatalf("Release with a pane in the copy = %v, want ErrPaneInCopy naming the role", err)
	}
	if sl := r.slot("factory1"); sl.State != store.SlotClaimed || sl.PRID == nil || *sl.PRID != pr.ID {
		t.Fatalf("factory1 = %s for %v, want still the task's", sl.State, sl.PRID)
	}
	if err := r.st.TransitionSession(r.ctx, s.ID, nil, store.SessionClosed, nil); err != nil {
		t.Fatal(err)
	}
	r.release(task)
	if sl := r.slot("factory1"); sl.State != store.SlotFree {
		t.Fatalf("factory1 = %s after the pane closed, want free", sl.State)
	}
}
