package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// quitTimedOut is the park failure of talkable#11932: the fixer's Claude
// pane was no idle shell 10 s after its ctrl+c.
func quitTimedOut(n int) error {
	return fmt.Errorf("agents: park pr %d: agents: quit session 535: %w: %w", n, agents.ErrBusy,
		fmt.Errorf("herdr pane p9: shell not idle after 10s (foreground: 2.1.296): %w", herdr.ErrTimeout))
}

// failParks makes every Park return err (nil: Park works again).
func (h *harness) failParks(err error) {
	h.ag.mu.Lock()
	h.ag.parkErr = err
	h.ag.mu.Unlock()
}

// nextParkAttempt ticks one poll interval at a time until the PR's next
// park attempt and returns how many ticks it took.
func (h *harness) nextParkAttempt(prID int64) int {
	h.t.Helper()
	before := h.parks(prID)
	for n := 1; n <= 200; n++ {
		h.advance(h.cfg.Daemon.PollInterval.Duration)
		h.tick()
		if h.parks(prID) > before {
			return n
		}
	}
	h.t.Fatalf("no park attempt within 200 ticks: %v", h.ag.all())
	return 0
}

// TestAFailedParkBacksOff: a failed park of a PR's idle agents is tried
// again after twice the last wait, starting from the tick interval (two
// ticks, then four, eight, ...) up to 12 hours, rather than on every tick
// (talkable#11932: 1,434 tries in 37 hours, each two ctrl+c into a live
// Claude pane). A round of the PR resets the wait.
func TestAFailedParkBacksOff(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.failParks(quitTimedOut(2))
	before := h.parks(pr.ID)
	h.advance(2*time.Hour + time.Minute)
	h.tick()
	if got := h.parks(pr.ID); got != before+1 {
		t.Fatalf("parks = %d, want the first try %d", got, before+1)
	}
	tick := h.cfg.Daemon.PollInterval.Duration
	wait := tick
	for i := 2; i <= 14; i++ {
		wait = min(2*wait, 12*time.Hour)
		h.advance(wait - tick)
		h.tick()
		if got := h.parks(pr.ID); got != before+i-1 {
			t.Fatalf("try %d came %s after the last one, before its wait of %s", i, wait-tick, wait)
		}
		h.advance(tick)
		h.tick()
		if got := h.parks(pr.ID); got != before+i {
			t.Fatalf("try %d did not come %s after the last one", i, wait)
		}
	}
	if wait != 12*time.Hour {
		t.Fatalf("the test never reached the cap: last wait %s", wait)
	}

	// A new round resets the wait: the next try comes once the PR is
	// reviewed again, not 12 hours after the last one, and the one after it
	// two ticks later.
	h.push(2, "b2")
	h.tick() // rereview_pending: the push's quiet period
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	rounds := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil { // dispatches the round, which waits at the gate
		t.Fatal(err)
	}
	h.awaitRound(rounds)
	close(h.rd.gate)
	h.settle()
	h.wantState(2, store.PRReviewed)
	tries := h.parks(pr.ID)
	if n := h.nextParkAttempt(pr.ID); n != 1 {
		t.Fatalf("the first try after the round came after %d ticks, want 1", n)
	}
	if got := h.parks(pr.ID); got != tries+1 {
		t.Fatalf("parks = %d, want %d", got, tries+1)
	}
	if n := h.nextParkAttempt(pr.ID); n != 2 {
		t.Fatalf("the try after the round's first failure came after %d ticks, want 2", n)
	}
}

