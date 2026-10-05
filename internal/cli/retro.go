package cli

// `magnum retro`: ask the daemon for a retro now (engine.ReqRetro). The
// retro itself runs in the daemon, in the background; this command queues
// the request, prints the daemon's one-line answer and leaves the results to
// `magnum misses`.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

const retroUsage = "retro [<ref>...] [--again] [--lookback <duration>] [--json]"

type retroOpts struct {
	again, json bool
	lookback    string // --lookback: config.ParseDuration syntax ("" = [learn] lookback)
}

func newRetroCmd(c *Context) *cobra.Command {
	var o retroOpts
	cmd := newCommand(groupAct, retroUsage, "run a retro now: what other reviewers caught on closed PRs that magnum missed",
		"Run a retro now. After a PR magnum reviewed closes, a retro collects what the other reviewers commented on "+
			"it, drops what magnum's own review already posted and has an agent classify the rest; the real misses "+
			"go to `magnum misses`. It looks at the PRs closed or merged within [learn] lookback (--lookback "+
			"replaces it, such as 14d or 36h) on which magnum posted a review and no retro has looked yet; --again "+
			"also takes those a retro did already. With PR references it looks at exactly those PRs, whenever they "+
			"closed and whether or not a retro did already (--again is implied for them); they must be merged or "+
			"closed and in the registry.\n\n"+
			"A retro runs whether or not [learn] enabled schedules the daily one, and even under `magnum pause`, but "+
			"not while the daemon drains for a restart or an infrastructure failure holds it. The daemon answers "+
			"at once and runs the retro in the background, one at a time. Exits 1 when no daemon runs (the request "+
			"stays queued and starts the retro when one does) or the daemon refuses it.",
		func(pos []string) int { return runRetro(c, o, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&o.again, "again", false, "also look at PRs a retro already looked at")
	fs.StringVar(&o.lookback, "lookback", "", "look at PRs closed within this `duration` (14d, 36h) instead of [learn] lookback")
	fs.BoolVar(&o.json, "json", false, "print the result as JSON")
	cmd.ValidArgsFunction = completeFlag(c.completeClosedPRs) // every argument is a PR
	return cmd
}

func runRetro(c *Context, o retroOpts, pos []string) int {
	o.lookback = strings.TrimSpace(o.lookback)
	if o.lookback != "" {
		if d, err := config.ParseDuration(o.lookback); err != nil || d <= 0 {
			return actUsage(c, "retro", fmt.Sprintf("--lookback %q: want a positive duration such as 14d or 36h", o.lookback), retroUsage)
		}
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "retro", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return retroMain(ctx, c, d, o, pos)
}

// retroJSON is what --json prints.
type retroJSON struct {
	Payload engine.RetroPayload `json:"payload"`
	Request *actRequestJSON     `json:"request,omitempty"`
	Error   string              `json:"error,omitempty"`
}

func retroMain(ctx context.Context, c *Context, d *actDeps, o retroOpts, refs []string) int {
	ids, err := retroResolve(ctx, d, refs)
	if err != nil {
		return cmdFail(c, "retro", err)
	}
	out := retroJSON{Payload: engine.RetroPayload{PRs: ids, Again: o.again, Lookback: o.lookback}}
	res, err := d.reqs().send(ctx, engine.ReqRetro, out.Payload, d.quick())
	if err != nil {
		return retroFail(c, o, out, err)
	}
	rv := res.view()
	out.Request = &rv
	switch {
	case res.PID == 0 && res.Pending():
		res.printNotes(c.Stderr)
		return retroFail(c, o, out, fmt.Errorf("no daemon is running: request %d stays queued and starts the retro once one runs: %s", res.ID(), actDaemonFix))
	case res.Failed():
		res.printNotes(c.Stderr)
		return retroFail(c, o, out, errors.New(res.Result()))
	case o.json:
		_ = writeJSON(c.Stdout, out)
		return 0
	}
	res.print(c.Stdout, c.Stderr)
	if res.Req.State == store.RequestDone {
		fmt.Fprintln(c.Stdout, "follow it with `magnum logs`; `magnum misses` lists the results")
	}
	return 0
}

func retroFail(c *Context, o retroOpts, out retroJSON, err error) int {
	if o.json {
		out.Error = err.Error()
		_ = writeJSON(c.Stdout, out)
		return 1
	}
	return cmdFail(c, "retro", err)
}

// retroResolve maps PR references to registry ids, each once, in the order
// typed; nothing is added from GitHub, and a reference the registry does not
// know, or that names a PR still open (a retro looks at merged or closed
// PRs only), is an error naming it.
func retroResolve(ctx context.Context, d *actDeps, refs []string) ([]int64, error) {
	var ids []int64
	for _, ref := range refs {
		pr, err := retroLookupPR(ctx, d.Store, d.refs(), ref)
		if err != nil {
			return nil, err
		}
		if pr.GHState == store.GHOpen {
			return nil, fmt.Errorf("%s is still open: a retro looks at merged or closed PRs only", ref)
		}
		if !slices.Contains(ids, pr.ID) {
			ids = append(ids, pr.ID)
		}
	}
	return ids, nil
}

// retroLookupPR finds the PR a reference names in the registry (retro and
// misses look at PRs magnum knows; they never read GitHub).
func retroLookupPR(ctx context.Context, st *store.Store, refs app.RefParser, ref string) (store.PR, error) {
	_, pr, err := app.LookupPR(ctx, st, refs, ref)
	if errors.Is(err, store.ErrNotFound) {
		return store.PR{}, fmt.Errorf("%s is not in the registry (`magnum prs --all` lists the PRs magnum knows)", ref)
	}
	return pr, err
}
