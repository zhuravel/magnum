package cli

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"
)

const kickUsage = "kick [poll|reconcile|schedule]"

// kickTargets are what a kick may name. The daemon runs one full tick for
// any of them: it polls, schedules and reconciles (when reconcile_interval
// has passed); the name is only echoed back.
var kickTargets = []string{"poll", "reconcile", "schedule"}

func newKickCmd(c *Context) *cobra.Command {
	cmd := newCommand(groupAct, kickUsage, "wake the daemon for a tick now (SIGUSR1)",
		"Wake the running daemon for a tick now (SIGUSR1) instead of waiting for the next poll. The tick polls, "+
			"schedules and reconciles (when reconcile_interval has passed); the optional target is only echoed "+
			"back. Exits 1 when no daemon runs.",
		func(pos []string) int { return runKick(c, pos) })
	cmd.ValidArgsFunction = completeFirst(func(string) []cobra.Completion { return kickTargets })
	return cmd
}

func runKick(c *Context, pos []string) int {
	what := ""
	switch {
	case len(pos) > 1:
		return actUsage(c, "kick", "at most one of poll, reconcile or schedule", kickUsage)
	case len(pos) == 1:
		if !slices.Contains(kickTargets, pos[0]) {
			return actUsage(c, "kick", fmt.Sprintf("unknown target %q (poll, reconcile or schedule)", pos[0]), kickUsage)
		}
		what = pos[0]
	}
	pid, err := actKickDaemon(c.Layout)
	if err != nil {
		return cmdFail(c, "kick", err)
	}
	if pid == 0 {
		fmt.Fprintf(c.Stderr, "magnum kick: no daemon is running: %s\n", actDaemonFix)
		return 1
	}
	msg := fmt.Sprintf("kicked the daemon (pid %d): a tick runs now", pid)
	if what == "reconcile" {
		msg += " (it reconciles when reconcile_interval has passed)"
	} else if what != "" {
		msg += " (" + what + ")"
	}
	fmt.Fprintln(c.Stdout, msg)
	return 0
}
