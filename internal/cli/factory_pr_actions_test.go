package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// f, F and K on your PR's row of the factory screen run the board's own
// code: magnum's fix request, its fix off and on, and its abort, queued in
// magnum's registry and answered by magnum's daemon, as the board's f, F
// and K do. They never go through the factory daemon: the factory's
// registry is not even made.
func TestFOnAnOwnPRRowAsksAndQueuesMagnumsFix(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, fixAnswer) }
	board := newScreenActions(h.c)
	defer board.Close()
	act := newFactoryActions(loaded(t, h.c), &factoryScreenSource{c: loaded(t, h.c), now: inspNow}, board)
	ctx := context.Background()
	const ref = "talkable/talkable#5"

	res, err := act.Fix(ctx, ref)
	if err != nil {
		t.Fatalf("fix: %v (%s)", err, h.errb.String())
	}
	if !strings.Contains(res.Text, fixAnswer) || len(res.Requests) != 1 || res.Requests[0].Kind != engine.ReqFix {
		t.Errorf("fix returned %q with requests %+v, want magnum's answer and its fix request", res.Text, res.Requests)
	}
	if _, err := act.FixOff(ctx, ref, true); err != nil {
		t.Fatalf("fix off: %v", err)
	}
	if _, err := act.FixOff(ctx, ref, false); err != nil {
		t.Fatalf("fix on: %v", err)
	}
	if err := h.st.SetKV(ctx, engine.KVPRFix(pr.ID), `{"cycle":1,"cap":3,"phase":"fixing"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := act.Abort(ctx, ref); err != nil {
		t.Fatalf("abort: %v (%s)", err, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 4 {
		t.Fatalf("%d requests in magnum's registry, want 4: %+v", len(reqs), reqs)
	}
	for i, want := range []engine.FixPayload{{}, {Off: true}, {On: true}} {
		p := actDecode[engine.FixPayload](t, reqs[i].Payload)
		if reqs[i].Kind != engine.ReqFix || p.Off != want.Off || p.On != want.On || p.Number != 5 {
			t.Errorf("request %d: %s %+v, want the fix request with off=%v on=%v", i, reqs[i].Kind, p, want.Off, want.On)
		}
	}
	if reqs[3].Kind != engine.ReqAbort {
		t.Errorf("request 4 is %s, want magnum's abort", reqs[3].Kind)
	}
	if fsx.Exists(h.c.Layout.FactoryDB()) {
		t.Error("the screen's PR keys made the factory's registry")
	}
}

// c on a PR's row or card of the factory screen runs the board's own CI fix
// (screenActions.FixCIPlan, FixCI; magnum fix --ci): the daemon's plan in a
// dry run, whose sentence is the question and whose binding y sends, then
// the start so bound, queued in magnum's registry and answered by magnum's
// daemon. They never go through the factory daemon, and a screen built
// without the board's actions refuses each.
func TestCOnAPRRowOfTheFactoryScreenIsTheBoardsCIFix(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.ciFixDaemon()
	board := newScreenActions(h.c)
	defer board.Close()
	src := &factoryScreenSource{c: loaded(t, h.c), now: inspNow}
	act := newFactoryActions(loaded(t, h.c), src, board)
	ctx := context.Background()
	const ref = "talkable/talkable#5"

	plan, err := act.FixCIPlan(ctx, ref)
	if err != nil {
		t.Fatalf("plan: %v (%s)", err, h.errb.String())
	}
	if want := "Fix the failed CI of " + ref + ": claude-fix fixes required check Completion at a1a1a1a (2 failed jobs) in review1 as fixer-app[bot], CI fix 1 of 3?"; plan.Ask != want {
		t.Errorf("the plan asks %q, want %q", plan.Ask, want)
	}
	a := ciFixAsked
	if c := plan.CIFix; c == nil || c.Head != a.Head || !slices.Equal(c.Entries, a.Entries) || c.Count != a.Count || !c.At.Equal(a.At) {
		t.Fatalf("the plan binds %+v, want %+v", plan.CIFix, a)
	}
	res, err := act.FixCI(ctx, ref, *plan.CIFix)
	if err != nil || !strings.Contains(res.Text, ciFixAnswer) {
		t.Fatalf("the start = %+v, %v; want the daemon's answer", res, err)
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests in magnum's registry, want the plan and the start: %+v", len(reqs), reqs)
	}
	if p := actDecode[engine.FixPayload](t, reqs[0].Payload); !p.DryRun || p.Mode != engine.FixModeCI || p.Number != 5 {
		t.Errorf("the plan's payload = %+v, want a dry run of a CI fix of #5", p)
	}
	if p := actDecode[engine.FixPayload](t, reqs[1].Payload); p.DryRun || p.Mode != engine.FixModeCI || p.CIAsked == nil || p.CIAsked.Head != a.Head {
		t.Errorf("the start's payload = %+v, want a CI fix bound to the plan", p)
	}
	if fsx.Exists(h.c.Layout.FactoryDB()) {
		t.Error("c on a PR's row made the factory's registry")
	}

	bare := newFactoryActions(loaded(t, h.c), src, nil)
	if _, err := bare.FixCIPlan(ctx, ref); !errors.Is(err, errNoBoard) {
		t.Errorf("the plan without the board's actions: %v, want %v", err, errNoBoard)
	}
	if _, err := bare.FixCI(ctx, ref, *plan.CIFix); !errors.Is(err, errNoBoard) {
		t.Errorf("the start without the board's actions: %v, want %v", err, errNoBoard)
	}
	if n := len(h.requests()); n != len(reqs) {
		t.Errorf("%d requests after the refusals, want %d", n, len(reqs))
	}
}

// o on a PR's row of the factory screen steps into magnum's pane of it
// through the board's own open (magnum open): herdr focuses the judge's
// live pane, and the terminal on magnum's session comes to the front. It
// never goes through the factory daemon.
func TestOOnAPRRowOpensMagnumsPane(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	board := newScreenActions(h.c)
	defer board.Close()
	act := newFactoryActions(loaded(t, h.c), &factoryScreenSource{c: loaded(t, h.c), now: inspNow}, board)
	if _, err := act.OpenPR(h.ctx, "talkable/talkable#5"); err != nil {
		t.Fatalf("open: %v (%s)", err, h.errb.String())
	}
	if len(h.hd.focused) != 1 || h.hd.focused[0] != "mg-talkable-5-judge" || len(h.reveals) != 1 {
		t.Errorf("focused %q with %d reveals, want the judge's pane and the terminal", h.hd.focused, len(h.reveals))
	}
	if fsx.Exists(h.c.Layout.FactoryDB()) {
		t.Error("o on a PR's row made the factory's registry")
	}
}

// r, R, z, M and U on a PR's row of the factory screen run the board's own
// calls (screenActions.Review, Snooze, Unsnooze, Mute, Unmute): magnum's
// review, snooze (as "the board", for 2h, or its lift), mute and unmute
// requests, queued in magnum's registry and answered by magnum's daemon,
// as the board's keys do. They never go through the factory daemon, and a
// screen built without the board's actions refuses each.
func TestThePRKeysOfTheFactoryScreenAreTheBoardsRequests(t *testing.T) {
	h := newActHarness(t)
	h.pid, h.held = 4242, true // a review is queued only while a daemon answers
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, "ok") }
	board := newScreenActions(h.c)
	defer board.Close()
	src := &factoryScreenSource{c: loaded(t, h.c), now: inspNow}
	act := newFactoryActions(loaded(t, h.c), src, board)
	ctx := context.Background()
	const ref = "talkable/talkable#5"
	start := h.now // the review's waits move the clock
	calls := []func(tui.FactoryActions) (tui.ActionResult, error){
		func(a tui.FactoryActions) (tui.ActionResult, error) { return a.Review(ctx, ref, tui.ReviewOpts{}) },
		func(a tui.FactoryActions) (tui.ActionResult, error) {
			return a.Review(ctx, ref, tui.ReviewOpts{Fresh: true})
		},
		func(a tui.FactoryActions) (tui.ActionResult, error) { return a.Snooze(ctx, ref, 2*time.Hour) },
		func(a tui.FactoryActions) (tui.ActionResult, error) { return a.Unsnooze(ctx, ref) },
		func(a tui.FactoryActions) (tui.ActionResult, error) { return a.Mute(ctx, ref) },
		func(a tui.FactoryActions) (tui.ActionResult, error) { return a.Unmute(ctx, ref) },
	}
	for i, call := range calls {
		if _, err := call(act); err != nil {
			t.Fatalf("call %d: %v (%s)", i, err, h.errb.String())
		}
	}
	reqs := h.requests()
	var kinds []string
	for _, r := range reqs {
		kinds = append(kinds, r.Kind)
	}
	if want := []string{engine.ReqReview, engine.ReqReview, engine.ReqSnooze, engine.ReqSnooze, engine.ReqMute, engine.ReqUnmute}; !slices.Equal(kinds, want) {
		t.Fatalf("requests %v in magnum's registry, want %v", kinds, want)
	}
	if p := actDecode[engine.ReviewPayload](t, reqs[0].Payload); p.Fresh || p.Number != 5 {
		t.Errorf("r's review %+v, want a review of #5", p)
	}
	if p := actDecode[engine.ReviewPayload](t, reqs[1].Payload); !p.Fresh {
		t.Errorf("R's review %+v, want a fresh one", p)
	}
	if p := actDecode[engine.SnoozePayload](t, reqs[2].Payload); p.Until.Sub(start) < 2*time.Hour || p.Until.Sub(start) > 2*time.Hour+time.Minute ||
		p.By != "the board" || p.Off {
		t.Errorf("z's snooze %+v, want 2h as the board", p)
	}
	if p := actDecode[engine.SnoozePayload](t, reqs[3].Payload); !p.Off {
		t.Errorf("z's lift %+v, want off", p)
	}
	if fsx.Exists(h.c.Layout.FactoryDB()) {
		t.Error("the screen's PR keys made the factory's registry")
	}

	bare := newFactoryActions(loaded(t, h.c), src, nil)
	for i, call := range calls {
		if _, err := call(bare); !errors.Is(err, errNoBoard) {
			t.Errorf("call %d without the board's actions: %v, want %v", i, err, errNoBoard)
		}
	}
	if n := len(h.requests()); n != len(reqs) {
		t.Errorf("%d requests after the refusals, want %d", n, len(reqs))
	}
}
