package runtime

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

func TestSendWritesTheLedgerBeforeThePrompt(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	text := "reproduce the bug; the token in the log was ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	var atPrompt []registry.LedgerEntry
	g.h.onPrompt = func(_ *fakeHerdr, _, _ string) {
		es, err := g.reg.LedgerOf(g.ctx, g.task.ID)
		if err != nil {
			t.Errorf("LedgerOf at the prompt: %v", err)
		}
		atPrompt = es
	}

	id := g.send("fx-repro", text)

	if len(atPrompt) != 1 {
		t.Fatalf("the ledger held %d entries when the prompt left, want 1", len(atPrompt))
	}
	e := atPrompt[0]
	if e.Delivered != registry.DeliveredUnknown || e.RunID != id || e.Role != "fx-repro" || e.AttemptID != g.attempt.ID ||
		e.Hash != PromptHash(text) {
		t.Fatalf("ledger entry at the prompt %+v, want unknown for run %s with the prompt's hash", e, id)
	}
	if strings.Contains(e.Text, "ghp_") || !strings.Contains(e.Text, "reproduce the bug") {
		t.Fatalf("ledger text %q, want the prompt scrubbed of its token", e.Text)
	}
	if got := g.ledger()[0].Delivered; got != registry.DeliveredYes {
		t.Fatalf("after the prompt the entry is %s, want yes", got)
	}
	if prompts := g.h.promptTexts(); !slices.Equal(prompts, []string{text}) {
		t.Fatalf("the agent got %q, want the prompt as written", prompts)
	}
}

func TestSendWaitsWhileTheOperatorIsInThePane(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	now := g.clock.Now()
	if err := g.st.UpdatePR(g.ctx, g.owner().ID, func(u *store.PRUpdate) { u.Set("human_active_at", now) }); err != nil {
		t.Fatal(err)
	}

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")

	if !errors.Is(err, ErrOperatorInPane) {
		t.Fatalf("Send = %v, want ErrOperatorInPane", err)
	}
	if n := len(g.h.promptTexts()); n != 0 {
		t.Fatalf("%d prompts typed into the operator's pane", n)
	}
	for _, e := range g.ledger() {
		if e.Delivered != registry.DeliveredNo {
			t.Fatalf("ledger entry %+v, want it settled as not sent", e)
		}
	}

	g.clock.Add(g.cfg.Factory.HumanCooldown.Duration + time.Second)
	g.send("fx-repro", "reproduce the bug")
}

// pendingPrompt leaves the state a daemon stopped between Submit and the
// ledger's verdict leaves: a run that never got past pending and an entry of
// unknown delivery, written at the clock's time; typed says herdr took the
// prompt before the daemon stopped.
func (g *rig) pendingPrompt(role, text string, typed bool) registry.LedgerEntry {
	g.t.Helper()
	run, err := g.r.mgr.NewRun(g.ctx, g.owner(), agents.Role(role), store.RunInitial, 1)
	if err != nil {
		g.t.Fatal(err)
	}
	e, err := g.reg.AppendLedger(g.ctx, registry.LedgerEntry{TaskID: g.task.ID, AttemptID: g.attempt.ID, Role: role,
		RunID: run.ID, Hash: PromptHash(text), Text: text})
	if err != nil {
		g.t.Fatal(err)
	}
	if typed {
		if _, err := g.h.AgentPrompt(g.ctx, g.agent(role), text, nil); err != nil {
			g.t.Fatal(err)
		}
	}
	return e
}

func TestAPromptOfUnknownDeliveryIsNotSentAgain(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	text := "reproduce the bug"
	e := g.pendingPrompt("fx-repro", text, true) // the session has no id yet: its transcript cannot be read

	for range 3 {
		got, err := g.r.Reconcile(g.ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		if got != registry.DeliveredUnknown {
			t.Fatalf("Reconcile with no transcript = %s, want unknown", got)
		}
		g.clock.Add(RedeliverAfter)
	}

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), text)
	if !errors.Is(err, ErrDeliveryUnknown) {
		t.Fatalf("Send = %v, want ErrDeliveryUnknown", err)
	}
	if prompts := g.h.promptTexts(); len(prompts) != 1 {
		t.Fatalf("prompts %q: a prompt of unknown delivery went out again", prompts)
	}
	if got := g.ledger()[0].Delivered; got != registry.DeliveredUnknown {
		t.Fatalf("the entry is %s, want still unknown", got)
	}
}

