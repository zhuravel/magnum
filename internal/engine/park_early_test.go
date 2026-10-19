package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// withSessionIDs records a session id for every live agent session of the
// PR, as herdr's session hook does (a shell row has none).
func (h *harness) withSessionIDs(prID int64) {
	h.t.Helper()
	ss, err := h.st.SessionsByPR(h.ctx, prID)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, s := range ss {
		if r, ok := h.cfg.RoleByNameOrAlias(nil, s.Role); !isLiveSession(s) || (ok && r.IsShell()) {
			continue
		}
		if err := h.st.UpdateSession(h.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("session_id", "sid-"+s.Role) }); err != nil {
			h.t.Fatal(err)
		}
	}
}

func isLiveSession(s store.Session) bool {
	return s.State == store.SessionLive || s.State == store.SessionStarting
}

// liveRoles are the roles of the PR's live sessions, sorted.
func (h *harness) liveRoles(prID int64) []string {
	h.t.Helper()
	ss, err := h.st.SessionsByPR(h.ctx, prID)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, s := range ss {
		if isLiveSession(s) {
			out = append(out, s.Role)
		}
	}
	slices.Sort(out)
	return out
}

// parkedEvents are PR #2's pr.parked events, oldest first.
func (h *harness) parkedEvents() []store.Event {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return slices.DeleteFunc(evs, func(ev store.Event) bool { return ev.Kind != "pr.parked" })
}

// earlyParks counts the PR's early parks (Agents.ParkSessions).
func (h *harness) earlyParks(prID int64) int {
	return h.ag.count(fmt.Sprintf("park_sessions:%d:", prID))
}

const (
	reviewersLive = "claude-review,claude-simplify,codex-judge" // the fake starts no codex-review shell
	judgeLive     = "codex-judge"
)

