package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// judgeTurnEnded records a judge turn of PR n that ended at at, as the
// pipeline's run rows leave it (submitted 10 minutes before).
func judgeTurnEnded(h *harness, n int, at time.Time) {
	h.t.Helper()
	sent := at.Add(-10 * time.Minute)
	judgeRunEnded(h, n, at, &sent)
}

// judgeRunEnded records a judge run of PR n that ended at at, submitted at
// sent (nil: never sent, a prompt refused before it reached the judge).
func judgeRunEnded(h *harness, n int, at time.Time, sent *time.Time) {
	h.t.Helper()
	pr := h.pr(n)
	state := store.RunVerified
	if sent == nil {
		state = store.RunAbandoned
	}
	if _, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: 1, Role: store.RoleJudge, Kind: store.RunInitial,
		TargetSHA: "b1", Identity: pr.Identity, ReviewerLogin: "zhuravel", State: state, SubmittedAt: sent, EndedAt: &at}); err != nil {
		h.t.Fatal(err)
	}
}

// coldRereview reviews PR #2 at b1, gives its judge a turn that ended idle
// before the re-review of b2 starts (the push, then 5 minutes of quiet),
// parks the sessions when parked is set (the agents then resume the ids in
// resume) and runs the re-review.
func coldRereview(t *testing.T, h *harness, idle time.Duration, parked bool) pipeline.RoundInput {
	t.Helper()
	h.reviewedPR(2, "b1")
	if parked {
		parkSessions(h, 2)
		h.ag.mu.Lock()
		h.ag.resumeIDs = map[agents.Role]string{agents.RoleJudge: "uuid-judge", agents.RoleClaude: "uuid-claude"}
		h.ag.mu.Unlock()
	}
	h.ag.mu.Lock()
	h.ag.efforts = nil
	h.ag.mu.Unlock()
	h.advance(30 * time.Minute)
	judgeTurnEnded(h, 2, h.clock.Now().Add(5*time.Minute-idle))
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the re-review", len(ins))
	}
	return ins[1]
}

func agentStarts(h *harness) []string {
	h.ag.mu.Lock()
	defer h.ag.mu.Unlock()
	return slices.Clone(h.ag.efforts)
}

// The prompt cache lasts about 1.5 hours: 25 of 39 resumed judge turns
// started cold and re-read their whole conversation uncached. A judge whose
// last turn ended longer than judge_fresh_after (90m) ago starts in a fresh
// session through the recovery (which reads the earlier reviews and threads
// from GitHub); the other roles keep their conversations.
func TestAColdJudgeStartsFreshThroughTheRecovery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		freshAfter time.Duration
		idle       time.Duration
		cold       bool
	}{
		{"idle just over the threshold", 90 * time.Minute, 91 * time.Minute, true},
		{"idle just under the threshold", 90 * time.Minute, 89 * time.Minute, false},
		{"judge_fresh_after 0 always resumes", 0, 5 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) { h.cfg.Pipeline.JudgeFreshAfter.Duration = tc.freshAfter })
			in := coldRereview(t, h, tc.idle, true)
			starts := agentStarts(h)
			if !slices.Contains(starts, "claude-review:medium:uuid-claude") {
				t.Errorf("starts %v: want claude-review resumed", starts)
			}
			cold := approvalEvents(t, h, 2, "round.judge_fresh_cold")
			if !tc.cold {
				if in.Kind != pipeline.KindRereview || !slices.Contains(starts, "codex-judge:high:uuid-judge") || len(cold) != 0 {
					t.Fatalf("kind %s starts %v cold events %+v: want the judge resumed", in.Kind, starts, cold)
				}
				return
			}
			if in.Kind != pipeline.KindRecovery || !in.ColdJudge || !slices.Contains(starts, "codex-judge:high:") || slices.Contains(starts, "codex-judge:high:uuid-judge") {
				t.Fatalf("kind %s cold %v starts %v: want a recovery with a fresh judge at its rereview effort", in.Kind, in.ColdJudge, starts)
			}
			if len(in.Roles) < 3 {
				t.Fatalf("roles %v: want the reviewers too", roleNames(in.Roles))
			}
			want := "the judge starts in a fresh session: its last turn ended 1h31m ago (judge_fresh_after 1h30m), so its prompt cache is cold"
			if len(cold) != 1 || cold[0].Message != want {
				t.Fatalf("round.judge_fresh_cold events: %+v, want %q", cold, want)
			}
			data := eventData(t, cold[0])
			if data["idle_seconds"] != float64(91*60) || data["fresh_after"] != "1h30m0s" {
				t.Fatalf("event data %v", data)
			}
		})
	}
}

