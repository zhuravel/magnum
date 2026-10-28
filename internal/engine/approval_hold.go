package engine

// Approval holds (DECISIONS "An approval rests on a whole-PR read"): a
// re-review reads the commits since the last review, and when its verdict
// is clean the judge posts APPROVE, so after several re-reviews an approval
// rests on one whole read long ago and a chain of deltas, each read on its
// own. On talkable#11483 magnum approved after a delta re-review of a fix,
// and a colleague's whole-branch round then found a second path to the
// same defect. So a re-review of a PR that had [pipeline]
// approve_after_full_pass re-reviews or more since its baseline (its last
// full pass, else its first review: rereviewsSinceFullPass) cannot approve
// (pipeline.RoundInput.HoldApproval): its no-findings event is COMMENT, and
// its prompt asks a clean verdict for the body line "A full pass of the
// whole PR follows before approval.". When such a review posts with the
// verdict clean, the PR is forced into a full pass of the same head at once
// (store.KVPRFresh = FreshFullPass, as `magnum review --full` does;
// fullPassDue names its rule fullPassApproval), every role fresh on the
// whole diff, whose clean verdict approves and which is the PR's new
// baseline. The hold applies to automatic and requested rounds alike (a
// delta check, a recovery, an audit pass, a re-review of the same head); a
// full pass, a post-merge review, a reply round, a CI check, a completion
// (complete.go: it completes a review that did not hold, which the
// re-reviews since the baseline now count), a dry run and an eval replay
// never hold, nor does a round in a repository whose clean
// review cannot lead to an approval (approvalPath: it comments anyway and
// nothing approves as the operator). Auto-approval as the operator waits
// for the full pass too (autoGateRefusal), and the status comment's posted
// line says that it follows.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVPRApprovalHold holds the PR's held review whose full pass follows
// (heldReview as JSON); any later review that posts deletes it.
func KVPRApprovalHold(prID int64) string { return fmt.Sprintf("pr.%d.approval_hold", prID) }

// kvPRApprovalHoldRound holds why the PR's round in flight cannot approve
// (approvalHoldDue), so a continue of a paused one holds too; the round's
// end deletes it, a pause keeps it for the continue.
func kvPRApprovalHoldRound(prID int64) string {
	return fmt.Sprintf("pr.%d.approval_hold_round", prID)
}

// fullPassApproval is the rule of the full pass that follows a held review
// (fullPassWhy.rule).
const fullPassApproval = "approval"

// approvalBy names, in the pr.forced event, who forces the full pass after
// a held review (forceReview): no act of the operator's.
const approvalBy = "magnum (approve_after_full_pass)"

// approvalHoldRefusal is why auto-approval waits on a held review
// (autoGateRefusal; the card's words).
const approvalHoldRefusal = "its review held the approval: a full pass of the whole PR follows before approval"

// heldReview is a review that held its approval (KVPRApprovalHold): its
// review, judge run, round and target, and why it held.
type heldReview struct {
	ReviewID int64     `json:"review_id"`
	Run      string    `json:"run"`
	Round    int       `json:"round"`
	Target   string    `json:"target"`
	Why      string    `json:"why"`
	At       time.Time `json:"at"`
}

// reason is why the full pass after h runs (round.full_pass, the round's
// start event).
func (h heldReview) reason() string {
	return fmt.Sprintf("before approval: the clean re-review of %s in round %d posted as a comment after %s",
		textx.ShortSHA(h.Target), h.Round, h.Why)
}

