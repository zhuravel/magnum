package fixwork

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// The fixtures of the tests that moved here from internal/engine with the
// code they test, as the engine's tests have them.

const (
	// fixHead is the head magnum reviewed, which the fixer starts from.
	fixHead = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	// fixPushed is the head the fixer pushes.
	fixPushed = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	// prPage is the operator's PR #2.
	prPage = "https://github.com/talkable/talkable/pull/2"
	// liveRunID is the run of the live case of 2026-11-09 that started no
	// job.
	liveRunID = 42673098082
)

// openNow is the look's time in the verdict tests.
var openNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// idleFacts are the facts of an own PR on which everything holds: magnum
// reviewed its head an hour ago and found nothing, a colleague approved,
// the read is whole, nothing is open and the settle passed. DecideOpen
// says idle; each test takes one thing away.
func idleFacts() Facts {
	head := fixHead
	activity := openNow.Add(-30 * time.Minute)
	return Facts{
		Now: openNow,
		PR: store.PR{ID: 2, Number: 2, URL: prPage, HeadSHA: head, ReviewedSHA: &head, State: store.PRReviewed, HeadChangedAt: openNow.Add(-time.Hour),
			ActivityAt: &activity, ReviewGate: &store.ReviewGate{Decision: "APPROVED"}},
		Sum:    &store.ReviewSummary{RunID: "r1", ReviewID: 101, SHA: head, URL: prPage + "#pullrequestreview-101", At: openNow.Add(-50 * time.Minute)},
		Whole:  true,
		Settle: Settle{Quiet: 3 * time.Minute, Max: 20 * time.Minute, Late: 2 * time.Hour, HandsOff: 30 * time.Minute, MaxLoops: 3},
		Ledger: Ledger{FixerHeads: []string{head}},
	}
}

// personThread is a colleague's unanswered thread item.
func personThread(id int64) Item {
	return Item{Key: ThreadKey(id, id), Class: ClassPerson, Kind: ItemThread, URL: prPage + "#discussion_r" + itoa(id)}
}

// wantVerdict fails unless v is verdict and its why holds why.
func wantVerdict(t *testing.T, v OpenVerdict, verdict, why string) {
	t.Helper()
	if v.Verdict != verdict || !strings.Contains(v.Why, why) {
		t.Errorf("verdict = %s %q, want %s %q", v.Verdict, v.Why, verdict, why)
	}
}

// itemKeys are the keys of items, sorted.
func itemKeys(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Key)
	}
	slices.Sort(out)
	return out
}

func itoa(n int64) string { return fmt.Sprint(n) }
