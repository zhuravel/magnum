package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

// A post-merge review: GitHub merged the PR, `magnum review` forced it back
// in line, and Candidates lists it like any forced PR. A merged PR nobody
// forced, and a PR closed without merging, never qualify.
func TestCandidatesIncludeAForcedMergedPR(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	now := t0.Add(time.Hour)
	clk.Set(now)

	postMerge := mustPR(t, st, repo.ID, 1, PRRereviewPending)
	setPR(t, st, postMerge.ID, func(u *PRUpdate) { u.Set("gh_state", GHMerged); u.Set("forced", true) })
	mergedQueued := mustPR(t, st, repo.ID, 2, PRQueued)
	setPR(t, st, mergedQueued.ID, func(u *PRUpdate) { u.Set("gh_state", GHMerged); u.Set("forced", true) })
	notForced := mustPR(t, st, repo.ID, 3, PRRereviewPending)
	setPR(t, st, notForced.ID, func(u *PRUpdate) { u.Set("gh_state", GHMerged) })
	closed := mustPR(t, st, repo.ID, 4, PRQueued)
	setPR(t, st, closed.ID, func(u *PRUpdate) { u.Set("gh_state", GHClosed); u.Set("forced", true) })
	mergedReviewed := mustPR(t, st, repo.ID, 5, PRClosed)
	setPR(t, st, mergedReviewed.ID, func(u *PRUpdate) { u.Set("gh_state", GHMerged); u.Set("forced", true) })

	got, err := st.Candidates(ctx, CandidateParams{Now: now, QuietPeriod: 5 * time.Minute, MinInterval: 30 * time.Minute, MaxRoundsPerDay: 6})
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, p := range got {
		nums = append(nums, p.Number)
	}
	slices.Sort(nums)
	if !slices.Equal(nums, []int{1, 2}) {
		t.Fatalf("candidates = %v, want the forced merged PRs 1 and 2", nums)
	}
}
