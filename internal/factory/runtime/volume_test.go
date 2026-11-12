package runtime

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
)

// deniedScreen is what a factory pane showed on 2026-11-10, once its herdr
// server could no longer read the external volume its copy is on: the
// shell's getcwd, then Claude Code's own refusal, then the shell again.
const deniedScreen = "shell-init: error retrieving current directory: getcwd: cannot access parent directories: Operation not permitted\n" +
	"mise WARN  Current directory does not exist or is not accessible: .\n" +
	"Can't read the current directory (EPERM). Start Claude Code from a different directory.\n" +
	"zhuravel@mac talkable.factory2 % "

// retried records your retry of the rig's task, as the engine's retry
// does.
func (g *rig) retried() {
	g.t.Helper()
	if _, err := g.reg.RecordInput(g.ctx, registry.Input{TaskID: g.task.ID, Kind: registry.InputRetry, Surface: "screen"}); err != nil {
		g.t.Fatal(err)
	}
}

// herdrEvents are the rig's task's herdr events, as "<phase>: <text>".
func (g *rig) herdrEvents() []string {
	g.t.Helper()
	evs, err := g.reg.TaskEvents(g.ctx, g.task.ID, "herdr", 0)
	if err != nil {
		g.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Phase+": "+ev.Text)
	}
	return out
}

// A role whose agent exits at its start because its pane cannot read the
// copy (macOS refused the factory's herdr server the volume), while the
// daemon reads the copy fine, gets the factory's herdr server stopped once,
// so the engine's reveal starts a fresh one, and is relaunched there once;
// its first prompt then goes. The stop and the relaunch are events; a
// screen read from before the stop (herdr restores a pane's scrollback) is
// no second denial.
func TestALaunchMacOSDeniesTheCopyGetsOneFreshHerdrServer(t *testing.T) {
	g := newRig(t)
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	w, ok := errors.AsType[*WaitError](err)
	if !ok || w.Who != pauseAgent || !strings.Contains(w.What, "restarts its herdr server") {
		t.Fatalf("Send = %v, want a wait on the restart of the factory's herdr server", err)
	}
	if n := g.h.stopCount(); n != 1 {
		t.Fatalf("%d stops of the herdr server, want 1", n)
	}
	evs, _ := g.r.Observe(g.ctx) // the server is down: the engine opens it again at once
	if len(only(evs, EventHerdrStopped)) != 1 {
		t.Fatalf("events %+v while the server is down, want the stop's", evs)
	}

	g.h.exitAtStart = "" // the reveal's fresh server reads the volume
	g.h.serverBack()
	if evs := g.observeN(3); len(evs) != 0 { // the row the stop ended is lost: the heal relaunches it, no ladder
		t.Fatalf("observe after the restart gave %+v, want nothing", evs)
	}
	if n := len(g.h.startCalls()); n != 1 {
		t.Fatalf("%d starts before the role's next prompt, want the launch alone", n)
	}
	g.send("fx-repro", "reproduce the bug")

	if n := len(g.h.startCalls()); n != 2 {
		t.Fatalf("%d starts, want the launch and one relaunch", n)
	}
	if n := g.h.stopCount(); n != 1 {
		t.Fatalf("%d stops of the herdr server, want 1", n)
	}
	if p := g.h.promptTexts(); p[len(p)-1] != "reproduce the bug" {
		t.Fatalf("prompts %q, want the first prompt to the relaunched agent", p)
	}
	got := g.herdrEvents()
	var stops, relaunches int
	for _, ev := range got {
		switch {
		case strings.HasPrefix(ev, "ok: stopped the factory's herdr server"):
			stops++
		case strings.HasPrefix(ev, "info: relaunched fx-repro"):
			relaunches++
		}
	}
	if stops != 1 || relaunches != 1 {
		t.Fatalf("herdr events %q, want one stop and one relaunch", got)
	}
}

// The observe pass that finds the pane lost before its first prompt, with
// the denial on its screen, heals it the same way.
func TestAPaneLostBeforeItsFirstPromptToTheDenialIsHealed(t *testing.T) {
	g := newRig(t)
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")

	var evs []Event
	for range 3 { // the server is down once the heal stopped it: those passes fail
		g.clock.Add(10 * time.Second)
		got, _ := g.r.Observe(g.ctx)
		evs = append(evs, got...)
	}
	if len(evs) != 1 || evs[0].Kind != EventHerdrStopped {
		t.Fatalf("observe gave %+v, want the heal's stop and no event for you", evs)
	}
	if n := g.h.stopCount(); n != 1 {
		t.Fatalf("%d stops of the herdr server, want 1", n)
	}
	if n := len(g.h.startCalls()); n != 1 {
		t.Fatalf("%d starts while the server is down, want the launch alone", n)
	}
}

