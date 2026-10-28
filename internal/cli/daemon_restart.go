package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
)

const (
	// daemonStopTimeout bounds the wait for a SIGTERMed daemon to exit: its
	// shutdown (rounds and heavy jobs are cancelled, then the toasts drain)
	// may take as long as launchd lets the job run after SIGTERM (the plist's
	// ExitTimeOut), and a little more.
	daemonStopTimeout = launchd.ExitTimeOut*time.Second + 5*time.Second
	daemonPollEvery   = 500 * time.Millisecond
	// launchdThrottle is the plist's ThrottleInterval: launchd starts the job
	// at most once per this long, so a restarted daemon can take that long
	// to appear.
	launchdThrottle = 10 * time.Second
	// launchdStartWait bounds the wait for launchd to report the new pid,
	// past its throttle, so a normal restart is not taken for a failed one.
	launchdStartWait = launchdThrottle + 5*time.Second
	// restartStepBudget bounds daemon-restart's stop-then-start step, which
	// an interrupt no longer cuts short: the stop of a daemon started by
	// hand, the kickstart (which waits out the old daemon's shutdown like a
	// stop), the wait for the new pid, the reload of a job launchd could not
	// spawn (its wait for the job to unload, as long as a stop) and the wait
	// after it, and a minute for the launchctl and ps calls between.
	restartStepBudget = 3*daemonStopTimeout + 2*launchdStartWait + time.Minute
)

// errLaunchdNotStarted is waitLaunchdStarted's answer when no new daemon ran
// within launchdStartWait.
var errLaunchdNotStarted = errors.New("no new daemon was running")

const daemonRestartUsage = "daemon-restart [--now | --drain | --when-idle] [--timeout D]"

// restartFlags are the parsed rounds-in-flight flags of daemon-restart and
// install (--when-idle: daemon-restart only).
type restartFlags struct {
	now, drain, whenIdle bool
	timeout              time.Duration
}

// addRestartFlags declares --now, --drain and --timeout on cmd.
func addRestartFlags(cmd *cobra.Command, f *restartFlags) {
	fs := cmd.Flags()
	fs.BoolVar(&f.now, "now", false, "restart even while review rounds, a notes curation or critique, the retro, a skill edit or gate, a fix job or a whiteboard job are in flight (rounds start over)")
	fs.BoolVar(&f.drain, "drain", false, "start nothing new, wait for the rounds and background jobs in flight, then restart")
	fs.DurationVar(&f.timeout, "timeout", defaultDrainTimeout, "with --drain: give up (and restart nothing) after this long")
}

// check refuses more than one of --now, --drain and --when-idle, and a
// non-positive --timeout.
func (f restartFlags) check(c *Context, cmd, usage string) bool {
	switch {
	case f.now && f.whenIdle, f.drain && f.whenIdle:
		fmt.Fprintf(c.Stderr, "magnum %s: use only one of --now, --drain and --when-idle\nusage: magnum %s\n", cmd, usage)
		return false
	case f.now && f.drain:
		fmt.Fprintf(c.Stderr, "magnum %s: use either --now or --drain\nusage: magnum %s\n", cmd, usage)
		return false
	case f.timeout <= 0:
		fmt.Fprintf(c.Stderr, "magnum %s: --timeout must be positive\nusage: magnum %s\n", cmd, usage)
		return false
	}
	return true
}

