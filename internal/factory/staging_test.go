package factory

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/prstatus"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/tui"
)

// fakeStaging is the factory's staging client in the tests: Status answers
// each env's preset status (unknown for one with none), Refresh records
// what it was asked, waits on block while it is set (or for its ctx),
// panics with panicWith while it is set, and reads (ReadAt moves to the
// clock's now) unless it fails or answers cached, as a Refresh within the
// minute does; Close counts.
type fakeStaging struct {
	mu        sync.Mutex
	now       func() time.Time
	statuses  map[string]staging.EnvStatus // by env name
	asked     []stagingAsk
	prs       []staging.PR
	block     chan struct{}
	started   chan struct{}
	err       error
	cached    bool
	panicWith string
	readAt    map[string]time.Time // by repository, lower case
	cut       int                  // Refreshes their ctx ended
	running   int
	closed    int
	// closedRunning is how many Refreshes still ran when Close came.
	closedRunning int
}

type stagingAsk struct {
	repo string
	open []staging.OpenPR
}

func (f *fakeStaging) Refresh(ctx context.Context, repo string, open []staging.OpenPR) error {
	f.mu.Lock()
	f.asked = append(f.asked, stagingAsk{repo, slices.Clone(open)})
	f.running++
	block, started, err, cached, boom := f.block, f.started, f.err, f.cached, f.panicWith
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
	if boom != "" {
		panic(boom)
	}
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.cut++
			f.mu.Unlock()
			return ctx.Err()
		}
	}
	if err == nil && !cached && f.now != nil {
		f.mu.Lock()
		if f.readAt == nil {
			f.readAt = map[string]time.Time{}
		}
		f.readAt[strings.ToLower(repo)] = f.now()
		f.mu.Unlock()
	}
	return err
}

func (f *fakeStaging) ReadAt(repo string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readAt[strings.ToLower(repo)]
}

func (f *fakeStaging) Status(pr staging.PR, envs []staging.Env) []staging.EnvStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs = append(f.prs, pr)
	out := make([]staging.EnvStatus, len(envs))
	for i, env := range envs {
		s, ok := f.statuses[env.Name]
		if !ok {
			s = staging.EnvStatus{State: staging.StateUnknown, Why: "no refresh yet"}
		}
		s.Env = env
		out[i] = s
	}
	return out
}

func (f *fakeStaging) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	f.closedRunning += f.running
}

// setFields runs edit on f under its lock.
func (f *fakeStaging) setFields(edit func(f *fakeStaging)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f)
}

// set makes env's status s.
func (f *fakeStaging) set(env string, s staging.EnvStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statuses == nil {
		f.statuses = map[string]staging.EnvStatus{}
	}
	f.statuses[env] = s
}

// refreshes is what Refresh was asked, in order.
func (f *fakeStaging) refreshes() []stagingAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// theStagings are the follow rig's repository's three staging
// environments, in the config's order.
var theStagings = []config.FactoryStaging{{Name: "void", Branch: "staging-void"}, {Name: "bart", Branch: "staging-bart"},
	{Name: "bastion", Branch: "staging-bastion"}}

// newStagingRig is the follow rig whose repository has theStagings and a
// factory staging client, the fake.
func newStagingRig(t *testing.T) (*followRig, *fakeStaging) {
	t.Helper()
	f := &fakeStaging{}
	r := newFollowRig(t, func(d *Deps) { d.Staging = f })
	f.now = r.clock.Now
	r.cfg.Factory.Repos = append(r.cfg.Factory.Repos, config.FactoryRepo{Repo: followRef.Repo, Base: "master", Staging: theStagings})
	return r, f
}

// countWrites counts the engine's writes of staging.json, which still
// reach the file.
func (r *followRig) countWrites() *atomic.Int32 {
	var n atomic.Int32
	r.e.stagingWrite = func(path string, b []byte) error {
		n.Add(1)
		return fsx.ReplaceFile(path, b, 0o600)
	}
	return &n
}

// screenDown is the screen's data while no factory daemon runs.
func (r *followRig) screenDown() tui.FactoryData {
	r.t.Helper()
	d, err := ScreenData(r.ctx, r.reg, nil, r.clock.Now(), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout, DaemonDown: true})
	if err != nil {
		r.t.Fatal(err)
	}
	return d
}

