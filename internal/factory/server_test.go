package factory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// A copy's server runs the repository's server command in the copy with the
// copy's env, once however often it is started, and its pid and start are
// recorded; a stop ends its process group and forgets it.
func TestStartServerRecordsThePidAndTheStartOfTheCopysServer(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	for range 2 {
		if err := r.c.StartServer(r.ctx, c); err != nil {
			t.Fatalf("StartServer: %v", err)
		}
	}
	if len(r.spawned) != 1 {
		t.Fatalf("spawned %+v, want one server however often it was started", r.spawned)
	}
	s := r.spawned[0]
	if s.Dir != c.Path || s.Command != "bin/worktree-run" || s.Env["REDIS_DB_OFFSET"] != "16" || s.Env["WT_BRANCH"] != "factory1" {
		t.Fatalf("spawned %+v, want bin/worktree-run in %s with the copy's env", s, c.Path)
	}
	rec, ok, err := r.c.serverOf(r.ctx, c.Name)
	if err != nil || !ok || rec.PID != s.PID || rec.Start != lstart(r.clock.Now()) {
		t.Fatalf("record = %+v, %v, %v; want pid %d started %s", rec, ok, err, s.PID, lstart(r.clock.Now()))
	}
	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatalf("StopServer: %v", err)
	}
	if want := []string{strconv.Itoa(s.PID) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want %v", r.kills, want)
	}
	if _, ok, _ := r.c.serverOf(r.ctx, c.Name); ok {
		t.Fatal("a stopped server is still recorded")
	}
}

// After a restart every recorded server is a stray (no stage owns it):
// Reconcile kills one whose pid and start both match the record, and leaves
// alone a process that took a dead server's pid, with another start. Both
// records are forgotten.
func TestReconcileKillsAStrayServerOnlyWhenPidAndStartTimeMatch(t *testing.T) {
	r := newCopiesRig(t, 2)
	one, two := r.claim(r.task("PR-1")), r.claim(r.task("PR-2"))
	for _, c := range []Copy{one, two} {
		if err := r.c.StartServer(r.ctx, c); err != nil {
			t.Fatalf("StartServer %s: %v", c.Name, err)
		}
	}
	stray, dead := r.spawned[0].PID, r.spawned[1].PID
	r.mu.Lock()
	for i, p := range r.table {
		if p.PID == dead { // the server died; another process got its pid later
			r.table[i] = execx.Process{PID: dead, PPID: 1, PGID: dead, Start: "Thu Nov  5 10:00:00 2026", Name: "postgres"}
		}
	}
	r.mu.Unlock()

	r.c = r.newCopies() // a restart: nothing in memory
	if err := r.c.Reconcile(r.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if want := []string{strconv.Itoa(stray) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want only the stray %d (pid %d is another process now)", r.kills, stray, dead)
	}
	for _, c := range []Copy{one, two} {
		if _, ok, _ := r.c.serverOf(r.ctx, c.Name); ok {
			t.Errorf("%s's server is still recorded after the reconcile", c.Name)
		}
	}
	if err := r.c.StopServer(r.ctx, two); err != nil || len(r.kills) != 1 {
		t.Fatalf("StopServer of a forgotten server = %v, kills %v; want nothing killed", err, r.kills)
	}
}

// A server the daemon spawned just before it crashed, before its record,
// is found after the restart by the pid it wrote itself and its start (the
// pidfile's time): Reconcile ends its group. A pid a later process took
// (its start is another) is never killed. Both pidfiles are forgotten.
func TestReconcileFindsAServerStartedJustBeforeACrash(t *testing.T) {
	r := newCopiesRig(t, 2)
	one, two := r.claim(r.task("PR-1")), r.claim(r.task("PR-2"))
	var pids []int
	for _, c := range []Copy{one, two} { // the crash point: spawned, never recorded
		pid, err := r.c.spawn(r.ctx, c.Path, nil, "bin/worktree-run", r.c.serverLog(c.Name), r.c.serverPidFile(c.Name))
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	r.mu.Lock()
	for i, p := range r.table {
		if p.PID == pids[1] { // that server died; a later process got its pid
			r.table[i] = execx.Process{PID: pids[1], PPID: 1, PGID: pids[1], Start: lstart(r.clock.Now().Add(time.Hour)), Name: "postgres"}
		}
	}
	r.mu.Unlock()

	r.c = r.newCopies()
	if err := r.c.Reconcile(r.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if want := []string{strconv.Itoa(pids[0]) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want only the unrecorded server %d", r.kills, pids[0])
	}
	for _, c := range []Copy{one, two} {
		if _, err := os.Stat(r.c.serverPidFile(c.Name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s's pidfile is still there: %v", c.Name, err)
		}
	}
}

// A stop signals the server's whole group once its leader is the recorded
// server, and waits for every member, not the leader only: a member that
// ignores SIGTERM gets SIGKILL after the grace.
func TestAServerStopEndsTheMembersItsLeaderLeaves(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	leader := r.spawned[0].PID
	r.mu.Lock()
	r.table = append(r.table, execx.Process{PID: leader + 100, PPID: leader, PGID: leader, Start: lstart(r.clock.Now()), Name: "ruby"})
	r.ignoreTerm = map[int]bool{leader + 100: true}
	r.mu.Unlock()

	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	if want := []string{strconv.Itoa(leader) + " terminated", strconv.Itoa(leader) + " killed"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want %v", r.kills, want)
	}
	if slices.ContainsFunc(r.table, func(p execx.Process) bool { return p.PGID == leader }) {
		t.Fatalf("a member of the server's group still runs: %+v", r.table)
	}
}

// A copy's server finds the factory's `open` before the system's, and has
// BROWSER=true, so nothing it runs opens a tab in your browser; the
// variables the copies never inherit stay out.
func TestTheServersPathFindsTheOpenShimFirst(t *testing.T) {
	env := serverEnv([]string{"PATH=/usr/bin:/bin", "GH_TOKEN=x", "BROWSER=open", "LANG=C"}, "/state/factory-shims", []string{"GH_TOKEN"})
	for _, want := range []string{"PATH=/state/factory-shims:/usr/bin:/bin", "BROWSER=true", "LANG=C"} {
		if !slices.Contains(env, want) {
			t.Errorf("env %v has no %s", env, want)
		}
	}
	for _, key := range []string{"PATH=", "BROWSER="} {
		if n := len(slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return !strings.HasPrefix(kv, key) })); n != 1 {
			t.Errorf("env %v has %d %s, want one", env, n, key)
		}
	}
	if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "GH_TOKEN=") }) {
		t.Errorf("env %v keeps GH_TOKEN", env)
	}
	if env := serverEnv(nil, "/s", nil); !slices.Contains(env, "PATH=/s") {
		t.Errorf("env with no PATH = %v, want PATH=/s", env)
	}
}

