package engine

// Post-merge review: `magnum review` of a PR GitHub merged before magnum
// reviewed its last push. The PR leaves closed (or released) for its
// claimable state, forced, so the dispatcher runs it like any forced round
// (store.Candidates takes a MERGED PR only when forced); the round is a
// re-review from reviewed_sha to the merged head (a first review when there
// is none) on refs/pull/N/head, which GitHub keeps after the merge. Every
// end of the round puts the PR back in closed with a fresh close grace, so
// the normal release follows: a posted review (onPosted), a dry run
// (onDryRun), a failure that would park an open PR in needs_attention
// (postMergeFailed) and an abort (settleStopped). Retries and pauses keep the
// PR forced in line, as for an open PR. No column marks the round: it is
// post-merge whenever GitHub merged the PR (postMerge), at dispatch
// (roundJob.postMerge) and after.

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// postMerge reports whether pr's round is a post-merge review: GitHub
// merged it.
func postMerge(pr store.PR) bool { return pr.GHState == store.GHMerged }

// postMergeRoundStates are the states of a PR GitHub merged whose post-merge
// round is due or running: the dispatcher holds a merged PR in them only while
// it is forced, so a mute must leave the forced mark alone (requestMute). A
// post-merge round that fails returns the PR to closed, never to
// needs_attention, so that state is not one of them.
var postMergeRoundStates = []string{store.PRQueued, store.PRRereviewPending, store.PRClaiming,
	store.PRReviewing, store.PRVerifying, store.PRPaused}

// postMergeRefusal is why `magnum review` of pr, which GitHub merged, is
// refused ("" = it is not): its merged head was reviewed already, or cleanup
// is releasing its checkout (releasing). A closed PR needs no such check:
// cleanup moves it closed → releasing before its first side effect, so the
// request's own closed → claimable compare-and-set and that one never both
// succeed.
func postMergeRefusal(label string, pr store.PR) string {
	switch {
	case deref(pr.ReviewedSHA) == pr.HeadSHA:
		return fmt.Sprintf("%s: its merged head %s was already reviewed", label, textx.ShortSHA(pr.HeadSHA))
	case pr.State == store.PRReleasing:
		return label + ": its checkout is being released; run `magnum review` again in a minute"
	}
	return ""
}

// postMergeScope is what a post-merge review of pr covers, for the
// request's answer: "post-merge review 1111111 → 2222222 (comment only)", or
// "of the merged head 2222222" without an earlier review.
func postMergeScope(pr store.PR) string {
	if rs := deref(pr.ReviewedSHA); rs != "" {
		return fmt.Sprintf("post-merge review %s → %s (comment only)", textx.ShortSHA(rs), textx.ShortSHA(pr.HeadSHA))
	}
	return fmt.Sprintf("post-merge review of the merged head %s (comment only)", textx.ShortSHA(pr.HeadSHA))
}

// postMergeBase is the commit a post-merge round of job reviews from:
// target's merge base with the base branch as it was before the merge, the
// first parent of the PR's merge commit (GitHub's mergeCommit: the merge
// commit, the squash commit or the last rebased commit; fetched by id when
// the clone lacks it). origin/<base> holds the merged head after a
// merge-commit merge, so its merge base with target is target itself and
// every reviewer's diff would be empty. Without a usable merge commit (GitHub
// does not say, it is target itself after a fast-forward, or it cannot be
// read), the base tip the PR's last Details fetch recorded (base_sha) stands
// in; "" when neither works (an event says so).
func (e *Engine) postMergeBase(ctx context.Context, job *roundJob, target string) string {
	if e.d.Git == nil {
		return ""
	}
	dir, subject := job.slot.Path, prSubject(job.repo, job.pr.Number)
	note := func(base, from string) string {
		e.event(ctx, "info", subject, "round.post_merge_base", fmt.Sprintf("post-merge round reviews %s..%s (%s)", textx.ShortSHA(base), textx.ShortSHA(target), from),
			map[string]any{"base_sha": base, "target_sha": target, "from": from})
		return base
	}
	if mc := e.mergeCommit(ctx, job); mc != "" && mc != target {
		if _, err := e.d.Git.RevParse(ctx, dir, mc); err != nil {
			if clone := cmp.Or(job.slot.MainClone, deref(job.repo.ClonePath)); clone != "" {
				if err := e.d.Git.FetchCommit(ctx, clone, mc, 0); err != nil {
					e.log.Info("post-merge base: fetch the merge commit", "subject", subject, "commit", textx.ShortSHA(mc), "err", err)
				}
			}
		}
		if parent, err := e.d.Git.RevParse(ctx, dir, mc+"^1"); err == nil {
			if mb, err := e.d.Git.MergeBase(ctx, dir, parent, target); err == nil {
				return note(mb, "the base before merge commit "+textx.ShortSHA(mc))
			}
		}
	}
	if b := deref(job.pr.BaseSHA); b != "" {
		if mb, err := e.d.Git.MergeBase(ctx, dir, b, target); err == nil {
			return note(mb, "the recorded base tip "+textx.ShortSHA(b))
		}
	}
	e.event(ctx, "warn", subject, "round.post_merge_base",
		fmt.Sprintf("post-merge round: no base before the merge found for %s; the reviewers compare with the base branch", textx.ShortSHA(target)), nil)
	return ""
}

