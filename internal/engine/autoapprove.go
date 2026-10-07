package engine

// Automatic approvals ([[watch]] auto_approve; DECISIONS "magnum approves as
// the operator when its review found nothing to fix"): GitHub does not count
// magnum's GitHub App approval toward a branch's required approvals, so a PR
// magnum found clean still waits for the operator. On the repositories a
// watch names, magnum posts the operator's own approval (auto_approve_as, a
// gh identity) on the head it reviewed once its latest review of that head
// left nothing to fix before merging, and withdraws it when a later review
// finds something that must be fixed; new commits alone leave it to the next
// review. It also holds back what autoapprove_gates.go names: a review that
// did not hear every reviewer, a PR its watch would not review on its own,
// one that changes the review agents' instructions, a head whose checks
// fail or run. The operator's word wins: a review of theirs by hand, or a
// dismissal of one of these approvals, stops it on that PR for good
// (`magnum unapprove --resume` lifts that). Every post and withdrawal is
// compare-and-set on the auto_approvals row, writes begin/ok/fail events,
// and is found on GitHub instead of repeated when its answer was lost; a
// withdrawal GitHub keeps refusing ends failed after autoApproveAttempts,
// and the approval of a merged or closed PR ends as history.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// ReqUnapprove is `magnum unapprove` and the board's D (UnapprovePayload).
const ReqUnapprove = "unapprove"

// UnapprovePayload withdraws the PR's automatic approval and stops
// auto-approval of the PR; Resume lets magnum approve it again instead.
type UnapprovePayload struct {
	PRTarget
	Resume bool `json:"resume,omitempty"`
}

// UnapproveMessage is the dismissal message of an automatic approval the
// operator withdraws (magnum unapprove, the board's D); RequestChangesMessage
// that of one `magnum request-changes` withdraws before it posts.
const (
	UnapproveMessage      = "magnum: this automatic approval is withdrawn by its owner (magnum unapprove)."
	RequestChangesMessage = "magnum: this automatic approval is withdrawn: its owner requested changes (magnum request-changes)."
)

// autoApprovalMarker starts the hidden marker that ends an automatic
// approval's body (AutoApprovalMarker); magnumRunMarker starts the marker
// of a review a magnum round posted.
const (
	autoApprovalMarker = "<!-- magnum:auto-approval"
	magnumRunMarker    = "<!-- magnum:run="
)

// AutoApprovalMarker is the marker of an automatic approval of head:
// "<!-- magnum:auto-approval head=<sha7> -->".
func AutoApprovalMarker(head string) string {
	return autoApprovalMarker + " head=" + textx.ShortSHA(head) + " -->"
}

// autoApproveRetry is how long after a failed post or withdrawal magnum
// tries again; autoApproveAttempts bounds the posts of one review and the
// dismissals of one withdrawal.
const (
	autoApproveRetry    = 5 * time.Minute
	autoApproveAttempts = 3
)

// autoToastWindow dedupes the toasts of one approval across restarts.
const autoToastWindow = 30 * 24 * time.Hour

var (
	kindAutoApproved  = notify.Kind{One: "PR approved as you", Many: "PRs approved as you"}
	kindAutoWithdrawn = notify.Kind{One: "approval of yours withdrawn", Many: "approvals of yours withdrawn"}
)

// autoApproveGitHub is what auto-approval reads from GitHub
// (*github.Client) beyond the GitHub interface, whose AllReviews lists a
// PR's reviews: who dismissed which review. A client without it never
// approves as the operator.
type autoApproveGitHub interface {
	ReviewDismissals(ctx context.Context, owner, repo string, number int) ([]github.ReviewDismissal, error)
}

var _ autoApproveGitHub = (*github.Client)(nil)

// verdictKind is the kind of a magnum review's verdict line (SKILL.md
// section 7).
type verdictKind int

const (
	verdictUnknown  verdictKind = iota
	verdictBlocking             // "Blocking: N problem(s) must be fixed before merging."
	verdictFix                  // "Fix N problem(s) before merging." (P2)
	verdictOptional             // "No blocking problems." (optional ones only)
	verdictClean                // "No problems found…", "No new problems since …"
)

var fixLine = regexp.MustCompile(`^Fix \d+ problems? `)

// reviewVerdictLine is the kind of the verdict line a magnum review starts
// with, past a re-review's bold lead ("**Re-review a → b:**", on its line or
// the line before); verdictUnknown when that line is none of the four.
func reviewVerdictLine(body string) verdictKind {
	var line string
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(l, "**"); ok {
			if _, after, ok := strings.Cut(rest, "**"); ok {
				if l = strings.TrimSpace(after); l == "" {
					continue // the lead alone: the verdict line follows
				}
			}
		}
		line = l
		break
	}
	switch {
	case strings.HasPrefix(line, "Blocking:"):
		return verdictBlocking
	case fixLine.MatchString(line):
		return verdictFix
	case strings.HasPrefix(line, "No blocking problems."):
		return verdictOptional
	case strings.HasPrefix(line, "No problems found"), strings.HasPrefix(line, "No new problems since"):
		return verdictClean
	}
	return verdictUnknown
}

