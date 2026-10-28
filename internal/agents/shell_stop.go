package agents

// A cut shell command is stopped for sure, and a pane something else keeps
// busy is left for a new one (DECISIONS "A cut shell command is killed, and
// a review that missed a reviewer is completed"): from 2026-10-16 to
// 2026-10-28, 12 codex-review runs ended busy. In 11 of them a push
// restarted the round, the restart's three ctrl+c did not end `codex
// review`, and the next run's line waited a minute for an idle shell and
// failed ("shell not idle after 1m0s (foreground: codex,magnum)"); the
// round went on without codex-review, and auto-approval refused its clean
// review. Magnum signals or closes only a command it proves its own: the
// db-lock of the shell line carries the run's id (ShellDBLockLine, the run
// token), herdr names the pane's foreground processes with their argv, and
// the process table gives each process of the job its identity (pid and
// start), which is read again before every signal. A shell cleanup never
// marks a human at work: a command magnum did not start, or one of its own
// it cannot stop, holds the switch of the checkout (ShellHoldError), which
// the engine keeps and reports.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Events of StopShell and of RunShell's move to a new pane, on the PR.
const (
	// EventShellStopBegin (warn) is recorded before StopShell's first
	// signal to an own run: the role, the pane, the run, the process group
	// (pgid) and the job it captured (tree: pid, pgid and name of each, never
	// argv).
	EventShellStopBegin = "pr.shell_stop_begin"
	// EventShellStopped (info): the step that stopped it (term, kill,
	// replace: the pane replaced, close: the pane closed without a
	// replacement, exited: it ended before a signal) and the seconds it
	// took.
	EventShellStopped = "pr.shell_stopped"
	// EventShellStopFailed (warn): the cause, not_own (nothing more was
	// sent: the command is not proven an own run, or herdr shows another one
	// now), signal (a signal was refused, the process table could not be
	// read, or a captured process outlived every step) or close (the pane
	// would not close).
	EventShellStopFailed = "pr.shell_stop_failed"
	// EventShellMoveBegin (warn), EventShellMoved (info) and
	// EventShellMoveFailed (warn, the cause split or registry): RunShell
	// moves a shell role to a new pane, as something that is no run of
	// magnum's keeps its pane busy; the busy pane gets nothing.
	EventShellMoveBegin  = "pr.shell_move_begin"
	EventShellMoved      = "pr.shell_moved"
	EventShellMoveFailed = "pr.shell_move_failed"
)

// ErrNotOwn is StopShell's and InterruptShell's answer when the command a
// shell role's pane runs is not proven the run's own (ownShell): they sent
// nothing (more).
var ErrNotOwn = errors.New("not magnum's own run")

// ErrShellHold is what a ShellHoldError is: a command keeps a shell pane in
// the checkout busy, so nothing may switch it.
var ErrShellHold = errors.New("a command keeps a shell pane in the checkout busy")

// ShellHoldError is ClearShellPanes' answer while a command keeps one of
// the PR's shell panes in the checkout busy: Foreign for a command magnum
// did not start (no run token of the PR's role there), else magnum's own
// command it could not stop, Cause saying why in plain words. It names the
// role and the pane, never the command's argv.
type ShellHoldError struct {
	Role, Pane, Checkout string
	Foreign              bool
	Cause                string
}

func (e *ShellHoldError) Error() string {
	where := e.Checkout
	if e.Pane != "" {
		where += " (" + e.Role + " pane " + e.Pane + ")"
	}
	if e.Foreign {
		return "a command magnum did not start keeps a shell pane in " + where + " busy"
	}
	return "magnum cannot stop its own command in " + where + ": " + e.Cause
}

// Is makes a ShellHoldError match ErrShellHold.
func (e *ShellHoldError) Is(target error) bool { return target == ErrShellHold }

// shellStopWait bounds the wait for an idle shell, and for the job's
// processes to be gone, after each of StopShell's signals; treePoll paces
// the look at the process table within it, and readFloor is the least a
// last look gets once the wait's budget is spent.
const (
	shellStopWait = 5 * time.Second
	treePoll      = 250 * time.Millisecond
	readFloor     = time.Second
)

// The causes of EventShellStopFailed.
const (
	stopNotOwn = "not_own"
	stopSignal = "signal"
	stopClose  = "close"
)

// notOwnError is ownShell's and recheck's ErrNotOwn: why, in words that
// carry nothing read from a process's argv, and whether the pane runs a
// command magnum did not start (foreign: no run token, another PR's,
// role's or checkout's line, another job by the time of a signal) rather
// than a state of magnum's own (a run in flight, a session no longer live,
// herdr unable to say).
type notOwnError struct {
	why     string
	foreign bool
}

func (e *notOwnError) Error() string { return e.why + ": " + ErrNotOwn.Error() }
func (e *notOwnError) Unwrap() error { return ErrNotOwn }

func notOwn(foreign bool, format string, a ...any) error {
	return &notOwnError{why: fmt.Sprintf(format, a...), foreign: foreign}
}

// shellToken is the run token a shell role's db-lock carries: its role,
// its checkout and its run (ShellDBLockLine).
type shellToken struct{ role, checkout, run string }

// runToken reads the run token from argv, a process's: magnum's binary
// (binary, or a program of its name), `db-lock`, then db-lock's flags up to
// `--`, --role, --checkout and --run among them. ok is false for anything
// else, an unknown flag of db-lock's too.
func runToken(argv []string, binary string) (shellToken, bool) {
	name := "magnum"
	if binary != "" {
		name = filepath.Base(binary)
	}
	if len(argv) < 2 || argv[1] != "db-lock" || (argv[0] != binary && filepath.Base(argv[0]) != name) {
		return shellToken{}, false
	}
	var t shellToken
	for i := 2; i < len(argv); i++ {
		if argv[i] == "--" {
			return t, t.role != "" && t.checkout != "" && t.run != ""
		}
		flag, val, inline := strings.Cut(argv[i], "=")
		if !inline {
			if i+1 >= len(argv) {
				break
			}
			i++
			val = argv[i]
		}
		switch flag {
		case "--checkout":
			t.checkout = val
		case "--role":
			t.role = val
		case "--run":
			t.run = val
		case "--timeout":
		default:
			return shellToken{}, false
		}
	}
	return shellToken{}, false
}