func TestAPromptIsSentAgainOnlyOnProof(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.observe()
	text := "reproduce the bug"
	e := g.pendingPrompt("fx-repro", text, false)
	sent := e.Sent
	g.writeTranscript("sid-1",
		userLine(sent.Add(-time.Hour), "an older prompt"),
		assistantLine(sent.Add(-time.Hour+time.Minute), "done"),
		assistantLine(sent.Add(30*time.Second), "the agent wrote after the prompt left"))

	for i, want := range []string{registry.DeliveredUnknown, registry.DeliveredUnknown, registry.DeliveredNo} {
		got, err := g.r.Reconcile(g.ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("read %d (%s after the first) = %s, want %s", i+1, time.Duration(i)*time.Minute, got, want)
		}
		g.clock.Add(time.Minute)
	}
	if got := g.ledger()[0].Delivered; got != registry.DeliveredNo {
		t.Fatalf("the entry is %s, want no", got)
	}

	g.send("fx-repro", text)

	if prompts := g.h.promptTexts(); !slices.Equal(prompts, []string{text}) {
		t.Fatalf("prompts %q, want exactly one resend", prompts)
	}
}

// No proof, no resend: a transcript that stops before the prompt was
// sent, a run that went past pending, a session prompted since all leave the
// entry unknown however long the reads go on.
func TestAPromptStaysUnknownWithoutEveryProof(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(g *rig, e registry.LedgerEntry)
	}{
		{"the transcript stops before the prompt", func(g *rig, e registry.LedgerEntry) {
			g.writeTranscript("sid-1", assistantLine(e.Sent.Add(-time.Minute), "done"))
		}},
		{"the run went past pending", func(g *rig, e registry.LedgerEntry) {
			g.writeTranscript("sid-1", assistantLine(e.Sent.Add(time.Minute), "working"))
			at := e.Sent
			if err := g.st.TransitionRun(g.ctx, e.RunID, nil, store.RunSubmitted, func(u *store.RunUpdate) { u.Set("submitted_at", at) }); err != nil {
				g.t.Fatal(err)
			}
		}},
		{"the session was prompted since", func(g *rig, e registry.LedgerEntry) {
			g.writeTranscript("sid-1", assistantLine(e.Sent.Add(time.Minute), "working"))
			at := e.Sent.Add(time.Second)
			if err := g.st.UpdateSession(g.ctx, g.session("fx-repro").ID, func(u *store.SessionUpdate) { u.Set("last_prompt_at", at) }); err != nil {
				g.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.launch("fx-repro")
			g.h.setSession(g.agent("fx-repro"), "sid-1")
			g.observe()
			e := g.pendingPrompt("fx-repro", "reproduce the bug", true)
			tc.after(g, e)
			for range 4 {
				if got, err := g.r.Reconcile(g.ctx, e); err != nil || got != registry.DeliveredUnknown {
					t.Fatalf("Reconcile = %s, %v, want unknown", got, err)
				}
				g.clock.Add(RedeliverAfter)
			}
		})
	}
}

