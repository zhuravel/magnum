package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
)

// A scheduled full pass ([pipeline] full_pass_every) is a recovery whose
// every session started fresh: the reviewers review the whole PR with their
// first-review prompts at their full effort, and the judge's own pass and
// its recovery prompt carry the full-pass clause (the full PR diff again as
// a first review reads it, then every earlier thread under the reply
// contract), unlike a cold judge's recovery, which keeps the reviewers on
// the new commits.
func TestAFullPassReviewsTheWholePRWithEveryRoleAsAFirstReview(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), e.judgePosts(808, "COMMENTED", "COMMENT").behavior(t)}
	in := e.ownInput(KindRecovery)
	in.FullPass = true
	in.Previous = &PreviousReview{ID: 3012345678, Event: "CHANGES_REQUESTED", SHA: prevSHA}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	clause := "This is a scheduled full pass: review the full PR diff again"
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) != 2 {
		t.Fatalf("judge prompts = %d, want the own pass and the candidates", len(judge))
	}
	mustContain(t, "own-pass prompt", judge[0].Text, "mode: recovery", "phase: own_pass", clause, "full_pass: true")
	mustContain(t, "candidates prompt", judge[1].Text, "mode: recovery", "phase: candidates", clause, "full_pass: true",
		"(missed earlier)", "(full pass):**")
	for _, p := range judge {
		if strings.Contains(p.Text, "git log --oneline "+prevSHA+".."+target) {
			t.Errorf("a full pass's judge is scoped to the new commits:\n%s", p.Text)
		}
	}
	claude := e.ag.submitsFor(agents.RoleClaude)[0].Text
	if first, _, _ := strings.Cut(claude, "\n"); strings.Contains(claude, "New commits were pushed") || !strings.HasSuffix(first, " high") {
		t.Errorf("a full pass's reviewer prompt starts %q: want its first-review prompt at its full effort (high)\n%s", first, claude)
	}
}
