package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Manual verdicts: `magnum approve` and `magnum request-changes` (and the
// board's A and C) post the reviewer's own verdict on the head magnum
// reviewed, as the PR's posting identity, for a repository whose policy only
// lets magnum comment or when the reviewer disagrees with its event. On a PR
// GitHub blocks on the operator's own approval (an App's never counts), an
// approval goes as the watch's auto_approve_as instead (approveAsOperator).
const (
	ReqApprove        = "approve"
	ReqRequestChanges = "request_changes"
)

// VerdictPayload is a manual verdict request.
type VerdictPayload struct {
	PRTarget
	// Message is the reviewer's own words, put before magnum's line.
	Message string `json:"message,omitempty"`
	// Force posts on the reviewed head although the PR moved on since.
	Force bool `json:"force,omitempty"`
	// As is the identity an approval posts as: "" or the PR's posting
	// identity posts as today; the watch's auto_approve_as, the operator's
	// own account, posts their approval, which GitHub counts
	// (approveAsOperator).
	As string `json:"as,omitempty"`
}

// KVPRManualVerdict holds the id of the review a manual verdict posted:
// rounds never dismiss it as their own stale review (it is the reviewer's
// decision), while an approval still follows the head (approval.go).
func KVPRManualVerdict(prID int64) string { return fmt.Sprintf("pr.%d.manual_verdict", prID) }

// requestVerdict posts a manual APPROVE or REQUEST_CHANGES on the PR's
// reviewed head and records it as the PR's latest review, with
// review.manual_verdict_begin, review.manual_verdict or
// review.manual_verdict_failed. A changes request withdraws the operator's
// standing automatic approval first (withdrawForChanges): it would still
// count for merging.
func (e *Engine) requestVerdict(ctx context.Context, p VerdictPayload, event string) (string, error) {
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	reviewed := deref(pr.ReviewedSHA)
	switch {
	case reviewed == "":
		return "", fmt.Errorf("magnum has not reviewed %s yet: `magnum review %s` first", label, label)
	case slices.Contains(store.InFlightStates, pr.State):
		return "", fmt.Errorf("a review round of %s is running: wait for its review, then decide", label)
	case pr.HeadSHA != reviewed && !p.Force:
		return "", fmt.Errorf("%s moved to %s since magnum reviewed %s: review it again first, or --force to post on %s",
			label, textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(reviewed), textx.ShortSHA(reviewed))
	}
	var sum *store.ReviewSummary
	sums, sumErr := e.st.LastReviewSummaries(ctx, []int64{pr.ID}) // a nil map on an error: the body leaves the review out
	if s, ok := sums[pr.ID]; ok {
		sum = &s
	}
	if p.As != "" && p.As != pr.Identity {
		if sumErr != nil { // the operator's approval follows magnum's round: it must be known
			return "", sumErr
		}
		return e.approveAsOperator(ctx, repo, pr, p, event, sum)
	}
	gh := e.gh(pr.Identity)
	if gh == nil {
		return "", fmt.Errorf("identity %q has no GitHub client", pr.Identity)
	}
	if event == "REQUEST_CHANGES" {
		if err := e.withdrawForChanges(ctx, repo, pr, label); err != nil {
			return "", err
		}
	}
	subject := prSubject(repo, pr.Number)
	e.event(ctx, "info", subject, "review.manual_verdict_begin", fmt.Sprintf("posting %s on %s at %s as %s", strings.ToLower(event), label,
		textx.ShortSHA(reviewed), pr.Identity), map[string]any{"event": event, "sha": reviewed, "identity": pr.Identity})
	rev, err := gh.CreateReview(ctx, repo.Owner, repo.Name, pr.Number, reviewed, event, verdictBody(event, p.Message, reviewed, sum))
	if err != nil {
		e.event(ctx, "warn", subject, "review.manual_verdict_failed", fmt.Sprintf("could not post %s on %s as %s: %v", strings.ToLower(event), label,
			pr.Identity, err), map[string]any{"event": event, "sha": reviewed, "identity": pr.Identity})
		return "", fmt.Errorf("post %s on %s: %w", strings.ToLower(event), label, err)
	}
	state := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED"}[event]
	if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
		u.Set("last_review_id", rev.ID)
		u.Set("last_review_event", state)
		if l := rev.UserLogin; l != "" {
			u.Set("last_review_login", l)
		}
	}); err != nil {
		return "", fmt.Errorf("%s posted (%s) but not recorded: %w", label, rev.HTMLURL, err)
	}
	e.setKV(ctx, KVPRManualVerdict(pr.ID), strconv.FormatInt(rev.ID, 10))
	done := map[string]string{"APPROVE": "approved", "REQUEST_CHANGES": "requested changes on"}[event]
	msg := fmt.Sprintf("%s %s at %s as %s: %s", done, label, textx.ShortSHA(reviewed), rev.UserLogin, rev.HTMLURL)
	e.seeStalemates(ctx, pr.ID) // the operator decided: stalemate.go
	e.event(ctx, "info", subject, "review.manual_verdict", msg,
		map[string]any{"event": event, "review_id": rev.ID, "sha": reviewed, "identity": pr.Identity})
	return msg, nil
}

