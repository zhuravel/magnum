package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// openParked restores the parked PR #2 with `magnum open` and returns the
// request's answer.
func (h *harness) openParked() string {
	h.t.Helper()
	id := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "restored talkable/talkable#2 in review1") {
		h.t.Fatalf("open request: %+v %q", r, deref(r.Result))
	}
	return deref(r.Result)
}

// heldOf is PR #2's hold record.
func (h *harness) heldOf() (OpenHold, bool) {
	h.t.Helper()
	return OpenHoldOf(h.ctx, h.st, h.pr(2).ID)
}

// wantPinned checks the pin of PR #2 and of slot review1.
func (h *harness) wantPinned(pinned bool) {
	h.t.Helper()
	pr, sl := h.pr(2), h.slot("review1")
	if pr.Pinned != pinned || sl.Pinned != pinned {
		h.t.Fatalf("PR pinned %v, slot %+v; want pinned %v", pr.Pinned, sl, pinned)
	}
}

// TestARestoreByOpenHoldsThePRUntilTheHoldEndsByItself: `magnum open` of a
// parked PR pins the PR and its slot as before, and records a hold ending
// open_hold (30m) after the open. Meanwhile a push's re-review waits on the
// hold (its wait says until when) and the idle park leaves the restored judge
// alone; once the hold ends, the tick unpins both with one
// pr.open_hold_ended event and the re-review runs in the same tick.
func TestARestoreByOpenHoldsThePRUntilTheHoldEndsByItself(t *testing.T) {
	h := newHarness(t)
	pr := h.parkedPR()
	h.advance(30 * time.Minute) // past the review's re-review interval
	opened := h.clock.Now()
	answer := h.openParked()
	end := waitClock(opened.Add(30*time.Minute), opened)
	if !strings.Contains(answer, "held by magnum open until "+end) || !strings.Contains(answer, "`magnum pin talkable/talkable#2` keeps it") {
		t.Fatalf("answer %q, want the hold until %s", answer, end)
	}
	h.wantPinned(true)
	hold, ok := h.heldOf()
	if !ok || !hold.Until.Equal(opened.Add(30*time.Minute)) || !hold.At.Equal(opened) {
		t.Fatalf("hold %+v (%v), want until %v", hold, ok, opened.Add(30*time.Minute))
	}
	if evs := h.events(evPROpened); len(evs) != 1 || !strings.Contains(evs[0].Message, "held until "+end) {
		t.Fatalf("pr.opened events = %+v", evs)
	}

	pollPR(h, 10*time.Minute, 2, "b2")
	rounds := len(h.rd.all())
	h.advance(10 * time.Minute) // past the quiet period and park_idle_after
	h.tick()
	if n := len(h.rd.all()); n != rounds {
		t.Fatalf("a round ran under the hold (%d rounds, want %d)", n, rounds)
	}
	h.wantState(2, store.PRRereviewPending)
	if w, _ := h.waitOf(2); w.Reason != WaitPinned || w.Short(h.clock.Now()) != "re-review · held → "+end ||
		!strings.Contains(w.Sentence("talkable#2", h.clock.Now()), "slot review1 is held by magnum open until "+end) {
		t.Fatalf("wait %+v: %q", w, w.Short(h.clock.Now()))
	}
	if _, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, store.RoleJudge); err != nil {
		t.Fatalf("the idle park took the held PR's judge: %v", err)
	}
	h.wantPinned(true)
	if evs := h.events(evPROpenHoldEnded); len(evs) != 0 {
		t.Fatalf("the hold ended early: %+v", evs)
	}

	h.advance(10*time.Minute + time.Second) // past the hold's end
	h.tick()
	h.wantPinned(false)
	if _, ok := h.heldOf(); ok {
		t.Fatal("the hold record outlived the hold")
	}
	evs := h.events(evPROpenHoldEnded)
	if len(evs) != 1 || evs[0].Level != "info" || evs[0].Message != "magnum open's hold (set "+pastClock(opened, h.clock.Now())+") ended: unpinned review1, its rounds go on" {
		t.Fatalf("pr.open_hold_ended events = %+v", evs)
	}
	if n := len(h.rd.all()); n != rounds+1 || h.rd.all()[n-1].TargetSHA != "b2" {
		t.Fatalf("rounds after the hold = %d, want the re-review of b2", n)
	}
	h.wantState(2, store.PRReviewed)
	h.tick()
	if evs := h.events(evPROpenHoldEnded); len(evs) != 1 {
		t.Fatalf("the hold ended twice: %+v", evs)
	}
}

