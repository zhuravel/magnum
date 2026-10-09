package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// noEveryReview turns toast_every_review off (the default): only the rounds
// the operator asked for toast.
func noEveryReview(h *harness) { h.cfg.Herdr.ToastEveryReview = false }

// shown lists the toasts shown so far ("title | body").
func (f *fakeNotifyHerdr) shown() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.toasts...)
}

// flush lets the informational batch come due and sends it.
func (h *harness) flush() {
	h.t.Helper()
	h.advance(2 * time.Minute)
	h.tick()
}

// toastsWith lists the toasts shown so far that contain sub.
func toastsWith(h *harness, sub string) []string {
	var out []string
	for _, t := range h.nh.shown() {
		if strings.Contains(t, sub) {
			out = append(out, t)
		}
	}
	return out
}

// A review the operator asked for toasts when it posts: the verdict, the
// findings by priority and how long it took; automatic rounds stay silent
// while toast_every_review is off.
func TestRequestedRoundToastsTheReviewItPosted(t *testing.T) {
	h := newHarness(t, noEveryReview)
	h.reviewedPR(2, "b1")
	h.flush()
	if got := h.nh.shown(); len(got) != 0 {
		t.Fatalf("an automatic round toasted: %q", got)
	}
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		h.advance(34 * time.Minute)
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 2, ReviewID: 202, Event: "CHANGES_REQUESTED",
			Findings: map[string]int{"P1": 1, "P2": 2}, ReviewCommit: in.TargetSHA, TargetSHA: in.TargetSHA}, nil
	}
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	h.flush()
	got := h.nh.shown()
	if len(got) != 1 || got[0] != "talkable#2: CHANGES_REQUESTED (1 P1, 2 P2) | Your requested review took 34m, posted as talkable[bot]." {
		t.Fatalf("toasts = %q", got)
	}
	h.flush()
	if n := len(h.nh.shown()); n != 1 {
		t.Fatalf("the posted review toasted %d times", n)
	}
}

// [herdr] notify = false silences the requested rounds' toasts too.
func TestRequestedRoundToastsRespectHerdrNotify(t *testing.T) {
	h := newHarness(t, noEveryReview, func(h *harness) { h.d.Notifier.Enabled = false })
	h.reviewedPR(2, "b1")
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	h.flush()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the requested one", n)
	}
	if got := h.nh.shown(); len(got) != 0 {
		t.Fatalf("toasts with notify off: %q", got)
	}
}

// A review the operator asked for toasts why it failed and when it retries;
// an automatic round's failure does not.
func TestRequestedRoundToastsWhyItFailed(t *testing.T) {
	h := newHarness(t, noEveryReview)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeTimeout, Round: 1, Error: "judge_timeout passed without a result"}, nil
	}
	h.queuedPR(2, "b1")
	h.advance(5 * time.Minute)
	h.tick() // the automatic round times out
	h.flush()
	if got := h.nh.shown(); len(got) != 0 {
		t.Fatalf("an automatic round's failure toasted: %q", got)
	}

	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	h.flush()
	got := toastsWith(h, "requested review failed")
	if len(got) != 1 || !strings.Contains(got[0], "timeout: judge_timeout passed without a result") || !strings.Contains(got[0], "retries at") {
		t.Fatalf("toasts = %q", h.nh.shown())
	}
}

// A requested round that waits for a person typing in the PR's panes did not
// fail: no toast.
func TestRequestedRoundWaitingForAPersonDoesNotToastAFailure(t *testing.T) {
	h := newHarness(t, noEveryReview)
	h.reviewedPR(2, "b1")
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Round: 2, Error: "a human is typing"}, agents.ErrHumanActive
	}
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	h.flush()
	if got := toastsWith(h, "failed"); len(got) != 0 {
		t.Fatalf("toasts = %q", got)
	}
}

