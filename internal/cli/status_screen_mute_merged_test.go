package cli

import (
	"maps"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/store"
)

// The dashboard's rows say whether their PR is flagged merged unreviewed or
// has that flag dismissed by a mute, so M can dismiss or restore it: queue and
// closing rows from the registry row, slot rows from the slot's PR.
func TestStatusDashDataCarriesTheMergeFlag(t *testing.T) {
	merged := func(n int, muted, forced bool) store.PR {
		return store.PR{Number: n, State: store.PRClosed, GHState: store.GHMerged, PrevState: new(store.PRQueued),
			HeadSHA: "b2", ReviewedSHA: new("b1"), Muted: muted, Forced: forced}
	}
	flagged, dismissed, mutedForced := merged(5, false, false), merged(6, true, false), merged(7, true, true)
	open := store.PR{Number: 8, State: store.PRReviewed, GHState: store.GHOpen, HeadSHA: "b2", ReviewedSHA: new("b1"), Muted: true}
	r := statusReport{
		GeneratedAt: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
		Slots: []inventory.SlotView{
			{Slot: store.Slot{Name: "review1", RepoFullName: "talkable/talkable"}, PR: &flagged},
			{Slot: store.Slot{Name: "review2", RepoFullName: "talkable/talkable"}, PR: &dismissed},
			{Slot: store.Slot{Name: "review3", RepoFullName: "talkable/talkable"}, PR: &open},
		},
		Queue: []statusPRLine{{Repo: "talkable/talkable", Number: 8, State: store.PRReviewed, rec: &open}},
		Closing: []statusPRLine{
			{Repo: "talkable/talkable", Number: 5, State: store.PRClosed, rec: &flagged},
			{Repo: "talkable/talkable", Number: 6, State: store.PRClosed, rec: &dismissed},
			{Repo: "talkable/talkable", Number: 7, State: store.PRClosed, rec: &mutedForced},
			{Repo: "talkable/talkable", Number: 9, State: store.PRClosed}, // no registry row at hand
		},
	}
	got := statusDashData(r, "talkable/talkable")

	type flags struct{ flagged, dismissed bool }
	slots := map[string]flags{}
	for _, s := range got.Slots {
		slots[s.Name] = flags{s.PRMergedUnreviewed, s.PRFlagDismissed}
	}
	if want := (map[string]flags{"review1": {true, false}, "review2": {false, true}, "review3": {false, false}}); !maps.Equal(slots, want) {
		t.Errorf("slot flags %v, want %v", slots, want)
	}
	queue := map[string]flags{}
	for _, q := range got.Queue {
		queue[q.Ref] = flags{q.MergedUnreviewed, q.FlagDismissed}
	}
	for ref, want := range map[string]flags{"talkable#5": {true, false}, "talkable#6": {false, true}, "talkable#7": {true, false},
		"talkable#8": {false, false}, "talkable#9": {false, false}} {
		if queue[ref] != want {
			t.Errorf("queue row %s flags %+v, want %+v", ref, queue[ref], want)
		}
	}
}

// The printed and drawn board get the same flags from the registry's row.
func TestPRsBoardRowCarriesFlagDismissed(t *testing.T) {
	row := prsBoardRow(store.BoardRow{Owner: "talkable", Name: "talkable", Number: 5, GHState: store.GHMerged, Muted: true, FlagDismissed: true}, nil)
	if !row.FlagDismissed || row.MergedUnreviewed {
		t.Errorf("row: dismissed %v flagged %v", row.FlagDismissed, row.MergedUnreviewed)
	}
	row = prsBoardRow(store.BoardRow{Owner: "talkable", Name: "talkable", Number: 6, GHState: store.GHMerged, MergedUnreviewed: true}, nil)
	if row.FlagDismissed || !row.MergedUnreviewed {
		t.Errorf("row: dismissed %v flagged %v", row.FlagDismissed, row.MergedUnreviewed)
	}
}