// After a round, the reviewers have nothing to keep warm for: their
// resumable sessions park park_idle_after (15m) after the round ended,
// alone, while the live Codex judge stays until judge_fresh_after (90m),
// within which the next round reuses it; then the judge, the codex-review
// shell and the workspace park with the PR's park. The early park records
// pr.parked with its roles and "partial", and closes the panes it retired.
// With judge_fresh_after "0" (always resume) the judge parks at
// park_idle_after, with the others in one park.
func TestIdleReviewersParkAtParkIdleAfterAndTheJudgeAtJudgeFreshAfter(t *testing.T) {
	t.Run("judge_fresh_after 90m", func(t *testing.T) {
		h := newHarness(t)
		pr := h.reviewedPR(2, "b1")
		h.withSessionIDs(pr.ID)
		if got := strings.Join(h.liveRoles(pr.ID), ","); got != reviewersLive {
			t.Fatalf("live roles after the round = %s, want %s", got, reviewersLive)
		}
		parks := h.parks(pr.ID)

		h.advance(14 * time.Minute)
		h.tick()
		if h.earlyParks(pr.ID) != 0 || h.parks(pr.ID) != parks {
			t.Fatalf("parked 14 minutes after the round: %v", h.ag.all())
		}
		h.advance(2 * time.Minute)
		h.tick()
		if got := h.ag.count(fmt.Sprintf("park_sessions:%d:claude-review,claude-simplify", pr.ID)); got != 1 || h.parks(pr.ID) != parks {
			t.Fatalf("early parks of the reviewers = %d, PR parks %d (want 1, %d): %v", got, h.parks(pr.ID), parks, h.ag.all())
		}
		if got := strings.Join(h.liveRoles(pr.ID), ","); got != judgeLive {
			t.Fatalf("live roles after the early park = %s, want %s", got, judgeLive)
		}
		if h.ag.count(fmt.Sprintf("close_panes:%d", pr.ID)) == 0 {
			t.Fatalf("the early park closed no retired pane: %v", h.ag.all())
		}
		evs := h.parkedEvents()
		if len(evs) != 1 {
			t.Fatalf("pr.parked events = %+v, want one", evs)
		}
		var data struct {
			Roles   []string `json:"roles"`
			Partial bool     `json:"partial"`
		}
		if err := json.Unmarshal(evs[0].Data, &data); err != nil || !data.Partial || !slices.Equal(data.Roles, []string{"claude-review", "claude-simplify"}) {
			t.Fatalf("early park data = %s (%v), want its roles and partial", evs[0].Data, err)
		}
		for _, want := range []string{"parked claude-review, claude-simplify", "park_idle_after 15m", "codex-judge stays until judge_fresh_after (1h30m)"} {
			if !strings.Contains(evs[0].Message, want) {
				t.Errorf("early park message %q lacks %q", evs[0].Message, want)
			}
		}

		h.advance(73 * time.Minute) // 89m after the round
		h.tick()
		if h.parks(pr.ID) != parks || h.earlyParks(pr.ID) != 1 {
			t.Fatalf("the judge parked before judge_fresh_after: %v", h.ag.all())
		}
		h.advance(2 * time.Minute)
		h.tick()
		if h.parks(pr.ID) != parks+1 || h.earlyParks(pr.ID) != 1 {
			t.Fatalf("PR parks = %d, want %d at judge_fresh_after: %v", h.parks(pr.ID), parks+1, h.ag.all())
		}
		if got := h.liveRoles(pr.ID); len(got) != 0 {
			t.Fatalf("live roles after the PR's park = %v, want none", got)
		}
		if evs := h.parkedEvents(); len(evs) != 2 || !strings.HasPrefix(evs[1].Message, "parked 1 idle agents (idle since ") ||
			strings.Contains(string(evs[1].Data), "partial") {
			t.Fatalf("the PR park's events = %+v, want today's last", evs)
		}
	})
	t.Run("judge_fresh_after 0", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Pipeline.JudgeFreshAfter.Duration = 0
		pr := h.reviewedPR(2, "b1")
		h.withSessionIDs(pr.ID)
		parks := h.parks(pr.ID)
		h.advance(14 * time.Minute)
		h.tick()
		if h.parks(pr.ID) != parks {
			t.Fatalf("parked 14 minutes after the round: %v", h.ag.all())
		}
		h.advance(2 * time.Minute)
		h.tick()
		if h.parks(pr.ID) != parks+1 || h.earlyParks(pr.ID) != 0 {
			t.Fatalf("PR parks = %d (want %d), early parks %d (want 0): %v", h.parks(pr.ID), parks+1, h.earlyParks(pr.ID), h.ag.all())
		}
		if got := h.liveRoles(pr.ID); len(got) != 0 {
			t.Fatalf("live roles after the park = %v, want none", got)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		h := newHarness(t)
		pr := h.reviewedPR(2, "b1")
		h.withSessionIDs(pr.ID)
		h.e.d.DryRun = true
		h.advance(16 * time.Minute)
		h.tick()
		if h.earlyParks(pr.ID) != 0 {
			t.Fatalf("a dry run parked: %v", h.ag.all())
		}
		if ops := h.e.rec.Ops(); !slices.ContainsFunc(ops, func(op PlannedOp) bool {
			return op.Action == "park" && strings.Contains(op.Detail, "claude-review, claude-simplify idle since") && strings.Contains(op.Detail, "codex-judge stays")
		}) {
			t.Fatalf("dry-run records = %+v, want the early park", ops)
		}
	})
}