// approvalHoldDue says why the round rs sets up for job cannot approve ("" =
// it may): approve_after_full_pass is set, the round is a re-review (a
// delta check, a recovery, an audit pass, a re-review of the same head, a
// round someone asked for), not a full pass, a post-merge review, an eval
// replay, a dry run, a reply round, a CI check or a completion, the repository has an
// approval to hold (approvalPath), and the PR had that many re-reviews or
// more since its last full pass, else its first review
// (rereviewsSinceFullPass).
func (e *Engine) approvalHoldDue(ctx context.Context, job *roundJob, rs *roundSetup, dryRun bool) string {
	after := e.cfg.Pipeline.ApproveAfterFullPass
	switch {
	case after <= 0, job.kind != pipeline.KindRereview, rs.fullPass, job.postMerge, job.evalHead != "", dryRun, rs.replies > 0, rs.ciRed,
		rs.complete != nil, !e.approvalPath(job, rs):
		return ""
	}
	n, last := e.rereviewsSinceFullPass(ctx, job.pr.ID)
	if n < after {
		return ""
	}
	since := "the first review"
	if last.Round > 0 {
		since = fmt.Sprintf("the full pass of round %d", last.Round)
	}
	return fmt.Sprintf("%s since %s (approve_after_full_pass %d)", textx.Count(n, "re-review", "re-reviews"), since, after)
}

// approvalPath reports whether a clean review of job's PR can lead to an
// approval: the judge's no-findings event there is APPROVE
// (judgeApproves), or magnum approves the repository's PRs as the operator
// (config.Config.AutoApproveFor). Without either a clean verdict posts as
// a comment anyway, and a hold would only add a full pass that cannot
// approve and a false line to the review.
func (e *Engine) approvalPath(job *roundJob, rs *roundSetup) bool {
	return e.cfg.AutoApproveFor(job.repo.FullName()) != nil || e.judgeApproves(job, rs)
}

// judgeApproves reports whether the judge of job's round posts a clean
// review as an APPROVE (pipeline.JudgeEvents: the [[repo]] override, else
// the posting identity's, COMMENT for an author the auto-approval lists
// refuse).
func (e *Engine) judgeApproves(job *roundJob, rs *roundSetup) bool {
	id := e.cfg.IdentityByName(job.pr.Identity)
	if id == nil && rs.src != nil { // the round runner's fallback (pipeline Runner.identityConfig)
		id = &config.Identity{Name: rs.src.Name(), Kind: rs.src.Kind(), Login: rs.src.Login()}
	}
	nf, _ := pipeline.JudgeEvents(e.cfg, job.repo.FullName(), id, deref(job.pr.AuthorLogin), false)
	return nf == "APPROVE"
}

// planApprovalHold settles whether the round rs sets up for job holds its
// approval (job.approvalHold): a continue goes on as the paused round
// decided (kvPRApprovalHoldRound), any other round decides now
// (approvalHoldDue) and records it, with a round.approval_hold event when
// it holds.
func (e *Engine) planApprovalHold(ctx context.Context, job *roundJob, rs *roundSetup, dryRun bool) {
	pr := job.pr
	if job.kind == kindContinue {
		job.approvalHold, _ = e.getKV(ctx, kvPRApprovalHoldRound(pr.ID))
	} else {
		job.approvalHold = e.approvalHoldDue(ctx, job, rs, dryRun)
	}
	if rs.kind == kindContinue {
		return // the paused round's record stands
	}
	if job.approvalHold == "" {
		e.delKV(ctx, kvPRApprovalHoldRound(pr.ID))
		return
	}
	e.setKV(ctx, kvPRApprovalHoldRound(pr.ID), job.approvalHold)
	if job.kind != kindContinue {
		e.event(ctx, "info", prSubject(job.repo, pr.Number), "round.approval_hold",
			"approval held: "+job.approvalHold+": a clean verdict posts as a comment, and a full pass of the whole PR follows before the approval",
			map[string]any{"reason": job.approvalHold, "approve_after_full_pass": e.cfg.Pipeline.ApproveAfterFullPass})
	}
}