// roundBlocks says whether a posted round left something that must be
// fixed before merging: a P0, P1 or P2 finding, new or still open, by the
// judge's result (sum: the earlier findings still open by priority, when it
// gives them) or the verdict line of the review it posted (body, "" when
// not read), which only a result without those priorities needs. known is
// false when neither tells: earlier findings are still open, the result
// does not give their priorities, and the verdict line was not read or is
// not one magnum knows.
func roundBlocks(sum store.ReviewSummary, body string) (blocks, known bool) {
	line := reviewVerdictLine(body)
	switch {
	case line == verdictBlocking || line == verdictFix:
		return true, true
	case sum.Counts[0]+sum.Counts[1]+sum.Counts[2] > 0 || sum.Verdict == store.VerdictBlocking:
		return true, true
	case sum.OpenCounts != nil:
		return sum.OpenCounts[0]+sum.OpenCounts[1]+sum.OpenCounts[2] > 0, true
	case sum.Open == 0:
		return false, true
	case line == verdictOptional || line == verdictClean:
		return false, true
	}
	return false, false
}

// autoFacts are what autoApproveRefusal decides from: the PR, magnum's
// latest posted round of it (nil = none), auto_approve_as's login and the
// PR's hold (nil = none).
type autoFacts struct {
	PR    store.PR
	Sum   *store.ReviewSummary
	Login string
	Hold  *store.AutoApproveHold
}

// autoApproveRefusal says why magnum does not approve f.PR as the operator,
// from the registry alone; "" when it may, once the operator's reviews on
// GitHub (operatorWord) and its review's verdict line agree. It refuses a
// PR that is not open, a draft, a muted one, the operator's own, one whose
// head magnum's latest verified review does not cover or that a round is
// due for or running on, one the operator stopped it on, one whose latest
// review is not magnum's round's (a manual verdict came after it) and one
// whose round's result says something must be fixed.
func autoApproveRefusal(f autoFacts) string {
	pr := f.PR
	switch {
	case pr.GHState != store.GHOpen:
		return "it is " + strings.ToLower(pr.GHState)
	case pr.IsDraft:
		return "it is a draft"
	case pr.Muted:
		return "it is muted"
	case github.SameAccount(github.Account(deref(pr.AuthorLogin), deref(pr.AuthorType)), f.Login):
		return "it is your own"
	case pr.State != store.PRReviewed:
		return "it is " + pr.State
	case deref(pr.ReviewedSHA) == "" || deref(pr.ReviewedSHA) != pr.HeadSHA:
		return "magnum has not reviewed its head"
	case f.Hold != nil && f.Hold.Held:
		return "stopped: " + f.Hold.Reason
	case f.Sum == nil || f.Sum.ReviewID == 0:
		return "magnum posted no review of it"
	case deref(pr.LastReviewID) != f.Sum.ReviewID:
		return "its latest review is not magnum's round's"
	}
	if blocks, _ := roundBlocks(*f.Sum, ""); blocks {
		return "magnum's review found something to fix"
	}
	return ""
}

// opWord is what the operator's own reviews on GitHub say.
type opWord struct {
	// hand is their latest review by hand (not one magnum posted as them:
	// a round's, an automatic approval) since the resume, if any.
	hand *github.Review
	// pending: they have a review in progress.
	pending bool
	// approved is their approval of the head when that is their latest
	// verdict; approvedByMagnum says it is one of magnum's automatic
	// approvals (its marker).
	approved         *github.Review
	approvedByMagnum bool
}

// handReason is why hand stops auto-approval, in the operator's words.
func (w opWord) handReason() string {
	if w.hand == nil {
		return ""
	}
	what := map[string]string{"APPROVED": "you approved it by hand", "CHANGES_REQUESTED": "you requested changes by hand",
		"COMMENTED": "you commented on it by hand"}[w.hand.State]
	if what == "" {
		what = "you reviewed it by hand"
	}
	if w.hand.URL != "" {
		what += " (" + w.hand.URL + ")"
	}
	return what
}

// magnumPosted reports whether a review body is one magnum posted: a
// round's review or an automatic approval (by their markers). A manual
// verdict (magnum approve, request-changes) is the operator's own.
func magnumPosted(body string) bool {
	return strings.Contains(body, magnumRunMarker) || strings.Contains(body, autoApprovalMarker)
}

// operatorWord reads the operator's (login's; an App of that name is not
// them) reviews of a PR whose head is head; reviews submitted at or before
// since (a resume) do not count as by hand.
func operatorWord(reviews []github.Review, login, head string, since time.Time) opWord {
	var w opWord
	var verdict *github.Review
	for i := range reviews {
		r := &reviews[i]
		if !github.SameAccount(github.Account(r.AuthorLogin, r.AuthorType), login) {
			continue
		}
		if strings.EqualFold(r.State, "PENDING") {
			w.pending = true
			continue
		}
		if !magnumPosted(r.Body) && (since.IsZero() || r.SubmittedAt.After(since)) && (w.hand == nil || !r.SubmittedAt.Before(w.hand.SubmittedAt)) {
			w.hand = r
		}
		switch strings.ToUpper(r.State) {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			if verdict == nil || !r.SubmittedAt.Before(verdict.SubmittedAt) {
				verdict = r
			}
		}
	}
	if verdict != nil && strings.EqualFold(verdict.State, "APPROVED") && head != "" && verdict.CommitOid == head {
		w.approved = verdict
		w.approvedByMagnum = strings.Contains(verdict.Body, AutoApprovalMarker(head))
	}
	return w
}

// reviewBody is the body of review id among reviews ("" when not listed).
func reviewBody(reviews []github.Review, id int64) string {
	for _, r := range reviews {
		if r.DatabaseID == id {
			return r.Body
		}
	}
	return ""
}

// repoOf is the repository of a full name ("owner/name").
func repoOf(full string) store.Repo {
	owner, name, _ := strings.Cut(full, "/")
	return store.Repo{Owner: owner, Name: name}
}

func timeKey(t *time.Time) string {
	if t == nil {
		return ""
	}
	return store.FormatTime(*t)
}

