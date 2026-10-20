package engine

// The shared GitHub floor (github_cost.go keeps the rate limits it reads):
// while an identity's GraphQL budget is below [github] budget_floor, the
// work that would start reading GitHub as it waits for the budget's reset
// instead of spending it to zero. A round's start waits at dispatch
// (WaitGitHub; a paused round's continuation finishes the round it began and
// is not held), and so does `magnum review`'s forced round, whose answer
// says so. A fix cycle is held: prepareFix reads nothing from GitHub below
// the floor and takes no pin off, and startFix refuses with
// githubBudgetError. `magnum fix` and autofix are kept as held fixes
// (KVPRFixHeld, with heldBudget) and a loop's next cycle is held in phase
// reviewing, as a drain holds them, and the tick that finds the budget
// recovered goes on with them before dispatch (continueBudgetHeld). A
// whiteboard is remembered as pending, as a drain remembers it, and made
// once the identity its job reads as recovered. The retro and a notes
// curation wait in their own queues (retro.go, notes_curate.go). Running
// rounds, fix jobs and whiteboards finish: the floor is checked only where
// work starts, never on a read.

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/zhuravel/magnum/internal/store"
)

// errGitHubBudget matches a githubBudgetError.
var errGitHubBudget = errors.New("the GitHub budget is below the floor")

// githubBudgetError refuses to start a fix cycle or a whiteboard while the
// GitHub budget of the identity it reads as is below the floor; job is the
// fix job prepareFix made (nil for a whiteboard), which a hold keeps
// without a second preparation.
type githubBudgetError struct {
	low githubLowBudget
	job *fixJob
}

func (e *githubBudgetError) Error() string { return e.low.text() }

func (e *githubBudgetError) Is(target error) bool { return target == errGitHubBudget }

// githubStarts is when work the low budget l holds starts, for an answer
// or a queue's entry: "starts when the GitHub budget of zhuravel recovers
// (github budget: zhuravel at 312 points until 17:13)".
func githubStarts(l githubLowBudget) string {
	return "starts when the GitHub budget of " + l.identity + " recovers (" + l.text() + ")"
}

// githubGate is the dispatch gate of a round whose poll or posting
// identity's GitHub budget is below the floor (the zero gate when neither
// is): the round would read as the one and post and verify as the other.
func (e *Engine) githubGate(job *roundJob) gate {
	low, ok := e.githubLow(job.watch.PollIdentity, job.pr.Identity)
	if !ok {
		return gate{}
	}
	return gate{reason: WaitGitHub, text: low.text(), identity: low.identity, until: low.until}
}

// holdForBudget marks the PR prID as held for the GitHub budget: the tick
// that finds its watch's poll identity's budget recovered goes on with its
// held fix or held next cycle (continueBudgetHeld).
func (e *Engine) holdForBudget(prID int64) {
	e.mu.Lock()
	e.budgetHeld[prID] = true
	e.mu.Unlock()
}

// continueBudgetHeld goes on, once a tick finds the GitHub budget they wait
// for recovered, with the fixes it held (budgetHeld): a `magnum fix` or an
// autofix held as a held fix starts as the restart would start it
// (startHeldFix), and a loop's held next cycle gets the decision the
// restart's recovery would make (recoverReviewingLoop); a budget still low
// holds them again. The whiteboards it left pending are made once the
// identity each one's job reads as recovered (budgetWhiteboards: a pass of
// the pending whiteboards, which leaves pending those whose identity is
// still low). Nothing goes on while
// the daemon drains: the drain's end or the restart decides them then.
func (e *Engine) continueBudgetHeld(ctx context.Context) {
	if e.d.DryRun || e.draining(ctx) || ctx.Err() != nil {
		return
	}
	e.mu.Lock()
	held := slices.Sorted(maps.Keys(e.budgetHeld))
	whiteboards := slices.Sorted(maps.Keys(e.budgetWhiteboards))
	e.mu.Unlock()
	for _, id := range held {
		pr, err := e.st.PRByID(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				e.mu.Lock()
				delete(e.budgetHeld, id)
				e.mu.Unlock()
			}
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		if w := e.cfg.WatchFor(repo.FullName()); w != nil {
			if _, low := e.githubLow(w.PollIdentity); low {
				continue
			}
		}
		e.mu.Lock()
		delete(e.budgetHeld, id)
		e.mu.Unlock()
		e.continueHeldLoop(ctx, id, "after the GitHub budget recovered", "while the GitHub budget held it")
	}
	recovered := false
	for _, id := range whiteboards {
		if _, low := e.githubLow(id); !low {
			e.mu.Lock()
			delete(e.budgetWhiteboards, id)
			e.mu.Unlock()
			recovered = true
		}
	}
	if recovered { // the pass leaves pending a whiteboard whose own identity is still low
		e.startPendingWhiteboards(ctx, "after the GitHub budget recovered")
	}
}
