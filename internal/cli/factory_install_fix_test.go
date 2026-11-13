package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
)

// fixLoop writes the factory's registry with a fix loop at work on
// example/shop#7 (its PR work row in phase fixing, cycle 1) and returns the
// registry and the row's id.
func fixLoop(t *testing.T, dt *daemonGroupTest) (*registry.Registry, int64) {
	t.Helper()
	return fixLoopIn(t, dt, registry.PRWorkFixing)
}

// fixLoopIn is fixLoop with the loop in phase.
func fixLoopIn(t *testing.T, dt *daemonGroupTest, phase string) (*registry.Registry, int64) {
	t.Helper()
	reg := registrytest.OpenFile(t, dt.ctx.Layout.FactoryDB(), registry.Options{})
	w, err := reg.EnsurePRWork(t.Context(), "example/shop", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdatePRWork(t.Context(), w.ID, []string{registry.PRWorkIdle}, phase, func(u *registry.PRWorkUpdate) {
		u.Set("cycle", 1)
		u.Set("cap", 3)
	}); err != nil {
		t.Fatal(err)
	}
	return reg, w.ID
}

// A restart of the factory daemon lands in the middle of a fix only while
// the loop holds an agent or a push: fixing (the fixer's turn),
// local_review (a local round of the fix cycle) and pushing (the push and
// its confirmation). There `factory install` refuses and `--drain` waits
// until the loop leaves the phase. A loop that is queued (for
// fix_max_active or a copy), waits for magnum's re-review or waits for you
// (held) holds neither, so install goes on and the drain does not wait for
// it, and idle is no work.
func TestTheDrainWaitsOnlyForAPhaseWithAnAgentOrAPush(t *testing.T) {
	for _, tc := range []struct {
		phase string
		waits bool
	}{
		{registry.PRWorkIdle, false}, {registry.PRWorkQueued, false}, {registry.PRWorkFixing, true}, {registry.PRWorkLocalReview, true},
		{registry.PRWorkPushing, true}, {registry.PRWorkReviewing, false}, {registry.PRWorkHeld, false},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			named := "fix loop of example/shop#7 (" + tc.phase + ", cycle 1"
			dt, _ := factoryInstallTest(t)
			writeFactoryWork(t, dt.ctx.Layout, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{}})
			fixLoopIn(t, dt, tc.phase)
			code := dt.run("factory", "install")
			switch {
			case tc.waits && (code != 1 || !strings.Contains(dt.stderr.String(), named)):
				t.Fatalf("install: exit %d, stderr %s; want the refusal naming %q", code, dt.stderr, named)
			case !tc.waits && (code != 0 || reloads(dt) != 1):
				t.Fatalf("install: exit %d, %d reloads, stderr %s; want the job reloaded at once", code, reloads(dt), dt.stderr)
			}

			dt, _ = factoryInstallTest(t)
			l := dt.ctx.Layout
			writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{}})
			reg, id := fixLoopIn(t, dt, tc.phase)
			sleeps := 0
			daemonSys.Sleep = func(context.Context, time.Duration) error {
				sleeps++
				v, _ := factory.ReadDrain(l)
				switch sleeps {
				case 1: // the daemon's tick took the drain
					writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: v, Work: []factory.WorkItem{}})
				case 3: // the loop left its phase
					if err := reg.UpdatePRWork(t.Context(), id, nil, registry.PRWorkReviewing, nil); err != nil {
						t.Errorf("the loop's move: %v", err)
					}
				}
				return nil
			}
			if code := dt.run("factory", "install", "--drain"); code != 0 || reloads(dt) != 1 {
				t.Fatalf("install --drain: exit %d, %d reloads, stderr %s", code, reloads(dt), dt.stderr)
			}
			out := dt.stdout.String()
			if tc.waits {
				actContains(t, out, named, "drained after 15s: nothing in flight")
				return
			}
			actContains(t, out, "drained after 5s: nothing in flight")
			if strings.Contains(out, "fix loop") {
				t.Fatalf("the drain named a loop that holds no agent and no push:\n%s", out)
			}
		})
	}
}

// A fix loop on one of your PRs is work in flight: `factory install
// --drain` waits for it, as for a local review round, and reloads the job
// only once the daemon holds the drain and the loop's PR work row is idle
// again; the progress names the loop.
func TestInstallDrainWaitsForAFixJob(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	l := dt.ctx.Layout
	writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{}})
	reg, id := fixLoop(t, dt)
	sleeps := 0
	daemonSys.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		v, _ := factory.ReadDrain(l)
		switch sleeps {
		case 1: // the daemon's tick took the drain; the fixer is at work
			writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: v, Work: []factory.WorkItem{}})
		case 3: // the loop ended at its cycle's end
			if err := reg.UpdatePRWork(t.Context(), id, nil, registry.PRWorkIdle, nil); err != nil {
				t.Errorf("the loop's end: %v", err)
			}
		}
		if sleeps < 3 && reloads(dt) != 0 {
			t.Errorf("sleep %d: the job was reloaded while the fix loop ran", sleeps)
		}
		return nil
	}

	if code := dt.run("factory", "install", "--drain"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	actContains(t, dt.stdout.String(), "waiting for 1 background job(s) in flight", "fix loop of example/shop#7 (fixing, cycle 1",
		"drained after 15s: nothing in flight")
	if reloads(dt) != 1 {
		t.Fatalf("bootstrap calls %d, want the job reloaded once", reloads(dt))
	}
}

// While a fix loop runs, plain install refuses and names it; a registry
// whose fix loops cannot be read fails closed, as a missing record does,
// and --now goes on.
func TestFactoryInstallRefusesWhileAFixLoopRuns(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	writeFactoryWork(t, dt.ctx.Layout, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{}})
	fixLoop(t, dt)
	if code := dt.run("factory", "install"); code != 1 {
		t.Fatalf("exit %d, want 1; stdout %s stderr %s", code, dt.stdout, dt.stderr)
	}
	actContains(t, dt.stderr.String(), "magnum factory install: 1 background job(s) in flight: fix loop of example/shop#7 (fixing, cycle 1",
		"fix: `magnum factory install --drain` starts nothing new and waits for these, --now interrupts them")
	if reloads(dt) != 0 {
		t.Fatal("the refusal reloaded the job")
	}

	dt, _ = factoryInstallTest(t)
	writeFactoryWork(t, dt.ctx.Layout, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{}})
	if err := os.WriteFile(dt.ctx.Layout.FactoryDB(), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("factory", "install"); code != 1 {
		t.Fatalf("an unreadable registry: exit %d, want 1; stderr %s", code, dt.stderr)
	}
	actContains(t, dt.stderr.String(), "cannot tell what the factory daemon (pid 250) runs (reading", "factory.db", "pass --now")
	if code := dt.run("factory", "install", "--now"); code != 0 || reloads(dt) != 1 {
		t.Fatalf("--now: exit %d, %d reloads; stderr %s", code, reloads(dt), dt.stderr)
	}
}