// A copy's server starts with BROWSER=true and the factory's `open` shim
// written first, which refuses to open anything and exits 0, so a server
// that opens a page at its start goes on without a tab in your browser.
func TestAServerStartsWithNoWayToOpenABrowserTab(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	if got := r.spawned[0].Env["BROWSER"]; got != "true" {
		t.Errorf("the server's env has BROWSER=%q, want true", got)
	}
	shim := filepath.Join(r.cfg.Layout.State(), "factory-shims", "open")
	if fi, err := os.Stat(shim); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("the open shim = %v, %v; want it at %s, mode 0755", fi, err, shim)
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(r.ctx, "/bin/sh", shim, "http://x")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the shim exits %v, want 0", err)
	}
	if !strings.Contains(stderr.String(), "refused") || !strings.Contains(stderr.String(), "http://x") {
		t.Errorf("the shim says %q, want that it refused open http://x", stderr.String())
	}
}

// qaRepo gives the rig's repository a [factory.repo.qa] with serverStop.
func (r *copiesRig) qaRepo(serverStop string) {
	r.cfg.Factory.Repos[0].QA = &config.FactoryQA{When: "bin/user-facing", URL: "http://app.example.localhost:{port}",
		ServerStop: serverStop}
}

// stopEvents are the kinds and phases of copy name's server and stop
// events, oldest first: "factory.server_stop begin", "factory.server_stopped".
func (r *copiesRig) stopEvents(name string) []string {
	r.t.Helper()
	evs, err := r.st.EventsBySubject(r.ctx, "slot:"+name, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if strings.HasPrefix(e.Kind, "factory.server_stop") || strings.HasPrefix(e.Kind, "factory.server_stray") {
			out = append(out, strings.TrimSpace(e.Kind+" "+store.Deref(e.Phase)))
		}
	}
	return out
}

// eventsSay reports whether one of copy name's events says text.
func (r *copiesRig) eventsSay(name, kind, text string) bool {
	r.t.Helper()
	evs, err := r.st.EventsBySubject(r.ctx, "slot:"+name, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return slices.ContainsFunc(evs, func(e store.Event) bool { return e.Kind == kind && strings.Contains(e.Message, text) })
}

func (r *copiesRig) stopOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.order)
}