func newDaemonRestartCmd(c *Context) *cobra.Command {
	var f restartFlags
	cmd := newCommand(groupDaemon, daemonRestartUsage, "restart the daemon (launchctl kickstart; a daemon outside launchd is stopped)",
		"Restart the daemon without rewriting the launchd plist: `launchctl kickstart -k` restarts the loaded "+
			"agent on the current binary, after stopping a daemon that was started by hand. Without a loaded "+
			"agent a running daemon is stopped (SIGTERM) and `magnum install` is suggested. The binary launchd runs "+
			"checks the configuration and renders every prompt first (`<binary> config`); the restart is refused when it "+
			"fails. While review rounds, a notes curation or critique, the retro, a skill edit or gate, a fix job, a whiteboard job, "+
			"a job on the heavy worker (a slot's provision, release or eviction, a slot request, the reconcile) or a GitHub "+
			"write as you (an automatic approval or its withdrawal) are in flight the restart is refused, because "+
			"it abandons them; --drain starts nothing new, waits for those in flight (a line every 15s, at most "+
			"--timeout, default 2h) and then restarts; --when-idle waits the same way without stopping anything and "+
			"restarts at the first moment nothing runs (it waits again when something starts in between); --now "+
			"restarts at once, and gives a GitHub write as you that started 30s to finish. ctrl+c, or closing the terminal, stops a wait and lifts a drain; once the restart "+
			"stops the daemon it finishes even after ctrl+c, so the daemon is not left down. "+fmt.Sprintf("It waits %s for the new daemon "+
			"(launchd's %s throttle, and more); when none runs and launchd reports that it cannot spawn the job (spawn failed, last exit "+
			"code 78), it reloads the job once, as `magnum install` does (launchctl bootout, then bootstrap of the same plist), unless a "+
			"daemon runs by then, and waits again.", launchdStartWait, launchdThrottle),
		func(pos []string) int { return runDaemonRestartCmd(c, pos, f) })
	addRestartFlags(cmd, &f)
	cmd.Flags().Lookup("timeout").Usage = "with --drain or --when-idle: give up (and restart nothing) after this long"
	cmd.Flags().BoolVar(&f.whenIdle, "when-idle", false, "wait, stopping nothing, until nothing is in flight (rounds, background jobs), then restart")
	return cmd
}

func runDaemonRestartCmd(c *Context, pos []string, f restartFlags) int {
	if !daemonGroupNoArgs(c, "daemon-restart", daemonRestartUsage, pos) || !f.check(c, "daemon-restart", daemonRestartUsage) {
		return 2
	}
	ctx, stop := daemonSys.signals()
	defer stop()
	out := c.Stdout
	run := daemonSys.runner(false, out)
	if !verifyBuild(ctx, c, run, "daemon-restart") {
		return 1
	}
	endDrain, ok := c.guardRounds(ctx, run, "daemon-restart", f)
	if !ok {
		return 1
	}
	defer endDrain()
	uid := daemonSys.UID()
	label := launchd.DefaultLabel

	st, err := launchd.Status(ctx, run, uid, label)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum daemon-restart: warning: %v\n", err)
	}
	pid, err := daemonSys.DaemonPID(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum daemon-restart: warning: %v\n", err)
		pid = 0
	}
	// --when-idle holds nothing: a round (or a notes curation, a retro) may
	// have started since the wait ended. The restart must not abandon it, so
	// the wait starts again.
	for f.whenIdle && pid > 0 {
		rounds, err := c.activeRounds(ctx)
		if err == nil && rounds.none() {
			break
		}
		if err == nil {
			what := "a round"
			if len(rounds.work) > 0 {
				what = "work"
			}
			fmt.Fprintf(out, "%s started in between (%s); waiting again\n", what, rounds)
		}
		if !waitIdle(ctx, c, "daemon-restart", f.timeout) {
			return 1
		}
	}

	// From here on the restart stops a daemon: an interrupt that cut the step
	// short between the stop and the start would leave no daemon running, so
	// the step runs to its end, bounded by its own budget.
	step, cancelStep := context.WithTimeout(context.WithoutCancel(ctx), restartStepBudget)
	defer cancelStep()
	if st.Loaded() {
		fmt.Fprintln(out, "restarting: the restart finishes even after ctrl+c, so the daemon is not left down")
		// A daemon started by hand holds the lock: launchd's copy would exit
		// at once as "already running", so stop it first.
		if pid > 0 && pid != st.PID {
			if err := stopMagnumDaemon(step, run, c.Layout, pid); err != nil {
				fmt.Fprintf(c.Stderr, "magnum daemon-restart: %v\n", err)
				return 1
			}
			fmt.Fprintf(out, "stopped the daemon running outside launchd (pid %d)\n", pid)
		}
		if err := launchd.Kickstart(step, run, uid, label); err != nil {
			fmt.Fprintf(c.Stderr, "magnum daemon-restart: %v\nfix: reload the job with `magnum install`\n", err)
			return 1
		}
		after, err := waitLaunchdStarted(step, run, uid, label, st.PID)
		if errors.Is(err, errLaunchdNotStarted) && after.CannotSpawn() {
			return reloadUnspawnable(step, c, run, uid, label, st.PID, after)
		}
		if err != nil {
			why := fmt.Sprintf("no new daemon was running after %s", launchdStartWait)
			if !errors.Is(err, errLaunchdNotStarted) {
				why = fmt.Sprintf("the wait for the new daemon was cut short (%v)", err)
			}
			fmt.Fprintf(c.Stderr, "magnum daemon-restart: launchd restarted %s, but %s (%s)\n"+
				"fix: read %s and %s, then `magnum daemon-restart` again (or `magnum install` to reload the job)\n",
				label, why, describeLaunchdState(after), launchd.LogPath(c.Layout.Logs()), c.Layout.DaemonLog())
			return 1
		}
		fmt.Fprintf(out, "restarted %s via launchd (%s); logs: %s\n", label, describeLaunchdState(after), c.Layout.DaemonLog())
		return 0
	}

	// Fallback: launchd does not manage magnum.
	if pid == 0 {
		fmt.Fprintf(c.Stderr, "magnum daemon-restart: the daemon is not running and launchd does not manage it (gui/%d/%s is not loaded)\n"+
			"fix: run `magnum install` to let launchd run it, or start it in the foreground with `magnum daemon`\n", uid, label)
		return 1
	}
	if err := stopMagnumDaemon(step, run, c.Layout, pid); err != nil {
		fmt.Fprintf(c.Stderr, "magnum daemon-restart: %v\n", err)
		return 1
	}
	fmt.Fprintf(c.Stderr, "magnum daemon-restart: stopped the daemon (pid %d), but launchd does not manage magnum, so nothing starts it again\n"+
		"fix: run `magnum install` to let launchd keep it running, or start it again with `magnum daemon`\n", pid)
	return 1
}

