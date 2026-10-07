package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

const codexFlagUsage = "codex-flag set <ref> [reason...] | codex-flag clear <ref>"

func newCodexFlagCmd(c *Context) *cobra.Command {
	cmd := newCommand(groupAct, codexFlagUsage, "flag a PR Codex warned about (never reviewed again), or clear that flag",
		"Codex may block an account it takes for a cyber abuser, also when all it reads is the team's own code. A round "+
			"Codex refuses (\"This content was flagged for possible cybersecurity risk\", its other safety warnings: "+
			"[kinds.<kind>.health_patterns] refused) ends at once and flags its PR: magnum never reviews that PR again, "+
			"on any head, by itself or when asked (no retry, continue, reply round or delta check; `magnum review` and the "+
			"board's review keys refuse with the reason). The board reads \"Codex flagged · never reviewed again\".\n\n"+
			"`set` flags a PR by hand, one Codex warned about elsewhere or before magnum could tell; the words after the ref "+
			"are the reason the card shows. `clear` lifts the flag after a y/N question on a terminal that names the "+
			"account risk; magnum then reviews the PR again as its watch says. The daemon applies both (queued until one runs).",
		func(pos []string) int { return runCodexFlag(c, pos) })
	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return []cobra.Completion{"set", "clear"}, cobra.ShellCompDirectiveNoFileComp
		case 1:
			return completeFrom(toComplete, c.completePRs)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runCodexFlag(c *Context, pos []string) int {
	switch {
	case len(pos) == 0 || (pos[0] != "set" && pos[0] != "clear"):
		return actUsage(c, "codex-flag", "set or clear?", codexFlagUsage)
	case len(pos) < 2:
		return actUsage(c, "codex-flag", "which PR?", codexFlagUsage)
	case pos[0] == "clear" && len(pos) > 2:
		return actUsage(c, "codex-flag", "clear takes one PR", codexFlagUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "codex-flag", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return codexFlagMain(ctx, c, d, pos[0] == "clear", pos[1], strings.Join(pos[2:], " "))
}

// codexFlagMain hands the flag (or its clearing) of the PR ref to the
// daemon. A clear asks y/N first, on a terminal only, naming when and where
// Codex flagged the PR and the account risk.
func codexFlagMain(ctx context.Context, c *Context, d *actDeps, clear bool, ref, reason string) int {
	t, err := d.resolveRef(ctx, ref)
	if err != nil {
		return cmdFail(c, "codex-flag", verbFix("codex-flag", err))
	}
	label := d.actLabel(t.full(), t.PR.Number)
	if clear {
		f, ok := codexFlagOf(ctx, d.Store, t.PR.ID)
		if !ok {
			fmt.Fprintf(c.Stdout, "%s is not flagged\n", label)
			return 0
		}
		if !d.StdinTTY || !d.StdoutTTY {
			return cmdFail(c, "codex-flag", fmt.Errorf("clear asks y/N on a terminal: run `magnum codex-flag clear %s` there", label))
		}
		q := fmt.Sprintf("%s. Clear the flag of %s? %s may block the account it runs under when it flags this PR again, "+
			"and magnum will review it again", strings.TrimSuffix(f.Sentence(label), "."), label, f.Who())
		if !d.confirm(ctx, c.Stdout, q) {
			fmt.Fprintln(c.Stderr, "the flag stays")
			if ctx.Err() != nil {
				return 130
			}
			return 1
		}
	}
	p := engine.CodexFlagPayload{PRTarget: t.prTarget(), Clear: clear, Reason: reason, By: "magnum codex-flag"}
	out, err := d.reqs().send(ctx, engine.ReqCodexFlag, p, d.quick())
	if err != nil {
		return cmdFail(c, "codex-flag", err)
	}
	if out.Pending() && out.PID == 0 {
		out.printNotes(c.Stderr)
		fmt.Fprintf(c.Stderr, "codex-flag %s is queued as request %d and applies when the daemon starts: %s\n", label, out.ID(), actDaemonFix)
		return 0
	}
	return out.print(c.Stdout, c.Stderr)
}

// codexFlagOf is the PR's Codex flag (engine.KVPRCodexFlag), when it has
// one; a registry that cannot say reads as none.
func codexFlagOf(ctx context.Context, st *store.Store, prID int64) (engine.CodexFlag, bool) {
	if st == nil || prID == 0 {
		return engine.CodexFlag{}, false
	}
	v, ok, err := st.GetKV(ctx, engine.KVPRCodexFlag(prID))
	if err != nil || !ok {
		return engine.CodexFlag{}, false
	}
	return engine.ParseCodexFlag(v)
}
