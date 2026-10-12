package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// confirmCloseMidRound takes #n off the OPEN list, GitHub answering state
// for it, and runs the three polls that confirm the close (the last a
// minute after the first) without waiting for rounds: a round held in the
// pipeline would never end.
func (h *harness) confirmCloseMidRound(n int, state string) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.gh.mu.Lock()
	h.gh.closed[n] = state
	h.gh.mu.Unlock()
	h.pollsMidRound()
}

// pollsMidRound runs three polls 30s apart without waiting for rounds.
func (h *harness) pollsMidRound() {
	h.t.Helper()
	for range 3 {
		h.advance(30 * time.Second)
		if err := h.e.Tick(h.ctx); err != nil {
			h.t.Fatalf("tick: %v", err)
		}
	}
}

// releaseRound lets a round held at h.rd.gate go on, and waits for it.
func (h *harness) releaseRound() {
	h.t.Helper()
	h.rd.mu.Lock()
	gate := h.rd.gate
	h.rd.gate = nil
	h.rd.mu.Unlock()
	if gate != nil {
		close(gate)
	}
	h.settle()
}

// In the window three rounds ran on PRs already merged, 34 role-minutes in
// all, one of them posting a review 11 minutes after the merge. A round in
// flight when the poll confirms the merge is now stopped as `magnum abort`
// stops one: its judge is interrupted, its runs are abandoned with the
// close as their error, its sessions parked and its pool slot handed back,
// and one round.stopped event says so with the outcome closed. The PR stays
// closed and is released after the grace as before.
func TestAConfirmedCloseStopsTheRound(t *testing.T) {
	var kh *keyHerdr
	h := newHarness(t, withKeys(&kh))
	pr := h.runningRound(7, "h7", false)
	h.working(7)
	run, err := h.judgeRun(pr, "initial", "h7", store.RunSubmitted, "", "")
	if err != nil {
		t.Fatal(err)
	}

	h.confirmCloseMidRound(7, "MERGED")
	for deadline := time.Now().Add(10 * time.Second); h.e.roundActive(pr.ID); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the round still runs on a merged PR")
		}
	}
	h.settle()
	cur := h.wantState(7, store.PRClosed)
	if cur.GHState != store.GHMerged || cur.ReleaseAfter == nil {
		t.Fatalf("closed PR: gh_state %s release_after %v", cur.GHState, cur.ReleaseAfter)
	}
	if got, want := kh.all(), []string{"agent:mg-t-7-codex-judge:ctrl+c", "agent:mg-t-7-codex-judge:ctrl+c"}; !slices.Equal(got, want) {
		t.Errorf("keys %v, want %v", got, want)
	}
	got, err := h.st.RunByID(h.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.RunAbandoned || deref(got.Error) != "stopped: the PR merged on GitHub" {
		t.Errorf("judge run %s %q, want abandoned by the merge", got.State, deref(got.Error))
	}
	if h.ag.count("park:"+itoa(pr.ID)) == 0 {
		t.Error("sessions not parked")
	}
	if !slices.Contains(h.sl.all(), "release:review1:merged") {
		t.Errorf("slot calls %v, want review1 released as merged", h.sl.all())
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Errorf("slot not handed back: %s pr=%v", sl.State, sl.PRID)
	}

	stopped := kindOf(subjectEvents(t, h, "pr:talkable/talkable#7"), "round.stopped")
	if n := len(h.events("round.stopped")); len(stopped) != 1 || n != 1 {
		t.Fatalf("round.stopped events = %d (%d on #7), want 1 on #7", n, len(stopped))
	}
	want := "round 1 stopped: the PR merged on GitHub (confirmed 3×); 1 agent interrupted, 1 run abandoned, sessions parked, slot review1 released"
	if stopped[0].Message != want || stopped[0].Level != "info" {
		t.Errorf("round.stopped = %s %q, want info %q", stopped[0].Level, stopped[0].Message, want)
	}
	var data struct {
		Outcome   string `json:"outcome"`
		Round     int    `json:"round"`
		GHState   string `json:"gh_state"`
		Confirmed int    `json:"confirmed"`
	}
	if err := json.Unmarshal(stopped[0].Data, &data); err != nil || data.Outcome != pipeline.OutcomeClosed || data.Round != 1 ||
		data.GHState != store.GHMerged || data.Confirmed != 3 {
		t.Errorf("round.stopped data %s: %+v (%v)", stopped[0].Data, data, err)
	}
	closed := h.events("pr.closed")
	if len(closed) != 1 || closed[0].Message != "merged on GitHub (confirmed 3×); stopping its round; slot handed back when it stops" {
		t.Errorf("pr.closed events = %+v", closed)
	}
	if h.hasEvent("pr:talkable/talkable#7", "pr.aborted") {
		t.Error("a close is no abort: pr.aborted recorded")
	}

	h.advance(10*time.Minute + time.Second)
	h.tick()
	h.wantState(7, store.PRReleased)
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want the one the merge stopped", n)
	}
}