// The denial that comes back after the one heal sends the task to you with
// one line, and nothing else restarts: no second stop, no relaunch. Your
// retry (you allowed herdr) starts the role's recoveries and the heal over:
// the role is relaunched, and its prompt goes.
func TestTheDenialAfterTheHealNeedsYouUntilYourRetry(t *testing.T) {
	g := newRig(t)
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); err == nil {
		t.Fatal("Send went through a pane that cannot read its copy")
	}
	g.h.serverBack() // the fresh server is denied the volume as well

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	d, ok := errors.AsType[*DeniedError](err)
	want := DeniedLine(volumeOf(g.copy), "Terminal") // [terminal] app's default
	if !ok || d.Error() != want {
		t.Fatalf("Send = %v, want %q", err, want)
	}
	if n, s := g.h.stopCount(), len(g.h.startCalls()); n != 1 || s != 2 {
		t.Fatalf("%d stops and %d starts, want one stop and one relaunch", n, s)
	}
	if evs := g.observeN(3); len(only(evs, EventLost)) != 0 || len(g.h.startCalls()) != 2 {
		t.Fatalf("observe gave %+v and %d starts: a role told to you was recovered", evs, len(g.h.startCalls()))
	}
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, &d) {
		t.Fatalf("Send again = %v, want the same line", err)
	}

	g.retried()
	g.h.exitAtStart = ""
	g.send("fx-repro", "reproduce the bug")
	if n, s := g.h.stopCount(), len(g.h.startCalls()); n != 1 || s != 3 {
		t.Fatalf("%d stops and %d starts after your retry, want one more relaunch", n, s)
	}
}

// After your retry the heal starts over: a relaunch macOS still denies
// gets one more fresh server.
func TestYourRetryStartsTheHealOver(t *testing.T) {
	g := newRig(t)
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")
	_, _ = g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	g.h.serverBack()
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*DeniedError)) {
		t.Fatalf("Send = %v, want the line for you", err)
	}
	g.observeN(3) // the denied pane is lost

	g.retried() // macOS still denies the server the volume
	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	if w, ok := errors.AsType[*WaitError](err); !ok || !strings.Contains(w.What, "restarts its herdr server") {
		t.Fatalf("Send after your retry = %v, want the heal's restart", err)
	}
	if n := g.h.stopCount(); n != 2 {
		t.Fatalf("%d stops after your retry, want one more", n)
	}
}

// An agent of another task live in the factory's herdr session holds the
// stop: the task waits, saying whose agent it waits for, and nothing is
// stopped; once that agent is gone, the next prompt's try heals.
func TestAnotherTasksLiveAgentHoldsTheServerStop(t *testing.T) {
	g := newRig(t)
	other := g.newTask("PR-12346")
	oa := g.begin(other, "investigate")
	if err := g.r.Launch(g.ctx, other, oa, Copy{Name: "factory2", Path: t.TempDir(), Number: 2}, g.roles("fx-repro")); err != nil {
		t.Fatal(err)
	}
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")

	for range 2 {
		_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
		w, ok := errors.AsType[*WaitError](err)
		if !ok || !strings.Contains(w.What, "PR-12346 fx-repro") || !strings.Contains(w.What, "cannot read") {
			t.Fatalf("Send = %v, want a wait that names PR-12346's agent and why", err)
		}
	}
	if n := g.h.stopCount(); n != 0 {
		t.Fatalf("%d stops while another task's agent lives in the session", n)
	}
	if got := g.herdrEvents(); len(got) != 1 || !strings.Contains(got[0], "PR-12346 fx-repro") {
		t.Fatalf("herdr events %q, want the wait said once", got)
	}

	if err := g.r.QuitRole(g.ctx, other, "fx-repro", "its stage ended"); err != nil {
		t.Fatal(err)
	}
	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	if w, ok := errors.AsType[*WaitError](err); !ok || !strings.Contains(w.What, "restarts its herdr server") {
		t.Fatalf("Send = %v once the other agent was gone, want the restart", err)
	}
	if n := g.h.stopCount(); n != 1 {
		t.Fatalf("%d stops once the other agent was gone, want 1", n)
	}
}

