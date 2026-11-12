package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
)

// loseAll runs role name's lost pane through its MaxRecoveries recoveries,
// each after its wait, and loses it once more: its events, EventLost last.
func (g *rig) loseAll(name string) []Event {
	g.t.Helper()
	var evs []Event
	for n := 1; n <= MaxRecoveries; n++ {
		g.h.losePane(g.agent(name))
		evs = append(evs, g.observe()...)
		evs = append(evs, g.afterWait(n)...)
		g.observeN(2)
	}
	g.h.losePane(g.agent(name))
	return append(evs, g.observe()...)
}

// Your retry after the recoveries ran out starts them over and spends none
// of them: the role gets MaxRecoveries recoveries again, after a restart
// too, since the count is read from your retry on.
func TestYourRetryStartsTheRecoveriesOverAndSpendsNone(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	if lost := only(g.loseAll("fx-repro"), EventLost); len(lost) != 1 {
		t.Fatalf("events %+v, want the recoveries run out", lost)
	}
	if j := g.r.Judged(g.task, "fx-repro"); !strings.Contains(j, "lost") {
		t.Fatalf("Judged = %q for a pane lost past its recoveries, want its judgment", j)
	}

	g.retried()
	g.restart()
	starts := len(g.h.startCalls())
	if err := g.r.RecoverLost(g.ctx, g.task, g.attempt, g.role("fx-repro")); err != nil {
		t.Fatal(err)
	}
	if s := g.h.startCalls(); len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts %+v after your retry, want the first recovery again: a resume", s[starts:])
	}
	g.observeN(2)
	var evs []Event
	for n := 2; n <= MaxRecoveries; n++ {
		g.h.losePane(g.agent("fx-repro"))
		evs = append(evs, g.observe()...)
		evs = append(evs, g.afterWait(n)...)
		g.observeN(2)
	}
	if lost := only(evs, EventLost); len(lost) != 0 || len(g.h.startCalls()) != starts+MaxRecoveries {
		t.Fatalf("events %+v and %d starts after your retry, want all %d recoveries again", evs, len(g.h.startCalls())-starts,
			MaxRecoveries)
	}
}

// A recovery that waits for its due time and is overtaken by the role's
// own progress (its turn ends, the next turn works) never runs later: the
// turn's end clears the wait, and the watchdog restarts no healthy session.
func TestAWaitedRecoveryOvertakenByATurnsEndRestartsNothing(t *testing.T) {
	g := newRig(t)
	g.lostThenResumed("fx-repro", "sid-1")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusWorking)
	g.watchdog()
	g.clock.Add(NoProgressAfter)
	g.watchdog() // the resumed session shows no progress: the next recovery waits for its due time
	starts := len(g.h.startCalls())
	g.endRun("fx-repro")
	g.send("fx-repro", "the next turn")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusWorking)
	g.observe()
	g.clock.Add(2 * time.Minute)
	g.watchdog()
	if n := len(g.h.startCalls()); n != starts {
		t.Fatalf("%d starts after the turn ended and the next one works, want none: a stale recovery restarted a healthy session", n-starts)
	}
}

// The recoveries count the same in memory and after a restart: a retry of
// yours for another cause while the role still has recoveries left starts
// nothing over in either, so the next loss after two recoveries takes the
// third (a fresh session), not a first one again.
func TestTheRecoveriesCountTheSameInMemoryAndAfterARestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		g := newRig(t)
		g.launch("fx-repro")
		g.h.setSession(g.agent("fx-repro"), "sid-1")
		g.send("fx-repro", "reproduce the bug")
		g.observeN(2)
		for n := 1; n <= 2; n++ {
			g.h.losePane(g.agent("fx-repro"))
			g.observe()
			g.afterWait(n)
			g.observeN(2)
		}
		g.retried() // your retry of a check's question, say
		if restart {
			g.restart()
		}
		starts := len(g.h.startCalls())
		g.h.losePane(g.agent("fx-repro"))
		g.observe()
		g.afterWait(3)
		if s := g.h.startCalls(); len(s) != starts+1 || strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume") {
			t.Fatalf("restart %v: starts %+v after two recoveries and an unrelated retry, want the third: a fresh session", restart, s[starts:])
		}
	}
}

