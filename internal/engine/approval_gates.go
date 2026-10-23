package engine

// The judge's own approval passes the gates of the approval as the operator
// (DECISIONS "The judge's own approval passes the approval's gates", spec
// 09 of the 2026-10-22 run). Where the posting identity's no-findings
// event is APPROVE (a gh identity's default, or a [[repo]] override), a
// clean review approves, and that approval passed none of the gates
// autoapprove_gates.go holds the approval as the operator to: on
// talkable#12056 the App's approval stood on a PR GitHub still called
// REVIEW_REQUIRED, and on a PR that changes its own workflows, carries red
// CI or is a draft such an approval is a misleading signal, or a real
// approval for an install whose judge posts as a gh login. Now a round
// that would approve a clean review (judgeApproves; never a post-merge
// review, an eval replay, a dry run or the operator's own PR, which
// comment anyway) is held to those gates by the same functions: at its
// dispatch the ones that need no review (a draft, a muted PR, one its
// watch would not review on its own, one that changes its own workflows or
// actions or what steers the review agents, or whose files magnum cannot
// list: approvalGatesDue, pipeline.RoundInput.NoApproval; functions of the
// registry, so every kind of round decides them again, a continue too),
// and the CI gate when the judge's prompt is written (judgeCIGate,
// pipeline.RoundInput.CIGate, a continue's prompt again), from the
// registry read the judge's
// failing-checks.json comes from: red CI that does not fail the way the
// base branch's tip fails refuses, checks that still run or pass let the
// APPROVE through. A refusal makes the no-findings event COMMENT, in the
// prompt and in its post_review line (post-review refuses an APPROVE), and
// one round.approval_refused event per head and reason says why, in the
// prompt's fixed words, never PR text; the CI gate's carries the CI state
// it decided by, and the judge's round.judge event records it whenever it
// was asked.

