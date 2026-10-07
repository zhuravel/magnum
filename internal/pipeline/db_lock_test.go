package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
)

// The judge's own pass and the reviewers run tests on the slot's databases
// at the same time: the reviewer's prompt and the judge's carry the db-lock
// line for the round's checkout, with the daemon's binary and each role's
// name, so they take turns.
func TestRoundPromptsNameTheCheckoutsDBLock(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), e.judgePosts(811, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	bin := e.layout.Binary()
	if bin == "" {
		t.Fatal("the test layout has no binary")
	}
	claude := e.ag.submitsFor(agents.RoleClaude)
	if want := "`" + agents.DBLockLine(bin, slotPath, string(agents.RoleClaude)) + " <command>`"; len(claude) != 1 || !strings.Contains(claude[0].Text, want) {
		t.Errorf("claude-review's prompt lacks %s:\n%+v", want, claude)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if want := "\ndb_lock: " + agents.DBLockLine(bin, slotPath, string(agents.RoleJudge)) + "\n"; len(judge) != 2 || !strings.Contains(judge[1].Text, want) {
		t.Errorf("the judge's candidates prompt lacks %q:\n%+v", want, judge)
	}
}
