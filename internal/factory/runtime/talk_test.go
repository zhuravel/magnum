package runtime

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// talkPrompt is the prompt the rig's Talk hands a fresh liaison.
const talkPrompt = "You are the liaison of PR-12345: read the task's folder and tell the operator what waits on them."

// talk calls Talk for the rig's task and returns the liaison's pane.
func (g *rig) talk() string {
	g.t.Helper()
	pane, err := g.r.Talk(g.ctx, g.task, talkPrompt)
	if err != nil {
		g.t.Fatalf("Talk: %v", err)
	}
	if pane == "" {
		g.t.Fatal("Talk named no pane")
	}
	return pane
}

// liaisonRowOf is the rig's task's liaison row as it is.
func (g *rig) liaisonRowOf() store.PR {
	g.t.Helper()
	pr, err := liaisonRowIn(g.ctx, g.st, g.task.ID)
	if err != nil {
		g.t.Fatalf("the liaison's row: %v", err)
	}
	return pr
}

// liaisonSession is the live session of the rig's task's liaison.
func (g *rig) liaisonSession() store.Session {
	g.t.Helper()
	s, err := g.st.LiveSessionByPRRole(g.ctx, g.liaisonRowOf().ID, LiaisonRole)
	if err != nil {
		g.t.Fatalf("the liaison's live session: %v", err)
	}
	return s
}

// liaisonLive reports whether the rig's task's liaison has a live session.
func (g *rig) liaisonLive() bool {
	g.t.Helper()
	_, err := g.st.LiveSessionByPRRole(g.ctx, g.liaisonRowOf().ID, LiaisonRole)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		g.t.Fatal(err)
	}
	return err == nil
}

// endLiaisonTurn ends the turn of the rig's task's liaison: its agent goes
// idle for two passes.
func (g *rig) endLiaisonTurn() {
	g.t.Helper()
	g.observe() // at work on it
	g.h.setStatus(g.liaisonAgent(), herdr.StatusIdle)
	g.observeN(2)
}

// moveAgent moves the agent named name to a new pane of a new workspace
// labeled as its old one, and closes its old pane: what a herdr restart
// that brought its conversation back elsewhere leaves. It returns the new
// pane.
func (f *fakeHerdr) moveAgent(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.agentLocked(name)
	if !ok {
		return ""
	}
	old := f.agents[i].PaneID
	label, cwd := "", ""
	for _, p := range f.panes {
		if p.ID == old {
			cwd = p.Cwd
			for _, w := range f.workspaces {
				if w.ID == p.WorkspaceID {
					label = w.Label
				}
			}
		}
	}
	f.nextWS++
	id := fmt.Sprintf("w%d", f.nextWS)
	f.workspaces = append(f.workspaces, herdr.Workspace{ID: id, Label: label})
	p := f.newPaneLocked(id, cwd)
	f.agents[i].PaneID, f.agents[i].WorkspaceID, f.agents[i].TabID = p.ID, id, p.TabID
	f.closePaneLocked(old)
	return p.ID
}

// Talk starts the task's liaison once: fx-liaison in a workspace of its own,
// "factory liaison <key>", whose working directory is the task's folder, and
// the prompt once, as the conversation's first. A second Talk finds the live
// liaison: the same pane, no new workspace, no new start, no prompt.
func TestTalkStartsOneLiaisonInTheTasksFolder(t *testing.T) {
	g := newRig(t)
	dir := g.layout.FactoryTaskDir(g.task.ID)

	pane := g.talk()

	if len(g.h.creates) != 1 || g.h.creates[0].Cwd != dir || g.h.creates[0].Label != "factory liaison PR-12345" {
		t.Fatalf("workspaces created %+v, want one labeled \"factory liaison PR-12345\" in %s", g.h.creates, dir)
	}
	if starts := g.h.startCalls(); len(starts) != 1 || starts[0].Name != g.liaisonAgent() || starts[0].PaneID != pane {
		t.Fatalf("agents started %+v, want fx-liaison's alone, in pane %s", starts, pane)
	}
	s := g.liaisonSession()
	if s.Role != LiaisonRole || store.Deref(s.Cwd) != dir || store.Deref(s.HerdrPaneID) != pane || s.State != store.SessionLive {
		t.Fatalf("the liaison's session %+v, want fx-liaison live in pane %s of %s", s, pane, dir)
	}
	if got := g.h.promptTexts(); !slices.Equal(got, []string{talkPrompt}) {
		t.Fatalf("prompts %q, want the liaison's once", got)
	}

	if again := g.talk(); again != pane {
		t.Fatalf("a second Talk named pane %s, want the live liaison's %s", again, pane)
	}
	if len(g.h.creates) != 1 || len(g.h.startCalls()) != 1 || len(g.h.promptTexts()) != 1 {
		t.Fatalf("a second Talk made %d workspaces, %d starts, %d prompts in all; want 1 each", len(g.h.creates),
			len(g.h.startCalls()), len(g.h.promptTexts()))
	}
}

