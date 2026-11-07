package engine

import (
	"testing"

	"github.com/zhuravel/magnum/internal/github"
)

// requiredCompletion makes GitHub's rulesets of talkable/talkable require
// Completion, a check of its CI workflow.
func requiredCompletion(h *harness) {
	h.gh.required = map[string][]string{"talkable/talkable": {"Completion"}}
}

// othersPassed opens PR #2 at head with the checks of the other apps
// passed and no run of CI: the rollup is SUCCESS.
func (h *harness) othersPassed(head string) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: head, author: "zhuravel", ci: "SUCCESS",
		checks: []github.Check{ciCheck("Pronto", "pronto", github.CheckPassed), ciCheck("", "Devin Review", github.CheckPassed)}})
}

// The live case: an own PR whose repository requires Completion, and whose
// CI workflow runs only when a PR opens. GitHub lists the other apps'
// checks at the head, all passed, and the rollup says SUCCESS; the look
// called the PR idle ("nothing open") while GitHub blocked its merge. Once
// the head is older than settle_max it now says that the required check
// did not run there.
func TestTheLookNeverCallsAHeadIdleWhoseRequiredCheckDidNotRun(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion)
	h.othersPassed(fixHead)
	h.tick() // reviewed clean
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a")
}