// settle waits until no Refresh the engine started runs.
func (r *followRig) settle() {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.e.stagingBusy() {
		if time.Now().After(deadline) {
			r.t.Fatal("a staging Refresh still runs")
		}
		time.Sleep(time.Millisecond)
	}
}

// screen is the factory screen's data at the rig's clock, read as the
// screen reads it: the registry and the files the daemon wrote.
func (r *followRig) screen() tui.FactoryData {
	r.t.Helper()
	d, err := ScreenData(r.ctx, r.reg, nil, r.clock.Now(), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout})
	if err != nil {
		r.t.Fatal(err)
	}
	return d
}

// stagingRow is the screen's row of the rig's task.
func (r *followRig) stagingRow(d tui.FactoryData) tui.FactoryRow {
	r.t.Helper()
	for _, row := range d.Rows {
		if row.ID == r.task.ID {
			return row
		}
	}
	r.t.Fatalf("no row of task %d in %+v", r.task.ID, d.Rows)
	return tui.FactoryRow{}
}

// cellStates are the names and states of cells, "name=state" each.
func cellStates(cells []tui.StagingCell) []string {
	var out []string
	for _, c := range cells {
		out = append(out, c.Name+"="+c.State)
	}
	return out
}

// Each staging state maps to the cell the painter draws, one cell per
// configured env in the config's order: latest, CI while ci.yml runs or
// failed on another change, CI ✗ (ci-fail) when it failed on the PR's own,
// deploying while Jenkins has not confirmed it, outdated, none and unknown.
func TestStagingCellsMapEachStateInTheConfigsOrder(t *testing.T) {
	for _, tc := range []struct {
		status staging.EnvStatus
		want   string
	}{
		{staging.EnvStatus{State: staging.StateLatest}, "latest"},
		{staging.EnvStatus{State: staging.StatePushed, Detail: staging.DetailCIRunning}, "ci"},
		{staging.EnvStatus{State: staging.StatePushed, Detail: staging.DetailCIFailedOurs}, "ci-fail"},
		{staging.EnvStatus{State: staging.StatePushed, Detail: staging.DetailCIFailedNotOurs}, "ci"},
		{staging.EnvStatus{State: staging.StatePushed, Detail: staging.DetailWaitingJenkins}, "deploying"},
		{staging.EnvStatus{State: staging.StatePushed, Detail: staging.DetailNotConfirmed}, "deploying"},
		{staging.EnvStatus{State: staging.StateOutdated, Behind: 2}, "outdated"},
		{staging.EnvStatus{State: staging.StateNone}, "none"},
		{staging.EnvStatus{State: staging.StateUnknown, Why: "cloning the mirror"}, "unknown"},
	} {
		if got := stagingState(tc.status); got != tc.want {
			t.Errorf("%s/%s: cell %q, want %q", tc.status.State, tc.status.Detail, got, tc.want)
		}
	}

	r, f := newStagingRig(t)
	f.set("void", staging.EnvStatus{State: staging.StateLatest})
	f.set("bart", staging.EnvStatus{State: staging.StatePushed, Detail: staging.DetailCIFailedOurs})
	f.set("bastion", staging.EnvStatus{State: staging.StateOutdated, Behind: 1})
	r.tick()
	r.settle()
	r.tick()
	row := r.stagingRow(r.screen())
	if got, want := cellStates(row.Stagings), []string{"void=latest", "bart=ci-fail", "bastion=outdated"}; !slices.Equal(got, want) {
		t.Fatalf("cells %q, want %q in the config's order", got, want)
	}
	for _, c := range row.Stagings {
		if !c.Seen.Equal(r.clock.Now()) || c.OnYou {
			t.Errorf("%s: seen %v, on you %v; want the read %v, not on you", c.Name, c.Seen, c.OnYou, r.clock.Now())
		}
	}
	card := r.screen().Cards[r.task.ID].Task
	if !slices.Equal(cellStates(card.Stagings), cellStates(row.Stagings)) {
		t.Errorf("the card's stagings %q, want the row's %q", cellStates(card.Stagings), cellStates(row.Stagings))
	}
	asked := f.refreshes()
	if len(asked) == 0 || asked[0].repo != followRef.Repo || !slices.Equal(asked[0].open, []staging.OpenPR{{Number: followRef.Number, Base: "master"}}) {
		t.Errorf("Refresh asked %+v, want %s with #%d on master", asked, followRef.Repo, followRef.Number)
	}
}

