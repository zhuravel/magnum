package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
)

// A re-review that holds its approval (approve_after_full_pass) names
// COMMENT for no findings though the repository allows APPROVE, its prompt
// says that a clean verdict posts as a comment and gives the body line a
// full pass follows with, and the round says why in its judge event; the
// same round without the hold offers APPROVE and says none of it.
func TestAHeldReReviewOffersNoApprove(t *testing.T) {
	for _, hold := range []bool{true, false} {
		e := newEnv(t)
		e.cfg.Repos = []config.Repo{{Repo: "talkable/talkable", NoFindingsEvent: "APPROVE"}}
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(671, "COMMENTED", "COMMENT").behavior(t)}
		in := e.input(KindRereview)
		in.HoldApproval = hold
		if _, err := e.r.RunRound(e.ctx, in); err != nil {
			t.Fatalf("RunRound: %v", err)
		}
		judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
		const line = "A full pass of the whole PR follows before approval."
		var said bool
		for _, ev := range e.events() {
			said = said || strings.Contains(ev.Message, "no APPROVE this round: a full pass of the whole PR follows before approval")
		}
		if !hold {
			mustContain(t, "judge prompt", judge, "no_findings_event: APPROVE")
			if strings.Contains(judge, line) || said {
				t.Fatalf("a round without the hold says it holds (event %v):\n%s", said, judge)
			}
			continue
		}
		mustContain(t, "judge prompt", judge, "no_findings_event: COMMENT", "blocking_event: REQUEST_CHANGES",
			"This round cannot approve", "a clean verdict posts as a comment", "`"+line+"`")
		if !said {
			t.Errorf("events %+v: want the judge event to say why no APPROVE", e.events())
		}
	}
}