// A reviewer idle while the judge works is not idle until the round ends:
// a tick during a 30-minute round parks nothing, and the reviewer, idle
// since the round's start, parks park_idle_after after the round's end.
func TestAReviewerIdleWhileTheJudgeWorksParksOnlyAfterTheRoundEnds(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.advance(5 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()                    // the push: re-review pending
	h.advance(26 * time.Minute) // past min_rereview_interval; the tick that dispatches parks nothing
	gate := make(chan struct{})
	h.rd.mu.Lock()
	h.rd.gate = gate
	h.rd.mu.Unlock()
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.advance(30 * time.Minute) // the judge works; the reviewers idled since the round's start
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.e.drainHeavy(h.ctx)
	if h.earlyParks(pr.ID) != 0 {
		t.Fatalf("parked during the round: %v", h.ag.all())
	}
	gate <- struct{}{}
	h.settle()
	h.wantState(2, store.PRReviewed)

	h.advance(14 * time.Minute)
	h.tick()
	if h.earlyParks(pr.ID) != 0 {
		t.Fatalf("parked 14 minutes after the round's end: %v", h.ag.all())
	}
	h.advance(2 * time.Minute)
	h.tick()
	if h.earlyParks(pr.ID) != 1 {
		t.Fatalf("early parks = %d, want 1 at park_idle_after after the round's end: %v", h.earlyParks(pr.ID), h.ag.all())
	}
}

// The whiteboard job that starts right after a round starts within
// park_idle_after of the round's end: the idle park, which counts from the
// end of the PR's last round, fix job or whiteboard job, takes nothing in
// the gap between the round's end and the job's start (the job's wait for
// Whiteboard holds it there), however long the agents idled before the
// round, and counts the job's end as work. The fix cycle's side of the gap
// is TestAParkPlannedBeforeTheLatestWorkEndedNeverHoldsThePR's, and the fix
// job's end TestAFixJobsEndStartsTheIdleClockAgain's.
func TestAWhiteboardRightAfterTheRoundMeetsNoPark(t *testing.T) {
	w := wbHarness(t, true)
	wbWriter(w.harness, wbDone("wb-1"))
	pr := w.wbReviewed()
	w.withSessionIDs(pr.ID)
	if len(w.wbRuns(pr.ID)) != 1 {
		t.Fatalf("whiteboard runs = %d, want the first", len(w.wbRuns(pr.ID)))
	}
	w.advance(time.Hour) // every agent idles an hour before the next round
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)  // a failure below still lets the job end
	w.hold.Store(&hold) // the next whiteboard waits for Whiteboard's status after the round
	w.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixPushed})
	w.tick() // the push: re-review pending
	w.advance(10 * time.Minute)
	before := len(w.rd.all())
	if err := w.e.Tick(w.ctx); err != nil { // the round runs and posts; the whiteboard job waits
		t.Fatal(err)
	}
	w.awaitRound(before)
	for deadline := time.Now().Add(10 * time.Second); w.e.roundActive(pr.ID); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the round never ended")
		}
	}
	if err := w.e.Tick(w.ctx); err != nil { // between the round and the whiteboard
		t.Fatal(err)
	}
	w.e.drainHeavy(w.ctx)
	if w.earlyParks(pr.ID) != 0 || w.parks(pr.ID) != 0 {
		t.Fatalf("parked between the round and its whiteboard: %v", w.ag.all())
	}
	release()
	w.settle()
	if n := len(w.wbRuns(pr.ID)); n != 2 {
		t.Fatalf("whiteboard runs = %d, want the second after the round", n)
	}
	if got := strings.Join(w.liveRoles(pr.ID), ","); !strings.Contains(got, "claude-whiteboard") || !strings.Contains(got, "claude-review") {
		t.Fatalf("live roles after the whiteboard = %s, want the reviewers and the writer", got)
	}
	w.advance(14 * time.Minute)
	w.tick()
	if w.earlyParks(pr.ID) != 0 || w.parks(pr.ID) != 0 {
		t.Fatalf("parked 14 minutes after the whiteboard job: %v", w.ag.all())
	}
	w.advance(2 * time.Minute)
	w.tick()
	if w.earlyParks(pr.ID) != 1 {
		t.Fatalf("early parks = %d, want 1 park_idle_after after the whiteboard job: %v", w.earlyParks(pr.ID), w.ag.all())
	}
}

// The idle park takes a draft's agents after its review (ineligible): the
// next round resumes them. A PR that needs attention keeps its agents (the
// remedies send the operator to read the pane), as do a pinned or held
// slot and a keystroke within human_cooldown.
func TestTheIdleParkTakesADraftsAgentsButNotAnAttentionPinHoldOrHumanPRs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(h *harness, pr store.PR)
		parked bool
	}{
		{"an ineligible draft", func(h *harness, _ store.PR) {
			withoutDrafts(h)
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", draft: true}) // a push to the draft
			h.tick()
			h.wantState(2, store.PRIneligible)
		}, true},
		{"needs attention", func(h *harness, pr store.PR) {
			if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRReviewed}, store.PRNeedsAttention, nil); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"a pinned slot", func(h *harness, pr store.PR) { h.setSlotOf(pr.ID, func(u *store.SlotUpdate) { u.Set("pinned", true) }) }, false},
		{"a held slot", func(h *harness, pr store.PR) {
			h.setSlotOf(pr.ID, func(u *store.SlotUpdate) { u.Set("hold_reason", "head_drift") })
		}, false},
		{"a keystroke within human_cooldown", func(h *harness, pr store.PR) {
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", h.clock.Now().Add(3*time.Hour-time.Minute)) }); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			pr := h.reviewedPR(2, "b1")
			h.withSessionIDs(pr.ID)
			tc.setup(h, pr)
			h.advance(3 * time.Hour)
			h.tick()
			if parked := len(h.liveRoles(pr.ID)) == 0; parked != tc.parked {
				t.Fatalf("parked %v, want %v: live %v, calls %v", parked, tc.parked, h.liveRoles(pr.ID), h.ag.all())
			}
		})
	}
}

