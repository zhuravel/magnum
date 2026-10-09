package engine

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// servedRounds makes PR n's live session of role the conversation conv that
// served rounds review rounds: a run in each round on its session row (and
// an own pass in the same round when ownPass is set, as the judge's
// parallel pass leaves it), each sent and ended now.
func servedRounds(h *harness, n int, role, conv string, rounds int, ownPass bool) store.Session {
	h.t.Helper()
	pr := h.pr(n)
	s, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, role)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.UpdateSession(h.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("session_id", conv) }); err != nil {
		h.t.Fatal(err)
	}
	now := h.clock.Now()
	for round := 1; round <= rounds; round++ {
		kinds := []string{store.RunRereview}
		if round == 1 {
			kinds[0] = store.RunInitial
		}
		if ownPass {
			kinds = append(kinds, store.RunOwnPass)
		}
		for _, kind := range kinds {
			if _, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: round, Role: role, SessionID: &s.ID, Kind: kind,
				TargetSHA: "b1", Identity: pr.Identity, ReviewerLogin: "zhuravel", State: store.RunVerified,
				SubmittedAt: &now, EndedAt: &now}); err != nil {
				h.t.Fatal(err)
			}
		}
	}
	return s
}

// rereviewB2 pushes b2 to the reviewed PR #2 and runs its re-review (the
// push, then the quiet period), counting only the re-review's agent starts.
func rereviewB2(t *testing.T, h *harness) pipeline.RoundInput {
	t.Helper()
	h.ag.mu.Lock()
	h.ag.efforts = nil
	h.ag.mu.Unlock()
	h.advance(30 * time.Minute)
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

// roleSessions is PR n's sessions of role, oldest first.
func roleSessions(h *harness, n int, role string) []store.Session {
	h.t.Helper()
	all, err := h.st.SessionsByPR(h.ctx, h.pr(n).ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return slices.DeleteFunc(all, func(s store.Session) bool { return s.Role != role })
}

// The Codex judge of a PR was refused by Codex's cybersecurity classifier
// in round 15 of a session resumed across all fifteen rounds (29M tokens of
// history), which flags the PR for good. A judge whose session served its
// max_session_rounds (the built-in 6; an own pass belongs to its round)
// starts the next round in a fresh session, a new generation, the way a
// cold judge does: the round becomes a recovery whose prompt has the judge
// read its earlier reviews and threads, while the reviewers keep theirs.
// A live judge is quit first (parked, history kept); a parked one is not
// resumed.
func TestARoleSessionIsStartedFreshAfterItsMaxRounds(t *testing.T) {
	for _, parked := range []bool{false, true} {
		name := map[bool]string{false: "a live session is quit", true: "a parked session is not resumed"}[parked]
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.reviewedPR(2, "b1")
			old := servedRounds(h, 2, store.RoleJudge, "uuid-judge", 6, true)
			if parked {
				parkSessions(h, 2)
				h.ag.mu.Lock()
				h.ag.resumeIDs = map[agents.Role]string{agents.RoleJudge: "uuid-judge", agents.RoleClaude: "uuid-claude"}
				h.ag.mu.Unlock()
			}
			in := rereviewB2(t, h)
			if in.Kind != pipeline.KindRecovery || !in.ColdJudge || len(in.Roles) < 3 {
				t.Fatalf("kind %s fresh judge %v roles %v: want a recovery whose judge alone starts fresh", in.Kind, in.ColdJudge, roleNames(in.Roles))
			}
			starts := agentStarts(h)
			if !slices.Contains(starts, "codex-judge:high:") || slices.Contains(starts, "codex-judge:high:uuid-judge") {
				t.Fatalf("starts %v: want a fresh judge at its rereview effort", starts)
			}
			quit := slices.Contains(h.ag.all(), fmt.Sprintf("quit:%d:%s", h.pr(2).ID, store.RoleJudge))
			if quit == parked {
				t.Fatalf("calls %v: want the judge quit only when it was live", h.ag.all())
			}
			if parked && !slices.Contains(starts, "claude-review:medium:uuid-claude") {
				t.Fatalf("starts %v: want claude-review resumed", starts)
			}
			if !parked && slices.ContainsFunc(starts, func(s string) bool { return s != "codex-judge:high:" }) {
				t.Fatalf("starts %v: want only the judge started, the reviewers' live sessions kept", starts)
			}
			ss := roleSessions(h, 2, store.RoleJudge)
			newest := ss[len(ss)-1]
			if newest.ID == old.ID || newest.Generation <= old.Generation || newest.State != store.SessionLive {
				t.Fatalf("judge sessions %+v: want a new live generation after %d", ss, old.Generation)
			}
			if cur, err := h.st.SessionByID(h.ctx, old.ID); err != nil || cur.State != store.SessionParked {
				t.Fatalf("the old judge session: %+v %v, want it parked (history kept)", cur, err)
			}
			evs := approvalEvents(t, h, 2, "agent.session_rotated")
			if len(evs) != 1 || evs[0].Message != "codex-judge: fresh session after 6 rounds" {
				t.Fatalf("agent.session_rotated events: %+v", evs)
			}
			data := eventData(t, evs[0])
			if data["role"] != store.RoleJudge || data["rounds"] != float64(6) || data["max_session_rounds"] != float64(6) || data["was_live"] != !parked {
				t.Fatalf("event data %v", data)
			}
			if cold := approvalEvents(t, h, 2, "round.judge_fresh_cold"); len(cold) != 0 {
				t.Fatalf("round.judge_fresh_cold events %+v: the judge is fresh because of its rounds", cold)
			}
		})
	}
}