// A judge still live in its pane (sessions park only after
// park_idle_after, 2h) is quit first, which parks its conversation; the
// reviewers' live sessions are left alone.
func TestAColdLiveJudgeIsQuitAndStartsFresh(t *testing.T) {
	h := newHarness(t)
	in := coldRereview(t, h, 100*time.Minute, false)
	pr := h.pr(2)
	if quits := h.ag.count(fmt.Sprintf("quit:%d:", pr.ID)); quits != 1 || !slices.Contains(h.ag.all(), fmt.Sprintf("quit:%d:%s", pr.ID, store.RoleJudge)) {
		t.Fatalf("calls %v: want the judge quit, alone", h.ag.all())
	}
	if in.Kind != pipeline.KindRecovery {
		t.Fatalf("kind %s, want a recovery", in.Kind)
	}
	starts := agentStarts(h)
	if !slices.Equal(starts, []string{"codex-judge:high:"}) {
		t.Fatalf("starts %v: want only a fresh judge at its rereview effort (the reviewers' sessions stay live)", starts)
	}
	parked := 0
	sessions, err := h.st.SessionsByPR(h.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.Role == store.RoleJudge && s.State == store.SessionParked {
			parked++
		}
	}
	if parked != 1 {
		t.Fatalf("parked judge sessions = %d, want the cold one kept", parked)
	}
	if evs := approvalEvents(t, h, 2, "round.judge_fresh_cold"); len(evs) != 1 {
		t.Fatalf("round.judge_fresh_cold events: %+v", evs)
	}
}

// herdr may still list a quit judge for a moment, and a fresh start under
// the same name adopted the quitting agent: 2 of the 3 cold judges that
// were live were lost (one after 0.5 s, its round failing 9 minutes later).
// The judge starts fresh only once herdr no longer lists the name.
func TestAColdJudgeStartsFreshOnlyOnceHerdrDropsTheQuitAgent(t *testing.T) {
	h := newHarness(t)
	quit := ""
	h.ag.onQuit = func(s store.Session) {
		quit = deref(s.AgentName)
		h.hd.linger(quit, 1) // listed for one more snapshot
	}
	listedAtStart := false
	h.ag.onStart = func(role config.Role, resume string) {
		if !role.Judge {
			return
		}
		snap, _ := h.hd.Snapshot(h.ctx)
		_, listedAtStart = snap.AgentByName(quit)
	}
	h.ag.mu.Lock()
	h.ag.resumeIDs = map[agents.Role]string{agents.RoleJudge: "uuid-judge"} // the conversation Quit parked
	h.ag.mu.Unlock()
	in := coldRereview(t, h, 100*time.Minute, false)
	starts := agentStarts(h)
	cold := approvalEvents(t, h, 2, "round.judge_fresh_cold")
	if in.Kind != pipeline.KindRecovery || !slices.Equal(starts, []string{"codex-judge:high:"}) || len(cold) != 1 {
		t.Fatalf("kind %s starts %v cold events %d: want a fresh judge", in.Kind, starts, len(cold))
	}
	if listedAtStart {
		t.Fatalf("the fresh judge started while herdr still listed %s, which it would adopt", quit)
	}
}

// When herdr keeps listing the quit judge, resuming it adopted the quitting
// agent under the role's name and lost it (agents.StartAgent adopts an
// agent of the role's name before it resumes): the cold judge fails the
// round's setup as busy instead, no agent starts and the PR waits, and the
// attempt after busyRetry finds the judge parked and starts it fresh.
func TestAColdJudgeHerdrKeepsListingIsNeitherResumedNorAdopted(t *testing.T) {
	h := newHarness(t)
	h.ag.onQuit = func(s store.Session) { h.hd.linger(deref(s.AgentName), -1) }
	h.ag.mu.Lock()
	h.ag.resumeIDs = map[agents.Role]string{agents.RoleJudge: "uuid-judge"} // the conversation Quit parked
	h.ag.mu.Unlock()
	h.reviewedPR(2, "b1")
	h.ag.mu.Lock()
	h.ag.efforts = nil
	h.ag.mu.Unlock()
	h.advance(30 * time.Minute)
	judgeTurnEnded(h, 2, h.clock.Now().Add(-95*time.Minute))
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d: the re-review ran while herdr listed the quit judge", n)
	}
	if starts := agentStarts(h); len(starts) != 0 {
		t.Fatalf("starts %v: want none, the quit judge neither resumed nor adopted", starts)
	}
	h.wantState(2, store.PRRereviewPending)

	h.advance(busyRetry + time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].Kind != pipeline.KindRecovery {
		t.Fatalf("rounds %d: want the re-review on the next attempt, as a recovery", len(ins))
	}
	if starts := agentStarts(h); !slices.Equal(starts, []string{"codex-judge:high:"}) {
		t.Fatalf("starts %v: want a fresh judge", starts)
	}
}