// TestAFailedParkRecordsOneEventPerCause: the first failure of a cause
// records pr.park_failed (warn) with the failure's class and the backoff;
// later failures of the same cause record nothing until the cause changes.
func TestAFailedParkRecordsOneEventPerCause(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.failParks(quitTimedOut(2))
	h.advance(2*time.Hour + time.Minute)
	h.tick() // failure 1, next try in 1m
	evs := h.events("pr.park_failed")
	if len(evs) != 1 {
		t.Fatalf("pr.park_failed events = %d, want 1", len(evs))
	}
	for _, want := range []string{"an agent did not quit", "shell not idle after 10s", "next try in 1m", "up to 12h"} {
		if evs[0].Level != "warn" || !strings.Contains(evs[0].Message, want) {
			t.Fatalf("event = %s %q, want warn naming %q", evs[0].Level, evs[0].Message, want)
		}
	}
	if !h.hasEvent("pr:talkable/talkable#2", "pr.park_failed") {
		t.Fatal("the event is not on the PR's subject")
	}
	h.nextParkAttempt(pr.ID) // failure 2
	h.nextParkAttempt(pr.ID) // failure 3
	if evs := h.events("pr.park_failed"); len(evs) != 1 {
		t.Fatalf("the same cause recorded %d events, want 1: %v", len(evs), evs)
	}

	h.failParks(fmt.Errorf("agents: park pr 2: run r-7 is working: %w", agents.ErrBusy))
	h.nextParkAttempt(pr.ID) // failure 4, next try in 8m
	h.nextParkAttempt(pr.ID) // failure 5
	evs = h.events("pr.park_failed")
	if len(evs) != 2 || !strings.Contains(evs[1].Message, "busy") || !strings.Contains(evs[1].Message, "next try in 8m") {
		t.Fatalf("events after the cause changed = %v, want a second one naming busy and 8m", evs)
	}

	h.failParks(quitTimedOut(2))
	h.nextParkAttempt(pr.ID) // failure 6: the first cause again is a change
	if evs := h.events("pr.park_failed"); len(evs) != 3 {
		t.Fatalf("events after the cause changed back = %d, want 3", len(evs))
	}
}

// TestAParkThatSucceedsAfterFailuresClearsTheBackoff: a park that works
// after failures records pr.parked as any park does and forgets the
// failures, so a later failure records its cause again and waits two
// ticks.
func TestAParkThatSucceedsAfterFailuresClearsTheBackoff(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.failParks(quitTimedOut(2))
	h.advance(2*time.Hour + time.Minute)
	h.tick()                 // failure 1
	h.nextParkAttempt(pr.ID) // failure 2, next try in 2m
	h.failParks(nil)
	if n := h.nextParkAttempt(pr.ID); n != 4 {
		t.Fatalf("the try after two failures came after %d ticks, want 4", n)
	}
	parked := h.events("pr.parked")
	if len(parked) != 1 || !strings.Contains(parked[0].Message, "the next round resumes them") {
		t.Fatalf("pr.parked = %v, want the usual one", parked)
	}
	live, err := h.st.LiveSessions(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(live, func(s store.Session) bool { return s.PRID == pr.ID }) {
		t.Fatal("sessions still live after the park")
	}
	h.e.parkMu.Lock()
	_, kept := h.e.parkFails[pr.ID]
	h.e.parkMu.Unlock()
	if kept {
		t.Fatal("the backoff outlived the park that worked")
	}

	// The next round resumes the agents; their park fails again.
	h.push(2, "b2")
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
	h.failParks(quitTimedOut(2))
	h.advance(2*time.Hour + time.Minute)
	tries := h.parks(pr.ID)
	h.tick()
	if got := h.parks(pr.ID); got != tries+1 {
		t.Fatalf("parks = %d, want %d", got, tries+1)
	}
	if evs := h.events("pr.park_failed"); len(evs) != 2 {
		t.Fatalf("pr.park_failed events = %d, want the cause recorded again after the park that worked", len(evs))
	}
	if n := h.nextParkAttempt(pr.ID); n != 2 {
		t.Fatalf("the try after the new failure came after %d ticks, want 2", n)
	}
}

// TestTheIdleParkTakesAnIdleFixerToo: a fix role's session is no exception
// to the idle park (DECISIONS "A park that fails backs off"): its
// conversation stays resumable, and the next fix job resumes it.
func TestTheIdleParkTakesAnIdleFixerToo(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	fixer, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: "claude-fix", AgentName: new("mg-t-2-claude-fix"),
		AgentKind: new("claude"), SessionID: new("c-1"), HerdrPaneID: new("p-fix"), HerdrWorkspaceID: new("w1"), State: store.SessionLive})
	if err != nil {
		t.Fatal(err)
	}
	before := h.parks(pr.ID)
	h.advance(2*time.Hour + time.Minute)
	h.tick()
	if got := h.parks(pr.ID); got != before+1 {
		t.Fatalf("parks = %d, want %d", got, before+1)
	}
	ss, err := h.st.SessionsByPR(h.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ss, func(s store.Session) bool { return s.ID == fixer.ID })
	if i < 0 || ss[i].State != store.SessionParked {
		t.Fatalf("the fixer's session = %+v, want parked", ss)
	}
}