// max_session_rounds = 0 keeps a session however many rounds it served,
// and a session under its limit is kept: an own pass is part of its round,
// not a round of its own.
func TestARoleWithoutMaxSessionRoundsKeepsItsSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		max     int
		rounds  int
		ownPass bool
	}{
		{"max_session_rounds 0", 0, 15, true},
		{"under the limit", 6, 5, false},
		{"an own pass does not count twice", 7, 6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) {
				for i := range h.cfg.Roles {
					if h.cfg.Roles[i].Judge {
						h.cfg.Roles[i].MaxSessionRounds = tc.max
					}
				}
			})
			h.reviewedPR(2, "b1")
			s := servedRounds(h, 2, store.RoleJudge, "uuid-judge", tc.rounds, tc.ownPass)
			in := rereviewB2(t, h)
			if in.Kind != pipeline.KindRereview || in.ColdJudge {
				t.Fatalf("kind %s fresh judge %v: want a re-review in the judge's session", in.Kind, in.ColdJudge)
			}
			if starts := agentStarts(h); len(starts) != 0 {
				t.Fatalf("starts %v: want the live sessions kept", starts)
			}
			if quits := h.ag.count("quit:"); quits != 0 {
				t.Fatalf("calls %v: want nothing quit", h.ag.all())
			}
			if ss := roleSessions(h, 2, store.RoleJudge); len(ss) != 1 || ss[0].ID != s.ID {
				t.Fatalf("judge sessions %+v: want the one session", ss)
			}
			if evs := approvalEvents(t, h, 2, "agent.session_rotated"); len(evs) != 0 {
				t.Fatalf("agent.session_rotated events: %+v", evs)
			}
		})
	}
}

// A session at work, or blocked on a dialog, is never quit: it serves the
// round as before and rotates at a later round's start.
func TestABusySessionIsNeverRotated(t *testing.T) {
	for _, status := range []herdr.Status{herdr.StatusWorking, herdr.StatusBlocked} {
		t.Run(string(status), func(t *testing.T) {
			h := newHarness(t)
			h.reviewedPR(2, "b1")
			s := servedRounds(h, 2, store.RoleJudge, "uuid-judge", 6, false)
			if err := h.st.UpdateSession(h.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("agent_status", string(status)) }); err != nil {
				t.Fatal(err)
			}
			job := &roundJob{pr: h.pr(2), kind: pipeline.KindRereview}
			got, err := h.e.rotateSessions(h.ctx, job, h.cfg.RolesFor(nil))
			if err != nil || len(got) != 0 {
				t.Fatalf("rotateSessions = %+v, %v: want none", got, err)
			}
			if quits := h.ag.count("quit:"); quits != 0 {
				t.Fatalf("calls %v: want nothing quit", h.ag.all())
			}
			if evs := approvalEvents(t, h, 2, "agent.session_rotated"); len(evs) != 0 {
				t.Fatalf("agent.session_rotated events: %+v", evs)
			}
		})
	}
}