// The round's RestartJudge (the pipeline calls it when the judge's session
// is gone at its own pass's prompt) marks the gone session lost and starts
// the judge again as the round started it: a cold judge fresh at its
// rereview effort, a lost session's recovery fresh at its full effort.
func TestTheRoundStartsAGoneJudgeAgainAsItStartedIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		idle    time.Duration
		resumed bool // the judge's conversation is resumable (else lost)
		want    string
	}{
		{"cold judge", 100 * time.Minute, true, "codex-judge:high:"},
		{"lost session", 10 * time.Minute, false, "codex-judge:xhigh:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var restartErr error
			var before, after []string
			lost := 0
			h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
				if in.Replies == 0 && in.Kind != pipeline.KindInitial && in.RestartJudge != nil {
					before = agentStarts(h)
					restartErr = in.RestartJudge(h.ctx)
					after = agentStarts(h)
					sessions, _ := h.st.SessionsByPR(h.ctx, in.PR.ID)
					for _, s := range sessions {
						if s.Role == store.RoleJudge && s.State == store.SessionLost {
							lost++
						}
					}
				}
				return h.rd.posted(h.ctx, in, 2)
			}
			if tc.resumed {
				coldRereview(t, h, tc.idle, true)
			} else {
				// coldRereview without the judge's conversation: its
				// re-review becomes a recovery.
				h.reviewedPR(2, "b1")
				parkSessions(h, 2)
				h.ag.mu.Lock()
				h.ag.resumeIDs, h.ag.efforts = map[agents.Role]string{agents.RoleClaude: "uuid-claude"}, nil
				h.ag.mu.Unlock()
				h.advance(30 * time.Minute)
				h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
				h.tick()
				h.advance(5 * time.Minute)
				h.tick()
			}
			if restartErr != nil {
				t.Fatalf("RestartJudge: %v", restartErr)
			}
			if len(after) != len(before)+1 || after[len(after)-1] != tc.want {
				t.Fatalf("starts %v → %v: want one more, %s", before, after, tc.want)
			}
			if lost != 1 {
				t.Fatalf("lost judge sessions = %d, want the gone one", lost)
			}
		})
	}
}

// A delta check of a cold judge runs with a fresh session, the check's
// existing variant: the judge alone at its rereview effort, which reads its
// previous review and threads first.
func TestAColdJudgeChecksADeltaInAFreshSession(t *testing.T) {
	h, _, _ := deltaCheckHarness(t)
	verdictRounds(h, "APPROVED")
	keptForCheck(t, h, codePatch(1))
	judgeTurnEnded(h, 2, h.clock.Now().Add(-2*time.Hour))
	pollPR(h, 5*time.Minute, 2, "b2")
	freshCheckWant(t, h, "the judge's last turn ended 2h5m ago (judge_fresh_after 1h30m)")
	if evs := approvalEvents(t, h, 2, "round.judge_fresh_cold"); len(evs) != 1 {
		t.Fatalf("round.judge_fresh_cold events: %+v", evs)
	}
}

// A re-review of the same head follows the same rule.
func TestAColdJudgeReDecidesTheSameHeadInAFreshSession(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	judgeTurnEnded(h, 2, h.clock.Now())
	h.advance(3 * time.Hour)
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the re-review", len(ins))
	}
	in := ins[1]
	if in.Kind != pipeline.KindRecovery || !in.SameHead || !slices.Equal(roleNames(in.Roles), []string{"codex-judge"}) {
		t.Fatalf("input: kind %s same head %v roles %v", in.Kind, in.SameHead, roleNames(in.Roles))
	}
	if starts := agentStarts(h); starts[len(starts)-1] != "codex-judge:high:" {
		t.Fatalf("starts %v: want a fresh judge at its rereview effort", starts)
	}
	fresh := approvalEvents(t, h, 2, "round.same_head_fresh")
	if len(fresh) != 1 || fresh[0].Message != "same head with a fresh judge session: the judge's last turn ended 3h ago (judge_fresh_after 1h30m)" {
		t.Fatalf("round.same_head_fresh events: %+v", fresh)
	}
}

// Only a turn the judge got counts: a run whose prompt never reached it (a
// fresh judge lost before its first prompt) still gets ended_at, and on
// talkable#11792 such a run, a minute old, made the next round resume a
// conversation idle for 5h22m.
func TestAColdJudgeCountsOnlyTurnsTheJudgeGot(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	turn := h.clock.Now()
	judgeTurnEnded(h, 2, turn)
	h.advance(5 * time.Hour)
	judgeRunEnded(h, 2, h.clock.Now().Add(-time.Minute), nil)
	judge := judgeAlone(h.cfg.RolesFor(nil))[0]
	if last := h.e.judgeLastTurn(h.ctx, h.pr(2).ID, judge); !last.Equal(turn) {
		t.Fatalf("the judge's last turn = %s, want the one it got at %s", last, turn)
	}
}

// A continue finishes a paused turn in its own conversation, however long
// the pause was.
func TestAContinueKeepsAColdJudgesConversation(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	judgeTurnEnded(h, 2, h.clock.Now())
	h.advance(3 * time.Hour)
	job := &roundJob{pr: h.pr(2), kind: kindContinue}
	rs := &roundSetup{toRun: h.cfg.RolesFor(nil)}
	if cold, why, err := h.e.coldJudge(h.ctx, job, rs); cold || err != nil {
		t.Fatalf("a continue starts the judge fresh: %s %v", why, err)
	}
}

func eventData(t *testing.T, ev store.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("event %s data %s: %v", ev.Kind, ev.Data, err)
	}
	return m
}
