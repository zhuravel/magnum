package evalrun

// A case's worktree (caseWorktree): a detached worktree of the
// repository's clone at the case's head, posted and local cases alike,
// prepared the way a per-PR review checkout of its repository is
// (internal/slots), so that a replay runs what a real round runs, and torn
// down before it goes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/dblock"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// caseLogName is the transcript of case name's worktree commands, its
// setup and teardown, under the install's logs (slots.Manager.RunLogged).
func caseLogName(name string) string { return "eval-case-" + name + ".log" }

// prepareLock is the lock a case's preparation holds while its copy_files
// and setup run: RunsRoot/locks/prepare.lock, a dblock flock, so that
// whoever replays (`magnum eval run`, the daemon's gate) prepares one case
// at a time, and a replay that crashes or is killed frees it.
func prepareLock(l paths.Layout) string { return filepath.Join(RunsRoot(l), "locks", "prepare.lock") }

// prepareWait bounds a case's wait for another case's preparation: the
// slots' setup timeout of one command.
var prepareWait = slots.SetupTimeout

// worktreeRemoveTimeout bounds the removal of a case's worktree after its
// replay.
const worktreeRemoveTimeout = time.Minute

// hookPostCheckout labels a pool's post_checkout commands in a case
// worktree's transcript.
const hookPostCheckout = "post_checkout"

// caseSlug is the workspace name of case worktree wt of PR (or synthetic)
// number: magnum-eval-<N>-<the first 6 hex digits of wt's SHA-256>, the
// WT_BRANCH its setup, round and teardown run as. So the databases a setup
// names from it are the worktree's own: never the PR's own per-PR
// worktree's (magnum-pr-<N>), a sibling case's of the PR, or the same
// case's in the other worktree directory (Options.Worktrees), and the same
// at every replay in wt, so the teardown of a kept worktree reaches the
// databases it kept. It stays whole in talkable's database names, which
// keep 30 characters of it (slots.DBSlug), for a number of up to 11 digits.
func caseSlug(number int, wt string) string {
	sum := sha256.Sum256([]byte(wt))
	return "magnum-eval-" + strconv.Itoa(number) + "-" + hex.EncodeToString(sum[:3])
}

// casePrep is how case name's worktree wt, of clone, is prepared and torn
// down (planCasePrep): plan is a per-PR worktree's under the worktree's
// own workspace name (caseSlug; slots.PlanPerPR: WT_BRANCH, the [[repo]]
// env, its copy_files, strip_env, setup and teardown, else the main clone's
// .config/wt.toml hooks), or, for a repository with a [[pool]], the pool's
// copy_files, strip_env, setup and teardown with the pool's env under that
// name ({slot} as it, WT_BRANCH and the workspace-name variables a per-PR
// worktree's), and post its post_checkout (pool: its scripts run through
// mise exec, as the pool's slots run them). plan.Env is also the round's
// panes' env. source is the clone the copy_files and the local mise file
// come from: the pool's main clone, else clone. m runs the commands.
type casePrep struct {
	name, wt, clone, source string
	plan                    slots.PerPRPlan
	post                    []slots.Hook
	pool                    bool
	m                       *slots.Manager
}