// stopMagnumDaemon sends SIGTERM to pid after checking it really is a magnum
// daemon (the pidfile may name a recycled pid), then waits for it to exit,
// at most daemonStopTimeout, or until ctx ends (an error saying the wait was
// interrupted; the SIGTERM stays sent).
func stopMagnumDaemon(ctx context.Context, run execx.Runner, layout paths.Layout, pid int) error {
	comm, args, err := engine.ProcessCommand(ctx, run, pid)
	if err != nil {
		// ps fails for a pid that is gone: the daemon may have exited
		// between the pidfile read and ps (a clean exit removes the pidfile).
		if cur, perr := daemonSys.DaemonPID(layout); perr == nil && cur != pid {
			return nil
		}
		return fmt.Errorf("cannot check that pid %d from %s is a magnum daemon (%v)\nfix: remove the stale pidfile if that process is gone, then retry", pid, layout.Pid(), err)
	}
	if !engine.LooksLikeDaemon(comm, args) {
		return fmt.Errorf("pid %d in %s is %q, not a magnum daemon; refusing to signal it\nfix: remove the stale pidfile %s, then retry",
			pid, layout.Pid(), execx.Redact(args), layout.Pid())
	}
	if err := daemonSys.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("signal daemon pid %d: %w", pid, err)
	}
	for waited := time.Duration(0); waited < daemonStopTimeout; waited += daemonPollEvery {
		if cur, err := daemonSys.DaemonPID(layout); err == nil && cur != pid {
			return nil
		}
		if err := daemonSys.Sleep(ctx, daemonPollEvery); err != nil {
			return fmt.Errorf("interrupted while waiting for daemon pid %d to exit after SIGTERM: %w\nfix: `magnum status` shows whether it still runs", pid, err)
		}
	}
	return fmt.Errorf("daemon pid %d did not exit within %s of SIGTERM\nfix: check %s, then `kill %d` again", pid, daemonStopTimeout, layout.DaemonLog(), pid)
}

// waitLaunchdStarted polls launchd until the job runs with a pid other than
// oldPID (nil), launchdStartWait passes (errLaunchdNotStarted), or ctx ends
// (ctx's error); it returns the last state seen either way.
func waitLaunchdStarted(ctx context.Context, run execx.Runner, uid int, label string, oldPID int) (launchd.Info, error) {
	var st launchd.Info
	for waited := time.Duration(0); ; waited += daemonPollEvery {
		st, _ = launchd.Status(ctx, run, uid, label)
		if st.State == launchd.Running && st.PID > 0 && st.PID != oldPID {
			return st, nil
		}
		if waited >= launchdStartWait {
			return st, errLaunchdNotStarted
		}
		if err := daemonSys.Sleep(ctx, daemonPollEvery); err != nil {
			return st, err
		}
	}
}

