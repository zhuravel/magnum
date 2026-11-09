package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// LocalRound is one round of the factory's local review of a change before
// its PR exists (docs/superpowers/specs/2026-11-03-factory-design.md,
// section 6.5): a replay's case (EvalCase) whose number is a high synthetic
// one, whose Checkout is the task's copy at Head (the change's head, on its
// branch) and whose PR is on no GitHub, with the change's base (BaseSHA,
// the commit its diff starts at) and the files the judge reads in place of
// GitHub's PR: DescriptionFile (the title and body, description.md) and
// TicketFile (ticket.md: the task's ticket, then the operator's answers to
// the factory's questions under "## The operator's answers to the factory's
// questions", evalrun.LocalTicket; "" = neither). The factory writes both
// before the round (evalrun.RunLocal).
type LocalRound struct {
	EvalCase
	BaseSHA                     string
	DescriptionFile, TicketFile string
	// Env is the environment every pane and the readiness step of the
	// round get, as the caller gives it: the engine derives none from the
	// checkout. The factory gives the env of the copy the round works in,
	// the one its own agents get there (its WT_BRANCH and env); the replay
	// of a local eval case (evalrun.RunCase) gives the env a posted replay's
	// per-PR worktree has for the synthetic number, so its roles never use
	// a copy's databases.
	Env map[string]string
}

// localJob is a local round's part of its round job (roundJob.local): the
// base, the files and the environment of its LocalRound.
type localJob struct {
	base, description, ticket string
	env                       map[string]string
}

// fullSHA is a full commit id.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// RunLocal runs one local round for c and returns the pipeline's result and
// the agents' PR row (for ReleaseEval), as RunEval runs a replay: on an
// engine built on a scratch layout only (ErrEvalLayout otherwise), a blind
// dry run of the watch's roles in Checkout, in the workspace "eval
// <repo>#<N>" (an earlier round's left open is closed first). It differs in
// what the round reads: its base is always c.BaseSHA, whatever the
// checkout's base branch says, so post-review anchors the comments on the
// local diff (--local-base), and the round input's Local names c's files,
// so the judge reads the title, the body and the ticket from them and
// nothing from GitHub, and the round reads no Jira issue; its panes and its
// readiness step get c.Env, the caller's (the factory's copy's env, or an
// eval replay's per-PR env of the synthetic number: LocalRound.Env), never
// one derived from the checkout. A case without a full base commit or a
// description file, or a judge-only one, never starts.
func (e *Engine) RunLocal(ctx context.Context, c LocalRound) (pipeline.RoundResult, store.PR, error) {
	if e.d.Layout.Scratch == "" {
		return pipeline.RoundResult{}, store.PR{}, ErrEvalLayout
	}
	switch {
	case c.Head == "" || c.Checkout == "" || c.Number <= 0 || c.URL == "":
		return pipeline.RoundResult{}, store.PR{}, errors.New("engine: a local round needs a number, URL, head and checkout")
	case !fullSHA.MatchString(c.BaseSHA):
		return pipeline.RoundResult{}, store.PR{}, fmt.Errorf("engine: local %s/%s#%d: the base %q is not a full commit id", c.Owner, c.Repo, c.Number, c.BaseSHA)
	case c.DescriptionFile == "":
		return pipeline.RoundResult{}, store.PR{}, fmt.Errorf("engine: local %s/%s#%d: no description file", c.Owner, c.Repo, c.Number)
	case c.JudgeOnly:
		return pipeline.RoundResult{}, store.PR{}, fmt.Errorf("engine: local %s/%s#%d: a local round runs every role", c.Owner, c.Repo, c.Number)
	}
	return e.runEval(ctx, c.EvalCase, &localJob{base: c.BaseSHA, description: c.DescriptionFile, ticket: c.TicketFile, env: maps.Clone(c.Env)})
}