// hasToken reports whether one of pi's foreground processes carries a run
// token (anyone's).
func (m *Manager) hasToken(pi herdr.ProcessInfo) bool {
	return slices.ContainsFunc(pi.Foreground, func(p herdr.Process) bool {
		_, ok := runToken(p.Argv, m.d.Layout.Binary())
		return ok
	})
}

// ownedShell is a command in a shell role's pane that ownShell proved an
// own run of magnum's: the pane, its shell, the job's process group, the
// db-lock carrying the run token and the run it names, and the job's
// processes (captureJob) with their identity.
type ownedShell struct {
	sess        store.Session
	pane        string
	shell, pgid int
	proc        herdr.Process
	tok         shellToken
	run         store.Run
	job         []execx.Process
}

// ownShell proves that pi, what session sess's pane runs, is an own run of
// magnum's, which only then may be signalled or closed. All of these hold:
// sess is the live shell session of the PR's role (its pane the role's);
// the pane's foreground job is not its shell, and holds a db-lock whose
// argv is magnum's binary with this role, this checkout (the session's
// cwd) and a run (runToken); that run is a run of this PR and role in the
// registry; and it is the cut run, or no longer in flight. Anything else
// is a *notOwnError (ErrNotOwn), whose words name nothing read from the
// argv.
func (m *Manager) ownShell(ctx context.Context, pr store.PR, sess store.Session, pi herdr.ProcessInfo, cut string) (ownedShell, error) {
	pane := store.Deref(sess.HerdrPaneID)
	if sess.PRID != pr.ID || !isLive(sess) || m.isAgentSession(sess) || pane == "" {
		return ownedShell{}, notOwn(false, "session %d is no live shell session of the PR", sess.ID)
	}
	if live, err := m.d.Store.LiveSessionByPRRole(ctx, pr.ID, sess.Role); err != nil || live.ID != sess.ID || store.Deref(live.HerdrPaneID) != pane {
		return ownedShell{}, notOwn(false, "pane %s is not the %s pane of the PR", pane, sess.Role)
	}
	if pi.ShellPID <= 1 || pi.ForegroundPGID <= 1 || pi.ForegroundPGID == pi.ShellPID {
		return ownedShell{}, notOwn(false, "pane %s runs no job", pane)
	}
	var (
		proc  herdr.Process
		tok   shellToken
		found bool
	)
	for _, p := range pi.Foreground {
		t, ok := runToken(p.Argv, m.d.Layout.Binary())
		if !ok {
			continue
		}
		if found && t != tok {
			return ownedShell{}, notOwn(true, "pane %s runs two db-locks", pane)
		}
		proc, tok, found = p, t, true
	}
	switch {
	case !found:
		return ownedShell{}, notOwn(true, "pane %s runs no db-lock with a run token", pane)
	case proc.PID <= 1 || proc.PID == pi.ShellPID || proc.PID == os.Getpid():
		return ownedShell{}, notOwn(true, "pane %s runs a db-lock magnum cannot signal", pane)
	case tok.role != sess.Role:
		return ownedShell{}, notOwn(true, "pane %s runs another role's line", pane)
	case store.Deref(sess.Cwd) == "" || !samePath(tok.checkout, store.Deref(sess.Cwd)):
		return ownedShell{}, notOwn(true, "pane %s runs a line for another checkout", pane)
	}
	run, err := m.d.Store.RunByID(ctx, tok.run)
	switch {
	case err != nil:
		return ownedShell{}, notOwn(true, "pane %s runs the line of a run the registry does not know", pane)
	case run.PRID != pr.ID || run.Role != sess.Role:
		return ownedShell{}, notOwn(true, "pane %s runs the line of another PR's or role's run", pane)
	case run.ID != cut && runInFlight(run):
		return ownedShell{}, notOwn(false, "pane %s runs the line of run %s, which is in flight", pane, run.ID)
	}
	return ownedShell{sess: sess, pane: pane, shell: pi.ShellPID, pgid: pi.ForegroundPGID, proc: proc, tok: tok, run: run}, nil
}

// runInFlight reports whether a run's turn may still be going on.
func runInFlight(r store.Run) bool {
	switch r.State {
	case store.RunPending, store.RunSubmitted, store.RunWorking:
		return true
	}
	return false
}

// markLaunched records that RunShell typed (or tried to type) the line of
// run runID; ShellLaunched reports it, and ShellDone drops it. A line never
// typed has nothing in a pane for magnum to key or stop.
func (m *Manager) markLaunched(runID string) {
	m.mu.Lock()
	m.launched[runID] = true
	m.mu.Unlock()
}

// ShellLaunched reports whether RunShell typed, or tried to type, the line
// of the shell role's run runID into its pane and the command may still run
// (in this daemon: an earlier daemon's rounds ended with it). Only a
// launched run's command is keyed (InterruptShell) or stopped (StopShell).
func (m *Manager) ShellLaunched(runID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.launched[runID]
}

// shellDone drops run runID from the launched runs: its command ended (its
// done marker came) or its stop was proven.
func (m *Manager) shellDone(runID string) {
	m.mu.Lock()
	delete(m.launched, runID)
	m.mu.Unlock()
}

// remember keeps o's captured job until it is proven gone (forget), so a
// pane that went idle while a captured process still runs is no proof
// that the command stopped (stopRemembered).
func (m *Manager) remember(o ownedShell) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.jobs[o.run.ID]; ok {
		for _, p := range prev.job {
			if !slices.ContainsFunc(o.job, p.Same) {
				o.job = append(o.job, p)
			}
		}
	}
	m.jobs[o.run.ID] = o
}

func (m *Manager) forget(runID string) {
	m.mu.Lock()
	delete(m.jobs, runID)
	m.mu.Unlock()
}

