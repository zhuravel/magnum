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

const factoryRetryUsage = "retry <task> [--accept-qa]"

func newFactoryRetryCmd(c *Context) *cobra.Command {
	var acceptQA bool
	cmd := newCommand("", factoryRetryUsage, "let a factory task that waits on you go on at its stage",
		"Let a factory task that waits on you (NEEDS YOU) go on at its stage, as the screen's retry does: <task> is "+
			"the task's id or its Jira key, and the command waits for the daemon's answer as `start` does. The "+
			"stage's own checks run again (a file you fixed, the local review's fresh loop after it stopped for you, "+
			"what a check kept for you, the publish's checks before any step); a prompt whose delivery is unknown is "+
			"still sent to nobody (`magnum factory let-go` lets one go, once you looked at its pane), and a task Codex never "+
			"works on stays so. When the task waits on a precheck failure and nothing changed since (the branch's head, "+
			"the precheck commands, their environment), the answer says so, and the task still goes on. A task that "+
			"does not wait on you is refused, and so is one whose questions wait for your answers (`magnum factory answer`). "+
			"--accept-qa also accepts the QA defects that block which the writer declined with its risk, when QA's fix loop "+
			"stopped for you on them: the next QA run at the task's head lets exactly those pass, the PR body names them as "+
			"accepted by you, and a new head blocks them again; only you can accept a defect QA finds, never the writer. A "+
			"plain retry walks QA again and hands a decline you did not accept to the writer once more, within its fix turns.",
		func(pos []string) int { return runFactoryRetryCmd(c, acceptQA, pos) })
	cmd.Flags().BoolVar(&acceptQA, "accept-qa", false, "accept the QA defects the writer declined with its risk, at the task's head")
	return cmd
}

func runFactoryRetryCmd(c *Context, acceptQA bool, pos []string) int {
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
	return sendFactoryRequest(ctx, c, reg, "factory retry", factory.ReqRetry, factory.RetryPayload{TaskID: t.ID, Surface: "cli", AcceptQA: acceptQA},
		"it lets the task go on")
}