// autoApprove posts and follows the operator's automatic approvals: every
// tick, after dispatch and before the needs-me toasts (a PR it approves no
// longer needs the operator). Dry runs change nothing.
func (e *Engine) autoApprove(ctx context.Context) { e.autoApproveFor(ctx, 0) }

// autoApproveRound is autoApprove for PR prID alone, run by a round's
// goroutine as the round that posted a review of it ends (runRound): the
// same decision and gates, without waiting for the next tick, whose pass
// stays the safety net.
func (e *Engine) autoApproveRound(ctx context.Context, prID int64) { e.autoApproveFor(ctx, prID) }

// autoApproveFor is autoApprove for PR only, or for every PR when only is 0,
// under autoMu.
func (e *Engine) autoApproveFor(ctx context.Context, only int64) {
	if e.d.DryRun || e.d.GitHub == nil {
		return
	}
	e.autoMu.Lock()
	defer e.autoMu.Unlock()
	live, err := e.st.LiveAutoApprovals(ctx)
	if err != nil {
		e.log.Warn("auto approvals", "err", err)
		return
	}
	var cands []store.RepoPR
	if e.cfg.AutoApproves() {
		if cands, err = e.st.AutoApproveCandidates(ctx); err != nil {
			e.log.Warn("auto approve candidates", "err", err)
			return
		}
	}
	if only != 0 {
		live = slices.DeleteFunc(live, func(a store.AutoApprovalPR) bool { return a.PRID != only })
		cands = slices.DeleteFunc(cands, func(c store.RepoPR) bool { return c.PR.ID != only })
	}
	if len(live)+len(cands) == 0 {
		return
	}
	ids := make([]int64, 0, len(live)+len(cands))
	for _, a := range live {
		ids = append(ids, a.PRID)
	}
	for _, c := range cands {
		ids = append(ids, c.PR.ID)
	}
	sums, err := e.st.LastReviewSummaries(ctx, ids)
	if err != nil {
		e.log.Warn("auto approvals: review summaries", "err", err)
		return
	}
	holds, err := e.st.AutoApproveHolds(ctx, ids)
	if err != nil {
		e.log.Warn("auto approvals: holds", "err", err)
		return
	}
	if e.autoSeen == nil {
		e.autoSeen, e.autoFollow, e.autoRefused = map[int64]string{}, map[int64]string{}, map[int64]string{}
	}
	for _, a := range live {
		e.followAutoApproval(ctx, a, summaryOf(sums, a.PRID), holdOf(holds, a.PRID))
	}
	for _, c := range cands {
		e.considerAutoApproval(ctx, c, summaryOf(sums, c.PR.ID), holdOf(holds, c.PR.ID))
	}
}

func summaryOf(m map[int64]store.ReviewSummary, id int64) *store.ReviewSummary {
	if s, ok := m[id]; ok {
		return &s
	}
	return nil
}

func holdOf(m map[int64]store.AutoApproveHold, id int64) *store.AutoApproveHold {
	if h, ok := m[id]; ok {
		return &h
	}
	return nil
}

// resumedSince is when the operator lifted a hold (only their reviews after
// it count); zero without one.
func resumedSince(h *store.AutoApproveHold) time.Time {
	if h == nil || h.Held {
		return time.Time{}
	}
	return h.At
}

// autoClient is identity id's GitHub client when it can do auto-approval
// (nil both ways when it cannot).
func (e *Engine) autoClient(id *config.Identity) (GitHub, autoApproveGitHub) {
	if id == nil {
		return nil, nil
	}
	gh := e.gh(id.Name)
	if gh == nil {
		return nil, nil
	}
	ag, ok := gh.(autoApproveGitHub)
	if !ok {
		return nil, nil
	}
	return gh, ag
}

// considerAutoApproval approves candidate c as the operator when its
// repository opts in, the registry allows it (autoApproveRefusal), no
// approval followed its latest review yet (one whose post failed is tried
// again after autoApproveRetry, autoApproveAttempts times), nothing holds
// it back (autoGateRefusal: recorded for the card), and GitHub's reviews,
// all of them, agree: none of the operator's by hand (else auto-approval of
// the PR stops), none in progress, no approval of the head of theirs, and
// a verdict line that leaves nothing to fix when the result cannot tell. An
// approval of magnum's GitHub has but the registry lost is recorded instead
// of posted again. GitHub is asked again only when the PR's latest round or
// updatedAt moved since it said no (or listed only part of the reviews).
func (e *Engine) considerAutoApproval(ctx context.Context, c store.RepoPR, sum *store.ReviewSummary, hold *store.AutoApproveHold) {
	id := e.cfg.AutoApproveFor(c.Repo)
	pr := c.PR
	if id == nil || autoApproveRefusal(autoFacts{PR: pr, Sum: sum, Login: id.Login, Hold: hold}) != "" {
		return
	}
	prior, hasPrior, err := e.st.AutoApprovalOfRun(ctx, pr.ID, sum.RunID)
	if err != nil {
		e.log.Warn("auto approval of the round", "pr", pr.ID, "err", err)
		return
	}
	if hasPrior && (prior.State != store.AutoFailed || prior.Attempts >= autoApproveAttempts || e.now().Before(prior.UpdatedAt.Add(autoApproveRetry))) {
		return
	}
	why := e.autoGateRefusal(ctx, c, *sum)
	e.noteAutoRefused(ctx, c, *sum, why)
	if why != "" {
		return
	}
	since := resumedSince(hold)
	fp := sum.RunID + "|" + timeKey(pr.GHUpdatedAt) + "|" + store.FormatTime(since)
	if !hasPrior && e.autoSeen[pr.ID] == fp {
		return
	}
	gh, ag := e.autoClient(id)
	if ag == nil {
		return
	}
	repo := repoOf(c.Repo)
	reviews, complete, err := gh.AllReviews(ctx, repo.Owner, repo.Name, pr.Number)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Info("auto approval: read the reviews", "pr", pr.ID, "err", err)
		}
		return
	}
	if !complete { // what GitHub left out may be the operator's review by hand
		e.autoSeen[pr.ID] = fp
		e.event(ctx, "info", prSubject(repo, pr.Number), "review.auto_approve_refused",
			fmt.Sprintf("magnum does not approve %s#%d as you: magnum cannot read all of its reviews on GitHub, so it cannot tell whether "+
				"you reviewed it by hand (asked again when the PR moves)", repo.FullName(), pr.Number),
			map[string]any{"reason": "incomplete reviews", "head_sha": pr.HeadSHA, "run_id": sum.RunID})
		return
	}
	var priorPtr *store.AutoApproval
	if hasPrior {
		priorPtr = &prior
	}
	w := operatorWord(reviews, id.Login, pr.HeadSHA, since)
	if w.approved != nil && w.approvedByMagnum {
		e.adoptAutoApproval(ctx, repo, pr, id, *sum, priorPtr, w.approved)
	}
	if w.hand != nil {
		e.stopAutoApproval(ctx, repo, pr, w.handReason())
		return
	}
	if w.approved != nil || w.pending {
		e.autoSeen[pr.ID] = fp
		return
	}
	if blocks, known := roundBlocks(*sum, reviewBody(reviews, sum.ReviewID)); blocks || !known {
		e.autoSeen[pr.ID] = fp
		return
	}
	e.postAutoApproval(ctx, repo, pr, id, *sum, priorPtr, gh)
}

