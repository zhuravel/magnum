package store

// Pull requests magnum approved that still need the operator: GitHub never
// counts a GitHub App's approval toward a branch's required approvals, so a
// PR magnum's App approved can still be blocked on the operator, whose
// approval is the one that counts, or on their own earlier changes request.

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// ReviewGate is prs.review_gate_json (migration 0019): what GitHub's branch
// protection made of a PR's reviews at the last Details fetch.
type ReviewGate struct {
	// Decision is GitHub's reviewDecision: APPROVED, CHANGES_REQUESTED or
	// REVIEW_REQUIRED; "" when the base branch requires no review.
	Decision string `json:"decision"`
	// Opinions are the latest approval or changes request (DISMISSED once
	// dismissed) of each reviewer with write access, which Decision counts
	// (GitHub's latestOpinionatedReviews, writersOnly); Login in Account
	// form, SubmittedAt not read.
	Opinions []LatestReview `json:"opinions"`
	// Complete is true when Opinions is every one (GitHub returns at most
	// 100).
	Complete bool `json:"complete"`
}

func (g ReviewGate) equal(o ReviewGate) bool {
	return g.Decision == o.Decision && g.Complete == o.Complete && slices.EqualFunc(g.Opinions, o.Opinions, LatestReview.equal)
}

// NeedsMe values: what a PR magnum approved waits for from the operator.
const (
	// NeedsMeApprove: GitHub still requires an approval that counts
	// (REVIEW_REQUIRED), which the operator's would give.
	NeedsMeApprove = "approve"
	// NeedsMeLift: the operator's own changes request is the only one
	// blocking the PR (CHANGES_REQUESTED): their approval, or a dismissal,
	// lifts it.
	NeedsMeLift = "lift"
)

// NeedsMeFacts are what NeedsMe decides from: the PR as the registry has it,
// and what the configuration and magnum's latest round say of its review.
type NeedsMeFacts struct {
	GHState string // prs.gh_state
	Draft   bool
	Author  string // the author's login ("" for a ghost)
	// HeadSHA is the PR's head and ReviewedSHA the commit magnum's latest
	// verified review stands for (prs.reviewed_sha).
	HeadSHA, ReviewedSHA string
	// LastReviewEvent is what magnum's latest review posted
	// (prs.last_review_event): APPROVED, COMMENTED, CHANGES_REQUESTED or
	// DISMISSED.
	LastReviewEvent string
	// CommentWhenClean: the PR's identity posts a comment, not an approval,
	// for a clean verdict on its repository ([[identity]] or [[repo]]
	// no_findings_event = "COMMENT"), so a clean comment is its approval.
	CommentWhenClean bool
	// Verdict is the verdict of magnum's latest posted round
	// (ReviewSummary.Verdict); only a comment reads it.
	Verdict string
	Gate    *ReviewGate // nil until the Details read it
}

// WantsVerdict reports whether NeedsMe reads f.Verdict: magnum's latest
// review is a comment from an identity that comments when clean.
func (f NeedsMeFacts) WantsVerdict() bool {
	return f.CommentWhenClean && reviewEvent(f.LastReviewEvent) == "COMMENTED"
}

// NeedsMe is what the operator's approval would fix on a PR magnum approved,
// "" when nothing: an open PR, not a draft and not authored by one of the
// operator's logins (mine), whose latest verified magnum review is on the
// current head and clean (an approval, or a clean comment from an identity
// that comments when clean), and which GitHub still blocks on a review:
// NeedsMeApprove for REVIEW_REQUIRED, NeedsMeLift for CHANGES_REQUESTED when
// every outstanding changes request is the operator's. Anyone else's changes
// request, an unread gate or a list of opinions GitHub cut leaves it alone.
func NeedsMe(f NeedsMeFacts, mine func(login string) bool) string {
	if f.GHState != GHOpen || f.Draft || f.Gate == nil || (f.Author != "" && mine(f.Author)) {
		return ""
	}
	if f.HeadSHA == "" || f.ReviewedSHA != f.HeadSHA {
		return ""
	}
	switch reviewEvent(f.LastReviewEvent) {
	case "APPROVED":
	case "COMMENTED":
		if !f.CommentWhenClean || f.Verdict != VerdictClean {
			return ""
		}
	default:
		return ""
	}
	switch f.Gate.Decision {
	case "REVIEW_REQUIRED":
		return NeedsMeApprove
	case "CHANGES_REQUESTED":
		if !f.Gate.Complete {
			return ""
		}
		lift := false
		for _, o := range f.Gate.Opinions {
			if o.State != "CHANGES_REQUESTED" {
				continue
			}
			if o.Login == "" || !mine(o.Login) {
				return ""
			}
			lift = true
		}
		if lift {
			return NeedsMeLift
		}
	}
	return ""
}