// A denial the daemon shares (it cannot read the copy either) is the
// copy's or its volume's, no herdr server's: nothing is stopped, the
// refused prompt is the stage's failure as before, and the lost pane takes
// the recoveries' steps: a relaunch at each, then the role is lost, to you.
func TestADenialTheDaemonSharesStopsNoServer(t *testing.T) {
	g := newRig(t)
	g.unreadable = true
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	if _, wait := errors.AsType[*WaitError](err); wait || !herdr.IsCode(err, herdr.CodeAgentNotFound) {
		t.Fatalf("Send = %v, want herdr's refusal as it is", err)
	}
	var evs []Event
	for n := 1; n <= MaxRecoveries; n++ {
		evs = append(evs, g.observeN(3)...)
		evs = append(evs, g.afterWait(n)...)
	}
	evs = append(evs, g.observeN(3)...)
	if n := g.h.stopCount(); n != 0 {
		t.Fatalf("%d stops of the herdr server for a copy the daemon cannot read", n)
	}
	if lost := only(evs, EventLost); len(lost) != 1 || len(g.h.startCalls()) != 1+MaxRecoveries {
		t.Fatalf("events %+v and %d starts, want a relaunch at each recovery, then lost", evs, len(g.h.startCalls()))
	}
}

// The line names the volume a copy is on, or the copy itself off a mounted
// volume.
func TestTheDenialNamesTheCopysVolume(t *testing.T) {
	for dir, want := range map[string]string{
		"/Volumes/Work/Projects/talkable.factory2": "/Volumes/Work",
		"/Volumes/Work": "/Volumes/Work",
		"/Users/zhuravel/Projects/talkable.factory2": "/Users/zhuravel/Projects/talkable.factory2",
	} {
		if got := volumeOf(dir); got != want {
			t.Errorf("volumeOf(%q) = %q, want %q", dir, got, want)
		}
	}
}

// Each CLI's refusal of its directory is read on its stable words; a
// line that only says "Operation not permitted" is not one.
func TestTheDenialIsReadOnItsStableWords(t *testing.T) {
	for screen, want := range map[string]bool{
		deniedScreen: true,
		"shell-init: error retrieving current directory: getcwd: cannot access parent directories: Operation not permitted\n$ ": true,
		"Error: Operation not permitted (os error 1)\n$ ":                                                                       true,
		"Error: Operation not permitted (os error 1)  \r\n$ ":                                                                   true,
		"Can't read the current directory (EPERM). Start Claude Code from a different directory.":                               true,
		"rm: /etc/hosts: Operation not permitted\n$ ":                                                                           false,
		"error: unexpected argument '--search' found\n":                                                                         false,
		"  └ rg: ./x: Operation not permitted (os error 1)\n":                                                                   false,
		"Error: Operation not permitted (os error 1) while reading ./x\n":                                                       false,
		"  Can't read the current directory (EPERM).\n":                                                                         false,
		"  ⎿  shell-init: error retrieving current directory: getcwd: Operation not permitted\n":                                false,
	} {
		if got := deniedLine(screen) != ""; got != want {
			t.Errorf("deniedLine(%q) found one: %v, want %v", screen, got, want)
		}
	}
}

// The line names the setting as macOS shows it: Removable Volumes under
// Files & Folders for a copy on a mounted volume (Full Disk Access where
// that toggle is absent), and the terminal app the reveal starts the
// server from, when it is one; for a copy elsewhere, Files & Folders.
func TestTheDenialNamesTheSettingAsMacOSShowsIt(t *testing.T) {
	for _, c := range []struct{ volume, app, want string }{
		{"/Volumes/Work", "iTerm2", "macOS blocks the factory's herdr server from reading /Volumes/Work: allow herdr and magnum (and " +
			"iTerm2, which starts the server) in System Settings > Privacy & Security > Files & Folders > Removable Volumes " +
			"(or Full Disk Access, if that toggle is absent), then retry"},
		{"/Users/zhuravel/Projects/talkable.factory2", "custom", "macOS blocks the factory's herdr server from reading " +
			"/Users/zhuravel/Projects/talkable.factory2: allow herdr and magnum in System Settings > Privacy & Security > Files & " +
			"Folders (or Full Disk Access, if that toggle is absent), then retry"},
	} {
		if got := DeniedLine(c.volume, c.app); got != c.want {
			t.Errorf("DeniedLine(%q, %q) =\n%s\nwant\n%s", c.volume, c.app, got, c.want)
		}
	}
	held := &DeniedError{Volume: "/Volumes/Work", App: "iTerm2", Held: "a pane of yours, w3:p2, runs ruby bin/rails c"}
	want := "macOS blocks the factory's herdr server from reading /Volumes/Work (System Settings > Privacy & Security > Files & " +
		"Folders > Removable Volumes), and its restart waited 30 minutes for a pane of yours, w3:p2, runs ruby bin/rails c: " +
		"close or end it, then retry, and the factory restarts the server"
	if got := held.Error(); got != want {
		t.Errorf("the line after a hold =\n%s\nwant\n%s", got, want)
	}
}