// postAutoApproval posts the operator's approval of pr's head with the
// configured body and the marker: the row goes posting (a new one, or the
// failed prior one), then standing with GitHub's review, or failed (a 403 or
// 422 for good) with review.auto_approve_begin, review.auto_approved or
// review.auto_approve_failed; a posted one is toasted.
func (e *Engine) postAutoApproval(ctx context.Context, repo store.Repo, pr store.PR, id *config.Identity, sum store.ReviewSummary,
	prior *store.AutoApproval, gh GitHub) {
	subject := prSubject(repo, pr.Number)
	full := repo.FullName()
	body, err := config.RenderAutoApproveBody(e.cfg.AutoApproveBodyFor(full), config.AutoApproveData{SHA: pr.HeadSHA,
		Short: textx.ShortSHA(pr.HeadSHA), ReviewURL: sum.URL, Repo: full, Number: pr.Number})
	if err != nil { // Validate renders it: a template that does not is a bug
		e.log.Warn("auto approval body", "repo", full, "err", err)
		return
	}
	body += "\n\n" + AutoApprovalMarker(pr.HeadSHA)
	var a store.AutoApproval
	if prior != nil {
		if err := e.st.TransitionAutoApproval(ctx, prior.ID, []string{store.AutoFailed}, store.AutoPosting, func(u *store.AutoApprovalUpdate) {
			u.Inc("attempts", 1)
		}); err != nil {
			e.log.Info("auto approval moved on", "pr", pr.ID, "err", err)
			return
		}
		a = *prior
		a.Attempts++
	} else if a, err = e.st.InsertAutoApproval(ctx, store.AutoApproval{PRID: pr.ID, RunID: sum.RunID, SourceReviewID: sum.ReviewID,
		SourceURL: sum.URL, HeadSHA: pr.HeadSHA, Identity: id.Name, Login: id.Login}); err != nil {
		e.log.Info("auto approval not begun", "pr", pr.ID, "err", err)
		return
	}
	data := map[string]any{"approval_id": a.ID, "head_sha": pr.HeadSHA, "identity": id.Name, "run_id": sum.RunID,
		"source_review_id": sum.ReviewID, "attempt": a.Attempts}
	e.event(ctx, "info", subject, "review.auto_approve_begin", fmt.Sprintf("approving %s as %s: magnum's review %d found nothing to fix (attempt %d)",
		textx.ShortSHA(pr.HeadSHA), id.Login, sum.ReviewID, a.Attempts), data)
	rev, err := gh.CreateReview(ctx, repo.Owner, repo.Name, pr.Number, pr.HeadSHA, "APPROVE", body)
	if err != nil {
		var apiErr *github.APIError
		final := errors.Is(err, github.ErrForbidden) || (errors.As(err, &apiErr) && apiErr.Status == 422)
		if uerr := e.st.TransitionAutoApproval(ctx, a.ID, []string{store.AutoPosting}, store.AutoFailed, func(u *store.AutoApprovalUpdate) {
			u.Set("error", err.Error())
			if final {
				u.Set("attempts", autoApproveAttempts)
			}
		}); uerr != nil {
			e.log.Warn("auto approval failed and not recorded", "pr", pr.ID, "err", uerr)
		}
		again := fmt.Sprintf("tried again in %s", humanDuration(autoApproveRetry))
		if final || a.Attempts >= autoApproveAttempts {
			again = "not tried again for this review"
		}
		e.event(ctx, "warn", subject, "review.auto_approve_failed", fmt.Sprintf("could not approve %s as %s: %v; %s",
			textx.ShortSHA(pr.HeadSHA), id.Login, err, again), data)
		return
	}
	e.autoPosted(ctx, repo, pr, a, rev.ID, rev.HTMLURL, e.now(), sum, data, "")
}

