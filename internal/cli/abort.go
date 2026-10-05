package cli

import (
	"context"
	"errors"
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
	running   bool // only a PR with a review running or waiting in line is a target (abort)
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
	return newStopCmd(c, stopAbort, "kill a PR's running review, or take back a queued one",
		"Kill the running (or paused) review of a PR: the daemon cancels the round, interrupts its agents (ctrl+c), "+
			"marks its runs abandoned (\"aborted by user\"), parks the sessions (conversations stay resumable) and "+
			"hands a pool slot back (a per-PR worktree is kept). A review that waits in line (one `magnum review` "+
			"asked for, or an automatic one) is taken back before it starts: its forced mark and what it asked for "+
			"(fresh sessions, roles, a dry run) go, and nothing else is touched. Either way the PR goes back to "+
			"reviewed, or baseline when it was never reviewed (a merged PR's post-merge review: back to closed), "+
			"and the next push queues it again. Exits 1 when no review of the PR runs or waits.")
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

// abortable are the PR states abort takes a review from: a round running
// (claiming, reviewing, verifying) or paused, and a review waiting in line
// (queued, rereview_pending).
var abortable = []string{store.PRClaiming, store.PRReviewing, store.PRVerifying, store.PRPaused, store.PRQueued, store.PRRereviewPending}

// stopMain hands an abort or ignore of ref to the daemon and waits for it.
// An abort of a PR with no review running or waiting fails here, before any
// request; without a daemon nothing runs, so an abort fails, and an ignore is
// queued for the next start (like mute).
func stopMain(ctx context.Context, c *Context, d *actDeps, k stopKind, ref string, o stopOpts) int {
	t, err := d.resolve(ctx, ref, "", "")
	if err != nil {
		return cmdFail(c, k.name, verbFix(k.name, err))
	}
	label := d.actLabel(t.full(), t.PR.Number)
	if k.running && !slices.Contains(abortable, t.PR.State) {
		return cmdFail(c, k.name, fmt.Errorf("no review of %s is running or queued (state %s)", label, t.PR.State))
	}
	// Nothing runs without a daemon: an abort is refused then (the next
	// daemon must not act on it); an ignore is queued for its start.
	rc := d.reqs()
	out, err := rc.send(ctx, k.req, engine.TargetPayload{PRTarget: t.prTarget()}, reqSend{NeedDaemon: k.running})
	switch {
	case errors.Is(err, errNoDaemon):
		return cmdFail(c, k.name, fmt.Errorf("nothing was queued for %s: %w", label, err))
	case err != nil:
		return cmdFail(c, k.name, err)
	}
	id := out.ID()
	if out.PID == 0 && out.Pending() {
		if o.json {
			_ = writeJSON(c.Stdout, out.view())
			return 0
		}
		out.printNotes(c.Stderr)
		fmt.Fprintf(c.Stderr, "%s %s is queued as request %d and applies when the daemon starts: %s\n", k.name, label, id, actDaemonFix)
		return 0
	}
	if !o.json {
		fmt.Fprintf(c.Stderr, "asked the daemon (pid %d) to %s %s (request %d)…\n", out.PID, k.name, label, id)
	}
	if out.Pending() {
		if err := rc.wait(ctx, &out, o.timeout, d.quickPoll()); err != nil && out.Pending() {
			return cmdFail(c, k.name, err)
		}
	}
	if o.json {
		_ = writeJSON(c.Stdout, out.view())
		if out.Req.State != store.RequestDone {
			return 1
		}
		return 0
	}
	if out.Pending() {
		return cmdFail(c, k.name, fmt.Errorf("request %d is still running after %s (`magnum logs request:%d` follows it)", id, o.timeout, id))
	}
	return out.print(c.Stdout, c.Stderr)
}
