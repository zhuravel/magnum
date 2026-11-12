package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
)

// Under a drain for a reboot the requests that would start work (a staging
// deploy, an open's restore, a slot's provision, repair or adoption, a
// cleanup, a release) are refused, each saying why and how to go on; a
// cancel of a staging deploy is control and goes on to be answered.
func TestARebootDrainRefusesTheRequestsThatStartWork(t *testing.T) {
	h := newHarness(t)
	h.startup()
	h.rebootDrain(50, DrainUntilBoot)
	h.tick()
	for _, kind := range []string{ReqStagingDeploy, ReqOpen, ReqProvision, ReqRepair, ReqAdopt, ReqCleanup, ReqRelease} {
		id := h.enqueue(kind, map[string]any{"ref": "1"})
		h.tick()
		h.e.stagingWG.Wait()
		r := h.request(id)
		if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "drains for a reboot") ||
			!strings.Contains(deref(r.Result), "magnum resume") {
			t.Errorf("%s under the drain: %s %q, want refused, saying why and how to go on", kind, r.State, deref(r.Result))
		}
	}
	id := h.enqueue(ReqStagingCancel, StagingCancelPayload{Effect: "dep-1"})
	h.tick()
	h.e.stagingWG.Wait()
	if r := h.request(id); strings.Contains(deref(r.Result), "drains for a reboot") {
		t.Fatalf("a staging cancel was refused under the drain: %q", deref(r.Result))
	}
}

// The reconcile starts no slot work under the drain and runs once it is
// lifted; a staging deploy that runs (an effect queued or running) holds the
// acknowledgement's quiet, named.
func TestARebootDrainHoldsTheReconcileAndCountsADeployInFlight(t *testing.T) {
	f := &fakeStaging{effects: map[string][]staging.Effect{"talkable/talkable": {{ID: "dep-7", Repo: "talkable/talkable", Env: envVoid,
		PR: staging.PR{Repo: "talkable/talkable", Number: 2}, State: staging.EffectRunning}}}}
	h := deployHarness(t, f)
	before, _ := h.e.getKV(h.ctx, store.KVDaemonLastReconcile)
	h.rebootDrain(51, DrainUntilBoot)
	h.advance(h.cfg.Daemon.ReconcileInterval.Duration + time.Minute)
	h.tick()

	if after, _ := h.e.getKV(h.ctx, store.KVDaemonLastReconcile); after != before {
		t.Fatalf("a reconcile ran under the drain (%q, was %q)", after, before)
	}
	ack, _ := h.drainAck()
	if ack.Quiet || !slices.Contains(ack.Work, "staging deploy of talkable/talkable#2 to void") {
		t.Fatalf("acknowledgement %+v, want the deploy that runs named", ack)
	}
	f.mu.Lock()
	f.effects = nil
	f.mu.Unlock()
	h.tick()
	if ack, _ := h.drainAck(); !ack.Quiet {
		t.Fatalf("acknowledgement %+v once the deploy ended, want quiet", ack)
	}

	if err := h.st.DeleteKV(h.ctx, KVDaemonRebootDrain); err != nil { // magnum resume
		t.Fatal(err)
	}
	h.tick()
	if after, _ := h.e.getKV(h.ctx, store.KVDaemonLastReconcile); after == before {
		t.Fatal("the reconcile the drain held did not run once it was lifted")
	}
}