// A repository without staging envs gets no Refresh and no cell, and
// neither does a task without a published PR; while no factory daemon runs,
// every cell reads unknown.
func TestNoStagingCellsWithoutStagingEnvsOrAPublishedPR(t *testing.T) {
	f := &fakeStaging{}
	r := newFollowRig(t, func(d *Deps) { d.Staging = f })
	f.set("void", staging.EnvStatus{State: staging.StateLatest})
	r.tick()
	r.settle()
	r.tick()
	if asked := f.refreshes(); len(asked) != 0 {
		t.Errorf("a repository without staging envs was refreshed: %+v", asked)
	}
	if row := r.stagingRow(r.screen()); len(row.Stagings) != 0 {
		t.Errorf("a repository without staging envs has cells %+v", row.Stagings)
	}

	r, f = newStagingRig(t)
	f.set("void", staging.EnvStatus{State: staging.StateLatest})
	coding := r.harness.task("PR-12362", registry.TaskActive, "coding")
	r.tick()
	r.settle()
	r.tick()
	d := r.screen()
	for _, row := range d.Rows {
		if row.ID == coding.ID && len(row.Stagings) != 0 {
			t.Errorf("a task without a published PR has cells %+v", row.Stagings)
		}
	}
	if len(r.stagingRow(d).Stagings) != 3 {
		t.Fatalf("the published task has cells %+v, want 3", r.stagingRow(d).Stagings)
	}
	for _, asked := range f.refreshes() {
		if len(asked.open) != 1 || asked.open[0].Number != followRef.Number {
			t.Errorf("Refresh asked for %+v, want the published PR alone", asked.open)
		}
	}

	if got := cellStates(r.stagingRow(r.screenDown()).Stagings); !slices.Equal(got, []string{"void=unknown", "bart=unknown", "bastion=unknown"}) {
		t.Errorf("cells %q while the daemon is down, want each unknown", got)
	}
}

// A Refresh runs off the Tick, on a goroutine of its own: a slow one (the
// first clones) never holds a tick up, a tick while it runs starts no
// second one, and the tick after it ended starts the next.
func TestARefreshRunsOffTheTick(t *testing.T) {
	r, f := newStagingRig(t)
	f.block, f.started = make(chan struct{}), make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		r.tick()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick waited for the Refresh")
	}
	select {
	case <-f.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no Refresh started")
	}
	r.tick()
	if n := len(f.refreshes()); n != 1 {
		t.Fatalf("%d Refreshes, want 1: a tick while one runs starts none", n)
	}
	close(f.block)
	r.settle()
	r.tick()
	r.settle()
	if n := len(f.refreshes()); n != 2 {
		t.Errorf("%d Refreshes, want the next tick's second", n)
	}
}

// A failed Refresh is logged once while its cause stays the same, again
// for another cause, and the cells keep no last good read.
func TestAFailedRefreshIsLoggedOncePerCause(t *testing.T) {
	r, f := newStagingRig(t)
	f.err = errors.New("staging example/shop: the mirror could not be read: exit 128 after 12s")
	for range 3 {
		r.tick()
		r.settle()
	}
	f.setFields(func(f *fakeStaging) {
		f.err = errors.New("staging example/shop: the mirror could not be read: exit 128 after 31s")
	})
	r.tick()
	r.settle()
	if n := strings.Count(r.log.String(), "the staging status of a repository is not read"); n != 1 {
		t.Errorf("%d warnings for one cause, want 1:\n%s", n, r.log)
	}
	f.setFields(func(f *fakeStaging) { f.err = errors.New("staging example/shop: no git env for the fetch") })
	r.tick()
	r.settle()
	if n := strings.Count(r.log.String(), "the staging status of a repository is not read"); n != 2 {
		t.Errorf("%d warnings for two causes, want 2:\n%s", n, r.log)
	}
	for _, c := range r.stagingRow(r.screen()).Stagings {
		if !c.Seen.IsZero() {
			t.Errorf("%s: seen %v after failed reads alone", c.Name, c.Seen)
		}
	}
}

