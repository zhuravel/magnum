package cli

// `magnum restart-sessions`: a running agent keeps the login it started with,
// so after a switch of account its live sessions spend the old account's
// budget. The daemon parks them (engine.ReqRestartSessions), and their next
// prompt resumes each one in a new process.

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
)

const restartSessionsUsage = "restart-sessions --tool <kind>|all [--pr <ref>] [--yes]"

type restartSessionsOpts struct {
	tool, pr string
	yes      bool
}

func newRestartSessionsCmd(c *Context) *cobra.Command {
	var o restartSessionsOpts
	cmd := newCommand(groupAct, restartSessionsUsage, "restart an agent kind's live sessions in new processes (after a switch of account)",
		"Restart the live sessions of an agent kind (codex, claude or any [kinds.<name>]; `magnum roles --kinds` lists them) "+
			"or of all kinds, of one PR with --pr: a running agent keeps the login it started with, so after a switch of account "+
			"its sessions spend the old account's budget. Each session parks (its pane closes, its conversation stays), an idle one "+
			"now and one at work after its turn, and its next prompt resumes the conversation in a new process; no round starts on "+
			"its PR before. A pinned PR's sessions are left alone. The daemon does it (refused when none runs), and the answer says "+
			"how many park now and how many after their turn. A terminal asks y/N first (--yes does not). A change of Codex login "+
			"restarts the Codex sessions on its own; `magnum resume --tool` restarts none.",
		func(pos []string) int { return runRestartSessions(c, o, pos) })
	fs := cmd.Flags()
	fs.StringVar(&o.tool, "tool", "", "the agent kind whose live sessions restart (codex, claude, ...) or all")
	fs.StringVar(&o.pr, "pr", "", "only that PR's sessions (URL, owner/repo#N, repo#N or N)")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	_ = cmd.RegisterFlagCompletionFunc("tool", completeFlag(c.completeKinds))
	_ = cmd.RegisterFlagCompletionFunc("pr", completeFlag(c.completePRs))
	return cmd
}

func runRestartSessions(c *Context, o restartSessionsOpts, pos []string) int {
	if len(pos) > 0 {
		return actUsage(c, "restart-sessions", "takes no arguments (--pr names a PR)", restartSessionsUsage)
	}
	if o.tool == "" {
		return actUsage(c, "restart-sessions", "which agent kind? --tool <kind> or all", restartSessionsUsage)
	}
	if code, refused := refuseInAgentPane(c, "restart-sessions"); refused {
		return code
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "restart-sessions", err)
	}
	defer d.Close()
	if msg := resumeToolRefusal(d.Cfg, o.tool); msg != "" {
		return actUsage(c, "restart-sessions", msg, restartSessionsUsage)
	}
	ctx, stop := signalContext()
	defer stop()
	return restartSessionsMain(ctx, c, d, o)
}

// restartSessionsMain asks y/N on a terminal, naming the live sessions it
// restarts (engine.RestartableSessions), and hands the restart to the
// running daemon: with none, nothing is queued.
func restartSessionsMain(ctx context.Context, c *Context, d *actDeps, o restartSessionsOpts) int {
	p := engine.RestartSessionsPayload{Tool: o.tool}
	var prID int64
	of := ""
	if o.pr != "" {
		t, err := d.resolve(ctx, o.pr, "", "")
		if err != nil {
			return cmdFail(c, "restart-sessions", verbFix("restart-sessions", err))
		}
		p.PRTarget, prID, of = t.prTarget(), t.PR.ID, " of "+d.actLabel(t.full(), t.PR.Number)
	}
	if d.StdinTTY && d.StdoutTTY && !o.yes {
		sessions, _, err := engine.RestartableSessions(ctx, d.Store, d.Cfg, o.tool, prID)
		if err != nil {
			return cmdFail(c, "restart-sessions", err)
		}
		if !d.confirm(ctx, c.Stderr, restartSessionsQuestion(len(sessions), o.tool, of)) {
			fmt.Fprintln(c.Stderr, "nothing restarted")
			if ctx.Err() != nil {
				return 130
			}
			return 1
		}
	}
	q := d.quick()
	q.NeedDaemon = true // a restart nobody waits for must not run when a daemon starts later
	out, err := d.reqs().send(ctx, engine.ReqRestartSessions, p, q)
	if err != nil {
		if errors.Is(err, errNoDaemon) {
			err = fmt.Errorf("nothing was queued: %w", err)
		}
		return cmdFail(c, "restart-sessions", err)
	}
	return out.print(c.Stdout, c.Stderr)
}

// restartSessionsQuestion is the y/N before a restart of n live sessions of
// kind ("all": every kind), of names a PR when one is named: "Restart the 3
// live codex sessions? They resume in new processes." (the settings
// screen's Runtime x asks the same).
func restartSessionsQuestion(n int, kind, of string) string {
	what := kind + " session"
	if kind == "all" {
		what = "agent session"
	}
	if n != 1 {
		what += "s"
	}
	return fmt.Sprintf("Restart the %d live %s%s? They resume in new processes.", n, what, of)
}
