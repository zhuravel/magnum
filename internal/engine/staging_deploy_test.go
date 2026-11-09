package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/staging"
)

// withMirrors puts the staging mirrors under /mirrors ([staging]
// mirror_dir): talkable/talkable's is stagingMirror.
func withMirrors(h *harness) { h.cfg.Staging.MirrorDir = "/mirrors" }

// awaitSteps waits until f was asked n Steps.
func awaitSteps(t *testing.T, f *fakeStaging, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); f.stepCount() < n; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d staging Steps, want %d", f.stepCount(), n)
		}
	}
}

// The review daemon's staging client deploys as the watch's fixer App: its
// merges are the bot's (CommitIdentity), its pushes go with the bot's env
// and the bot's rules (StagingPushEnv, which never pushes to master), Step
// and Cancel find every effect under mirror_dir (MirrorRoot), the envs are
// the config's, and a conflict goes to the engine's resolver
// (staging_resolve.go). The engine's parts are bound once the engine
// exists: before that they refuse, the resolver too.
func TestTheStagingClientDeploysAsTheFixerApp(t *testing.T) {
	h := newHarness(t, withStagings(nil), withFixerApp, withMirrors)
	h.open(prSpec{n: 2, head: fixHead, author: "zhuravel"})
	h.tick()
	a := &app.App{Config: h.cfg, Runner: &execx.Fake{}, Logger: h.d.Logger}
	bound := &stagingBound{}
	d := stagingDepsOf(a, bound)
	switch {
	case d.Resolver == nil || d.Resolver != staging.Resolver(bound.resolver):
		t.Error("the staging client has no resolver of the engine's: a conflict would end the effect")
	case d.MirrorRoot == nil || d.PushEnv == nil || d.CommitIdentity == nil || d.Stagings == nil || d.GitEnv == nil || d.Mirror == nil || d.Event == nil:
		t.Fatal("the staging client lacks a part of the deploy: mirror root, push env, commit identity, envs, git env, mirror or events")
	}
	if root, ok := d.MirrorRoot(); root != "/mirrors" || !ok {
		t.Errorf("MirrorRoot = %q, %v; want /mirrors", root, ok)
	}
	if got := d.Stagings("talkable/talkable"); len(got) != 3 || got[0] != (staging.Env{Name: "void", Branch: "staging-void"}) {
		t.Errorf("Stagings = %v", got)
	}
	if _, _, err := d.CommitIdentity(h.ctx, "talkable/talkable"); err == nil {
		t.Error("CommitIdentity answered before the engine was bound")
	}
	if _, err := d.PushEnv(h.ctx, "talkable/talkable", stagingMerge, "staging-void"); err == nil {
		t.Error("PushEnv answered before the engine was bound")
	}
	if _, err := d.Resolver.Result(h.ctx, "void-2-x.r1.00000000"); err == nil {
		t.Error("the resolver answered before the engine was bound")
	}
	d.Event(testEffect(1, staging.StepClaim, staging.EffectRunning), staging.StepClaim, staging.PhaseOK, "")
	if got := len(h.events("staging.deploy")); got != 0 {
		t.Errorf("%d events before the engine was bound", got)
	}
	bound.bind(h.e)
	d.Event(testEffect(1, staging.StepClaim, staging.EffectRunning), staging.StepClaim, staging.PhaseOK, "")
	if got := len(h.events("staging.deploy")); got != 1 {
		t.Errorf("%d events once bound, want 1", got)
	}
	if name, _, err := d.CommitIdentity(h.ctx, "talkable/talkable"); err != nil || name != fixerAppLogin {
		t.Errorf("CommitIdentity = %q, %v; want the fixer App's bot %s", name, err, fixerAppLogin)
	}
	if _, err := d.PushEnv(h.ctx, "talkable/talkable", stagingMerge, "master"); err == nil || !strings.Contains(err.Error(), "never pushes to master") {
		t.Errorf("PushEnv to master: %v", err)
	}
	if res, err := d.Resolver.Result(h.ctx, "void-2-x.r1.00000000"); err != nil || res.State != staging.ResolveFailed || res.Why != whyUnknownJob {
		t.Errorf("the bound resolver's Result of no job = %+v, %v; want failed, unknown job", res, err)
	}
}

// Step runs on a goroutine of its own, once at the start and then every
// interval, never in the tick: a deploy's git (a fetch, a merge, a push)
// can take minutes. The daemon's stop ends the Step in flight (its ctx) and
// waits for it before it closes the client, also when the Step goes on a
// while after its ctx ended (a push in flight finishes first).
func TestStagingStepsRunOffTheTickAndStopBeforeTheClientCloses(t *testing.T) {
	f := &fakeStaging{}
	h := newHarness(t, withStagings(f))
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.e.goStagingSteps(ctx, time.Millisecond)
	awaitSteps(t, f, 3)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the staging Steps did not stop with their ctx")
	}

	f = &fakeStaging{stepGate: make(chan struct{}), stepLinger: time.Second}
	h = newHarness(t, withStagings(f))
	h.open(prSpec{n: 1, head: "aaa1111"})
	ctx, cancel = context.WithCancel(h.ctx)
	run := h.goRun(ctx, Options{NoSignals: true})
	awaitSteps(t, f, 1) // the first one, at the start, waits on its gate
	for deadline := time.Now().Add(10 * time.Second); daemonTicked(h) == ""; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the daemon did not tick while a staging Step ran")
		}
	}
	cancel()
	select {
	case err := <-run:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the daemon did not stop while a staging Step ran")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed != 1 || f.closedStepping || f.stepping != 0 {
		t.Fatalf("closed %d times, while a Step ran: %v, Steps running: %d", f.closed, f.closedStepping, f.stepping)
	}
}

// daemonTicked is the daemon's last tick as the registry has it, "" before
// the first.
func daemonTicked(h *harness) string {
	v, _ := h.kvOf("daemon.last_tick")
	return v
}

// A Step that panics is contained, and the next one waits a backoff (a
// minute after the first panic), not the interval: a bug costs no panic
// event per interval.
func TestAPanickingStagingStepWaitsABackoff(t *testing.T) {
	f := &fakeStaging{stepPanics: true}
	h := newHarness(t, withStagings(f))
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.e.goStagingSteps(ctx, time.Millisecond)
	awaitSteps(t, f, 1)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if n := f.stepCount(); n != 1 {
		t.Fatalf("%d Steps after a panic within the backoff, want 1", n)
	}
	if got := len(h.events("engine.panic")); got != 1 {
		t.Fatalf("%d panic events, want 1", got)
	}
}

// Without a repository with staging envs nothing of a deploy runs either:
// no Step goroutine, whatever client the daemon has.
func TestWithoutStagingsNoStagingStepRuns(t *testing.T) {
	f := &fakeStaging{}
	h := newHarness(t, func(h *harness) { h.d.Staging = f })
	select {
	case <-h.e.goStagingSteps(h.ctx, time.Millisecond):
	case <-time.After(10 * time.Second):
		t.Fatal("a daemon without staging envs runs staging Steps")
	}
	if n := f.stepCount(); n != 0 {
		t.Fatalf("%d Steps without staging envs", n)
	}
	h = newHarness(t)
	select {
	case <-h.e.goStagingSteps(h.ctx, time.Millisecond):
	case <-time.After(10 * time.Second):
		t.Fatal("a daemon without a staging client runs staging Steps")
	}
}
