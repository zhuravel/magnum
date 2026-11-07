package runtime

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
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

// A liaison whose first prompt never went out (herdr rejected it before
// sending) gets it at the next Talk, once: a live liaison is no proof that
// its conversation ever got a prompt. A prompt that went out is never sent
// again, so a third Talk sends nothing.
func TestTalkSendsTheFirstPromptAfterAFailedOne(t *testing.T) {
	g := newRig(t)
	rejected := false
	g.h.mu.Lock()
	g.h.promptErr = func(string) error { // called with the fake's lock held
		if rejected {
			return nil
		}
		rejected = true
		return &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentBlocked, Message: "the agent is not ready"}
	}
	g.h.mu.Unlock()

	first, err := g.r.Talk(g.ctx, g.task, talkPrompt)
	if err == nil {
		t.Fatal("Talk whose first prompt herdr rejected went through")
	}
	pane := store.Deref(g.liaisonSession().HerdrPaneID)
	if first != pane || pane == "" {
		t.Fatalf("Talk whose first prompt failed named pane %q, want the live liaison's %q with its error", first, pane)
	}
	runs, err := g.st.RunsByPR(g.ctx, g.liaisonRowOf().ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].SubmittedAt != nil || runs[0].State != store.RunFailed {
		t.Fatalf("the liaison's runs after the rejected prompt %+v, want one that failed before it went out", runs)
	}

	if again := g.talk(); again != pane {
		t.Fatalf("the second Talk named pane %s, want the live liaison's %s", again, pane)
	}
	if got := g.h.promptTexts(); !slices.Equal(got, []string{talkPrompt, talkPrompt}) {
		t.Fatalf("herdr got prompts %q, want the rejected one and the one the second Talk sent", got)
	}
	g.talk()
	if got := g.h.promptTexts(); len(got) != 2 {
		t.Fatalf("herdr got %d prompts after the third Talk, want 2: a prompt that went out is never sent again", len(got))
	}
	if starts := g.h.startCalls(); len(starts) != 1 {
		t.Fatalf("agents started %+v, want the one liaison", starts)
	}
}

// A liaison whose first prompt waits for you (you typed into its pane
// within the human cooldown: agents.ErrHumanActive) is live in a pane you
// may step into: Talk names that pane with the error, and once the cooldown
// is over the next Talk sends the prompt.
func TestTalkNamesTheLiaisonsPaneWhenItsFirstPromptWaitsForYou(t *testing.T) {
	g := newRig(t)
	rejected := false
	g.h.mu.Lock()
	g.h.promptErr = func(string) error { // called with the fake's lock held
		if rejected {
			return nil
		}
		rejected = true
		return &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentBlocked, Message: "the agent is not ready"}
	}
	g.h.mu.Unlock()
	if _, err := g.r.Talk(g.ctx, g.task, talkPrompt); err == nil {
		t.Fatal("Talk whose first prompt herdr rejected went through")
	}
	g.clock.Add(2 * time.Minute) // past a start's grace: herdr showing the agent at work is you
	g.h.setStatus(g.liaisonAgent(), herdr.StatusWorking)
	g.observe()
	g.h.setStatus(g.liaisonAgent(), herdr.StatusIdle)
	if at := g.liaisonRowOf().HumanActiveAt; at == nil {
		t.Fatal("herdr did not show you in the liaison's pane")
	}

	pane, err := g.r.Talk(g.ctx, g.task, talkPrompt)

	if !errors.Is(err, agents.ErrHumanActive) {
		t.Fatalf("Talk while you are in the liaison's pane: %v, want ErrHumanActive", err)
	}
	if want := store.Deref(g.liaisonSession().HerdrPaneID); pane != want || pane == "" {
		t.Fatalf("Talk named pane %q with its error, want the live liaison's %q", pane, want)
	}
	g.clock.Add(g.cfg.Factory.HumanCooldown.Duration + time.Minute)
	g.talk()
	if got := g.h.promptTexts(); !slices.Equal(got, []string{talkPrompt, talkPrompt}) {
		t.Fatalf("herdr got prompts %q, want the rejected one and the one sent after the cooldown", got)
	}
}

