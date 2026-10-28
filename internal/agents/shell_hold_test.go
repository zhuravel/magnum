package agents

import (
	"context"
	"errors"
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

// paneCwd sets where herdr's snapshot shows pane working, its cwd and its
// foreground's ("" for both: herdr does not say).
func (e *env) paneCwd(pane, cwd string) {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	for i := range e.h.panes {
		if e.h.panes[i].ID == pane {
			e.h.panes[i].Cwd, e.h.panes[i].ForegroundCwd = cwd, cwd
		}
	}
}

// lintJob is the process info of a pane whose foreground runs a line
// without the run token.
func lintJob(pane string) herdr.ProcessInfo {
	return herdr.ProcessInfo{PaneID: pane, ShellPID: shellPID, ForegroundPGID: 500,
		Foreground: []herdr.Process{{PID: 500, Name: "lint", Argv: []string{"bin/lint", "--all"}}}}
}

// The plain ctrl+c of a line without the run token goes only to the line of
// a custom role (its command leaves the db-lock line out) that RunShell
// typed, in a pane herdr still shows as the role's: in the session's
// checkout, and recorded by no other PR's live session. A built-in line
// whose token is gone from the pane, a pane herdr shows in another checkout
// and a pane another PR's live session records get no key, no signal and
// no close, though the live row still names the pane.
func TestTheTokenlessCtrlCGoesOnlyToACustomLineInThePaneStillTheRoles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tagged bool // the line RunShell typed carries the run token, as a built-in role's does
		set    func(e *env, pane string)
		keyed  bool
	}{
		{"a custom line in the role's pane", false, nil, true},
		{"a built-in line whose token is gone from the pane", true, nil, false},
		{"a pane herdr shows in another checkout", false, func(e *env, pane string) { e.paneCwd(pane, "/Users/x/Projects/other") }, false},
		{"a pane another PR's live session records", false, func(e *env, pane string) {
			other := e.otherPR(11921)
			if _, err := e.st.CreateSession(e.ctx, store.Session{PRID: other.ID, Role: string(RoleCodexReview), AgentKind: new(KindShell),
				HerdrPaneID: new(pane), Cwd: new(e.slotOf(other)), State: store.SessionLive}); err != nil {
				e.t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			run, pane := e.shellRun(store.RunSubmitted)
			e.m.shellDone(run.ID) // RunShell types its line below
			e.setProcs(pane, idlePane(pane))
			line := "bin/lint --all"
			if tc.tagged {
				line = ShellDBLockLine(e.m.d.Layout.Binary(), shellCheckout, string(RoleCodexReview), run.ID) + " codex review"
			}
			e.h.errs["PaneWaitOutput"] = &herdr.Error{Method: "pane.wait_for_output", Code: herdr.CodeTimeout, Message: "t"}
			if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, line, DoneMarker(run.ID), time.Minute); err == nil {
				t.Fatal("RunShell = nil, want its wait to time out with the line still running")
			}
			e.setProcs(pane, lintJob(pane))
			if tc.set != nil {
				tc.set(e, pane)
			}
			e.h.mu.Lock()
			e.h.keys = nil // RunShell's ctrl+u
			e.h.mu.Unlock()
			err := e.m.InterruptShell(e.ctx, run)
			if tc.keyed && err != nil || !tc.keyed && !errors.Is(err, ErrNotOwn) {
				t.Errorf("InterruptShell = %v, want keyed %t (else ErrNotOwn)", err, tc.keyed)
			}
			if keyed := slices.Contains(e.keysSent(), pane+":ctrl+c"); keyed != tc.keyed || len(e.keysSent()) > 1 {
				t.Errorf("keys = %v, want ctrl+c %t", e.keysSent(), tc.keyed)
			}
			if len(e.signalsTo()) != 0 || len(e.h.paneCloses) != 0 {
				t.Errorf("signals %v, closes %v", e.signalsTo(), e.h.paneCloses)
			}
		})
	}
}