// withdrawForChanges withdraws the operator's standing automatic approval
// of pr (label) as its identity before `magnum request-changes` posts:
// GitHub would count it for merging next to the changes request. One being
// withdrawn already, or none, is nothing to do; one being posted, or a
// withdrawal GitHub did not take, refuses the changes request (magnum tries
// the withdrawal again, and the operator asks again).
func (e *Engine) withdrawForChanges(ctx context.Context, repo store.Repo, pr store.PR, label string) error {
	e.autoMu.Lock() // a round's goroutine may be approving the PR as the operator (autoApproveRound)
	defer e.autoMu.Unlock()
	live, ok, err := e.st.LiveAutoApproval(ctx, pr.ID)
	switch {
	case err != nil:
		return err
	case !ok || live.State == store.AutoDismissing:
		return nil
	case live.State == store.AutoPosting:
		return fmt.Errorf("an approval of yours on %s is being posted: try again in a moment", label)
	}
	gh, _ := e.autoClient(e.cfg.IdentityByName(live.Identity))
	if gh == nil {
		return fmt.Errorf("identity %q has no GitHub client to withdraw your approval of %s (review %d): dismiss it on GitHub first", live.Identity,
			label, live.ReviewID)
	}
	if err := e.withdrawAutoApproval(ctx, repo, pr, live, gh, store.AutoEndedOperator, RequestChangesMessage); err != nil {
		return fmt.Errorf("withdraw your approval of %s first: %w; no changes were requested (magnum tries the withdrawal again)", label, err)
	}
	return nil
}

// ApproveAsFor is the identity A (on a row GitHub blocks on the operator)
// and `magnum approve --as` post the operator's own approval of
// repository fullName's PRs as: the covering watch's auto_approve_as,
// whether or not its auto_approve names the repository; nil when the watch
// names none (GitHub then counts only an approval by hand).
func ApproveAsFor(cfg *config.Config, fullName string) *config.Identity {
	if cfg == nil {
		return nil
	}
	w := cfg.WatchFor(fullName)
	if w == nil || w.AutoApproveAs == "" {
		return nil
	}
	return cfg.IdentityByName(w.AutoApproveAs)
}

// OperatorApprovalRefusal says why the operator's own approval of pr cannot
// be posted as login (their auto_approve_as account) now, "" when it can:
// auto-approval's preconditions (autoApproveRefusal), less the two that
// exist only because auto-approval acts without asking. The operator's stop
// (their changes request or review by hand, a dismissal, magnum unapprove)
// and a manual verdict posted after magnum's round both keep auto-approval
// from overriding the operator, who here is the one asking: A lifts their
// own ✗ on purpose. What auto-approval holds a PR back for besides
// (autoGateRefusal: a reviewer's missing report, a PR its watch would not
// review on its own, the agents' instructions, the head's checks) does not
// refuse it either: the operator decides by hand, the board shows those.
// sum is magnum's latest posted round of pr (nil: none).
func OperatorApprovalRefusal(pr store.PR, sum *store.ReviewSummary, login string) string {
	if sum != nil {
		id := sum.ReviewID
		pr.LastReviewID = &id
	}
	return autoApproveRefusal(autoFacts{PR: pr, Sum: sum, Login: login})
}

