package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

func opsLayout(t *testing.T) paths.Layout {
	t.Helper()
	l := paths.Layout{Home: t.TempDir()}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestAcquireOpsHoldsBothLocks(t *testing.T) {
	l := opsLayout(t)
	unlock, who, err := acquireOps(l)
	if err != nil || who != opsMine {
		t.Fatalf("acquireOps: who %v err %v", who, err)
	}
	// A daemon starting now finds its lock held, and ops.lock held too: it
	// can tell a command's in-process job from another daemon.
	for _, p := range []string{l.Lock(), l.OpsLock()} {
		if _, held, err := engine.AcquireLock(p); err != nil || !held {
			t.Fatalf("%s while the job runs: held %v err %v", p, held, err)
		}
	}
	if _, who, _ := acquireOps(l); who != opsBusy {
		t.Fatalf("second command: who %v, want busy", who)
	}
	unlock()
	for _, p := range []string{l.Lock(), l.OpsLock()} {
		u, held, err := engine.AcquireLock(p)
		if err != nil || held {
			t.Fatalf("%s after unlock: held %v err %v", p, held, err)
		}
		u()
	}
}

func TestAcquireOpsDefersToTheDaemon(t *testing.T) {
	l := opsLayout(t)
	daemon, _, err := engine.AcquireLock(l.Lock())
	if err != nil {
		t.Fatal(err)
	}
	defer daemon()
	if unlock, who, err := acquireOps(l); err != nil || who != opsDaemon || unlock != nil {
		t.Fatalf("daemon running: who %v err %v", who, err)
	}
	// ops.lock was given back: the next command gets the same answer.
	u, held, err := engine.AcquireLock(l.OpsLock())
	if err != nil || held {
		t.Fatalf("ops.lock left held: %v %v", held, err)
	}
	u()
}

func TestSlotsProvisionRefusesWhileAnotherCommandRuns(t *testing.T) {
	f, e, ops, _ := slotsFixture(t)
	pool, _ := e.pool("talkable/talkable")
	other, held, err := engine.AcquireLock(f.Ctx.Layout.OpsLock())
	if err != nil || held {
		t.Fatal("ops lock")
	}
	defer other()
	if code := e.provision(context.Background(), pool, 1); code != 1 || !strings.Contains(f.Err.String(), "another magnum command") {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if len(ops.provisioned) != 0 {
		t.Fatal("provisioned while another command holds ops.lock")
	}
	if reqs, _ := e.st.PendingRequests(context.Background(), 0); len(reqs) != 0 {
		t.Fatalf("queued %+v", reqs)
	}
}

func TestSlotsHandOffSaysTheDaemonRuns(t *testing.T) {
	f, e, _, _ := slotsFixture(t)
	inspSeedSlot(t, e.st, f.Home, "review2", store.SlotBroken, nil)
	daemon, _, err := engine.AcquireLock(f.Ctx.Layout.Lock())
	if err != nil {
		t.Fatal(err)
	}
	defer daemon()
	if code := e.repair(context.Background(), "review2"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	actContains(t, f.Out.String(), "the daemon is running", "queued as request 1")
}

func TestSlotsAdoptWithoutPools(t *testing.T) {
	f, e, ops, _ := slotsFixture(t)
	cfg := *e.cfg
	cfg.Pools = []config.Pool{}
	e.cfg = &cfg
	if code := e.adopt(context.Background(), f.Home+"/talkable.review4"); code != 1 || !strings.Contains(f.Err.String(), "no [[pool]]") {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if len(ops.adopted) != 0 {
		t.Fatal("adopted without a pool")
	}
}

func TestReleaseRefusesWhileAnotherCommandRuns(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.busy = true
	if code := h.cmd("release", "5", "--yes"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "another magnum command")
	if h.cleaner.applied != 0 || len(h.requests()) != 0 {
		t.Fatalf("applied %d requests %d", h.cleaner.applied, len(h.requests()))
	}
}

func TestReleaseWaitNeedsAWokenDaemon(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.held, h.pid = true, 0 // the daemon holds its lock, but no usable pidfile
	if code := h.cmd("release", "5", "--wait"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if h.sleeps != 0 {
		t.Fatalf("waited %d polls for a daemon that was not woken", h.sleeps)
	}
	actContains(t, h.out.String(), "queued as request 1")
	actContains(t, h.errb.String(), "not waiting")
}