// setSlotOf edits the slot holding the PR.
func (h *harness) setSlotOf(prID int64, set func(u *store.SlotUpdate)) {
	h.t.Helper()
	sl, has, err := h.e.slotOf(h.ctx, prID)
	if err != nil || !has {
		h.t.Fatalf("slot: %v %v", has, err)
	}
	if err := h.st.UpdateSlotFields(h.ctx, sl.ID, set); err != nil {
		h.t.Fatal(err)
	}
}

// A session without a conversation to resume is never parked early: it
// waits for the PR's park. A working agent keeps every session of the PR,
// and an early park the agents refuse (an agent at work by the time it
// runs, say) leaves the sessions live and records the busy failure.
func TestAnEarlyParkTakesOnlyResumableIdleSessions(t *testing.T) {
	t.Run("no session id", func(t *testing.T) {
		h := newHarness(t)
		pr := h.reviewedPR(2, "b1") // the fake records no session ids
		parks := h.parks(pr.ID)
		h.advance(16 * time.Minute)
		h.tick()
		if h.earlyParks(pr.ID) != 0 || h.parks(pr.ID) != parks {
			t.Fatalf("parked sessions without a conversation early: %v", h.ag.all())
		}
		h.advance(75 * time.Minute)
		h.tick()
		if h.parks(pr.ID) != parks+1 || h.earlyParks(pr.ID) != 0 {
			t.Fatalf("PR parks = %d, want %d at judge_fresh_after: %v", h.parks(pr.ID), parks+1, h.ag.all())
		}
	})
	t.Run("a kind with session_source none", func(t *testing.T) {
		h := newHarness(t)
		pr := h.reviewedPR(2, "b1")
		h.withSessionIDs(pr.ID) // herdr may show an id the kind never resumes
		k, ok := h.cfg.Kinds["claude"]
		if !ok {
			t.Fatalf("no claude kind: %v", h.cfg.KindNames())
		}
		k.SessionSource = config.SessionNone
		h.cfg.Kinds["claude"] = k
		h.advance(16 * time.Minute)
		h.tick()
		if h.earlyParks(pr.ID) != 0 {
			t.Fatalf("parked sessions of a kind that never resumes early: %v", h.ag.all())
		}
	})
	for _, status := range []herdr.Status{herdr.StatusWorking, herdr.StatusBlocked} {
		t.Run("an agent "+string(status), func(t *testing.T) {
			h := newHarness(t)
			pr := h.reviewedPR(2, "b1")
			h.withSessionIDs(pr.ID)
			judge, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "codex-judge")
			if err != nil {
				t.Fatal(err)
			}
			if err := h.st.UpdateSession(h.ctx, judge.ID, func(u *store.SessionUpdate) { u.Set("agent_status", string(status)) }); err != nil {
				t.Fatal(err)
			}
			h.advance(16 * time.Minute)
			h.tick()
			if h.earlyParks(pr.ID) != 0 {
				t.Fatalf("parked beside a %s agent: %v", status, h.ag.all())
			}
		})
	}
	t.Run("a refused early park", func(t *testing.T) {
		h := newHarness(t)
		pr := h.reviewedPR(2, "b1")
		h.withSessionIDs(pr.ID)
		h.ag.mu.Lock()
		h.ag.parkSessionsErr = fmt.Errorf("agents: park pr 2: claude-review is working: %w", agents.ErrBusy)
		h.ag.mu.Unlock()
		h.advance(16 * time.Minute)
		h.tick()
		if h.earlyParks(pr.ID) != 1 {
			t.Fatalf("early parks = %d, want the one try: %v", h.earlyParks(pr.ID), h.ag.all())
		}
		if got := strings.Join(h.liveRoles(pr.ID), ","); got != reviewersLive {
			t.Fatalf("live roles = %s, want all of them: %s", got, reviewersLive)
		}
		evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(evs, func(ev store.Event) bool {
			return ev.Kind == "pr.park_failed" && strings.Contains(ev.Message, "("+parkBusy+":")
		}) {
			t.Fatalf("no busy pr.park_failed: %+v", evs)
		}
		if len(h.parkedEvents()) != 0 {
			t.Fatal("pr.parked recorded for a park that failed")
		}
		h.tick() // the backoff holds the next try
		if h.earlyParks(pr.ID) != 1 {
			t.Fatalf("tried again inside the backoff: %v", h.ag.all())
		}
	})
}

