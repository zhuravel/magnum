package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// queuedPR opens PR #n at head after a first sync and returns it queued.
func (h *harness) queuedPR(n int, head string) store.PR {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
	h.tick()
	return h.wantState(n, store.PRQueued)
}

// pushed records a new head of PR #n as the poller does (for a script that
// runs inside a round, where no tick can, so it returns its error).
func (h *harness) pushed(n int, head string) error {
	pr, err := h.lookupPR(n)
	if err != nil {
		return err
	}
	return h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("head_sha", head) })
}

func posted(target string, restarts int) pipeline.RoundResult {
	return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 1, ReviewID: 300, Event: "COMMENTED",
		ReviewCommit: target, TargetSHA: target, Restarts: restarts}
}

func TestRoundRestartChecksTheNewHeadOutInItsSlot(t *testing.T) {
	h := newHarness(t)
	pr := h.queuedPR(2, "b1")
	h.advance(5 * time.Minute)
	var during store.Slot
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.MaxRestarts != 2 || in.DispatchedHead != "b1" || in.Switch == nil {
			t.Errorf("restart input: max %d dispatched %q switch %v", in.MaxRestarts, in.DispatchedHead, in.Switch != nil)
			return pipeline.RoundResult{Outcome: pipeline.OutcomeError}, errors.New("no restart wiring")
		}
		if err := h.pushed(2, "b2"); err != nil {
			return failRound(t, err)
		}
		sw, err := in.Switch(context.Background(), "b2")
		if err != nil {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: err.Error()}, err
		}
		if during, err = h.lookupSlot("review1"); err != nil {
			return failRound(t, err)
		}
		if sw.TargetSHA != "b2" || sw.BaseSHA != "base0000" || sw.ForcePushed {
			t.Errorf("switched = %+v", sw)
		}
		return posted(sw.TargetSHA, 1), nil
	}
	h.tick()

	if during.State != store.SlotBusy || deref(during.CheckedOutSHA) != "b2" {
		t.Errorf("slot after the switch = %s at %q, want busy at b2", during.State, deref(during.CheckedOutSHA))
	}
	if want := "checkout:review1:" + itoa(pr.ID) + ":b2"; !slices.Contains(h.sl.all(), want) {
		t.Errorf("slot calls %v lack %s", h.sl.all(), want)
	}
	got := h.wantState(2, store.PRReviewed)
	if deref(got.ReviewedSHA) != "b2" {
		t.Errorf("reviewed_sha = %q, want b2 (the restart's head)", deref(got.ReviewedSHA))
	}
	if sl := h.slot("review1"); sl.State != store.SlotHeld {
		t.Errorf("slot after the round = %s, want held", sl.State)
	}
	if n := len(h.rd.appended()); n != 0 {
		t.Errorf("a review of the current head got a note: %v", h.rd.appended())
	}
}

func TestRoundRestartSwitchFailureKeepsTheSlotBusy(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, "b1")
	h.advance(5 * time.Minute)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		h.sl.mu.Lock()
		h.sl.holdErr = errors.New("guard: a human changed the slot")
		h.sl.mu.Unlock()
		_, err := in.Switch(context.Background(), "b2")
		if err == nil || !strings.Contains(err.Error(), "checkout in review1") {
			t.Errorf("switch error = %v", err)
		}
		if sl, lerr := h.lookupSlot("review1"); lerr != nil || sl.State != store.SlotBusy {
			t.Errorf("slot after a failed switch = %s (%v), want busy (the round still runs)", sl.State, lerr)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: "restart on b2: " + err.Error()}, err
	}
	h.tick()
	pr := h.pr(2)
	if pr.State != store.PRQueued || !strings.Contains(deref(pr.LastError), "restart on b2") {
		t.Errorf("PR after a failed restart = %s %q", pr.State, deref(pr.LastError))
	}
}

func TestPushDuringTheJudgeNotesTheReview(t *testing.T) {
	for _, tc := range []struct {
		name      string
		commits   int // GitHub's compare of b1...b3; -1 = it fails
		appendErr error
		wantEvent string
	}{
		{"noted", 2, nil, "round.review_noted"},
		{"cannot edit", 2, errors.New("update review: forbidden"), "round.review_note_failed"},
		// The poller's head is behind the reviewed one: nothing arrived.
		{"no new commits", 0, nil, ""},
		{"compare fails", -1, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.queuedPR(2, "b1")
			h.advance(5 * time.Minute)
			switch {
			case tc.commits > 0:
				h.gh.compare["b1...b3"] = github.CompareStats{Commits: tc.commits}
				h.gh.files = map[string][]github.FileDelta{"b1...b3": codePatch(40)} // above the re-review threshold
			case tc.commits == 0:
				h.gh.compare["b1...b3"] = github.CompareStats{}
				h.gh.files = map[string][]github.FileDelta{"b1...b3": {}}
				h.gh.statuses = map[string]string{"b1...b3": "behind"}
			}
			h.rd.appendErr = tc.appendErr
			h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
				if err := h.pushed(2, "b3"); err != nil { // after the reviewers: the judge reviews b1
					return failRound(t, err)
				}
				return posted(in.TargetSHA, 0), nil
			}
			h.tick()
			pr := h.wantState(2, store.PRRereviewPending)
			if deref(pr.ReviewedSHA) != "b1" {
				t.Errorf("reviewed_sha = %q", deref(pr.ReviewedSHA))
			}
			var want []string
			if tc.wantEvent != "" {
				want = []string{"300:_Reviewed b1; 2 commits arrived during the review, re-review follows._"}
			}
			if got := h.rd.appended(); !slices.Equal(got, want) {
				t.Errorf("notes = %q, want %q", got, want)
			}
			found := false
			evs, _ := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
			for _, ev := range evs {
				found = found || (tc.wantEvent != "" && ev.Kind == tc.wantEvent) || (tc.wantEvent == "" && strings.HasPrefix(ev.Kind, "round.review_note"))
			}
			if found != (tc.wantEvent != "") {
				t.Errorf("note event found = %v, want %q", found, tc.wantEvent)
			}
		})
	}
}

