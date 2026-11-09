package cli

// `magnum eval add --local-task <id> --round <n>`: a round of the factory's
// local review as an eval case. It freezes what the round read
// (evalrun.FreezeLocal, read only: the factory's registry and the round's
// own files) in the case's directory beside the learned corpus
// (evalrun.LocalCaseDir) and prints the case for the operator's corpus,
// where the operator gives it the defects the round should have found, in
// the syntax of a posted case's.

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/textx"
)

const evalAddUsage = "--local-task ID --round N [--cycle N] [--name NAME]"

// evalAddFlags are the parsed `magnum eval add` flags.
type evalAddFlags struct {
	task         int64
	round, cycle int
	name         string
}

func newEvalAddCmd(c *Context) *cobra.Command {
	var f evalAddFlags
	cmd := newCommand("", "add "+evalAddUsage, "freeze a round of the factory's local review as a local eval case",
		"Freeze round N of factory task ID's local review as a local eval case, so that `magnum eval run` replays it: "+
			"a change to the review skill or the judge prompts can then be proven on a round that missed. It reads, read "+
			"only, the factory's registry (the task's repository and branch, the head the round's cycle reviewed, the roles "+
			"its begin event names, when each answer was given) and the round's own files (its result in the task's "+
			"directory; the description.md, ticket.md and prepass.json of its scratch directory: the title and plan, the "+
			"ticket, the base it diffed from), and the operator's answers as the round could read them (those its "+
			"ticket file held, else answers.md when nothing was answered or written since the round began). It copies "+
			"them to the case's own directory beside the learned corpus (learn/local/<name>/ in the data directory), "+
			"never to read the task's directory again, and refuses a round it cannot freeze whole, saying what is "+
			"missing. Then it prints the round's findings and the [[case]] to add to ~/.config/magnum/"+evalCorpusFile+
			" with the defects the round should have found (the posted cases' syntax; question = true asks that the "+
			"finding be a question for you, at the defect's severity or stronger). --cycle picks the review cycle when "+
			"the task's loops ran round N more than once; --name names the case (default task-<id>-round-<n>).",
		func(pos []string) int {
			switch {
			case len(pos) > 0:
				return inspUsage(c, "eval add", "no arguments expected", evalAddUsage)
			case f.task <= 0:
				return inspUsage(c, "eval add", "--local-task names the factory task (its id, `magnum factory` lists them)", evalAddUsage)
			case f.round < 1 || f.round > config.MaxFactoryCycles:
				return inspUsage(c, "eval add", fmt.Sprintf("--round is the round of its local review, 1 to %d", config.MaxFactoryCycles), evalAddUsage)
			case f.cycle < 0:
				return inspUsage(c, "eval add", "--cycle is a review cycle's number", evalAddUsage)
			case f.name != "" && !eval.ValidName(f.name):
				return inspUsage(c, "eval add", fmt.Sprintf("--name %q is no case name ([a-z0-9][a-z0-9-], at most 48)", f.name), evalAddUsage)
			}
			return runEvalAdd(c, f)
		})
	fs := cmd.Flags()
	fs.Int64Var(&f.task, "local-task", 0, "the factory task whose local review round to freeze")
	fs.IntVar(&f.round, "round", 0, "the round of its local review")
	fs.IntVar(&f.cycle, "cycle", 0, "the review cycle, when several ran the round")
	fs.StringVar(&f.name, "name", "", "the case's name (default task-<id>-round-<n>)")
	return cmd
}

