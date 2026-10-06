package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// The judge's own pass (a run of kind own_pass, prompted with the
// reviewers) is part of the reviewers' stage, never the judge's turn: the
// review's marker stays the judge's run, and a round whose judge was
// prompted only for its own pass has not prompted its judge.
func TestLatestJudgeNeverTakesTheOwnPass(t *testing.T) {
	h := newHarness(t)
	at := h.clock.Now()
	ownOnly := []store.Run{
		{ID: "r-claude", Round: 2, Role: store.RoleClaude, Kind: store.RunInitial, State: store.RunWorking, SubmittedAt: &at},
		{ID: "r-judge", Round: 2, Role: store.RoleJudge, Kind: store.RunInitial, State: store.RunPending, TargetSHA: "b1"},
		{ID: "r-own", Round: 2, Role: store.RoleJudge, Kind: store.RunOwnPass, State: store.RunWorking, TargetSHA: "b1", SubmittedAt: &at},
		{ID: "r-own-cont", Round: 2, Role: store.RoleJudge, Kind: store.RunOwnPass, State: store.RunFailed, TargetSHA: "b1",
			SubmittedAt: &at, Outcome: store.Ptr("usage_limit")},
	}
	j := h.e.latestJudge(ownOnly)
	if j.marker == nil || j.marker.ID != "r-judge" || j.prompted != nil {
		t.Fatalf("own pass only: latestJudge = marker %+v prompted %+v; want marker r-judge, nothing prompted", j.marker, j.prompted)
	}
	// Once the candidates prompt went out, it is the judge's turn.
	candidates := append([]store.Run(nil), ownOnly...)
	candidates[1].State, candidates[1].SubmittedAt = store.RunFailed, &at
	candidates[1].Outcome = store.Ptr("usage_limit")
	j = h.e.latestJudge(candidates)
	if j.marker == nil || j.marker.ID != "r-judge" || j.prompted == nil || j.prompted.ID != "r-judge" {
		t.Fatalf("candidates prompted: latestJudge = marker %+v prompted %+v", j.marker, j.prompted)
	}
}

// A daemon restart while the judge does its own pass starts the round
// again, as one while the reviewers run does: the own pass is part of the
// reviewers' stage, so the PR is not paused to continue a judge turn that
// never began (that continue would ask the judge to post without the
// reviewers' reports). Once the candidates prompt went out, the restart
// pauses the PR and the judge's turn continues, as before.
func TestDaemonRestartDuringOwnPassStartsTheRoundAgain(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"}, prSpec{n: 3, head: "c1"})
	h.advance(time.Minute)
	h.tick()
	h.advance(time.Minute)
	start := h.clock.Now()
	for _, n := range []int{2, 3} {
		if err := h.st.TransitionPR(h.ctx, h.pr(n).ID, nil, store.PRReviewing, func(u *store.PRUpdate) {
			u.Set("last_round_started_at", start)
		}); err != nil {
			t.Fatal(err)
		}
	}
	h.advance(time.Second)
	sent := h.clock.Now()
	run := func(n int, role, kind, state string, submitted bool) {
		t.Helper()
		r := store.Run{PRID: h.pr(n).ID, Round: 2, Role: role, Kind: kind, TargetSHA: "b2", Identity: "talkable-app",
			ReviewerLogin: "talkable[bot]", State: state, PromptText: "p"}
		if submitted {
			r.SubmittedAt = &sent
		}
		if _, err := h.st.CreateRun(h.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// #2: the reviewers and the judge's own pass work; the judge's run waits.
	run(2, "claude-review", store.RunRereview, store.RunWorking, true)
	run(2, "codex-judge", store.RunRereview, store.RunPending, false)
	run(2, "codex-judge", store.RunOwnPass, store.RunWorking, true)
	// #3: the own pass ended and the judge works on the candidates.
	run(3, "claude-review", store.RunInitial, store.RunVerified, true)
	run(3, "codex-judge", store.RunOwnPass, store.RunVerified, true)
	run(3, "codex-judge", store.RunInitial, store.RunWorking, true)

	h.e = New(h.d)
	h.e.recoverRows(h.ctx)
	if pr := h.pr(2); pr.State == store.PRPaused || deref(pr.LastError) != "daemon restarted while the reviewers ran; the round starts again" {
		t.Errorf("#2 (own pass) = %s, last_error %q; want back in line", pr.State, deref(pr.LastError))
	}
	if pr := h.pr(3); pr.State != store.PRPaused || !strings.Contains(deref(pr.LastError), "during the judge's turn") {
		t.Errorf("#3 (candidates) = %s, last_error %q; want paused to continue the judge's turn", pr.State, deref(pr.LastError))
	}
}

// A round gets the judge's own pass from [pipeline] judge_own_pass
// ("parallel" by default), which a [[watch]] overrides.
func TestRoundInputFollowsJudgeOwnPass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(c *config.Config)
		want  bool
		after bool
	}{
		{"default", func(*config.Config) {}, true, false},
		{"pipeline after", func(c *config.Config) { c.Pipeline.JudgeOwnPass = config.OwnPassAfter }, false, true},
		{"watch parallel over pipeline after", func(c *config.Config) {
			c.Pipeline.JudgeOwnPass = config.OwnPassAfter
			c.Watches[0].JudgeOwnPass = config.OwnPassParallel
		}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) { tc.set(h.cfg) })
			h.reviewedPR(2, "b1")
			ins := h.rd.all()
			if len(ins) != 1 || ins[0].OwnPass != tc.want {
				t.Fatalf("rounds = %d, OwnPass %v; want %v", len(ins), len(ins) > 0 && ins[0].OwnPass, tc.want)
			}
		})
	}
}

// The judge works during the reviewers' stage now, and dispatch's
// working-Codex count sees it there: a judge working on its own pass beside
// codex-review holds a new round at max_total_working_codex, as two
// working reviewers would, while the same round with its judge idle (one
// prompt after the reviewers) leaves room.
func TestWorkingCodexCountIncludesTheJudgeInTheReviewersStage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		judgeStatus herdr.Status
		held        bool
	}{
		{"judge on its own pass", herdr.StatusWorking, true},
		{"judge idle until the reviewers end", herdr.StatusIdle, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := 3
			h := newHarness(t, func(h *harness) { h.cfg.Daemon.MaxTotalWorkingCodex = limit })
			h.hd.mu.Lock()
			h.hd.agents = append(h.hd.agents,
				herdr.AgentInfo{PaneID: "px1", Name: "mg-other-9-codex-judge", Agent: "codex", AgentStatus: tc.judgeStatus},
				herdr.AgentInfo{PaneID: "px2", Name: "mg-other-9-codex-review", Agent: "codex", AgentStatus: herdr.StatusWorking})
			h.hd.mu.Unlock()
			h.open(prSpec{n: 1, head: "base1"})
			h.startup()
			h.tick()
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
			h.tick() // #2 queued
			h.advance(5 * time.Minute)
			h.tick()
			pr := h.pr(2)
			gate := fdGate(h, pr.ID)
			switch {
			case tc.held && (pr.State != store.PRQueued || !strings.HasPrefix(gate, "working Codex agents at the limit (2 working")):
				t.Fatalf("#2 = %s, gate %q; want held at the limit", pr.State, gate)
			case !tc.held && pr.State != store.PRReviewed:
				t.Fatalf("#2 = %s, gate %q; want reviewed", pr.State, gate)
			}
		})
	}
}
