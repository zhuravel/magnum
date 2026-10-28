package agents

// A cut shell command is stopped for sure, and a pane something else keeps
// busy is left for a new one (DECISIONS "A cut shell command is killed, and
// a review that missed a reviewer is completed"): from 2026-10-16 to
// 2026-10-28, 12 codex-review runs ended busy. In 11 of them a push
// restarted the round, the restart's three ctrl+c did not end `codex
// review`, and the next run's line waited a minute for an idle shell and
// failed ("shell not idle after 1m0s (foreground: codex,magnum)"); the
// round went on without codex-review, and auto-approval refused its clean
// review. Magnum signals only a command it proves its own: the db-lock of
// the shell line carries the run's id (ShellDBLockLine, the run token), and
// herdr names the pane's foreground processes with their argv.

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
	"testing"
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
	// (pgid) and the tree under the run token's db-lock (tree: pid, pgid and
	// name of each, never argv).
	EventShellStopBegin = "pr.shell_stop_begin"
	// EventShellStopped (info): the step that stopped it (term, kill,
	// replace: the pane replaced, close: the pane closed without a
	// replacement) and the seconds it took.
	EventShellStopped = "pr.shell_stopped"
	// EventShellStopFailed (warn): the cause, not_own (nothing was sent:
	// the command is not proven an own run, or herdr shows another one now),
	// signal (a signal was refused) or close (the pane would not close).
	EventShellStopFailed = "pr.shell_stop_failed"
	// EventShellMoveBegin (warn), EventShellMoved (info) and
	// EventShellMoveFailed (warn, the cause split or registry): RunShell
	// moves a shell role to a new pane, as something that is no run of
	// magnum's keeps its pane busy; the busy pane gets nothing.
	EventShellMoveBegin  = "pr.shell_move_begin"
	EventShellMoved      = "pr.shell_moved"
	EventShellMoveFailed = "pr.shell_move_failed"
)

// ErrNotOwn is StopShell's answer when the command a shell role's pane runs
// is not proven an own run of magnum's (ownShell): it sent nothing.
var ErrNotOwn = errors.New("not magnum's own run")

// shellStopWait bounds the wait for an idle shell after each of StopShell's
// signals.
const shellStopWait = 5 * time.Second

// The causes of EventShellStopFailed.
const (
	stopNotOwn = "not_own"
	stopSignal = "signal"
	stopClose  = "close"
)

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

// ownedShell is a command in a shell role's pane that ownShell proved an
// own run of magnum's: the pane, its shell, the job's process group, the
// db-lock carrying the run token and the run it names.
type ownedShell struct {
	sess        store.Session
	pane        string
	shell, pgid int
	proc        herdr.Process
	tok         shellToken
	run         store.Run
}

