package cli

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
)

const (
	// daemonStopTimeout bounds the wait for a SIGTERMed daemon to exit
	// (rounds and heavy jobs are cancelled on stop).
	daemonStopTimeout = 30 * time.Second
	daemonPollEvery   = 500 * time.Millisecond
	// launchdStartWait bounds the wait for launchd to report the new pid.
	launchdStartWait = 5 * time.Second
)

const daemonRestartUsage = "daemon-restart [--now | --drain [--timeout D]]"

// restartFlags are the parsed rounds-in-flight flags of daemon-restart and
// install.
type restartFlags struct {
	now, drain bool
	timeout    time.Duration
}

// addRestartFlags declares --now, --drain and --timeout on cmd.
func addRestartFlags(cmd *cobra.Command, f *restartFlags) {
	fs := cmd.Flags()
	fs.BoolVar(&f.now, "now", false, "restart even while review rounds are in flight (they start over)")
	fs.BoolVar(&f.drain, "drain", false, "start no new rounds, wait for the ones in flight, then restart")
	fs.DurationVar(&f.timeout, "timeout", defaultDrainTimeout, "with --drain: give up (and restart nothing) after this long")
}

// check refuses --now with --drain and a non-positive --timeout.
func (f restartFlags) check(c *Context, cmd, usage string) bool {
	switch {
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
			"agent a running daemon is stopped (SIGTERM) and `magnum install` is suggested. bin/magnum checks the "+
			"configuration and renders every prompt first (`bin/magnum config`); the restart is refused when it "+
			"fails. While review rounds are in flight the restart is refused, because it abandons them; --drain "+
			"stops new rounds, waits for those in flight (a line every 15s, at most --timeout, default 2h) and then "+
			"restarts; --now restarts at once.",
		func(pos []string) int { return runDaemonRestartCmd(c, pos, f) })
	addRestartFlags(cmd, &f)
	return cmd
}

func runDaemonRestartCmd(c *Context, pos []string, f restartFlags) int {
	if !daemonGroupNoArgs(c, "daemon-restart", daemonRestartUsage, pos) || !f.check(c, "daemon-restart", daemonRestartUsage) {
		return 2
	}
	ctx, stop := signalContext()
	defer stop()
	out := c.Stdout
	run := daemonSys.runner(false, out)
	if !verifyBuild(ctx, c, run, "daemon-restart") {
		return 1
	}
	endDrain, ok := c.guardRounds(ctx, run, "daemon-restart", f.now, f.drain, f.timeout)
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

	if st.Loaded() {
		// A daemon started by hand holds the lock: launchd's copy would exit
		// at once as "already running", so stop it first.
		if pid > 0 && pid != st.PID {
			if err := stopMagnumDaemon(ctx, run, c.Layout, pid); err != nil {
				fmt.Fprintf(c.Stderr, "magnum daemon-restart: %v\n", err)
				return 1
			}
			fmt.Fprintf(out, "stopped the daemon running outside launchd (pid %d)\n", pid)
		}
		if err := launchd.Kickstart(ctx, run, uid, label); err != nil {
			fmt.Fprintf(c.Stderr, "magnum daemon-restart: %v\nfix: reload the job with `magnum install`\n", err)
			return 1
		}
		after, ok := waitLaunchdStarted(ctx, run, uid, label, st.PID)
		if !ok {
			fmt.Fprintf(c.Stderr, "magnum daemon-restart: launchd restarted %s, but no new daemon was running after %s (%s)\n"+
				"fix: read %s and %s, then `magnum daemon-restart` again (or `magnum install` to reload the job)\n",
				label, launchdStartWait, describeLaunchdState(after), launchd.LogPath(c.Layout.Logs()), c.Layout.DaemonLog())
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
	if err := stopMagnumDaemon(ctx, run, c.Layout, pid); err != nil {
		fmt.Fprintf(c.Stderr, "magnum daemon-restart: %v\n", err)
		return 1
	}
	fmt.Fprintf(c.Stderr, "magnum daemon-restart: stopped the daemon (pid %d), but launchd does not manage magnum, so nothing starts it again\n"+
		"fix: run `magnum install` to let launchd keep it running, or start it again with `magnum daemon`\n", pid)
	return 1
}

// stopMagnumDaemon sends SIGTERM to pid after checking it really is a magnum
// daemon (the pidfile may name a recycled pid), then waits for it to exit.
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
		daemonSys.Sleep(daemonPollEvery)
	}
	return fmt.Errorf("daemon pid %d did not exit within %s of SIGTERM\nfix: check %s, then `kill %d` again", pid, daemonStopTimeout, layout.DaemonLog(), pid)
}

// waitLaunchdStarted polls launchd until the job runs with a pid other than
// oldPID (ok), or launchdStartWait passes (not ok); it returns the last state
// seen either way.
func waitLaunchdStarted(ctx context.Context, run execx.Runner, uid int, label string, oldPID int) (launchd.Info, bool) {
	var st launchd.Info
	for waited := time.Duration(0); ; waited += daemonPollEvery {
		st, _ = launchd.Status(ctx, run, uid, label)
		if st.State == launchd.Running && st.PID > 0 && st.PID != oldPID {
			return st, true
		}
		if waited >= launchdStartWait {
			return st, false
		}
		daemonSys.Sleep(daemonPollEvery)
	}
}