// A shell pane herdr cannot place (its snapshot gives no cwd) stays in the
// checkout barrier: a command that runs there holds the switch, and the
// row is not marked lost (a retired row keeps its pane too). So does a pane
// herdr shows outside the checkout whose foreground carries magnum's line
// for this checkout. Nothing is signalled, and magnum remembers no job of
// theirs (as after a daemon restart).
func TestAShellPaneHerdrCannotPlaceStaysInTheCheckoutBarrier(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cwd     string
		retired bool // the row that records the pane is retired
		own     bool // the foreground runs magnum's line of an earlier run for the checkout
	}{
		{"no cwd, magnum's line", "", false, true},
		{"no cwd, another command", "", false, false},
		{"no cwd, a retired row", "", true, true},
		{"outside the checkout, magnum's line for it", "/Users/x/Projects/other", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			live := e.session(RoleCodexReview)
			pane := store.Deref(live.HerdrPaneID)
			w := e.world(pane)
			if tc.own {
				earlier, _ := e.shellRun(store.RunAbandoned)
				w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, earlier.ID))
			} else {
				e.setProcs(pane, otherJob(pane))
			}
			if tc.retired {
				if err := e.st.TransitionSession(e.ctx, live.ID, nil, store.SessionClosed, nil); err != nil {
					t.Fatal(err)
				}
			}
			e.paneCwd(pane, tc.cwd)
			e.m.mu.Lock()
			jobs := len(e.m.jobs)
			e.m.mu.Unlock()
			if jobs != 0 {
				t.Fatalf("remembered jobs = %d, want none", jobs)
			}
			err := e.m.ClearShellPanes(e.ctx, e.pr, shellCheckout)
			var hold *ShellHoldError
			if !errors.As(err, &hold) {
				t.Fatalf("ClearShellPanes = %v, want a hold while the command runs", err)
			}
			if hold.Foreign == tc.own {
				t.Errorf("hold = %+v, want foreign %t", hold, !tc.own)
			}
			if len(e.signalsTo()) != 0 || len(e.keysSent()) != 0 || len(e.h.paneCloses) != 0 {
				t.Errorf("signals %v, keys %v, closes %v", e.signalsTo(), e.keysSent(), e.h.paneCloses)
			}
			if tc.own && !w.alive(dbLockPID) {
				t.Error("the command is gone")
			}
			s, err := e.st.SessionByID(e.ctx, live.ID)
			if err != nil {
				t.Fatal(err)
			}
			if s.State == store.SessionLost || store.Deref(s.HerdrPaneID) != pane {
				t.Errorf("session = %s in %q, want it kept in pane %s", s.State, store.Deref(s.HerdrPaneID), pane)
			}
		})
	}
}

// ClearShellPanes asks herdr nothing while the PR has no shell pane in the
// checkout, so a herdr outage holds no checkout the PR's shell roles never
// ran in. With one there, a herdr that cannot say is ErrHerdrUnread, never
// a hold for magnum's own command.
func TestClearShellPanesAsksHerdrOnlyForAShellPaneInTheCheckout(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	e.h.errs["Snapshot"] = errors.New("herdr: connection refused")
	if err := e.m.ClearShellPanes(e.ctx, e.pr, "/Users/x/Projects/talkable.review2"); err != nil {
		t.Errorf("ClearShellPanes in another checkout = %v, want nil", err)
	}
	if calls := e.h.callsWith("Snapshot"); len(calls) != 0 {
		t.Errorf("herdr calls %v for a checkout without a shell pane of the PR", calls)
	}
	err := e.m.ClearShellPanes(e.ctx, e.pr, shellCheckout)
	if !errors.Is(err, ErrHerdrUnread) || errors.Is(err, ErrShellHold) {
		t.Errorf("ClearShellPanes = %v, want ErrHerdrUnread and no hold", err)
	}
}

// tokenless makes role r a custom shell role whose command leaves the
// db-lock line out: its lines carry no run token.
func (e *env) tokenless(r Role) {
	e.t.Helper()
	for i := range e.cfg.Roles {
		if e.cfg.Roles[i].Name == string(r) {
			e.cfg.Roles[i].Command, e.cfg.Roles[i].Prompt = "bin/lint --all", ""
		}
	}
	e.cfg.Normalize()
}

// A custom role's line without the run token, still running in the role's
// live pane, holds the checkout as that role's command, never as one magnum
// did not start; the same foreground in a built-in role's pane is foreign.
func TestATokenlessRolesCommandHoldsAsItsOwn(t *testing.T) {
	for _, custom := range []bool{true, false} {
		t.Run(fmt.Sprintf("custom %t", custom), func(t *testing.T) {
			e := newEnv(t)
			if custom {
				e.tokenless(RoleCodexReview)
			}
			e.shellWorkspace()
			pane := store.Deref(e.session(RoleCodexReview).HerdrPaneID)
			e.setProcs(pane, lintJob(pane))
			err := e.m.ClearShellPanes(e.ctx, e.pr, shellCheckout)
			var hold *ShellHoldError
			if !errors.As(err, &hold) {
				t.Fatalf("ClearShellPanes = %v, want a hold", err)
			}
			if hold.Tokenless != custom || hold.Foreign == custom {
				t.Errorf("hold = %+v, want tokenless %t and foreign %t", hold, custom, !custom)
			}
			if len(e.signalsTo()) != 0 || len(e.keysSent()) != 0 {
				t.Errorf("signals %v, keys %v", e.signalsTo(), e.keysSent())
			}
		})
	}
}