// autoPosted records approval a posted as GitHub's review reviewID (url) at
// at, with review.auto_approved and a toast; how says how it was found
// ("" = its post answered).
func (e *Engine) autoPosted(ctx context.Context, repo store.Repo, pr store.PR, a store.AutoApproval, reviewID int64, url string, at time.Time,
	sum store.ReviewSummary, data map[string]any, how string) {
	if err := e.st.TransitionAutoApproval(ctx, a.ID, []string{store.AutoPosting}, store.AutoStanding, func(u *store.AutoApprovalUpdate) {
		u.Set("review_id", reviewID)
		u.Set("review_url", url)
		u.Set("posted_at", at)
		u.Set("error", nil)
	}); err != nil {
		e.log.Warn("auto approval posted and not recorded", "pr", pr.ID, "review_id", reviewID, "err", err)
		return
	}
	data["review_id"] = reviewID
	e.event(ctx, "info", prSubject(repo, pr.Number), "review.auto_approved", fmt.Sprintf("approved %s as %s (review %d)%s: %s",
		textx.ShortSHA(a.HeadSHA), a.Login, reviewID, how, url), data)
	label := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("auto-approved:%d", a.ID), Title: "magnum: approved as you: " + label,
		Body: fmt.Sprintf("%s\nmagnum's review of %s found nothing to fix (`magnum unapprove %s` withdraws it).", pr.URL,
			textx.ShortSHA(a.HeadSHA), label),
		Line: "approved as you: " + label + " " + pr.URL, Kind: kindAutoApproved, Window: autoToastWindow})
}

// adoptAutoApproval records r, an approval of the head GitHub has with
// magnum's marker but the registry does not (the answer to its post was
// lost), as the PR's standing approval: never posted twice.
func (e *Engine) adoptAutoApproval(ctx context.Context, repo store.Repo, pr store.PR, id *config.Identity, sum store.ReviewSummary,
	prior *store.AutoApproval, r *github.Review) {
	var a store.AutoApproval
	var err error
	if prior != nil {
		if err = e.st.TransitionAutoApproval(ctx, prior.ID, []string{store.AutoFailed}, store.AutoPosting, nil); err == nil {
			a = *prior
		}
	} else {
		a, err = e.st.InsertAutoApproval(ctx, store.AutoApproval{PRID: pr.ID, RunID: sum.RunID, SourceReviewID: sum.ReviewID,
			SourceURL: sum.URL, HeadSHA: pr.HeadSHA, Identity: id.Name, Login: id.Login})
	}
	if err != nil {
		e.log.Info("auto approval not adopted", "pr", pr.ID, "err", err)
		return
	}
	at := r.SubmittedAt
	if at.IsZero() {
		at = e.now()
	}
	data := map[string]any{"approval_id": a.ID, "head_sha": pr.HeadSHA, "identity": id.Name, "run_id": sum.RunID, "source_review_id": sum.ReviewID}
	e.autoPosted(ctx, repo, pr, a, r.DatabaseID, r.URL, at, sum, data, " (found on GitHub: the answer to its post was lost)")
}

// stopAutoApproval stops auto-approval of pr for good (until `magnum
// unapprove --resume`), with why, and writes review.auto_approve_stopped;
// a PR already stopped keeps its first reason.
func (e *Engine) stopAutoApproval(ctx context.Context, repo store.Repo, pr store.PR, why string) {
	if h, ok, err := e.st.AutoApproveHold(ctx, pr.ID); err != nil || (ok && h.Held) {
		return
	}
	if err := e.st.SetAutoApproveHold(ctx, store.AutoApproveHold{PRID: pr.ID, Held: true, Reason: why, At: e.now()}); err != nil {
		e.log.Warn("auto approval hold", "pr", pr.ID, "err", err)
		return
	}
	delete(e.autoSeen, pr.ID)
	e.event(ctx, "info", prSubject(repo, pr.Number), "review.auto_approve_stopped",
		fmt.Sprintf("magnum no longer approves %s#%d as you: %s (magnum unapprove --resume lifts it)", repo.FullName(), pr.Number, why),
		map[string]any{"reason": why})
}

// followAutoApproval follows the PR's live approval a: the approval of a
// merged or closed PR ends as history (closeAutoApproval); a post the
// daemon did not see answered is found on GitHub or counted failed; a
// withdrawal GitHub did not take is tried again after autoApproveRetry (at
// most autoApproveAttempts times); a standing one goes when a later round
// leaves something to fix, and is checked against GitHub (a dismissal, a
// review by hand) when the PR's updatedAt or latest round moved.
func (e *Engine) followAutoApproval(ctx context.Context, a store.AutoApprovalPR, sum *store.ReviewSummary, hold *store.AutoApproveHold) {
	repo := repoOf(a.Repo)
	if a.PR.GHState != store.GHOpen && (a.State == store.AutoStanding || a.State == store.AutoDismissing) {
		e.closeAutoApproval(ctx, repo, a)
		return
	}
	gh, ag := e.autoClient(e.cfg.IdentityByName(a.Identity))
	if ag == nil {
		return // its identity left the configuration: nothing to act as
	}
	switch a.State {
	case store.AutoPosting:
		e.resumeAutoPost(ctx, repo, a, gh)
	case store.AutoDismissing:
		if !e.now().Before(a.UpdatedAt.Add(autoApproveRetry)) {
			_ = e.withdrawAutoApproval(ctx, repo, a.PR, a.AutoApproval, gh, a.EndedBy, a.EndReason)
		}
	case store.AutoStanding:
		e.followStanding(ctx, repo, a, sum, hold, gh, ag)
	}
}