// The liaison works in the task's folder, never in its copy: a task whose
// copy was released (published, its agents parked, the copy gone) still
// gets its liaison there, and the task's own agents stay parked.
func TestTalkWorksAfterTheCopyIsFreed(t *testing.T) {
	g := newRig(t)
	g.launch("fx-writer")
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if err := g.reg.TransitionTask(g.ctx, g.task.ID, nil, registry.TaskPublished, func(u *registry.TaskUpdate) { u.Set("copy", "") }); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(g.copy); err != nil {
		t.Fatal(err)
	}
	task, err := g.reg.TaskByID(g.ctx, g.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	g.task = task
	dir := g.layout.FactoryTaskDir(g.task.ID)

	pane := g.talk()

	if s := g.liaisonSession(); store.Deref(s.Cwd) != dir || store.Deref(s.HerdrPaneID) != pane {
		t.Fatalf("the liaison's session works in %s (pane %s), want %s (pane %s)", store.Deref(s.Cwd), store.Deref(s.HerdrPaneID), dir, pane)
	}
	if got := g.h.promptTexts(); !slices.Equal(got, []string{talkPrompt}) {
		t.Fatalf("prompts %q, want the liaison's alone", got)
	}
	if _, err := g.st.LiveSessionByPRRole(g.ctx, g.owner().ID, "fx-writer"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fx-writer's session after the liaison's start: %v, want none live", err)
	}
}

// A liaison parked with its task resumes its conversation at the next Talk:
// the agent starts with the conversation's id, and a resumed conversation
// gets no first prompt again.
func TestTalkResumesAParkedLiaison(t *testing.T) {
	g := newRig(t)
	g.talk()
	g.h.setSession(g.liaisonAgent(), "sid-liaison")
	g.endLiaisonTurn()
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if g.liaisonLive() {
		t.Fatal("the liaison is live after Park")
	}

	pane := g.talk()

	starts := g.h.startCalls()
	if len(starts) != 2 || !slices.Contains(starts[1].Args, "sid-liaison") || starts[1].PaneID != pane {
		t.Fatalf("agents started %+v, want the liaison resumed from sid-liaison in pane %s", starts, pane)
	}
	if s := g.liaisonSession(); store.Deref(s.SessionID) != "sid-liaison" || store.Deref(s.Cwd) != g.layout.FactoryTaskDir(g.task.ID) {
		t.Fatalf("the liaison's session %+v, want conversation sid-liaison in the task's folder", s)
	}
	if got := g.h.promptTexts(); len(got) != 1 {
		t.Fatalf("prompts %q, want the first Talk's alone: a resumed conversation gets none", got)
	}
}

// A restart keeps the liaison: the new runtime's recovery binds its
// session again where herdr shows it (here in another pane, as a herdr that
// restarted too shows it), and Talk finds it live there: no new workspace,
// no new start, no second first prompt.
func TestARestartKeepsTheLiaison(t *testing.T) {
	g := newRig(t)
	g.talk()
	g.h.setSession(g.liaisonAgent(), "sid-liaison")
	g.endLiaisonTurn()
	moved := g.h.moveAgent(g.liaisonAgent())
	creates := len(g.h.creates)

	g.restart()
	changed, err := g.r.Recover(g.ctx, g.task)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !slices.Contains(changed, "fx-liaison rebound") {
		t.Fatalf("the restart's recovery changed %q, want the liaison rebound", changed)
	}
	pane := g.talk()

	if pane != moved {
		t.Fatalf("Talk after the restart named pane %s, want %s, where herdr shows the liaison", pane, moved)
	}
	if len(g.h.creates) != creates || len(g.h.startCalls()) != 1 || len(g.h.promptTexts()) != 1 {
		t.Fatalf("after the restart: %d workspaces made, %d starts and %d prompts in all; want none new, 1 and 1",
			len(g.h.creates)-creates, len(g.h.startCalls()), len(g.h.promptTexts()))
	}
}

// What you type into the liaison's pane is a typed input of its task, with
// the surface "liaison": the observe pass hands it as an EventTyped of the
// task, with herdr's proof that you typed it. The prompt Talk sent it is the
// factory's own, no turn of yours.
func TestTypedChatWithTheLiaisonCountsForItsTask(t *testing.T) {
	g := newRig(t)
	sent := g.clock.Now()
	g.talk()
	g.h.setSession(g.liaisonAgent(), "sid-liaison")
	g.endLiaisonTurn()
	g.clock.Add(2 * time.Minute)
	typed := g.clock.Now()
	g.writeTranscript("sid-liaison",
		userLine(sent, talkPrompt),
		assistantLine(sent.Add(2*time.Second), "PR-12345 waits on you for one question."),
		userLine(typed, "why does the critic object?"),
	)

	g.h.setStatus(g.liaisonAgent(), herdr.StatusWorking) // you type, no prompt of the factory's
	evs := only(g.observe(), EventTyped)
	g.h.setStatus(g.liaisonAgent(), herdr.StatusIdle)

	if len(evs) != 1 || evs[0].TaskID != g.task.ID || evs[0].Role != LiaisonRole || evs[0].Surface != SurfaceLiaison {
		t.Fatalf("typed events %+v, want one of the task's liaison, surface %q", evs, SurfaceLiaison)
	}
	want := []Turn{{At: typed, Text: "why does the critic object?", Before: "PR-12345 waits on you for one question.", Stage: "investigate",
		AgentState: string(herdr.StatusIdle), Origin: registry.OriginHerdr}}
	if got := evs[0].Turns; !slices.EqualFunc(got, want, sameTurn) {
		t.Fatalf("turns %+v\nwant  %+v", got, want)
	}
}

// Parking a task (a drop, a push, the Mac short on memory) parks its
// liaison as well as its own agents.
func TestDroppingATaskParksItsLiaison(t *testing.T) {
	g := newRig(t)
	g.launch("fx-writer")
	g.talk()
	g.endLiaisonTurn()

	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park: %v", err)
	}

	if g.liaisonLive() {
		t.Fatal("the liaison is live after its task's Park")
	}
	if _, err := g.st.LiveSessionByPRRole(g.ctx, g.owner().ID, "fx-writer"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fx-writer's session after Park: %v, want none live", err)
	}
	g.h.mu.Lock()
	defer g.h.mu.Unlock()
	if _, ok := g.h.agentLocked(g.liaisonAgent()); ok {
		t.Fatal("herdr still runs the liaison after Park")
	}
}

