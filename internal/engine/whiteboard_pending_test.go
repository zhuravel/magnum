package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// wbReviewedUnderDrain has PR #2's first round post its review at fixHead
// while a drain for a restart holds the daemon: the round is in flight when
// the drain starts, and a drain waits for the rounds in flight, so their
// reviews post inside it. The drain names no drainer (only the restart, or
// its command's own end, lifts it), so the test's ticks keep it.
func (w *wbEnv) wbReviewedUnderDrain() store.PR {
	w.t.Helper()
	w.open(prSpec{n: 1, head: "base1"})
	w.startup()
	w.tick() // first sync: #1 baseline
	w.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead})
	w.tick() // #2 queued
	w.advance(5 * time.Minute)
	w.rd.mu.Lock()
	w.rd.gate = make(chan struct{})
	w.rd.mu.Unlock()
	before := len(w.rd.all())
	if err := w.e.Tick(w.ctx); err != nil {
		w.t.Fatal(err)
	}
	w.awaitRound(before) // dispatched: the round waits at the gate
	w.e.setKV(w.ctx, KVDaemonDraining, store.FormatTime(w.clock.Now()))
	close(w.rd.gate)
	w.settle() // the review posts under the drain
	w.rd.mu.Lock()
	w.rd.gate = nil
	w.rd.mu.Unlock()
	return w.wantState(2, store.PRReviewed)
}

// wbPending is PR prID's pending whiteboard (ok false: none).
func (w *wbEnv) wbPending(prID int64) (whiteboardPending, bool) {
	w.t.Helper()
	v, ok := kvValue(w.harness, kvPRWhiteboardPending(prID))
	if !ok {
		return whiteboardPending{}, false
	}
	p, parsed := parseWhiteboardPending(v)
	if !parsed {
		w.t.Fatalf("pending whiteboard %q does not parse", v)
	}
	return p, true
}

// wbHeadRuns are PR prID's whiteboard runs of head.
func (w *wbEnv) wbHeadRuns(prID int64, head string) []store.Run {
	w.t.Helper()
	var out []store.Run
	for _, r := range w.wbRuns(prID) {
		if r.TargetSHA == head {
			out = append(out, r)
		}
	}
	return out
}

// wbWantSkippedForTheDrain fails unless PR #2's whiteboard was skipped for
// the drain with the promise that it is made after the restart, and the PR
// and the reviewed head are remembered.
func (w *wbEnv) wbWantSkippedForTheDrain(pr store.PR) {
	w.t.Helper()
	if runs := w.wbRuns(pr.ID); len(runs) != 0 {
		w.t.Fatalf("whiteboard runs during the drain = %+v", runs)
	}
	ev, ok := w.fixEvent("whiteboard.skipped")
	if !ok || ev.Message != "the daemon is draining; it is made after the restart" || ev.Level != "info" || !strings.Contains(string(ev.Data), fixHead) {
		w.t.Errorf("whiteboard.skipped = %+v, %v", ev, ok)
	}
	if p, ok := w.wbPending(pr.ID); !ok || p.Head != fixHead || p.At.IsZero() {
		w.t.Fatalf("pending whiteboard = %+v, %v; want the reviewed head %s and the time", p, ok, fixHead)
	}
}

// A review that posts while the daemon drains for a restart has its
// whiteboard skipped with the promise that it is made after the restart,
// and the PR and its reviewed head are remembered (pr.<id>.whiteboard_pending):
// on 2026-10-14 to 10-17, 23 of 87 whiteboards were skipped by drains and
// 22 of those heads never got one, since the next posted review was the
// only next chance. A drain that ends without a restart (its command ends:
// interrupted or timed out) makes it then: the job runs for that head, and
// the entry is gone. `magnum whiteboard` during the drain is refused as
// before: the operator asks again.
func TestAWhiteboardSkippedByADrainIsMadeWhenTheDrainEnds(t *testing.T) {
	w := wbHarness(t, true)
	wbWriter(w.harness, wbDone(wbSession))
	pr := w.wbReviewedUnderDrain()
	w.wbWantSkippedForTheDrain(pr)

	id := w.wbRequest()
	w.wbAnswered(id)
	w.wbFailed(id, "the daemon is draining; try after the restart")

	if err := w.st.DeleteKV(w.ctx, KVDaemonDraining); err != nil { // the drain's command ends (its endDrain)
		t.Fatal(err)
	}
	w.tick()
	runs := w.wbRuns(pr.ID)
	if len(runs) != 1 || runs[0].TargetSHA != fixHead || runs[0].State != store.RunVerified {
		t.Fatalf("whiteboard runs after the drain = %+v, want one written for %s", runs, fixHead)
	}
	if rec, ok := w.wbRecord(); !ok || rec.Head != fixHead || rec.Session != wbSession {
		t.Errorf("whiteboard record = %+v, %v", rec, ok)
	}
	if p, ok := w.wbPending(pr.ID); ok {
		t.Errorf("the pending whiteboard stays after its job started: %+v", p)
	}
	w.tick()
	if runs := w.wbRuns(pr.ID); len(runs) != 1 {
		t.Errorf("whiteboard runs = %+v, want the one", runs)
	}
	w.wantState(2, store.PRReviewed)
}

