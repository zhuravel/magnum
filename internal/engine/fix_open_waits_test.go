package engine

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// The loop the look starts while a person's handed items wait is limited to
// the work never handed: its fixer's file marks each waiting item `waits`
// (FixState.Waits, the items' keys), which the skill leaves alone, so no
// push or reply answers a colleague's open change request before the
// author did; the item it never had is not marked.
func TestTheLoopOverAPersonsWaitingItemLeavesItAlone(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	person := extThread(4322458360, "lib/sandbox.rb", 185, h.clock.Now().Add(-time.Hour), "Why does it run twice?", "rev-ann")
	h.setThreads(person)
	cycleFixer(h, git,
		func(id string) (string, string) {
			return "", fixReport(id, "nothing", "0 threads", "- 4322458360 lib/sandbox.rb:185: not fixed. only the author can say why")
		},
		func(id string) (string, string) {
			return "", fixReport(id, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
		})
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // loop 1 leaves the colleague's question to you
	pr := h.wantState(2, store.PRNeedsAttention)
	id := h.fix(FixPayload{Done: true, Seen: deref(pr.LastError)})
	h.handle()
	h.wantDone(id)
	lint := extThread(4322458500, "spec/a_spec.rb", 12, h.clock.Now(), "Repeated describe block", "lint-bot")
	h.setThreads(person, lint)
	h.advance(time.Minute)
	h.touch(fixHead)
	h.pastSettle()
	h.tick()
	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 {
		t.Fatalf("fix runs = %d, want a second loop on the lint thread while the colleague's question waits", len(runs))
	}
	if st := h.ledgerOf(2); len(st.Starts) != 2 {
		t.Errorf("ledger starts = %v", st.Starts)
	}
	x := readExternalOf(t, runs[1])
	waits := map[int64]bool{}
	for _, th := range x.Threads {
		waits[th.CommentID] = th.Waits
	}
	if !waits[4322458360] || waits[4322458500] {
		t.Errorf("waits in the second loop's file = %v, want the colleague's thread alone", waits)
	}
}
