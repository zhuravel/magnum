package engine

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

func TestPollStoresBoardFieldsAndWholePRSize(t *testing.T) {
	h := newHarness(t)
	sub := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	h.open(prSpec{n: 1, head: "a1", assignees: []string{"zhuravel", "dependabot[bot]"}, request: true,
		reviews: []github.LatestReview{
			{State: "APPROVED", SubmittedAt: sub, AuthorLogin: "rev-ann", AuthorType: "User", CommitOid: "a0"},
			{State: "PENDING", AuthorLogin: "zhuravel", AuthorType: "User"},
		}})
	h.startup()
	h.tick()

	pr := h.wantState(1, store.PRBaseline)
	now := h.clock.Now()
	if !reflect.DeepEqual(pr.Assignees, []string{"zhuravel", "dependabot"}) ||
		!reflect.DeepEqual(pr.RequestedReviewers, []string{"zhuravel", "team:engineers"}) || !pr.ReviewRequested {
		t.Fatalf("people: assignees %v requested %v (%v)", pr.Assignees, pr.RequestedReviewers, pr.ReviewRequested)
	}
	wantReviews := []store.LatestReview{{Login: "rev-ann", State: "APPROVED", SubmittedAt: &sub, CommitSHA: "a0"},
		{Login: "zhuravel", State: "PENDING"}}
	if len(pr.LatestReviews) != 2 || pr.LatestReviews[0].Login != "rev-ann" || !pr.LatestReviews[0].SubmittedAt.Equal(sub) ||
		pr.LatestReviews[0].CommitSHA != "a0" || pr.LatestReviews[1] != wantReviews[1] {
		t.Fatalf("latest reviews = %+v", pr.LatestReviews)
	}
	if deref(pr.BaseSHA) != fakeBaseOid || pr.DetailsAt == nil || !pr.DetailsAt.Equal(now) {
		t.Fatalf("base %v details_at %v", pr.BaseSHA, pr.DetailsAt)
	}
	// Nothing reviewed by the PR's identity: the whole PR, sized by Details, no Compare.
	want := store.SinceReview{Source: store.SinceFromBase, Base: fakeBaseOid, Head: "a1", Commits: 1, Files: 1,
		Additions: 10, Deletions: 1, ComputedAt: now}
	if pr.SinceReview == nil || !equalSince(*pr.SinceReview, want) {
		t.Fatalf("since review = %+v, want %+v", pr.SinceReview, want)
	}
	if n := h.gh.count("compare:"); n != 0 {
		t.Fatalf("compare calls = %d", n)
	}

	// An unchanged PR is not re-fetched and its since_review stays.
	h.advance(time.Minute)
	h.tick()
	if n := h.gh.count("details:"); n != 1 {
		t.Fatalf("details calls = %d", n)
	}
	if got := h.pr(1); !got.SinceReview.ComputedAt.Equal(now) {
		t.Fatalf("since review rewritten: %+v", got.SinceReview)
	}
}

func TestSinceReviewFromIdentityReviewOnGitHub(t *testing.T) {
	h := newHarness(t)
	sub := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	// The App's earlier review (GraphQL login "talkable" for talkable[bot]).
	spec := prSpec{n: 1, head: "a1", reviews: []github.LatestReview{
		{State: "COMMENTED", SubmittedAt: sub, AuthorLogin: "talkable", AuthorType: "Bot", CommitOid: "a0"}}}
	h.gh.compare["a0...a1"] = github.CompareStats{Commits: 2, Files: -1, Additions: 30, Deletions: 4}
	h.open(spec)
	h.startup()
	h.tick()
	pr := h.pr(1)
	if pr.SinceReview == nil || pr.SinceReview.Source != store.SinceFromReview || pr.SinceReview.Base != "a0" ||
		pr.SinceReview.Head != "a1" || pr.SinceReview.Files != -1 || pr.SinceReview.Additions != 30 || pr.SinceReview.Commits != 2 {
		t.Fatalf("since review = %+v", pr.SinceReview)
	}
	// GitHub's updatedAt moves (a comment), the head does not: Details again, no second Compare.
	h.advance(time.Minute)
	spec.updated = h.clock.Now()
	spec.assignees = []string{"rev-ann"}
	h.open(spec)
	h.tick()
	if d, c := h.gh.count("details:"), h.gh.count("compare:"); d != 2 || c != 1 {
		t.Fatalf("details %d compare %d", d, c)
	}
	if got := h.pr(1); !reflect.DeepEqual(got.Assignees, []string{"rev-ann"}) {
		t.Fatalf("assignees not refreshed: %v", got.Assignees)
	}
}