// settleApprovalHold keeps a review that posted (onPosted) for the hold: a
// round that held its approval and whose verdict is clean records the held
// review (KVPRApprovalHold) and forces a full pass of the PR at once, as
// `magnum review --full` does (forceReview), which the next dispatch runs
// with every role fresh on the whole diff (fullPassDue: fullPassApproval);
// any other review drops the record, the full pass's included.
func (e *Engine) settleApprovalHold(ctx context.Context, job *roundJob, pr store.PR, in pipeline.RoundInput, res pipeline.RoundResult) {
	if !in.HoldApproval || job.postMerge || !e.postedClean(ctx, pr.ID, res.ReviewID) {
		e.delKV(ctx, KVPRApprovalHold(pr.ID))
		return
	}
	cur, err := e.st.PRByID(ctx, pr.ID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "approval hold: the PR after its review", "pr", pr.ID)
		return
	}
	if cur.GHState != store.GHOpen || !slices.Contains([]string{store.PRReviewed, store.PRRereviewPending, store.PRQueued}, cur.State) {
		e.log.Info("approval hold: no full pass follows", "pr", pr.ID, "state", cur.State, "gh_state", cur.GHState)
		e.delKV(ctx, KVPRApprovalHold(pr.ID))
		return
	}
	held := heldReview{ReviewID: res.ReviewID, Run: res.JudgeRunID, Round: res.Round, Target: in.TargetSHA, Why: job.approvalHold, At: e.now().UTC()}
	b, err := json.Marshal(held)
	if err != nil {
		return
	}
	e.setKV(ctx, KVPRApprovalHold(pr.ID), string(b))
	subject := prSubject(job.repo, pr.Number)
	if _, err := e.forceReview(ctx, job.repo, cur, ReviewPayload{Full: true}, nil, approvalBy); err != nil {
		e.event(ctx, "warn", subject, "pr.approval_full_pass_failed", "the full pass after the held review could not be queued: "+err.Error(),
			map[string]any{"review_id": res.ReviewID})
		return
	}
	e.event(ctx, "info", subject, "pr.approval_full_pass",
		fmt.Sprintf("the clean re-review of %s posted as a comment; a full pass of the whole PR on %s follows before approval",
			textx.ShortSHA(in.TargetSHA), textx.ShortSHA(cur.HeadSHA)),
		map[string]any{"review_id": res.ReviewID, "target_sha": in.TargetSHA, "head_sha": cur.HeadSHA})
}

// postedClean reports whether review reviewID, which a round of PR prID
// just posted, is its latest and has the verdict clean (the judge's result
// the round stored: store.ReviewSummary).
func (e *Engine) postedClean(ctx context.Context, prID, reviewID int64) bool {
	if reviewID == 0 {
		return false
	}
	sums, err := e.st.LastReviewSummaries(ctx, []int64{prID})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "approval hold: review summary", "pr", prID)
		return false
	}
	s, ok := sums[prID]
	return ok && s.ReviewID == reviewID && s.Verdict == store.VerdictClean
}

// heldReviewOf is PR prID's held review (KVPRApprovalHold) when it is
// review reviewID (the PR's last one, for the callers); ok is false
// otherwise.
func (e *Engine) heldReviewOf(ctx context.Context, prID, reviewID int64) (heldReview, bool) {
	v, ok := e.getKV(ctx, KVPRApprovalHold(prID))
	h, parsed := parseHeldReview(v)
	if !ok || !parsed || h.ReviewID != reviewID {
		return heldReview{}, false
	}
	return h, true
}

// HeldReviewID is the review a KVPRApprovalHold value names: the one whose
// approval waits for the full pass after it, while it is the PR's latest
// (heldReviewOf's rule, which the board's FINDINGS follows: a later review
// drops the record, and one left behind names an older review). ok is
// false for a value that names none.
func HeldReviewID(v string) (int64, bool) {
	h, ok := parseHeldReview(v)
	return h.ReviewID, ok
}

// parseHeldReview reads a KVPRApprovalHold value; ok is false for one that
// does not parse or names no review.
func parseHeldReview(v string) (heldReview, bool) {
	var h heldReview
	if v == "" || json.Unmarshal([]byte(v), &h) != nil || h.ReviewID == 0 {
		return heldReview{}, false
	}
	return h, true
}

// approvalPassDue reports whether pr's last review held its approval and
// its full pass is still asked for (store.KVPRFresh): the status comment's
// posted line says that it follows.
func (e *Engine) approvalPassDue(ctx context.Context, pr store.PR) bool {
	if _, ok := e.heldReviewOf(ctx, pr.ID, deref(pr.LastReviewID)); !ok {
		return false
	}
	v, _ := e.getKV(ctx, store.KVPRFresh(pr.ID))
	return v == store.FreshFullPass
}