// The daemon's stop cancels a Refresh in flight and then closes the
// staging client, within the drain.
func TestTheDaemonClosesItsStagingClientWhenItStops(t *testing.T) {
	r, f := newStagingRig(t)
	f.block, f.started = make(chan struct{}), make(chan struct{}, 1)
	sigs := r.captureSignals()
	done := make(chan error, 1)
	go func() { done <- r.e.Run(r.ctx) }()
	select {
	case <-f.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no Refresh started")
	}
	(<-sigs) <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed != 1 || f.cut != 1 || f.closedRunning != 0 {
		t.Errorf("closed %d, refreshes cut %d, still running at the close %d; want the Refresh cut, then one Close", f.closed, f.cut, f.closedRunning)
	}
}

// The status file's stagings follow the staging status, one per env in the
// config's order in the file's words (latest is deployed, a CI that failed
// on the PR's change failed, the rest of pushed deploying, outdated and
// none none), Staging the first of them for the readers before stagings;
// an env whose status is unknown keeps the word the file had.
func TestThePRStatusFileCarriesEachStaging(t *testing.T) {
	r, f := newStagingRig(t)
	r.published("stagings")
	f.set("void", staging.EnvStatus{State: staging.StateLatest})
	f.set("bart", staging.EnvStatus{State: staging.StatePushed, Detail: staging.DetailCIFailedOurs})
	f.set("bastion", staging.EnvStatus{State: staging.StatePushed, Detail: staging.DetailWaitingJenkins})
	r.tick()
	got := r.mustStatus()
	want := []prstatus.Staging{{Env: "void", State: prstatus.StagingDeployed}, {Env: "bart", State: prstatus.StagingFailed},
		{Env: "bastion", State: prstatus.StagingDeploying}}
	if !slices.Equal(got.Stagings, want) || got.Staging != want[0] {
		t.Fatalf("stagings %+v, staging %+v; want %+v, the first as staging", got.Stagings, got.Staging, want)
	}

	f.set("void", staging.EnvStatus{State: staging.StateUnknown, Why: "stale"})
	f.set("bart", staging.EnvStatus{State: staging.StateOutdated, Behind: 1})
	f.set("bastion", staging.EnvStatus{State: staging.StatePushed, Detail: staging.DetailCIFailedNotOurs})
	r.clock.Add(time.Minute)
	r.tick()
	got = r.mustStatus()
	want = []prstatus.Staging{{Env: "void", State: prstatus.StagingDeployed}, {Env: "bart", State: prstatus.StagingNone},
		{Env: "bastion", State: prstatus.StagingDeploying}}
	if !slices.Equal(got.Stagings, want) || got.Staging != want[0] {
		t.Errorf("stagings %+v, staging %+v; want %+v (void kept)", got.Stagings, got.Staging, want)
	}
}

// A repository whose staging env is named otherwise than a slug writes the
// env without its name, as the status file's contract takes it.
func TestAStagingEnvThatIsNoSlugIsWrittenWithoutItsName(t *testing.T) {
	r, f := newStagingRig(t)
	r.cfg.Factory.Repos[len(r.cfg.Factory.Repos)-1].Staging = []config.FactoryStaging{{Name: "Void Env", Branch: "staging-void"}}
	r.published("slug")
	f.set("Void Env", staging.EnvStatus{State: staging.StateLatest})
	r.tick()
	if got := r.mustStatus(); !slices.Equal(got.Stagings, []prstatus.Staging{{State: prstatus.StagingDeployed}}) {
		t.Errorf("stagings %+v, want one without a name", got.Stagings)
	}
}