// planCasePrep plans case name's worktree wt of clone as a per-PR review
// checkout of repo (owner/name) for PR number based on base, its commands
// run through run. A wt.toml that cannot be read, parsed or templated is
// slots.ErrHook.
func (d Deps) planCasePrep(cfg *config.Config, run execx.Runner, name, repo string, number int, wt, clone, base string) (casePrep, error) {
	p := casePrep{name: name, wt: wt, clone: clone, source: clone,
		m: slots.New(slots.Deps{Run: run, Layout: d.Layout, Repos: cfg.Repos, SecretEnv: keyEnvs(cfg), LookPath: d.lookPath})}
	slug := caseSlug(number, wt)
	rc := cfg.RepoFor(repo)
	pool := cfg.PoolFor(repo)
	if pool == nil {
		var err error
		p.plan, err = slots.PlanPerPR(rc, repo, slug, wt, clone, base)
		return p, err
	}
	// The pool's env, as its slots' scripts and panes have it ({slot} as the
	// worktree's name: FAKE_AWS and the like stay), with the per-PR names
	// over it, so nothing picks a slot's databases.
	env := pool.SlotEnv(slug)
	maps.Copy(env, slots.WorktreeEnv(rc, slug, wt, clone))
	p.pool, p.source = true, pool.MainClone
	p.plan = slots.PerPRPlan{Source: "[[pool]] " + pool.Repo, Slug: slug, Env: env,
		Copy: slices.Clone(pool.CopyFiles), Strip: slots.StripKeys(pool.StripEnv)}
	for i, c := range pool.Setup {
		p.plan.Setup = append(p.plan.Setup, slots.Hook{Type: slots.HookSetup, Name: strconv.Itoa(i + 1), Command: c})
	}
	for i, c := range pool.PostCheckout {
		p.post = append(p.post, slots.Hook{Type: hookPostCheckout, Name: strconv.Itoa(i + 1), Command: c})
	}
	for i, c := range pool.Teardown {
		p.plan.Teardown = append(p.plan.Teardown, slots.Hook{Type: slots.HookTeardown, Name: strconv.Itoa(i + 1), Command: c})
	}
	return p, nil
}

// keyEnvs are the identities' private_key_env names: the variables of the
// process's environment that hold an App's private key, which a case
// worktree's commands never inherit (slots.Deps.SecretEnv, as the app's).
func keyEnvs(cfg *config.Config) []string {
	var out []string
	for _, id := range cfg.Identities {
		if id.PrivateKeyEnv != "" {
			out = append(out, id.PrivateKeyEnv)
		}
	}
	return out
}

// bare reports whether the repository has nothing to copy or run: its case
// worktree stays as git adds it, as every replay's did before.
func (p casePrep) bare() bool {
	return len(p.plan.Copy) == 0 && len(p.plan.Setup) == 0 && len(p.post) == 0 && len(p.plan.Teardown) == 0
}

// render renders the source clone's local mise file into the worktree
// with the plan's env and without its strip keys, and copies the
// copy_files that exist (slots.Manager.PreparePerPR).
func (p casePrep) render(ctx context.Context) error {
	return p.m.PreparePerPR(ctx, store.Slot{Name: "eval " + p.name, Path: p.wt, MainClone: p.source}, p.plan)
}

// run runs hook h in the worktree with the plan's env under timeout,
// through the Manager's heavy lock and with the case's transcript: a
// pool's script through mise exec, a per-PR hook as the slots run one
// (mise exec when mise is there, else /bin/sh).
func (p casePrep) run(ctx context.Context, h slots.Hook, timeout time.Duration) error {
	label := fmt.Sprintf("%s eval %s %s: %s", h.Type, p.name, h.Name, h.Command)
	if p.pool {
		return p.m.RunHeavy(ctx, p.wt, p.plan.Env, h.Command, label, caseLogName(p.name), timeout)
	}
	return p.m.RunLogged(ctx, p.m.ShellCmd(p.wt, p.plan.Env, h.Command, label, timeout), caseLogName(p.name))
}

