package factory

// The staging status of the published tasks' PRs (internal/staging, R27):
// where each PR stands on each [[factory.repo.staging]] environment of its
// repository. The daemon owns one staging client (Deps.Staging, NewStaging),
// built only when a repository has staging envs. Each tick starts, for every
// such repository with a published task's PR, one Refresh off the loop (the
// first clones the mirror; Refresh itself reads at most once a minute),
// never two at a time, none while the daemon drains or the disk hold is in
// force, and one that panicked again only after a backoff; and it reads
// each PR's statuses (Status: a pure lookup of the last Refresh). They become
// the PR's staging cells, which the tick writes to staging.json
// (paths.Layout.FactoryStaging) for the screen, another process that runs no
// Refresh and no git, and its status file's stagings (pr_status.go), each
// only when a state changed. A cell's Seen is the time of the read that
// first saw its state: a Refresh that fetched (ReadAt), never one that
// answered from its minute's cache, so a read that changes nothing writes
// nothing. A failed Refresh is logged once per cause. The daemon's stop
// cancels a Refresh in flight with the rest of the work off the loop, then
// closes the client within the drain (closeStaging). Nothing here deploys:
// no Plan, no Deploy.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/prstatus"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// Staging is the staging status the factory reads: internal/staging's
// Client, its read side alone, and when it last read a repository (ReadAt:
// the last fetch of the repository's mirror that worked; zero before one),
// which NewStaging adds.
type Staging interface {
	Refresh(ctx context.Context, repo string, open []staging.OpenPR) error
	Status(pr staging.PR, envs []staging.Env) []staging.EnvStatus
	ReadAt(repo string) time.Time
	Close()
}

// NewStaging is the factory daemon's staging client: internal/staging's on
// d, with the repositories' envs from cfg (config.Stagings), the time of
// each mirror's last fetch that worked (ReadAt; a Refresh within the minute,
// or one that took another process's read, fetches nothing), and its
// fetch's git env from gitEnv on the context of the Refresh that asks for it,
// which the daemon's stop cancels (an App's token mint ends with the drain).
func NewStaging(cfg *config.Config, d staging.Deps, gitEnv func(ctx context.Context, repo string) map[string]string) Staging {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	fc := &fetchClock{Git: d.Git, now: now, at: map[string]time.Time{}}
	c := &stagingClient{mirror: d.Mirror, git: fc, gitEnv: gitEnv, calls: map[string]context.Context{}}
	d.Git, d.GitEnv = fc, c.env
	d.Stagings = func(repo string) []staging.Env { return stagingEnvs(cfg.Stagings(repo)) }
	c.Client = staging.New(d)
	return c
}

// stagingClient is NewStaging's client: internal/staging's, the clock of
// its fetches, and the contexts of the Refreshes in flight by repository in
// lower case (the engine runs one per repository at a time).
type stagingClient struct {
	*staging.Client
	mirror func(repo string) (dir string, ok bool)
	git    *fetchClock
	gitEnv func(ctx context.Context, repo string) map[string]string

	mu    sync.Mutex
	calls map[string]context.Context
}

// Refresh is internal/staging's, its context kept for its git env.
func (c *stagingClient) Refresh(ctx context.Context, repo string, open []staging.OpenPR) error {
	k := strings.ToLower(repo)
	c.mu.Lock()
	c.calls[k] = ctx //nolint:fatcontext // the call's own context, kept for its git env until it returns, never derived from
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.calls, k)
		c.mu.Unlock()
	}()
	return c.Client.Refresh(ctx, repo, open)
}

// env is staging.Deps.GitEnv: gitEnv on the context of repo's Refresh in
// flight (none: one that never ends by itself).
func (c *stagingClient) env(repo string) map[string]string {
	if c.gitEnv == nil {
		return nil
	}
	c.mu.Lock()
	ctx := c.calls[strings.ToLower(repo)]
	c.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return c.gitEnv(ctx, repo)
}

// ReadAt is the time of the last fetch of repo's mirror that worked, zero
// before one or without a mirror.
func (c *stagingClient) ReadAt(repo string) time.Time {
	dir, ok := c.mirror(repo)
	if !ok {
		return time.Time{}
	}
	return c.git.lastAt(dir)
}

// fetchClock is staging.Git with the time of each mirror's last Fetch that
// worked, by its dir.
type fetchClock struct {
	staging.Git
	now func() time.Time

	mu sync.Mutex
	at map[string]time.Time
}

