package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// heldFixDuringADrain has `magnum fix` asked on the operator's reviewed PR
// #2 (two P2 findings at fixHead) while `magnum daemon-restart --drain`
// holds the daemon, and checks the answer: the request is held, not
// refused, and nothing starts or is written to the loop's row meanwhile.
func heldFixDuringADrain(t *testing.T, h *harness) store.PR {
	t.Helper()
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
	id := h.fix(FixPayload{})
	h.handle()
	const want = "held; the fix of talkable/talkable#2 starts after the restart if the review on a1a1a1a still stands (the daemon is draining)"
	if res := h.wantDone(id); res != want {
		t.Fatalf("magnum fix during the drain = %q, want %q", res, want)
	}
	if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); !ok {
		t.Fatal("the held fix is not recorded")
	}
	if ev, ok := h.fixEvent("fix.held"); !ok || ev.Level != "info" || !strings.Contains(ev.Message, "after the restart") {
		t.Fatalf("fix.held = %+v, %v", ev, ok)
	}
	again := h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(again); !strings.HasPrefix(res, "held already (asked ") {
		t.Fatalf("a second magnum fix during the drain = %q", res)
	}
	h.tick()
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Fatalf("fix runs = %d during the drain, want none", n)
	}
	if st, ok := h.fixStateOf(2); ok {
		t.Fatalf("the held request wrote the loop's row: %+v", st)
	}
	return pr
}

// From 2026-10-14 on, `magnum fix` during a drain was refused ("the daemon
// is draining; try after the restart"): talkable#11932's next fix came two
// days later, talkable#11483's four hours later. The request is now held
// and answered so, and after the restart, or once the drain is lifted
// without one, the fix starts cycle 1 on the review that still stands, as
// the request would have, with nothing dropped.
func TestAFixAskedDuringADrainStartsAfterTheRestartWhenTheReviewStands(t *testing.T) {
	for _, end := range []string{"restart", "lifted"} {
		t.Run(end, func(t *testing.T) {
			h, git := fixHarness(t)
			pr := heldFixDuringADrain(t, h)
			pushingFixer(h, git, fixPushed)

			if end == "restart" {
				h.startup()
			} else {
				if err := h.st.DeleteKV(h.ctx, KVDaemonDraining); err != nil { // the CLI's endDrain
					t.Fatal(err)
				}
				h.tick()
			}
			runs := h.fixRuns(pr.ID)
			if len(runs) != 1 || runs[0].TargetSHA != fixHead {
				t.Fatalf("fix runs after the drain = %+v, want cycle 1 on %s", runs, fixHead)
			}
			if st, ok := h.fixStateOf(2); !ok || st.Cycle != 1 || st.By != fixBy || st.Since != fixHead {
				t.Errorf("fix state after the drain = %+v, %v; want the loop the request started, at cycle 1", st, ok)
			}
			if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); ok {
				t.Error("the held fix outlived its start")
			}
			if ev, ok := h.fixEvent("fix.dropped"); ok {
				t.Errorf("fix.dropped recorded: %s", ev.Message)
			}
		})
	}
}

// A held fix whose PR moved on during the drain (the operator pushed, so
// the review no longer stands at the head) is dropped after the restart
// with fix.dropped naming why and a toast, and no fixer starts.
func TestAFixAskedDuringADrainIsDroppedWhenTheHeadMoved(t *testing.T) {
	const moved = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	h, git := fixHarness(t)
	pr := heldFixDuringADrain(t, h)
	pushingFixer(h, git, fixPushed)
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: moved, author: "zhuravel"}) // a push during the drain
	if cur := h.pr(2); cur.HeadSHA != moved {
		t.Fatalf("head %s, want the push %s", cur.HeadSHA, moved)
	}

	h.startup()
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Fatalf("fix runs = %d after the restart, want none", n)
	}
	ev, ok := h.fixEvent("fix.dropped")
	if !ok || ev.Level != "warn" || !strings.Contains(ev.Message, "the head moved to c3c3c3c since it was asked on a1a1a1a") ||
		!strings.Contains(ev.Message, "after the restart") {
		t.Fatalf("fix.dropped = %+v, %v", ev, ok)
	}
	if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); ok {
		t.Error("the dropped fix is still held")
	}
	if st, ok := h.fixStateOf(2); ok {
		t.Errorf("a loop started: %+v", st)
	}
	h.flush()
	if got := toastsWith(h, "fix dropped"); len(got) != 1 {
		t.Errorf("toasts = %v, want one saying the held fix was dropped", h.nh.shown())
	}
}

// `magnum abort` takes a held fix back: nothing starts after the restart.
func TestAbortDropsAFixHeldForTheRestart(t *testing.T) {
	h, git := fixHarness(t)
	pr := heldFixDuringADrain(t, h)
	pushingFixer(h, git, fixPushed)
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if res := h.wantDone(id); res != "aborted talkable/talkable#2: the fix held for the restart is dropped" {
		t.Fatalf("magnum abort = %q", res)
	}
	if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); ok {
		t.Fatal("the held fix outlived the abort")
	}
	h.startup()
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Fatalf("fix runs = %d after the restart, want none", n)
	}
}