// A factory whose herdr session is yours never has it stopped: its denial
// goes to you at once.
func TestAFactorySessionThatIsYoursIsNeverStopped(t *testing.T) {
	g := newRig(t)
	g.cfg.Factory.HerdrSession = "default"
	g.restart()
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	if _, ok := errors.AsType[*DeniedError](err); !ok {
		t.Fatalf("Send = %v, want the line for you", err)
	}
	if n := g.h.stopCount(); n != 0 {
		t.Fatalf("%d stops of your own herdr session", n)
	}
}

// A role whose pane is lost before its first prompt (its CLI exited at its
// start) is relaunched at each of its recoveries, each after its wait, then
// lost: the task goes to you. Your retry starts its recoveries over, so the
// next prompt relaunches it and goes.
func TestALostRoleIsRelaunchedAtEachRecoveryThenOnYourRetry(t *testing.T) {
	g := newRig(t)
	g.h.exitAtStart = "error: unexpected argument '--search' found\n"
	g.launch("fx-repro")
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); err == nil {
		t.Fatal("Send went through to an agent that exited")
	}

	var evs []Event
	for n := 1; n <= MaxRecoveries; n++ {
		evs = append(evs, g.observeN(3)...)
		evs = append(evs, g.afterWait(n)...)
	}
	evs = append(evs, g.observeN(3)...)
	if lost := only(evs, EventLost); len(lost) != 1 || lost[0].Reason != "no progress after 3 recoveries" {
		t.Fatalf("events %+v, want lost after 3 recoveries", evs)
	}
	if n := len(g.h.startCalls()); n != 1+MaxRecoveries {
		t.Fatalf("%d starts, want the launch and a relaunch at each recovery", n)
	}
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.Is(err, agents.ErrNoSession) {
		t.Fatalf("Send before your retry = %v, want it held: the role is yours", err)
	}
	if n := len(g.h.startCalls()); n != 1+MaxRecoveries {
		t.Fatalf("%d starts before your retry, want %d", n, 1+MaxRecoveries)
	}

	g.retried()
	g.h.exitAtStart = "" // you fixed the CLI
	g.send("fx-repro", "reproduce the bug")
	if n := len(g.h.startCalls()); n != 2+MaxRecoveries {
		t.Fatalf("%d starts after your retry, want one more relaunch", n)
	}
	if p := g.h.promptTexts(); !slices.Contains(p, "reproduce the bug") {
		t.Fatalf("prompts %q, want the first prompt to the relaunched agent", p)
	}
}

// A role parked while its task waited on you gets its conversation resumed
// on the stage's next prompt, which goes as after a launch: no prompt of a
// recovery's own comes first.
func TestAParkedRolesNextPromptResumesItsConversation(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observe()
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.observeN(2)
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatal(err)
	}
	starts, prompts := len(g.h.startCalls()), len(g.h.promptTexts())

	g.send("fx-repro", "the operator answered")

	s := g.h.startCalls()
	if len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts %+v, want sid-1 resumed", s[starts:])
	}
	if p := g.h.promptTexts()[prompts:]; !slices.Equal(p, []string{"the operator answered"}) {
		t.Fatalf("prompts %q after the park, want the stage's alone", p)
	}
}

// parkedAfterATurn launches the role named name with conversation sid,
// sends it a prompt, ends its turn and parks the task, as the memory hold
// parks a task that waits on you.
func (g *rig) parkedAfterATurn(name, sid string) {
	g.t.Helper()
	g.launch(name)
	g.h.setSession(g.agent(name), sid)
	g.send(name, "reproduce the bug")
	g.endTurnAndPark(name)
}

// endTurnAndPark ends the turn of the role named name and parks the task.
func (g *rig) endTurnAndPark(name string) {
	g.t.Helper()
	g.observe()
	g.h.setStatus(g.agent(name), herdr.StatusIdle)
	if lost := only(g.observeN(2), EventLost); len(lost) != 0 {
		g.t.Fatalf("a turn's end gave %+v", lost)
	}
	if err := g.r.Park(g.ctx, g.task); err != nil {
		g.t.Fatal(err)
	}
}

// A role parked three times in one attempt (the Mac short on memory while
// its task waits on you) is resumed from the same conversation after each
// of your answers, and none of it counts against the lost-pane ladder: no
// fresh session, no EventLost, and a real loss after it still resumes.
func TestAParkedRoleIsResumedOnEachAnswerOffTheLostPaneLadder(t *testing.T) {
	g := newRig(t)
	g.parkedAfterATurn("fx-repro", "sid-1")
	for i := range 3 {
		starts := len(g.h.startCalls())
		g.send("fx-repro", fmt.Sprintf("the operator answered %d", i+1))
		s := g.h.startCalls()
		if len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
			t.Fatalf("answer %d: starts %+v, want sid-1 resumed", i+1, s[starts:])
		}
		g.endTurnAndPark("fx-repro")
	}
	g.send("fx-repro", "the operator answered 4")
	g.observe()
	g.h.losePane(g.agent("fx-repro"))
	starts := len(g.h.startCalls())
	if evs := append(g.observe(), g.afterWait(1)...); len(only(evs, EventLost)) != 0 {
		t.Fatalf("a real loss after the parks gave %+v", evs)
	}
	if s := g.h.startCalls(); len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts %+v after a real loss, want its first resume", s[starts:])
	}
}

