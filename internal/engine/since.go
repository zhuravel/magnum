package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// maxComparesPerRepo caps the Compare calls one repository's poll makes; PRs
// over the cap keep their previous since_review until a later poll.
const maxComparesPerRepo = 20

// sinceBase picks what since_review_json measures from: magnum's last
// verified review (reviewed_sha), else the commit of the PR identity's own
// latest review on GitHub, else the base branch tip (the whole PR).
// refreshSinceReview looks further (ownReviewCommit) when GitHub cut the
// latest-review list short.
func (e *Engine) sinceBase(pr store.PR) (source, base string) {
	if rs := deref(pr.ReviewedSHA); rs != "" {
		return store.SinceFromReviewed, rs
	}
	login := e.reviewerLogin(pr.Identity)
	for _, r := range pr.LatestReviews {
		if r.CommitSHA != "" && r.State != "PENDING" && github.SameAccount(r.Login, login) {
			return store.SinceFromReview, r.CommitSHA
		}
	}
	return store.SinceFromBase, deref(pr.BaseSHA)
}

// refreshSinceReview keeps prs.since_review_json in step with the PR's
// (review base, head). A reviewed base costs one comparison per new pair,
// none when the poll's re-review gate compared it already (a pair GitHub
// cannot compare is stored with Error, so it is not retried): the shared
// measure (measureRange), so a range that merged the base branch in or was
// rebased counts only the PR's own commits, files and lines (two more
// calls, or none after the gate's), and the raw comparison's files and
// lines when the PR's own diff cannot be compared in full, its commits
// still the PR's own when both sides were read (rangeMeasure.commits). A PR
// nothing reviewed yet takes its whole size from the Details fetched in
// this poll (d, nil when none), which need no extra call. budget is the
// repository's remaining comparison allowance for this poll (one per
// pair); a failed comparison other than not-found spends the rest of it. A
// size measured by an older rule (store.SinceReviewVersion) is measured
// again once.
func (e *Engine) refreshSinceReview(ctx context.Context, gh GitHub, repo store.Repo, pr store.PR, d *github.PRDetails, budget *int) {
	source, base := e.sinceBase(pr)
	if source == store.SinceFromBase && d != nil && !d.LatestReviewsComplete {
		// The identity's own review may be past the latestReviews page.
		if sha := e.ownReviewCommit(ctx, gh, repo, pr, budget); sha != "" {
			source, base = store.SinceFromReview, sha
		}
	}
	cur := pr.SinceReview
	next := store.SinceReview{Source: source, Base: base, Head: pr.HeadSHA, ComputedAt: e.now()}
	switch {
	case source == store.SinceFromBase:
		if d == nil || d.HeadRefOid != pr.HeadSHA || d.BaseRefOid == "" {
			return // sized by the next Details fetch
		}
		next.Base = d.BaseRefOid
		next.Commits, next.Files, next.Additions, next.Deletions = d.Commits, d.ChangedFiles, d.Additions, d.Deletions
		if cur != nil && sameSince(*cur, next) {
			return
		}
	case cur != nil && cur.Source == source && cur.Base == base && cur.Head == pr.HeadSHA && cur.Version >= store.SinceReviewVersion:
		return // this pair is already measured
	case base == pr.HeadSHA:
		// The head is what was reviewed: nothing new.
		next.Version = store.SinceReviewVersion
	default:
		if *budget <= 0 {
			return
		}
		*budget--
		branch := prBase(repo, pr)
		m, err := e.measureRange(ctx, gh, repo, branch, base, pr.HeadSHA) // the gate's comparisons of this poll serve it
		next.Version = store.SinceReviewVersion
		switch {
		case errors.Is(err, github.ErrNotFound):
			next.Error = fmt.Sprintf("GitHub cannot compare %s...%s (a commit is gone)", textx.ShortSHA(base), textx.ShortSHA(pr.HeadSHA))
		case err != nil:
			*budget = 0
			if e.changed("compare:"+repo.FullName(), err.Error()) {
				e.event(ctx, "warn", "repo:"+repo.FullName(), "poll.compare_error", fmt.Sprintf("since-review size of #%d: %v", pr.Number, err), nil)
			}
			return
		default:
			e.changed("compare:"+repo.FullName(), "")
			cs := m.push.Stats
			next.Commits, next.Files, next.Additions, next.Deletions = m.commits(), cs.Files, cs.Additions, cs.Deletions
			switch {
			case m.ownOK:
				next.Files, next.Additions, next.Deletions = len(m.own.changed), m.own.additions, m.own.deletions
				next.BaseMerged, next.BaseRef = true, branch
			case viaBase(m.push):
				next.Raw, next.BaseRef = true, branch
			}
		}
	}
	if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("since_review_json", next) }); err != nil {
		e.log.Warn("store since review", "pr", pr.ID, "err", err)
	}
}

// ownReviewCommit is the commit of the PR identity's newest review among the
// PR's last 30 reviews (ReviewsWithMarker without a marker), "" when there is
// none. It spends one call of the repository's Compare budget; a failure is
// reported once per distinct error.
func (e *Engine) ownReviewCommit(ctx context.Context, gh GitHub, repo store.Repo, pr store.PR, budget *int) string {
	if *budget <= 0 {
		return ""
	}
	*budget--
	reviews, err := gh.ReviewsWithMarker(ctx, repo.Owner, repo.Name, pr.Number, "")
	key := "own-review:" + repo.FullName()
	if err != nil {
		if e.changed(key, err.Error()) {
			e.event(ctx, "warn", "repo:"+repo.FullName(), "poll.reviews_error", fmt.Sprintf("own review of #%d: %v", pr.Number, err), nil)
		}
		return ""
	}
	e.changed(key, "")
	login := e.reviewerLogin(pr.Identity)
	for _, r := range slices.Backward(reviews) {
		if r.CommitOid != "" && r.State != "PENDING" && github.SameAccount(github.Account(r.AuthorLogin, r.AuthorType), login) {
			return r.CommitOid
		}
	}
	return ""
}

// sameSince compares everything but ComputedAt.
func sameSince(a, b store.SinceReview) bool {
	a.ComputedAt = b.ComputedAt
	return a == b
}
