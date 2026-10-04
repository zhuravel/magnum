package cli

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// stopKind is `magnum abort` or `magnum ignore`: both hand the daemon a
// request that stops the PR's agents, parks its sessions and frees its slot.
type stopKind struct {
	name, req string
	running   bool // only a PR with a running review is a target (abort)
}

var (
	stopAbort  = stopKind{name: "abort", req: engine.ReqAbort, running: true}
	stopIgnore = stopKind{name: "ignore", req: engine.ReqIgnore}
)

type stopOpts struct {
	timeout time.Duration
	json    bool
}

// stopTimeout bounds the wait for the daemon: interrupting the agents,
// parking the sessions and releasing a pool slot take a while.
const stopTimeout = 3 * time.Minute

func stopUsage(k stopKind) string { return k.name + " <ref> [--timeout d] [--json]" }

func newAbortCmd(c *Context) *cobra.Command {
	return newStopCmd(c, stopAbort, "kill a PR's running review",
		"Kill the running review of a PR: the daemon cancels the round, interrupts its agents (ctrl+c), marks its "+
			"runs abandoned (\"aborted by user\"), parks the sessions (conversations stay resumable) and hands a pool "+
			"slot back (a per-PR worktree is kept). The PR goes back to reviewed, or baseline when it was never "+
			"reviewed: the next push queues it again. Exits 1 when no review of the PR is running.")
}

func newStopCmd(c *Context, k stopKind, short, long string) *cobra.Command {
	o := stopOpts{timeout: stopTimeout}
	cmd := newCommand(groupAct, stopUsage(k), short, long, func(pos []string) int { return runStop(c, k, o, pos) })
	fs := cmd.Flags()
	fs.DurationVar(&o.timeout, "timeout", stopTimeout, "how long to wait for the daemon (0 = no limit)")
	fs.BoolVar(&o.json, "json", false, "print the request outcome as JSON")
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runStop(c *Context, k stopKind, o stopOpts, pos []string) int {
	switch {
	case len(pos) == 0:
		return actUsage(c, k.name, "which PR?", stopUsage(k))
	case len(pos) > 1:
		return actUsage(c, k.name, "one PR at a time", stopUsage(k))
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, k.name, err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return stopMain(ctx, c, d, k, pos[0], o)
}

// inFlight are the PR states of a running round (claiming, reviewing,
// verifying).
var inFlight = []string{store.PRClaiming, store.PRReviewing, store.PRVerifying}

// stopMain hands an abort or ignore of ref to the daemon and waits for it.
// An abort of a PR without a running round fails here, before any request;
// without a daemon nothing runs, so an abort fails, and an ignore is queued
// for the next start (like mute).
func stopMain(ctx context.Context, c *Context, d *actDeps, k stopKind, ref string, o stopOpts) int {
	t, err := d.resolve(ctx, ref, "", "")
	if err != nil {
		return cmdFail(c, k.name, err)
	}
	label := d.actLabel(t.full(), t.PR.Number)
	if k.running && !slices.Contains(inFlight, t.PR.State) {
		return cmdFail(c, k.name, fmt.Errorf("no review of %s is running (state %s)", label, t.PR.State))
	}
	id, pid, err := d.submit(ctx, k.req, engine.TargetPayload{PRTarget: t.prTarget()})
	if err != nil {
		if id == 0 {
			return cmdFail(c, k.name, err)
		}
		fmt.Fprintln(c.Stderr, err)
	}
	if pid == 0 {
		if k.running {
			// Nothing runs without a daemon: the next one must not act on it.
			why := "no review of " + label + " is running: no daemon is running"
			_ = d.Store.CompleteRequest(ctx, id, store.RequestFailed, why)
			return cmdFail(c, k.name, fmt.Errorf("%s (%s)", why, actDaemonFix))
		}
		req, err := d.Store.RequestByID(ctx, id)
		if err != nil {
			return cmdFail(c, k.name, err)
		}
		if o.json {
			_ = writeJSON(c.Stdout, actRequestView(req, pid))
			return 0
		}
		fmt.Fprintf(c.Stderr, "%s %s is queued as request %d and applies when the daemon starts: %s\n", k.name, label, id, actDaemonFix)
		return 0
	}
	if !o.json {
		fmt.Fprintf(c.Stderr, "asked the daemon (pid %d) to %s %s (request %d)…\n", pid, k.name, label, id)
	}
	req, err := d.await(ctx, id, d.quickPoll(), o.timeout)
	if err != nil && req.ID == 0 {
		return cmdFail(c, k.name, err)
	}
	if o.json {
		_ = writeJSON(c.Stdout, actRequestView(req, pid))
		if req.State != store.RequestDone {
			return 1
		}
		return 0
	}
	if req.State == store.RequestPending {
		return cmdFail(c, k.name, fmt.Errorf("request %d is still running after %s (`magnum logs request:%d` follows it)", id, o.timeout, id))
	}
	return actRequestOutcome(c.Stdout, c.Stderr, req, pid)
}
