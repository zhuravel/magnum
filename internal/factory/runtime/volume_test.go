package runtime

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
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
	want := "macOS blocks the factory's herdr server from reading " + volumeOf(g.copy) + ": allow herdr (and magnum) for removable volumes in " +
		"System Settings > Privacy & Security > Files and Folders, then retry"
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
// the recoveries' steps: one relaunch, then the role is lost, to you.
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
	for range 3 {
		evs = append(evs, g.observeN(3)...)
	}
	if n := g.h.stopCount(); n != 0 {
		t.Fatalf("%d stops of the herdr server for a copy the daemon cannot read", n)
	}
	if lost := only(evs, EventLost); len(lost) != 1 || len(g.h.startCalls()) != 2 {
		t.Fatalf("events %+v and %d starts, want one relaunch, then lost", evs, len(g.h.startCalls()))
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
		"Can't read the current directory (EPERM). Start Claude Code from a different directory.":                               true,
		"rm: /etc/hosts: Operation not permitted\n$ ":                                                                           false,
		"error: unexpected argument '--search' found\n":                                                                         false,
	} {
		if got := deniedLine(screen) != ""; got != want {
			t.Errorf("deniedLine(%q) found one: %v, want %v", screen, got, want)
		}
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
// start) is relaunched once, then lost: the task goes to you. Your retry
// starts its recoveries over, so the next prompt relaunches it and goes.
func TestALostRoleIsRelaunchedOnceThenOnYourRetry(t *testing.T) {
	g := newRig(t)
	g.h.exitAtStart = "error: unexpected argument '--search' found\n"
	g.launch("fx-repro")
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); err == nil {
		t.Fatal("Send went through to an agent that exited")
	}

	var evs []Event
	for range 3 {
		evs = append(evs, g.observeN(3)...)
	}
	if lost := only(evs, EventLost); len(lost) != 1 || lost[0].Reason != "no progress after 1 recoveries" {
		t.Fatalf("events %+v, want lost after 1 recovery", evs)
	}
	if n := len(g.h.startCalls()); n != 2 {
		t.Fatalf("%d starts, want the launch and one relaunch", n)
	}
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "reproduce the bug"); !errors.Is(err, agents.ErrNoSession) {
		t.Fatalf("Send before your retry = %v, want it held: the role is yours", err)
	}
	if n := len(g.h.startCalls()); n != 2 {
		t.Fatalf("%d starts before your retry, want 2", n)
	}

	g.retried()
	g.h.exitAtStart = "" // you fixed the CLI
	g.send("fx-repro", "reproduce the bug")
	if n := len(g.h.startCalls()); n != 3 {
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