func TestBurstOfPushesLengthensTheQuietPeriod(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pushes []string
		quiet  time.Duration
	}{
		{"two pushes", []string{"b2", "b3"}, 5 * time.Minute},
		{"three pushes within 30m", []string{"b2", "b3", "b4"}, 15 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.queuedPR(2, "b1")
			var last time.Time
			for _, head := range tc.pushes {
				h.advance(time.Minute)
				last = h.clock.Now()
				h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: head})
				h.tick()
			}
			pr := h.wantState(2, store.PRQueued)
			if want := last.Add(tc.quiet); pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(want) {
				t.Fatalf("next_eligible_at = %v, want %v", pr.NextEligibleAt, want)
			}
			h.advance(5 * time.Minute)
			h.tick()
			rounds := len(h.rd.all())
			if tc.quiet > 5*time.Minute && rounds != 0 {
				t.Fatalf("a burst PR was reviewed after the push quiet period")
			}
			h.advance(tc.quiet - 5*time.Minute)
			h.tick()
			if n := len(h.rd.all()); n != 1 || h.rd.all()[0].TargetSHA != tc.pushes[len(tc.pushes)-1] {
				t.Fatalf("rounds = %+v", h.rd.all())
			}
		})
	}
}

func TestWatchTurnsTheBurstRuleOff(t *testing.T) {
	off := 0
	h := newHarness(t, func(h *harness) { h.cfg.Watches[0].BurstPushes = &off })
	h.queuedPR(2, "b1")
	var last time.Time
	for _, head := range []string{"b2", "b3", "b4"} {
		h.advance(time.Minute)
		last = h.clock.Now()
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: head})
		h.tick()
	}
	if pr := h.wantState(2, store.PRQueued); !pr.NextEligibleAt.Equal(last.Add(5 * time.Minute)) {
		t.Fatalf("next_eligible_at = %v, want the push quiet period", pr.NextEligibleAt)
	}
}

func TestRereviewStartsAgentsAtTheRereviewEffort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume map[agents.Role]string
		want   []string
	}{
		{"resumed judge", map[agents.Role]string{agents.RoleJudge: "uuid-judge", agents.RoleClaude: "uuid-claude"},
			[]string{"codex-judge:high:uuid-judge", "claude-review:medium:uuid-claude"}},
		// Without a conversation the judge re-reads the history (recovery)
		// at its full effort.
		{"fresh judge", nil, []string{"codex-judge:xhigh:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			pr := h.reviewedPR(2, "b1")
			sessions, err := h.st.SessionsByPR(h.ctx, pr.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range sessions {
				if err := h.st.TransitionSession(h.ctx, s.ID, []string{store.SessionLive}, store.SessionParked, nil); err != nil {
					t.Fatal(err)
				}
			}
			h.ag.mu.Lock()
			h.ag.resumeIDs, h.ag.efforts = tc.resume, nil
			h.ag.mu.Unlock()
			h.advance(30 * time.Minute)
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
			h.tick()
			h.advance(5 * time.Minute)
			h.tick()
			if ins := h.rd.all(); len(ins) != 2 {
				t.Fatalf("rounds = %d, want 2", len(ins))
			}
			h.ag.mu.Lock()
			got := slices.Clone(h.ag.efforts)
			h.ag.mu.Unlock()
			for _, w := range tc.want {
				if !slices.Contains(got, w) {
					t.Errorf("starts %v lack %s", got, w)
				}
			}
		})
	}
}

func TestLatestJudgeSkipsTheRunARestartReplaced(t *testing.T) {
	h := newHarness(t)
	at := h.clock.Now()
	runs := []store.Run{
		{ID: "r-old", Round: 3, Role: store.RoleJudge, Kind: store.RunRereview, State: store.RunAbandoned, TargetSHA: "b1"},
		{ID: "r-claude", Round: 3, Role: store.RoleClaude, Kind: store.RunRereview, State: store.RunVerified, SubmittedAt: &at},
		{ID: "r-new", Round: 3, Role: store.RoleJudge, Kind: store.RunRereview, State: store.RunFailed, TargetSHA: "b2", SubmittedAt: &at},
		{ID: "r-cont", Round: 3, Role: store.RoleJudge, Kind: store.RunContinue, State: store.RunSubmitted, TargetSHA: "b2", SubmittedAt: &at},
	}
	j := h.e.latestJudge(runs)
	if j.marker == nil || j.marker.ID != "r-new" || j.prompted == nil || j.prompted.ID != "r-cont" {
		t.Fatalf("latestJudge = marker %+v prompted %+v", j.marker, j.prompted)
	}
}
