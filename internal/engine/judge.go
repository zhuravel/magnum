package engine

import (
	"context"
	"slices"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// roundJudge is the judge's side of a PR's latest round, from its run rows.
type roundJudge struct {
	round int
	// prompted is the newest judge run of the round that was sent (a
	// continue or nudge of the original turn, or the original itself); nil
	// when the round never prompted its judge. The judge's own pass (runs of
	// kind own_pass, prompted with the reviewers: pipeline ownpass.go) is
	// part of the reviewers' stage, never its turn.
	prompted *store.Run
	// marker is the run whose id the round's review carries: the round's
	// first initial, rereview or recovery judge run, skipping one abandoned
	// before it was ever sent (a restart on a newer head replaces the
	// judge's run: pipeline restart). Continue and nudge runs quote it
	// (pipeline.RoundInput.ContinueRunID), whatever their own outcome, so a
	// second pause continues with the same marker. It falls back to
	// prompted when the original row is missing.
	marker *store.Run
}

// latestJudge finds the judge runs of the latest round in runs (oldest
// first, as store.RunsByPR returns them).
func (e *Engine) latestJudge(runs []store.Run) roundJudge {
	var j roundJudge
	for _, r := range runs {
		j.round = max(j.round, r.Round)
	}
	for i := range runs {
		r := &runs[i]
		if r.Round != j.round || !e.isJudge(r.Role) || r.Kind == store.RunOwnPass {
			continue
		}
		replaced := r.State == store.RunAbandoned && r.SubmittedAt == nil
		if j.marker == nil && r.Kind != store.RunContinue && r.Kind != store.RunNudge && !replaced {
			j.marker = r
		}
		if r.SubmittedAt != nil {
			j.prompted = r
		}
	}
	if j.marker == nil {
		j.marker = j.prompted
	}
	return j
}

// resumableOutcomes are the outcomes of a failed judge run whose turn a
// continue finishes: the pauses, and a stop (daemon shutdown) after which the
// judge's agent may still be working.
var resumableOutcomes = []string{pipeline.OutcomeUsageLimit, pipeline.OutcomeLoginRequired,
	pipeline.OutcomeOverloaded, pipeline.OutcomeStopped}

// judgePrompted reports whether the PR's current round already sent its
// judge prompt and that turn is not over, from the run rows: the newest
// prompted judge run of the latest round is still in flight (submitted,
// working, ended) or failed with a resumable outcome, and it belongs to the
// round the PR is in (not created before last_round_started_at, so a crash
// between "PR to reviewing" and the new round's runs never resumes the
// previous, finished round). Crash recovery pauses such a PR, so the turn is
// continued (and its review found by marker) instead of prompted again.
func (e *Engine) judgePrompted(ctx context.Context, pr store.PR) bool {
	runs, err := e.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		e.log.Warn("recovery: runs", "pr", pr.ID, "err", err)
		return false
	}
	r := e.latestJudge(runs).prompted
	if r == nil {
		return false
	}
	if pr.LastRoundStartedAt != nil && r.CreatedAt.Before(*pr.LastRoundStartedAt) {
		return false
	}
	switch r.State {
	case store.RunSubmitted, store.RunWorking, store.RunEnded:
		return true
	case store.RunFailed:
		return slices.Contains(resumableOutcomes, deref(r.Outcome))
	}
	return false
}