func TestSinceReviewFollowsReviewedSHA(t *testing.T) {
	h := newHarness(t)
	reviewed := h.reviewedPR(2, "b1")
	if deref(reviewed.LastReviewLogin) != "talkable[bot]" {
		t.Fatalf("last_review_login = %v, want the App login with [bot]", reviewed.LastReviewLogin)
	}

	// The next poll sees reviewed_sha == head: nothing new, no Compare.
	h.advance(time.Minute)
	h.tick()
	pr := h.pr(2)
	if s := pr.SinceReview; s == nil || s.Source != store.SinceFromReviewed || s.Base != "b1" || s.Head != "b1" ||
		s.Commits != 0 || s.Files != 0 || s.Error != "" {
		t.Fatalf("since review after the review = %+v", s)
	}

	// A push: one Compare reviewed_sha...head.
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 1, Files: 2, Additions: 7, Deletions: 3}
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	pr = h.wantState(2, store.PRRereviewPending)
	want := store.SinceReview{Source: store.SinceFromReviewed, Base: "b1", Head: "b2", Commits: 1, Files: 2, Additions: 7,
		Deletions: 3, ComputedAt: h.clock.Now()}
	if pr.SinceReview == nil || !equalSince(*pr.SinceReview, want) {
		t.Fatalf("since review = %+v, want %+v", pr.SinceReview, want)
	}
	// Further polls (unchanged, or updated without a push) never compare the same pair again.
	h.advance(time.Minute)
	h.tick()
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", labels: []string{"x"}})
	h.tick()
	if n := h.gh.count("compare:"); n != 1 {
		t.Fatalf("compare calls = %d, want 1", n)
	}

	// A pair GitHub cannot compare is stored with an error and not retried.
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b3"})
	h.tick()
	h.advance(time.Minute)
	h.tick()
	pr = h.pr(2)
	if s := pr.SinceReview; s == nil || s.Head != "b3" || s.Error == "" || s.Commits != 0 {
		t.Fatalf("not-found compare = %+v", s)
	}
	if n := h.gh.count("compare:"); n != 2 {
		t.Fatalf("compare calls = %d, want 2", n)
	}
}

func TestCompareFailureSpendsTheBudgetAndRetries(t *testing.T) {
	h := newHarness(t)
	sub := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	byApp := func(commit string) []github.LatestReview {
		return []github.LatestReview{{State: "APPROVED", SubmittedAt: sub, AuthorLogin: "talkable", AuthorType: "Bot", CommitOid: commit}}
	}
	h.gh.compareErr = &github.APIError{Op: "compare", Status: 502, Message: "Bad Gateway"}
	h.open(prSpec{n: 1, head: "a1", reviews: byApp("a0")}, prSpec{n: 2, head: "c1", reviews: byApp("c0")})
	h.startup()
	h.tick()
	if n := h.gh.count("compare:"); n != 1 {
		t.Fatalf("compare calls after a failure = %d, want 1 (the rest of the budget is spent)", n)
	}
	if h.pr(1).SinceReview != nil || h.pr(2).SinceReview != nil {
		t.Fatal("a failed compare must not store a since_review")
	}

	h.gh.mu.Lock()
	h.gh.compareErr = nil
	h.gh.compare["a0...a1"] = github.CompareStats{Commits: 1, Files: 1, Additions: 1}
	h.gh.compare["c0...c1"] = github.CompareStats{Commits: 2, Files: 2, Additions: 2}
	h.gh.mu.Unlock()
	h.advance(time.Minute)
	h.tick()
	if n := h.gh.count("compare:"); n != 3 {
		t.Fatalf("compare calls = %d, want 3", n)
	}
	if a, c := h.pr(1).SinceReview, h.pr(2).SinceReview; a == nil || a.Commits != 1 || c == nil || c.Commits != 2 {
		t.Fatalf("retried since reviews = %+v / %+v", a, c)
	}
}

