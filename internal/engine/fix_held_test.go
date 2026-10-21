package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
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
	if res := h.wantDone(id); res != "aborted talkable/talkable#2: the fix held during a drain is dropped" {
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

// pauseClaude pauses the fixer's kind, claude, for its usage limit until
// until.
func pauseClaude(h *harness, until time.Time) {
	h.e.setToolPause(h.ctx, agents.KindClaude, string(agents.HealthUsageLimit), "", until)
}

// On 2026-10-19 talkable#12054's fix, asked during a drain, was dropped
// after the restart because the fixer's kind was paused ("claude paused
// (usage_limit) until 14:00"), and the operator had to ask again by hand. A
// held fix whose fixer's kind is paused when it would start now waits out
// the pause: it stays held (kind_pause) with no fix.dropped, `magnum fix
// --wait` says it starts when the pause ends, and the tick after the pause
// ends starts it. A pause hold whose PR moved on meanwhile is dropped, as a
// drain's is.
func TestAFixHeldThroughADrainWaitsOutTheFixersPause(t *testing.T) {
	const moved = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	for _, push := range []bool{false, true} {
		t.Run(map[bool]string{false: "the pause ends", true: "the head moved"}[push], func(t *testing.T) {
			h, git := fixHarness(t)
			pr := heldFixDuringADrain(t, h)
			pushingFixer(h, git, fixPushed)
			pauseClaude(h, h.clock.Now().Add(2*time.Hour))
			h.startup() // the restart, with the fixer's kind still paused
			if n := len(h.fixRuns(pr.ID)); n != 0 {
				t.Fatalf("fix runs = %d while claude is paused, want none", n)
			}
			if ev, ok := h.fixEvent("fix.dropped"); ok {
				t.Fatalf("the held fix was dropped for the pause: %s", ev.Message)
			}
			raw, ok := kvValue(h, KVPRFixHeld(pr.ID))
			if !ok || !strings.Contains(raw, `"why":"kind_pause"`) || HeldFixStarts(raw) != "when claude's pause ends" {
				t.Fatalf("held fix = %q, %v; want it held for claude's pause", raw, ok)
			}
			h.tick()
			if n := len(h.fixRuns(pr.ID)); n != 0 {
				t.Fatalf("fix runs = %d while claude is paused, want none", n)
			}
			if push {
				reqPoll(h, prSpec{n: 2, head: moved, author: "zhuravel"}) // a push during the pause
			}

			h.advance(2*time.Hour + time.Minute) // the pause ends
			h.tick()
			if push {
				if n := len(h.fixRuns(pr.ID)); n != 0 {
					t.Fatalf("fix runs = %d after the push, want none", n)
				}
				ev, ok := h.fixEvent("fix.dropped")
				if !ok || ev.Level != "warn" || !strings.Contains(ev.Message, "while claude was paused") ||
					!strings.Contains(ev.Message, "the head moved to c3c3c3c since it was asked on a1a1a1a") {
					t.Fatalf("fix.dropped = %+v, %v", ev, ok)
				}
				if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); ok {
					t.Error("the dropped fix is still held")
				}
				return
			}
			runs := h.fixRuns(pr.ID)
			if len(runs) != 1 || runs[0].TargetSHA != fixHead {
				t.Fatalf("fix runs after the pause = %+v, want cycle 1 on %s", runs, fixHead)
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

// A fix held for the GitHub budget whose fixer's kind is paused when the
// budget recovers waits for the pause instead of being dropped, and one
// held for the pause whose budget is low when the pause ends waits for the
// budget: no hold is dropped for a reason the daemon can wait out.
func TestAHeldFixPassesFromTheBudgetToThePauseAndBack(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	pushingFixer(h, git, fixPushed)
	h.lowBudget("zhuravel", 300, h.clock.Now().Add(30*time.Minute))
	id := h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(id); !strings.HasPrefix(res, "held; ") {
		t.Fatalf("magnum fix below the floor = %q", res)
	}
	pauseClaude(h, h.clock.Now().Add(time.Hour))

	h.advance(31 * time.Minute) // the budget recovers; claude is still paused
	h.tick()
	raw, ok := kvValue(h, KVPRFixHeld(pr.ID))
	if !ok || HeldFixStarts(raw) != "when claude's pause ends" {
		t.Fatalf("held fix after the budget's recovery = %q, %v; want it held for claude's pause", raw, ok)
	}
	h.lowBudget("zhuravel", 200, h.clock.Now().Add(time.Hour))

	h.advance(30 * time.Minute) // the pause ends; the budget is low again
	h.tick()
	raw, ok = kvValue(h, KVPRFixHeld(pr.ID))
	if !ok || HeldFixStarts(raw) != "when the GitHub budget recovers" {
		t.Fatalf("held fix after the pause = %q, %v; want it held for the GitHub budget", raw, ok)
	}
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Fatalf("fix runs = %d while held, want none", n)
	}

	h.advance(31 * time.Minute)
	h.tick()
	if runs := h.fixRuns(pr.ID); len(runs) != 1 || runs[0].TargetSHA != fixHead {
		t.Fatalf("fix runs once nothing holds it = %+v, want cycle 1 on %s", runs, fixHead)
	}
	if ev, ok := h.fixEvent("fix.dropped"); ok {
		t.Errorf("fix.dropped recorded: %s", ev.Message)
	}
}

// `magnum abort` takes back a held fix in the words of the hold it was in:
// a drain's, the GitHub budget's or the fixer's pause, no longer "the fix
// held for the restart" for all three.
func TestAbortingAFixTheBudgetHeldSaysSo(t *testing.T) {
	for _, c := range []struct {
		name, note string
		hold       func(t *testing.T, h *harness)
	}{
		{"drain", "the fix held during a drain is dropped", func(t *testing.T, h *harness) {
			t.Helper()
			heldFixDuringADrain(t, h)
		}},
		{"budget", "the fix held while the GitHub budget was low is dropped", func(t *testing.T, h *harness) {
			t.Helper()
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			h.lowBudget("zhuravel", 300, h.clock.Now().Add(30*time.Minute))
			id := h.fix(FixPayload{})
			h.handle()
			if res := h.wantDone(id); !strings.HasPrefix(res, "held; ") {
				t.Fatalf("magnum fix below the floor = %q", res)
			}
		}},
		{"pause", "the fix held while claude was paused is dropped", func(t *testing.T, h *harness) {
			t.Helper()
			heldFixDuringADrain(t, h)
			pauseClaude(h, h.clock.Now().Add(2*time.Hour))
			h.startup()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, git := fixHarness(t)
			c.hold(t, h)
			pushingFixer(h, git, fixPushed)
			pr := h.pr(2)
			if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); !ok {
				t.Fatal("no held fix to abort")
			}
			id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
			h.handle()
			if res := h.wantDone(id); res != "aborted talkable/talkable#2: "+c.note {
				t.Errorf("magnum abort = %q, want %q", res, "aborted talkable/talkable#2: "+c.note)
			}
			if ev, ok := h.fixEvent("fix.dropped"); !ok || ev.Message != c.note+" by magnum abort" {
				t.Errorf("fix.dropped = %+v, %v", ev, ok)
			}
			if _, ok := kvValue(h, KVPRFixHeld(pr.ID)); ok {
				t.Error("the held fix outlived the abort")
			}
		})
	}
}
