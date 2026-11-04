package runtime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// launchCut leaves the state a daemon stopped inside Launch leaves between
// the workspace's creation and its first row (EnsureWorkspace creates the
// workspace, then records its root pane): the attempt launching, labelled,
// and herdr's workspace with its root pane in the copy, which no row names.
func (g *rig) launchCut(t registry.Task, a registry.Attempt) herdr.WorkspaceCreated {
	g.t.Helper()
	label := WorkspaceLabel(t, a)
	if err := g.reg.TransitionAttempt(g.ctx, a.ID, []string{registry.AttemptLaunching}, "",
		func(u *registry.AttemptUpdate) { u.Set("workspace_label", label) }); err != nil {
		g.t.Fatal(err)
	}
	ws, err := g.h.WorkspaceCreate(g.ctx, herdr.WorkspaceCreateOptions{Cwd: g.copy, Label: label})
	if err != nil {
		g.t.Fatal(err)
	}
	return ws
}

// workspaceIDs are the ids of herdr's workspaces.
func (g *rig) workspaceIDs() []string {
	g.h.mu.Lock()
	defer g.h.mu.Unlock()
	var out []string
	for _, w := range g.h.workspaces {
		out = append(out, w.ID)
	}
	return out
}

// A daemon that stopped between a workspace's creation and its row finds
// the workspace after the restart: its attempt still launches, so the
// attempt's launch takes it over (its root pane serves the root role, a
// split the next) instead of making a second one.
func TestRestartDuringLaunchAdoptsTheWorkspace(t *testing.T) {
	g := newRig(t)
	ws := g.launchCut(g.task, g.attempt)

	g.restart()
	orphans, err := g.r.SettleWorkspaces(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Orphan{Workspace: ws.Workspace.ID, Label: WorkspaceLabel(g.task, g.attempt), Attempt: g.attempt.ID, TaskID: g.task.ID,
		Adopted: true, Why: "its attempt's launch takes it over"}
	if !slices.Equal(orphans, []Orphan{want}) {
		t.Fatalf("orphans %+v, want %+v", orphans, want)
	}
	g.launch("fx-repro", "fx-history")

	if len(g.h.creates) != 1 {
		t.Fatalf("%d workspaces created, want none after the one the crash left", len(g.h.creates))
	}
	repro, history := g.session("fx-repro"), g.session("fx-history")
	if store.Deref(repro.HerdrWorkspaceID) != ws.Workspace.ID || store.Deref(repro.HerdrPaneID) != ws.RootPane.ID {
		t.Fatalf("fx-repro sits in pane %s of %s, want the adopted root pane %s of %s", store.Deref(repro.HerdrPaneID),
			store.Deref(repro.HerdrWorkspaceID), ws.RootPane.ID, ws.Workspace.ID)
	}
	if store.Deref(history.HerdrWorkspaceID) != ws.Workspace.ID || store.Deref(history.HerdrPaneID) == ws.RootPane.ID {
		t.Fatalf("fx-history sits in pane %s of %s, want a split of %s", store.Deref(history.HerdrPaneID),
			store.Deref(history.HerdrWorkspaceID), ws.Workspace.ID)
	}
	if starts := g.h.startCalls(); len(starts) != 2 || starts[0].PaneID != ws.RootPane.ID {
		t.Fatalf("agents started %+v, want fx-repro's in the adopted pane first", starts)
	}
	if a, err := g.reg.AttemptByID(g.ctx, g.attempt.ID); err != nil || a.State != registry.AttemptRunning {
		t.Fatalf("attempt %+v (%v), want running", a, err)
	}
}

// What no attempt goes on with is closed: a workspace no row names whose
// attempt ended or that names no attempt the registry has. A workspace
// with an agent in it (Recover's to rebind), one a row names, and one whose
// label names no attempt ("factory notes" is no attempt's label) are left
// alone.
func TestSettleWorkspacesClosesWhatNoAttemptGoesOnWith(t *testing.T) {
	g := newRig(t)
	ended := g.launchCut(g.task, g.attempt)
	if err := g.reg.TransitionAttempt(g.ctx, g.attempt.ID, nil, registry.AttemptFailed, nil); err != nil {
		t.Fatal(err)
	}
	unknown, err := g.h.WorkspaceCreate(g.ctx, herdr.WorkspaceCreateOptions{Cwd: g.copy, Label: "factory PR-12399 t99-investigate-1"})
	if err != nil {
		t.Fatal(err)
	}
	other := g.newTask("PR-12346")
	withAgent := g.launchCut(other, g.begin(other, "investigate"))
	if _, err := g.h.AgentStart(g.ctx, herdr.AgentStartOptions{PaneID: withAgent.RootPane.ID, Name: "someone", Kind: "claude"}); err != nil {
		t.Fatal(err)
	}
	third := g.newTask("PR-12347")
	named := g.launchCut(third, g.begin(third, "investigate"))
	pr, err := g.r.Owner(g.ctx, third)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.st.CreateSession(g.ctx, store.Session{PRID: pr.ID, Role: "fx-repro", HerdrPaneID: &named.RootPane.ID,
		HerdrWorkspaceID: &named.Workspace.ID, State: store.SessionParked}); err != nil {
		t.Fatal(err)
	}
	yours, err := g.h.WorkspaceCreate(g.ctx, herdr.WorkspaceCreateOptions{Cwd: g.copy, Label: "factory notes"})
	if err != nil {
		t.Fatal(err)
	}

	g.restart()
	orphans, err := g.r.SettleWorkspaces(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Orphan{
		{Workspace: ended.Workspace.ID, Label: WorkspaceLabel(g.task, g.attempt), Attempt: g.attempt.ID, TaskID: g.task.ID,
			Why: "its attempt " + g.attempt.ID + " is failed"},
		{Workspace: unknown.Workspace.ID, Label: "factory PR-12399 t99-investigate-1", Attempt: "t99-investigate-1",
			Why: "the registry has no attempt t99-investigate-1"},
	}
	if !slices.Equal(orphans, want) {
		t.Fatalf("orphans %+v, want %+v", orphans, want)
	}
	if got := g.workspaceIDs(); !slices.Equal(got, []string{withAgent.Workspace.ID, named.Workspace.ID, yours.Workspace.ID}) {
		t.Fatalf("workspaces %v, want the one with an agent, the named one and yours", got)
	}
}

// Each task's attempt has a label of its own, which ends in the attempt's
// id: a task without a key too. After a restart each attempt's launch
// adopts its own workspace only.
func TestWorkspaceLabelsAreUniquePerTaskAndAttempt(t *testing.T) {
	g := newRig(t)
	keyless, err := g.reg.CreateTask(g.ctx, registry.Task{Prompt: "credits vanish on copy", Kind: "bug", Repo: "talkable/talkable", State: registry.TaskActive,
		Stage: "investigate"})
	if err != nil {
		t.Fatal(err)
	}
	second := g.begin(g.task, "judge")
	theirs := g.begin(keyless, "investigate")
	labels := map[string]registry.Attempt{}
	for _, c := range []struct {
		task registry.Task
		a    registry.Attempt
	}{{g.task, g.attempt}, {g.task, second}, {keyless, theirs}} {
		l := WorkspaceLabel(c.task, c.a)
		if _, dup := labels[l]; dup {
			t.Fatalf("label %q twice", l)
		}
		if !strings.HasPrefix(l, "factory ") || !strings.HasSuffix(l, " "+c.a.ID) {
			t.Fatalf("label %q, want factory and the task's key, then the attempt %s", l, c.a.ID)
		}
		labels[l] = c.a
	}
	if l := WorkspaceLabel(keyless, theirs); l != "factory task 2 "+theirs.ID {
		t.Fatalf("a keyless task's label %q", l)
	}

	mine := g.launchCut(g.task, g.attempt)
	other := g.launchCut(keyless, theirs)
	g.restart()
	orphans, err := g.r.SettleWorkspaces(g.ctx)
	if err != nil || len(orphans) != 2 || orphans[0].TaskID != g.task.ID || orphans[1].TaskID != keyless.ID ||
		!orphans[0].Adopted || !orphans[1].Adopted {
		t.Fatalf("orphans %+v (%v), want each task's own adopted", orphans, err)
	}
	g.launch("fx-repro")
	if s := g.session("fx-repro"); store.Deref(s.HerdrWorkspaceID) != mine.Workspace.ID {
		t.Fatalf("the task's role sits in %s, want its own workspace %s", store.Deref(s.HerdrWorkspaceID), mine.Workspace.ID)
	}
	if err := g.r.Launch(g.ctx, keyless, theirs, g.claimed(), g.roles("fx-repro")); err != nil {
		t.Fatal(err)
	}
	pr, err := g.r.Owner(g.ctx, keyless)
	if err != nil {
		t.Fatal(err)
	}
	s, err := g.st.LiveSessionByPRRole(g.ctx, pr.ID, "fx-repro")
	if err != nil || store.Deref(s.HerdrWorkspaceID) != other.Workspace.ID {
		t.Fatalf("the keyless task's role sits in %s (%v), want its own workspace %s", store.Deref(s.HerdrWorkspaceID), err, other.Workspace.ID)
	}
	if len(g.h.creates) != 2 {
		t.Fatalf("%d workspaces created, want only the two the crashes left", len(g.h.creates))
	}
}

// A run at work when the daemon stopped is observed after the restart, in
// its pane or in the one herdr restored its conversation to (Recover binds
// the session there again), and its end comes; nothing is sent again.
func TestAFreshRuntimeObservesTheRunAtWork(t *testing.T) {
	for _, moved := range []bool{false, true} {
		g := newRig(t)
		g.launch("fx-repro")
		agent := g.agent("fx-repro")
		g.h.setSession(agent, "sid-1")
		id := g.send("fx-repro", "reproduce the bug")
		g.observe()
		if moved { // herdr restarted too and restored the conversation in a new pane
			g.h.mu.Lock()
			p := g.h.newPaneLocked("w9", g.copy)
			g.h.workspaces = append(g.h.workspaces, herdr.Workspace{ID: "w9", Label: "restored"})
			for i := range g.h.agents {
				if g.h.agents[i].Name == agent {
					g.h.closePaneLocked(g.h.agents[i].PaneID)
				}
			}
			g.h.agents = append(g.h.agents, herdr.AgentInfo{PaneID: p.ID, WorkspaceID: "w9", Name: "claude", Agent: "claude",
				AgentStatus: herdr.StatusWorking, AgentSession: &herdr.AgentSession{Kind: "id", Value: "sid-1"}})
			g.h.mu.Unlock()
		}

		g.restart()
		changed, err := g.r.Recover(g.ctx, g.task)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool][]string{false: nil, true: {"fx-repro rebound"}}[moved]; !slices.Equal(changed, want) {
			t.Fatalf("moved=%v: Recover changed %v, want %v", moved, changed, want)
		}
		s := g.session("fx-repro")
		g.h.mu.Lock()
		for i := range g.h.agents {
			if g.h.agents[i].PaneID == store.Deref(s.HerdrPaneID) {
				g.h.agents[i].AgentStatus = herdr.StatusIdle
			}
		}
		g.h.mu.Unlock()
		ended := only(g.observeN(2), EventEnded)
		if len(ended) != 1 || ended[0].RunID != id {
			t.Fatalf("moved=%v: ended %+v, want the run %s from before the restart", moved, ended, id)
		}
		if prompts := g.h.promptTexts(); !slices.Equal(prompts, []string{"reproduce the bug"}) {
			t.Fatalf("moved=%v: prompts %q, want nothing sent again", moved, prompts)
		}
	}
}

// A prompt the daemon stopped in the middle of (its entry unknown, its run
// never past pending) that the transcript shows arrived is settled by the
// fresh runtime: delivered, its run watched as submitted, never sent again.
func TestAFreshRuntimeWatchesAPromptItsTranscriptHolds(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.observe()
	text := "reproduce the bug"
	e := g.pendingPrompt("fx-repro", text, true)
	g.writeTranscript("sid-1", userLine(e.Sent.Add(time.Second), text))

	g.restart()
	if got, err := g.r.Reconcile(g.ctx, e); err != nil || got != registry.DeliveredYes {
		t.Fatalf("Reconcile = %s, %v, want yes", got, err)
	}
	if run := g.storeRun(e.RunID); run.State != store.RunSubmitted || run.SubmittedAt == nil {
		t.Fatalf("run %s is %s, want submitted: it is watched", run.ID, run.State)
	}
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	if ended := only(g.observeN(2), EventEnded); len(ended) != 1 || ended[0].RunID != e.RunID {
		t.Fatalf("ended %+v, want the delivered run", ended)
	}
	if prompts := g.h.promptTexts(); !slices.Equal(prompts, []string{text}) {
		t.Fatalf("prompts %q, want it once", prompts)
	}
}

// A restart in the middle of the proof that a prompt never arrived starts
// the proof again: the fresh runtime's first read neither settles it nor
// calls you, however long the delivery has been unknown; the read after
// RedeliverAfter settles it as never arrived and says so (EventUndelivered),
// and the prompt goes once.
func TestAFreshRuntimeProvesAPromptNeverArrivedBeforeItGoesAgain(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.observe()
	text := "reproduce the bug"
	e := g.pendingPrompt("fx-repro", text, false)
	g.writeTranscript("sid-1", assistantLine(e.Sent.Add(30*time.Second), "the agent wrote after the prompt left"))
	g.clock.Add(UnknownAfter)
	if got, err := g.r.Reconcile(g.ctx, e); err != nil || got != registry.DeliveredUnknown {
		t.Fatalf("the first read = %s, %v, want unknown", got, err)
	}

	g.restart()
	evs := g.observe()
	if got := append(only(evs, EventDeliveryUnknown), only(evs, EventUndelivered)...); len(got) != 0 {
		t.Fatalf("the fresh runtime's first read gave %+v, want nothing while its proof runs", got)
	}
	g.clock.Add(RedeliverAfter)
	undelivered := only(g.observe(), EventUndelivered)
	if want := (Event{TaskID: g.task.ID, Role: "fx-repro", RunID: e.RunID, Kind: EventUndelivered,
		Reason: "the prompt to fx-repro never arrived (its transcript, read twice, has no such turn): it goes once more"}); !reflect.DeepEqual(undelivered, []Event{want}) {
		t.Fatalf("undelivered %+v, want %+v", undelivered, want)
	}
	if got := g.ledger()[0].Delivered; got != registry.DeliveredNo {
		t.Fatalf("the entry is %s, want no", got)
	}
	g.send("fx-repro", text)
	if prompts := g.h.promptTexts(); !slices.Equal(prompts, []string{text}) {
		t.Fatalf("prompts %q, want the one send after the proof", prompts)
	}
}

// Review Focus 2: a prompt whose transcript cannot be read stays unknown
// through any number of restarts. Each fresh runtime reads it, tells you
// once it is past UnknownAfter, and refuses to send to the role: herdr
// never gets the prompt a second time.
func TestAPromptOfUnknownDeliveryStaysUnsentAcrossRestarts(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro") // no conversation id: no transcript to read
	text := "reproduce the bug"
	g.pendingPrompt("fx-repro", text, true)
	for i := range 3 {
		g.restart()
		e := g.ledger()[0]
		if got, err := g.r.Reconcile(g.ctx, e); err != nil || got != registry.DeliveredUnknown {
			t.Fatalf("restart %d: Reconcile = %s, %v, want unknown", i+1, got, err)
		}
		g.clock.Add(UnknownAfter)
		evs := g.observeN(2)
		if len(only(evs, EventUndelivered)) != 0 || len(only(evs, EventDeliveryUnknown)) != 1 {
			t.Fatalf("restart %d: events %+v, want you told once and no proof", i+1, evs)
		}
		if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), text); !errors.Is(err, ErrDeliveryUnknown) {
			t.Fatalf("restart %d: Send = %v, want ErrDeliveryUnknown", i+1, err)
		}
	}
	if prompts := g.h.promptTexts(); !slices.Equal(prompts, []string{text}) {
		t.Fatalf("prompts %q, want the one before the first restart", prompts)
	}
	if got := g.ledger()[0].Delivered; got != registry.DeliveredUnknown {
		t.Fatalf("the entry is %s, want still unknown", got)
	}
}