func (r *copiesRig) signalled() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.signals)
}

// The repository's server_stop (overmind's own stop, whose tmux server
// escapes the group) runs in the copy, bounded, before the server's group
// is killed, and the events say so in that order.
func TestAServerStopRunsServerStopBeforeTheGroupKill(t *testing.T) {
	r := newCopiesRig(t, 1)
	r.qaRepo("bin/overmind-kill")
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	leader := r.spawned[0].PID
	r.mu.Lock()
	r.order = nil
	r.mu.Unlock()

	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	if want := []string{"mise bin/overmind-kill", fmt.Sprintf("kill %d terminated", leader)}; !slices.Equal(r.stopOrder(), want) {
		t.Fatalf("the stop ran %v, want %v", r.stopOrder(), want)
	}
	calls := r.calls("bin/overmind-kill", c.Path)
	if len(calls) != 1 || calls[0].Timeout != 30*time.Second || calls[0].Env["WT_BRANCH"] != "factory1" {
		t.Fatalf("server_stop ran %+v, want once in %s with the copy's env, bounded by 30s", calls, c.Path)
	}
	want := []string{"factory.server_stop begin", "factory.server_stop_command begin", "factory.server_stop_command ok",
		"factory.server_stopped", "factory.server_stop ok"}
	if got := r.stopEvents(c.Name); !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// A server_stop that fails is an event, and the stop goes on: the group is
// still killed.
func TestAFailingServerStopStillKillsTheGroup(t *testing.T) {
	r := newCopiesRig(t, 1)
	r.qaRepo("bin/worktree-setup") // the rig's setup fails while failSetup counts
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	leader := r.spawned[0].PID
	r.failSetup[c.Name] = 1
	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	if want := []string{strconv.Itoa(leader) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want %v after a failed server_stop", r.kills, want)
	}
	if got := r.stopEvents(c.Name); !slices.Contains(got, "factory.server_stop_command fail") || !slices.Contains(got, "factory.server_stop ok") {
		t.Fatalf("events %v, want server_stop's failure and the stop ok", got)
	}
}

// A listener the server's group left in the copy (overmind's tmux pane runs
// outside the group, its cwd in the copy) is stopped while its pid and its
// start match the process table, and then the proof passes.
func TestAServerStopKillsAListenerLeftInTheCopy(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	r.listen(listener{PID: 777, Port: c.Port, Command: "ruby", Cwd: c.Path + "/tmp"}, 0)

	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatalf("StopServer: %v", err)
	}
	if want := []string{"777 terminated"}; !slices.Equal(r.signalled(), want) {
		t.Fatalf("signals %v, want %v", r.signalled(), want)
	}
	if !r.eventsSay(c.Name, "factory.server_stray", "a listener the group left") {
		t.Fatalf("no event names the listener the group left: %v", r.stopEvents(c.Name))
	}
	if want := []string{"factory.server_stop begin", "factory.server_stopped", "factory.server_stray begin",
		"factory.server_stray ok", "factory.server_stop ok"}; !slices.Equal(r.stopEvents(c.Name), want) {
		t.Fatalf("events %v, want %v", r.stopEvents(c.Name), want)
	}
}

// A listener that took the pid of one the proof listed (another start) is
// never signalled.
func TestAServerStopNeverSignalsAPidAnotherProcessTook(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	r.listen(listener{PID: 777, Port: c.Port, Command: "ruby", Cwd: c.Path}, 0)
	r.c.listeners = func(ctx context.Context) ([]listener, error) {
		ls, err := r.listeners(ctx)
		r.mu.Lock()
		for i, p := range r.table { // the listener ends; a later process takes its pid
			if p.PID == 777 {
				r.table[i].Start = lstart(r.clock.Now().Add(time.Hour))
			}
		}
		r.listening = nil
		r.mu.Unlock()
		return ls, err
	}
	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatalf("StopServer: %v", err)
	}
	if got := r.signalled(); len(got) != 0 {
		t.Fatalf("signals %v, want none: pid 777 is another process now", got)
	}
}

// A process outside the copy, on another port, is none of the factory's: it
// is never signalled.
func TestAServerStopLeavesAListenerOutsideTheCopyAlone(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	r.listen(listener{PID: 888, Port: 9999, Command: "node", Cwd: "/elsewhere"}, 0)
	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatalf("StopServer: %v", err)
	}
	if got := r.signalled(); len(got) != 0 {
		t.Fatalf("signals %v, want none", got)
	}
}

