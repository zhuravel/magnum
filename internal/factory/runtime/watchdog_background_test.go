package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
)

// A claude agent that ended its turn while work it started in the background
// still runs sits idle at its prompt, and the agents manager keeps its run
// open for that work. The watchdog's nudge used to tell such an agent that
// it seemed stuck, and the agent spent a turn answering that it was done and
// nothing hung. The nudge now names the work that holds the turn.

// backgroundUse is a transcript line of the tool use id, a Bash command run in
// the background (Claude Code's run_in_background): a task the agent does not
// hear back from until a task notification names it.
func backgroundUse(at time.Time, id string) transcriptLine {
	l := transcriptLine{Type: "assistant", Timestamp: at}
	l.Message = &struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{"assistant", []map[string]any{{"type": "tool_use", "id": id, "name": "Bash",
		"input": map[string]any{"command": "bin/rspec spec/models", "run_in_background": true}}}}
	return l
}

// notification is the user line Claude Code writes when the background work
// of tool use id finished.
func notification(at time.Time, id string) transcriptLine {
	l := userLine(at, "<task-notification>\n<task-id>bg-"+id+"</task-id>\n<tool-use-id>"+id+"</tool-use-id>\n"+
		"<status>completed</status>\n</task-notification>")
	l.TurnOrigin = ""
	return l
}

// startTurn launches fx-repro on conversation sid-1 and sends it first. Its
// transcript is the first prompt and lines (stamped from the send on; none
// when lines is nil: no file of that conversation), and the agent works on a
// command whose screen never changes.
func (g *rig) startTurn(first string, lines func(sent time.Time) []transcriptLine) {
	g.t.Helper()
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	sent := g.clock.Now()
	g.send("fx-repro", first)
	if lines != nil {
		g.writeTranscript("sid-1", append([]transcriptLine{userLine(sent, first)}, lines(sent)...)...)
	}
	g.observe() // at work on it
	g.h.screen(g.agent("fx-repro"), "Bash(bin/rspec)\nrunning rspec\n")
}

// transcriptFile is the path of the Claude transcript of conversation sid.
func (g *rig) transcriptFile(sid string) string {
	g.t.Helper()
	path, err := filepath.Glob(filepath.Join(g.claudeDir, "projects", "*", sid+".jsonl"))
	if err != nil || len(path) != 1 {
		g.t.Fatalf("the transcript of %s: %v, %v", sid, path, err)
	}
	return path[0]
}

// appendJSONL adds lines to the transcript at path (its directory made),
// which Claude Code only ever appends to.
func (g *rig) appendJSONL(path string, lines ...any) {
	g.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		g.t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		g.t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		data, err := json.Marshal(l)
		if err != nil {
			g.t.Fatal(err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			g.t.Fatal(err)
		}
	}
}

// appendTranscript adds lines to the Claude transcript of conversation sid.
func (g *rig) appendTranscript(sid string, lines ...transcriptLine) {
	g.t.Helper()
	for _, l := range lines {
		g.appendJSONL(g.transcriptFile(sid), l)
	}
}

// goIdle ends the agent's turn: it sits idle at its prompt. It returns the
// events of the passes that show it so: with background work in its
// transcript the manager holds the run open, so none of them ends it.
func (g *rig) goIdle() []Event {
	g.t.Helper()
	g.h.screen(g.agent("fx-repro"), "Reproducing.\n\n❯ \n")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	return g.observeN(4)
}

// idleTurn is startTurn for an agent that ended its turn and sits idle at its
// prompt (goIdle).
func (g *rig) idleTurn(first string, lines func(sent time.Time) []transcriptLine) []Event {
	g.t.Helper()
	g.startTurn(first, lines)
	return g.goIdle()
}

// stuckTurn lets the idle agent's turn pass its role's timeout and stay still
// for NoProgressAfter: the watchdog's first step, the nudge, comes at the end.
func (g *rig) stuckTurn() {
	g.t.Helper()
	g.clock.Add(g.role("fx-repro").Timeout.Duration - NoProgressAfter)
	g.watchdog()
	g.clock.Add(NoProgressAfter)
	g.watchdog()
}

// background starts n background tool uses, one a second.
func background(n int) func(sent time.Time) []transcriptLine {
	return func(sent time.Time) []transcriptLine {
		var out []transcriptLine
		for i := range n {
			out = append(out, backgroundUse(sent.Add(time.Duration(i+1)*time.Second), "toolu_"+string(rune('a'+i))))
		}
		return out
	}
}