// remembered is the job captured for run runID, if any.
func (m *Manager) remembered(runID string) (ownedShell, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.jobs[runID]
	return o, ok
}

// runSession is the PR and the session of a shell role's run.
func (m *Manager) runSession(ctx context.Context, run store.Run) (store.PR, store.Session, error) {
	if run.SessionID == nil {
		return store.PR{}, store.Session{}, fmt.Errorf("agents: %s run %s has no session", run.Role, run.ID)
	}
	sess, err := m.d.Store.SessionByID(ctx, *run.SessionID)
	if err != nil {
		return store.PR{}, store.Session{}, fmt.Errorf("agents: %s run %s: %w", run.Role, run.ID, err)
	}
	pr, err := m.d.Store.PRByID(ctx, run.PRID)
	if err != nil {
		return store.PR{}, store.Session{}, fmt.Errorf("agents: %s run %s: %w", run.Role, run.ID, err)
	}
	return pr, sess, nil
}

// InterruptShell sends ctrl+c to the command a shell role's run left in
// its pane, only once RunShell typed the run's line (ShellLaunched) and
// only to the pane of the role's live session. A pane whose foreground
// carries this run's own token (ownShell) has its job captured first
// (captureJob, remembered), so a later idle shell is no proof while a
// captured process runs on. A line of a role without the run token (a
// `command` of the operator's that leaves the db-lock line out) gets the
// plain ctrl+c it always got. A line never typed, an idle shell and a gone
// pane need nothing (nil); a pane whose foreground carries another run's
// token, or that is not the role's, gets nothing: ErrNotOwn, or the error
// that kept herdr from saying.
func (m *Manager) InterruptShell(ctx context.Context, run store.Run) error {
	if !m.ShellLaunched(run.ID) {
		return nil
	}
	pr, sess, err := m.runSession(ctx, run)
	if err != nil {
		return err
	}
	pane := store.Deref(sess.HerdrPaneID)
	pi, err := m.d.Herdr.PaneProcessInfo(ctx, pane)
	switch {
	case isGone(err):
		return nil
	case err != nil:
		return fmt.Errorf("agents: interrupt %s in pane %s: %w", run.Role, pane, err)
	case herdr.IdleShell(pi):
		return nil
	}
	if !m.hasToken(pi) {
		// A token-less line: only the role's own pane, as before the token.
		if live, err := m.d.Store.LiveSessionByPRRole(ctx, pr.ID, run.Role); err != nil || live.ID != sess.ID || store.Deref(live.HerdrPaneID) != pane {
			return fmt.Errorf("agents: interrupt %s: %w", run.Role, notOwn(false, "pane %s is not the %s pane of the PR", pane, run.Role))
		}
	} else {
		o, err := m.ownShell(ctx, pr, sess, pi, run.ID)
		if err == nil && o.run.ID != run.ID {
			err = notOwn(false, "pane %s runs the line of run %s", pane, o.run.ID)
		}
		if err != nil {
			return fmt.Errorf("agents: interrupt %s in pane %s: %w", run.Role, pane, err)
		}
		if err := m.captureJob(ctx, &o); err != nil {
			m.logErr(ctx, err, "agents: %s: capture the job of run %s before its ctrl+c: %v", run.Role, run.ID, err)
		} else {
			m.remember(o)
		}
	}
	if err := m.d.Herdr.PaneSendKeys(ctx, pane, "ctrl+c"); err != nil {
		return fmt.Errorf("agents: interrupt %s in pane %s: %w", run.Role, pane, err)
	}
	return nil
}

// StopShell stops the command a shell role's run (the cut run) left
// running in its session's pane, when ownShell proves it an own run of
// magnum's (that run, or a run of the role no longer in flight), and only
// once RunShell typed the run's line (a line never typed: nil). It
// captures the job (captureJob: the members of the job's process group and
// the tree under the run token's db-lock, each with its identity, from the
// process table), records EventShellStopBegin, and then for SIGTERM and
// SIGKILL in turn (escalate): reads the process table again, asks herdr
// again (recheck), signals the group, reads the table once more and
// signals each captured process it shows as itself (pid and start:
// execx.Find), and waits up to shellStopWait for an idle shell and every
// captured process gone. A pane SIGKILL leaves busy is replaced
// (replaceShell). A pane that is an idle shell, or gone, is a stop only
// once the job captured for the run (at its interrupt, or at an earlier
// stop) is gone too (stopRemembered). Success is nil and EventShellStopped,
// anything else fails (EventShellStopFailed) and the command counts as
// still running. Without Deps.Signal nothing is signalled. It never marks
// a human at work.
func (m *Manager) StopShell(ctx context.Context, run store.Run) error {
	if !m.ShellLaunched(run.ID) {
		return nil // nothing was typed for it
	}
	pr, sess, err := m.runSession(ctx, run)
	if err != nil {
		return err
	}
	if err := m.stopShell(ctx, pr, sess, run.ID); err != nil {
		return err
	}
	m.shellDone(run.ID)
	return nil
}

// stopShell is StopShell for the command in session sess's pane; cut is
// the run that may still be in flight ("": none).
func (m *Manager) stopShell(ctx context.Context, pr store.PR, sess store.Session, cut string) error {
	pane := store.Deref(sess.HerdrPaneID)
	fail := func(err error) error { return fmt.Errorf("agents: stop %s in pane %s: %w", sess.Role, pane, err) }
	pi, err := m.d.Herdr.PaneProcessInfo(ctx, pane)
	switch {
	case isGone(err), err == nil && herdr.IdleShell(pi):
		if cut == "" {
			return nil
		}
		return m.stopRemembered(ctx, pr, cut)
	case err != nil:
		return fail(err)
	}
	o, err := m.ownShell(ctx, pr, sess, pi, cut)
	if err != nil {
		m.stopFailed(ctx, pr, sess, pane, cut, stopNotOwn, err)
		return fail(err)
	}
	if err := m.captureJob(ctx, &o); err != nil {
		m.stopFailed(ctx, pr, sess, pane, o.run.ID, stopSignal, err)
		return fail(err)
	}
	m.remember(o)
	o, _ = m.remembered(o.run.ID) // with what an earlier look captured
	m.event(ctx, m.prSubject(ctx, pr.ID), "warn", EventShellStopBegin,
		fmt.Sprintf("%s: the command of run %s still runs in pane %s: SIGTERM to its process group %d and the %d processes of its job, SIGKILL when it goes on",
			sess.Role, o.run.ID, pane, o.pgid, len(o.job)),
		map[string]any{"role": sess.Role, "pane": pane, "run": o.run.ID, "pgid": o.pgid, "tree": o.job})
	return m.escalate(ctx, pr, &o, true)
}