// A role started again before its prompt passes every hold a launch
// passes: the engine's (a disk or memory hold, a drain for an install),
// with its words, and the daemon's own drain on SIGTERM; nothing starts
// meanwhile.
func TestAHoldKeepsBringBackFromStartingARole(t *testing.T) {
	g := newRig(t)
	g.parkedAfterATurn("fx-repro", "sid-1")
	starts := len(g.h.startCalls())

	g.hold("memory", "the Mac's memory pressure is critical")
	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "the operator answered")
	if w, ok := errors.AsType[*WaitError](err); !ok || w.Who != "memory" || w.What != "the Mac's memory pressure is critical" {
		t.Fatalf("Send under the engine's hold = %v, want its wait", err)
	}
	g.hold("", "")
	g.r.BeginDrain()
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "the operator answered"); !errors.Is(err, ErrDraining) {
		t.Fatalf("Send while the daemon drains = %v, want ErrDraining", err)
	}
	if n := len(g.h.startCalls()); n != starts {
		t.Fatalf("%d starts under the holds", n-starts)
	}
}

// Roles started again before their prompts take turns Stagger apart
// while the Mac's memory pressure is at warn, as launches do.
func TestBringBacksAtMemoryWarnAreStaggered(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro", "fx-history")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.h.setSession(g.agent("fx-history"), "sid-2")
	g.send("fx-repro", "reproduce the bug")
	g.send("fx-history", "read the history")
	g.observe()
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.h.setStatus(g.agent("fx-history"), herdr.StatusIdle)
	g.observeN(2)
	if err := g.r.Park(g.ctx, g.task); err != nil {
		t.Fatal(err)
	}
	g.setMemory(engine.MemoryWarn)
	slept := len(g.sleeps())
	g.send("fx-repro", "the operator answered")
	g.send("fx-history", "the operator answered")
	if got := g.sleeps()[slept:]; !slices.Contains(got, Stagger) {
		t.Fatalf("sleeps %v, want the second start Stagger after the first", got)
	}
}

// What the pane showed before this launch (a shell's earlier output, the
// scrollback herdr restores) is no denial: an agent that exits for another
// reason stops no server.
func TestOldScrollbackIsNoDenial(t *testing.T) {
	g := newRig(t)
	g.h.paneScreen = "Error: Operation not permitted (os error 1)\nzhuravel@mac talkable.factory2 % "
	g.h.exitAtStart = "codex --search\nerror: the argument '--search' cannot be used multiple times\n"
	g.launch("fx-repro")

	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); err == nil {
		t.Fatal("Send went through to an agent that exited")
	}
	g.observeN(3)
	if n := g.h.stopCount(); n != 0 {
		t.Fatalf("%d stops for a refusal from before the launch", n)
	}
}

// The command line of a pane of yours that holds the stop goes into the
// task's wait and its herdr events scrubbed (scrub.Text), as all the text
// the factory keeps: execx.Redact left the value of PGPASSWORD=x psql.
func TestAPaneOfYoursThatHoldsTheStopIsNamedWithoutItsSecret(t *testing.T) {
	g := newRig(t)
	pane := g.h.yourPane("PGPASSWORD=pgpw-env psql -h db")
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	w, ok := errors.AsType[*WaitError](err)
	if !ok || !strings.Contains(w.What, "a pane of yours, "+pane+", runs PGPASSWORD=<redacted> psql") {
		t.Fatalf("Send = %v, want a wait that names your pane's command, scrubbed", err)
	}
	for _, text := range append(g.herdrEvents(), w.What) {
		if strings.Contains(text, "pgpw-env") {
			t.Fatalf("%q holds the pane's password", text)
		}
	}
}

