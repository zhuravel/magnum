package engine

// The review daemon's deploy side of internal/staging, on the operator's
// word: the staging client's parts the engine gives it (the fixer App's bot
// as the merges' author and the pushes' identity, the events into the
// registry), and the Step of every live deploy, on a goroutine of its own
// every stagingStepEvery (a deploy's git can take minutes), which the
// daemon's stop ends before it closes the client. Nothing of it runs
// without a watched repository's staging envs.

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/staging"
)

// stagingStepEvery is how often the review daemon steps the live staging
// deploys (staging.Client.Step: one step of each).
const stagingStepEvery = 15 * time.Second

// errStagingUnbound is what the engine's parts of the staging client answer
// before FromApp bound them to the engine.
var errStagingUnbound = errors.New("staging: the review daemon is not ready yet")

// stagingBound is the engine's part of the staging client's deps:
// CommitIdentity, PushEnv and Event act as the engine, which FromApp makes
// after the client (Deps.Staging), so it binds them once it exists; before
// that they refuse (an event is dropped).
type stagingBound struct {
	e      atomic.Pointer[Engine]
	mirror func(repo string) (dir string, ok bool) // staging.MirrorDir over [staging] mirror_dir
	// resolver is the client's Resolver (staging_resolve.go), bound with
	// the engine.
	resolver *stagingResolver
}

// bind makes e the engine the parts act as.
func (b *stagingBound) bind(e *Engine) {
	b.e.Store(e)
	if b.resolver != nil {
		b.resolver.bind(e)
	}
}

// commitIdentity is Deps.CommitIdentity: the fixer App's bot
// (Engine.CommitIdentity).
func (b *stagingBound) commitIdentity(ctx context.Context, repo string) (string, string, error) {
	e := b.e.Load()
	if e == nil {
		return "", "", errStagingUnbound
	}
	return e.CommitIdentity(ctx, repo)
}

// pushEnv is Deps.PushEnv: the fixer App's bot's env of one push, by its
// rules (Engine.StagingPushEnv over the mirrors).
func (b *stagingBound) pushEnv(ctx context.Context, repo, merge, target string) (map[string]string, error) {
	e := b.e.Load()
	if e == nil {
		return nil, errStagingUnbound
	}
	return e.StagingPushEnv(b.mirror)(ctx, repo, merge, target)
}

// stagingDepsOf is the review daemon's staging client's deps (stagingFromApp):
// its git in the mirrors under [staging] mirror_dir (stagingRunner), GitHub
// and the fetch's git env as each repository's poll identity, the envs from
// config.Stagings, Step's and Cancel's scan of mirror_dir (MirrorRoot), and
// b's engine parts: the merges by the fixer App's bot, its pushes with the
// bot's env, and the resolver of a conflict (staging_resolve.go), whose
// jobs work in the review slots with the App's git (a.Git).
func stagingDepsOf(a *app.App, b *stagingBound) staging.Deps {
	cfg := a.Config
	ids := &stagingIDs{a: a, warned: map[string]string{}}
	b.mirror = staging.MirrorDir(cfg.Staging.MirrorDir)
	b.resolver = &stagingResolver{}
	if a.Git != nil {
		b.resolver.git = a.Git
	}
	return staging.Deps{
		Git:            staging.NewGit(stagingRunner{a.Runner}),
		GitHub:         staging.NewGitHub(ids.client),
		Mirror:         b.mirror,
		MirrorRoot:     staging.MirrorRoot(cfg.Staging.MirrorDir),
		GitEnv:         ids.gitEnv,
		Stagings:       func(repo string) []staging.Env { return StagingEnvs(cfg, repo) },
		CommitIdentity: b.commitIdentity,
		PushEnv:        b.pushEnv,
		Resolver:       b.resolver,
		Event:          b.event,
		Log:            a.Logger,
	}
}

// goStagingSteps steps the live staging deploys on a goroutine of its own
// until ctx ends: at once (a restart resumes the effects it left), then
// every interval, a Step at a time, a panicking one contained and the next
// waiting a backoff (stagingBackoff). Its channel closes when it has
// stopped; at once when the daemon has no staging client or no repository
// with staging envs.
func (e *Engine) goStagingSteps(ctx context.Context, every time.Duration) <-chan struct{} {
	done := make(chan struct{})
	s := e.d.Staging
	if s == nil || every <= 0 || len(StagingRepos(e.cfg)) == 0 {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		panics := 0
		for {
			panicked := e.safely(ctx, "staging step", "", func() { e.noteStagingStep(ctx, s.Step(ctx)) }, nil)
			wait := every
			if panicked {
				panics++
				wait = stagingBackoff(panics)
			} else {
				panics = 0
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
	return done
}

// stagingBackoff is the wait after the n-th panic in a row of a staging
// Refresh or Step: a minute, twice as long after each next one, at most
// stagingPanicMax.
func stagingBackoff(n int) time.Duration {
	if n <= 6 {
		return min(stagingPanicFirst<<(n-1), stagingPanicMax)
	}
	return stagingPanicMax
}

// noteStagingStep logs a failed Step once while its cause stays the same,
// and that the Steps run again after one.
func (e *Engine) noteStagingStep(ctx context.Context, err error) {
	if err != nil && isStop(ctx, err) {
		return
	}
	msg := ""
	if err != nil {
		msg = execx.Redact(err.Error())
	}
	e.stagingMu.Lock()
	last := e.stagingStepLogged
	e.stagingStepLogged = msg
	e.stagingMu.Unlock()
	switch {
	case msg != "" && msg != last:
		e.log.Warn("staging: a deploy step failed; its effect says why", "err", msg)
	case msg == "" && last != "":
		e.log.Info("staging: the deploy steps run again")
	}
}