// The wait on you names every prompt of unknown delivery, and tells one
// from several.
func TestUnknownReasonNamesEveryRole(t *testing.T) {
	one, two := UnknownReason("fx-repro"), UnknownReason("fx-repro", "fx-cause")
	if one != "a prompt to fx-repro may or may not have arrived: look at its pane" ||
		two != "prompts to fx-repro and fx-cause may or may not have arrived: look at their panes" {
		t.Fatalf("UnknownReason = %q / %q", one, two)
	}
	for _, s := range []string{one, two, UnknownReason("a", "b", "c")} {
		if !IsUnknownReason(s) {
			t.Errorf("IsUnknownReason(%q) = false", s)
		}
	}
	if IsUnknownReason("the fx-repro agent wrote no valid findings-fx-repro.md") {
		t.Error("IsUnknownReason took another wait for an unknown delivery")
	}
}

// A launch whose task works in a workspace of its own already takes that
// one (EnsureWorkspace reuses it): the leftover a crash made for the
// attempt holds nothing, and is closed rather than adopted.
func TestALaunchClosesItsLeftoverWhenTheTaskHasAWorkspace(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	own := store.Deref(g.session("fx-repro").HerdrWorkspaceID)
	next := g.begin(g.task, "judge")
	cut := g.launchCut(g.task, next)

	g.restart()
	if err := g.r.Launch(g.ctx, g.task, next, g.claimed(), g.roles("fx-judge")); err != nil {
		t.Fatal(err)
	}
	if s := g.session("fx-judge"); store.Deref(s.HerdrWorkspaceID) != own {
		t.Fatalf("fx-judge sits in %s, want the task's own workspace %s", store.Deref(s.HerdrWorkspaceID), own)
	}
	if got := g.workspaceIDs(); !slices.Equal(got, []string{own}) || got[0] == cut.Workspace.ID {
		t.Fatalf("workspaces %v, want only the task's own: the leftover closed", got)
	}
}