// A pane of yours in the factory's session that runs something (no agent
// of the factory's: a console, a server, an editor) holds the stop, and the
// task's wait names it; once it is an idle shell, the next prompt heals.
func TestAPaneOfYoursInTheSessionHoldsTheServerStop(t *testing.T) {
	g := newRig(t)
	pane := g.h.yourPane("ruby bin/rails c")
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	if w, ok := errors.AsType[*WaitError](err); !ok || !strings.Contains(w.What, "a pane of yours, "+pane+", runs ruby bin/rails c") {
		t.Fatalf("Send = %v, want a wait that names your pane", err)
	}
	if n := g.h.stopCount(); n != 0 {
		t.Fatalf("%d stops while your pane runs a console", n)
	}
	g.h.idlePane(pane)
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*WaitError)) {
		t.Fatalf("Send = %v once your pane is idle, want the restart's wait", err)
	}
	if n := g.h.stopCount(); n != 1 {
		t.Fatalf("%d stops once your pane is idle, want 1", n)
	}
}

// Two tasks denied by one server get one stop: the second task's role,
// launched before the stop, is relaunched on the fresh server, and a
// fresh server that is denied too goes to you without a second stop.
func TestTwoTasksDeniedTogetherStopTheServerOnce(t *testing.T) {
	g := newRig(t)
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")
	other := g.newTask("PR-12346")
	oa := g.begin(other, "investigate")
	if err := g.r.Launch(g.ctx, other, oa, Copy{Name: "factory2", Path: g.copy, Number: 2}, g.roles("fx-repro")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*WaitError)) {
		t.Fatalf("Send = %v, want the restart's wait", err)
	}
	g.h.serverBack()

	var err error
	for range 3 {
		if _, err = g.r.Send(g.ctx, other, oa, g.role("fx-repro"), "reproduce the bug"); errors.As(err, new(*DeniedError)) {
			break
		}
	}
	if !errors.As(err, new(*DeniedError)) {
		t.Fatalf("the other task's Send = %v, want the line once its relaunch is denied too", err)
	}
	if n := g.h.stopCount(); n != 1 {
		t.Fatalf("%d stops for two tasks on one server, want 1", n)
	}
}

// A task launched on the fresh server after another task's stop, and
// denied too, goes to you at once: one stop serves every task for
// stopWindow.
func TestADenialOnTheFreshServerOfAnotherTasksStopGoesToYou(t *testing.T) {
	g := newRig(t)
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*WaitError)) {
		t.Fatalf("Send = %v, want the restart's wait", err)
	}
	g.h.serverBack()
	g.clock.Add(time.Minute)

	other := g.newTask("PR-12346")
	oa := g.begin(other, "investigate")
	if err := g.r.Launch(g.ctx, other, oa, Copy{Name: "factory2", Path: g.copy, Number: 2}, g.roles("fx-repro")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.Send(g.ctx, other, oa, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*DeniedError)) {
		t.Fatalf("the other task's Send = %v, want the line at once", err)
	}
	if n := g.h.stopCount(); n != 1 {
		t.Fatalf("%d stops for two tasks within the window, want 1", n)
	}
}

// A server restarted by somebody else while the heal waited (you, from
// your terminal) is not stopped again: the role is relaunched on it.
func TestAServerRestartedMeanwhileIsNotStoppedAgain(t *testing.T) {
	g := newRig(t)
	other := g.newTask("PR-12346")
	oa := g.begin(other, "investigate")
	if err := g.r.Launch(g.ctx, other, oa, Copy{Name: "factory2", Path: t.TempDir(), Number: 2}, g.roles("fx-repro")); err != nil {
		t.Fatal(err)
	}
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*WaitError)) {
		t.Fatalf("Send = %v, want the wait on the other task's agent", err)
	}

	g.h.exitAtStart = ""
	g.h.restartedElsewhere()
	starts := len(g.h.startCalls())
	for range 2 {
		_, _ = g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	}
	if n := g.h.stopCount(); n != 0 {
		t.Fatalf("%d stops of a server restarted meanwhile", n)
	}
	if n := len(g.h.startCalls()); n != starts+1 {
		t.Fatalf("%d starts after the restart, want the role relaunched", n-starts)
	}
}

// This task's own liaison holds the stop with words of its own (you are
// talking with it), and never for good: after holdLimit the task goes to
// you with a line that names it, not the privacy setting alone.
func TestThisTasksOwnLiaisonHoldsTheStopThenGoesToYou(t *testing.T) {
	g := newRig(t)
	if _, err := g.r.Talk(g.ctx, g.task, talkPrompt); err != nil {
		t.Fatal(err)
	}
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	if w, ok := errors.AsType[*WaitError](err); !ok || !strings.Contains(w.What, "this task's liaison") || strings.Contains(w.What, "another task") {
		t.Fatalf("Send = %v, want a wait on this task's liaison in its own words", err)
	}
	g.clock.Add(holdLimit + time.Minute)
	_, err = g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	if d, ok := errors.AsType[*DeniedError](err); !ok || !strings.Contains(d.Error(), "waited 30 minutes for this task's liaison: "+
		"exit the liaison, then retry") || strings.Contains(d.Error(), "allow herdr") {
		t.Fatalf("Send after %v = %v, want the line that names the liaison", holdLimit, err)
	}
	if n := g.h.stopCount(); n != 0 {
		t.Fatalf("%d stops under your talk", n)
	}
}