// The key belongs to any session role: a reviewer that served its rounds
// starts fresh alone, at its re-review effort with its re-review prompt
// (the round stays a re-review), and the judge keeps its session.
func TestAReviewerRotatesAloneAndTheRoundStaysAReReview(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		for i := range h.cfg.Roles {
			if h.cfg.Roles[i].Name == store.RoleClaude {
				h.cfg.Roles[i].MaxSessionRounds = 2
			}
		}
	})
	h.reviewedPR(2, "b1")
	servedRounds(h, 2, store.RoleJudge, "uuid-judge", 2, true)
	servedRounds(h, 2, store.RoleClaude, "uuid-claude", 2, false)
	in := rereviewB2(t, h)
	if in.Kind != pipeline.KindRereview || in.ColdJudge {
		t.Fatalf("kind %s fresh judge %v: want a re-review", in.Kind, in.ColdJudge)
	}
	if starts := agentStarts(h); !slices.Equal(starts, []string{"claude-review:medium:"}) {
		t.Fatalf("starts %v: want only claude-review, fresh at its rereview effort", starts)
	}
	if calls := h.ag.all(); h.ag.count("quit:") != 1 || !slices.Contains(calls, fmt.Sprintf("quit:%d:%s", h.pr(2).ID, store.RoleClaude)) {
		t.Fatalf("calls %v: want claude-review quit, alone", calls)
	}
	evs := approvalEvents(t, h, 2, "agent.session_rotated")
	if len(evs) != 1 || evs[0].Message != "claude-review: fresh session after 2 rounds" {
		t.Fatalf("agent.session_rotated events: %+v", evs)
	}
}

// A continue finishes its paused turn in the session it was paused in, even
// when that turn was the session's last round: rotation happens only when a
// round starts.
func TestAContinueNeverRotatesTheSession(t *testing.T) {
	h := newHarness(t)
	resetAt := time.Time{}
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.Kind == pipeline.KindContinue || in.Kind == pipeline.KindInitial {
			return h.rd.posted(h.ctx, in, 1)
		}
		// The sixth round: the judge is prompted on its session, then
		// Codex's usage limit pauses the round.
		s, err := h.st.LiveSessionByPRRole(h.ctx, in.PR.ID, store.RoleJudge)
		if err != nil {
			return failRound(t, err)
		}
		run, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: 6, Role: store.RoleJudge, SessionID: &s.ID, Kind: in.Kind,
			TargetSHA: in.TargetSHA, Identity: in.PR.Identity, ReviewerLogin: "talkable[bot]", State: store.RunPending, PromptText: "judge"})
		if err != nil {
			return failRound(t, err)
		}
		now := h.clock.Now()
		if err := h.st.TransitionRun(h.ctx, run.ID, nil, store.RunFailed, func(u *store.RunUpdate) {
			u.Set("submitted_at", now)
			u.Set("outcome", pipeline.OutcomeUsageLimit)
			u.Set("error", "judge pane: usage limit")
		}); err != nil {
			return failRound(t, err)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeUsageLimit, Round: 6, JudgeRunID: run.ID,
			Pause: &pipeline.Pause{Kind: string(agents.HealthUsageLimit), Tool: agents.KindCodex, Until: resetAt}}, nil
	}
	h.reviewedPR(2, "b1")
	s := servedRounds(h, 2, store.RoleJudge, "uuid-judge", 5, true)
	resetAt = h.clock.Now().Add(2 * time.Hour)
	in := rereviewB2(t, h)
	if in.Kind != pipeline.KindRereview {
		t.Fatalf("kind %s: want the sixth round in the judge's session", in.Kind)
	}
	h.wantState(2, store.PRPaused)

	h.advance(2 * time.Hour)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 3 || ins[2].Kind != pipeline.KindContinue || ins[2].Round != 6 {
		t.Fatalf("rounds %d (last %+v): want the paused round continued", len(ins), ins[len(ins)-1].Kind)
	}
	if starts := agentStarts(h); len(starts) != 0 {
		t.Fatalf("starts %v: want the judge's session kept", starts)
	}
	if quits := h.ag.count("quit:"); quits != 0 {
		t.Fatalf("calls %v: want nothing quit", h.ag.all())
	}
	if ss := roleSessions(h, 2, store.RoleJudge); len(ss) != 1 || ss[0].ID != s.ID {
		t.Fatalf("judge sessions %+v: want the one session", ss)
	}
	if evs := approvalEvents(t, h, 2, "agent.session_rotated"); len(evs) != 0 {
		t.Fatalf("agent.session_rotated events: %+v", evs)
	}
}