// stopRemembered stops what is left of the job captured for run runID
// (remember) once its pane is an idle shell or gone: nothing when no job
// was captured or every captured process is gone (the run's command
// stopped); else SIGTERM, then SIGKILL, to each captured process the table
// shows as itself (escalate on the idle pane), with the events of a stop.
func (m *Manager) stopRemembered(ctx context.Context, pr store.PR, runID string) error {
	o, ok := m.remembered(runID)
	if !ok {
		return nil
	}
	table, err := m.processTable(ctx, 0)
	if err != nil {
		m.stopFailed(ctx, pr, o.sess, o.pane, o.run.ID, stopSignal, err)
		return fmt.Errorf("agents: stop %s in pane %s: %w", o.sess.Role, o.pane, err)
	}
	o.refresh(table)
	if len(o.job) == 0 {
		m.forget(runID)
		return nil
	}
	m.event(ctx, m.prSubject(ctx, pr.ID), "warn", EventShellStopBegin,
		fmt.Sprintf("%s: the shell of pane %s is idle, but %d processes of run %s's job still run (%s): SIGTERM to each, SIGKILL when it goes on",
			o.sess.Role, o.pane, len(o.job), o.run.ID, jobNames(o.job)),
		map[string]any{"role": o.sess.Role, "pane": o.pane, "run": o.run.ID, "pgid": o.pgid, "tree": o.job})
	return m.escalate(ctx, pr, &o, false)
}

// escalate sends o's job SIGTERM, then SIGKILL, as StopShell says, and
// replaces a pane SIGKILL leaves busy when mayReplace. A stop that worked
// forgets the job; one that did not leaves it remembered.
func (m *Manager) escalate(ctx context.Context, pr store.PR, o *ownedShell, mayReplace bool) error {
	fail := func(cause string, err error) error {
		if cause == stopNotOwn {
			m.stopFailedOwn(ctx, pr, *o, err)
		} else {
			m.stopFailed(ctx, pr, o.sess, o.pane, o.run.ID, cause, err)
		}
		return fmt.Errorf("agents: stop %s in pane %s: %w", o.sess.Role, o.pane, err)
	}
	done := func(step string, began time.Time, repl string) error {
		m.forget(o.run.ID)
		m.shellDone(o.run.ID)
		m.stopped(ctx, pr, *o, step, began, repl)
		return nil
	}
	began := m.now()
	last := "exited"
	for i, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		table, err := m.processTable(ctx, 0)
		if err != nil {
			return fail(stopSignal, fmt.Errorf("nothing more was sent: %w", err))
		}
		o.refresh(table)
		idle, err := m.recheck(ctx, *o, table)
		if err != nil {
			return fail(stopNotOwn, err)
		}
		if len(o.job) == 0 && idle {
			return done(last, began, "")
		}
		if err := m.signalJob(ctx, o, idle, table, sig); err != nil {
			return fail(stopSignal, err)
		}
		last = []string{"term", "kill"}[i]
		paneIdle, gone := m.waitStopped(ctx, o, shellStopWait)
		if paneIdle && gone {
			return done(last, began, "")
		}
		if ctx.Err() != nil {
			return fail(stopSignal, fmt.Errorf("stopped waiting after %s: %w", sigName(sig), ctx.Err()))
		}
	}
	table, err := m.processTable(ctx, 0)
	if err != nil {
		return fail(stopSignal, err)
	}
	o.refresh(table)
	idle, err := m.recheck(ctx, *o, table)
	switch {
	case err != nil:
		return fail(stopNotOwn, err)
	case idle && len(o.job) == 0:
		return done(last, began, "")
	case idle || !mayReplace:
		m.remember(*o)
		return fail(stopSignal, fmt.Errorf("%d processes of its job still run after SIGKILL (%s)", len(o.job), jobNames(o.job)))
	}
	return m.replaceShell(ctx, pr, *o, began)
}

// processTable reads the process table within budget (0: the read's own
// bound).
func (m *Manager) processTable(ctx context.Context, budget time.Duration) ([]execx.Process, error) {
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	table, err := execx.ProcessTable(ctx, m.d.Runner)
	if err != nil {
		return nil, fmt.Errorf("cannot read the process table: %w", err)
	}
	return table, nil
}

// captureJob reads the job of o from the process table: the members of its
// process group and the processes under its db-lock (execx.Descendants:
// one in a group or a session of its own too), each with its identity. The
// db-lock must be a member of the group; the pane's shell, its group, pid
// 1 and the daemon are never in the job.
func (m *Manager) captureJob(ctx context.Context, o *ownedShell) error {
	table, err := m.processTable(ctx, 0)
	if err != nil {
		return fmt.Errorf("nothing was sent: %w", err)
	}
	if !slices.ContainsFunc(table, func(p execx.Process) bool { return p.PID == o.proc.PID && p.PGID == o.pgid && !p.Zombie }) {
		return notOwn(false, "the process table shows no db-lock %d in process group %d", o.proc.PID, o.pgid)
	}
	var job []execx.Process
	add := func(p execx.Process) {
		if p.Zombie || p.PID <= 1 || p.PID == o.shell || p.PGID == o.shell || p.PID == os.Getpid() || p.Start == "" ||
			slices.ContainsFunc(job, p.Same) {
			return
		}
		job = append(job, p)
	}
	for _, p := range table {
		if p.PGID == o.pgid {
			add(p)
		}
	}
	for _, p := range execx.Descendants(table, o.proc.PID) {
		add(p)
	}
	o.job = job
	return nil
}

