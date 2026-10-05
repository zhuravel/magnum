package store

import (
	"context"
	"testing"
	"time"
)

// ClosedSince adds the PRs GitHub merged or closed within the window
// (merged_at, else closed_at) to the open ones, without IncludeClosed, and
// leaves older closed PRs out.
func TestBoardClosedSinceAddsRecentlyClosedPRs(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	upsert := func(n int, gh string, merged, closed *time.Time) PR {
		t.Helper()
		res, err := st.UpsertPRFromGitHub(ctx, GitHubPR{RepoID: repo.ID, NodeID: "PR_" + itoa(int64(n)), Number: n, URL: "u" + itoa(int64(n)),
			HeadSHA: "head" + itoa(int64(n)), GHState: gh, GHUpdatedAt: Ptr(t0), MergedAt: merged, ClosedAt: closed,
			InitialState: PRQueued, Identity: "talkable-app"})
		if err != nil {
			t.Fatal(err)
		}
		return res.PR
	}
	upsert(1, GHOpen, nil, nil)
	upsert(2, GHMerged, Ptr(t0.Add(-time.Hour)), Ptr(t0.Add(-time.Hour)))       // merged an hour ago
	upsert(3, GHClosed, nil, Ptr(t0.Add(-2*time.Hour)))                         // closed two hours ago
	upsert(4, GHMerged, Ptr(t0.Add(-48*time.Hour)), Ptr(t0.Add(-48*time.Hour))) // merged two days ago
	upsert(5, GHMerged, Ptr(t0.Add(-30*time.Hour)), Ptr(t0.Add(-time.Hour)))    // merged_at wins over closed_at
	numbers := func(f BoardFilter) []int {
		t.Helper()
		rows, err := st.Board(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for _, r := range rows {
			out = append(out, r.Number)
		}
		return out
	}
	if got := numbers(BoardFilter{}); len(got) != 1 || got[0] != 1 {
		t.Fatalf("no window = %v, want only the open #1", got)
	}
	got := numbers(BoardFilter{ClosedSince: t0.Add(-24 * time.Hour)})
	if len(got) != 3 || !containsAll(got, 1, 2, 3) {
		t.Fatalf("24h window = %v, want #1 open and #2, #3 closed within it", got)
	}
	if got := numbers(BoardFilter{ClosedSince: t0.Add(-24 * time.Hour), IncludeClosed: true}); len(got) != 5 {
		t.Fatalf("window and IncludeClosed = %v, want every PR", got)
	}
	if got := numbers(BoardFilter{ClosedSince: t0.Add(-24 * time.Hour), States: []string{PRQueued}}); len(got) != 5 {
		t.Fatalf("States = %v, want every queued PR (States overrides the window)", got)
	}
	rows, err := st.Board(ctx, BoardFilter{ClosedSince: t0.Add(-24 * time.Hour), Repo: "talkable"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Number == 2 && (!r.MergedAt.Equal(t0.Add(-time.Hour)) || !r.ClosedAt.Equal(t0.Add(-time.Hour))) {
			t.Fatalf("#2 times: merged %v closed %v", r.MergedAt, r.ClosedAt)
		}
		if r.Number == 3 && (!r.MergedAt.IsZero() || !r.ClosedAt.Equal(t0.Add(-2*time.Hour))) {
			t.Fatalf("#3 times: merged %v closed %v", r.MergedAt, r.ClosedAt)
		}
	}
}

func containsAll(got []int, want ...int) bool {
	seen := map[int]bool{}
	for _, n := range got {
		seen[n] = true
	}
	for _, n := range want {
		if !seen[n] {
			return false
		}
	}
	return true
}

// A PR is merged unreviewed when GitHub merged it in a state where magnum
// meant to review it (a round due or running) before its head was reviewed.
// A muted one whose forced mark is gone (the mute handler clears it on a PR
// GitHub no longer lists as open) has its flag dismissed.
func TestIsMergedUnreviewed(t *testing.T) {
	for _, c := range []struct {
		name                     string
		gh, prev, head, reviewed string
		muted, forced            bool
		want                     bool
		dismissed                bool // muted, would be flagged unmuted
	}{
		{name: "rereview pending, head moved", gh: GHMerged, prev: PRRereviewPending, head: "b2", reviewed: "b1", want: true},
		{name: "queued, never reviewed", gh: GHMerged, prev: PRQueued, head: "b1", want: true},
		{name: "reviewing", gh: GHMerged, prev: PRReviewing, head: "b2", reviewed: "b1", want: true},
		{name: "claiming", gh: GHMerged, prev: PRClaiming, head: "b2", want: true},
		{name: "verifying", gh: GHMerged, prev: PRVerifying, head: "b2", reviewed: "b1", want: true},
		{name: "paused", gh: GHMerged, prev: PRPaused, head: "b2", reviewed: "b1", want: true},
		{name: "needs attention", gh: GHMerged, prev: PRNeedsAttention, head: "b2", reviewed: "b1", want: true},
		{name: "forced while muted", gh: GHMerged, prev: PRQueued, head: "b2", muted: true, forced: true, want: true},
		{name: "stale forced mark, never muted", gh: GHMerged, prev: PRQueued, head: "b2", reviewed: "b1", forced: true, want: true},
		{name: "dismissed: muted, the stale forced mark cleared", gh: GHMerged, prev: PRQueued, head: "b2", reviewed: "b1", muted: true, dismissed: true},
		{name: "restored: unmuted again, forced not restored", gh: GHMerged, prev: PRQueued, head: "b2", reviewed: "b1", want: true},
		{name: "head reviewed", gh: GHMerged, prev: PRRereviewPending, head: "b1", reviewed: "b1"},
		{name: "closed, not merged", gh: GHClosed, prev: PRRereviewPending, head: "b2", reviewed: "b1"},
		{name: "still open", gh: GHOpen, prev: "", head: "b2", reviewed: "b1"},
		{name: "baseline", gh: GHMerged, prev: PRBaseline, head: "b1"},
		{name: "ineligible (skipped or ignored)", gh: GHMerged, prev: PRIneligible, head: "b1"},
		{name: "reviewed", gh: GHMerged, prev: PRReviewed, head: "b2", reviewed: "b1"},
		{name: "muted", gh: GHMerged, prev: PRQueued, head: "b2", reviewed: "b1", muted: true, dismissed: true},
		{name: "muted, head reviewed: nothing to dismiss", gh: GHMerged, prev: PRQueued, head: "b1", reviewed: "b1", muted: true},
		{name: "muted, closed not merged: nothing to dismiss", gh: GHClosed, prev: PRQueued, head: "b2", reviewed: "b1", muted: true},
		{name: "muted, closed from reviewed: nothing to dismiss", gh: GHMerged, prev: PRReviewed, head: "b2", reviewed: "b1", muted: true},
		{name: "muted, ignored: nothing to dismiss", gh: GHMerged, prev: PRIneligible, head: "b2", muted: true},
		{name: "no prev state", gh: GHMerged, prev: "", head: "b2", reviewed: "b1"},
	} {
		if got := IsMergedUnreviewed(c.gh, c.prev, c.head, c.reviewed, c.muted, c.forced); got != c.want {
			t.Errorf("%s: IsMergedUnreviewed = %v, want %v", c.name, got, c.want)
		}
		if got := IsFlagDismissed(c.gh, c.prev, c.head, c.reviewed, c.muted, c.forced); got != c.dismissed {
			t.Errorf("%s: IsFlagDismissed = %v, want %v", c.name, got, c.dismissed)
		}
		pr := PR{GHState: c.gh, HeadSHA: c.head, Muted: c.muted, Forced: c.forced}
		if c.prev != "" {
			pr.PrevState = Ptr(c.prev)
		}
		if c.reviewed != "" {
			pr.ReviewedSHA = Ptr(c.reviewed)
		}
		if got := pr.MergedUnreviewed(); got != c.want {
			t.Errorf("%s: PR.MergedUnreviewed = %v, want %v", c.name, got, c.want)
		}
		if got := pr.FlagDismissed(); got != c.dismissed {
			t.Errorf("%s: PR.FlagDismissed = %v, want %v", c.name, got, c.dismissed)
		}
		if c.want && c.dismissed {
			t.Errorf("%s: a PR is never both flagged and dismissed", c.name)
		}
	}
}

// The board row carries the flag and what its card needs: the state the PR
// closed in and both SHAs.
func TestBoardRowMergedUnreviewed(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	mk := func(n int, state, reviewed string) PR {
		t.Helper()
		res, err := st.UpsertPRFromGitHub(ctx, GitHubPR{RepoID: repo.ID, NodeID: "PR_" + itoa(int64(n)), Number: n, URL: "u" + itoa(int64(n)),
			HeadSHA: "b2", GHState: GHOpen, GHUpdatedAt: Ptr(t0), InitialState: state, Identity: "talkable-app"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdatePR(ctx, res.PR.ID, func(u *PRUpdate) {
			if reviewed != "" {
				u.Set("reviewed_sha", reviewed)
			}
		}); err != nil {
			t.Fatal(err)
		}
		// Closed the way the engine confirms it: prev_state keeps the state left.
		if err := st.TransitionPR(ctx, res.PR.ID, []string{state}, PRClosed, func(u *PRUpdate) {
			u.Copy("prev_state", "state")
			u.Set("gh_state", GHMerged)
			u.Set("merged_at", t0.Add(-time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
		return res.PR
	}
	mk(1, PRRereviewPending, "b1")
	mk(2, PRBaseline, "")
	mk(3, PRRereviewPending, "b2")
	muted := mk(4, PRRereviewPending, "b1")  // muted after it closed: dismissed
	forced := mk(5, PRRereviewPending, "b1") // muted and requested before it closed: still flagged
	for _, c := range []struct {
		pr     PR
		forced bool
	}{{muted, false}, {forced, true}} {
		if err := st.UpdatePR(ctx, c.pr.ID, func(u *PRUpdate) {
			u.Set("muted", true)
			u.Set("forced", c.forced)
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.Board(ctx, BoardFilter{ClosedSince: t0.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	flags, dismissed := map[int]bool{}, map[int]bool{}
	for _, r := range rows {
		flags[r.Number], dismissed[r.Number] = r.MergedUnreviewed, r.FlagDismissed
		if r.Number == 1 && (r.PrevState != PRRereviewPending || r.ReviewedSHA != "b1" || r.HeadSHA != "b2") {
			t.Fatalf("#1 row: prev %q reviewed %q head %q", r.PrevState, r.ReviewedSHA, r.HeadSHA)
		}
	}
	if len(rows) != 5 || !flags[1] || flags[2] || flags[3] || flags[4] || !flags[5] {
		t.Fatalf("merged unreviewed = %v (%d rows), want #1 and #5", flags, len(rows))
	}
	if dismissed[1] || dismissed[2] || dismissed[3] || !dismissed[4] || dismissed[5] {
		t.Fatalf("flag dismissed = %v, want only #4", dismissed)
	}
}
