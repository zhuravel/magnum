package agents

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// mutate changes the world's process pid under its lock.
func (w *procWorld) mutate(pid int, change func(fp *fakeProc)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, fp := range w.procs {
		if fp.p.PID == pid {
			change(fp)
		}
	}
}

// A captured child that moves to a group or session of its own after the
// capture (setsid) and outlives SIGTERM is still the job's: its pid and
// start are the same, so it gets SIGKILL, and the stop succeeds only once
// it is gone.
func TestStopShellKeepsACapturedChildThatChangedItsGroup(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
	w.set(isSandbox, false, true, true)
	prev := e.sigs.on
	e.sigs.on = func(pid int, sig syscall.Signal) error {
		err := prev(pid, sig)
		if pid == -jobPGID && sig == syscall.SIGTERM {
			w.mutate(sandboxPID, func(fp *fakeProc) { fp.p.PGID, fp.p.PPID = 901, 1 }) // its parent gone, a session of its own
		}
		return err
	}
	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v", err)
	}
	if !slices.Contains(e.signalsTo(), fmt.Sprintf("%d:KILL", sandboxPID)) || w.alive(sandboxPID) {
		t.Errorf("signals = %v, alive %t: want SIGKILL to the child that changed its group", e.signalsTo(), w.alive(sandboxPID))
	}
}

// After the group's signal the process table is read again before the
// signal to each process: a process the group signal ended whose pid went
// to another process (a later start) gets nothing.
func TestStopShellReadsTheTableAgainAfterTheGroupSignal(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
	prev := e.sigs.on
	e.sigs.on = func(pid int, sig syscall.Signal) error {
		err := prev(pid, sig)
		if pid == -jobPGID {
			// codex ended with its group; another process got its pid at once.
			w.mu.Lock()
			w.procs = append(w.procs, &fakeProc{p: execx.Process{PID: codexPID, PPID: 1, PGID: 902, Name: "codex",
				Start: "Wed Oct 28 10:16:40 2026"}, term: true, kill: true})
			w.mu.Unlock()
		}
		return err
	}
	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v", err)
	}
	if slices.Contains(e.signalsTo(), fmt.Sprintf("%d:TERM", codexPID)) || !w.alive(codexPID) {
		t.Errorf("signals = %v: the process that got codex's pid was signalled", e.signalsTo())
	}
}

// herdr may still name the job's group for a moment after the job ended
// (the shell has not taken the terminal back): with no process of that
// group left in the process table, that is the job's end, a stop that
// worked, never another job's command.
func TestStopShellTakesAGroupThatEndedForTheEndOfTheJob(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
	var lag atomic.Bool
	prev := e.sigs.on
	e.sigs.on = func(pid int, sig syscall.Signal) error {
		if pid == -jobPGID {
			lag.Store(true)
		}
		return prev(pid, sig)
	}
	// From the group signal on, herdr's view lags: the old group, with a
	// foreground of no process of the job, at every read and idle wait.
	lagging := func(f *fakeHerdr, p string) {
		if p == pane && lag.Load() {
			f.procs[p] = herdr.ProcessInfo{PaneID: p, ShellPID: shellPID, ForegroundPGID: jobPGID,
				Foreground: []herdr.Process{{PID: 999, Name: "zsh", Argv: []string{"zsh"}}}}
		}
	}
	e.h.onProcInfo = func(f *fakeHerdr, p string, _ int) { lagging(f, p) }
	e.h.onWaitIdle = func(f *fakeHerdr, p string, _ time.Duration) { lagging(f, p) }
	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v, want the job's end", err)
	}
	if !lag.Load() {
		t.Fatal("the job's group got no signal")
	}
	if f := e.eventsOf(EventShellStopFailed); len(f) != 0 {
		t.Errorf("failed events = %+v", f)
	}
	if got := e.eventsOf(EventShellStopped); len(got) != 1 {
		t.Errorf("stopped events = %+v, want one", got)
	}
}

// A ctrl+c that left the pane an idle shell is no stop while a process the
// interrupt captured still runs (a sandbox child): StopShell signals that
// process by its identity, SIGTERM then SIGKILL, and succeeds only once it
// is gone; one that outlives SIGKILL fails the stop. The group, no longer
// the pane's foreground, gets nothing.
func TestStopShellAfterAnIdlePaneStillStopsWhatItCapturedAtTheInterrupt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		killable bool
	}{{"gone on SIGKILL", true}, {"outlives SIGKILL", false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			run, pane := e.shellRun(store.RunSubmitted)
			w := e.world(pane)
			w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
			w.set(isSandbox, false, tc.killable, true)
			e.h.onKeys = func(f *fakeHerdr, target string, keys []string) { // ctrl+c ends the group alone
				if target != pane || !slices.Contains(keys, "ctrl+c") {
					return
				}
				w.mu.Lock()
				w.procs = slices.DeleteFunc(w.procs, func(fp *fakeProc) bool { return fp.p.PGID == jobPGID })
				w.mu.Unlock()
				f.procs[pane] = idlePane(pane)
			}
			if err := e.m.InterruptShell(e.ctx, run); err != nil {
				t.Fatal(err)
			}
			err := e.m.StopShell(e.ctx, run)
			got := e.signalsTo()
			if !slices.Contains(got, fmt.Sprintf("%d:TERM", sandboxPID)) || !slices.Contains(got, fmt.Sprintf("%d:KILL", sandboxPID)) {
				t.Errorf("signals = %v, want SIGTERM then SIGKILL to the captured child", got)
			}
			if slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "-") }) {
				t.Errorf("signals = %v: a group signal to a pane that is an idle shell", got)
			}
			if (err == nil) != tc.killable {
				t.Errorf("StopShell = %v, want ok %t", err, tc.killable)
			}
			if !tc.killable && !e.m.ShellLaunched(run.ID) {
				t.Error("a stop that failed dropped the run from the launched ones")
			}
		})
	}
}