func TestAnIdleAgentHeldByBackgroundWorkIsToldWhatHoldsItNotThatItSeemsStuck(t *testing.T) {
	g := newRig(t)
	if ended := only(g.idleTurn("reproduce the bug", background(2)), EventEnded); len(ended) != 0 {
		t.Fatalf("the idle agent's run ended with its background work running: %+v", ended)
	}

	g.stuckTurn()

	prompts := g.h.promptTexts()
	if len(prompts) != 2 {
		t.Fatalf("prompts %q, want the first and the watchdog's nudge", prompts)
	}
	nudge := prompts[1]
	if !strings.Contains(nudge, "2 background tasks") || !strings.Contains(nudge, "TaskStop") || strings.Contains(nudge, "seem stuck") {
		t.Fatalf("the nudge %q, want it to name the 2 background tasks that hold the turn, not tell the agent it seems stuck", nudge)
	}
	if nudge == nudgeText {
		t.Fatal("the agent got the nudge of a hung command")
	}
	// Tell lets the agent's background work go, so ending its turn ends magnum's wait: the text says it.
	for _, want := range []string{"wait for it", "before you end this turn", "once this turn ends, magnum no longer waits for that work",
		"end your turn", "If a command you run hangs, stop it and go on another way"} {
		if !strings.Contains(nudge, want) {
			t.Fatalf("the nudge %q lacks %q: it must tell the agent that once it ends its turn magnum no longer waits for its work", nudge, want)
		}
	}
	if strings.Contains(nudge, "kept your turn open") || strings.Contains(nudge, "waits for them") {
		t.Fatalf("the nudge %q promises a wait that ending the turn does not keep", nudge)
	}
}

func TestOneBackgroundTaskIsNamedInTheSingular(t *testing.T) {
	g := newRig(t)
	g.idleTurn("reproduce the bug", background(1))

	g.stuckTurn()

	nudge := g.h.promptTexts()[1]
	if !strings.Contains(nudge, "1 background task ") || strings.Contains(nudge, "1 background tasks") || strings.Contains(nudge, "wait for one") ||
		!strings.Contains(nudge, "no longer waits for that work") || !strings.Contains(nudge, "If a command you run hangs") {
		t.Fatalf("the nudge %q, want 1 background task, in the singular throughout, the end of magnum's wait and the hung command's clause", nudge)
	}
}

func TestTheLedgerKeepsTheBackgroundNudgeAsItWasSentWithItsHash(t *testing.T) {
	g := newRig(t)
	g.idleTurn("reproduce the bug", background(2))

	g.stuckTurn()

	sent, ledger := g.h.promptTexts()[1], g.ledger()
	if len(ledger) != 2 || !strings.Contains(sent, "2 background tasks") {
		t.Fatalf("ledger %+v, nudge %q, want the first prompt and the nudge that names the 2 background tasks", ledger, sent)
	}
	if e := ledger[1]; e.Text != sent || e.Hash != PromptHash(sent) || e.Delivered != registry.DeliveredYes {
		t.Fatalf("the nudge's entry %+v, want the text sent (%q), its hash and a delivery", e, sent)
	}
}

// What the watchdog types is no turn of yours, the background nudge as little
// as the other: the ledger's hash of what was sent tells them apart from
// what you type.
func TestTheBackgroundNudgeIsNoTypedTurn(t *testing.T) {
	g := newRig(t)
	g.idleTurn("reproduce the bug", background(2))
	g.stuckTurn()
	nudge := g.h.promptTexts()[1]
	if !strings.Contains(nudge, "background tasks") {
		t.Fatalf("the nudge %q does not name background work", nudge)
	}
	g.endRun("fx-repro") // the nudge let the agent's background work go: its turn ends
	g.clock.Add(2 * time.Minute)
	at := g.clock.Now()
	g.appendTranscript("sid-1", userLine(at.Add(-time.Minute), nudge)) // the nudge as Claude Code records the prompt it took

	got := typedTurns(append(g.youAtWork("fx-repro"), g.observeN(13)...))

	if len(got) != 0 {
		t.Fatalf("handed %+v: the watchdog's nudge counted as a turn you typed", got)
	}
}

// With nothing running in the background the turn is hung and the nudge says
// what it always said: a command that hangs is to be stopped.
func TestWithoutBackgroundWorkTheNudgeIsTheHungCommandsOne(t *testing.T) {
	for name, lines := range map[string]func(sent time.Time) []transcriptLine{
		"a transcript with no tool use": func(time.Time) []transcriptLine { return nil },
		"a foreground tool": func(sent time.Time) []transcriptLine {
			return []transcriptLine{assistantLine(sent.Add(time.Second), "Running the specs.")}
		},
		"work that finished": func(sent time.Time) []transcriptLine {
			return append(background(1)(sent), notification(sent.Add(time.Minute), "toolu_a"), assistantLine(sent.Add(2*time.Minute), "They passed."))
		},
		"no transcript to read": nil,
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t)
			g.startTurn("reproduce the bug", lines)

			g.stuckTurn()

			if prompts := g.h.promptTexts(); len(prompts) != 2 || prompts[1] != nudgeText {
				t.Fatalf("prompts %q, want the first and the nudge of a hung command", prompts)
			}
			if e := g.ledger()[1]; e.Text != nudgeText || e.Hash != PromptHash(nudgeText) {
				t.Fatalf("the nudge's entry %+v, want %q and its hash", e, nudgeText)
			}
		})
	}
}