// A round the close finds in its checkout, before any of its agents was
// prompted, stops there: no round reaches the pipeline, the event has no
// round number to give, and the slot goes back once the checkout returned
// (TestCloseGraceWaitsForARoundInItsCheckout keeps a forced one's).
func TestAConfirmedCloseStopsARoundInItsCheckout(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick() // #2 queued
	h.sl.mu.Lock()
	h.sl.checkoutGate = make(chan struct{})
	h.sl.checkoutStarted = make(chan struct{}, 1)
	started := h.sl.checkoutStarted
	h.sl.mu.Unlock()
	h.advance(5 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the round never reached its checkout")
	}
	pr := h.pr(2)

	h.confirmCloseMidRound(2, "CLOSED")
	for deadline := time.Now().Add(10 * time.Second); h.e.roundActive(pr.ID); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the round still runs on a closed PR")
		}
	}
	h.settle()

	h.wantState(2, store.PRClosed)
	if n := len(h.rd.all()); n != 0 {
		t.Fatalf("rounds in the pipeline = %d, want none", n)
	}
	stopped := kindOf(subjectEvents(t, h, "pr:talkable/talkable#2"), "round.stopped")
	want := "round stopped: the PR closed on GitHub (confirmed 3×); sessions parked, slot review1 released"
	if len(stopped) != 1 || stopped[0].Message != want {
		t.Fatalf("round.stopped events = %+v, want one %q", stopped, want)
	}
	if !slices.Contains(h.sl.all(), "release:review1:closed") {
		t.Fatalf("slot calls %v, want review1 released as closed", h.sl.all())
	}
}

// A close on GitHub stops only the rounds nobody asked for: one forced by
// `magnum review` (the board's r too), one a review request started, and a
// post-merge review (a merged PR, which the poll never confirms again) run
// on as before, and the PR's close waits for them.
func TestAConfirmedCloseLeavesARoundSomeoneAskedForRunning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(h *harness) int // starts the round, held in the pipeline; returns the PR's number
		close func(h *harness, n int)
	}{
		{"forced", func(h *harness) int {
			h.open(prSpec{n: 1, head: "base1"})
			h.startup()
			h.tick() // first sync: #1 baseline
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: "h7"})
			h.tick() // #7 queued
			h.holdRounds()
			before := len(h.rd.all())
			h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "7"}})
			h.advance(time.Hour)
			if err := h.e.Tick(h.ctx); err != nil {
				h.t.Fatal(err)
			}
			h.awaitRound(before)
			if !h.pr(7).Forced {
				h.t.Fatal("the round is not forced")
			}
			return 7
		}, func(h *harness, n int) { h.confirmCloseMidRound(n, "MERGED") }},
		{"review request", func(h *harness) int {
			h.reviewedPR(2, "b1")
			h.advance(time.Minute)
			reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
			h.holdRounds()
			before := len(h.rd.all())
			h.advance(time.Minute) // the request's debounce
			if err := h.e.Tick(h.ctx); err != nil {
				h.t.Fatal(err)
			}
			h.awaitRound(before)
			return 2
		}, func(h *harness, n int) { h.confirmCloseMidRound(n, "CLOSED") }},
		{"post-merge", func(h *harness) int {
			h.mergedBeforeReReview()
			h.holdRounds()
			before := len(h.rd.all())
			h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
			if err := h.e.Tick(h.ctx); err != nil {
				h.t.Fatal(err)
			}
			h.awaitRound(before)
			return 2
		}, func(h *harness, _ int) { h.pollsMidRound() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			n := tc.start(h)
			pr := h.wantState(n, store.PRReviewing)
			tc.close(h, n)

			if !h.e.roundActive(pr.ID) {
				t.Fatal("the round was stopped")
			}
			if evs := h.events("round.stopped"); len(evs) != 0 {
				t.Fatalf("round.stopped events: %+v", evs)
			}
			for _, ev := range h.events("pr.closed") {
				if strings.Contains(ev.Message, "stopping its round") {
					t.Fatalf("pr.closed says the round stops: %q", ev.Message)
				}
			}
			h.releaseRound()
			if n := len(h.rd.all()); n == 0 {
				t.Fatal("no round ran")
			}
			if cur := h.pr(n); !slices.Contains(closingStates, cur.State) {
				t.Fatalf("PR #%d state = %s after its round, want it closed", n, cur.State)
			}
		})
	}
}

// holdRounds makes the next rounds wait in the pipeline until their
// context ends or releaseRound lets them go on.
func (h *harness) holdRounds() {
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
}