// An early park that keeps failing (a reviewer that does not quit) backs
// off as any failed park does, but does not hold the PR's park: once every
// session is past its threshold (the judge's judge_fresh_after) the PR's
// park comes at the next tick, hard after the quit timeouts, not at the end
// of the early park's doubled wait.
func TestAnEarlyParkThatKeepsFailingDoesNotHoldThePRsPark(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.ag.mu.Lock()
	h.ag.parkSessionsErr = quitTimedOut(2)
	h.ag.mu.Unlock()
	tick := h.cfg.Daemon.PollInterval.Duration
	for elapsed := tick; elapsed < h.cfg.Pipeline.JudgeFreshAfter.Duration; elapsed += tick {
		h.advance(tick)
		h.tick()
	}
	if n := h.earlyParks(pr.ID); n < 2 || n > 8 {
		t.Fatalf("early park tries = %d, want a few, backed off: %v", n, h.ag.all())
	}
	if h.ag.count(fmt.Sprintf("park_hard:%d", pr.ID))+h.parks(pr.ID) != 0 {
		t.Fatalf("the PR's park came before judge_fresh_after: %v", h.ag.all())
	}
	h.advance(tick)
	h.tick()
	if got := h.ag.count(fmt.Sprintf("park_hard:%d", pr.ID)); got != 1 {
		t.Fatalf("hard parks = %d, want the PR's park at judge_fresh_after, hard after the quit timeouts: %v", got, h.ag.all())
	}
	if got := h.liveRoles(pr.ID); len(got) != 0 {
		t.Fatalf("live roles after the PR's park = %v, want none", got)
	}
}

// The next round of a PR whose reviewers parked early resumes the ones it
// runs through the role's conversation (ensureAgent) and reuses the live
// judge.
func TestTheNextRoundResumesTheParkedReviewersAndReusesTheLiveJudge(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.advance(16 * time.Minute)
	h.tick()
	if got := strings.Join(h.liveRoles(pr.ID), ","); got != judgeLive {
		t.Fatalf("live roles after the early park = %s, want %s", got, judgeLive)
	}
	h.ag.mu.Lock()
	h.ag.resumeIDs = map[agents.Role]string{"claude-review": "sid-claude-review", "claude-simplify": "sid-claude-simplify"}
	starts := len(h.ag.startedRoles)
	h.ag.mu.Unlock()

	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(15 * time.Minute) // past min_rereview_interval, within judge_fresh_after
	h.tick()
	h.wantState(2, store.PRReviewed)
	if want := "start:" + itoa(pr.ID) + ":claude-review:sid-claude-review"; !slices.Contains(h.ag.all(), want) {
		t.Errorf("no %s: %v", want, h.ag.all())
	}
	h.ag.mu.Lock()
	started := slices.Clone(h.ag.startedRoles[starts:])
	h.ag.mu.Unlock()
	if !slices.Equal(started, []agents.Role{"claude-review"}) {
		t.Fatalf("the next round started %v, want the parked claude-review alone (the live judge reused)", started)
	}
	if got := strings.Join(h.liveRoles(pr.ID), ","); got != "claude-review,codex-judge" {
		t.Fatalf("live roles after the next round = %s, want the re-review's claude-review and codex-judge", got)
	}
}

