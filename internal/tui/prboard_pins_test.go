package tui

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// pinRow is a pinned PR on the board; closedAgo > 0 makes GitHub's state
// MERGED and the row one of the recently closed section.
func pinRow(n int, pinned bool, closedAgo time.Duration) PRBoardRow {
	r := PRBoardRow{Ref: "talkable/talkable#" + strconv.Itoa(n), Owner: "talkable", Repo: "talkable", Number: n,
		Title: "PR " + strconv.Itoa(n), Author: "ann", State: "reviewed", GHState: "OPEN", ActivityAt: ago(time.Hour),
		Slot: "~/Projects/talkable.review" + strconv.Itoa(n%10), Pinned: pinned}
	if closedAgo > 0 {
		r.State, r.GHState, r.Recent, r.ClosedAt = "closed", "MERGED", true, boardNow.Add(-closedAgo)
	}
	return r
}

func boardSummary(t *testing.T, rows []PRBoardRow) string {
	t.Helper()
	src := &fakeBoardSource{rows: rows}
	m := testPRBoard(context.Background(), src, &fakeActions{}, PRBoardOptions{Now: func() time.Time { return boardNow }, SelfLogins: boardSelf})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 240, Height: 24})
	next, _ = next.(prBoardModel).Update(prbDataMsg{rows: rows})
	return viewOf(next.(prBoardModel))
}

// A pin outlives its PR, and the summary pill says how many of the pins hold
// a PR that GitHub closed: "4 pinned · 2 on closed PRs", words in the pill,
// no new line. Without such a pin the pill is what it was.
func TestBoardSummaryNamesThePinsOnClosedPRs(t *testing.T) {
	view := boardSummary(t, []PRBoardRow{
		pinRow(10001, true, 0), pinRow(10002, true, 0),
		pinRow(10003, true, 5*time.Hour), pinRow(10004, true, 2*time.Hour),
		pinRow(10005, false, time.Hour), // closed, not pinned: not counted
	})
	if !strings.Contains(view, "4 pinned · 2 on closed PRs") {
		t.Errorf("summary lacks %q:\n%s", "4 pinned · 2 on closed PRs", view)
	}

	view = boardSummary(t, []PRBoardRow{pinRow(10001, true, 0), pinRow(10002, true, 0), pinRow(10005, false, time.Hour)})
	if !strings.Contains(view, "2 pinned") || strings.Contains(view, "on closed PRs") {
		t.Errorf("summary without a pin on a closed PR:\n%s", view)
	}
}