// A process outside the copy that listens on the copy's own port (your dev
// server, another copy's, a review slot's) survives the stop: it is never
// signalled, whatever port it holds, and the stop fails with the reason, so
// QA does not walk it. The factory's own server is stopped and forgotten.
func TestAServerStopLeavesAForeignListenerOnTheCopysPortAlone(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	leader := r.spawned[0].PID
	r.listen(listener{PID: 889, Port: c.Port, Command: "node", Cwd: "/Users/alice/shop"}, 0)

	err := r.c.StopServer(r.ctx, c)
	want := fmt.Sprintf("port %d is held by a process outside the copy", c.Port)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("StopServer = %v, want %q", err, want)
	}
	if _, held := errors.AsType[portHeld](err); !held {
		t.Fatalf("StopServer = %T, want a portHeld", err)
	}
	if got := r.signalled(); len(got) != 0 {
		t.Fatalf("signals %v, want none: pid 889 is not the factory's", got)
	}
	r.mu.Lock()
	foreign := r.alive(889)
	r.mu.Unlock()
	if !foreign {
		t.Fatal("the foreign listener is gone")
	}
	if want := []string{strconv.Itoa(leader) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want only the factory's server %v", r.kills, want)
	}
	if _, ok, _ := r.c.serverOf(r.ctx, c.Name); ok {
		t.Fatal("the factory's stopped server is still recorded")
	}
	if !slices.Contains(r.stopEvents(c.Name), "factory.server_stop fail") {
		t.Fatalf("events %v, want factory.server_stop fail", r.stopEvents(c.Name))
	}
}

// Something that answers on the copy's port that lsof does not show (a
// process of another user) is outside the copy too.
func TestAServerStopNamesAnUnseenHolderOfThePort(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	r.answers = map[string]bool{fmt.Sprintf("[::1]:%d", c.Port): true}
	err := r.c.StopServer(r.ctx, c)
	if _, held := errors.AsType[portHeld](err); !held || !strings.Contains(err.Error(), fmt.Sprintf("port %d is held by a process outside the copy", c.Port)) {
		t.Fatalf("StopServer = %v, want the port held by a process outside the copy", err)
	}
}

// A stop of a copy whose server the factory never started kills nothing:
// no server_stop, no group, no listener, not even one in the copy on its
// port, and it proves nothing.
func TestAStopOfACopyWhoseRunStartedNoServerKillsNothing(t *testing.T) {
	r := newCopiesRig(t, 1)
	r.qaRepo("bin/overmind-kill")
	c := r.claim(r.task("PR-1"))
	r.listen(listener{PID: 777, Port: c.Port, Command: "ruby", Cwd: c.Path}, 0)
	r.listen(listener{PID: 888, Port: 3000, Command: "node", Cwd: "/elsewhere"}, 0)
	r.mu.Lock()
	r.order = nil
	r.mu.Unlock()

	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatalf("StopServer: %v", err)
	}
	if got := r.stopOrder(); len(got) != 0 || len(r.kills) != 0 {
		t.Fatalf("a stop with no server ran %v, kills %v; want nothing", got, r.kills)
	}
	if got := r.stopEvents(c.Name); len(got) != 0 {
		t.Fatalf("events %v, want none", got)
	}
}

// A member of the server's process group is stopped with the group, wherever
// its working directory is, and is never signalled alone.
func TestAServerStopEndsAListenerInItsGroup(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	leader := r.spawned[0].PID
	r.listen(listener{PID: leader + 1, Port: c.Port, Command: "puma", Cwd: "/private/tmp"}, leader)

	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatalf("StopServer: %v", err)
	}
	if want := []string{strconv.Itoa(leader) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want %v", r.kills, want)
	}
	if got := r.signalled(); len(got) != 0 {
		t.Fatalf("signals %v, want none: the group kill ended it", got)
	}
}

// A listener in the copy that no signal ends fails the stop with its pid,
// its command and its port, and its record stays, so the next stop or a
// restart's reconcile tries again.
func TestAServerStopThatCannotFreeThePortFails(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	r.listen(listener{PID: 777, Port: c.Port, Command: "ruby", Cwd: c.Path}, 0)
	r.mu.Lock()
	r.unkillable = map[int]bool{777: true}
	r.mu.Unlock()

	err := r.c.StopServer(r.ctx, c)
	want := fmt.Sprintf("the server of factory1 left pid 777 (ruby) listening on :%d", c.Port)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("StopServer = %v, want %q", err, want)
	}
	if want := []string{"777 terminated", "777 killed"}; !slices.Equal(r.signalled(), want) {
		t.Fatalf("signals %v, want %v", r.signalled(), want)
	}
	if !slices.Contains(r.stopEvents(c.Name), "factory.server_stop fail") {
		t.Fatalf("events %v, want factory.server_stop fail", r.stopEvents(c.Name))
	}
	if _, ok, _ := r.c.serverOf(r.ctx, c.Name); !ok {
		t.Fatal("the record is gone while the server's listener runs")
	}
	r.mu.Lock()
	r.unkillable = nil
	r.mu.Unlock()
	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatalf("the next stop = %v, want the listener ended", err)
	}
}

