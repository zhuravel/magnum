package engine

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// A round that panics (its pipeline, or anything else the round goroutine
// runs, on one PR's data) settles its PR as a failed round, with a charged
// attempt and an engine.panic event, and the daemon goes on: no crash, no
// restart that would kill every round in flight and send the same PR again
// uncharged. maxAttempts bounds the retries as for any failure.
func TestARoundThatPanicsIsAChargedFailureAndTheDaemonGoesOn(t *testing.T) {
	h := newHarness(t)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		// The pipeline had created its runs: one still pending, one sent.
		for _, st := range []string{store.RunPending, store.RunWorking} {
			if _, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: 1, Role: "claude-review", Kind: in.Kind,
				TargetSHA: in.TargetSHA, Identity: in.PR.Identity, ReviewerLogin: "zhuravel", State: st, PromptText: "review"}); err != nil {
				t.Error(err)
			}
		}
		panic("index out of range [3] with length 2")
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick() // dispatched: the round panics

	pr := h.wantState(2, store.PRQueued)
	if pr.Attempts != 1 || pr.NextAttemptAt == nil || !strings.Contains(deref(pr.LastError), "index out of range") {
		t.Fatalf("after the panic: attempts=%d next=%v last_error=%q", pr.Attempts, pr.NextAttemptAt, deref(pr.LastError))
	}
	if sl := h.slot("review1"); sl.State != store.SlotHeld {
		t.Fatalf("slot review1 = %s, want held", sl.State)
	}
	evs := h.events("engine.panic")
	if len(evs) != 1 || evs[0].Level != "error" || !strings.Contains(evs[0].Message, "index out of range") {
		t.Fatalf("engine.panic events = %+v", evs)
	}
	subj, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range subj {
		found = found || ev.Kind == "engine.panic"
	}
	if !found {
		t.Fatal("the engine.panic event is not on the PR's subject")
	}
	runs, err := h.st.RunsByPR(h.ctx, pr.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	for _, r := range runs {
		if r.State != store.RunAbandoned && r.State != store.RunFailed {
			t.Errorf("run %s (%s) left %s: it would count as active", r.ID, r.Role, r.State)
		}
	}

	// The retries are bounded: the third panic leaves the PR for a person.
	h.advance(time.Minute)
	h.tick()
	h.advance(2 * time.Minute)
	h.tick()
	pr = h.wantState(2, store.PRNeedsAttention)
	if pr.Attempts != maxAttempts {
		t.Fatalf("attempts = %d, want %d", pr.Attempts, maxAttempts)
	}
	if n := len(h.events("engine.panic")); n != maxAttempts {
		t.Fatalf("engine.panic events = %d, want %d", n, maxAttempts)
	}

	// The daemon went on: a new push is reviewed as usual.
	h.rd.script = nil
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
}

// A heavy job that panics fails alone: the worker goes on with the next job
// and the job's key is free again for the next tick.
func TestAHeavyJobThatPanicsLeavesTheWorkerRunning(t *testing.T) {
	h := newHarness(t)
	if !h.e.enqueueHeavy("test:panic", func(context.Context) error { panic("boom in a heavy job") }) {
		t.Fatal("not queued")
	}
	ran := false
	h.e.enqueueHeavy("test:next", func(context.Context) error { ran = true; return nil })
	h.e.drainHeavy(h.ctx)
	if !ran {
		t.Fatal("the job after the panic never ran")
	}
	evs := h.events("engine.panic")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "boom in a heavy job") || !strings.Contains(evs[0].Message, "test:panic") {
		t.Fatalf("engine.panic events = %+v", evs)
	}
	if !h.e.enqueueHeavy("test:panic", func(context.Context) error { return nil }) {
		t.Fatal("the panicked job's key is still taken")
	}
}

// panickySlots is the fake slot manager whose ProvisionPool panics.
type panickySlots struct {
	*fakeSlots
	provisions atomic.Int32
}

func (p *panickySlots) ProvisionPool(context.Context, config.Pool, int) error {
	p.provisions.Add(1)
	panic("boom in a provision")
}

// A request whose heavy job panics ends as failed, once: the heavy worker
// recovers the panic, and the request is not left pending to run, and
// panic, again on every tick.
func TestARequestWhoseHeavyJobPanicsFailsOnce(t *testing.T) {
	ps := &panickySlots{}
	h := newHarness(t, func(h *harness) {
		h.cfg.Pools[0].Max = 3
		ps.fakeSlots = h.sl
		h.d.Slots = ps
	})
	id := h.enqueue(ReqProvision, ProvisionPayload{Count: 1})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "boom in a provision") {
		t.Fatalf("request after the panic: %+v %q", r, deref(r.Result))
	}
	h.tick()
	h.tick()
	if n := ps.provisions.Load(); n != 1 {
		t.Fatalf("provisions = %d, want 1: the failed request ran again", n)
	}
	if evs := h.events("engine.panic"); len(evs) != 1 || !strings.Contains(evs[0].Message, fmt.Sprintf("request:%d", id)) {
		t.Fatalf("engine.panic events = %+v", evs)
	}
}

// A panic while the panic is settled is logged, never raised again.
func TestASettleThatPanicsTooIsOnlyLogged(t *testing.T) {
	h := newHarness(t)
	settled := false
	panicked := h.e.safely(h.ctx, "test unit", "", func() { panic("first") }, func(context.Context, string) {
		settled = true
		panic("second")
	})
	if !panicked || !settled {
		t.Fatalf("panicked=%v settled=%v", panicked, settled)
	}
	if h.e.safely(h.ctx, "test unit", "", func() {}, nil) {
		t.Fatal("a body that returned reported a panic")
	}
}
