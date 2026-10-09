package engine

import (
	"fmt"
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

// When the PR's own diff cannot be compared in full (a patch too large to
// send: no patch, its lines counted), the files and lines are the push's
// comparison, master's changes included, and say so (raw); the commits are
// still the PR's own, which need only the two comparisons' commit lists.
// Only when a comparison of the own diff fails are they the push's too.
func TestSinceReviewKeepsTheRawSizeButThePRsOwnCommitsWhenTheOwnDiffIsIncomplete(t *testing.T) {
	tooLarge := slices.Clone(ownAfterConflict)
	tooLarge[0] = github.FileDelta{Path: tooLarge[0].Path, Status: "modified", BlobSHA: "5716ca5", Truncated: true}
	for _, tc := range []struct {
		name    string
		after   []github.FileDelta
		commits int
	}{
		{"a patch too large", tooLarge, 2},
		{"the comparison after the push fails", nil, 13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.gh.shas = map[string][]string{"master..." + reviewedTip: {"c1", "c2"}, "master..." + mergedHead: {"c1", "c2", "c3", "mc"}}
			h.gh.compare["master..."+reviewedTip] = github.CompareStats{Commits: 2}
			h.gh.compare["master..."+mergedHead] = github.CompareStats{Commits: 4}
			h.reviewedPR(2, reviewedTip)
			setPushFiles(h, true, "ahead", ownBefore, tc.after)
			h.gh.mu.Lock()
			h.gh.compare[reviewedTip+"..."+mergedHead] = github.CompareStats{Commits: 13, Files: 5, Additions: 48, Deletions: 47}
			h.gh.mu.Unlock()
			pollPR(h, time.Minute, 2, mergedHead)
			s := h.pr(2).SinceReview
			if s == nil || s.Commits != tc.commits || s.Files != 5 || s.Additions != 48 || s.Deletions != 47 || !s.Raw || s.BaseMerged || s.BaseRef != "master" {
				t.Fatalf("since review = %+v, want %d commits and the push's 5 files, +48 -47, marked raw", s, tc.commits)
			}
		})
	}
}

// The live case (a stacked PR): the PR's base branch is another feature
// branch, which the PR merged after its review. The push lists that
// branch's commits and files (44 commits, GitHub's cap of 300 files), so
// only the PR's own diff tells what changed: before the push every file has
// its patch, after it the same patches and two empty files GitHub sends no
// patch for. Those are complete (an empty diff, not a missing one), so the
// size is the PR's own: its own commits and the two added files, not 44
// commits and 300 files.
func TestSinceReviewOfAStackedPRThatMergedItsBaseIsThePRsOwn(t *testing.T) {
	const stack = "feature-render-view"
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: reviewedTip, base: stack})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)

	push := reviewedTip + "..." + mergedHead
	capped := make([]github.FileDelta, github.CompareFileLimit)
	for i := range capped {
		capped[i] = github.FileDelta{Path: fmt.Sprintf("app/views/stack_%d.rb", i), Status: "modified", Patch: "@@ -1 +1 @@\n-a\n+b", Truncated: true}
	}
	keep := func(dir string) github.FileDelta {
		return github.FileDelta{Path: "app/views/partials/" + dir + "/.keep", Status: "added", BlobSHA: emptyBlob}
	}
	after := append(slices.Clone(ownBefore), keep("partials"), keep("static_assets"))
	h.gh.mu.Lock()
	h.gh.files = map[string][]github.FileDelta{push: capped, stack + "..." + reviewedTip: ownBefore, stack + "..." + mergedHead: after}
	h.gh.compare[push] = github.CompareStats{Commits: 44, Files: -1, Additions: 9570, Deletions: 554}
	h.gh.merges = map[string]bool{push: true}
	h.gh.shas = map[string][]string{stack + "..." + reviewedTip: {"c1", "c2"}, stack + "..." + mergedHead: {"c1", "c2", "c3", "mc"}}
	h.gh.compare[stack+"..."+reviewedTip] = github.CompareStats{Commits: 2}
	h.gh.compare[stack+"..."+mergedHead] = github.CompareStats{Commits: 4}
	h.gh.mu.Unlock()
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: mergedHead, base: stack})
	h.tick()

	pr := h.wantState(2, store.PRRereviewPending)
	want := store.SinceReview{Source: store.SinceFromReviewed, Base: reviewedTip, Head: mergedHead, Commits: 2, Files: 2,
		BaseMerged: true, BaseRef: stack, Version: store.SinceReviewVersion, ComputedAt: h.clock.Now()}
	if pr.SinceReview == nil || !equalSince(*pr.SinceReview, want) {
		t.Fatalf("since review = %+v, want %+v", pr.SinceReview, want)
	}
	if rec, ok := h.e.deltaRecord(h.ctx, pr.ID); !ok || rec.AddedFiles != 2 || rec.Lines != 0 || !rec.Complete {
		t.Fatalf("delta record %+v, want the two files the PR now adds", rec)
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

// A size measured before a file without a patch could be compared (version
// 1: an empty or binary file made the PR's own diff incomplete, so a push
// that merged the base branch kept the raw size, the base branch's commits
// counted as the PR's) is measured again once.
func TestSinceReviewMeasuredBeforePatchlessFilesCompareIsMeasuredAgain(t *testing.T) {
	h := newHarness(t)
	pr := reviewedThenPushed(t, h, "", rubyMixed)
	old := *pr.SinceReview
	old.Version, old.Commits, old.Raw, old.BaseRef = 1, 44, true, "master"
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("since_review_json", old) }); err != nil {
		t.Fatal(err)
	}
	pollPR(h, time.Minute, 2, "b2")
	if s := h.pr(2).SinceReview; s == nil || s.Commits != 1 || s.Raw || s.Version != store.SinceReviewVersion {
		t.Fatalf("since review = %+v, want measured again", s)
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
		t.Run(tc.name, func(t *testing.T) {
			if add, del := ownLineDelta(tc.before, tc.after); add != tc.add || del != tc.del {
				t.Errorf("+%d -%d, want +%d -%d", add, del, tc.add, tc.del)
			}
		})
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
