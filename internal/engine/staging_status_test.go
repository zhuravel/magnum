package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
)

// fakeStaging is the daemon's staging client in tests: Refresh records what
// it was asked and, with gate set, waits for it (or its ctx); Status answers
// status by PR number (none on every env otherwise).
type fakeStaging struct {
	mu         sync.Mutex
	refreshes  []fakeRefresh
	gate       chan struct{}
	err        error
	status     map[int][]staging.EnvStatus
	statusOf   []staging.PR // every Status call's PR
	busy       int          // Refreshes running now
	closed     int
	closedBusy bool // Close ran while a Refresh still ran
	afterStop  bool // a Refresh started after its ctx ended
}

type fakeRefresh struct {
	repo string
	open []staging.OpenPR
}

func (f *fakeStaging) Refresh(ctx context.Context, repo string, open []staging.OpenPR) error {
	f.mu.Lock()
	f.refreshes = append(f.refreshes, fakeRefresh{repo, slices.Clone(open)})
	f.busy++
	gate, err := f.gate, f.err
	if ctx.Err() != nil {
		f.afterStop = true
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.busy--; f.mu.Unlock() }()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeStaging) Status(pr staging.PR, envs []staging.Env) []staging.EnvStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusOf = append(f.statusOf, pr)
	if ss, ok := f.status[pr.Number]; ok {
		return slices.Clone(ss)
	}
	out := make([]staging.EnvStatus, len(envs))
	for i, e := range envs {
		out[i] = staging.EnvStatus{Env: e, State: staging.StateNone}
	}
	return out
}

func (f *fakeStaging) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	if f.busy > 0 {
		f.closedBusy = true
	}
}

func (f *fakeStaging) calls() []fakeRefresh {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.refreshes)
}

func (f *fakeStaging) statusCalls() []staging.PR {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.statusOf)
}

// testStagings are talkable/talkable's staging envs in the tests.
var testStagings = []config.FactoryStaging{
	{Name: "void", Branch: "staging-void"}, {Name: "bart", Branch: "staging-bart"}, {Name: "bastion", Branch: "staging-bastion"},
}

// withStagings gives talkable/talkable the three staging envs and the
// engine f as its staging client (nil f: none).
func withStagings(f *fakeStaging) func(*harness) {
	return func(h *harness) {
		h.cfg.Factory.Repos = append(h.cfg.Factory.Repos, config.FactoryRepo{Repo: "talkable/talkable", Staging: slices.Clone(testStagings)})
		if f != nil {
			h.d.Staging = f
		}
	}
}

// stagingRecord is PR #n's stored staging status; false when none.
func (h *harness) stagingRecord(n int) (StagingRecord, bool) {
	h.t.Helper()
	v, ok := h.kvOf(KVPRStaging(h.pr(n).ID))
	if !ok {
		return StagingRecord{}, false
	}
	rec, ok := ParseStagingRecord(v)
	if !ok {
		h.t.Fatalf("PR #%d's staging record %q does not parse", n, v)
	}
	return rec, true
}

// stagingTick runs one tick that must return while a Refresh may still
// wait, then lets the rounds it started end.
func (h *harness) stagingTick() {
	h.t.Helper()
	done := make(chan error, 1)
	go func() { done <- h.e.Tick(h.ctx) }()
	select {
	case err := <-done:
		if err != nil {
			h.t.Fatalf("tick: %v", err)
		}
	case <-time.After(30 * time.Second):
		h.t.Fatal("the tick waited for the staging Refresh")
	}
	h.settle()
}

// awaitRefreshes waits until f was asked n Refreshes.
func awaitRefreshes(t *testing.T, f *fakeStaging, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); len(f.calls()) < n; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d staging Refreshes, want %d", len(f.calls()), n)
		}
	}
}