func TestPRsWithoutDetailsAtAreFetchedOnce(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1", assignees: []string{"rev-ann"}})
	h.startup()
	h.tick()
	// What a registry migrated from schema 1 looks like.
	if _, err := h.st.DB().ExecContext(h.ctx, "UPDATE prs SET details_at = NULL, since_review_json = NULL, assignees_json = '[]' WHERE number = 2"); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	h.tick()
	h.advance(time.Minute)
	h.tick()
	if n := h.gh.count("details:talkable/talkable:[2]"); n != 1 {
		t.Fatalf("backfill details calls = %d (calls %v)", n, h.gh.calls)
	}
	pr := h.wantState(2, store.PRBaseline)
	if pr.DetailsAt == nil || !reflect.DeepEqual(pr.Assignees, []string{"rev-ann"}) || pr.SinceReview == nil {
		t.Fatalf("backfilled PR = %+v", pr)
	}
}

// TestDepartedAuthorsAreNotReviewed: a branch PR whose author is no longer a
// member or collaborator (they left the organization) is ineligible when it
// appears and when an old one is pushed to; a PR recorded before the column
// existed gets its association from one Details fetch.
func TestDepartedAuthorsAreNotReviewed(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1", author: "former-dev", assoc: "CONTRIBUTOR"})
	h.startup()
	h.tick() // first sync: baseline
	if pr := h.wantState(1, store.PRBaseline); deref(pr.AuthorAssociation) != "CONTRIBUTOR" {
		t.Fatalf("association %q", deref(pr.AuthorAssociation))
	}

	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "a2", author: "former-dev", assoc: "CONTRIBUTOR"},
		prSpec{n: 2, head: "b1", author: "gone-dev", assoc: "FIRST_TIME_CONTRIBUTOR"},
		prSpec{n: 3, head: "c1", author: "outside", assoc: "COLLABORATOR"})
	h.tick()
	for _, n := range []int{1, 2} {
		pr := h.wantState(n, store.PRIneligible)
		if r := deref(pr.SkipReason); !strings.Contains(r, "skip_departed_authors") || !strings.Contains(r, "left talkable") {
			t.Fatalf("#%d skip reason %q", n, r)
		}
	}
	h.wantState(3, store.PRQueued) // a collaborator has access

	// A registry from before the column: one Details fetch backfills it.
	if _, err := h.st.DB().ExecContext(h.ctx, "UPDATE prs SET author_association = NULL WHERE number = 3"); err != nil {
		t.Fatal(err)
	}
	before := h.gh.count("details:talkable/talkable:[3]")
	for range 3 {
		h.advance(time.Minute)
		h.tick()
	}
	if n := h.gh.count("details:talkable/talkable:[3]") - before; n != 1 {
		t.Fatalf("backfill details calls = %d", n)
	}
	if pr := h.pr(3); deref(pr.AuthorAssociation) != "COLLABORATOR" {
		t.Fatalf("backfilled association %q", deref(pr.AuthorAssociation))
	}
}

// equalSince compares two SinceReview values with ComputedAt as an instant:
// a time read back from the registry's JSON is in UTC, the test clock's in
// the local zone, which DeepEqual tells apart when the zone is UTC.
func equalSince(a, b store.SinceReview) bool {
	if !a.ComputedAt.Equal(b.ComputedAt) {
		return false
	}
	a.ComputedAt, b.ComputedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}