// ownShell proves that pi, what session sess's pane runs, is an own run of
// magnum's, which only then may be signalled or closed. All of these hold:
// sess is the live shell session of the PR's role (its pane the role's);
// the pane's foreground job is not its shell, and holds a db-lock whose
// argv is magnum's binary with this role, this checkout (the session's
// cwd) and a run (runToken); that run is a run of this PR and role in the
// registry; and it is the cut run, or no longer in flight. Anything else
// is ErrNotOwn, with why.
func (m *Manager) ownShell(ctx context.Context, pr store.PR, sess store.Session, pi herdr.ProcessInfo, cut string) (ownedShell, error) {
	notOwn := func(format string, a ...any) (ownedShell, error) {
		return ownedShell{}, fmt.Errorf("%s: %w", fmt.Sprintf(format, a...), ErrNotOwn)
	}
	pane := store.Deref(sess.HerdrPaneID)
	if sess.PRID != pr.ID || !isLive(sess) || m.isAgentSession(sess) || pane == "" {
		return notOwn("session %d is no live shell session of the PR", sess.ID)
	}
	if live, err := m.d.Store.LiveSessionByPRRole(ctx, pr.ID, sess.Role); err != nil || live.ID != sess.ID || store.Deref(live.HerdrPaneID) != pane {
		return notOwn("pane %s is not the %s pane of the PR", pane, sess.Role)
	}
	if pi.ShellPID <= 1 || pi.ForegroundPGID <= 1 || pi.ForegroundPGID == pi.ShellPID {
		return notOwn("pane %s runs no job", pane)
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
			return notOwn("pane %s runs two db-locks", pane)
		}
		proc, tok, found = p, t, true
	}
	switch {
	case !found:
		return notOwn("pane %s runs no db-lock with a run token", pane)
	case proc.PID <= 1 || proc.PID == pi.ShellPID || proc.PID == os.Getpid():
		return notOwn("pane %s: the db-lock's pid %d", pane, proc.PID)
	case tok.role != sess.Role:
		return notOwn("pane %s runs a line of %s", pane, tok.role)
	case store.Deref(sess.Cwd) == "" || !samePath(tok.checkout, store.Deref(sess.Cwd)):
		return notOwn("pane %s runs a line for another checkout", pane)
	}
	run, err := m.d.Store.RunByID(ctx, tok.run)
	switch {
	case err != nil:
		return notOwn("run %s: %v", tok.run, err)
	case run.PRID != pr.ID || run.Role != sess.Role:
		return notOwn("run %s is no %s run of the PR", run.ID, sess.Role)
	case run.ID != cut && runInFlight(run):
		return notOwn("run %s is in flight", run.ID)
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

// StopShell stops the command a shell role's run (the cut run) left
// running in its session's pane, when ownShell proves it an own run of
// magnum's (that run, or a run of the role no longer in flight): SIGTERM
// to the job's process group and to the tree under the run token's db-lock
// (execx.Descendants of the process table, so a sandbox's child in a group
// of its own too), then up to shellStopWait for an idle shell; SIGKILL the
// same way and another wait; and when the pane is still busy, a
// replacement split with the cwd and environment the sessions recorded,
// the session pointed at it, and the old pane closed (without a recorded
// environment the pane closes all the same and the session is lost, as
// clearDeadLaunch does). herdr is asked again before each signal and the
// close: a job group or a db-lock that changed gets nothing. An idle shell,
// or a pane that is gone, is nil at once. Not own is ErrNotOwn, nothing
// sent. Records EventShellStopBegin before the first signal, then
// EventShellStopped or EventShellStopFailed.
func (m *Manager) StopShell(ctx context.Context, run store.Run) error {
	if run.SessionID == nil {
		return nil // nothing was typed for it
	}
	sess, err := m.d.Store.SessionByID(ctx, *run.SessionID)
	if err != nil {
		return fmt.Errorf("agents: stop %s run %s: %w", run.Role, run.ID, err)
	}
	pr, err := m.d.Store.PRByID(ctx, run.PRID)
	if err != nil {
		return fmt.Errorf("agents: stop %s run %s: %w", run.Role, run.ID, err)
	}
	return m.stopShell(ctx, pr, sess, run.ID)
}

// stopShell is StopShell for the command in session sess's pane; cut is
// the run that may still be in flight ("": none).
func (m *Manager) stopShell(ctx context.Context, pr store.PR, sess store.Session, cut string) error {
	pane := store.Deref(sess.HerdrPaneID)
	fail := func(err error) error { return fmt.Errorf("agents: stop %s in pane %s: %w", sess.Role, pane, err) }
	pi, err := m.d.Herdr.PaneProcessInfo(ctx, pane)
	switch {
	case isGone(err):
		return nil
	case err != nil:
		return fail(err)
	case herdr.IdleShell(pi):
		return nil
	}
	o, err := m.ownShell(ctx, pr, sess, pi, cut)
	if err != nil {
		m.stopFailed(ctx, pr, sess, pane, cut, stopNotOwn, err)
		return fail(err)
	}
	var tree []execx.Process
	began := m.now()
	steps := []struct {
		name string
		sig  syscall.Signal
	}{{"term", syscall.SIGTERM}, {"kill", syscall.SIGKILL}}
	last := ""
	for i, step := range steps {
		idle, err := m.recheck(ctx, o)
		switch {
		case idle && i == 0:
			return nil // it ended on its own: nothing was sent
		case idle:
			m.stopped(ctx, pr, o, last, began, "")
			return nil
		case err != nil:
			m.stopFailed(ctx, pr, sess, pane, o.run.ID, stopNotOwn, err)
			return fail(err)
		}
		tree = m.runTree(ctx, o, tree)
		if i == 0 {
			m.event(ctx, m.prSubject(ctx, pr.ID), "warn", EventShellStopBegin,
				fmt.Sprintf("%s: the command of run %s still runs in pane %s: SIGTERM to its process group %d and the %d processes under its db-lock, SIGKILL when it goes on",
					sess.Role, o.run.ID, pane, o.pgid, len(tree)),
				map[string]any{"role": sess.Role, "pane": pane, "run": o.run.ID, "pgid": o.pgid, "tree": tree})
		}
		if err := m.signalJob(o, tree, step.sig); err != nil {
			m.stopFailed(ctx, pr, sess, pane, o.run.ID, stopSignal, err)
			return fail(err)
		}
		last = step.name
		switch _, err := m.d.Herdr.WaitIdleShell(ctx, pane, shellStopWait); {
		case err == nil, isGone(err):
			m.stopped(ctx, pr, o, step.name, began, "")
			return nil
		case ctx.Err() != nil:
			m.stopFailed(ctx, pr, sess, pane, o.run.ID, stopSignal, fmt.Errorf("stopped waiting after SIG%s: %w", strings.ToUpper(step.name), ctx.Err()))
			return fail(ctx.Err())
		}
	}
	idle, err := m.recheck(ctx, o)
	switch {
	case idle:
		m.stopped(ctx, pr, o, last, began, "")
		return nil
	case err != nil:
		m.stopFailed(ctx, pr, sess, pane, o.run.ID, stopNotOwn, err)
		return fail(err)
	}
	return m.replaceShell(ctx, pr, o, began)
}

// recheck asks herdr again what o's pane runs, right before a signal or
// the close: idle when it is an idle shell or gone; ErrNotOwn when its job
// group or its db-lock changed since ownShell looked.
func (m *Manager) recheck(ctx context.Context, o ownedShell) (idle bool, err error) {
	pi, err := m.d.Herdr.PaneProcessInfo(ctx, o.pane)
	switch {
	case isGone(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("herdr cannot say what pane %s runs (%w): %w", o.pane, err, ErrNotOwn)
	case herdr.IdleShell(pi):
		return true, nil
	case pi.ForegroundPGID != o.pgid || pi.ShellPID != o.shell:
		return false, fmt.Errorf("pane %s runs another job now: %w", o.pane, ErrNotOwn)
	}
	if !slices.ContainsFunc(pi.Foreground, func(p herdr.Process) bool {
		t, ok := runToken(p.Argv, m.d.Layout.Binary())
		return p.PID == o.proc.PID && ok && t == o.tok
	}) {
		return false, fmt.Errorf("pane %s runs another db-lock now: %w", o.pane, ErrNotOwn)
	}
	return false, nil
}

// runTree is the tree under o's db-lock in the process table now, and
// those of earlier (an earlier signal's tree) the table still shows under
// the same name: a child whose parent the earlier signal ended is under
// the db-lock no more. The pane's shell and its group, pid 1 and the
// daemon are never in it. Without a table it is earlier, as it stands.
func (m *Manager) runTree(ctx context.Context, o ownedShell, earlier []execx.Process) []execx.Process {
	if m.d.Runner == nil {
		return earlier
	}
	table, err := execx.ProcessTable(ctx, m.d.Runner)
	if err != nil {
		m.logErr(ctx, err, "agents: %s: the processes under run %s's db-lock: %v", o.sess.Role, o.run.ID, err)
		return earlier
	}
	tree := execx.Descendants(table, o.proc.PID)
	for _, e := range earlier {
		still := slices.ContainsFunc(table, func(t execx.Process) bool { return t.PID == e.PID && t.Name == e.Name })
		if still && !slices.ContainsFunc(tree, func(t execx.Process) bool { return t.PID == e.PID }) {
			tree = append(tree, e)
		}
	}
	return slices.DeleteFunc(tree, func(p execx.Process) bool {
		return p.PID <= 1 || p.PID == o.shell || p.PGID == o.shell || p.PID == os.Getpid()
	})
}

// signalJob sends sig to o's job group and to every process of tree; a
// process already gone (ESRCH) is no failure.
func (m *Manager) signalJob(o ownedShell, tree []execx.Process, sig syscall.Signal) error {
	var errs []error
	if err := m.signal(-o.pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		errs = append(errs, fmt.Errorf("signal process group %d: %w", o.pgid, err))
	}
	for _, p := range tree {
		if err := m.signal(p.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("signal %s (pid %d): %w", p.Name, p.PID, err))
		}
	}
	return errors.Join(errs...)
}

// signal is Deps.Signal, else syscall.Kill; a test binary never signals a
// real process.
func (m *Manager) signal(pid int, sig syscall.Signal) error {
	if m.d.Signal != nil {
		return m.d.Signal(pid, sig)
	}
	if testing.Testing() {
		return fmt.Errorf("agents: no Deps.Signal in a test binary: pid %d not signalled", pid)
	}
	return syscall.Kill(pid, sig)
}

// replaceShell replaces o's pane, which SIGKILL left busy (see StopShell).
func (m *Manager) replaceShell(ctx context.Context, pr store.PR, o ownedShell, began time.Time) error {
	role := Role(o.sess.Role)
	fail := func(err error) error {
		m.stopFailed(ctx, pr, o.sess, o.pane, o.run.ID, stopClose, err)
		return fmt.Errorf("agents: stop %s in pane %s: %w", role, o.pane, err)
	}
	sessions, err := m.d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return fail(err)
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		snap = herdr.Snapshot{} // the sessions' cwd, then
	}
	repl, err := m.splitReplacement(ctx, snap, sessions, role, o.pane)
	if err != nil {
		return fail(fmt.Errorf("split a replacement: %w", err))
	}
	if err := m.d.Herdr.PaneClose(ctx, o.pane); err != nil && !isGone(err) {
		if repl != "" {
			if cerr := m.d.Herdr.PaneClose(context.WithoutCancel(ctx), repl); cerr != nil && !isGone(cerr) {
				m.logErr(ctx, cerr, "agents: close the replacement %s again: %v", repl, cerr)
			}
		}
		return fail(fmt.Errorf("close the pane: %w: %w", ErrBusy, err))
	}
	if err := m.afterClear(ctx, pr, role, sessions, o.pane, repl); err != nil {
		m.logErr(ctx, err, "agents: %s: after closing pane %s: %v", role, o.pane, err)
	}
	if repl == "" {
		m.stopped(ctx, pr, o, "close", began, "")
		return nil
	}
	m.stopped(ctx, pr, o, "replace", began, repl)
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

// stopFailed records EventShellStopFailed with cause.
func (m *Manager) stopFailed(ctx context.Context, pr store.PR, sess store.Session, pane, run, cause string, err error) {
	m.event(ctx, m.prSubject(ctx, pr.ID), "warn", EventShellStopFailed,
		fmt.Sprintf("%s: the command in pane %s was not stopped (%s): %v", sess.Role, pane, cause, err),
		map[string]any{"role": sess.Role, "pane": pane, "run": run, "cause": cause})
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
	names := make([]string, 0, len(pi.Foreground))
	for _, p := range pi.Foreground {
		names = append(names, p.Name)
	}
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
	if err := m.d.Store.UpdateSession(ctx, sess.ID, func(u *store.SessionUpdate) { u.Set("herdr_pane_id", repl) }); err != nil {
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