// reviewEvent is a review event in GitHub's past-tense form: APPROVED,
// COMMENTED, CHANGES_REQUESTED, DISMISSED (the posted APPROVE, COMMENT and
// REQUEST_CHANGES too).
func reviewEvent(e string) string {
	e = strings.ToUpper(strings.TrimSpace(e))
	switch e {
	case "APPROVE":
		return "APPROVED"
	case "COMMENT":
		return "COMMENTED"
	case "REQUEST_CHANGES":
		return "CHANGES_REQUESTED"
	}
	return e
}

// NeedsMeFacts are p's facts for NeedsMe; the caller adds CommentWhenClean
// and, when it WantsVerdict, Verdict.
func (p PR) NeedsMeFacts() NeedsMeFacts {
	return NeedsMeFacts{GHState: p.GHState, Draft: p.IsDraft, Author: Deref(p.AuthorLogin), HeadSHA: p.HeadSHA,
		ReviewedSHA: Deref(p.ReviewedSHA), LastReviewEvent: Deref(p.LastReviewEvent), Gate: p.ReviewGate}
}

// NeedsMeFacts are b's facts for NeedsMe; the caller adds CommentWhenClean
// and, when it WantsVerdict, Verdict.
func (b BoardRow) NeedsMeFacts() NeedsMeFacts {
	return NeedsMeFacts{GHState: b.GHState, Draft: b.Draft, Author: b.Author, HeadSHA: b.HeadSHA,
		ReviewedSHA: b.ReviewedSHA, LastReviewEvent: b.LastReviewEvent, Gate: b.ReviewGate}
}

// NeedsMePR is an open PR that needs the operator.
type NeedsMePR struct {
	PR      PR
	Repo    string // owner/name
	NeedsMe string // NeedsMeApprove or NeedsMeLift
}

// NeedsMePRs lists the open PRs that need the operator (NeedsMe), by
// repository and number. commentsWhenClean tells whether an identity posts
// a comment for a clean verdict on a repository (owner/name); mine whether a
// login is one of the operator's. It reads magnum's latest round only for a
// PR whose last review is such a comment.
func (s *Store) NeedsMePRs(ctx context.Context, commentsWhenClean func(repo, identity string) bool, mine func(login string) bool) ([]NeedsMePR, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("p", prColumns)+`, r.owner || '/' || r.name
FROM prs p JOIN repos r ON r.id = p.repo_id
WHERE p.gh_state = ? AND p.is_draft = 0 AND p.review_gate_json IS NOT NULL AND p.reviewed_sha = p.head_sha
ORDER BY r.owner, r.name, p.number`, GHOpen)
	if err != nil {
		return nil, fmt.Errorf("needs me: %w", err)
	}
	type cand struct {
		pr    PR
		repo  string
		facts NeedsMeFacts
	}
	list, err := collect(rows, func(sc scanner) (cand, error) {
		var c cand
		var repo string
		p, err := scanPR(rowWithTail{sc, &repo})
		c.pr, c.repo = p, repo
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("needs me: %w", err)
	}
	var ids []int64
	for i := range list {
		c := &list[i]
		c.facts = c.pr.NeedsMeFacts()
		c.facts.CommentWhenClean = commentsWhenClean != nil && commentsWhenClean(c.repo, c.pr.Identity)
		if c.facts.WantsVerdict() {
			ids = append(ids, c.pr.ID)
		}
	}
	sums, err := s.LastReviewSummaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []NeedsMePR
	for _, c := range list {
		c.facts.Verdict = sums[c.pr.ID].Verdict
		if n := NeedsMe(c.facts, mine); n != "" {
			out = append(out, NeedsMePR{PR: c.pr, Repo: c.repo, NeedsMe: n})
		}
	}
	return out, nil
}

// rowWithTail scans a row of prColumns followed by more columns into tail.
type rowWithTail struct {
	sc   scanner
	tail *string
}

func (r rowWithTail) Scan(dest ...any) error { return r.sc.Scan(append(dest, r.tail)...) }
