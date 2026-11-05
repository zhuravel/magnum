package cli

// `magnum factory let-go`: your let-go of a prompt whose delivery is
// unknown, through the factory daemon's requests table as `retry` sends
// its request. You looked at the role's pane; the prompt is never sent
// again, and the task goes on without it.

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/fsx"
)

const factoryLetGoUsage = "let-go <task> --role <role>"

func newFactoryLetGoCmd(c *Context) *cobra.Command {
	var role string
	cmd := newCommand("", factoryLetGoUsage, "let go of a factory prompt whose delivery is unknown",
		"Let go of the prompts to a role of a factory task whose delivery nobody can tell (the task waits on you: "+
			"\"a prompt to <role> may or may not have arrived: look at its pane\"): <task> is the task's id or its "+
			"Jira key, --role the role the wait names, and the command waits for the daemon's answer as `retry` does. "+
			"Look at the role's pane first: a prompt you let go is never sent again, whether or not it arrived. The "+
			"task goes on without it: a role whose turn was that prompt is not watched, so the task waits on you "+
			"until the role's file is written (tell its agent in its pane), then `magnum factory retry` lets it pass. "+
			"The daemon refuses a role with no prompt of unknown delivery.",
		func(pos []string) int { return runFactoryLetGoCmd(c, role, pos) })
	cmd.Flags().StringVar(&role, "role", "", "the role whose prompt you let go (the wait names it)")
	return cmd
}

func runFactoryLetGoCmd(c *Context, role string, pos []string) int {
	role = strings.TrimSpace(role)
	switch {
	case len(pos) != 1:
		return inspUsage(c, "factory let-go", "which task? its id or its Jira key", factoryLetGoUsage)
	case role == "":
		return inspUsage(c, "factory let-go", "which role? --role names the role the task's wait names", factoryLetGoUsage)
	}
	if !fsx.Exists(c.Layout.FactoryDB()) {
		fmt.Fprintln(c.Stderr, "magnum factory let-go: the factory has no task yet (`magnum factory start` makes one)")
		return 1
	}
	reg, err := openFactoryRegistry(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory let-go: %v\n", err)
		return 1
	}
	defer reg.Close()
	ctx, stop := daemonSys.signals()
	defer stop()
	t, err := factoryTaskOf(ctx, reg, pos[0])
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory let-go: %v\n", err)
		return 1
	}
	return sendFactoryRequest(ctx, c, reg, "factory let-go", factory.ReqLetGo, factory.LetGoPayload{TaskID: t.ID, Role: role, Surface: "cli"},
		"it lets the prompt go")
}