// closeAutoApproval ends live approval a of a merged or closed PR: nothing
// is left to follow or withdraw there (a withdrawal under way is dropped),
// and its row stops being read on every tick. It writes
// review.auto_approval_ended.
func (e *Engine) closeAutoApproval(ctx context.Context, repo store.Repo, a store.AutoApprovalPR) {
	what := strings.ToLower(a.PR.GHState)
	why := "the PR was " + what + ": the approval is history"
	if a.State == store.AutoDismissing {
		why = "the PR was " + what + " before its withdrawal went through"
	}
	e.endAutoApproval(ctx, repo, a.PR, a.AutoApproval, []string{store.AutoStanding, store.AutoDismissing}, store.AutoEndedClosed, why)
	delete(e.autoFollow, a.ID)
}

// resumeAutoPost settles a post a stopped daemon left without its answer:
// the approval GitHub has with magnum's marker on its head, else failed (a
// later tick tries again). A list GitHub cut short proves nothing: it is
// asked again on the next tick.
func (e *Engine) resumeAutoPost(ctx context.Context, repo store.Repo, a store.AutoApprovalPR, gh GitHub) {
	reviews, complete, err := gh.AllReviews(ctx, repo.Owner, repo.Name, a.PR.Number)
	if err != nil {
		return
	}
	w := operatorWord(reviews, a.Login, a.HeadSHA, time.Time{})
	if w.approved != nil && w.approvedByMagnum {
		data := map[string]any{"approval_id": a.ID, "head_sha": a.HeadSHA, "identity": a.Identity, "run_id": a.RunID}
		e.autoPosted(ctx, repo, a.PR, a.AutoApproval, w.approved.DatabaseID, w.approved.URL, w.approved.SubmittedAt,
			store.ReviewSummary{RunID: a.RunID}, data, " (found on GitHub after a restart)")
		return
	}
	if !complete {
		return
	}
	_ = e.st.TransitionAutoApproval(ctx, a.ID, []string{store.AutoPosting}, store.AutoFailed, func(u *store.AutoApprovalUpdate) {
		u.Set("error", "the daemon stopped before GitHub answered")
	})
}

// followStanding reads GitHub's reviews when the PR moved: a dismissal of
// standing approval a is recorded first (by the operator or someone else,
// auto-approval of the PR stops; by GitHub on a push, it does not), so a
// review GitHub already dismissed is never withdrawn again; then a is
// withdrawn when the PR's latest round is a later one that leaves something
// to fix (or cannot tell: earlier findings open, the verdict line unread or
// unknown), and a review of the operator's by hand since stops
// auto-approval (an approval or changes request of theirs also supersedes
// a). A list GitHub cut short still withdraws (the safe way), and tells
// nothing else until the PR moves.
func (e *Engine) followStanding(ctx context.Context, repo store.Repo, a store.AutoApprovalPR, sum *store.ReviewSummary,
	hold *store.AutoApproveHold, gh GitHub, ag autoApproveGitHub) {
	runID := ""
	if sum != nil {
		runID = sum.RunID
	}
	fp := runID + "|" + timeKey(a.PR.GHUpdatedAt)
	if e.autoFollow[a.ID] == fp {
		return
	}
	reviews, complete, err := gh.AllReviews(ctx, repo.Owner, repo.Name, a.PR.Number)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Info("auto approval: read the reviews", "pr", a.PRID, "err", err)
		}
		return
	}
	for _, r := range reviews {
		if r.DatabaseID == a.ReviewID && strings.EqualFold(r.State, "DISMISSED") {
			if e.dismissedOnGitHub(ctx, repo, a, ag) {
				delete(e.autoFollow, a.ID)
			}
			return
		}
	}
	if sum != nil && sum.RunID != a.RunID {
		if blocks, known := roundBlocks(*sum, reviewBody(reviews, sum.ReviewID)); blocks || !known {
			what := "found blocking problems"
			if !known {
				what = "left earlier findings open"
			}
			msg := fmt.Sprintf("magnum's review of %s %s; this automatic approval is withdrawn ([review](%s)).", textx.ShortSHA(sum.SHA), what, sum.URL)
			if err := e.withdrawAutoApproval(ctx, repo, a.PR, a.AutoApproval, gh, store.AutoEndedMagnum, msg); err == nil {
				delete(e.autoFollow, a.ID)
			}
			return
		}
	}
	if !complete {
		e.log.Info("auto approval: GitHub listed only part of the reviews; followed again when the PR moves", "pr", a.PRID)
		e.autoFollow[a.ID] = fp
		return
	}
	since := resumedSince(hold)
	if a.PostedAt != nil && a.PostedAt.After(since) {
		since = *a.PostedAt
	}
	if w := operatorWord(reviews, a.Login, a.PR.HeadSHA, since); w.hand != nil {
		e.stopAutoApproval(ctx, repo, a.PR, w.handReason())
		if s := strings.ToUpper(w.hand.State); s == "APPROVED" || s == "CHANGES_REQUESTED" {
			e.endAutoApproval(ctx, repo, a.PR, a.AutoApproval, []string{store.AutoStanding}, store.AutoEndedOperator,
				"superseded by your review by hand ("+w.hand.URL+")")
			delete(e.autoFollow, a.ID)
			return
		}
	}
	e.autoFollow[a.ID] = fp
}