// Fetch is the Git's, its time kept when it worked.
func (f *fetchClock) Fetch(ctx context.Context, dir string, env map[string]string, refspecs []string) error {
	err := f.Git.Fetch(ctx, dir, env, refspecs)
	if err == nil {
		f.mu.Lock()
		f.at[dir] = f.now()
		f.mu.Unlock()
	}
	return err
}

// lastAt is dir's last Fetch that worked, zero before one.
func (f *fetchClock) lastAt(dir string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at[dir]
}

// stagingRefreshBound bounds one Refresh: a fetch that hangs ends there,
// and the next tick starts another. The first Refresh of a repository
// clones the mirror on a context of its own and returns.
const stagingRefreshBound = 10 * time.Minute

// The backoff of a repository whose Refresh panicked: a minute after the
// first panic in a row, twice as long after each next one, at most
// stagingPanicMax.
const (
	stagingPanicFirst = time.Minute
	stagingPanicMax   = 30 * time.Minute
)

// stagingReads is the engine's record of the staging status, behind its
// mutex: the repositories whose Refresh runs, the last real read of each
// (a Refresh that returned nil after a fetch), the cause of its last
// failure as logged, its panics in a row and when its next Refresh may
// start after them, by repository in lower case; the published tasks' cells
// as the tick last made them, with since when each env's status is unknown,
// by task; the staging.json the daemon last wrote, and the cause of its last
// failed write.
type stagingReads struct {
	mu      sync.Mutex
	running map[string]bool
	read    map[string]time.Time
	cause   map[string]string
	panics  map[string]int
	retryAt map[string]time.Time

	tasks map[int64]*stagingTaskMem

	written  []byte
	saved    bool
	writeErr string
}

// stagingTaskMem is what the tick keeps of a published task's staging
// status: its PR, its cells (each with the read that first saw its state)
// and since when each env's status is unknown (zero: it is known).
type stagingTaskMem struct {
	pr      int
	cells   []stagingFileCell
	unknown []time.Time
}

func newStagingReads() stagingReads {
	return stagingReads{running: map[string]bool{}, read: map[string]time.Time{}, cause: map[string]string{},
		panics: map[string]int{}, retryAt: map[string]time.Time{}, tasks: map[int64]*stagingTaskMem{}}
}

// taskStaging is a published task's PR's staging status this tick: its PR,
// each env's status in the config's order, and since when each is unknown
// (zero: it is known).
type taskStaging struct {
	ref     PRRef
	envs    []staging.EnvStatus
	unknown []time.Time
}

// stagingEnvs are a repository's staging envs as internal/staging takes
// them, in the config's order.
func stagingEnvs(list []config.FactoryStaging) []staging.Env {
	out := make([]staging.Env, len(list))
	for i, s := range list {
		out[i] = staging.Env{Name: s.Name, Branch: s.Branch}
	}
	return out
}

// stagingPass reads the staging status of the published tasks' PRs (see
// the file's comment): refs are their PRs (publishedRefs), states magnum's
// view of them (followPRs; nil: none read, and a PR's base is its
// repository's). It writes the screen's staging.json when a cell changed,
// and returns each task's statuses, by task id; none without a staging
// client.
func (e *Engine) stagingPass(tasks []registry.Task, refs map[int64]PRRef, states map[PRRef]PRState) map[int64]taskStaging {
	out := map[int64]taskStaging{}
	if e.d.Staging != nil {
		now := e.now()
		open := map[string][]staging.OpenPR{}
		var repos []string
		for _, t := range tasks {
			ref, ok := refs[t.ID]
			if !ok {
				continue
			}
			repo, ok := e.cfg.FactoryRepo(t.Repo)
			if !ok || len(repo.Staging) == 0 {
				continue
			}
			base := cmp.Or(states[ref].BaseRef, repo.Base)
			if _, seen := open[repo.Repo]; !seen {
				repos = append(repos, repo.Repo)
			}
			open[repo.Repo] = append(open[repo.Repo], staging.OpenPR{Number: ref.Number, Base: base})
			pr := staging.PR{Repo: repo.Repo, Number: ref.Number, Base: base}
			envs := e.d.Staging.Status(pr, stagingEnvs(repo.Staging))
			out[t.ID] = taskStaging{ref: ref, envs: envs, unknown: e.unknownSince(t.ID, ref.Number, envs, now)}
		}
		for _, repo := range repos {
			e.startRefresh(repo, open[repo])
		}
	}
	e.saveStagings(tasks, out)
	return out
}