// TestInputInTheHeldPRsPanesExtendsTheHold: a hold lasts until
// human_cooldown (15m) after the last input in the PR's panes when that is
// later than the open plus open_hold, and its end event says so.
func TestInputInTheHeldPRsPanesExtendsTheHold(t *testing.T) {
	h := newHarness(t)
	pr := h.parkedPR()
	opened := h.clock.Now()
	h.openParked()
	h.advance(25 * time.Minute)
	typed := h.clock.Now()
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", typed) }); err != nil {
		t.Fatal(err)
	}
	h.advance(6 * time.Minute) // past the open plus open_hold
	h.tick()
	h.wantPinned(true)
	h.advance(9*time.Minute + time.Second) // 15m after the input
	h.tick()
	h.wantPinned(false)
	evs := h.events(evPROpenHoldEnded)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "(set "+pastClock(opened, h.clock.Now())+", last input in its panes "+pastClock(typed, h.clock.Now())+")") {
		t.Fatalf("pr.open_hold_ended events = %+v", evs)
	}
}

// TestMagnumPinMakesAnOpensHoldLasting: `magnum pin` of a PR an open holds
// drops the hold, says so, and the pin outlives the hold's end.
func TestMagnumPinMakesAnOpensHoldLasting(t *testing.T) {
	h := newHarness(t)
	h.parkedPR()
	h.openParked()
	id := h.enqueue(ReqPin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "magnum open's hold is now a lasting pin") {
		t.Fatalf("pin: %+v %q", r, deref(r.Result))
	}
	if _, ok := h.heldOf(); ok {
		t.Fatal("the pin kept the hold")
	}
	h.advance(2 * time.Hour)
	h.tick()
	h.wantPinned(true)
	if evs := h.events(evPROpenHoldEnded); len(evs) != 0 {
		t.Fatalf("a lasting pin ended as a hold: %+v", evs)
	}
}

// TestMagnumUnpinEndsAnOpensHold: `magnum unpin` ends the hold with the pin;
// the tick has no hold left to end.
func TestMagnumUnpinEndsAnOpensHold(t *testing.T) {
	h := newHarness(t)
	h.parkedPR()
	h.openParked()
	id := h.enqueue(ReqUnpin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("unpin: %+v %q", r, deref(r.Result))
	}
	h.wantPinned(false)
	if _, ok := h.heldOf(); ok {
		t.Fatal("the unpin kept the hold")
	}
	h.advance(time.Hour)
	h.tick()
	if evs := h.events(evPROpenHoldEnded); len(evs) != 0 {
		t.Fatalf("an unpinned PR's hold ended: %+v", evs)
	}
}

// TestOpenHoldZeroPinsForGood: open_hold = "0" keeps the lasting pin: no
// hold is recorded and the pin outlives any time.
func TestOpenHoldZeroPinsForGood(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Daemon.OpenHold.Duration = 0 })
	h.parkedPR()
	if answer := h.openParked(); !strings.Contains(answer, "(pinned; `magnum unpin talkable/talkable#2` lets reviews use it again)") {
		t.Fatalf("answer %q, want a lasting pin", answer)
	}
	if _, ok := h.heldOf(); ok {
		t.Fatal("open_hold 0 recorded a hold")
	}
	h.advance(3 * time.Hour)
	h.tick()
	h.wantPinned(true)
}

// TestARestartKeepsAnOpensHold: the hold is the registry's, so a new daemon
// on it keeps the hold, does not move its end, and ends it on time.
func TestARestartKeepsAnOpensHold(t *testing.T) {
	h := newHarness(t)
	h.parkedPR()
	h.openParked()
	before, _ := h.heldOf()
	h.advance(10 * time.Minute)
	h.e = New(h.d)
	h.startup()
	h.tick()
	h.wantPinned(true)
	if after, ok := h.heldOf(); !ok || !after.Until.Equal(before.Until) {
		t.Fatalf("hold after the restart %+v (%v), want %+v", after, ok, before)
	}
	if evs := h.events(evPROpenHoldSet); len(evs) != 0 {
		t.Fatalf("the restart converted a hold: %+v", evs)
	}
	h.advance(21 * time.Minute)
	h.tick()
	h.wantPinned(false)
	if evs := h.events(evPROpenHoldEnded); len(evs) != 1 {
		t.Fatalf("pr.open_hold_ended events = %+v", evs)
	}
}

// TestAFocusOfALivePaneSetsNoHold: an open of a PR whose judge is live
// restores nothing, so it pins and holds nothing.
func TestAFocusOfALivePaneSetsNoHold(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	id := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "nothing to restore") {
		t.Fatalf("open: %+v %q", r, deref(r.Result))
	}
	if h.pr(2).Pinned {
		t.Fatal("a focus pinned the PR")
	}
	if _, ok := h.heldOf(); ok {
		t.Fatal("a focus recorded a hold")
	}
}

