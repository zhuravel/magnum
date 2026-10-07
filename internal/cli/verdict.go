package cli

// `magnum approve` and `magnum request-changes`: the reviewer's own verdict
// on the head magnum reviewed, posted by the daemon as the PR's identity
// (engine.requestVerdict). For repositories whose policy lets magnum only
// comment, and for overriding its event. `approve --as` the watch's
// auto_approve_as posts the operator's own approval, which GitHub counts.

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

type verdictOpts struct {
	message, as string
	force, json bool
}

const (
	verdictUsage = "<ref> [-m TEXT] [--force] [--json]"
	approveUsage = "<ref> [-m TEXT] [--as IDENTITY] [--force] [--json]"
)

func newApproveCmd(c *Context) *cobra.Command {
	return newVerdictCmd(c, "approve", engine.ReqApprove, "approve a PR magnum reviewed, as its posting identity or as you",
		"Post an APPROVE review on the head magnum reviewed, as the PR's posting identity, with a body that "+
			"names magnum's review (and -m's words first). For a repository where magnum only comments, or when "+
			"you decide differently from its review. The PR's head must still be the reviewed one (--force posts "+
			"on the reviewed head anyway). Like magnum's own approvals it is withdrawn when real new commits "+
			"land, unless the repository keeps approvals; magnum's later rounds never dismiss it otherwise.\n\n"+
			"GitHub never counts an App's approval toward a branch's required approvals: --as with the watch's "+
			"auto_approve_as (your own gh account) posts your approval instead, the board's A on a row that needs "+
			"you (\"✔ needs you\", \"✔ lift your ✗\"). It is refused where magnum would not approve as you on its "+
			"own (a draft, a muted PR, a head magnum has not reviewed, a review that found something to fix), "+
			"but not for your own changes request or stop, and it is followed like an automatic approval: a later "+
			"review that finds something to fix withdraws it, and `magnum unapprove` (the board's D) does too. "+
			"--as with the PR's posting identity is the approval without it.")
}

func newRequestChangesCmd(c *Context) *cobra.Command {
	return newVerdictCmd(c, "request-changes", engine.ReqRequestChanges, "request changes on a PR magnum reviewed, as its posting identity",
		"Post a REQUEST_CHANGES review on the head magnum reviewed, as the PR's posting identity, with a body "+
			"that names magnum's review and its findings (and -m's words first). It stands until you approve "+
			"or dismiss it: magnum's later rounds never dismiss it as their own stale review. The PR's head must "+
			"still be the reviewed one (--force posts on the reviewed head anyway).")
}

func newVerdictCmd(c *Context, name, req, summary, long string) *cobra.Command {
	var o verdictOpts
	usage := verdictUsage
	if req == engine.ReqApprove {
		usage = approveUsage
	}
	cmd := newCommand(groupAct, name+" "+usage, summary, long, func(pos []string) int { return runVerdict(c, name, req, usage, o, pos) })
	fs := cmd.Flags()
	fs.StringVarP(&o.message, "message", "m", "", "your words, put before magnum's line in the review body")
	if req == engine.ReqApprove {
		fs.StringVar(&o.as, "as", "", "the identity to approve as: the watch's auto_approve_as (your own account, whose approval GitHub counts) or the PR's posting identity")
		_ = cmd.RegisterFlagCompletionFunc("as", completeFlag(c.completeIdentities))
	}
	fs.BoolVar(&o.force, "force", false, "post on the reviewed head although the PR moved on since")
	fs.BoolVar(&o.json, "json", false, "print the request outcome as JSON")
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runVerdict(c *Context, name, req, usage string, o verdictOpts, pos []string) int {
	if len(pos) != 1 {
		return actUsage(c, name, "which PR?", usage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, name, err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return verdictMain(ctx, c, d, name, req, pos[0], o)
}

// verdictMain resolves the PR and hands the verdict to the daemon.
func verdictMain(ctx context.Context, c *Context, d *actDeps, name, req, ref string, o verdictOpts) int {
	t, err := targetResolve(ctx, d, ref, "", "")
	if err != nil {
		return cmdFail(c, name, verbFix(name, err))
	}
	if !t.hasPR() {
		return cmdFail(c, name, fmt.Errorf("%s names no PR", ref))
	}
	payload := engine.VerdictPayload{PRTarget: t.prTarget(), Message: o.message, Force: o.force, As: o.as}
	return submitAndReport(ctx, c, d, name, d.actLabel(t.full(), t.PR.Number), req, payload, o.json)
}

// submitAndReport hands a verdict to the daemon, waits briefly for its
// answer and prints it. A verdict posts to GitHub: with no daemon to post it
// now it is refused, never left queued to post whenever a daemon starts.
func submitAndReport(ctx context.Context, c *Context, d *actDeps, name, label, req string, payload any, asJSON bool) int {
	q := d.quick()
	q.NeedDaemon = true
	out, err := d.reqs().send(ctx, req, payload, q)
	if err != nil {
		if errors.Is(err, errNoDaemon) {
			err = fmt.Errorf("%s %s: nothing was queued: %w", name, label, err)
		}
		return cmdFail(c, name, err)
	}
	if asJSON {
		_ = writeJSON(c.Stdout, out.view())
		return out.jsonCode()
	}
	return out.print(c.Stdout, c.Stderr)
}

// prsApproveAs names, on each board row that needs the operator (NeedsMe,
// once prsAutoApprovals cleared the rows approved as them), whom A approves
// it as: the watch's auto_approve_as, with why the daemon would refuse it
// now (engine.OperatorApprovalRefusal on the registry's PR and sums,
// magnum's latest posted rounds). A row whose watch names none keeps nil,
// and A refuses it.
func prsApproveAs(ctx context.Context, st *store.Store, cfg *config.Config, rows []store.BoardRow, sums map[int64]store.ReviewSummary,
	out []tui.PRBoardRow) error {
	for i, r := range rows {
		if out[i].NeedsMe == "" {
			continue
		}
		id := engine.ApproveAsFor(cfg, r.Owner+"/"+r.Name)
		if id == nil {
			continue
		}
		pr, err := st.PRByID(ctx, r.PRID)
		if err != nil {
			return err
		}
		var sum *store.ReviewSummary
		if s, ok := sums[r.PRID]; ok {
			sum = &s
		}
		out[i].ApproveAs = &tui.ApproveAs{Identity: id.Name, Login: cmp.Or(id.Login, id.Name),
			Refusal: engine.OperatorApprovalRefusal(pr, sum, id.Login)}
	}
	return nil
}
