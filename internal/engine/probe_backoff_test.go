package engine

import (
	"testing"
	"time"
)

// Two reviews posted together ask Whiteboard at once, and both asks time
// out: the wait is the first one's, not doubled by the second, whose ask
// started before the wait was known. So is the timeout of `magnum
// whiteboard`, which asks inside the wait. An ask that starts once the
// wait ended and times out doubles it, and an answer ends it.
func TestATimeoutOfAnAskStartedInsideTheWaitDoesNotDoubleIt(t *testing.T) {
	t.Parallel()
	var b probeBackoff
	t0 := time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)
	if next := b.asked(t0, true); !next.Equal(t0.Add(probeBackoffFirst)) {
		t.Fatalf("first timeout: next %v", next)
	}
	second := t0.Add(time.Second)
	if next := b.asked(second, true); !next.Equal(second.Add(probeBackoffFirst)) {
		t.Fatalf("an ask started inside the wait: next %v, want the same wait from its start", next)
	}
	if _, _, waiting := b.waiting(second.Add(probeBackoffFirst - time.Second)); !waiting {
		t.Fatal("not waiting within the wait")
	}
	later := second.Add(probeBackoffFirst)
	if _, _, waiting := b.waiting(later); waiting {
		t.Fatal("still waiting once the wait ended")
	}
	if next := b.asked(later, true); !next.Equal(later.Add(2 * probeBackoffFirst)) {
		t.Fatalf("an ask after the wait: next %v, want the wait doubled", next)
	}
	last := later
	for range 10 {
		last = last.Add(probeBackoffMax)
		b.asked(last, true)
	}
	if _, next, _ := b.waiting(last); !next.Equal(last.Add(probeBackoffMax)) {
		t.Fatalf("after many timeouts: next %v, want at most probeBackoffMax", next)
	}
	if next := b.asked(last.Add(time.Second), false); !next.IsZero() {
		t.Fatalf("an answer: next %v, want none", next)
	}
	if _, _, waiting := b.waiting(last.Add(time.Second)); waiting {
		t.Fatal("still waiting after an answer")
	}
}
