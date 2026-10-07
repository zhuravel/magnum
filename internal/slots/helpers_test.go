package slots

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// fakeMySQL is an in-memory stand-in for *mysqlx.Client.
type fakeMySQL struct {
	mu        sync.Mutex
	dbs       map[string]bool
	versions  map[string]string // db -> MAX(schema_migrations.version)
	drops     []string          // names actually dropped
	guards    []mysqlx.Guard    // guard passed to each DropAll
	listCalls int
}

func newFakeMySQL() *fakeMySQL {
	return &fakeMySQL{dbs: map[string]bool{}, versions: map[string]string{}}
}

func (f *fakeMySQL) add(version string, names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range names {
		f.dbs[n] = true
		f.versions[n] = version
	}
}

func (f *fakeMySQL) remove(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range names {
		delete(f.dbs, n)
		delete(f.versions, n)
	}
}

func (f *fakeMySQL) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dbs[name]
}

func (f *fakeMySQL) ListPrefixed(ctx context.Context, prefixes []string) ([]mysqlx.Database, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	var out []mysqlx.Database
	for n := range f.dbs {
		slug, ok := mysqlx.Slug(n)
		if !ok || !hasDBPrefix(n, prefixes) {
			continue
		}
		out = append(out, mysqlx.Database{Name: n, Slug: slug, SizeMB: 12.5})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeMySQL) SchemaMigrationsMax(ctx context.Context, db string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dbs[db] {
		return "", fmt.Errorf("fake: %s: %w", db, mysqlx.ErrNotFound)
	}
	return f.versions[db], nil
}

func (f *fakeMySQL) DropAll(ctx context.Context, names []string, g mysqlx.Guard) []mysqlx.DropResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.guards = append(f.guards, g)
	var out []mysqlx.DropResult
	for _, n := range names {
		if err := g.Check(n); err != nil {
			out = append(out, mysqlx.DropResult{Name: n, Err: err})
			continue
		}
		delete(f.dbs, n)
		f.drops = append(f.drops, n)
		out = append(out, mysqlx.DropResult{Name: n})
	}
	return out
}

// recRunner sends git to a real runner (temp repos) and everything else to a
// Fake, recording every call in order.
type recRunner struct {
	real execx.Runner
	fake *execx.Fake
	// fakeGit routes a git command to the Fake instead (e.g. clone).
	fakeGit func(c execx.Cmd) bool

	mu    sync.Mutex
	calls []execx.Cmd
}

func (r *recRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	if c.Name == "git" && (r.fakeGit == nil || !r.fakeGit(c)) {
		ctx, cancel := context.WithTimeout(ctx, gitGuard)
		defer cancel()
		return r.real.Run(ctx, c)
	}
	return r.fake.Run(ctx, c)
}

// gitCalls returns recorded git calls whose args (after -C dir) start with sub.
func (r *recRunner) gitCalls(sub ...string) []execx.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []execx.Cmd
	for _, c := range r.calls {
		if c.Name != "git" {
			continue
		}
		args := c.Args
		if len(args) >= 2 && args[0] == "-C" {
			args = args[2:]
		}
		if len(args) >= len(sub) && slices.Equal(args[:len(sub)], sub) {
			out = append(out, c)
		}
	}
	return out
}

func (r *recRunner) reset() {
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
}

// miseCall is a parsed `mise -C <dir> exec -- env K=V… /bin/sh -c <cmd>`.
type miseCall struct {
	Cmd    execx.Cmd
	Dir    string
	Env    map[string]string
	Script string
}

func parseMiseExec(c execx.Cmd) (miseCall, bool) {
	a := c.Args
	if c.Name != "mise" || len(a) < 7 || a[0] != "-C" || a[2] != "exec" || a[3] != "--" || a[4] != "env" {
		return miseCall{}, false
	}
	mc := miseCall{Cmd: c, Dir: a[1], Env: map[string]string{}}
	i := 5
	for ; i < len(a) && strings.Contains(a[i], "=") && !strings.HasPrefix(a[i], "/"); i++ {
		k, v, _ := strings.Cut(a[i], "=")
		mc.Env[k] = v
	}
	if len(a)-i != 3 || a[i] != "/bin/sh" || a[i+1] != "-c" {
		return miseCall{}, false
	}
	mc.Script = a[i+2]
	return mc, true
}

// harness wires a Manager to a temp store, temp git repos, a Fake for mise and
// a fake MySQL.
type harness struct {
	t      *testing.T
	ctx    context.Context
	root   string
	origin string // bare repo (shared fixture, read-only)
	main   string // main clone (pool main_clone), the test's own copy
	st     *store.Store
	now    time.Time
	fake   *execx.Fake
	run    *recRunner
	my     *fakeMySQL
	layout paths.Layout
	pool   config.Pool
	m      *Manager

	snap    herdr.Snapshot
	snapErr error
	procs   map[string]herdr.ProcessInfo

	mu         sync.Mutex
	scripts    []miseCall     // every mise exec, in order
	failScript map[string]int // script -> remaining failures
	setupSlug  func(slot string) string
	freeDisk   uint64

	shaMaster string
	shaPR7    string // touches db/schema.rb and Gemfile.lock
	shaPR8    string // README only
}

