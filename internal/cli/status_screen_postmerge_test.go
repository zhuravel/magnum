package cli

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/store"
)

// The dashboard's rows carry GitHub's state of their PR, so its y/N question
// can tell a merged PR (a post-merge review) from an open one: queue and
// closing rows from the registry row, slot rows from the slot's PR; a row
// with no registry row leaves it unknown.
func TestStatusDashDataCarriesGitHubState(t *testing.T) {
	merged := store.PR{Number: 5, State: store.PRQueued, GHState: store.GHMerged, URL: "https://github.com/talkable/talkable/pull/5"}
	open := store.PR{Number: 6, State: store.PRReviewed, GHState: store.GHOpen, URL: "https://github.com/talkable/talkable/pull/6"}
	closed := store.PR{Number: 8, State: store.PRClosed, GHState: store.GHClosed}
	r := statusReport{
		GeneratedAt: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		Slots: []inventory.SlotView{
			{Slot: store.Slot{Name: "review1", RepoFullName: "talkable/talkable"}, PR: &merged},
			{Slot: store.Slot{Name: "review2", RepoFullName: "talkable/talkable"}, PR: &open},
			{Slot: store.Slot{Name: "review3", RepoFullName: "talkable/talkable"}}, // holds no PR
		},
		Queue: []statusPRLine{
			{Repo: "talkable/talkable", Number: 5, State: store.PRQueued, rec: &merged},
			{Repo: "talkable/talkable", Number: 6, State: store.PRReviewed, rec: &open},
			{Repo: "talkable/talkable", Number: 7, State: store.PRQueued}, // no registry row at hand
		},
		Closing: []statusPRLine{{Repo: "talkable/talkable", Number: 8, State: store.PRClosed, rec: &closed}},
	}
	got := statusDashData(r, "talkable/talkable")

	var slots []string
	for _, s := range got.Slots {
		slots = append(slots, s.Name+" "+s.PRRef+" "+s.PRGHState)
	}
	if want := []string{"review1 talkable#5 MERGED", "review2 talkable#6 OPEN", "review3  "}; !slices.Equal(slots, want) {
		t.Errorf("slots %q, want %q", slots, want)
	}
	var queue []string
	for _, q := range got.Queue {
		queue = append(queue, q.Ref+" "+q.GHState)
	}
	if want := []string{"talkable#5 MERGED", "talkable#6 OPEN", "talkable#7 ", "talkable#8 CLOSED"}; !slices.Equal(queue, want) {
		t.Errorf("queue %q, want %q", queue, want)
	}
}
