package cli

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

func TestAbortHandsTheRunningReviewToTheDaemon(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.pid = 7
	h.onSleep = func(h *actHarness) {
		if h.sleeps == 2 {
			h.completePending(store.RequestDone, "aborted talkable/talkable#5: round stopped, sessions parked, PR baseline")
		}
	}
	if code := h.cmd("abort", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqAbort {
		t.Fatalf("requests %+v", reqs)
	}
	if p := actDecode[engine.TargetPayload](t, reqs[0].Payload); p.Repo != "talkable/talkable" || p.Number != 5 {
		t.Fatalf("payload %+v", p)
	}
	actContains(t, h.errb.String(), "asked the daemon (pid 7) to abort talkable#5 (request 1)")
	actContains(t, h.out.String(), "aborted talkable/talkable#5: round stopped")
}

func TestAbortWithoutARunningReviewFailsAtOnce(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.pid = 7
	if code := h.cmd("abort", "talkable#5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "no review of talkable#5 is running or queued (state reviewed)")
	if len(h.requests()) != 0 || h.kicks != 0 {
		t.Fatal("asked the daemon although nothing runs")
	}
}

// A review that waits in line (a forced one `magnum review` asked for, or an
// automatic one) and a paused round can be taken back: the abort goes to the
// daemon as for a running round. Only the daemon can take it back, so without
// one nothing is queued.
func TestAbortTakesBackAQueuedReview(t *testing.T) {
	for _, state := range []string{store.PRQueued, store.PRRereviewPending, store.PRPaused} {
		h := newActHarness(t)
		pr := h.seedPR("talkable/talkable", 5, state)
		h.setPR(pr.ID, state, func(u *store.PRUpdate) { u.Set("forced", true) })
		h.pid = 7
		h.onSleep = func(h *actHarness) {
			h.completePending(store.RequestDone, "took back the forced review of talkable/talkable#5: PR reviewed")
		}
		if code := h.cmd("abort", "5"); code != 0 {
			t.Fatalf("%s: exit %d: %s", state, code, h.errb.String())
		}
		if reqs := h.requests(); len(reqs) != 1 || reqs[0].Kind != engine.ReqAbort {
			t.Fatalf("%s: requests %+v", state, reqs)
		}
		actContains(t, h.out.String(), "took back the forced review of talkable/talkable#5")

		h.pid = 0
		h.errb.Reset()
		if code := h.cmd("abort", "5"); code != 1 || !strings.Contains(h.errb.String(), "nothing was queued for talkable#5: no daemon is running") {
			t.Fatalf("%s without a daemon: exit %d: %s", state, code, h.errb.String())
		}
	}
}

func TestAbortWithoutADaemonLeavesNoRequestBehind(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewing) // a crashed daemon's row
	if code := h.cmd("abort", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "no daemon is running")
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("the request must not wait for the next daemon: %+v", reqs)
	}
	// The daemon died between the check and the kick: withdrawn.
	h.d.Running = func() (int, error) { return 4242, nil }
	if code := h.cmd("abort", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if reqs := h.requests(); len(reqs) != 1 || reqs[0].State != store.RequestFailed {
		t.Fatalf("the request must not wait for the next daemon: %+v", reqs)
	}
}

func TestAbortReportsAFailureAndATimeout(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRClaiming)
	h.pid = 7
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestFailed, "aborted talkable/talkable#5: round stopped; sessions not parked: busy")
	}
	if code := h.cmd("abort", "5"); code != 1 {
		t.Fatalf("failed abort: exit %d", code)
	}
	actContains(t, h.errb.String(), "sessions not parked: busy")

	h.errb.Reset()
	h.onSleep = nil
	if code := h.cmd("abort", "5", "--timeout", "5s"); code != 1 {
		t.Fatalf("timed out abort: exit %d", code)
	}
	actContains(t, h.errb.String(), "request 2 is still running after 5s")
}

func TestIgnoreQueuesWithoutADaemonAndWaitsWithOne(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	if code := h.cmd("ignore", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "ignore talkable#5 is queued as request 1 and applies when the daemon starts")
	if reqs := h.requests(); len(reqs) != 1 || reqs[0].Kind != engine.ReqIgnore || reqs[0].State != store.RequestPending {
		t.Fatalf("requests %+v", reqs)
	}

	h.errb.Reset()
	h.pid = 7
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "ignored talkable/talkable#5: sessions parked, PR ineligible")
	}
	if code := h.cmd("ignore", "5", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), `"kind": "ignore"`, `"state": "done"`, "PR ineligible")
}

func TestStopUsage(t *testing.T) {
	h := newActHarness(t)
	for _, args := range [][]string{{"abort"}, {"ignore", "1", "2"}} {
		h.errb.Reset()
		if code := h.cmd(args[0], args[1:]...); code != 2 || !strings.Contains(h.errb.String(), "usage: magnum "+args[0]+" <ref>") {
			t.Errorf("%v: exit %d: %s", args, code, h.errb.String())
		}
	}
}