// prepare prepares the case's worktree as the slots prepare a per-PR one
// and check one out: the local mise file of the source clone rendered with
// the plan's env and without its strip keys, the copy_files that exist
// copied from the source clone (render), then each setup command in
// order, each under the slots' setup timeout, then the render again (a
// setup may write the local mise file over the rendered one, as
// talkable's copies the main clone's, and a slot's checkout renders it
// before each round: the strip keys never reach the round), then a pool's
// post_checkout under the slots' deps timeout (a fresh checkout at the head
// has none of its installs, which a slot's checkout runs when the head's
// lock files differ from those it last installed); the first failure stops
// it. It holds prepareLock throughout, waiting at most prepareWait for
// another case's preparation (saying once whom it waits for). setUp is
// whether it got past the first render, so the worktree has anything to
// tear down. A failure is the case's environment (eval.OutcomeEnvironment),
// its error naming the command and its first error line; a cancelled ctx is
// the replay's error (OutcomeError). Nothing runs for a bare plan.
func (d Deps) prepare(ctx context.Context, p casePrep, o Options) (setUp bool, outcome string, err error) {
	if p.bare() {
		return false, "", nil
	}
	me := dblock.Holder{Command: "prepare " + p.name + " (run " + filepath.Base(o.RunDir) + ")", PID: os.Getpid()}
	lock, err := dblock.Acquire(ctx, prepareLock(d.Layout), me, prepareWait, func(h dblock.Holder) {
		d.warn("case %s: waiting for another case's worktree to be prepared (%s)", p.name, h)
	})
	var held *dblock.TimeoutError
	switch {
	case errors.As(err, &held):
		return false, eval.OutcomeEnvironment, fmt.Errorf("another case's worktree was being prepared for %s (%s): this one was not", prepareWait, held.Holder)
	case err != nil && ctx.Err() != nil:
		return false, OutcomeError, fmt.Errorf("waiting to prepare the worktree %s: %w", tilde(p.wt), ctx.Err())
	case err != nil:
		return false, eval.OutcomeEnvironment, fmt.Errorf("prepare the worktree %s: %w", tilde(p.wt), err)
	}
	defer lock.Release()
	d.progress("preparing %s as a review checkout of %s (%s, %s)", tilde(p.wt), cmpSource(p.plan.Source), p.plan.Slug, tilde(p.source))
	if err := p.render(ctx); err != nil {
		return false, eval.OutcomeEnvironment, fmt.Errorf("render the local mise file and copy_files into the worktree %s: %s", tilde(p.wt), textx.FirstLine(err.Error()))
	}
	logPath := tilde(filepath.Join(d.Layout.Logs(), caseLogName(p.name)))
	for _, h := range p.plan.Setup {
		if err := p.run(ctx, h, slots.SetupTimeout); err != nil {
			if ctx.Err() != nil {
				return true, OutcomeError, fmt.Errorf("setup `%s` was cut short: %w", h.Command, ctx.Err())
			}
			return true, eval.OutcomeEnvironment, fmt.Errorf("setup `%s` failed: %s\nlog: %s", h.Command, firstErrorLine(err, slots.SetupTimeout), logPath)
		}
	}
	if len(p.plan.Setup) > 0 {
		if err := p.render(ctx); err != nil {
			return true, eval.OutcomeEnvironment, fmt.Errorf("render the local mise file and copy_files into the worktree %s again after its setup: %s",
				tilde(p.wt), textx.FirstLine(err.Error()))
		}
	}
	for _, h := range p.post {
		if err := p.run(ctx, h, slots.DepsTimeout); err != nil {
			if ctx.Err() != nil {
				return true, OutcomeError, fmt.Errorf("post_checkout `%s` was cut short: %w", h.Command, ctx.Err())
			}
			return true, eval.OutcomeEnvironment, fmt.Errorf("post_checkout `%s` failed: %s\nlog: %s", h.Command, firstErrorLine(err, slots.DepsTimeout), logPath)
		}
	}
	return true, "", nil
}

// cmpSource is a plan's source for the progress line: "its config" when
// it has no commands of its own (copy_files alone).
func cmpSource(source string) string {
	if source == "" {
		return "its config"
	}
	return source
}