// A proof whose reads fail (the registry or the agents store refuses) does
// not hold its task for good: past its bound it is no longer under way, so
// its task is told to you (which sends nothing); a proof that runs to its
// second read in time is unaffected.
func TestAProofWhoseReadsFailStopsHoldingItsTask(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.observe()
	e := g.pendingPrompt("fx-repro", "reproduce the bug", false)
	g.writeTranscript("sid-1", assistantLine(e.Sent.Add(30*time.Second), "the agent wrote after the prompt left"))
	if got, err := g.r.Reconcile(g.ctx, e); err != nil || got != registry.DeliveredUnknown || !g.r.Proving(e.ID) {
		t.Fatalf("the first read = %s, %v, proving %v; want its proof begun", got, err, g.r.Proving(e.ID))
	}
	failing, cancel := context.WithCancel(g.ctx)
	cancel()
	for range 3 {
		g.clock.Add(time.Minute)
		if _, err := g.r.Reconcile(failing, e); err == nil {
			t.Fatal("a read on a store that refuses succeeded")
		}
	}
	if !g.r.Proving(e.ID) {
		t.Fatal("the proof ended after 3 minutes of failed reads, want it under way within its bound")
	}
	g.clock.Add(3 * time.Minute)
	if g.r.Proving(e.ID) {
		t.Fatal("a proof whose reads failed for 6 minutes is still under way: its task would hold for good")
	}
	if prompts := g.h.promptTexts(); len(prompts) != 0 {
		t.Fatalf("prompts %q, want none sent", prompts)
	}
}

