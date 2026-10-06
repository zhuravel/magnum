package pipeline

import (
	"context"

	"github.com/zhuravel/magnum/internal/agents"
)

// addProjectDeclined tells the judge which of the round's agent CLIs ran
// without the PR's changes to their project config in the checkout (Codex
// with it untrusted, Claude with the user's settings only): the PR's
// records, written at each launch, name the round's head
// (agents.NoteDeclinedProjects). The skill adds a line to the review's
// Checks for each.
func (rd *round) addProjectDeclined(ctx context.Context, jd *agents.JudgeData) {
	agents.NoteDeclinedProjects(ctx, rd.r.Store, rd.pr.ID, rd.in.TargetSHA, jd)
}
