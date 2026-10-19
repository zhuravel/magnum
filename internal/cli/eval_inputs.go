package cli

// What a replay records besides its score, so that runs can be compared and
// reused: the inputs it reviewed with (evalrun.Inputs: the judge skill, the
// roles' prompts, the role config, the stakes lines and the simplification
// caps, as one hash, and evalrun.ReviewerInputs, the reviewers' part of it; a
// candidate's, evalrun.WithCandidate, when it replays one). The notes its judge read and the Codex points it used are
// recorded per case (internal/evalrun). `magnum eval baseline` lists the
// stored replays a new run with the same inputs would repeat; it reads, it
// never replays.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/evalrun"
)

const evalBaselineUsage = "[--corpus FILE] [--case NAME]..."

func newEvalBaselineCmd(c *Context) *cobra.Command {
	var corpus string
	var cases []string
	cmd := newCommand("", "baseline "+evalBaselineUsage, "the stored replays a run of these cases would repeat",
		"For each case of the corpus (or each --case), list the stored runs that replayed it at the head the corpus "+
			"pins, from a clean checkout, with the inputs this binary and config give a review now (the judge skill, "+
			"the roles' prompts, the role config, the stakes lines and the simplification caps; run.json's inputs), "+
			"newest first. A new replay of such a case repeats one of them unless magnum's code changed since its "+
			"commit (eval-at.sh compares the Go files). It only reads: no replay starts.\n\n"+
			"Output, tab-separated: `inputs <hash>`, then per case one `reuse <case> <run> <commit> <found>/<total>` "+
			"line per stored run, or `run <case>` when there is none.",
		func(pos []string) int {
			if len(pos) > 0 {
				return inspUsage(c, "eval baseline", "no arguments expected", evalBaselineUsage)
			}
			return runEvalBaseline(c, corpus, cases)
		})
	fs := cmd.Flags()
	fs.StringVar(&corpus, "corpus", "", evalCorpusFlagUsage)
	fs.StringArrayVar(&cases, "case", nil, "only this case (repeatable)")
	return cmd
}

func runEvalBaseline(c *Context, corpusFlag string, names []string) int {
	if err := c.LoadConfig(); err != nil {
		return cmdFail(c, "eval baseline", fmt.Errorf("%w (fix config.toml; `magnum config` validates it)", err))
	}
	corpus, err := eval.LoadCorpora(evalCorpusPath(c, corpusFlag))
	if err != nil {
		return cmdFail(c, "eval baseline", err)
	}
	cases, err := corpus.Select(names)
	if err != nil {
		return cmdFail(c, "eval baseline", err)
	}
	runs, err := eval.ListRuns(evalRunsRoot(c.Layout))
	if err != nil {
		return cmdFail(c, "eval baseline", err)
	}
	inputs := evalrun.Inputs(c.Config)
	fmt.Fprintf(c.Stdout, "inputs\t%s\n", inputs)
	for _, ec := range cases {
		found := eval.Baselines(runs, inputs, ec)
		if len(found) == 0 {
			fmt.Fprintf(c.Stdout, "run\t%s\n", ec.Name)
			continue
		}
		for _, b := range found {
			commit := b.Commit
			if commit == "" {
				commit = "-"
			}
			fmt.Fprintf(c.Stdout, "reuse\t%s\t%s\t%s\t%d/%d\n", ec.Name, b.Run, commit, b.Case.Score.Found, b.Case.Score.Total)
		}
	}
	return 0
}
