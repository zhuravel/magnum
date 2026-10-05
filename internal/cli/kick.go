package cli

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"
)

const kickUsage = "kick"

// kickTargets are the names a kick took before every kick ran the same full
// tick (it polls, schedules and reconciles when reconcile_interval has
// passed). They are still accepted, so an older herdr plugin's
// `kick reconcile` keeps working, and ignored with a note.
var kickTargets = []string{"poll", "reconcile", "schedule"}

func newKickCmd(c *Context) *cobra.Command {
	return newCommand(groupAct, kickUsage, "wake the daemon for a tick now (SIGUSR1)",
		"Wake the running daemon for a tick now (SIGUSR1) instead of waiting for the next poll. The tick polls, "+
			"schedules and reconciles (when reconcile_interval has passed). Exits 1 when no daemon runs.",
		func(pos []string) int { return runKick(c, pos) })
}

func runKick(c *Context, pos []string) int {
	switch {
	case len(pos) > 1 || (len(pos) == 1 && !slices.Contains(kickTargets, pos[0])):
		return actUsage(c, "kick", "kick takes no arguments", kickUsage)
	case len(pos) == 1:
		fmt.Fprintf(c.Stderr, "magnum kick: %q is ignored and will go away: every tick polls, schedules and reconciles\n", pos[0])
	}
	pid, err := actKickDaemon(c.Layout)
	if err != nil {
		return cmdFail(c, "kick", err)
	}
	if pid == 0 {
		fmt.Fprintf(c.Stderr, "magnum kick: no daemon is running: %s\n", actDaemonFix)
		return 1
	}
	fmt.Fprintf(c.Stdout, "kicked the daemon (pid %d): a tick runs now\n", pid)
	return 0
}
