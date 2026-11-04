package runtime

import (
	"errors"
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

// recordLine is a user entry Claude Code writes with no origin: a slash
// command's record, a command's output, a compaction's summary.
func recordLine(at time.Time, text string) transcriptLine {
	l := userLine(at, text)
	l.TurnOrigin = ""
	return l
}

// commandLine is the record Claude Code files for slash command name typed
// with args.
func commandLine(at time.Time, name, args string) transcriptLine {
	return recordLine(at, "<command-name>"+name+"</command-name>\n<command-message>"+name[1:]+"</command-message>\n<command-args>"+
		args+"</command-args>")
}

// youAtWork shows herdr the agent of the role named name at work on no
// prompt of the factory's for one observe pass, as when you typed into its
// pane (the agents layer's ObsHumanActive), and returns that pass's events.
func (g *rig) youAtWork(name string) []Event {
	g.t.Helper()
	g.h.setStatus(g.agent(name), herdr.StatusWorking)
	evs := g.observe()
	g.h.setStatus(g.agent(name), herdr.StatusIdle)
	if at := g.owner().HumanActiveAt; at == nil || !at.Equal(g.clock.Now()) {
		g.t.Fatalf("herdr did not show you in %s's pane: human_active_at %v", name, at)
	}
	return evs
}

// setTurnErr makes the next read of a transcript's user turns fail with err.
func (g *rig) setTurnErr(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.turnErr = err
}

// typedTurns are the turns the events of kind EventTyped hand over, in order.
func typedTurns(evs []Event) []Turn {
	var out []Turn
	for _, ev := range only(evs, EventTyped) {
		out = append(out, ev.Turns...)
	}
	return out
}

// turnsOf are the turns the EventTyped events of role hand over.
func turnsOf(evs []Event, role string) []Turn {
	var out []Turn
	for _, ev := range only(evs, EventTyped) {
		if ev.Role == role {
			out = append(out, ev.Turns...)
		}
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

// endRun ends the run of the role named name: its agent goes idle for two
// passes.
func (g *rig) endRun(name string) {
	g.t.Helper()
	g.observe() // at work on it
	g.h.setStatus(g.agent(name), herdr.StatusIdle)
	if ended := only(g.observeN(2), EventEnded); len(ended) != 1 {
		g.t.Fatalf("the run of %s did not end: %+v", name, ended)
	}
}

// A turn you typed into a pane while herdr showed you there is handed to the
// engine with the proof of its origin, the stage, the agent's state when it
// arrived (at work on the factory's prompt, or idle) and what the agent said
// in its two turns before it; the factory's own prompt and a tool's result
// are no such turn, and a turn typed while the factory's run was in flight,
// with nobody shown in the pane, has no proof. A read that finds nothing new
// hands nothing.
func TestATurnTheOperatorTypedIsHandedWithItsProof(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	sent := g.clock.Now()
	g.send("fx-repro", "reproduce the bug")
	g.endRun("fx-repro")
	g.clock.Add(2 * time.Minute)
	during, typed := sent.Add(5*time.Second), g.clock.Now()
	g.writeTranscript("sid-1",
		userLine(sent, "reproduce the bug"),
		assistantLine(sent.Add(2*time.Second), "Reproducing."),
		userLine(during, "look at the refund job"),
		toolResultLine(during.Add(time.Second), "toolu_1", "class Refund"),
		assistantLine(during.Add(3*time.Second), "It reproduces on the refund path."),
		userLine(typed, "only the refund path"),
	)

	evs := only(g.youAtWork("fx-repro"), EventTyped)

	if len(evs) != 1 || evs[0].TaskID != g.task.ID || evs[0].Role != "fx-repro" {
		t.Fatalf("typed events %+v, want one of fx-repro's", evs)
	}
	want := []Turn{
		{At: during, Text: "look at the refund job", Before: "Reproducing.", Stage: "investigate", AgentState: string(herdr.StatusWorking),
			Origin: registry.OriginUnknown},
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

// What the factory and the agents layer type into a pane is no turn of
// yours: the prompts of the ledger (by their hash, the watchdog's nudge
// too), the after-deny prompt of the session's kind and the records of the
// slash commands they type (/model, /compact, /exit). Neither is what
// Claude Code writes itself (a command's output, a compaction's summary) or
// a tool's result, though herdr showed you in the pane within two minutes of
// them. A slash command of yours the factory never types stays yours.
func TestTheFactorysOwnPromptIsNoTypedTurn(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	sent := g.clock.Now()
	g.send("fx-repro", "reproduce the bug")
	if _, err := g.reg.AppendLedger(g.ctx, registry.LedgerEntry{TaskID: g.task.ID, AttemptID: g.attempt.ID, Role: "fx-repro",
		Hash: PromptHash(nudgeText), Text: nudgeText}); err != nil {
		t.Fatal(err)
	}
	g.endRun("fx-repro")
	g.clock.Add(2 * time.Minute)
	at := g.clock.Now()
	summary := recordLine(at.Add(-30*time.Second), "This session is being continued from a previous conversation that ran out of context.")
	summary.IsCompactSummary = true
	g.writeTranscript("sid-1",
		userLine(sent, "  reproduce the bug\r\n"), // the line ends and the space a transcript may add
		toolResultLine(sent.Add(time.Second), "toolu_1", "class Refund"),
		userLine(sent.Add(2*time.Second), nudgeText),
		userLine(sent.Add(3*time.Second), config.DefaultAfterDenyPrompt),
		commandLine(at.Add(-50*time.Second), "/model", "opus"),
		recordLine(at.Add(-49*time.Second), "<local-command-stdout>Set model to opus</local-command-stdout>"),
		recordLine(at.Add(-48*time.Second), "<local-command-stderr>the model is busy</local-command-stderr>"),
		commandLine(at.Add(-40*time.Second), "/compact", ""),
		summary,
		recordLine(at.Add(-20*time.Second), "<bash-stdout>nothing to commit</bash-stdout><bash-stderr></bash-stderr>"),
		commandLine(at.Add(-15*time.Second), "/exit", ""),
		commandLine(at.Add(-10*time.Second), "/cost", ""),
	)

	got := typedTurns(append(g.youAtWork("fx-repro"), g.observeN(13)...))

	if len(got) != 1 || got[0].Text != "<command-name>/cost</command-name>\n<command-message>cost</command-message>\n<command-args></command-args>" ||
		got[0].Origin != registry.OriginHerdr {
		t.Fatalf("handed %+v, want only your /cost: the factory's own prompts and commands and Claude Code's records are no turn of yours", got)
	}
}

// A turn nothing proves you typed (herdr showed nobody in its pane within
// two minutes of it) is handed with the origin unknown, for evaluation only.
// While it is younger than those two minutes it waits for a later read, as
// herdr may not have shown the agent at work on it yet.
func TestATurnWithoutProofIsHandedAsUnknown(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	typed := g.clock.Now()
	g.writeTranscript("sid-1",
		userLine(typed.Add(-10*time.Minute), "an old line"),
		assistantLine(typed.Add(-9*time.Minute), "Noted."),
		userLine(typed, "only the refund path"),
	)
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

// The proof is the turn's own pane's: herdr showing you at work in a
// sibling pane of the task proves nothing typed in this one.
func TestYouInASiblingPaneProveNoTurn(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro", "fx-history")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.h.setSession(g.agent("fx-history"), "sid-2")
	g.observe()
	g.clock.Add(2 * time.Minute)
	typed := g.clock.Now()
	g.writeTranscript("sid-1", userLine(typed, "only the refund path"))
	g.writeTranscript("sid-2", userLine(typed, "read the ledger first"))

	first := g.youAtWork("fx-history")
	if got := turnsOf(first, "fx-history"); len(got) != 1 || got[0].Origin != registry.OriginHerdr {
		t.Fatalf("fx-history's turns %+v, want its own, proven", got)
	}
	if got := turnsOf(first, "fx-repro"); len(got) != 0 {
		t.Fatalf("fx-repro's turns %+v, want none while its proof may come", got)
	}
	g.clock.Add(2 * time.Minute)
	if got := turnsOf(g.observe(), "fx-repro"); len(got) != 1 || got[0].Origin != registry.OriginUnknown {
		t.Fatalf("fx-repro's turns %+v, want its turn of unknown origin: you were in the sibling pane", got)
	}
}

// A restart forgets what the runtime read: its first read hands every turn
// of the transcript again, those it handed before and one typed while the
// daemon was down, for the engine to record each once. What herdr showed of
// you before the restart is forgotten too, so that turn has no proof.
func TestATypedTurnIsHandedAgainAfterARestart(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	g.clock.Add(2 * time.Minute)
	first := g.clock.Now()
	g.writeTranscript("sid-1", userLine(first, "only the refund path"))
	if got := typedTurns(g.youAtWork("fx-repro")); len(got) != 1 || got[0].Origin != registry.OriginHerdr {
		t.Fatalf("before the restart: %+v, want one turn, proven", got)
	}

	down := g.clock.Now()
	g.writeTranscript("sid-1", userLine(first, "only the refund path"), userLine(down, "and skip archived points"))
	g.restart()
	g.clock.Add(2 * time.Minute) // past the wait for a proof that no longer comes

	got := typedTurns(g.observe())
	if len(got) != 2 || got[0].Text != "only the refund path" || got[1].Text != "and skip archived points" ||
		got[1].Origin != registry.OriginUnknown {
		t.Fatalf("after the restart: %+v, want both turns, the new one unproven", got)
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

// A pane that started and ended between two reads, which no read saw live,
// gets its last read all the same.
func TestATurnInAPaneNoReadSawLiveIsHanded(t *testing.T) {
	g := newRig(t)
	g.observe() // a read, before the pane
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.observe()
	g.writeTranscript("sid-1", userLine(g.clock.Now(), "keep the old column"))
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park: %v", err)
	}
	g.clock.Add(time.Minute)

	if got := typedTurns(g.observe()); len(got) != 1 || got[0].Text != "keep the old column" {
		t.Fatalf("handed %+v, want the turn of the pane no read saw live", got)
	}
}

// A pane lost while the daemon was down gets its last read from the next
// daemon's first read.
func TestATurnInAPaneLostWhileTheDaemonWasDownIsHanded(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	g.writeTranscript("sid-1", userLine(g.clock.Now(), "use the replica"))
	s := g.session("fx-repro")
	g.restart()
	if err := g.st.TransitionSession(g.ctx, s.ID, []string{store.SessionLive}, store.SessionLost, nil); err != nil {
		t.Fatal(err)
	}
	g.clock.Add(time.Minute)

	if got := typedTurns(g.observe()); len(got) != 1 || got[0].Text != "use the replica" {
		t.Fatalf("handed %+v, want the turn of the pane lost while the daemon was down", got)
	}
}

// A last read that fails is tried again at the next read, not forgotten.
func TestAFailedLastReadIsTriedAgain(t *testing.T) {
	g := newRig(t)
	g.claudeRole("fx-repro", "sid-1")
	g.writeTranscript("sid-1", userLine(g.clock.Now(), "stop here"))
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park: %v", err)
	}
	g.setTurnErr(errors.New("read the transcript: interrupted"))
	g.clock.Add(time.Minute)
	if got := typedTurns(g.observe()); len(got) != 0 {
		t.Fatalf("a failed read handed %+v", got)
	}

	g.clock.Add(time.Minute)
	if got := typedTurns(g.observe()); len(got) != 1 || got[0].Text != "stop here" {
		t.Fatalf("the read after the failed one handed %+v, want the turn", got)
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
	g.writeTranscript("sid-1", userLine(typed.Add(-10*time.Minute), "an old line"), userLine(typed, "only the refund path"))
	g.clock.Add(time.Minute)
	before := g.turnReadCount()

	got := typedTurns(g.observe())

	if len(got) != 1 {
		t.Fatalf("typed turns %+v, want the old one", got)
	}
	if !g.r.Proving(e.ID) {
		t.Fatal("the pass did not read the transcript for the unknown prompt's proof")
	}
	if n := g.turnReadCount() - before; n != 1 {
		t.Fatalf("the pass read the transcript's turns %d times, want once", n)
	}
}