const (
	schemaV1 = "2026_09_14_085954"
	schemaV2 = "2026_09_20_000000"
)

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRun(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func schemaRB(v string) string {
	return "ActiveRecord::Schema[8.1].define(version: " + v + ") do\nend\n"
}

// newHarness marks the test parallel: each harness owns its temp dirs, main
// clone, store and fakes, and only reads the shared fixtures (TestMain pins
// the process-wide git environment, so tests need no t.Setenv).
func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Parallel()
	fx := fixtures(t).talkable
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: context.Background(), root: root, now: t0, my: newFakeMySQL(),
		procs: map[string]herdr.ProcessInfo{}, failScript: map[string]int{}, freeDisk: 100 << 30}
	h.origin, h.shaMaster, h.shaPR7, h.shaPR8 = fx.origin, fx.master, fx.pr7, fx.pr8
	// A clone of origin with .mise.local.toml (gitignored) beside the checkout.
	h.main = filepath.Join(root, "talkable")
	copyRepo(t, fx.main, h.main)

	st, err := store.Open(filepath.Join(root, "home", "state", "magnum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Clock = func() time.Time { return h.now }
	h.st = st

	h.layout = paths.Layout{Home: filepath.Join(root, "home")}
	h.pool = config.Pool{
		Repo: "talkable/talkable", MainClone: h.main, SlotName: "review{n}",
		SlotPath: filepath.Join(root, "talkable.review{n}"), Base: "master", Min: 1, Max: 4,
		MinFreeDiskGB: 15,
		CopyFiles:     []string{".mise.local.toml"}, StripEnv: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"},
		Setup: []string{"bin/worktree-setup"}, Teardown: []string{"bin/worktree-archive"},
		SchemaPaths:  []string{"db/"},
		ResetDB:      []string{"SKIP_TEST_DATABASE=1 bin/rails db:schema:load", "bin/rails db:seed"},
		PostCheckout: []string{"bundle check >/dev/null || bundle install --jobs 4"},
		Databases:    []string{"talkable_development__{slug}", "talkable_test__{slug}"},
		Env:          map[string]string{"WT_BRANCH": "{slot}", "CONDUCTOR_WORKSPACE_NAME": ""},
	}
	h.fake = &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"mise"}, Fn: h.mise},
	}}
	h.run = &recRunner{real: &execx.Real{}, fake: h.fake}
	h.m = h.newManager(Deps{})
	return h
}

func (h *harness) newManager(d Deps) *Manager {
	d.Store = h.st
	d.Run = h.run
	d.MySQL = h.my
	d.Layout = h.layout
	if d.Snapshot == nil {
		d.Snapshot = func(ctx context.Context) (herdr.Snapshot, error) { return h.snap, h.snapErr }
	}
	if d.ProcessInfo == nil {
		d.ProcessInfo = func(ctx context.Context, pane string) (herdr.ProcessInfo, error) {
			pi, ok := h.procs[pane]
			if !ok {
				return herdr.ProcessInfo{PaneID: pane, ShellPID: 10, ForegroundPGID: 10}, nil
			}
			return pi, nil
		}
	}
	if d.FreeDiskBytes == nil {
		d.FreeDiskBytes = func(string) (uint64, error) { return h.freeDisk, nil }
	}
	if d.Now == nil {
		d.Now = func() time.Time { return h.now }
	}
	if d.LookPath == nil { // hermetic: mise is "installed" whatever the host has
		d.LookPath = func(file string) (string, error) { return "/fake/bin/" + file, nil }
	}
	return New(d)
}

var versionRe = regexp.MustCompile(`define\(version: ([0-9_]+)\)`)