// A release stops the factory's server and goes on when a process outside
// the copy holds its port: that process is not the copy's to hand back.
func TestAReleaseGoesOnWhenAProcessOutsideTheCopyHoldsItsPort(t *testing.T) {
	r := newCopiesRig(t, 1)
	tk := r.task("PR-1")
	c := r.claim(tk)
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	r.listen(listener{PID: 889, Port: c.Port, Command: "node", Cwd: "/Users/alice/shop"}, 0)
	r.release(tk)
	if got := r.signalled(); len(got) != 0 {
		t.Fatalf("signals %v, want none", got)
	}
	b, err := os.ReadFile(filepath.Join(r.cfg.Layout.FactoryTaskDir(tk.ID), "handoff.md"))
	if err != nil || !strings.Contains(string(b), fmt.Sprintf("port %d is held by a process outside the copy", c.Port)) {
		t.Fatalf("handoff.md = %q, %v; want it to say who holds the port", b, err)
	}
}

// lsof's field output: the listeners (a process's every listening socket,
// IPv4 and IPv6, once per port) and the working directories of the pids.
func TestParseLsofListenersAndCwd(t *testing.T) {
	out := "p777\ncruby\nf9\nn*:3110\nf10\nn[::1]:3110\nf11\nn127.0.0.1:3135\np888\ncnode\nf20\nn*:9999\np999\ncbroken\nf3\nnnot-a-port\n"
	want := []listener{{PID: 777, Port: 3110, Command: "ruby"}, {PID: 777, Port: 3135, Command: "ruby"}, {PID: 888, Port: 9999, Command: "node"}}
	if got := parseLsofListeners([]byte(out)); !slices.Equal(got, want) {
		t.Fatalf("listeners %+v, want %+v", got, want)
	}
	if got := parseLsofListeners(nil); len(got) != 0 {
		t.Fatalf("no output gave %+v", got)
	}
	cwd := parseLsofCwd([]byte("p777\nfcwd\nn/copies/factory1\np888\nfcwd\nn/Users/alice/my app\n"))
	if cwd[777] != "/copies/factory1" || cwd[888] != "/Users/alice/my app" || len(cwd) != 2 {
		t.Fatalf("cwd %v", cwd)
	}
}

// A copy's own processes are those whose working directory is the copy or
// below it, never a sibling whose name only starts the same.
func TestInDirTakesTheCopyAndBelowOnly(t *testing.T) {
	for dir, want := range map[string]bool{
		"/copies/factory1": true, "/copies/factory1/tmp": true, "/copies/factory10": false, "/copies": false, "": false,
	} {
		if got := inDir(dir, "/copies/factory1"); got != want {
			t.Errorf("inDir(%q) = %v, want %v", dir, got, want)
		}
	}
}

