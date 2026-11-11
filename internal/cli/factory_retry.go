package cli

// `magnum factory retry`: your retry of a factory task that waits on you,
// through the factory daemon's requests table as the screen sends it. The
// task goes on at its stage, whose own checks run again; the daemon refuses
// a task that does not wait on you.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
)

const factoryRetryUsage = "retry <task> [--accept-qa] [--surface cli|liaison]"

func newFactoryRetryCmd(c *Context) *cobra.Command {
	var acceptQA bool
	var where whereFlags
	cmd := newCommand("", factoryRetryUsage, "let a factory task that waits on you go on at its stage",
		"Let a factory task that waits on you (NEEDS YOU) go on at its stage, as the screen's retry does: <task> is "+
			"the task's id or its Jira key, and the command waits for the daemon's answer as `start` does. The "+
			"stage's own checks run again (a file you fixed, the local review's fresh loop after it stopped for you, "+
			"what a check kept for you, the publish's checks before any step). "+
			fmt.Sprintf("A retry at the cap of fix turns (a writer whose change the factory's checks refuse after %d fix turns "+
				"in the stage; the watchdog's nudges, resumes and the prompts of your answers spend none) runs the checks again; "+
				"for checks that still fail, the writer gets %d more fix turns, then the task waits on you again (\"does not "+
				"pass after %d more fix turns since your retry at the cap\"), and each retry at the cap gives %d again. The "+
				"agent's loop guard starts over at your retry too, so it lets those turns out rather than stop them as a loop "+
				"without progress. When your retry cannot be recorded, its answer says so: the task goes on, and the writer "+
				"gets no more fix turns. ", factory.StageFixTurns, factory.RetryFixTurns, factory.RetryFixTurns,
				factory.RetryFixTurns)+
			"A prompt whose delivery is unknown is still sent to nobody (`magnum factory let-go` lets one go, once you "+
			"looked at its pane), and a task Codex never works on stays so. When the task waits on a precheck failure and "+
			"nothing changed since (the branch's head, the precheck commands, their environment), the answer says so, and "+
			"the task still goes on. A task that "+
			"does not wait on you is refused, and so is one whose questions wait for your answers (`magnum factory answer`). "+
			"--accept-qa also accepts what QA's newest run left you at the task's head. The QA defects that block which the writer "+
			"declined with its risk, when QA's fix loop stopped for you on them: the next QA run at the task's head lets exactly "+
			"those pass, the PR body names them as accepted by you, and a new head blocks them again; only you can accept a defect "+
			"QA finds, never the writer. After a QA run that did not walk again the page of a defect the writer fixed or declined, "+
			"or did not find a decline again at the head it was made at, it lets that fix go unchecked, or accepts that decline, "+
			"at the task's head alone: the PR body names it as accepted by you, and a run at a new head walks the page again. After "+
			"a walk that reached its pages, found nothing that blocks there and could not reach a state, it accepts the states as "+
			"ones QA could not check: the task goes on at once, no walk more, with them flagged on the card and in the PR body as "+
			"accepted by you. A plain retry walks QA again: it hands a decline you did not accept to the writer once more, within "+
			"its fix turns, and a second walk that misses only states of the acceptance checks the first missed goes on with them "+
			"flagged, while one that misses a state of no acceptance check waits on you again. --surface liaison says the factory's "+
			"liaison runs the retry for you, on your word in its chat (the default, cli, is you by hand); the factory "+
			"records it so, as for answer and decide.",
		func(pos []string) int { return runFactoryRetryCmd(c, acceptQA, where, pos) })
	cmd.Flags().BoolVar(&acceptQA, "accept-qa", false, "accept what QA left you at the task's head: the writer's declines, its fixes or declines QA did not walk again, or the states QA could not reach")
	cmd.Flags().StringVar(&where.surface, "surface", cliSurface, "where you give this word: "+cliSurface+" (by hand) or "+runtime.SurfaceLiaison+
		" (the factory's liaison, which runs it for you)")
	return cmd
}

func runFactoryRetryCmd(c *Context, acceptQA bool, where whereFlags, pos []string) int {
	if len(pos) != 1 {
		return inspUsage(c, "factory retry", "which task? its id or its Jira key", factoryRetryUsage)
	}
	surface, _, err := where.resolve()
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory retry: %v\n", err)
		return 2
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
	return sendFactoryRequest(ctx, c, reg, "factory retry", factory.ReqRetry, factory.RetryPayload{TaskID: t.ID, Surface: surface, AcceptQA: acceptQA},
		"it lets the task go on")
}