// Without a staging client (no repository has staging envs), the status
// file has no stagings, its staging row none, as before.
func TestNoStagingClientWritesNoStagings(t *testing.T) {
	r := newFollowRig(t)
	r.cfg.Factory.Repos = append(r.cfg.Factory.Repos, config.FactoryRepo{Repo: followRef.Repo, Base: "master", Staging: theStagings})
	r.published("none")
	r.tick()
	if got := r.mustStatus(); got.Stagings != nil || got.Staging != (prstatus.Staging{State: prstatus.StagingNone}) {
		t.Errorf("stagings %+v, staging %+v; want none", got.Stagings, got.Staging)
	}
	if _, err := os.Stat(r.layout.FactoryStaging()); err != nil {
		t.Errorf("the screen's staging file: %v, want one that says no task has cells", err)
	}
}

// The staging status of a repository reads as the poll identity of the
// watch that covers it (its identity when it has no poll_identity), as
// magnum's review daemon reads that repository, so the two daemons share
// the mirror and its state with one login; a repository no watch covers
// reads as the gh identity the factory publishes as.
func TestTheStagingStatusReadsAsTheReposPollIdentity(t *testing.T) {
	cfg := config.Defaults()
	cfg.Identities = []config.Identity{{Name: "me", Kind: "gh", Login: "zhuravel"},
		{Name: "poller", Kind: "app", Login: "poller[bot]", AppID: 1, InstallationID: 2}}
	cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"shop"}, Identity: "me", PollIdentity: "poller"},
		{Owner: "example", Include: []string{"site"}, Identity: "me"}}
	for repo, want := range map[string]string{"example/shop": "poller", "Example/Shop": "poller", "example/site": "me", "other/app": "me"} {
		id, err := StagingIdentity(cfg, repo)
		if err != nil || id.Name != want {
			t.Errorf("%s reads as %q (%v), want %q", repo, id.Name, err, want)
		}
	}
	cfg.Identities = cfg.Identities[1:]
	if _, err := StagingIdentity(cfg, "other/app"); err == nil {
		t.Error("a repository no watch covers, without a gh identity, reads as someone")
	}
}

// With no staging state changing, neither the screen's staging.json nor the
// PR's status file is written again, however many reads and ticks go by: a
// cell's Seen stays the read that first saw its state. One state that
// changes writes each once, and moves that cell's Seen alone. Before, every
// tick rewrote staging.json, its Seen the time of the last Refresh call.
func TestUnchangedStagingStatesWriteNothing(t *testing.T) {
	r, f := newStagingRig(t)
	r.published("unchanged")
	writes := r.countWrites()
	f.set("void", staging.EnvStatus{State: staging.StateLatest})
	f.set("bart", staging.EnvStatus{State: staging.StateNone})
	f.set("bastion", staging.EnvStatus{State: staging.StateNone})
	r.tick()
	r.settle()
	r.tick() // the first read gives the cells their Seen
	r.settle()
	first := r.clock.Now()
	n := writes.Load()
	status := r.mustStatus()
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(r.statusPath(), old, old); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		r.clock.Add(45 * time.Second)
		if i%2 == 1 { // half the calls answer from the minute's cache, the rest read
			f.setFields(func(f *fakeStaging) { f.cached = true })
		} else {
			f.setFields(func(f *fakeStaging) { f.cached = false })
		}
		r.tick()
		r.settle()
	}
	if got := writes.Load() - n; got != 0 {
		t.Errorf("staging.json written %d times in 10 ticks with no state change, want none", got)
	}
	if st, err := os.Stat(r.statusPath()); err != nil || !st.ModTime().Equal(old) {
		t.Errorf("the status file was written again with no state change (%v, %v)", st, err)
	}
	for _, c := range r.stagingRow(r.screen()).Stagings {
		if !c.Seen.Equal(first) {
			t.Errorf("%s: seen %v, want the read that first saw its state, %v", c.Name, c.Seen, first)
		}
	}

	f.setFields(func(f *fakeStaging) { f.cached = false })
	r.clock.Add(45 * time.Second)
	r.tick() // this tick's Refresh reads bart's new state
	r.settle()
	changed := r.clock.Now()
	f.set("bart", staging.EnvStatus{State: staging.StatePushed, Detail: staging.DetailCIFailedOurs})
	n = writes.Load()
	r.clock.Add(45 * time.Second)
	r.tick()
	r.settle()
	r.clock.Add(45 * time.Second)
	r.tick()
	r.settle()
	if got := writes.Load() - n; got != 1 {
		t.Errorf("staging.json written %d times for one state change, want once", got)
	}
	if got := r.mustStatus(); got.UpdatedAt.Equal(status.UpdatedAt) || got.Stagings[1].State != prstatus.StagingFailed {
		t.Errorf("the status file after the change: %+v, want bart failed and a new updated_at", got.Stagings)
	}
	cells := r.stagingRow(r.screen()).Stagings
	if !cells[0].Seen.Equal(first) || !cells[1].Seen.Equal(changed) || !cells[2].Seen.Equal(first) {
		t.Errorf("seen %v %v %v, want void and bastion at %v, bart at the read that saw its change %v",
			cells[0].Seen, cells[1].Seen, cells[2].Seen, first, changed)
	}
}

