package engine

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// A PR whose only change was invisible (GitHub's updatedAt moved, nothing a
// reviewer sees did) keeps its activity time: the radar still reads the
// moved updatedAt as a change and fetches the Details again, which the
// registry records as gh_updated_at, while the board's activity stays the
// Details' own. Once merged, its activity is the merge.
func TestAnInvisibleChangeKeepsThePRsActivity(t *testing.T) {
	h := newHarness(t)
	quiet := h.clock.Now().Add(-5 * 24 * time.Hour) // the last push, five days ago
	h.open(prSpec{n: 2, head: "b1", activity: quiet})
	h.startup()
	h.tick() // first sync: #2 is a baseline with its Details
	activity := func(when string, want time.Time) store.PR {
		t.Helper()
		pr := h.pr(2)
		if pr.ActivityAt == nil || !pr.ActivityAt.Equal(want) {
			t.Fatalf("%s: activity_at = %v, want %v", when, pr.ActivityAt, want)
		}
		rows, err := h.st.Board(h.ctx, store.BoardFilter{IncludeClosed: true})
		if err != nil || len(rows) != 1 || !rows[0].ActivityAt.Equal(want) {
			t.Fatalf("%s: board = %+v, %v", when, rows, err)
		}
		return pr
	}
	activity("first sync", quiet)

	h.advance(time.Hour)
	moved := h.clock.Now()
	before := h.gh.count("details:talkable/talkable:[2]")
	h.open(prSpec{n: 2, head: "b1", activity: quiet, updated: moved}) // a project field changed
	h.tick()
	if n := h.gh.count("details:talkable/talkable:[2]") - before; n != 1 {
		t.Fatalf("the moved updatedAt fetched the Details %d times, want once", n)
	}
	if pr := activity("invisible change", quiet); pr.GHUpdatedAt == nil || !pr.GHUpdatedAt.Equal(moved) {
		t.Fatalf("gh_updated_at = %v, want the radar's %v", pr.GHUpdatedAt, moved)
	}

	// Merged: GitHub's merge time (the fake's 2026-10-05 09:00 UTC) is its
	// last activity.
	h.advance(time.Minute)
	h.open()
	h.gh.closed[2] = "MERGED"
	h.tick()
	h.advance(confirmGap)
	h.tick()
	if pr := h.wantState(2, store.PRClosed); pr.MergedAt == nil {
		t.Fatalf("merged PR: %+v", pr)
	}
	activity("merged", time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC))
}