// publishedRefs are the published tasks' PRs this tick, by task id: as
// each task's link names it (the task's repository and number without one).
// A task whose link cannot be read is left out, which is logged.
func (e *Engine) publishedRefs(ctx context.Context, tasks []registry.Task) map[int64]PRRef {
	refs := map[int64]PRRef{}
	for _, t := range tasks {
		if t.State != registry.TaskPublished || t.PRNumber <= 0 {
			continue
		}
		ref := PRRef{Repo: t.Repo, Number: t.PRNumber}
		if l, err := e.reg.LinkedPR(ctx, t.ID); err == nil && l.Number > 0 {
			ref = PRRef{Repo: cmp.Or(l.Repo, t.Repo), Number: l.Number}
		} else if err != nil && !errors.Is(err, registry.ErrNotFound) {
			e.log.Warn("follow a published task's PR: read its link", "task", t.ID, "err", err)
			continue
		}
		refs[t.ID] = ref
	}
	return refs
}

// unknownSince is since when each of task id's envs (its PR number pr) has
// an unknown status at now, kept from tick to tick: zero for one that is
// known.
func (e *Engine) unknownSince(id int64, pr int, envs []staging.EnvStatus, now time.Time) []time.Time {
	e.stg.mu.Lock()
	defer e.stg.mu.Unlock()
	m := e.stg.tasks[id]
	if m == nil || m.pr != pr || len(m.unknown) != len(envs) {
		m = &stagingTaskMem{pr: pr, unknown: make([]time.Time, len(envs))}
		e.stg.tasks[id] = m
	}
	for i, s := range envs {
		switch {
		case s.State != staging.StateUnknown:
			m.unknown[i] = time.Time{}
		case m.unknown[i].IsZero():
			m.unknown[i] = now
		}
	}
	return append([]time.Time(nil), m.unknown...)
}

// startRefresh starts repo's Refresh of open off the loop, on the engine's
// background context (a drain cancels it and waits), bounded by
// stagingRefreshBound, under a recover (refreshPanicked); none while one
// runs, the daemon drains, the disk hold is in force (the first Refresh
// clones the whole history) or a panic's backoff lasts.
func (e *Engine) startRefresh(repo string, open []staging.OpenPR) {
	if e.draining.Load() || e.holds[HoldDisk] != nil {
		return
	}
	k := strings.ToLower(repo)
	e.stg.mu.Lock()
	if e.stg.running[k] || e.now().Before(e.stg.retryAt[k]) {
		e.stg.mu.Unlock()
		return
	}
	e.stg.running[k] = true
	e.stg.mu.Unlock()
	before := e.d.Staging.ReadAt(repo)
	e.bg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				e.refreshPanicked(repo, r, debug.Stack())
			}
		}()
		ctx, cancel := context.WithTimeout(e.bgCtx, stagingRefreshBound)
		defer cancel()
		err := e.d.Staging.Refresh(ctx, repo, open)
		e.refreshed(repo, before, err)
	})
}

// refreshed records the end of repo's Refresh, err its error, before the
// read time it started on: one that returned nil after it fetched (ReadAt
// moved) is the repository's last real read; any that returned nil ends a
// panic's backoff. A failure is logged once while its cause stays the same
// (sameWords: its numbers aside), a cut by the daemon's stop not at all.
func (e *Engine) refreshed(repo string, before time.Time, err error) {
	k := strings.ToLower(repo)
	var at time.Time
	if err == nil {
		at = e.d.Staging.ReadAt(repo)
	}
	e.stg.mu.Lock()
	defer e.stg.mu.Unlock()
	delete(e.stg.running, k)
	if err == nil {
		if e.stg.cause[k] != "" {
			e.log.Info("the staging status of a repository is read again", "repo", repo)
		}
		if at.After(before) {
			e.stg.read[k] = at
		}
		e.stg.cause[k], e.stg.panics[k] = "", 0
		delete(e.stg.retryAt, k)
		return
	}
	if e.bgCtx.Err() != nil {
		return // the daemon stops: a cut Refresh is no failure
	}
	msg := textx.FirstLine(execx.Redact(err.Error()))
	if prev := e.stg.cause[k]; prev != "" && sameWords(prev, msg) {
		return
	}
	e.stg.cause[k] = msg
	e.log.Warn("the staging status of a repository is not read", "repo", repo, "err", msg)
}