// No [[factory.repo.staging]] entry for a watched repository makes no
// staging client: a config without any (the operator's today), one whose
// entries name a repository no [[watch]] covers, a dry run and a tagged App
// (a replay, the factory's local review). One for a watched repository
// makes one, which reads nothing as it is made.
func TestNoStagingEntryForAWatchedRepositoryMakesNoStagingClient(t *testing.T) {
	h := newHarness(t)
	newApp := func(cfg *config.Config) *app.App {
		return &app.App{Config: cfg, Runner: &execx.Fake{}, Logger: h.d.Logger}
	}
	if s := stagingFromApp(newApp(h.cfg)); s != nil {
		t.Fatalf("a config without staging entries made a staging client: %T", s)
	}
	unwatched := *h.cfg
	unwatched.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Staging: slices.Clone(testStagings)}}
	if s := stagingFromApp(newApp(&unwatched)); s != nil {
		t.Fatal("staging entries of a repository no [[watch]] covers made a staging client")
	}
	if got := StagingRepos(&unwatched); len(got) != 0 {
		t.Fatalf("StagingRepos = %v, want none", got)
	}
	watched := *h.cfg
	watched.Factory.Repos = []config.FactoryRepo{{Repo: "talkable/talkable", Staging: slices.Clone(testStagings)}}
	for _, mod := range []func(*app.App){func(a *app.App) { a.DryRun = true }, func(a *app.App) { a.AgentTag = "eval" }} {
		a := newApp(&watched)
		mod(a)
		if s := stagingFromApp(a); s != nil {
			t.Fatal("a dry run or a tagged App made a staging client")
		}
	}
	s := stagingFromApp(newApp(&watched))
	if s == nil {
		t.Fatal("staging entries of a watched repository made no staging client")
	}
	s.Close()
	if got := StagingRepos(&watched); !slices.Equal(got, []string{"talkable/talkable"}) {
		t.Fatalf("StagingRepos = %v", got)
	}
	want := []staging.Env{{Name: "void", Branch: "staging-void"}, {Name: "bart", Branch: "staging-bart"}, {Name: "bastion", Branch: "staging-bastion"}}
	if got := StagingEnvs(&watched, "Talkable/Talkable"); !slices.Equal(got, want) {
		t.Fatalf("StagingEnvs = %v, want %v", got, want)
	}
	if got := StagingEnvs(&watched, "example/shop"); got != nil {
		t.Fatalf("StagingEnvs of a repository without entries = %v", got)
	}
}

// Without a staging client the daemon runs nothing of staging and writes no
// record, whatever the config says; with one but no staging entry it asks
// it nothing.
func TestWithoutStagingsNothingOfStagingRuns(t *testing.T) {
	h := newHarness(t, withStagings(nil))
	h.open(prSpec{n: 1, head: "aaa1111"})
	h.tick()
	h.tick()
	if _, ok := h.stagingRecord(1); ok {
		t.Fatal("a daemon without a staging client stored a staging record")
	}

	f := &fakeStaging{}
	h = newHarness(t, func(h *harness) { h.d.Staging = f })
	h.open(prSpec{n: 1, head: "aaa1111"})
	h.tick()
	h.tick()
	h.e.stagingWG.Wait()
	if c, s := f.calls(), f.statusCalls(); len(c) != 0 || len(s) != 0 {
		t.Fatalf("a config without staging entries asked the client %d Refreshes and %d Statuses", len(c), len(s))
	}
	if _, ok := h.stagingRecord(1); ok {
		t.Fatal("a config without staging entries stored a staging record")
	}
}

// The tick starts a repository's Refresh on a goroutine of its own and
// returns while it runs (the first one clones the mirror: minutes), with
// the open PRs' numbers and bases; a tick while it still runs starts no
// second one, and the next tick after it ended starts the next.
func TestTheTickRunsEachStagingRefreshOffTheTickOneAtATime(t *testing.T) {
	f := &fakeStaging{gate: make(chan struct{})}
	h := newHarness(t, withStagings(f))
	h.open(prSpec{n: 1, head: "aaa1111"}, prSpec{n: 2, head: "bbb2222", base: "release"})
	h.stagingTick()
	awaitRefreshes(t, f, 1)
	got := f.calls()[0]
	want := []staging.OpenPR{{Number: 1, Base: "master"}, {Number: 2, Base: "release"}}
	if got.repo != "talkable/talkable" || !slices.Equal(got.open, want) {
		t.Fatalf("Refresh(%s, %v), want talkable/talkable with %v", got.repo, got.open, want)
	}
	h.stagingTick()
	if n := len(f.calls()); n != 1 {
		t.Fatalf("a tick while the Refresh ran started another: %d Refreshes", n)
	}
	close(f.gate)
	h.e.stagingWG.Wait()
	h.stagingTick()
	awaitRefreshes(t, f, 2)
	h.e.stagingWG.Wait()
}