// refresh keeps of o's job what table shows as itself (execx.Find: pid
// and start, whatever group it moved to), with its row of now, and adds
// the processes born under them since, which are the job's too.
func (o *ownedShell) refresh(table []execx.Process) {
	var job []execx.Process
	for _, j := range o.job {
		if now, ok := execx.Find(table, j); ok {
			job = append(job, now)
		}
	}
	for _, j := range slices.Clone(job) {
		for _, d := range execx.Descendants(table, j.PID) {
			if !d.Zombie && d.Start != "" && d.PID > 1 && d.PID != o.shell && d.PGID != o.shell && d.PID != os.Getpid() &&
				!slices.ContainsFunc(job, d.Same) {
				job = append(job, d)
			}
		}
	}
	o.job = job
}

// recheck asks herdr again what o's pane runs, right before a signal or
// the close: idle when it is an idle shell or gone, or when herdr still
// names the job's group but the table (read right before) shows no
// process in it any more (the job ended, the shell has not taken the
// terminal back yet). Otherwise the job must still be o's: the same shell
// and foreground group, a foreground process that o captured and the
// table still shows as itself in that group, and no foreground db-lock
// with another run's token. Anything else is a *notOwnError: another job
// runs there now, which nothing more is sent to.
func (m *Manager) recheck(ctx context.Context, o ownedShell, table []execx.Process) (idle bool, err error) {
	pi, err := m.d.Herdr.PaneProcessInfo(ctx, o.pane)
	switch {
	case isGone(err):
		return true, nil
	case err != nil:
		return false, notOwn(false, "herdr cannot say what pane %s runs (%v)", o.pane, err)
	case herdr.IdleShell(pi):
		return true, nil
	case pi.ForegroundPGID != o.pgid || pi.ShellPID != o.shell:
		return false, notOwn(true, "pane %s runs another job now", o.pane)
	}
	led := false
	for _, f := range pi.Foreground {
		if t, ok := runToken(f.Argv, m.d.Layout.Binary()); ok && t != o.tok {
			return false, notOwn(true, "pane %s runs another run's line now", o.pane)
		}
		for _, j := range o.job {
			if j.PID == f.PID {
				if now, ok := execx.Find(table, j); ok && now.PGID == o.pgid {
					led = true
				}
			}
		}
	}
	if !led {
		if !slices.ContainsFunc(table, func(p execx.Process) bool { return p.PGID == o.pgid && !p.Zombie }) {
			return true, nil // the group is gone: the job ended
		}
		return false, notOwn(true, "pane %s runs no process of the job magnum captured", o.pane)
	}
	return false, nil
}

// signalJob sends sig to o's job group (unless the pane is an idle shell
// now: the group is no longer its foreground), then reads the process table
// again and sends sig to each process of o's job it shows as itself, so a
// process the group signal ended and whose pid went to another gets
// nothing. A process already gone (ESRCH) is no failure; a table that
// cannot be read sends nothing more.
func (m *Manager) signalJob(ctx context.Context, o *ownedShell, idle bool, table []execx.Process, sig syscall.Signal) error {
	var errs []error
	if !idle && slices.ContainsFunc(o.job, func(j execx.Process) bool {
		now, ok := execx.Find(table, j)
		return ok && now.PGID == o.pgid
	}) {
		if err := m.signal(-o.pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("signal process group %d: %w", o.pgid, err))
		}
		fresh, err := m.processTable(ctx, 0)
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("after the group's %s: %w", sigName(sig), err))...)
		}
		table = fresh
		o.refresh(table)
	}
	for _, j := range o.job {
		if _, ok := execx.Find(table, j); !ok {
			continue
		}
		if err := m.signal(j.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("signal %s (pid %d): %w", j.Name, j.PID, err))
		}
	}
	return errors.Join(errs...)
}

// waitStopped waits until a deadline d from now for o's pane to be an idle
// shell (or gone) and for every process of o's job to be gone (a zombie
// is), and keeps in o what still runs. Every read and every sleep gets
// what is left of d (a last look at least readFloor); a table it cannot
// read leaves the job as it was.
func (m *Manager) waitStopped(ctx context.Context, o *ownedShell, d time.Duration) (paneIdle, gone bool) {
	deadline := m.now().Add(d)
	left := func() time.Duration { return deadline.Sub(m.now()) }
	_, err := m.d.Herdr.WaitIdleShell(ctx, o.pane, max(left(), 0))
	if paneIdle = err == nil || isGone(err); !paneIdle {
		return false, false
	}
	return true, m.waitGone(ctx, o, deadline)
}

// waitGone looks at the process table until deadline for every process
// of o's job to be gone; every read and sleep gets what is left of it.
func (m *Manager) waitGone(ctx context.Context, o *ownedShell, deadline time.Time) bool {
	left := func() time.Duration { return deadline.Sub(m.now()) }
	for i := 0; ; i++ {
		table, err := m.processTable(ctx, max(left(), readFloor))
		if err != nil {
			return false
		}
		o.refresh(table)
		if len(o.job) == 0 {
			return true
		}
		if left() <= 0 || i >= int(shellStopWait/treePoll) || m.sleep(ctx, min(treePoll, left())) != nil {
			return false
		}
	}
}

// signal is Deps.Signal; without it nothing is signalled (an error).
func (m *Manager) signal(pid int, sig syscall.Signal) error {
	if m.d.Signal == nil {
		return fmt.Errorf("agents: no Deps.Signal: pid %d not signalled", pid)
	}
	return m.d.Signal(pid, sig)
}

// sigName names a signal of StopShell's.
func sigName(s syscall.Signal) string {
	if s == syscall.SIGKILL {
		return "SIGKILL"
	}
	return "SIGTERM"
}

