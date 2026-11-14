package cli

// `magnum factory finish-review`: end a factory task's local review now,
// through the factory daemon's requests table as the screen's S sends it.
// It asks y/N on a terminal first, naming what happens (the plan the
// daemon answers with too); --dry-run says it and sends nothing.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/fsx"
)

const factoryFinishUsage = "finish-review <task> [--dry-run]"

func newFactoryFinishCmd(c *Context) *cobra.Command {
	var dry bool
	cmd := newCommand("", factoryFinishUsage, "end a factory task's local review now: no round starts after the one in flight",
		"End a factory task's local review now (<task> is its id or its Jira key), as the factory screen's S does. The round "+
			"in flight finishes, never cut, and no round starts after it; between rounds, the review ends at once on the writer's "+
			"fix of the last round's findings, which no round reviews (the ready card lists them under FIXED AFTER), and a head no "+
			"round reviewed yet gets its one round first, but for one that only merges the base cleanly into the head the review "+
			"passed, which gets none: the stage passes at once, and the review's end before the merge holds there. Otherwise "+
			"the stage ends as at [factory.local_review] max_rounds: a safe stop "+
			"(no P0 or P1, a verdict that does not block, no finding the writer declined in its loop, no product question open) goes to the ready card with its "+
			"open findings under KNOWN, the card saying the review was ended by you; any other waits on you, and your retry lets the "+
			"loop go on. A second opinion that has not begun after a clean round does not run: the review ends clean on that round, "+
			"and the card says it ended clean. The command says what happens to the task now and asks y/N on a terminal (only y sends the request, and "+
			"it waits for the daemon's answer as `retry` does; off a terminal it asks nothing and sends nothing); --dry-run says it "+
			"and sends nothing. A task that is not in its local review at work is refused with the reason: one at another stage, "+
			"one that waits on you (its retry or your answers come first), one done or stopped.",
		func(pos []string) int { return runFactoryFinishCmd(c, dry, pos) })
	cmd.Flags().BoolVar(&dry, "dry-run", false, "say what happens to the task; send nothing")
	return cmd
}

func runFactoryFinishCmd(c *Context, dry bool, pos []string) int {
	const name = "factory finish-review"
	if len(pos) != 1 {
		return inspUsage(c, name, "which task? its id or its Jira key", factoryFinishUsage)
	}
	if !fsx.Exists(c.Layout.FactoryDB()) {
		fmt.Fprintln(c.Stderr, "magnum factory finish-review: the factory has no task yet (`magnum factory start` makes one)")
		return 1
	}
	// The stage's type comes from the config's kinds, which a command run
	// from the terminal has not loaded yet: without it every task reads as
	// outside its local review.
	if err := c.LoadConfig(); err != nil {
		fmt.Fprintf(c.Stderr, "magnum %s: %v\n", name, err)
		return 1
	}
	reg, err := openFactoryRegistry(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum %s: %v\n", name, err)
		return 1
	}
	defer reg.Close()
	ctx, stop := daemonSys.signals()
	defer stop()
	t, err := factoryTaskOf(ctx, reg, pos[0])
	if err == nil {
		err = factory.FinishReviewRefusal(c.Config, t)
	}
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum %s: %v\n", name, err)
		return 1
	}
	plan, err := factory.FinishReviewPlan(ctx, reg, c.Layout, t)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum %s: %v\n", name, err)
		return 1
	}
	key := factory.TaskKey(t)
	if dry {
		fmt.Fprintf(c.Stdout, "dry-run: %s: %s\n", key, plan)
		return 0
	}
	if !inspCanAsk(c) {
		fmt.Fprintf(c.Stderr, "magnum %s: it asks y/N on a terminal: run it there (--dry-run says what it does)\n", name)
		return 1
	}
	if !inspConfirm(ctx, c, factory.FinishReviewQuestion(key, plan)) {
		fmt.Fprintln(c.Stderr, "the local review goes on")
		if ctx.Err() != nil {
			return 130
		}
		return 1
	}
	return sendFactoryRequest(ctx, c, reg, name, factory.ReqFinishReview, factory.FinishReviewPayload{TaskID: t.ID, Surface: cliSurface},
		"it ends the task's local review")
}
