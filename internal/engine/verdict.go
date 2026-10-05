package engine

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Manual verdicts: `magnum approve` and `magnum request-changes` (and the
// board's A and C) post the reviewer's own verdict on the head magnum
// reviewed, as the PR's posting identity, for a repository whose policy only
// lets magnum comment or when the reviewer disagrees with its event.
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
}

// KVPRManualVerdict holds the id of the review a manual verdict posted:
// rounds never dismiss it as their own stale review (it is the reviewer's
// decision), while an approval still follows the head (approval.go).
func KVPRManualVerdict(prID int64) string { return fmt.Sprintf("pr.%d.manual_verdict", prID) }

// requestVerdict posts a manual APPROVE or REQUEST_CHANGES on the PR's
// reviewed head and records it as the PR's latest review.
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
	gh := e.gh(pr.Identity)
	if gh == nil {
		return "", fmt.Errorf("identity %q has no GitHub client", pr.Identity)
	}
	var sum *store.ReviewSummary
	if sums, err := e.st.LastReviewSummaries(ctx, []int64{pr.ID}); err == nil {
		if s, ok := sums[pr.ID]; ok {
			sum = &s
		}
	}
	rev, err := gh.CreateReview(ctx, repo.Owner, repo.Name, pr.Number, reviewed, event, verdictBody(event, p.Message, reviewed, sum))
	if err != nil {
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
	e.event(ctx, "info", prSubject(repo, pr.Number), "review.manual_verdict", msg,
		map[string]any{"event": event, "review_id": rev.ID, "sha": reviewed, "identity": pr.Identity})
	return msg, nil
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
