package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// A round that posts a clean review approves as the operator as soon as it
// ends, in its own goroutine, through the tick's decision and gates: no tick
// after the round is needed (on talkable#12006 the approval came 36 seconds
// after the round, at the next tick). The tick's pass stays the safety net
// and posts nothing twice.
func TestACleanRoundApprovesAsTheOperatorBeforeTheNextTick(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil { // dispatches the round, which waits at the gate
		t.Fatal(err)
	}
	h.awaitRound(before)
	if got := h.created(); len(got) != 0 {
		t.Fatalf("approved before the review: %q", got)
	}
	close(h.rd.gate) // the round posts its clean review and ends; no tick follows
	h.settle()
	h.wantState(2, store.PRReviewed)
	want := []string{"APPROVE@b1:" + autoBody("b1", prURL2+"#pullrequestreview-501")}
	if got := h.created(); !slices.Equal(got, want) {
		t.Fatalf("posted = %q\nwant %q", got, want)
	}
	if a := h.autoApproval(2); a.State != store.AutoStanding || a.RunID != "run-1" {
		t.Fatalf("approval = %+v", a)
	}
	h.tick()
	if got := h.created(); len(got) != 1 {
		t.Fatalf("approved again at the tick: %q", got)
	}
}

// A round that leaves something to fix approves nothing at its end either.
func TestABlockingRoundApprovesNothingAtItsEnd(t *testing.T) {
	h, plan := newAutoHarness(t)
	plan.set(2, blockingRound)
	h.reviewedPR(2, "b1")
	if got := h.created(); len(got) != 0 {
		t.Fatalf("posted = %q", got)
	}
}