// mise simulates `mise trust` and `mise exec` for the scripts magnum runs.
func (h *harness) mise(c execx.Cmd) (execx.Result, error) {
	if len(c.Args) >= 3 && c.Args[0] == "-C" && c.Args[2] == "trust" {
		return execx.Result{}, nil
	}
	mc, ok := parseMiseExec(c)
	if !ok {
		return execx.Result{}, fmt.Errorf("unexpected mise call %v", c.Args)
	}
	h.mu.Lock()
	h.scripts = append(h.scripts, mc)
	fail := h.failScript[mc.Script]
	if fail > 0 {
		h.failScript[mc.Script] = fail - 1
	}
	slugFn := h.setupSlug
	h.mu.Unlock()
	if fail != 0 {
		return execx.Result{Code: 1, Stderr: []byte("boom ghp_abcdefghijklmnopqrstuvwxyz0123456789")},
			&execx.ExitError{Cmd: c, Code: 1, Stderr: "boom"}
	}
	slot := mc.Env["WT_BRANCH"]
	switch {
	case mc.Script == "bin/worktree-setup":
		slug := slot
		if slugFn != nil {
			slug = slugFn(slot)
		}
		writeFile(h.t, filepath.Join(mc.Dir, "tmp", ".worktree-db-slug"), slug)
		h.my.add(strings.ReplaceAll(h.schemaVersionAt(mc.Dir), "_", ""), h.pool.DBNames(slug)...)
		return execx.Result{Stdout: []byte("setup ok token ghp_abcdefghijklmnopqrstuvwxyz0123456789\n")}, nil
	case mc.Script == "bin/worktree-archive":
		h.my.remove(h.pool.DBNames(slot)[0]) // leaves the test db behind on purpose
		return execx.Result{}, nil
	case strings.Contains(mc.Script, "db:schema:load"):
		for _, n := range h.pool.DBNames(slot) {
			h.my.add(strings.ReplaceAll(h.schemaVersionAt(mc.Dir), "_", ""), n)
		}
		return execx.Result{}, nil
	}
	return execx.Result{}, nil
}

func (h *harness) schemaVersionAt(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "db", "schema.rb"))
	if err != nil {
		return ""
	}
	m := versionRe.FindStringSubmatch(string(b))
	if m == nil {
		return ""
	}
	return m[1]
}

func (h *harness) scriptCalls(script string) []miseCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []miseCall
	for _, s := range h.scripts {
		if script == "" || s.Script == script {
			out = append(out, s)
		}
	}
	return out
}

func (h *harness) clearScripts() {
	h.mu.Lock()
	h.scripts = nil
	h.mu.Unlock()
}

func (h *harness) slot(name string) store.Slot {
	h.t.Helper()
	sl, err := h.st.SlotByName(h.ctx, name)
	if err != nil {
		h.t.Fatalf("SlotByName %s: %v", name, err)
	}
	return sl
}

func (h *harness) repo() store.Repo {
	h.t.Helper()
	r, err := h.st.UpsertRepo(h.ctx, store.Repo{NodeID: "R_talkable", Owner: "talkable", Name: "talkable",
		WatchOwner: "talkable", DefaultBranch: "master", Mode: store.RepoModePool})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) pr(repoID int64, number int, sha, state string) store.PR {
	h.t.Helper()
	res, err := h.st.UpsertPRFromGitHub(h.ctx, store.GitHubPR{
		RepoID: repoID, NodeID: "PR_" + strconv.Itoa(number) + "_" + strconv.FormatInt(repoID, 10), Number: number,
		URL: "https://github.com/x/y/pull/" + strconv.Itoa(number), HeadSHA: sha, GHState: store.GHOpen,
		BaseRef: store.Ptr("master"), InitialState: state, Identity: "talkable-app",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return res.PR
}

// provisioned provisions pool slot n and fails the test on error.
func (h *harness) provisioned(n int) store.Slot {
	h.t.Helper()
	if err := h.m.ProvisionPool(h.ctx, h.pool, n); err != nil {
		h.t.Fatalf("ProvisionPool(%d): %v", n, err)
	}
	return h.slot(h.pool.Slot(n))
}

// claimedCheckout provisions slot 1, claims it for PR number at sha and checks it out.
func (h *harness) claimedCheckout(number int, sha string) (store.Slot, store.PR) {
	h.t.Helper()
	h.provisioned(1)
	pr := h.pr(h.repo().ID, number, sha, store.PRQueued)
	sl, err := h.m.Claim(h.ctx, pr, h.pool)
	if err != nil {
		h.t.Fatalf("Claim: %v", err)
	}
	if err := h.m.Checkout(h.ctx, sl, pr, h.pool, sha); err != nil {
		h.t.Fatalf("Checkout: %v", err)
	}
	return h.slot(sl.Name), pr
}

func wantHold(t *testing.T, err error, reason string) ErrHold {
	t.Helper()
	var hold ErrHold
	if !errors.As(err, &hold) {
		t.Fatalf("err = %v, want ErrHold{%s}", err, reason)
	}
	if hold.Reason != reason {
		t.Fatalf("hold reason = %q (%s), want %q", hold.Reason, hold.Detail, reason)
	}
	return hold
}

// openAssignment is the PR's open assignment, or store.ErrNotFound.
func (h *harness) openAssignment(prID int64) (store.Assignment, error) {
	as, err := h.st.AssignmentsByPR(h.ctx, prID)
	if err != nil {
		return store.Assignment{}, err
	}
	for _, a := range slices.Backward(as) {
		if a.EndedAt == nil {
			return a, nil
		}
	}
	return store.Assignment{}, store.ErrNotFound
}
