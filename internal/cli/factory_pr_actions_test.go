package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
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
	act := newFactoryActions(h.c, &factoryScreenSource{c: h.c, now: inspNow}, board)
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
	act := newFactoryActions(h.c, &factoryScreenSource{c: h.c, now: inspNow}, board)
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