// A talk that starts a liaison while the heal would stop the server holds
// the stop: the new liaison would die in it.
func TestATalkInFlightHoldsTheServerStop(t *testing.T) {
	g := newRig(t)
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")
	g.r.talkMu.Lock()
	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	g.r.talkMu.Unlock()
	if w, ok := errors.AsType[*WaitError](err); !ok || !strings.Contains(w.What, "talk") {
		t.Fatalf("Send = %v, want a wait on the talk", err)
	}
	if n := g.h.stopCount(); n != 0 {
		t.Fatalf("%d stops during a talk", n)
	}
}

// A herdr without server.stop (before 0.9.5) cannot be healed: the line
// goes to you at once.
func TestAHerdrWithoutServerStopSendsTheLineAtOnce(t *testing.T) {
	g := newRig(t)
	g.h.stopErr = &herdr.Error{Method: "server.stop", Code: herdr.CodeUnknownMethod, Message: "server.stop"}
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*DeniedError)) {
		t.Fatalf("Send = %v, want the line for you", err)
	}
}

// Once you retried after the line, the heal you were told of holds the
// role no longer: a lost pane takes the recoveries' steps again before any
// prompt.
func TestARetryAfterTheLineLetsTheRecoveriesTakeALostPane(t *testing.T) {
	g := newRig(t)
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")
	_, _ = g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	g.h.serverBack()
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*DeniedError)) {
		t.Fatalf("Send = %v, want the line", err)
	}
	if g.r.healing(g.ctx, g.task.ID, g.attempt.ID, "fx-repro") == nil {
		t.Fatal("the heal told to you holds the role no longer before your retry")
	}
	g.retried()
	if g.r.healing(g.ctx, g.task.ID, g.attempt.ID, "fx-repro") != nil {
		t.Fatal("the heal told to you still holds the role after your retry")
	}
}