// A prompt of unknown delivery that a later read of the observe pass finds
// in its transcript is settled delivered and says so (EventDelivered), once:
// its run is watched as submitted, and nothing is sent again.
func TestALaterReadThatFindsThePromptSaysItArrived(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.observe()
	text := "reproduce the bug"
	e := g.pendingPrompt("fx-repro", text, true)
	g.clock.Add(UnknownAfter)
	if evs := g.observeN(2); len(only(evs, EventDelivered)) != 0 || len(only(evs, EventDeliveryUnknown)) != 1 {
		t.Fatalf("events %+v before the transcript holds the prompt, want you told and no delivery", evs)
	}

	g.writeTranscript("sid-1", userLine(e.Sent.Add(time.Second), text)) // the CLI flushed its transcript at last
	g.clock.Add(time.Minute)
	delivered := only(g.observe(), EventDelivered)
	want := Event{TaskID: g.task.ID, Role: "fx-repro", RunID: e.RunID, Kind: EventDelivered,
		Reason: "the prompt to fx-repro arrived (its transcript holds it): its run is watched, never sent again"}
	if !reflect.DeepEqual(delivered, []Event{want}) {
		t.Fatalf("delivered %+v, want %+v", delivered, want)
	}
	if run := g.storeRun(e.RunID); run.State != store.RunSubmitted || g.ledger()[0].Delivered != registry.DeliveredYes {
		t.Fatalf("run %s is %s, entry %s: want submitted and delivered", run.ID, run.State, g.ledger()[0].Delivered)
	}
	g.clock.Add(time.Minute)
	if again := only(g.observe(), EventDelivered); len(again) != 0 {
		t.Fatalf("the delivery was told again: %+v", again)
	}
	if prompts := g.h.promptTexts(); !slices.Equal(prompts, []string{text}) {
		t.Fatalf("prompts %q, want nothing sent again", prompts)
	}
}