// jobNames names the processes of a job for a message: their names, never
// their argv.
func jobNames(job []execx.Process) string {
	names := make([]string, 0, len(job))
	for _, j := range job {
		names = append(names, j.Name)
	}
	return strings.Join(names, ",")
}

// replaceShell replaces o's pane, which SIGKILL left busy (see StopShell):
// a replacement split with the cwd and environment the PR's sessions
// recorded, the session pointed at it (only while it is live in the old
// pane, and marked live then, so an observe tick whose snapshot predates
// the move does not judge it lost), then herdr and the process table asked
// again right before the old pane's close. A pane that is an idle shell by
// then keeps the role (the replacement is closed again); one that runs
// another job now is left alone, the role kept there and the replacement
// closed, as not own. Without a recorded environment the pane closes all
// the same and the session is lost, as clearDeadLaunch does. The stop
// succeeds only once every captured process is gone after the close.
func (m *Manager) replaceShell(ctx context.Context, pr store.PR, o ownedShell, began time.Time) error {
	role := Role(o.sess.Role)
	failAs := func(cause string, err error) error {
		m.remember(o)
		m.stopFailed(ctx, pr, o.sess, o.pane, o.run.ID, cause, err)
		return fmt.Errorf("agents: stop %s in pane %s: %w", role, o.pane, err)
	}
	sessions, err := m.d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return failAs(stopClose, err)
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		snap = herdr.Snapshot{} // the sessions' cwd, then
	}
	repl, err := m.splitReplacement(ctx, snap, sessions, role, o.pane)
	if err != nil {
		return failAs(stopClose, fmt.Errorf("split a replacement: %w", err))
	}
	moved := false
	retire := func() {
		if moved {
			if err := m.pointSession(context.WithoutCancel(ctx), o.sess.ID, repl, o.pane); err != nil {
				m.logErr(ctx, err, "agents: point %s session %d back at pane %s: %v", role, o.sess.ID, o.pane, err)
			}
		}
		if repl != "" {
			if err := m.d.Herdr.PaneClose(context.WithoutCancel(ctx), repl); err != nil && !isGone(err) {
				m.logErr(ctx, err, "agents: close the unused replacement %s: %v", repl, err)
			}
		}
	}
	if repl != "" {
		if err := m.pointSession(ctx, o.sess.ID, o.pane, repl); err != nil {
			retire()
			if errors.Is(err, store.ErrConflict) {
				return failAs(stopNotOwn, notOwn(false, "the %s session is no longer live in pane %s", role, o.pane))
			}
			return failAs(stopClose, fmt.Errorf("point the session at the replacement %s: %w", repl, err))
		}
		moved = true
	}
	table, err := m.processTable(ctx, 0)
	if err != nil {
		retire()
		return failAs(stopSignal, fmt.Errorf("so the pane stays: %w", err))
	}
	o.refresh(table)
	idle, err := m.recheck(ctx, o, table)
	switch {
	case err != nil:
		retire()
		m.remember(o)
		m.stopFailedOwn(ctx, pr, o, err)
		return fmt.Errorf("agents: stop %s in pane %s: %w", role, o.pane, err)
	case idle:
		retire()
		if len(o.job) == 0 {
			m.forget(o.run.ID)
			m.shellDone(o.run.ID)
			m.stopped(ctx, pr, o, "kill", began, "")
			return nil
		}
		return failAs(stopSignal, fmt.Errorf("%d processes of its job still run after SIGKILL (%s)", len(o.job), jobNames(o.job)))
	}
	if err := m.d.Herdr.PaneClose(ctx, o.pane); err != nil && !isGone(err) {
		retire()
		return failAs(stopClose, fmt.Errorf("close the pane: %w: %w", ErrBusy, err))
	}
	if repl != "" {
		m.mu.Lock()
		m.replaced[o.pane] = repl
		m.mu.Unlock()
		if err := m.d.Herdr.PaneRename(ctx, repl, TaggedPaneLabel(m.d.Tag, pr.Number, role)); err != nil {
			m.logErr(ctx, err, "agents: name the replacement %s: %v", repl, err)
		}
	} else if err := m.afterClear(ctx, pr, role, sessions, o.pane, ""); err != nil {
		m.logErr(ctx, err, "agents: %s: after closing pane %s: %v", role, o.pane, err)
	}
	if !m.waitGone(ctx, &o, m.now().Add(shellStopWait)) {
		return failAs(stopSignal, fmt.Errorf("%d processes of its job still run after its pane closed (%s)", len(o.job), jobNames(o.job)))
	}
	m.forget(o.run.ID)
	m.shellDone(o.run.ID)
	if repl == "" {
		m.stopped(ctx, pr, o, "close", began, "")
		return nil
	}
	m.stopped(ctx, pr, o, "replace", began, repl)
	return nil
}

// pointSession points session id at pane to, only while it is live and
// in pane from (store.ErrConflict otherwise), and marks it live now: an
// observe tick whose snapshot predates the move does not judge it lost
// (LostGrace).
func (m *Manager) pointSession(ctx context.Context, id int64, from, to string) error {
	if err := m.d.Store.TransitionSession(ctx, id, liveStates, "", func(u *store.SessionUpdate) {
		u.Where("herdr_pane_id", from)
		u.Set("herdr_pane_id", to)
	}); err != nil {
		return err
	}
	m.markedLive(id, m.now())
	return nil
}

// stopped records EventShellStopped: step stopped o's command (repl: the
// pane that took its pane's place, "" for none).
func (m *Manager) stopped(ctx context.Context, pr store.PR, o ownedShell, step string, began time.Time, repl string) {
	secs := int(m.now().Sub(began).Round(time.Second) / time.Second)
	msg := fmt.Sprintf("%s: the command of run %s in pane %s stopped (%s, %ds)", o.sess.Role, o.run.ID, o.pane, step, secs)
	data := map[string]any{"role": o.sess.Role, "run": o.run.ID, "pane": o.pane, "step": step, "seconds": secs}
	if repl != "" {
		msg += "; the role goes on in pane " + repl
		data["replacement"] = repl
	}
	m.event(ctx, m.prSubject(ctx, pr.ID), "info", EventShellStopped, msg, data)
}