// A requested round that cannot start for a reason only the operator can
// lift toasts once, after it has waited for it: 2 minutes, 15 for a drain.
func TestRequestedRoundHeldByTheOperatorToastsOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
		after time.Duration
		want  string
	}{
		{"agent kind paused", func(h *harness) {
			h.reviewedPR(2, "b1")
			h.e.setToolPause(h.ctx, "codex", "usage_limit", "limit", h.clock.Now().Add(5*time.Hour))
		}, 2 * time.Minute, "codex paused (usage_limit)"},
		{"identity unhealthy", func(h *harness) {
			h.reviewedPR(2, "b1")
			h.app.checkErr = "the App's key was revoked"
			h.e.setKV(h.ctx, store.KVIdentityCheck("talkable-app"), "fail")
		}, 2 * time.Minute, "identity talkable-app"},
		{"slot kept by its guard", func(h *harness) {
			h.openedPR()
			if err := h.st.UpdateSlotFields(h.ctx, h.slot("review1").ID, func(u *store.SlotUpdate) {
				u.Set("hold_reason", slots.HoldDirtyWorktree)
			}); err != nil {
				h.t.Fatal(err)
			}
		}, 2 * time.Minute, "slot review1 is held: dirty_worktree"},
		{"drain", func(h *harness) {
			h.reviewedPR(2, "b1")
			h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
		}, 15 * time.Minute, "the restart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, noEveryReview)
			tc.setup(h)
			before := len(h.rd.all())
			h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
			h.tick()
			h.advance(tc.after - 30*time.Second)
			h.tick()
			if n := h.e.batch.Pending(); n != 0 || len(toastsWith(h, "can't start")) != 0 {
				t.Fatalf("toasted before the review was held %s", tc.after)
			}
			h.advance(time.Minute)
			h.tick()
			h.flush()
			got := toastsWith(h, "can't start")
			if len(got) != 1 || !strings.HasPrefix(got[0], "talkable#2: requested review can't start | ") || !strings.Contains(got[0], tc.want) {
				t.Fatalf("toasts = %q, want one naming %q", h.nh.shown(), tc.want)
			}
			for range 3 {
				h.advance(10 * time.Minute)
				h.tick()
			}
			if n := len(toastsWith(h, "can't start")); n != 1 {
				t.Fatalf("held toast sent %d times", n)
			}
			if n := len(h.rd.all()); n != before {
				t.Fatalf("a round ran (%d) while it was held", n-before)
			}
		})
	}
}

// `magnum pause` shows how long it has lasted and how many review requests
// it holds, in the tab bar and the registry (for `magnum status`), and a
// person's request it holds toasts once per pause.
func TestPauseShowsSinceAndTheRequestsItHolds(t *testing.T) {
	h := newHarness(t, noEveryReview)
	h.reviewedPR(2, "b1")
	h.enqueue(ReqPause, PausePayload{Reason: "release freeze"})
	h.tick()
	pausedAt := h.clock.Now()
	if at, ok := h.e.kvTime(h.ctx, KVDaemonPausedAt); !ok || !at.Equal(pausedAt) {
		t.Fatalf("%s = %v (%v), want %v", KVDaemonPausedAt, at, ok, pausedAt)
	}
	h.advance(3 * time.Hour)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
	h.flush()

	if _, _, line, ok := readTabBarFile(h.layout.TabBar()); !ok || !strings.Contains(line, "paused 3h · 1 request held") {
		t.Fatalf("tab bar = %q", line)
	}
	if v, _ := kvValue(h, KVDaemonPausedHeld); v != "1" {
		t.Fatalf("%s = %q, want 1", KVDaemonPausedHeld, v)
	}
	want := "talkable#2: alice asked for your review | magnum is paused (since " + pausedAt.Local().Format("15:04") + ")"
	got := toastsWith(h, "asked for your review")
	if len(got) != 1 || !strings.HasPrefix(got[0], want) {
		t.Fatalf("toasts = %q, want one starting %q", h.nh.shown(), want)
	}
	for range 3 {
		h.advance(10 * time.Minute)
		h.tick()
	}
	if n := len(toastsWith(h, "asked for your review")); n != 1 {
		t.Fatalf("the held request toasted %d times in one pause", n)
	}
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d: the pause did not hold the request", n)
	}

	// The resume ends it: the request runs, the pause's records go.
	h.enqueue(ReqResume, PausePayload{})
	h.tick()
	for _, k := range []string{KVDaemonPausedAt, KVDaemonPausedHeld} {
		if _, ok := kvValue(h, k); ok {
			t.Fatalf("%s kept after resume", k)
		}
	}
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d after resume, want the requested one", n)
	}
	if _, _, line, _ := readTabBarFile(h.layout.TabBar()); strings.Contains(line, "paused") {
		t.Fatalf("tab bar after resume = %q", line)
	}
}

// A pause from a build that did not record its start gets the start of its
// daemon.paused event.
func TestPauseStartIsTakenFromItsEventWhenUnrecorded(t *testing.T) {
	h := newHarness(t, noEveryReview)
	h.reviewedPR(2, "b1")
	at := h.clock.Now()
	h.e.setKV(h.ctx, store.KVDaemonPaused, "1")
	h.e.event(h.ctx, "info", "", "daemon.paused", "automation paused", nil)
	h.advance(90 * time.Minute)
	h.tick()
	if got, ok := h.e.kvTime(h.ctx, KVDaemonPausedAt); !ok || !got.Equal(at) {
		t.Fatalf("%s = %v (%v), want the event's %v", KVDaemonPausedAt, got, ok, at)
	}
	if _, _, line, _ := readTabBarFile(h.layout.TabBar()); !strings.Contains(line, "paused 1h") {
		t.Fatalf("tab bar = %q", line)
	}
}