func runEvalAdd(c *Context, f evalAddFlags) int {
	if err := c.LoadConfig(); err != nil {
		return cmdFail(c, "eval add", fmt.Errorf("%w (fix config.toml; `magnum config` validates it)", err))
	}
	name := f.name
	if name == "" {
		name = evalrun.LocalCaseName(f.task, f.round)
	}
	user, learned := evalCorpusPath(c, "")
	if corpus, err := eval.LoadCorpora(user, learned); err == nil && slices.ContainsFunc(corpus.Cases, func(cs eval.Case) bool { return cs.Name == name }) {
		return cmdFail(c, "eval add", fmt.Errorf("a case named %s is already in the corpus: give the round another --name", name))
	}
	ctx, stop := signalContext()
	defer stop()
	deps := evalrun.Deps{Layout: c.Layout, Config: c.Config}
	in, res, err := evalrun.FreezeLocal(ctx, deps, f.task, f.round, f.cycle)
	if err != nil {
		return cmdFail(c, "eval add", err)
	}
	dir := evalrun.LocalCaseDir(c.Layout, name)
	if err := evalrun.SaveLocalInputs(dir, in); err != nil {
		return cmdFail(c, "eval add", err)
	}
	w := c.Stdout
	fmt.Fprintf(w, "froze round %d (cycle %d) of factory task %d on %s (%s, head %s, base %s; roles %s) in %s\n", in.Round, in.Cycle, in.Task,
		in.Repo, in.Branch, textx.ShortSHA(in.HeadSHA), textx.ShortSHA(in.BaseSHA), strings.Join(in.Roles, ", "), inspTilde(dir))
	if in.CodexNever {
		var codex []string
		for _, role := range in.Roles {
			if r, ok := c.Config.RoleByNameOrAlias(nil, role); ok && r.AgentKind() == config.KindCodex {
				codex = append(codex, role)
			}
		}
		if len(codex) > 0 {
			fmt.Fprintf(w, "Codex never works on task %d again (the factory's mark), and the round ran %s: eval run refuses the case\n", in.Task, strings.Join(codex, ", "))
		} else {
			fmt.Fprintf(w, "Codex never works on task %d again (the factory's mark); the round ran without Codex, so eval run replays it\n", in.Task)
		}
	}
	verdict := ""
	if res.Verdict != "" {
		verdict = " (verdict " + res.Verdict + ")"
	}
	fmt.Fprintf(w, "the round ended %s%s with %s\n", res.Outcome, verdict, textx.Count(len(res.Findings), "finding", "findings"))
	for _, lf := range res.Findings {
		fmt.Fprintf(w, "  %s\n", evalLocalFinding(lf))
	}
	fmt.Fprintf(w, "\nAdd the case to %s with the defects the round should have found (eval.toml.example has the syntax), "+
		"then replay it with `magnum eval run --case %s`:\n\n", inspTilde(evalDefaultCorpus(c)), name)
	fmt.Fprintf(w, "[[case]]\nname = %q\nkind = %q\npr = %q   # round %d of factory task %d: %d + task × 10 + round, no PR on GitHub\nhead = %q\n"+
		"why = %q\n\n", name, eval.KindLocal, in.PR(), in.Round, in.Task, eval.LocalNumberBase, in.HeadSHA,
		fmt.Sprintf("round %d of factory task %d", in.Round, in.Task))
	fmt.Fprintln(w, "# [[case.defect]]")
	for _, l := range [][2]string{
		{`id = "…"`, "[a-z0-9-], unique in the case"},
		{`title = "…"`, ""},
		{`severity = "P2"`, "the weakest priority that counts: the factory asks you only P0 to P2"},
		{"question = true", "the finding must be a question for you (P2 or stronger without a severity), not a change the writer makes"},
		{`paths = ["app/**/*.rb"]`, "globs, as a posted case's"},
		{`match = ["…"]`, "case-insensitive regular expressions on the finding's title and body"},
	} {
		if l[1] == "" {
			fmt.Fprintf(w, "# %s\n", l[0])
			continue
		}
		fmt.Fprintf(w, "# %-24s # %s\n", l[0], l[1])
	}
	return 0
}

// evalLocalFinding is one finding of a local round as `eval add` lists it:
// its priority, what kind it is, where and its title.
func evalLocalFinding(lf evalrun.LocalFinding) string {
	parts := []string{cmp.Or(lf.Severity, "-")}
	switch {
	case lf.Simplification:
		parts = append(parts, "simplification")
	case lf.Question:
		parts = append(parts, "question")
	}
	where := cmp.Or(lf.Path, "-")
	if lf.Line > 0 {
		where = fmt.Sprintf("%s:%d", lf.Path, lf.Line)
	}
	return strings.Join(append(parts, where, textx.Clip(textx.FirstLine(lf.Title), 100)), " ")
}

// evalDefaultCorpus is the operator's corpus file: eval.toml in the user
// config's directory (~/.config/magnum), where a local case goes once its
// defects are written.
func evalDefaultCorpus(c *Context) string {
	dir := c.Layout.ConfigDir()
	if dir == "" {
		dir = filepath.Join(paths.Expand("~"), ".config", "magnum")
	}
	return filepath.Join(dir, evalCorpusFile)
}

// evalCaseWhat is how `eval run` names a case on its line: its PR, and
// for a local case the round and task its synthetic number names.
func evalCaseWhat(ec eval.Case) string {
	if !ec.Local() {
		return ec.PR
	}
	task, round, ok := evalrun.LocalRoundOf(ec.Number)
	if !ok {
		return ec.PR + " (local)"
	}
	return fmt.Sprintf("%s (local round %d of task %d)", ec.PR, round, task)
}