// Each wait after a signal has one deadline, shellStopWait, which every
// read of the process table and every sleep shares: slow reads end the
// wait sooner, they do not stretch it to 21 of them.
func TestTheStopsWaitIsBoundedByItsDeadline(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
	w.set(isSandbox, false, true, true) // the pane goes idle at SIGTERM, the child does not
	w.onPS = func(*procWorld, int) { e.clock.Add(2 * time.Second) }
	var atTerm, atKill int
	prev := e.sigs.on
	e.sigs.on = func(pid int, sig syscall.Signal) error {
		w.mu.Lock()
		switch {
		case pid == sandboxPID && sig == syscall.SIGTERM:
			atTerm = w.psReads
		case pid == sandboxPID && sig == syscall.SIGKILL:
			atKill = w.psReads
		}
		w.mu.Unlock()
		return prev(pid, sig)
	}
	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v", err)
	}
	if atKill == 0 || atKill-atTerm > 6 {
		t.Errorf("%d reads of the process table between SIGTERM and SIGKILL, want the wait's 5 s to bound them", atKill-atTerm)
	}
}

// The hold check's stop runs to its end when the round's setup is cut (an
// abort, a shutdown) between SIGTERM and SIGKILL: the job is never left
// half stopped.
func TestClearShellPanesFinishesItsStopWhenItsCallerIsCut(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	earlier, pane := e.shellRun(store.RunAbandoned)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, earlier.ID))
	w.set(anyProc, false, true, true)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	prev := e.sigs.on
	e.sigs.on = func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			cancel()
		}
		return prev(pid, sig)
	}
	if err := e.m.ClearShellPanes(ctx, e.pr, shellCheckout); err != nil {
		t.Fatalf("ClearShellPanes = %v, want the stop finished", err)
	}
	if !slices.Contains(e.signalsTo(), fmt.Sprintf("-%d:KILL", jobPGID)) {
		t.Errorf("signals = %v, want SIGKILL after the cut", e.signalsTo())
	}
}

// A stop that replaces the pane points the session at the replacement
// before the old pane closes, and an observe tick never takes that for a
// lost session: one whose snapshot predates the move (the replacement not
// in it) waits out LostGrace from the move, and one that read the row
// before the move (the old pane, gone from herdr since) cannot mark the
// row lost, since it no longer records that pane.
func TestAnObserveTickNeverLosesARepointedShellSession(t *testing.T) {
	for _, tc := range []string{"a snapshot from before the move", "a row read before the move"} {
		t.Run(tc, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			e.clock.Add(10 * time.Minute) // long after the session started
			run, pane := e.shellRun(store.RunSubmitted)
			w := e.world(pane)
			w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
			w.set(anyProc, false, false, true)
			before, _ := e.h.Snapshot(e.ctx)
			capturedAt := e.clock.Now()
			stale := e.session(RoleCodexReview)
			if err := e.m.StopShell(e.ctx, run); err != nil {
				t.Fatalf("StopShell = %v", err)
			}
			repl := store.Deref(e.session(RoleCodexReview).HerdrPaneID)
			if repl == pane {
				t.Fatal("the stop did not replace the pane")
			}
			if tc == "a snapshot from before the move" {
				if _, err := e.m.ObserveSnapshotAt(e.ctx, before, capturedAt); err != nil {
					t.Fatal(err)
				}
			} else {
				after, _ := e.h.Snapshot(e.ctx)
				later := e.clock.Now().Add(time.Minute) // past LostGrace from the move
				if _, err := e.m.observeOne(e.ctx, stale, after, paneIndex(after), nil, later, later); err != nil {
					t.Fatal(err)
				}
			}
			if s, err := e.st.SessionByID(e.ctx, stale.ID); err != nil || !isLive(s) || store.Deref(s.HerdrPaneID) != repl {
				t.Errorf("session = %+v, %v; want it live in the replacement %s", s, err, repl)
			}
		})
	}
}

// A run whose command ended, or whose stop was proven, leaves the launched
// runs: the list does not grow for the daemon's life.
func TestALaunchedRunIsForgottenOnceItEnds(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	e.world(pane).job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatal(err)
	}
	e.h.waitLine = "MAGNUM_DONE_r-7 0"
	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, "the line", DoneMarker("r-7"), time.Minute); err != nil {
		t.Fatal(err)
	}
	e.m.mu.Lock()
	n := len(e.m.launched)
	e.m.mu.Unlock()
	if n != 0 || e.m.ShellLaunched(run.ID) || e.m.ShellLaunched("r-7") {
		t.Errorf("launched runs = %d, want none once their commands ended", n)
	}
}
