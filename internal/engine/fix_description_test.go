package engine

import (
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// The fix prompt lets the fixer edit the PR's description only when it
// says that the PR is the operator's own (agents.FixPromptInput.OwnPR), and
// the fix job says so from its own check of the PR's author (ownPR), which
// it refuses every other PR by: a PR the factory opened is the operator's
// too, since the factory opens it as the operator.
func TestTheFixJobTellsTheFixerThatThePRIsTheOperatorsOwn(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	if !h.fixInput().OwnPR {
		t.Error("the fix prompt of the operator's own PR does not say that it is the operator's own")
	}
}