import (
	"context"
	"fmt"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// kvPRJudgeApprovalRefused records "<head>|<reason>" of the PR's latest
// round.approval_refused event (noteApprovalRefused), so the event is
// written once per head and reason, also across a restart.
func kvPRJudgeApprovalRefused(prID int64) string {
	return fmt.Sprintf("pr.%d.judge_approval_refused", prID)
}

// ciFailsJudge is the CI gate's refusal in the judge's prompt and its event.
const ciFailsJudge = "the head's checks fail"

// approvalGated reports whether the round rs sets up for job is held to
// the approval's gates: its judge would approve a clean review
// (judgeApproves), and it is no post-merge review, eval replay or dry run,
// nor a round of the operator's own PR (self-authored: its judge comments
// anyway), none of which posts an approval.
func (e *Engine) approvalGated(job *roundJob, rs *roundSetup, dryRun bool) bool {
	return !job.postMerge && job.evalHead == "" && !dryRun && !e.ownPR(job.pr) && e.judgeApproves(job, rs)
}

// approvalGatesDue says, in the judge prompt's fixed words, why the round
// rs sets up for job cannot approve by the approval's gates that need no
// review, decided at its dispatch on the PR as dispatch read it ("" = they
// let it): a draft or muted PR (prStateRefusal), one its watch would not
// review on its own (watchRefusal: a forced round's), one that changes
// what steers the review agents or its own workflows or actions, or whose
// head's files magnum cannot list (changedFilesRefusal). The CI gate
// decides later, when the judge's prompt is written (judgeCIGate).
func (e *Engine) approvalGatesDue(ctx context.Context, job *roundJob, rs *roundSetup, dryRun bool) string {
	if !e.approvalGated(job, rs, dryRun) {
		return ""
	}
	if why, short := prStateRefusal(job.pr); why != "" {
		return short
	}
	c := store.RepoPR{PR: job.pr, Repo: job.repo.FullName()}
	if why, short := e.watchRefusal(ctx, c); why != "" {
		return short
	}
	_, short := e.changedFilesRefusal(ctx, job.pr)
	return short
}

// planApprovalGates settles the approval's gates of the round rs sets up
// for job: the dispatch gates (approvalGatesDue) are decided now, for every
// kind of round, a continue and one that began as a continue and became a
// fresh round too (they read the registry, which a paused round's answer
// would only copy), with a round.approval_refused event when they refuse
// (once per head and reason, noteApprovalRefused); a round held to the
// gates asks the CI gate when its judge's prompt is written (job.ciGated),
// a continue's again.
func (e *Engine) planApprovalGates(ctx context.Context, job *roundJob, rs *roundSetup, dryRun bool) {
	job.ciGated = e.approvalGated(job, rs, dryRun)
	job.noApproval = e.approvalGatesDue(ctx, job, rs, dryRun)
	if job.noApproval != "" {
		e.noteApprovalRefused(ctx, job.repo, job.pr, rs.target, job.noApproval, "")
	}
}

// judgeCIGate is the CI gate of job's round (pipeline.RoundInput.CIGate),
// which its judge asks when its prompt is written, with the PR as the
// registry has it then and the round's target: the gate of the approval as
// the operator (ciGateOf, the same answer, recorded nowhere else), whose
// refusal (the target's checks fail, other than the way the base branch's
// tip fails them) it records once per head (noteApprovalRefused, with the
// CI state); checks that still run or pass, and checks that fail as the
// base tip's do, let the APPROVE through. A registry whose PR moved past
// the target holds a newer head's checks: CIUnknown, through.
func (e *Engine) judgeCIGate(job *roundJob) func(context.Context, store.PR, string) pipeline.CIGateResult {
	repo := job.repo
	return func(ctx context.Context, pr store.PR, target string) pipeline.CIGateResult {
		if pr.HeadSHA != target {
			return pipeline.CIGateResult{State: pipeline.CIUnknown}
		}
		ci := e.ciGateOf(ctx, store.RepoPR{PR: pr, Repo: repo.FullName()})
		if ci.why == "" || ci.waits {
			return pipeline.CIGateResult{State: ci.state}
		}
		e.noteApprovalRefused(ctx, repo, pr, target, ciFailsJudge, ci.state)
		return pipeline.CIGateResult{Refusal: ciFailsJudge, State: ci.state}
	}
}

// noteApprovalRefused writes one round.approval_refused event for pr's
// head and why (the judge prompt's fixed words, never PR text), once per
// head and reason (kvPRJudgeApprovalRefused); ci, the CI state the CI gate
// decided by, is in its data when the CI gate refused.
func (e *Engine) noteApprovalRefused(ctx context.Context, repo store.Repo, pr store.PR, head, why, ci string) {
	key, mark := kvPRJudgeApprovalRefused(pr.ID), head+"|"+why
	if v, _ := e.getKV(ctx, key); v == mark {
		return
	}
	e.setKV(ctx, key, mark)
	data := map[string]any{"reason": why, "head_sha": head}
	if ci != "" {
		data["ci"] = ci
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "round.approval_refused",
		fmt.Sprintf("the judge does not approve %s#%d: %s; a clean verdict posts as a comment", repo.FullName(), pr.Number, why), data)
}

// settleNoApprovalReview keeps, for the needs-you list, whether the review
// a round just posted (res.ReviewID) is one the approval's gates turned
// from an approval into a comment (res.NoApproval, the pipeline's: the
// dispatch gates' or the CI gate's words): store.KVPRNoApprovalReview
// names it, and the list takes it, when clean, for an approval only a
// person can give ("approve by hand: ..."). Any other review magnum posts
// drops the record.
func (e *Engine) settleNoApprovalReview(ctx context.Context, pr store.PR, res pipeline.RoundResult) {
	if res.ReviewID == 0 {
		return
	}
	if res.NoApproval == "" {
		e.delKV(ctx, store.KVPRNoApprovalReview(pr.ID))
		return
	}
	e.setKV(ctx, store.KVPRNoApprovalReview(pr.ID), store.NoApprovalReview{ReviewID: res.ReviewID, Reason: res.NoApproval}.Value())
}