// refreshPanicked records repo's Refresh that panicked with r (stack its
// stack): the daemon goes on, the next Refresh of the repository waits a
// backoff that doubles with each panic in a row (stagingPanicFirst, at most
// stagingPanicMax), and the panic is logged once while it repeats.
func (e *Engine) refreshPanicked(repo string, r any, stack []byte) {
	k := strings.ToLower(repo)
	now := e.now()
	e.stg.mu.Lock()
	defer e.stg.mu.Unlock()
	delete(e.stg.running, k)
	e.stg.panics[k]++
	wait := stagingPanicMax
	if n := e.stg.panics[k]; n < 6 {
		wait = min(stagingPanicFirst<<(n-1), stagingPanicMax)
	}
	e.stg.retryAt[k] = now.Add(wait)
	msg := "panic: " + textx.FirstLine(execx.Redact(fmt.Sprint(r)))
	if prev := e.stg.cause[k]; prev != "" && sameWords(prev, msg) {
		return
	}
	e.stg.cause[k] = msg
	e.log.Error("a staging Refresh panicked; the daemon goes on, and the next waits", "repo", repo, "panic", msg, "wait", wait,
		"stack", execx.Redact(string(stack)))
}

// stagingBusy reports whether a Refresh the engine started still runs.
func (e *Engine) stagingBusy() bool {
	e.stg.mu.Lock()
	defer e.stg.mu.Unlock()
	return len(e.stg.running) > 0
}

// closeStaging closes the staging client as the daemon stops, after the
// work off the loop (its Refreshes) ended: it ends the mirror's clone and
// lets go of what the client holds, within what is left of the drain's
// wait.
func (e *Engine) closeStaging() {
	if e.d.Staging == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		e.d.Staging.Close()
		close(done)
	}()
	wait := e.drainWaitNow()
	select {
	case <-done:
	case <-time.After(wait):
		e.log.Warn("the staging client did not close within the drain's wait; the daemon stops anyway", "wait", wait)
	}
}

// stagingState is the screen's word for env status s: the painter's states
// (tui.StagingCell). A pushed PR's cell says where its deploy is: CI while
// ci.yml runs or failed on another change, ci-fail when it failed on the
// PR's own, deploying once it passed and no deployment confirms it yet.
func stagingState(s staging.EnvStatus) string {
	switch s.State {
	case staging.StateLatest:
		return "latest"
	case staging.StatePushed:
		switch s.Detail {
		case staging.DetailCIRunning, staging.DetailCIFailedNotOurs:
			return "ci"
		case staging.DetailCIFailedOurs:
			return "ci-fail"
		}
		return "deploying"
	case staging.StateOutdated:
		return "outdated"
	case staging.StateNone:
		return "none"
	}
	return "unknown"
}

// stagingScreen is staging.json (paths.Layout.FactoryStaging): the staging
// cells of the published tasks' PRs as the factory daemon last read them,
// by task id, each with its PR's number; the screen draws them. Its fields
// are named here, never by the screen's Go types, so a daemon and a screen
// of two builds read one file.
type stagingScreen struct {
	Tasks map[int64]stagingScreenTask `json:"tasks"`
}

// stagingScreenTask is a task's entry of staging.json.
type stagingScreenTask struct {
	PR    int               `json:"pr"`
	Cells []stagingFileCell `json:"cells"`
}

// stagingFileCell is one cell of staging.json: its env's name, its state in
// the painter's words (stagingState) and the time of the read that first
// saw it (zero before a read).
type stagingFileCell struct {
	Name  string    `json:"name"`
	State string    `json:"state"`
	Seen  time.Time `json:"seen"`
}

// saveStagings writes staging.json from the tick's statuses st of tasks
// when a cell changed since this daemon's last write, its first included
// unless the file already says the same. A cell keeps its Seen while its
// state stays; a cell whose state changed, or that has no Seen yet, takes
// its repository's last real read. So a tick, or a read, that changes no
// state writes nothing. The file is rebuilt at each daemon's first tick,
// so it goes without the syncs (fsx.ReplaceFile). A failed write is logged
// once until a write works, and tried again on the next tick.
func (e *Engine) saveStagings(tasks []registry.Task, st map[int64]taskStaging) {
	f := stagingScreen{Tasks: map[int64]stagingScreenTask{}}
	e.stg.mu.Lock()
	for _, t := range tasks {
		ts, ok := st[t.ID]
		if !ok {
			continue
		}
		m := e.stg.tasks[t.ID]
		read := e.stg.read[strings.ToLower(t.Repo)]
		cells := make([]stagingFileCell, len(ts.envs))
		for i, s := range ts.envs {
			c := stagingFileCell{Name: s.Env.Name, State: stagingState(s), Seen: read}
			if m != nil && m.pr == ts.ref.Number && i < len(m.cells) {
				if p := m.cells[i]; p.Name == c.Name && p.State == c.State && !p.Seen.IsZero() {
					c.Seen = p.Seen
				}
			}
			cells[i] = c
		}
		if m != nil {
			m.cells = cells
		}
		f.Tasks[t.ID] = stagingScreenTask{PR: ts.ref.Number, Cells: cells}
	}
	for id := range e.stg.tasks { // a task no longer published keeps nothing
		if _, ok := st[id]; !ok {
			delete(e.stg.tasks, id)
		}
	}
	saved, written := e.stg.saved, e.stg.written
	e.stg.mu.Unlock()
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	b = append(b, '\n')
	path := e.d.Layout.FactoryStaging()
	if !saved {
		if old, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(old, b) {
			saved, written = true, b
		}
	}
	if saved && bytes.Equal(b, written) {
		e.stg.mu.Lock()
		e.stg.saved, e.stg.written = true, b
		e.stg.mu.Unlock()
		return
	}
	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		write := e.stagingWrite
		if write == nil {
			write = func(path string, b []byte) error { return fsx.ReplaceFile(path, b, 0o600) }
		}
		err = write(path, b)
	}
	e.stg.mu.Lock()
	defer e.stg.mu.Unlock()
	if err != nil {
		if msg := textx.FirstLine(execx.Redact(err.Error())); !sameWords(msg, e.stg.writeErr) {
			e.stg.writeErr = msg
			e.log.Warn("save the staging cells for the screen", "err", msg)
		}
		return
	}
	e.stg.saved, e.stg.written, e.stg.writeErr = true, b, ""
}

