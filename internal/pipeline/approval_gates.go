package pipeline

// The approval's gates for the judge (DECISIONS "The judge's own approval
// passes the approval's gates", spec 09): a clean
// review approves where the identity's no-findings event is APPROVE, and
// that approval passed none of the gates the approval as the operator does.
// Now the engine decides at dispatch the gates that need no review (a
// draft, a muted PR, one its watch would not review on its own, one that
// changes its own workflows or actions or what steers the review agents,
// or whose files magnum cannot list: RoundInput.NoApproval) and, through
// RoundInput.CIGate, the CI gate when the judge's prompt is written, from
// the registry read failing-checks.json comes from: the judge and the gate
// see one CI. A refusal makes the no-findings event COMMENT, which the
// prompt's no_findings_event and its post_review line's --no-findings-event
// both render from (post-review then refuses an APPROVE), and the prompt
// says why in fixed words.

import (
	"context"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// approveEvent is the no-findings event of a round that approves a clean
// review.
const approveEvent = "APPROVE"

// The CI states the approval's CI gate decides by (CIGateResult.State),
// which the round's events record (round.judge's ci_gate, the engine's
// round.approval_refused): every check that counts passed or skipped, or
// none ran (CIGreen); one still runs (CIRunning, which lets the APPROVE
// through: the one risk the gate's moment accepts); every failing one
// fails the same way on the base branch's tip (CIRedLikeBase, through);
// one fails otherwise, or magnum cannot tell (CIRed, refused); the
// registry holds the checks of a newer head than the round's target, or no
// rollup of the PR was ever read (CIUnknown, through).
const (
	CIGreen       = "green"
	CIRunning     = "running"
	CIRedLikeBase = "red-like-base"
	CIRed         = "red"
	CIUnknown     = "unknown"
)

// CIGateResult is what the approval's CI gate (RoundInput.CIGate) says of
// the round's target when the judge's prompt is written: Refusal, why the
// round cannot approve in a few fixed words ("" = it may), and State, the
// CI state it decided by.
type CIGateResult struct {
	Refusal string
	State   string
}

// registryPR is the round's PR as the registry has it now (the round's
// input when there is no registry or the read fails): the judge prompt's
// failing checks and the approval's CI gate read its checks once.
func (rd *round) registryPR(ctx context.Context) store.PR {
	pr := rd.in.PR
	if rd.r.Store == nil {
		return pr
	}
	if cur, err := rd.r.Store.PRByID(ctx, pr.ID); err == nil {
		return cur
	}
	return pr
}

// applyCIGate asks the approval's CI gate (RoundInput.CIGate) about pr, as
// the registry has it for the judge prompt about to go out, while the
// round would approve a clean review (jd's no-findings event is still
// APPROVE): a refusal makes the event COMMENT and says why in the prompt
// (jd.NoApproval). It returns the CI state the gate decided by, "" when it
// was not asked (no gate, a blind replay, a round that comments anyway).
func (rd *round) applyCIGate(ctx context.Context, jd *agents.JudgeData, pr store.PR) string {
	if rd.in.CIGate == nil || rd.in.Blind || jd.NoFindingsEvent != approveEvent {
		return ""
	}
	g := rd.in.CIGate(ctx, pr, rd.in.TargetSHA)
	if g.Refusal != "" {
		jd.NoFindingsEvent, jd.NoApproval = noApproveEvent, g.Refusal
	}
	return g.State
}
