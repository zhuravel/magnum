package cli

// `magnum merge-parent`: ask the daemon to merge the PR's parent, its base
// branch as GitHub names it, into the operator's own PR, as the fixer App's
// bot (engine/fix_parent.go, the fix job's merge mode). A clean merge is
// the daemon's (merge-tree, commit-tree, a push with a lease), a conflict
// the fix role's, and a conflict that needs a product decision is listed
// for the operator. It shares `magnum fix`'s request, question and
// follower (fixMain).

import (
	"github.com/spf13/cobra"
)

const mergeParentUsage = "merge-parent <url|owner/repo#N|repo#N|N> [--wait] [--timeout <duration>] [--dry-run] [--json] [--yes]"

func newMergeParentCmd(c *Context) *cobra.Command {
	o := fixOpts{merge: true}
	cmd := newCommand(groupAct, mergeParentUsage, "merge your PR's base branch into it now, as the fixer App's bot (--wait follows it)",
		"Merge the PR's parent into your own PR now: its base branch as GitHub names it when the job starts (a stacked PR's "+
			"parent is the branch below it, never master by default), with or without a magnum review. It needs a watch "+
			"whose fix_identity is a dedicated fixer App: every commit and push is its bot's. A merge with no conflict is the "+
			"daemon's own: `git merge-tree`, then `git commit-tree` with the PR's head and the base's tip as parents, by the "+
			"bot, then a push with a lease on the head. A conflict goes to the fix role (claude-fix by default), which "+
			"resolves it keeping both sides' intent, runs the specs of the resolved files and pushes the merge alone; a "+
			"conflict that needs a product decision is listed for you (Needs you). Never a rebase, never a force-push. A "+
			"merge that brings the base's own .github/workflows changes is pushed with a token of the daemon's own that may "+
			"write workflows, only while every workflow file of the merge is as the base has it; while the App lacks the "+
			"Workflows permission it is refused (\"needs Workflows on the App\") and left to you. A head that has the base's "+
			"tip already has nothing to merge. After the push magnum starts the repository's ci_dispatch workflow on it "+
			"two minutes later.\n\n"+
			"A terminal asks y/N first, naming who merges which tip of which base into which branch and as which login, "+
			"from the daemon's dry run, which fetches the base first. y sends the merge bound to that plan: the daemon "+
			"refuses it, saying what moved and that nothing was merged, when the PR's head, its base, the base's tip or the "+
			"merge's conflicts changed since, or when the question is 10 minutes old. --yes asks nothing and binds nothing: "+
			"the daemon merges the base's tip as its fetch then brings it. Without a terminal (an agent's shell, a pipe, a "+
			"script) a merge needs --yes, and on a terminal --json needs it too; a review agent's pane is refused. "+
			"The daemon refuses, with the reason as its answer, a PR that is not yours or lives in a fork, a fix role that "+
			"is no fixer App, a base it does not know, a round or fix loop in flight, a pinned PR or slot, a snooze, a person "+
			"typing in its panes and a paused agent kind. --wait follows the merge to its end; --timeout (default 2h, 0 = no "+
			"limit) stops following it after that long; `magnum abort` stops it. --dry-run only asks the daemon what would happen.",
		func(pos []string) int { return runMergeParent(c, o, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&o.wait, "wait", false, "follow the merge until it ends: merged, needs you, aborted or ended")
	fs.DurationVar(&o.timeout, "timeout", fixDefaultTimeout, "with --wait: stop following after this `duration` (0 = no limit); the merge goes on")
	fs.BoolVar(&o.dryRun, "dry-run", false, "ask the daemon what would happen; nothing is started or changed")
	fs.BoolVar(&o.json, "json", false, "print the result as JSON (progress goes to stderr)")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation: the merge is bound to no plan, and merges the tip the daemon's fetch brings")
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runMergeParent(c *Context, o fixOpts, pos []string) int {
	switch {
	case len(pos) == 0:
		return actUsage(c, o.verb(), "which PR?", mergeParentUsage)
	case len(pos) > 1:
		return actUsage(c, o.verb(), "one PR at a time", mergeParentUsage)
	}
	if code, refused := refuseInAgentPane(c, o.verb()); refused {
		return code
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, o.verb(), err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return fixMain(ctx, c, d, pos[0], o)
}