// readStagingScreen is staging.json in layout l as the factory daemon last
// wrote it: no entry before its first tick, and none for a file that does
// not read (the next daemon writes it whole again).
func readStagingScreen(l paths.Layout) map[int64]stagingScreenTask {
	b, err := os.ReadFile(l.FactoryStaging())
	if err != nil {
		return nil
	}
	var f stagingScreen
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return f.Tasks
}

// stagingCellsOf are task t's row's staging cells, from staging.json
// (tasks): its published PR's (number n) when its repository has staging
// envs in cfg, each cell's age the time since the read that first saw its
// state. While no factory daemon runs (down), every cell is unknown: no
// one reads them. None for another PR's entry or a task without one.
func stagingCellsOf(cfg *config.Config, tasks map[int64]stagingScreenTask, t registry.Task, n int, down bool) []tui.StagingCell {
	ts, ok := tasks[t.ID]
	if !ok || t.State != registry.TaskPublished || n <= 0 || ts.PR != n || len(cfg.Stagings(t.Repo)) == 0 {
		return nil
	}
	cells := make([]tui.StagingCell, len(ts.Cells))
	for i, c := range ts.Cells {
		cells[i] = tui.StagingCell{Name: c.Name, State: c.State, Seen: c.Seen}
		if down {
			cells[i].State = "unknown"
		}
	}
	return cells
}

// prStagings are a published PR's stagings in its status file's words at
// now, from its statuses this tick (ts): each env's state (prStagingState),
// its Env through prstatus.StagingEnv, in the config's order. An env whose
// status is unknown keeps the state old (the file's) had for it at its
// place while it has been unknown for less than staging.StaleAfter (a
// restart, a read that fails for a while, change no word, and so write no
// file); after that, or without such a state, it is none.
func prStagings(ts taskStaging, old []prstatus.Staging, now time.Time) []prstatus.Staging {
	out := make([]prstatus.Staging, len(ts.envs))
	for i, s := range ts.envs {
		out[i] = prstatus.Staging{Env: prstatus.StagingEnv(s.Env.Name), State: prstatus.StagingNone}
		st, ok := prStagingState(s)
		switch {
		case ok:
			out[i].State = st
		case i < len(old) && old[i].Env == out[i].Env && i < len(ts.unknown) && now.Sub(ts.unknown[i]) < staging.StaleAfter:
			out[i].State = old[i].State
		}
	}
	return out
}

// prStagingState is env status s in the status file's words: latest is
// deployed; pushed is deploying, failed when ci.yml failed on the PR's own
// change (one that failed on another change, or cannot be told, is
// deploying: the enum has no word for a deploy that waits on someone
// else's red CI, none would say the PR is not on the branch, and failed
// would blame it); outdated (an older head of the PR) and none are none.
// ok is false for unknown, which has no word.
func prStagingState(s staging.EnvStatus) (prstatus.StagingState, bool) {
	switch s.State {
	case staging.StateLatest:
		return prstatus.StagingDeployed, true
	case staging.StatePushed:
		if s.Detail == staging.DetailCIFailedOurs {
			return prstatus.StagingFailed, true
		}
		return prstatus.StagingDeploying, true
	case staging.StateOutdated, staging.StateNone:
		return prstatus.StagingNone, true
	}
	return "", false
}