// mergeCommit is the merge commit GitHub reports for job's merged PR, read
// as the watch's poll identity ("" when it cannot say, or the answer is not
// a full commit id).
func (e *Engine) mergeCommit(ctx context.Context, job *roundJob) string {
	gh := e.gh(job.watch.PollIdentity)
	if gh == nil {
		return ""
	}
	states, _, err := gh.ConfirmStates(ctx, job.repo.Owner, job.repo.Name, []int{job.pr.Number})
	if err != nil {
		e.log.Info("post-merge base: merge commit", "subject", prSubject(job.repo, job.pr.Number), "err", err)
		return ""
	}
	if mc := states[job.pr.Number].MergeCommitOid; isCommitID(mc) {
		return mc
	}
	return ""
}

// isCommitID reports whether s is a full commit id (SHA-1 or SHA-256 hex).
func isCommitID(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
}

// releaseAfter is when the PR of a post-merge round that ended now, back in
// closed, is released: after a fresh close grace, so its panes stay that
// long for a look at the round.
func (e *Engine) releaseAfter() time.Time { return e.now().Add(e.cfg.Daemon.CloseGrace.Duration) }

// postMergeFailed ends a post-merge round that could not post where an open
// PR would need attention: needs_attention would keep a merged PR from its
// release forever, so the PR goes back to closed with the reason kept
// (last_error), a warning and one toast. `magnum review` tries again.
func (e *Engine) postMergeFailed(ctx context.Context, job *roundJob, pr store.PR, from []string, why, msg string, extra func(*store.PRUpdate)) {
	err := e.st.TransitionPR(ctx, pr.ID, from, store.PRClosed, func(u *store.PRUpdate) {
		u.Set("forced", false)
		u.Set("next_attempt_at", nil)
		u.Set("release_after", e.releaseAfter())
		u.Set("last_error", msg)
		if extra != nil {
			extra(u)
		}
	})
	if err != nil {
		e.log.Info("PR moved on during the post-merge round", "pr", pr.ID, "err", err)
		return
	}
	e.delKV(ctx, kvPRDryRun(pr.ID), kvPRRedecide(pr.ID)) // the forced request ended with it
	e.clearRequested(ctx, pr.ID)
	if job.hasSlo {
		_ = e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotClaimed, store.SlotBusy}, store.SlotHeld, nil)
	}
	label := fmt.Sprintf("%s#%d", job.repo.Name, pr.Number)
	reason := attention.Explain(why, msg, label)
	e.event(ctx, "warn", prSubject(job.repo, pr.Number), "pr.post_merge_failed",
		fmt.Sprintf("post-merge review not posted (%s: %s); closed again, slot released after %s", why, msg, e.cfg.Daemon.CloseGrace.Duration),
		map[string]any{"stage": reason.Stage, "cause": reason.Cause, "why": why})
	e.urgent(fmt.Sprintf("post-merge:%d:%s", pr.ID, why), "magnum: "+label+" post-merge review failed", reason.Summary, attentionWindow)
}
