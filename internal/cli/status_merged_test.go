package cli

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// mergedFixture is statusFixture with a local-time clock (so "15:04" and
// "Jan 2 15:04" read the same in every time zone) and the recent_closed
// window set explicitly.
func mergedFixture(t *testing.T, window time.Duration) (*store.Store, statusDeps, time.Time) {
	t.Helper()
	_, st, d, _ := statusFixture(t)
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	d.Now = func() time.Time { return now }
	cfg := *d.Config
	cfg.Board.RecentClosed = config.Duration{Duration: window}
	d.Config = &cfg
	return st, d, now
}

// closedPR seeds a PR in state from and closes it as the engine does: the
// state it closed in goes to prev_state, GitHub's state becomes ghState.
// reviewed is the reviewed_sha ("" = never reviewed).
func closedPR(t *testing.T, st *store.Store, number int, from, ghState, reviewed string, mergedAt, closedAt *time.Time) {
	t.Helper()
	_, pr := inspSeedPR(t, st, "talkable/talkable", number, from, func(u *store.PRUpdate) {
		if reviewed != "" {
			u.Set("reviewed_sha", reviewed)
		}
	})
	err := st.TransitionPR(context.Background(), pr.ID, []string{from}, store.PRClosed, func(u *store.PRUpdate) {
		u.Copy("prev_state", "state")
		u.Set("gh_state", ghState)
		if mergedAt != nil {
			u.Set("merged_at", *mergedAt)
		}
		if closedAt != nil {
			u.Set("closed_at", *closedAt)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

// mergedAttention is the attention rows about merged-unreviewed PRs, as
// "subject=message", in the order the status lists them.
func mergedAttention(t *testing.T, d statusDeps) []string {
	t.Helper()
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range r.Attention {
		if len(a.Message) >= 17 && a.Message[:17] == "merged unreviewed" {
			got = append(got, a.Subject+"="+a.Message)
			if a.Fix != "" {
				t.Errorf("%s carries a fix %q; there is nothing to run", a.Subject, a.Fix)
			}
		}
	}
	return got
}

const mergedHead = "abcdef0123456789abcdef0123456789abcdef01" // the head inspSeedPR gives every PR

// A PR GitHub merged before magnum reviewed its last push is listed under
// ATTENTION with the merge time, newest first, after the rows that need a
// fix, and still shows in the closed list.
func TestStatusListsAPRMergedBeforeItWasReviewed(t *testing.T) {
	st, d, now := mergedFixture(t, 24*time.Hour)
	today := now.Add(-2 * time.Hour)                              // 16:00 today
	yesterday := time.Date(2026, 10, 2, 22, 30, 0, 0, time.Local) // 19h30m ago
	closedPR(t, st, 12001, store.PRReviewing, store.GHMerged, "", &yesterday, nil)
	closedPR(t, st, 12002, store.PRQueued, store.GHMerged, "", &today, nil)

	got := mergedAttention(t, d)
	want := []string{"talkable#12002=merged unreviewed at 16:00", "talkable#12001=merged unreviewed at Oct 2 22:30"}
	if !slices.Equal(got, want) {
		t.Fatalf("merged-unreviewed attention = %q, want %q", got, want)
	}

	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, a := range r.Attention {
		subjects = append(subjects, a.Subject)
	}
	needsFix, first := slices.Index(subjects, "talkable#11950"), slices.Index(subjects, "talkable#12002")
	if needsFix < 0 || first < 0 || needsFix > first {
		t.Errorf("attention order %v: the PR that needs a fix (talkable#11950) must come before the merged ones", subjects)
	}
	var closing []int
	for _, c := range r.Closing {
		closing = append(closing, c.Number)
	}
	if !slices.Contains(closing, 12001) || !slices.Contains(closing, 12002) {
		t.Errorf("closing list %v lacks the merged PRs: they stay there as before", closing)
	}
}

// Closed by GitHub without merged_at (never seen merging): closed_at dates
// it; with neither time there is nothing to date, so it is skipped.
func TestStatusDatesAMergedUnreviewedPRByClosedAtWhenMergedAtIsMissing(t *testing.T) {
	st, d, now := mergedFixture(t, 24*time.Hour)
	closed := now.Add(-3 * time.Hour) // 15:00 today
	closedPR(t, st, 12003, store.PRQueued, store.GHMerged, "", nil, &closed)
	closedPR(t, st, 12004, store.PRQueued, store.GHMerged, "", nil, nil)

	got := mergedAttention(t, d)
	if want := []string{"talkable#12003=merged unreviewed at 15:00"}; !slices.Equal(got, want) {
		t.Fatalf("merged-unreviewed attention = %q, want %q", got, want)
	}
}

// Only a PR merged inside the window, in a state magnum meant to review, with
// a head it never reviewed, is listed.
func TestStatusSkipsClosedPRsThatAreNotMergedUnreviewedInTheWindow(t *testing.T) {
	st, d, now := mergedFixture(t, 24*time.Hour)
	recent := now.Add(-time.Hour)
	before := now.Add(-25 * time.Hour)                                               // the window is 24h
	closedPR(t, st, 12010, store.PRQueued, store.GHMerged, "", &before, nil)         // merged before the window
	closedPR(t, st, 12011, store.PRBaseline, store.GHMerged, "", &recent, nil)       // baseline: never meant to be reviewed
	closedPR(t, st, 12012, store.PRQueued, store.GHMerged, mergedHead, &recent, nil) // the head it merged at was reviewed
	closedPR(t, st, 12013, store.PRQueued, store.GHClosed, "", nil, &recent)         // closed, not merged
	closedPR(t, st, 12014, store.PRReviewing, store.GHMerged, "", &recent, nil)      // the one that counts

	got := mergedAttention(t, d)
	if want := []string{"talkable#12014=merged unreviewed at " + recent.Format("15:04")}; !slices.Equal(got, want) {
		t.Fatalf("merged-unreviewed attention = %q, want %q", got, want)
	}
}

// recent_closed = 0 turns the board's recent window off, and with it this
// list.
func TestStatusListsNoMergedUnreviewedPRWhenRecentClosedIsZero(t *testing.T) {
	st, d, now := mergedFixture(t, 0)
	recent := now.Add(-time.Hour)
	closedPR(t, st, 12020, store.PRQueued, store.GHMerged, "", &recent, nil)

	if got := mergedAttention(t, d); len(got) != 0 {
		t.Fatalf("recent_closed = 0 still lists %q", got)
	}

	d.Config = nil // no config: no window to read
	if got := mergedAttention(t, d); len(got) != 0 {
		t.Fatalf("a status without a config lists %q", got)
	}
}