// stopFailedOwn is stopFailed for a recheck that found the pane is no
// longer o's (not_own).
func (m *Manager) stopFailedOwn(ctx context.Context, pr store.PR, o ownedShell, err error) {
	m.stopFailed(ctx, pr, o.sess, o.pane, o.run.ID, stopNotOwn, err)
}

// stopFailed records EventShellStopFailed with cause; run is the run the
// registry names ("" = none).
func (m *Manager) stopFailed(ctx context.Context, pr store.PR, sess store.Session, pane, run, cause string, err error) {
	data := map[string]any{"role": sess.Role, "pane": pane, "cause": cause}
	if run != "" {
		data["run"] = run
	}
	m.event(ctx, m.prSubject(ctx, pr.ID), "warn", EventShellStopFailed,
		fmt.Sprintf("%s: the command in pane %s was not stopped (%s): %v", sess.Role, pane, cause, err), data)
}

// readyShell is the pane RunShell types the line of run runID into: paneID
// once it is an idle shell, or the pane that took its place. A pane busy
// with an own run of the role no longer in flight (ownShell) is stopped at
// once (stopShell). Anything else gets IdleShellTimeout, as a fresh pane's
// shell needs a moment; when it is still busy then, the role moves to a new
// pane (moveShell) and the busy pane gets nothing: no key, no signal, no
// close. Without an environment to move with, it is ErrBusy as it was.
func (m *Manager) readyShell(ctx context.Context, pr store.PR, role config.Role, paneID, runID string) (string, error) {
	sess, err := m.d.Store.LiveSessionByPRRole(ctx, pr.ID, role.Name)
	ours := err == nil && store.Deref(sess.HerdrPaneID) == paneID && !m.isAgentSession(sess)
	if ours {
		if pi, err := m.d.Herdr.PaneProcessInfo(ctx, paneID); err == nil && !herdr.IdleShell(pi) {
			// The stop is bounded (two waits of shellStopWait, a new pane):
			// it ends even when the round does, so its events end too.
			if o, err := m.ownShell(ctx, pr, sess, pi, ""); err == nil && o.run.ID != runID &&
				m.stopShell(context.WithoutCancel(ctx), pr, sess, "") == nil {
				if cur, err := m.d.Store.SessionByID(ctx, sess.ID); err == nil && isLive(cur) && store.Deref(cur.HerdrPaneID) != "" {
					sess, paneID = cur, store.Deref(cur.HerdrPaneID) // a replacement when the kill left it busy
				}
			}
		}
	}
	pi, err := m.d.Herdr.WaitIdleShell(ctx, paneID, IdleShellTimeout)
	if err == nil {
		return paneID, nil
	}
	if !herdr.IsTimeout(err) {
		return "", fmt.Errorf("pane %s: %w", paneID, err)
	}
	busy := fmt.Errorf("pane %s: %w: %w", paneID, ErrBusy, err)
	if !ours {
		return "", busy
	}
	repl, merr := m.moveShell(ctx, pr, sess, pi)
	switch {
	case merr != nil:
		return "", fmt.Errorf("%w; no new pane: %w", busy, merr)
	case repl == "":
		return "", busy
	}
	if _, err := m.d.Herdr.WaitIdleShell(ctx, repl, IdleShellTimeout); err != nil {
		if herdr.IsTimeout(err) {
			return "", fmt.Errorf("pane %s: %w: %w", repl, ErrBusy, err)
		}
		return "", fmt.Errorf("pane %s: %w", repl, err)
	}
	return repl, nil
}

// moveShell moves session sess, a shell role's, from its pane, which pi
// shows something that is no run of magnum's keeps busy, to a new one:
// split from it with the cwd and environment the PR's sessions recorded
// (splitReplacement) and named like it; a closed row of the role records
// the busy pane, which CloseRetiredPanes closes once it is an idle shell,
// and sess points at the new pane. It returns the new pane; "" without a
// recorded environment (nothing done). Records EventShellMoveBegin, then
// EventShellMoved or EventShellMoveFailed.
func (m *Manager) moveShell(ctx context.Context, pr store.PR, sess store.Session, pi herdr.ProcessInfo) (string, error) {
	role, pane := Role(sess.Role), store.Deref(sess.HerdrPaneID)
	sessions, err := m.d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(sessions, func(s store.Session) bool { return s.Role == sess.Role && len(s.Env) > 0 }) {
		return "", nil
	}
	names := foregroundNames(pi)
	subject := m.prSubject(ctx, pr.ID)
	m.event(ctx, subject, "warn", EventShellMoveBegin,
		fmt.Sprintf("%s: pane %s is still busy after %s (foreground: %s) with no run of magnum's: the role moves to a new pane, and the busy one is left as it is",
			role, pane, IdleShellTimeout, cmp.Or(strings.Join(names, ","), "unknown")),
		map[string]any{"role": sess.Role, "pane": pane, "foreground": names})
	failed := func(cause string, err error) (string, error) {
		m.event(ctx, subject, "warn", EventShellMoveFailed, fmt.Sprintf("%s: no new pane for pane %s (%s): %v", role, pane, cause, err),
			map[string]any{"role": sess.Role, "pane": pane, "cause": cause})
		return "", err
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		snap = herdr.Snapshot{} // the sessions' cwd, then
	}
	repl, err := m.splitReplacement(ctx, snap, sessions, role, pane)
	if err != nil || repl == "" {
		return failed("split", cmp.Or(err, errors.New("no recorded environment")))
	}
	undo := func() {
		if err := m.d.Herdr.PaneClose(context.WithoutCancel(ctx), repl); err != nil && !isGone(err) {
			m.logErr(ctx, err, "agents: close the new pane %s again: %v", repl, err)
		}
	}
	now := m.now()
	if _, err := m.d.Store.CreateSession(ctx, store.Session{PRID: pr.ID, Role: sess.Role, Generation: sess.Generation,
		AgentKind: new(KindShell), HerdrWorkspaceID: sess.HerdrWorkspaceID, HerdrTabID: sess.HerdrTabID, HerdrPaneID: &pane,
		Cwd: sess.Cwd, State: store.SessionClosed, ClosedAt: &now}); err != nil {
		undo()
		return failed("registry", err)
	}
	if err := m.pointSession(ctx, sess.ID, pane, repl); err != nil {
		undo()
		return failed("registry", err)
	}
	if err := m.d.Herdr.PaneRename(ctx, repl, TaggedPaneLabel(m.d.Tag, pr.Number, role)); err != nil {
		m.logErr(ctx, err, "agents: name the new pane %s: %v", repl, err)
	}
	m.event(ctx, subject, "info", EventShellMoved, fmt.Sprintf("%s moved from pane %s to pane %s", role, pane, repl),
		map[string]any{"role": sess.Role, "from": pane, "to": repl})
	return repl, nil
}

