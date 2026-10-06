package pipeline

import (
	"context"

	"github.com/zhuravel/magnum/internal/agents"
)

// addProjectDeclined tells the judge that the round's Codex sessions ran with
// the checkout untrusted because the PR changes .codex/: the PR's record
// (agents.CodexProjectDeclined, written at each Codex launch) names the
// round's head. The skill adds a line to the review's Checks.
func (rd *round) addProjectDeclined(ctx context.Context, jd *agents.JudgeData) {
	if n, ok := agents.CodexProjectDeclined(ctx, rd.r.Store, rd.pr.ID); ok && n.Head == rd.in.TargetSHA {
		jd.CodexProjectDeclined = true
	}
}