// One read of a copy that macOS holds is in flight at a time: a second
// check while it blocks answers at once, starting no second read, and the
// read that comes back is taken.
func TestABlockedReadOfTheCopyHoldsOneProbe(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	reads := 0
	p := newDirProber(func(string) error {
		mu.Lock()
		reads++
		mu.Unlock()
		<-release
		return nil
	}, 20*time.Millisecond)
	for range 2 {
		if p.readable("/Volumes/Work/x") {
			t.Fatal("a check while the read blocks counted as readable")
		}
	}
	mu.Lock()
	n := reads
	mu.Unlock()
	if n != 1 {
		t.Fatalf("%d reads while the first blocks, want 1", n)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for !p.readable("/Volumes/Work/x") {
		if time.Now().After(deadline) {
			t.Fatal("the read that came back is never taken")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// What a launch printed is told from what the pane showed before it by
// their overlap: the pane's earlier lines are cut, even when the launch
// prints the very same lines again, and a screen whose old lines scrolled
// off is the launch's whole.
func TestSinceLaunchCutsWhatThePaneShowedBefore(t *testing.T) {
	before := "Error: Operation not permitted (os error 1)\nzhuravel@mac talkable.factory2 % "
	for _, c := range []struct{ name, before, screen, want string }{
		{"a fresh pane", "", "Can't read the current directory (EPERM).\n", "Can't read the current directory (EPERM).\n"},
		{"earlier output", before, before + "codex --search\nerror: bad flag\n", "zhuravel@mac talkable.factory2 % codex --search\nerror: bad flag\n"},
		{"the same lines again", deniedScreen, deniedScreen + deniedScreen,
			"zhuravel@mac talkable.factory2 % " + deniedScreen},
		{"scrolled off", before, "Can't read the current directory (EPERM).\n$ ", "Can't read the current directory (EPERM).\n$ "},
		{"a read that ends with a newline", before + "\n", before + "codex --search\nerror: bad flag\n",
			"zhuravel@mac talkable.factory2 % codex --search\nerror: bad flag\n"},
		{"a read padded with blank rows", before + "\n\n\n", before + "codex --search\nerror: bad flag\n\n\n",
			"zhuravel@mac talkable.factory2 % codex --search\nerror: bad flag\n\n\n"},
	} {
		if got := sinceLaunch(c.screen, paneMark(c.before)); got != c.want {
			t.Errorf("%s: sinceLaunch = %q, want %q", c.name, got, c.want)
		}
	}
	if deniedLine(sinceLaunch(deniedScreen+deniedScreen, paneMark(deniedScreen))) == "" {
		t.Error("a launch that printed the refusal again is no denial")
	}
	for _, pad := range []string{"\n", "\n\n\n"} {
		if got := paneMark(before + pad); got != "Error: Operation not permitted (os error 1)" {
			t.Errorf("paneMark of a read padded with %q = %q, want the old line alone", pad, got)
		}
	}
}

// A pre-read of the pane that fails leaves no mark: that launch says
// nothing of a refusal (the recoveries take it), never the whole
// scrollback, old lines included.
func TestAFailedPreReadOfThePaneSaysNothingOfThatLaunch(t *testing.T) {
	g := newRig(t)
	g.h.paneScreen = "Error: Operation not permitted (os error 1)\nzhuravel@mac talkable.factory2 % "
	g.h.exitAtStart = "codex --search\nerror: the argument '--search' cannot be used multiple times\n"
	g.h.failReads(errors.New("herdr pane.read: timeout"))
	g.launch("fx-repro")
	g.h.failReads(nil)

	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); err == nil {
		t.Fatal("Send went through to an agent that exited")
	}
	g.observeN(3)
	if n := g.h.stopCount(); n != 0 {
		t.Fatalf("%d stops for a launch whose pane was not read before it", n)
	}
}

// While the stop waits on something else in the session, no prompt's try
// runs the CLI's login check or reads the budget: those come once nothing
// holds the stop, right before it.
func TestAWaitingStopRunsNoLoginCheck(t *testing.T) {
	g := newRig(t)
	other := g.newTask("PR-12346")
	oa := g.begin(other, "investigate")
	if err := g.r.Launch(g.ctx, other, oa, Copy{Name: "factory2", Path: t.TempDir(), Number: 2}, g.roles("fx-repro")); err != nil {
		t.Fatal(err)
	}
	g.h.exitAtStart = deniedScreen
	g.launch("fx-repro")
	checks := len(g.run.CallsWithPrefix("claude", "auth", "status"))
	for range 3 {
		if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*WaitError)) {
			t.Fatalf("Send = %v, want the wait on the other task's agent", err)
		}
	}
	if n := len(g.run.CallsWithPrefix("claude", "auth", "status")) - checks; n != 0 {
		t.Fatalf("%d login checks while the stop waited, want none", n)
	}
	if err := g.r.QuitRole(g.ctx, other, "fx-repro", "its stage ended"); err != nil {
		t.Fatal(err)
	}
	_, _ = g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
	if n := len(g.run.CallsWithPrefix("claude", "auth", "status")) - checks; n != 1 || g.h.stopCount() != 1 {
		t.Fatalf("%d login checks and %d stops once nothing held it, want one of each", n, g.h.stopCount())
	}
}

// Whatever else holds the stop (a pane of yours that runs something,
// another task's agent) holds it for holdLimit at most, as the liaison
// does: then the task goes to you with a line that names what held it,
// not the privacy setting alone.
func TestWhatHoldsTheStopHoldsItHalfAnHourThenGoesToYou(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(g *rig) string
	}{
		{"a pane of yours", func(g *rig) string {
			return "a pane of yours, " + g.h.yourPane("ruby bin/rails c") + ", runs ruby bin/rails c"
		}},
		{"another task's agent", func(g *rig) string {
			other := g.newTask("PR-12346")
			oa := g.begin(other, "investigate")
			if err := g.r.Launch(g.ctx, other, oa, Copy{Name: "factory2", Path: g.t.TempDir(), Number: 2}, g.roles("fx-repro")); err != nil {
				g.t.Fatal(err)
			}
			return "PR-12346 fx-repro"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t)
			holder := c.setup(g)
			g.h.exitAtStart = deniedScreen
			g.launch("fx-repro")
			if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*WaitError)) {
				t.Fatalf("Send = %v, want the wait", err)
			}
			g.clock.Add(holdLimit - time.Minute)
			if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.As(err, new(*WaitError)) {
				t.Fatalf("Send before %v = %v, want the wait still", holdLimit, err)
			}
			g.clock.Add(2 * time.Minute)
			_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug")
			d, ok := errors.AsType[*DeniedError](err)
			if !ok || !strings.Contains(d.Error(), "waited 30 minutes for "+holder+": close or end it, then retry") ||
				!strings.Contains(d.Error(), "Files & Folders > Removable Volumes") && strings.HasPrefix(g.copy, "/Volumes/") ||
				strings.Contains(d.Error(), "allow herdr") {
				t.Fatalf("Send after %v = %v, want the line that names %s", holdLimit, err, holder)
			}
			if n := g.h.stopCount(); n != 0 {
				t.Fatalf("%d stops while it was held", n)
			}
		})
	}
}