// approveAsOperator posts the operator's own approval of pr's reviewed head
// as the watch's auto_approve_as (A on a row GitHub blocks on them, magnum
// approve --as), which GitHub counts where it never counts an App's. It is
// refused where OperatorApprovalRefusal says, and recorded like an
// automatic approval with its marker: the PR's latest review stays
// magnum's round's, a later round that leaves something to fix withdraws
// it (followStanding), D and magnum unapprove withdraw it, and a post whose
// answer a stopped daemon lost is found on GitHub (resumeAutoPost).
func (e *Engine) approveAsOperator(ctx context.Context, repo store.Repo, pr store.PR, p VerdictPayload, event string,
	sum *store.ReviewSummary) (string, error) {
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	id := ApproveAsFor(e.cfg, repo.FullName())
	switch {
	case event != "APPROVE":
		return "", fmt.Errorf("--as %s only approves: request changes as %s, the PR's posting identity", p.As, pr.Identity)
	case id == nil:
		return "", fmt.Errorf("GitHub counts only your approval: the watch of %s names no auto_approve_as, so approve it on GitHub: %s", label, pr.URL)
	case p.As != id.Name:
		return "", fmt.Errorf("--as takes %s (the watch's auto_approve_as) or %s (the PR's posting identity), not %s", id.Name, pr.Identity, p.As)
	}
	login := cmp.Or(id.Login, id.Name)
	if why := OperatorApprovalRefusal(pr, sum, id.Login); why != "" {
		return "", fmt.Errorf("%s is not approved as %s: %s", label, login, why)
	}
	e.autoMu.Lock() // a round's goroutine may be approving the PR as the operator (autoApproveRound)
	defer e.autoMu.Unlock()
	switch live, ok, err := e.st.LiveAutoApproval(ctx, pr.ID); {
	case err != nil:
		return "", err
	case ok && live.State == store.AutoStanding:
		return "", fmt.Errorf("an approval of yours stands on %s (review %d of %s): magnum unapprove %s withdraws it", label, live.ReviewID,
			textx.ShortSHA(live.HeadSHA), label)
	case ok:
		return "", fmt.Errorf("an approval of yours on %s is %s: try again in a moment", label, live.State)
	}
	gh, _ := e.autoClient(id)
	if gh == nil {
		return "", fmt.Errorf("identity %q has no GitHub client", id.Name)
	}
	a, err := e.st.InsertAutoApproval(ctx, store.AutoApproval{PRID: pr.ID, RunID: sum.RunID, SourceReviewID: sum.ReviewID, SourceURL: sum.URL,
		HeadSHA: pr.HeadSHA, Identity: id.Name, Login: id.Login})
	if err != nil {
		return "", fmt.Errorf("approve %s as %s: %w", label, login, err)
	}
	body := verdictBody(event, p.Message, pr.HeadSHA, sum) + "\n" + AutoApprovalMarker(pr.HeadSHA)
	subject := prSubject(repo, pr.Number)
	data := map[string]any{"event": event, "sha": pr.HeadSHA, "identity": id.Name, "approval_id": a.ID}
	e.event(ctx, "info", subject, "review.manual_verdict_begin", fmt.Sprintf("approving %s at %s as %s", label, textx.ShortSHA(pr.HeadSHA), login), data)
	rev, err := gh.CreateReview(ctx, repo.Owner, repo.Name, pr.Number, pr.HeadSHA, event, body)
	if err != nil { // as postAutoApproval: a 403 or 422 is final, any other may be tried again by auto-approval
		e.event(ctx, "warn", subject, "review.manual_verdict_failed", fmt.Sprintf("could not approve %s as %s: %v", label, login, err), data)
		var apiErr *github.APIError
		final := errors.Is(err, github.ErrForbidden) || (errors.As(err, &apiErr) && apiErr.Status == 422)
		if uerr := e.st.TransitionAutoApproval(ctx, a.ID, []string{store.AutoPosting}, store.AutoFailed, func(u *store.AutoApprovalUpdate) {
			u.Set("error", err.Error())
			if final {
				u.Set("attempts", autoApproveAttempts)
			}
		}); uerr != nil {
			e.log.Warn("approval as the operator failed and not recorded", "pr", pr.ID, "err", uerr)
		}
		return "", fmt.Errorf("approve %s as %s: %w", label, login, err)
	}
	if err := e.st.TransitionAutoApproval(ctx, a.ID, []string{store.AutoPosting}, store.AutoStanding, func(u *store.AutoApprovalUpdate) {
		u.Set("review_id", rev.ID)
		u.Set("review_url", rev.HTMLURL)
		u.Set("posted_at", e.now())
		u.Set("error", nil)
	}); err != nil {
		return "", fmt.Errorf("%s approved as %s (%s) but not recorded: %w", label, login, rev.HTMLURL, err)
	}
	msg := fmt.Sprintf("approved %s at %s as %s: %s", label, textx.ShortSHA(pr.HeadSHA), cmp.Or(rev.UserLogin, login), rev.HTMLURL)
	e.seeStalemates(ctx, pr.ID) // the operator decided: stalemate.go
	data["review_id"] = rev.ID
	e.event(ctx, "info", subject, "review.manual_verdict", msg, data)
	return msg + " (withdrawn when a later review finds something to fix)", nil
}

// verdictBody is a manual verdict's review body: the reviewer's words, a
// line naming magnum's review it follows (with its findings), and a marker.
func verdictBody(event, message, sha string, sum *store.ReviewSummary) string {
	var b strings.Builder
	if m := strings.TrimSpace(message); m != "" {
		b.WriteString(m + "\n\n")
	}
	what := "Approved"
	if event == "REQUEST_CHANGES" {
		what = "Changes requested"
	}
	fmt.Fprintf(&b, "%s after magnum's review of `%s`", what, textx.ShortSHA(sha))
	if sum != nil {
		var parts []string
		for i, n := range sum.Counts {
			if n > 0 {
				parts = append(parts, fmt.Sprintf("%d P%d", n, i))
			}
		}
		if len(parts) > 0 {
			b.WriteString(" (" + strings.Join(parts, ", ") + ")")
		}
		if sum.URL != "" {
			b.WriteString(": " + sum.URL)
		}
	}
	b.WriteString(".\n\n<!-- magnum:verdict=" + event + " head=" + textx.ShortSHA(sha) + " -->")
	return b.String()
}
