package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// operatorQuietHours is the operator's quiet_hours: the harness's day
// (10:00 on) is inside them until 12:00.
const operatorQuietHours = "03:00-12:00"

// The live case: quiet_hours (03:00-12:00) held about 11% of colleagues'
// pushes until 09:00 UTC, small delta checks and reply rounds included,
// which run the judge alone for a few minutes. Inside quiet hours a 7-line
// delta check, a reply round and a review request of the reviewed head (the
// judge alone) start; a first review, a 200-line re-review and a review
// request of it wait, their wait saying "quiet hours"; `magnum review`
// starts a full round as before.
func TestQuietHoursLetTheJudgeAloneRun(t *testing.T) {
	quiet := func(h *harness) { h.cfg.Daemon.QuietHours = operatorQuietHours }
	wantHeld := func(t *testing.T, h *harness, rounds int, short string) {
		t.Helper()
		reqWantRounds(t, h, rounds)
		w := waitOf(t, h, 2)
		if w.Reason != WaitQuietHours || w.Short(h.clock.Now()) != short {
			t.Fatalf("wait = %+v (%q), want %q", w, w.Short(h.clock.Now()), short)
		}
	}

	t.Run("a 7-line delta check starts", func(t *testing.T) {
		h := newHarness(t)
		pushedWithDelta(t, h, codeDelta(7)) // b2 at 10:45
		quiet(h)
		h.tick()
		if w := waitOf(t, h, 2); !w.DeltaCheck || w.Reason != WaitQuiet {
			t.Fatalf("wait = %+v, want the delta check after the push quiet period", w)
		}
		h.advance(5 * time.Minute)
		h.tick()
		ins := h.rd.all()
		if len(ins) != 2 || ins[1].DeltaCheck == nil || ins[1].DeltaCheck.Lines != 7 || ins[1].TargetSHA != "b2" {
			t.Fatalf("rounds = %d, want the delta check of b2 inside quiet hours", len(ins))
		}
	})

	t.Run("a Codex-flagged PR's delta check does not", func(t *testing.T) {
		h := newHarness(t)
		pr := pushedWithDelta(t, h, codeDelta(7))
		quiet(h)
		if err := h.e.setCodexFlag(h.ctx, pr.ID, CodexFlag{Kind: "codex", Head: "b1", At: h.clock.Now(), By: "magnum codex-flag"}); err != nil {
			t.Fatal(err)
		}
		h.advance(5 * time.Minute)
		h.tick()
		reqWantRounds(t, h, 1)
		h.wantState(2, store.PRIneligible)
	})

	t.Run("a reply round starts", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		quiet(h)
		h.advance(10 * time.Minute)
		reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{threadReply(h, "alice")}})
		if w := waitOf(t, h, 2); w.Reason != WaitReplies || w.Replies != 1 {
			t.Fatalf("wait = %+v, want the reply debounce", w)
		}
		repliedRounds(h)
		h.advance(3 * time.Minute)
		h.tick()
		ins := h.rd.all()
		if len(ins) != 2 || ins[1].Replies != 1 || !ins[1].SameHead || ins[1].TargetSHA != "b1" {
			t.Fatalf("rounds = %d, want the reply round inside quiet hours", len(ins))
		}
	})

	t.Run("a review request of the reviewed head starts", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		quiet(h)
		h.advance(time.Minute)
		reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
		h.advance(time.Minute)
		h.tick()
		ins := h.rd.all()
		if len(ins) != 2 || !ins[1].SameHead || ins[1].TargetSHA != "b1" {
			t.Fatalf("rounds = %d, want the judge alone on the reviewed head inside quiet hours", len(ins))
		}
	})

	t.Run("a first review waits", func(t *testing.T) {
		h := newHarness(t, quiet)
		h.open(prSpec{n: 1, head: "base1"})
		h.startup()
		h.tick()
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "a1"})
		h.tick()
		h.advance(5 * time.Minute)
		h.tick()
		h.wantState(2, store.PRQueued)
		wantHeld(t, h, 0, "review · quiet hours → 12:00")
	})

	t.Run("a 200-line re-review waits", func(t *testing.T) {
		h := newHarness(t)
		pushedWithDelta(t, h, codeDelta(200))
		quiet(h)
		h.advance(time.Hour)
		h.tick()
		wantHeld(t, h, 1, "re-review · quiet hours → 12:00")
		if w := waitOf(t, h, 2); w.DeltaCheck {
			t.Fatalf("wait = %+v, want a full re-review", w)
		}
	})

	t.Run("a review request of 200 lines waits", func(t *testing.T) {
		h := newHarness(t)
		pushedWithDelta(t, h, codeDelta(200))
		quiet(h)
		h.advance(time.Minute)
		reqPoll(h, prSpec{n: 2, head: "b2", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
		h.advance(time.Hour)
		h.tick()
		wantHeld(t, h, 1, "re-review · quiet hours → 12:00")
	})

	t.Run("magnum review starts a full round", func(t *testing.T) {
		h := newHarness(t)
		pushedWithDelta(t, h, codeDelta(200))
		quiet(h)
		h.advance(time.Hour)
		if _, err := h.st.EnqueueRequest(h.ctx, ReqReview, ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}}); err != nil {
			t.Fatal(err)
		}
		h.tick()
		ins := h.rd.all()
		if len(ins) != 2 || ins[1].Kind != pipeline.KindRereview || ins[1].DeltaCheck != nil || ins[1].SameHead || ins[1].TargetSHA != "b2" {
			t.Fatalf("rounds = %d, want the forced full re-review of b2", len(ins))
		}
	})
}