// The writer's fresh session at its fix budget keeps its lost-pane budget
// as it was: a loss after it takes the role's next recovery and then the
// question, never three recoveries more. Your answer's fresh start does
// start them over.
func TestTheFixBudgetsFreshSessionKeepsThePanesRecoveries(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	for n := 1; n <= 2; n++ {
		g.h.losePane(g.agent("fx-repro"))
		g.observe()
		g.afterWait(n)
		g.observeN(2)
	}
	if _, err := g.r.SendFresh(g.ctx, g.task, g.attempt, g.role("fx-repro"), "fix it", false); err != nil {
		t.Fatal(err)
	}
	g.observeN(2)
	var evs []Event
	g.h.losePane(g.agent("fx-repro"))
	evs = append(evs, g.observe()...)
	evs = append(evs, g.afterWait(3)...)
	g.observeN(2)
	g.h.losePane(g.agent("fx-repro"))
	evs = append(evs, g.observe()...)
	if lost := only(evs, EventLost); len(lost) != 1 || lost[0].Reason != "no progress after 3 recoveries" {
		t.Fatalf("events %+v, want the third recovery, then lost: the fix budget's fresh session spends no pane budget", evs)
	}
}

// The writer's go-ons at its fix budget start the runtime's loop ladder
// over (MadeProgress), as your retry does: with fix_go_on at 2, 4 or 7, a
// writer that makes progress is never compacted, started afresh by the
// ladder, or stopped as making no progress.
func TestTheGoOnsStartTheLoopLadderOver(t *testing.T) {
	for _, goOns := range []int{2, 4, 7} {
		g := newRig(t)
		g.launch("fx-repro")
		g.h.setSession(g.agent("fx-repro"), "sid-1")
		for i := 1; i <= MaxStagePrompts; i++ { // the first prompt and the 5 fix turns of the cap
			if _, err := g.ladderSend(i); err != nil {
				t.Fatalf("prompt %d: %v", i, err)
			}
		}
		_, before := g.ladderSteps()
		for k := range goOns {
			g.r.MadeProgress(g.task, g.attempt, "fx-repro") // the go-on's grant
			for j := 1; j <= 2; j++ {
				if _, err := g.ladderSend(100*(k+1) + j); err != nil {
					t.Fatalf("fix_go_on %d, go-on %d, turn %d: %v", goOns, k+1, j, err)
				}
			}
		}
		if compacts, starts := g.ladderSteps(); compacts != 0 || starts != before {
			t.Fatalf("fix_go_on %d: %d compactions and %d fresh starts during the go-ons, want none", goOns, compacts, starts-before)
		}
	}
}

// A fresh session the stage asks for (the writer's at its fix budget)
// starts its loop's ladder over too: the prompts after it are counted from
// it, so no compaction comes for the prompts before it.
func TestAFreshSessionStartsTheLoopsLadderOver(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	for i := 1; i <= MaxStagePrompts; i++ {
		if _, err := g.ladderSend(i); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	g.h.setStatus(g.agent("fx-repro"), "idle")
	if _, err := g.r.SendFresh(g.ctx, g.task, g.attempt, g.role("fx-repro"), "fix turn, fresh", false); err != nil {
		t.Fatal(err)
	}
	g.observeN(2)
	for i := 1; i < MaxStagePrompts; i++ {
		if _, err := g.ladderSend(100 + i); err != nil {
			t.Fatalf("prompt %d after the fresh session: %v", i, err)
		}
	}
	if compacts, _ := g.ladderSteps(); compacts != 0 {
		t.Fatalf("%d compactions after the fresh session, want none: its prompts count from it", compacts)
	}
}

// A fresh start your answer asks for (SendFresh) starts the role in a new
// session with the attempt's first prompt and the words after it, at once,
// and its recoveries start over: a loss after it gets all of them again.
func TestAFreshStartStartsTheRoleWithItsContextAndItsRecoveriesOver(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	g.loseAll("fx-repro")
	starts := len(g.h.startCalls())

	id, err := g.r.SendFresh(g.ctx, g.task, g.attempt, g.role("fx-repro"), "look at the seed data first", true)
	if err != nil || id == "" {
		t.Fatalf("SendFresh = %q, %v", id, err)
	}
	s := g.h.startCalls()
	if len(s) != starts+1 || strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume") {
		t.Fatalf("starts %+v, want one fresh session", s[starts:])
	}
	p := g.h.promptTexts()
	if last := p[len(p)-1]; !strings.HasPrefix(last, "reproduce the bug") || !strings.HasSuffix(last, "look at the seed data first") ||
		TurnText(last) != "look at the seed data first" {
		t.Fatalf("the fresh session's prompt %q, want the first prompt, then the words", last)
	}
	g.observeN(2)
	if lost := only(g.loseAll("fx-repro"), EventLost); len(lost) != 1 || len(g.h.startCalls()) != starts+1+MaxRecoveries {
		t.Fatalf("lost %+v after %d starts, want all %d recoveries again first", lost, len(g.h.startCalls())-starts-1, MaxRecoveries)
	}
}