// reloadCheck is reloadGuard's answer: the job is reloaded only when reload
// says so.
type reloadCheck struct {
	st      launchd.Info // the state read right before the bootout
	started bool         // launchd runs a new daemon now: nothing to reload
	held    int          // a live pid in the pidfile: a daemon outside launchd, or a recycled pid
	refused string       // why else the job must not be reloaded
}

// reload reports whether the guard lets the reload run.
func (k reloadCheck) reload() bool { return !k.started && k.held == 0 && k.refused == "" }

// errReloadHeld is the guard's no to launchd.Reload; its reloadCheck says why.
var errReloadHeld = errors.New("the guard holds the reload")

// reloadGuard re-reads, right before the bootout, what calls for the
// reload: launchd knows the job, runs no process for it and still cannot
// spawn it (CannotSpawn), and no live pid is in the pidfile. A new daemon (a
// pid other than oldPID) that launchd started meanwhile is started, a pid in
// the pidfile is held for the caller to name; it reads nothing else.
func reloadGuard(ctx context.Context, run execx.Runner, layout paths.Layout, uid int, label string, oldPID int) reloadCheck {
	st, err := launchd.Status(ctx, run, uid, label)
	switch {
	case err != nil:
		return reloadCheck{st: st, refused: fmt.Sprintf("cannot read the job's state (%v)", err)}
	case !st.Loaded():
		return reloadCheck{st: st, refused: "the job is not loaded"}
	case st.State == launchd.Running && st.PID > 0 && st.PID != oldPID:
		return reloadCheck{st: st, started: true}
	case st.State == launchd.Running || st.PID > 0:
		return reloadCheck{st: st, refused: fmt.Sprintf("launchd runs the job (%s)", describeLaunchdState(st))}
	case !st.CannotSpawn():
		return reloadCheck{st: st, refused: fmt.Sprintf("launchd no longer reports a failed spawn (%s)", describeLaunchdState(st))}
	}
	switch pid, err := daemonSys.DaemonPID(layout); {
	case err != nil:
		return reloadCheck{st: st, refused: fmt.Sprintf("cannot read the pidfile (%v)", err)}
	case pid > 0:
		return reloadCheck{st: st, held: pid}
	}
	return reloadCheck{st: st}
}

// notADaemon says why pid is not a magnum daemon, as ps sees it ("" when it
// is one): the pidfile may name a recycled pid.
func notADaemon(ctx context.Context, run execx.Runner, pid int) string {
	comm, args, err := engine.ProcessCommand(ctx, run, pid)
	switch {
	case err != nil:
		return fmt.Sprintf("ps cannot tell what it is (%v)", err)
	case !engine.LooksLikeDaemon(comm, args):
		return fmt.Sprintf("is not a magnum daemon (%q), so the pidfile is stale: remove it", execx.Redact(args))
	}
	return ""
}

