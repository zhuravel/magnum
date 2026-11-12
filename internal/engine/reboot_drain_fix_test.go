package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/steps"
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

// A held merge's push whose loop row a stop left (the start's recovery,
// recoverHeldPush, ends it) runs nothing: the drain does not wait for it,
// and says what the start after the reboot does with it.
func TestARebootDrainLeavesTheRowOfACutHeldPushToTheStart(t *testing.T) {
	h, _ := holdMerged(t)
	held, _ := h.heldMergeOfPR()
	if err := h.e.setFixState(h.ctx, h.pr(2).ID, FixState{Cycle: 1, Cap: 1, Phase: FixPhaseFixing, Mode: FixModeMerge, By: heldPushBy,
		Head: held.Head, Since: held.Head, Role: "claude-fix", StartedAt: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	h.rebootDrain(92, DrainUntilBoot)
	ack := h.tickedAck()
	const want = "the push of the held merge of talkable/talkable#2, cut by a stop: the start after the reboot ends its loop, and P asks the push again"
	if !ack.Quiet || len(ack.Work) != 0 || !slices.Contains(ack.Left, want) {
		t.Fatalf("acknowledgement %+v, want quiet, leaving %q", ack, want)
	}
}

// A loop whose settle confirms its fixer's push while GitHub does not
// answer (FixState.Confirm, no job holding it) is the drain's work up to the
// cap of its confirmation: GitHub's answer settles the fix as pushed, where
// a reboot would fail it as unconfirmed. Past the cap the fix fails as
// unconfirmed (capPushes) and the drain goes quiet, leaving its push to the
// late settle, which recovers it after the reboot.
func TestARebootDrainWaitsForAConfirmingLoopUpToItsCap(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	pushFixer(h, git, func() {
		h.gh.mu.Lock()
		h.gh.detailsErr, h.gh.radarErr = ghDown(), ghDown()
		h.gh.mu.Unlock()
		git.mu.Lock()
		git.lsErr = gitDown()
		git.mu.Unlock()
	})
	h.fix(FixPayload{})
	h.handle()
	rec, ok := h.fixPushOf(2)
	if st, _ := h.fixStateOf(2); !ok || st.Confirm == nil {
		t.Fatalf("fix state %+v, record %+v: want the loop confirming its push", st, rec)
	}
	h.rebootDrain(93, DrainUntilBoot)
	ack := h.tickedAck()
	want := fmt.Sprintf("confirming the push of b2b2b2b on talkable/talkable#2 (GitHub does not answer; at most until %s)",
		rec.Since.Add(fixConfirmCap).Local().Format("15:04"))
	if ack.Quiet || !slices.Contains(ack.Work, want) {
		t.Fatalf("acknowledgement %+v, want not quiet, naming %q", ack, want)
	}
	h.advance(20 * time.Minute)
	if ack := h.tickedAck(); ack.Quiet || !slices.Contains(ack.Work, want) {
		t.Fatalf("acknowledgement %+v 20 minutes on, want the loop still waited for", ack)
	}

	h.advance(fixConfirmCap)
	ack = h.tickedAck()
	const left = "the push of b2b2b2b on talkable/talkable#2, which GitHub did not confirm: the late settle recovers it after the reboot"
	if !ack.Quiet || slices.ContainsFunc(ack.Work, func(w string) bool { return strings.Contains(w, "confirming") }) || !slices.Contains(ack.Left, left) {
		t.Fatalf("acknowledgement %+v past the cap, want quiet, leaving %q", ack, left)
	}
	if rec, ok := h.fixPushOf(2); !ok || rec.Kind != fixPushUnconfirmed {
		t.Errorf("the record = %+v, %v; want the push unconfirmed, for the late settle", rec, ok)
	}
}

// A loop still marked confirming past its cap, which the tick's check of
// the confirmations (capPushes) did not end yet, is never waited for: the
// drain leaves it to the late settle after the reboot.
func TestARebootDrainLeavesAConfirmingLoopPastItsCap(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	since := h.clock.Now().Add(-fixConfirmCap - time.Minute)
	if err := h.e.setFixState(h.ctx, pr.ID, FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Head: fixHead, Since: fixHead, Report: "r-1",
		StartedAt: since, Confirm: &FixConfirm{Head: fixPushed, Since: since}}); err != nil {
		t.Fatal(err)
	}
	work, left := h.e.rebootFixWork(h.ctx, nil, &rebootState{})
	const want = "confirming the push of b2b2b2b on talkable/talkable#2: GitHub did not answer for 30m; the late settle recovers it after the reboot"
	if len(work) != 0 || !slices.Contains(left, want) {
		t.Fatalf("work %q, left %q; want no work, leaving %q", work, left, want)
	}
}

// The settle of a confirmed push that stopped between its steps (the next
// poll resumes it) is the drain's work while GitHub does not answer, up to
// 30 minutes from when the drain first saw it; then the drain leaves it to
// the poll after the reboot, which resumes it.
func TestARebootDrainWaitsForASettleBetweenItsStepsUpToTheCap(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	rec := fixPush{Kind: fixPushUnconfirmed, Run: "run-settle", Pushed: fixPushed, Since: h.clock.Now().Add(-time.Hour), Failed: h.clock.Now().Add(-time.Hour)}
	if err := writeKVJSON(h.ctx, h.e, kvPRFixPush(pr.ID), "fix push", rec); err != nil {
		t.Fatal(err)
	}
	if err := steps.Step(h.ctx, h.st, pushSubject(rec.Run), pushFirstStep(rec), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	h.gh.mu.Lock()
	h.gh.radarErr, h.gh.detailsErr = ghDown(), ghDown()
	h.gh.mu.Unlock()
	h.rebootDrain(95, DrainUntilBoot)
	ack := h.tickedAck()
	const want = "the settle of the confirmed push of b2b2b2b on talkable/talkable#2 (its next steps run at the next poll)"
	if ack.Quiet || !slices.Contains(ack.Work, want) {
		t.Fatalf("acknowledgement %+v, want not quiet, naming %q", ack, want)
	}
	h.advance(fixConfirmCap)
	ack = h.tickedAck()
	const left = "the settle of the confirmed push of b2b2b2b on talkable/talkable#2, stopped between its steps: the poll after the reboot resumes it"
	if !ack.Quiet || !slices.Contains(ack.Left, left) {
		t.Fatalf("acknowledgement %+v past the cap, want quiet, leaving %q", ack, left)
	}
}
