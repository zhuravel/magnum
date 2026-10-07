package tui

import (
	"context"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The UPDATED column, its sort and the card's Updated read a PR's activity
// time, not GitHub's updatedAt, which a change no reviewer sees (a project
// field) moves: a PR whose last activity was five days ago reads "5d" and
// sorts below one active two hours ago, though GitHub updated it 11 minutes
// ago.
func TestPRBoardUpdatedIsTheActivityTime(t *testing.T) {
	rows := []PRBoardRow{
		{Ref: "talkable/talkable#1", Owner: "talkable", Repo: "talkable", Number: 1, Title: "Quiet one", Author: "alice",
			State: "reviewed", GHState: "OPEN", ActivityAt: ago(5 * 24 * time.Hour), GitHubUpdatedAt: ago(11 * time.Minute)},
		{Ref: "talkable/talkable#2", Owner: "talkable", Repo: "talkable", Number: 2, Title: "Busy one", Author: "alice",
			State: "reviewed", GHState: "OPEN", ActivityAt: ago(2 * time.Hour), GitHubUpdatedAt: ago(2 * time.Hour)},
	}
	src := &fakeBoardSource{rows: rows}
	m := testPRBoard(context.Background(), src, &fakeActions{}, PRBoardOptions{Now: func() time.Time { return boardNow }, SelfLogins: boardSelf})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 20}, prbDataMsg{rows: src.rows})
	if got := boardRefs(m); !slices.Equal(got, []string{"talkable/talkable#2", "talkable/talkable#1"}) {
		t.Fatalf("updated sort = %v, want the latest activity first", got)
	}
	v := viewOf(m)
	mustContain(t, v, "5d")
	mustNotContain(t, v, "11m")
	m, _ = send(t, m, keyMsg("j"), keyMsg("enter"))
	card := viewOf(m)
	mustContain(t, card, "5d ago")
	mustNotContain(t, card, "11m ago")
}