func TestAPromptFoundInTheTranscriptIsDelivered(t *testing.T) {
	for _, tc := range []struct {
		name string
		line func(at time.Time, text string) transcriptLine
	}{
		{"typed", userLine},
		{"queued during a turn", func(at time.Time, text string) transcriptLine {
			return transcriptLine{Type: "queue-operation", Timestamp: at, Operation: "enqueue", Content: text}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.launch("fx-repro")
			g.h.setSession(g.agent("fx-repro"), "sid-1")
			g.observe()
			text := "reproduce the bug"
			e := g.pendingPrompt("fx-repro", text, true)
			g.writeTranscript("sid-1", tc.line(e.Sent.Add(2*time.Second), text+"\n"))

			got, err := g.r.Reconcile(g.ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			if got != registry.DeliveredYes {
				t.Fatalf("Reconcile = %s, want yes", got)
			}
			if got := g.ledger()[0].Delivered; got != registry.DeliveredYes {
				t.Fatalf("the entry is %s, want yes", got)
			}
			g.send("fx-repro", "write findings.md")
		})
	}
}

// A herdr error after the request left (the connection closed while herdr
// waited for the agent to start working) is no proof the prompt never
// arrived: the entry stays unknown and nothing is sent again. A dial that
// failed is: the prompt never left.
func TestAHerdrErrorAfterThePromptLeftIsNoProofItNeverArrived(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.promptErr = func(string) error {
		return fmt.Errorf("herdr agent.prompt: connection closed without a response: %w", io.EOF)
	}

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	if err == nil {
		t.Fatal("Send = nil, want herdr's error")
	}
	if got := g.ledger()[0].Delivered; got != registry.DeliveredUnknown {
		t.Fatalf("the entry is %s, want unknown", got)
	}
	g.h.promptErr = nil
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.Is(err, ErrDeliveryUnknown) {
		t.Fatalf("the next Send = %v, want ErrDeliveryUnknown", err)
	}
	if n := len(g.h.promptTexts()); n != 1 {
		t.Fatalf("%d prompts: the prompt went out again", n)
	}

	d := newRig(t)
	d.launch("fx-repro")
	d.h.promptErr = func(string) error {
		return fmt.Errorf("herdr agent.prompt: %w: dial unix: connect: connection refused", herdr.ErrUnavailable)
	}
	if _, err := d.r.Send(d.ctx, d.task, d.attempt, d.role("fx-repro"), "reproduce the bug"); err == nil {
		t.Fatal("Send = nil, want the dial's error")
	}
	if got := d.ledger()[0].Delivered; got != registry.DeliveredNo {
		t.Fatalf("after a failed dial the entry is %s, want no", got)
	}
	d.h.promptErr = nil
	d.send("fx-repro", "reproduce the bug")
}

// An unknown delivery never stalls a task in silence: past UnknownAfter it
// is EventDeliveryUnknown, once, for the operator to look at the pane, and
// their one action (LetGo) lets the role go on without a blind resend. A
// run the factory itself gives up (a fresh session) lets go of its prompts.
func TestAnUnknownDeliveryNeverStallsSilently(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.pendingPrompt("fx-repro", "reproduce the bug", true)

	g.clock.Add(UnknownAfter)
	evs := g.observeN(2)
	unknown := only(evs, EventDeliveryUnknown)
	if len(unknown) != 1 || unknown[0].Role != "fx-repro" ||
		unknown[0].Reason != "a prompt to fx-repro may or may not have arrived: look at its pane" {
		t.Fatalf("events %+v, want one EventDeliveryUnknown for fx-repro", evs)
	}
	g.clock.Add(time.Hour)
	if again := only(g.observeN(2), EventDeliveryUnknown); len(again) != 0 {
		t.Fatalf("the unknown delivery was reported again: %+v", again)
	}

	n, err := g.r.LetGo(g.ctx, g.task, "fx-repro")
	if err != nil || n != 1 {
		t.Fatalf("LetGo = %d, %v, want 1 entry", n, err)
	}
	if got := g.ledger()[0].Delivered; got != registry.DeliveredAbandoned {
		t.Fatalf("the entry is %s, want abandoned", got)
	}
	g.send("fx-repro", "go on")
	if prompts := g.h.promptTexts(); !slices.Equal(prompts, []string{"reproduce the bug", "go on"}) {
		t.Fatalf("prompts %q, want no blind resend", prompts)
	}

	f := newRig(t)
	f.launch("fx-repro")
	f.pendingPrompt("fx-repro", "reproduce the bug", true) // no conversation id: a lost pane starts fresh
	f.observeN(2)
	f.h.losePane(f.agent("fx-repro"))
	f.observe()
	if got := f.ledger()[0].Delivered; got != registry.DeliveredAbandoned {
		t.Fatalf("the prompt of the run the fresh session gave up is %s, want abandoned", got)
	}
	f.observeN(2)
	f.send("fx-repro", "go on")
}

// A prompt the operator let go stays let go: Reconcile answers with the
// stored verdict, from a stale copy of the entry too, and never turns it
// into a "no" a caller would resend on.
func TestReconcileNeverTurnsALetGoPromptIntoNo(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.observe()
	e := g.pendingPrompt("fx-repro", "reproduce the bug", false)
	g.writeTranscript("sid-1", assistantLine(e.Sent.Add(30*time.Second), "the agent wrote after the prompt left"))
	if got, err := g.r.Reconcile(g.ctx, e); err != nil || got != registry.DeliveredUnknown {
		t.Fatalf("the first read = %s, %v, want unknown", got, err)
	}
	if n, err := g.r.LetGo(g.ctx, g.task, "fx-repro"); err != nil || n != 1 {
		t.Fatalf("LetGo = %d, %v", n, err)
	}

	for i := range 3 {
		g.clock.Add(RedeliverAfter)
		if got, err := g.r.Reconcile(g.ctx, e); err != nil || got != registry.DeliveredAbandoned {
			t.Fatalf("read %d of the stale entry = %s, %v, want abandoned", i+2, got, err)
		}
	}
	stored := g.ledger()[0]
	if stored.Delivered != registry.DeliveredAbandoned {
		t.Fatalf("the entry is %s, want abandoned", stored.Delivered)
	}
	if got, err := g.r.Reconcile(g.ctx, stored); err != nil || got != registry.DeliveredAbandoned {
		t.Fatalf("Reconcile of the stored entry = %s, %v, want abandoned", got, err)
	}
}

// LetGo frees the runs it held: the role's pending run ends, the agents
// store lists it no more, and the task's agents park.
func TestLetGoFreesTheRunsItHeld(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	e := g.pendingPrompt("fx-repro", "reproduce the bug", false)
	if err := g.r.Park(g.ctx, g.task); !errors.Is(err, agents.ErrBusy) {
		t.Fatalf("Park with the run pending = %v, want ErrBusy", err)
	}

	if n, err := g.r.LetGo(g.ctx, g.task, "fx-repro"); err != nil || n != 1 {
		t.Fatalf("LetGo = %d, %v", n, err)
	}

	active, err := g.st.ActiveRuns(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(active, func(r store.Run) bool { return r.ID == e.RunID }) {
		t.Fatalf("the run of the prompt let go is still active: %s", g.storeRun(e.RunID).State)
	}
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park after LetGo = %v", err)
	}
}

// A prompt that did arrive although its Submit failed (the connection
// closed after herdr typed it) is watched once its transcript proves it:
// its run is submitted again, and its end comes as any run's.
func TestAPromptFoundAfterAFailedRunIsWatched(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.observe()
	g.h.promptErr = func(string) error {
		return fmt.Errorf("herdr agent.prompt: connection closed without a response: %w", io.EOF)
	}
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); err == nil {
		t.Fatal("Send = nil, want herdr's error")
	}
	g.h.promptErr = nil
	e := g.ledger()[0]
	if r := g.storeRun(e.RunID); r.State != store.RunFailed {
		t.Fatalf("the run is %s, want failed", r.State)
	}
	g.writeTranscript("sid-1", userLine(e.Sent.Add(2*time.Second), "reproduce the bug"))

	if got, err := g.r.Reconcile(g.ctx, e); err != nil || got != registry.DeliveredYes {
		t.Fatalf("Reconcile = %s, %v, want yes", got, err)
	}
	if r := g.storeRun(e.RunID); r.State != store.RunSubmitted {
		t.Fatalf("the delivered prompt's run is %s, want submitted", r.State)
	}
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	ended := only(g.observeN(2), EventEnded)
	if len(ended) != 1 || ended[0].RunID != e.RunID {
		t.Fatalf("events %+v, want the delivered run's end", ended)
	}
}

