package runtime

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// toolResultLine is a transcript line of a tool's result, which Claude Code
// files as a user entry.
func toolResultLine(at time.Time, id, text string) transcriptLine {
	l := transcriptLine{Type: "user", Timestamp: at}
	l.Message = &struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{"user", []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": text}}}
	return l
}

// youActive sets the owner row's human_active_at to at: herdr showed you in
// one of the task's panes then.
func (g *rig) youActive(at time.Time) {
	g.t.Helper()
	if err := g.st.UpdatePR(g.ctx, g.owner().ID, func(u *store.PRUpdate) { u.Set("human_active_at", at) }); err != nil {
		g.t.Fatal(err)
	}
}

// typedTurns are the turns the events of kind EventTyped hand over, in order.
func typedTurns(evs []Event) []Turn {
	var out []Turn
	for _, ev := range only(evs, EventTyped) {
		out = append(out, ev.Turns...)
	}
	return out
}

// claudeRole is a factory role on Claude, with a session to type into: its
// transcript is conversation sid's.
func (g *rig) claudeRole(name, sid string) {
	g.t.Helper()
	g.launch(name)
	g.h.setSession(g.agent(name), sid)
	g.observe()
}

// A turn you typed into a pane while herdr showed you there is handed to the
// engine with the proof of its origin, the stage, the agent's state when it
// arrived (at work on the factory's prompt, or idle) and what the agent said
// in its two turns before it; the factory's own prompt and a tool's result
// are no such turn. A read that finds nothing new hands nothing.
func TestATurnTheOperatorTypedIsHandedWithItsProof(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	sent := g.clock.Now()
	g.send("fx-repro", "reproduce the bug")
	g.observe() // at work on it
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	if ended := only(g.observeN(2), EventEnded); len(ended) != 1 {
		t.Fatalf("the run did not end: %+v", ended)
	}
	during, typed := sent.Add(5*time.Second), g.clock.Now().Add(time.Second)
	g.writeTranscript("sid-1",
		userLine(sent, "reproduce the bug"),
		assistantLine(sent.Add(2*time.Second), "Reproducing."),
		userLine(during, "look at the refund job"),
		toolResultLine(during.Add(time.Second), "toolu_1", "class Refund"),
		assistantLine(during.Add(3*time.Second), "It reproduces on the refund path."),
		userLine(typed, "only the refund path"),
	)
	g.youActive(typed.Add(5 * time.Second))
	g.clock.Add(time.Minute)

	evs := only(g.observe(), EventTyped)

	if len(evs) != 1 || evs[0].TaskID != g.task.ID || evs[0].Role != "fx-repro" {
		t.Fatalf("typed events %+v, want one of fx-repro's", evs)
	}
	want := []Turn{
		{At: during, Text: "look at the refund job", Before: "Reproducing.", Stage: "investigate", AgentState: string(herdr.StatusWorking),
			Origin: registry.OriginHerdr},
		{At: typed, Text: "only the refund path", Before: "Reproducing.\n\nIt reproduces on the refund path.", Stage: "investigate",
			AgentState: string(herdr.StatusIdle), Origin: registry.OriginHerdr},
	}
	if got := evs[0].Turns; !slices.EqualFunc(got, want, sameTurn) {
		t.Fatalf("turns %+v\nwant  %+v", got, want)
	}

	g.clock.Add(time.Minute)
	if again := typedTurns(g.observe()); len(again) != 0 {
		t.Fatalf("a read of an unchanged transcript handed %+v", again)
	}
}

func sameTurn(a, b Turn) bool {
	return a.At.Equal(b.At) && a.Text == b.Text && a.Before == b.Before && a.Stage == b.Stage && a.AgentState == b.AgentState &&
		a.Origin == b.Origin
}

// The prompts the factory sent (by the ledger's hash, the watchdog's nudge
// too) and the after-deny prompt the agents layer types for the session's
// kind are no turn of yours, and neither is a tool's result, whoever herdr
// showed in the pane.
func TestTheFactorysOwnPromptIsNoTypedTurn(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	sent := g.clock.Now()
	g.send("fx-repro", "reproduce the bug")
	if _, err := g.reg.AppendLedger(g.ctx, registry.LedgerEntry{TaskID: g.task.ID, AttemptID: g.attempt.ID, Role: "fx-repro",
		Hash: PromptHash(nudgeText), Text: nudgeText}); err != nil {
		t.Fatal(err)
	}
	g.writeTranscript("sid-1",
		userLine(sent, "  reproduce the bug\r\n"), // the line ends and the space a transcript may add
		toolResultLine(sent.Add(time.Second), "toolu_1", "class Refund"),
		userLine(sent.Add(2*time.Second), nudgeText),
		userLine(sent.Add(3*time.Second), config.DefaultAfterDenyPrompt),
	)
	g.youActive(sent.Add(time.Second))
	g.clock.Add(time.Minute)

	if got := typedTurns(g.observeN(13)); len(got) != 0 {
		t.Fatalf("the factory's own prompts were handed as typed: %+v", got)
	}
}

// A turn nothing proves you typed (herdr showed nobody in the task's panes
// within two minutes of it) is handed with the origin unknown, for
// evaluation only. While it is younger than those two minutes it waits for a
// later read, as herdr may not have shown the agent at work on it yet.
func TestATurnWithoutProofIsHandedAsUnknown(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	typed := g.clock.Now()
	g.writeTranscript("sid-1",
		userLine(typed.Add(-10*time.Minute), "an old line"),
		assistantLine(typed.Add(-9*time.Minute), "Noted."),
		userLine(typed, "only the refund path"),
	)
	g.youActive(typed.Add(-5 * time.Minute)) // you were in a pane, but long before
	g.clock.Add(time.Minute)

	got := typedTurns(g.observe())
	if len(got) != 1 || got[0].Text != "an old line" || got[0].Origin != registry.OriginUnknown {
		t.Fatalf("first read handed %+v, want only the old line, of unknown origin", got)
	}

	g.clock.Add(time.Minute)
	got = typedTurns(g.observe())
	if len(got) != 2 || got[1].Text != "only the refund path" || got[1].Origin != registry.OriginUnknown || got[1].Before != "Noted." {
		t.Fatalf("a read two minutes on handed %+v, want the turn, of unknown origin", got)
	}
}

// A restart forgets what the runtime read: its first read hands every turn
// of the transcript again, those it handed before and one typed while the
// daemon was down, for the engine to record each once.
func TestATypedTurnIsHandedAgainAfterARestart(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	first := g.clock.Now()
	g.writeTranscript("sid-1", userLine(first, "only the refund path"))
	g.youActive(first)
	g.clock.Add(time.Minute)
	if got := typedTurns(g.observe()); len(got) != 1 {
		t.Fatalf("before the restart: %+v, want one turn", got)
	}

	down := g.clock.Now()
	g.writeTranscript("sid-1", userLine(first, "only the refund path"), userLine(down, "and skip archived points"))
	g.youActive(down)
	g.restart()
	g.clock.Add(time.Minute)

	got := typedTurns(g.observe())
	if len(got) != 2 || got[0].Text != "only the refund path" || got[1].Text != "and skip archived points" ||
		got[1].Origin != registry.OriginHerdr {
		t.Fatalf("after the restart: %+v, want both turns", got)
	}
}

// A pane that ends between two reads (its task parked) gets one last read:
// what you typed into it just before is handed, and nothing waits for a
// later read that will never come.
func TestATurnTypedJustBeforeItsPaneEndsIsHanded(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	typed := g.clock.Now()
	g.writeTranscript("sid-1", userLine(typed, "stop here, I take it from there"))
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park: %v", err)
	}
	g.clock.Add(time.Minute)

	got := typedTurns(g.observe())
	if len(got) != 1 || got[0].Text != "stop here, I take it from there" || got[0].Origin != registry.OriginUnknown {
		t.Fatalf("after the pane ended: %+v, want the turn typed before, of unknown origin", got)
	}
	g.clock.Add(time.Minute)
	if again := typedTurns(g.observe()); len(again) != 0 {
		t.Fatalf("an ended pane was read again: %+v", again)
	}
}

// A pass reads a transcript once: the turns it hands and the proof that a
// prompt of unknown delivery never arrived (Reconcile) come from the same
// read.
func TestAPassReadsATranscriptOnce(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	e := g.pendingPrompt("fx-repro", "reproduce the bug", false)
	typed := g.clock.Now().Add(time.Second)
	g.writeTranscript("sid-1", userLine(typed, "only the refund path"))
	g.youActive(typed)
	g.clock.Add(time.Minute)
	before := g.turnReadCount()

	got := typedTurns(g.observe())

	if len(got) != 1 {
		t.Fatalf("typed turns %+v, want one", got)
	}
	if !g.r.Proving(e.ID) {
		t.Fatal("the pass did not read the transcript for the unknown prompt's proof")
	}
	if n := g.turnReadCount() - before; n != 1 {
		t.Fatalf("the pass read the transcript's turns %d times, want once", n)
	}
}
