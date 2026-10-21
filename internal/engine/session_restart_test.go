package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// TestRestartSessionsParksTheIdleOnesNowAndTheBusyOnesAfterTheirTurn: `magnum
// restart-sessions --tool codex` parks every live Codex session: an idle one
// now, one at work once its turn ended, a pinned PR's never, and no Claude
// session; the answer counts them.
func TestRestartSessionsParksTheIdleOnesNowAndTheBusyOnesAfterTheirTurn(t *testing.T) {
	h, _ := newLoginHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	judge := h.liveSessionOf(pr.ID, store.RoleJudge)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"}, prSpec{n: 4, head: "d1"})
	h.tick()
	busy := h.liveSession(h.pr(3).ID, store.RoleJudge, agents.KindCodex, h.clock.Now(), herdr.StatusWorking)
	pinnedPR := h.pr(4)
	if err := h.st.UpdatePR(h.ctx, pinnedPR.ID, func(u *store.PRUpdate) { u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	pinned := h.liveSession(pinnedPR.ID, store.RoleJudge, agents.KindCodex, h.clock.Now(), herdr.StatusIdle)

	id, err := h.st.EnqueueRequest(h.ctx, ReqRestartSessions, RestartSessionsPayload{Tool: agents.KindCodex})
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	req, err := h.st.RequestByID(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := "restarting 2 live codex sessions: 1 park now, 1 after its turn; each resumes in a new process at its next prompt; " +
		"1 session of pinned PRs left alone"
	if req.State != store.RequestDone || deref(req.Result) != want {
		t.Fatalf("request %s: %q, want %q", req.State, deref(req.Result), want)
	}
	if st := h.sessionState(judge.ID); st != store.SessionParked {
		t.Fatalf("#2's idle judge is %s, want parked", st)
	}
	if got := strings.Join(h.liveRoles(pr.ID), ","); got != "claude-review,claude-simplify" {
		t.Fatalf("#2's live sessions = %s, want its Claude ones", got)
	}
	if st := h.sessionState(busy.ID); st != store.SessionLive {
		t.Fatalf("#3's working judge is %s, want live until its turn ends", st)
	}
	if st := h.sessionState(pinned.ID); st != store.SessionLive {
		t.Fatalf("the pinned PR's judge is %s, want live", st)
	}

	if err := h.st.UpdateSession(h.ctx, busy.ID, func(u *store.SessionUpdate) { u.Set("agent_status", string(herdr.StatusIdle)) }); err != nil {
		t.Fatal(err)
	}
	h.advance(30 * time.Second)
	h.tick()
	if st := h.sessionState(busy.ID); st != store.SessionParked {
		t.Fatalf("after its turn #3's judge is %s, want parked", st)
	}
	if st := h.sessionState(pinned.ID); st != store.SessionLive {
		t.Fatalf("the pinned PR's judge is %s", st)
	}
}

// TestRestartSessionsNarrowsToAPRAndAKind: --pr restarts only that PR's
// sessions of the kind, "all" every kind's, a kind with no live session
// says so, and an unknown kind is refused.
func TestRestartSessionsNarrowsToAPRAndAKind(t *testing.T) {
	h, _ := newLoginHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick()
	other := h.liveSession(h.pr(3).ID, "claude-review", agents.KindClaude, h.clock.Now(), herdr.StatusIdle)

	res, err := h.e.requestRestartSessions(h.ctx, RestartSessionsPayload{PRTarget: PRTarget{Ref: "talkable#2"}, Tool: agents.KindClaude})
	want := "restarting 2 live claude sessions of talkable/talkable#2: 2 park now, 0 after their turn; each resumes in a new process at its next prompt"
	if err != nil || res != want {
		t.Fatalf("restart claude of #2 = %q, %v; want %q", res, err, want)
	}
	h.settle()
	if got := strings.Join(h.liveRoles(pr.ID), ","); got != store.RoleJudge {
		t.Fatalf("#2's live sessions = %s, want its judge alone", got)
	}
	if st := h.sessionState(other.ID); st != store.SessionLive {
		t.Fatalf("#3's claude session is %s, want live", st)
	}

	res, err = h.e.requestRestartSessions(h.ctx, RestartSessionsPayload{PRTarget: PRTarget{Ref: "talkable#2"}, Tool: agents.KindClaude})
	if err != nil || res != "no live claude session of talkable/talkable#2 to restart" {
		t.Fatalf("again = %q, %v", res, err)
	}
	res, err = h.e.requestRestartSessions(h.ctx, RestartSessionsPayload{Tool: "all"})
	if err != nil || !strings.HasPrefix(res, "restarting 2 live agent sessions: 2 park now, 0 after their turn") {
		t.Fatalf("restart all = %q, %v", res, err)
	}
	h.settle()
	if got := h.liveRoles(pr.ID); len(got) != 0 || h.sessionState(other.ID) != store.SessionParked {
		t.Fatalf("after all: #2 live %v, #3's %s", got, h.sessionState(other.ID))
	}
	for _, p := range []RestartSessionsPayload{{Tool: "gemini"}, {}} {
		if res, err := h.e.requestRestartSessions(h.ctx, p); err == nil {
			t.Fatalf("restart %+v = %q, want a refusal", p, res)
		}
	}
}
