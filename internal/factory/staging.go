package factory

// The staging status of the published tasks' PRs (internal/staging, R27):
// where each PR stands on each [[factory.repo.staging]] environment of its
// repository. The daemon owns one staging client (Deps.Staging), built only
// when a repository has staging envs. Each tick starts, for every such
// repository with a published task's PR, one Refresh off the loop (the
// first clones the mirror; Refresh itself runs at most once a minute), never
// two at a time, and reads each PR's statuses (Status: a pure lookup of the
// last Refresh). They become the PR's staging cells, which the tick writes
// to staging.json (paths.Layout.FactoryStaging) for the screen, another
// process that runs no Refresh and no git, and its status file's stagings
// (pr_status.go). A failed Refresh is logged once per cause. The daemon's
// stop cancels a Refresh in flight with the rest of the work off the loop,
// then closes the client within the drain (closeStaging). Nothing here
// deploys: no Plan, no Deploy.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
// Client (staging.New), its read side alone.
type Staging interface {
	Refresh(ctx context.Context, repo string, open []staging.OpenPR) error
	Status(pr staging.PR, envs []staging.Env) []staging.EnvStatus
	Close()
}

// The factory's client is internal/staging's.
var _ Staging = (*staging.Client)(nil)

// stagingRefreshBound bounds one Refresh: a fetch that hangs ends there,
// and the next tick starts another. The first Refresh of a repository
// clones the mirror on a context of its own and returns.
const stagingRefreshBound = 10 * time.Minute

// stagingReads is the engine's record of the Refreshes: the repositories
// whose Refresh runs, the last good Refresh of each and the cause of its
// last failure as logged, by repository in lower case; the staging.json the
// daemon last wrote, and the cause of its last failed write.
type stagingReads struct {
	mu      sync.Mutex
	running map[string]bool
	good    map[string]time.Time
	cause   map[string]string

	written  []byte
	saved    bool
	writeErr string
}