// With the agents' own park (agents.Manager on a fake herdr), the early
// park parks the reviewers with their conversations and closes their panes
// alone: the judge's pane, the codex-review shell's, the operator's shell
// and the workspace stay; at judge_fresh_after the PR's park takes the rest.
func TestAnEarlyParkClosesOnlyThePanesOfTheSessionsItParks(t *testing.T) {
	h, sh := newStuckHarness(t)
	pr := h.reviewedPR(2, "b1")
	ws, operator, _ := operatorShell(t, h, sh, 2)
	paneOf := map[string]string{}
	for _, s := range h.sessionsByRole(pr.ID) {
		paneOf[s.Role] = deref(s.HerdrPaneID)
	}

	h.advance(16 * time.Minute)
	h.tick()
	want := []string{paneOf["codex-judge"], paneOf["codex-review"], operator}
	if got := sh.panesOf(ws); !sameSet(got, want) {
		t.Fatalf("workspace panes after the early park = %v, want the judge's, the shell's and the operator's %v", got, want)
	}
	for _, s := range h.sessionsByRole(pr.ID) {
		switch s.Role {
		case "claude-review", "claude-simplify":
			if s.State != store.SessionParked || deref(s.SessionID) == "" {
				t.Errorf("%s = %s (id %q), want parked with its conversation", s.Role, s.State, deref(s.SessionID))
			}
		default:
			if !isLiveSession(s) {
				t.Errorf("%s = %s, want live", s.Role, s.State)
			}
		}
	}
	// The early park closed each pane itself, before parking its session:
	// the retired-pane close after it finds nothing of theirs left.
	if got := panesClosed(t, h, 2); len(got) != 0 {
		t.Fatalf("%s events = %+v, want none: the early park closed the reviewers' panes", agents.EventPanesClosed, got)
	}
	parkedOK := 0
	for _, ev := range approvalEvents(t, h, 2, agents.EventSessionParked) {
		if strings.Contains(ev.Message, "closed") {
			parkedOK++
		}
	}
	if parkedOK != 2 {
		t.Fatalf("%s events naming the pane's close = %d, want one per reviewer", agents.EventSessionParked, parkedOK)
	}

	h.advance(75 * time.Minute)
	h.tick()
	if got := sh.panesOf(ws); !slices.Equal(got, []string{operator}) {
		t.Fatalf("workspace panes after the PR's park = %v, want the operator's shell alone", got)
	}
	if got := h.liveRoles(pr.ID); len(got) != 0 {
		t.Fatalf("live roles after the PR's park = %v, want none", got)
	}
}

// sessionsByRole are the PR's newest session of each role.
func (h *harness) sessionsByRole(prID int64) map[string]store.Session {
	h.t.Helper()
	ss, err := h.st.SessionsByPR(h.ctx, prID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]store.Session{}
	for _, s := range ss {
		if cur, ok := out[s.Role]; !ok || s.ID > cur.ID {
			out[s.Role] = s
		}
	}
	return out
}

// A park job is planned at a tick and runs on the heavy worker, maybe after
// a round of the PR that started and ended meanwhile. Such a job never takes
// the PR: in the gap between the round's end and the fix cycle (or the
// whiteboard) that follows it, its reservation would refuse their start, and
// the fix loop would end needing attention. A plan made after the round
// takes the PR as before.
func TestAParkPlannedBeforeTheLatestWorkEndedNeverHoldsThePR(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.advance(20 * time.Minute)
	planned := h.clock.Now() // the tick that queued the park job
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(31 * time.Minute)
	h.tick() // the re-review runs and ends after the plan
	h.wantState(2, store.PRReviewed)

	if h.e.reserveParkWork(pr.ID, planned) {
		h.e.endEviction(pr.ID)
		t.Fatal("a park planned before the round's end took the PR")
	}
	if !h.e.reserve(pr.ID, &roundHandle{cancel: func() {}, fix: true}) { // the fix cycle after the round
		t.Fatalf("the fix cycle after the round was refused: %s", h.e.heldReason(pr.ID))
	}
	h.e.unreserve(pr.ID)

	// Run late enough that a plan made now would park the reviewers, the
	// stale job still takes nothing: it never reserves, so never plans again.
	h.withSessionIDs(pr.ID)
	h.advance(16 * time.Minute)
	parks := h.parks(pr.ID)
	if err := h.e.parkJob(pr.ID, planned)(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.parks(pr.ID) != parks || h.earlyParks(pr.ID) != 0 {
		t.Fatalf("the stale park job took the PR and parked: %v", h.ag.all())
	}
	h.tick() // a plan made now parks them
	if h.earlyParks(pr.ID) != 1 {
		t.Fatalf("early parks = %d, want the fresh plan's: %v", h.earlyParks(pr.ID), h.ag.all())
	}
}

// A fix job's end counts as the PR's work as a round's does: the agents
// the fixer did not use, idle since the review, park park_idle_after after
// the fix job ended, not after the round before it.
func TestAFixJobsEndStartsTheIdleClockAgain(t *testing.T) {
	h, git := fixHarness(t, realSleep)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	h.withSessionIDs(pr.ID)
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.advance(10 * time.Minute)
	h.fix(FixPayload{})
	h.handle() // the fix job runs to its end, 10 minutes after the review
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d, want the one", n)
	}
	h.advance(10 * time.Minute)
	h.tick()
	if h.earlyParks(pr.ID) != 0 {
		t.Fatalf("parked 10 minutes after the fix job (20 after the review): %v", h.ag.all())
	}
	h.advance(6 * time.Minute)
	h.tick()
	if h.earlyParks(pr.ID) != 1 {
		t.Fatalf("early parks = %d, want 1 park_idle_after after the fix job: %v", h.earlyParks(pr.ID), h.ag.all())
	}
}

