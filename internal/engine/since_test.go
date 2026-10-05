package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// What changed since the review counts the PR's own changes across a merge
// of master (the board's SINCE REVIEW and the card): its own commits, the
// file whose own change differs and that change's lines, marked
// base_merged, from the comparisons the re-review gate made (no call more).
func TestSinceReviewCountsThePRsOwnChangesAcrossABaseMerge(t *testing.T) {
	h := newHarness(t)
	h.gh.shas = map[string][]string{"master..." + reviewedTip: {"c1", "c2"}, "master..." + mergedHead: {"c1", "c2", "c3", "mc"}}
	h.gh.compare["master..."+reviewedTip] = github.CompareStats{Commits: 2}
	h.gh.compare["master..."+mergedHead] = github.CompareStats{Commits: 4}
	pr := baseMergePush(t, h, true, "ahead", ownBefore, ownAfterConflict)
	want := store.SinceReview{Source: store.SinceFromReviewed, Base: reviewedTip, Head: mergedHead, Commits: 2, Files: 1,
		Additions: 1, Deletions: 1, BaseMerged: true, BaseRef: "master", Version: store.SinceReviewVersion, ComputedAt: h.clock.Now()}
	if pr.SinceReview == nil || !equalSince(*pr.SinceReview, want) {
		t.Fatalf("since review = %+v, want %+v", pr.SinceReview, want)
	}
	if got := rangeCalls(h.gh, reviewedTip, mergedHead); len(got) != 1 {
		t.Fatalf("compares of the push = %q, want one", got)
	}
	if got := ownDiffCalls(h); len(got) != 2 {
		t.Fatalf("own diff compares = %q, want one per side", got)
	}
}

// When the PR's own diff cannot be compared in full, the size is the push's
// comparison, master's changes included, and says so (raw).
func TestSinceReviewKeepsTheRawSizeWhenTheOwnDiffIsIncomplete(t *testing.T) {
	truncatedAfter := slices.Clone(ownAfterConflict)
	truncatedAfter[0].Truncated = true
	h := newHarness(t)
	pr := baseMergePush(t, h, true, "ahead", ownBefore, truncatedAfter)
	s := pr.SinceReview
	if s == nil || s.Commits != 13 || !s.Raw || s.BaseMerged || s.BaseRef != "master" {
		t.Fatalf("since review = %+v, want the raw 13 commits marked raw", s)
	}
}

// A plain push is sized as before: the comparison's own counts.
func TestSinceReviewOfAPlainPushIsTheComparison(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	reqSetFiles(h, "b1...b2", rubyMixed)
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 1, Files: 1, Additions: 1, Deletions: 1}
	pollPR(h, time.Minute, 2, "b2")
	s := h.pr(2).SinceReview
	if s == nil || s.Commits != 1 || s.Files != 1 || s.Additions != 1 || s.Deletions != 1 || s.Raw || s.BaseMerged || s.BaseRef != "" {
		t.Fatalf("since review = %+v", s)
	}
}

// A size measured before merges were told apart is measured again once.
func TestOldSinceReviewOfAPairIsMeasuredAgainOnce(t *testing.T) {
	h := newHarness(t)
	pr := reviewedThenPushed(t, h, "", rubyMixed)
	old := *pr.SinceReview
	old.Version, old.Commits = 0, 25
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("since_review_json", old) }); err != nil {
		t.Fatal(err)
	}
	pollPR(h, time.Minute, 2, "b2")
	pollPR(h, time.Minute, 2, "b2")
	if s := h.pr(2).SinceReview; s == nil || s.Commits != 1 || s.Version != store.SinceReviewVersion {
		t.Fatalf("since review = %+v, want measured again", s)
	}
	if got := rangeCalls(h.gh, "b1", "b2"); len(got) != 2 {
		t.Fatalf("compares of b1...b2 = %q, want the push's and one more", got)
	}
}

func TestOwnLineDelta(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after []string
		add, del      int
	}{
		{"unchanged", []string{"+a", "-b"}, []string{"+a", "-b"}, 0, 0},
		{"an added line changed", []string{"+x * 2"}, []string{"+x * 3"}, 1, 1},
		{"a line no longer added", []string{"+a", "+b"}, []string{"+a"}, 0, 1},
		{"a line no longer removed", []string{"-old"}, nil, 1, 0},
		{"a line now removed", nil, []string{"-old"}, 0, 1},
		{"copies count", []string{"+a"}, []string{"+a", "+a", "+a"}, 2, 0},
		{"the no-newline marker counts nothing", []string{"+a"}, []string{"+a", `\ No newline at end of file`}, 0, 0},
	} {
		if add, del := ownLineDelta(tc.before, tc.after); add != tc.add || del != tc.del {
			t.Errorf("%s: +%d -%d, want +%d -%d", tc.name, add, del, tc.add, tc.del)
		}
	}
}

func TestOwnCommitsAreTheNewSHAsOrTheTotalsWhenCut(t *testing.T) {
	before := github.PushComparison{Commits: 2, SHAs: []string{"c1", "c2"}}
	if n := ownCommits(before, github.PushComparison{Commits: 4, SHAs: []string{"c1", "c2", "c3", "mc"}}); n != 2 {
		t.Errorf("merge: %d, want 2", n)
	}
	if n := ownCommits(before, github.PushComparison{Commits: 2, SHAs: []string{"r1", "r2"}}); n != 2 {
		t.Errorf("rebase: %d, want every rewritten commit", n)
	}
	if n := ownCommits(before, github.PushComparison{Commits: 150, SHAs: []string{"c1"}}); n != 148 {
		t.Errorf("cut list: %d, want the difference of the totals", n)
	}
}
