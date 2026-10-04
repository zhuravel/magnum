package engine

// An App approval follows the head (backlog 7): one watched repository
// needs a single approval and keeps approvals across pushes, and 24 of 50
// approved merges there in 30 days landed commits newer than the approval.
// When a PR whose last magnum review by an App identity approved it gets a
// head with new commits, magnum dismisses that approval before the
// re-review is queued; the re-review posts the verdict for the new head.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// ApprovalDismissMessage is the reason a dismissed approval shows on GitHub.
const ApprovalDismissMessage = "magnum: new commits since this approval; a re-review follows"

// reviewDismissed is what last_review_event records once magnum dismissed
// the review (GitHub's state for it).
const reviewDismissed = "DISMISSED"

// kvApprovalKept records "<review id>@<head>" when the approval was kept
// because that head has no commits the approval did not see; kvApprovalRefused
// the review id GitHub refused to dismiss (403). Neither is asked again.
func kvApprovalKept(prID int64) string    { return fmt.Sprintf("pr.%d.approval_kept", prID) }
func kvApprovalRefused(prID int64) string { return fmt.Sprintf("pr.%d.approval_refused", prID) }

// isApproval reports whether a recorded review event approved the PR.
func isApproval(event string) bool {
	switch strings.ToUpper(strings.TrimSpace(event)) {
	case "APPROVED", "APPROVE":
		return true
	}
	return false
}

// approvalIdentity is the App identity that posted pr's last review: the
// configured App whose login posted it (last_review_login), else the PR's
// identity when that is an App and no login was recorded. nil when the
// review is not an App's.
func (e *Engine) approvalIdentity(pr store.PR) *config.Identity {
	login := deref(pr.LastReviewLogin)
	if login == "" {
		if id := e.cfg.IdentityByName(pr.Identity); id != nil && id.Kind == "app" {
			return id
		}
		return nil
	}
	for i := range e.cfg.Identities {
		if id := &e.cfg.Identities[i]; id.Kind == "app" && github.SameLogin(id.Login, login) {
			return id
		}
	}
	return nil
}

// followApproval dismisses the approval pr's App identity posted on its
// reviewed commit once the head carries commits that approval did not see
// (GitHub's compare of the two; when it cannot tell, the approval goes).
// It leaves alone: other logins' reviews, a review that did not approve,
// keep_approvals ([[repo]] or [[watch]]), a head with no new commits (an
// ancestor of the reviewed commit, recorded so it is not compared again)
// and a review GitHub already refused to dismiss (403, reported once). A
// dismissed review is recorded as DISMISSED with a review.approval_dismissed
// event; any other failure is retried at the next poll. Dry runs change
// nothing.
func (e *Engine) followApproval(ctx context.Context, repo store.Repo, pr store.PR) {
	reviewID, reviewed := deref(pr.LastReviewID), deref(pr.ReviewedSHA)
	if e.d.DryRun || e.d.GitHub == nil || reviewID == 0 || reviewed == "" || reviewed == pr.HeadSHA ||
		!isApproval(deref(pr.LastReviewEvent)) {
		return
	}
	id := e.approvalIdentity(pr)
	full := repo.FullName()
	if id == nil || e.cfg.KeepApprovals(full) {
		return
	}
	idStr := strconv.FormatInt(reviewID, 10)
	if v, ok := e.getKV(ctx, kvApprovalRefused(pr.ID)); ok && v == idStr {
		return
	}
	kept := idStr + "@" + pr.HeadSHA
	if v, ok := e.getKV(ctx, kvApprovalKept(pr.ID)); ok && v == kept {
		return
	}
	gh := e.d.GitHub(id.Name)
	if gh == nil {
		return
	}
	subject := prSubject(repo, pr.Number)
	data := map[string]any{"review_id": reviewID, "reviewed_sha": reviewed, "head_sha": pr.HeadSHA, "identity": id.Name}
	cs, cerr := gh.Compare(ctx, repo.Owner, repo.Name, reviewed, pr.HeadSHA)
	switch {
	case cerr == nil && cs.Commits == 0:
		e.setKV(ctx, kvApprovalKept(pr.ID), kept)
		e.event(ctx, "info", subject, "review.approval_kept",
			fmt.Sprintf("approval %d kept: head %s has no commits since %s", reviewID, short(pr.HeadSHA), short(reviewed)), data)
		return
	case cerr != nil:
		if ctx.Err() != nil {
			return
		}
		e.log.Info("approval: compare failed; dismissing it anyway", "pr", pr.ID, "err", cerr)
	}
	err := gh.DismissReview(ctx, repo.Owner, repo.Name, pr.Number, reviewID, ApprovalDismissMessage)
	switch {
	case err == nil:
		uerr := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
			u.Where("last_review_id", reviewID)
			u.Set("last_review_event", reviewDismissed)
		})
		if uerr != nil && !errors.Is(uerr, store.ErrConflict) {
			e.log.Warn("approval dismissed but not recorded", "pr", pr.ID, "err", uerr)
		}
		what := "new commits"
		if cerr == nil {
			what = fmt.Sprintf("%d new commit%s", cs.Commits, map[bool]string{true: "", false: "s"}[cs.Commits == 1])
		}
		e.event(ctx, "info", subject, "review.approval_dismissed",
			fmt.Sprintf("dismissed approval %d by %s on %s: %s up to %s; a re-review follows", reviewID, id.Login, short(reviewed), what, short(pr.HeadSHA)), data)
	case errors.Is(err, github.ErrForbidden):
		e.setKV(ctx, kvApprovalRefused(pr.ID), idStr)
		e.event(ctx, "warn", subject, "review.approval_dismiss_refused",
			fmt.Sprintf("could not dismiss approval %d by %s (missing permission; not retried): %v", reviewID, id.Login, err), data)
	default:
		if ctx.Err() == nil {
			e.log.Warn("approval: dismiss failed; retrying at the next poll", "pr", pr.ID, "review_id", reviewID, "err", err)
		}
	}
}
