package runtime

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
)

// ladderSend sends fx-repro prompt i of the rig's attempt, a second after
// whatever came before it (your retry, say), its pane idle first.
func (g *rig) ladderSend(i int) (string, error) {
	g.t.Helper()
	g.clock.Add(time.Second)
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	text := "reproduce the bug"
	if i > 1 {
		text = "fix turn " + strconv.Itoa(i)
	}
	return g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), text)
}

// failedTurn sends fx-repro prompt i, ends its run and verifies it failed
// with why, as the factory's checks of a change they refuse do.
func (g *rig) failedTurn(i int, why string) {
	g.t.Helper()
	id, err := g.ladderSend(i)
	if err != nil {
		g.t.Fatalf("prompt %d: %v", i, err)
	}
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.observeN(2)
	if err := g.r.Verify(g.ctx, id, false, why); err != nil {
		g.t.Fatalf("verify prompt %d: %v", i, err)
	}
}

// ladderSteps are the steps the ladder took so far: the compactions run in
// the role's pane, and the sessions started.
func (g *rig) ladderSteps() (compacts, starts int) {
	g.t.Helper()
	for _, c := range g.h.runCalls() {
		if c.Command == agents.CompactCommand {
			compacts++
		}
	}
	return compacts, len(g.h.startCalls())
}

// Your retry goes on in the attempt that stopped, so it must lift a ladder
// that ran out there: the next prompt goes out as it is, and the ladder
// climbs again from it, to you again (EventNoProgress once more).
func TestARetryInTheSameAttemptStartsARunOutLadderOver(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	for i := 1; i <= 3*MaxStagePrompts; i++ {
		if _, err := g.ladderSend(i); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	if _, err := g.ladderSend(3*MaxStagePrompts + 1); !errors.Is(err, ErrNoProgress) {
		t.Fatalf("Send once the ladder ran out = %v, want ErrNoProgress", err)
	}
	if evs := only(g.observe(), EventNoProgress); len(evs) != 1 {
		t.Fatalf("events %+v, want no progress once", evs)
	}

	g.clock.Add(time.Minute)
	g.retried()
	compacts, starts := g.ladderSteps()
	before := len(g.h.promptTexts())
	if _, err := g.ladderSend(3*MaxStagePrompts + 1); err != nil {
		t.Fatalf("the prompt after your retry: %v, want it sent", err)
	}
	if c, s := g.ladderSteps(); len(g.h.promptTexts()) != before+1 || c != compacts || s != starts {
		t.Fatalf("after your retry %d prompts, %d compactions, %d starts; want %d, %d, %d: the prompt sent as it is", len(g.h.promptTexts()),
			c, s, before+1, compacts, starts)
	}

	for i := 3*MaxStagePrompts + 2; i <= 6*MaxStagePrompts; i++ {
		if _, err := g.ladderSend(i); err != nil {
			t.Fatalf("prompt %d after your retry: %v", i, err)
		}
	}
	if _, err := g.ladderSend(6*MaxStagePrompts + 1); !errors.Is(err, ErrNoProgress) {
		t.Fatalf("Send once the ladder ran out again = %v, want ErrNoProgress", err)
	}
	if evs := only(g.observe(), EventNoProgress); len(evs) != 1 {
		t.Fatalf("events %+v, want no progress told again", evs)
	}
}

// The live case of 2026-11-10: the same failing check after every turn up
// to the factory's cap (a compaction at the third). Each of 3 retries at
// the cap starts the ladder over, from the stage's prompts then, so the 2
// fix turns it gives the writer go out as they are: exactly 2 prompts, no
// ErrNoProgress, no fresh session, no compaction.
func TestARetryAtTheCapStartsTheLadderOverForItsFixTurns(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	const why = "precheck.md: jest: bin/jest exited 1"
	for i := 1; i <= MaxStagePrompts; i++ {
		g.failedTurn(i, why)
	}
	compacts, starts := g.ladderSteps()
	if compacts != 1 {
		t.Fatalf("%d compactions before your retry, want one after the third same failure", compacts)
	}

	const fixTurns = 2 // the factory's RetryFixTurns
	for retry := 1; retry <= 3; retry++ {
		g.clock.Add(time.Minute)
		g.retried()
		before := len(g.h.promptTexts())
		first := MaxStagePrompts + (retry-1)*fixTurns + 1
		for i := first; i < first+fixTurns; i++ {
			g.failedTurn(i, why) // fails the test on any refusal, ErrNoProgress among them
		}
		if n := len(g.h.promptTexts()) - before; n != fixTurns {
			t.Fatalf("retry %d let %d prompts out, want %d", retry, n, fixTurns)
		}
		if c, s := g.ladderSteps(); c != compacts || s != starts {
			t.Fatalf("retry %d's fix turns took %d compactions and %d starts, want none: the ladder starts over at your retry",
				retry, c-compacts, s-starts)
		}
	}
}

// A retry while the ladder took no step and the role is under the
// factory's cap (the task waited on you for another cause) leaves the
// ladder as it was: the seventh prompt of the stage still compacts.
func TestARetryBelowTheCapBeforeAnyStepLeavesTheLadderAsItWas(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	for i := 1; i <= 3; i++ {
		if _, err := g.ladderSend(i); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	g.clock.Add(time.Minute)
	g.retried()
	for i := 4; i <= MaxStagePrompts+1; i++ {
		if _, err := g.ladderSend(i); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	if !slices.ContainsFunc(g.h.runCalls(), func(r paneRunCall) bool { return r.Command == agents.CompactCommand }) {
		t.Fatalf("pane runs %+v, want /compact before the seventh prompt, your retry below the cap notwithstanding", g.h.runCalls())
	}
}
