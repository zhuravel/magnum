package tui

// c, a CI fix of the row's PR on the board and the dashboard (the engine's
// fix_ci.go, `magnum fix --ci`): the daemon's plan first (a dry run changes
// nothing), whose question goes up as the y/N, and y sends the CI fix
// bound to what the plan asked about (CIFixAsked), which the daemon checks
// again before it starts: a head, failed checks or count of CI fixes that
// moved since, or a question 10 minutes old, starts nothing.

import (
	"context"
	"time"
)

// CIFixAsked is what a CI fix's y/N binds its start to: the PR's head, the
// failed required entries, the CI fixes the PR had, and when the daemon
// planned it.
type CIFixAsked struct {
	Head    string
	Entries []string
	Count   int
	At      time.Time
}

// fixCIRun is what c runs on ref on the board and the dashboard: the
// daemon's plan, whose question then goes up as the y/N, and y the CI fix
// bound to it. A plan without a question or a binding asks nothing (the
// daemon's refusal is the action's error).
func fixCIRun(ref string) (string, actionFunc) {
	what := rowActDefs[actFixCI].what + " " + ref
	return what + " plan", func(ctx context.Context, act DashboardActions) (ActionResult, error) {
		res, err := act.FixCIPlan(ctx, ref)
		if err == nil && res.Ask != "" && res.CIFix != nil {
			asked := *res.CIFix
			res.next = &pendingAction{question: res.Ask, what: what, target: ref, wrap: true,
				fn: func(ctx context.Context, act DashboardActions) (ActionResult, error) {
					return act.FixCI(ctx, ref, asked)
				}}
		}
		return res, err
	}
}