// Parking a task leaves alone a liaison you are with: one at work (its
// turn in flight, as when it runs the decide that dropped the task) and
// one you typed into within the factory's human cooldown, as the factory
// leaves the task's own panes you are in. The task's Park goes through, and
// once the cooldown is over, the next Park parks the liaison.
func TestParkingATaskLeavesTheLiaisonYouAreIn(t *testing.T) {
	g := newRig(t)
	g.talk() // its turn in flight

	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park with the liaison at work: %v", err)
	}
	if !g.liaisonLive() {
		t.Fatal("Park parked the liaison at work")
	}

	g.endLiaisonTurn()
	g.clock.Add(2 * time.Minute) // past a start's grace: herdr showing the agent at work is you
	g.h.focus(g.liaisonAgent())  // you in its pane
	g.h.setStatus(g.liaisonAgent(), herdr.StatusWorking)
	g.observe()
	g.h.setStatus(g.liaisonAgent(), herdr.StatusIdle)
	g.h.focus("")
	g.observeN(2)
	if at := g.liaisonRowOf().HumanActiveAt; at == nil {
		t.Fatal("herdr did not show you in the liaison's pane")
	}

	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park with you in the liaison: %v", err)
	}
	if !g.liaisonLive() {
		t.Fatal("Park parked the liaison you typed into a moment ago")
	}

	g.clock.Add(g.cfg.Factory.HumanCooldown.Duration + time.Minute)
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park after the cooldown: %v", err)
	}
	if g.liaisonLive() {
		t.Fatal("the liaison is live after the cooldown's Park")
	}
}

// A liaison's turn that ended is settled at once: nothing checks it for an
// end fact (its end is no event of the task's), so it never stays among the
// agents store's active runs.
func TestALiaisonsTurnThatEndedLeavesNoActiveRun(t *testing.T) {
	g := newRig(t)
	g.talk()
	g.endLiaisonTurn()

	runs, err := g.st.RunsByPR(g.ctx, g.liaisonRowOf().ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != store.RunVerified || runs[0].EndedAt == nil {
		t.Fatalf("the liaison's runs %+v, want its one turn ended and settled", runs)
	}
	active, err := g.st.ActiveRuns(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active runs %+v, want none once the liaison's turn ended", active)
	}
}

// A liaison's workspace a crash left between its creation and its first
// row (no row names its panes, no agent in it) is closed after the restart,
// with its task named; a live liaison's workspace is left alone.
func TestSettleWorkspacesClosesALiaisonsLeftover(t *testing.T) {
	g := newRig(t)
	g.talk() // the live liaison of PR-12345
	other := g.newTask("PR-12346")
	left, err := g.h.WorkspaceCreate(g.ctx, herdr.WorkspaceCreateOptions{Cwd: g.layout.FactoryTaskDir(other.ID),
		Label: liaisonLabel(other)})
	if err != nil {
		t.Fatal(err)
	}
	live := store.Deref(g.liaisonSession().HerdrWorkspaceID)

	g.restart()
	orphans, err := g.r.SettleWorkspaces(g.ctx)
	if err != nil {
		t.Fatalf("SettleWorkspaces: %v", err)
	}

	if len(orphans) != 1 || orphans[0].Workspace != left.Workspace.ID || orphans[0].TaskID != other.ID || orphans[0].Adopted ||
		orphans[0].Label != "factory liaison PR-12346" {
		t.Fatalf("orphans %+v, want the leftover %s of PR-12346 closed", orphans, left.Workspace.ID)
	}
	if ids := g.workspaceIDs(); !slices.Equal(ids, []string{live}) {
		t.Fatalf("workspaces %v, want the live liaison's %s alone", ids, live)
	}
}
