package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/launchd"
)

func newDaemonStopCmd(c *Context) *cobra.Command {
	var now bool
	cmd := newCommand(groupDaemon, "daemon-stop [--now]", "stop the daemon and unload its launchd job (keeps the plist; `magnum install` loads it again)",
		"Stop the daemon for development: the launchd job is unloaded with `launchctl bootout` so launchd does not "+
			"restart it, and a daemon started by hand gets SIGTERM. The plist stays in place; `magnum install` (or "+
			"`launchctl bootstrap`) brings launchd back. `mise run dev` calls this before starting air. While review "+
			"rounds are in flight the stop is refused, because it abandons them; --now stops anyway.",
		func(pos []string) int { return runDaemonStopCmd(c, pos, now) })
	cmd.Flags().BoolVar(&now, "now", false, "stop even while review rounds are in flight (they start over)")
	return cmd
}

func runDaemonStopCmd(c *Context, pos []string, now bool) int {
	if !daemonGroupNoArgs(c, "daemon-stop", "daemon-stop [--now]", pos) {
		return 2
	}
	if !c.refuseWhileRoundsRun("daemon-stop", now) {
		return 1
	}
	ctx := context.Background()
	run := daemonSys.runner(false, c.Stdout)
	uid := daemonSys.UID()
	label := launchd.DefaultLabel

	// A Status error leaves the job's state unknown: bootout anyway, so
	// launchctl's own error (or its success) decides.
	st, statusErr := launchd.Status(ctx, run, uid, label)
	if statusErr != nil {
		fmt.Fprintf(c.Stderr, "magnum daemon-stop: warning: %v\n", statusErr)
	}
	stopped := false
	if statusErr != nil || st.Loaded() {
		if err := launchd.Bootout(ctx, run, uid, label); err != nil {
			fmt.Fprintf(c.Stderr, "magnum daemon-stop: %v\n", err)
			return 1
		}
		fmt.Fprintf(c.Stdout, "unloaded %s from launchd (plist kept; `magnum install` loads it again)\n", label)
		stopped = true
	}
	pid, err := daemonSys.DaemonPID(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum daemon-stop: warning: %v\n", err)
		pid = 0
	}
	if pid > 0 && pid != st.PID {
		if err := stopMagnumDaemon(ctx, run, c.Layout, pid); err != nil {
			fmt.Fprintf(c.Stderr, "magnum daemon-stop: %v\n", err)
			return 1
		}
		fmt.Fprintf(c.Stdout, "stopped the daemon running outside launchd (pid %d)\n", pid)
		stopped = true
	}
	if !stopped {
		fmt.Fprintln(c.Stdout, "the daemon is not running and launchd does not manage it")
	}
	return 0
}