// leadOnATeammate starts fx-repro's turn on conversation sid-1, has it spawn a
// teammate and go idle (its run held open by the teammate at work), and
// returns the function that makes the teammate write step i.
func (g *rig) leadOnATeammate() (step func(i int)) {
	g.t.Helper()
	const mate = "adrafts-0123456789abcdef"
	g.startTurn("build it", func(time.Time) []transcriptLine { return nil })
	lead := g.transcriptFile("sid-1")
	mateFile := filepath.Join(filepath.Dir(lead), "sid-1", "subagents", "agent-"+mate+".jsonl")
	at := g.clock.Now()
	g.appendJSONL(lead,
		rawEntry(at, "assistant", false, "", []map[string]any{{"type": "tool_use", "id": "toolu_mate1", "name": "Agent",
			"input": map[string]any{"description": "Draft", "name": "drafts", "prompt": "Build the drafts", "run_in_background": true}}}, nil),
		rawEntry(at.Add(time.Second), "user", false, "", []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_mate1", "content": "Spawned."}},
			map[string]any{"status": "teammate_spawned", "agentId": mate, "agent_id": mate, "name": "drafts"}),
	)
	g.appendJSONL(mateFile, rawEntry(at.Add(2*time.Second), "user", true, mate, `<teammate-message teammate_id="team-lead">Build it.</teammate-message>`, nil))
	if ended := only(g.goIdle(), EventEnded); len(ended) != 0 {
		g.t.Fatalf("the lead's run ended with its teammate at work: %+v", ended)
	}
	return func(i int) {
		now, id := g.clock.Now(), fmt.Sprintf("toolu_m%d", i)
		g.appendJSONL(mateFile,
			rawEntry(now, "assistant", true, mate, []map[string]any{{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "bin/rspec"}}}, nil),
			rawEntry(now.Add(time.Second), "user", true, mate, []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": "12 examples, 0 failures"}}, nil))
	}
}

// A lead whose own screen and transcript are still, past its role's timeout,
// is not stuck while a teammate it spawned works: the teammate's transcript
// grows. BackgroundProgress, a text of each teammate's transcript size, is part
// of what the watchdog reads, so the lead is not nudged on a teammate's
// progress, and is nudged (with the teammate counted) once that stops too.
func TestALeadIsNotNudgedWhileATeammatesTranscriptGrows(t *testing.T) {
	g := newRig(t)
	step := g.leadOnATeammate()

	g.clock.Add(g.role("fx-repro").Timeout.Duration - NoProgressAfter)
	g.watchdog()
	for i := range 4 { // 2 x NoProgressAfter, the teammate going on
		g.clock.Add(NoProgressAfter / 2)
		step(i)
		if evs := g.watchdog(); len(evs) != 0 {
			t.Fatalf("watchdog events %+v while the teammate worked", evs)
		}
	}
	if prompts := g.h.promptTexts(); len(prompts) != 1 {
		t.Fatalf("prompts %q: the lead was nudged while its teammate's transcript grew", prompts)
	}

	g.clock.Add(NoProgressAfter / 2) // the teammate wrote nothing more
	g.watchdog()
	if prompts := g.h.promptTexts(); len(prompts) != 1 {
		t.Fatalf("prompts %q: the lead was nudged before NoProgressAfter passed since its teammate's last progress", prompts)
	}
	g.clock.Add(NoProgressAfter / 2)
	g.watchdog()
	prompts := g.h.promptTexts()
	if len(prompts) != 2 || !strings.Contains(prompts[1], "1 background task ") {
		t.Fatalf("prompts %q, want the nudge naming the teammate once it stopped writing for NoProgressAfter", prompts)
	}
}

// A lead whose teammate stopped writing takes the later steps like any hung
// turn: the resume comes NoProgressAfter after the nudge, not a pass later
// (the reading taken at the nudge carries the teammates' part, so the next
// pass sees no movement in it).
func TestALeadWhoseTeammateStoppedWritingGoesOnToTheResumeAfterTheNudge(t *testing.T) {
	g := newRig(t)
	step := g.leadOnATeammate()
	g.clock.Add(g.role("fx-repro").Timeout.Duration - NoProgressAfter)
	g.watchdog()
	g.clock.Add(NoProgressAfter / 2)
	step(0) // the teammate's last words
	g.watchdog()
	g.clock.Add(NoProgressAfter)
	g.watchdog()
	if prompts := g.h.promptTexts(); len(prompts) != 2 || !strings.Contains(prompts[1], "1 background task ") {
		t.Fatalf("prompts %q, want the nudge naming the teammate", prompts)
	}
	starts := len(g.h.startCalls())

	g.clock.Add(NoProgressAfter)
	g.watchdog()

	s := g.h.startCalls()
	if len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts after the nudge %+v, want the conversation resumed NoProgressAfter after it", s[starts:])
	}
}

// rawEntry is a transcript entry as Claude Code writes it, for what
// transcriptLine has no field for: a teammate's (sidechain, agent id) and a
// tool use's result.
func rawEntry(at time.Time, typ string, sidechain bool, agent string, content any, result map[string]any) map[string]any {
	l := map[string]any{"type": typ, "timestamp": at, "isSidechain": sidechain, "message": map[string]any{"role": typ, "content": content}}
	if agent != "" {
		l["agentId"] = agent
	}
	if result != nil {
		l["toolUseResult"] = result
	}
	return l
}