// The liaison's agent exits (your /exit, a crash of its CLI) and leaves a
// shell in its pane, and a Talk comes before an observe pass marked its row
// lost: Talk reconciles the row with herdr first, so the conversation is
// resumed in that pane on a row of its own, and a resumed conversation that
// had its first prompt gets none. Without it, a fresh agent took the live
// row, kept the old conversation's id, and got no first prompt.
func TestTalkAfterTheLiaisonExitedResumesItsConversation(t *testing.T) {
	g := newRig(t)
	g.talk()
	g.h.setSession(g.liaisonAgent(), "sid-liaison")
	g.endLiaisonTurn()
	old := g.liaisonSession()
	pane := store.Deref(old.HerdrPaneID)
	g.h.mu.Lock()
	g.h.agents = slices.DeleteFunc(g.h.agents, func(a herdr.AgentInfo) bool { return a.PaneID == pane }) // the shell stays
	g.h.mu.Unlock()

	again := g.talk()

	starts := g.h.startCalls()
	if len(starts) != 2 || !slices.Contains(starts[1].Args, "sid-liaison") || starts[1].PaneID != pane || again != pane {
		t.Fatalf("agents started %+v (Talk named pane %s), want the conversation sid-liaison resumed in pane %s", starts, again, pane)
	}
	if s := g.liaisonSession(); s.ID == old.ID || store.Deref(s.SessionID) != "sid-liaison" {
		t.Fatalf("the liaison's live session %d carries %q, want a new row resuming sid-liaison (the old row %d)", s.ID,
			store.Deref(s.SessionID), old.ID)
	}
	gone, err := g.st.SessionByID(g.ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gone.State != store.SessionLost {
		t.Fatalf("the exited agent's row is %s, want lost", gone.State)
	}
	if got := g.h.promptTexts(); len(got) != 1 {
		t.Fatalf("herdr got prompts %q, want the first Talk's alone: the resumed conversation had its first prompt", got)
	}
}

// The liaison runs the daemon's `magnum factory` commands in its pane. A
// daemon run under MAGNUM_HOME (the checkout layout) gives the pane the same
// MAGNUM_HOME, so they reach the daemon's registry; a daemon of the XDG
// layout gives none, as its binary finds the same directories by itself.
func TestTheLiaisonsPaneCarriesTheDaemonsMagnumHome(t *testing.T) {
	g := newRig(t) // the checkout layout: a Home, no data or state directory
	g.talk()
	if home := g.liaisonSession().Env["MAGNUM_HOME"]; home != g.layout.Home || home == "" {
		t.Fatalf("the liaison's pane env MAGNUM_HOME = %q, want the daemon's %q", home, g.layout.Home)
	}
	if home := g.h.creates[0].Env["MAGNUM_HOME"]; home != g.layout.Home {
		t.Fatalf("herdr made the liaison's pane with MAGNUM_HOME = %q, want %q", home, g.layout.Home)
	}

	g.layout = paths.Layout{Home: t.TempDir(), DataDir: t.TempDir(), StateDir: t.TempDir()} // XDG, the binary in a checkout
	g.cfg.Layout = g.layout
	g.restart()
	g.task = g.newTask("PR-12346")
	g.talk()
	if home, ok := g.liaisonSession().Env["MAGNUM_HOME"]; ok {
		t.Fatalf("the liaison's pane env MAGNUM_HOME = %q under the XDG layout, want none", home)
	}
	if home, ok := g.h.creates[1].Env["MAGNUM_HOME"]; ok {
		t.Fatalf("herdr made the liaison's pane with MAGNUM_HOME = %q under the XDG layout, want none", home)
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

// A park that comes while a Talk starts or prompts the task's liaison (the
// engine runs Talk off its loop) leaves the liaison alone and returns at
// once: parking it between its start and its first prompt would quit an
// agent the Talk is about to prompt. Park never waits for the Talk, which
// can take minutes; the engine parks the liaison itself when the task has
// ended meanwhile.
func TestParkLeavesTheLiaisonAloneWhileATalkWorks(t *testing.T) {
	g := newRig(t)
	inPrompt, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	g.h.mu.Lock()
	g.h.onPrompt = func(*fakeHerdr, string, string) { // called with the fake's lock held
		close(inPrompt)
		<-release
	}
	g.h.mu.Unlock()
	talkDone := make(chan error, 1)
	go func() {
		_, err := g.r.Talk(g.ctx, g.task, talkPrompt)
		talkDone <- err
	}()
	select {
	case <-inPrompt:
	case <-time.After(20 * time.Second):
		t.Fatal("Talk never prompted the liaison")
	}

	parkDone := make(chan error, 1)
	go func() { parkDone <- g.r.Park(g.ctx, g.task) }()
	select {
	case err := <-parkDone:
		if err != nil {
			t.Fatalf("Park while Talk works: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Park waited for the Talk")
	}

	once.Do(func() { close(release) })
	if err := <-talkDone; err != nil {
		t.Fatalf("Talk: %v", err)
	}
	if !g.liaisonLive() {
		t.Fatal("Park parked the liaison a Talk was prompting")
	}
	if got := g.h.promptTexts(); !slices.Equal(got, []string{talkPrompt}) {
		t.Fatalf("prompts %q, want the Talk's alone", got)
	}
}

// The guard is the task's own: a Talk that starts one task's liaison keeps
// no other task's park waiting or skipped.
func TestParkOfATaskGoesThroughWhileAnotherTasksLiaisonStarts(t *testing.T) {
	g := newRig(t)
	first := g.task
	g.talk()
	g.endLiaisonTurn() // idle: a park may park it
	second := g.newTask("PR-12346")
	inCreate, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	g.h.onCreate = func(o herdr.WorkspaceCreateOptions) { // without the fake's lock
		if o.Label == liaisonLabel(second) {
			close(inCreate)
			<-release
		}
	}
	talkDone := make(chan error, 1)
	go func() {
		_, err := g.r.Talk(g.ctx, second, talkPrompt)
		talkDone <- err
	}()
	select {
	case <-inCreate:
	case <-time.After(20 * time.Second):
		t.Fatal("Talk never made the second liaison's workspace")
	}

	parkDone := make(chan error, 1)
	go func() { parkDone <- g.r.Park(g.ctx, first) }()
	select {
	case err := <-parkDone:
		if err != nil {
			t.Fatalf("Park of the first task while Talk starts the second's liaison: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Park of the first task waited for the second's Talk")
	}
	if g.liaisonLive() { // g.task is the first
		t.Fatal("Park of the first task left its idle liaison live")
	}

	once.Do(func() { close(release) })
	if err := <-talkDone; err != nil {
		t.Fatalf("Talk: %v", err)
	}
	g.task = second
	if !g.liaisonLive() {
		t.Fatal("the second task's liaison is not live after its Talk")
	}
}

// LiaisonSession is the session the CLI steps into after a talk: the task's
// live liaison, in the pane Talk named; none before the first talk and none
// once the liaison is parked.
func TestLiaisonSessionIsTheLiveLiaisonInTheTalksPane(t *testing.T) {
	g := newRig(t)
	if _, err := LiaisonSession(g.ctx, g.st, g.task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LiaisonSession before the first talk: %v, want ErrNotFound", err)
	}

	pane := g.talk()

	s, err := LiaisonSession(g.ctx, g.st, g.task.ID)
	if err != nil || store.Deref(s.HerdrPaneID) != pane || store.Deref(s.AgentName) != g.liaisonAgent() || s.Role != LiaisonRole {
		t.Fatalf("LiaisonSession: %+v, %v; want fx-liaison %s in pane %s", s, err, g.liaisonAgent(), pane)
	}

	g.endLiaisonTurn()
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatal(err)
	}
	if _, err := LiaisonSession(g.ctx, g.st, g.task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LiaisonSession of a parked liaison: %v, want ErrNotFound", err)
	}
}

// The liaison's agent exits (your /exit, a crash of its CLI) and an observe
// pass marks its row lost before the next Talk: Talk finds no live liaison
// and resumes the lost conversation on a row of its own, and the
// conversation, which had its first prompt, gets none.
func TestTalkAfterTheLiaisonsRowWasMarkedLostResumesItsConversation(t *testing.T) {
	g := newRig(t)
	g.talk()
	g.h.setSession(g.liaisonAgent(), "sid-liaison")
	g.endLiaisonTurn()
	old := g.liaisonSession()
	pane := store.Deref(old.HerdrPaneID)
	g.h.mu.Lock()
	g.h.agents = slices.DeleteFunc(g.h.agents, func(a herdr.AgentInfo) bool { return a.PaneID == pane }) // the shell stays
	g.h.mu.Unlock()
	g.clock.Add(agents.LostGrace)
	g.observe()
	if lost, err := g.st.SessionByID(g.ctx, old.ID); err != nil || lost.State != store.SessionLost {
		t.Fatalf("the exited agent's row after an observe pass: %+v, %v; want lost", lost, err)
	}

	again := g.talk()

	starts := g.h.startCalls()
	if len(starts) != 2 || !slices.Contains(starts[1].Args, "sid-liaison") || starts[1].PaneID != again {
		t.Fatalf("agents started %+v (Talk named pane %s), want the conversation sid-liaison resumed in that pane", starts, again)
	}
	if s := g.liaisonSession(); s.ID == old.ID || store.Deref(s.SessionID) != "sid-liaison" || store.Deref(s.HerdrPaneID) != again {
		t.Fatalf("the liaison's live session %d carries %q in pane %s, want a new row resuming sid-liaison in pane %s (the old row %d)",
			s.ID, store.Deref(s.SessionID), store.Deref(s.HerdrPaneID), again, old.ID)
	}
	if got := g.h.promptTexts(); len(got) != 1 {
		t.Fatalf("herdr got prompts %q, want the first Talk's alone: the resumed conversation had its first prompt", got)
	}
}

// youTypeIntoTheLiaison is you typing into the liaison's pane ("drop it",
// "push it"): herdr shows its agent at work with no prompt of the
// factory's, so the liaison's row notes you, and the agent stays at work.
func (g *rig) youTypeIntoTheLiaison() {
	g.t.Helper()
	g.clock.Add(2 * time.Minute) // past a start's grace: herdr showing the agent at work is you
	g.h.focus(g.liaisonAgent())
	g.h.setStatus(g.liaisonAgent(), herdr.StatusWorking)
	g.observe()
	if at := g.liaisonRowOf().HumanActiveAt; at == nil || !at.Equal(g.clock.Now()) {
		g.t.Fatalf("herdr did not show you in the liaison's pane: human_active_at %v", at)
	}
}

// endTask moves the rig's task to state, as the engine's drop (stopped) or
// push (published) does before it parks the task.
func (g *rig) endTask(state string) {
	g.t.Helper()
	if err := g.reg.TransitionTask(g.ctx, g.task.ID, nil, state, nil); err != nil {
		g.t.Fatal(err)
	}
	task, err := g.reg.TaskByID(g.ctx, g.task.ID)
	if err != nil {
		g.t.Fatal(err)
	}
	g.task = task
}

// A push or a drop you ask the liaison for in its chat comes with the
// task's park (the engine parks the task in the tick that answers the
// decide, and again when the push is published), which finds you in the
// liaison's pane and its agent at work, and leaves it live. Once its turn
// ended and you left its pane for the human cooldown, an observe pass parks
// it: nothing else ever parks the liaison of a task that ended, or that was
// published while the liaison was live.
func TestALiaisonThatPushedOrDroppedItsTaskParksOnceYouLeft(t *testing.T) {
	for _, tc := range []struct{ name, state string }{
		{"drop", registry.TaskStopped},
		{"push", registry.TaskPublished},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.talk()
			g.h.setSession(g.liaisonAgent(), "sid-liaison")
			g.endLiaisonTurn()
			g.youTypeIntoTheLiaison()
			g.endTask(tc.state)
			if err := g.r.Park(g.ctx, g.task); err != nil {
				t.Fatalf("Park: %v", err)
			}
			if !g.liaisonLive() {
				t.Fatal("the task's park parked the liaison you typed into a moment ago")
			}

			g.h.setStatus(g.liaisonAgent(), herdr.StatusIdle) // its turn ends
			g.h.focus("")
			g.observeN(2)
			if !g.liaisonLive() {
				t.Fatal("an observe pass parked the liaison within the human cooldown after you typed into it")
			}

			g.clock.Add(g.cfg.Factory.HumanCooldown.Duration)
			g.observe()

			if g.liaisonLive() {
				t.Fatalf("the liaison of a %s task is live after you left its pane for the human cooldown", tc.state)
			}
			g.h.mu.Lock()
			_, running := g.h.agentLocked(g.liaisonAgent())
			g.h.mu.Unlock()
			if running {
				t.Fatal("herdr still runs the liaison")
			}
		})
	}
}

// The liaison's turn may still be in flight when the cooldown is over: the
// observe pass that sees it end parks the liaison of the task that ended.
func TestALiaisonAtWorkWhenItsTaskEndedParksOnceItsTurnEnds(t *testing.T) {
	g := newRig(t)
	g.talk() // its turn in flight: the liaison runs the decide that drops the task
	g.endTask(registry.TaskStopped)
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if !g.liaisonLive() {
		t.Fatal("the task's park parked the liaison at work")
	}

	g.endLiaisonTurn()

	if g.liaisonLive() {
		t.Fatal("the liaison of a stopped task is live after its turn ended")
	}
}

// The push parks the liaison that was live then. You talk with a published
// task's liaison (its folder stays): a talk after the push starts it again,
// and no observe pass parks it for its task's publish.
func TestATalkAfterThePushKeepsItsLiaison(t *testing.T) {
	g := newRig(t)
	g.talk()
	g.h.setSession(g.liaisonAgent(), "sid-liaison")
	g.endLiaisonTurn()
	g.endTask(registry.TaskPublished)
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if g.liaisonLive() {
		t.Fatal("the push's park left the idle liaison live")
	}
	g.clock.Add(time.Minute)

	g.talk()
	g.endLiaisonTurn()
	g.clock.Add(g.cfg.Factory.HumanCooldown.Duration)
	g.observeN(2)

	if !g.liaisonLive() {
		t.Fatal("an observe pass parked the liaison a talk started after the push")
	}
}
