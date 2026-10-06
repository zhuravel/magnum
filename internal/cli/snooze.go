package cli

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
)

const snoozeUsage = "snooze <ref> [--for 2h | --until 18:00 | --off]"

// snoozeDefault is how long a snooze without --for or --until lasts.
const snoozeDefault = 2 * time.Hour

type snoozeOpts struct {
	dur   time.Duration
	until string
	off   bool
	by    string // who asks, for the card: "" = magnum snooze
}

func newSnoozeCmd(c *Context) *cobra.Command {
	var o snoozeOpts
	cmd := newCommand(groupAct, snoozeUsage, "hold a PR's automatic reviews for a while (reviews you ask for still run)",
		"Snooze a PR: until the snooze ends no automatic round starts on it (a push, a re-review, a reply round, a delta "+
			"check), while `magnum review`, the board's review keys and a review request on GitHub still run one. It lasts "+
			"--for (default 2h) or until --until (a local time like 18:00, tomorrow once past, or an RFC3339 timestamp), "+
			"ends on its own and survives a daemon restart; --off lifts it. A round in flight finishes. The daemon applies "+
			"it (queued until one runs). The board's z does the same for 2h.",
		func(pos []string) int { return runSnooze(c, o, pos) })
	fs := cmd.Flags()
	fs.DurationVar(&o.dur, "for", 0, "snooze for this long (default 2h)")
	fs.StringVar(&o.until, "until", "", "snooze until this local time (18:00) or RFC3339 timestamp")
	fs.BoolVar(&o.off, "off", false, "lift the PR's snooze")
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runSnooze(c *Context, o snoozeOpts, pos []string) int {
	switch {
	case len(pos) == 0:
		return actUsage(c, "snooze", "which PR?", snoozeUsage)
	case len(pos) > 1:
		return actUsage(c, "snooze", "one PR at a time", snoozeUsage)
	case o.dur < 0:
		return actUsage(c, "snooze", "--for must be positive", snoozeUsage)
	case o.dur > 0 && o.until != "", o.off && (o.dur > 0 || o.until != ""):
		return actUsage(c, "snooze", "use one of --for, --until and --off", snoozeUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "snooze", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return snoozeMain(ctx, c, d, pos[0], o)
}

// snoozeMain hands the snooze (or its lifting) of the PR ref to the daemon,
// which applies it; with no daemon running it waits in the queue.
func snoozeMain(ctx context.Context, c *Context, d *actDeps, ref string, o snoozeOpts) int {
	p := engine.SnoozePayload{Off: o.off, By: cmp.Or(o.by, "magnum snooze")}
	if !o.off {
		now := d.now()
		p.Until = now.Add(cmp.Or(o.dur, snoozeDefault))
		if o.until != "" {
			t, err := pauseParseUntil(o.until, now)
			if err != nil {
				return actUsage(c, "snooze", err.Error(), snoozeUsage)
			}
			p.Until = t
		}
	}
	t, err := d.resolveRef(ctx, ref)
	if err != nil {
		return cmdFail(c, "snooze", verbFix("snooze", err))
	}
	p.PRTarget = t.prTarget()
	label := d.actLabel(t.full(), t.PR.Number)
	out, err := d.reqs().send(ctx, engine.ReqSnooze, p, d.quick())
	if err != nil {
		return cmdFail(c, "snooze", err)
	}
	if out.Pending() && out.PID == 0 {
		out.printNotes(c.Stderr)
		fmt.Fprintf(c.Stderr, "snooze %s is queued as request %d and applies when the daemon starts: %s\n", label, out.ID(), actDaemonFix)
		return 0
	}
	return out.print(c.Stdout, c.Stderr)
}
