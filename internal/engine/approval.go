package engine

// An App approval follows the head (backlog 7): one watched repository
// needs a single approval and keeps approvals across pushes, and 24 of 50
// approved merges there in 30 days landed commits newer than the approval.
// When a PR whose last magnum review by an App identity approved it gets a
// head with new commits, magnum dismisses that approval before the
// re-review is queued; the re-review posts the verdict for the new head.
// A small delta that gets a delta check keeps the approval until the check
// posts (keepForDeltaCheck).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
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
// PR's identity when it is an App with the login that posted it
// (last_review_login), else the first configured App with that login (one
// App installed on several owners has an identity per installation, all
// with one login), else the PR's identity when that is an App and no login
// was recorded. nil when the review is not an App's: a user named like the
// App ("zhuravel", the App "zhuravel[bot]") is not it.
func (e *Engine) approvalIdentity(pr store.PR) *config.Identity {
	login := deref(pr.LastReviewLogin)
	own := e.cfg.IdentityByName(pr.Identity)
	if own != nil && own.Kind != "app" {
		own = nil
	}
	if login == "" {
		return own
	}
	if own != nil && github.SameAccount(own.Login, login) {
		return own
	}
	for i := range e.cfg.Identities {
		if id := &e.cfg.Identities[i]; id.Kind == "app" && github.SameAccount(id.Login, login) {
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
// ancestor of the reviewed commit, recorded so it is not compared again),
// a review GitHub already refused to dismiss (403, reported once) and an
// approval kept for a delta check (keepForDeltaCheck). A dismissed review
// is recorded as DISMISSED with a review.approval_dismissed event; any other
// failure is retried at the next poll. Dry runs change nothing.
func (e *Engine) followApproval(ctx context.Context, repo store.Repo, pr store.PR) {
	if p, ok := e.pendingApproval(ctx, pr.ID); ok && p.Dismiss != "" {
		e.dismissKept(ctx, repo, pr, p, p.Dismiss) // a dismissal a round's end asked for and GitHub did not answer
		if p.ReviewID == deref(pr.LastReviewID) {
			return
		}
	}
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
	gh := e.gh(id.Name)
	if gh == nil {
		return
	}
	keep, why := e.keepForDeltaCheck(ctx, repo, pr, id)
	if keep {
		return
	}
	subject := prSubject(repo, pr.Number)
	data := map[string]any{"review_id": reviewID, "reviewed_sha": reviewed, "head_sha": pr.HeadSHA, "identity": id.Name}
	cs, cerr := e.compareStats(ctx, gh, repo, reviewed, pr.HeadSHA) // the trivial-delta check's comparison, when it made one
	switch {
	case cerr == nil && cs.Commits == 0:
		e.setKV(ctx, kvApprovalKept(pr.ID), kept)
		e.delKV(ctx, kvApprovalPending(pr.ID)) // it covers the head: no check is due for it
		e.event(ctx, "info", subject, "review.approval_kept",
			fmt.Sprintf("approval %d kept: head %s has no commits since %s", reviewID, textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(reviewed)), data)
		return
	case cerr != nil:
		if ctx.Err() != nil {
			return
		}
		e.log.Info("approval: compare failed; dismissing it anyway", "pr", pr.ID, "err", cerr)
	}
	what := "new commits"
	if cerr == nil {
		what = textx.Count(cs.Commits, "new commit", "new commits")
	}
	e.dismissApproval(ctx, repo, pr, gh, id, reviewID, reviewed, what, why, data)
}

// dismissApproval dismisses reviewID, id's approval of reviewed on pr, as id
// (gh): with ApprovalDismissMessage, or for an approval kept for a delta
// check with the reason it goes now (why, "" = none), records the review as
// DISMISSED while it is pr's last one and writes review.approval_dismissed;
// what names the commits since it ("2 new commits"). A 403 is remembered
// (kvApprovalRefused) and reported once; other failures are logged for the
// next try. It reports whether the dismissal is settled (done or refused).
func (e *Engine) dismissApproval(ctx context.Context, repo store.Repo, pr store.PR, gh GitHub, id *config.Identity, reviewID int64,
	reviewed, what, why string, data map[string]any) bool {
	subject := prSubject(repo, pr.Number)
	message, follows := ApprovalDismissMessage, "a re-review follows"
	if why != "" {
		message, follows = "magnum: new commits since this approval; "+why, why
		data["reason"] = why
	}
	err := gh.DismissReview(ctx, repo.Owner, repo.Name, pr.Number, reviewID, message)
	switch {
	case err == nil:
		uerr := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
			u.Where("last_review_id", reviewID)
			u.Set("last_review_event", reviewDismissed)
		})
		if uerr != nil && !errors.Is(uerr, store.ErrConflict) {
			e.log.Warn("approval dismissed but not recorded", "pr", pr.ID, "err", uerr)
		}
		e.delKV(ctx, kvApprovalPending(pr.ID))
		e.event(ctx, "info", subject, "review.approval_dismissed",
			fmt.Sprintf("dismissed approval %d by %s on %s: %s up to %s; %s", reviewID, id.Login, textx.ShortSHA(reviewed), what, textx.ShortSHA(pr.HeadSHA), follows), data)
		return true
	case errors.Is(err, github.ErrForbidden):
		e.setKV(ctx, kvApprovalRefused(pr.ID), strconv.FormatInt(reviewID, 10))
		e.delKV(ctx, kvApprovalPending(pr.ID))
		e.event(ctx, "warn", subject, "review.approval_dismiss_refused",
			fmt.Sprintf("could not dismiss approval %d by %s (missing permission; not retried): %v", reviewID, id.Login, err), data)
		return true
	}
	if ctx.Err() == nil {
		e.log.Warn("approval: dismiss failed; retrying at the next poll", "pr", pr.ID, "review_id", reviewID, "err", err)
	}
	return false
}

// Approvals kept for a delta check (DECISIONS "An approval stands while a
// delta check of the commits since is due"): the approval of the commit
// before a small delta is not dismissed at the push; the delta check's
// review supersedes it (an approval) or it goes then (anything else), and
// it goes when no check posted within approvalCheckWait of the time the
// check was due, quiet hours left out (keptApprovalDeadline).

// kvApprovalPending holds the approval kept for a delta check
// (pendingApproval as JSON).
func kvApprovalPending(prID int64) string { return fmt.Sprintf("pr.%d.approval_pending", prID) }

// approvalCheckWait is how long an approval stands for a delta check that
// has not posted, counted from the time the check is due (the push quiet
// period after the first push it does not cover) and only while it may run:
// [daemon] quiet_hours do not count (approvalDeadline). An approval never
// covers unreviewed code for long.
const approvalCheckWait = time.Hour

// keptApprovalDeadline is when an approval kept for a delta check of the
// commits since the first push it does not cover (since) goes if no check
// posted: approvalCheckWait after the check is due (since and the watch's
// push quiet period), quiet hours left out. A `magnum pause` is not left
// out: the operator holds the rounds on purpose, and an approval of code
// nobody checks goes.
func (e *Engine) keptApprovalDeadline(w *config.Watch, since time.Time) time.Time {
	due := since.Add(e.cfg.ThrottleFor(w).PushQuietPeriod.Duration)
	return approvalDeadline(e.cfg.Daemon.QuietHours, due, approvalCheckWait)
}

// approvalDeadline is the time wait of time outside the quiet hours spec
// (config.ParseQuietHours, local) has passed since due: quiet hours at due
// move its start to their end, and quiet hours that begin before the wait
// ran out add their length. Without quiet hours it is due plus wait.
func approvalDeadline(spec string, due time.Time, wait time.Duration) time.Time {
	w, ok, err := config.ParseQuietHours(spec)
	if err != nil || !ok {
		return due.Add(wait)
	}
	t := due
	for wait > 0 {
		if quietHoursNow(spec, t) {
			t = quietHoursEnd(spec, t)
			continue
		}
		n := t.Local()
		start := time.Date(n.Year(), n.Month(), n.Day(), w.Start/60, w.Start%60, 0, 0, n.Location())
		if !start.After(n) {
			start = start.AddDate(0, 0, 1)
		}
		if !t.Add(wait).After(start) {
			return t.Add(wait)
		}
		wait -= start.Sub(t)
		t = start
	}
	return t
}

// pendingApproval is an approval kept for a delta check (kvApprovalPending).
type pendingApproval struct {
	ReviewID int64  `json:"review_id"`
	Identity string `json:"identity"` // the App identity that posted it
	Reviewed string `json:"reviewed"` // the commit it approved
	Head     string `json:"head"`     // the head the check is due for
	Lines    int    `json:"lines"`    // the delta's changed code lines
	// Since is the first push the approval does not cover.
	Since time.Time `json:"since"`
	// Dismiss is why it goes, when a round's end asked for its dismissal
	// and GitHub did not answer: the next poll tries again.
	Dismiss string `json:"dismiss,omitempty"`
}

// pendingApproval reads kvApprovalPending.
func (e *Engine) pendingApproval(ctx context.Context, prID int64) (pendingApproval, bool) {
	v, ok := e.getKV(ctx, kvApprovalPending(prID))
	var p pendingApproval
	if !ok || v == "" || json.Unmarshal([]byte(v), &p) != nil || p.ReviewID == 0 {
		return pendingApproval{}, false
	}
	return p, true
}

func (e *Engine) setPendingApproval(ctx context.Context, prID int64, p pendingApproval) {
	if b, err := json.Marshal(p); err == nil {
		e.setKV(ctx, kvApprovalPending(prID), string(b))
	}
}

// keepForDeltaCheck decides whether id's approval of pr's reviewed commit
// stands for a delta check: the PR's measured delta from that commit to its
// head (KVPRDelta; for a head a round in flight has not measured yet, this
// tick's comparison) gets one (eligibility.DeltaCheck, whatever round the
// PR then runs) and its deadline has not passed (keptApprovalDeadline). A
// newly kept approval is recorded (kvApprovalPending) with
// review.approval_kept_for_check. When it does not stand and was kept so
// far, why says why it goes now.
func (e *Engine) keepForDeltaCheck(ctx context.Context, repo store.Repo, pr store.PR, id *config.Identity) (keep bool, why string) {
	reviewID, reviewed := deref(pr.LastReviewID), deref(pr.ReviewedSHA)
	p, pending := e.pendingApproval(ctx, pr.ID)
	pending = pending && p.ReviewID == reviewID
	w := e.cfg.WatchFor(repo.FullName())
	if w == nil {
		return false, ""
	}
	gone := func(reason string) (bool, string) {
		if pending {
			return false, reason
		}
		return false, ""
	}
	var size DeltaSize
	since := e.now()
	if rec, ok := e.deltaRecord(ctx, pr.ID); ok && rec.From == reviewed && rec.To == pr.HeadSHA {
		size = rec.DeltaSize
		if !rec.Since.IsZero() {
			since = rec.Since
		}
	} else if !pending {
		return false, "" // an unmeasured delta: the approval goes, as before
	} else if dc := e.checkDelta(ctx, repo, *w, prBase(repo, pr), reviewed, pr.HeadSHA); dc.measured {
		size = dc.size // a push while the check runs
	} else {
		return gone("the commits since it could not be measured; a re-review follows")
	}
	if pending && !p.Since.IsZero() {
		since = p.Since
	}
	f := eligibility.PRFacts{ReviewedSHA: reviewed, DeltaReadable: size.Readable(), DeltaLines: size.Lines, DeltaAddedFiles: size.AddedFiles}
	deadline := e.keptApprovalDeadline(w, since)
	switch {
	case !eligibility.DeltaCheck(e.cfg.ThrottleFor(w), f):
		return gone("they are no longer a small delta; a re-review follows")
	case !e.now().Before(deadline):
		return gone(fmt.Sprintf("no check of them posted within %s of becoming due; a re-review follows", humanDuration(approvalCheckWait)))
	}
	if pending && p.Head == pr.HeadSHA {
		return true, ""
	}
	e.setPendingApproval(ctx, pr.ID, pendingApproval{ReviewID: reviewID, Identity: id.Name, Reviewed: reviewed, Head: pr.HeadSHA,
		Lines: size.Lines, Since: since.UTC()})
	e.event(ctx, "info", prSubject(repo, pr.Number), "review.approval_kept_for_check",
		fmt.Sprintf("approval %d by %s on %s stands for a delta check of %s up to %s (at most until %s)", reviewID, id.Login,
			textx.ShortSHA(reviewed), textx.Count(size.Lines, "line", "lines"), textx.ShortSHA(pr.HeadSHA),
			deadline.Local().Format("15:04")),
		map[string]any{"review_id": reviewID, "reviewed_sha": reviewed, "head_sha": pr.HeadSHA, "identity": id.Name, "lines": size.Lines})
	return true, ""
}

// settleKeptApproval settles the approval kept for a delta check once a
// review of pr posted (res, on target): an approval supersedes it
// (review.approval_superseded), any other verdict dismisses it.
func (e *Engine) settleKeptApproval(ctx context.Context, repo store.Repo, pr store.PR, target string, res pipeline.RoundResult) {
	p, ok := e.pendingApproval(ctx, pr.ID)
	if !ok || res.ReviewID == 0 || res.ReviewID == p.ReviewID {
		return
	}
	if !isApproval(res.Event) {
		e.dismissKept(ctx, repo, pr, p, fmt.Sprintf("their check posted %s", strings.ToLower(cmp.Or(res.Event, "no verdict"))))
		return
	}
	e.delKV(ctx, kvApprovalPending(pr.ID))
	e.event(ctx, "info", prSubject(repo, pr.Number), "review.approval_superseded",
		fmt.Sprintf("approval %d on %s superseded by approval %d on %s", p.ReviewID, textx.ShortSHA(p.Reviewed), res.ReviewID, textx.ShortSHA(target)),
		map[string]any{"review_id": p.ReviewID, "reviewed_sha": p.Reviewed, "new_review_id": res.ReviewID, "target_sha": target})
}

// keptApprovalFailed dismisses the approval kept for a delta check when the
// PR's round failed (why says how): no check will post for it soon.
func (e *Engine) keptApprovalFailed(ctx context.Context, repo store.Repo, pr store.PR, why string) {
	if p, ok := e.pendingApproval(ctx, pr.ID); ok {
		e.dismissKept(ctx, repo, pr, p, "their check failed: "+why)
	}
}

// dismissKept dismisses the kept approval p as the identity that posted it,
// with the reason it goes (why); a dismissal GitHub does not answer stays
// recorded for the next poll (pendingApproval.Dismiss).
func (e *Engine) dismissKept(ctx context.Context, repo store.Repo, pr store.PR, p pendingApproval, why string) {
	id := e.cfg.IdentityByName(p.Identity)
	var gh GitHub
	if id != nil {
		gh = e.gh(id.Name)
	}
	if e.d.DryRun || gh == nil {
		e.delKV(ctx, kvApprovalPending(pr.ID))
		return
	}
	data := map[string]any{"review_id": p.ReviewID, "reviewed_sha": p.Reviewed, "head_sha": pr.HeadSHA, "identity": id.Name}
	if !e.dismissApproval(ctx, repo, pr, gh, id, p.ReviewID, p.Reviewed, "new commits", why, data) && p.Dismiss != why {
		p.Dismiss = why
		e.setPendingApproval(ctx, pr.ID, p)
	}
}