// teardown runs the case's teardown commands in its worktree, each under
// the slots' teardown timeout, whatever ctx says (a replay cut short tears
// its worktree down too): a failure is said (d.warn) and the next one runs,
// as the slots tear a per-PR worktree down. A worktree that is gone, or a
// plan without teardown, runs nothing.
func (d Deps) teardown(ctx context.Context, p casePrep) {
	if len(p.plan.Teardown) == 0 || !fsx.Exists(p.wt) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, h := range p.plan.Teardown {
		if err := p.run(ctx, h, slots.TeardownTimeout); err != nil {
			d.warn("case %s: teardown `%s` failed (the worktree goes anyway): %s", p.name, h.Command, firstErrorLine(err, slots.TeardownTimeout))
		}
	}
}

// firstErrorLine is what a failed command says in one line: its exit
// status and the first line of its error output that is not blank, "timed
// out after <timeout>" for one its timeout ended, else the first line of
// the error itself. It never repeats the command line, which carries the
// worktree's env.
func firstErrorLine(err error, timeout time.Duration) string {
	if ee, ok := errors.AsType[*execx.ExitError](err); ok {
		if line := textx.FirstLine(ee.Stderr); line != "" {
			return fmt.Sprintf("exit %d: %s", ee.Code, line)
		}
		return fmt.Sprintf("exit %d", ee.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out after " + timeout.String()
	}
	if re, ok := errors.AsType[*execx.RunError](err); ok {
		return textx.FirstLine(re.Err.Error())
	}
	return textx.FirstLine(err.Error())
}

// addCaseWorktree adds case name's worktree (caseWorktree) of clone at
// head, detached, and prepares it (prepare) as a per-PR review checkout of
// repo for PR number based on base, its commands run through run, under
// the worktree's own workspace name (caseSlug): p is its plan, whose Env
// the round's panes get. A worktree an earlier run left (Keep, an
// interrupted run) is torn down and removed first, so a kept worktree
// keeps its databases until it goes. done tears the worktree down, once
// its preparation got as far as anything to tear down, and removes it
// (nothing with o.Keep): the caller runs it whatever happens next, a failed
// preparation included; it is never nil. A case that cannot be prepared is
// outcome (eval.OutcomeEnvironment, or OutcomeError for the replay's own
// failure) with err.
func (d Deps) addCaseWorktree(ctx context.Context, cfg *config.Config, git *gitx.Client, run execx.Runner, o Options,
	name, repo string, number int, clone, head, base string) (p casePrep, done func(), outcome string, err error) {
	done = func() {}
	wt := caseWorktree(d, o, name)
	p, planErr := d.planCasePrep(cfg, run, name, repo, number, wt, clone, base)
	if planErr != nil {
		return p, done, eval.OutcomeEnvironment, fmt.Errorf("prepare the worktree as a review checkout of %s: %w", repo, planErr)
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o700); err != nil {
		return p, done, OutcomeError, err
	}
	// One path per case across runs (of `magnum eval run`, or of the
	// gate's, Options.Worktrees): the agents trust the checkout they start
	// in (Codex's and Claude's own config files), and a new path per run
	// would add an entry there every time. A worktree left by Keep or an
	// interrupted run is torn down and replaced.
	if _, err := os.Stat(wt); err == nil {
		d.teardown(ctx, p)
		if err := git.WorktreeRemove(ctx, clone, wt, true); err != nil {
			return p, done, OutcomeError, fmt.Errorf("replace the previous worktree %s: %w", tilde(wt), err)
		}
	}
	if err := git.WorktreeAdd(ctx, clone, wt, head, true, ""); err != nil {
		return p, done, OutcomeError, err
	}
	setUp := false
	if !o.Keep {
		done = func() {
			if setUp {
				d.teardown(ctx, p)
			}
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeRemoveTimeout)
			defer cancel()
			if err := git.WorktreeRemove(cctx, clone, wt, true); err != nil {
				d.warn("removing worktree %s: %v", tilde(wt), err)
			}
		}
	}
	setUp, outcome, err = d.prepare(ctx, p, o)
	return p, done, outcome, err
}