// dismissedOnGitHub records standing approval a, which GitHub lists as
// dismissed though magnum did not dismiss it: who did (the PR's timeline),
// and a stop of auto-approval of the PR unless GitHub dismissed it as stale
// on a push. It reports whether it was recorded.
func (e *Engine) dismissedOnGitHub(ctx context.Context, repo store.Repo, a store.AutoApprovalPR, ag autoApproveGitHub) bool {
	ds, err := ag.ReviewDismissals(ctx, repo.Owner, repo.Name, a.PR.Number)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Info("auto approval: read the dismissals", "pr", a.PRID, "err", err)
		}
		return false
	}
	var d *github.ReviewDismissal
	for i := range ds {
		if ds[i].ReviewID == a.ReviewID {
			d = &ds[i]
		}
	}
	by, why := store.AutoEndedSomeone, "dismissed on GitHub (by whom, its timeline does not say)"
	switch {
	case d == nil:
	case d.ByPush:
		by, why = store.AutoEndedPush, "GitHub dismissed it as stale when "+textx.ShortSHA(d.Commit)+" was pushed"
	case github.SameAccount(d.Actor, a.Login):
		by, why = store.AutoEndedOperator, "you dismissed it"
	case d.Actor != "":
		why = d.Actor + " dismissed it"
	}
	if !e.endAutoApproval(ctx, repo, a.PR, a.AutoApproval, []string{store.AutoStanding}, by, why) {
		return false
	}
	if by != store.AutoEndedPush {
		e.stopAutoApproval(ctx, repo, a.PR, why+" (review "+fmt.Sprint(a.ReviewID)+")")
	}
	return true
}

// endAutoApproval records approval a, in one of the states from, as ended
// without magnum's withdrawal (by, why: on GitHub, or with its PR) with
// review.auto_approval_ended; it reports whether the row moved.
func (e *Engine) endAutoApproval(ctx context.Context, repo store.Repo, pr store.PR, a store.AutoApproval, from []string, by, why string) bool {
	if err := e.st.TransitionAutoApproval(ctx, a.ID, from, store.AutoDismissed, func(u *store.AutoApprovalUpdate) {
		u.Set("ended_by", by)
		u.Set("end_reason", why)
		u.Set("ended_at", e.now())
	}); err != nil {
		e.log.Info("auto approval moved on", "approval", a.ID, "err", err)
		return false
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "review.auto_approval_ended",
		fmt.Sprintf("automatic approval %d of %s by %s ended: %s", a.ReviewID, textx.ShortSHA(a.HeadSHA), a.Login, why),
		map[string]any{"approval_id": a.ID, "review_id": a.ReviewID, "ended_by": by})
	return true
}

// withdrawAutoApproval dismisses approval a as its identity with message:
// the row goes dismissing (by, message, its attempts from 0; a row already
// dismissing keeps its own), then dismissed once GitHub answers, no longer
// has the review, or refuses (422) a review it lists as dismissed already
// (withdrawnOnGitHub), with review.auto_approval_withdraw_begin,
// review.auto_approval_withdrawn or review.auto_approval_withdraw_failed. A
// withdrawal of magnum's own is toasted; one GitHub did not take stays
// dismissing, tried again after autoApproveRetry, and after
// autoApproveAttempts it ends failed with one urgent toast.
func (e *Engine) withdrawAutoApproval(ctx context.Context, repo store.Repo, pr store.PR, a store.AutoApproval, gh GitHub, by, message string) error {
	subject := prSubject(repo, pr.Number)
	if a.State == store.AutoStanding {
		if err := e.st.TransitionAutoApproval(ctx, a.ID, []string{store.AutoStanding}, store.AutoDismissing, func(u *store.AutoApprovalUpdate) {
			u.Set("ended_by", by)
			u.Set("end_reason", message)
			u.Set("attempts", 0)
		}); err != nil {
			return fmt.Errorf("automatic approval %d moved on: %w", a.ID, err)
		}
		a.State, a.Attempts = store.AutoDismissing, 0
	}
	attempt := a.Attempts + 1
	data := map[string]any{"approval_id": a.ID, "review_id": a.ReviewID, "head_sha": a.HeadSHA, "identity": a.Identity, "by": by, "attempt": attempt}
	e.event(ctx, "info", subject, "review.auto_approval_withdraw_begin",
		fmt.Sprintf("withdrawing approval %d of %s by %s (attempt %d): %s", a.ReviewID, textx.ShortSHA(a.HeadSHA), a.Login, attempt, message), data)
	err := gh.DismissReview(ctx, repo.Owner, repo.Name, pr.Number, a.ReviewID, message)
	ended, how := by, ""
	var apiErr *github.APIError
	switch {
	case err == nil:
	case errors.Is(err, github.ErrNotFound):
		ended, err = store.AutoEndedGone, nil
	case errors.As(err, &apiErr) && apiErr.Status == 422:
		switch e.withdrawnOnGitHub(ctx, gh, repo, pr.Number, a.ReviewID) {
		case store.AutoEndedGone:
			ended, how, err = store.AutoEndedGone, " (GitHub no longer has it)", nil
		case store.AutoDismissed:
			how, err = " (GitHub lists it as dismissed already)", nil
		}
	}
	if err != nil {
		return e.withdrawFailed(ctx, repo, pr, a, attempt, err, data)
	}
	if uerr := e.st.TransitionAutoApproval(ctx, a.ID, []string{store.AutoDismissing}, store.AutoDismissed, func(u *store.AutoApprovalUpdate) {
		u.Set("ended_by", ended)
		u.Set("ended_at", e.now())
		u.Set("error", nil)
	}); uerr != nil {
		e.log.Warn("auto approval withdrawn and not recorded", "approval", a.ID, "err", uerr)
	}
	e.event(ctx, "info", subject, "review.auto_approval_withdrawn",
		fmt.Sprintf("withdrew approval %d of %s by %s%s: %s", a.ReviewID, textx.ShortSHA(a.HeadSHA), a.Login, how, message), data)
	if by == store.AutoEndedMagnum {
		label := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
		e.info(notify.Item{Key: fmt.Sprintf("auto-withdrawn:%d", a.ID), Title: "magnum: withdrew your approval: " + label,
			Body: pr.URL + "\n" + message, Line: "withdrew your approval: " + label + " " + pr.URL, Kind: kindAutoWithdrawn, Window: autoToastWindow})
	}
	return nil
}

