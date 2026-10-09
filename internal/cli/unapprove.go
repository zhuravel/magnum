package cli

// `magnum unapprove`: withdraw the approval magnum posted as you on a PR
// ([[watch]] auto_approve) and stop it approving that PR as you; --resume
// lets it again. The daemon dismisses the review as your identity
// (engine.requestUnapprove).

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/textx"
)

type unapproveOpts struct {
	resume, yes, json bool
}

const unapproveUsage = "unapprove <ref> [--resume] [--yes] [--json]"

func newUnapproveCmd(c *Context) *cobra.Command {
	var o unapproveOpts
	cmd := newCommand(groupAct, unapproveUsage, "withdraw the approval magnum posted as you on a PR, and stop it approving that PR",
		"Dismiss the approval magnum posted as you (a [[watch]]'s auto_approve_as) on the PR, as you, and stop magnum approving "+
			"that PR as you, as when you dismiss one of its approvals or review the PR by hand on GitHub. Without an approval "+
			"standing it only stops it. A terminal asks y/N first (--yes does not; there --json needs --yes). --resume lets "+
			"magnum approve the PR as you again after a clean review; your reviews from before then no longer stop it.",
		func(pos []string) int { return runUnapprove(c, o, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&o.resume, "resume", false, "let magnum approve the PR as you again (dismisses nothing)")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	fs.BoolVar(&o.json, "json", false, "print the request outcome as JSON")
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runUnapprove(c *Context, o unapproveOpts, pos []string) int {
	switch {
	case len(pos) == 0:
		return actUsage(c, "unapprove", "which PR?", unapproveUsage)
	case len(pos) > 1:
		return actUsage(c, "unapprove", "one PR at a time", unapproveUsage)
	}
	if code, refused := refuseInAgentPane(c, "unapprove"); refused {
		return code
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "unapprove", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return unapproveMain(ctx, c, d, pos[0], o)
}

// unapproveMain resolves the PR, asks on a terminal (naming the approval
// that stands) and hands the request to the daemon.
func unapproveMain(ctx context.Context, c *Context, d *actDeps, ref string, o unapproveOpts) int {
	t, err := targetResolve(ctx, d, ref, "", "")
	if err != nil {
		return cmdFail(c, "unapprove", verbFix("unapprove", err))
	}
	if !t.hasPR() {
		return cmdFail(c, "unapprove", fmt.Errorf("%s names no PR", ref))
	}
	label := d.actLabel(t.full(), t.PR.Number)
	if d.StdinTTY && d.StdoutTTY && !o.yes {
		if o.json { // the question would land in the JSON; skipping it would withdraw unasked
			return actUsage(c, "unapprove", "--json needs --yes to apply (a terminal asks y/N first); nothing withdrawn", unapproveUsage)
		}
		q := fmt.Sprintf("Let magnum approve %s as you again after a clean review?", label)
		if !o.resume {
			q = fmt.Sprintf("No automatic approval of %s stands. Stop magnum approving it as you?", label)
			if a, ok, err := d.Store.LiveAutoApproval(ctx, t.PR.ID); err == nil && ok {
				q = fmt.Sprintf("Withdraw your automatic approval of %s (review %d on %s) and stop magnum approving it as you?",
					label, a.ReviewID, textx.ShortSHA(a.HeadSHA))
			}
		}
		if !d.confirm(ctx, c.Stdout, q) {
			fmt.Fprintln(c.Stderr, "nothing withdrawn")
			if ctx.Err() != nil {
				return 130
			}
			return 1
		}
	}
	payload := engine.UnapprovePayload{PRTarget: t.prTarget(), Resume: o.resume}
	return submitAndReport(ctx, c, d, "unapprove", label, engine.ReqUnapprove, payload, o.json)
}
