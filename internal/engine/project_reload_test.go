package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// reloadingClaude makes claude-review's sessions reload the checkout's
// project config they loaded (a Claude session started with it, as
// agents.Manager.ReloadsProject reports) and records, for each quit, the
// commit the slot had checked out then.
func reloadingClaude(h *harness) *[]string {
	var at []string
	h.ag.mu.Lock()
	h.ag.reloads = map[string]bool{store.RoleClaude: true}
	h.ag.resumeIDs = map[agents.Role]string{agents.RoleClaude: "uuid-claude"}
	h.ag.onQuit = func(s store.Session) {
		at = append(at, s.Role+"@"+deref(h.slot("review1").CheckedOutSHA))
	}
	h.ag.mu.Unlock()
	return &at
}

func liveRole(h *harness, n int, role string) bool {
	h.t.Helper()
	s, err := h.st.LiveSessionByPRRole(h.ctx, h.pr(n).ID, role)
	return err == nil && s.State == store.SessionLive
}

// Claude Code reloads .claude/settings.json (hooks included) and its skills
// while it runs, and loads one a later commit adds: a claude-review session
// that started with the project config loaded would take the new head's
// from disk once the checkout moves under it. A re-review on a new head
// quits it first (its conversation parked), while the slot still has the
// reviewed head, and resumes it after the checkout, when its start decides
// with the new head; a session that does not reload (the judge) stays.
func TestALiveClaudeSessionIsParkedBeforeTheCheckoutMoves(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	if !liveRole(h, 2, store.RoleClaude) || !liveRole(h, 2, store.RoleJudge) {
		t.Fatal("the first review left no live sessions")
	}
	at := reloadingClaude(h)
	h.ag.mu.Lock()
	h.ag.efforts = nil
	h.ag.mu.Unlock()
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the re-review", n)
	}
	pr := h.pr(2)
	if !slices.Equal(*at, []string{store.RoleClaude + "@b1"}) {
		t.Fatalf("quits (role@checked-out head) = %v, want claude-review before the checkout of b2", *at)
	}
	if slices.Contains(h.ag.all(), fmt.Sprintf("quit:%d:%s", pr.ID, store.RoleJudge)) {
		t.Fatalf("calls %v: the judge does not reload its project config", h.ag.all())
	}
	if starts := agentStarts(h); !slices.ContainsFunc(starts, func(s string) bool { return s == "claude-review:medium:uuid-claude" }) {
		t.Fatalf("starts %v: want claude-review resumed on the new head", starts)
	}
	evs := approvalEvents(t, h, 2, "round.project_reload_parked")
	if len(evs) != 1 || evs[0].Message != "claude-review parked before the checkout moves: its agent reloads the checkout's "+
		"project config while it runs, so it resumes on the new head with what that head allows" {
		t.Fatalf("events = %+v", evs)
	}
}

// A session that reloads the project config and is working (or blocked)
// cannot be quit safely, and the checkout must not move under it: the
// round waits, uncharged, and nothing is checked out.
func TestAWorkingClaudeSessionHoldsTheCheckout(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	reloadingClaude(h)
	s, err := h.st.LiveSessionByPRRole(h.ctx, h.pr(2).ID, store.RoleClaude)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpdateSession(h.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("agent_status", "working") }); err != nil {
		t.Fatal(err)
	}
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want none on b2 while claude-review works", n)
	}
	if slices.ContainsFunc(h.sl.all(), func(c string) bool { return c == fmt.Sprintf("checkout:review1:%d:b2", h.pr(2).ID) }) {
		t.Fatalf("slot calls %v: the checkout moved under a working session", h.sl.all())
	}
	if !liveRole(h, 2, store.RoleClaude) {
		t.Fatal("a working session was quit")
	}
}

// A push while the reviewers run restarts them on the new head in place
// (RoundInput.Switch): a session that reloads the project config it loaded
// is quit before the switch moves the checkout and resumed after it, so the
// restart prompts it on the new head with what that head allows.
func TestRoundRestartParksAReloadingSessionAcrossTheSwitch(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, "b1")
	h.advance(5 * time.Minute)
	var at *[]string
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if !liveRole(h, 2, store.RoleClaude) {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeError}, errors.New("claude-review is not live")
		}
		at = reloadingClaude(h)
		h.ag.mu.Lock()
		h.ag.efforts = nil
		h.ag.mu.Unlock()
		h.pushed(2, "b2")
		sw, err := in.Switch(context.Background(), "b2")
		if err != nil {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: err.Error()}, err
		}
		if !liveRole(h, 2, store.RoleClaude) {
			t.Error("claude-review is not live again after the switch")
		}
		return posted(sw.TargetSHA, 1), nil
	}
	h.tick()
	if at == nil || !slices.Equal(*at, []string{store.RoleClaude + "@b1"}) {
		t.Fatalf("quits (role@checked-out head) = %v, want claude-review before the switch to b2", at)
	}
	if starts := agentStarts(h); !slices.ContainsFunc(starts, func(s string) bool { return s == "claude-review:high:uuid-claude" }) {
		t.Fatalf("starts %v: want claude-review resumed after the switch", starts)
	}
	if got := h.wantState(2, store.PRReviewed); deref(got.ReviewedSHA) != "b2" {
		t.Fatalf("reviewed_sha = %q, want b2", deref(got.ReviewedSHA))
	}
}