// The quiet-hours gate for each kind of round: inside quiet hours a round
// of the judge alone on a re-review (a delta check, a re-review of the same
// head, a reply round, requested or not) and a forced round start; a first
// review, a full re-review (a requested one too) and the continue of a
// paused turn wait, with the gate's sentence. Outside them nothing waits.
func TestQuietHoursHoldOnlyFullRoundsAndContinues(t *testing.T) {
	h := newHarness(t)
	h.startup()
	h.tick()
	rereview := func(mod func(j *roundJob)) *roundJob {
		j := &roundJob{kind: pipeline.KindRereview, pr: store.PR{ReviewedSHA: store.Ptr("b1"), HeadSHA: "b2"}}
		if mod != nil {
			mod(j)
		}
		return j
	}
	cases := []struct {
		name  string
		job   *roundJob
		waits bool
	}{
		{"a first review", &roundJob{kind: pipeline.KindInitial}, true},
		{"a full re-review", rereview(nil), true},
		{"a requested full re-review", rereview(func(j *roundJob) { j.requested = true }), true},
		{"a continued turn", rereview(func(j *roundJob) { j.kind, j.continued = kindContinue, true }), true},
		{"a continued delta check", rereview(func(j *roundJob) { j.kind, j.continued, j.deltaCheck = kindContinue, true, true }), true},
		{"a continue whose checkout is gone", rereview(func(j *roundJob) { j.continued = true }), true},
		{"a delta check", rereview(func(j *roundJob) { j.deltaCheck = true }), false},
		{"a re-review of the same head", rereview(func(j *roundJob) { j.sameHead = true }), false},
		{"a requested re-review of the same head", rereview(func(j *roundJob) { j.sameHead, j.requested = true, true }), false},
		{"a reply round", rereview(func(j *roundJob) { j.sameHead, j.replies = true, 2 }), false},
		{"a forced first review", &roundJob{kind: pipeline.KindInitial, pr: store.PR{Forced: true}}, false},
		{"a forced full re-review", rereview(func(j *roundJob) { j.pr.Forced = true }), false},
	}
	h.cfg.Daemon.QuietHours = operatorQuietHours
	for _, tc := range cases {
		g := h.e.quietHoursGate(tc.job)
		if (g.text != "") != tc.waits {
			t.Errorf("%s: gate = %+v, want waiting %v", tc.name, g, tc.waits)
		}
		if tc.waits && (g.reason != WaitQuietHours || !strings.Contains(g.text, "quiet hours ("+operatorQuietHours+")")) {
			t.Errorf("%s: gate = %+v", tc.name, g)
		}
	}
	for _, spec := range []string{"", "13:00-03:00"} {
		h.cfg.Daemon.QuietHours = spec
		for _, tc := range cases {
			if g := h.e.quietHoursGate(tc.job); g.text != "" {
				t.Errorf("quiet_hours %q at 10:00: %s waits: %+v", spec, tc.name, g)
			}
		}
	}
}

// A commit that arrives during a review as a small delta gets a check by
// itself inside quiet hours, so the review's note promises it, as outside
// them; a delta a full re-review needs is "not reviewed yet" there
// (TestMovedHeadNoteFollowsThePRsWait).
func TestMovedHeadNotePromisesTheDeltaCheckInsideQuietHours(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Daemon.RereviewMinLines, h.cfg.Daemon.DeltaCheck = 30, true })
	notes := pushDuringReview(t, h, rubyMixed, func() { h.cfg.Daemon.QuietHours = operatorQuietHours })
	want := "101:_Reviewed b1; 1 commit arrived during the review, a short check of that commit follows after the quiet period._"
	if !slices.Equal(notes, []string{want}) {
		t.Fatalf("notes = %q, want %q", notes, want)
	}
}