// A whiteboard a drain skipped is made at the start the drain waited for:
// the restart runs the job once for the remembered head and drops the
// entry, so a later tick or start makes no second.
func TestAPendingWhiteboardIsMadeAfterTheRestart(t *testing.T) {
	w := wbHarness(t, true)
	wbWriter(w.harness, wbDone(wbSession))
	pr := w.wbReviewedUnderDrain()
	w.wbWantSkippedForTheDrain(pr)

	w.startup() // the restart the drain waited for
	if v, ok := kvValue(w.harness, KVDaemonDraining); ok {
		t.Fatalf("the drain survived the restart: %q", v)
	}
	runs := w.wbRuns(pr.ID)
	if len(runs) != 1 || runs[0].TargetSHA != fixHead || runs[0].State != store.RunVerified {
		t.Fatalf("whiteboard runs after the restart = %+v, want one written for %s", runs, fixHead)
	}
	if got := kindOf(subjectEvents(t, w.harness, "pr:talkable/talkable#2"), "whiteboard.start"); len(got) != 1 {
		t.Errorf("whiteboard.start events = %+v, want one", got)
	}
	if p, ok := w.wbPending(pr.ID); ok {
		t.Errorf("the pending whiteboard stays after its job started: %+v", p)
	}
	w.tick()
	w.startup()
	if runs := w.wbRuns(pr.ID); len(runs) != 1 {
		t.Errorf("whiteboard runs = %+v, want the one", runs)
	}
}

// A pending whiteboard whose PR moved on before the drain ended is dropped:
// a push makes the remembered head stale (whiteboard.stopped "superseded by
// a push") and a close leaves the whiteboard no reader ("the PR closed").
// No job starts for the remembered head and the entry is gone; the push's
// own review, when one posts, has its own whiteboard.
func TestAPendingWhiteboardIsDroppedWhenTheHeadMoved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		moved func(w *wbEnv, pr store.PR)
		why   string
	}{
		{"pushed", func(w *wbEnv, _ store.PR) {
			w.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixPushed})
			w.tick() // the push, seen under the drain
			if got := w.pr(2).HeadSHA; got != fixPushed {
				w.t.Fatalf("head = %s, want the push %s", got, fixPushed)
			}
		}, "superseded by a push"},
		{"closed", func(w *wbEnv, _ store.PR) {
			w.open(prSpec{n: 1, head: "base1"})
			w.gh.mu.Lock()
			w.gh.closed[2] = "CLOSED"
			w.gh.mu.Unlock()
			w.tick() // the close, seen under the drain
			w.advance(confirmGap)
			w.tick() // and confirmed
			if got := w.pr(2).GHState; got == store.GHOpen {
				w.t.Fatalf("GitHub state = %s, want the close", got)
			}
		}, "the PR closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := wbHarness(t, true)
			wbWriter(w.harness, wbDone(wbSession))
			pr := w.wbReviewedUnderDrain()
			w.wbWantSkippedForTheDrain(pr)
			tc.moved(w, pr)

			if err := w.st.DeleteKV(w.ctx, KVDaemonDraining); err != nil { // the drain's command ends (its endDrain)
				t.Fatal(err)
			}
			w.tick()
			if runs := w.wbHeadRuns(pr.ID, fixHead); len(runs) != 0 {
				t.Errorf("whiteboard runs of the remembered head %s = %+v, want none", fixHead, runs)
			}
			ev, ok := w.fixEvent("whiteboard.stopped")
			if !ok || !strings.Contains(ev.Message, "stopped: "+tc.why) || ev.Level != "info" || !strings.Contains(string(ev.Data), fixHead) {
				t.Errorf("whiteboard.stopped = %+v, %v; want %q", ev, ok, tc.why)
			}
			if p, ok := w.wbPending(pr.ID); ok {
				t.Errorf("the pending whiteboard stays after it was dropped: %+v", p)
			}
		})
	}
}