// withdrawnOnGitHub reads review id of PR number again after GitHub
// refused its dismissal: store.AutoDismissed when GitHub lists it as
// dismissed (someone or a push did it meanwhile), store.AutoEndedGone when
// a whole list lacks it, "" when it still stands or GitHub cannot say.
func (e *Engine) withdrawnOnGitHub(ctx context.Context, gh GitHub, repo store.Repo, number int, id int64) string {
	reviews, complete, err := gh.AllReviews(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return ""
	}
	for _, r := range reviews {
		if r.DatabaseID == id {
			if strings.EqualFold(r.State, "DISMISSED") {
				return store.AutoDismissed
			}
			return ""
		}
	}
	if complete {
		return store.AutoEndedGone
	}
	return ""
}

// withdrawFailed records the attempt-th dismissal of approval a that
// GitHub did not take (err): the row stays dismissing, tried again after
// autoApproveRetry, until autoApproveAttempts, when it ends failed with one
// urgent toast (the approval may still stand: the operator dismisses it).
// It returns err.
func (e *Engine) withdrawFailed(ctx context.Context, repo store.Repo, pr store.PR, a store.AutoApproval, attempt int, err error,
	data map[string]any) error {
	final := attempt >= autoApproveAttempts
	to := "" // stays dismissing
	if final {
		to = store.AutoFailed
	}
	if uerr := e.st.TransitionAutoApproval(ctx, a.ID, []string{store.AutoDismissing}, to, func(u *store.AutoApprovalUpdate) {
		u.Set("error", err.Error())
		u.Set("attempts", attempt)
		if final {
			u.Set("ended_at", e.now())
		}
	}); uerr != nil {
		e.log.Warn("auto approval withdrawal failed and not recorded", "approval", a.ID, "err", uerr)
	}
	subject := prSubject(repo, pr.Number)
	if !final {
		e.event(ctx, "warn", subject, "review.auto_approval_withdraw_failed", fmt.Sprintf("could not withdraw approval %d by %s: %v; tried again in %s "+
			"(attempt %d of %d)", a.ReviewID, a.Login, err, humanDuration(autoApproveRetry), attempt, autoApproveAttempts), data)
		return err
	}
	e.event(ctx, "warn", subject, "review.auto_approval_withdraw_failed", fmt.Sprintf("could not withdraw approval %d by %s: %v; gave up after %d "+
		"attempts: it may still stand, dismiss it on GitHub", a.ReviewID, a.Login, err, attempt), data)
	label := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
	e.urgent(fmt.Sprintf("auto-withdraw-failed:%d", a.ID), "magnum: could not withdraw your approval of "+label,
		fmt.Sprintf("%s\n%v. magnum gave up after %d attempts: dismiss it on GitHub.", pr.URL, err, attempt), autoToastWindow)
	return err
}

// requestUnapprove is `magnum unapprove` and the board's D: it stops
// auto-approval of the PR and withdraws its standing automatic approval as
// the operator; with Resume it lifts the stop instead (the operator's
// reviews before now no longer count).
func (e *Engine) requestUnapprove(ctx context.Context, p UnapprovePayload) (string, error) {
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	e.autoMu.Lock() // a round's goroutine may be deciding about the PR (autoApproveRound)
	defer e.autoMu.Unlock()
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	subject := prSubject(repo, pr.Number)
	if p.Resume {
		if err := e.st.SetAutoApproveHold(ctx, store.AutoApproveHold{PRID: pr.ID, Held: false, Reason: "resumed with magnum unapprove --resume",
			At: e.now()}); err != nil {
			return "", err
		}
		delete(e.autoSeen, pr.ID)
		e.event(ctx, "info", subject, "review.auto_approve_resumed", "auto-approval resumed (magnum unapprove --resume)", nil)
		return fmt.Sprintf("auto-approval of %s resumed: magnum may approve it as you after a clean review (your reviews before now no longer stop it)", label), nil
	}
	live, ok, err := e.st.LiveAutoApproval(ctx, pr.ID)
	if err != nil {
		return "", err
	}
	e.stopAutoApproval(ctx, repo, pr, "withdrawn with magnum unapprove")
	if !ok {
		return fmt.Sprintf("no automatic approval of %s stands; magnum will not approve it as you (magnum unapprove --resume %s undoes that)", label, label), nil
	}
	switch live.State {
	case store.AutoPosting:
		return "", fmt.Errorf("the automatic approval of %s is being posted: try again in a moment", label)
	case store.AutoDismissing:
		return fmt.Sprintf("the automatic approval of %s is already being withdrawn; magnum will not approve it as you again", label), nil
	}
	gh, _ := e.autoClient(e.cfg.IdentityByName(live.Identity))
	if gh == nil {
		return "", fmt.Errorf("identity %q has no GitHub client: dismiss review %d of %s on GitHub", live.Identity, live.ReviewID, label)
	}
	if err := e.withdrawAutoApproval(ctx, repo, pr, live, gh, store.AutoEndedOperator, UnapproveMessage); err != nil {
		return "", fmt.Errorf("withdraw the automatic approval of %s: %w (magnum tries again)", label, err)
	}
	return fmt.Sprintf("withdrew the automatic approval of %s (review %d by %s); magnum will not approve it as you again", label, live.ReviewID, live.Login), nil
}