// staging.json names its fields itself (name, state, seen), never by the Go
// names of the screen's types: a daemon and a screen of two builds read one
// file.
func TestTheStagingFileNamesItsOwnFields(t *testing.T) {
	r, f := newStagingRig(t)
	f.set("void", staging.EnvStatus{State: staging.StateLatest})
	r.tick()
	b, err := os.ReadFile(r.layout.FactoryStaging())
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Tasks map[string]struct {
			PR    int              `json:"pr"`
			Cells []map[string]any `json:"cells"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	cells := file.Tasks[strconv.FormatInt(r.task.ID, 10)].Cells
	if len(cells) != 3 {
		t.Fatalf("staging.json %s: want the task's three cells", b)
	}
	keys := slices.Sorted(maps.Keys(cells[0]))
	if !slices.Equal(keys, []string{"name", "seen", "state"}) || cells[0]["name"] != "void" || cells[0]["state"] != "latest" {
		t.Errorf("a cell %v, want name, state and seen", cells[0])
	}
}

// A Refresh that panics is contained as every other goroutine off the loop
// is: the daemon goes on, the panic is logged once while it repeats, and the
// next Refresh of the repository starts only after a backoff that doubles;
// a good Refresh ends the backoff.
func TestAPanickingRefreshIsContainedAndRetriedAfterABackoff(t *testing.T) {
	r, f := newStagingRig(t)
	f.set("void", staging.EnvStatus{State: staging.StateLatest})
	f.setFields(func(f *fakeStaging) { f.panicWith = "staging: index out of range [3] with length 3" })
	r.tick()
	r.settle()
	if n := len(f.refreshes()); n != 1 {
		t.Fatalf("%d Refreshes, want 1", n)
	}
	r.clock.Add(45 * time.Second)
	r.tick() // within the first backoff, a minute
	r.settle()
	if n := len(f.refreshes()); n != 1 {
		t.Errorf("%d Refreshes within the backoff, want still 1", n)
	}
	r.clock.Add(30 * time.Second)
	r.tick()
	r.settle()
	if n := len(f.refreshes()); n != 2 {
		t.Errorf("%d Refreshes after the backoff, want 2", n)
	}
	r.clock.Add(90 * time.Second)
	r.tick() // the second backoff is two minutes
	r.settle()
	if n := len(f.refreshes()); n != 2 {
		t.Errorf("%d Refreshes within the second backoff, want still 2", n)
	}
	if n := strings.Count(r.log.String(), "a staging Refresh panicked"); n != 1 {
		t.Errorf("the panic logged %d times, want once:\n%s", n, r.log)
	}
	f.setFields(func(f *fakeStaging) { f.panicWith = "" })
	r.clock.Add(time.Minute)
	r.tick()
	r.settle()
	r.clock.Add(45 * time.Second)
	r.tick()
	r.settle()
	if n := len(f.refreshes()); n != 4 {
		t.Errorf("%d Refreshes, want a good one after the backoff and the next tick's", n)
	}
}

// No Refresh starts while the disk hold is in force: the first clones the
// whole history, and the factory parks agents for the disk then.
func TestNoRefreshStartsUnderTheDiskHold(t *testing.T) {
	r, f := newStagingRig(t)
	dir := t.TempDir()
	repo := &r.cfg.Factory.Repos[len(r.cfg.Factory.Repos)-1]
	repo.Copies, repo.MinFreeDiskGB = dir+"/shop.factory{n}", 15
	var free atomic.Uint64
	free.Store(1 << 30)
	r.e.d.Disk = func(string) (uint64, error) { return free.Load(), nil }
	r.tick()
	r.settle()
	if n := len(f.refreshes()); n != 0 {
		t.Errorf("%d Refreshes under the disk hold, want none", n)
	}
	free.Store(100 << 30)
	r.tick()
	r.settle()
	if n := len(f.refreshes()); n != 1 {
		t.Errorf("%d Refreshes once the hold lifted, want 1", n)
	}
}

// An env whose status stays unknown keeps the status file's word only
// while it is younger than staging.StaleAfter: reads that fail for long
// leave no "deployed" of days ago in the PR's comment.
func TestAnUnknownStagingKeepsTheFilesWordOnlyForAWhile(t *testing.T) {
	r, f := newStagingRig(t)
	r.published("unknown")
	f.set("void", staging.EnvStatus{State: staging.StateLatest})
	r.tick()
	if got := r.mustStatus().Stagings[0].State; got != prstatus.StagingDeployed {
		t.Fatalf("void %s, want deployed", got)
	}
	f.set("void", staging.EnvStatus{State: staging.StateUnknown, Why: "the mirror's volume is not mounted"})
	r.clock.Add(time.Minute)
	r.tick()
	if got := r.mustStatus().Stagings[0].State; got != prstatus.StagingDeployed {
		t.Errorf("void %s a minute into the unknown, want deployed kept", got)
	}
	r.clock.Add(staging.StaleAfter)
	r.tick()
	if got := r.mustStatus().Stagings[0].State; got != prstatus.StagingNone {
		t.Errorf("void %s past staging.StaleAfter of unknown, want none", got)
	}
}

// stubGit is a staging.Git whose Base fails, so a Refresh goes no further
// than its git env and its lock, and whose Fetch fails when fail is set.
type stubGit struct {
	staging.Git
	fail bool
}

func (stubGit) Base(context.Context, string, string) (string, error) { return "", errors.New("stub") }

func (g stubGit) Fetch(context.Context, string, map[string]string, []string) error {
	if g.fail {
		return errors.New("stub fetch failed")
	}
	return nil
}

type ctxKey struct{}

// The staging client the daemon builds hands its git env the context of
// the Refresh that asks for it, which the daemon's stop cancels, so a mint
// of an App's token ends with the drain; and ReadAt moves only on a fetch
// that worked.
func TestTheStagingClientsGitEnvTakesTheRefreshsContext(t *testing.T) {
	root := t.TempDir() + "/staging"
	var got any
	cfg := config.Defaults()
	c := NewStaging(cfg, staging.Deps{Git: stubGit{}, Mirror: staging.MirrorDir(root), MirrorRoot: staging.MirrorRoot(root)},
		func(ctx context.Context, repo string) map[string]string {
			got = ctx.Value(ctxKey{})
			return map[string]string{"GH_HOST": "github.com"}
		})
	defer c.Close()
	ctx := context.WithValue(t.Context(), ctxKey{}, "the refresh")
	_ = c.Refresh(ctx, "example/shop", nil)
	if got != "the refresh" {
		t.Fatalf("the git env's context carries %v, want the Refresh's", got)
	}

	sc := c.(*stagingClient)
	dir, _ := staging.MirrorDir(root)("example/shop")
	sc.git.Git = stubGit{fail: true}
	if err := sc.git.Fetch(t.Context(), dir, nil, nil); err == nil || !c.ReadAt("example/shop").IsZero() {
		t.Errorf("a failed fetch: %v, read at %v; want an error and no read", err, c.ReadAt("example/shop"))
	}
	sc.git.Git = stubGit{}
	if err := sc.git.Fetch(t.Context(), dir, nil, nil); err != nil || c.ReadAt("example/shop").IsZero() {
		t.Errorf("a fetch that worked: %v, read at %v; want a read", err, c.ReadAt("example/shop"))
	}
}