// taskStaging is a published task's PR's staging status this tick: its PR
// and each env's status in the config's order.
type taskStaging struct {
	ref  PRRef
	envs []staging.EnvStatus
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
// the file's comment), states being magnum's view of them (followPRs; nil:
// none read, and a PR's base is its repository's), and writes the screen's
// staging.json. It returns each task's statuses, by task id; none without a
// staging client.
func (e *Engine) stagingPass(ctx context.Context, tasks []registry.Task, states map[PRRef]PRState) map[int64]taskStaging {
	out := map[int64]taskStaging{}
	if e.d.Staging != nil {
		open := map[string][]staging.OpenPR{}
		var repos []string
		for _, t := range tasks {
			if t.State != registry.TaskPublished || t.PRNumber <= 0 {
				continue
			}
			repo, ok := e.cfg.FactoryRepo(t.Repo)
			if !ok || len(repo.Staging) == 0 {
				continue
			}
			ref, ok := e.publishedRef(ctx, t)
			if !ok {
				continue
			}
			base := cmp.Or(states[ref].BaseRef, repo.Base)
			if _, seen := open[repo.Repo]; !seen {
				repos = append(repos, repo.Repo)
			}
			open[repo.Repo] = append(open[repo.Repo], staging.OpenPR{Number: ref.Number, Base: base})
			pr := staging.PR{Repo: repo.Repo, Number: ref.Number, Base: base}
			out[t.ID] = taskStaging{ref: ref, envs: e.d.Staging.Status(pr, stagingEnvs(repo.Staging))}
		}
		for _, repo := range repos {
			e.startRefresh(repo, open[repo])
		}
	}
	e.saveStagings(tasks, out)
	return out
}

// publishedRef is published task t's PR as its link names it (the task's
// repository and number without one); ok is false when the link cannot be
// read, which is logged.
func (e *Engine) publishedRef(ctx context.Context, t registry.Task) (PRRef, bool) {
	ref := PRRef{Repo: t.Repo, Number: t.PRNumber}
	l, err := e.reg.LinkedPR(ctx, t.ID)
	switch {
	case err == nil && l.Number > 0:
		ref = PRRef{Repo: cmp.Or(l.Repo, t.Repo), Number: l.Number}
	case err != nil && !errors.Is(err, registry.ErrNotFound):
		e.log.Warn("read a published task's staging status: read its PR's link", "task", t.ID, "err", err)
		return PRRef{}, false
	}
	return ref, true
}

// startRefresh starts repo's Refresh of open off the loop, on the engine's
// background context (a drain cancels it and waits), bounded by
// stagingRefreshBound; none while one runs or the daemon drains.
func (e *Engine) startRefresh(repo string, open []staging.OpenPR) {
	if e.draining.Load() {
		return
	}
	k := strings.ToLower(repo)
	e.stg.mu.Lock()
	if e.stg.running[k] {
		e.stg.mu.Unlock()
		return
	}
	e.stg.running[k] = true
	e.stg.mu.Unlock()
	e.bg.Go(func() {
		ctx, cancel := context.WithTimeout(e.bgCtx, stagingRefreshBound)
		defer cancel()
		e.refreshed(repo, e.d.Staging.Refresh(ctx, repo, open))
	})
}

// refreshed records the end of repo's Refresh, err its error: a good one is
// the cells' last good read; a failure is logged once while its cause stays
// the same (sameWords: its numbers aside), a cut by the daemon's stop not at
// all.
func (e *Engine) refreshed(repo string, err error) {
	k := strings.ToLower(repo)
	e.stg.mu.Lock()
	defer e.stg.mu.Unlock()
	delete(e.stg.running, k)
	if err == nil {
		if e.stg.cause[k] != "" {
			e.log.Info("the staging status of a repository is read again", "repo", repo)
		}
		e.stg.good[k], e.stg.cause[k] = e.now(), ""
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

// stagingBusy reports whether a Refresh the engine started still runs.
func (e *Engine) stagingBusy() bool {
	e.stg.mu.Lock()
	defer e.stg.mu.Unlock()
	return len(e.stg.running) > 0
}

// stagingSeen is repo's last good Refresh, zero before one.
func (e *Engine) stagingSeen(repo string) time.Time {
	e.stg.mu.Lock()
	defer e.stg.mu.Unlock()
	return e.stg.good[strings.ToLower(repo)]
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

// stagingCell is the screen's cell of env status s, read last good at seen:
// the painter's states (tui.StagingCell). A pushed PR's cell says where its
// deploy is: CI while ci.yml runs or failed on another change, ci-fail when
// it failed on the PR's own, deploying once it passed and no deployment
// confirms it yet. No cell is on you: only a conflict out of tries is.
func stagingCell(s staging.EnvStatus, seen time.Time) tui.StagingCell {
	c := tui.StagingCell{Name: s.Env.Name, Seen: seen}
	switch s.State {
	case staging.StateLatest:
		c.State = "latest"
	case staging.StatePushed:
		switch s.Detail {
		case staging.DetailCIRunning, staging.DetailCIFailedNotOurs:
			c.State = "ci"
		case staging.DetailCIFailedOurs:
			c.State = "ci-fail"
		default:
			c.State = "deploying"
		}
	case staging.StateOutdated:
		c.State = "outdated"
	case staging.StateNone:
		c.State = "none"
	default:
		c.State = "unknown"
	}
	return c
}

// stagingScreen is staging.json (paths.Layout.FactoryStaging): the staging
// cells of the published tasks' PRs as the factory daemon last read them,
// by task id, each with its PR's number; the screen draws them.
type stagingScreen struct {
	Tasks map[int64]stagingScreenTask `json:"tasks"`
}

// stagingScreenTask is a task's entry of staging.json.
type stagingScreenTask struct {
	PR    int               `json:"pr"`
	Cells []tui.StagingCell `json:"cells"`
}

// saveStagings writes staging.json from the tick's statuses st of tasks,
// when it changed since this daemon's last write (and on its first tick,
// so a restart clears what the last run left). A failed write is logged
// once until a write works, and tried again on the next tick.
func (e *Engine) saveStagings(tasks []registry.Task, st map[int64]taskStaging) {
	f := stagingScreen{Tasks: map[int64]stagingScreenTask{}}
	for _, t := range tasks {
		ts, ok := st[t.ID]
		if !ok {
			continue
		}
		seen := e.stagingSeen(t.Repo)
		cells := make([]tui.StagingCell, len(ts.envs))
		for i, s := range ts.envs {
			cells[i] = stagingCell(s, seen)
		}
		f.Tasks[t.ID] = stagingScreenTask{PR: ts.ref.Number, Cells: cells}
	}
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	e.stg.mu.Lock()
	same := e.stg.saved && bytes.Equal(b, e.stg.written)
	e.stg.mu.Unlock()
	if same {
		return
	}
	path := e.d.Layout.FactoryStaging()
	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
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
// not read (the next tick writes it whole again).
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

// stagingCellsOf are task t's row's staging cells at now, from staging.json
// (tasks): its published PR's (number n) when its repository has staging
// envs in cfg; a cell whose last good read is over staging.StaleAfter old
// (its daemon stopped, or reads fail) is unknown. None for another PR's
// entry or a task without one.
func stagingCellsOf(cfg *config.Config, tasks map[int64]stagingScreenTask, t registry.Task, n int, now time.Time) []tui.StagingCell {
	ts, ok := tasks[t.ID]
	if !ok || t.State != registry.TaskPublished || n <= 0 || ts.PR != n || len(cfg.Stagings(t.Repo)) == 0 {
		return nil
	}
	cells := make([]tui.StagingCell, len(ts.Cells))
	for i, c := range ts.Cells {
		if now.Sub(c.Seen) > staging.StaleAfter && c.State != "unknown" {
			c.State = "unknown"
		}
		cells[i] = c
	}
	return cells
}

// prStagings are a published PR's stagings in its status file's words, from
// its statuses envs this tick: each env's state (prStagingState), its Env
// through prstatus.StagingEnv, in the config's order. An env whose status
// is unknown keeps the state old (the file's) had for it at its place, or
// is none: an unknown read changes no word, and so writes no file.
func prStagings(envs []staging.EnvStatus, old []prstatus.Staging) []prstatus.Staging {
	out := make([]prstatus.Staging, len(envs))
	for i, s := range envs {
		out[i] = prstatus.Staging{Env: prstatus.StagingEnv(s.Env.Name), State: prstatus.StagingNone}
		if st, ok := prStagingState(s); ok {
			out[i].State = st
		} else if i < len(old) && old[i].Env == out[i].Env {
			out[i].State = old[i].State
		}
	}
	return out
}

// prStagingState is env status s in the status file's words: latest is
// deployed; pushed is deploying, failed when ci.yml failed on the PR's own
// change; outdated (an older head of the PR) and none are none. ok is false
// for unknown, which has no word.
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