// Every open PR of a repository with stagings gets its Status stored
// compactly under pr.<id>.staging once the repository's first Refresh in
// this daemon returned (before it, every env reads "no refresh yet", which
// a restart would flash on the board): the head it was told for and, per
// env in the config's order, its state and what the board and the CLI show
// of it. A PR whose head moved is told again; a PR that closed loses its
// record.
func TestEveryOpenPRsStagingStatusIsStoredForTheBoardAndTheCLI(t *testing.T) {
	pushed := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	deployed := time.Date(2026, 10, 5, 9, 20, 0, 0, time.UTC)
	void, bart, bastion := staging.Env{Name: "void", Branch: "staging-void"}, staging.Env{Name: "bart", Branch: "staging-bart"},
		staging.Env{Name: "bastion", Branch: "staging-bastion"}
	f := &fakeStaging{gate: make(chan struct{}), status: map[int][]staging.EnvStatus{
		1: {
			{Env: void, State: staging.StateLatest, Tip: "tip0000", InTip: "aaa1111", PushedAt: pushed, DeployedTip: "tip0000", DeployedAt: deployed,
				RunURL: "https://github.com/talkable/talkable/actions/runs/7"},
			{Env: bart, State: staging.StatePushed, Detail: staging.DetailCIFailedOurs, Tip: "tip1111", InTip: "aaa1111", PushedAt: pushed,
				RunURL: "https://github.com/talkable/talkable/actions/runs/8", Why: "spec/models/coupon_spec.rb", DeployedAt: deployed},
			{Env: bastion, State: staging.StateOutdated, Tip: "tip2222", InTip: "old0000", Behind: 2, PushedAt: pushed, DeployedAt: deployed},
		},
	}}
	h := newHarness(t, withStagings(f))
	h.open(prSpec{n: 1, head: "aaa1111"}, prSpec{n: 2, head: "bbb2222"})
	h.stagingTick()
	awaitRefreshes(t, f, 1)
	h.stagingTick()
	if _, ok := h.stagingRecord(1); ok {
		t.Fatal("a record was stored before the repository's first Refresh returned")
	}
	close(f.gate)
	h.e.stagingWG.Wait()
	h.stagingTick()
	h.e.stagingWG.Wait()

	rec, ok := h.stagingRecord(1)
	if !ok {
		t.Fatal("no staging record for #1")
	}
	want := StagingRecord{Head: "aaa1111", Envs: []StagingEnv{
		{Env: "void", State: "latest", InTip: "aaa1111", PushedAt: pushed, DeployedAt: deployed, RunURL: "https://github.com/talkable/talkable/actions/runs/7"},
		{Env: "bart", State: "pushed", Detail: "ci_failed_ours", InTip: "aaa1111", PushedAt: pushed,
			RunURL: "https://github.com/talkable/talkable/actions/runs/8", Why: "spec/models/coupon_spec.rb"},
		{Env: "bastion", State: "outdated", InTip: "old0000", Behind: 2, PushedAt: pushed},
	}}
	if !stagingRecordsEqual(rec, want) {
		t.Fatalf("record\n%+v\nwant\n%+v", rec, want)
	}
	raw, _ := h.kvOf(KVPRStaging(h.pr(1).ID))
	for _, leaves := range []string{"tip0000", "tip1111", "deployed_tip"} {
		if strings.Contains(raw, leaves) {
			t.Errorf("the record keeps %q, which nothing shows: %s", leaves, raw)
		}
	}
	none, ok := h.stagingRecord(2)
	if !ok || none.Head != "bbb2222" || len(none.Envs) != 3 || none.Envs[0].State != "none" || none.Envs[2].Env != "bastion" {
		t.Fatalf("#2's record %+v (%v)", none, ok)
	}
	if strings.Contains(mustKV(t, h, KVPRStaging(h.pr(2).ID)), "at\"") {
		t.Errorf("a none env keeps a time: %s", mustKV(t, h, KVPRStaging(h.pr(2).ID)))
	}

	// #1 moves to a new head: told again for it. #2 closes: its record goes.
	h.open(prSpec{n: 1, head: "ccc3333"})
	h.gh.closed[2] = "MERGED"
	h.advance(time.Minute)
	h.stagingTick()
	h.advance(confirmGap)
	h.stagingTick()
	h.e.stagingWG.Wait()
	h.stagingTick()
	if rec, _ := h.stagingRecord(1); rec.Head != "ccc3333" {
		t.Fatalf("#1's record is for %s after its head moved to ccc3333", rec.Head)
	}
	if calls := f.statusCalls(); calls[len(calls)-1].Head != "ccc3333" || calls[len(calls)-1].Base != "master" || calls[len(calls)-1].Repo != "talkable/talkable" {
		t.Fatalf("the last Status was for %+v", calls[len(calls)-1])
	}
	if pr := h.pr(2); pr.GHState == store.GHOpen {
		t.Fatalf("#2 is still open: %s", pr.GHState)
	}
	if _, ok := h.stagingRecord(2); ok {
		t.Fatal("a PR that closed keeps its staging record")
	}
}

