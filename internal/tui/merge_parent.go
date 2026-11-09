package tui

// P, a merge of the row's PR's parent on the board, the dashboard and the
// factory screen: the daemon's plan first (a dry run changes nothing),
// whose question goes up as the y/N, and y sends the merge bound to what
// the plan asked about (MergeAsked), which the daemon checks again before
// it merges: a head, base, tip or conflict that moved since, or a question
// 10 minutes old, merges nothing.

import (
	"context"
	"time"
)

// MergeAsked is what a merge of the parent's y/N binds its merge to: the
// PR's head, the parent (its base branch) and the tip of it the plan read,
// how many files the merge conflicts in (0: none) and when the daemon
// planned it.
type MergeAsked struct {
	Head, Base, Tip string
	Conflicts       int
	At              time.Time
}

// mergeParentRun is what P runs on ref on the board and the dashboard: the
// daemon's plan, whose question then goes up as the y/N, and y the merge
// bound to it. A plan without a question or a binding (nothing to merge)
// asks nothing.
func mergeParentRun(ref string) (string, actionFunc) {
	what := rowActDefs[actMergeParent].what + " " + ref
	return what + " plan", func(ctx context.Context, act DashboardActions) (ActionResult, error) {
		res, err := act.MergeParentPlan(ctx, ref)
		if err == nil && res.Ask != "" && res.Parent != nil {
			asked := *res.Parent
			res.next = &pendingAction{question: res.Ask, what: what, target: ref, wrap: true,
				fn: func(ctx context.Context, act DashboardActions) (ActionResult, error) {
					return act.MergeParent(ctx, ref, asked)
				}}
		}
		return res, err
	}
}
