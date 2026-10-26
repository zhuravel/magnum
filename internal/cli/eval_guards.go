package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/learn"
)

const evalGuardsUsage = "[--n N] [--corpus FILE]"

func newEvalGuardsCmd(c *Context) *cobra.Command {
	var corpus string
	var n int
	cmd := newCommand("", "guards "+evalGuardsUsage, "the cases the skill gate would guard now",
		"Print, one per line, the cases the skill gate would replay now as its guards (the [learn] gate_guards "+
			"places, chosen as the gate chooses them): cases whose newest stored replay at the head the corpus pins "+
			"found a defect, the least recently replayed first, of a pull request a watch covers that no refused replay "+
			"or Codex flag marks. An improve run passes them to eval-at.sh with the cases a change targets, so the "+
			"replays show what the change loses as well as what it finds. It only reads: nothing is written or replayed. "+
			"--n is how many (default 2); --corpus reads that file alone instead of both corpora the gate reads.",
		func(pos []string) int {
			if len(pos) > 0 {
				return inspUsage(c, "eval guards", "no arguments expected", evalGuardsUsage)
			}
			if n < 0 {
				return inspUsage(c, "eval guards", fmt.Sprintf("--n must be 0 or more, not %d", n), evalGuardsUsage)
			}
			return runEvalGuards(c, corpus, n)
		})
	fs := cmd.Flags()
	fs.IntVar(&n, "n", 2, "how many guard cases")
	fs.StringVar(&corpus, "corpus", "", evalCorpusFlagUsage)
	return cmd
}

// runEvalGuards prints the guard places learn.SelectCases gives a gate
// with no target (learn.GuardsFrom over every stored run: no candidate's
// replays to leave out, no proposal to date), over the corpora the gate
// reads (cli.daemonGate's: a default corpus file that is not there is
// none) or --corpus alone, the gate's watches and its flags (a refused
// replay of a case of the PR, a Codex flag the registry holds:
// evalrun.Refused, which opens the registry read-only).
func runEvalGuards(c *Context, corpusFlag string, n int) int {
	if err := c.LoadConfig(); err != nil {
		return cmdFail(c, "eval guards", fmt.Errorf("%w (fix config.toml; `magnum config` validates it)", err))
	}
	user, learned := evalCorpusPath(c, corpusFlag)
	if corpusFlag == "" && user != "" && !fsx.Exists(user) {
		user = ""
	}
	corpus, err := eval.LoadCorpora(user, learned)
	if err != nil {
		return cmdFail(c, "eval guards", err)
	}
	runs, err := eval.ListRuns(evalRunsRoot(c.Layout))
	if err != nil {
		return cmdFail(c, "eval guards", err)
	}
	ctx, stop := signalContext()
	defer stop()
	deps := evalrun.Deps{Layout: c.Layout, Config: c.Config}
	flagged := func(pr string) bool {
		for _, ec := range corpus.Cases {
			if ec.PR == pr && evalrun.Refused(ctx, deps, ec) != "" {
				return true
			}
		}
		return false
	}
	watched := func(pr string) bool {
		repo, _, _ := strings.Cut(pr, "#")
		return c.Config.WatchFor(repo) != nil
	}
	for _, gc := range learn.SelectCases(corpus, nil, watched, flagged, 0, learn.GuardsFrom(runs, n, "", time.Time{})) {
		if gc.Guard {
			fmt.Fprintln(c.Stdout, gc.Case.Name)
		}
	}
	return 0
}
