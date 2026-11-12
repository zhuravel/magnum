package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// tickedAck ticks once and returns the drain's acknowledgement, failing when
// there is none.
func (h *harness) tickedAck() DrainAck {
	h.t.Helper()
	_ = h.e.Tick(h.ctx) // a poll GitHub does not answer fails the tick's error, not the drain
	h.settle()
	ack, ok := h.drainAck()
	if !ok {
		h.t.Fatal("no acknowledgement of the drain")
	}
	return ack
}

// The push of a held merge runs off the loop with the fixer App's Workflows
// token, under a fix reservation: the drain names it as that push and is
// not quiet while it runs, and once it ended the daemon is quiet.
func TestTheRebootDrainNamesTheHeldMergesPushAndWaitsForIt(t *testing.T) {
	h, mg := holdMerged(t)
	gate := startParkedPush(t, h, mg)
	h.rebootDrain(90, DrainUntilBoot)
	if err := h.e.Tick(h.ctx); err != nil { // no settle: it would wait for the parked push
		close(gate)
		t.Fatal(err)
	}
	ack, _ := h.drainAck()
	const want = "push of the held merge of talkable/talkable#2 (Workflows token)"
	if ack.Quiet || !slices.Contains(ack.Work, want) {
		close(gate)
		h.settle()
		t.Fatalf("acknowledgement %+v while the held merge's push runs, want not quiet, naming %q", ack, want)
	}
	close(gate)
	h.settle()
	if _, pushes := mg.made(); len(pushes) != 1 {
		t.Fatalf("pushes = %v, want the held merge's", pushes)
	}
	if ack := h.tickedAck(); !ack.Quiet || len(ack.Work) != 0 {
		t.Fatalf("acknowledgement %+v once the push ended, want quiet", ack)
	}
}

// Under the drain the y of a held merge pushes nothing: it is refused in the
// drain's words, and the merge stays held for P after it.
func TestARebootDrainRefusesTheYOfAHeldMerge(t *testing.T) {
	h, mg := holdMerged(t)
	asked := h.planMerge().Asked
	h.rebootDrain(91, DrainUntilBoot)
	r := h.mergeBound(asked)
	if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "drains for a reboot") || strings.Contains(deref(r.Result), "restart") {
		t.Fatalf("the y under the drain = %s %q, want refused for the drain for a reboot", r.State, deref(r.Result))
	}
	wantNoPush(t, mg)
	if h.heldRow() == "" {
		t.Error("the held merge went")
	}
}