// reloadUnspawnable heals a restart that launchd cannot spawn (first: the
// state launchdStartWait ended on, CannotSpawn) and returns the command's
// exit code. It writes daemon.reload_begin and finds the plist first, then
// reloads the job once as `magnum install` loads it: launchd.Reload of the
// plist on disk, which checks the plist is the job's and asks reloadGuard
// right before the bootout, with no registry write or other wait between
// them. It waits for the new daemon again. One event ends every begin:
// daemon.reloaded, daemon.reload_failed, daemon.reload_skipped (a daemon runs
// by the guard's look: success, the job left as it was) or
// daemon.reload_refused (the job left as it was, and why); nothing tries it
// twice.
func reloadUnspawnable(ctx context.Context, c *Context, run execx.Runner, uid int, label string, oldPID int, first launchd.Info) int {
	state := describeLaunchdState(first)
	notStarted := fmt.Sprintf("launchd restarted %s, but no new daemon was running after %s (%s)", label, launchdStartWait, state)
	logs := fmt.Sprintf("read %s and %s", launchd.LogPath(c.Layout.Logs()), c.Layout.DaemonLog())
	installFix := fmt.Sprintf("fix: %s, check `launchctl print gui/%d/%s`, then `magnum install --now`, which writes the plist again and loads it",
		logs, uid, label)
	daemonSys.restartEvent(ctx, c, "warn", "daemon.reload_begin", fmt.Sprintf("launchd could not spawn %s %s after `magnum daemon-restart` (%s): "+
		"reloading the job once (pid %d)", label, launchdStartWait, state, os.Getpid()))
	refuse := func(why string) int {
		why = execx.Redact(why)
		daemonSys.restartEvent(ctx, c, "warn", "daemon.reload_refused", fmt.Sprintf("%s was not reloaded: %s", label, why))
		fmt.Fprintf(c.Stderr, "magnum daemon-restart: %s; the job was not reloaded: %s\n"+
			"fix: %s, then `magnum daemon-restart` again (or `magnum install` to reload the job)\n", notStarted, why, logs)
		return 1
	}
	skip := func(why string) {
		daemonSys.restartEvent(ctx, c, "info", "daemon.reload_skipped", fmt.Sprintf("%s: %s was not reloaded", why, label))
	}
	userHome, err := daemonSys.UserHome()
	if err != nil {
		return refuse(fmt.Sprintf("cannot find your home directory, where its plist is (%v)", err))
	}
	path := launchd.AgentPath(userHome, label)
	fmt.Fprintf(c.Stdout, "launchd could not spawn %s after the restart (%s); reloading the job once, as `magnum install` does "+
		"(launchctl bootout, then bootstrap %s)\n", label, state, path)
	var chk reloadCheck
	err = launchd.Reload(ctx, run, uid, label, path, func(ctx context.Context) error {
		if chk = reloadGuard(ctx, run, c.Layout, uid, label, oldPID); !chk.reload() {
			return errReloadHeld
		}
		return nil
	})
	switch {
	case errors.Is(err, launchd.ErrNotReloaded) && chk.started:
		skip(fmt.Sprintf("launchd started %s meanwhile (pid %d)", label, chk.st.PID))
		fmt.Fprintf(c.Stdout, "launchd started %s meanwhile; the job was not reloaded\nrestarted %s via launchd (%s); logs: %s\n",
			label, label, describeLaunchdState(chk.st), c.Layout.DaemonLog())
		return 0
	case errors.Is(err, launchd.ErrNotReloaded) && chk.held > 0:
		if why := notADaemon(ctx, run, chk.held); why != "" {
			return refuse(fmt.Sprintf("pid %d holds the daemon's pidfile %s but %s", chk.held, c.Layout.Pid(), why))
		}
		skip(fmt.Sprintf("a magnum daemon runs outside launchd (pid %d)", chk.held))
		fmt.Fprintf(c.Stdout, "a magnum daemon runs outside launchd (pid %d); the job was not reloaded, and launchd still reports %s; logs: %s\n",
			chk.held, describeLaunchdState(chk.st), c.Layout.DaemonLog())
		return 0
	case errors.Is(err, launchd.ErrNotReloaded) && chk.refused != "":
		return refuse(chk.refused)
	case errors.Is(err, launchd.ErrNotReloaded):
		return refuse(err.Error()) // the plist: missing, without a label, another job's
	case err != nil:
		now := "its state unreadable"
		if st, serr := launchd.Status(ctx, run, uid, label); serr == nil {
			now = describeLaunchdState(st)
		}
		why := execx.Redact(err.Error())
		daemonSys.restartEvent(ctx, c, "error", "daemon.reload_failed", fmt.Sprintf("reloading %s failed: %s (%s)", label, why, now))
		fmt.Fprintf(c.Stderr, "magnum daemon-restart: %s, and reloading the job failed: %s (%s)\n%s\n", notStarted, why, now, installFix)
		return 1
	}
	after, err := waitLaunchdStarted(ctx, run, uid, label, oldPID)
	if err != nil {
		why := fmt.Sprintf("still none %s after reloading the job", launchdStartWait)
		if !errors.Is(err, errLaunchdNotStarted) {
			why = fmt.Sprintf("the wait after reloading the job was cut short (%v)", err)
		}
		daemonSys.restartEvent(ctx, c, "error", "daemon.reload_failed", fmt.Sprintf("reloaded %s, but %s (%s)", label, why, describeLaunchdState(after)))
		fmt.Fprintf(c.Stderr, "magnum daemon-restart: %s, and %s (%s)\n%s\n", notStarted, why, describeLaunchdState(after), installFix)
		return 1
	}
	daemonSys.restartEvent(ctx, c, "info", "daemon.reloaded", fmt.Sprintf("reloaded %s: the daemon runs (pid %d)", label, after.PID))
	fmt.Fprintf(c.Stdout, "restarted %s via launchd after reloading the job (%s); logs: %s\n", label, describeLaunchdState(after), c.Layout.DaemonLog())
	return 0
}