// A fresh runtime (a daemon restart) reads Codex-never from the task in the
// registry, whatever copy of the task it is given: a Codex role of a task
// Codex refused runs as its Claude fallback, and nothing starts on Codex.
func TestAFreshRuntimeReadsTheTasksCodexNever(t *testing.T) {
	g := newRig(t)
	stale := g.task
	if err := g.reg.TransitionTask(g.ctx, g.task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("codex_never", true) }); err != nil {
		t.Fatal(err)
	}
	g.restart()

	if err := g.r.Launch(g.ctx, stale, g.attempt, g.claimed(), g.roles("fx-cause")); err != nil {
		t.Fatal(err)
	}

	for _, s := range g.h.startCalls() {
		if s.Kind == agents.KindCodex {
			t.Fatalf("a Codex agent started on a task Codex refused: %+v", s)
		}
	}
	if s := g.h.startCalls(); len(s) != 1 || s[0].Name != g.agent("fx-cause-claude") {
		t.Fatalf("starts %+v, want the Claude fallback", s)
	}
}

// A watchdog nudge rides on the hung run it nudges. When herdr's answer to
// it is lost (a closed connection), its entry stays unknown; letting that
// entry go must leave the run, whose own prompt arrived, alive and watched.
func TestLetGoOfANudgeKeepsTheLiveRun(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	id := g.send("fx-repro", "reproduce the bug")
	g.observe()
	timeout := g.role("fx-repro").Timeout.Duration
	g.clock.Add(timeout - NoProgressAfter)
	g.watchdog()
	g.h.promptErr = func(text string) error {
		if strings.Contains(text, "stuck") {
			return fmt.Errorf("herdr agent.prompt: connection closed without a response: %w", io.EOF)
		}
		return nil
	}
	g.clock.Add(NoProgressAfter)
	g.watchdog() // step 1: the nudge, its answer lost
	g.h.promptErr = nil
	es := g.ledger()
	if len(es) != 2 || es[1].RunID != id || es[1].Delivered != registry.DeliveredUnknown {
		t.Fatalf("ledger %+v, want the nudge on run %s, unknown", es, id)
	}

	if n, err := g.r.LetGo(g.ctx, g.task, "fx-repro"); err != nil || n != 1 {
		t.Fatalf("LetGo = %d, %v", n, err)
	}

	if r := g.storeRun(id); r.State != store.RunWorking {
		t.Fatalf("the live run is %s after the nudge was let go, want working", r.State)
	}
	active, err := g.st.ActiveRuns(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(active, func(r store.Run) bool { return r.ID == id }) {
		t.Fatal("the live run left the active runs")
	}
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	if ended := only(g.observeN(2), EventEnded); len(ended) != 1 || ended[0].RunID != id {
		t.Fatalf("events %+v, want the live run's end", ended)
	}
}

// A run Submit left submitted (herdr timed out once the prompt was in) may
// be the agent's live turn: letting its unknown prompt go keeps it watched.
func TestLetGoKeepsARunHerdrMayHaveStarted(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.promptErr = func(string) error {
		return &herdr.Error{Method: "agent.prompt", Code: herdr.CodeTimeout, Message: "timed out"}
	}
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); err == nil {
		t.Fatal("Send = nil, want herdr's timeout")
	}
	g.h.promptErr = nil
	e := g.ledger()[0]
	if r := g.storeRun(e.RunID); r.State != store.RunSubmitted || e.Delivered != registry.DeliveredUnknown {
		t.Fatalf("run %s, entry %s: want submitted and unknown", r.State, e.Delivered)
	}

	if n, err := g.r.LetGo(g.ctx, g.task, "fx-repro"); err != nil || n != 1 {
		t.Fatalf("LetGo = %d, %v", n, err)
	}

	if r := g.storeRun(e.RunID); r.State != store.RunSubmitted {
		t.Fatalf("the run herdr may have started is %s after LetGo, want still submitted", r.State)
	}
}
