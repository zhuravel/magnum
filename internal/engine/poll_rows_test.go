package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// A poll reads the repository's open rows and the rows its radar lists: a
// released or closed PR no longer on the radar stays unread, and one the
// radar shows again (reopened) is read, so it keeps its row and returns to
// its state.
func TestThePollReadsOnlyTheOpenRowsAndTheRadars(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick()
	h.gh.closed[2], h.gh.closed[3] = "CLOSED", "MERGED"
	h.open(prSpec{n: 1, head: "base1"})
	for range 3 {
		h.advance(30 * time.Second)
		h.tick()
	}
	h.advance(11 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReleased)
	if p := h.pr(3); p.GHState != store.GHMerged {
		t.Fatalf("#3 = %+v, want merged", p)
	}

	repo := repoID(t, h)
	numbers := func(radar ...int) []int {
		t.Helper()
		var rr []github.PRRadar
		for _, n := range radar {
			rr = append(rr, github.PRRadar{NodeID: h.pr(n).NodeID, Number: n})
		}
		rows, err := h.e.pollRows(h.ctx, repo, rr)
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for _, p := range rows {
			out = append(out, p.Number)
		}
		return out
	}
	if got := numbers(1); !slices.Equal(got, []int{1}) {
		t.Errorf("rows of a radar of #1 = %v, want [1]: the released #2 and merged #3 stay unread", got)
	}
	if got := numbers(1, 2); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("rows of a radar of #1 and #2 = %v, want [1 2]", got)
	}

	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.wantState(2, store.PRReviewed)
}
