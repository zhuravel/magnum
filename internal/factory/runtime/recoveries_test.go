package runtime

import (
	"strings"
	"testing"
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

	g.retried()
	g.restart()
	starts := len(g.h.startCalls())
	if err := g.r.RecoverLost(g.ctx, g.task, g.attempt, g.role("fx-repro")); err != nil {
		t.Fatal(err)
	}
	g.afterWait(1)
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
	if _, err := g.r.SendFresh(g.ctx, g.task, g.attempt, g.role("fx-repro"), "fix turn, fresh"); err != nil {
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

	id, err := g.r.SendFresh(g.ctx, g.task, g.attempt, g.role("fx-repro"), "look at the seed data first")
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