// A server start that fails leaves no pidfile behind: a later task in the
// same copy that starts no server has nothing of its own stopped or killed
// at the next daemon start, not even a listener on the copy's port, and the
// repository's server_stop never runs in its copy.
func TestAFailedServerStartLeavesNothingThatStopsALaterTask(t *testing.T) {
	r := newCopiesRig(t, 1)
	r.qaRepo("bin/overmind-kill")
	first := r.task("PR-1")
	c := r.claim(first)
	r.endAtOnce = true
	if err := r.c.StartServer(r.ctx, c); err == nil || !strings.Contains(err.Error(), "ended at once") {
		t.Fatalf("StartServer = %v, want the server ended at once", err)
	}
	if _, err := os.Stat(r.c.serverPidFile(c.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed start left its pidfile: %v", err)
	}
	r.release(first)
	later := r.task("PR-2")
	c = r.claim(later)
	r.listen(listener{PID: 777, Port: c.Port, Command: "ruby", Cwd: c.Path}, 0) // the later task's own test server
	r.mu.Lock()
	r.order = nil
	r.mu.Unlock()

	r.c = r.newCopies()
	if err := r.c.Reconcile(r.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := r.stopOrder(); len(got) != 0 {
		t.Fatalf("the restart ran %v in a copy whose task started no server, want nothing", got)
	}
}

// A start whose process table cannot be read stops the server it spawned and
// leaves no pidfile either.
func TestAServerStartThatCannotReadTheTableStopsItsServer(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	r.failTable = 1
	if err := r.c.StartServer(r.ctx, c); err == nil {
		t.Fatal("StartServer = nil with no process table")
	}
	if want := []string{strconv.Itoa(r.spawned[0].PID) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want %v", r.kills, want)
	}
	if _, err := os.Stat(r.c.serverPidFile(c.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the start left its pidfile: %v", err)
	}
}

// A pidfile whose server no longer runs (an older build left it, or its
// server ended) names no process of the factory's: the restart stops
// nothing for it and forgets it. A stop with no record forgets one too.
func TestAPidfileWhoseServerIsGoneStopsNothing(t *testing.T) {
	r := newCopiesRig(t, 2)
	r.qaRepo("bin/overmind-kill")
	one, two := r.claim(r.task("PR-1")), r.claim(r.task("PR-2"))
	for _, c := range []Copy{one, two} {
		r.write(r.c.serverPidFile(c.Name), "4100\n")
	}
	r.listen(listener{PID: 777, Port: one.Port, Command: "ruby", Cwd: one.Path}, 0)
	if err := r.c.StopServer(r.ctx, two); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.c.serverPidFile(two.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stop with no record kept the pidfile: %v", err)
	}
	r.mu.Lock()
	r.order = nil
	r.mu.Unlock()

	r.c = r.newCopies()
	if err := r.c.Reconcile(r.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := r.stopOrder(); len(got) != 0 {
		t.Fatalf("the restart ran %v for a pidfile whose server is gone, want nothing", got)
	}
	if _, err := os.Stat(r.c.serverPidFile(one.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stale pidfile is still there: %v", err)
	}
}

// A server spawned just before a crash, never recorded, still runs: the
// restart runs the whole stop for it, server_stop, its group and the listener
// it left in the copy on its port.
func TestReconcileStopsAnUnrecordedServerAndWhatItLeftInTheCopy(t *testing.T) {
	r := newCopiesRig(t, 1)
	r.qaRepo("bin/overmind-kill")
	c := r.claim(r.task("PR-1"))
	pid, err := r.c.spawn(r.ctx, c.Path, nil, "bin/worktree-run", r.c.serverLog(c.Name), r.c.serverPidFile(c.Name))
	if err != nil {
		t.Fatal(err)
	}
	r.listen(listener{PID: 777, Port: c.Port, Command: "ruby", Cwd: c.Path}, 0)
	r.mu.Lock()
	r.order = nil
	r.mu.Unlock()

	r.c = r.newCopies()
	if err := r.c.Reconcile(r.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{"mise bin/overmind-kill", fmt.Sprintf("kill %d terminated", pid), "signal 777 terminated"}
	if got := r.stopOrder(); !slices.Equal(got, want) {
		t.Fatalf("the restart ran %v, want %v", got, want)
	}
}

// A stop runs on its own bound, whatever the context it is given: one that
// is cancelled or past its deadline (QA's cap, the daemon stopping) still
// ends the server's group and the browser's, and forgets them. So does a
// reconcile.
func TestAStopOnAnEndedContextStillEndsTheServerAndTheBrowser(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	defer stop()
	for name, ctx := range map[string]context.Context{"cancelled": cancelled, "expired": expired} {
		t.Run(name, func(t *testing.T) {
			r := newCopiesRig(t, 2)
			tk := r.task("PR-1")
			c := r.claim(tk)
			if err := r.c.StartServer(r.ctx, c); err != nil {
				t.Fatal(err)
			}
			if _, err := r.c.StartBrowser(r.ctx, c, r.runDir(tk.ID)); err != nil {
				t.Fatal(err)
			}
			if err := r.c.StopServer(ctx, c); err != nil {
				t.Fatalf("StopServer: %v", err)
			}
			if err := r.c.StopBrowser(ctx, c); err != nil {
				t.Fatalf("StopBrowser: %v", err)
			}
			want := []string{strconv.Itoa(r.spawned[0].PID) + " terminated", strconv.Itoa(r.browsers[0].PID) + " terminated"}
			if !slices.Equal(r.kills, want) {
				t.Fatalf("kills %v, want %v", r.kills, want)
			}
			if _, err := os.Stat(filepath.Join(r.runDir(tk.ID), "chrome")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the profile is still there: %v", err)
			}

			other := r.task("PR-2")
			c2 := r.claim(other)
			if err := r.c.StartServer(r.ctx, c2); err != nil {
				t.Fatal(err)
			}
			r.c = r.newCopies()
			if err := r.c.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got := r.kills[len(r.kills)-1]; got != strconv.Itoa(r.spawned[1].PID)+" terminated" {
				t.Fatalf("kills %v, want the stray ended last", r.kills)
			}
			for _, cp := range []Copy{c, c2} {
				if _, ok, _ := r.c.serverOf(r.ctx, cp.Name); ok {
					t.Errorf("%s's server is still recorded", cp.Name)
				}
			}
		})
	}
}

// A helper of the factory's own agents in the copy (a test server, a debugger)
// that listens on another port than the copy's server is not the server's:
// it survives the stop, and the stop is no failure.
func TestAServerStopLeavesAnAgentsHelperInTheCopyAlone(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	r.listen(listener{PID: 778, Port: 9229, Command: "node", Cwd: c.Path}, 0)
	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatalf("StopServer: %v", err)
	}
	if got := r.signalled(); len(got) != 0 {
		t.Fatalf("signals %v, want none: the helper is no part of the server", got)
	}
	r.mu.Lock()
	alive := r.alive(778)
	r.mu.Unlock()
	if !alive {
		t.Fatal("the agent's helper is gone")
	}
}

// A record whose pid another process took since (another start), and that
// process listens on the copy's port from outside the copy: it is never
// signalled, and the stop says the port is held.
func TestAStopOfAStaleRecordWhosePidListensOnThePortSignalsNothing(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	pid := r.spawned[0].PID
	r.mu.Lock()
	r.table = slices.DeleteFunc(r.table, func(p execx.Process) bool { return p.PID == pid })
	r.mu.Unlock()
	r.clock.Add(time.Hour)
	r.listen(listener{PID: pid, Port: c.Port, Command: "node", Cwd: "/Users/alice/shop"}, 0)

	err := r.c.StopServer(r.ctx, c)
	if _, held := errors.AsType[portHeld](err); !held {
		t.Fatalf("StopServer = %v, want the port held", err)
	}
	if len(r.kills) != 0 || len(r.signalled()) != 0 {
		t.Fatalf("kills %v, signals %v; want none", r.kills, r.signalled())
	}
}

// A record whose leader is gone while what it left still listens in the copy
// on its port (overmind's tmux pane) is stopped before a new server starts,
// so the new one can bind the port.
func TestAStartAfterADeadLeaderStopsWhatItLeftFirst(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	pid := r.spawned[0].PID
	r.mu.Lock()
	r.table = slices.DeleteFunc(r.table, func(p execx.Process) bool { return p.PID == pid })
	r.order = nil
	r.mu.Unlock()
	r.listen(listener{PID: 779, Port: c.Port, Command: "ruby", Cwd: c.Path}, 0)

	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	want := []string{"signal 779 terminated", fmt.Sprintf("spawn %d", r.spawned[1].PID)}
	if got := r.stopOrder(); !slices.Equal(got, want) {
		t.Fatalf("the start ran %v, want %v", got, want)
	}
}

// A server group that outlives SIGKILL stays recorded and fails the stop, so
// the next stop or a restart tries again.
func TestAServerGroupThatOutlivesSIGKILLStaysRecorded(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := r.c.StartServer(r.ctx, c); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.unkillable = map[int]bool{r.spawned[0].PID: true}
	r.mu.Unlock()
	if err := r.c.StopServer(r.ctx, c); err == nil || !strings.Contains(err.Error(), "outlived SIGKILL") {
		t.Fatalf("StopServer = %v, want the group that outlived SIGKILL", err)
	}
	if _, ok, _ := r.c.serverOf(r.ctx, c.Name); !ok {
		t.Fatal("a server that still runs is forgotten")
	}
}

// The copy's checkout may be reached by a link: lsof shows the target as a
// process's cwd, and that is in the copy too.
func TestInCopyTakesTheTargetOfALinkedCopy(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "volume", "factory1")
	if err := os.MkdirAll(filepath.Join(target, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "factory1")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	for cwd, want := range map[string]bool{
		filepath.Join(resolved, "tmp"): true, link: true, filepath.Join(filepath.Dir(resolved), "factory10"): false, root: false,
	} {
		if got := inCopy(cwd, link); got != want {
			t.Errorf("inCopy(%q, %q) = %v, want %v", cwd, link, got, want)
		}
	}
}

// lsof runs twice, the listeners and then their working directories. Its
// exit 1 with nothing but warnings on stderr is "none found"; with an error
// on stderr it fails the listing.
func TestLsofListenersReadsTheListenersAndTheirDirectories(t *testing.T) {
	cases := []struct {
		name           string
		listen, cwd    execx.Result
		want           []listener
		wantErr        bool
		wantSecondCall bool
	}{
		{name: "two", listen: execx.Result{Stdout: []byte("p777\ncruby\nf9\nn*:3010\np888\ncnode\nf3\nn*:9999\n")},
			cwd:            execx.Result{Stdout: []byte("p777\nfcwd\nn/copies/factory1\np888\nfcwd\nn/elsewhere\n")},
			want:           []listener{{PID: 777, Port: 3010, Command: "ruby", Cwd: "/copies/factory1"}, {PID: 888, Port: 9999, Command: "node", Cwd: "/elsewhere"}},
			wantSecondCall: true},
		{name: "none", listen: execx.Result{Code: 1}},
		{name: "a warning", listen: execx.Result{Code: 1, Stderr: []byte("lsof: WARNING: can't stat() smbfs file system /Volumes/x\n")}},
		{name: "an error", listen: execx.Result{Code: 1, Stderr: []byte("lsof: unknown option: -Z\n")}, wantErr: true},
		{name: "a pid gone", listen: execx.Result{Stdout: []byte("p777\ncruby\nf9\nn*:3010\n")},
			cwd: execx.Result{Code: 1}, want: []listener{{PID: 777, Port: 3010, Command: "ruby"}}, wantSecondCall: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &execx.Fake{Rules: []execx.Rule{
				{Prefix: []string{"lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpcn"}, Result: tc.listen},
				{Prefix: []string{"lsof", "-a", "-d", "cwd", "-p", "777,888", "-Fpn"}, Result: tc.cwd},
				{Prefix: []string{"lsof", "-a", "-d", "cwd", "-p", "777", "-Fpn"}, Result: tc.cwd},
			}}
			c := &copySet{run: fake}
			got, err := c.lsofListeners(t.Context())
			if (err != nil) != tc.wantErr || !slices.Equal(got, tc.want) {
				t.Fatalf("lsofListeners = %+v, %v; want %+v (error %v)", got, err, tc.want, tc.wantErr)
			}
			if second := len(fake.Calls) == 2; second != tc.wantSecondCall {
				t.Fatalf("lsof ran %d times: %+v", len(fake.Calls), fake.Calls)
			}
		})
	}
}

// The real spawn starts the server with the factory's shims first on its
// PATH, BROWSER=true and none of the variables the copies never inherit.
// The fake mise writes its env to a FIFO that the test reads to the end
// (readFIFO), so the test waits for all of env's output, however long a
// busy machine takes to start the server, and never reads a file that env
// is still writing.
func TestSpawnServerGivesTheServerTheShimsAndNoSecrets(t *testing.T) {
	storetest.Serial(t)
	bin := t.TempDir()
	out := filepath.Join(t.TempDir(), "env")
	if err := syscall.Mkfifo(out, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "mise"), []byte("#!/bin/sh\nenv > '"+out+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := bin + ":" + os.Getenv("PATH")
	t.Setenv("PATH", path)
	t.Setenv("GH_TOKEN", "gho_secret")
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	if err := os.MkdirAll(r.cfg.Layout.Logs(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.spawnServer(r.ctx, c.Path, c.Env, "bin/worktree-run", r.c.serverLog(c.Name), r.c.serverPidFile(c.Name)); err != nil {
		t.Fatal(err)
	}
	env := readFIFO(t, out)
	lines := strings.Split(string(env), "\n")
	for _, want := range []string{"PATH=" + r.cfg.Layout.FactoryShims() + ":" + path, "BROWSER=true"} {
		if !slices.Contains(lines, want) {
			t.Errorf("the server's env has no %s:\n%s", want, env)
		}
	}
	if strings.Contains(string(env), "GH_TOKEN") {
		t.Errorf("the server's env has GH_TOKEN:\n%s", env)
	}
}

// readFIFO reads the FIFO at path from the moment a writer opens it until
// its last writer closes it. If no writer has closed it within a minute, it
// opens the write end itself, so a read still waiting for a writer ends, and
// fails t.
func readFIFO(t *testing.T, path string) []byte {
	t.Helper()
	type read struct {
		b   []byte
		err error
	}
	done := make(chan read, 1)
	go func() {
		b, err := os.ReadFile(path)
		done <- read{b, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.b
	case <-time.After(time.Minute):
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatalf("no writer of %s closed it within a minute", path)
		return nil
	}
}
