package cli

// `magnum factory retry`: your retry of a factory task that waits on you,
// through the factory daemon's requests table as the screen sends it. The
// task goes on at its stage, whose own checks run again; the daemon refuses
// a task that does not wait on you.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/fsx"
)

const factoryRetryUsage = "retry <task>"

func newFactoryRetryCmd(c *Context) *cobra.Command {
	return newCommand("", factoryRetryUsage, "let a factory task that waits on you go on at its stage",
		"Let a factory task that waits on you (NEEDS YOU) go on at its stage, as the screen's retry does: <task> is "+
			"the task's id or its Jira key, and the command waits for the daemon's answer as `start` does. The "+
			"stage's own checks run again (a file you fixed, the local review's fresh loop after it stopped for you, "+
			"what a check kept for you, the publish's checks before any step); a prompt whose delivery is unknown is "+
			"still sent to nobody (let it go on the screen), and a task Codex never works on stays so. A task that "+
			"does not wait on you is refused.",
		func(pos []string) int { return runFactoryRetryCmd(c, pos) })
}

func runFactoryRetryCmd(c *Context, pos []string) int {
	if len(pos) != 1 {
		return inspUsage(c, "factory retry", "which task? its id or its Jira key", factoryRetryUsage)
	}
	if !fsx.Exists(c.Layout.FactoryDB()) {
		fmt.Fprintln(c.Stderr, "magnum factory retry: the factory has no task yet (`magnum factory start` makes one)")
		return 1
	}
	reg, err := openFactoryRegistry(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory retry: %v\n", err)
		return 1
	}
	defer reg.Close()
	ctx, stop := daemonSys.signals()
	defer stop()
	t, err := factoryTaskOf(ctx, reg, pos[0])
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory retry: %v\n", err)
		return 1
	}
	return sendFactoryRequest(ctx, c, reg, "factory retry", factory.ReqRetry, factory.RetryPayload{TaskID: t.ID, Surface: "cli"},
		"it lets the task go on")
}