// An early park whose batch partly fails still records the sessions it
// parked (pr.parked with their roles) and closes their panes, beside the
// failure's pr.park_failed and backoff.
func TestAnEarlyParkThatPartlyFailsRecordsWhatItParked(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.ag.mu.Lock()
	h.ag.parkSessionsErr = quitTimedOut(2) // claude-simplify does not quit
	h.ag.parkSessionsOK = []string{"claude-review"}
	h.ag.mu.Unlock()
	h.advance(16 * time.Minute)
	h.tick()
	if got := strings.Join(h.liveRoles(pr.ID), ","); got != "claude-simplify,codex-judge" {
		t.Fatalf("live roles = %s, want claude-review parked and claude-simplify left", got)
	}
	evs := h.parkedEvents()
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"roles":["claude-review"]`) || !strings.HasPrefix(evs[0].Message, "parked claude-review (park_idle_after 15m)") {
		t.Fatalf("pr.parked events = %+v, want claude-review's", evs)
	}
	if h.ag.count(fmt.Sprintf("close_panes:%d", pr.ID)) == 0 {
		t.Fatalf("the parked session's pane was not closed: %v", h.ag.all())
	}
	all, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(all, func(ev store.Event) bool { return ev.Kind == "pr.park_failed" }) {
		t.Fatal("no pr.park_failed for the session that failed")
	}
}

// Each parked role is named with the threshold it idled past: a judge past
// judge_fresh_after parks early beside a reviewer past park_idle_after
// while another reviewer, prompted lately, stays.
func TestAnEarlyParkNamesEachRolesThreshold(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.advance(91 * time.Minute)
	claude, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "claude-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpdateSession(h.ctx, claude.ID, func(u *store.SessionUpdate) { u.Set("last_prompt_at", h.clock.Now().Add(-5*time.Minute)) }); err != nil {
		t.Fatal(err)
	}
	h.tick()
	evs := h.parkedEvents()
	want := "parked codex-judge (judge_fresh_after 1h30m) and claude-simplify (park_idle_after 15m), idle since "
	if len(evs) != 1 || !strings.HasPrefix(evs[0].Message, want) || !strings.Contains(evs[0].Message, "claude-review stays until park_idle_after (15m)") {
		t.Fatalf("pr.parked events = %+v, want %q... with claude-review staying", evs, want)
	}
}

// Before the PR has a work-end record (the first run after the upgrade
// that added it) the idle clock starts at the latest end of a run of its
// last round, so reviewers idle since their own turns do not park from a
// status change earlier than the round's end.
func TestWithoutAWorkEndRecordTheClockStartsAtTheLastRunsEnd(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.e.delKV(h.ctx, kvPRWorkEnded(pr.ID))
	ended := h.clock.Now().Add(10 * time.Minute) // the judge's turn ended 10 minutes into the round
	run, err := h.st.CreateRun(h.ctx, store.Run{ID: "run-judge-2", PRID: pr.ID, Round: 1, Role: "codex-judge", Kind: store.RunInitial,
		TargetSHA: "b1", Identity: pr.Identity, State: store.RunPending, PromptText: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionRun(h.ctx, run.ID, nil, store.RunEnded, func(u *store.RunUpdate) { u.Set("ended_at", ended) }); err != nil {
		t.Fatal(err)
	}
	h.advance(16 * time.Minute)
	h.tick()
	if h.earlyParks(pr.ID) != 0 {
		t.Fatalf("parked 6 minutes after the round's last turn: %v", h.ag.all())
	}
	h.advance(10 * time.Minute)
	h.tick()
	if h.earlyParks(pr.ID) != 1 {
		t.Fatalf("early parks = %d, want 1 park_idle_after after the last turn: %v", h.earlyParks(pr.ID), h.ag.all())
	}
}