// A failed Refresh still counts as read: the statuses are told, and say why
// they are unknown (Status's Why).
func TestAFailedStagingRefreshStillTellsTheStatuses(t *testing.T) {
	f := &fakeStaging{err: errors.New("staging talkable/talkable: the mirror could not be read")}
	h := newHarness(t, withStagings(f))
	h.open(prSpec{n: 1, head: "aaa1111"})
	h.stagingTick()
	h.e.stagingWG.Wait()
	h.stagingTick()
	h.e.stagingWG.Wait()
	if _, ok := h.stagingRecord(1); !ok {
		t.Fatal("a failed Refresh left the PR without a record")
	}
}

// The daemon's stop ends the Refresh in flight (its ctx), waits for it, and
// only then closes the client, which ends its background clones.
func TestTheDaemonsStopWaitsForTheStagingRefreshThenClosesTheClient(t *testing.T) {
	f := &fakeStaging{gate: make(chan struct{})}
	h := newHarness(t, withStagings(f))
	h.open(prSpec{n: 1, head: "aaa1111"})
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true})
	awaitRefreshes(t, f, 1)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the daemon did not stop")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed != 1 || f.closedBusy || f.busy != 0 {
		t.Fatalf("closed %d times, while a Refresh ran: %v, Refreshes running: %d", f.closed, f.closedBusy, f.busy)
	}
}

// Under --once the tick's context outlives the stop: the stop itself ends
// the Refresh in flight before it closes the client.
func TestAOnceRunsStopEndsItsStagingRefresh(t *testing.T) {
	f := &fakeStaging{gate: make(chan struct{})}
	h := newHarness(t, withStagings(f))
	h.open(prSpec{n: 1, head: "aaa1111"})
	done := h.goRun(h.ctx, Options{Once: true, NoSignals: true})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the --once run did not stop: its staging Refresh still waited")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed != 1 || f.closedBusy || len(f.refreshes) != 1 {
		t.Fatalf("closed %d times, while a Refresh ran: %v, %d Refreshes", f.closed, f.closedBusy, len(f.refreshes))
	}
}

// stagingRecordsEqual compares two records, times by instant.
func stagingRecordsEqual(a, b StagingRecord) bool {
	return a.Head == b.Head && slices.EqualFunc(a.Envs, b.Envs, func(x, y StagingEnv) bool {
		return x.Env == y.Env && x.State == y.State && x.Detail == y.Detail && x.InTip == y.InTip && x.Behind == y.Behind &&
			x.PushedAt.Equal(y.PushedAt) && x.DeployedAt.Equal(y.DeployedAt) && x.RunURL == y.RunURL && x.Why == y.Why
	})
}

// The record round-trips, and one that cannot be read is none.
func TestAStagingRecordRoundTrips(t *testing.T) {
	rec := StagingRecord{Head: "aaa1111", Envs: []StagingEnv{{Env: "void", State: "pushed", Detail: "ci_running", InTip: "aaa1111",
		PushedAt: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)}}}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ParseStagingRecord(string(b))
	if !ok || !stagingRecordsEqual(got, rec) {
		t.Fatalf("round trip %+v (%v)", got, ok)
	}
	for _, bad := range []string{"", "{", `{"envs":[]}`, `[]`} {
		if _, ok := ParseStagingRecord(bad); ok {
			t.Errorf("ParseStagingRecord(%q) read a record", bad)
		}
	}
}
