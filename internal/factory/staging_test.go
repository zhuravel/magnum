package factory

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/prstatus"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/tui"
)

// fakeStaging is the factory's staging client in the tests: Status answers
// each env's preset status (unknown for one with none), Refresh records
// what it was asked and waits on block while it is set (or for its ctx),
// and Close counts.
type fakeStaging struct {
	mu       sync.Mutex
	statuses map[string]staging.EnvStatus // by env name
	asked    []stagingAsk
	prs      []staging.PR
	block    chan struct{}
	started  chan struct{}
	err      error
	cut      int // Refreshes their ctx ended
	running  int
	closed   int
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
	block, started, err := f.block, f.started, f.err
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
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
	return err
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
	r.cfg.Factory.Repos = append(r.cfg.Factory.Repos, config.FactoryRepo{Repo: followRef.Repo, Base: "master", Staging: theStagings})
	return r, f
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
		seen := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
		c := stagingCell(tc.status, seen)
		if c.State != tc.want || c.OnYou || !c.Seen.Equal(seen) {
			t.Errorf("%s/%s: cell %+v, want %q, not on you, seen %v", tc.status.State, tc.status.Detail, c, tc.want, seen)
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
		if !c.Seen.Equal(r.clock.Now()) {
			t.Errorf("%s: seen %v, want the last good Refresh %v", c.Name, c.Seen, r.clock.Now())
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
// neither does a task without a published PR; a cell whose last good read
// is older than staging.StaleAfter (its daemon stopped) reads unknown.
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

	r.clock.Add(staging.StaleAfter + time.Minute) // the daemon stopped: no tick wrote the file since
	if got := cellStates(r.stagingRow(r.screen()).Stagings); !slices.Equal(got, []string{"void=unknown", "bart=unknown", "bastion=unknown"}) {
		t.Errorf("stale cells %q, want each unknown", got)
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
	f.mu.Lock()
	f.err = errors.New("staging example/shop: the mirror could not be read: exit 128 after 31s")
	f.mu.Unlock()
	r.tick()
	r.settle()
	if n := strings.Count(r.log.String(), "the staging status of a repository is not read"); n != 1 {
		t.Errorf("%d warnings for one cause, want 1:\n%s", n, r.log)
	}
	f.mu.Lock()
	f.err = errors.New("staging example/shop: no git env for the fetch")
	f.mu.Unlock()
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