// foregroundNames are the names of pi's foreground processes, never their
// argv.
func foregroundNames(pi herdr.ProcessInfo) []string {
	names := make([]string, 0, len(pi.Foreground))
	for _, p := range pi.Foreground {
		names = append(names, p.Name)
	}
	return names
}

// ClearShellPanes makes sure, before something switches the checkout of
// the slot at checkout (a round's, the fixer's, magnum open's, a release),
// that no command runs in a shell pane of the PR's there. First what is
// left of a job magnum captured for one of the PR's runs is stopped
// (stopRemembered). Then the PR's shell sessions, live or retired, whose
// cwd is checkout: only a pane herdr still shows as the session's (the
// herdr id checks of the retired panes, inHerdr, and paneReason for a live
// row, whose pane a herdr restart gave to another becomes lost), and that
// no other PR's live session records. A live session's pane busy with an
// own run of its role no longer in flight (ownShell) is stopped (stopShell,
// on a context an abort or a shutdown cannot cut between SIGTERM and
// SIGKILL; each step stays bounded). A pane still busy holds the switch: a
// *ShellHoldError (ErrShellHold), Foreign for a command magnum did not
// start, else magnum's own with why it cannot stop it, herdr unable to say
// included. It never marks a human at work, and records no event: the
// engine keeps the hold and says so.
func (m *Manager) ClearShellPanes(ctx context.Context, pr store.PR, checkout string) error {
	ctx = context.WithoutCancel(ctx)
	hold := func(s store.Session, pane string, foreign bool, cause string) error {
		return &ShellHoldError{Role: s.Role, Pane: pane, Checkout: checkout, Foreign: foreign, Cause: cause}
	}
	m.mu.Lock()
	var mine []ownedShell
	for _, o := range m.jobs {
		if o.run.PRID == pr.ID {
			mine = append(mine, o)
		}
	}
	m.mu.Unlock()
	for _, o := range mine {
		if err := m.stopRemembered(ctx, pr, o.run.ID); err != nil {
			return hold(o.sess, o.pane, false, execx.Redact(err.Error()))
		}
	}
	sessions, others, err := m.sessionsBesideOthers(ctx, pr)
	if err != nil {
		return fmt.Errorf("agents: the shell panes of pr %d: %w", pr.Number, err)
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return hold(store.Session{}, "", false, "herdr cannot say what its shell panes run: "+execx.Redact(err.Error()))
	}
	sessions, view := m.inHerdr(ctx, pr, snap, sessions)
	seen := map[string]bool{}
	for _, s := range slices.Backward(sessions) {
		pane := store.Deref(s.HerdrPaneID)
		if m.isAgentSession(s) || pane == "" || seen[pane] || store.Deref(s.Cwd) == "" || !samePath(store.Deref(s.Cwd), checkout) {
			continue
		}
		seen[pane] = true
		p, ok := view.panes[pane]
		if !ok || others.pane(pane) {
			continue // gone, or another PR's
		}
		if why := view.paneReason(p, s); why != "" {
			if isLive(s) { // herdr gave the pane to another: the row is lost, for the next round to lay a pane out
				if err := m.markLost(ctx, s); err != nil {
					m.logErr(ctx, err, "agents: %s session %d: %v", s.Role, s.ID, err)
				}
			}
			continue
		}
		pi, err := m.d.Herdr.PaneProcessInfo(ctx, pane)
		switch {
		case isGone(err):
			continue
		case err != nil:
			return hold(s, pane, false, "herdr cannot say what the pane runs: "+execx.Redact(err.Error()))
		case herdr.IdleShell(pi):
			continue
		}
		if !isLive(s) {
			return hold(s, pane, !m.ownToken(ctx, pr, s, pi), "its pane is no longer the role's, so magnum does not signal it")
		}
		_, err = m.ownShell(ctx, pr, s, pi, "")
		if no, ok := errors.AsType[*notOwnError](err); ok {
			if no.foreign {
				return hold(s, pane, true, "")
			}
			return hold(s, pane, false, no.why)
		}
		if err := m.stopShell(ctx, pr, s, ""); err != nil {
			return hold(s, pane, false, execx.Redact(err.Error()))
		}
	}
	return nil
}

// ownToken reports whether pi's foreground carries the run token of a run
// of PR pr's role s.Role in the session's checkout: magnum's own line in a
// pane that is no longer the role's.
func (m *Manager) ownToken(ctx context.Context, pr store.PR, s store.Session, pi herdr.ProcessInfo) bool {
	for _, p := range pi.Foreground {
		t, ok := runToken(p.Argv, m.d.Layout.Binary())
		if !ok || t.role != s.Role || !samePath(t.checkout, store.Deref(s.Cwd)) {
			continue
		}
		if r, err := m.d.Store.RunByID(ctx, t.run); err == nil && r.PRID == pr.ID && r.Role == s.Role {
			return true
		}
	}
	return false
}