// TestALastingPinOfAnEarlierOpenBecomesAHoldAtTheStart: at the start, a PR
// whose pin `magnum open` made (its newest pin event) and that has no hold
// gets one ending open_hold after the start, with one event; a pin `magnum
// pin` made stays lasting. Holds are on when the daemon starts, so an open
// made under open_hold = 0 converts.
func TestALastingPinOfAnEarlierOpenBecomesAHoldAtTheStart(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Daemon.OpenHold.Duration = 0 })
	h.parkedPR()
	h.openParked() // a lasting pin, as before holds
	pin := h.enqueue(ReqPin, TargetPayload{PRTarget: PRTarget{Ref: "1"}})
	h.tick()
	if r := h.request(pin); r.State != store.RequestDone {
		t.Fatalf("pin #1: %+v %q", r, deref(r.Result))
	}
	h.advance(time.Hour)
	h.cfg.Daemon.OpenHold.Duration = 30 * time.Minute
	start := h.clock.Now()
	h.e = New(h.d)
	h.startup()
	hold, ok := h.heldOf()
	if !ok || !hold.Until.Equal(start.Add(30*time.Minute)) {
		t.Fatalf("hold %+v (%v), want until %v", hold, ok, start.Add(30*time.Minute))
	}
	if _, ok := OpenHoldOf(h.ctx, h.st, h.pr(1).ID); ok {
		t.Fatal("a pin magnum pin made became a hold")
	}
	evs := h.events(evPROpenHoldSet)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "is now a hold until "+waitClock(start.Add(30*time.Minute), start)) {
		t.Fatalf("pr.open_hold_set events = %+v", evs)
	}
	h.startup() // a second start finds the hold
	if evs := h.events(evPROpenHoldSet); len(evs) != 1 {
		t.Fatalf("a second start converted again: %+v", evs)
	}
	h.advance(31 * time.Minute)
	h.tick()
	h.wantPinned(false)
	if !h.pr(1).Pinned {
		t.Fatal("the lasting pin of #1 ended")
	}
}

// TestThePausedRoundContinuesWhenTheOpensHoldEnds is the talkable#12048
// case: a round paused on a usage limit, its judge parked, the operator ran
// `magnum open` to read the panes, which restored the judge and pinned the
// PR, and the paused round could not continue until `magnum unpin`. With the
// hold the paused round waits while it holds and continues once it ends.
func TestThePausedRoundContinuesWhenTheOpensHoldEnds(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.rd.script = judgeLimitRound(t, h)
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRPaused)
	if err := h.ag.Park(h.ctx, h.pr(2)); err != nil { // the login change parked the judge
		t.Fatal(err)
	}
	opened := h.clock.Now()
	h.openParked()
	resume := h.enqueue(ReqResume, PausePayload{Tool: agents.KindCodex})
	h.tick()
	if r := h.request(resume); r.State != store.RequestDone {
		t.Fatalf("resume: %+v %q", r, deref(r.Result))
	}
	rounds := len(h.rd.all())
	h.advance(10 * time.Minute)
	h.tick()
	h.wantState(2, store.PRPaused)
	if n := len(h.rd.all()); n != rounds {
		t.Fatalf("the paused round continued under the hold (%d rounds, want %d)", n, rounds)
	}
	h.advance(opened.Add(30*time.Minute + time.Second).Sub(h.clock.Now()))
	h.tick()
	h.wantPinned(false)
	ins := h.rd.all()
	if len(ins) != rounds+1 || ins[len(ins)-1].Kind != pipeline.KindContinue || ins[len(ins)-1].TargetSHA != "b2" {
		t.Fatalf("rounds after the hold: %d, want the continue of b2", len(ins))
	}
	h.wantState(2, store.PRReviewed)
}

// TestAnOpenInFlightKeepsTheHoldItRecorded: an open records its hold before
// it pins the PR (holdOpened), so a tick that meets the record of a PR not
// pinned yet leaves it while the open holds the PR; once nothing does, a
// record whose PR is not pinned holds nothing and goes.
func TestAnOpenInFlightKeepsTheHoldItRecorded(t *testing.T) {
	h := newHarness(t)
	pr := h.parkedPR()
	v, err := OpenHold{Until: h.clock.Now().Add(30 * time.Minute), At: h.clock.Now()}.marshal()
	if err != nil {
		t.Fatal(err)
	}
	h.e.setKV(h.ctx, KVPROpenHold(pr.ID), v)
	if !h.e.reserve(pr.ID, &roundHandle{cancel: func() {}, open: true}) {
		t.Fatal("reserve")
	}
	h.e.endOpenHolds(h.ctx)
	if _, ok := h.heldOf(); !ok {
		t.Fatal("the tick dropped the hold of an open in flight")
	}
	h.e.unreserve(pr.ID)
	h.e.endOpenHolds(h.ctx)
	if _, ok := h.heldOf(); ok {
		t.Fatal("the record of a PR no pin holds stayed")
	}
}
