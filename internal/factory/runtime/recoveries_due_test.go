package runtime

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// A recovery that waits for its due time outlives a restart: the watchdog
// finds the role's run in flight on a session that is no longer live and
// takes the role's next recovery, with no call of the stage's.
func TestAWaitedRecoveryOutlivesARestart(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	g.h.losePane(g.agent("fx-repro"))
	g.observe() // recovery 1, at once
	g.observeN(2)
	g.h.losePane(g.agent("fx-repro"))
	g.observe() // recovery 2 waits its minute
	starts := len(g.h.startCalls())
	g.restart() // the wait in memory is gone
	g.watchdog()
	g.afterWait(2)
	g.afterWait(2)
	if got := len(g.h.startCalls()); got != starts+1 {
		t.Fatalf("%d starts after a restart during the wait, want the waited recovery once", got-starts)
	}
}

// A due recovery of a task that waits on you (another role's questions) is
// kept, not dropped: once the task is at work again, the role is
// recovered.
func TestADueRecoveryWaitsForATaskThatWaitsOnYou(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	g.h.losePane(g.agent("fx-repro"))
	g.observe()
	g.observeN(2)
	g.h.losePane(g.agent("fx-repro"))
	g.observe() // recovery 2 waits its minute
	starts := len(g.h.startCalls())
	g.setTaskState(registry.TaskNeedsYou)
	g.afterWait(2)
	if got := len(g.h.startCalls()); got != starts {
		t.Fatalf("%d starts while the task waits on you, want none", got-starts)
	}
	g.setTaskState(registry.TaskActive)
	g.watchdog()
	if got := len(g.h.startCalls()); got != starts+1 {
		t.Fatalf("%d starts once the task is at work again, want the kept recovery", got-starts)
	}
}

// A recovery that waits after a no-progress finding is void once the
// session moves again (its transcript or its screen changed during the
// wait): the watchdog restarts no session that works.
func TestAWaitedRecoveryOfASessionThatMovesAgainIsVoid(t *testing.T) {
	g := newRig(t)
	g.lostThenResumed("fx-repro", "sid-1")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusWorking)
	g.watchdog()
	g.clock.Add(NoProgressAfter)
	g.watchdog() // no progress: the next recovery waits for its due time
	starts := len(g.h.startCalls())
	g.h.screen(g.agent("fx-repro"), "Bash(bin/rspec)\nrunning the specs again\n")
	g.watchdog()
	g.afterWait(3)
	if got := len(g.h.startCalls()); got != starts {
		t.Fatalf("%d starts after the session moved again during the wait, want none", got-starts)
	}
}

// When a role's recoveries run out, its run in flight on the lost session
// is handed over (abandoned with RecoveryPrefix), so a stage rebuilt by
// your retry recovers the role (RecoverLost).
func TestTheRecoveriesRunningOutHandTheRunOver(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	g.loseAll("fx-repro")
	pr := g.owner()
	runs, err := g.st.RunsByPR(g.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		switch run.State {
		case store.RunPending, store.RunSubmitted, store.RunWorking:
			t.Fatalf("run %s of %s is still in flight on the lost session", run.ID, run.Role)
		case store.RunAbandoned:
			if run.Role == "fx-repro" && !strings.HasPrefix(store.Deref(run.Error), RecoveryPrefix) {
				t.Fatalf("run %s abandoned with %q, want %q", run.ID, store.Deref(run.Error), RecoveryPrefix)
			}
		}
	}
}

// StartOver starts a lost role's recoveries over (your answer to its
// question, for a role that took no prompt yet): it is lost no more, and a
// "starts over" event counts the recoveries from it.
func TestStartOverStartsALostRolesRecoveriesOver(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	g.loseAll("fx-repro")
	g.r.StartOver(g.ctx, g.task, g.attempt, g.role("fx-repro"), "your answer")
	st, err := g.r.stateFor(g.ctx, g.attempt.ID, "fx-repro")
	if err != nil || st.lost || g.r.recoveriesUsed(g.ctx, st) != 0 {
		t.Fatalf("after StartOver the role is lost %v with %d recoveries used (%v); want neither", st.lost, g.r.recoveriesUsed(g.ctx, st), err)
	}
}

// setTaskState moves the rig's task to state (a test's stand-in for the
// factory's own moves).
func (g *rig) setTaskState(state string) {
	g.t.Helper()
	if err := g.reg.TransitionTask(g.ctx, g.task.ID, nil, state, func(u *registry.TaskUpdate) { u.Because("a test") }); err != nil {
		g.t.Fatal(err)
	}
}

// A recovery that waits after a no-progress finding outlives a restart as
// such: the restart takes a fresh look at the session (a new baseline) and
// runs the recovery at its due time unless the session moves, though its
// session is live.
func TestANoProgressWaitOutlivesARestart(t *testing.T) {
	g := newRig(t)
	g.lostThenResumed("fx-repro", "sid-1")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusWorking)
	g.watchdog()
	g.clock.Add(NoProgressAfter)
	g.watchdog() // no progress: recovery 3 waits its 5 minutes
	starts := len(g.h.startCalls())
	g.restart()
	g.watchdog()
	g.afterWait(3)
	if got := len(g.h.startCalls()); got != starts+1 {
		t.Fatalf("%d starts after a restart during a no-progress wait, want the recovery once", got-starts)
	}
}

// A waited recovery voided in memory (the session moved again) is voided in
// its events too: a restart reads no due recovery back.
func TestAVoidedWaitStaysVoidAfterARestart(t *testing.T) {
	g := newRig(t)
	g.lostThenResumed("fx-repro", "sid-1")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusWorking)
	g.watchdog()
	g.clock.Add(NoProgressAfter)
	g.watchdog() // no progress: the next recovery waits
	starts := len(g.h.startCalls())
	g.h.screen(g.agent("fx-repro"), "Bash(bin/rspec)\nrunning the specs again\n")
	g.watchdog() // the session moved: the wait is void
	g.restart()
	g.watchdog()
	g.afterWait(3)
	if got := len(g.h.startCalls()); got != starts {
		t.Fatalf("%d starts after a restart: the voided wait came back", got-starts)
	}
}
